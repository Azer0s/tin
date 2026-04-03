package codegen

// runtime.go - ARC (automatic reference counting) helpers, string builders,
// global string constants, and lazily-declared runtime/C functions.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Basic C runtime declarations

// ensurePrintf declares printf if not already done.
func (cg *CodeGen) ensurePrintf() *ir.Func {
	if cg.printfFn != nil {
		return cg.printfFn
	}

	cg.printfFn = cg.ensureExternDecl("printf", irtypes.I32,
		[]*ir.Param{ir.NewParam("format", irtypes.I8Ptr)}, true)

	return cg.printfFn
}

// ensureMalloc declares malloc if not already done.
func (cg *CodeGen) ensureMalloc() *ir.Func {
	if cg.mallocFn != nil {
		return cg.mallocFn
	}

	cg.mallocFn = cg.ensureExternDecl("malloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.mallocFn
}

// ensureFree declares free if not already done.
func (cg *CodeGen) ensureFree() *ir.Func {
	if cg.freeFn != nil {
		return cg.freeFn
	}

	cg.freeFn = cg.ensureExternDecl("free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.freeFn
}

// ARC helpers

// ensureRCAlloc lazily declares _tin_rc_alloc(size i64) i8*.
func (cg *CodeGen) ensureRCAlloc() *ir.Func {
	if cg.rcAllocFn != nil {
		return cg.rcAllocFn
	}

	cg.rcAllocFn = cg.ensureExternDecl("_tin_rc_alloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.rcAllocFn
}

// ensureRetain lazily declares _tin_retain(ptr i8*).
func (cg *CodeGen) ensureRetain() *ir.Func {
	if cg.retainFn != nil {
		return cg.retainFn
	}

	cg.retainFn = cg.ensureExternDecl("_tin_retain", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.retainFn
}

// ensureRelease lazily declares _tin_release(ptr i8*).
func (cg *CodeGen) ensureRelease() *ir.Func {
	if cg.releaseFn != nil {
		return cg.releaseFn
	}

	cg.releaseFn = cg.ensureExternDecl("_tin_release", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.releaseFn
}

// ensureReleaseFatElemArray lazily declares _tin_release_fat_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each fat-ptr element then frees.
func (cg *CodeGen) ensureReleaseFatElemArray() *ir.Func {
	if cg.releaseFatElemArrayFn != nil {
		return cg.releaseFatElemArrayFn
	}

	cg.releaseFatElemArrayFn = cg.ensureExternDecl("_tin_release_fat_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseFatElemArrayFn
}

// ensureReleaseAnyElemArray lazily declares _tin_release_any_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each `any` element then frees.
func (cg *CodeGen) ensureReleaseAnyElemArray() *ir.Func {
	if cg.releaseAnyElemArrayFn != nil {
		return cg.releaseAnyElemArrayFn
	}

	cg.releaseAnyElemArrayFn = cg.ensureExternDecl("_tin_release_any_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseAnyElemArrayFn
}

// ensureReleaseFnElemArray lazily declares _tin_release_fn_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each closure env then frees.
func (cg *CodeGen) ensureReleaseFnElemArray() *ir.Func {
	if cg.releaseFnElemArrayFn != nil {
		return cg.releaseFnElemArrayFn
	}

	cg.releaseFnElemArrayFn = cg.ensureExternDecl("_tin_release_fn_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseFnElemArrayFn
}

// ensureReleaseClosure lazily declares _tin_release_closure(env i8*).
// Decrements the closure env RC; when RC hits 0, calls the per-closure dtor
// (stored at env field 0) to release RC-tracked captures, then frees the block.
func (cg *CodeGen) ensureReleaseClosure() *ir.Func {
	if cg.releaseClosureFn != nil {
		return cg.releaseClosureFn
	}

	cg.releaseClosureFn = cg.ensureExternDecl("_tin_release_closure", irtypes.Void,
		[]*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}, false)

	return cg.releaseClosureFn
}

// ensureReleaseAny lazily declares _tin_release_any(tag i32, data i8*).
// For anyTagFn (5): also releases the closure env before freeing the data block.
// For all other tags: equivalent to _tin_release(data).
func (cg *CodeGen) ensureReleaseAny() *ir.Func {
	if cg.releaseAnyFn != nil {
		return cg.releaseAnyFn
	}

	cg.releaseAnyFn = cg.ensureExternDecl("_tin_release_any", irtypes.Void,
		[]*ir.Param{ir.NewParam("tag", irtypes.I32), ir.NewParam("data", irtypes.I8Ptr)}, false)

	return cg.releaseAnyFn
}

// isRCTrackedType returns true for types whose heap data is ARC-managed:
//   - strings      {i8*, i64}           - ptr is either immortal (-1 sentinel) or rc-alloc'd
//   - fat arrays   {T*,  i64}           - ptr is always rc-alloc'd
//   - any          {i32, i8*}           - ptr is rc-alloc'd (boxed value)
//   - fat fn ptrs  {fn(i8*,...)*, i8*}  - env (field 1) is rc-alloc'd (null for named-fn wrappers)
func isRCTrackedType(t irtypes.Type) bool {
	return isStringType(t) || isFatArrayPtr(t) || isAnyType(t) || isFatFnPtr(t)
}

// isCopyExpr returns true when an AST expression produces a reference to
// existing heap data rather than a fresh allocation.  The caller must
// retain the result before storing it in a new alloca.
func isCopyExpr(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.Identifier:
		// Named variable - its scope entry owns the RC reference.
		return true
	case *ast.FieldAccess:
		// Borrowing a field from a struct - the struct retains ownership.
		// Do not release after use; the struct's scope release handles it.
		return true
	case *ast.IndexExpr:
		// Borrowing an element from an array - the array retains ownership.
		return true
	case *ast.AsExpr:
		// Casting to `any` boxes the source value into a fresh _tin_rc_alloc.
		// The resulting any value is a NEW allocation, not a copy of an existing
		// reference.  Treating it as a copy would emit an extra retain (RC=2) that
		// the single scope-exit release cannot balance, causing a leak.
		if st, ok2 := n.Type.(*ast.SimpleType); ok2 && st.Name == "any" {
			return false
		}
		// Other casts (sv as string, n as i64, etc.) do not allocate new memory;
		// the underlying value still owns the RC reference.  Propagate through so
		// that `sv as string` (where sv is a named variable) is treated as a copy
		// and not released after a call.  Without this, the ARC release loop would
		// drop the RC after the callee returns AND scope-exit would drop it again,
		// double-freeing the underlying string.
		return isCopyExpr(n.Expr)
	case *ast.DerefExpr:
		// Dereferencing a pointer (*ptr) loads an ARC-managed value that is still
		// owned by the pointee.  The caller must retain to get an independent copy;
		// without a retain, both the new variable and the original heap block will
		// release the same inner RC pointers, causing a double-free.
		return true
	}

	return false
}

// isTemporaryProducer returns true when an expression is known to return a
// freshly heap-allocated RC-tracked value (rc = 1) that the caller owns.
// Used to release intermediates that are never stored in a named variable.
//
// Covered cases:
//   - CallExpr:           function may return a heap-allocated RC value
//   - BinExpr("++"):      string/array concat always creates a fresh allocation
//   - InterpolatedString: snprintf result is _tin_rc_alloc'd
//   - ArrayLit:           non-empty array literal is _tin_rc_alloc'd
func isTemporaryProducer(node ast.Node) bool {
	if _, ok := node.(*ast.CallExpr); ok {
		return true
	}

	if be, ok := node.(*ast.BinExpr); ok {
		return be.Op == "++"
	}

	if _, ok := node.(*ast.InterpolatedString); ok {
		return true
	}

	if al, ok := node.(*ast.ArrayLit); ok {
		return len(al.Elems) > 0 // empty [] has no heap block
	}

	return false
}

// elemNeedsRelease reports whether a scope variable with element type elemType
// requires any ARC or deinit processing at scope exit.  Returns false for
// primitive types (int, float, raw pointers) and for named structs with no RC
// fields, no deinit, and no nested structs - so emitScopeRelease can skip the
// load entirely rather than loading and then emitting nothing.
func (cg *CodeGen) elemNeedsRelease(elemType irtypes.Type) bool {
	switch elemType.(type) {
	case *irtypes.IntType, *irtypes.FloatType, *irtypes.PointerType:
		return false
	}
	// RC-tracked fat types (strings, arrays, closures, any): always need release.
	if isRCTrackedType(elemType) {
		return true
	}
	// Named struct: need release only if it has a deinit, RC fields, or nested structs.
	structName := cg.typeNameOf(elemType)
	if structName == "" {
		return true // anonymous struct (e.g. from external code) - be conservative
	}

	if cg.curScope != nil {
		if _, hasDeinit := cg.curScope.lookup(structName + "_deinit"); hasDeinit {
			return true
		}
	}

	for _, ft := range cg.structFieldLLVMTypes[structName] {
		if isRCTrackedType(ft) {
			return true
		}

		if _, isNested := ft.(*irtypes.StructType); isNested {
			return true // may contain RC fields deeper in
		}
	}

	return false
}

// extractRCDataPtr extracts the ARC heap data pointer (i8*) from a
// string, fat-array, or any value.  Returns nil for non-ARC types.
func (cg *CodeGen) extractRCDataPtr(block *ir.Block, val value.Value, t irtypes.Type) value.Value {
	if isStringType(t) {
		// String {i8*, i64}: field 0 is already i8* - no bitcast needed
		return block.NewExtractValue(val, 0)
	}

	if isFatArrayPtr(t) {
		// Fat array {T*, i64}: field 0 is T* - bitcast to i8* for _tin_retain/release
		dataPtr := block.NewExtractValue(val, 0)

		return block.NewBitCast(dataPtr, irtypes.I8Ptr)
	}

	if isAnyType(t) {
		// any {i32, i8*}: field 1 is the i8* data pointer
		return block.NewExtractValue(val, 1)
	}

	return nil
}

func (cg *CodeGen) walkRCStructFields(block *ir.Block, val value.Value, visit func(value.Value)) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	structName := cg.typeNameOf(val.Type())
	if structName == "" {
		return
	}

	fieldTypes := cg.structFieldLLVMTypes[structName]
	offset := 1 + cg.vtableOffset(structName)
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)

	for i, ft := range fieldTypes {
		_, isNestedStruct := ft.(*irtypes.StructType)
		if !isRCTrackedType(ft) && !isNestedStruct {
			continue
		}

		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(offset+i)))
		fieldVal := block.NewLoad(ft, gep)
		visit(fieldVal)
	}
}

// emitRetain emits a _tin_retain call for an ARC-tracked value.
// For named structs, it also retains any RC-tracked fields.
func (cg *CodeGen) emitRetain(block *ir.Block, val value.Value) {
	t := val.Type()
	// Closure fat pointer: retain the env field (i8*). _tin_retain handles null env.
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureRetain(), envField)

		return
	}

	rcPtr := cg.extractRCDataPtr(block, val, t)
	if rcPtr != nil {
		block.NewCall(cg.ensureRetain(), rcPtr)

		return
	}
	// Named struct: retain RC-tracked fields so copies are independent.
	cg.walkRCStructFields(block, val, func(fieldVal value.Value) {
		cg.emitRetain(block, fieldVal)
	})
}

