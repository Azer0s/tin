package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// sortedVariants returns the entries of variants sorted by tag so that
// codegen emission is deterministic across program runs (Go's map
// iteration is randomized per run, which would otherwise blow byte-for-
// byte determinism in the IR - see TestIRDeterminism).
func sortedVariants(variants map[string]*dataVariantInfo) []struct {
	Name string
	Info *dataVariantInfo
} {
	entries := make([]struct {
		Name string
		Info *dataVariantInfo
	}, 0, len(variants))

	for name, vi := range variants {
		entries = append(entries, struct {
			Name string
			Info *dataVariantInfo
		}{Name: name, Info: vi})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Info.Tag < entries[j].Info.Tag
	})

	return entries
}

// checkNoWildcardInVariantFields enforces that ADT variant fields do
// not reference a call-site-supplied wildcard slot. A field's type is
// fixed at declaration; a wildcard's concrete type isn't known until
// the call site fills it. Method bodies (which are monomorphized per
// call) may freely use the wildcard - only field types are forbidden.
func checkNoWildcardInVariantFields(n *ast.DataDecl) error {
	for _, v := range n.Variants {
		for _, f := range v.Fields {
			if typeExprContainsWildcard(f.Type) {
				return fmt.Errorf("data %s: variant %s field %q has type %s which references a call-site-forwarded slot (`_`); field layout is fixed at declaration but the slot's concrete type is only known per call. Wildcards may appear in trait bounds and method signatures (which monomorphize per call), not in field types",
					n.Name, v.Name, f.Name, fmtTypeExpr(f.Type))
			}
		}
	}

	return nil
}

// unusedWildcardSlots returns warning messages for wildcard slots
// declared in the data's trait bounds that are never referenced by
// any variant or method, OR are referenced exactly once in a single
// method (in which case a method-level generic type parameter would
// express the same intent without obscuring the type at call sites).
// Each violation gets one warning string in the returned slice.
func unusedWildcardSlots(n *ast.DataDecl) []string {
	declaredNames := map[string]bool{}

	declaredAnon := false

	for _, impl := range n.Implements {
		walkWildcardNames(impl, declaredNames, &declaredAnon)
	}

	if !declaredAnon && len(declaredNames) == 0 {
		return nil
	}

	used := map[string]bool{}
	usedAnon := false

	// usageCount counts how many distinct method signatures (params
	// + return type) reference each named wildcard. A count of 1
	// means the slot is forwarded through the data's bound but is
	// only ever consumed by one method - a method-level type
	// parameter would localize the choice to that method instead of
	// every call to the data type.
	usageCount := map[string]int{}

	for _, m := range n.Methods {
		seenInMethod := map[string]bool{}
		seenAnonInMethod := false

		for _, p := range m.Params {
			walkWildcardNames(p.Type, used, &usedAnon)
			walkWildcardNames(p.Type, seenInMethod, &seenAnonInMethod)
		}

		walkWildcardNames(m.RetType, used, &usedAnon)
		walkWildcardNames(m.RetType, seenInMethod, &seenAnonInMethod)

		for name := range seenInMethod {
			usageCount[name]++
		}
	}

	var out []string

	if declaredAnon && !usedAnon {
		out = append(out, fmt.Sprintf(
			"data %s declares an anonymous `_` wildcard in its trait bound but never references it; the call-site forwarding is unnecessary",
			n.Name))
	}

	for name := range declaredNames {
		switch {
		case !used[name]:
			out = append(out, fmt.Sprintf(
				"data %s declares wildcard slot `_: %s` in its trait bound but never references %s; the call-site forwarding is unnecessary",
				n.Name, name, name))
		case usageCount[name] == 1:
			out = append(out, fmt.Sprintf(
				"data %s declares wildcard slot `_: %s` but only one method references it; consider making %s a method-level generic instead so the type choice is local to that method",
				n.Name, name, name))
		}
	}

	return out
}

// walkWildcardNames collects every wildcard name from a type
// expression into the names set; flips anon when an anonymous `_`
// is seen.
func walkWildcardNames(te ast.TypeExpr, names map[string]bool, anon *bool) {
	if te == nil {
		return
	}

	switch t := te.(type) {
	case *ast.WildcardType:
		if t.Name == "" {
			*anon = true
		} else {
			names[t.Name] = true
		}
	case *ast.GenericType:
		for _, p := range t.TypeParams {
			walkWildcardNames(p, names, anon)
		}
	case *ast.PointerType:
		walkWildcardNames(t.Elem, names, anon)
	case *ast.ArrayType:
		walkWildcardNames(t.Elem, names, anon)
	case *ast.UnionTypeExpr:
		for _, u := range t.Types {
			walkWildcardNames(u, names, anon)
		}
	}
}

// fmtTypeExpr renders a TypeExpr in user-facing Tin syntax. Falls
// back to the AST's String() method when the type isn't one of the
// common shapes.
func fmtTypeExpr(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}

	return te.String()
}

// checkConstraintsReferenceDeclared rejects where-guards that
// reference a name that is not declared in the type's TypeParams or
// Wildcards. Catches typos like `data Foo[T] where Q is comp` where
// Q is undeclared. The error names the available declared params /
// wildcards so the user can spot the typo.
func checkConstraintsReferenceDeclared(declName string, typeParams, wildcards []string, constraints []ast.TypeConstraint) error {
	if len(constraints) == 0 {
		return nil
	}

	declared := map[string]bool{}
	for _, p := range typeParams {
		declared[p] = true
	}

	for _, w := range wildcards {
		declared[w] = true
	}

	for _, c := range constraints {
		if !declared[c.TypeParam] {
			return fmt.Errorf("%s: where-guard `where %s is %s` references %s, which is not in the type-param list (have: %s); add %s as a type parameter or as a wildcard slot `_: %s`",
				declName, c.TypeParam, typeBoundString(c.Bound), c.TypeParam,
				declaredNamesString(typeParams, wildcards), c.TypeParam, c.TypeParam)
		}
	}

	return nil
}

// declaredNamesString joins type-params and wildcards into a single
// human-readable list, prefixing wildcard slots with "_: " so the
// kind is obvious.
func declaredNamesString(typeParams, wildcards []string) string {
	var parts []string

	parts = append(parts, typeParams...)

	for _, w := range wildcards {
		parts = append(parts, "_: "+w)
	}

	if len(parts) == 0 {
		return "(none)"
	}

	return strings.Join(parts, ", ")
}

