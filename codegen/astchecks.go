package codegen

// Cheap, syntax-directed checks. Each one walks the AST once and emits a
// diagnostic on a recognizable shape - no dataflow, no type inference
// beyond what the existing scope already knows.

import (
	"reflect"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// runAstChecks runs every syntax-level check over the program.
func (cg *CodeGen) runAstChecks(prog *ast.Program) {
	if cg.replMode {
		return
	}

	// AST-time struct registry.  cg.structDeclsByName populates during
	// codegen, *after* runAstChecks, so the redundant-cast check on
	// struct literals would otherwise see an empty map.  Walk the
	// program here to build a name -> decl map independent of codegen
	// ordering.  Imported structs are not visible here, but the bulk
	// of struct literals reference user-declared structs in the same
	// translation unit, so the check still has high coverage.
	astStructDecls := map[string]*ast.StructDecl{}

	for _, stmt := range prog.Stmts {
		walkAST(stmt, func(n ast.Node) {
			if sd, ok := n.(*ast.StructDecl); ok {
				astStructDecls[sd.Name] = sd
			}
		})
	}

	for _, n := range prog.Stmts {
		if fd, ok := n.(*ast.FuncDecl); ok {
			cg.checkInfiniteRecursion(fd)
			cg.checkFiberMisuse(fd)
			cg.checkUnguardedTraitDowncast(fd)
			cg.checkRedundantReturnCast(fd)
		}
		// Track whether the immediately-enclosing FuncDecl declares
		// a Result-shaped return type. The match-as-try lint only
		// fires when the enclosing fn returns Result, since `try`
		// otherwise can't compile inside it.
		prevReturnsResult := cg.curFnReturnsResult

		if fd, ok := n.(*ast.FuncDecl); ok {
			if astReturnTypeIsResult(fd.RetType) {
				cg.curFnReturnsResult++
			}
		}

		walkAST(n, func(node ast.Node) {
			switch e := node.(type) {
			case *ast.BinExpr:
				cg.checkIdenticalOperands(e)
				cg.checkArithIdentity(e)
				cg.checkFloatEqual(e)
			case *ast.AsExpr:
				cg.checkUselessCast(e)
			case *ast.IfStmt:
				cg.checkEmptyIfBody(e)
			case *ast.ForStmt:
				cg.checkLoopInvariant(e)
			case *ast.VarDecl:
				cg.checkRedundantArrayElemCasts(e)
			case *ast.CallExpr:
				cg.checkRedundantArgCasts(e)
			case *ast.StructLit:
				cg.checkRedundantStructFieldCasts(e, astStructDecls)
			case *ast.MatchStmt:
				cg.checkResultMatchAntipattern(e)
			}
		})

		cg.curFnReturnsResult = prevReturnsResult
	}

	cg.checkMagicNumbers(prog)
	cg.checkStyle(prog)
}

// checkRedundantReturnCast warns when a function with declared return
// type T returns `<literal> as T`.  The implicit return-coercion
// would auto-coerce the literal already; the explicit cast adds
// nothing.  Mirrors checkRedundantArrayElemCasts at the return-stmt
// granularity.
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

	decl, ok := cg.dataDecls[gt.Name]
	if !ok {
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

	decl, ok := cg.dataDecls[gt.Name]
	if !ok || len(decl.TypeParams) != len(gt.TypeParams) {
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

// checkRedundantArrayElemCasts walks a VarDecl whose annotated type
// pins the slot type and warns on `<literal> as T` where T already
// matches the slot.  Auto-coercion of literals to the declared slot
// type is unconditional in Tin -- the explicit cast adds nothing:
//
//	let a json5 = "hello"          // works without `as json5`
//	let x [u32; 2] = [0x1, 0x2]    // works without `as u32`
//
// Three slot shapes are checked:
//  1. `let x T = <lit> as T`           (single value)
//  2. `let x [T; N] = [<lit> as T,..]` (fixed array element)
//  3. `let x [T]    = [<lit> as T,..]` (fat array element)
//
// The check is intentionally conservative: only literals are flagged.
// A cast like `let x u32 = my_i64 as u32` is a real conversion and
// must keep the `as`.
func (cg *CodeGen) checkRedundantArrayElemCasts(vd *ast.VarDecl) {
	if vd.Type == nil || vd.Value == nil {
		return
	}

	// Single-value form: `let x T = <lit> as T`.
	if slot := simpleTypeName(vd.Type); slot != "" {
		cg.warnRedundantLiteralCast(vd.Value, slot)
	}

	// Tuple form: `let x (T1, T2) = (<lit1> as T1, <lit2> as T2)`.
	if slots := tupleSlotTypes(vd.Type); len(slots) > 0 {
		cg.checkTupleSlotCasts(vd.Value, slots)

		return
	}

	// Generic ADT form: `let x Result[T, E] = Ctor(<lit> as T)`.
	cg.checkAdtVariantCasts(vd.Value, vd.Type)

	// Array form: `let x [T] = [<lit> as T, ...]`.
	elemTypeName := arrayElemTypeName(vd.Type)
	if elemTypeName == "" {
		return
	}

	lit, ok := vd.Value.(*ast.ArrayLit)
	if !ok {
		return
	}

	for _, el := range lit.Elems {
		cg.warnRedundantLiteralCast(el, elemTypeName)
	}
}

// warnRedundantLiteralCast emits the redundant-type-cast warning when
// `expr` is the shape `<literal> as <slotType>` AND the literal would
// auto-coerce to the slot type without the cast.  The slot pin alone
// is not enough: `"hello" as i64` would (correctly) be rejected by the
// compiler because string has no coercion path to i64, so flagging
// that as redundant would be a lie.
func (cg *CodeGen) warnRedundantLiteralCast(expr ast.Node, slotType string) {
	ae, ok := expr.(*ast.AsExpr)
	if !ok {
		return
	}

	castTo := simpleTypeName(ae.Type)
	if castTo == "" || castTo != slotType {
		return
	}

	litKind := literalKind(ae.Expr)
	if litKind == "" {
		return
	}

	if !cg.literalKindCoercesTo(litKind, slotType) {
		return
	}

	cg.warn(DiagRedundantTypeCast, ae.Pos(),
		"redundant `as %s`: surrounding type already pins this slot to %s",
		castTo, castTo)
}

// literalKind classifies a literal node into a coarse kind for
// coercion checks.  Returns "" for non-literal expressions so the
// caller can short-circuit without the full target-type analysis.
func literalKind(n ast.Node) string {
	switch n.(type) {
	case *ast.IntLit:
		return "int"
	case *ast.FloatLit:
		return "float"
	case *ast.CharLit:
		return "char"
	case *ast.StringLit:
		return "string"
	case *ast.BoolLit:
		return "bool"
	case *ast.AtomLit:
		return "atom"
	}

	return ""
}

// literalKindCoercesTo encodes Tin's literal -> slot-type coercion
// matrix.  An int literal auto-coerces into any integer type and any
// float type; string into string; bool into bool; etc.  `any` accepts
// every literal kind.  Tagged unions (`type t = i64 | string | ...`)
// accept a literal whose kind matches at least one member.
//
// Returns false on unknown slot types so the warning never fires on
// shapes the rule does not understand.
func (cg *CodeGen) literalKindCoercesTo(litKind, slotType string) bool {
	if slotType == "any" {
		return true
	}
	// Direct primitive matches.
	switch slotType {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "byte":
		return litKind == "int" || litKind == "char"
	case "f32", "f64":
		return litKind == "float" || litKind == "int"
	case "string":
		return litKind == "string"
	case "bool":
		return litKind == "bool"
	case "char":
		return litKind == "char" || litKind == "int"
	case "atom":
		return litKind == "atom"
	}

	// Tagged-union slot: recurse over members.
	if members, ok := cg.unionTypeMembers[slotType]; ok {
		for _, mem := range members {
			memName := simpleTypeName(mem)
			if memName == "" {
				continue
			}

			if cg.literalKindCoercesTo(litKind, memName) {
				return true
			}
		}

		return false
	}

	return false
}

// arrayElemTypeName returns the user-visible element type name when t
// is a fat array (`[T]`) or fixed-size array (`[T; N]`).  Returns ""
// for any other shape.
func arrayElemTypeName(t ast.TypeExpr) string {
	if v, ok := t.(*ast.ArrayType); ok {
		return simpleTypeName(v.Elem)
	}

	return ""
}

// checkUnguardedTraitDowncast walks the function body and warns when
// `expr as *Concrete` downcasts a trait pointer to a concrete struct
// pointer without a same-type `expr is *Concrete` guard in the
// enclosing control-flow path.
//
// The canonical safe pattern is:
//
//	if e is *FlagError:
//	  let fe = e as *FlagError
//	  ...
//
// The check tracks the (identifier, target struct name) pairs that have
// been guarded by an `is` test along the current path.  When we see a
// matching `as` outside any guarded region, we warn at the cast site.
//
// The walk is conservative: only `is` checks at the *root* of an if
// condition (or as one side of an `&&` chain rooted at the condition)
// are considered guards.  More elaborate flow does not extend the
// guarded set, so the check trades recall for zero false positives in
// the long-tail.
func (cg *CodeGen) checkUnguardedTraitDowncast(fn *ast.FuncDecl) {
	if fn.Body == nil {
		return
	}

	type guard struct {
		varName    string
		structName string
	}

	var (
		guarded []guard
		walk    func(ast.Node)
	)

	hasGuard := func(varName, structName string) bool {
		for _, g := range guarded {
			if g.varName == varName && g.structName == structName {
				return true
			}
		}

		return false
	}

	// extractIsGuards walks an if-condition expression and yields each
	// `ident is *Concrete` it finds at the top level or under a chain of
	// `&&` operators.  Other shapes (||, !, etc.) are deliberately
	// ignored because they don't pin the dynamic type along the Then
	// branch.
	extractIsGuards := func(cond ast.Node) []guard {
		var (
			out  []guard
			walk func(n ast.Node)
		)

		walk = func(n ast.Node) {
			switch v := n.(type) {
			case *ast.BinExpr:
				if v.Op == "&&" {
					walk(v.Left)
					walk(v.Right)
				}
			case *ast.IsExpr:
				id, ok := v.Expr.(*ast.Identifier)
				if !ok {
					return
				}

				pt, ok := v.Type.(*ast.PointerType)
				if !ok {
					return
				}

				sn := simpleTypeName(pt.Elem)
				if sn == "" {
					return
				}

				out = append(out, guard{varName: id.Name, structName: sn})
			}
		}

		walk(cond)

		return out
	}

	// Build a syntactic name -> Tin TypeExpr map from the parameters
	// and any explicitly-typed `let name T = ...` bindings reachable in
	// the body.  Codegen scopes are not populated yet at AST-check time,
	// so we cannot use staticTypeOf here -- this map is the local
	// substitute and is intentionally narrow (we want zero false
	// positives).
	typeOfName := map[string]ast.TypeExpr{}

	for _, p := range fn.Params {
		if p.Type != nil {
			typeOfName[p.Name] = p.Type
		}
	}

	walkAST(fn.Body, func(n ast.Node) {
		if ld, ok := n.(*ast.VarDecl); ok && ld.Type != nil {
			typeOfName[ld.Name] = ld.Type
		}
	})

	// isKnownTrait reports whether `name` (possibly module-qualified
	// like `errors::Err`) refers to a registered trait.  cg.traits is
	// keyed by bare trait name, so we strip the module prefix before
	// looking up.
	isKnownTrait := func(name string) bool {
		bare := name
		if i := strings.LastIndex(name, "::"); i >= 0 {
			bare = name[i+2:]
		}

		_, ok := cg.traits[bare]

		return ok
	}

	// isKnownStruct mirrors isKnownTrait for the structTypes registry.
	// Cross-package struct names are stored mangled with `__` instead
	// of `::`, so we try the original SimpleType name, the bare suffix,
	// and the `::` -> `__` substitution before giving up.
	isKnownStruct := func(name string) bool {
		if _, ok := cg.structTypes[name]; ok {
			return true
		}

		mangled := strings.ReplaceAll(name, "::", "__")
		if _, ok := cg.structTypes[mangled]; ok {
			return true
		}

		bare := name
		if i := strings.LastIndex(name, "::"); i >= 0 {
			bare = name[i+2:]
		}

		_, ok := cg.structTypes[bare]

		return ok
	}

	// nameRefersToTraitPointer reports whether `name`'s declared
	// syntactic type is `*Trait` (pointer-to-trait).  Value-form trait
	// downcasts to a pointer struct are a hard error in genAsExpr, so
	// the unguarded-downcast warning only needs to consider the legal
	// pointer-to-pointer shape here.
	nameRefersToTraitPointer := func(name string) bool {
		t, ok := typeOfName[name]
		if !ok {
			return false
		}

		pt, isPtr := t.(*ast.PointerType)
		if !isPtr {
			return false
		}

		return isKnownTrait(simpleTypeName(pt.Elem))
	}

	// asTargetsTraitDowncast reports whether e is the canonical
	// trait-downcast shape: `ident as *Concrete` where ident is of
	// trait or trait-pointer type and Concrete is a known struct.
	// Other shapes are either compile errors (e.g. `*Trait as
	// Concrete`, handled in genAsExpr) or legal coercions, so the
	// warning intentionally only fires on the form that is *legal but
	// unchecked* without a guard.
	asTargetsTraitDowncast := func(e *ast.AsExpr) (varName, structName string, ok bool) {
		id, isIdent := e.Expr.(*ast.Identifier)
		if !isIdent {
			return "", "", false
		}

		pt, isPtr := e.Type.(*ast.PointerType)
		if !isPtr {
			return "", "", false
		}

		sn := simpleTypeName(pt.Elem)
		if sn == "" {
			return "", "", false
		}
		// Source must be a trait pointer.
		if !nameRefersToTraitPointer(id.Name) {
			return "", "", false
		}
		// Target must be a known struct, not another trait or primitive.
		if isKnownTrait(sn) {
			return "", "", false
		}

		if !isKnownStruct(sn) {
			return "", "", false
		}

		return id.Name, sn, true
	}

	walk = func(n ast.Node) {
		if n == nil {
			return
		}

		switch v := n.(type) {
		case *ast.IfStmt:
			added := extractIsGuards(v.Cond)
			before := len(guarded)
			guarded = append(guarded, added...)

			walk(v.Then)

			guarded = guarded[:before]
			// Else branches don't inherit Then-side guards.
			if v.Else != nil {
				walk(v.Else)
			}
		case *ast.AsExpr:
			if vn, sn, ok := asTargetsTraitDowncast(v); ok && !hasGuard(vn, sn) {
				cg.warn(DiagUnguardedTraitDowncast, v.Pos(),
					"downcast `%s as *%s` from a trait pointer is unchecked; "+
						"guard with `if %s is *%s:` first or accept that a "+
						"type mismatch produces a wild pointer",
					vn, sn, vn, sn)
			}

			walk(v.Expr)
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		default:
			// Generic walk: visit children via reflection-style traversal.
			walkAST(v, func(child ast.Node) {
				switch c := child.(type) {
				case *ast.IfStmt:
					walk(c)
				case *ast.AsExpr:
					if vn, sn, ok := asTargetsTraitDowncast(c); ok && !hasGuard(vn, sn) {
						cg.warn(DiagUnguardedTraitDowncast, c.Pos(),
							"downcast `%s as *%s` from a trait pointer is unchecked; "+
								"guard with `if %s is *%s:` first or accept that a "+
								"type mismatch produces a wild pointer",
							vn, sn, vn, sn)
					}
				}
			})
		}
	}

	walk(fn.Body)
}

// simpleTypeName extracts the user-visible name from a SimpleType.
// Module-qualified names (`errors::Err`) are stored as SimpleType with
// the `::` already in Name, so a single case covers both shapes.
// Returns "" for any other TypeExpr (generic, array, pointer, etc.).
func simpleTypeName(t ast.TypeExpr) string {
	if v, ok := t.(*ast.SimpleType); ok {
		return v.Name
	}

	return ""
}

// checkInfiniteRecursion flags a `f(x, y) = ... f(x, y) ...` where the
// recursive call passes the same arguments as the parameters and there's
// no observable change to those arguments before the call. Catches the
// classic typo where the user forgot to decrement a counter.
//
// The check is conservative: it only fires when EVERY function call to
// itself in the body uses identical-shaped args, which keeps it from
// flagging legitimate recursion that wraps a base-case branch.
func (cg *CodeGen) checkInfiniteRecursion(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	if len(fn.Params) == 0 {
		return
	}

	var (
		anySelfCall  bool
		allIdentical = true
		firstCallPos ast.Pos
		firstSeen    bool
	)

	walkAST(fn.Body, func(n ast.Node) {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}

		id, ok := c.Func.(*ast.Identifier)
		if !ok || id.Name != fn.Name {
			return
		}

		anySelfCall = true

		if !firstSeen {
			firstCallPos = c.Pos()
			firstSeen = true
		}

		if len(c.Args) != len(fn.Params) {
			allIdentical = false

			return
		}

		for i, arg := range c.Args {
			pid, ok := arg.(*ast.Identifier)
			if !ok || pid.Name != fn.Params[i].Name {
				allIdentical = false

				return
			}
		}
	})

	if !anySelfCall || !allIdentical {
		return
	}

	cg.warn(DiagInfiniteRecursion, firstCallPos,
		"recursive call to %q passes the same arguments as its parameters; "+
			"the recursion never makes progress", fn.Name)
}

// checkIdenticalOperands flags `x == x`, `x != x`, `x - x`, etc., where
// both sides are the same syntactic expression. For floats the rewrite
// would be unsound (NaN), so the check is skipped on float operands AND
// on operands whose type can't yet be statically determined -- runAstChecks
// runs before scope is fully built, and exprIsFloat conservatively returns
// false for unknown identifiers; without the explicit unknown-skip we'd
// fire on `let v = 0.0/0.0; if v != v: ...` and miss the NaN check the
// programmer wrote.
func (cg *CodeGen) checkIdenticalOperands(e *ast.BinExpr) {
	// Logical ops where the duplicated operand is redundant -- `x && x`
	// is just `x`, `x || x` is just `x`. Side-effect-free identifiers
	// only; a duplicated CallExpr could be intentional (idempotency
	// check) and we have no purity info this early in the pipeline.
	if e.Op == "&&" || e.Op == "||" {
		if astEqual(e.Left, e.Right) && isPureForDuplicateCheck(e.Left) {
			cg.warn(DiagIdenticalOperands, e.Pos(),
				"both sides of %q are identical; the duplicate is redundant", e.Op)
		}
		// `x && !x` / `x || !x`: contradictions/tautologies.
		if neg, ok := e.Right.(*ast.UnaryExpr); ok && (neg.Op == "!" || neg.Op == "not") {
			if astEqual(e.Left, neg.Expr) && isPureForDuplicateCheck(e.Left) {
				cg.warnTautologyAndOr(e)
			}
		}

		if neg, ok := e.Left.(*ast.UnaryExpr); ok && (neg.Op == "!" || neg.Op == "not") {
			if astEqual(neg.Expr, e.Right) && isPureForDuplicateCheck(e.Right) {
				cg.warnTautologyAndOr(e)
			}
		}

		return
	}

	switch e.Op {
	case "==", "!=", "<", "<=", ">", ">=", "-", "&", "|", "^", "/", "%":
	default:
		return
	}

	if !astEqual(e.Left, e.Right) {
		return
	}
	// Float operand -> keep silent; NaN makes != / == meaningful.
	if cg.exprIsFloat(e.Left) {
		return
	}
	// Unknown type (typically: identifier whose let-binding hasn't been
	// resolved yet) -> skip rather than risk a false positive on a float.
	if cg.staticTypeOf(e.Left) == nil {
		return
	}

	cg.warn(DiagIdenticalOperands, e.Pos(),
		"both sides of %q are identical; result is constant", e.Op)
}

// warnTautologyAndOr fires the boolean-fold warning for `x && !x`
// (always false) or `x || !x` (always true).
func (cg *CodeGen) warnTautologyAndOr(e *ast.BinExpr) {
	val := e.Op == "||"
	cg.warn(DiagBoolAnalysis, e.Pos(),
		"%q with operand and its negation is always %v", e.Op, val)
}

// isPureForDuplicateCheck reports whether an expression is safe to
// flag as a duplicate operand without risking a false positive on a
// side-effecting call. Conservative: only allows identifiers, literals,
// field accesses, and unary/binary trees over those.
func isPureForDuplicateCheck(n ast.Node) bool {
	switch e := n.(type) {
	case *ast.Identifier, *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit, *ast.NilLit:
		return true
	case *ast.FieldAccess:
		return isPureForDuplicateCheck(e.Expr)
	case *ast.UnaryExpr:
		return isPureForDuplicateCheck(e.Expr)
	case *ast.BinExpr:
		return isPureForDuplicateCheck(e.Left) && isPureForDuplicateCheck(e.Right)
	}

	return false
}

// checkArithIdentity flags arithmetic / bitwise ops that fold to a known
// constant (or are no-ops) because one operand is a saturating identity:
// x & 0, x * 0, x + 0, x | -1, etc.
func (cg *CodeGen) checkArithIdentity(e *ast.BinExpr) {
	lConst := cg.tryFoldExpr(e.Left)
	rConst := cg.tryFoldExpr(e.Right)

	chk := func(c foldedValue, side string) {
		if c.kind != foldInt {
			return
		}

		switch e.Op {
		case "&":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s & 0 is always 0", side)
			}
		case "|":
			if c.intVal == -1 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s | -1 is always -1", side)
			}
		case "*":
			switch c.intVal {
			case 0:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s * 0 is always 0", side)
			case 1:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s * 1 is a no-op", side)
			}
		case "+":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s + 0 is a no-op", side)
			}
		case "-":
			if c.intVal == 0 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x - 0 is a no-op")
			}
		case "/":
			if c.intVal == 1 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x / 1 is a no-op")
			}
		case "<<", ">>":
			if c.intVal == 0 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"shift by 0 is a no-op")
			}
		}
	}

	if lConst.kind == foldInt && rConst.kind != foldInt {
		chk(lConst, "0")
	}

	if rConst.kind == foldInt && lConst.kind != foldInt {
		chk(rConst, "x")
	}
}

