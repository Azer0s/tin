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

// traitBaseName returns the bare name of a trait TypeExpr (ignoring type params).
func traitBaseName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		return t.Name
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
	}

	for _, impl := range n.Implements {
		name := traitBaseName(impl)
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
				if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
					injected.Params = append([]ast.Param{
						{Name: "this", Type: &ast.SimpleType{Name: n.Name}},
					}, injected.Params...)
				} else {
					// Fix this param to use this struct's type.
					newParams := make([]ast.Param, len(injected.Params))
					copy(newParams, injected.Params)
					newParams[0].Type = &ast.SimpleType{Name: n.Name}
					injected.Params = newParams
				}
				aug.Methods = append(aug.Methods, &injected)
			}
		}
	}

	return aug
}

func (cg *CodeGen) genStructDecl(n *ast.StructDecl) error {
	if len(n.TypeParams) > 0 {
		return nil // generic template - only compiled when monomorphized
	}
	orig := n // keep original for Implements list
	n = cg.augmentStructFromTraits(n)
	n.Implements = orig.Implements // preserve for vtable generation
	st, ok := cg.structTypes[n.Name]
	if !ok {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// Prepend vtable pointer fields for each non-implicit implemented trait
	// buildTraitFatPtrTypeInst is idempotent; calling it here ensures the vtable
	// struct type is registered before we reference its pointer type.
	var vtableInstKeys []string
	var vtableFieldTypes []irtypes.Type
	for _, impl := range orig.Implements {
		traitName := traitBaseName(impl)
		if traitName == "implicit" {
			continue
		}
		if _, ok := cg.traits[traitName]; !ok {
			continue
		}
		instKey := traitImplKey(impl)
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
	cg.structVtableOrder[n.Name] = vtableInstKeys

	// Build user field types
	var userFieldTypes []irtypes.Type
	var fieldNames []string
	for _, f := range n.Fields {
		ft, err := cg.tinTypeToLLVM(f.Type)
		if err != nil {
			return err
		}
		userFieldTypes = append(userFieldTypes, ft)
		fieldNames = append(fieldNames, f.Name)
	}
	// Assign a compile-time type ID for this struct (used by any boxing /
	// runtime type checks).  IDs are stable within a compilation unit.
	if _, exists := cg.structTypeIDs[n.Name]; !exists {
		cg.structTypeIDs[n.Name] = cg.nextTypeID
		cg.nextTypeID++
	}
	// Final layout: [i32 type_id, vtable_0*, vtable_1*, ..., user_field_0, ...]
	// The leading i32 is always field 0; a *struct can be bitcast to *any
	// and the type read directly from field 0.
	st.Fields = append([]irtypes.Type{irtypes.I32}, append(vtableFieldTypes, userFieldTypes...)...)
	cg.structFields[n.Name] = fieldNames // user-visible names only
	cg.structFieldLLVMTypes[n.Name] = userFieldTypes

	// Record which traits this struct implements (for typeof/traitof).
	var implNames []string
	for _, impl := range n.Implements {
		implNames = append(implNames, impl.String())
	}
	cg.structImpls[n.Name] = implNames

	// Generate methods as top-level functions with struct-qualified names.
	for _, m := range n.Methods {
		if err := cg.genStructMethod(n.Name, m); err != nil {
			return err
		}
	}

	// For qualified methods (e.g. fn iter[char]::idx), also register them
	// under the plain name (e.g. struct_idx) when no other method with that
	// plain name already exists. This lets non-disambiguated call sites work.
	plainMethodNames := map[string]bool{}
	for _, m := range n.Methods {
		if m.TraitQualifier == "" {
			plainMethodNames[m.Name] = true
		}
	}
	for _, m := range n.Methods {
		if m.TraitQualifier == "" {
			continue
		}
		if plainMethodNames[m.Name] {
			continue // a plain method already covers this name
		}
		plainName := n.Name + "_" + m.Name
		if _, exists := cg.curScope.lookup(plainName); !exists {
			qualName := methodScopeName(n.Name, m)
			if entry, ok := cg.curScope.lookup(qualName); ok {
				cg.curScope.set(plainName, entry)
				plainMethodNames[m.Name] = true // mark so only first qualifier wins
			}
		}
	}

	// Generate vtable wrappers and global constants for each implemented trait.
	if err := cg.genTraitVtables(n); err != nil {
		return err
	}
	return nil
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
		// rename the self parameter from the generic struct name to concrete
		if st, ok := newType.(*ast.SimpleType); ok && st.Name == genericName {
			newType = &ast.SimpleType{Name: concreteName}
		}
		newParams[i] = ast.Param{Name: p.Name, Type: newType, IsConst: p.IsConst, IsVarArgs: p.IsVarArgs}
	}
	newRet := substituteTypeInTypeExpr(m.RetType, subst)
	newBody := substituteStructNameInBody(m.Body, genericName, concreteName)
	return &ast.FuncDecl{
		Name:     m.Name,
		Params:   newParams,
		RetType:  newRet,
		Body:     newBody,
		Tags:     m.Tags,
		IsStatic: m.IsStatic,
	}
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
		if typeName == genericName {
			typeName = concreteName
		}
		return &ast.StructLit{TypeName: typeName, Fields: newFields, Positional: n.Positional}
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
func (cg *CodeGen) genTypeDecl(n *ast.TypeDecl) error {
	// Tagged union alias: "type u = i8 | string"
	if ut, ok := n.Type.(*ast.UnionTypeExpr); ok {
		return cg.genTaggedUnionTypeDecl(n.Name, ut)
	}
	gt, ok := n.Type.(*ast.GenericType)
	if !ok {
		// Simple alias - already registered in preregister. Nothing to do.
		return nil
	}

	tmpl, isTmpl := cg.genericStructs[gt.Name]
	if !isTmpl {
		// GenericType refers to something other than a generic struct
		// (e.g. a generic trait instantiation used as a type alias).
		cg.typeAliases[n.Name] = n.Type
		return nil
	}

	// Build type-parameter substitution: tmpl.TypeParams[i] -> gt.TypeParams[i]
	subst := make(map[string]ast.TypeExpr)
	for i, paramName := range tmpl.TypeParams {
		if i < len(gt.TypeParams) {
			subst[paramName] = gt.TypeParams[i]
		}
	}

	// Build the concrete struct by substituting type params in every field.
	concrete := &ast.StructDecl{
		Name:       n.Name,
		Implements: tmpl.Implements,
	}
	for _, f := range tmpl.Fields {
		concrete.Fields = append(concrete.Fields, ast.StructField{
			Name:      f.Name,
			Type:      substituteTypeInTypeExpr(f.Type, subst),
			Tags:      f.Tags,
			IsForward: f.IsForward,
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
		} else {
			concrete.Methods = append(concrete.Methods, substituteMethod(m, tmpl.Name, n.Name, subst))
		}
	}
	// Any overrides that don't shadow a template method are appended.
	for _, ov := range n.Overrides {
		if _, already := overrideSet[ov.Name]; !already {
			continue // already applied above
		}
		concrete.Methods = append(concrete.Methods, ov)
	}

	// Register the concrete struct type (opaque first, just like preregister).
	if _, exists := cg.structTypes[n.Name]; !exists {
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	return cg.genStructDecl(concrete)
}

// buildTraitFatPtrType computes (and caches) the LLVM fat-pointer type for a
// trait: { i8*, vtable_struct* }.  The vtable struct has one fn-ptr slot per
// trait method, each with signature (i8* self, ...) -> ret.
// traitImplKey returns a unique string key for a trait impl TypeExpr.
// For "named" -> "named"; for "iter[i64]" -> "iter_i64".
func traitImplKey(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		key := t.Name
		for _, tp := range t.TypeParams {
			key += "_" + traitImplKey(tp)
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

	var methodNames []string
	var fnPtrTypes []irtypes.Type

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
	}

	vtableSt := irtypes.NewStruct(fnPtrTypes...)
	vtableSt.SetName(instKey + "_vtable")
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, vtableSt)

	fatPtr := irtypes.NewStruct(irtypes.I8Ptr, irtypes.NewPointer(vtableSt))
	fatPtr.SetName(instKey + "_iface")
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, fatPtr)

	cg.traitVtableStructTypes[instKey] = vtableSt
	cg.traitFatPtrTypes[instKey] = fatPtr
	cg.traitMethodOrder[traitName] = methodNames // shared across instantiations
	cg.traitInstKeys[instKey] = traitName
	return fatPtr, nil
}

// genTraitVtables generates, for each trait that structName implements:
//  1. One wrapper function per trait method: structName__instKey__methodName(i8* self, ...)
//  2. One vtable global constant referencing those wrappers.
func (cg *CodeGen) genTraitVtables(n *ast.StructDecl) error {
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
							if fnEntry, ok2 := cg.curScope.lookup(methodScopeName(n.Name, m)); ok2 {
								if fn, ok3 := fnEntry.val.(*ir.Func); ok3 {
									cg.implicitConvFns[n.Name] = append(
										cg.implicitConvFns[n.Name],
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

		td, ok := cg.traits[traitName]
		if !ok {
			continue
		}
		instKey := traitImplKey(impl)
		vtableKey := n.Name + "__" + instKey
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

		structSt := cg.structTypes[n.Name]
		if structSt == nil {
			continue
		}
		structPtrType := irtypes.NewPointer(structSt)

		// Generate one wrapper per trait method.
		var wrappers []constant.Constant
		for i, methodName := range methodNames {
			wrapSlot := vtableSt.Fields[i].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			wrapperName := n.Name + "__" + instKey + "__" + methodName
			wrapParams := make([]*ir.Param, len(wrapSlot.Params))
			wrapParams[0] = ir.NewParam("self", irtypes.I8Ptr)
			for pi := 1; pi < len(wrapSlot.Params); pi++ {
				wrapParams[pi] = ir.NewParam(fmt.Sprintf("a%d", pi), wrapSlot.Params[pi])
			}
			wrapFn := cg.mod.NewFunc(wrapperName, wrapSlot.RetType, wrapParams...)

			entry := wrapFn.NewBlock("entry")
			// Cast i8* self -> structType*, load struct value.
			selfPtr := entry.NewBitCast(wrapParams[0], structPtrType)
			selfVal := entry.NewLoad(structSt, selfPtr)

			// Look up concrete method.
			// First try the trait-qualified name "Struct_traitKey_method",
			// then fall back to the plain "Struct_method".
			qualifiedName := n.Name + "_" + traitQualifierKey(instKey) + "_" + methodName
			concreteName := n.Name + "_" + methodName
			concreteFn, ok := cg.curScope.lookup(qualifiedName)
			if ok {
				concreteName = qualifiedName
			} else {
				concreteFn, ok = cg.curScope.lookup(concreteName)
			}
			if !ok {
				return fmt.Errorf("trait vtable: missing concrete method %s (also tried %s)", concreteName, qualifiedName)
			}
			concreteFunc := concreteFn.val.(*ir.Func)

			// Build call args: selfVal + extra params.
			callArgs := []value.Value{selfVal}
			for pi := 1; pi < len(wrapParams); pi++ {
				callArgs = append(callArgs, wrapParams[pi])
			}
			callArgs = cg.adaptArgs(entry, callArgs, concreteFunc.Sig)
			result := entry.NewCall(concreteFunc, callArgs...)
			if irtypes.IsVoid(result.Type()) {
				entry.NewRet(nil)
			} else {
				entry.NewRet(result)
			}
			wrappers = append(wrappers, wrapFn)
		}

		// Build vtable global constant.
		vtableConst := constant.NewStruct(vtableSt, wrappers...)
		vtableGlobal := cg.mod.NewGlobalDef(vtableKey+"_vtable_data", vtableConst)
		vtableGlobal.Immutable = true
		cg.traitVtableGlobals[vtableKey] = vtableGlobal
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
// pointer and instKey if so.
func (cg *CodeGen) tryCoerceToIter(block *ir.Block, iterVal value.Value) (value.Value, string, bool) {
	// Case 1: already a trait fat pointer.
	if instKey, ok := cg.isTraitFatPtr(iterVal.Type()); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}
		if baseTrait == "iter" {
			return iterVal, instKey, true
		}
		return nil, "", false
	}

	// Case 2: concrete struct that has an iter[T] vtable registered.
	structName := cg.typeNameOf(iterVal.Type())
	if structName == "" {
		return nil, "", false
	}
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
		// Coerce to iter fat pointer.
		fatPtr, err := cg.coerceToTrait(block, iterVal, instKey)
		if err != nil {
			continue
		}
		return fatPtr, instKey, true
	}
	return nil, "", false
}

// genForIterTrait generates a for-in loop over a value that implements iter[T].
// It calls len() (vtable slot 0) for the count, and get(i) (vtable slot 1) for
// each element.
func (cg *CodeGen) genForIterTrait(block *ir.Block, s *ast.ForStmt, iterFatPtr value.Value, instKey string) (*ir.Block, error) {
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

	block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenI64 := cg.coerce(condBlock, totalLen, irtypes.I64)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenI64)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

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
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.curScope = cg.curScope.parent
	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

// coerceToTrait constructs a trait fat pointer {i8* data, vtable*} from a
// concrete struct value or pointer, given the target instKey (e.g. "named" or "iter_i64").
// If structVal is already a *struct (e.g. from malloc), the heap pointer is
// used directly as the data pointer instead of allocating new stack space.
func (cg *CodeGen) coerceToTrait(block *ir.Block, structVal value.Value, instKey string) (value.Value, error) {
	structType := structVal.Type()

	var dataPtr value.Value
	var concreteType irtypes.Type

	if pt, ok := structType.(*irtypes.PointerType); ok {
		// Already a pointer (e.g. from malloc + bitcast). Use it directly.
		dataPtr = block.NewBitCast(structVal, irtypes.I8Ptr)
		concreteType = pt.ElemType
	} else {
		// Value type: alloca to get a stable pointer.
		alloca := block.NewAlloca(structType)
		block.NewStore(structVal, alloca)
		dataPtr = block.NewBitCast(alloca, irtypes.I8Ptr)
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

// genDataDecl registers a non-generic data type as a tagged union.
// Layout: { i32 type_id, i8 variant_tag, [payload_bytes x i8] payload }
// Field 0: i32  - compile-time type ID (same registry as structs, for any boxing)
// Field 1: i8   - variant discriminant: None=0, typed variants 1, 2, ...
// Field 2: payload bytes (size = max variant payload)
// Having type_id as field 0 means *data can be bitcast to *any.
func (cg *CodeGen) genDataDecl(n *ast.DataDecl) error {
	if len(n.TypeParams) > 0 {
		// Generic - stored as template; instantiated lazily in tinTypeToLLVM.
		cg.genericDataDecls[n.Name] = n
		return nil
	}
	// Compute max payload size across all typed (non-None) variants.
	var maxSize uint64
	for _, v := range n.Variants {
		if v.Type == nil {
			continue // None
		}
		lt, err := cg.tinTypeToLLVM(v.Type)
		if err != nil {
			return err
		}
		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}
	if maxSize == 0 {
		maxSize = 1
	}
	// Build { i32, i8, [maxSize x i8] }
	payloadType := irtypes.NewArray(maxSize, irtypes.I8)
	// Reuse the opaque placeholder registered in preregister (if any), so any
	// pointers already referencing it remain valid after we fill in the fields.
	st := cg.structTypes[n.Name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}
	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I8, payloadType}
	// Assign a compile-time type ID.
	if _, exists := cg.dataTypeIDs[n.Name]; !exists {
		cg.dataTypeIDs[n.Name] = cg.nextTypeID
		cg.nextTypeID++
	}
	cg.dataDecls[n.Name] = n
	// Assign tag constants.
	tagIdx := int8(0)
	noneTagSet := false
	for i, v := range n.Variants {
		if v.Name == "None" || v.Type == nil && v.Name == "None" {
			key := fmt.Sprintf("%s.%d", n.Name, i)
			cg.dataVariantTags[key] = 0
			cg.enumValues[n.Name+".None"] = 0
			noneTagSet = true
		}
	}
	if !noneTagSet {
		tagIdx = 1
	} else {
		tagIdx = 1
	}
	for i, v := range n.Variants {
		if v.Name == "None" {
			continue
		}
		key := fmt.Sprintf("%s.%d", n.Name, i)
		cg.dataVariantTags[key] = tagIdx
		tagIdx++
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

// instantiateDataType creates a concrete tagged-union struct for a generic
// data type instantiated with specific type parameters.
func (cg *CodeGen) instantiateDataType(dd *ast.DataDecl, typeArgs []ast.TypeExpr) (irtypes.Type, error) {
	// Build substitution map: type param name -> concrete TypeExpr.
	subst := map[string]ast.TypeExpr{}
	for i, tp := range dd.TypeParams {
		if i < len(typeArgs) {
			subst[tp] = typeArgs[i]
		}
	}
	// Compute max payload size.
	var maxSize uint64
	for _, v := range dd.Variants {
		if v.Type == nil {
			continue
		}
		concreteType := substituteTypeExpr(v.Type, subst)
		lt, err := cg.tinTypeToLLVM(concreteType)
		if err != nil {
			return nil, err
		}
		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}
	if maxSize == 0 {
		maxSize = 1
	}
	payloadType := irtypes.NewArray(maxSize, irtypes.I8)
	// Layout: { i32 type_id, i8 variant_tag, [maxSize x i8] payload }
	st := irtypes.NewStruct(irtypes.I32, irtypes.I8, payloadType)
	// Build instance key (e.g. "maybe__string").
	var keyParts []string
	for _, ta := range typeArgs {
		keyParts = append(keyParts, typeExprName(ta))
	}
	instName := dd.Name + "__" + strings.Join(keyParts, "__")
	// Reuse existing named struct if already instantiated.
	if existing := cg.structTypes[instName]; existing != nil {
		return existing, nil
	}
	st.SetName(instName)
	cg.structTypes[instName] = st
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	// Store a concrete DataDecl (with substituted variants) for is-checking.
	concretDecl := &ast.DataDecl{Name: instName, Variants: make([]ast.DataVariant, len(dd.Variants))}
	for i, v := range dd.Variants {
		if v.Type == nil {
			concretDecl.Variants[i] = v
		} else {
			concretDecl.Variants[i] = ast.DataVariant{Name: v.Name, Type: substituteTypeExpr(v.Type, subst)}
		}
	}
	cg.dataDecls[instName] = concretDecl
	// Also register under the original name if not already there.
	if _, exists := cg.dataDecls[dd.Name]; !exists {
		cg.dataDecls[dd.Name] = concretDecl
	}
	// Assign compile-time type IDs for the instantiated type.
	if _, exists := cg.dataTypeIDs[instName]; !exists {
		cg.dataTypeIDs[instName] = cg.nextTypeID
		cg.nextTypeID++
	}
	if _, exists := cg.dataTypeIDs[dd.Name]; !exists {
		cg.dataTypeIDs[dd.Name] = cg.dataTypeIDs[instName]
	}
	// Populate dataVariantTags for the instantiated type (mirrors genDataDecl logic).
	tagIdx := int8(1)
	for i, v := range concretDecl.Variants {
		key := fmt.Sprintf("%s.%d", instName, i)
		if v.Name == "None" || v.Type == nil {
			cg.dataVariantTags[key] = 0
		} else {
			cg.dataVariantTags[key] = tagIdx
			tagIdx++
		}
	}
	return st, nil
}

// substituteTypeExpr replaces simple type names according to subst map.
func substituteTypeExpr(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil {
		return nil
	}
	switch t := te.(type) {
	case *ast.SimpleType:
		if replacement, ok := subst[t.Name]; ok {
			return replacement
		}
	}
	return te
}

// wrapDataVariant wraps a value into a data type tagged union.
// Returns nil if wrapping is not possible (type mismatch).
func (cg *CodeGen) wrapDataVariant(block *ir.Block, val value.Value, targetSt *irtypes.StructType, dataName string) value.Value {
	dd, ok := cg.dataDecls[dataName]
	if !ok {
		return nil
	}
	// Find the typed variant whose LLVM type matches the value's type.
	for i, v := range dd.Variants {
		if v.Type == nil {
			continue // None
		}
		lt, err := cg.tinTypeToLLVM(v.Type)
		if err != nil {
			continue
		}
		if !lt.Equal(val.Type()) {
			// Try coercion check: same structure
			if llvmTypeSize(lt) != llvmTypeSize(val.Type()) {
				continue
			}
		}
		// Found matching variant.
		key := fmt.Sprintf("%s.%d", dataName, i)
		tag := cg.dataVariantTags[key]
		alloca := block.NewAlloca(targetSt)
		// Store type_id at field 0.
		if typeID, ok := cg.dataTypeIDs[dataName]; ok {
			typeIDGEP := block.NewGetElementPtr(targetSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGEP)
		}
		// Store variant tag at field 1.
		tagGEP := block.NewGetElementPtr(targetSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I8, int64(tag)), tagGEP)
		// Store payload at field 2: bitcast the payload field pointer to the value's type.
		payloadGEP := block.NewGetElementPtr(targetSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(val.Type()))
		block.NewStore(val, payloadPtr)
		return block.NewLoad(targetSt, alloca)
	}
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
