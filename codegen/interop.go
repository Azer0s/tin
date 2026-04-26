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
	"fmt"
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
	"tin_release":           true,
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
	// Pre-scan for #packed struct declarations so the validator can
	// allow them at the boundary (the wrapper rebuilds a Tin-layout
	// instance from the C-layout the caller provided).
	cg.interopPackedStructs = collectPackedStructNames(stmts)

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

// collectPackedStructNames returns the set of struct names declared
// with the `#packed` tag. Used by the interop validator to allow
// pass-by-value packed structs at the boundary (the wrapper rebuilds
// the Tin-layout from the C-layout the caller provides).
func collectPackedStructNames(stmts []ast.Node) map[string]bool {
	out := map[string]bool{}

	for _, node := range stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if hasTag(sd.Tags, "packed") {
			out[sd.Name] = true
		}
	}

	return out
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
		return cg.nodeErr(fn, "fn %s: #interop function names cannot start with __tin_interop_ (reserved internal-symbol prefix)",
			fn.Name)
	}

	if typeExprContains(fn.RetType, "Future") {
		return cg.nodeErr(fn, "fn %s: #interop return type contains Future[T]; C has no way to await",
			fn.Name)
	}

	for _, p := range fn.Params {
		if typeExprContains(p.Type, "any") {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; the any type is not C-representable - no stable layout exists for boxed values",
				fn.Name, p.Name, p.Type)
		}

		if reason := cg.interopTypeReason(p.Type, false); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; %s",
				fn.Name, p.Name, p.Type, reason)
		}
	}

	if fn.RetType != nil {
		if reason := cg.interopTypeReason(fn.RetType, true); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop return type %s; %s",
				fn.Name, fn.RetType, reason)
		}
	}

	return nil
}

