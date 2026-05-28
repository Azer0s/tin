package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genCallFieldAccess handles the FieldAccess branch of genCallExpr (instance
// methods, trait calls, static dispatch, field-fn-pointer fallback).
// Always returns; never falls through to assign callee/calleeType.
func (cg *CodeGen) genCallFieldAccess(block *ir.Block, e *ast.CallExpr, fn *ast.FieldAccess) (value.Value, error) {
	var (
		callee     value.Value
		calleeType *irtypes.FuncType
	)

	// Static dispatch: TypeName.method() where TypeName is a struct type, not a variable.
	// Must be checked BEFORE trying to evaluate fn.Expr as a value, because type names
	// are not in scope as values and would cause "undefined identifier" errors.
	if staticName, typeArgStr := cg.tryResolveStructTypeName(fn.Expr); staticName != "" {
		methodKey := staticName + "_" + fn.Field
		baseStaticName := staticName // preserved for error messages before typeArgStr overwrites staticName
		// Also try the concrete monomorphized key when a type arg is present.
		if typeArgStr != "" {
			// Each part can itself be a nested generic, a pointer/array,
			// a qualified name, or a type alias. Parse via the same
			// type-key string parser the canonical machinery uses, then
			// run typeExprCanonicalKey to resolve aliases (so e.g.
			// `type Ptr = *i64; Atomic[Ptr].new(...)` instantiates as
			// `Atomic__*i64`, the same struct `Atomic[*i64].new(...)`
			// would). splitTopLevelTypeArgs respects bracket depth so
			// inner commas (`HashMap[K, List[i64]]`) don't split wrong.
			typeArgTEs := splitTopLevelTypeArgs(typeArgStr)
			resolvedParts := make([]string, len(typeArgTEs))

			for i, te := range typeArgTEs {
				resolvedParts[i] = cg.typeExprCanonicalKey(te)
			}

			concreteName := staticName + "__" + strings.Join(resolvedParts, "__")
			if cg.structTypeFor(CanonKey(concreteName)) == nil {
				if _, isGeneric := cg.genericStructsByArity[staticName]; isGeneric {
					synthDecl := &ast.TypeDecl{
						Name: concreteName,
						Type: &ast.GenericType{Name: staticName, TypeParams: typeArgTEs},
					}
					if mErr := cg.genTypeDecl(synthDecl); mErr != nil {
						return nil, cg.nodeErr(e, "instantiating %s: %v", cg.diagStructName(concreteName), mErr)
					}
				}
			}

			if cg.structTypeFor(CanonKey(concreteName)) != nil {
				methodKey = concreteName + "_" + fn.Field
				staticName = concreteName
			}
		}

		// Overloaded static method: evaluate args first, resolve best variant.
		if variants, hasOverloads := cg.overloads[methodKey]; hasOverloads {
			llArgs := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}

				llArgs = append(llArgs, av)

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}

			best := cg.resolveOverload(variants, llArgs)
			if best == nil {
				typeName := cg.diagStructName(baseStaticName)
				if typeArgStr != "" {
					typeName = cg.diagStructName(baseStaticName) + "[" + strings.ReplaceAll(typeArgStr, ",", ", ") + "]"
				}

				return nil, cg.nodeErr(e, "no matching overload for %s::%s (got %d arg(s))", typeName, fn.Field, len(llArgs))
			}

			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, cg.nodeErr(e, "overload %s not found in scope", best.irName)
			}

			var ovCallee value.Value

			if oEntry.isAlloc {
				ptrType := oEntry.val.Type().(*irtypes.PointerType)
				ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
			} else {
				ovCallee = oEntry.val
			}

			preCoerceVals := append([]value.Value(nil), llArgs...)
			if f2, ok2 := ovCallee.(*ir.Func); ok2 {
				llArgs = cg.adaptArgs(block, llArgs, f2.Sig)
			}

			result := block.NewCall(ovCallee, llArgs...)

			for i, astArg := range e.Args {
				if i >= len(preCoerceVals) || i >= len(llArgs) {
					break
				}

				cg.emitCallArgReleaseForRet(block, astArg, preCoerceVals[i], llArgs[i], result.Type())
			}

			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}

		if entry, ok := cg.curScope.lookup(methodKey); ok {
			if f, isFn := entry.val.(*ir.Func); isFn && cg.isStaticMethodIR(f, staticName) {
				llArgs := make([]value.Value, 0, len(e.Args))
				for _, arg := range e.Args {
					av, err2 := cg.genExpr(block, arg)
					if err2 != nil {
						return nil, err2
					}

					llArgs = append(llArgs, av)

					if cg.curBlock != nil && cg.curBlock != block {
						block = cg.curBlock
					}
				}

				preCoerceVals := append([]value.Value(nil), llArgs...)
				llArgs = cg.adaptArgs(block, llArgs, f.Sig)

				result := block.NewCall(cg.resolveColoredFn(f), llArgs...)

				for i, astArg := range e.Args {
					if i >= len(preCoerceVals) || i >= len(llArgs) {
						break
					}

					cg.emitCallArgReleaseForRet(block, astArg, preCoerceVals[i], llArgs[i], result.Type())
				}

				if irtypes.IsVoid(result.Type()) {
					return nil, nil
				}

				return result, nil
			}
		}
	}

	// Method call: obj.method(args...) or ptr->method(args...)
	objVal, err := cg.genExpr(block, fn.Expr)
	if err != nil {
		return nil, err
	}
	// genExpr on a CallExpr receiver may have split the block
	// (e.g. a heavy/recursive call inside a $coro or $colored body
	// emits a pre-call yield via genCallSiteYieldFor and advances
	// cg.curBlock).  Refresh `block` so subsequent emits land
	// downstream of the split instead of in the original block,
	// which is now terminated.  Without this refresh, the IR
	// verifier reports "missing terminator" on the yield's
	// `afterBlk` (the call landed there but nothing else did) or
	// "instruction does not dominate all uses" (later loads
	// referenced %objVal but were emitted into the original block,
	// not the block where %objVal was actually computed).
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	// -> operator: dereference the pointer-to-struct to get the struct value.
	if fn.IsPtr {
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			objVal = block.NewLoad(pt.ElemType, objVal)
		}
	}

	// Trait fat-pointer dispatch: if obj is {i8*, vtable*}, use vtable.
	if traitName, ok := cg.isTraitFatPtr(objVal.Type()); ok {
		result, err := cg.callTraitMethod(block, objVal, traitName, fn.Field, e.Args)
		if err != nil {
			return nil, cg.nodeErr(e, "%v", err)
		}

		return result, nil
	}
	// Auto-deref: *TraitFatPtr -> load the fat pointer and dispatch through vtable.
	if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
		if traitName, ok2 := cg.isTraitFatPtr(pt.ElemType); ok2 {
			loaded := block.NewLoad(pt.ElemType, objVal)

			result, err := cg.callTraitMethod(block, loaded, traitName, fn.Field, e.Args)
			if err != nil {
				return nil, cg.nodeErr(e, "%v", err)
			}
			// objVal owns rc=1 on the iface block when it was
			// freshly allocated by the caller (fn returning
			// *Trait, &Struct{...} expr literal, [*Trait]
			// indexing returning the element).  Loads from a
			// binding's alloca denote borrows; the binding's
			// own scope-exit release handles the iface, so we
			// must NOT release here.  Mirrors
			// emitCallArgReleaseForRet's *Trait_iface path.
			if _, isLoad := objVal.(*ir.InstLoad); !isLoad && !isCopyExpr(fn.Expr) {
				cg.emitRelease(block, objVal)
			}

			return result, nil
		}
	}

	// Concrete struct method: resolve as StructName_method.
	// When obj is a pointer-to-struct (*T), use the pointee's name for method
	// lookup but keep objVal as the pointer (the thisArg logic below handles it).
	objLookupType := objVal.Type()
	if pt, ok := objLookupType.(*irtypes.PointerType); ok {
		if cg.typeNameOf(pt.ElemType) != "" {
			objLookupType = pt.ElemType
		}
	}

	structName := cg.typeNameOf(objLookupType)
	methodName := structName + "_" + fn.Field

	// Overloaded method: evaluate args first to pick the best variant.
	if variants, hasOverloads := cg.overloads[methodName]; hasOverloads {
		argVals := make([]value.Value, 0, len(e.Args))
		for _, arg := range e.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			argVals = append(argVals, av)

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}
		}

		best := cg.resolveOverload(variants, argVals)
		if best == nil {
			return nil, cg.nodeErr(e, "no matching overload for %s.%s (got %d arg(s))", cg.diagStructName(structName), fn.Field, len(argVals))
		}

		oEntry, oOk := cg.curScope.lookup(best.irName)
		if !oOk {
			return nil, cg.nodeErr(e, "overload %s not found in scope", best.irName)
		}

		var ovCallee value.Value

		if oEntry.isAlloc {
			ptrType := oEntry.val.Type().(*irtypes.PointerType)
			ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
		} else {
			ovCallee = oEntry.val
		}
		// Static method called on an instance: don't pass the instance as receiver.
		ovIsStatic := false
		if f, ok2 := ovCallee.(*ir.Func); ok2 {
			ovIsStatic = cg.isStaticMethodIR(f, structName)
		}

		var llArgs []value.Value
		if ovIsStatic {
			llArgs = make([]value.Value, 0, len(argVals))
			llArgs = append(llArgs, argVals...)
		} else {
			// Build thisArg (pointer receiver if needed).
			thisArg := objVal

			if f, ok2 := ovCallee.(*ir.Func); ok2 && len(f.Sig.Params) > 0 {
				firstParam := f.Sig.Params[0]
				if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr {
					if pt.ElemType.Equal(objVal.Type()) {
						if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
							thisArg = lv
						} else {
							tmp := block.NewAlloca(objVal.Type())
							block.NewStore(objVal, tmp)
							thisArg = tmp
						}
					}
				}
			}

			llArgs = make([]value.Value, 0, len(argVals)+1)
			llArgs = append(llArgs, thisArg)
			llArgs = append(llArgs, argVals...)
		}

		if f, ok2 := ovCallee.(*ir.Func); ok2 {
			llArgs = cg.adaptArgs(block, llArgs, f.Sig)
		}

		result := block.NewCall(ovCallee, llArgs...)
		// ARC: release temporary RC-tracked args (same logic as genCallExpr bottom).
		thisOff := 1
		if ovIsStatic {
			thisOff = 0
		}

		for i, astArg := range e.Args {
			if i >= len(argVals) || i+thisOff >= len(llArgs) {
				break
			}

			cg.emitCallArgReleaseForRet(block, astArg, argVals[i], llArgs[i+thisOff], result.Type())
		}

		// ARC: release temporary struct receiver (method chain temporaries).
		if !fn.IsPtr && isTemporaryProducer(fn.Expr) {
			cg.emitRelease(block, objVal)
		}

		if irtypes.IsVoid(result.Type()) {
			return nil, nil
		}

		return result, nil
	}

	entry, ok := cg.curScope.lookup(methodName)
	if !ok {
		// Check for a generic method template (e.g. map_opt[r] on option__i64).
		if tmpls, isGenericMethod := cg.genericMethodTemplates[methodName]; isGenericMethod && len(tmpls) > 0 {
			tmpl := tmpls[0]
			_ = tmpls
			// Pre-scan args for bare generic-method-refs (no explicit T),
			// e.g. `r.map(V::get)`.  These can't be eagerly evaluated --
			// V::get has no scope binding -- so defer them until typeSubst
			// is computed, then materialize each with the T inferred from
			// the corresponding param's FuncType return slot.  Without
			// this dance, ToString-style polymorphic helpers used as
			// .map / .filter args force the user to type each call site's
			// T explicitly even when the surrounding context could supply
			// it via returnTypeHint.
			callArgs := make([]value.Value, len(e.Args))
			lazyMethodRefs := make(map[int][]*ast.FuncDecl)

			for i, arg := range e.Args {
				if sa, ok := arg.(*ast.ScopeAccess); ok && len(sa.Path) >= 2 {
					mn := sa.Path[len(sa.Path)-1]
					tn := sa.Path[0]

					if len(sa.Path) > 2 {
						tn = sa.Path[len(sa.Path)-2]
					}

					if mtmpls, isM := cg.genericMethodTemplates[tn+"_"+mn]; isM && len(mtmpls) > 0 {
						lazyMethodRefs[i] = mtmpls

						continue
					}
				}

				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}

				callArgs[i] = av

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
			// Build arg list for type inference: this + call args.  Lazy
			// args are skipped via nil slots; inferTypeArgs's nil guard
			// keeps them from participating in argument-shape inference
			// but the return-type-hint pass still binds the corresponding
			// type-param when the caller supplied one.
			inferArgs := make([]value.Value, 0, len(callArgs)+1)
			inferArgs = append(inferArgs, objVal)
			inferArgs = append(inferArgs, callArgs...)
			typeSubst := cg.inferTypeArgs(tmpl, inferArgs)
			// Method-level overload-by-where-clause: when the same
			// `Type_method` has multiple generic templates differing only
			// in their `where T is X` constraint shape, pick the entry
			// whose constraints are satisfied by the inferred subst.
			// This lets a `data Value` expose:
			//   fn get[T] where T is i64           = ...
			//   fn get[T] where T is Option[string] = ...
			//   ...
			// and `let n i64 = v.get()` /  `.map(Value::get)` route to
			// the matching body.  The first overload's `tmpl` is used to
			// infer the subst (their type-param names + signatures are
			// uniform across overloads for the user-intended pattern); we
			// then re-pick the body to compile.
			if len(tmpls) > 1 {
				if picked := cg.pickGenericMethodOverload(tmpls, typeSubst); picked != nil {
					tmpl = picked
				}
			}
			// Now that typeSubst is known, materialize each lazy
			// method-ref using the param's substituted FuncType: the
			// fn's return type after substitution is the inferred T for
			// the method-ref.  E.g. `r.map(V::get)` -- .map's f param is
			// `fn(t) u`; after t=V and u=i64 are bound (the latter via
			// the outer let-binding's return-type hint), the inferred T
			// for V::get is `u`'s substitution, i64.
			for i, mtmpls := range lazyMethodRefs {
				paramIdx := i + 1 // +1 for `this`
				if paramIdx >= len(tmpl.Params) {
					return nil, cg.nodeErr(e, "generic method-ref arg #%d has no matching parameter", i)
				}

				paramFnType, ok := tmpl.Params[paramIdx].Type.(*ast.FuncType)
				if !ok {
					return nil, cg.nodeErr(e, "generic method-ref arg #%d: expected fn-type parameter", i)
				}
				// The fn's return type may itself reference a type param of
				// the outer call (e.g. .map[u]'s `fn(t) u` -- the return is
				// `u`).  Resolve through typeSubst so we get the concrete
				// type the outer caller wants.
				retTE := paramFnType.RetType

				var retCanon string

				if simple, ok := retTE.(*ast.SimpleType); ok {
					if name, found := typeSubst[simple.Name]; found {
						retCanon = name.Canon
					} else {
						retCanon = simple.Name
					}
				} else {
					retCanon = cg.typeExprCanonicalKey(retTE)
				}

				if retCanon == "" {
					return nil, cg.nodeErr(e, "generic method-ref arg #%d: cannot determine T from .%s's signature", i, methodName)
				}

				fakeIndex := &ast.Identifier{Name: retCanon}
				mtmplKey := ""

				if sa, ok := e.Args[i].(*ast.ScopeAccess); ok && len(sa.Path) >= 2 {
					mn := sa.Path[len(sa.Path)-1]
					tn := sa.Path[0]

					if len(sa.Path) > 2 {
						tn = sa.Path[len(sa.Path)-2]
					}

					mtmplKey = tn + "_" + mn
				}

				if mtmplKey == "" {
					return nil, cg.nodeErr(e, "generic method-ref arg #%d: missing template key", i)
				}
				// Pick the overload whose where-clauses match the
				// resolved T -- build a transient subst with the lazy
				// method's lone type param bound to the inferred ret.
				mtmpl := mtmpls[0]

				if len(mtmpls) > 1 && len(mtmpl.TypeParams) >= 1 {
					mSubst := map[string]TypeName{
						mtmpl.TypeParams[0]: cg.typeNameFromCanon(retCanon),
					}

					if picked := cg.pickGenericMethodOverload(mtmpls, mSubst); picked != nil {
						mtmpl = picked
					}
				}

				fn, err2 := cg.materializeGenericMethodRef(mtmpl, mtmplKey, fakeIndex)
				if err2 != nil {
					return nil, cg.nodeErr(e, "generic method-ref arg #%d: %v", i, err2)
				}

				callArgs[i] = fn
			}
			instKey := ""

			for i, tp := range tmpl.TypeParams {
				if i > 0 {
					instKey += "__"
				}

				if name, found := typeSubst[tp]; found {
					instKey += name.Canon
				} else {
					instKey += tp
				}
			}
			// Monomorphize: use the full method scope name as the function name so
			// the IR name becomes "structName_methodName__instKey".
			tmplCopy := *tmpl
			tmplCopy.Name = methodName

			concreteFunc, err2 := cg.monomorphizeFunc(&tmplCopy, instKey, typeSubst)
			if err2 != nil {
				return nil, err2
			}
			// Build LLVM call args: this + call args, adapted to signature.
			thisArg := objVal

			if len(concreteFunc.Sig.Params) > 0 {
				if pt, isPtr := concreteFunc.Sig.Params[0].(*irtypes.PointerType); isPtr {
					if pt.ElemType.Equal(objVal.Type()) {
						if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
							thisArg = lv
						} else {
							tmp := block.NewAlloca(objVal.Type())
							block.NewStore(objVal, tmp)
							thisArg = tmp
						}
					}
				}
			}

			llArgs := make([]value.Value, 0, len(callArgs)+1)
			llArgs = append(llArgs, thisArg)
			llArgs = append(llArgs, callArgs...)
			llArgs = cg.adaptArgs(block, llArgs, concreteFunc.Sig)
			result := block.NewCall(cg.resolveColoredFn(concreteFunc), llArgs...)
			// ARC: release temporary RC-tracked call arguments (index 1+ in llArgs; 0 is this).
			for i, astArg := range e.Args {
				if i >= len(callArgs) || i+1 >= len(llArgs) {
					break
				}

				cg.emitCallArgReleaseForRet(block, astArg, callArgs[i], llArgs[i+1], result.Type())
			}

			// ARC: release temporary struct receiver (method chain temporaries).
			if !fn.IsPtr && isTemporaryProducer(fn.Expr) {
				cg.emitRelease(block, objVal)
			}

			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}
		// Also check without prefix.
		entry, ok = cg.curScope.lookup(fn.Field)
	}

	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			callee = block.NewLoad(ptrType.ElemType, entry.val)
		} else {
			callee = entry.val
		}
		// Static method called on an instance: skip the instance receiver.
		instIsStatic := false
		if f, ok2 := callee.(*ir.Func); ok2 {
			instIsStatic = cg.isStaticMethodIR(f, structName)
		}

		var llArgs []value.Value

		llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
		if instIsStatic {
			llArgs = make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err := cg.genExpr(block, arg)
				if err != nil {
					return nil, err
				}

				llArgs = append(llArgs, av)
				llArgsPreCoerce = append(llArgsPreCoerce, av)

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
		} else {
			// Determine the first argument: if the method expects a pointer
			// receiver (*Struct), pass the address of the object rather than
			// its value so that mutations through `this` are visible to the caller.
			thisArg := objVal

			if f, ok2 := callee.(*ir.Func); ok2 && len(f.Sig.Params) > 0 {
				firstParam := f.Sig.Params[0]
				if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr {
					if pt.ElemType.Equal(objVal.Type()) {
						// Try to get the lvalue (alloca) for the receiver expression.
						if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
							thisArg = lv
						} else {
							// Fallback: store to a temp alloca (mutations are lost,
							// but this keeps the call type-correct).
							tmp := block.NewAlloca(objVal.Type())
							block.NewStore(objVal, tmp)
							thisArg = tmp
						}
					}
				}
			}

			llArgs = make([]value.Value, 0, len(e.Args)+1)
			llArgs = append(llArgs, thisArg)

			for _, arg := range e.Args {
				av, err := cg.genExpr(block, arg)
				if err != nil {
					return nil, err
				}

				llArgs = append(llArgs, av)
				llArgsPreCoerce = append(llArgsPreCoerce, av)

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
		}
		// Call-site generics: when the called method's return type
		// originally contained a wildcard slot and a return-type hint
		// differs from the callee's actual return type, route through
		// the per-target monomorphization so the call's result is
		// directly the target shape (no caller-side rewrap).
		if f, ok2 := callee.(*ir.Func); ok2 {
			if decl := cg.funcDecls[methodName]; decl != nil && decl.RetTypeHasWildcard {
				if cg.returnTypeHint == nil {
					return nil, cg.nodeErr(e,
						"%s.%s has a wildcard slot in its return type that needs context to fill (a let-binding type annotation, the enclosing function's return type, or an argument type expectation). Annotate the receiving binding (e.g. `let x %s = ...`) or call the method through `try` inside a function whose return type fixes the slot.",
						cg.diagStructName(structName), strings.TrimPrefix(methodName, structName+"_"),
						cg.diagStructName(structName))
				}

				if !cg.returnTypeHint.Equal(f.Sig.RetType) {
					bareMethod := strings.TrimPrefix(methodName, structName+"_")
					if monoFn, ok3 := cg.ensureWildcardMono(structName, bareMethod, objVal.Type(), cg.returnTypeHint); ok3 {
						callee = monoFn
					}
				}
			}
		}

		// Adapt arg types to function signature.  The `this` receiver
		// sits at llArgs[0] when instIsStatic is false; the user-
		// authored e.Args correspond to llArgs[1..].  Autocopy fires
		// on the receiver (when the method mutates `this`) and per
		// user arg.
		if f, ok2 := callee.(*ir.Func); ok2 {
			calleeType = f.Sig

			argSliceStart := 1
			if instIsStatic {
				argSliceStart = 0
			}

			if !instIsStatic && len(llArgs) > 0 {
				llArgs[0] = cg.maybeAutoCopyReceiverVal(block, fn.Expr, llArgs[0])
			}

			if argSliceStart < len(llArgs) {
				userArgs := llArgs[argSliceStart:]
				cg.applyAutoCopyToArgVals(block, e.Args, userArgs)
			}

			llArgs = cg.adaptArgs(block, llArgs, calleeType)
		}

		// Auto-yield before calling a heavy or recursive method.
		block = cg.genCallSiteYieldFor(block, methodName)
		// Route to $colored when emitting in cooperative context.
		if f, ok := callee.(*ir.Func); ok {
			callee = cg.resolveColoredFn(f)
		}

		result := block.NewCall(callee, llArgs...)
		// ARC: release temporary RC-tracked args.
		thisOff := 1
		if instIsStatic {
			thisOff = 0
		}

		for i, astArg := range e.Args {
			if i >= len(llArgsPreCoerce) || i+thisOff >= len(llArgs) {
				break
			}

			cg.emitCallArgReleaseForRet(block, astArg, llArgsPreCoerce[i], llArgs[i+thisOff], result.Type())
		}

		// ARC: release temporary struct receiver (method chain temporaries).
		// When the receiver is a temporary produced by a call (e.g. foo().bar()),
		// the struct returned by foo() has its RC fields retained by foo's return
		// but is never stored in a named variable and thus never released at scope
		// exit.  Release it here to balance the retain emitted by foo's return.
		if !fn.IsPtr && isTemporaryProducer(fn.Expr) {
			cg.emitRelease(block, objVal)
		}

		if irtypes.IsVoid(result.Type()) {
			return nil, nil
		}

		return result, nil
	}
	// Fallback: the "method" might be a callable function field on the struct.
	// e.g. struct handler { validate fn(i64) bool } called as h.validate(x).
	// Same fallback for `*Struct` receivers (`this.f(x)` inside a
	// method where this is *Self), GEP'ing through the pointer
	// directly so the field is reached without an intermediate
	// copy + the call sees the live struct.
	var fieldGepBase value.Value
	if _, isStruct := objVal.Type().(*irtypes.StructType); isStruct {
		fieldGepBase = block.NewAlloca(objVal.Type())
		block.NewStore(objVal, fieldGepBase)
	} else if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
		if _, ok2 := pt.ElemType.(*irtypes.StructType); ok2 {
			fieldGepBase = objVal
		}
	}

	if fieldGepBase != nil {
		gep := cg.emitFieldGEP(block, fieldGepBase, structName, fn.Field)
		if gep != nil {
			if pt, ok := gep.Type().(*irtypes.PointerType); ok {
				fieldVal := block.NewLoad(pt.ElemType, gep)
				if isFatFnPtr(fieldVal.Type()) {
					return cg.callFatFn(block, fieldVal, e.Args)
				}
			}
		}
	}

	if witnesses, stripped := cg.deadStrippedMethods[structName][fn.Field]; stripped {
		return nil, cg.nodeErr(e, "%s.%s %s",
			cg.diagStructName(structName), fn.Field, formatStripWitnesses(witnesses))
	}

	if _, isPtr := objLookupType.(*irtypes.PointerType); isPtr {
		return nil, cg.nodeErr(e, "undefined method: %s.%s (possible missing dereference)", cg.diagStructName(structName), fn.Field)
	}

	return nil, cg.nodeErr(e, "undefined method: %s.%s", cg.diagStructName(structName), fn.Field)
}

