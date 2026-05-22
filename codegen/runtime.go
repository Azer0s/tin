package codegen

// runtime.go - ARC (automatic reference counting) helpers, string builders,
// global string constants, and lazily-declared runtime/C functions.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Basic C runtime declarations

// ensurePrintf declares printf if not already done.

func (cg *CodeGen) emitRetain(block *ir.Block, val value.Value) {
	cg.emittingARC = true

	defer func() { cg.emittingARC = false }()

	t := val.Type()
	// Closure fat pointer: retain the env field (i8*, slot 3). _tin_retain handles null env.
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 3)
		block.NewCall(cg.ensureRetain(), envField)

		return
	}
	// Primitive *T: route through the arena-aware retain.  The runtime
	// short-circuits on foreign pointers (outside the Tin arena) and on
	// interior pointers (header-magic mismatch), so this is safe to
	// emit unconditionally for any *intT / *floatT / *void.
	if isPrimitivePtr(t) {
		ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
		block.NewCall(cg.ensureRetainPtr(), ptrI8)

		return
	}

	rcPtr := cg.extractRCDataPtr(block, val, t)
	if rcPtr != nil {
		block.NewCall(cg.ensureRetain(), rcPtr)

		return
	}
	// ADT value: tag-dispatched retain walks the active variant's payload.
	// The outer struct's declared fields {i32, i8, [N x i8]} hide the real
	// payload layout from walkRCStructFields, so we emit a dispatch here.
	if cg.isDataType(t) {
		cg.emitDataValueRetain(block, val)

		return
	}

	// cLayoutStruct value: bump the rc-block that c_data_ptr borrows into.
	// Heap-bound increments; stack-bound (immortal sentinel) is a no-op.
	if structName := cg.typeNameOf(t); structName != "" && cg.cLayoutStructs[structName] {
		cg.emitCLayoutStructRetain(block, val, structName)

		return
	}
	// Named struct: retain RC-tracked fields so copies are independent.
	// Use emitStructFieldRetain for each field to also handle *TinStruct pointer fields.
	cg.walkRCStructFieldsEx(block, val,
		func(fieldVal value.Value) {
			cg.emitStructFieldRetain(block, fieldVal)
		},
		func(fieldPtr value.Value, at *irtypes.ArrayType) {
			cg.emitFixedArrayRetain(block, fieldPtr, at)
		})
}

// emitStructFieldRetain retains a single struct field value.
// Unlike emitRetain (which is also called for function parameter pointers), this
// function is only called for fields loaded from a struct value being copied.
// It explicitly handles owning *TinStruct pointer fields AND owning *Trait
// iface fields that emitRetain skips: both are RC-managed heap blocks but
// Tin's calling convention treats their bare pointer form as a borrow at
// function entry.  When the value sits inside a struct that's being copied
// (struct param entry retain, struct lit copy, etc.), the field IS owning
// and must be retained so that scope-exit releases stay balanced.
func (cg *CodeGen) emitStructFieldRetain(block *ir.Block, fieldVal value.Value) {
	t := fieldVal.Type()
	// Owning pointer to a known Tin struct OR a trait fat-ptr iface struct:
	// retain via _tin_retain.  Iface structs live in cg.traitFatPtrTypes
	// (not cg.structTypes), so detect them via shape.
	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			isTinStruct := cg.structTypeFor(CanonKey(innerSt.Name())) != nil
			if isTinStruct || isTraitFatPtrShape(innerSt) {
				ptrI8 := block.NewBitCast(fieldVal, irtypes.I8Ptr)
				block.NewCall(cg.ensureRetain(), ptrI8)

				return
			}
		}
	}

	cg.emitRetain(block, fieldVal)
}

// emitRelease emits a _tin_release call for an ARC-tracked value.
// For named structs it first calls deinit (if defined), then releases any
// RC-tracked fields and recurses into nested struct fields.
func (cg *CodeGen) emitRelease(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, false)
}