// callbackInnerReason restricts the param/return types allowed inside
// a callback signature crossing the interop boundary. Only primitives
// and `*void` / `*<primitive>` work because the per-signature thunk
// emitted in codegen does not marshal complex types. The returned
// reason is plugged in after `callback parameter ` / `callback return `
// at the call site, so phrase the message to read naturally either way.
func callbackInnerReason(t ast.TypeExpr) string {
	if t == nil {
		return ""
	}

	if st, ok := t.(*ast.SimpleType); ok {
		switch {
		case interopAllowedPrimitives[st.Name]:
			return ""
		case st.Name == "void":
			return ""
		}

		return "type " + st.Name + " is not a primitive C type"
	}

	if pt, ok := t.(*ast.PointerType); ok {
		if elem, ok := pt.Elem.(*ast.SimpleType); ok {
			if elem.Name == "void" || interopAllowedPrimitives[elem.Name] || elem.Name == "char" || elem.Name == "byte" {
				return ""
			}
		}

		return "pointer must be *void or a pointer to a primitive"
	}

	return "type is not allowed inside a callback signature"
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
			return "Tin strings are ARC-managed fat pointers; a raw byte copy would produce dangling references in the callee - pass strings individually, or model the array C-side as const char* const*"
		case st.Name == "atom":
			return "atom has no stable C representation"
		case st.Name == "any":
			return "any is not C-representable"
		case st.Name == "void":
			return "[void] is not representable"
		}
		// Named user type (struct / trait / ADT) - no stable C layout
		// guarantee, often ARC-managed inside.
		return "named user types are not representable as fat-array elements; use [u8] as a payload or pass items individually"
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
func (cg *CodeGen) interopTypeReason(t ast.TypeExpr, isReturn bool) string {
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
			return "any is not C-representable"
		case v.Name == "atom":
			return "atom has no stable C representation"
		}
		// Allow #packed structs by value: the wrapper applies SysV's
		// struct-coercion rules so the C-side struct ABI agrees with
		// what LLVM emits for our wrapper signature. Non-packed user
		// structs would carry vtable pointers and field padding that
		// C cannot anticipate, so they remain rejected.
		if cg.interopPackedStructs[v.Name] {
			return ""
		}

		return "named user types must be either *void (opaque handle) or struct{#packed} for pass-by-value at the interop boundary"

	case *ast.PointerType:
		// Allow *void, *<primitive>, or *<another-pointer>. Reject
		// pointer-to-named-struct: Tin's struct layout has a hidden
		// type_id prefix (and possibly vtable pointers) that C cannot
		// know about, so a C-allocated struct passed by pointer would
		// silently read wrong fields. Force users into *void for
		// struct handles.
		switch elem := v.Elem.(type) {
		case *ast.SimpleType:
			if elem.Name == "void" || interopAllowedPrimitives[elem.Name] || elem.Name == "char" || elem.Name == "byte" {
				return ""
			}

			return "*" + elem.Name + " is unsafe at the interop boundary because Tin's struct layout has a hidden type_id prefix; use *void as an opaque handle"
		case *ast.PointerType:
			return cg.interopTypeReason(v.Elem, false)
		}

		return "this pointer type is not safe at the interop boundary; use *void as an opaque handle"

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
		// Callbacks: only allowed as parameters today, and only with
		// primitive/pointer-shaped sub-signatures. The wrapper boxes
		// the raw C fn pointer into a per-signature thunk.
		if isReturn {
			return "function-pointer returns are not supported; only callback parameters are wrapped"
		}

		for _, pt := range v.Params {
			if r := callbackInnerReason(pt); r != "" {
				return "callback parameter " + r
			}
		}

		if v.RetType != nil {
			if r := callbackInnerReason(v.RetType); r != "" {
				return "callback return " + r
			}
		}

		return ""

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
		kind := cg.classifyInteropParam(p.Type)
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
		case "callback":
			// Wrapper takes the raw C function pointer as i8*.
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8Ptr))
		case "packed":
			structName := p.Type.(*ast.SimpleType).Name

			shape, _, err := cg.packedABIShape(structName)
			if err != nil {
				return cg.nodeErr(fn, "fn %s: #interop parameter %q: %v", fn.Name, p.Name, err)
			}

			switch shape {
			case "i8", "i16", "i32", "i64", "f32", "f64":
				wrapperParams = append(wrapperParams,
					ir.NewParam(p.Name, abiRegisterType(shape)))
			case "two_i64":
				wrapperParams = append(wrapperParams,
					ir.NewParam(p.Name+".lo", irtypes.I64),
					ir.NewParam(p.Name+".hi", irtypes.I64))
			case "byval":
				rawTy := cg.packedUserStructType(structName)

				bv := ir.NewParam(p.Name, irtypes.I8Ptr)
				bv.Attrs = append(bv.Attrs, ir.Byval{Typ: rawTy})
				wrapperParams = append(wrapperParams, bv)
			}
		default:
			t, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, t))
		}
	}

	retKind := cg.classifyInteropReturn(fn.RetType)

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
	case "packed":
		structName := fn.RetType.(*ast.SimpleType).Name

		shape, _, err := cg.packedABIShape(structName)
		if err != nil {
			return cg.nodeErr(fn, "fn %s: #interop return: %v", fn.Name, err)
		}

		switch shape {
		case "i8", "i16", "i32", "i64", "f32", "f64":
			retType = abiRegisterType(shape)
		case "two_i64":
			retType = irtypes.NewStruct(irtypes.I64, irtypes.I64)
		case "byval":
			// sret hidden first param; the wrapper's nominal return
			// is void. The sret target is the user-fields-only struct
			// type so the wrapper signature matches the C ABI for the
			// equivalent packed struct.
			rawTy := cg.packedUserStructType(structName)

			sret := ir.NewParam(".sret", irtypes.NewPointer(rawTy))
			sret.Attrs = append(sret.Attrs, ir.SRet{Typ: rawTy})
			wrapperParams = append([]*ir.Param{sret}, wrapperParams...)
			retType = irtypes.Void
		}
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

	// Skip past the prepended sret slot when the return is byval.
	wrapperIdx := 0
	if retKind == "packed" {
		if shape, _, _ := cg.packedABIShape(fn.RetType.(*ast.SimpleType).Name); shape == "byval" {
			wrapperIdx = 1
		}
	}

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
		case "packed":
			structName := fn.Params[paramIdx].Type.(*ast.SimpleType).Name

			shape, _, err := cg.packedABIShape(structName)
			if err != nil {
				return cg.nodeErr(fn, "fn %s: #interop parameter %q: %v", fn.Name, fn.Params[paramIdx].Name, err)
			}

			tinTy, err := cg.tinTypeToLLVM(fn.Params[paramIdx].Type)
			if err != nil {
				return err
			}

			tinAlloca := block.NewAlloca(tinTy)

			// Init type_id at field 0.
			tid := constant.NewInt(irtypes.I32, int64(cg.structTypeIDs[structName]))
			tidSlot := block.NewGetElementPtr(tinTy, tinAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			block.NewStore(tid, tidSlot)

			userOffBytes := cg.userFieldsByteOffset(structName)
			tinAllocaI8 := block.NewBitCast(tinAlloca, irtypes.I8Ptr)
			userBase := block.NewGetElementPtr(irtypes.I8, tinAllocaI8,
				constant.NewInt(irtypes.I64, int64(userOffBytes)))

			switch shape {
			case "i8", "i16", "i32", "i64", "f32", "f64":
				v := wrapperParams[wrapperIdx]
				wrapperIdx++

				slot := block.NewBitCast(userBase, irtypes.NewPointer(abiRegisterType(shape)))
				block.NewStore(v, slot)
			case "two_i64":
				lo := wrapperParams[wrapperIdx]
				hi := wrapperParams[wrapperIdx+1]
				wrapperIdx += 2

				loSlot := block.NewBitCast(userBase, irtypes.NewPointer(irtypes.I64))
				block.NewStore(lo, loSlot)

				hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
					constant.NewInt(irtypes.I64, 8))
				hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(irtypes.I64))
				block.NewStore(hi, hiSlot)
			case "byval":
				bvParam := wrapperParams[wrapperIdx]
				wrapperIdx++

				// memcpy the user-fields region from the byval ptr.
				_, totalSize, _ := cg.packedABIShape(structName)
				cg.emitMemcpy(block, userBase, bvParam, int64(totalSize))
			}

			loaded := block.NewLoad(tinTy, tinAlloca)
			args = append(args, loaded)
		case "callback":
			rawCb := wrapperParams[wrapperIdx]
			wrapperIdx++

			ft := fn.Params[paramIdx].Type.(*ast.FuncType)
			thunk, err := cg.getOrCreateCallbackThunk(ft)
			if err != nil {
				return err
			}
			// Build an ARC-managed env block: 16 bytes layout
			//   offset 0: destructor fn (NULL)
			//   offset 8: raw C fn pointer (read by the thunk)
			// The destructor slot is required because Tin's
			// _tin_release_closure unconditionally invokes whatever
			// pointer it finds at env+0 when rc reaches zero.
			envI8 := block.NewCall(cg.ensureRCAlloc(),
				constant.NewInt(irtypes.I64, 16))

			i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)

			dtorSlot := block.NewBitCast(envI8, i8PtrPtr)
			block.NewStore(constant.NewNull(irtypes.I8Ptr), dtorSlot)

			fnSlotPtr := block.NewGetElementPtr(irtypes.I8, envI8,
				constant.NewInt(irtypes.I32, 8))
			fnSlotPtrCast := block.NewBitCast(fnSlotPtr, i8PtrPtr)
			block.NewStore(rawCb, fnSlotPtrCast)

			// Build the Tin fat fn-ptr `{thunk, env}`.
			fatTy, err := cg.tinTypeToLLVM(ft)
			if err != nil {
				return err
			}

			st := fatTy.(*irtypes.StructType)

			alloca := block.NewAlloca(st)
			fnFieldPtr := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			envFieldPtr := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 1))

			thunkCast := block.NewBitCast(thunk, st.Fields[0])
			block.NewStore(thunkCast, fnFieldPtr)
			block.NewStore(envI8, envFieldPtr)

			loaded := block.NewLoad(st, alloca)
			args = append(args, loaded)

			// The internal entry retains+releases env per Tin's
			// fn-arg convention; we drop our originating reference
			// after the call so the env block is freed at rc=0.
			allocatedTemps = append(allocatedTemps, envI8)
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
	case "packed":
		structName := fn.RetType.(*ast.SimpleType).Name

		shape, totalSize, err := cg.packedABIShape(structName)
		if err != nil {
			return cg.nodeErr(fn, "fn %s: #interop return: %v", fn.Name, err)
		}

		tinTy, _ := cg.tinTypeToLLVM(fn.RetType)
		tinAlloca := block.NewAlloca(tinTy)
		block.NewStore(rawRet, tinAlloca)

		userOffBytes := cg.userFieldsByteOffset(structName)
		tinAllocaI8 := block.NewBitCast(tinAlloca, irtypes.I8Ptr)
		userBase := block.NewGetElementPtr(irtypes.I8, tinAllocaI8,
			constant.NewInt(irtypes.I64, int64(userOffBytes)))

		switch shape {
		case "i8", "i16", "i32", "i64", "f32", "f64":
			slot := block.NewBitCast(userBase, irtypes.NewPointer(abiRegisterType(shape)))
			finalRet = block.NewLoad(abiRegisterType(shape), slot)
		case "two_i64":
			loSlot := block.NewBitCast(userBase, irtypes.NewPointer(irtypes.I64))
			lo := block.NewLoad(irtypes.I64, loSlot)

			hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
				constant.NewInt(irtypes.I64, 8))
			hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(irtypes.I64))
			hi := block.NewLoad(irtypes.I64, hiSlot)

			pairTy := irtypes.NewStruct(irtypes.I64, irtypes.I64)
			pairAlloca := block.NewAlloca(pairTy)
			loP := block.NewGetElementPtr(pairTy, pairAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			hiP := block.NewGetElementPtr(pairTy, pairAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 1))
			block.NewStore(lo, loP)
			block.NewStore(hi, hiP)
			finalRet = block.NewLoad(pairTy, pairAlloca)
		case "byval":
			// sret: copy user fields into the caller-provided slot.
			sretParam := wrapperParams[0] // sret was prepended
			cg.emitMemcpy(block, sretParam, userBase, int64(totalSize))
			// finalRet stays nil; we ret void below.
		}
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
//   "string"   - Tin string fat pointer
//   "slice"    - Tin fat array [T]
//   "bool"     - Tin bool (i1) widened to/from C uint8_t
//   "callback" - fn(...) typed; wrapped via per-signature thunk
//   "packed"   - by-value #packed user struct
//   ""         - passthrough (primitives, pointers)
func (cg *CodeGen) classifyInteropParam(t ast.TypeExpr) string {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			return "string"
		case "bool":
			return "bool"
		}

		if cg.interopPackedStructs[st.Name] {
			return "packed"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	if _, ok := t.(*ast.FuncType); ok {
		return "callback"
	}

	return ""
}

