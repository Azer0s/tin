package codegen

// Struct-level scoped tag propagation.
//
// A struct can declare control tags with an `@scope` qualifier in its
// `{#tag@scope}` header: `struct{#pure@fn #const@field} Name = ...`.
// Scoped tags are NOT properties of the struct itself - they propagate
// to matching members:
//
//   @fn         every method (instance + static)
//   @method     instance methods only
//   @static_fn  static methods only
//   @field      every field
//
// Propagation is applied once, before any tag-consuming pass, and
// mutates member AST nodes in place so downstream code sees the
// expanded tag set without needing to know propagation happened.
//
// Conflict policy: CSS-style cascade. A member-level tag that already
// conflicts with a propagated tag wins silently; the propagated tag is
// not added. Extern methods carry an auto-`#sideffect` so `#pure@fn`
// over an extern body skips that method cleanly.
//
// Compatibility matrix (rejected at the struct decl site if violated):
//
//   #pure / #sideffect / #no_recurse / #no_thread / #no_autoyield
//     => @fn, @method, @static_fn
//   #heavy / #async => @fn, @method
//   #const => @field
//   #handover / #packed => never (not scoped-tag-able)
//
// For @field propagation today, only `#const` is defined. `#const@field`
// acts as a default-flipper: unmarked fields become const; `var`-marked
// fields remain mutable. `const`-marked fields already are const.

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
)

// methodConflictPairs lists tag pairs that cannot coexist on the same
// member. Scoped propagation skips the tag when the conflicting
// partner is already present (member-level always wins). Ordering in
// each pair does not matter - checkConflict covers both directions.
var methodConflictPairs = [][2]string{
	{"pure", "sideffect"},
	{"heavy", "no_autoyield"},
}

// propagateStructScopedTags mutates methods and fields of d to pick up
// any struct-level scoped tags. Called before pass-1.5 / codegen
// consumes method tags so every downstream check sees the expanded
// set. Returns an error for tag-scope mismatches (e.g. #pure@field,
// #packed@fn, #const@fn).
func (cg *CodeGen) propagateStructScopedTags(d *ast.StructDecl) error {
	if d == nil || len(d.ScopedTags) == 0 {
		return nil
	}

	for _, st := range d.ScopedTags {
		if err := cg.validateScopedTag(d, st); err != nil {
			return err
		}

		switch st.Scope {
		case "fn":
			for _, m := range d.Methods {
				applyMemberTag(&m.Tags, st.Name)
			}
		case "method":
			for _, m := range d.Methods {
				if m.IsStatic {
					continue
				}

				applyMemberTag(&m.Tags, st.Name)
			}
		case "static_fn":
			for _, m := range d.Methods {
				if !m.IsStatic {
					continue
				}

				applyMemberTag(&m.Tags, st.Name)
			}
		case "field":
			cg.propagateFieldTag(d, st.Name)
		}
	}

	return nil
}

// validateScopedTag rejects tag-scope pairings that the compatibility
// matrix does not allow. A good error cites the struct name and tag
// so the user knows where to edit.
func (cg *CodeGen) validateScopedTag(d *ast.StructDecl, st ast.ScopedTag) error {
	if st.Scope == "field" {
		if st.Name == "const" {
			return nil
		}

		return fmt.Errorf("struct %s: tag #%s cannot be scoped @field (only #const is a valid @field tag)",
			d.Name, st.Name)
	}
	// @fn / @method / @static_fn: method-level tags only.
	switch st.Name {
	case "pure", "sideffect", "no_recurse", "no_thread", "no_autoyield":
		// fine for any method scope
		return nil
	case "heavy", "async":
		if st.Scope == "static_fn" {
			return fmt.Errorf("struct %s: tag #%s does not apply to static methods (use @fn or @method)",
				d.Name, st.Name)
		}

		return nil
	case "handover":
		return fmt.Errorf("struct %s: tag #handover is extern-only and cannot be scoped on a struct", d.Name)
	case "packed":
		return fmt.Errorf("struct %s: tag #packed is struct-level and cannot be scoped (drop the @%s qualifier)",
			d.Name, st.Scope)
	case "const":
		return fmt.Errorf("struct %s: tag #const is a field tag (use @field, not @%s)",
			d.Name, st.Scope)
	}

	return fmt.Errorf("struct %s: unknown scoped tag #%s", d.Name, st.Name)
}

// applyMemberTag adds tagName to the member's tag slice when it is not
// already present AND does not conflict with an existing tag. Member-
// level tags always win; propagation is best-effort.
func applyMemberTag(existing *[]string, tagName string) {
	for _, t := range *existing {
		if t == tagName {
			return
		}
	}

	for _, pair := range methodConflictPairs {
		a, b := pair[0], pair[1]

		conflictingPartner := ""

		switch tagName {
		case a:
			conflictingPartner = b
		case b:
			conflictingPartner = a
		}

		if conflictingPartner == "" {
			continue
		}

		for _, t := range *existing {
			if t == conflictingPartner {
				return // member-level wins silently
			}
		}
	}

	*existing = append(*existing, tagName)
}

// propagateFieldTag applies a scoped `#tag@field` propagation. Today
// only `#const` is valid; its effect is a default-flip: every field
// that is neither explicitly `const` nor explicitly `var` becomes
// const.
func (cg *CodeGen) propagateFieldTag(d *ast.StructDecl, tagName string) {
	if tagName != "const" {
		return // validated by validateScopedTag before arrival
	}

	for i := range d.Fields {
		f := &d.Fields[i]
		// Skip fields the user explicitly annotated either way.
		if f.IsConst || f.IsVar {
			continue
		}

		f.IsConst = true
	}
}
