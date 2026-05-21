package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) ensureStructPtrReleaseFn(structName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structPtrReleaseFns[structName]; ok {
		return fn
	}

	ptrType := irtypes.NewPointer(st)
	fnName := structName + "__release_ptr"
	fn := cg.activeModule().NewFunc(fnName, irtypes.Void, ir.NewParam("ptr", ptrType))
	// weak_odr lets multiple pkg modules emit the same symbol without
	// link conflict: any pkg that scope-releases a `*<struct>` -- including
	// trait-iface fat ptrs whose vtable thunk dispatches here -- references
	// the helper.  Pre-fix the linkage was the IR default (external), which
	// happens to work today because cg.structPtrReleaseFns caches the
	// definition to a single CodeGen-active module, but the cache is fragile
	// across incremental rebuilds where the cached `.o` for one pkg may be
	// invalidated and re-emitted under a different pkg context.  Mirror the
	// pattern in ensureElemRetainHelper / ensureElemReleaseHelper.
	fn.Linkage = enum.LinkageWeakODR
	// Cache before generating body to handle any hypothetical recursive reference.
	cg.structPtrReleaseFns[structName] = fn

	entry := fn.NewBlock("entry")
	provCheck := fn.NewBlock("prov_check")
	doRelease := fn.NewBlock("do_release")
	releaseChildren := fn.NewBlock("release_children")
	exit := fn.NewBlock("exit")

	// Null guard.
	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, provCheck)

	// Provenance check: pointers that fall outside Tin's arena (C-allocated,
	// stack, static data, foreign mmap) short-circuit to no-op so the
	// header math on `ptrI8 - sizeof(TinRCHdr)` never reads garbage.
	// cLayoutStructs that aren't allocated through Tin's wrapper path
	// (e.g. a raw extern *Foo cast from *void) flow safely through this
	// check.  Cost: one call + branch per release; mimalloc's range
	// lookup is ~3-5 cycles in the predicted case.
	provPtr := provCheck.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	provOK := provCheck.NewCall(cg.ensureIsManaged(), provPtr)
	provIsManaged := provCheck.NewICmp(enum.IPredNE, provOK, constant.NewInt(irtypes.I32, 0))
	provCheck.NewCondBr(provIsManaged, doRelease, exit)

	// Load the struct value BEFORE decrementing RC (the block is still valid
	// since we hold a reference). Then atomically decrement RC. If the block
	// was freed (last ref), release child fields from the loaded value.
	structVal := doRelease.NewLoad(st, fn.Params[0])
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)
	doRelease.NewCondBr(isOne, releaseChildren, exit)

	// Block was freed (last reference). Release RC-tracked child fields
	// from the loaded struct value (which is on the stack, still valid).
	// Trait fat-ptrs handle their owned `data` field below via the
	// vtable's data-release thunk; calling the generic emitRelease here
	// for an iface would double-release `data` (extractRCDataPtr returns
	// the data field for iface shapes), so skip it.
	//
	// cLayoutStructs: the wrapper block is the rc-block (whether allocated
	// by emitStructPtrBorrow for *S returns or wrapNativeStructToTin for
	// S-by-value returns), and was just freed.  c_data_ptr is either a C
	// borrow (no Tin RC) or pointed inside the now-freed wrapper block;
	// either way, calling emitRelease here would corrupt or use-after-free.
	if !isTraitFatPtrShape(st) && !cg.cLayoutStructs[structName] {
		cg.emitRelease(releaseChildren, structVal)
	}

	// Trait-iface fat ptr: dispatch via the vtable's data-release
	// thunk (last slot) to call the wrapped concrete struct's
	// release_ptr.  Raw _tin_release would only free the outer block
	// and leak any RC-tracked fields (string / fat-array / nested
	// structs).  An unexpected vtable shape here panics at codegen
	// time rather than emitting the old silent raw-release fallback:
	// the genTraitVtables path always appends the data-release thunk
	// as the last slot, so a shape mismatch indicates a codegen
	// invariant break that we want to surface loudly during
	// development -- the silent fallback would manifest as a slow,
	// hard-to-diagnose leak in any consumer that releases the iface.
	if isTraitFatPtrShape(st) {
		dataField := releaseChildren.NewExtractValue(structVal, 0)
		vtableField := releaseChildren.NewExtractValue(structVal, 1)

		vtablePtrType, ok := st.Fields[1].(*irtypes.PointerType)
		if !ok {
			panic(fmt.Sprintf("ensureStructPtrReleaseFn: trait iface %s vtable field is not a pointer type", structName))
		}

		vtableSt, ok2 := vtablePtrType.ElemType.(*irtypes.StructType)
		if !ok2 || len(vtableSt.Fields) == 0 {
			panic(fmt.Sprintf("ensureStructPtrReleaseFn: trait iface %s vtable struct has no fields", structName))
		}

		lastIdx := len(vtableSt.Fields) - 1
		lastFieldType := vtableSt.Fields[lastIdx]

		lastPt, ok3 := lastFieldType.(*irtypes.PointerType)
		if !ok3 {
			panic(fmt.Sprintf("ensureStructPtrReleaseFn: trait iface %s vtable last slot is not a pointer", structName))
		}

		lastFnType, ok4 := lastPt.ElemType.(*irtypes.FuncType)
		if !ok4 || len(lastFnType.Params) != 1 || !lastFnType.Params[0].Equal(irtypes.I8Ptr) || !irtypes.IsVoid(lastFnType.RetType) {
			panic(fmt.Sprintf("ensureStructPtrReleaseFn: trait iface %s vtable last slot is not void(i8*) data-release thunk", structName))
		}

		releaseFnSlot := releaseChildren.NewGetElementPtr(vtableSt, vtableField,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(lastIdx)))
		releaseFn := releaseChildren.NewLoad(lastFieldType, releaseFnSlot)

		releaseChildren.NewCall(releaseFn, dataField)
		releaseChildren.NewBr(exit)

		exit.NewRet(nil)

		return fn
	}

	releaseChildren.NewBr(exit)

	exit.NewRet(nil)

	return fn
}