// pickGenericMethodOverload picks the generic-method template whose
// where-clauses match `subst`.  Constrained overloads are tried first;
// an unconstrained overload only wins as the fallback when no constrained
// match exists.  Returns nil only when every overload's constraints fail
// AND no unconstrained fallback exists.
func (cg *CodeGen) pickGenericMethodOverload(tmpls []*ast.FuncDecl, subst map[string]TypeName) *ast.FuncDecl {
	if len(tmpls) == 0 {
		return nil
	}

	if len(tmpls) == 1 {
		return tmpls[0]
	}

	var fallback *ast.FuncDecl

	for _, t := range tmpls {
		if len(t.Constraints) == 0 {
			if fallback == nil {
				fallback = t
			}

			continue
		}

		ok := true

		for _, c := range t.Constraints {
			tn, found := subst[c.TypeParam]
			if !found {
				ok = false

				break
			}

			satisfied, _ := cg.typeBoundSatisfied(tn.Canon, c.Bound)
			if !satisfied {
				ok = false

				break
			}
		}

		if ok {
			return t
		}
	}

	return fallback
}

// materializeGenericMethodRef monomorphizes a generic method template for the
// given explicit type-arg index (a single Identifier or a comma-encoded
// Identifier name).  Returns the resulting concrete *ir.Func as a value,
// so callers using `Type::method[T]` as a value (e.g. `.map(V::get[i64])`)
// get a usable fn-ptr.  Mirrors the type-arg parsing in
// genCallFieldAccessGeneric.
func (cg *CodeGen) materializeGenericMethodRef(tmpl *ast.FuncDecl, templateKey string, index ast.Node) (value.Value, error) {
	var rawParts []string

	switch ix := index.(type) {
	case *ast.Identifier:
		rawParts = strings.Split(ix.Name, ",")
	default:
		key := cg.exprToTypeParamKey(index)
		if key == "" {
			return nil, fmt.Errorf("%s[...]: cannot parse type arguments", templateKey)
		}

		rawParts = []string{key}
	}

	if len(rawParts) != len(tmpl.TypeParams) {
		return nil, fmt.Errorf("%s[%s]: expected %d type arg(s), got %d",
			templateKey, strings.Join(rawParts, ","), len(tmpl.TypeParams), len(rawParts))
	}

	typeSubst := make(map[string]TypeName, len(rawParts))
	for i, part := range rawParts {
		part = strings.TrimSpace(part)
		typeSubst[tmpl.TypeParams[i]] = cg.typeNameFromCanon(part)
	}

	instKey := ""

	for i, tp := range tmpl.TypeParams {
		if i > 0 {
			instKey += "__"
		}

		instKey += typeSubst[tp].Canon
	}

	tmplCopy := *tmpl
	tmplCopy.Name = templateKey

	concreteFunc, err := cg.monomorphizeFunc(&tmplCopy, instKey, typeSubst)
	if err != nil {
		return nil, err
	}

	return concreteFunc, nil
}