// checkMethodsAgainstImpls rejects trait-qualified method definitions
// (`fn TraitName::method`) when the data/struct does not list
// TraitName in its implements list (the parens after the type-param
// list, e.g. `data Foo(Reader, Writer) =`). Without this check a typo
// in the qualifier or a forgotten Implements entry compiles silently
// and the method is dead code.
//
// Both static (`fn ::method`) and unqualified methods are skipped:
// they don't claim a trait impl. Only qualified methods get checked.
func checkMethodsAgainstImpls(declName, declKind string, impls []ast.TypeExpr, methods []*ast.FuncDecl) error {
	if len(methods) == 0 {
		return nil
	}

	implTraits := map[string]bool{}

	for _, impl := range impls {
		for _, name := range collectTraitBaseNames(impl) {
			implTraits[name] = true
		}
	}

	for _, m := range methods {
		if m.TraitQualifier == "" {
			continue
		}
		// Static methods on traits write `::method`; the qualifier
		// is empty in that case, so we never reach here for them.
		// Trait-qualified methods produce a non-empty qualifier
		// like `Reader` / `myt[T, Pair[_: W, T]]` / `pkg::Trait`.
		bare := traitBaseFromQualifier(m.TraitQualifier)
		if bare == "" {
			continue
		}

		if !implTraits[bare] {
			return fmt.Errorf("%s %s: method `fn %s::%s` references trait %q, but %s does not declare trait %q in its implements list. Add %s to %s's parens (e.g. `%s %s(...,%s) =`)",
				declKind, declName, m.TraitQualifier, m.Name, bare, declName, bare, bare, declName, declKind, declName, bare)
		}
	}

	return nil
}

// traitBaseFromQualifier extracts the bare trait name from a
// qualifier string. Strips package prefixes (`pkg::Trait` -> `Trait`)
// and type-args (`Trait[T, U]` -> `Trait`).
func traitBaseFromQualifier(q string) string {
	// Strip type-args first: anything from the first '[' onward.
	if idx := strings.Index(q, "["); idx >= 0 {
		q = q[:idx]
	}
	// Strip package: keep the last segment after `::`.
	if idx := strings.LastIndex(q, "::"); idx >= 0 {
		q = q[idx+2:]
	}

	return q
}

// collectTraitBaseNames extracts the bare trait name(s) from a
// trait-impl TypeExpr. For SimpleType / GenericType the base name is
// the type's name; for a UnionTypeExpr (rare) collect from each
// alternative. Package qualifiers strip down to the last segment so
// `pkg::Trait` matches a `Trait` qualifier on a method.
func collectTraitBaseNames(t ast.TypeExpr) []string {
	switch v := t.(type) {
	case *ast.SimpleType:
		name := v.Name

		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return []string{name}
	case *ast.GenericType:
		name := v.Name

		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return []string{name}
	case *ast.UnionTypeExpr:
		var out []string
		for _, u := range v.Types {
			out = append(out, collectTraitBaseNames(u)...)
		}

		return out
	}

	return nil
}

// dataVariantInfo holds the per-variant layout of an ADT variant.
// Tag is the ordinal index (declaration order). PayloadType is the LLVM struct
// packed from the variant's fields (empty struct for nullary variants).
// Tag is int64 to mirror the i64 in-memory layout chosen for alignment; an
// int8 cap silently truncated at the 129th variant.
type dataVariantInfo struct {
	Tag         int64
	PayloadType *irtypes.StructType
	Fields      []ast.StructField
}

// genDataDecl generates the LLVM layout for a `data Foo = V0 | V1(...)` decl.
// Runtime layout mirrors tagged unions: { i32 type_id, i8 tag, [N x i8] payload }.
// N is sized to the largest variant payload.
//
// Generic ADTs are registered but not emitted here - monomorphization happens
// on-demand when a concrete instance like `Option[i32]` is used.
func (cg *CodeGen) genDataDecl(n *ast.DataDecl) error {
	// Wildcard slots are call-site-supplied: their concrete type is only
	// known per call. A variant payload field referencing a wildcard
	// would force the runtime layout to depend on per-call info, which
	// is impossible since the layout is fixed at declaration.
	if err := checkNoWildcardInVariantFields(n); err != nil {
		return cg.nodeErr(n, "%s", err)
	}

	if err := checkConstraintsReferenceDeclared(n.Name, n.TypeParams, n.Wildcards, n.Constraints); err != nil {
		return cg.nodeErr(n, "%s", err)
	}

	if err := checkMethodsAgainstImpls(n.Name, "data", n.Implements, n.Methods); err != nil {
		return cg.nodeErr(n, "%s", err)
	}

	if warnMsgs := unusedWildcardSlots(n); len(warnMsgs) > 0 {
		for _, m := range warnMsgs {
			cg.warn(DiagUnusedWildcard, n.Pos(), "%s", m)
		}
	}

	cg.dataDecls[n.Name] = n

	if len(n.TypeParams) > 0 {
		// Generic ADTs are emitted on-demand during monomorphization. Each
		// concrete instance registers its own variant names in
		// dataVariantLookup (see monomorphizeDataDecl).
		return nil
	}

	// For non-generic ADTs, register every variant so bare constructor refs
	// resolve without qualification.
	for _, v := range n.Variants {
		cg.dataVariantLookup[v.Name] = appendUnique(cg.dataVariantLookup[v.Name], n.Name)
	}

	if err := cg.emitConcreteData(n.Name, n); err != nil {
		return err
	}

	return cg.genDataMethods(n.Name, n)
}

