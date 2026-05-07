package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

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
		if _, ok2 := cg.traits[name]; !ok2 {
			if idx := strings.LastIndex(name, "::"); idx >= 0 {
				name = name[idx+2:]
			}
		}

		trait, ok := cg.traits[name]
		if !ok {
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
				// Bind "this" parameter to this struct type.
				injected := *m
				// Mark the injected method as a trait impl so methodScopeName
				// produces "Struct_<trait>_<method>" (matching what the vtable
				// wrapper looks up). Without this, default-bodied trait methods
				// would be predeclared under the bare name and the wrapper's
				// qualified lookup would miss them.
				injected.TraitQualifier = name

				ptrType := &ast.PointerType{Elem: &ast.SimpleType{Name: n.Name}}
				if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
					injected.Params = append([]ast.Param{
						{Name: "this", Type: ptrType},
					}, injected.Params...)
				} else {
					// Fix this param to use pointer-to-struct so that mutations to
					// forward fields inside the default method body persist to the
					// caller's variable.
					newParams := make([]ast.Param, len(injected.Params))
					copy(newParams, injected.Params)
					newParams[0].Type = ptrType
					injected.Params = newParams
				}

				aug.Methods = append(aug.Methods, &injected)
			}
		}
	}

	return aug
}

func (cg *CodeGen) genStructDecl(n *ast.StructDecl) error {
	if err := cg.genStructLayout(n); err != nil {
		return err
	}

	return cg.genStructMethods(n)
}

// genStructLayout emits the struct's LLVM type definition and records all
// field-level metadata (types, names, tags, vtables, trait-impl list). No
// method body is compiled. Split out of genStructDecl so the package-load
// pipeline can compile field layouts BEFORE ADT payloads are sized, which
// fixes the "ADT payload baked in as [1 x i8] because inner struct was
// still opaque" bug.
func (cg *CodeGen) genStructLayout(n *ast.StructDecl) error {
	if len(n.TypeParams) > 0 {
		return nil // generic template - only compiled when monomorphized
	}

	// Propagate struct-level scoped tags onto matching members before any
	// tag-consuming pass runs. Idempotent: already-applied tags are not
	// re-added.
	if err := cg.propagateStructScopedTags(n); err != nil {
		return err
	}

	orig := n // keep original for Implements list
	n = cg.augmentStructFromTraits(n)
	n.Implements = orig.Implements // preserve for vtable generation
	// Canonical key/IR-name for all maps and LLVM.  For package structs this is
	// "pkgName__StructName" (e.g. "sync__Unit"); for user structs it is bare.
	structKey := cg.pkgStructKey(n.Name)

	if hasTag(n.Tags, "packed") {
		cg.packedStructs[structKey] = true
	}

	if hasTag(n.Tags, "no_copy") {
		cg.noCopyStructs[structKey] = true
	}

	if hasTag(n.Tags, "closed") {
		cg.closedStructs[structKey] = true
	}

	// Record the AST decl + originating file so post-passes (warning checks,
	// reflection helpers) can walk fields and bodies for any struct, not just
	// those in the top-level program.
	cg.structDeclsByName[structKey] = n
	if cg.filename != "" {
		cg.structDeclFiles[structKey] = cg.filename
	}

	// #no_copy fields would let the containing struct's copy alias the
	// no-copy cell -- defeats the whole point. Reject at decl time so the
	// programmer is told to switch to `*S` before any code depends on it.
	for _, f := range n.Fields {
		if name := cg.noCopyValueTypeName(f.Type); name != "" {
			return cg.nodeErr(n,
				"struct %s field %q has type %s which is #no_copy: copying %s would alias the cell. Use *%s instead",
				prettyStructName(structKey), f.Name, prettyStructName(name),
				prettyStructName(structKey), prettyStructName(name))
		}
	}

	st, ok := cg.structTypes[structKey]
	if !ok {
		st = irtypes.NewStruct()
		st.SetName(structKey)
		cg.structTypes[structKey] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// Prepend vtable pointer fields for each non-implicit implemented trait
	// buildTraitFatPtrTypeInst is idempotent; calling it here ensures the vtable
	// struct type is registered before we reference its pointer type.
	var (
		vtableInstKeys   []string
		vtableFieldTypes []irtypes.Type
	)

	for _, impl := range orig.Implements {
		traitName := traitBaseName(impl)
		if traitName == "implicit" || traitName == "coerce" {
			continue
		}
		// Strip package qualifier (e.g. "io::AsyncReader" -> "AsyncReader")
		// so that "struct Foo (io::Bar)" correctly resolves to trait "Bar".
		if _, ok := cg.traits[traitName]; !ok {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		if _, ok := cg.traits[traitName]; !ok {
			continue
		}

		instKey := traitImplKey(impl)
		// If traitImplKey returned a bare name (no pkg__ prefix), check if the trait
		// belongs to a package and use the canonical qualified instKey instead.
		// This ensures bare trait references (e.g. "struct Foo(JsonSerializable)")
		// and qualified references (e.g. "struct Foo(json::JsonSerializable)") both
		// produce the same vtable/fat-ptr types, so that tinTypeToLLVM and coerce agree.
		if !strings.Contains(instKey, "__") {
			if qualKey, ok2 := cg.traitBareToQualInstKey[instKey]; ok2 {
				instKey = qualKey
			}
		}

		typeSubst := map[string]irtypes.Type{}

		if gt, ok2 := impl.(*ast.GenericType); ok2 {
			td := cg.traits[traitName]
			for i, tpName := range td.TypeParams {
				if i < len(gt.TypeParams) {
					lt, err := cg.tinTypeToLLVM(gt.TypeParams[i])
					if err != nil {
						return err
					}

					typeSubst[tpName] = lt
				}
			}
		}

		if _, err := cg.buildTraitFatPtrTypeInst(traitName, instKey, typeSubst); err != nil {
			return err
		}

		vtableSt := cg.traitVtableStructTypes[instKey]
		vtableInstKeys = append(vtableInstKeys, instKey)
		vtableFieldTypes = append(vtableFieldTypes, irtypes.NewPointer(vtableSt))
	}

	cg.structVtableOrder[structKey] = vtableInstKeys

	// Build user field types
	var (
		userFieldTypes []irtypes.Type
		fieldNames     []string
		fieldTinTypes  []ast.TypeExpr
	)

	for _, f := range n.Fields {
		ft, err := cg.tinTypeToLLVM(f.Type)
		if err != nil {
			return err
		}

		userFieldTypes = append(userFieldTypes, ft)
		fieldNames = append(fieldNames, f.Name)
		fieldTinTypes = append(fieldTinTypes, f.Type)
	}

	cg.structFieldTinTypes[structKey] = fieldTinTypes

	// Record weak fields for this struct.
	weakSet := make(map[string]bool)

	for _, f := range n.Fields {
		if f.IsWeak {
			weakSet[f.Name] = true
		}
	}

	cg.structWeakFields[structKey] = weakSet

	// Record const fields for this struct. Writes to these are rejected by
	// checkFieldWritable before codegen emits a store.
	constSet := make(map[string]bool)

	for _, f := range n.Fields {
		if f.IsConst {
			constSet[f.Name] = true
		}
	}

	cg.structConstFields[structKey] = constSet
	// Assign a compile-time type ID for this struct (used by any boxing /
	// runtime type checks).  IDs are stable within a compilation unit.
	if _, exists := cg.structTypeIDs[structKey]; !exists {
		cg.structTypeIDs[structKey] = cg.nextTypeID
		cg.nextTypeID++
	}
	// For cLayoutStructs, declare a %S.native type with just user fields (C layout),
	// then declare %S wrapper as:
	//   { i32 type_id, vtable_ptrs..., i8* c_data_ptr }
	// No inline fields in the LLVM type; handover/literal allocations use
	// sizeof(%S) + sizeof(%S.native) bytes via GEP+1 overflow area.
	// All field accesses go through c_data_ptr for live mutation visibility.
	if cg.cLayoutStructs[structKey] {
		// Build native field types: nested cLayoutStruct fields must use their
		// %S.native types (not the wrapper types) to match the C memory layout.
		// Set structFieldLLVMTypes early so tinStructNativeLLVM can resolve
		// transitively nested cLayoutStruct fields.
		cg.structFieldLLVMTypes[structKey] = userFieldTypes

		nativeFieldTypes := make([]irtypes.Type, len(userFieldTypes))
		for i, ft := range userFieldTypes {
			if innerSt, ok2 := ft.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				if inner, err2 := cg.tinStructNativeLLVM(innerSt.Name()); err2 == nil {
					nativeFieldTypes[i] = inner
				} else {
					nativeFieldTypes[i] = ft
				}
			} else {
				nativeFieldTypes[i] = ft
			}
		}

		nativeSt := irtypes.NewStruct(nativeFieldTypes...)
		nativeSt.SetName(structKey + ".native")

		if cg.packedStructs[structKey] {
			nativeSt.Packed = true
		}

		cg.mod.TypeDefs = append(cg.mod.TypeDefs, nativeSt)
		cg.nativeStructTypes[structKey] = nativeSt
		// Also register under the ".native" key so tinStructNativeLLVM (which caches
		// in cg.structTypes[name+".native"]) finds the already-declared type and does
		// not create a second LLVM type definition.
		cg.structTypes[structKey+".native"] = nativeSt

		// Wrapper: { i32, vtable_ptrs..., i8* c_data_ptr }
		wrapperFields := append([]irtypes.Type{irtypes.I32},
			append(vtableFieldTypes, irtypes.I8Ptr)...)
		st.Fields = wrapperFields
	} else {
		// Final layout: [i32 type_id, vtable_0*, ..., user_field_0, ...]
		// The leading i32 is always field 0; a *struct can be bitcast to *any
		// and the type read directly from field 0.
		st.Fields = append([]irtypes.Type{irtypes.I32}, append(vtableFieldTypes, userFieldTypes...)...)
		if cg.packedStructs[structKey] {
			st.Packed = true
		}
	}

	cg.structFields[structKey] = fieldNames // user-visible names only
	if !cg.cLayoutStructs[structKey] {
		// For cLayoutStructs, structFieldLLVMTypes was already set above (before
		// tinStructNativeLLVM calls for nested field type resolution).
		cg.structFieldLLVMTypes[structKey] = userFieldTypes
	}
	// Record field tags (@"..." annotations).
	fieldTags := make(map[string]string, len(n.Fields))
	for _, f := range n.Fields {
		if len(f.Tags) > 0 {
			fieldTags[f.Name] = f.Tags[0]
		}
	}

	cg.structFieldTags[structKey] = fieldTags

	// Record which traits this struct implements (for traitof).
	// Store the fully-qualified display name (pkg::TraitName form, matching typeof)
	// so that 'traitof' atoms compare consistently with qualified atom literals
	// like '"json::JsonSerializable"'.
	// Bare names (e.g. "JsonSerializable" when used without pkg:: prefix) are
	// normalized to their canonical qualified form via traitBareToQualInstKey.
	var implNames []string

	for _, impl := range n.Implements {
		dn := traitDisplayName(impl)

		// If this is a bare name and we know its canonical pkg-qualified instKey,
		// convert "json__JsonSerializable" -> "json::JsonSerializable" for the display name.
		if idx := strings.LastIndex(dn, "::"); idx < 0 {
			if qualInstKey, ok2 := cg.traitBareToQualInstKey[dn]; ok2 {
				// Convert "pkg__TraitName" -> "pkg::TraitName" for the display form.
				dn = strings.Replace(qualInstKey, "__", "::", 1)
			}
		}

		implNames = append(implNames, dn)
	}

	cg.structImpls[structKey] = implNames

	// Materialize each impl as a section entry for the link-time
	// reflection table walker (D1, codegen/reflect_table.go +
	// runtime/reflect_table.c). Routed to the active pkg module so each
	// pkg's impls land in its own .o.
	for _, tn := range implNames {
		cg.emitImplSectionEntry(structKey, tn)
	}

	return nil
}

// genStructMethods emits method bodies, trait-chain shims, and vtable
// wrappers for a non-generic struct. Must be called after genStructLayout
// for the same declaration AND after ADT layouts are emitted, so that any
// ADT types referenced in method bodies (e.g. Result[T, E] with T a
// package-local struct) see fully-laid-out inner types.
func (cg *CodeGen) genStructMethods(n *ast.StructDecl) error {
	if len(n.TypeParams) > 0 {
		return nil
	}

	orig := n
	n = cg.augmentStructFromTraits(n)
	n.Implements = orig.Implements
	structKey := cg.pkgStructKey(n.Name)

	// Register plain-name aliases for trait-qualified methods BEFORE generating
	// method bodies, so that intra-struct cross-method calls (e.g. another method
	// calling `this.measure()` against `fn iter::measure(this Foo)`) can resolve
	// `Foo_measure` to the qualified impl while the body is being compiled.
	// (For top-level structs, methods are already predeclared via predeclareMethod;
	// for package structs they aren't yet, so this pre-pass is a no-op there and
	// the post-pass below catches them.)
	cg.registerPlainMethodAliases(structKey, n.Methods)

	// Generate methods as top-level functions with struct-qualified names.
	// Methods with their own TypeParams (e.g. map_opt[r]) are stored as templates
	// and monomorphized on-demand at call sites.
	for _, m := range n.Methods {
		if len(m.TypeParams) > 0 {
			templateKey := structKey + "_" + m.Name
			cg.genericMethodTemplates[templateKey] = m

			continue
		}

		if err := cg.genStructMethod(structKey, m); err != nil {
			return err
		}
	}

	// Re-run alias registration AFTER bodies are generated. For package structs
	// methods are predeclared+bodied inline in genStructMethod, so the pre-pass
	// above couldn't see them yet. This pass picks up the now-registered
	// qualified names and exposes them under their bare aliases.
	cg.registerPlainMethodAliases(structKey, n.Methods)

	// Trait init/deinit chaining: for each implemented trait, if the trait defines
	// fn init/deinit with a default body AND the struct overrides that method,
	// compile the trait's version under a unique name so both can be called.
	for _, impl := range orig.Implements {
		traitName := traitBaseName(impl)
		if _, ok := cg.traits[traitName]; !ok {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		td, ok := cg.traits[traitName]
		if !ok {
			continue
		}

		for _, tm := range td.Methods {
			if tm.Body == nil || tm.IsVirtual {
				continue
			}

			if tm.Name != "init" && tm.Name != "deinit" {
				continue
			}
			// Check if the original struct has its own init/deinit (not trait-qualified)
			structHasOwn := false

			for _, sm := range orig.Methods {
				if sm.Name == tm.Name && sm.TraitQualifier == "" {
					structHasOwn = true

					break
				}
			}

			if !structHasOwn {
				continue
			}
			// Compile the trait's method under a unique chain name
			chainName := structKey + "__traitchain__" + traitName + "__" + tm.Name
			if _, exists := cg.curScope.lookup(chainName); !exists {
				if err := cg.genFuncDeclAs(tm, chainName); err != nil {
					return err
				}
			}

			if entry, ok2 := cg.curScope.lookup(chainName); ok2 {
				if fn, ok3 := entry.val.(*ir.Func); ok3 {
					if tm.Name == "init" {
						cg.traitChainedInits[structKey] = append(cg.traitChainedInits[structKey], fn)
					} else {
						cg.traitChainedDeinits[structKey] = append(cg.traitChainedDeinits[structKey], fn)
					}
				}
			}
		}
	}

	// Generate vtable wrappers and global constants for each implemented trait.
	if err := cg.genTraitVtables(n); err != nil {
		return err
	}

	return nil
}

// registerPlainMethodAliases walks methods and aliases each trait-qualified
// method (e.g. fn iter::get on struct Foo, predeclared as Foo_iter_get) under
// its plain name (Foo_get) so that bare call sites resolve. If a plain method
// of the same name exists, it wins; if multiple qualified methods share a
// plain name, the first one wins.
func (cg *CodeGen) registerPlainMethodAliases(structKey string, methods []*ast.FuncDecl) {
	plainMethodNames := map[string]bool{}

	for _, m := range methods {
		if m.TraitQualifier == "" {
			plainMethodNames[m.Name] = true
		}
	}

	for _, m := range methods {
		if m.TraitQualifier == "" {
			continue
		}

		if plainMethodNames[m.Name] {
			continue
		}

		plainName := structKey + "_" + m.Name
		if _, exists := cg.curScope.lookup(plainName); exists {
			continue
		}

		qualName := methodScopeName(structKey, m)
		if entry, ok := cg.curScope.lookup(qualName); ok {
			cg.curScope.set(plainName, entry)

			plainMethodNames[m.Name] = true
		}
	}
}

// checkAllTraitImplsComplete walks every struct declaration and verifies each
// listed trait's virtual methods have a matching qualified impl. Default-bodied
// methods stay optional. Generic struct templates and `implicit` (special trait)
// are skipped - templates are only checked on monomorphization, and implicit
// has its own resolution pathway via implicitConvFns.
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
			if _, ok2 := cg.traits[traitName]; !ok2 {
				if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
					traitName = traitName[idx+2:]
				}
			}

			if traitName == "implicit" || traitName == "coerce" {
				continue
			}

			td, ok2 := cg.traits[traitName]
			if !ok2 {
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

// substituteTypeInTypeExpr replaces named type parameters in a TypeExpr
// using the provided substitution map (param name -> replacement TypeExpr).
func substituteTypeInTypeExpr(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil || len(subst) == 0 {
		return te
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		if rep, ok := subst[t.Name]; ok {
			return rep
		}
	case *ast.GenericType:
		changed := false

		newParams := make([]ast.TypeExpr, len(t.TypeParams))
		for i, p := range t.TypeParams {
			newP := substituteTypeInTypeExpr(p, subst)

			newParams[i] = newP
			if newP != p {
				changed = true
			}
		}

		if changed {
			return &ast.GenericType{Name: t.Name, TypeParams: newParams}
		}
	case *ast.PointerType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.PointerType{Elem: newElem}
		}
	case *ast.ArrayType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.ArrayType{Elem: newElem, Size: t.Size}
		}
	}

	return te
}

