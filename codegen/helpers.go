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

// Helper utilities

// closureCapture describes a variable captured from the enclosing scope.
type closureCapture struct {
	name   string
	val    value.Value
	llvmTy irtypes.Type
	byRef  bool // true: store the alloca pointer; false: store the loaded value
}

// closureCtx saves the mutable per-function state so it can be restored after
// emitting a nested closure / thunk function.
type closureCtx struct {
	fn                *ir.Func
	scope             *scope
	curBlock          *ir.Block
	deferFnI8s        []value.Value
	deferFrames       []value.Value
	deferEnvs         []value.Value
	deferRetSlotParam value.Value
	fnDeferRetAlloca  value.Value
	deferThunkRetType irtypes.Type
	inCoroFn          bool
}

// pushClosureCtx saves the current function context, switches cg to f, and
// roots the new scope at the module-level (global) scope.
func (cg *CodeGen) pushClosureCtx(f *ir.Func) closureCtx {
	prev := closureCtx{cg.curFn, cg.curScope, cg.curBlock, cg.pendingDeferFnI8s, cg.pendingDeferFrames, cg.pendingDeferEnvs, cg.curDeferRetSlotParam, cg.curFnDeferRetAlloca, cg.curDeferThunkRetType, cg.inCoroFn}
	cg.curFn = f
	cg.curBlock = nil
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.curDeferRetSlotParam = nil
	cg.curFnDeferRetAlloca = nil
	cg.curDeferThunkRetType = nil
	// Thunks and closures are plain functions, not coroutines. Resetting
	// inCoroFn prevents emitTerminator from emitting coro completion code
	// (which would create cross-function SSA references) inside the thunk.
	cg.inCoroFn = false

	global := prev.scope
	for global.parent != nil {
		global = global.parent
	}

	cg.curScope = newScope(global)

	return prev
}

// popClosureCtx restores the function context saved by pushClosureCtx.
func (cg *CodeGen) popClosureCtx(prev closureCtx) {
	cg.curFn = prev.fn
	cg.curScope = prev.scope
	cg.curBlock = prev.curBlock
	cg.pendingDeferFnI8s = prev.deferFnI8s
	cg.pendingDeferFrames = prev.deferFrames
	cg.pendingDeferEnvs = prev.deferEnvs
	cg.curDeferRetSlotParam = prev.deferRetSlotParam
	cg.curFnDeferRetAlloca = prev.fnDeferRetAlloca
	cg.curDeferThunkRetType = prev.deferThunkRetType
	cg.inCoroFn = prev.inCoroFn
}

// buildEnv heap-allocates an env struct for the given captures and stores each
// captured value into it. Returns the i8* pointer to the env and the struct
// type (nil struct type and null pointer when there are no captures).
func (cg *CodeGen) buildEnv(block *ir.Block, captures []closureCapture) (value.Value, *irtypes.StructType) {
	if len(captures) == 0 {
		return constant.NewNull(irtypes.I8Ptr), nil
	}

	fields := make([]irtypes.Type, len(captures))
	for i, c := range captures {
		fields[i] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)
	// sizeof(*envStructType) via GEP trick: null + 1 element then ptrtoint.
	nullPtr := constant.NewNull(irtypes.NewPointer(envStructType))
	oneGEP := block.NewGetElementPtr(envStructType, nullPtr, constant.NewInt(irtypes.I32, 1))
	envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
	envI8 := block.NewCall(cg.ensureMalloc(), envSize)

	envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))
	for i, c := range captures {
		gep := block.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(c.val, gep)
	}

	return envI8, envStructType
}

// posStr formats a source position as "filename:line:col".
// If node is non-nil its position is used; otherwise cg.currentPos is used.
// Returns just the filename when no line info is available.
func (cg *CodeGen) posStr(node ast.Node) string {
	var p ast.Pos
	if node != nil {
		p = node.Pos()
	}

	if p.Line == 0 {
		p = cg.currentPos
	}

	if p.Line == 0 {
		return cg.filename
	}

	return fmt.Sprintf("%s:%d:%d", cg.filename, p.Line, p.Col)
}

