package codegen

// `#interop` control tag - validation pass.
//
// `fn{#interop} name(...)` requests a C-callable wrapper alongside the
// Tin-internal entry point. v1 only validates that the function is in
// a shape we will actually be able to wrap; codegen for the wrapper
// itself comes in a later phase.
//
// Phase A (declaration-level):
//   - Cannot also be `#async` (an async fn is a coroutine; C cannot drive one)
//   - Return type must not contain `Future[T]` (no way for C to await)
//   - No parameter type may contain `any` (no stable C representation)
//   - Cannot be generic (no concrete name for the wrapper symbol)
//   - Cannot be a struct method (v1: top-level functions only)
//   - Cannot be `extern` (already C, has its own symbol)
//   - Cannot be named `main` (would clobber the binary's entry point)
//   - Two `#interop` functions sharing a name are rejected here rather
//     than letting the linker speak.
//
// Phase B (type whitelist):
//   - Each parameter must be a primitive, pointer, `string`, or fat
//     array `[T]`.
//   - Return type must be a primitive, pointer, `string`, fat array,
//     or `void`.
//   - Anything else (struct, trait object, ADT, union, fn, tuple)
//     rejected with a per-position diagnostic.

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// reservedInteropNames are symbols whose external collision would
// either break linking or, worse, silently shadow a runtime helper.
// `main` is the obvious one; the `tin_*` set covers the public C
// boundary helpers shipped in runtime/interop.c. Note: any name with
// the `__tin_interop_` prefix would also collide with a wrapper's
// hidden internal symbol, but matching by prefix lives in
// validateInteropFunc since it is structural rather than a fixed
// list.
var reservedInteropNames = map[string]bool{
	"main":                  true,
	"tin_runtime_init":      true,
	"tin_set_extern_alloc":  true,
	"tin_extern_alloc":      true,
	"tin_interop_str_in":    true,
	"tin_interop_str_out":   true,
	"tin_interop_slice_in":  true,
	"tin_interop_slice_out": true,
}

// checkAllInteropFuncs walks the program AST and validates every
// `#interop`-tagged function. Methods on structs are walked separately
// from top-level functions so the diagnostic can say "method" rather
// than "function" for a method-level violation.
func (cg *CodeGen) checkAllInteropFuncs(stmts []ast.Node) error {
	seen := make(map[string]bool)

	for _, node := range stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if !hasTag(n.Tags, "interop") {
				continue
			}

			if err := cg.validateInteropFunc(n); err != nil {
				return err
			}

			if seen[n.Name] {
				return cg.nodeErr(n, "fn %s: duplicate #interop function name", n.Name)
			}

			seen[n.Name] = true

		case *ast.StructDecl:
			for _, m := range n.Methods {
				if hasTag(m.Tags, "interop") {
					return cg.nodeErr(m, "fn %s.%s: #interop is not allowed on methods (top-level functions only in v1)",
						n.Name, m.Name)
				}
			}
		}
	}

	return nil
}

