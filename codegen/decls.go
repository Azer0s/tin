package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// traitDisplayName returns the fully-qualified display name of a trait TypeExpr,
// preserving the pkg::TraitName form (e.g. "json::JsonSerializable").
// Used for traitof atoms so they match qualified atom literals consistently with typeof.
func traitDisplayName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		return t.Name
	}

	return ""
}

// traitBaseName returns the bare (unqualified) name of a trait TypeExpr.
func traitBaseName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	case *ast.GenericType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	}

	return ""
}

// structHasMethod checks whether a struct directly defines a method named name.
func structHasMethod(s *ast.StructDecl, name string) bool {
	for _, m := range s.Methods {
		if m.Name == name {
			return true
		}
	}

	return false
}

// structHasField checks whether a struct directly declares a field named name.
func structHasField(s *ast.StructDecl, name string) bool {
	for _, f := range s.Fields {
		if f.Name == name {
			return true
		}
	}

	return false
}

// augmentStructFromTraits returns a copy of the struct with forward fields and
// default methods injected from implemented traits.
func (cg *CodeGen) augmentStructFromTraits(n *ast.StructDecl) *ast.StructDecl {
	if len(n.Implements) == 0 {
		return n
	}

	aug := &ast.StructDecl{
		Name:       n.Name,
		TypeParams: n.TypeParams,
		Fields:     append([]ast.StructField{}, n.Fields...),
		Methods:    append([]*ast.FuncDecl{}, n.Methods...),
		Tags:       n.Tags,
		ScopedTags: n.ScopedTags,
	}

	for _, impl := range n.Implements {
		name := traitBaseName(impl)
		// Strip package qualifier (e.g. "io::AsyncReader" -> "AsyncReader").
		if cg.traitFor(CanonKey(name)) == nil {
			if idx := strings.LastIndex(name, "::"); idx >= 0 {
				name = name[idx+2:]
			}
		}

		trait := cg.traitFor(CanonKey(name))
		if trait == nil {
			continue
		}

		// Inject forward fields that the struct doesn't already have.
		for _, ff := range trait.ForwardFields {
			if !structHasField(aug, ff.Name) {
				aug.Fields = append(aug.Fields, ff)
			}
		}

		// Inject default (non-virtual) methods the struct doesn't override.
		for _, m := range trait.Methods {
			if m.IsVirtual || m.Body == nil {
				continue // virtual - struct must provide its own
			}

			if !structHasMethod(aug, m.Name) {
				// Bind "this" parameter to this struct type, preserving
				// the receiver shape the trait declared.  Pointer-form
				// receivers stay pointer; value-form receivers stay
				// value.  Rule 2 in docs/06-traits.md: every method
				// implementing a trait on a struct shares its receiver
				// shape with the trait def, so the auto-injected
				// default has to follow the def's lead -- not silently
				// flip to *Self the way it did before.
				injected := *m
				// Mark the injected method as a trait impl so methodScopeName
				// produces "Struct_<trait>_<method>" (matching what the vtable
				// wrapper looks up). Without this, default-bodied trait methods
				// would be predeclared under the bare name and the wrapper's
				// qualified lookup would miss them.
				injected.TraitQualifier = name

				wasPointer := false
				if len(m.Params) > 0 && m.Params[0].Name == "this" {
					_, wasPointer = m.Params[0].Type.(*ast.PointerType)
				}

				var thisType ast.TypeExpr = &ast.SimpleType{Name: n.Name}
				if wasPointer {
					thisType = &ast.PointerType{Elem: &ast.SimpleType{Name: n.Name}}
				}

				if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
					injected.Params = append([]ast.Param{
						{Name: "this", Type: thisType},
					}, injected.Params...)
				} else {
					newParams := make([]ast.Param, len(injected.Params))
					copy(newParams, injected.Params)
					newParams[0].Type = thisType
					injected.Params = newParams
				}

				aug.Methods = append(aug.Methods, &injected)
			}
		}
	}

	return aug
}

func (cg *CodeGen) lookupTemplateFile(tmplName string) string {
	// Generic templates are tagged at preregister time -- every
	// monomorphization shares the same source.
	if f := cg.genericStructTmplFiles[tmplName]; f != "" {
		return f
	}

	if f := cg.genericStructTmplFiles[cg.pkgStructKey(tmplName)]; f != "" {
		return f
	}

	// Non-generic structs go through structDeclFiles only.
	if f := cg.structDeclFiles[cg.pkgStructKey(tmplName)]; f != "" {
		return f
	}

	if f := cg.structDeclFiles[tmplName]; f != "" {
		return f
	}

	suffix := "__" + tmplName

	for k, v := range cg.structDeclFiles {
		if v == "" {
			continue
		}

		if strings.HasSuffix(k, suffix) {
			return v
		}
	}

	return ""
}