// checkFloatEqual fires on `==` / `!=` between float operands. The
// warning is default-off because direct float equality is sometimes
// intentional (NaN canary, exact zero check).
func (cg *CodeGen) checkFloatEqual(e *ast.BinExpr) {
	if e.Op != "==" && e.Op != "!=" {
		return
	}

	if !cg.exprIsFloat(e.Left) && !cg.exprIsFloat(e.Right) {
		return
	}

	cg.warn(DiagFloatEqual, e.Pos(),
		"direct equality on floats is fragile; consider `abs(a - b) < eps`")
}

// checkUselessCast flags `x as T` when x is statically already T.
func (cg *CodeGen) checkUselessCast(e *ast.AsExpr) {
	src := cg.staticTypeOf(e.Expr)
	if src == nil {
		return
	}

	dst, err := cg.tinTypeToLLVM(e.Type)
	if err != nil || dst == nil {
		return
	}

	if !src.Equal(dst) {
		return
	}

	cg.warn(DiagUselessCast, e.Pos(),
		"cast to %s has no effect; the expression is already %s", dst, src)
}

// checkEmptyIfBody flags `if x: { }` or `else: { }` where the block is
// empty. Almost always an unfinished edit. An explicit `pass` keyword is
// the user telling us the empty body is intentional, so we suppress.
// checkResultMatchAntipattern walks a two-arm `match ...: case Ok / case Err`
// and suggests the shorter Result-method form when one fits:
//
//   - `return Err(...)` propagation         -> `let x = try expr`
//   - `panic(...)` on Err                   -> `expr.unwrap()` or .expect
//   - return-a-default on Err               -> `expr.unwrap_or(default)`
//   - `Ok(f(v))` / `Err(passthrough)`        -> `expr.map(f)`
//   - `Err(g(e))` / `Ok(passthrough)`        -> `expr.map_err(g)`
//
// Stays conservative: skips guards, default arms, anything that's not
// the two-arm Ok/Err shape, and Ok bodies with control flow.
func (cg *CodeGen) checkResultMatchAntipattern(s *ast.MatchStmt) {
	if s == nil || len(s.Cases) != 2 || s.Default != nil {
		return
	}

	var okCase, errCase *ast.MatchCase

	for i := range s.Cases {
		c := &s.Cases[i]
		if c.Guard != nil {
			return
		}

		name := patternVariantName(c.Pattern)
		switch name {
		case "Ok":
			if okCase != nil {
				return
			}

			okCase = c
		case "Err":
			if errCase != nil {
				return
			}

			errCase = c
		default:
			return
		}
	}

	if okCase == nil || errCase == nil {
		return
	}

	okBinder := singleBinderOf(okCase.Pattern)
	errBinder := singleBinderOf(errCase.Pattern)

	// Order matters: more-specific lints win over the generic try
	// propagation. Each call emits at most one diagnostic.
	if cg.matchSuggestsUnwrap(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsExpect(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsUnwrapOr(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsMap(s, okCase, errCase, okBinder, errBinder) {
		return
	}

	if cg.matchSuggestsMapErr(s, okCase, errCase, okBinder, errBinder) {
		return
	}

	cg.matchSuggestsTry(s, okCase, errCase)
}

// singleBinderOf returns the single binder name in an Ok(x) / Err(e)
// pattern, or "" if the pattern doesn't bind exactly one name (e.g.
// `Ok(_)`, `Ok()`, `Err`, `Err(_, _)`).
func singleBinderOf(pat ast.Node) string {
	call, ok := pat.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return ""
	}

	id, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return ""
	}

	if id.Name == "_" {
		return ""
	}

	return id.Name
}

// matchSuggestsUnwrap fires for `case Ok(v): v / case Err(_): panic(...)`,
// or the Ok-arm assignment variant that reduces to the same shape.
// .unwrap() panics with a fixed "result::unwrap on Err" message; if the
// user wants their own text, .expect("msg") covers it.
func (cg *CodeGen) matchSuggestsUnwrap(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockPanicsConstant(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` panics on Err and yields the Ok value -- prefer `expr.unwrap()` (or `expr.expect(\"msg\")` for a custom panic message)")

	return true
}

// matchSuggestsExpect fires for the unwrap shape above but where the
// panic message is a non-trivial expression -- still .expect-shaped.
func (cg *CodeGen) matchSuggestsExpect(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockPanicsArbitrary(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` panics on Err and yields the Ok value -- prefer `expr.expect(\"<message>\")`")

	return true
}

// matchSuggestsUnwrapOr fires when both arms produce a final value of
// the same Ok type: the Err arm yields a default, the Ok arm yields
// the bound value untouched.
func (cg *CodeGen) matchSuggestsUnwrapOr(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockReturnsSomeNonErrValue(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` returns a default on Err and the bound value on Ok -- prefer `expr.unwrap_or(default)`")

	return true
}

// matchSuggestsMap fires for `Ok(v): Ok(f(v))` / `Err(m): Err(m)` --
// pure transformation of the Ok payload.
func (cg *CodeGen) matchSuggestsMap(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder, errBinder string) bool {
	if okBinder == "" || errBinder == "" {
		return false
	}

	if !blockReturnsCtor(errCase.Body, "Err", errBinder) {
		return false
	}

	if !blockReturnsCtorOfTransform(okCase.Body, "Ok", okBinder) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` transforms only the Ok payload -- prefer `expr.map(fn(v) ...)`")

	return true
}

// matchSuggestsMapErr fires for `Ok(v): Ok(v)` / `Err(m): Err(g(m))`.
func (cg *CodeGen) matchSuggestsMapErr(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder, errBinder string) bool {
	if okBinder == "" || errBinder == "" {
		return false
	}

	if !blockReturnsCtor(okCase.Body, "Ok", okBinder) {
		return false
	}

	if !blockReturnsCtorOfTransform(errCase.Body, "Err", errBinder) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` transforms only the Err payload -- prefer `expr.map_err(fn(e) ...)`")

	return true
}

// matchSuggestsTry is the fallback "looks like err-propagation"
// catch.  Only fires inside a function whose declared return type is
// `Result[_, _]` and whose Err arm propagates the error verbatim
// (e.g. `return Err(e)`).  Without the return-type guard the lint
// was firing on any match-with-early-return-on-Err, including ones
// where the enclosing function isn't even a Result-returner -- there
// `try` wouldn't compile.
func (cg *CodeGen) matchSuggestsTry(s *ast.MatchStmt, okCase, errCase *ast.MatchCase) {
	if cg.curFnReturnsResult == 0 {
		return
	}

	if !blockReturnsErrPassthrough(errCase.Body) {
		return
	}

	if !okArmIsRewriteableToTry(okCase.Body) {
		return
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` on a Result propagates the error verbatim -- prefer `let x = try expr` (or `try expr.map_err(...)` when the error type needs to change)")
}

// blockReturnsOnlyTheBinder returns true when b is a single
// `return BINDER` (or just `BINDER` as the arm-result expression).
func blockReturnsOnlyTheBinder(b *ast.Block, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		if id, ok := v.Value.(*ast.Identifier); ok {
			return id.Name == binder
		}
	case *ast.ExprStmt:
		if id, ok := v.Expr.(*ast.Identifier); ok {
			return id.Name == binder
		}
	}

	return false
}

// blockPanicsConstant returns true when b's only statement is a
// `panic(<string literal or interpolated string>)` call.
func blockPanicsConstant(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	es, ok := b.Stmts[0].(*ast.ExprStmt)
	if !ok {
		return false
	}

	call, ok := es.Expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "panic" {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	switch call.Args[0].(type) {
	case *ast.StringLit, *ast.InterpolatedString:
		return true
	}

	return false
}

// blockPanicsArbitrary returns true when b's only statement is a
// `panic(<anything>)` call.  Used after blockPanicsConstant has
// already taken the simpler shape.
func blockPanicsArbitrary(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	es, ok := b.Stmts[0].(*ast.ExprStmt)
	if !ok {
		return false
	}

	call, ok := es.Expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "panic" {
		return false
	}

	return true
}

// blockReturnsSomeNonErrValue returns true when b is a single
// `return <expr>` where the expression is NOT `Err(...)` -- the
// shape of a default-value fallback.
func blockReturnsSomeNonErrValue(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	ret, ok := b.Stmts[0].(*ast.ReturnStmt)
	if !ok || ret.Value == nil {
		return false
	}

	if call, isCall := ret.Value.(*ast.CallExpr); isCall {
		if id, ok2 := call.Func.(*ast.Identifier); ok2 && id.Name == "Err" {
			return false
		}
	}

	return true
}

// blockReturnsCtor returns true when b is exactly `return CTOR(BINDER)` /
// `CTOR(BINDER)` (arm-result form).  Used to identify pass-through arms.
func blockReturnsCtor(b *ast.Block, ctor, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	var expr ast.Node

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		expr = v.Value
	case *ast.ExprStmt:
		expr = v.Expr
	default:
		return false
	}

	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != ctor {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	argID, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return false
	}

	return argID.Name == binder
}

// blockReturnsCtorOfTransform returns true when b is `return CTOR(<expr
// not equal to bare BINDER>)` -- the binder is referenced but
// transformed inside the constructor.
func blockReturnsCtorOfTransform(b *ast.Block, ctor, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	var expr ast.Node

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		expr = v.Value
	case *ast.ExprStmt:
		expr = v.Expr
	default:
		return false
	}

	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != ctor {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}
	// Bail if the arg is just the bare binder (that's the pass-through
	// case handled by blockReturnsCtor).
	if argID, ok2 := call.Args[0].(*ast.Identifier); ok2 && argID.Name == binder {
		return false
	}
	// Must reference the binder somewhere; a constant-replacement
	// arm isn't really a `.map(...)` -- it's a different shape.
	return astReferencesIdentifier(call.Args[0], binder)
}

// blockReturnsErrPassthrough returns true when b is `return Err(<binder>)`
// where the Err's argument is some identifier -- the shape of a
// straight error propagation that `try` replaces.
func blockReturnsErrPassthrough(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	ret, ok := b.Stmts[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}

	call, ok := ret.Value.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "Err" {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	_, isID := call.Args[0].(*ast.Identifier)

	return isID
}

// astReturnTypeIsResult reports whether a FuncDecl.RetType is a
// `Result[_, _]` shape. Recognized: bare `Result` (no params) and
// `Result[T, E]`, possibly qualified as `result::Result`. Used by
// the match-as-try lint to gate its noisiness.
func astReturnTypeIsResult(t ast.TypeExpr) bool {
	if t == nil {
		return false
	}

	switch tt := t.(type) {
	case *ast.SimpleType:
		return tt.Name == "Result" || tt.Name == "result::Result"
	case *ast.GenericType:
		return tt.Name == "Result" || tt.Name == "result::Result"
	}

	return false
}

// astReferencesIdentifier walks expr and reports whether any
// sub-expression is an Identifier whose name matches `name`.
func astReferencesIdentifier(expr ast.Node, name string) bool {
	found := false

	walkAST(expr, func(n ast.Node) {
		if found {
			return
		}

		if id, ok := n.(*ast.Identifier); ok && id.Name == name {
			found = true
		}
	})

	return found
}

// patternVariantName extracts the variant constructor name from a
// match pattern, e.g. "Ok" from `Ok(v)`, "Err" from `Err(_)`. Returns
// "" for patterns that aren't variant ctors.
func patternVariantName(p ast.Node) string {
	switch pat := p.(type) {
	case *ast.CallExpr:
		if id, ok := pat.Func.(*ast.Identifier); ok {
			return id.Name
		}
	case *ast.Identifier:
		// Niladic variant like `case None:` -- still has a name.
		return pat.Name
	}

	return ""
}

// okArmIsRewriteableToTry returns true when the Ok arm's body is the
// trivial "use the bound value" shape that `try` would replace. Keeps
// the lint conservative: complex bodies stay un-flagged.
func okArmIsRewriteableToTry(b *ast.Block) bool {
	if b == nil || len(b.Stmts) == 0 || len(b.Stmts) > 3 {
		return false
	}

	for _, s := range b.Stmts {
		switch s.(type) {
		case *ast.AssignStmt, *ast.VarDecl, *ast.ExprStmt, *ast.ReturnStmt:
			continue
		default:
			return false
		}
	}

	return true
}

func (cg *CodeGen) checkEmptyIfBody(s *ast.IfStmt) {
	if s.Then != nil && len(s.Then.Stmts) == 0 && !s.Then.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty if body")
	}

	if s.Else != nil && len(s.Else.Stmts) == 0 && !s.Else.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty else body")
	}
}

// exprIsFloat reports whether expr's static type is f32 or f64. Conservative
// (returns false on unknown), so the check never fires when in doubt.
func (cg *CodeGen) exprIsFloat(expr ast.Node) bool {
	t := cg.staticTypeOf(expr)
	if t == nil {
		return false
	}

	return irtypes.IsFloat(t)
}

// checkLoopInvariant flags pure expressions inside a loop body whose
// operands are never written (or address-taken) within the body or post
// statements. The hint suggests hoisting the expression before the loop.
//
// Only arithmetic/bitwise/comparison/boolean/unary/field/cast trees over
// identifiers and literals are considered: anything reached through a
// call, pointer deref, or indexed read may observe state we don't
// statically prove invariant, so we leave those alone.
//
// Emits at the maximal invariant subtree -- if `(a + b) * c` is fully
// invariant, fires once on the whole expression rather than separately on
// each subterm.
func (cg *CodeGen) checkLoopInvariant(loop *ast.ForStmt) {
	if loop.Body == nil || len(loop.Body.Stmts) == 0 {
		return
	}

	mutated := map[string]bool{}

	collectLoopMutations(loop.Body, mutated)

	if loop.Post != nil {
		collectLoopMutations(loop.Post, mutated)
	}

	if loop.VarName != "" {
		mutated[loop.VarName] = true
	}

	cg.walkLicm(loop.Body, false, mutated)
}

// walkLicm descends through n's children. When it finds a BinExpr or
// UnaryExpr that is fully loop-invariant and references at least one
// identifier (so the optimizer can't trivially fold it), it warns -- but
// only at the outermost such subtree, by passing parentInv=true to
// children of an emitted node so they suppress their own emit.
//
// Nested ForStmt and LambdaExpr bodies are skipped: nested loops get
// their own checkLoopInvariant pass, and lambdas may be invoked outside
// the loop where "loop-invariant" no longer applies.
func (cg *CodeGen) walkLicm(n ast.Node, parentInv bool, mutated map[string]bool) {
	if n == nil {
		return
	}
	// Same typed-nil guard as walkAST: an interface value can wrap a nil
	// concrete pointer (e.g. *ast.Block) which `n == nil` misses, and
	// dereferencing it (n.Pos(), or any field access) would segfault.
	if rv := reflect.ValueOf(n); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return
	}

	switch n.(type) {
	case *ast.ForStmt, *ast.LambdaExpr:
		return
	}

	isInv := false

	switch n.(type) {
	case *ast.BinExpr, *ast.UnaryExpr:
		if isLoopInvariantExpr(n, mutated) && containsIdentifier(n) {
			isInv = true

			if !parentInv {
				cg.warn(DiagLoopInvariant, n.Pos(),
					"expression does not depend on loop state; consider hoisting it before the loop")
			}
		}
	}

	switch e := n.(type) {
	case *ast.Block:
		for _, s := range e.Stmts {
			cg.walkLicm(s, false, mutated)
		}
	case *ast.IfStmt:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)

		for _, ei := range e.ElseIfs {
			cg.walkLicm(ei.Cond, false, mutated)
			cg.walkLicm(ei.Body, false, mutated)
		}

		cg.walkLicm(e.Else, false, mutated)
	case *ast.MatchStmt:
		cg.walkLicm(e.Expr, false, mutated)

		for _, c := range e.Cases {
			cg.walkLicm(c.Pattern, false, mutated)
			cg.walkLicm(c.Guard, false, mutated)
			cg.walkLicm(c.Body, false, mutated)
		}

		cg.walkLicm(e.Default, false, mutated)
	case *ast.AssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.AugAssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.PostfixStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.VarDecl:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ReturnStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.EchoStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ExprStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.DeferStmt:
		cg.walkLicm(e.Call, false, mutated)
	case *ast.BinExpr:
		cg.walkLicm(e.Left, isInv, mutated)
		cg.walkLicm(e.Right, isInv, mutated)
	case *ast.UnaryExpr:
		cg.walkLicm(e.Expr, isInv, mutated)
	case *ast.CallExpr:
		cg.walkLicm(e.Func, false, mutated)

		for _, a := range e.Args {
			cg.walkLicm(a, false, mutated)
		}
	case *ast.IndexExpr:
		cg.walkLicm(e.Expr, false, mutated)
		cg.walkLicm(e.Index, false, mutated)
	case *ast.FieldAccess:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AsExpr:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AwaitExpr:
		cg.walkLicm(e.Future, false, mutated)
	case *ast.SpawnExpr:
		cg.walkLicm(e.Call, false, mutated)
		cg.walkLicm(e.DoBlock, false, mutated)
	case *ast.TernaryExpr:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)
		cg.walkLicm(e.Else, false, mutated)
	case *ast.TaggedBlock:
		cg.walkLicm(e.Body, false, mutated)
	}
}