// emitRelease emits a _tin_release call for an ARC-tracked value.
// For named structs it first calls deinit (if defined), then releases any
// RC-tracked fields and recurses into nested struct fields.
func (cg *CodeGen) emitRelease(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, false)
}

// emitReleaseNoDeinit is like emitRelease but suppresses the deinit call for
// the top-level value.  Used when releasing the `this` parameter of a deinit
// method itself to prevent infinite recursion.  Field releases still recurse
// normally (nested struct fields call their own deinit as usual).
func (cg *CodeGen) emitReleaseNoDeinit(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, true)
}

func (cg *CodeGen) emitReleaseInner(block *ir.Block, val value.Value, skipDeinit bool) {
	t := val.Type()
	// Closure fat pointer: release the env via _tin_release_closure (null-safe).
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureReleaseClosure(), envField)

		return
	}
	// any value: use tag-aware release that also handles closure envs inside.
	if isAnyType(t) {
		tag := block.NewExtractValue(val, 0)
		data := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureReleaseAny(), tag, data)

		return
	}
	// Fat array with RC-tracked element type: use a combined release that
	// decrements the outer RC and, only when it hits 0, releases each element
	// and frees the outer block.  This prevents double-free when the array is
	// shared (e.g. struct copy): the copy's release decrements RC without
	// touching elements; only the last owner's release triggers element cleanup.
	if isFatArrayPtr(t) {
		if st, ok := t.(*irtypes.StructType); ok {
			if pt, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
				elemType := pt.ElemType
				if isRCTrackedType(elemType) {
					dataPtr := block.NewExtractValue(val, 0)
					length := block.NewExtractValue(val, 1)

					dataPtrI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
					if isAnyType(elemType) {
						block.NewCall(cg.ensureReleaseAnyElemArray(), dataPtrI8, length)
					} else if isFatFnPtr(elemType) {
						// Array of closures: env is field 1 (offset 8); use dedicated fn.
						block.NewCall(cg.ensureReleaseFnElemArray(), dataPtrI8, length)
					} else {
						// string {i8*,i64} or nested fat array {T*,i64}: field 0 is RC ptr
						block.NewCall(cg.ensureReleaseFatElemArray(), dataPtrI8, length)
					}
					// Combined function handles outer block free - don't call _tin_release.
					return
				}
			}
		}
	}

	rcPtr := cg.extractRCDataPtr(block, val, t)
	if rcPtr != nil {
		block.NewCall(cg.ensureRelease(), rcPtr)

		return
	}
	// Named struct: call deinit (if defined) before releasing RC fields so
	// the user-supplied cleanup runs while fields are still valid.
	if !skipDeinit {
		structName := cg.typeNameOf(val.Type())
		if structName != "" && cg.curScope != nil {
			deinitName := structName + "_deinit"
			if entry, ok := cg.curScope.lookup(deinitName); ok {
				if fn, ok2 := entry.val.(*ir.Func); ok2 {
					args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
					block.NewCall(fn, args...)
				}
			}
			// Call chained trait deinit methods (for traits that also define fn deinit).
			for _, traitDeinitFn := range cg.traitChainedDeinits[structName] {
				args := cg.adaptArgs(block, []value.Value{val}, traitDeinitFn.Sig)
				block.NewCall(traitDeinitFn, args...)
			}
		}
	}
	// Release RC-tracked fields and recurse into nested struct fields.
	// Propagate skipDeinit so that parameter-copy teardown does not call deinit
	// on nested struct fields (the caller's emitRelease already handles that).
	cg.walkRCStructFields(block, val, func(fieldVal value.Value) {
		cg.emitReleaseInner(block, fieldVal, skipDeinit)
	})
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
func (cg *CodeGen) emitScopeRelease(block *ir.Block, s *scope) {
	if block == nil || block.Term != nil {
		return
	}

	for _, entry := range s.vars {
		if !entry.isAlloc || entry.noRelease {
			continue
		}

		ptrType, ok := entry.val.Type().(*irtypes.PointerType)
		if !ok {
			continue
		}
		// isHeapOwned: variable holds a _tin_rc_alloc'd pointer returned by a
		// heap-promoting callee.  Use chain release to free all RC blocks.
		if entry.isHeapOwned {
			heapPtr := block.NewLoad(ptrType.ElemType, entry.val)
			cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)

			continue
		}
		// Slice variables store the base allocation pointer separately so that
		// ARC release hits the real ARC header rather than an interior pointer.
		if entry.basePtr != nil {
			block.NewCall(cg.ensureRelease(), entry.basePtr)

			continue
		}

		elemType := ptrType.ElemType
		if !cg.elemNeedsRelease(elemType) {
			continue
		}

		loaded := block.NewLoad(elemType, entry.val)
		if entry.noDeinit {
			cg.emitReleaseNoDeinit(block, loaded)
		} else {
			cg.emitRelease(block, loaded)
		}
	}
}