// genDataMethods emits method bodies declared in a `data` block as
// top-level LLVM functions named `<DataName>_<method>` (or trait-qualified
// when the method has a TraitQualifier). Mirrors the simpler subset of
// genStructMethods needed for ADT trait impls; the receiver type is the
// ADT itself rather than a struct.
//
// Trait-impl machinery (vtable wrappers, default-method injection,
// trait-chain shims) is not yet plumbed through for ADTs - that lands in
// the unified static-table commit. Direct static-dispatch method calls
// resolve through the emitted functions immediately.
func (cg *CodeGen) genDataMethods(adtName string, n *ast.DataDecl) error {
	if len(n.Methods) == 0 && len(n.Implements) == 0 {
		return nil
	}

	// Predeclare every non-generic method signature so intra-ADT
	// cross-method calls (e.g. `fn ::print(this T) = return this.message()`
	// where message is a trait-qualified impl on the same ADT) resolve
	// during body compilation. Mirrors top-level structs' predeclare
	// pass that runs before genStructMethods.
	for _, m := range n.Methods {
		if len(m.TypeParams) > 0 || m.IsExtern != "" {
			continue
		}

		if err := cg.predeclareMethod(adtName, m); err != nil {
			return err
		}
	}

	// Pre-register plain-name aliases so a method body that calls another
	// method on the same ADT (e.g. one trait method delegating to another)
	// can resolve through the bare name.
	cg.registerPlainMethodAliases(adtName, n.Methods)

	for _, m := range n.Methods {
		if len(m.TypeParams) > 0 {
			templateKey := adtName + "_" + m.Name
			cg.genericMethodTemplates[templateKey] = m

			continue
		}

		if err := cg.genStructMethod(adtName, m); err != nil {
			return err
		}
	}

	// Re-register after bodies are emitted so trait-qualified methods that
	// were predeclared during body codegen surface under their plain names
	// (mirrors genStructMethods's symmetric pre/post registration).
	cg.registerPlainMethodAliases(adtName, n.Methods)

	// Emit vtables and trait-impl globals for each implemented trait.
	// Reuse the struct path by passing a synthetic StructDecl. ADTs use
	// bare names everywhere (LLVM type, method symbols, vtable keys),
	// while genTraitVtables would otherwise apply pkgStructKey and key
	// vtables under `pkg__Name__instKey` while typeNameOf later returns
	// just `Name`. Suspend the package context so genTraitVtables lines
	// up with the bare-name convention.
	if len(n.Implements) > 0 {
		shim := &ast.StructDecl{
			Name:       n.Name,
			Implements: n.Implements,
			Methods:    n.Methods,
		}

		prevPkg := cg.currentPkg
		cg.currentPkg = ""

		err := cg.genTraitVtables(shim)
		cg.currentPkg = prevPkg

		if err != nil {
			return err
		}
	}

	return nil
}

// monomorphizeDataDecl substitutes the template's type parameters with the
// supplied concrete types and emits a per-instance concrete ADT under the
// synthesized name (e.g. Option + [i32] -> Option__i32). The synthetic name
// is also registered in dataDecls so subsequent constructor and match
// lookups can find its variant info. Variant names are recorded in
// dataVariantLookup tied to the concrete name; callers that use a bare
// constructor like `Some(42)` resolve through the original (bare) name
// entries, and the codegen uses expected-type context to pick the concrete
// instance.
func (cg *CodeGen) monomorphizeDataDecl(tmpl *ast.DataDecl, typeArgs []ast.TypeExpr, concreteName string) error {
	// Monomorphization re-walks the template's method signatures,
	// which reference the template ADT by its bare name (e.g.
	// `this Result[t, e]`). The strict-bare-type check in
	// tinTypeToLLVM would reject that bare reference because the
	// USER's scope (where monomorphization is invoked) hasn't pulled
	// in `Result` via selective import. Suppress the check for the
	// duration of this monomorphization - every name here is
	// compiler-synthesized from the template, not user-written.
	prev := cg.suppressBareTypeCheck
	cg.suppressBareTypeCheck = true

	defer func() { cg.suppressBareTypeCheck = prev }()

	if len(typeArgs) != len(tmpl.TypeParams) {
		return fmt.Errorf("data %s: expected %d type arg(s), got %d",
			tmpl.Name, len(tmpl.TypeParams), len(typeArgs))
	}

	subst := make(map[string]ast.TypeExpr, len(typeArgs)+1)
	for i, name := range tmpl.TypeParams {
		subst[name] = typeArgs[i]
	}

	// Convention: anonymous `_` in a partial-bound trait header (e.g.
	// `tryable[T, Result[_, E]]`) resolves to the impl's success slot,
	// which by Tin convention is the data's first type parameter. This
	// is a syntactic shortcut - the underlying type after substitution
	// is identical to writing `Result[T, E]` explicitly, but lets impls
	// communicate intent ("the success slot is impl-determined") in the
	// signature. Real existential cross-T propagation requires a deeper
	// type-system feature (see masterplan).
	if len(typeArgs) > 0 {
		subst["_"] = typeArgs[0]
	}

	// Substitute the Implements list so trait-impl keys derived from it
	// (via bareTraitImplKey / traitImplKey) line up with the
	// substituted method-qualifier keys produced by methodScopeName.
	// Without this the vtable-emission lookup uses the unsubstituted
	// impl bound while the methods register under the substituted form.
	substImpls := make([]ast.TypeExpr, len(tmpl.Implements))
	for i, impl := range tmpl.Implements {
		substImpls[i] = substituteTypeInTypeExpr(impl, subst)
	}

	concrete := &ast.DataDecl{
		Name:       concreteName,
		Variants:   make([]ast.DataVariant, len(tmpl.Variants)),
		Implements: substImpls,
	}

	for vi, v := range tmpl.Variants {
		newFields := make([]ast.StructField, len(v.Fields))
		for fi, f := range v.Fields {
			newFields[fi] = ast.StructField{
				Name:      f.Name,
				Type:      substituteTypeParams(f.Type, subst),
				Tags:      f.Tags,
				IsForward: f.IsForward,
				IsWeak:    f.IsWeak,
				IsOwn:     f.IsOwn,
				IsConst:   f.IsConst,
				IsVar:     f.IsVar,
			}
		}

		concrete.Variants[vi] = ast.DataVariant{Pos: v.Pos, Name: v.Name, Fields: newFields}
	}

	// Substitute type params in method bodies and rename the receiver
	// type from the generic ADT name to its concrete monomorphization.
	concrete.Methods = make([]*ast.FuncDecl, 0, len(tmpl.Methods))
	for _, m := range tmpl.Methods {
		concrete.Methods = append(concrete.Methods,
			substituteMethod(m, tmpl.Name, concreteName, subst))
	}

	cg.dataDecls[concreteName] = concrete

	for _, v := range concrete.Variants {
		cg.dataVariantLookup[v.Name] = appendUnique(cg.dataVariantLookup[v.Name], concreteName)
	}

	if err := cg.emitConcreteData(concreteName, concrete); err != nil {
		return err
	}

	// Emit method bodies at module scope so the scope entries
	// (`<ConcreteName>_<method>` -> *ir.Func) survive past whichever
	// package's load triggered this monomorphization.  Without the
	// swap, a stdlib package that first instantiates e.g.
	// `Result[string, errors::Err]` (jwt is the canonical example)
	// registers `Result__string__errors__Err_expect` in its own
	// package scope, which is torn down when loadPackageFromSource
	// returns -- and a later `r.expect(...)` from the user program
	// fails to find the method even though the IR func is still
	// linked in.  Mirrors the same dance in genTypeDecl for generic
	// struct monomorphization.
	prevScope := cg.curScope
	if cg.moduleScope != nil && cg.curScope != cg.moduleScope {
		cg.curScope = cg.moduleScope
	}

	err := cg.genDataMethods(concreteName, concrete)
	cg.curScope = prevScope

	return err
}