// genCallFieldAccessGeneric handles the `obj.method[T, ...](args)` form: the
// parser builds CallExpr{IndexExpr{FieldAccess{obj, method}, T}, args}; we
// take the FieldAccess and the explicit type-arg index here and route to a
// monomorphization of the receiver type's generic-method template with the
// caller-supplied type substitution.  This is the "explicit form" path; the
// argument- and return-type inference paths live in genCallFieldAccess.
func (cg *CodeGen) genCallFieldAccessGeneric(block *ir.Block, e *ast.CallExpr, fa *ast.FieldAccess, index ast.Node) (value.Value, error) {
	objVal, err := cg.genExpr(block, fa.Expr)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if fa.IsPtr {
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			objVal = block.NewLoad(pt.ElemType, objVal)
		}
	}

	objLookupType := objVal.Type()
	if pt, ok := objLookupType.(*irtypes.PointerType); ok {
		if cg.typeNameOf(pt.ElemType) != "" {
			objLookupType = pt.ElemType
		}
	}

	structName := cg.typeNameOf(objLookupType)
	if structName == "" {
		return nil, cg.nodeErr(e, "obj.method[T](...): cannot resolve receiver type")
	}

	templateKey := structName + "_" + fa.Field

	tmpls, ok := cg.genericMethodTemplates[templateKey]
	if !ok || len(tmpls) == 0 {
		return nil, cg.nodeErr(e, "undefined generic method: %s.%s", cg.diagStructName(structName), fa.Field)
	}

	tmpl := tmpls[0]
	// IndexExpr.Index encodes the explicit type args: a single arg arrives as
	// a bare Identifier, multiple args as a comma-joined Identifier name
	// (the parser stitches `T, U` into `"T,U"` so the postfix code can hand
	// it back through the same DCOLON/DOT paths).  Parse each piece as a
	// type-key string so nested generics and aliases land on the same
	// canonicalization path used by every other monomorphization site.
	var rawParts []string

	switch ix := index.(type) {
	case *ast.Identifier:
		rawParts = strings.Split(ix.Name, ",")
	default:
		key := cg.exprToTypeParamKey(index)
		if key == "" {
			return nil, cg.nodeErr(e, "%s.%s[...]: cannot parse type arguments", cg.diagStructName(structName), fa.Field)
		}

		rawParts = []string{key}
	}

	if len(rawParts) != len(tmpl.TypeParams) {
		return nil, cg.nodeErr(e, "%s.%s[%s]: expected %d type arg(s), got %d",
			cg.diagStructName(structName), fa.Field, strings.Join(rawParts, ","),
			len(tmpl.TypeParams), len(rawParts))
	}

	typeSubst := make(map[string]TypeName, len(rawParts))
	for i, part := range rawParts {
		part = strings.TrimSpace(part)
		typeSubst[tmpl.TypeParams[i]] = cg.typeNameFromCanon(part)
	}
	// Where-clause overload pick: with explicit T, the right overload is
	// the one whose where-clause matches the user-supplied type.
	if len(tmpls) > 1 {
		if picked := cg.pickGenericMethodOverload(tmpls, typeSubst); picked != nil {
			tmpl = picked
		}
	}

	callArgs := make([]value.Value, 0, len(e.Args))
	for _, arg := range e.Args {
		av, err2 := cg.genExpr(block, arg)
		if err2 != nil {
			return nil, err2
		}

		callArgs = append(callArgs, av)

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	instKey := ""

	for i, tp := range tmpl.TypeParams {
		if i > 0 {
			instKey += "__"
		}

		instKey += typeSubst[tp].Canon
	}

	tmplCopy := *tmpl
	tmplCopy.Name = templateKey

	concreteFunc, err := cg.monomorphizeFunc(&tmplCopy, instKey, typeSubst)
	if err != nil {
		return nil, err
	}

	thisArg := objVal
	if len(concreteFunc.Sig.Params) > 0 {
		if pt, isPtr := concreteFunc.Sig.Params[0].(*irtypes.PointerType); isPtr {
			if pt.ElemType.Equal(objVal.Type()) {
				if lv, err2 := cg.genLValue(block, fa.Expr); err2 == nil {
					thisArg = lv
				} else {
					tmp := block.NewAlloca(objVal.Type())
					block.NewStore(objVal, tmp)
					thisArg = tmp
				}
			}
		}
	}

	llArgs := make([]value.Value, 0, len(callArgs)+1)
	llArgs = append(llArgs, thisArg)
	llArgs = append(llArgs, callArgs...)
	llArgs = cg.adaptArgs(block, llArgs, concreteFunc.Sig)
	result := block.NewCall(cg.resolveColoredFn(concreteFunc), llArgs...)

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}