// emitDiscardedValueRelease releases a value that the user dropped via
// `let _ = expr` (or `discard expr`).  Routes through the right release
// helper based on the value's shape: RC-tracked fat types release via
// _tin_release; ADTs walk their active variant; structs walk their RC
// and nested fields.  Skips raw pointers / primitives that can't carry
// an owning reference.
func (cg *CodeGen) emitDiscardedValueRelease(block *ir.Block, val value.Value, astArg ast.Node) {
	if val == nil || block == nil || block.Term != nil {
		return
	}

	t := val.Type()
	// String / array / any / closure / trait fat-ptr / iface fat-ptr:
	// the value owns rc=1 of its outer block.  Release matches the
	// existing logic for temp args in callGenericFromMap.
	if isRCTrackedType(t) {
		if astArg != nil && isCopyExpr(astArg) {
			return
		}

		cg.emitRelease(block, val)

		return
	}
	// ADT by value: tag-dispatched release walks the active variant.
	if cg.isDataType(t) {
		cg.emitDataValueRelease(block, val)

		return
	}
	// Named struct by value: walk RC fields + nested struct fields.
	// Skip when the source expression is a borrow shape (Identifier
	// etc.) - the original owner releases at its own scope exit.
	if astArg != nil && isCopyExpr(astArg) {
		return
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			if cg.isDataType(innerSt) {
				relFn := cg.ensureDataPtrReleaseFn(innerSt.Name(), innerSt)
				if relFn != nil {
					block.NewCall(relFn, val)
				}

				return
			}

			relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
			block.NewCall(relFn, val)

			return
		}
	}

	if _, ok := t.(*irtypes.StructType); ok && cg.elemNeedsRelease(t) {
		cg.emitRelease(block, val)
	}
}

// emitReleaseNoDeinit is like emitRelease but suppresses the deinit call for
// the top-level value.  Used when releasing the `this` parameter of a deinit
// method itself to prevent infinite recursion.  Field releases still recurse
// normally (nested struct fields call their own deinit as usual).
func (cg *CodeGen) emitReleaseNoDeinit(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, true)
}

