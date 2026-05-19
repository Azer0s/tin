package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) callPrintTrait(block *ir.Block, val value.Value) (value.Value, bool) {
	t := val.Type()
	// Case 1: concrete struct - look up structName_print directly.
	if structName := cg.typeNameOf(t); structName != "" {
		if e, ok := cg.curScope.lookup(structName + "_print"); ok {
			if fn, ok2 := e.val.(*ir.Func); ok2 {
				args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)

				return block.NewCall(fn, args...), true
			}
		}
	}
	// Case 1b: pointer to struct - load the struct and dispatch print.
	if pt, ok := t.(*irtypes.PointerType); ok {
		if structName := cg.typeNameOf(pt.ElemType); structName != "" {
			if e, ok2 := cg.curScope.lookup(structName + "_print"); ok2 {
				if fn, ok3 := e.val.(*ir.Func); ok3 {
					loaded := block.NewLoad(pt.ElemType, val)
					args := cg.adaptArgs(block, []value.Value{loaded}, fn.Sig)

					return block.NewCall(fn, args...), true
				}
			}
		}
	}
	// Case 2: print trait fat pointer - dispatch through vtable.
	if instKey, ok := cg.isTraitFatPtr(t); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}

		if baseTrait == "print" {
			strVal, err := cg.callTraitMethod(block, val, instKey, instKey, nil)
			if err == nil && strVal != nil {
				return strVal, true
			}
		}
	}

	return nil, false
}

// vtableOffset returns the number of vtable pointer fields prepended to the

// fieldIndex returns the LLVM field index for a named user field, accounting
// for the leading i32 type_id and vtable pointer fields at the front.
// Layout: [i32 type_id, vtable_0*, ..., user_field_0, ...]

// isStringType returns true if t is the tin string fat-pointer type {i8*, i64}.

// isAnyType returns true if t is the tin `any` fat-pointer type {i32, i8*}.

// boxToAny boxes val into an `any` fat-pointer {i32 type_id, i8* data}.
func (cg *CodeGen) boxToAny(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		val = constant.NewInt(irtypes.I64, 0)
	}
	// If already any, pass through.
	if isAnyType(val.Type()) {
		return val
	}

	anyType := anyFatPtrType()
	alloca := block.NewAlloca(anyType)

	t := val.Type()

	var (
		tag     int32
		dataPtr value.Value
	)

	// Use ARC-managed allocations for boxed data so typeof/release work.
	rcAlloc := cg.ensureRCAlloc()

	switch {
	case isAtomType(t):
		// Atom values: box as anyTagInt using the i32 hash code widened to i64.
		// This makes `ftype == 'i64` work via integer comparison in _tin_any_eq.
		tag = anyTagInt
		hashCode := cg.extractAtomCode(block, val) // i32
		i64Hash := block.NewSExt(hashCode, irtypes.I64)
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 8))
		iPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.I64))
		block.NewStore(i64Hash, iPtr)

		dataPtr = rawPtr
	case isStringType(t):
		tag = anyTagString
		sz := constant.NewInt(irtypes.I64, 24) // {i8*, i64 len, i64 cap} = 24 bytes
		rawPtr := block.NewCall(rcAlloc, sz)
		strPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, strPtr)
		// The any data block owns one reference to the inner string's
		// i8* ptr -- the matching release is in _tin_release_any
		// (tag=2 branch).  Without this retain, the string would be
		// freed by its original scope's release while the any block
		// still holds a (dangling) pointer to it; with the new release
		// path freeing the inner ptr, missing the retain here would
		// double-free or leak depending on caller shape.
		innerPtr := block.NewExtractValue(val, 0)
		block.NewCall(cg.ensureRetain(), innerPtr)

		dataPtr = rawPtr
	case t.Equal(irtypes.I1):
		tag = anyTagBool
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 1))
		boolPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.I1))
		block.NewStore(val, boolPtr)

		dataPtr = rawPtr
	case irtypes.IsFloat(t):
		tag = anyTagFloat

		var f64Val value.Value
		if t == irtypes.Double {
			f64Val = val
		} else {
			f64Val = block.NewFPExt(val, irtypes.Double)
		}

		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 8))
		fPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.Double))
		block.NewStore(f64Val, fPtr)

		dataPtr = rawPtr
	case irtypes.IsInt(t):
		tag = anyTagInt
		i64Val := cg.coerce(block, val, irtypes.I64)
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 8))
		iPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.I64))
		block.NewStore(i64Val, iPtr)

		dataPtr = rawPtr
	case isFatFnPtr(t):
		// Fat function pointer {sync*, colored*, coro*, i8* env}:
		// heap-copy the struct so the any can outlive its stack
		// alloca.  Use anyTagFn (5) for all fat fn ptrs so the
		// any-release path can detect closures and release their env.
		tag = anyTagFn

		sz := llvmTypeSize(t)
		if sz == 0 {
			sz = 32 // four pointers (3 fn slots + env)
		}

		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		fnPtrStore := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, fnPtrStore)
		// Retain env (slot 3) so the any data block independently owns a
		// reference to it.  _tin_retain is null-safe (handles null env
		// for wrapped named functions).  Reading any non-env slot here
		// would feed a code-segment pointer to _tin_retain, which reads
		// the RC header 16 bytes BEFORE that address -- silent corruption.
		envField := block.NewExtractValue(val, 3)
		block.NewCall(cg.ensureRetain(), envField)

		dataPtr = rawPtr
	case irtypes.IsPointer(t):
		// A pointer to a FuncType is a named/extern function reference; give
		// it the fn tag so typeof() returns 'fn(...) instead of 'ptr.
		if pt, ok2 := t.(*irtypes.PointerType); ok2 {
			if fnType, isFnType := pt.ElemType.(*irtypes.FuncType); isFnType {
				tag = cg.ensureFnTypeID(fnSigName(fnType, false))
			} else if innerSt, ok3 := pt.ElemType.(*irtypes.StructType); ok3 && innerSt.Name() != "" {
				// Pointer-to-named-struct: route through the struct's
				// type_id whenever boxing is safe -- i.e. when
				// emitAnyDispatchRegistrations will register a per-
				// type-id helper for this struct (#no_copy wrappers
				// or deinit-only-with-primitive-fields). For other
				// shapes (RC-tracked field content, ADTs, etc.) the
				// dispatch is intentionally skipped because boxing
				// doesn't retain inner field RCs and re-releasing
				// would double-free.
				name := innerSt.Name()
				id, hasID := cg.structTypeIDs[name]

				if hasID && cg.structEligibleForAnyDispatch(name, innerSt) {
					tag = id

					ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
					block.NewCall(cg.ensureRetain(), ptrI8)
				} else {
					tag = anyTagPtr
				}
			} else {
				tag = anyTagPtr
			}
		} else {
			tag = anyTagPtr
		}

		dataPtr = block.NewBitCast(val, irtypes.I8Ptr)
	case isVectorType(t):
		// SIMD vector: heap-allocate the vector value (16-byte aligned due to
		// the padded TinRCHdr, so 128-bit SIMD is safe).
		canonName := llvmTypeName(t) // e.g. "f32x4", "i8x16"
		if _, exists := cg.structTypeIDs[canonName]; !exists {
			cg.structTypeIDs[canonName] = cg.nextTypeID
			cg.nextTypeID++
		}

		tag = cg.structTypeIDs[canonName]
		sz, _ := llvmTypeSizeAlign(t)
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		vPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, vPtr)

		dataPtr = rawPtr
	default:
		// Named struct or data type: heap-allocate so the any can escape.
		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			if id, ok2 := cg.structTypeIDs[st.Name()]; ok2 {
				tag = id
			} else if id, ok2 := cg.unionTypeIDs[st.Name()]; ok2 {
				tag = id
			} else {
				tag = anyTagPtr // unknown named type - treat as opaque pointer
			}
		} else {
			tag = anyTagInt
		}

		sz := llvmTypeSize(t)
		if sz == 0 {
			sz = 8
		}

		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		vPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, vPtr)

		dataPtr = rawPtr
	}

	// Layout: {i32 type_id, i8* data}
	tagGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(tag)), tagGep)

	ptrGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(dataPtr, ptrGep)

	return block.NewLoad(anyType, alloca)
}