// validateInteropFunc runs all per-declaration checks. The order is
// chosen so the most surprising rejection comes first (e.g. `#async`
// conflicts before `Future[T]` mention).
func (cg *CodeGen) validateInteropFunc(fn *ast.FuncDecl) error {
	if hasTag(fn.Tags, "async") {
		return cg.nodeErr(fn, "fn %s: #interop and #async cannot be combined; an async fn is a coroutine and cannot be invoked from C",
			fn.Name)
	}

	if fn.IsExtern != "" {
		return cg.nodeErr(fn, "fn %s: #interop on an extern declaration is meaningless (the symbol is already C)",
			fn.Name)
	}

	if len(fn.TypeParams) > 0 {
		return cg.nodeErr(fn, "fn %s: #interop cannot be applied to a generic function (no single C symbol exists for an un-instantiated template)",
			fn.Name)
	}

	if reservedInteropNames[fn.Name] {
		return cg.nodeErr(fn, "fn %s: #interop functions cannot use the reserved name %q (would clash with a runtime symbol)",
			fn.Name, fn.Name)
	}

	if strings.HasPrefix(fn.Name, "__tin_interop_") {
		return cg.nodeErr(fn, "fn %s: #interop function names cannot start with `__tin_interop_` (reserved internal-symbol prefix)",
			fn.Name)
	}

	if typeExprContains(fn.RetType, "Future") {
		return cg.nodeErr(fn, "fn %s: #interop return type must not contain Future[T]; C has no way to await",
			fn.Name)
	}

	for _, p := range fn.Params {
		if typeExprContains(p.Type, "any") {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s which contains `any`; no stable C representation exists for boxed values",
				fn.Name, p.Name, p.Type)
		}

		if reason := interopTypeReason(p.Type, false); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; %s",
				fn.Name, p.Name, p.Type, reason)
		}
	}

	if fn.RetType != nil {
		if reason := interopTypeReason(fn.RetType, true); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop return type %s; %s",
				fn.Name, fn.RetType, reason)
		}
	}

	return nil
}

// interopElemTypeReason restricts the element type allowed inside a
// fat array `[T]` at the boundary. Only primitives and pointers can
// safely cross via memcpy. Reject everything else with a friendly
// message. Boolean elements are rejected separately because of the
// i1<->i8 ABI mismatch.
func interopElemTypeReason(t ast.TypeExpr) string {
	if st, ok := t.(*ast.SimpleType); ok {
		switch {
		case st.Name == "bool":
			return "[bool] forces a per-element i1/i8 conversion the marshaler does not perform; use [u8] (0 = false, non-zero = true) instead"
		case interopAllowedPrimitives[st.Name]:
			return ""
		case st.Name == "string":
			return "Tin strings are ARC-managed fat pointers; a raw byte copy would produce dangling references in the callee. Pass strings individually or model the array C-side as `const char* const*`."
		case st.Name == "atom":
			return "atom has no stable C representation"
		case st.Name == "any":
			return "any is not C-representable"
		case st.Name == "void":
			return "[void] is not representable"
		}
		// Named user type (struct / trait / ADT) - no stable C layout
		// guarantee, often ARC-managed inside.
		return "named user types are not representable as array elements; use a [u8] payload or pass items individually"
	}

	if _, ok := t.(*ast.PointerType); ok {
		return "" // [*T] is OK - pointers are 8 bytes, no ARC.
	}

	if _, ok := t.(*ast.ArrayType); ok {
		return "nested fat arrays are ARC-managed; a raw byte copy would produce dangling references in the callee"
	}

	return "type is not allowed as a fat-array element"
}

// interopAllowedPrimitives lists the SimpleType names that pass through
// the FFI boundary unchanged. Includes the explicit numeric/bool/char
// set plus a small set of canonical aliases (`size_t`, `uint32`) the
// language doc treats as bare types in interop contexts.
var interopAllowedPrimitives = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true,
	"u8": true, "u16": true, "u32": true, "u64": true,
	"f32": true, "f64": true,
	"bool":  true,
	"char":  true,
	"byte":  true,
	"size_t": true,
	"uint32": true,
}

