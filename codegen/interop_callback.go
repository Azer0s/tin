package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) getOrCreateCallbackThunk(ft *ast.FuncType) (*ir.Func, error) {
	// Tin-side LLVM types (what the thunk's signature exposes).
	tinRet := irtypes.Type(irtypes.Void)

	if ft.RetType != nil {
		t, err := cg.tinTypeToLLVM(ft.RetType)
		if err != nil {
			return nil, err
		}

		tinRet = t
	}

	tinParams := make([]irtypes.Type, len(ft.Params))

	for i, p := range ft.Params {
		t, err := cg.tinTypeToLLVM(p)
		if err != nil {
			return nil, err
		}

		tinParams[i] = t
	}
	// C-side LLVM signature: each Tin param expands into 1 or 2 C
	// args depending on its kind (slices split into ptr+len). The
	// return is always one value (slice returns are rejected by the
	// validator above).
	cParams := make([]irtypes.Type, 0, len(tinParams))
	cExpansion := make([]int, len(tinParams)) // number of C args per Tin param

	for i, p := range ft.Params {
		expand := cParamExpansion(p, tinParams[i])
		cExpansion[i] = len(expand)
		cParams = append(cParams, expand...)
	}

	cRet := cReturnLLVM(ft.RetType, tinRet)

	key := callbackSigKey(tinRet, tinParams)

	if cg.interopCbThunks == nil {
		cg.interopCbThunks = make(map[string]*ir.Func)
	}

	if f, ok := cg.interopCbThunks[key]; ok {
		return f, nil
	}

	thunkName := "__tin_interop_cb_thunk_" + key

	thunkParams := make([]*ir.Param, 0, len(tinParams)+1)
	thunkParams = append(thunkParams, ir.NewParam("env", irtypes.I8Ptr))

	for i, t := range tinParams {
		thunkParams = append(thunkParams, ir.NewParam(fmt.Sprintf("a%d", i), t))
	}

	thunk := cg.mod.NewFunc(thunkName, tinRet, thunkParams...)
	thunk.Linkage = enum.LinkageInternal

	tb := thunk.NewBlock("entry")

	// env layout (matches the wrapper allocation):
	//   offset 0: destructor (NULL, ignored here)
	//   offset 8: raw C fn pointer
	rawFnTy := irtypes.NewFunc(cRet, cParams...)
	rawFnPtrTy := irtypes.NewPointer(rawFnTy)

	fnSlot := tb.NewGetElementPtr(irtypes.I8, thunkParams[0],
		constant.NewInt(irtypes.I32, 8))
	fnSlotCast := tb.NewBitCast(fnSlot, irtypes.NewPointer(rawFnPtrTy))
	rawFn := tb.NewLoad(rawFnPtrTy, fnSlotCast)

	// Per-arg marshaling. Each Tin-shape thunk param becomes one or
	// two C-shape values for the inner call.
	args := make([]value.Value, 0, len(cParams))

	for i, p := range ft.Params {
		pieces := cg.marshalTinToCInThunk(tb, thunkParams[i+1], p, tinParams[i])
		args = append(args, pieces...)
		_ = cExpansion[i] // documented above; only used for clarity
	}

	if cRet.Equal(irtypes.Void) {
		tb.NewCall(rawFn, args...)
		tb.NewRet(nil)
	} else {
		r := tb.NewCall(rawFn, args...)
		ret := cg.marshalCToTinInThunk(tb, r, ft.RetType, cRet, tinRet)
		tb.NewRet(ret)
	}

	cg.interopCbThunks[key] = thunk

	return thunk, nil
}

// cParamExpansion returns the LLVM types each Tin callback parameter
// expands into on the C side. Strings shrink to const char* (i8*);
// slices expand to (T*, i64) two args; bool widens to i8; everything
// else passes through.
func cParamExpansion(t ast.TypeExpr, tinTy irtypes.Type) []irtypes.Type {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			return []irtypes.Type{irtypes.I8Ptr}
		case "bool":
			return []irtypes.Type{irtypes.I8}
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		// Slice: T* + i64 length.
		fatStruct := tinTy.(*irtypes.StructType)

		return []irtypes.Type{fatStruct.Fields[0], irtypes.I64}
	}

	return []irtypes.Type{tinTy}
}