// isLoopInvariantExpr reports whether n is a pure expression all of
// whose identifier operands are not in the mutated set. Conservative:
// returns false for any node kind that could reach a side-effecting
// operation (calls, derefs, indexing, address-of, await, spawn).
func isLoopInvariantExpr(n ast.Node, mutated map[string]bool) bool {
	if n == nil {
		return true
	}

	switch e := n.(type) {
	case *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit, *ast.NilLit:
		return true
	case *ast.Identifier:
		return !mutated[e.Name]
	case *ast.FieldAccess:
		return isLoopInvariantExpr(e.Expr, mutated)
	case *ast.UnaryExpr:
		return isPureUnaryOp(e.Op) && isLoopInvariantExpr(e.Expr, mutated)
	case *ast.BinExpr:
		return isPureBinOp(e.Op) &&
			isLoopInvariantExpr(e.Left, mutated) &&
			isLoopInvariantExpr(e.Right, mutated)
	case *ast.AsExpr:
		return isLoopInvariantExpr(e.Expr, mutated)
	}

	return false
}

func isPureBinOp(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%",
		"&", "|", "^", "<<", ">>",
		"==", "!=", "<", "<=", ">", ">=",
		"&&", "||":
		return true
	}

	return false
}

func isPureUnaryOp(op string) bool {
	switch op {
	case "-", "+", "!", "~", "not":
		return true
	}

	return false
}

