package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkAllTraitImplsComplete(stmts []ast.Node) error {
	for _, node := range stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok || len(sd.TypeParams) > 0 || len(sd.Implements) == 0 {
			continue
		}

		structKey := cg.pkgStructKey(sd.Name)
		// Build set of qualified scope names predeclared for this struct.
		// We check membership rather than scope-lookup because scope contains
		// many other entries that aren't methods of this struct.
		methodNames := map[string]bool{}

		for _, m := range sd.Methods {
			methodNames[methodScopeName(structKey, m)] = true
			methodNames[structKey+"_"+m.Name] = true
		}

		var missing []string

		for _, impl := range sd.Implements {
			traitName := traitBaseName(impl)
			if cg.traitFor(CanonKey(traitName)) == nil {
				if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
					traitName = traitName[idx+2:]
				}
			}

			if traitName == "implicit" || traitName == "coerce" {
				continue
			}

			td := cg.traitFor(CanonKey(traitName))
			if td == nil {
				continue
			}

			if td.IsAlias {
				// For as-fn aliases, the impl is `fn ::T(...)` (predeclared as
				// `Struct_T_T` per Phase 1 parser convention) or the trait's own
				// default if it has one.
				wantQual := structKey + "_" + traitName + "_" + traitName
				wantBare := structKey + "_" + traitName

				if methodNames[wantQual] || methodNames[wantBare] {
					continue
				}

				missing = append(missing, fmt.Sprintf("fn ::%s(this %s, ...)", traitName, sd.Name))

				continue
			}

			for _, m := range td.Methods {
				// Default-bodied methods are optional.
				if !m.IsVirtual && m.Body != nil {
					continue
				}

				wantQual := structKey + "_" + traitName + "_" + m.Name
				wantQualWithArgs := structKey + "_" + traitQualifierKey(bareTraitImplKey(impl)) + "_" + m.Name

				if methodNames[wantQual] || methodNames[wantQualWithArgs] {
					continue
				}

				missing = append(missing,
					fmt.Sprintf("fn %s::%s(this %s, ...)", traitName, m.Name, sd.Name))
			}
		}

		if len(missing) > 0 {
			return cg.nodeErr(sd, "struct %s declares trait(s) %s but does not implement: %s",
				sd.Name, traitListDisplay(sd.Implements), strings.Join(missing, "; "))
		}
		// Per-method receiver-shape match: each impl method's first
		// param shape must mirror the trait def's corresponding
		// method.  Mixed-shape traits are fine (Map[K,V] read methods
		// are value-receiver, mutating methods are pointer-receiver);
		// what's rejected is the user writing `*Self` on an impl
		// whose trait def says `Self`, or vice versa.  Without this,
		// `trait Seq[t] = fn next(this Seq[t])` paired with
		// `fn Seq[i64]::next(this *Range)` would dispatch through a
		// hidden value-load on what the user thinks is a pointer
		// borrow.
		if err := cg.checkImplReceiversMatchTraitDef(sd); err != nil {
			return err
		}
	}

	return nil
}

// checkImplReceiversMatchTraitDef enforces that each impl method's
// `this` receiver shape matches the receiver declared by the
// corresponding trait def method -- IF the trait def declared one.
// When the trait def's method omits `this` entirely (the implicit
// form), the impl is free to declare value or pointer receiver per
// its own needs.  Skip static methods, unknown traits, and impls
// whose corresponding trait method was declared without `this`.
func (cg *CodeGen) checkImplReceiversMatchTraitDef(sd *ast.StructDecl) error {
	for _, m := range sd.Methods {
		if m.TraitQualifier == "" {
			continue
		}

		bare := traitBaseFromQualifier(m.TraitQualifier)
		if bare == "" {
			continue
		}

		td := cg.traitFor(CanonKey(bare))
		if td == nil {
			continue
		}

		var defMethod *ast.FuncDecl

		for _, tm := range td.Methods {
			if tm.Name == m.Name {
				defMethod = tm

				break
			}
		}

		if defMethod == nil {
			continue
		}

		if len(m.Params) == 0 || m.Params[0].Name != "this" {
			continue
		}
		// Trait def didn't declare `this` -- the impl is free.  This
		// is the "no contract" path; the trait author opted out of
		// pinning receiver shape and each impl picks for itself.
		if len(defMethod.Params) == 0 || defMethod.Params[0].Name != "this" {
			continue
		}

		_, implIsPtr := m.Params[0].Type.(*ast.PointerType)
		_, defIsPtr := defMethod.Params[0].Type.(*ast.PointerType)

		if implIsPtr == defIsPtr {
			continue
		}

		implShape := "this " + sd.Name
		if implIsPtr {
			implShape = "this *" + sd.Name
		}

		defShape := "this " + bare
		if defIsPtr {
			defShape = "this *" + bare
		}

		return cg.nodeErr(m,
			"impl %s::%s for %s has receiver `%s` but trait %s declared `%s`; the impl must use the same receiver shape the trait declared",
			bare, m.Name, sd.Name, implShape, bare, defShape)
	}

	return nil
}

// traitListDisplay formats a struct's Implements list for diagnostics.
func traitListDisplay(impls []ast.TypeExpr) string {
	parts := make([]string, len(impls))
	for i, t := range impls {
		parts[i] = traitDisplayName(t)
	}

	return strings.Join(parts, ", ")
}

// Type-alias / monomorphization
