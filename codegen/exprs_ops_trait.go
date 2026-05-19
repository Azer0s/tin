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

type opTraitImplEntry struct {
	// paramTypes are the LLVM types of the method's non-receiver parameters,
	// in source order. For binary ops there is one entry; for unary ops the
	// slice is empty.
	paramTypes []irtypes.Type
	fn         *ir.Func
}

// recordOpTraitImpl appends an entry to cg.opTraitImpls if traitName is one
// of the built-in operator traits.
func (cg *CodeGen) recordOpTraitImpl(structKey, traitName string, fn *ir.Func) {
	if !isBuiltinOpTraitName(traitName) {
		return
	}

	if fn == nil || len(fn.Sig.Params) == 0 {
		return
	}

	paramTypes := make([]irtypes.Type, 0, len(fn.Sig.Params)-1)
	for i := 1; i < len(fn.Sig.Params); i++ {
		paramTypes = append(paramTypes, fn.Sig.Params[i])
	}

	key := structKey + "/" + traitName

	for _, e := range cg.opTraitImpls[key] {
		if e.fn == fn {
			return // already registered
		}
	}

	cg.opTraitImpls[key] = append(cg.opTraitImpls[key], opTraitImplEntry{
		paramTypes: paramTypes,
		fn:         fn,
	})
}

// isBuiltinOpTraitName reports whether name is one of the operator traits
// registered by registerBuiltinOpTraits. Mirror of the highlighter's table.
func isBuiltinOpTraitName(name string) bool {
	switch name {
	case "add", "sub", "mul", "div", "mod",
		"neg", "pos", "not",
		"comp", "ord",
		"index", "index_set",
		"concat":
		return true
	}

	return false
}

// extractOpTraitName returns the trait base name from a method's TraitQualifier
// when that method is recognized as an op-trait impl (e.g. "add[Vec3, Vec3]"
// -> "add"). Returns "" if the qualifier is not an op-trait reference.
func extractOpTraitName(traitQualifier string) string {
	if traitQualifier == "" {
		return ""
	}

	bare := stripQualifierModule(traitQualifier)
	// Drop type-arg suffix "[..]" if present.
	if idx := strings.IndexByte(bare, '['); idx >= 0 {
		bare = bare[:idx]
	}

	if isBuiltinOpTraitName(bare) {
		return bare
	}

	return ""
}

// binOpTraitName maps a binary operator token to its operator-trait name.
// Returns "" if op has no trait dispatch (e.g. `&&`, `||`, `<<`, `>>`).
func binOpTraitName(op string) string {
	switch op {
	case "+":
		return "add"
	case "-":
		return "sub"
	case "*":
		return "mul"
	case "/":
		return "div"
	case "%":
		return "mod"
	case "++":
		return "concat"
	case "==", "!=":
		return "comp"
	case "<", "<=", ">", ">=":
		return "ord"
	}

	return ""
}

// unaryOpTraitName maps a unary operator token to its operator-trait name.
// Returns ("", false) if op has no trait dispatch.
func unaryOpTraitName(op string) (string, bool) {
	switch op {
	case "-":
		return "neg", true
	case "+":
		return "pos", true
	case "!":
		return "not", true
	}

	return "", false
}

// compoundAssignTraitName maps a compound-assignment operator (`+=`, `-=`, ...)
// to its operator-trait name, used by genAugAssign to desugar `a OP= b` into
// `a = a.OP(b)` when the LHS is a user struct.
func compoundAssignTraitName(op string) string {
	switch op {
	case "+=":
		return "add"
	case "-=":
		return "sub"
	case "*=":
		return "mul"
	case "/=":
		return "div"
	case "%=":
		return "mod"
	case "++=":
		return "concat"
	}

	return ""
}

// binOpIsCommutative reports whether op is mathematically commutative for
// asymmetric primitive+struct dispatch. `==` is symmetric in result; `+` and
// `*` are commutative for the cases users typically expect (Vec + scalar).
// Non-commutative ops require the user to provide the explicit `struct OP prim`
// form themselves.
func binOpIsCommutative(op string) bool {
	switch op {
	case "+", "*", "==", "!=":
		return true
	}

	return false
}