func (cg *CodeGen) emitReleaseInner(block *ir.Block, val value.Value, skipDeinit bool) {
	cg.emittingARC = true

	defer func() { cg.emittingARC = false }()

	t := val.Type()
	// Owning pointer to a known Tin struct: delegate to the null-safe per-struct
	// release helper so that its RC-tracked and nested-struct-pointer fields are
	// recursively released before the block itself is freed.
	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			if cg.isDataType(innerSt) {
				relFn := cg.ensureDataPtrReleaseFn(innerSt.Name(), innerSt)
				if relFn != nil {
					block.NewCall(relFn, val)

					return
				}
			}

			relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
			block.NewCall(relFn, val)

			return
		}
	}
	// Closure fat pointer: release the env (slot 3) via _tin_release_closure (null-safe).
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 3)
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
				// Dispatch to the most specific release function for the element type.
				// All combined functions handle the outer buffer free - don't also
				// call _tin_release after returning.
				dataPtr := block.NewExtractValue(val, 0)
				length := block.NewExtractValue(val, 1)
				dataPtrI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)

				if isAnyType(elemType) {
					// [any]: tag-aware release handles closure envs
					block.NewCall(cg.ensureReleaseAnyElemArray(), dataPtrI8, length)

					return
				}

				if isFatFnPtr(elemType) {
					// [fn]: closure env is slot 3 (offset 24) of {coro,colored,sync,env}
					block.NewCall(cg.ensureReleaseFnElemArray(), dataPtrI8, length)

					return
				}

				if isStringType(elemType) {
					// [string]: string data pointer is at field 0 - optimized path
					block.NewCall(cg.ensureReleaseFatElemArray(), dataPtrI8, length)

					return
				}

				if pt2, isPtr := elemType.(*irtypes.PointerType); isPtr {
					// [*T]: check if the inner type T has RC fields that need
					// deep release (load T, release its fields, free the block).
					if cg.elemNeedsRelease(pt2.ElemType) {
						cg.emitGenericFatArrayRelease(block, val, elemType)

						return
					}

					// T has no RC fields: raw pointer release suffices.
					block.NewCall(cg.ensureReleasePtrElemArray(), dataPtrI8, length)

					return
				}

				if cg.elemNeedsRelease(elemType) {
					// [[T]] nested arrays or structs with RC fields: per-element helper
					// correctly recurses into inner element types at any depth.
					cg.emitGenericFatArrayRelease(block, val, elemType)

					return
				}
			}
		}
	}

	// Value-form trait fat-ptr: dispatch via the vtable's data-release thunk
	// (last slot) on the data ptr so the underlying concrete struct's
	// per-type release_ptr walks its RC fields before the block is freed.
	// A raw _tin_release here would decrement the iface block's rc and
	// free it without releasing inner heap-allocated fields like a
	// StringErr's heap-built `msg` from `errors::wrap`.
	if st, ok := t.(*irtypes.StructType); ok && isTraitFatPtrShape(st) {
		dataField := block.NewExtractValue(val, 0)
		vtableField := block.NewExtractValue(val, 1)

		vtablePtrType, ok2 := st.Fields[1].(*irtypes.PointerType)
		if ok2 {
			if vtableSt, ok3 := vtablePtrType.ElemType.(*irtypes.StructType); ok3 && len(vtableSt.Fields) > 0 {
				lastIdx := len(vtableSt.Fields) - 1
				lastFieldType := vtableSt.Fields[lastIdx]

				if lastPt, ok4 := lastFieldType.(*irtypes.PointerType); ok4 {
					if lastFnType, ok5 := lastPt.ElemType.(*irtypes.FuncType); ok5 &&
						len(lastFnType.Params) == 1 &&
						lastFnType.Params[0].Equal(irtypes.I8Ptr) &&
						irtypes.IsVoid(lastFnType.RetType) {
						releaseFnSlot := block.NewGetElementPtr(vtableSt, vtableField,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(lastIdx)))
						releaseFn := block.NewLoad(lastFieldType, releaseFnSlot)
						block.NewCall(releaseFn, dataField)

						return
					}
				}
			}
		}
	}

	// Primitive *T: arena-aware release, mirror of the retain side.
	if isPrimitivePtr(t) {
		ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
		block.NewCall(cg.ensureReleasePtr(), ptrI8)

		return
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
					// Guard: the deinit's `this` param type must match
					// the value being released. Without this we silently
					// fall into a function that bit-reinterprets `val`
					// as a different struct (a real bug seen with
					// Atomic[t]'s dual-impl deinit when a field load
					// has type T but the struct has been monomorphized
					// to Atomic__T and the static lookup picks the
					// outer deinit).
					if len(fn.Sig.Params) > 0 && fn.Sig.Params[0].Equal(val.Type()) {
						args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
						block.NewCall(fn, args...)
					}
				}
			}
			// Call chained trait deinit methods (for traits that also define fn deinit).
			for _, traitDeinitFn := range cg.traitChainedDeinits[structName] {
				// Suppress coerceToTrait's deferred scope-exit release;
				// we emit a tighter release immediately after the call.
				prevSuppress := cg.suppressIfaceScopeRelease
				cg.suppressIfaceScopeRelease = true

				args := cg.adaptArgs(block, []value.Value{val}, traitDeinitFn.Sig)
				cg.suppressIfaceScopeRelease = prevSuppress

				block.NewCall(traitDeinitFn, args...)
				// Release iface temporaries adaptArgs constructed via
				// coerceToTrait; see twin comment in genStructLit.
				for _, a := range args {
					if isTraitFatPtrShape(a.Type()) {
						dataField := block.NewExtractValue(a, 0)
						block.NewCall(cg.ensureRelease(), dataField)
					}
				}
			}
		}
	}
	// ADT value: dispatch on tag and release the active variant's owning
	// payload fields. The outer struct holds only {i32, i8, [N x i8]} so the
	// generic walkRCStructFields below can't see inside the payload.
	if cg.isDataType(t) {
		cg.emitDataValueRelease(block, val)

		return
	}

	// cLayoutStruct value: release the wrapper + native rc-block.  Heap-bound
	// (extern wrapper return) frees the block; stack-bound (struct literal)
	// reads the immortal-RC sentinel written by genCLayoutStructLit and skips.
	if structName := cg.typeNameOf(t); structName != "" && cg.cLayoutStructs[structName] {
		cg.emitCLayoutStructRelease(block, val, structName)

		return
	}

	// Release RC-tracked fields and recurse into nested struct fields.
	// Propagate skipDeinit so that parameter-copy teardown does not call deinit
	// on nested struct fields (the caller's emitRelease already handles that).
	cg.walkRCStructFieldsEx(block, val,
		func(fieldVal value.Value) {
			cg.emitReleaseInner(block, fieldVal, skipDeinit)
		},
		func(fieldPtr value.Value, at *irtypes.ArrayType) {
			cg.emitFixedArrayRelease(block, fieldPtr, at)
		})
}

