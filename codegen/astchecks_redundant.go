package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkRedundantReturnCast(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.RetType == nil {
		return
	}
	// Single-name return type: `fn foo() T`.  Tuple returns are
	// special-cased so each slot can be checked independently.
	retSimple := simpleTypeName(fn.RetType)
	retTuple := tupleSlotTypes(fn.RetType)

	walkAST(fn.Body, func(n ast.Node) {
		rs, ok := n.(*ast.ReturnStmt)
		if !ok || rs.Value == nil {
			return
		}

		if retSimple != "" {
			cg.warnRedundantLiteralCast(rs.Value, retSimple)
		}

		if len(retTuple) > 0 {
			cg.checkTupleSlotCasts(rs.Value, retTuple)
		}

		cg.checkAdtVariantCasts(rs.Value, fn.RetType)
	})
}

// tupleSlotTypes returns the per-slot type names for `(T1, T2, ...)` /
// `Tuple[T1, T2]` shapes, or nil for any other type expression.  Names
// for non-SimpleType slots come back as "" so the caller skips them.
func tupleSlotTypes(t ast.TypeExpr) []string {
	gt, ok := t.(*ast.GenericType)
	if !ok {
		return nil
	}

	if !strings.HasPrefix(gt.Name, "Tuple") {
		return nil
	}

	out := make([]string, len(gt.TypeParams))
	for i, p := range gt.TypeParams {
		out[i] = simpleTypeName(p)
	}

	return out
}

// checkTupleSlotCasts walks a tuple literal value against the per-slot
// types and emits the redundant-cast warning for each `<lit> as Ti`
// element.  Non-tuple values short-circuit.
func (cg *CodeGen) checkTupleSlotCasts(value ast.Node, slots []string) {
	tl, ok := value.(*ast.TupleLit)
	if !ok || len(tl.Elems) != len(slots) {
		return
	}

	for i, el := range tl.Elems {
		if slots[i] == "" {
			continue
		}

		cg.warnRedundantLiteralCast(el, slots[i])
	}
}

// adtVariantFieldTypes resolves per-arg slot types when slot is a
// generic ADT instantiation (e.g. `Result[json5, Unit]`) and value is a
// variant constructor call (e.g. `Ok(...)`).  The variant's declared
// field types are looked up on the ADT decl, then type parameters are
// substituted from the slot's instantiation so the returned names
// reflect what the constructor's args must be.
//
// Returns nil for any unrecognized shape.
func (cg *CodeGen) adtVariantFieldTypes(slot ast.TypeExpr, value ast.Node) []string {
	gt, ok := slot.(*ast.GenericType)
	if !ok {
		return nil
	}
	// Tuple sugar already handled by tupleSlotTypes.
	if strings.HasPrefix(gt.Name, "Tuple") {
		return nil
	}

	call, ok := value.(*ast.CallExpr)
	if !ok {
		return nil
	}

	ctorName := ""
	if id, ok := call.Func.(*ast.Identifier); ok {
		ctorName = id.Name
	}

	if ctorName == "" {
		return nil
	}

	decl := cg.dataDeclFor(CanonKey(gt.Name))
	if decl == nil {
		return nil
	}

	if len(decl.TypeParams) != len(gt.TypeParams) {
		return nil
	}

	subst := make(map[string]string, len(decl.TypeParams))
	for i, tp := range decl.TypeParams {
		subst[tp] = simpleTypeName(gt.TypeParams[i])
	}

	for _, v := range decl.Variants {
		if v.Name != ctorName {
			continue
		}

		out := make([]string, len(v.Fields))
		for i, f := range v.Fields {
			n := simpleTypeName(f.Type)
			if sub, has := subst[n]; has && sub != "" {
				n = sub
			}

			out[i] = n
		}

		return out
	}

	return nil
}

// checkAdtVariantCasts walks a variant constructor call's args against
// the resolved field types and emits redundant-cast warnings for each
// `<lit> as Ti` that matches.  Recurses into nested constructor calls
// so deep shapes like `Ok(Some(42 as i64))` get checked at every
// level: the outer constructor pins Some's slot, Some pins i64's
// slot, and the inner literal cast fires the warning.
func (cg *CodeGen) checkAdtVariantCasts(value ast.Node, slot ast.TypeExpr) {
	args := cg.adtVariantFieldTypes(slot, value)
	if len(args) == 0 {
		return
	}

	call, ok := value.(*ast.CallExpr)
	if !ok || len(call.Args) != len(args) {
		return
	}

	for i, arg := range call.Args {
		if args[i] == "" {
			continue
		}
		// Direct slot: warn if the arg is `<lit> as Ti`.
		cg.warnRedundantLiteralCast(arg, args[i])
		// Recurse: when the slot is itself a generic ADT and arg is
		// another constructor call, the inner constructor's args are
		// pinned by the substituted variant field type.  Resolve
		// that and let checkAdtVariantCasts walk the inner call.
		recursiveSlot := cg.variantArgTypeExpr(slot, value, i)
		if recursiveSlot != nil {
			cg.checkAdtVariantCasts(arg, recursiveSlot)
		}
	}
}