// interopTypeReason returns "" when t is allowed at an interop
// boundary, or a short reason string otherwise. isReturn loosens the
// rule for `void`. The reason is plugged into the diagnostic message
// at the call site so the user sees both the offending type and a
// pointer at why.
func interopTypeReason(t ast.TypeExpr, isReturn bool) string {
	if t == nil {
		if isReturn {
			return "" // void
		}

		return "void parameters are not representable"
	}

	switch v := t.(type) {
	case *ast.SimpleType:
		switch {
		case interopAllowedPrimitives[v.Name]:
			return ""
		case v.Name == "string":
			return ""
		case v.Name == "void":
			if isReturn {
				return ""
			}

			return "void parameters are not representable"
		case v.Name == "any":
			// Already caught by the typeExprContains pass; keep the
			// message consistent here too.
			return "`any` is not C-representable"
		case v.Name == "atom":
			return "`atom` has no stable C representation in v1"
		}
		// Unknown SimpleType - treat as a user struct/trait/ADT name.
		// v1 disallows all named types at the boundary; users wanting
		// struct interop should pass *Struct explicitly.
		return "v1 does not allow named user types at the interop boundary; pass a pointer (*" + v.Name + ") instead"

	case *ast.PointerType:
		return "" // any *T is fine - opaque to Tin's marshalling

	case *ast.ArrayType:
		// Fat arrays [T]: v1 allows; size != -1 (fixed-size [T;N]) is
		// rejected because there is no clean C ABI for a Tin
		// fixed-size value array distinct from `T*`.
		if v.Size != -1 {
			return "fixed-size arrays are not representable; use a fat array [T] or a pointer *T"
		}
		// The marshaler does a raw memcpy of `len * sizeof(T)` bytes
		// across the C/Tin boundary. That is only safe when T has no
		// ARC headers - i.e. T is a primitive or pointer. Strings,
		// nested slices, structs, ADTs, etc. would be copied as
		// shallow byte blobs without retain/release, producing
		// dangling-pointer crashes the moment Tin touches them.
		if reason := interopElemTypeReason(v.Elem); reason != "" {
			return "array element type " + v.Elem.String() + " is not safe to memcpy across the boundary: " + reason
		}

		return ""

	case *ast.GenericType:
		return "generic types like " + v.Name + "[T] are not representable"

	case *ast.FuncType:
		return "fn-typed values (closures, function pointers) are not representable; use a *void with a hand-written shim if needed"

	case *ast.TupleArrayType:
		return "tuple-array destructuring types (@[...]) are not representable"

	case *ast.UnionTypeExpr:
		return "union types are not representable"
	}

	return "type is not allowed at the interop boundary in v1"
}

// programHasInteropFunc reports whether any top-level function in the
// program is tagged `#interop`. Used by the synthetic-main suppression
// in codegen so library-mode programs don't ship an unwanted main
// symbol that would collide with the C consumer's entry point.
func programHasInteropFunc(stmts []ast.Node) bool {
	for _, node := range stmts {
		if fn, ok := node.(*ast.FuncDecl); ok && hasTag(fn.Tags, "interop") {
			return true
		}
	}

	return false
}

// emitInteropWrappers walks the program for #interop-tagged functions
// and emits a C-callable wrapper for each. The wrapper:
//   1. Calls _tin_runtime_init_once() (idempotent).
//   2. Marshals each argument from its C ABI shape to the Tin shape
//      (string, fat array, bool widening; primitives passthrough).
//   3. Calls the Tin-internal entry point.
//   4. Marshals the return value back to a C-friendly shape.
//   5. Releases any temporary ARC allocations created at the boundary.
func (cg *CodeGen) emitInteropWrappers(stmts []ast.Node) error {
	for _, node := range stmts {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || !hasTag(fn.Tags, "interop") {
			continue
		}

		if err := cg.emitInteropWrapperFor(fn); err != nil {
			return err
		}
	}

	return nil
}