// emitAllScopeReleases emits _tin_release for all ARC-tracked variables in
// the current scope chain.  skipName, if non-empty, skips that variable
// (used to transfer ownership of a return value to the caller).
func (cg *CodeGen) emitAllScopeReleases(block *ir.Block, skipName string) {
	if block == nil || block.Term != nil {
		return
	}

	s := cg.curScope
	for s != nil {
		for name, entry := range s.vars {
			if name == skipName || !entry.isAlloc || entry.isGlobal || entry.noRelease {
				continue
			}

			ptrType, ok := entry.val.Type().(*irtypes.PointerType)
			if !ok {
				continue
			}
			// isHeapOwned: chain release.
			if entry.isHeapOwned {
				heapPtr := block.NewLoad(ptrType.ElemType, entry.val)
				cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)

				continue
			}
			// Slice variables: release the base allocation pointer, not the fat-ptr.
			if entry.basePtr != nil {
				block.NewCall(cg.ensureRelease(), entry.basePtr)

				continue
			}

			elemType := ptrType.ElemType
			if !cg.elemNeedsRelease(elemType) {
				continue
			}

			loaded := block.NewLoad(elemType, entry.val)
			if entry.noDeinit {
				cg.emitReleaseNoDeinit(block, loaded)
			} else {
				cg.emitRelease(block, loaded)
			}
		}

		if s.isFunctionBoundary {
			break
		}

		s = s.parent
	}
}