// cReturnLLVM returns the C-side LLVM return type for a callback.
// Strings become i8* (the thunk copies into a Tin string on the way
// back). Bool becomes i8. Everything else passes through.
func cReturnLLVM(ret ast.TypeExpr, tinRet irtypes.Type) irtypes.Type {
	if ret == nil {
		return irtypes.Void
	}

	if st, ok := ret.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			return irtypes.I8Ptr
		case "bool":
			return irtypes.I8
		}
	}

	return tinRet
}

// marshalTinToCInThunk converts one Tin-shape thunk parameter into the
// 1-or-2 C-shape values expected by the underlying C function. Side
// effects: emits IR into block.
func (cg *CodeGen) marshalTinToCInThunk(b *ir.Block, p *ir.Param, t ast.TypeExpr, tinTy irtypes.Type) []value.Value {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			// Extract the data pointer; the Tin string is
			// NUL-terminated by construction so it doubles as a
			// const char*.
			dataPtr := b.NewExtractValue(p, 0)

			return []value.Value{dataPtr}
		case "bool":
			return []value.Value{b.NewZExt(p, irtypes.I8)}
		}
	}

	if _, ok := t.(*ast.ArrayType); ok {
		dataPtr := b.NewExtractValue(p, 0)
		lenVal := b.NewExtractValue(p, 1)

		return []value.Value{dataPtr, lenVal}
	}

	return []value.Value{p}
}

// marshalCToTinInThunk converts the C-side return value back to its
// Tin shape. Strings get copied into a fresh ARC Tin string; bool
// gets icmp'd; everything else passes through.
func (cg *CodeGen) marshalCToTinInThunk(b *ir.Block, v value.Value, ret ast.TypeExpr, cTy, tinTy irtypes.Type) value.Value {
	if ret != nil {
		if st, ok := ret.(*ast.SimpleType); ok {
			switch st.Name {
			case "string":
				// v is i8* from C. Copy into a Tin ARC string.
				return cg.callExtern(b, cg.ensureInteropStrIn(), v)
			case "bool":
				return b.NewICmp(enum.IPredNE, v, constant.NewInt(irtypes.I8, 0))
			}
		}
	}

	return v
}

