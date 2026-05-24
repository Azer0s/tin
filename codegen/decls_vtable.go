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
	case *ast.WildcardType:
		// Wildcard slots are existentials resolved by the impl-matcher.
		// Encode the slot's name (or an anonymous placeholder) so two
		// distinct wildcards in the same bound don't collide.
		if t.Name != "" {
			return "_W_" + t.Name
		}

		return "_W"
	case *ast.ArrayType:
		// Array shapes appear in monomorphized trait bounds when the
		// caller filled a slot with `[byte]` or similar. The same key
		// scheme as elsewhere: dynamic arrays use `[]elem`,
		// fixed-size arrays use `[elem; n]`. Mirror what
		// typeExprCanonicalKey emits for ADT monomorphization names.
		if t.Size < 0 {
			return "[]" + traitImplKey(t.Elem)
		}

		return fmt.Sprintf("[%s;%d]", traitImplKey(t.Elem), t.Size)
	case *ast.PointerType:
		if t.IsConst {
			return "const_ptr_" + traitImplKey(t.Elem)
		}

		return "ptr_" + traitImplKey(t.Elem)
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
	case *ast.WildcardType:
		if t.Name != "" {
			return "_W_" + t.Name
		}

		return "_W"
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
	if fp := cg.ifaceFor(CanonKey(instKey)); fp != nil {
		return fp, nil
	}

	td := cg.traitFor(CanonKey(traitName))
	if td == nil {
		return nil, fmt.Errorf("unknown trait: %s", traitName)
	}

	// Pre-register stub types in the cache before resolving method types.
	// This breaks potential infinite recursion when a trait method returns
	// its own trait type (e.g. fn inc(this counter) counter = ...).
	vtableStub := irtypes.NewStruct()
	vtableStub.SetName(instKey + "_vtable")
	fatPtrStub := irtypes.NewStruct(irtypes.I8Ptr, irtypes.NewPointer(vtableStub))
	fatPtrStub.SetName(instKey + "_iface")
	cg.traitInstKeys[instKey] = traitName
	cg.recordTraitIface(CanonKey(instKey), fatPtrStub, vtableStub)

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

	// Append a per-vtable data-release fn pointer at the END of the
	// vtable.  Layout: [method_0, ..., method_n, data_release_fn].
	// Method indices are unchanged (they stay in [0, n)); existing
	// method-dispatch GEPs work as-is.  ensureStructPtrReleaseFn for
	// the iface fat-ptr loads index n (the last field) and calls it
	// on the data pointer to run the concrete struct's release_ptr,
	// so RC-tracked fields of the wrapped struct are released too --
	// raw _tin_release alone would only free the outer block.
	dataReleaseFnType := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr))
	fnPtrTypes = append(fnPtrTypes, dataReleaseFnType)

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

			selfLLVM := cg.structTypeFor(CanonKey(structKey))

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
		if cg.traitFor(CanonKey(traitName)) == nil {
			if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
				traitName = traitName[idx+2:]
			}
		}

		td := cg.traitFor(CanonKey(traitName))
		if td == nil {
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

		vtableSt := cg.vtableFor(CanonKey(instKey))
		methodNames := cg.traitMethodOrder[traitName]

		structSt := cg.structTypeFor(CanonKey(structKey))
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
					cg.diagStructName(structKey), cg.traitDisplayName(traitName), methodName,
					cg.traitDisplayName(traitName), methodName, cg.diagStructName(structKey))
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
				fatPtrType := cg.ifaceFor(CanonKey(retInstKey))
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
					cg.diagStructName(structKey), cg.traitDisplayName(traitName), methodName,
					cg.traitDisplayName(traitName), methodName, cg.diagStructName(structKey))
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

		// Append the concrete struct's release_ptr fn to the vtable
		// (as the LAST slot -- method indices are unchanged).  Uses a
		// data-release thunk that takes i8* (the iface's data field)
		// and casts to the concrete struct pointer before invoking
		// the standard struct release_ptr.  This lets the iface's
		// own release_ptr in ensureStructPtrReleaseFn dispatch via
		// the vtable to release RC-tracked fields of the wrapped
		// struct (otherwise a raw _tin_release would only free the
		// outer block, leaking string / fat-array fields).
		dataReleaseThunk := cg.ensureTraitDataReleaseThunk(structKey, structSt)
		dataReleaseFnType := vtableSt.Fields[len(vtableSt.Fields)-1].(*irtypes.PointerType)
		thunkConst := constant.NewBitCast(dataReleaseThunk, dataReleaseFnType)
		wrappers = append(wrappers, thunkConst)

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