// substituteTypeParams walks a type expression replacing named type
// parameters with their concrete bindings. Used during ADT monomorphization
// to specialize variant field types.
func substituteTypeParams(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil {
		return nil
	}

	switch t := te.(type) {
	case *ast.WildcardType:
		key := "_"
		if t.Name != "" {
			key = t.Name
		}

		if replaced, ok := subst[key]; ok {
			return replaced
		}

		return t
	case *ast.SimpleType:
		if replaced, ok := subst[t.Name]; ok {
			return replaced
		}

		return t
	case *ast.PointerType:
		return &ast.PointerType{Elem: substituteTypeParams(t.Elem, subst), IsConst: t.IsConst}
	case *ast.ArrayType:
		return &ast.ArrayType{Elem: substituteTypeParams(t.Elem, subst), Size: t.Size}
	case *ast.GenericType:
		newParams := make([]ast.TypeExpr, len(t.TypeParams))
		for i, tp := range t.TypeParams {
			newParams[i] = substituteTypeParams(tp, subst)
		}

		return &ast.GenericType{Name: t.Name, TypeParams: newParams}
	}

	return te
}

// emitConcreteData emits the outer tagged-union struct and the per-variant
// payload structs for a concrete (non-generic, or already-monomorphized) ADT.
func (cg *CodeGen) emitConcreteData(name string, n *ast.DataDecl) error {
	variants := make(map[string]*dataVariantInfo, len(n.Variants))

	var maxSize uint64

	for i, v := range n.Variants {
		payloadFields := make([]irtypes.Type, 0, len(v.Fields))

		for _, f := range v.Fields {
			ft, err := cg.tinTypeToLLVM(f.Type)
			if err != nil {
				return fmt.Errorf("data %s: variant %s: %w", name, v.Name, err)
			}
			// Force the field's struct layout to be complete before
			// the size computation below.  Without this, an ADT
			// monomorphization that fires before its referenced user
			// structs are laid out (Phase A processes the structs in
			// AST order; a generic ADT instantiation triggered by an
			// earlier predeclare hits the struct while its
			// `irtypes.StructType.Fields` slice is still empty) gets
			// llvmTypeSize == 0 for the payload, sizes the payload
			// buffer to the smaller variant's needs, and silently
			// truncates the user struct stored in the larger variant.
			if structFt, ok := ft.(*irtypes.StructType); ok && len(structFt.Fields) == 0 {
				structKey := structFt.Name()
				if sd := cg.structDeclsByName[structKey]; sd != nil {
					if err := cg.genStructLayout(sd); err != nil {
						return fmt.Errorf("data %s: variant %s: forcing layout of %s: %w",
							name, v.Name, structKey, err)
					}
				}
			}

			payloadFields = append(payloadFields, ft)
		}

		payloadSt := irtypes.NewStruct(payloadFields...)

		if len(payloadFields) > 0 {
			if sz := llvmTypeSize(payloadSt); sz > maxSize {
				maxSize = sz
			}
		}

		variants[v.Name] = &dataVariantInfo{
			Tag:         int64(i),
			PayloadType: payloadSt,
			Fields:      v.Fields,
		}
	}

	if maxSize == 0 {
		maxSize = 1
	}

	payloadArr := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypes[name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(name)
		cg.structTypes[name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// Tag widened from i8 to i64 so (a) the trailing [N x i8] payload
	// lands at an 8-byte-aligned offset and (b) the whole ADT struct
	// is itself 8-byte aligned at the alloca / cell level.  Without
	// the widening, payload structs containing i64 fields get
	// bitcast onto a 4-aligned location and aligned loads on
	// aarch64 read garbage (i32 fields fit in the first 4 bytes of
	// the misaligned region and worked by accident; i64+ blew up
	// immediately).
	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I64, payloadArr}

	cg.dataVariants[name] = variants

	if _, ok := cg.dataTypeIDs[name]; !ok {
		cg.dataTypeIDs[name] = cg.nextTypeID
		cg.nextTypeID++
	}

	return nil
}

// wrapDataVariant constructs a value for an ADT variant: sets the type_id,
// writes the tag byte, and stores the packed payload struct into the payload
// buffer. Returns a loaded struct value (the ADT's outer type).
//
// retainMask, if non-nil, parallels args: a `true` entry means the i'th arg
// originates from an existing owner (variable, field access, etc.) and the
// ADT must take its own retain so the scope-exit release of the originating
// owner does not dangle the ADT's pointer. Fresh-literal/call-result args
// already carry RC=1 that is transferred into the ADT, so their entries
// should be `false` (no retain). Callers that cannot distinguish may pass
// nil, which is equivalent to all-false (literal semantics).
func (cg *CodeGen) wrapDataVariant(block *ir.Block, adtName, variantName string, args []value.Value, retainMask []bool) (value.Value, error) {
	vars := cg.dataVariants[adtName]
	if vars == nil {
		return nil, fmt.Errorf("data %s: not registered", adtName)
	}

	vi := vars[variantName]
	if vi == nil {
		return nil, fmt.Errorf("data %s: unknown variant %s", adtName, variantName)
	}

	if len(args) != len(vi.Fields) {
		return nil, fmt.Errorf("data %s: variant %s expects %d fields, got %d",
			adtName, variantName, len(vi.Fields), len(args))
	}

	outerSt := cg.structTypes[adtName]
	if outerSt == nil {
		return nil, fmt.Errorf("data %s: outer LLVM struct missing", adtName)
	}

	alloca := block.NewAlloca(outerSt)

	typeIDGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(cg.dataTypeIDs[adtName])), typeIDGEP)

	tagGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, vi.Tag), tagGEP)

	if len(args) > 0 {
		payloadGEP := block.NewGetElementPtr(outerSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for i, arg := range args {
			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			// Coerce + type-check FIRST so a mismatch errors out before
			// we emit the retain.  Otherwise the retain is leaked when
			// coerce can't bridge the types -- and on the success path
			// the retain would target the pre-coerce value, which is
			// wrong if coerce produced a fresh value (e.g. T -> any
			// boxing).
			fieldType := vi.PayloadType.Fields[i]
			arg = cg.coerce(block, arg, fieldType)

			if !arg.Type().Equal(fieldType) {
				return nil, fmt.Errorf(
					"variant %s field %d: cannot store %s where %s is expected",
					variantName, i,
					cg.tinTypeDisplay(arg.Type()),
					cg.tinTypeDisplay(fieldType))
			}

			f := vi.Fields[i]

			if !f.IsWeak && retainMask != nil && i < len(retainMask) && retainMask[i] {
				cg.emitStructFieldRetain(block, arg)
			}

			block.NewStore(arg, fieldPtr)
		}
	}

	return block.NewLoad(outerSt, alloca), nil
}