// substituteMethod returns a copy of m with type params substituted and
// the self-parameter type renamed from genericName to concreteName.
func substituteMethod(m *ast.FuncDecl, genericName, concreteName string, subst map[string]ast.TypeExpr) *ast.FuncDecl {
	newParams := make([]ast.Param, len(m.Params))
	for i, p := range m.Params {
		newType := substituteTypeInTypeExpr(p.Type, subst)
		// rename the self parameter from the generic struct name to concrete.
		// Handle both value receiver (SimpleType) and pointer receiver (*SimpleType).
		if st, ok := newType.(*ast.SimpleType); ok && st.Name == genericName {
			newType = &ast.SimpleType{Name: concreteName}
		} else if pt, ok := newType.(*ast.PointerType); ok {
			if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 && st.Name == genericName {
				newType = &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
			} else if gt, ok2 := pt.Elem.(*ast.GenericType); ok2 && gt.Name == genericName {
				newType = &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
			}
		}

		newParams[i] = ast.Param{Name: p.Name, Type: newType, IsConst: p.IsConst, IsVarArgs: p.IsVarArgs}
	}

	newRet := substituteTypeInTypeExpr(m.RetType, subst)
	newBody := substituteStructNameInBody(m.Body, genericName, concreteName)

	out := &ast.FuncDecl{
		Name:           m.Name,
		TraitQualifier: m.TraitQualifier,
		TypeParams:     m.TypeParams,
		Constraints:    m.Constraints,
		Params:         newParams,
		RetType:        newRet,
		Body:           newBody,
		Tags:           m.Tags,
		IsStatic:       m.IsStatic,
		IsExtern:       m.IsExtern,
		IsVirtual:      m.IsVirtual,
	}
	out.SetPos(m.Pos())

	return out
}

