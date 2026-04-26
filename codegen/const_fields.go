package codegen

// Per-field const enforcement.
//
// A field declared `const x T` in a struct body cannot be written through
// its named identifier after the struct is constructed. This is a purely
// compile-time, syntactic check: only AST-level writes via the named
// field are rejected. Reflective or indirect paths (setfield, address-of
// then pointer write) are NOT blocked - const is a lint-level tag, not a
// runtime guarantee.
//
// Coverage:
//   - AssignStmt         s.f = x
//   - AugAssignStmt      s.f += x / -= / *= / ...
//   - PostfixStmt        s.f++ / s.f--
//
// Deliberately NOT rejected (const is compile-time-only):
//   - SetfieldExpr       setfield(s, "f", v)   - reflective write
//   - AddressOfExpr      &s.f                  - address-taking itself is
//                                                not an assignment; writes
//                                                through the returned
//                                                pointer bypass static
//                                                tracking by design.
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