// containsIdentifier reports whether n's subtree contains at least one
// *ast.Identifier. Used by the LICM check so we don't emit on pure
// constant expressions like `1 + 2` -- the optimizer folds those without
// a programmer-visible improvement.
func containsIdentifier(n ast.Node) bool {
	found := false

	walkAST(n, func(c ast.Node) {
		if _, ok := c.(*ast.Identifier); ok {
			found = true
		}
	})

	return found
}

// collectLoopMutations records every name that is written, address-
// taken, or rebound inside the loop body. Conservative on every front:
// any `&x`, any assignment-target chain rooted at an Identifier, and any
// VarDecl introduces the bound name into the mutated set. CallExpr args
// of the form `&x` also mark x, since the callee can mutate through the
// address.
func collectLoopMutations(body ast.Node, mutated map[string]bool) {
	walkAST(body, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.AssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.AugAssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.PostfixStmt:
			markBaseIdent(e.Expr, mutated)
		case *ast.VarDecl:
			mutated[e.Name] = true
		case *ast.AddrExpr:
			markBaseIdent(e.Val, mutated)
		case *ast.AddressOfExpr:
			markBaseIdent(e.Expr, mutated)
		}
	})
}

// markBaseIdent walks an l-value chain (FieldAccess / IndexExpr /
// DerefExpr) down to its root identifier and records the name. A write
// through `obj.field`, `arr[i]`, or `*p` means we can no longer prove
// `obj`, `arr`, or `p` is loop-invariant, so the entire base name is
// marked.
func markBaseIdent(n ast.Node, m map[string]bool) {
	for n != nil {
		switch e := n.(type) {
		case *ast.Identifier:
			m[e.Name] = true

			return
		case *ast.FieldAccess:
			n = e.Expr
		case *ast.IndexExpr:
			n = e.Expr
		case *ast.DerefExpr:
			n = e.Expr
		default:
			return
		}
	}
}