// resolveVariantName finds which ADT declares a variant with the given name.
// Returns the single ADT name if unambiguous, an empty string if unknown, or
// an error if ambiguous.
func (cg *CodeGen) resolveVariantName(variantName string) (string, error) {
	adts := cg.dataVariantLookup[variantName]
	switch len(adts) {
	case 0:
		return "", nil
	case 1:
		return adts[0], nil
	default:
		return "", fmt.Errorf("constructor %q is ambiguous (declared by: %v) - qualify explicitly", variantName, adts)
	}
}

// isDataVariant returns true if variantName is known to be a constructor of
// any registered ADT.
func (cg *CodeGen) isDataVariant(variantName string) bool {
	return len(cg.dataVariantLookup[variantName]) > 0
}

// dataVariantInfoFor returns the variant info for an ADT/variant pair.
func (cg *CodeGen) dataVariantInfoFor(adtName, variantName string) *dataVariantInfo {
	if vars, ok := cg.dataVariants[adtName]; ok {
		return vars[variantName]
	}

	return nil
}

// genDataScopeCtorCall handles `Adt::Variant(args)` and `Adt[T, U]::Variant(args)`
// style constructor calls routed through a ScopeAccess expression. Returns
// (value, handled=true, err) when the path matches a known ADT; otherwise
// (nil, false, nil) so the caller can fall through to struct/pkg dispatch.
func (cg *CodeGen) genDataScopeCtorCall(block *ir.Block, fn *ast.ScopeAccess, args []ast.Node) (value.Value, bool, error) {
	if len(fn.Path) < 2 {
		return nil, false, nil
	}

	variantName := fn.Path[len(fn.Path)-1]
	typePart := fn.Path[0]

	if len(fn.Path) > 2 {
		// 3-element paths (`pkg::Adt::Variant`) -- type is the
		// second-to-last component.
		typePart = fn.Path[len(fn.Path)-2]
	}

	typeParamStr := ""
	if i := strings.IndexByte(typePart, '['); i >= 0 {
		typeParamStr = typePart[i+1 : len(typePart)-1]
		typePart = typePart[:i]
	}

	adtName := typePart
	// Accept `pkg::Adt[...]::Variant` (e.g. `result::Result[i64, string]::Ok`)
	// by falling back to the bare name when the package-qualified form
	// is not registered under its qualified key.  ADTs live in a flat
	// namespace; qualification is a disambiguation hint, not a separate
	// declaration.
	if _, ok := cg.dataDecls[adtName]; !ok {
		if idx := strings.LastIndex(adtName, "::"); idx >= 0 {
			bare := adtName[idx+2:]
			if _, ok2 := cg.dataDecls[bare]; ok2 {
				adtName = bare
			}
		}
	}

	if _, ok := cg.dataDecls[adtName]; !ok {
		return nil, false, nil
	}

	// Concrete instance: Option[i32]::Some -> concrete Option__i32.
	if typeParamStr != "" {
		tmpl := cg.dataDecls[adtName]
		if tmpl == nil {
			return nil, true, fmt.Errorf("data %s: template not found", adtName)
		}

		rawParts := splitTopLevel(typeParamStr, ',')
		resolvedParts := make([]string, len(rawParts))
		resolvedTEs := make([]ast.TypeExpr, len(rawParts))

		for i, raw := range rawParts {
			resolvedTEs[i] = parseTypeParamStr(raw)
			// Resolve through typeAliases so names resolved in the same
			// way as the declared function signature types, keeping the
			// monomorphic name consistent (e.g. Value -> json__Value).
			resolvedParts[i] = cg.typeExprCanonicalKey(resolvedTEs[i])
		}

		concreteName := adtName + "__" + strings.Join(resolvedParts, "__")
		if _, done := cg.structTypes[concreteName]; !done {
			if err := cg.monomorphizeDataDecl(tmpl, resolvedTEs, concreteName); err != nil {
				return nil, true, err
			}
		}

		adtName = concreteName
	}

	vi := cg.dataVariantInfoFor(adtName, variantName)
	if vi == nil {
		return nil, true, fmt.Errorf("data %s: unknown variant %q", adtName, variantName)
	}

	if len(args) != len(vi.Fields) {
		return nil, true, fmt.Errorf("data %s: variant %s expects %d argument(s), got %d",
			adtName, variantName, len(vi.Fields), len(args))
	}

	argVals := make([]value.Value, len(args))
	retainMask := make([]bool, len(args))

	for i, a := range args {
		v, err := cg.genExpr(block, a)
		if err != nil {
			return nil, true, err
		}
		// `cur || b`-style short-circuit operands evaluate across
		// several blocks and leave the IR insertion point parked on a
		// merge block; pick that up so the subsequent coerce / wrap
		// stores into the right block (otherwise we emit instructions
		// into the OLD block referencing values that only exist after
		// the merge -- a dominance error).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		argVals[i] = cg.coerce(block, v, vi.PayloadType.Fields[i])
		// retainMask: true means the arg is a borrow whose source still
		// owns the +1 RC, so the ADT needs its own retain to keep the
		// payload alive past the source's scope-exit.  Fresh allocations
		// (`as string`/`as Trait` casts that lower to a runtime call
		// returning rc=1, `_tin_bytes_from_buf` results) already own
		// their rc; a second retain here would leave them unbalanced
		// and leak by exactly 1 per construction.  Mirrors the
		// freshIface / freshCallResult exemptions in the let-binding
		// retain logic at the top of genVarDecl.
		retainMask[i] = isCopyExpr(a) && !isFreshBytesAlloc(argVals[i]) && !isFreshCallResult(argVals[i])
	}

	v, err := cg.wrapDataVariant(block, adtName, variantName, argVals, retainMask)

	return v, true, err
}

// splitTopLevel splits s on sep while respecting `[...]` nesting. Used to
// parse ADT generic arg lists where nested brackets are possible
// (e.g. `Option[Result[i32, string]]`).
func splitTopLevel(s string, sep byte) []string {
	var (
		out   []string
		start int
		depth int
	)

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))

				start = i + 1
			}
		}
	}

	out = append(out, strings.TrimSpace(s[start:]))

	return out
}