// getOrCreateClosureDispatcher returns the per-signature dispatcher
// that the runtime trampoline tail-jumps to when a Tin closure
// returned through `#interop` is invoked from C. The dispatcher takes
// the C-shape arguments the trampoline was called with, marshals them
// to Tin shape (mirror of getOrCreateCallbackThunk; same direction as
// emitInteropWrapperFor's per-arg handling), then calls the captured
// Tin fn(env, tin_args...). The Tin return is then marshaled back to
// the C-shape return.
//
// Closure-data lookup uses a scratch register set by the trampoline:
// %r10 on x86_64, x16 (IP0) on AArch64. Both are caller-saved and not
// used by the SysV/AAPCS64 ABIs to pass arguments, so they survive the
// trampoline's indirect jump into the dispatcher untouched. The
// dispatcher's FIRST IR instruction MUST be the inline-asm read of
// that register: any earlier instruction (alloca, marshaling call)
// risks the prologue or register allocator clobbering the scratch.
// The asm is marked SideEffect so LLVM may not sink/CSE it.
//
// FRAGILE: standard x86_64/aarch64 prologues do not touch r10/x16,
// but stack-protector instrumentation, sanitizers (ASan/MSan), or any
// future LLVM pass that inserts work BEFORE entry-block IR could
// clobber the scratch register. This module must not be built with
// -fstack-protector / -fsanitize=*; the runtime trampoline contract
// assumes the scratch register survives end-to-end. If a future
// codegen change starts attaching such attributes, those attributes
// must be explicitly suppressed for dispatchers.
//
// One dispatcher per unique Tin signature; cached on cg.
func (cg *CodeGen) getOrCreateClosureDispatcher(ft *ast.FuncType) (*ir.Func, error) {
	tinRet := irtypes.Type(irtypes.Void)

	if ft.RetType != nil {
		t, err := cg.tinTypeToLLVM(ft.RetType)
		if err != nil {
			return nil, err
		}

		tinRet = t
	}

	tinParams := make([]irtypes.Type, len(ft.Params))

	for i, p := range ft.Params {
		t, err := cg.tinTypeToLLVM(p)
		if err != nil {
			return nil, err
		}

		tinParams[i] = t
	}

	// C-side dispatcher signature: each Tin param expands into 1 or 2
	// C params (slices split into ptr+len). Reuse the same expansion
	// rules as the callback-param thunk so the C ABI surfaces match.
	cParams := make([]irtypes.Type, 0, len(tinParams))
	cExpansion := make([]int, len(tinParams))

	for i, p := range ft.Params {
		expand := cParamExpansion(p, tinParams[i])
		cExpansion[i] = len(expand)
		cParams = append(cParams, expand...)
	}

	cRet := cReturnLLVM(ft.RetType, tinRet)

	key := callbackSigKey(tinRet, tinParams)

	if cg.interopDispatchers == nil {
		cg.interopDispatchers = make(map[string]*ir.Func)
	}

	if f, ok := cg.interopDispatchers[key]; ok {
		return f, nil
	}

	dispName := "__tin_interop_closure_dispatch_" + key

	dispParams := make([]*ir.Param, len(cParams))
	for i, t := range cParams {
		dispParams[i] = ir.NewParam(fmt.Sprintf("a%d", i), t)
	}

	disp := cg.mod.NewFunc(dispName, cRet, dispParams...)
	disp.Linkage = enum.LinkageInternal

	tb := disp.NewBlock("entry")

	// Read the trampoline scratch register FIRST. Anything prior risks
	// clobbering it (function prologue is fine; calls and most
	// instructions can spill into r10/x16). SideEffect prevents CSE /
	// sinking; the resulting SSA value is then safe to use anywhere.
	asmFnTy := irtypes.NewFunc(irtypes.I8Ptr)

	var asm *ir.InlineAsm

	if cg.targetIsARM64() {
		asm = ir.NewInlineAsm(irtypes.NewPointer(asmFnTy), "mov $0, x16", "=r")
	} else {
		asm = ir.NewInlineAsm(irtypes.NewPointer(asmFnTy), "movq %r10, $0", "=r")
	}

	asm.SideEffect = true
	cp := tb.NewCall(asm)

	// cp points to a 16-byte block: { i8* fn, i8* env }.
	fatTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	fatPtr := tb.NewBitCast(cp, irtypes.NewPointer(fatTy))

	fnSlot := tb.NewGetElementPtr(fatTy, fatPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 0))
	envSlot := tb.NewGetElementPtr(fatTy, fatPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 1))

	fnRaw := tb.NewLoad(irtypes.I8Ptr, fnSlot)
	env := tb.NewLoad(irtypes.I8Ptr, envSlot)

	// Marshal each Tin param's C-shape value(s) back to its Tin shape.
	// Track ARC temporaries (Tin strings / slices we freshly allocated
	// to wrap C inputs) so we can release them after the inner call.
	// Without this, every closure invocation would leak one block per
	// string/slice arg.
	cIdx := 0
	tinArgs := make([]value.Value, 0, len(tinParams)+1)
	tinArgs = append(tinArgs, env)

	var allocatedTemps []value.Value

	for i, p := range ft.Params {
		pieces := dispParams[cIdx : cIdx+cExpansion[i]]
		cIdx += cExpansion[i]

		v, temp := cg.marshalCToTinForDispatch(tb, pieces, p, tinParams[i])
		tinArgs = append(tinArgs, v)

		if temp != nil {
			allocatedTemps = append(allocatedTemps, temp)
		}
	}

	// Inner Tin signature: ret_ty (i8* env, tin_params...).
	innerParams := append([]irtypes.Type{irtypes.I8Ptr}, tinParams...)
	innerFnTy := irtypes.NewFunc(tinRet, innerParams...)
	fnTyped := tb.NewBitCast(fnRaw, irtypes.NewPointer(innerFnTy))

	releaseFn := cg.ensureRelease()

	if cRet.Equal(irtypes.Void) {
		tb.NewCall(fnTyped, tinArgs...)

		for _, t := range allocatedTemps {
			tb.NewCall(releaseFn, t)
		}

		tb.NewRet(nil)
	} else {
		tinRetVal := tb.NewCall(fnTyped, tinArgs...)

		for _, t := range allocatedTemps {
			tb.NewCall(releaseFn, t)
		}

		cRetVal := cg.marshalTinToCForDispatch(tb, tinRetVal, ft.RetType, tinRet, cRet)
		tb.NewRet(cRetVal)
	}

	cg.interopDispatchers[key] = disp

	return disp, nil
}