// emitInteropWrapperFor emits a single wrapper. Assumes the validation
// pass already proved the signature is wrappable.
//
// Marshaling layer:
//   - string param: C `const char*` -> ARC TinString (released after).
//   - string return: TinString -> C buffer via tin_extern_alloc.
//   - slice [T] param: C (T*, i64) -> ARC TinSlice (released after).
//   - slice [T] return: TinSlice -> C out-params via tin_extern_alloc;
//     wrapper return becomes i32 status (0=OK, nonzero=OOM).
//   - bool: C uint8_t -> i1 (icmp ne 0); reverse for returns.
//   - everything else: passthrough (primitives, pointers).
func (cg *CodeGen) emitInteropWrapperFor(fn *ast.FuncDecl) error {
	entry, ok := cg.curScope.lookup(fn.Name)
	if !ok {
		return cg.nodeErr(fn, "fn %s: #interop wrapper cannot find internal entry point", fn.Name)
	}

	internalFn, ok := entry.val.(*ir.Func)
	if !ok {
		return cg.nodeErr(fn, "fn %s: #interop entry resolved to non-function value", fn.Name)
	}

	// Build the wrapper's C-ABI signature, remapping per-param.
	// Each Tin param can expand into 1 or more wrapper params:
	//   primitive / pointer -> 1 wrapper param (passthrough)
	//   string              -> 1 wrapper param (i8*)
	//   slice [T]           -> 2 wrapper params (T*, i64)
	wrapperParams := make([]*ir.Param, 0, len(fn.Params))
	paramKinds := make([]string, 0, len(fn.Params))
	// For slice params, we capture the per-param elem byte size so the
	// runtime helper sees the right copy length.
	sliceElemSizes := make(map[int]uint64)

	for paramIdx, p := range fn.Params {
		kind := classifyInteropParam(p.Type)
		paramKinds = append(paramKinds, kind)

		switch kind {
		case "string":
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8Ptr))
		case "slice":
			at := p.Type.(*ast.ArrayType)

			elemTy, err := cg.tinTypeToLLVM(at.Elem)
			if err != nil {
				return err
			}

			sliceElemSizes[paramIdx] = llvmTypeSize(elemTy)

			wrapperParams = append(wrapperParams,
				ir.NewParam(p.Name, irtypes.NewPointer(elemTy)),
				ir.NewParam(p.Name+"_len", irtypes.I64))
		case "bool":
			// C `_Bool` / `uint8_t` is a full byte; Tin `bool` is i1.
			// Take the byte at the boundary, truncate to i1 before
			// passing to the internal entry.
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8))
		default:
			t, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, t))
		}
	}

	retKind := classifyInteropReturn(fn.RetType)

	var (
		retType       irtypes.Type = irtypes.Void
		sliceElemSize uint64
	)

	switch retKind {
	case "void":
		retType = irtypes.Void
	case "string":
		retType = irtypes.I8Ptr
	case "bool":
		// Match C's byte-sized bool ABI; we zero-extend the i1 from
		// the internal call into this i8.
		retType = irtypes.I8
	case "slice":
		// Reshape: status return + two trailing out-params.
		at := fn.RetType.(*ast.ArrayType)

		elemTy, err := cg.tinTypeToLLVM(at.Elem)
		if err != nil {
			return err
		}

		sliceElemSize = llvmTypeSize(elemTy)
		retType = irtypes.I32

		wrapperParams = append(wrapperParams,
			ir.NewParam("out_data", irtypes.NewPointer(irtypes.NewPointer(elemTy))),
			ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	default:
		if fn.RetType != nil {
			t, err := cg.tinTypeToLLVM(fn.RetType)
			if err != nil {
				return err
			}

			retType = t
		}
	}

	wrapper := cg.mod.NewFunc(fn.Name, retType, wrapperParams...)
	block := wrapper.NewBlock("entry")
	block.NewCall(cg.ensureRuntimeInitOnce())

	// Per-arg marshaling. We track Tin temporaries (strings, slices)
	// created here so we can release them after the internal call.
	args := make([]value.Value, 0, len(fn.Params))

	var allocatedTemps []value.Value

	wrapperIdx := 0

	for paramIdx, kind := range paramKinds {
		switch kind {
		case "string":
			cstr := wrapperParams[wrapperIdx]
			wrapperIdx++

			tinStr := block.NewCall(cg.ensureInteropStrIn(), cstr)
			args = append(args, tinStr)

			ptrField := block.NewExtractValue(tinStr, 0)
			allocatedTemps = append(allocatedTemps, ptrField)
		case "bool":
			b := wrapperParams[wrapperIdx]
			wrapperIdx++

			// Truncate the byte to i1 (Tin's bool); semantically this
			// matches C's "non-zero is true" rule because LLVM trunc to
			// i1 takes the low bit, but combined with `cmp ne 0` would
			// be more faithful. Use icmp ne 0 for clarity and to
			// preserve the C semantic that 0x02 is true.
			zero := constant.NewInt(irtypes.I8, 0)
			i1Val := block.NewICmp(enum.IPredNE, b, zero)
			args = append(args, i1Val)
		case "slice":
			dataPtr := wrapperParams[wrapperIdx]
			lenVal := wrapperParams[wrapperIdx+1]
			wrapperIdx += 2

			// Cast the typed C pointer to i8* for the runtime call.
			dataI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
			elemSize := constant.NewInt(irtypes.I64, int64(sliceElemSizes[paramIdx]))
			rawSlice := block.NewCall(cg.ensureInteropSliceIn(), dataI8, lenVal, elemSize)

			// rawSlice is {i8*, i64}. The internal expects {T*, i64}.
			// Extract, bitcast the pointer, and reassemble.
			internalSliceTy, err := cg.tinTypeToLLVM(fn.Params[paramIdx].Type)
			if err != nil {
				return err
			}

			st := internalSliceTy.(*irtypes.StructType)
			rawData := block.NewExtractValue(rawSlice, 0)
			rawLen := block.NewExtractValue(rawSlice, 1)
			typedData := block.NewBitCast(rawData, st.Fields[0])

			alloca := block.NewAlloca(st)
			ptrField := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			lenField := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 1))
			block.NewStore(typedData, ptrField)
			block.NewStore(rawLen, lenField)
			loaded := block.NewLoad(st, alloca)
			args = append(args, loaded)

			allocatedTemps = append(allocatedTemps, rawData)
		default:
			p := wrapperParams[wrapperIdx]
			wrapperIdx++

			args = append(args, p)
		}
	}

	// Call the internal entry.
	var rawRet value.Value

	if internalFn.Sig.RetType.Equal(irtypes.Void) {
		block.NewCall(internalFn, args...)
	} else {
		rawRet = block.NewCall(internalFn, args...)
	}

	// Marshal the return value out to C.
	var (
		finalRet value.Value
		retTinPtr value.Value // ARC ptr to release after extraction
	)

	switch retKind {
	case "string":
		finalRet = block.NewCall(cg.ensureInteropStrOut(), rawRet)
		retTinPtr = block.NewExtractValue(rawRet, 0)
	case "bool":
		// Zero-extend the internal i1 to i8 for the C ABI.
		finalRet = block.NewZExt(rawRet, irtypes.I8)
	case "slice":
		// Pull out the typed-slice fields and rebuild as the i8*-typed
		// slice the runtime helper expects. Then call slice_out which
		// fills the user's out-params and returns 0/1.
		typedData := block.NewExtractValue(rawRet, 0)
		lenVal := block.NewExtractValue(rawRet, 1)
		dataI8 := block.NewBitCast(typedData, irtypes.I8Ptr)

		rawSliceTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
		alloca := block.NewAlloca(rawSliceTy)
		ptrField := block.NewGetElementPtr(rawSliceTy, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 0))
		lenField := block.NewGetElementPtr(rawSliceTy, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 1))
		block.NewStore(dataI8, ptrField)
		block.NewStore(lenVal, lenField)
		rawSlice := block.NewLoad(rawSliceTy, alloca)

		// out_data and out_len are the last two wrapper params.
		outData := wrapperParams[len(wrapperParams)-2]
		outLen := wrapperParams[len(wrapperParams)-1]
		// out_data is T**; cast to i8** for the helper.
		outDataI8 := block.NewBitCast(outData, irtypes.NewPointer(irtypes.I8Ptr))
		elemSize := constant.NewInt(irtypes.I64, int64(sliceElemSize))

		finalRet = block.NewCall(cg.ensureInteropSliceOut(),
			rawSlice, elemSize, outDataI8, outLen)

		// Release the typed data buffer Tin allocated for the slice.
		retTinPtr = dataI8
	default:
		finalRet = rawRet
	}

	// Release any temporary Tin allocations we created for params.
	releaseFn := cg.ensureRelease()

	for _, ptr := range allocatedTemps {
		block.NewCall(releaseFn, ptr)
	}
	// Release the returned Tin string after we've copied it out.
	if retTinPtr != nil {
		block.NewCall(releaseFn, retTinPtr)
	}

	if retType.Equal(irtypes.Void) {
		block.NewRet(nil)
	} else {
		block.NewRet(finalRet)
	}

	return nil
}