// genDataConstructorCall emits a call-style ADT constructor `Variant(args...)`.
// Returns the constructed ADT value (outer struct). Returns (nil, nil) when
// the variant cannot be resolved in the current context, so callers can fall
// through to the normal function-call dispatch.
//
// Resolution order:
//  1. When returnTypeHint is set to a known ADT struct type, prefer that ADT
//     (used for `let x Result[i32,e] = Ok(42)` and arg-position inference).
//  2. Otherwise, if the variant is uniquely owned by one ADT, use it.
//  3. If generic monomorphization hasn't run yet and the variant is still
//     ambiguous, give up (caller likely needs explicit path qualification).
func (cg *CodeGen) genDataConstructorCall(block *ir.Block, variantName string, args []ast.Node) (value.Value, error) {
	adt := cg.preferAdtFromHint(variantName)
	if adt == "" {
		var err error

		adt, err = cg.resolveVariantName(variantName)
		if err != nil {
			return nil, err
		}
	}

	if adt == "" {
		return nil, nil
	}

	vi := cg.dataVariantInfoFor(adt, variantName)
	if vi == nil {
		return nil, nil
	}

	if len(args) != len(vi.Fields) {
		return nil, fmt.Errorf("data %s: variant %s expects %d argument(s), got %d",
			adt, variantName, len(vi.Fields), len(args))
	}

	argVals := make([]value.Value, len(args))
	retainMask := make([]bool, len(args))

	for i, a := range args {
		v, err2 := cg.genExpr(block, a)
		if err2 != nil {
			return nil, err2
		}
		// Pick up any block advance caused by short-circuit operands
		// (`a || b`, `a && b`) before the subsequent coerce/wrap;
		// otherwise we store into the original block while the operand
		// values only exist after the merge, producing a dominance
		// error.  Same fix as in genDataScopeCtorCall.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		expected := vi.PayloadType.Fields[i]
		argVals[i] = cg.coerce(block, v, expected)
		// Same fresh-alloc exemption as the unqualified variant
		// constructor above: skip the retain when the arg already owns
		// its rc (e.g. `as string` slice cast, `_tin_bytes_from_buf`
		// result) so the construction doesn't push rc from 1 to 2 with
		// only one matching release ever fired.
		retainMask[i] = isCopyExpr(a) && !isFreshBytesAlloc(argVals[i]) && !isFreshCallResult(argVals[i])
	}

	return cg.wrapDataVariant(block, adt, variantName, argVals, retainMask)
}

// genDataNullaryConstructor emits a value expression for a nullary variant
// such as `None` or `Leaf`. Returns (nil, nil) when the identifier is not a
// known nullary variant, so callers can fall through to normal lookup.
func (cg *CodeGen) genDataNullaryConstructor(block *ir.Block, variantName string) (value.Value, error) {
	if !cg.isDataVariant(variantName) {
		return nil, nil
	}

	adt := cg.preferAdtFromHint(variantName)
	if adt == "" {
		var err error

		adt, err = cg.resolveVariantName(variantName)
		if err != nil {
			return nil, err
		}
	}

	if adt == "" {
		return nil, nil
	}

	vi := cg.dataVariantInfoFor(adt, variantName)
	if vi == nil || len(vi.Fields) != 0 {
		return nil, nil
	}

	return cg.wrapDataVariant(block, adt, variantName, nil, nil)
}

// preferAdtFromHint picks the ADT name that owns variantName AND matches the
// current returnTypeHint. Used to disambiguate bare constructor calls when
// the expected target type is known (let-bindings with annotation, function
// arguments, return values).
func (cg *CodeGen) preferAdtFromHint(variantName string) string {
	if cg.returnTypeHint == nil {
		return ""
	}

	st, ok := cg.returnTypeHint.(*irtypes.StructType)
	if !ok {
		return ""
	}

	hintAdt := st.Name()
	if hintAdt == "" {
		return ""
	}

	for _, adt := range cg.dataVariantLookup[variantName] {
		if adt == hintAdt {
			return adt
		}
	}

	return ""
}

// isDataMatchPattern reports whether pat is an ADT match arm pattern:
// either a call `Ctor(bindings...)` on a known variant, or a bare
// identifier naming a nullary variant from some ADT. The concrete ADT is
// resolved later from the scrutinee's type; ambiguous variant names (e.g.
// `Empty` declared by both `Box[i64]` and `Box[string]`) are fine here.
func (cg *CodeGen) isDataMatchPattern(pat ast.Node) bool {
	switch p := pat.(type) {
	case *ast.CallExpr:
		if id, ok := p.Func.(*ast.Identifier); ok {
			return cg.isDataVariant(id.Name)
		}

		return false
	case *ast.Identifier:
		// Treat as a nullary-variant pattern only if at least one registered
		// ADT declares a nullary variant with this name.
		for _, adt := range cg.dataVariantLookup[p.Name] {
			if vi := cg.dataVariantInfoFor(adt, p.Name); vi != nil && len(vi.Fields) == 0 {
				return true
			}
		}

		return false
	}

	return false
}

// dataPatternVariantName returns the variant name referenced by a match
// pattern that isDataMatchPattern accepts.
func dataPatternVariantName(pat ast.Node) string {
	switch p := pat.(type) {
	case *ast.CallExpr:
		if id, ok := p.Func.(*ast.Identifier); ok {
			return id.Name
		}
	case *ast.Identifier:
		return p.Name
	}

	return ""
}

// isDataType returns true if the given LLVM struct type corresponds to a
// registered ADT.
func (cg *CodeGen) isDataType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok {
		return false
	}

	_, ok = cg.dataVariants[st.Name()]

	return ok
}

