package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

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

		return cg.traitFor(CanonKey(bare)) != nil
	}

	// isKnownStruct mirrors isKnownTrait for the structTypes registry.
	// Cross-package struct names are stored mangled with `__` instead
	// of `::`, so we try the original SimpleType name, the bare suffix,
	// and the `::` -> `__` substitution before giving up.
	isKnownStruct := func(name string) bool {
		if cg.structTypeFor(CanonKey(name)) != nil {
			return true
		}

		mangled := strings.ReplaceAll(name, "::", "__")
		if cg.structTypeFor(CanonKey(mangled)) != nil {
			return true
		}

		bare := name
		if i := strings.LastIndex(name, "::"); i >= 0 {
			bare = name[i+2:]
		}

		return cg.structTypeFor(CanonKey(bare)) != nil
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
	// Pointer slot like `*Foo`: surface as the formatted name so the
	// redundant-cast walker can match `nil as *Foo` against a declared
	// `*Foo` slot.  Recurse so multi-level pointers (`**Foo`) work too.
	if pt, ok := t.(*ast.PointerType); ok {
		inner := simpleTypeName(pt.Elem)
		if inner == "" {
			return ""
		}

		return "*" + inner
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