// classifyInteropParam returns the marshaling shape for a Tin
// parameter type. Recognized:
//   "string" - Tin string fat pointer
//   "slice"  - Tin fat array [T]
//   "bool"   - Tin bool (i1) widened to/from C uint8_t
//   ""       - passthrough (primitives, pointers, packed structs)
func classifyInteropParam(t ast.TypeExpr) string {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			return "string"
		case "bool":
			return "bool"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	return ""
}

// classifyInteropReturn returns the marshaling shape for a Tin return
// type. "void" / "string" / "slice" / "bool" / "" (passthrough).
func classifyInteropReturn(t ast.TypeExpr) string {
	if t == nil {
		return "void"
	}

	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "void":
			return "void"
		case "string":
			return "string"
		case "bool":
			return "bool"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	return ""
}

// ensureInteropStrIn declares `TinString tin_interop_str_in(i8*)`.
func (cg *CodeGen) ensureInteropStrIn() *ir.Func {
	const name = "tin_interop_str_in"

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			return f
		}
	}

	f := cg.mod.NewFunc(name, stringFatPtrType(),
		ir.NewParam("cstr", irtypes.I8Ptr))
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})

	return f
}

// ensureInteropStrOut declares `i8* tin_interop_str_out(TinString)`.
func (cg *CodeGen) ensureInteropStrOut() *ir.Func {
	const name = "tin_interop_str_out"

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			return f
		}
	}

	f := cg.mod.NewFunc(name, irtypes.I8Ptr,
		ir.NewParam("s", stringFatPtrType()))
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})

	return f
}