// classifyInteropReturn returns the marshaling shape for a Tin return
// type. "void" / "string" / "slice" / "bool" / "packed" / "" (passthrough).
func (cg *CodeGen) classifyInteropReturn(t ast.TypeExpr) string {
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

		if cg.interopPackedStructs[st.Name] {
			return "packed"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	return ""
}

// packedABIShape categorises a #packed Tin struct for the SysV
// x86_64 / AAPCS64 boundary:
//   "i8" / "i16" / "i32" / "i64": all fields naturally align AND total
//                                 size <= 8 - clang would coerce to
//                                 the matching integer register.
//   "two_i64":                    same but 9-16 bytes.
//   "byval":                      anything else (mixed alignment, or
//                                 size > 16). Passed via byval ptr,
//                                 returned via sret hidden ptr.
// Returns ("", err) if the struct is not eligible (e.g. has trait
// vtables - the wrapper would not know how to fill them).
func (cg *CodeGen) packedABIShape(structName string) (string, int, error) {
	if cg.vtableOffset(structName) != 0 {
		return "", 0, fmt.Errorf("packed struct %s implements traits; pass-by-value at the interop boundary requires no vtable pointers", structName)
	}

	userTypes := cg.structFieldLLVMTypes[structName]
	if userTypes == nil {
		return "", 0, fmt.Errorf("packed struct %s: layout not yet computed", structName)
	}

	size := 0
	for _, t := range userTypes {
		size += int(llvmTypeSize(t))
	}

	if size == 0 {
		return "", 0, fmt.Errorf("packed struct %s has zero size", structName)
	}
	// SSE-class single field: clang lowers a struct holding exactly
	// one f32/f64 to the bare float/double ABI (XMM register). Match
	// it so the cross-language signature lines up.
	if size <= 8 && naturallyAligned(userTypes) && len(userTypes) == 1 {
		if irtypes.IsFloat(userTypes[0]) {
			switch userTypes[0].(*irtypes.FloatType).Kind {
			case irtypes.FloatKindFloat:
				return "f32", size, nil
			case irtypes.FloatKindDouble:
				return "f64", size, nil
			}
		}
	}
	// Multi-float same-eightbyte (e.g. 2x f32) would need <2 x float>
	// vector ABI; not implemented in v1.
	if size <= 8 && naturallyAligned(userTypes) && allFloatFields(userTypes) && len(userTypes) > 1 {
		return "", size, fmt.Errorf("packed struct %s contains multiple float fields in one eightbyte; v1 does not implement the SSE vector ABI - pass *%s instead", structName, structName)
	}
	// Single-register integer coercion: works when the packed layout
	// matches the natural byte layout AND the struct fits in one
	// integer eightbyte. Both LLVM and clang lower it to the same
	// integer type so the cross-language ABI agrees.
	if size <= 8 && naturallyAligned(userTypes) {
		switch size {
		case 1:
			return "i8", size, nil
		case 2:
			return "i16", size, nil
		case 3, 4:
			return "i32", size, nil
		case 5, 6, 7, 8:
			return "i64", size, nil
		}
	}
	// Non-natural alignment of any size: clang uses byval/sret.
	if !naturallyAligned(userTypes) {
		return "byval", size, nil
	}

	return "", size, fmt.Errorf("packed struct %s (%d bytes, naturally aligned) is too large for v1 by-value interop; pass *%s instead", structName, size, structName)
}

// allFloatFields reports whether every field is a float type.
func allFloatFields(types []irtypes.Type) bool {
	for _, t := range types {
		if !irtypes.IsFloat(t) {
			return false
		}
	}

	return true
}

// naturallyAligned reports whether a packed list of LLVM types would
// have the SAME byte layout under natural alignment - i.e. packed
// would not actually change anything. When true, clang treats the
// struct the same as its non-packed counterpart and chooses register
// passing for small sizes; when false, clang chooses byval.
func naturallyAligned(types []irtypes.Type) bool {
	off := uint64(0)

	for _, t := range types {
		sz, align := llvmTypeSizeAlign(t)
		if align == 0 {
			align = 1
		}

		if off%align != 0 {
			return false
		}

		off += sz
	}

	return true
}

// abiRegisterType returns the LLVM type matching one of the
// register-passed ABI shapes ("i8".."i64", "f32", "f64").
func abiRegisterType(shape string) irtypes.Type {
	switch shape {
	case "i8":
		return irtypes.I8
	case "i16":
		return irtypes.I16
	case "i32":
		return irtypes.I32
	case "i64":
		return irtypes.I64
	case "f32":
		return irtypes.Float
	case "f64":
		return irtypes.Double
	}

	return nil
}

// userFieldsByteOffset returns the byte offset where a struct's user
// fields start, accounting for the i32 type_id at index 0 and any
// vtable pointer slots. For #packed structs (no padding), this is the
// offset our register-coerced ABI shape stores into.
func (cg *CodeGen) userFieldsByteOffset(structName string) int {
	off := 4 // i32 type_id
	for range cg.structVtableOrder[structName] {
		off += 8 // each vtable pointer is 8 bytes on 64-bit targets
	}

	return off
}

// packedUserStructType returns a packed LLVM struct type containing
// just the user fields of a #packed Tin struct. Used as the byval/sret
// target type so the wrapper signature matches what clang would emit
// for the equivalent C `__attribute__((packed))` struct argument.
func (cg *CodeGen) packedUserStructType(structName string) *irtypes.StructType {
	st := irtypes.NewStruct(cg.structFieldLLVMTypes[structName]...)
	st.Packed = true

	return st
}

// emitMemcpy emits a `llvm.memcpy.p0i8.p0i8.i64(dst, src, n, false)`
// call. dst and src are bitcast to i8* if needed.
func (cg *CodeGen) emitMemcpy(block *ir.Block, dst, src value.Value, n int64) {
	dstI8 := dst
	srcI8 := src

	if _, ok := dst.Type().(*irtypes.PointerType); ok && !dst.Type().Equal(irtypes.I8Ptr) {
		dstI8 = block.NewBitCast(dst, irtypes.I8Ptr)
	}

	if _, ok := src.Type().(*irtypes.PointerType); ok && !src.Type().Equal(irtypes.I8Ptr) {
		srcI8 = block.NewBitCast(src, irtypes.I8Ptr)
	}

	block.NewCall(cg.ensureMemcpy(),
		dstI8, srcI8,
		constant.NewInt(irtypes.I64, n),
		constant.NewInt(irtypes.I1, 0))
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

// getOrCreateCallbackThunk returns a Tin-calling-convention thunk for
// the given callback signature. The thunk receives `i8* env` first
// (Tin's fat fn-ptr ABI), reads the raw C function pointer from env,
// converts each argument from its Tin shape to its C ABI shape (the
// only difference today is bool: Tin i1 vs C uint8_t), calls the C
// function, converts the return back to Tin shape, and returns. One
// thunk is emitted per unique Tin signature and cached on the codegen.
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
	// C-side LLVM types (what env is bitcast to). Today only bool
	// (i1 in Tin / i8 in C) differs. Keep the maps parallel to the
	// Tin shape so per-position conversion is straightforward.
	cRet := cTypeOfTinForCallback(tinRet)
	cParams := make([]irtypes.Type, len(tinParams))

	for i, tt := range tinParams {
		cParams[i] = cTypeOfTinForCallback(tt)
	}

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

	args := make([]value.Value, len(cParams))
	for i := range cParams {
		args[i] = tinToCInThunk(tb, thunkParams[i+1], tinParams[i], cParams[i])
	}

	if cRet.Equal(irtypes.Void) {
		tb.NewCall(rawFn, args...)
		tb.NewRet(nil)
	} else {
		r := tb.NewCall(rawFn, args...)
		tb.NewRet(cToTinInThunk(tb, r, cRet, tinRet))
	}

	cg.interopCbThunks[key] = thunk

	return thunk, nil
}

// cTypeOfTinForCallback maps a Tin-side LLVM type to the C-side LLVM
// type clang would use for the same position in a callback signature.
// Today the only difference is bool (Tin i1 -> C i8); everything else
// passes through.
func cTypeOfTinForCallback(t irtypes.Type) irtypes.Type {
	if t.Equal(irtypes.I1) {
		return irtypes.I8
	}

	return t
}

// tinToCInThunk converts a Tin-shape value to its C-shape counterpart
// at the thunk's call site. Only bool needs work (zext i1 -> i8);
// everything else is a no-op.
func tinToCInThunk(b *ir.Block, v value.Value, tinTy, cTy irtypes.Type) value.Value {
	if tinTy.Equal(irtypes.I1) && cTy.Equal(irtypes.I8) {
		return b.NewZExt(v, irtypes.I8)
	}

	return v
}

// cToTinInThunk converts a C-shape return value back to Tin shape.
// Only bool needs work (icmp ne 0); everything else is a no-op.
func cToTinInThunk(b *ir.Block, v value.Value, cTy, tinTy irtypes.Type) value.Value {
	if tinTy.Equal(irtypes.I1) && cTy.Equal(irtypes.I8) {
		return b.NewICmp(enum.IPredNE, v, constant.NewInt(irtypes.I8, 0))
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