// variantArgTypeExpr resolves the i-th constructor argument's slot
// TypeExpr by walking the ADT's variant fields and substituting the
// generic type parameters with the slot's instantiation arguments.
// For `Result[Option[i64], string]` and Ok's i=0 field declared as
// `t`, the substitution gives back `Option[i64]` -- the right input
// for a nested checkAdtVariantCasts call.  Returns nil for shapes
// the rule does not understand.
func (cg *CodeGen) variantArgTypeExpr(slot ast.TypeExpr, value ast.Node, i int) ast.TypeExpr {
	gt, ok := slot.(*ast.GenericType)
	if !ok {
		return nil
	}

	if strings.HasPrefix(gt.Name, "Tuple") {
		return nil
	}

	call, ok := value.(*ast.CallExpr)
	if !ok {
		return nil
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok {
		return nil
	}

	decl := cg.dataDeclFor(CanonKey(gt.Name))
	if decl == nil || len(decl.TypeParams) != len(gt.TypeParams) {
		return nil
	}

	subst := make(map[string]ast.TypeExpr, len(decl.TypeParams))
	for j, tp := range decl.TypeParams {
		subst[tp] = gt.TypeParams[j]
	}

	for _, v := range decl.Variants {
		if v.Name != id.Name {
			continue
		}

		if i >= len(v.Fields) {
			return nil
		}

		return substituteTypeParams(v.Fields[i].Type, subst)
	}

	return nil
}

// checkRedundantArgCasts inspects a function-call site and emits a
// redundant-cast warning for each `<lit> as T` arg whose target T
// matches the corresponding parameter's declared type.  The lookup
// uses resolveCalleeFuncDecl so qualified `pkg::fn(...)` calls and
// overload-mangled funcDecls keys both find the right declaration
// deterministically.  Method calls (FieldAccess) and dynamic-call
// shapes are still skipped because resolving the receiver / overload
// would require real type inference.
func (cg *CodeGen) checkRedundantArgCasts(call *ast.CallExpr) {
	if call == nil || len(call.Args) == 0 {
		return
	}

	switch call.Func.(type) {
	case *ast.Identifier, *ast.ScopeAccess:
	default:
		return
	}

	fn := cg.resolveCalleeFuncDecl(call)
	if fn == nil {
		return
	}

	// Walk min(args, params); a mismatched count is someone else's
	// problem (the call-arity error fires from genCallExpr).
	n := len(call.Args)
	if len(fn.Params) < n {
		n = len(fn.Params)
	}

	for i := 0; i < n; i++ {
		paramType := simpleTypeName(fn.Params[i].Type)
		if paramType == "" {
			continue
		}

		cg.warnRedundantLiteralCast(call.Args[i], paramType)
	}
}

// checkRedundantStructFieldCasts inspects a struct literal and emits
// a redundant-cast warning for each `<lit> as T` field initializer
// whose target T matches the corresponding declared field type.
// Both named (`Foo{x: ...}`) and positional (`Foo{1, 2}`) shapes are
// covered.  Uses the AST-time registry passed in because cg's own
// structDeclsByName is not populated until codegen, after this pass.
func (cg *CodeGen) checkRedundantStructFieldCasts(lit *ast.StructLit, astDecls map[string]*ast.StructDecl) {
	if lit == nil {
		return
	}

	decl := astDecls[lit.TypeName]
	if decl == nil {
		return
	}

	if len(lit.Fields) > 0 {
		// Build a name -> type map for named-field lookup.
		fieldType := make(map[string]string, len(decl.Fields))
		for _, f := range decl.Fields {
			fieldType[f.Name] = simpleTypeName(f.Type)
		}

		for _, f := range lit.Fields {
			ft := fieldType[f.Name]
			if ft == "" {
				continue
			}

			cg.warnRedundantLiteralCast(f.Value, ft)
		}

		return
	}
	// Positional: pair index-for-index against the decl's user fields.
	for i, val := range lit.Positional {
		if i >= len(decl.Fields) {
			break
		}

		ft := simpleTypeName(decl.Fields[i].Type)
		if ft == "" {
			continue
		}

		cg.warnRedundantLiteralCast(val, ft)
	}
}

// collectAwaitedOrSpawnedCalls scans every `await <call>` and
// `spawn <call>` in the program and returns the set of inner CallExpr
// pointers.  Used by the bare-async-call lints to skip sites where the
// user already wrapped the call in the canonical form.