// sourceBindingHoldsFreshRCPtr reports whether `allocaPtr` is the
// alloca of a scope entry that was initialized from a fresh
// `_tin_rc_alloc`'d pointer (`&StructLit{...}`, `&Variant(...)`,
// field-access ownership transfers).  Used by `coerceToTrait`'s
// pointer-source path so a `return s` from such a binding picks the
// OWNING vtable rather than the borrow vtable -- the caller's
// release needs to free the heap block, which only the owning
// vtable's data-release slot does.  Without this check, the LLVM
// Load that materializes the binding's stored pointer hides the RC
// provenance from `pointerProvenanceIsRCAlloc`, and every iface
// returned by such a function leaks its data on every release.
func (cg *CodeGen) sourceBindingHoldsFreshRCPtr(allocaPtr value.Value) bool {
	if cg.curScope == nil {
		return false
	}

	for s := cg.curScope; s != nil; s = s.parent {
		var found bool

		s.each(func(_ string, entry *scopeEntry) {
			if entry == nil || !entry.isAlloc {
				return
			}

			if entry.val == allocaPtr && (entry.holdsFreshRCPtr || entry.isHeapOwned) {
				found = true
			}
		})

		if found {
			return true
		}
	}

	return false
}

// sourceBindingIsEarlyHeap reports whether `allocaPtr` is the alloca
// of a scope entry that's been early-heap-promoted (so its scope-exit
// release is suppressed).  Used by buildPtrToTraitBorrow to decide
// whether to emit a balancing retain on the iface's data field.
func (cg *CodeGen) sourceBindingIsEarlyHeap(allocaPtr value.Value) bool {
	if cg.curScope == nil {
		return false
	}

	for s := cg.curScope; s != nil; s = s.parent {
		var found bool

		s.each(func(_ string, entry *scopeEntry) {
			if entry == nil || !entry.isAlloc {
				return
			}

			if entry.val == allocaPtr && entry.isEarlyHeap {
				found = true
			}
		})

		if found {
			return true
		}
	}

	return false
}

// sourceBindingPointsToBorrowedStorage reports whether `allocaPtr` is the
// alloca for a binding whose value is the address of a stack/global cell
// (i.e. a borrowed pointer).  Used by buildPtrToTraitBorrow's indirect
// path to route `let q = &b; let a *Trait = q` to the borrow vtable
// instead of treating q as an owning heap pointer.
func (cg *CodeGen) sourceBindingPointsToBorrowedStorage(allocaPtr value.Value) bool {
	if cg.curScope == nil {
		return false
	}

	for s := cg.curScope; s != nil; s = s.parent {
		var found bool

		s.each(func(_ string, entry *scopeEntry) {
			if entry == nil || !entry.isAlloc {
				return
			}

			if entry.val == allocaPtr && entry.pointsToBorrowedStorage {
				found = true
			}
		})

		if found {
			return true
		}
	}

	return false
}

// ensureTraitDataReleaseThunk returns (and caches) a tiny `void(i8*)`
// thunk that bitcasts the data pointer to *<structKey> and invokes the
// per-struct release_ptr.  Stored in each (struct, trait) vtable's
// last slot; consumed by the iface's release_ptr to teardown the
// wrapped concrete struct's RC-tracked fields when the iface RC hits 0.
func (cg *CodeGen) ensureTraitDataReleaseThunk(structKey string, structSt *irtypes.StructType) *ir.Func {
	if fn, ok := cg.traitDataReleaseThunks[structKey]; ok {
		return fn
	}

	fnName := structKey + "__trait_data_release"
	fn := cg.activeModule().NewFunc(fnName, irtypes.Void, ir.NewParam("data", irtypes.I8Ptr))
	// weak_odr matches ensureElemRetainHelper / ensureElemReleaseHelper:
	// the symbol is shared across pkg modules (any pkg that widens a
	// `*<struct>` to `*Trait` references it via its vtable's data-release
	// slot).  Default external-linkage would either link-error on
	// duplicate emission across modules, or silently undefined-symbol
	// when an incremental rebuild materializes the helper in a
	// different pkg's `.o` than the consumer's cached `.o` references.
	fn.Linkage = enum.LinkageWeakODR
	cg.traitDataReleaseThunks[structKey] = fn

	entry := fn.NewBlock("entry")
	structPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(structSt))

	// ADTs have their own release function (ensureDataPtrReleaseFn) that
	// dispatches per-variant; the struct release path would treat the ADT
	// as a flat struct and skip variant-specific payload teardown,
	// leaking RC-tracked fields nested inside variants. Detect ADTs by
	// presence in cg.dataVariants and route accordingly.
	var relFn *ir.Func
	if _, isADT := cg.dataVariants[structKey]; isADT {
		relFn = cg.ensureDataPtrReleaseFn(structKey, structSt)
	} else {
		relFn = cg.ensureStructPtrReleaseFn(structKey, structSt)
	}

	if relFn != nil {
		entry.NewCall(relFn, structPtr)
	}

	entry.NewRet(nil)

	return fn
}