// lookupOpMethod resolves a struct method that implements a built-in operator
// trait, picking by exact non-receiver argument types. Returns nil if no impl
// matches.
//
// `argTypes` are the LLVM types of the user-visible operands (one for binary,
// none for unary). The receiver is implicit and not included.
//
// Both top-level and package-loaded structs register impls under the full
// canonical struct name (`Box` vs. `decimal__Value`); the lookup key here
// matches whatever `typeNameOf(operand)` returns at the call site so the
// two always agree.
func (cg *CodeGen) lookupOpMethod(structName, traitName string, argTypes []irtypes.Type) *ir.Func {
	key := structName + "/" + traitName

	for _, e := range cg.opTraitImpls[key] {
		if paramTypesEqual(e.paramTypes, argTypes) {
			return e.fn
		}
	}

	return nil
}

// paramTypesEqual reports whether two LLVM type slices are element-wise equal.
func paramTypesEqual(a, b []irtypes.Type) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}

	return true
}

// emitOpDispatch emits a call to a previously-resolved operator-trait impl.
// `recv` is the receiver value; `args` are the user-visible operands. The
// receiver is adapted to value-vs-pointer to match the method signature.
func (cg *CodeGen) emitOpDispatch(block *ir.Block, fn *ir.Func, recv value.Value, args []value.Value) (value.Value, error) {
	sig := fn.Sig
	if len(sig.Params) == 0 {
		return nil, fmt.Errorf("operator dispatch: method %s has no receiver", fn.Name())
	}

	recvParam := sig.Params[0]

	var recvArg value.Value

	if pt, isPtr := recvParam.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(recv.Type()) {
		alloca := block.NewAlloca(recv.Type())
		block.NewStore(recv, alloca)
		recvArg = alloca
	} else {
		recvArg = recv
	}

	callArgs := make([]value.Value, 0, len(args)+1)
	callArgs = append(callArgs, recvArg)
	callArgs = append(callArgs, args...)

	callArgs = cg.adaptArgs(block, callArgs, sig)
	result := block.NewCall(fn, callArgs...)

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// dispatchBinOp tries to lower a struct-operand binary expression to an
// operator-trait method call. Returns (result, true, nil) on success;
// (nil, false, nil) when no impl exists; (nil, true, err) on error.
func (cg *CodeGen) dispatchBinOp(block *ir.Block, e *ast.BinExpr, left, right value.Value, lt, rt irtypes.Type) (value.Value, bool, error) {
	traitName := binOpTraitName(e.Op)
	if traitName == "" {
		return nil, false, nil
	}

	if isStructType(lt) {
		structName := cg.typeNameOf(lt)
		if fn := cg.lookupOpMethod(structName, traitName, []irtypes.Type{rt}); fn != nil {
			res, err := cg.emitOpDispatch(block, fn, left, []value.Value{right})
			if err != nil {
				return nil, true, err
			}

			return cg.finishBinOpDispatch(block, e.Op, res), true, nil
		}

		// Implicit-coerce fallback: an impl `add[S, P]` is registered
		// but rt != P; if rt can implicit[P] into the param type the
		// user's intent is "promote rhs to P and call".  Mirrors how
		// `let v P = rval` would fire the implicit at any other slot.
		if fn, coerced := cg.findOpImplWithCoerce(block, structName, traitName, right, rt); fn != nil {
			res, err := cg.emitOpDispatch(block, fn, left, []value.Value{coerced})
			if err != nil {
				return nil, true, err
			}

			return cg.finishBinOpDispatch(block, e.Op, res), true, nil
		}
	}

	if isStructType(rt) && !isStructType(lt) && binOpIsCommutative(e.Op) {
		structName := cg.typeNameOf(rt)
		if fn := cg.lookupOpMethod(structName, traitName, []irtypes.Type{lt}); fn != nil {
			res, err := cg.emitOpDispatch(block, fn, right, []value.Value{left})
			if err != nil {
				return nil, true, err
			}

			return cg.finishBinOpDispatch(block, e.Op, res), true, nil
		}

		// Same implicit-coerce fallback as above, on the swapped form.
		if fn, coerced := cg.findOpImplWithCoerce(block, structName, traitName, left, lt); fn != nil {
			res, err := cg.emitOpDispatch(block, fn, right, []value.Value{coerced})
			if err != nil {
				return nil, true, err
			}

			return cg.finishBinOpDispatch(block, e.Op, res), true, nil
		}
	}

	return nil, false, nil
}

// findOpImplWithCoerce looks for an op-trait impl on structName whose
// single param P accepts `val` via implicit[P] conversion.  Returns the
// impl fn and a coerced value of type P on success; (nil, nil) when no
// impl matches.  Only fires when the impl is unary-on-the-non-receiver
// (single param past the receiver) so the rhs is unambiguously the
// coercion target.
func (cg *CodeGen) findOpImplWithCoerce(
	block *ir.Block,
	structName, traitName string,
	val value.Value, valType irtypes.Type,
) (*ir.Func, value.Value) {
	key := structName + "/" + traitName

	for _, impl := range cg.opTraitImpls[key] {
		if len(impl.paramTypes) != 1 {
			continue
		}

		paramT := impl.paramTypes[0]
		if paramT.Equal(valType) {
			continue // direct lookup would have hit this
		}

		if !cg.canImplicitCoerce(valType, paramT) {
			continue
		}

		coerced := cg.coerce(block, val, paramT)
		if coerced.Type().Equal(paramT) {
			return impl.fn, coerced
		}
	}

	return nil, nil
}

// canImplicitCoerce reports whether src can be implicit[T]-converted to
// target.  Walks the implicit conversion registry; mirrors the lookup in
// coerce() but as a predicate so dispatchBinOp can pick an impl without
// firing side-effecting IR.
func (cg *CodeGen) canImplicitCoerce(src, target irtypes.Type) bool {
	targetName := cg.typeNameOf(target)
	if targetName == "" {
		return false
	}

	for _, entry := range cg.implicitConvFns[targetName] {
		if entry.srcLLVM.Equal(src) {
			return true
		}
	}

	return false
}

// finishBinOpDispatch post-processes the impl-method result for operators
// whose Tin semantics differ from the trait method's raw return value.
//
//	`!=`     : negate the bool returned by `comp`.
//	`<,<=,>,>=` : compare the integer returned by `ord` against 0.
//
// For the ord comparisons the constant 0 is coerced to the result's actual
// integer type so that an impl returning e.g. i32 (instead of the canonical
// i64) still produces well-typed IR.
func (cg *CodeGen) finishBinOpDispatch(block *ir.Block, op string, res value.Value) value.Value {
	if res == nil {
		return res
	}

	zero := func() value.Value {
		return cg.coerce(block, constant.NewInt(irtypes.I64, 0), res.Type())
	}

	switch op {
	case "!=":
		return block.NewICmp(enum.IPredEQ, res, constant.NewBool(false))
	case "<":
		return block.NewICmp(enum.IPredSLT, res, zero())
	case "<=":
		return block.NewICmp(enum.IPredSLE, res, zero())
	case ">":
		return block.NewICmp(enum.IPredSGT, res, zero())
	case ">=":
		return block.NewICmp(enum.IPredSGE, res, zero())
	}

	return res
}

// closedConstructorHint returns a comma-joined list of "<TypeName>.<m>(...)"
// for every static method of typeName, so the #closed diagnostic shows
// the actual entry points the user should call.
func (cg *CodeGen) closedConstructorHint(typeName string) string {
	pretty := cg.diagStructName(typeName)

	sd := cg.structDeclsByName[typeName]
	if sd == nil {
		// Try the bare name in case typeName is a monomorphized form
		// (Cell__i64) and the decl was registered under "Cell".
		if idx := strings.Index(typeName, "__"); idx > 0 {
			sd = cg.structDeclsByName[typeName[:idx]]
		}
	}

	if sd == nil {
		return "use a static constructor instead of a struct literal"
	}

	var names []string

	for _, m := range sd.Methods {
		if !m.IsStatic {
			continue
		}

		names = append(names, fmt.Sprintf("%s.%s(...)", pretty, m.Name))
	}

	if len(names) == 0 {
		return "use a static constructor instead of a struct literal"
	}

	return "e.g. " + strings.Join(names, " or ")
}