func (cg *CodeGen) ensureMemcpy() *ir.Func {
	if cg.memcpyFn != nil {
		return cg.memcpyFn
	}
	// LLVM intrinsic: declare void @llvm.memcpy.p0i8.p0i8.i64(i8*, i8*, i64, i1)
	f := cg.mod.NewFunc("llvm.memcpy.p0i8.p0i8.i64", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1),
	)
	f.Blocks = nil
	cg.memcpyFn = f

	return f
}

// ensureAnyEq declares _tin_any_eq if not already done.
// Signature: i64 _tin_any_eq({i32, i8*} a, {i32, i8*} b)
func (cg *CodeGen) ensureAnyEq() *ir.Func {
	if cg.anyEqFn != nil {
		return cg.anyEqFn
	}

	anyT := anyFatPtrType()
	cg.anyEqFn = cg.ensureExternDecl("_tin_any_eq", irtypes.I64,
		[]*ir.Param{ir.NewParam("a", anyT), ir.NewParam("b", anyT)}, false)

	return cg.anyEqFn
}

// ensureStrcmp declares strcmp if not already done.
func (cg *CodeGen) ensureStrcmp() *ir.Func {
	if cg.strcmpFn != nil {
		return cg.strcmpFn
	}

	cg.strcmpFn = cg.ensureExternDecl("strcmp", irtypes.I32,
		[]*ir.Param{ir.NewParam("s1", irtypes.I8Ptr), ir.NewParam("s2", irtypes.I8Ptr)}, false)

	return cg.strcmpFn
}