// checkMagicNumbers flags int and float literals that aren't in the
// universal exempt set ({-1, 0, 1, 2}) and aren't in a context where
// embedding the literal directly is conventional.
//
// Two-pass design: the first walkAST classifies each node into context
// buckets (const-init descendants, array-index descendants, direct
// comparison/bitwise operands -- including those wrapped in unary +/-
// or `as` casts). The second walk inspects each literal against its
// bucket flags and emits when none of them apply.
//
// Default-off; gated through warn() but we also short-circuit the
// classification pass when the diagnostic is disabled to avoid building
// per-node maps for nothing.
func (cg *CodeGen) checkMagicNumbers(prog *ast.Program) {
	if !cg.diagEnabled(DiagMagicNumber) {
		return
	}

	inConst := map[ast.Node]bool{}
	inIndex := map[ast.Node]bool{}
	cmpOperand := map[ast.Node]bool{}
	bitOperand := map[ast.Node]bool{}

	classify := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.VarDecl:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.TopLevelVar:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.IndexExpr:
			if e.Index != nil {
				walkAST(e.Index, func(c ast.Node) { inIndex[c] = true })
			}
		case *ast.BinExpr:
			switch e.Op {
			case "==", "!=", "<", "<=", ">", ">=":
				peelLitContext(e.Left, cmpOperand)
				peelLitContext(e.Right, cmpOperand)
			case "&", "|", "^", "<<", ">>":
				peelLitContext(e.Left, bitOperand)
				peelLitContext(e.Right, bitOperand)
			}
		}
	}

	flag := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.IntLit:
			if shouldFlagIntMagic(e, inConst[e], inIndex[e], cmpOperand[e], bitOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %d; consider naming it as a const", e.Value)
			}
		case *ast.FloatLit:
			if shouldFlagFloatMagic(e, inConst[e], inIndex[e], cmpOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %g; consider naming it as a const", e.Value)
			}
		}
	}

	for _, s := range prog.Stmts {
		walkAST(s, classify)
	}

	for _, s := range prog.Stmts {
		walkAST(s, flag)
	}
}