// substituteStructNameInBody walks the AST body and replaces any StructLit
// with TypeName == genericName to use concreteName instead.
func substituteStructNameInBody(node ast.Node, genericName, concreteName string) ast.Node {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.StructLit:
		newFields := make([]ast.StructLitField, len(n.Fields))
		for i, f := range n.Fields {
			newFields[i] = ast.StructLitField{Name: f.Name, Value: substituteStructNameInBody(f.Value, genericName, concreteName)}
		}

		typeName := n.TypeName
		// Only rename bare (no TypeArgs) struct literals.  If TypeArgs are present,
		// genStructLit will resolve the concrete name at codegen time via typeAliases
		// (set by monomorphizeFunc).  Pre-renaming here AND dropping the TypeArgs
		// causes genStructLit to use the wrong concrete struct (e.g. box__i64 instead
		// of box__string when r=string), producing a type-mismatch panic.
		if typeName == genericName && len(n.TypeArgs) == 0 {
			typeName = concreteName
		}

		return &ast.StructLit{TypeName: typeName, TypeArgs: n.TypeArgs, Fields: newFields, Positional: n.Positional}
	case *ast.Block:
		newStmts := make([]ast.Node, len(n.Stmts))
		for i, s := range n.Stmts {
			newStmts[i] = substituteStructNameInBody(s, genericName, concreteName)
		}

		return &ast.Block{Stmts: newStmts}
	case *ast.ReturnStmt:
		return &ast.ReturnStmt{Value: substituteStructNameInBody(n.Value, genericName, concreteName)}
	case *ast.VarDecl:
		return &ast.VarDecl{Name: n.Name, Type: n.Type, Value: substituteStructNameInBody(n.Value, genericName, concreteName), IsConst: n.IsConst}
	case *ast.IfStmt:
		newIf := *n

		newIf.Cond = substituteStructNameInBody(n.Cond, genericName, concreteName)
		if n.Then != nil {
			if b, ok := substituteStructNameInBody(n.Then, genericName, concreteName).(*ast.Block); ok {
				newIf.Then = b
			}
		}

		if n.Else != nil {
			if b, ok := substituteStructNameInBody(n.Else, genericName, concreteName).(*ast.Block); ok {
				newIf.Else = b
			}
		}

		return &newIf
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substituteStructNameInBody(a, genericName, concreteName)
		}

		return &ast.CallExpr{Func: substituteStructNameInBody(n.Func, genericName, concreteName), Args: newArgs}
	case *ast.BinExpr:
		return &ast.BinExpr{Left: substituteStructNameInBody(n.Left, genericName, concreteName), Op: n.Op, Right: substituteStructNameInBody(n.Right, genericName, concreteName)}
	case *ast.FieldAccess:
		return &ast.FieldAccess{Expr: substituteStructNameInBody(n.Expr, genericName, concreteName), Field: n.Field, IsPtr: n.IsPtr}
	}

	return node
}

// genTypeDecl handles "type X = SomeType [override = ...]" declarations.
// For simple aliases (type char = u8) the alias was already recorded in
// preregister; this function handles the struct-monomorphization case
// (type point = tuple[f32]) which requires actual LLVM type generation.
// expandGenericAlias handles a TypeDecl whose RHS names a generic type
// alias rather than a concrete struct template. It substitutes the outer
// synthetic decl's type arguments into the alias's RHS and then calls
// genTypeDecl on the expanded decl. Enforces the alias's where-bounds at
// expansion time with concrete types so the error message names the
// alias's constraint, not the underlying struct's.
func (cg *CodeGen) expandGenericAlias(synth *ast.TypeDecl, aliasTmpl *ast.TypeDecl, aliasInstance *ast.GenericType) error {
	subst := make(map[string]ast.TypeExpr, len(aliasTmpl.TypeParams))
	for i, paramName := range aliasTmpl.TypeParams {
		if i < len(aliasInstance.TypeParams) {
			subst[paramName] = aliasInstance.TypeParams[i]
		}
	}
	// Enforce the alias's own bounds before expanding.
	for _, c := range aliasTmpl.Constraints {
		argTE, ok := subst[c.TypeParam]
		if !ok {
			continue
		}

		concreteName := typeExprToString(argTE)
		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return fmt.Errorf("%d:%d: type %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, aliasTmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
	}
	// Expand the alias RHS with the concrete substitution. The RHS is
	// likely a GenericType (`Pair[string, T]`) whose T we substitute.
	expandedRHS := substituteTypeInTypeExpr(aliasTmpl.Type, subst)

	expandedDecl := &ast.TypeDecl{
		Name: synth.Name,
		Type: expandedRHS,
	}

	return cg.genTypeDecl(expandedDecl)
}

func (cg *CodeGen) genTypeDecl(n *ast.TypeDecl) error {
	// Tagged union alias: "type u = i8 | string"
	if ut, ok := n.Type.(*ast.UnionTypeExpr); ok {
		return cg.genTaggedUnionTypeDecl(n.Name, ut)
	}
	// Register generic type aliases (those with their own TypeParams) so
	// `StrPair[i32]{...}` can resolve by substituting the alias's params
	// into its RHS and re-monomorphizing the underlying struct.
	// Checking for `len(n.TypeParams) > 0 && alias is generic or compound`
	// covers the StrPair/Pair case without interfering with concrete
	// aliases like `type BoxI32 = Box[i32]` which have no TypeParams.
	if len(n.TypeParams) > 0 {
		cg.genericTypeAliases[n.Name] = n
	}

	gt, ok := n.Type.(*ast.GenericType)
	if !ok {
		// Simple alias - already registered in preregister. Nothing to do.
		return nil
	}

	arity := len(gt.TypeParams)

	var tmpl *ast.StructDecl

	isTmpl := false

	qualGtName := cg.typeExprCanonicalKey(&ast.SimpleType{Name: gt.Name})
	if arityMap, ok := cg.genericStructsByArity[qualGtName]; ok {
		tmpl, isTmpl = arityMap[arity]
	}
	// If the referenced name is ITSELF a generic type alias, expand it
	// recursively: substitute the outer alias's type params into the
	// inner alias's RHS and rerun genTypeDecl on the expanded decl. This
	// lets a chain like `type Wrapper[T] = StrPair[T]` work.
	if !isTmpl {
		if aliasTmpl, isAlias := cg.genericTypeAliases[gt.Name]; isAlias && len(gt.TypeParams) == len(aliasTmpl.TypeParams) {
			return cg.expandGenericAlias(n, aliasTmpl, gt)
		}
	}

	if !isTmpl {
		// GenericType refers to something other than a generic struct
		// (e.g. a generic trait instantiation used as a type alias).
		cg.typeAliases[n.Name] = n.Type

		return nil
	}

	// Concrete generic alias (no type params on the alias itself, like
	// `type GI = G[i64]`): instead of building a duplicate struct named
	// GI, monomorphize G[i64] under its canonical name (G__i64) and
	// register GI as an alias for that canonical name. Without this,
	// `let outer = G[GI]{v: inner}` would resolve GI as a fresh struct
	// distinct from G__i64 and the store would type-mismatch on inner.
	//
	// Skip when the alias declaration carries method overrides -- those
	// overrides need to live on a distinct struct (the alias name), so
	// keep the existing monomorphize-as-separate-struct path for that
	// case. See examples/type_alias.tin "override show method".
	if len(n.TypeParams) == 0 && len(n.Overrides) == 0 {
		canonicalName := cg.typeExprCanonicalKey(gt)
		if canonicalName != n.Name {
			if _, alreadyDone := cg.structTypes[canonicalName]; !alreadyDone {
				if err := cg.genTypeDecl(&ast.TypeDecl{
					Name: canonicalName,
					Type: gt,
				}); err != nil {
					return err
				}
			}

			cg.typeAliases[n.Name] = &ast.SimpleType{Name: canonicalName}

			return nil
		}
	}

	// Build type-parameter substitution: tmpl.TypeParams[i] -> gt.TypeParams[i]
	subst := make(map[string]ast.TypeExpr)

	for i, paramName := range tmpl.TypeParams {
		if i < len(gt.TypeParams) {
			subst[paramName] = gt.TypeParams[i]
		}
	}

	// Validate generic type constraints (e.g. "where t is addable").
	// Build a string-keyed map from the substitution for constraint checking.
	typeSubst := make(map[string]string, len(subst))
	for param, te := range subst {
		typeSubst[param] = typeExprToString(te)
	}
	// Struct-template's own constraints (e.g. the template says T must be
	// ord). These apply to any instantiation regardless of the type alias.
	for _, c := range tmpl.Constraints {
		concreteName, ok := typeSubst[c.TypeParam]
		if !ok {
			continue
		}

		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return fmt.Errorf("%d:%d: struct %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, tmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
	}
	// Type-alias's own constraints. These only make sense on a concrete
	// instantiation (e.g. StrPair[i32]), not on the template declaration
	// where every alias type parameter is still symbolic. Detect that by
	// checking whether any of the template's type-parameter names appears
	// in the RHS's type arguments; if so, skip the check and let the
	// instantiation path re-check with concrete substitutes.
	if len(n.Constraints) > 0 && !typeArgsContainAnyOf(gt.TypeParams, n.TypeParams) {
		aliasSubst := make(map[string]string, len(n.TypeParams))

		for i, paramName := range n.TypeParams {
			if i < len(gt.TypeParams) {
				aliasSubst[paramName] = typeExprToString(gt.TypeParams[i])
			}
		}

		for _, c := range n.Constraints {
			concreteName, ok := aliasSubst[c.TypeParam]
			if !ok {
				continue
			}

			if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
				return fmt.Errorf("%d:%d: type %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
					c.Pos.Line, c.Pos.Col, n.Name, concreteName, concreteName,
					c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
			}
		}
	}

	// Build the concrete struct by substituting type params in every field and trait.
	// Implements must be substituted so that e.g. Future[t](Awaitable[t]) ->
	// Future__i64(Awaitable[i64]) uses the correct concrete trait instance key.
	var concreteImpls []ast.TypeExpr
	for _, impl := range tmpl.Implements {
		concreteImpls = append(concreteImpls, substituteTypeInTypeExpr(impl, subst))
	}

	concrete := &ast.StructDecl{
		Name:       n.Name,
		Implements: concreteImpls,
		Tags:       tmpl.Tags,
		ScopedTags: tmpl.ScopedTags,
	}
	for _, f := range tmpl.Fields {
		concrete.Fields = append(concrete.Fields, ast.StructField{
			Pos:       f.Pos,
			Name:      f.Name,
			Type:      substituteTypeInTypeExpr(f.Type, subst),
			Tags:      f.Tags,
			IsForward: f.IsForward,
			IsWeak:    f.IsWeak,
			IsOwn:     f.IsOwn,
			IsConst:   f.IsConst,
			IsVar:     f.IsVar,
		})
	}

	// Build method set: start with template methods (substituted), then
	// apply overrides from the TypeDecl.
	overrideSet := make(map[string]*ast.FuncDecl)
	for _, ov := range n.Overrides {
		overrideSet[ov.Name] = ov
	}

	for _, m := range tmpl.Methods {
		if ov, ok := overrideSet[m.Name]; ok {
			concrete.Methods = append(concrete.Methods, ov)

			delete(overrideSet, m.Name)

			continue
		}

		// Method-level where guards: a method on a generic struct whose
		// `where t is X` clause does NOT hold for the concrete
		// instantiation is dead-stripped from this concrete struct.
		// Calling the method on the wrong instantiation then produces
		// the dead-strip diagnostic at the call site (see emitDeadStripError)
		// listing every failing constraint -- one entry per stripped
		// overload, since the same method name can have multiple
		// where-guarded variants.
		if witness := cg.methodConstraintWitness(m, typeSubst); witness != "" {
			if cg.deadStrippedMethods[n.Name] == nil {
				cg.deadStrippedMethods[n.Name] = make(map[string][]string)
			}

			cg.deadStrippedMethods[n.Name][m.Name] = append(cg.deadStrippedMethods[n.Name][m.Name], witness)

			continue
		}

		concrete.Methods = append(concrete.Methods, substituteMethod(m, tmpl.Name, n.Name, subst))
	}
	// Any overrides that don't shadow a template method are appended.
	for _, ov := range n.Overrides {
		if _, already := overrideSet[ov.Name]; !already {
			continue // already applied above
		}

		concrete.Methods = append(concrete.Methods, ov)
	}

	// Where-guard ambiguity check: if two methods share the same name and
	// signature and BOTH satisfied their where-guards for this concrete
	// instantiation, the call site can't pick between them -- without
	// this check the user gets a misleading "no matching overload" error
	// at the call site instead.
	if amb := findAmbiguousMethods(concrete.Methods); amb != nil {
		return cg.ambiguousMethodError(n.Name, amb)
	}

	// Propagate the template's scoped tags onto the fresh concrete's
	// members. Must happen before the pre-registration loops below that
	// inspect m.Tags (for #async, overloads, predeclare).
	if err := cg.propagateStructScopedTags(concrete); err != nil {
		return err
	}

	// Register the concrete struct type (opaque first, just like preregister).
	// n.Name is already the full concrete name (e.g. "Future__sync__Unit"), so
	// no package prefix should be applied.
	if _, exists := cg.structTypes[n.Name]; !exists {
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// Register type-param substitutions as aliases so that expressions inside
	// method bodies (e.g. `let out T`, `sizeof(T)`) resolve to the concrete type.
	prevAliases := make(map[string]ast.TypeExpr, len(subst))
	for param, typeExpr := range subst {
		if old, had := cg.typeAliases[param]; had {
			prevAliases[param] = old
		}

		cg.typeAliases[param] = typeExpr
	}

	// Compile struct methods at module scope so they are visible everywhere,
	// not just inside the function that triggered the on-demand monomorphization.
	// Clear currentPkg so that the concrete struct (whose name already embeds
	// the canonical form) does not get an additional package prefix applied.
	prevScope := cg.curScope
	if cg.moduleScope != nil && cg.curScope != cg.moduleScope {
		cg.curScope = cg.moduleScope
	}

	prevPkg := cg.currentPkg
	cg.currentPkg = ""

	// Detect overloaded method names and predeclare all concrete methods exactly
	// once. This mirrors pass 1.8/1.9 for non-generic structs and is required so
	// that overload entries in cg.overloads are registered before genStructDecl
	// compiles method bodies and before call sites resolve variants.
	// The guard prevents double-registration when genTypeDecl is called more than
	// once for the same concrete type name.
	if !cg.genericMethodsSetUp[n.Name] {
		cg.genericMethodsSetUp[n.Name] = true

		methodCounts := make(map[string]int)

		for _, m := range concrete.Methods {
			if len(m.TypeParams) > 0 || m.IsExtern != "" {
				continue
			}

			key := methodScopeName(n.Name, m)
			methodCounts[key]++
		}

		for key, count := range methodCounts {
			if count > 1 {
				cg.overloadedNames[key] = true
			}
		}

		for _, m := range concrete.Methods {
			if len(m.TypeParams) > 0 {
				continue
			}

			if preErr := cg.predeclareMethod(n.Name, m); preErr != nil {
				cg.currentPkg = prevPkg
				cg.curScope = prevScope

				return preErr
			}

			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}

			scopeKey := methodScopeName(n.Name, m)
			cg.coroCallable[scopeKey] = true

			cg.funcDecls[scopeKey] = m

			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.currentPkg = prevPkg
				cg.curScope = prevScope

				return preErr
			}
		}
	}

	err := cg.genStructDecl(concrete)
	cg.currentPkg = prevPkg
	cg.curScope = prevScope

	// genStructDecl tagged structDeclFiles[concrete.Name] with cg.filename
	// -- but cg.filename here is whoever instantiated the generic, NOT the
	// template's source. Override so per-line `//!-Wno-` directives on
	// the template (e.g. Channel's _ptr field) silence the diagnostic
	// for every monomorphization.
	//
	// The template was originally registered under its OWN package's key
	// (e.g. "sync__Channel"), but currentPkg has been restored to the
	// instantiating context. Look up the template by both the
	// current-pkg-prefixed key and the bare name, then sweep for any
	// "<pkg>__<tmplName>" entry as a final fallback so cross-package
	// generic instantiations also pick up the template's source path.
	if tmplFile := cg.lookupTemplateFile(tmpl.Name); tmplFile != "" {
		cg.structDeclFiles[concrete.Name] = tmplFile
	}

	// Restore previous type aliases (removes the T->string etc. temporaries).
	for param := range subst {
		if old, had := prevAliases[param]; had {
			cg.typeAliases[param] = old
		} else {
			delete(cg.typeAliases, param)
		}
	}

	return err
}

// buildTraitFatPtrType computes (and caches) the LLVM fat-pointer type for a
// trait: { i8*, vtable_struct* }.  The vtable struct has one fn-ptr slot per
// trait method, each with signature (i8* self, ...) -> ret.
// traitImplKey returns a unique string key for a trait impl TypeExpr.
// Package qualifiers are converted from "::" to "__" so that
// "sync::Awaitable" and "Awaitable" (if it is the same trait) keep distinct
// keys per package when needed, while still being safely usable as identifiers.
// For "named" -> "named"; for "iter[i64]" -> "iter__i64".
func traitImplKey(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return strings.ReplaceAll(t.Name, "::", "__")
	case *ast.GenericType:
		key := strings.ReplaceAll(t.Name, "::", "__")
		for _, tp := range t.TypeParams {
			key += "__" + traitImplKey(tp)
		}

		return key
	}

	return "unknown"
}

