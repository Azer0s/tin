package codegen

import (
	"fmt"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

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

	cg.recordData(CanonKey(n.Name), n)

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

	cg.recordData(CanonKey(concreteName), concrete)

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

	st := cg.structTypeFor(CanonKey(name))
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(name)
		cg.recordLLVM(CanonKey(name), st)
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