// nodeErr returns an error prefixed with the source location of node.
func (cg *CodeGen) nodeErr(node ast.Node, format string, args ...interface{}) error {
	return fmt.Errorf("%s: %s", cg.posStr(node), fmt.Sprintf(format, args...))
}

// displayStructName returns the user-facing name for a struct canonical key.
// Package-qualified structs like "http__Client" are presented as "http::Client".
// Bare names (user-level structs) are returned unchanged.
func (cg *CodeGen) displayStructName(canonicalKey string) string {
	if dn, ok := cg.structDisplayNames[canonicalKey]; ok {
		return dn
	}

	return canonicalKey
}

// tinTypeDisplay returns a user-facing description of an LLVM type using
// Tin syntax: `decimal::Value` rather than the internal `%decimal__Value`,
// `*Box` rather than `%Box*`, `[decimal::Value]` for fat arrays, and so
// on. Used in diagnostic strings so errors don't leak the package-mangling
// scheme back at the user.
func (cg *CodeGen) tinTypeDisplay(t irtypes.Type) string {
	if t == nil {
		return "void"
	}

	switch tt := t.(type) {
	case *irtypes.PointerType:
		return "*" + cg.tinTypeDisplay(tt.ElemType)
	case *irtypes.ArrayType:
		return "[" + cg.tinTypeDisplay(tt.ElemType) + "]"
	case *irtypes.VectorType:
		return cg.tinTypeDisplay(tt.ElemType) + "x" + fmt.Sprintf("%d", tt.Len)
	case *irtypes.StructType:
		// Anonymous structs that the compiler uses for fat pointers: surface
		// them as the user-facing equivalent.
		if tt.Name() == "" {
			if isStringType(tt) {
				return "string"
			}

			if isAnyType(tt) {
				return "any"
			}

			if isFatArrayPtr(tt) && len(tt.Fields) == 2 {
				if pt, ok := tt.Fields[0].(*irtypes.PointerType); ok {
					return "[" + cg.tinTypeDisplay(pt.ElemType) + "]"
				}
			}
		}
	}

	name := llvmTypeName(t)
	if dn, ok := cg.structDisplayNames[name]; ok {
		return dn
	}

	return name
}

// buildClosureEnv heap-allocates an RC-managed env struct for lambda closure captures.
// Layout: { i8* dtor_fn_ptr, capture_0, capture_1, ... } (dtor at field 0).
// All RC-tracked captures are retained so the env independently owns them.
// dtorFn may be nil if there are no RC-tracked captures (dtor slot is set to null).
func (cg *CodeGen) buildClosureEnv(block *ir.Block, captures []closureCapture, dtorFn *ir.Func) (value.Value, *irtypes.StructType) {
	if len(captures) == 0 {
		return constant.NewNull(irtypes.I8Ptr), nil
	}
	// Field 0: i8* dtor; fields 1..N: captures.
	fields := make([]irtypes.Type, len(captures)+1)

	fields[0] = irtypes.I8Ptr
	for i, c := range captures {
		fields[i+1] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)

	// Compute size via GEP trick.
	nullPtr := constant.NewNull(irtypes.NewPointer(envStructType))
	oneGEP := block.NewGetElementPtr(envStructType, nullPtr, constant.NewInt(irtypes.I32, 1))
	envSize := block.NewPtrToInt(oneGEP, irtypes.I64)

	// Use _tin_rc_alloc so the env lifetime is reference-counted.
	envI8 := block.NewCall(cg.ensureRCAlloc(), envSize)
	envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))

	// Store dtor pointer as field 0.
	dtorGep := block.NewGetElementPtr(envStructType, envTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	if dtorFn != nil {
		dtorI8 := block.NewBitCast(dtorFn, irtypes.I8Ptr)
		block.NewStore(dtorI8, dtorGep)
	} else {
		block.NewStore(constant.NewNull(irtypes.I8Ptr), dtorGep)
	}

	// Store captures at fields 1..N and retain RC-tracked ones.
	for i, c := range captures {
		gep := block.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))
		block.NewStore(c.val, gep)

		if isRCTrackedType(c.llvmTy) {
			cg.emitRetain(block, c.val)
		}
	}

	return envI8, envStructType
}