// marshalCToTinForDispatch is the dispatcher counterpart to
// marshalTinToCInThunk: it takes the 1-or-2 C-shape parameter values
// the trampoline received and rebuilds a single Tin-shape value plus
// (optionally) a pointer the caller must release after the inner Tin
// call returns.
//
//   - string: i8* (NUL-terminated C string) -> TinString via
//     tin_interop_str_in (allocates RC string; data ptr returned for
//     post-call release).
//   - bool: i8 -> i1 via icmp ne 0 (no temp).
//   - slice: (T*, i64) -> RC-headed Tin slice via tin_interop_slice_in
//     (the raw C pointer cannot be passed straight through because the
//     callee may retain/release it expecting an RC header at -16).
//     The fresh data pointer is returned for post-call release.
//   - everything else: passthrough (no temp).
func (cg *CodeGen) marshalCToTinForDispatch(b *ir.Block, pieces []*ir.Param,
	t ast.TypeExpr, tinTy irtypes.Type) (value.Value, value.Value) {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			tinStr := cg.callExtern(b, cg.ensureInteropStrIn(), pieces[0])
			temp := b.NewExtractValue(tinStr, 0)

			return tinStr, temp
		case "bool":
			return b.NewICmp(enum.IPredNE, pieces[0], constant.NewInt(irtypes.I8, 0)), nil
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		// Allocate a fresh RC-headed Tin slice that copies the C-side
		// (T*, i64) so the inner Tin callee sees the slice's data
		// pointer as something it can safely retain/release.
		elemTy, err := cg.tinTypeToLLVM(at.Elem)
		if err != nil {
			// Validation already rejected unsupported element types;
			// surface unexpected errors as a panic during codegen so a
			// silently malformed dispatcher never ships.
			panic(fmt.Sprintf("dispatcher slice marshal: %v", err))
		}

		dataI8 := b.NewBitCast(pieces[0], irtypes.I8Ptr)
		elemSize := constant.NewInt(irtypes.I64, int64(llvmTypeSize(elemTy)))
		rawSlice := cg.callExtern(b, cg.ensureInteropSliceIn(), dataI8, pieces[1], elemSize)

		// rawSlice is {i8*, i64}. The internal Tin closure expects
		// {T*, i64}; bitcast the data field and reassemble.
		st := tinTy.(*irtypes.StructType)
		rawData := b.NewExtractValue(rawSlice, 0)
		rawLen := b.NewExtractValue(rawSlice, 1)
		typedData := b.NewBitCast(rawData, st.Fields[0])

		alloca := b.NewAlloca(st)
		ptrField := b.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 0))
		lenField := b.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 1))
		b.NewStore(typedData, ptrField)
		b.NewStore(rawLen, lenField)

		return b.NewLoad(st, alloca), rawData
	}

	return pieces[0], nil
}