// ensureHeapBlockReleaseFn returns (or lazily emits) a null-safe release
// helper for an RC-allocated heap block whose content is one element of
// type t.  The function decrements the block's RC; if it hits zero, the
// loaded element is passed through emitRelease so its ARC sub-fields
// (e.g. element strings of a [string] fat array) are released before
// the block itself is freed.
//
// Signature:
//
//	define void @__tin_release_heap_<key>(<t>* %ptr) {
//	entry:
//	  br i1 (ptr == null), exit, do
//	do:
//	  v        = load <t>, ptr
//	  freed    = _tin_release_struct(bitcast ptr to i8*)
//	  br i1 (freed == 1), free_kids, exit
//	free_kids:
//	  emitRelease(v)   // walks ARC sub-fields recursively
//	  br exit
//	exit:
//	  ret void
//	}
//
// Used by releaseUnreturned for unreturned early-heap-promoted locals
// whose type is not a named struct (so the existing ensureStructPtrReleaseFn
// doesn't apply): fat arrays, strings, anys, fat fn ptrs.  Pre-fix that
// path called raw _tin_release on the heap block, which freed the outer
// block but never released the element's RC sub-fields.
func (cg *CodeGen) ensureHeapBlockReleaseFn(t irtypes.Type) *ir.Func {
	key := cg.elemTypeKey(t)
	if fn, ok := cg.heapBlockReleaseFns[key]; ok {
		return fn
	}

	ptrType := irtypes.NewPointer(t)
	name := "__tin_release_heap_" + key
	fn := cg.activeModule().NewFunc(name, irtypes.Void, ir.NewParam("ptr", ptrType))
	// weak_odr matches the other shared per-type ARC helpers.
	fn.Linkage = enum.LinkageWeakODR
	cg.heapBlockReleaseFns[key] = fn

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	freeKids := fn.NewBlock("free_kids")
	exit := fn.NewBlock("exit")

	// Null guard.
	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load BEFORE decrement (the block is still valid since we hold a ref).
	elemVal := doRelease.NewLoad(t, fn.Params[0])
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)
	doRelease.NewCondBr(isOne, freeKids, exit)

	cg.emitRelease(freeKids, elemVal)
	freeKids.NewBr(exit)

	exit.NewRet(nil)

	return fn
}