// unpackClosureEnv unpacks captures from a closure env (built by buildClosureEnv).
// GEP indices are offset by 1 to skip the dtor pointer at field 0.
// Uses env field GEPs directly (useEnvDirect=true semantics): mutations to
// captured variables persist across closure calls.
func (cg *CodeGen) unpackClosureEnv(entry *ir.Block, f *ir.Func, envStructType *irtypes.StructType, captures []closureCapture) {
	if len(captures) == 0 || envStructType == nil {
		return
	}

	envRaw := f.Params[0]

	envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
	for i, c := range captures {
		// Field index = i+1 (offset by 1 to skip dtor slot at field 0).
		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))
		// Retain RC-tracked captures so each closure call holds its own reference.
		if isRCTrackedType(c.llvmTy) {
			loaded := entry.NewLoad(c.llvmTy, gep)
			cg.emitRetain(entry, loaded)
		}
		// Use env GEP directly so mutations persist across calls (counter closures, etc.)
		cg.curScope.set(c.name, &scopeEntry{val: gep, isAlloc: true, noDeinit: true})
	}
}

// unpackEnv unpacks captured values from the env struct into the current scope.
// byRef captures load the stored alloca pointer; non-byRef captures use the env
// field GEP directly (useEnvDirect=true, for lambdas whose env persists across
// calls) or copy the value to a local alloca (useEnvDirect=false, for coro/defer
// thunks that free the env after unpacking).
func (cg *CodeGen) unpackEnv(entry *ir.Block, f *ir.Func, envStructType *irtypes.StructType, captures []closureCapture, useEnvDirect bool) {
	if len(captures) == 0 || envStructType == nil {
		return
	}

	envRaw := f.Params[0]

	envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
	for i, c := range captures {
		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i)))
		if c.byRef {
			allocaPtr := entry.NewLoad(c.llvmTy, gep)
			// noRelease: the captured variable is owned by the outer scope; the
			// defer thunk must not release it at scope exit.  Only the outer scope
			// (which allocated the variable) is responsible for releasing it.
			cg.curScope.set(c.name, &scopeEntry{val: allocaPtr, isAlloc: true, noDeinit: true, noRelease: true})
		} else if useEnvDirect {
			// Use the env field GEP directly so mutations persist across calls
			// (e.g. counter closures).  The env is heap-allocated and outlives
			// individual invocations.
			// ARC: retain RC-tracked captures so each call holds its own reference.
			if isRCTrackedType(c.llvmTy) {
				loaded := entry.NewLoad(c.llvmTy, gep)
				cg.emitRetain(entry, loaded)
			}

			cg.curScope.set(c.name, &scopeEntry{val: gep, isAlloc: true, noDeinit: true})
		} else {
			alloca := entry.NewAlloca(c.llvmTy)
			loaded := entry.NewLoad(c.llvmTy, gep)
			entry.NewStore(loaded, alloca)
			// ARC: retain RC-tracked captures so that each invocation holds its own ref.
			if isRCTrackedType(c.llvmTy) {
				cg.emitRetain(entry, loaded)
			}
			// noDeinit: the captured variable is a borrow from the enclosing scope.
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true, noDeinit: true})
		}
	}
}