// bareTraitImplKey is like traitImplKey but strips the module prefix from the
// outer trait name only ("io::AsyncReader" -> "AsyncReader",
// "iter[json::Value]" -> "iter__json__Value"). Used to derive the scope-name
// suffix that methodScopeName produces from a user-written trait qualifier
// like "fn AsyncReader::read", so the vtable wrapper can resolve impls written
// without the module prefix.
func bareTraitImplKey(te ast.TypeExpr) string {
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

		key := name
		for _, tp := range t.TypeParams {
			key += "__" + traitImplKey(tp)
		}

		return key
	}

	return "unknown"
}

// resolveTypeWithSubst converts a TypeExpr to LLVM type, substituting any

// buildTraitFatPtrType computes (and caches) the fat-pointer type for a
// non-generic trait by instKey == traitName.
func (cg *CodeGen) buildTraitFatPtrType(traitName string) (*irtypes.StructType, error) {
	return cg.buildTraitFatPtrTypeInst(traitName, traitName, nil)
}

// buildTraitFatPtrTypeInst computes and caches the fat-pointer type for a
// (possibly generic) trait instantiation.
//
//	traitName - base trait name (e.g. "iter")
//	instKey   - unique instance key (e.g. "iter_i64")
//	typeSubst - map from trait type-param name -> concrete LLVM type
func (cg *CodeGen) buildTraitFatPtrTypeInst(traitName, instKey string, typeSubst map[string]irtypes.Type) (*irtypes.StructType, error) {
	if fp, ok := cg.traitFatPtrTypes[instKey]; ok {
		return fp, nil
	}

	td, ok := cg.traits[traitName]
	if !ok {
		return nil, fmt.Errorf("unknown trait: %s", traitName)
	}

	// Pre-register stub types in the cache before resolving method types.
	// This breaks potential infinite recursion when a trait method returns
	// its own trait type (e.g. fn inc(this counter) counter = ...).
	vtableStub := irtypes.NewStruct()
	vtableStub.SetName(instKey + "_vtable")
	fatPtrStub := irtypes.NewStruct(irtypes.I8Ptr, irtypes.NewPointer(vtableStub))
	fatPtrStub.SetName(instKey + "_iface")
	cg.traitVtableStructTypes[instKey] = vtableStub
	cg.traitFatPtrTypes[instKey] = fatPtrStub
	cg.traitInstKeys[instKey] = traitName

	var (
		methodNames []string
		fnPtrTypes  []irtypes.Type
	)

	if td.IsAlias {
		// "trait X as fn(params) ret" - single method whose name is the trait name
		// and whose signature comes from the alias function type.
		ft, ok := td.AliasType.(*ast.FuncType)
		if !ok {
			return nil, fmt.Errorf("trait %s: alias type is not a function type", traitName)
		}

		methodNames = []string{traitName}
		params := []irtypes.Type{irtypes.I8Ptr} // implicit self

		for _, p := range ft.Params {
			pt, err := cg.resolveTypeWithSubst(p, typeSubst)
			if err != nil {
				return nil, err
			}

			params = append(params, pt)
		}

		var ret irtypes.Type = irtypes.Void

		if ft.RetType != nil {
			var err error

			ret, err = cg.resolveTypeWithSubst(ft.RetType, typeSubst)
			if err != nil {
				return nil, err
			}
		}

		fnPtrTypes = []irtypes.Type{irtypes.NewPointer(irtypes.NewFunc(ret, params...))}
	} else {
		for _, m := range td.Methods {
			methodNames = append(methodNames, m.Name)
			// Wrapper signature: (i8* self, non-self params...) -> ret
			params := []irtypes.Type{irtypes.I8Ptr}

			for i, p := range m.Params {
				if i == 0 {
					continue // skip self
				}

				pt, err := cg.resolveTypeWithSubst(p.Type, typeSubst)
				if err != nil {
					return nil, err
				}

				params = append(params, pt)
			}

			var ret irtypes.Type = irtypes.Void

			if m.RetType != nil {
				var err error

				ret, err = cg.resolveTypeWithSubst(m.RetType, typeSubst)
				if err != nil {
					return nil, err
				}
			}

			ft := irtypes.NewFunc(ret, params...)
			fnPtrTypes = append(fnPtrTypes, irtypes.NewPointer(ft))
		}

		// Append $coro slots (after all sync slots) for {#async} virtual methods.
		// Each $coro slot has signature (i8* self, params...) -> i8*.
		var asyncNames []string

		for _, m := range td.Methods {
			if !isAsyncTag(m.Tags) {
				continue
			}

			asyncNames = append(asyncNames, m.Name)
			coroParams := []irtypes.Type{irtypes.I8Ptr}

			for i, p := range m.Params {
				if i == 0 {
					continue // skip self
				}

				pt, err := cg.resolveTypeWithSubst(p.Type, typeSubst)
				if err != nil {
					return nil, err
				}

				coroParams = append(coroParams, pt)
			}

			fnPtrTypes = append(fnPtrTypes, irtypes.NewPointer(irtypes.NewFunc(irtypes.I8Ptr, coroParams...)))
		}

		cg.traitAsyncMethodNames[traitName] = asyncNames
	}

	// Fill in the vtable struct fields now that method types are resolved.
	vtableStub.Fields = fnPtrTypes
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, vtableStub)
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, fatPtrStub)

	cg.traitMethodOrder[traitName] = methodNames // shared across instantiations

	return fatPtrStub, nil
}