// marshalTinToCForDispatch is the dispatcher counterpart to
// marshalCToTinInThunk: takes the Tin return value and produces the
// C-shape return.
//
//   - string: TinString -> i8* (NUL-terminated C string) via tin_interop_str_out
//   - bool:   i1 -> i8 via zext
//   - everything else: passthrough.
func (cg *CodeGen) marshalTinToCForDispatch(b *ir.Block, v value.Value,
	ret ast.TypeExpr, tinTy, cTy irtypes.Type) value.Value {
	if ret != nil {
		if st, ok := ret.(*ast.SimpleType); ok {
			switch st.Name {
			case "string":
				out := cg.callExtern(b, cg.ensureInteropStrOut(), v)
				// Release the Tin string returned by the inner closure
				// after we've copied its bytes into the C buffer.
				ptr := b.NewExtractValue(v, 0)
				b.NewCall(cg.ensureRelease(), ptr)

				return out
			case "bool":
				return b.NewZExt(v, irtypes.I8)
			}
		}
	}

	return v
}

// callbackSigKey is a stable string used both as a cache key and as
// the suffix on the thunk's IR name.
func callbackSigKey(ret irtypes.Type, params []irtypes.Type) string {
	var sb strings.Builder

	sb.WriteString(sanitizeIRTypeName(ret))

	for _, p := range params {
		sb.WriteString("_")
		sb.WriteString(sanitizeIRTypeName(p))
	}

	return sb.String()
}

// sanitizeIRTypeName turns an IR type into a short identifier-safe
// fragment for embedding in symbol names.
func sanitizeIRTypeName(t irtypes.Type) string {
	if t.Equal(irtypes.Void) {
		return "void"
	}

	s := t.String()

	var b strings.Builder

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	return b.String()
}

// ensureRuntimeInitOnce returns the IR declaration for the runtime
// init helper, declaring it lazily on first use in the active module.
func (cg *CodeGen) ensureRuntimeInitOnce() *ir.Func {
	return cg.ensureRuntimeHelper("tin_runtime_init", irtypes.Void)
}

// ensureMakeTrampoline declares
// `i8* tin_make_trampoline(i8* fn, i8* env, i8* dispatcher)`. The
// runtime allocates a slot in an mmap'd executable page, stores
// (fn, env) in the slot's data area, writes the per-arch trampoline
// machine code in the slot's code area, and returns the address of
// the code (the value the C caller will use as a function pointer).
func (cg *CodeGen) ensureMakeTrampoline() *ir.Func {
	const name = "tin_make_trampoline"

	// Cached field is the source of truth across all scopes -- multiple
	// call paths into this helper (interop return-fn, callback args)
	// previously each declared their own and produced a duplicate
	// `declare` in the final IR, which `opt` rejected.
	if cg.makeTrampolineFn != nil {
		return cg.makeTrampolineFn
	}

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			cg.makeTrampolineFn = f

			return f
		}
	}

	f := cg.mod.NewFunc(name, irtypes.I8Ptr,
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("env", irtypes.I8Ptr),
		ir.NewParam("dispatcher", irtypes.I8Ptr))
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})
	cg.makeTrampolineFn = f

	return f
}

// typeExprContains returns true when the type tree rooted at t names
// a type whose root identifier matches `name`. Walks SimpleType,
// GenericType, ArrayType, PointerType, FuncType, TupleArrayType.
// Used by the interop validator to spot `Future` and `any` anywhere
// in a parameter or return position.
func typeExprContains(t ast.TypeExpr, name string) bool {
	if t == nil {
		return false
	}

	switch v := t.(type) {
	case *ast.SimpleType:
		return v.Name == name
	case *ast.GenericType:
		if v.Name == name {
			return true
		}

		for _, p := range v.TypeParams {
			if typeExprContains(p, name) {
				return true
			}
		}
	case *ast.ArrayType:
		return typeExprContains(v.Elem, name)
	case *ast.PointerType:
		return typeExprContains(v.Elem, name)
	case *ast.FuncType:
		for _, p := range v.Params {
			if typeExprContains(p, name) {
				return true
			}
		}

		return typeExprContains(v.RetType, name)
	case *ast.TupleArrayType:
		for _, p := range v.ElemTypes {
			if typeExprContains(p, name) {
				return true
			}
		}
	}

	return false
}
