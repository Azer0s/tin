package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

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

// checkPtrTraitInFuncSig emits -Wptr-trait whenever a function's param
// or return type names a `*Trait`.  The trait fat-pointer already
// stores a heap pointer in its data field, so the outer `*` is a
// second indirection on top of one Tin's runtime supplies for free.
// The value-form `Trait` is the canonical shape: source `&b` aliases,
// source `b` snapshots -- the explicit `&` at the coerce site is the
// alias signal.  Default-on; silence per-decl with `//!-Wno-ptr-trait`.
func (cg *CodeGen) checkPtrTraitInFuncSig(fd *ast.FuncDecl) {
	for _, p := range fd.Params {
		cg.warnPtrTrait(fd.Pos(), p.Type)
	}

	cg.warnPtrTrait(fd.Pos(), fd.RetType)
}

// checkPtrTraitInStruct emits -Wptr-trait for every field whose
// declared type is `*Trait`.
func (cg *CodeGen) checkPtrTraitInStruct(sd *ast.StructDecl) {
	for _, f := range sd.Fields {
		cg.warnPtrTrait(sd.Pos(), f.Type)
	}
}

// warnPtrTrait walks a TypeExpr (recursing into pointers / arrays /
// generic args / fn params + ret) and fires -Wptr-trait for any
// PointerType{Elem: <trait name>}.
func (cg *CodeGen) warnPtrTrait(pos ast.Pos, te ast.TypeExpr) {
	switch t := te.(type) {
	case *ast.PointerType:
		if cg.typeExprNamesTrait(t.Elem) {
			cg.warn(DiagPtrTrait, pos,
				"`*%s` is rarely the right shape - trait fat-pointers already carry a heap pointer internally; prefer the value-form `%s` (the `&` at the coerce site is the alias signal)",
				te.String()[1:], te.String()[1:])
		}

		cg.warnPtrTrait(pos, t.Elem)
	case *ast.ArrayType:
		cg.warnPtrTrait(pos, t.Elem)
	case *ast.GenericType:
		for _, tp := range t.TypeParams {
			cg.warnPtrTrait(pos, tp)
		}
	case *ast.FuncType:
		for _, p := range t.Params {
			cg.warnPtrTrait(pos, p)
		}

		cg.warnPtrTrait(pos, t.RetType)
	}
}

// typeExprNamesTrait reports whether te names a declared trait (bare
// or generic instantiation thereof).  Used by warnPtrTrait to decide
// whether to flag a leading `*` as the discouraged `*Trait` shape.
func (cg *CodeGen) typeExprNamesTrait(te ast.TypeExpr) bool {
	switch t := te.(type) {
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return cg.traitFor(CanonKey(name)) != nil
	case *ast.GenericType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return cg.traitFor(CanonKey(name)) != nil
	}

	return false
}

// checkTraitSnapshotMutation fires -Wtrait-snapshot-mutation when a
// `let x Trait = expr` (or struct field initialiser) coerces a value
// source into a trait whose impl on the source struct has any
// pointer-receiver method.  The compile is legal -- value source
// gives the trait fat-ptr its own heap-allocated snapshot -- but
// readers usually expect the alias form (`Trait = &b`) when the
// trait can mutate.  The warning suggests the `&` fix, tailored to
// whether the RHS is a struct creation or a binding reference.
func (cg *CodeGen) checkTraitSnapshotMutation(vd *ast.VarDecl) {
	if vd.Type == nil || vd.Value == nil {
		return
	}

	traitName := ""

	switch t := vd.Type.(type) {
	case *ast.SimpleType:
		traitName = t.Name
	case *ast.GenericType:
		traitName = t.Name
	default:
		return
	}

	bare := traitName
	if idx := strings.LastIndex(bare, "::"); idx >= 0 {
		bare = bare[idx+2:]
	}

	if cg.traitFor(CanonKey(bare)) == nil {
		return
	}
	// RHS is already address-of -- alias form chosen, no warning needed.
	if u, ok := vd.Value.(*ast.UnaryExpr); ok && u.Op == "&" {
		return
	}

	var sourceStructName string

	switch rhs := vd.Value.(type) {
	case *ast.StructLit:
		sourceStructName = rhs.TypeName
		// Strip package qualifier (`pkg::Foo` -> `Foo`).
		if idx := strings.LastIndex(sourceStructName, "::"); idx >= 0 {
			sourceStructName = sourceStructName[idx+2:]
		}
	case *ast.Identifier:
		// Identifier RHS: needs scope to resolve.  Static check
		// isn't reliable here; skip.
		return
	default:
		return
	}

	if sourceStructName == "" {
		return
	}

	sd := cg.structDeclsByName[sourceStructName]
	if sd == nil {
		return
	}

	var ptrMethods []string

	for _, m := range sd.Methods {
		if m.TraitQualifier == "" {
			continue
		}

		mt := traitBaseFromQualifier(m.TraitQualifier)
		if mt != bare {
			continue
		}

		if len(m.Params) == 0 || m.Params[0].Name != "this" {
			continue
		}

		if _, isPtr := m.Params[0].Type.(*ast.PointerType); isPtr {
			ptrMethods = append(ptrMethods, m.Name)
		}
	}

	if len(ptrMethods) == 0 {
		return
	}

	cg.warn(DiagTraitSnapshotMutation, vd.Pos(),
		"value-source coerce to %s: impl for %s has pointer-receiver method(s) (%s) - the trait fat-ptr will own a snapshot, so mutations through *Self methods won't propagate to the original; use `&%s{...}` for the alias form",
		traitName, sourceStructName, strings.Join(ptrMethods, ", "), sourceStructName)
}