// genTraitVtables generates, for each trait that structName implements:
//  1. One wrapper function per trait method: structName__instKey__methodName(i8* self, ...)
//  2. One vtable global constant referencing those wrappers.
func (cg *CodeGen) genTraitVtables(n *ast.StructDecl) error {
	// Use the canonical struct key (e.g. "sync__Unit") for all map lookups and
	// IR names so that structs from different packages never collide.
	structKey := cg.pkgStructKey(n.Name)
	for _, impl := range n.Implements {
		traitName := traitBaseName(impl)

		// Special trait: implicit
		// No vtable: find the static method whose first-param type matches T,
		// then register it as an implicit conversion function.
		if traitName == "implicit" {
			gt, ok := impl.(*ast.GenericType)
			if ok && len(gt.TypeParams) > 0 {
				srcLLVM, err := cg.tinTypeToLLVM(gt.TypeParams[0])
				if err == nil {
					for _, m := range n.Methods {
						if !m.IsStatic || len(m.Params) != 1 {
							continue
						}

						paramLLVM, err2 := cg.tinTypeToLLVM(m.Params[0].Type)
						if err2 != nil {
							continue
						}

						if paramLLVM.Equal(srcLLVM) {
							// Multiple `static fn ::implicit(...)` impls share the
							// same scope key and get overload-mangled. Look up by
							// the mangled name when this method is in the overload
							// set, otherwise the bare key.
							key := methodScopeName(structKey, m)

							lookupName := key
							if cg.overloadedNames[key] {
								lookupName = overloadMangledName(key, methodParamSig(m, structKey))
							}

							if fnEntry, ok2 := cg.curScope.lookup(lookupName); ok2 {
								if fn, ok3 := fnEntry.val.(*ir.Func); ok3 {
									cg.implicitConvFns[structKey] = append(
										cg.implicitConvFns[structKey],
										implicitConvEntry{srcLLVM: srcLLVM, fn: fn},
									)
								}
							}

							break
						}
					}
				}
			}

			continue // no vtable for implicit
		}

		// Special trait: coerce[T]
		//
		// Direction-flip of implicit[T]: `implicit[T]` says "T flows
		// into S" while `coerce[T]` says "S flows into T".  The user
		// writes `static fn ::coerce(this S) T = ...` and any `s as T`
		// dispatches through that method.  The receiver is value-form
		// `this S`, mirroring the operator-trait convention; the body
		// returns a fresh T derived from the struct's data.
		//
		// At call sites, genAsExpr looks up the coerce registry by
		// the source struct's key; if the requested target type
		// matches a registered method's return type, the cast lowers
		// to a direct call to that static fn.  Multiple
		// implementations are resolved by overload mangling on the
		// (Struct, RetType) pair so `coerce[i64]` and `coerce[string]`
		// can co-exist on the same struct.
		if traitName == "coerce" {
			gt, ok := impl.(*ast.GenericType)
			if !ok || len(gt.TypeParams) == 0 {
				continue
			}

			tgtLLVM, err := cg.tinTypeToLLVM(gt.TypeParams[0])
			if err != nil {
				continue
			}

			selfLLVM := cg.structTypes[structKey]

			for _, m := range n.Methods {
				if !m.IsStatic || len(m.Params) != 1 {
					continue
				}

				if m.Name != "coerce" {
					continue
				}

				paramLLVM, err2 := cg.tinTypeToLLVM(m.Params[0].Type)
				if err2 != nil {
					continue
				}

				if selfLLVM != nil && !paramLLVM.Equal(selfLLVM) {
					continue
				}

				retLLVM, err3 := cg.tinTypeToLLVM(m.RetType)
				if err3 != nil {
					continue
				}

				if !retLLVM.Equal(tgtLLVM) {
					continue
				}

				key := methodScopeName(structKey, m)

				lookupName := key
				if cg.overloadedNames[key] {
					lookupName = overloadMangledName(key, methodParamSig(m, structKey))
				}

				if fnEntry, ok2 := cg.curScope.lookup(lookupName); ok2 {
					if fn, ok3 := fnEntry.val.(*ir.Func); ok3 {
						cg.coerceConvFns[structKey] = append(
							cg.coerceConvFns[structKey],
							coerceConvEntry{tgtLLVM: tgtLLVM, fn: fn},
						)
					}
				}

				break
			}

			continue // no vtable for coerce
		}

		// Strip package qualifier from traitName (e.g. "io::AsyncReader" -> "AsyncReader").
		if _, ok2 := cg.traits[traitName]; !ok2 {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		td, ok := cg.traits[traitName]
		if !ok {
			continue
		}

		instKey := traitImplKey(impl)
		// Normalize bare instKeys to their canonical qualified form so that
		// bare trait refs ("JsonSerializable") and qualified refs ("json::JsonSerializable")
		// use the same vtable/fat-ptr types.
		if !strings.Contains(instKey, "__") {
			if qualKey, ok2 := cg.traitBareToQualInstKey[instKey]; ok2 {
				instKey = qualKey
			}
		}

		vtableKey := structKey + "__" + instKey
		if _, ok := cg.traitVtableGlobals[vtableKey]; ok {
			continue // already generated
		}

		// Build type substitution for generic traits.
		typeSubst := map[string]irtypes.Type{}

		if gt, ok := impl.(*ast.GenericType); ok {
			for i, tpName := range td.TypeParams {
				if i < len(gt.TypeParams) {
					lt, err := cg.tinTypeToLLVM(gt.TypeParams[i])
					if err != nil {
						return err
					}

					typeSubst[tpName] = lt
				}
			}
		}

		// Ensure the fat-pointer type (and vtable struct) is built.
		if _, err := cg.buildTraitFatPtrTypeInst(traitName, instKey, typeSubst); err != nil {
			return err
		}

		vtableSt := cg.traitVtableStructTypes[instKey]
		methodNames := cg.traitMethodOrder[traitName]

		structSt := cg.structTypes[structKey]
		if structSt == nil {
			continue
		}

		structPtrType := irtypes.NewPointer(structSt)

		// Pre-declare the vtable global with a zero initializer so that wrappers
		// generating fat-ptr return values can reference it before it is filled in.
		// Its initializer will be set to the real vtable constant after wrappers are built.
		preDeclaredVtableGlobal := cg.activeModule().NewGlobal(vtableKey+"_vtable_data", vtableSt)
		preDeclaredVtableGlobal.Immutable = true
		cg.traitVtableGlobals[vtableKey] = preDeclaredVtableGlobal

		// Generate one wrapper per trait method.
		var wrappers []constant.Constant

		// Bare-name key for matching qualified impls, e.g. "AsyncReader" or
		// "iter__i64". Used by both sync and $coro wrapper lookups.
		bareKey := traitQualifierKey(bareTraitImplKey(impl))

		// Trait methods with default bodies (non-virtual) keep accepting the bare
		// override form `fn methodName(this Foo)` - that's how init/deinit
		// chaining works and how struct-side overrides for any default-bodied
		// trait method are written. Virtual methods MUST be qualified.
		isDefaultBodied := map[string]bool{}

		for _, tm := range td.Methods {
			if !tm.IsVirtual && tm.Body != nil {
				isDefaultBodied[tm.Name] = true
			}
		}

		for i, methodName := range methodNames {
			wrapSlot := vtableSt.Fields[i].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			wrapperName := structKey + "__" + instKey + "__" + methodName
			wrapParams := make([]*ir.Param, len(wrapSlot.Params))

			wrapParams[0] = ir.NewParam("self", irtypes.I8Ptr)
			for pi := 1; pi < len(wrapSlot.Params); pi++ {
				wrapParams[pi] = ir.NewParam(fmt.Sprintf("a%d", pi), wrapSlot.Params[pi])
			}

			wrapFn := cg.activeModule().NewFunc(wrapperName, wrapSlot.RetType, wrapParams...)

			entry := wrapFn.NewBlock("entry")
			// Cast i8* self -> structType*, load struct value.
			selfPtr := entry.NewBitCast(wrapParams[0], structPtrType)
			selfVal := entry.NewLoad(structSt, selfPtr)

			// Look up concrete method. The qualified scope name uses the bare
			// trait name (module prefix stripped) so that fn AsyncReader::read
			// and fn io::AsyncReader::read both resolve when the struct lists
			// either form. registerPlainMethodAliases also wires up the bare
			// alias (Struct_method -> Struct_<trait>_method) so user code that
			// calls the method without qualification still resolves; the vtable
			// wrapper, however, requires the qualified form so a bare-named
			// method that happens to share the trait method's name does not
			// silently bind.
			// Scope-name forms accepted, in priority order:
			//   1. qualifiedName     - includes type args ("Struct_iter__i64_method")
			//                          for explicit `fn iter[i64]::method` impls.
			//   2. baseQualifiedName - no type args ("Struct_iter_method") for the
			//                          common `fn iter::method` form which covers
			//                          all instantiations the struct lists.
			//   3. bareName          - "Struct_method" - only accepted when the trait
			//                          method has a default body (init/deinit chain
			//                          pattern, override of any default-bodied method).
			//                          Virtual methods must be qualified.
			qualifiedName := structKey + "_" + bareKey + "_" + methodName
			baseQualifiedName := structKey + "_" + traitName + "_" + methodName
			bareName := structKey + "_" + methodName

			concreteFn, ok := cg.curScope.lookup(qualifiedName)
			if !ok && baseQualifiedName != qualifiedName {
				concreteFn, ok = cg.curScope.lookup(baseQualifiedName)
			}

			if !ok && isDefaultBodied[methodName] {
				concreteFn, ok = cg.curScope.lookup(bareName)
			}

			if !ok {
				// Try overloaded variants of either qualified name (matching arity).
				wantArity := len(wrapSlot.Params) - 1 // subtract self (i8*)

				candidateNames := []string{qualifiedName, baseQualifiedName}
				if isDefaultBodied[methodName] {
					candidateNames = append(candidateNames, bareName)
				}

				for _, name := range candidateNames {
					if variants, hasOL := cg.overloads[name]; hasOL {
						for _, v := range variants {
							if v.arity == wantArity {
								if entry, ok2 := cg.curScope.lookup(v.irName); ok2 {
									concreteFn = entry
									ok = true

									break
								}
							}
						}
					}

					if ok {
						break
					}
				}
			}

			if !ok {
				return fmt.Errorf("trait vtable: struct %s does not implement %s.%s; expected fn %s::%s(this %s, ...)",
					structKey, traitName, methodName, traitName, methodName, structKey)
			}

			concreteFunc := concreteFn.val.(*ir.Func)

			// Build call args: first arg is either selfPtr (pointer receiver) or
			// selfVal (value receiver), depending on what the concrete method expects.
			var firstArg value.Value

			if len(concreteFunc.Sig.Params) > 0 {
				if pt, isPtr := concreteFunc.Sig.Params[0].(*irtypes.PointerType); isPtr && pt.ElemType.Equal(structSt) {
					firstArg = selfPtr // pointer receiver: mutations persist in-place
				} else {
					firstArg = selfVal // value receiver: pass by value
				}
			} else {
				firstArg = selfVal
			}

			callArgs := []value.Value{firstArg}
			for pi := 1; pi < len(wrapParams); pi++ {
				callArgs = append(callArgs, wrapParams[pi])
			}

			callArgs = cg.adaptArgs(entry, callArgs, concreteFunc.Sig)
			result := entry.NewCall(concreteFunc, callArgs...)

			if irtypes.IsVoid(wrapSlot.RetType) {
				entry.NewRet(nil)
			} else if retInstKey, isFatPtrRet := cg.isTraitFatPtr(wrapSlot.RetType); isFatPtrRet && result.Type().Equal(structSt) {
				// Trait method returns the trait type itself (e.g. fn mutate(this T) T).
				// The concrete method returned the updated struct; we already wrote it
				// back to selfPtr. Construct a fat-pointer using the original data ptr
				// (selfPtr / wrapParams[0]) and the statically-known vtable global so
				// the returned fat-pointer is valid beyond this wrapper's stack frame.
				fatPtrType := cg.traitFatPtrTypes[retInstKey]
				retVtableKey := structKey + "__" + retInstKey

				retVtableGlobal := cg.traitVtableGlobals[retVtableKey]
				if fatPtrType != nil && retVtableGlobal != nil {
					fpAlloca := entry.NewAlloca(fatPtrType)
					dpGep := entry.NewGetElementPtr(fatPtrType, fpAlloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
					entry.NewStore(wrapParams[0], dpGep) // i8* self
					vpGep := entry.NewGetElementPtr(fatPtrType, fpAlloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
					entry.NewStore(retVtableGlobal, vpGep)
					entry.NewRet(entry.NewLoad(fatPtrType, fpAlloca))
				} else {
					entry.NewRet(result)
				}
			} else {
				// Comma-ok adapter for ::index: user impl returns (V, bool),
				// trait slot returns just V. Extract field at the V slot
				// so the vtable shim still type-checks. Fat-ptr trait
				// dispatch through this slot intentionally drops the ok
				// bit; callers that want the bool must go through the
				// direct dispatch in genIndexExpr (which handles comma-ok
				// unwrap).
				//
				// Tin's tuple struct shape is `{ i32 type_tag, T1, T2 }`,
				// so the V/bool pair lives at fields 1 and 2.
				if st, ok := result.Type().(*irtypes.StructType); ok && len(st.Fields) == 3 {
					if it, isInt := st.Fields[2].(*irtypes.IntType); isInt && it.BitSize == 1 &&
						st.Fields[1].Equal(wrapSlot.RetType) {
						v := entry.NewExtractValue(result, 1)
						entry.NewRet(v)

						wrappers = append(wrappers, wrapFn)

						continue
					}
				}
				// Coerce return value to wrapper signature if needed (e.g. user
				// impl returns i32 but the trait alias signature says i64).
				ret := cg.coerce(entry, result, wrapSlot.RetType)
				entry.NewRet(ret)
			}

			wrappers = append(wrappers, wrapFn)
		}

		// Generate $coro wrapper functions for {#async} virtual methods.
		// These are appended to `wrappers` after all sync wrappers so the vtable
		// constant is built in the same order as the vtable struct fields.
		asyncNames := cg.traitAsyncMethodNames[traitName]
		for i, methodName := range asyncNames {
			coroSlotIdx := len(methodNames) + i
			coroSlot := vtableSt.Fields[coroSlotIdx].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			wrapperName := structKey + "__" + instKey + "__" + methodName + "$coro"
			wrapParams := make([]*ir.Param, len(coroSlot.Params))

			wrapParams[0] = ir.NewParam("self", irtypes.I8Ptr)
			for pi := 1; pi < len(coroSlot.Params); pi++ {
				wrapParams[pi] = ir.NewParam(fmt.Sprintf("a%d", pi), coroSlot.Params[pi])
			}

			wrapFn := cg.activeModule().NewFunc(wrapperName, irtypes.I8Ptr, wrapParams...)

			entry := wrapFn.NewBlock("entry")
			selfPtr := entry.NewBitCast(wrapParams[0], structPtrType)
			selfVal := entry.NewLoad(structSt, selfPtr)

			// Look up the concrete $coro method. Same canonicalisation as the
			// sync wrapper: bare trait name (module prefix stripped) for the
			// qualified key.
			qualifiedCoroName := structKey + "_" + bareKey + "_" + methodName + "$coro"
			baseQualifiedCoroName := structKey + "_" + traitName + "_" + methodName + "$coro"
			bareCoroName := structKey + "_" + methodName + "$coro"

			concreteCoro, ok2 := cg.curScope.lookup(qualifiedCoroName)
			if !ok2 && baseQualifiedCoroName != qualifiedCoroName {
				concreteCoro, ok2 = cg.curScope.lookup(baseQualifiedCoroName)
			}

			if !ok2 && isDefaultBodied[methodName] {
				concreteCoro, ok2 = cg.curScope.lookup(bareCoroName)
			}

			if !ok2 {
				return fmt.Errorf("trait vtable: struct %s does not implement async %s.%s; expected fn %s::%s(this %s, ...) {#async}",
					structKey, traitName, methodName, traitName, methodName, structKey)
			}

			concreteCoroFn := concreteCoro.val.(*ir.Func)

			callArgs := []value.Value{selfVal}
			for pi := 1; pi < len(wrapParams); pi++ {
				callArgs = append(callArgs, wrapParams[pi])
			}

			callArgs = cg.adaptArgs(entry, callArgs, concreteCoroFn.Sig)
			hdl := entry.NewCall(concreteCoroFn, callArgs...)
			entry.NewRet(hdl)

			wrappers = append(wrappers, wrapFn)
		}

		// Build vtable global constant and update the pre-declared global's initializer.
		vtableConst := constant.NewStruct(vtableSt, wrappers...)
		if preVtable, preDeclared := cg.traitVtableGlobals[vtableKey]; preDeclared {
			// The global was pre-declared before wrapper generation; fill in the init now.
			preVtable.Init = vtableConst
			preVtable.Immutable = true
		} else {
			vtableGlobal := cg.activeModule().NewGlobalDef(vtableKey+"_vtable_data", vtableConst)
			vtableGlobal.Immutable = true
			cg.traitVtableGlobals[vtableKey] = vtableGlobal
		}
	}

	return nil
}

// isTraitFatPtr reports whether t is a trait fat-pointer {i8*, vtable_struct*}.
func (cg *CodeGen) isTraitFatPtr(t irtypes.Type) (string, bool) {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return "", false
	}

	if st.Fields[0] != irtypes.I8Ptr {
		return "", false
	}

	pt, ok := st.Fields[1].(*irtypes.PointerType)
	if !ok {
		return "", false
	}

	vst, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return "", false
	}
	// Check it's a known trait vtable struct (returns instKey, not traitName).
	for instKey, vs := range cg.traitVtableStructTypes {
		if vs == vst {
			return instKey, true
		}
	}

	return "", false
}

// tryCoerceToIter detects whether iterVal implements iter[T] (either already a
// fat pointer or a concrete struct with an iter vtable) and returns the fat
// pointer and instKey if so. The fourth return is `ownsData`: true when this
// call materialized a fresh value-source heap allocation in the iface's data
// ptr (so the caller is responsible for releasing it), false when iterVal
// was already a fat pointer or a borrowed pointer source.
func (cg *CodeGen) tryCoerceToIter(block *ir.Block, iterVal value.Value) (value.Value, string, bool, bool) {
	// Case 1: already a trait fat pointer.
	if instKey, ok := cg.isTraitFatPtr(iterVal.Type()); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}

		if baseTrait == "iter" {
			return iterVal, instKey, true, false
		}

		return nil, "", false, false
	}

	// Case 2: concrete struct that has an iter[T] vtable registered.
	structName := cg.typeNameOf(iterVal.Type())
	if structName == "" {
		return nil, "", false, false
	}

	// coerceToTrait heap-allocates the iface data ptr unconditionally
	// (both value-source and pointer-source branches), so the caller
	// always owns the resulting ARC ref and must release it on loop exit.
	ownsData := true

	for vtableKey := range cg.traitVtableGlobals {
		// vtableKey format: "structName__instKey"
		prefix := structName + "__"
		if len(vtableKey) <= len(prefix) || vtableKey[:len(prefix)] != prefix {
			continue
		}

		instKey := vtableKey[len(prefix):]

		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}

		if baseTrait != "iter" {
			continue
		}
		// Coerce to iter fat pointer. Suppress the deferred scope-exit
		// release that coerceToTrait would otherwise register --
		// genForIterTrait emits its own release at the loop's exit
		// block, scoped tightly to the iteration rather than the
		// surrounding fn.
		prevSuppress := cg.suppressIfaceScopeRelease
		cg.suppressIfaceScopeRelease = true

		fatPtr, err := cg.coerceToTrait(block, iterVal, instKey)
		cg.suppressIfaceScopeRelease = prevSuppress

		if err != nil {
			continue
		}

		return fatPtr, instKey, true, ownsData
	}

	return nil, "", false, false
}