// callPrintTrait tries to call the print trait method on val, returning the
// resulting string value and true, or (nil, false) if not applicable.
// Handles both concrete-struct dispatch and print fat-pointer dispatch.
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
		sz := constant.NewInt(irtypes.I64, 16) // {i8*, i64} = 16 bytes
		rawPtr := block.NewCall(rcAlloc, sz)
		strPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, strPtr)

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
		// Fat function pointer { fn(i8*,...)*, i8* }: heap-copy the struct so the
		// any can outlive its stack alloca.  Use anyTagFn (5) for all fat fn ptrs
		// so the any-release path can detect closures and release their env.
		tag = anyTagFn

		sz := llvmTypeSize(t)
		if sz == 0 {
			sz = 16 // two pointers
		}

		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		fnPtrStore := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, fnPtrStore)
		// Retain env so the any data block independently owns a reference to it.
		// _tin_retain is null-safe (handles null env for wrapped named functions).
		envField := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureRetain(), envField)

		dataPtr = rawPtr
	case irtypes.IsPointer(t):
		// A pointer to a FuncType is a named/extern function reference; give
		// it the fn tag so typeof() returns 'fn(...) instead of 'ptr.
		if pt, ok2 := t.(*irtypes.PointerType); ok2 {
			if fnType, isFnType := pt.ElemType.(*irtypes.FuncType); isFnType {
				tag = cg.ensureFnTypeID(fnSigName(fnType, false))
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
func (cg *CodeGen) toBool(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		return constant.NewInt(irtypes.I1, 0)
	}

	t := val.Type()
	if t.Equal(irtypes.I1) {
		return val
	}

	if irtypes.IsInt(t) {
		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), t)

		return block.NewICmp(enum.IPredNE, val, zero)
	}

	if irtypes.IsFloat(t) {
		zero := constant.NewFloat(t.(*irtypes.FloatType), 0)

		return block.NewFCmp(enum.FPredONE, val, zero)
	}

	if irtypes.IsPointer(t) {
		null := constant.NewNull(t.(*irtypes.PointerType))

		return block.NewICmp(enum.IPredNE, val, null)
	}

	return constant.NewInt(irtypes.I1, 1)
}

