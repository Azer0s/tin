package codegen

import (
	"github.com/Azer0s/tin/ast"
)

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
	case *ast.NilLit:
		return "nil"
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
		// nil-as-any is genuinely ambiguous (which pointer type?), so
		// the `as any` is NOT redundant for nil sources.  Every other
		// literal kind boxes unambiguously into any.
		return litKind != "nil"
	}
	// `nil` coerces into any pointer-typed slot.  The slot's declared
	// type already pins the pointee, so the cast adds nothing.  Common
	// shape: `let p *Foo = nil as *Foo` -- the slot annotation `*Foo`
	// already constrains the right-hand side.
	if litKind == "nil" {
		return len(slotType) > 0 && slotType[0] == '*'
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