// genForIterTrait generates a for-in loop over a value that implements iter[T].
// It calls len() (vtable slot 0) for the count, and get(i) (vtable slot 1) for
// each element.
func (cg *CodeGen) genForIterTrait(block *ir.Block, s *ast.ForStmt, iterFatPtr value.Value, instKey string, ownsData bool) (*ir.Block, error) {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	// Look up method order: ["len", "get"]
	methodOrder := cg.traitMethodOrder[baseTrait]
	lenSlot, getSlot := -1, -1

	for i, name := range methodOrder {
		switch name {
		case "len":
			lenSlot = i
		case "get":
			getSlot = i
		}
	}

	if lenSlot < 0 || getSlot < 0 {
		return nil, fmt.Errorf("iter trait %s missing len/get methods", instKey)
	}

	vtableSt := cg.traitVtableStructTypes[instKey]

	// Determine element type from get's return type (vtable slot getSlot).
	getFnType := vtableSt.Fields[getSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	elemType := getFnType.RetType

	// Helper to load a function pointer from a vtable slot.
	loadSlot := func(b *ir.Block, vtablePtr value.Value, slot int) value.Value {
		slotFnType := vtableSt.Fields[slot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		gep := b.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slot)))

		return b.NewLoad(irtypes.NewPointer(slotFnType), gep)
	}

	// Extract components of fat pointer.
	dataPtr := block.NewExtractValue(iterFatPtr, 0)
	vtablePtr := block.NewExtractValue(iterFatPtr, 1)

	// The iterFatPtr was constructed by tryCoerceToIter -> coerceToTrait,
	// which heap-allocates the source struct via _tin_rc_alloc when the
	// iter value is by-value (not already a pointer). The for-loop owns
	// that ARC reference and must release it on exit so the storage is
	// reclaimed. Without this, every for-in over a value-typed iter
	// leaked the heap-allocated source.

	// Call len().
	lenFnType := vtableSt.Fields[lenSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	lenFnPtr := loadSlot(block, vtablePtr, lenSlot)
	totalLen := block.NewCall(lenFnPtr, cg.adaptArgs(block, []value.Value{dataPtr}, lenFnType)...)

	// Alloca for index.
	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	condBlock := cg.newBlock("iterfor.cond")
	bodyBlock := cg.newBlock("iterfor.body")
	afterBlock := cg.newBlock("iterfor.after")

	brToCond := block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenI64 := cg.coerce(condBlock, totalLen, irtypes.I64)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenI64)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// Body: call get(idx).
	cg.curScope = newScope(cg.curScope)

	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	getFnPtr := loadSlot(bodyBlock, vtablePtr, getSlot)
	getArgs := cg.adaptArgs(bodyBlock, []value.Value{dataPtr, bodyIdx}, getFnType)
	elemVal := bodyBlock.NewCall(getFnPtr, getArgs...)

	// Register loop variable.
	if s.VarName != "" {
		elemAlloca := bodyBlock.NewAlloca(elemType)
		bodyBlock.NewStore(elemVal, elemAlloca)
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true})
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	// Release the iter iface's data ptr at loop exit if we own it (i.e.,
	// tryCoerceToIter heap-allocated it from a value-source struct).
	// Pointer-source iterVals are borrowed and the original *T owner
	// handles cleanup -- releasing here would corrupt non-RC malloc'd
	// blocks (see malloc_dispatch.tin pattern).
	if ownsData {
		afterBlock.NewCall(cg.ensureRelease(), dataPtr)
	}

	return afterBlock, nil
}