// peelLitContext records `n` and any literal-shaped descendant reached
// through unary +/- or an `as` cast. Captures `5`, `-5`, and `5 as i64`
// uniformly so the magic-number check exempts a literal regardless of
// the syntactic decoration around it.
func peelLitContext(n ast.Node, m map[ast.Node]bool) {
	for n != nil {
		m[n] = true

		switch e := n.(type) {
		case *ast.UnaryExpr:
			if e.Op == "-" || e.Op == "+" {
				n = e.Expr

				continue
			}
		case *ast.AsExpr:
			n = e.Expr

			continue
		}

		return
	}
}

// shouldFlagIntMagic decides whether an integer literal warrants a
// magic-number warning given its context flags.
func shouldFlagIntMagic(e *ast.IntLit, inConst, inIdx, cmpOp, bitOp bool) bool {
	if e.Big != nil {
		return false
	}

	if inConst || inIdx || cmpOp {
		return false
	}

	v := e.Value
	if v >= -1 && v <= 2 {
		return false
	}

	if bitOp && isBitOpExempt(v) {
		return false
	}

	return true
}

// shouldFlagFloatMagic decides whether a float literal warrants a
// magic-number warning. Floats don't get a bit-op exemption since
// bitwise ops aren't defined for them.
func shouldFlagFloatMagic(e *ast.FloatLit, inConst, inIdx, cmpOp bool) bool {
	if inConst || inIdx || cmpOp {
		return false
	}

	switch e.Value {
	case -1, 0, 0.5, 1, 2:
		return false
	}

	return true
}