// genEchoAny emits a runtime type-dispatch printf for an `any` value.
func (cg *CodeGen) genEchoAny(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	anyType := anyFatPtrType()

	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(val, anyAlloca)
	// Layout: {i32 type_id, i8* data}
	tagGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tag := block.NewLoad(irtypes.I32, tagGep)
	dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)

	f := cg.curFn
	id := cg.labelCount
	cg.labelCount++
	intBlock := f.NewBlock(fmt.Sprintf("any.int.%d", id))
	floatBlock := f.NewBlock(fmt.Sprintf("any.float.%d", id))
	strBlock := f.NewBlock(fmt.Sprintf("any.str.%d", id))
	boolBlock := f.NewBlock(fmt.Sprintf("any.bool.%d", id))
	ptrBlock := f.NewBlock(fmt.Sprintf("any.ptr.%d", id))
	doneBlock := f.NewBlock(fmt.Sprintf("any.done.%d", id))

	block.NewSwitch(tag, ptrBlock,
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagInt)), intBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagFloat)), floatBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagString)), strBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagBool)), boolBlock),
	)

	// int branch
	i64Ptr := intBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.I64))
	ival := intBlock.NewLoad(irtypes.I64, i64Ptr)
	intBlock.NewCall(printf, cg.newGlobalString("%lld\n"), ival)
	intBlock.NewBr(doneBlock)

	// float branch
	f64Ptr := floatBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.Double))
	fval := floatBlock.NewLoad(irtypes.Double, f64Ptr)
	floatBlock.NewCall(printf, cg.newGlobalString("%g\n"), fval)
	floatBlock.NewBr(doneBlock)

	// string branch
	strFatType := stringFatPtrType()
	strFatPtrPtr := strBlock.NewBitCast(dataPtr, irtypes.NewPointer(strFatType))
	strFatVal := strBlock.NewLoad(strFatType, strFatPtrPtr)
	strDataPtr := cg.extractStringPtr(strBlock, strFatVal)
	strBlock.NewCall(printf, cg.newGlobalString("%s\n"), strDataPtr)
	strBlock.NewBr(doneBlock)

	// bool branch
	boolPtr := boolBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.I1))
	bval := boolBlock.NewLoad(irtypes.I1, boolPtr)
	bval32 := boolBlock.NewZExt(bval, irtypes.I32)
	boolBlock.NewCall(printf, cg.newGlobalString("%d\n"), bval32)
	boolBlock.NewBr(doneBlock)

	// ptr branch (default)
	ptrBlock.NewCall(printf, cg.newGlobalString("%p\n"), dataPtr)
	ptrBlock.NewBr(doneBlock)

	return doneBlock, nil
}

// toBool converts a value to i1.