// coerceToTrait constructs a trait fat pointer {i8* data, vtable*} from a
// concrete struct value or pointer, given the target instKey (e.g. "named" or "iter_i64").
// If structVal is already a *struct (e.g. from malloc), the heap pointer is
// used directly as the data pointer instead of allocating new stack space.
// traitPointerReceiverMethods returns the method names of trait `instKey`
// whose `this` receiver is a pointer (`*Self` or `*Trait[...]`).
// Returns nil for unknown traits or traits with all-value receivers.
//
// Used by coerceToTrait to reject value-form coercion when the trait
// has any pointer-receiver method -- that combination silently mutates
// a heap copy and the user almost certainly meant `let a *T = &b`.
func (cg *CodeGen) traitPointerReceiverMethods(instKey string) []string {
	bareTraitName := bareTraitNameFromKey(instKey)

	td, ok := cg.traits[bareTraitName]
	if !ok {
		return nil
	}

	var ptrMethods []string

	for _, m := range td.Methods {
		if len(m.Params) == 0 {
			continue
		}

		first := m.Params[0]
		if first.Name != "this" {
			continue
		}

		if _, isPtr := first.Type.(*ast.PointerType); isPtr {
			ptrMethods = append(ptrMethods, m.Name)
		}
	}

	return ptrMethods
}

// traitDisplayName renders an instKey ("Reader", "Awaitable__i64") as
// the user-visible source form ("Reader", "Awaitable[i64]"). Same
// shape as prettyStructName but kept separate because traits use a
// different mangling pipeline.
func (cg *CodeGen) traitDisplayName(instKey string) string {
	return prettyStructName(instKey)
}

// bareTraitNameFromKey strips the "__T1__T2..." instantiation suffix
// off an instKey, returning the bare trait name registered in
// cg.traits. For non-generic traits the key is already bare, so this
// is a no-op.
func bareTraitNameFromKey(instKey string) string {
	if i := strings.Index(instKey, "__"); i >= 0 {
		return instKey[:i]
	}

	return instKey
}