// emitCLayoutStructRelease releases the rc-block backing a cLayoutStruct
// value.  The wrapper's c_data_ptr points to the native portion which sits
// one wrapper-element past the rc-base (whether heap-allocated via
// _tin_rc_alloc or stack-allocated as part of genCLayoutStructLit's
// composite layout with an immortal-RC sentinel).  Step back one wrapper
// element (LLVM GEP -1) and call _tin_release; the sentinel makes the
// stack-bound case a safe no-op.
func (cg *CodeGen) emitCLayoutStructRelease(block *ir.Block, val value.Value, structName string) {
	tinSt := cg.structTypeFor(CanonKey(structName))
	if tinSt == nil {
		panic(fmt.Sprintf("emitCLayoutStructRelease: no IR struct for %q", structName))
	}

	cDataIdx := cg.cDataPtrIndex(structName)
	if cDataIdx < 0 {
		panic(fmt.Sprintf("emitCLayoutStructRelease: %q has no c_data_ptr field", structName))
	}

	cDataPtr := block.NewExtractValue(val, uint64(cDataIdx))
	cDataAsSt := block.NewBitCast(cDataPtr, irtypes.NewPointer(tinSt))
	rcBase := block.NewGetElementPtr(tinSt, cDataAsSt,
		constant.NewInt(irtypes.I64, -1))
	rcBaseI8 := block.NewBitCast(rcBase, irtypes.I8Ptr)
	// Flag in bit 0 of the flags field tells _tin_release_clayout to
	// no-op for borrowed wrappers (pointer-extern returns where
	// c_data_ptr lives outside the rc-block).
	flagsVal := block.NewExtractValue(val, uint64(cg.clayoutFlagsIndex(structName)))
	block.NewCall(cg.ensureCLayoutRelease(), rcBaseI8, flagsVal)
}

// emitCLayoutStructRetain bumps the rc-block for a cLayoutStruct value
// being copied.  Mirror of emitCLayoutStructRelease: heap-bound bumps,
// stack-bound is a no-op via the immortal sentinel.
func (cg *CodeGen) emitCLayoutStructRetain(block *ir.Block, val value.Value, structName string) {
	tinSt := cg.structTypeFor(CanonKey(structName))
	if tinSt == nil {
		panic(fmt.Sprintf("emitCLayoutStructRetain: no IR struct for %q", structName))
	}

	cDataIdx := cg.cDataPtrIndex(structName)
	if cDataIdx < 0 {
		panic(fmt.Sprintf("emitCLayoutStructRetain: %q has no c_data_ptr field", structName))
	}

	cDataPtr := block.NewExtractValue(val, uint64(cDataIdx))
	cDataAsSt := block.NewBitCast(cDataPtr, irtypes.NewPointer(tinSt))
	rcBase := block.NewGetElementPtr(tinSt, cDataAsSt,
		constant.NewInt(irtypes.I64, -1))
	rcBaseI8 := block.NewBitCast(rcBase, irtypes.I8Ptr)
	// Flag in bit 0 of the flags field tells _tin_retain_clayout to
	// no-op for borrowed wrappers (pointer-extern returns where
	// c_data_ptr lives outside the rc-block).
	flagsVal := block.NewExtractValue(val, uint64(cg.clayoutFlagsIndex(structName)))
	block.NewCall(cg.ensureCLayoutRetain(), rcBaseI8, flagsVal)
}

// ensureStructPtrReleaseFn lazily creates (or returns a cached) null-safe pointer
// release function for the named Tin struct:
//
//	define void @{structName}__release_ptr({struct}* %ptr) {
//	entry:
//	  %is_null = icmp eq {struct}* %ptr, null
//	  br i1 %is_null, label %exit, label %do_release
//	do_release:
//	  %val = load {struct}, {struct}* %ptr
//	  ; release RC-tracked fields
//	  %ptr_i8 = bitcast {struct}* %ptr to i8*
//	  call void @_tin_release(i8* %ptr_i8)
//	  br label %exit
//	exit:
//	  ret void
//	}
//
// This is used by emitScopeRelease / emitAllScopeReleases when a scope variable
// has type *T (pointer to a Tin struct) so that a safe release is emitted without
// splitting the caller's basic block.
