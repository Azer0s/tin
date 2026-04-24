package codegen

// Per-field const enforcement.
//
// A field declared `const x T` in a struct body cannot be written after the
// struct is constructed. The check is purely compile-time: every AST site
// that takes a field-access target for a write asks checkFieldWritable
// before emitting IR. Construction paths (struct literal, positional init,
// destructuring let, match bindings) never flow through these sites and
// are therefore unaffected.
//
// Coverage:
//   - AssignStmt         s.f = x
//   - AugAssignStmt      s.f += x / -= / *= / ...
//   - PostfixStmt        s.f++ / s.f--
//   - SetfieldExpr       setfield(s, "f", v)
//   - AddressOfExpr      &s.f (strict: rejected outright, matches plan)
//
// Resolution:
//   fa := target.(*ast.FieldAccess)
//   parent type := astInferType(fa.Expr)
//   unwrap one pointer level (fa.IsPtr or explicit `*p.f`)
//   structName := typeNameOf(parent type)
//   reject when structConstFields[structName][fa.Field] is set.

import (
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// checkFieldWritable returns a diagnostic when target is a FieldAccess
// whose struct declares the field `const`. Other target kinds (plain
// Identifier, IndexExpr, DerefExpr, etc.) are not the concern of this
// check and return nil.
//
// False negatives are possible when astInferType cannot resolve the parent
// type (e.g. deeply generic contexts); this errs on the side of letting
// code through. Soundness in the common case (named-struct method bodies,
// local-variable assignments) is what matters for the user experience.
func (cg *CodeGen) checkFieldWritable(target ast.Node) error {
	fa, ok := target.(*ast.FieldAccess)
	if !ok {
		return nil
	}

	structName := cg.parentStructNameOf(fa)
	if structName == "" {
		return nil
	}

	if cg.structConstFields[structName][fa.Field] {
		return cg.nodeErr(fa, "cannot assign to const field %s.%s", structName, fa.Field)
	}

	return nil
}

// parentStructNameOf resolves the struct type that contains fa.Field and
// returns its name, or "" when the type cannot be statically determined.
func (cg *CodeGen) parentStructNameOf(fa *ast.FieldAccess) string {
	t := cg.astInferType(fa.Expr)
	if t == nil {
		return ""
	}
	// For both `obj.field` and `obj->field`, unwrap one pointer level if the
	// inferred parent type is *Struct. IsPtr distinguishes the surface form
	// but the pointer unwrap is identical.
	if pt, ok := t.(*irtypes.PointerType); ok {
		t = pt.ElemType
	}

	return cg.typeNameOf(t)
}

// checkSetfieldWritable validates `setfield(expr, "field", v)` when the
// field name is a string literal. Dynamic field names (variable-held
// strings) cannot be checked statically and fall through - the runtime
// strcmp chain emitted by genSetfield still completes the write, but
// catching dynamic-const violations would require a runtime check we
// have chosen not to emit for performance reasons. Document this limit
// in the user-facing const spec if it becomes observable.
func (cg *CodeGen) checkSetfieldWritable(e *ast.SetfieldExpr) error {
	lit, ok := e.Field.(*ast.StringLit)
	if !ok {
		return nil
	}

	t := cg.astInferType(e.Expr)
	if t == nil {
		return nil
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		t = pt.ElemType
	}

	structName := cg.typeNameOf(t)
	if structName == "" {
		return nil
	}

	if cg.structConstFields[structName][lit.Value] {
		return cg.nodeErr(e, "setfield: cannot assign to const field %s.%s", structName, lit.Value)
	}

	return nil
}