// buildPtrToTraitBorrow lowers `let a *Trait = &b` (or any other coerce
// where target is `*FatPtr` and source is `*Struct`). Builds a stack-
// temporary fat ptr `{cast(structPtr, i8*), vtable}` and returns its
// address -- a true borrow, mutations via *a propagate to *structPtr.
//
// Returns nil when the struct doesn't implement the trait or the fat-
// ptr type isn't registered; the caller falls through to the generic
// coerce path (which currently emits a wrong bitcast -- that's the
// fallback we're trying to replace).
//
// Lifetime: stack-temp lives for the enclosing function frame. Same
// gotcha as returning `*T` of any local -- Tin does not statically
// prevent it, but it doesn't catch fire on common in-frame uses
// (which is what the user's `let a *T = &b; (*a).foo(); echo b` test
// exercises).
func (cg *CodeGen) buildPtrToTraitBorrow(block *ir.Block, structPtr value.Value, traitName string, fatPtrType irtypes.Type) value.Value {
	pt, ok := structPtr.Type().(*irtypes.PointerType)
	if !ok {
		return nil
	}

	structName := cg.typeNameOf(pt.ElemType)
	if structName == "" {
		return nil
	}

	vtableKey := structName + "__" + traitName

	vtableGlobal, ok := cg.traitVtableGlobals[vtableKey]
	if !ok {
		return nil
	}

	fatPtrSt, ok := fatPtrType.(*irtypes.StructType)
	if !ok {
		return nil
	}

	// The fat ptr struct lives on the heap so a `*Trait` value can outlive
	// the frame that built it (e.g. returned, sent down a channel, captured
	// by an escaping closure). Stack-allocating here would dangle on every
	// such use; the few extra bytes per coerce are noise. data field points
	// at the source struct directly (no copy of the underlying value).
	sz := cg.llvmSizeOf(block, fatPtrSt)
	heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
	temp := block.NewBitCast(heapI8, irtypes.NewPointer(fatPtrSt))
	// Record that the current function returns a *Trait whose data is an
	// escape-promoted heap block. The flag rides up to callers via
	// fnReturnsOwningIface so their let-binding releases data on drop.
	// (We don't have the source AST here, so the most conservative
	// option is "every buildPtrToTraitBorrow caller in a fn with any
	// escaping var owns its iface data" -- which is the common case.)
	if cg.curFn != nil && len(cg.curFnEscapingVars) > 0 {
		cg.fnReturnsOwningIface[cg.curFn.Name()] = true
	}

	dataGEP := block.NewGetElementPtr(fatPtrSt, temp,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dataI8 := block.NewBitCast(structPtr, irtypes.I8Ptr)
	block.NewStore(dataI8, dataGEP)

	vtableGEP := block.NewGetElementPtr(fatPtrSt, temp,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(vtableGlobal, vtableGEP)

	return temp
}

func (cg *CodeGen) coerceToTrait(block *ir.Block, structVal value.Value, instKey string) (value.Value, error) {
	structType := structVal.Type()

	// Receiver/source mismatch: if the trait has any pointer-receiver
	// methods, target=value-trait coercion would silently mutate a heap
	// copy regardless of whether the source was `b` or `&b` -- Tin's
	// `Trait` always heap-copies. Reject both. The user must use
	// `let a *Trait = &b` to get a real borrow.
	//
	// Stash the diagnostic on cg.coerceLastErr so the caller
	// (genVarDecl, etc.) can surface it positioned at the user's
	// source line -- coerce() itself returns Value with no error
	// channel and is called from 87+ sites we don't want to touch.
	if missing := cg.traitPointerReceiverMethods(instKey); len(missing) > 0 {
		err := fmt.Errorf(
			"trait %s has pointer-receiver methods (%s); value-form coercion silently mutates a heap copy. "+
				"Use `let a *%s = &b` to mutate the original, or rewrite the trait's receivers to %s if a copy is intended",
			cg.traitDisplayName(instKey),
			strings.Join(missing, ", "),
			cg.traitDisplayName(instKey),
			cg.traitDisplayName(instKey))
		cg.coerceLastErr = err

		return nil, err
	}

	var (
		dataPtr      value.Value
		concreteType irtypes.Type
	)

	if pt, ok := structType.(*irtypes.PointerType); ok {
		// Pointer source (e.g. *T from `&T{...}` or mem::malloc + cast).
		// Copy the pointee into a fresh _tin_rc_alloc so the iface always
		// owns its own ARC-tracked storage. We can't safely retain the
		// source pointer here because we don't know whether it has an
		// RC header (`&T{...}` does, `mem::malloc(sizeof(T))` does not),
		// and _tin_retain on a non-RC pointer corrupts the previous
		// 16 bytes. Copying gives uniform iface release semantics across
		// every coercion source. Tradeoff: pointer-receiver methods
		// dispatched through the iface mutate the heap copy, not the
		// original *T -- code that relies on mutation visibility through
		// the iface must call methods directly on the *T (or use the
		// value-source `let var iface_T = struct_value` pattern, which
		// has the same heap-copy semantics).
		structSt := pt.ElemType
		szGEP := block.NewGetElementPtr(structSt,
			constant.NewNull(irtypes.NewPointer(structSt)),
			constant.NewInt(irtypes.I32, 1))
		szInt := block.NewPtrToInt(szGEP, irtypes.I64)
		heapPtr := block.NewCall(cg.ensureRCAlloc(), szInt)
		typedDst := block.NewBitCast(heapPtr, irtypes.NewPointer(structSt))
		srcVal := block.NewLoad(structSt, structVal)
		block.NewStore(srcVal, typedDst)

		dataPtr = heapPtr
		concreteType = structSt
	} else {
		// Heap-allocate the source struct so the iface's `this` pointer
		// survives across coroutine suspends. A stack alloca here would die
		// the moment the constructing coroutine suspends (the resume
		// function's stack frame is freed on suspend), and any spawned
		// fiber that captured the iface would later read freed memory --
		// which on AArch64 reliably corrupts (the next worker-stack frame
		// overwrites it), and on AMD64 happens to look intact under most
		// scheduling but isn't guaranteed.
		//
		// _tin_rc_alloc gives us an ARC header so existing release paths
		// reclaim the storage when the iface goes out of scope.
		szGEP := block.NewGetElementPtr(structType,
			constant.NewNull(irtypes.NewPointer(structType)),
			constant.NewInt(irtypes.I32, 1))
		szInt := block.NewPtrToInt(szGEP, irtypes.I64)
		heapPtr := block.NewCall(cg.ensureRCAlloc(), szInt)
		typedPtr := block.NewBitCast(heapPtr, irtypes.NewPointer(structType))
		block.NewStore(structVal, typedPtr)

		dataPtr = heapPtr
		concreteType = structType
	}

	// The vtable global is a compile-time constant that is always correct for
	// the (struct, trait) pair - including malloc'd structs whose embedded
	// vtable field has not yet been initialized.
	structName := cg.typeNameOf(concreteType)
	vtableKey := structName + "__" + instKey

	vtableGlobal, ok := cg.traitVtableGlobals[vtableKey]
	if !ok {
		return nil, fmt.Errorf("no vtable for %s implementing %s", structName, instKey)
	}

	fatPtrType, fpOk := cg.traitFatPtrTypes[instKey]
	if !fpOk {
		return nil, fmt.Errorf("no fat-ptr type for trait %s", instKey)
	}

	// Build fat pointer {i8* data, vtable*}.
	ifaceAlloca := block.NewAlloca(fatPtrType)
	dataGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, dataGep)
	vtableGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(vtableGlobal, vtableGep)

	// Defer the heap-block release to the enclosing scope's exit. We
	// can't release immediately after this call returns because spawn'd
	// fibers in the callee may still be using the data ptr at that
	// point; we can't release at iface-temporary-end because temporaries
	// have no precise scope. Tying the release to the calling scope
	// handles both: the scope's `await` must complete before the scope
	// exits, which guarantees every captured ptr has been read by then.
	//
	// Skip the synthetic entry when the iface is being constructed for
	// a let-binding (the let entry's ownsIfaceData flag handles release
	// directly), for a for-iter loop (genForIterTrait emits its own
	// release at loop exit), or for a trait init/deinit chain call
	// (genStructLit / emitReleaseInner emit their own tighter release).
	// Those callers pre-mark cg.suppressIfaceScopeRelease.
	if cg.curScope != nil && !cg.suppressIfaceScopeRelease {
		ptrSlot := block.NewAlloca(irtypes.I8Ptr)
		block.NewStore(dataPtr, ptrSlot)

		name := fmt.Sprintf(".iface_data_%d", cg.strCount)
		cg.strCount++
		cg.curScope.set(name, &scopeEntry{
			val: ptrSlot, isAlloc: true, releaseRawPtr: true,
		})
	}

	return block.NewLoad(fatPtrType, ifaceAlloca), nil
}

func (cg *CodeGen) genEnumDecl(n *ast.EnumDecl) error {
	// Idempotent: skip if already registered (can be called from preregister AND pass 3).
	if _, alreadyDone := cg.enumTypes[n.Name]; alreadyDone {
		return nil
	}
	// Determine base LLVM type.
	var baseType irtypes.Type = irtypes.I32

	if n.BaseType != nil {
		bt, err := cg.tinTypeToLLVM(n.BaseType)
		if err != nil {
			return err
		}

		baseType = bt
	}

	cg.enumTypes[n.Name] = baseType

	// Register member values.
	var nextVal int64 = 0

	for _, m := range n.Members {
		var val int64

		if m.Value != nil {
			// Evaluate constant expression.
			if il, ok := m.Value.(*ast.IntLit); ok {
				val = il.Value
			} else {
				val = nextVal
			}
		} else {
			val = nextVal
		}

		key := n.Name + "." + m.Name
		cg.enumValues[key] = val
		nextVal = val + 1
	}

	return nil
}

// genTaggedUnionTypeDecl generates the LLVM layout for a tagged union declared
// via "type u = i8 | string". Layout: { i32 type_id, i8 tag, [maxSize x i8] payload }.
// type_id is a compile-time constant (same pool as structs/data) for any boxing and typeof.
// Tag 0 = first variant, 1 = second, etc.
func (cg *CodeGen) genTaggedUnionTypeDecl(name string, ut *ast.UnionTypeExpr) error {
	var maxSize uint64 = 1

	for _, te := range ut.Types {
		lt, err := cg.tinTypeToLLVM(te)
		if err != nil {
			return err
		}

		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}

	payloadType := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypes[name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(name)
		cg.structTypes[name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}
	// Assign a compile-time type ID (same pool as structs/data types).
	typeID := cg.nextTypeID
	cg.nextTypeID++
	cg.unionTypeIDs[name] = typeID
	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I8, payloadType}
	cg.unionTypeMembers[name] = ut.Types

	return nil
}

// genUnionDecl generates the LLVM layout for a native C-style union declared
// via "union u = as_i8 i8 | as_string string". Layout: { [maxSize x i8] storage }.
// No tag - members overlap the same memory region.
func (cg *CodeGen) genUnionDecl(n *ast.UnionDecl) error {
	var maxSize uint64 = 1

	for _, m := range n.Members {
		lt, err := cg.tinTypeToLLVM(m.Type)
		if err != nil {
			return err
		}

		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}

	storageType := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypes[n.Name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	st.Fields = []irtypes.Type{storageType}
	cg.nativeUnionDecls[n.Name] = n

	return nil
}

// wrapTaggedUnionVariant wraps a value into a tagged union struct.
// Layout: { i32 type_id, i8 tag, [N x i8] payload }. Returns nil if no variant matches.
func (cg *CodeGen) wrapTaggedUnionVariant(block *ir.Block, val value.Value, targetSt *irtypes.StructType, unionName string) value.Value {
	members := cg.unionTypeMembers[unionName]
	tag := int8(-1)
	// First pass: exact type match.
	for i, te := range members {
		lt, err := cg.tinTypeToLLVM(te)
		if err != nil {
			continue
		}

		if lt.Equal(val.Type()) {
			tag = int8(i)

			break
		}
	}
	// Second pass: same size (for int widening), but not float vs int.
	if tag < 0 {
		for i, te := range members {
			lt, err := cg.tinTypeToLLVM(te)
			if err != nil {
				continue
			}

			if irtypes.IsFloat(lt) != irtypes.IsFloat(val.Type()) {
				continue // never conflate float and int variants of same size
			}

			if llvmTypeSize(lt) == llvmTypeSize(val.Type()) {
				tag = int8(i)

				break
			}
		}
	}

	if tag < 0 {
		return nil
	}

	alloca := block.NewAlloca(targetSt)
	// Field 0 = i32 type_id.
	typeIDVal := int32(0)
	if id, ok := cg.unionTypeIDs[unionName]; ok {
		typeIDVal = id
	}

	typeIDGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(typeIDVal)), typeIDGEP)
	// Field 1 = i8 tag.
	tagGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I8, int64(tag)), tagGEP)
	// Field 2 = [N x i8] payload via bitcast.
	payloadGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(val.Type()))
	block.NewStore(val, payloadPtr)

	return block.NewLoad(targetSt, alloca)
}

// wrapNativeUnion stores a value into a native union's storage via bitcast.
// Layout: { [N x i8] storage }.
// The stored value is coerced to the storage size: if val is larger than the
// array, it is truncated to the array's byte length; if smaller, stored as-is.
func (cg *CodeGen) wrapNativeUnion(block *ir.Block, val value.Value, targetSt *irtypes.StructType) value.Value {
	alloca := block.NewAlloca(targetSt)
	storageGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	// Determine storage capacity (array element count = bytes).
	storageBytes := uint64(0)
	if arr, ok := targetSt.Fields[0].(*irtypes.ArrayType); ok {
		storageBytes = arr.Len
	}

	storedVal := val
	// If the value is wider than the storage, truncate to the storage size.
	if storageBytes > 0 {
		valBytes := llvmTypeSize(val.Type())
		if valBytes > storageBytes {
			var storeType irtypes.Type

			switch storageBytes {
			case 1:
				storeType = irtypes.I8
			case 2:
				storeType = irtypes.I16
			case 4:
				storeType = irtypes.I32
			default:
				storeType = irtypes.I64
			}

			if irtypes.IsInt(val.Type()) && irtypes.IsInt(storeType) {
				storedVal = block.NewTrunc(val, storeType)
			}
		}
	}

	valPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(storedVal.Type()))
	block.NewStore(storedVal, valPtr)

	return block.NewLoad(targetSt, alloca)
}

// lookupTemplateFile resolves the source-file path that originally
// declared a generic struct template. Empty string when not found.
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

	pretty := prettyStructName(structName)

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