// findAmbiguousMethods returns a slice of methods that collide with
// another in the set: same name AND same parameter-type signature.
// Returns nil when every (name, signature) pair is unique.
//
// Two methods with identical signatures only ever survive
// monomorphization together when both their where-guards held for the
// concrete type substitution -- a genuine ambiguity the user must
// resolve at source.
func findAmbiguousMethods(methods []*ast.FuncDecl) []*ast.FuncDecl {
	type key struct {
		name string
		sig  string
	}

	groups := make(map[key][]*ast.FuncDecl, len(methods))

	for _, m := range methods {
		if m.IsExtern != "" {
			continue
		}

		sig := paramSig(m)
		k := key{name: m.Name, sig: sig}
		groups[k] = append(groups[k], m)
	}

	for _, g := range groups {
		if len(g) > 1 {
			return g
		}
	}

	return nil
}

// paramSig returns a signature string built from each parameter's type
// (rendered via typeExprText so package-qualified names line up). The
// receiver `this` is included since it carries the concrete struct
// type that distinguishes overloads across struct boundaries.
func paramSig(m *ast.FuncDecl) string {
	var b strings.Builder

	for i, p := range m.Params {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(typeExprText(p.Type))
	}

	return b.String()
}

// ambiguousMethodError formats the multi-overload diagnostic for a
// concrete struct whose where-guards left two same-signature methods
// alive after monomorphization. Each "declared at" entry shows the
// originating file:line:col so the user can find the colliding decls
// even when the struct's methods live in a different file from the
// instantiation site (e.g. user-file `Atomic[i64]` referencing two
// stdlib overloads).
func (cg *CodeGen) ambiguousMethodError(structName string, methods []*ast.FuncDecl) error {
	first := methods[0]

	// Methods always live with their struct: look up the originating
	// file via the same registry the per-line `//!-Wno-` lookup uses.
	declFile := cg.lookupTemplateFile(structName)
	if declFile == "" {
		// Strip monomorphization suffix and try again with the bare
		// template name (Box__i64 -> Box).
		bare := structName
		if idx := strings.Index(structName, "__"); idx > 0 {
			bare = structName[:idx]
		}

		declFile = cg.lookupTemplateFile(bare)
	}

	if declFile == "" {
		declFile = cg.filename
	}

	pretty := cg.diagStructName(structName)

	// Anchor the diagnostic at the call site that triggered
	// monomorphization (cg.currentPos). The user wants to see WHERE
	// they wrote the ambiguous call -- the overload definitions
	// themselves are listed as bullets so they can find each
	// definition to fix.
	var details strings.Builder
	for _, m := range methods {
		details.WriteString("\n  candidate: ")
		details.WriteString(pretty)
		details.WriteByte('.')
		details.WriteString(m.Name)
		details.WriteString(" with ")
		details.WriteString(whereGuardSummary(m.Constraints))
		details.WriteString(" (declared at ")
		fmt.Fprintf(&details, "%s:%d:%d", declFile, m.Pos().Line, m.Pos().Col)
		details.WriteByte(')')
	}

	// Anchor at the call site (cg.currentPos) so the user sees WHERE
	// they wrote the ambiguous call. Falls back to the first overload
	// when no call-site position is in flight (e.g. monomorphization
	// triggered by a type alias rather than a method call).
	anchorFile := cg.filenameForDiag()
	anchorPos := cg.currentPos

	if anchorPos.Line == 0 {
		anchorPos = first.Pos()
		anchorFile = declFile
	}

	return fmt.Errorf("%s:%d:%d: %s.%s is ambiguous for this instantiation: %d overloads with the same signature satisfy their where-guards. Drop the redundant guard, or distinguish by parameter type.%s",
		anchorFile, anchorPos.Line, anchorPos.Col,
		pretty, first.Name,
		len(methods), details.String())
}

// whereGuardSummary renders a method's where-clauses as
// "where t is X and r is Y" (or "no where-guard" when the method has
// none) for use in ambiguity / dead-strip diagnostics.
func whereGuardSummary(cs []ast.TypeConstraint) string {
	if len(cs) == 0 {
		return "no where-guard"
	}

	var b strings.Builder

	for i, c := range cs {
		if i > 0 {
			b.WriteString(" and ")
		}

		b.WriteString("where ")
		b.WriteString(c.TypeParam)
		b.WriteString(" is ")
		b.WriteString(typeBoundString(c.Bound))
	}

	return b.String()
}