// ensureDataPtrReleaseFn lazily generates a null-safe release function for a
// pointer to an ADT value. It:
//
//  1. Null-guards the pointer.
//  2. Loads the tag byte.
//  3. Switches on tag to a per-variant block.
//  4. In each variant block, bitcasts the payload to the variant's struct
//     layout and emits emitRelease on every RC-tracked or owning-pointer
//     field (the standard struct-field release machinery takes it from
//     there).
//  5. Calls _tin_release on the outer block.
//
// Weak fields are skipped (they're non-owning by construction).
func (cg *CodeGen) ensureDataPtrReleaseFn(adtName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structPtrReleaseFns[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	ptrType := irtypes.NewPointer(st)
	fnName := adtName + "__data_release_ptr"
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("ptr", ptrType))

	cg.structPtrReleaseFns[adtName] = fn

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	exit := fn.NewBlock("exit")

	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load the full struct onto the stack BEFORE decrementing RC so that
	// payload reads remain valid even if _tin_release_struct frees the block.
	loadedVal := doRelease.NewLoad(st, fn.Params[0])
	stackCopy := doRelease.NewAlloca(st)
	doRelease.NewStore(loadedVal, stackCopy)

	tagGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := doRelease.NewLoad(irtypes.I64, tagGEP)

	payloadGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	// Decrement RC on the outer block; proceed to descend into children only
	// when we were the last reference (RC hit 0).
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)

	dispatch := fn.NewBlock("dispatch")
	doRelease.NewCondBr(isOne, dispatch, exit)

	var switchCases []*ir.Case

	for _, e := range sortedVariants(variants) {
		variantName, vi := e.Name, e.Info
		if !cg.variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, vi.Tag), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			cg.emitRelease(caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	dispatch.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// variantHasReleasableField returns true if any of the variant's fields carry
// an owning reference that needs release (RC-tracked type, owning pointer
// to a registered struct/ADT, or embedded named struct that itself has
// RC-tracked fields).
//
// The embedded-struct branch is what catches `Result.Ok(event_with_time)`
// where event_with_time is `struct { name string ... }`: the variant's
// payload field is the struct by value, not a pointer, so the
// pointer-to-struct branch above doesn't fire. Without this check the
// match scrutinee for `match parse(...): Ok(ev) -> ...` is judged
// "no owning fields" and the inner string leaks at scope exit.
func (cg *CodeGen) variantHasReleasableField(vi *dataVariantInfo) bool {
	for i, f := range vi.Fields {
		if f.IsWeak {
			continue
		}

		t := vi.PayloadType.Fields[i]
		if isRCTrackedType(t) {
			return true
		}

		if pt, ok := t.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				return true
			}
		}

		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			if cg.elemNeedsRelease(t) {
				return true
			}
		}
	}

	return false
}

// fieldNeedsOwningRelease returns true when a payload field type represents an
// owning reference (RC-tracked fat type, pointer to a named struct, or
// an embedded named struct with RC fields). See variantHasReleasableField
// for the embedded-struct rationale.
func (cg *CodeGen) fieldNeedsOwningRelease(t irtypes.Type) bool {
	if isRCTrackedType(t) {
		return true
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			return true
		}
	}

	if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
		if cg.elemNeedsRelease(t) {
			return true
		}
	}

	return false
}