// isBitOpExempt reports whether v is a recognizable bit pattern that
// commonly appears as a bitwise operand: power of two (single-bit set
// or shift count) or 2^N - 1 (an all-ones mask).
func isBitOpExempt(v int64) bool {
	if v <= 0 {
		return v == 0 || v == -1
	}

	if v&(v-1) == 0 {
		return true
	}

	if (v+1)&v == 0 {
		return true
	}

	return false
}

// diagEnabled reports whether a default-off diagnostic has been opted
// into via -W<name>, -Wpedantic, etc. Default-on diagnostics always
// return true.
func (cg *CodeGen) diagEnabled(name string) bool {
	if !defaultOffWarnings[name] {
		return true
	}

	if s := cg.diags[name]; s != nil {
		return s.enabled
	}

	return false
}

// astEqual reports whether two AST nodes are syntactically identical for
// the purposes of identical-operand detection. Conservative: only
// recognizes shapes that have no chance of side effects (identifiers,
// field access on the same target, integer literals).
func astEqual(a, b ast.Node) bool {
	switch x := a.(type) {
	case *ast.Identifier:
		y, ok := b.(*ast.Identifier)

		return ok && x.Name == y.Name
	case *ast.IntLit:
		y, ok := b.(*ast.IntLit)
		if !ok {
			return false
		}

		if (x.Big == nil) != (y.Big == nil) {
			return false
		}

		if x.Big != nil {
			return x.Big.Cmp(y.Big) == 0
		}

		return x.Value == y.Value
	case *ast.BoolLit:
		y, ok := b.(*ast.BoolLit)

		return ok && x.Value == y.Value
	case *ast.FieldAccess:
		y, ok := b.(*ast.FieldAccess)

		return ok && x.Field == y.Field && astEqual(x.Expr, y.Expr)
	}

	return false
}