// newGlobalString creates a private unnamed_addr constant for a string,
// returning a pointer to its first byte.  The global is wrapped in a
// { i64, [N x i8] } struct whose i64 field holds TIN_IMMORTAL_RC (-1) so
// that _tin_retain / _tin_release treat it as an immortal, never-freed block.
//
//goland:noinspection GoSnakeCaseUsage
func (cg *CodeGen) newGlobalString(s string) value.Value {
	data := []byte(s)
	data = append(data, 0) // null terminator
	arrType := irtypes.NewArray(uint64(len(data)), irtypes.I8)
	ca := constant.NewCharArray(data)

	// Wrap in { i64, [N x i8] } with immortal ARC header (rc = -1)
	immortalRC := constant.NewInt(irtypes.I64, -1)
	hdrStructType := irtypes.NewStruct(irtypes.I64, arrType)
	hdrConst := constant.NewStruct(hdrStructType, immortalRC, ca)

	g := cg.mod.NewGlobalDef(fmt.Sprintf("str.%d", cg.strCount), hdrConst)
	g.Immutable = true
	g.Linkage = enum.LinkagePrivate
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr
	cg.strCount++

	// GEP: { i64, [N x i8] }* -> [N x i8]* -> i8* (skipping the 8-byte ARC header)
	i32_0 := constant.NewInt(irtypes.I32, 0)
	i32_1 := constant.NewInt(irtypes.I32, 1)
	gep := constant.NewGetElementPtr(hdrStructType, g, i32_0, i32_1, i32_0)
	gep.InBounds = true

	return gep
}