// coerce converts a value to the target type, inserting casts as needed.
func (cg *CodeGen) coerce(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	if val == nil || target == nil {
		return val
	}

	src := val.Type()
	if src.Equal(target) {
		return val
	}

	// Tagged union: wrap a value into a tagged union type (type u = i8 | string).
	if targetSt, ok := target.(*irtypes.StructType); ok {
		if targetName := cg.typeNameOf(target); targetName != "" {
			if _, isUnion := cg.unionTypeMembers[targetName]; isUnion {
				if !src.Equal(target) {
					if wrapped := cg.wrapTaggedUnionVariant(block, val, targetSt, targetName); wrapped != nil {
						return wrapped
					}
				}
			}
		}
	}
	// Native union: store value into union storage (union u_named = ...).
	if targetSt, ok := target.(*irtypes.StructType); ok {
		if targetName := cg.typeNameOf(target); targetName != "" {
			if _, isNative := cg.nativeUnionDecls[targetName]; isNative {
				if !src.Equal(target) {
					return cg.wrapNativeUnion(block, val, targetSt)
				}
			}
		}
	}
	// Trait fat-pointer: coerce a concrete struct or `any` into the trait iface.
	if traitName, ok := cg.isTraitFatPtr(target); ok {
		if _, srcIsTrait := cg.isTraitFatPtr(src); !srcIsTrait {
			if isAnyType(src) {
				if result, err := cg.coerceAnyToTrait(block, val, traitName); err == nil {
					return result
				}
			} else {
				result, err := cg.coerceToTrait(block, val, traitName)
				if err == nil {
					return result
				}
			}
		}
	}

	// implicit[T] conversion: struct S implements implicit[T], call static fn.
	if targetName := cg.typeNameOf(target); targetName != "" {
		for _, entry := range cg.implicitConvFns[targetName] {
			if entry.srcLLVM.Equal(src) {
				return block.NewCall(entry.fn, val)
			}
		}
	}

	// Named function pointer -> fat-fn-ptr: wrap in a thin shim with (i8* env, params...).
	// This enables passing named functions (including extern) to higher-order functions.
	// For async fat-fn-ptrs (inner fn returns i8*), wrap the $coro variant instead.
	if isFatFnPtr(target) && !isFatFnPtr(src) {
		if _, ok := src.(*irtypes.PointerType); ok {
			if isAsyncFatFnPtr(target) {
				return cg.wrapAsyncFnAsFatPtr(block, val, target)
			}

			return cg.wrapFnAsFatPtr(block, val, target)
		}
	}

	// Fat-array coercion is deliberately narrow: only the untyped empty-array
	// literal ({i8*, i64} produced by `[]` with no known target element type)
	// is silently retyped to the target's element type. Any other cross-type
	// fat-array coercion is REJECTED here - callers must either pass the right
	// element type to begin with (see genArrayLitWithElemType plumbing in
	// genArgWithTargetType and call-site args), or write an explicit cast:
	//   let xs [i64] = [1, 2, 3]
	//   consume(xs as [i32])     // element-wise narrowing via genAsExpr
	// Silent implicit narrowing would hide precision-loss bugs (the original
	// motivation for removing the auto-convert path: it was converting
	// non-empty [i64] literals to zero-length [i32] without any user feedback).
	if isFatArrayPtr(src) && isFatArrayPtr(target) {
		srcPt := src.(*irtypes.StructType).Fields[0].(*irtypes.PointerType)
		tgtPt := target.(*irtypes.StructType).Fields[0].(*irtypes.PointerType)

		if srcPt.ElemType.Equal(tgtPt.ElemType) {
			return val // same element type: already correct
		}

		if srcPt.ElemType.Equal(irtypes.I8) && !tgtPt.ElemType.Equal(irtypes.I8) {
			// Empty-array literal only. The untyped {i8*, i64} form is only
			// produced by genArrayLitWithElemType(nil) for empty literals.
			return cg.zeroValue(target)
		}
		// Cross-type fat arrays (e.g. [i64] -> [i32]): leave val unchanged.
		// adaptArgs / call-site validation reports this as a compile error with
		// a hint about `x as [T]`.
		return val
	}

	// %__atom -> string fat-ptr or i8*: convert via __tin_atom_to_string.
	if isAtomType(src) {
		code := cg.extractAtomCode(block, val)

		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		if isFatPtrType(target) {
			return strFatPtr
		}

		if _, ok := target.(*irtypes.PointerType); ok {
			rawPtr := cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}

			return block.NewBitCast(rawPtr, target)
		}
	}

	// Fat-pointer (string / dynamic array) -> raw C pointer: extract data ptr.
	// This enables passing Tin strings directly to extern C functions.
	if isFatPtrType(src) {
		if _, ok := target.(*irtypes.PointerType); ok {
			rawPtr := cg.extractFatPtrData(block, val, src.(*irtypes.StructType))
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}

			return block.NewBitCast(rawPtr, target)
		}
	}

	switch {
	// Any type: box the value.
	case isAnyType(target) && !isAnyType(src):
		return cg.boxToAny(block, val)

	// Int -> Int: extend or truncate.
	case irtypes.IsInt(src) && irtypes.IsInt(target):
		sBits := src.(*irtypes.IntType).BitSize

		tBits := target.(*irtypes.IntType).BitSize
		if sBits < tBits {
			return block.NewSExt(val, target)
		} else if sBits > tBits {
			return block.NewTrunc(val, target)
		}

		return val

	// Float -> Float.
	case irtypes.IsFloat(src) && irtypes.IsFloat(target):
		sBits := floatBits(src.(*irtypes.FloatType))

		tBits := floatBits(target.(*irtypes.FloatType))
		if sBits < tBits {
			return block.NewFPExt(val, target)
		} else if sBits > tBits {
			return block.NewFPTrunc(val, target)
		}

		return val

	// Int -> Float.
	case irtypes.IsInt(src) && irtypes.IsFloat(target):
		return block.NewSIToFP(val, target)

	// Float -> Int.
	case irtypes.IsFloat(src) && irtypes.IsInt(target):
		return block.NewFPToSI(val, target)

	// Pointer -> Pointer.
	case irtypes.IsPointer(src) && irtypes.IsPointer(target):
		return block.NewBitCast(val, target)

	// Int -> Pointer.
	case irtypes.IsInt(src) && irtypes.IsPointer(target):
		return block.NewIntToPtr(val, target)

	// Pointer -> Int.
	case irtypes.IsPointer(src) && irtypes.IsInt(target):
		return block.NewPtrToInt(val, target)
	}

	// Unbox any to a scalar (int, float), struct, or string fat-ptr.
	// Extract the data pointer from the any fat-ptr and load the value.
	if isAnyType(src) && (irtypes.IsInt(target) || irtypes.IsFloat(target) || isStructType(target) || isStringType(target) || isVectorType(target)) {
		anyType := anyFatPtrType()
		anyAlloca := block.NewAlloca(anyType)
		block.NewStore(val, anyAlloca)
		ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
		typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(target))

		return block.NewLoad(target, typedPtr)
	}

	// Pointer-to-struct -> struct value: load the pointed-to value.
	// This handles value-receiver methods called on a pointer (e.g. p.method()
	// where method takes 'this T' but p is '*T').
	if pt, ok := src.(*irtypes.PointerType); ok {
		if pt.ElemType.Equal(target) {
			return block.NewLoad(target, val)
		}
	}

	// Last resort: bitcast if same size.

	return val
}