// ensureInteropSliceIn declares
// `TinSlice tin_interop_slice_in(i8*, i64, i64)`. The third arg is
// the per-element byte size, baked into the call from the wrapper.
func (cg *CodeGen) ensureInteropSliceIn() *ir.Func {
	const name = "tin_interop_slice_in"

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			return f
		}
	}

	sliceTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)

	f := cg.mod.NewFunc(name, sliceTy,
		ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("elem_size", irtypes.I64))
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})

	return f
}

// ensureInteropSliceOut declares
// `i32 tin_interop_slice_out(TinSlice, i64 elem_size, i8** out_data,
// i64* out_len)`.
func (cg *CodeGen) ensureInteropSliceOut() *ir.Func {
	const name = "tin_interop_slice_out"

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			return f
		}
	}

	sliceTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)

	f := cg.mod.NewFunc(name, irtypes.I32,
		ir.NewParam("s", sliceTy),
		ir.NewParam("elem_size", irtypes.I64),
		ir.NewParam("out_data", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})

	return f
}

// ensureRuntimeInitOnce returns the IR declaration for the runtime
// init helper, declaring it lazily on first use.
func (cg *CodeGen) ensureRuntimeInitOnce() *ir.Func {
	const name = "tin_runtime_init"

	if entry, ok := cg.curScope.lookup(name); ok {
		if f, isFn := entry.val.(*ir.Func); isFn {
			return f
		}
	}

	f := cg.mod.NewFunc(name, irtypes.Void)
	cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})

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

