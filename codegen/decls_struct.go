package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genStructDecl(n *ast.StructDecl) error {
	if err := checkConstraintsReferenceDeclared(n.Name, n.TypeParams, n.Wildcards, n.Constraints); err != nil {
		return cg.nodeErr(n, "%s", err)
	}

	if err := checkMethodsAgainstImpls(n.Name, "struct", n.Implements, n.Methods); err != nil {
		return cg.nodeErr(n, "%s", err)
	}

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
				cg.diagStructName(structKey), f.Name, cg.diagStructName(name),
				cg.diagStructName(structKey), cg.diagStructName(name))
		}
	}

	st := cg.structTypeFor(CanonKey(structKey))
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(structKey)
		cg.recordLLVM(CanonKey(structKey), st)
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
		if cg.traitFor(CanonKey(traitName)) == nil {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		if cg.traitFor(CanonKey(traitName)) == nil {
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
			td := cg.traitFor(CanonKey(traitName))
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

		vtableSt := cg.vtableFor(CanonKey(instKey))
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
		cg.recordLLVM(CanonKey(structKey+".native"), nativeSt)

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
		if cg.traitFor(CanonKey(traitName)) == nil {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		td := cg.traitFor(CanonKey(traitName))
		if td == nil {
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
		qualName := methodScopeName(structKey, m)

		// Mirror the FuncDecl under the plain key independently of
		// the scope alias check below, so callers that look up by
		// `Type_method` (e.g. genCallExpr's call-site-generics hook)
		// find the impl's metadata - including RetTypeHasWildcard -
		// even if the scope alias was already established by an
		// earlier registerPlainMethodAliases pass.
		if decl, ok2 := cg.funcDecls[qualName]; ok2 {
			if _, present := cg.funcDecls[plainName]; !present {
				cg.funcDecls[plainName] = decl
			}
		}

		if _, exists := cg.curScope.lookup(plainName); exists {
			continue
		}

		if entry, ok := cg.curScope.lookup(qualName); ok {
			cg.curScope.set(plainName, entry)

			plainMethodNames[m.Name] = true
		}
	}
}

// rebindAdtMethodsInScope re-registers per-instantiation ADT method
// scope entries in the current scope. Mirrors registerPlainMethodAliases
// but for the no-trait-qualifier methods that get scope-registered
// directly under structKey + "_" + methodName at predeclare time.
// Used when a generic ADT instantiation is re-encountered after its
// original monomorphization scope has been torn down (tin-test
// wrapper boundaries, cross-package, etc.), so a later
// `r.method()` call site still finds the method.
//
// Looks up the IR function via cg.allFuncs() because cg.funcDecls
// alone only gives us the AST decl, and method dispatch needs the
// ir.Func value.
func (cg *CodeGen) rebindAdtMethodsInScope(structKey string, methods []*ast.FuncDecl) {
	for _, m := range methods {
		if m.TraitQualifier != "" {
			continue
		}

		if m.IsExtern != "" {
			continue
		}

		if len(m.TypeParams) > 0 {
			continue
		}

		key := structKey + "_" + m.Name
		if _, exists := cg.curScope.lookup(key); exists {
			continue
		}

		for _, f := range cg.allFuncs() {
			if f.Name() == key {
				cg.curScope.set(key, &scopeEntry{val: f})

				break
			}
		}
	}
}

// checkAllTraitImplsComplete walks every struct declaration and verifies each
// listed trait's virtual methods have a matching qualified impl. Default-bodied
// methods stay optional. Generic struct templates and `implicit` (special trait)
// are skipped - templates are only checked on monomorphization, and implicit