// buildStringFatPtr creates a tin string fat-pointer {i8*, i64} from a literal string.
func (cg *CodeGen) buildStringFatPtr(block *ir.Block, s string) value.Value {
	ptr := cg.newGlobalString(s)
	length := constant.NewInt(irtypes.I64, int64(len(s)))
	fatPtrType := stringFatPtrType()
	alloca := block.NewAlloca(fatPtrType)
	// store ptr into field 0
	gep0 := block.NewGetElementPtr(fatPtrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(ptr, gep0)
	// store length into field 1
	gep1 := block.NewGetElementPtr(fatPtrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(length, gep1)

	return block.NewLoad(fatPtrType, alloca)
}

// extractStringPtr extracts the i8* data pointer from a tin string fat-ptr.
func (cg *CodeGen) extractStringPtr(block *ir.Block, fatPtr value.Value) value.Value {
	fatPtrType := stringFatPtrType()
	alloca := block.NewAlloca(fatPtrType)
	block.NewStore(fatPtr, alloca)
	gep := block.NewGetElementPtr(fatPtrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	raw := block.NewLoad(irtypes.I8Ptr, gep)
	// Null-safety: a zero-initialized string has data=null; treat as empty string "".
	nullPtr := constant.NewNull(irtypes.I8Ptr)
	emptyPtr := cg.newGlobalString("")
	isNull := block.NewICmp(enum.IPredEQ, raw, nullPtr)

	return block.NewSelect(isNull, emptyPtr, raw)
}

// extractStringLen extracts the i64 length from a tin string fat-ptr.
func (cg *CodeGen) extractStringLen(block *ir.Block, fatPtr value.Value) value.Value {
	fatPtrType := stringFatPtrType()
	alloca := block.NewAlloca(fatPtrType)
	block.NewStore(fatPtr, alloca)
	gep := block.NewGetElementPtr(fatPtrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

	return block.NewLoad(irtypes.I64, gep)
}

// panic builtin

// ensurePanicFn lazily declares the _tin_panic external function.
func (cg *CodeGen) ensurePanicFn() *ir.Func {
	if cg.tinPanicFn != nil {
		return cg.tinPanicFn
	}

	cg.tinPanicFn = cg.mod.NewFunc("_tin_panic", irtypes.Void,
		ir.NewParam("msg", irtypes.I8Ptr),
	)

	return cg.tinPanicFn
}

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable
// terminator is appended so the block is valid LLVM IR.
func (cg *CodeGen) genBuiltinPanic(block *ir.Block, msgNode ast.Node) (value.Value, error) {
	msg, err := cg.genExpr(block, msgNode)
	if err != nil {
		return nil, err
	}

	var msgPtr value.Value
	if isStringType(msg.Type()) {
		msgPtr = cg.extractStringPtr(block, msg)
	} else {
		msgPtr = block.NewBitCast(msg, irtypes.I8Ptr)
	}

	block.NewCall(cg.ensurePanicFn(), msgPtr)
	// _tin_panic normally calls exit(1) and never returns.  However, when a
	// deferred function calls recover(), _tin_panic returns instead of exiting.
	// We must emit a valid terminator so the IR block is well-formed.
	//
	// Inside a coroutine body we emit the proper coro completion path so that
	// the fiber is marked as done and the coro frame is cleaned up correctly.
	// (A bare `ret` in a presplit coroutine body bypasses llvm.coro.end and
	// leaves the frame in an undefined state when the destroy path is called.)
	// If a subsequent explicit `return` statement is present (e.g. `return 0`
	// after `panic(...)`), its genCoroReturn call will overwrite block.Term with
	// the correct br->coro.final, which is harmless.
	if cg.inCoroFn {
		cg.ensureFiberRuntime()
		// If _tin_panic returns (panic was caught by defer+recover in this coro),
		// complete with the defer-override value if a thunk set one, otherwise
		// the zero value of the declared return type.  Passing nil would leave
		// the fiber result as NULL, causing a null dereference in the outer awaiter.
		cg.emitCoroComplete(block, cg.recoverRetVal(block))
		cg.emitFinalSuspend(block, cg.curCoroFrame)
	} else {
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			block.NewRet(nil)
		} else {
			block.NewRet(cg.zeroValue(retType))
		}
	}

	return nil, nil
}

// ensureSliceSubslice lazily declares _tin_slice_subslice(TinSlice s, i64 start, i64 elem_size) -> TinSlice.
// TinSlice has the same layout as a fat array: { i8*, i64 }.
func (cg *CodeGen) ensureSliceSubslice() *ir.Func {
	if cg.sliceSubsliceFn != nil {
		return cg.sliceSubsliceFn
	}

	sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
	cg.sliceSubsliceFn = cg.mod.NewFunc("_tin_slice_subslice", sliceType,
		ir.NewParam("s", sliceType),
		ir.NewParam("start", irtypes.I64),
		ir.NewParam("elem_size", irtypes.I64),
	)

	return cg.sliceSubsliceFn
}

// ensureRecoverFn lazily declares the _tin_recover() -> TinString extern.
func (cg *CodeGen) ensureRecoverFn() *ir.Func {
	if cg.tinRecoverFn != nil {
		return cg.tinRecoverFn
	}

	cg.tinRecoverFn = cg.mod.NewFunc("_tin_recover", stringFatPtrType())

	return cg.tinRecoverFn
}

// genBuiltinRecover implements recover(): returns the panic message from a
// deferred function, or an empty string if not currently panicking.
func (cg *CodeGen) genBuiltinRecover(block *ir.Block) (value.Value, error) {
	return block.NewCall(cg.ensureRecoverFn()), nil
}

// ensureSnprintf lazily declares the snprintf external function.
// int snprintf(char* buf, size_t n, const char* format, ...)
func (cg *CodeGen) ensureSnprintf() *ir.Func {
	if cg.sprintfFn != nil {
		return cg.sprintfFn
	}

	cg.sprintfFn = cg.ensureExternDecl("snprintf", irtypes.I32,
		[]*ir.Param{ir.NewParam("buf", irtypes.I8Ptr), ir.NewParam("n", irtypes.I64), ir.NewParam("format", irtypes.I8Ptr)}, true)

	return cg.sprintfFn
}