// emitDataValueRetain tag-dispatches retain over an ADT value's payload.
func (cg *CodeGen) emitDataValueRetain(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueRetainFn(st.Name(), st)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// emitDataValueRelease releases the active variant's owning fields for an
// ADT value. Implemented as a single call to a per-ADT helper function so
// that the caller's basic block is not split.
func (cg *CodeGen) emitDataValueRelease(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueFieldFn(st.Name(), st,
		"__data_release_val", cg.dataValueReleaseFns,
		(*CodeGen).emitRelease)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// ensureDataValueRetainFn generates a per-ADT helper that retains all owning
// fields of the active variant's payload. The releaser counterpart is inlined
// directly into emitDataValueRelease via ensureDataValueFieldFn.
func (cg *CodeGen) ensureDataValueRetainFn(adtName string, st *irtypes.StructType) *ir.Func {
	return cg.ensureDataValueFieldFn(adtName, st,
		"__data_retain_val", cg.dataValueRetainFns,
		(*CodeGen).emitStructFieldRetain)
}

// ensureDataValueFieldFn is the common skeleton: lookup cache, precompute the
// "any variant has a releasable field" short-circuit, emit the tag-dispatch
// switch, and for each releasable field in each variant call the supplied
// emitField method (a pointer-to-method so the caller can pick retain vs
// release). All owning fields are processed (pointer-to-struct and
// RC-tracked fat types); weak fields are skipped.
func (cg *CodeGen) ensureDataValueFieldFn(
	adtName string,
	st *irtypes.StructType,
	suffix string,
	cache map[string]*ir.Func,
	emitField func(*CodeGen, *ir.Block, value.Value),
) *ir.Func {
	if fn, ok := cache[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	any := false

	for _, vi := range variants {
		if cg.variantHasReleasableField(vi) {
			any = true

			break
		}
	}

	if !any {
		cache[adtName] = nil

		return nil
	}

	fnName := adtName + suffix
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("val", st))
	cache[adtName] = fn

	entry := fn.NewBlock("entry")
	exit := fn.NewBlock("exit")

	stackCopy := entry.NewAlloca(st)
	entry.NewStore(fn.Params[0], stackCopy)

	tagGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := entry.NewLoad(irtypes.I64, tagGEP)

	payloadGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	var switchCases []*ir.Case

	for _, e := range sortedVariants(variants) {
		variantName, vi := e.Name, e.Info
		if !cg.variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, vi.Tag), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			// Trait fat-ptr value fields embedded in an ADT variant
			// payload need value-form retain/release directly.
			// Going through emitField (emitRelease /
			// emitStructFieldRetain) hits walkRCStructFields which
			// doesn't know iface struct shape; pre-fix the iface
			// block leaked on release and aliased without a +1 RC
			// on retain. Both must be paired or copies double-free.
			if ft, ok := vi.PayloadType.Fields[fi].(*irtypes.StructType); ok && isTraitFatPtrShape(ft) {
				emitFatPtrRetainOrRelease(cg, caseBlock, fieldVal, ft, suffix == "__data_retain_val")

				continue
			}

			emitField(cg, caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	entry.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// emitFatPtrRetainOrRelease emits inline retain/release for a value-form
// trait fat-ptr {i8* data, vtable*} embedded in an ADT variant
// payload.
//
// Retain: `_tin_retain(data)` - the iface data block was alloc'd by
// coerceToTrait via _tin_rc_alloc, so retain is always safe (the RC
// header is at data - sizeof(TinRCHdr)).
//
// Release: dispatch through the vtable's data-release thunk (last
// slot) - the thunk decrements the data block's RC and walks nested
// RC fields when the block hits 0. We do NOT additionally
// _tin_release: the thunk already releases the block.
func emitFatPtrRetainOrRelease(cg *CodeGen, block *ir.Block, val value.Value, st *irtypes.StructType, retain bool) {
	dataField := block.NewExtractValue(val, 0)

	if retain {
		block.NewCall(cg.ensureRetain(), dataField)

		return
	}

	vtableField := block.NewExtractValue(val, 1)

	vtablePtrType, ok := st.Fields[1].(*irtypes.PointerType)
	if !ok {
		return
	}

	vtableSt, ok2 := vtablePtrType.ElemType.(*irtypes.StructType)
	if !ok2 || len(vtableSt.Fields) == 0 {
		return
	}

	lastIdx := len(vtableSt.Fields) - 1
	lastFieldType := vtableSt.Fields[lastIdx]

	lastPt, ok3 := lastFieldType.(*irtypes.PointerType)
	if !ok3 {
		return
	}

	lastFnType, ok4 := lastPt.ElemType.(*irtypes.FuncType)
	if !ok4 || len(lastFnType.Params) != 1 ||
		!lastFnType.Params[0].Equal(irtypes.I8Ptr) ||
		!irtypes.IsVoid(lastFnType.RetType) {
		return
	}

	releaseFnSlot := block.NewGetElementPtr(vtableSt, vtableField,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(lastIdx)))
	releaseFn := block.NewLoad(lastFieldType, releaseFnSlot)
	block.NewCall(releaseFn, dataField)
}

// adtHasFatPtrField reports whether any variant of the given ADT
// type carries a trait fat-ptr value field. Used by genReturn to
// decide whether to suppress synthetic iface-data scope-releases for
// the function exit.
func (cg *CodeGen) adtHasFatPtrField(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok {
		return false
	}

	variants := cg.dataVariants[st.Name()]
	if variants == nil {
		return false
	}

	for _, vi := range variants {
		for fi := range vi.Fields {
			if vi.Fields[fi].IsWeak {
				continue
			}

			if ft, ok := vi.PayloadType.Fields[fi].(*irtypes.StructType); ok && isTraitFatPtrShape(ft) {
				return true
			}
		}
	}

	return false
}

// suppressIfaceDataScopeReleases marks every synthetic
// `.iface_data_*` scope entry in the current function scope as
// noRelease, so emitAllScopeReleases skips its _tin_release call.
// Used by genReturn when the return value is an ADT whose payload
// transferred an iface to the caller - the caller's data_release_val
// becomes the sole owner.
func (cg *CodeGen) suppressIfaceDataScopeReleases() {
	if cg.curScope == nil {
		return
	}

	s := cg.curScope
	for s != nil {
		s.each(func(name string, e *scopeEntry) {
			if e.releaseRawPtr {
				e.noRelease = true
			}
		})

		if s.isFunctionBoundary {
			break
		}

		s = s.parent
	}
}

// genAdtIsExpr handles `x is Ctor(bindings...)` and `x is NullaryVariant` on
// an ADT-typed scrutinee. Returns (value, handled=true, err) when it
// recognizes the form; otherwise returns (nil, false, nil) so the caller can
// fall through to the existing union-is-check logic.
func (cg *CodeGen) genAdtIsExpr(block *ir.Block, scrut value.Value, e *ast.IsExpr) (value.Value, bool, error) {
	st, ok := scrut.Type().(*irtypes.StructType)
	if !ok {
		return nil, false, nil
	}

	adtName := st.Name()

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil, false, nil
	}

	variantName := ""

	var binders []string

	// Explicit constructor pattern `is Ok(v)` stored in Pattern.
	if e.Pattern != nil {
		switch p := e.Pattern.(type) {
		case *ast.CallExpr:
			if id, ok2 := p.Func.(*ast.Identifier); ok2 {
				variantName = id.Name
				binders = dataPatternBinders(p)
			}
		case *ast.Identifier:
			variantName = p.Name
		}
	}

	// Nullary variant parsed through the Type path: `is None`, `is Leaf`.
	if variantName == "" && e.Type != nil {
		if simple, ok2 := e.Type.(*ast.SimpleType); ok2 {
			if _, isVariant := variants[simple.Name]; isVariant {
				variantName = simple.Name
			}
		}
	}

	if variantName == "" {
		return nil, false, nil
	}

	vi := variants[variantName]
	if vi == nil {
		return nil, true, fmt.Errorf("data %s: unknown variant %q in is-check", adtName, variantName)
	}

	if len(binders) != 0 && len(binders) != len(vi.Fields) {
		return nil, true, fmt.Errorf("data %s: variant %s expects %d binding(s), got %d",
			adtName, variantName, len(vi.Fields), len(binders))
	}

	alloca := block.NewAlloca(st)
	block.NewStore(scrut, alloca)

	tagGEP := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagVal := block.NewLoad(irtypes.I64, tagGEP)
	cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I64, vi.Tag))

	if len(vi.Fields) > 0 && len(binders) == len(vi.Fields) {
		payloadGEP := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi := range vi.Fields {
			name := binders[fi]
			if name == "" || name == "_" {
				continue
			}

			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldTy := vi.PayloadType.Fields[fi]
			fieldVal := block.NewLoad(fieldTy, fieldPtr)

			bindAlloca := block.NewAlloca(fieldTy)
			block.NewStore(fieldVal, bindAlloca)

			// IsExpr is side-effecting across all clauses of `where`/`if`
			// chains: the load runs even when the tag check fails. Any retain
			// here would operate on mis-interpreted bytes (another variant's
			// payload), so we bind as a borrow (noRelease: true, no retain),
			// matching the semantics of union type-check `if v is n i64`.
			//
			// Callers that need to own the payload should prefer `match`,
			// whose per-case scope only evaluates bindings after the tag has
			// already matched.
			cg.curScope.set(name, &scopeEntry{val: bindAlloca, isAlloc: true, noRelease: true})
		}
	}

	return cmp, true, nil
}

// isExhaustiveDataMatch returns true when every variant of the ADT named by
// the scrutinee is covered by some case arm (and no arm has a guard). Guards
// make exhaustiveness unprovable at compile time, so we conservatively return
// false when any arm is guarded.
func (cg *CodeGen) isExhaustiveDataMatch(s *ast.MatchStmt, adtName string) bool {
	if s.Default != nil {
		return true
	}

	if len(s.Cases) == 0 {
		return false
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return false
	}

	covered := make(map[string]bool, len(variants))

	for _, c := range s.Cases {
		if c.Guard != nil {
			return false
		}

		name := dataPatternVariantName(c.Pattern)
		if name == "" {
			return false
		}

		if _, ok := variants[name]; !ok {
			return false
		}

		covered[name] = true
	}

	for name := range variants {
		if !covered[name] {
			return false
		}
	}

	return true
}

// dataPatternBinders returns the list of binder names (from `case Ok(v):`
// the binders are ["v"]). For nullary patterns, returns nil. An identifier
// pattern is treated as "_" if it is "_" or "nil"; otherwise it is a
// fresh binding with that name.
func dataPatternBinders(pat ast.Node) []string {
	call, ok := pat.(*ast.CallExpr)
	if !ok {
		return nil
	}

	out := make([]string, len(call.Args))

	for i, a := range call.Args {
		switch v := a.(type) {
		case *ast.Identifier:
			out[i] = v.Name
		default:
			out[i] = "_"
		}
	}

	return out
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}

	return append(xs, s)
}