// ensureHeapChainReleaseFn lazily generates a null-safe release function for a
// depth-N cLayoutStruct pointer chain written by N*S out-param write-backs.
//
// For depth=1: delegates to ensureStructPtrReleaseFn (existing null-safe helper).
// For depth>1: generates:
//
//	define void @structName__chain_N((N)*S.wrapper* %ptr) {
//	entry:
//	  br i1 (ptr==null), exit, do_release
//	do_release:
//	  inner = load (N-1)*S.wrapper, ptr
//	  call @structName__chain_{N-1}(inner)
//	  call _tin_release(bitcast ptr to i8*)
//	  br exit
//	exit:
//	  ret void
//	}
func (cg *CodeGen) ensureHeapChainReleaseFn(structName string, depth int) *ir.Func {
	if depth == 1 {
		wrapperSt := cg.structTypeFor(CanonKey(structName))

		return cg.ensureStructPtrReleaseFn(structName, wrapperSt)
	}

	key := fmt.Sprintf("%s__chain_%d", structName, depth)
	if fn, ok := cg.chainReleaseFns[key]; ok {
		return fn
	}

	// Build parameter type: (depth)*S.wrapper
	wrapperSt := cg.structTypeFor(CanonKey(structName))

	var paramType irtypes.Type = wrapperSt
	for i := 0; i < depth; i++ {
		paramType = irtypes.NewPointer(paramType)
	}

	fn := cg.activeModule().NewFunc(key, irtypes.Void, ir.NewParam("ptr", paramType))
	cg.chainReleaseFns[key] = fn // cache before generating body (handles recursive refs)

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	exit := fn.NewBlock("exit")

	// Null guard.
	isNull := entry.NewICmp(enum.IPredEQ,
		entry.NewBitCast(fn.Params[0], irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load inner (depth-1)*S.wrapper.
	innerType := paramType.(*irtypes.PointerType).ElemType
	innerPtr := doRelease.NewLoad(innerType, fn.Params[0])

	// Recursively release the inner chain.
	innerRelFn := cg.ensureHeapChainReleaseFn(structName, depth-1)
	doRelease.NewCall(innerRelFn, innerPtr)

	// Free this RC block.
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	doRelease.NewCall(cg.ensureRelease(), ptrI8)
	doRelease.NewBr(exit)

	exit.NewRet(nil)

	return fn
}

// cLayoutStructBaseName peels all pointer layers of a Tin AST type and returns
// the base struct name, or "" if the type is not a pointer-to-struct chain.
// E.g. *pvec2 -> "pvec2", **pvec2 -> "pvec2", ***pvec2 -> "pvec2".
func cLayoutStructBaseName(te ast.TypeExpr) string {
	cur := te

	for {
		pt, ok := cur.(*ast.PointerType)
		if !ok {
			return ""
		}

		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			return st.Name
		}

		cur = pt.Elem
	}
}

// emitHeapChainRelease releases a heap-promoted pointer chain of the given depth.
// For depth=1 (*T): loads T, releases T's ARC sub-fields, frees the RC block.
// For depth>1 (**T, ***T, ...): recursively releases inner chains before freeing
// the outer RC block.  This handles nested heap promotion (alloc_nested, etc.).
func (cg *CodeGen) emitHeapChainRelease(block *ir.Block, heapPtr value.Value, depth int) {
	ptrType, ok := heapPtr.Type().(*irtypes.PointerType)
	if !ok {
		return
	}

	elemType := ptrType.ElemType

	// For depth=1 with a named struct type: always use the null-safe per-struct
	// release helper. ensureStructPtrReleaseFn cascades through *TinStruct pointer
	// fields (e.g. a linked-list node's next field) even when elemNeedsRelease
	// returns false - elemNeedsRelease only checks for RC-tracked fat types and
	// nested struct values, not *TinStruct pointer fields.
	if depth == 1 {
		if st, ok2 := elemType.(*irtypes.StructType); ok2 && st.Name() != "" {
			if cg.isDataType(st) {
				if relFn := cg.ensureDataPtrReleaseFn(st.Name(), st); relFn != nil {
					block.NewCall(relFn, heapPtr)

					return
				}
			}

			relFn := cg.ensureStructPtrReleaseFn(st.Name(), st)
			block.NewCall(relFn, heapPtr)

			return
		}
	}

	// When the heap block holds a primitive leaf with no RC sub-fields, skip
	// the load entirely: loading from a potentially-NULL heapPtr would
	// segfault, and emitRelease would be a no-op anyway.  _tin_release is
	// null-safe, so call it directly.
	if depth == 1 && !cg.elemNeedsRelease(elemType) {
		rcI8 := block.NewBitCast(heapPtr, irtypes.I8Ptr)
		block.NewCall(cg.ensureRelease(), rcI8)

		return
	}

	// Load T from the heap block.
	tVal := block.NewLoad(elemType, heapPtr)

	if depth == 1 {
		// Leaf: release T's ARC sub-fields (handles strings/arrays inside structs).
		cg.emitRelease(block, tVal)
	} else {
		// Non-leaf: the loaded value is itself a heap-promoted pointer chain.
		cg.emitHeapChainRelease(block, tVal, depth-1)
	}

	// Free this RC block.
	rcI8 := block.NewBitCast(heapPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureRelease(), rcI8)
}

// emitScopeRelease emits _tin_release for all ARC-tracked variables in scope s
// whose block has not yet been terminated.  Named structs with RC-tracked
// fields are also cleaned up via emitRelease's recursive handling.