// convertFatArray converts a {T1*, i64} fat-array to a {T2*, i64}.
//
//   - Same-size element types: reinterpret the data pointer, keep the length.
//     No copy. Covers different signedness (i32 <-> u32), pointer-type changes.
//   - Integer elements of different size: delegates to the _tin_slice_convert_int
//     runtime helper which allocates a fresh buffer and truncates/sign-extends
//     element-wise. Keeping the loop in the runtime avoids introducing control
//     flow inside `coerce`, which would break callers that use the static
//     `block` parameter to continue emitting after the coerce returns.
//   - Anything else (float<->int, struct repacking): returns val unchanged,
//     which will fail LLVM verification at the call and surface loudly.
func (cg *CodeGen) convertFatArray(block *ir.Block, val value.Value, srcSt, tgtSt *irtypes.StructType) value.Value {
	srcPt := srcSt.Fields[0].(*irtypes.PointerType)
	tgtPt := tgtSt.Fields[0].(*irtypes.PointerType)
	srcElem := srcPt.ElemType
	tgtElem := tgtPt.ElemType

	srcSz := llvmTypeSize(srcElem)
	tgtSz := llvmTypeSize(tgtElem)

	// Spill to alloca and extract ptr/len.
	srcSpill := block.NewAlloca(srcSt)
	block.NewStore(val, srcSpill)
	lenGep := block.NewGetElementPtr(srcSt, srcSpill,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	srcLen := block.NewLoad(irtypes.I64, lenGep)
	ptrGep := block.NewGetElementPtr(srcSt, srcSpill,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	srcData := block.NewLoad(srcPt, ptrGep)

	if srcSz == tgtSz {
		newData := block.NewBitCast(srcData, tgtPt)
		resAlloca := block.NewAlloca(tgtSt)
		resPtrGep := block.NewGetElementPtr(tgtSt, resAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(newData, resPtrGep)
		resLenGep := block.NewGetElementPtr(tgtSt, resAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(srcLen, resLenGep)

		return block.NewLoad(tgtSt, resAlloca)
	}

	if !irtypes.IsInt(srcElem) || !irtypes.IsInt(tgtElem) {
		return val
	}

	// Build {i8*, i64} raw slice of source and call runtime converter.
	rawSlice := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
	rawAlloca := block.NewAlloca(rawSlice)
	rawPtrGep := block.NewGetElementPtr(rawSlice, rawAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(block.NewBitCast(srcData, irtypes.I8Ptr), rawPtrGep)
	rawLenGep := block.NewGetElementPtr(rawSlice, rawAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(srcLen, rawLenGep)
	rawVal := block.NewLoad(rawSlice, rawAlloca)

	srcSigned := int64(1)
	if isUnsignedIntLLVMType(srcElem) {
		srcSigned = 0
	}

	convResult := block.NewCall(cg.ensureSliceConvertInt(), rawVal,
		constant.NewInt(irtypes.I64, int64(srcSz)),
		constant.NewInt(irtypes.I64, int64(tgtSz)),
		constant.NewInt(irtypes.I32, srcSigned))

	// Reinterpret {i8*, i64} result as {T2*, i64}.
	resRawAlloca := block.NewAlloca(rawSlice)
	block.NewStore(convResult, resRawAlloca)
	castPtr := block.NewBitCast(resRawAlloca, irtypes.NewPointer(tgtSt))

	return block.NewLoad(tgtSt, castPtr)
}

// isUnsignedIntLLVMType returns true for integer types the codegen prefers
// to treat as unsigned when widening (e.g. u8/char/byte for the runtime's
// signedness flag in slice conversion).
func isUnsignedIntLLVMType(t irtypes.Type) bool {
	// llir/ir doesn't track signedness on IntType, so infer from bit width:
	// we conservatively treat i8 as unsigned (char/byte/u8 all lower to i8)
	// and rely on Tin's narrowing rules on the source side. The impact is
	// only on sign/zero extension when widening; for truncation and same-width
	// conversions there is no difference.
	if it, ok := t.(*irtypes.IntType); ok && it.BitSize == 8 {
		return true
	}

	return false
}

// coerceAnyToTrait constructs a trait fat-pointer {i8* data, vtable*} from an
// `any` value, selecting the correct vtable at runtime via the any's type_id.
// The select chain iterates all structs that implement the trait; the data
// pointer is extracted directly from the any's heap block so mutations through
// the fat-pointer persist (supporting pointer-receiver trait methods).
func (cg *CodeGen) coerceAnyToTrait(block *ir.Block, anyVal value.Value, instKey string) (value.Value, error) {
	fatPtrType, ok := cg.traitFatPtrTypes[instKey]
	if !ok {
		return nil, fmt.Errorf("coerceAnyToTrait: no fat-ptr type for trait %s", instKey)
	}

	vtableSt, ok2 := cg.traitVtableStructTypes[instKey]
	if !ok2 {
		return nil, fmt.Errorf("coerceAnyToTrait: no vtable struct type for trait %s", instKey)
	}

	vtablePtrType := irtypes.NewPointer(vtableSt)

	// Extract type_id from the any value.
	typeIDVal := cg.extractAnyTypeID(block, anyVal)

	// Extract the raw i8* data pointer from the any value.
	anyType := anyFatPtrType()
	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, anyAlloca)
	ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)

	// Build select chain: type_id -> correct vtable pointer.
	var vtableResult value.Value = constant.NewNull(vtablePtrType)

	for _, st0 := range cg.sortedStructTypeIDs() {
		sn := st0.name
		typeID := st0.id
		vtableKey := sn + "__" + instKey

		vg, hasVtable := cg.traitVtableGlobals[vtableKey]
		if !hasVtable {
			continue
		}

		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))
		vtableResult = block.NewSelect(isMatch, vg, vtableResult)
	}

	// Construct the trait fat-pointer {i8* data, vtable*}.
	ifaceAlloca := block.NewAlloca(fatPtrType)
	dataGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, dataGep)
	vtableGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(vtableResult, vtableGep)

	return block.NewLoad(fatPtrType, ifaceAlloca), nil
}

// constCoerce coerces a compile-time constant to the target type without a
// block (used for const preregistration). Handles int/float narrowing/widening.
func (cg *CodeGen) constCoerce(v value.Value, target irtypes.Type) value.Value {
	if v == nil || target == nil || v.Type().Equal(target) {
		return v
	}

	c, ok := v.(constant.Constant)
	if !ok {
		return v
	}

	src := v.Type()
	switch {
	case irtypes.IsInt(src) && irtypes.IsInt(target):
		if ci, ok2 := c.(*constant.Int); ok2 {
			return constant.NewInt(target.(*irtypes.IntType), ci.X.Int64())
		}
	case irtypes.IsFloat(src) && irtypes.IsFloat(target):
		return c
	case irtypes.IsInt(src) && irtypes.IsFloat(target):
		if ci, ok2 := c.(*constant.Int); ok2 {
			return constant.NewFloat(target.(*irtypes.FloatType), float64(ci.X.Int64()))
		}
	case irtypes.IsFloat(src) && irtypes.IsInt(target):
		if cf, ok2 := c.(*constant.Float); ok2 {
			fv, _ := cf.X.Float64()

			return constant.NewInt(target.(*irtypes.IntType), int64(fv))
		}
	}

	return v
}

// checkConstantCompatible returns an error if a constant LLVM value cannot be
// safely coerced to targetType.  Specifically it rejects:
//   - A negative integer literal coercing to an unsigned integer type.
//   - An integer literal that exceeds the maximum value for the target type.
//
// Float truncation (f64 -> f32) is always allowed; precision loss is acceptable.
func checkConstantCompatible(c constant.Constant, targetType irtypes.Type) error {
	intConst, ok := c.(*constant.Int)
	if !ok {
		return nil // floats and other constants are fine
	}

	targetInt, ok2 := targetType.(*irtypes.IntType)
	if !ok2 {
		return nil // not an integer target
	}

	bits := int(targetInt.BitSize)
	val := intConst.X // *big.Int

	// Negative literal -> unsigned type.
	// In Tin, all unsigned widths are tracked as signed bit patterns in i8/i16/i32/i64.
	// We detect "intended unsigned" by checking whether the source constant came from
	// a clearly signed context.  For now we simply reject negative values coercing
	// into any sub-64-bit integer (u8/u16/u32) where the result would truncate sign.
	if val.Sign() < 0 && bits < 64 {
		return fmt.Errorf("constant %s cannot be coerced to %d-bit integer: negative value would lose sign", val.String(), bits)
	}

	// Integer literal overflow check (positive values only).
	if val.Sign() >= 0 && bits < 64 {
		maxVal := (int64(1) << bits) - 1
		if val.IsInt64() && val.Int64() > maxVal {
			return fmt.Errorf("constant %s overflows %d-bit integer type", val.String(), bits)
		}
	}

	return nil
}

func floatBits(t *irtypes.FloatType) int {
	switch t.Kind { //nolint:exhaustive // X86_FP80/PPC_FP128 are not used by tin
	case irtypes.FloatKindHalf:
		return 16
	case irtypes.FloatKindFloat:
		return 32
	case irtypes.FloatKindDouble:
		return 64
	case irtypes.FloatKindFP128:
		return 128
	default:
		return 64
	}
}

// zeroValue returns the zero constant for a given type.
func (cg *CodeGen) zeroValue(t irtypes.Type) value.Value {
	switch {
	case irtypes.IsInt(t):
		return constant.NewInt(t.(*irtypes.IntType), 0)
	case irtypes.IsFloat(t):
		return constant.NewFloat(t.(*irtypes.FloatType), 0)
	case irtypes.IsPointer(t):
		return constant.NewNull(t.(*irtypes.PointerType))
	case irtypes.IsStruct(t):
		st := t.(*irtypes.StructType)

		fields := make([]constant.Constant, len(st.Fields))
		for i, f := range st.Fields {
			fields[i] = cg.zeroValue(f).(constant.Constant)
		}

		return constant.NewStruct(st, fields...)
	case irtypes.IsArray(t):
		return constant.NewZeroInitializer(t)
	}

	return constant.NewInt(irtypes.I64, 0)
}

// isUnsignedTinType returns true when a Tin TypeExpr is one of the unsigned
// integer types: u8, u16, u32, u64 (and their aliases char/byte/uint/size_t).
// byteArrayElemType returns the element type name when t is a [byte], [u8], or
// [char] array type, and "" otherwise.  Used by genEcho to select per-element
// printf format: "byte" -> %02x, "u8" -> %u, "char" -> %c.
func byteArrayElemType(t ast.TypeExpr) string {
	at, ok := t.(*ast.ArrayType)
	if !ok {
		return ""
	}

	st, ok2 := at.Elem.(*ast.SimpleType)
	if !ok2 {
		return ""
	}

	switch st.Name {
	case "byte", "u8", "char":
		return st.Name
	}

	return ""
}

// scalar8BitTypeName returns the Tin type name for 8-bit scalar types:
// "char", "byte", "u8", or "i8".  Returns "" for all other types.
// Used to dispatch printf format in interpolation/echo: char->%c, byte->%x, u8/%u/i8->%d.
func scalar8BitTypeName(t ast.TypeExpr) string {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return ""
	}

	switch st.Name {
	case "char", "byte", "u8", "i8":
		return st.Name
	}

	return ""
}

// scalar128BitTypeName returns "i128", "u128", or "f128" when t is one of those
// types. Returns "" for all other types. Used by echo/interpolation dispatch.
func scalar128BitTypeName(t ast.TypeExpr) string {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return ""
	}

	switch st.Name {
	case "i128", "u128", "f128":
		return st.Name
	}

	return ""
}

func isUnsignedTinType(t ast.TypeExpr) bool {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return false
	}

	switch st.Name {
	case "u8", "char", "byte", "u16", "u32", "uint32", "u64", "uint", "size_t", "u128":
		return true
	}

	return false
}