// ensureTraitBorrowVtable returns (and caches) a parallel "borrow" vtable
// for (structKey, instKey).  Identical to the regular vtable except the
// last slot (the data-release thunk) is replaced with a no-op.  Used by
// buildPtrToTraitBorrow when the source is a stack alloca so that the
// iface's release path frees only the iface block itself and leaves the
// borrowed stack memory alone -- avoiding an uninit read of the
// pseudo-RC-header at (stack_ptr - sizeof(TinRCHdr)).
//
// The retain-balance reasoning that made the regular vtable's data
// release correct for heap sources no longer applies: borrows hold no
// reference, so neither retain nor data release fires.  Returns nil
// when the regular vtable hasn't been registered yet (caller falls
// back to the owning path, which will fail at link time -- the only
// way to hit that is a bug in the registration order).
func (cg *CodeGen) ensureTraitBorrowVtable(vtableKey string) *ir.Global {
	if g, ok := cg.traitBorrowVtableGlobals[vtableKey]; ok {
		return g
	}

	owning, ok := cg.traitVtableGlobals[vtableKey]
	if !ok || owning.Init == nil {
		return nil
	}

	vtableSt, ok := owning.ContentType.(*irtypes.StructType)
	if !ok || len(vtableSt.Fields) == 0 {
		return nil
	}

	noop := cg.ensureTraitBorrowNoopRelease()
	if noop == nil {
		return nil
	}

	// Reuse every method slot from the owning vtable; swap the trailing
	// data-release slot with the no-op.
	origInit, ok := owning.Init.(*constant.Struct)
	if !ok || len(origInit.Fields) != len(vtableSt.Fields) {
		return nil
	}

	newFields := make([]constant.Constant, len(origInit.Fields))
	copy(newFields, origInit.Fields)

	dataReleaseSlot := len(newFields) - 1

	slotType, slotOk := vtableSt.Fields[dataReleaseSlot].(*irtypes.PointerType)
	if !slotOk {
		return nil
	}

	newFields[dataReleaseSlot] = constant.NewBitCast(noop, slotType)

	borrowConst := constant.NewStruct(vtableSt, newFields...)
	borrowGlobal := cg.activeModule().NewGlobalDef(vtableKey+"_borrow_vtable_data", borrowConst)
	borrowGlobal.Immutable = true
	cg.traitBorrowVtableGlobals[vtableKey] = borrowGlobal

	return borrowGlobal
}

// ensureTraitBorrowNoopRelease returns the single per-module `void(i8*)`
// no-op installed in the data-release slot of every borrow vtable.
// weak_odr so the same symbol unifies across pkg objects.
func (cg *CodeGen) ensureTraitBorrowNoopRelease() *ir.Func {
	const name = "_tin_iface_borrow_noop_release"
	if entry, ok := cg.curScope.lookup(name); ok {
		if fn, ok2 := entry.val.(*ir.Func); ok2 {
			return fn
		}
	}

	for _, fn := range cg.mod.Funcs {
		if fn.Name() == name {
			return fn
		}
	}

	fn := cg.activeModule().NewFunc(name, irtypes.Void, ir.NewParam("data", irtypes.I8Ptr))
	fn.Linkage = enum.LinkageWeakODR

	entry := fn.NewBlock("entry")
	entry.NewRet(nil)

	return fn
}

// isTraitFatPtrPtrType reports whether t is a POINTER to a trait
// fat-pointer struct (i.e. `*Trait_iface` in source).  Borrow-form
// trait coercion (`fn f(a *Trait); f(structPtr)`) lowers to this
// shape: buildPtrToTraitBorrow heap-allocates the fat-pointer block
// and returns its address as `*Trait_iface`.  Used by
// emitCallArgReleaseForRet to know when a coerced call arg owns a
// freshly allocated iface block that must be released after the
// call returns.
func (cg *CodeGen) isTraitFatPtrPtrType(t irtypes.Type) bool {
	pt, ok := t.(*irtypes.PointerType)
	if !ok {
		return false
	}

	_, isIface := cg.isTraitFatPtr(pt.ElemType)

	return isIface
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
	for canon, r := range cg.types {
		if r.Vtable == vst {
			return string(canon), true
		}
	}

	return "", false
}

// tryCoerceToIter detects whether iterVal implements iter[T] (either already a
// fat pointer or a concrete struct with an iter vtable) and returns the fat
// pointer and instKey if so. The fourth return is `ownsData`: true when this
// call materialized a fresh value-source heap allocation in the iface's data
// ptr (so the caller is responsible for releasing it), false when iterVal
