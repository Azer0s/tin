package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genUnaryExpr(block *ir.Block, e *ast.UnaryExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	// genExpr may have advanced cg.curBlock through short-circuit && / ||.
	// The unary op (xor, fneg, sub, load) consumes `val` (often a phi
	// rooted in a merge block) and must be emitted there, not in the
	// stale input block. Without this `!(a || b)` lowers to an `xor`
	// in `entry` that uses a phi defined later in the merge -- invalid
	// SSA: "Instruction does not dominate all uses".
	if cg.curBlock != nil {
		block = cg.curBlock
	}

	// Operator overloading dispatch (Phase 3): if the operand is a user
	// struct that implements the corresponding built-in unary operator trait,
	// lower to a method call. Falls through to the primitive switch
	// otherwise; primitive structs (any, string, fat array) are excluded by
	// isStructType.
	if isStructType(val.Type()) {
		if traitName, isOp := unaryOpTraitName(e.Op); isOp {
			structName := cg.typeNameOf(val.Type())
			if fn := cg.lookupOpMethod(structName, traitName, nil); fn != nil {
				return cg.emitOpDispatch(block, fn, val, nil)
			}

			return nil, cg.nodeErr(e, "unary operator %q is not defined for operand of type %s", e.Op, cg.tinTypeDisplay(val.Type()))
		}
	}

	switch e.Op {
	case "-":
		if irtypes.IsFloat(val.Type()) {
			return block.NewFNeg(val), nil
		}

		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), val.Type())

		return block.NewSub(zero, val), nil
	case "!":
		b := cg.toBool(block, val)

		return block.NewXor(b, constant.NewInt(irtypes.I1, 1)), nil
	case "~":
		minusOne := cg.coerce(block, constant.NewInt(irtypes.I64, -1), val.Type())

		return block.NewXor(val, minusOne), nil
	case "*":
		// Dereference
		if pt, ok := val.Type().(*irtypes.PointerType); ok {
			return block.NewLoad(pt.ElemType, val), nil
		}

		return val, nil
	}

	return val, nil
}

func (cg *CodeGen) genCallExpr(block *ir.Block, e *ast.CallExpr) (value.Value, error) {
	// Resolve callee.
	var (
		callee     value.Value
		calleeType *irtypes.FuncType
	)

	switch fn := e.Func.(type) {
	case *ast.Identifier:
		// CTFE: evaluate #pure #no_recurse calls with constant arguments at compile time.
		if ctfeResult, err := cg.tryEvalPureCall(e); err != nil {
			return nil, err
		} else if ctfeResult != nil {
			return ctfeResult, nil
		}
		// Macro expansion: check before scope lookup.
		macroName := fn.Name
		if macro, ok := cg.macros[macroName]; ok {
			return cg.expandMacro(block, macro, e.Args, fn.Pos())
		}
		// Also check with trailing ! stripped (for macro! call syntax).
		if strings.HasSuffix(fn.Name, "!") {
			baseName := fn.Name[:len(fn.Name)-1]
			if macro, ok := cg.macros[baseName+"!"]; ok {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}

			if macro, ok := cg.macros[baseName]; ok {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}
		}
		// #no_excl: allow calling macro! as plain function name (without !).
		// Only applies when the macro has the "no_excl" tag.
		if !strings.HasSuffix(fn.Name, "!") {
			if macro, ok := cg.macros[fn.Name+"!"]; ok && macroHasTag(macro, "no_excl") {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}
		}
		// Built-in: len(expr)
		if fn.Name == "len" && len(e.Args) == 1 {
			return cg.genBuiltinLen(block, e.Args[0])
		}
		// Built-in: panic(msg)
		if fn.Name == "panic" && len(e.Args) == 1 {
			return cg.genBuiltinPanic(block, e.Args[0])
		}
		// Built-in: recover() - retrieve panic message from deferred function.
		if fn.Name == "recover" && len(e.Args) == 0 {
			return cg.genBuiltinRecover(block)
		}
		// Built-in: default(TypeName) - returns the zero value for a type.
		// Used in generic code to produce a typed zero without knowing the concrete type.
		if fn.Name == "default" && len(e.Args) == 1 {
			return cg.genBuiltinDefault(block, e.Args[0])
		}
		// Built-in: sourcepos(symbol_or_expr) - returns the atom for
		// "<name>@<file>:<line>:<col>" (identifier arg) or
		// "<file>:<line>:<col>" (expression arg or no arg). Recognized
		// only when the name "sourcepos" is not lexically shadowed;
		// otherwise the shadowing binding wins and we fall through.
		//
		// The no-arg form is the natural call site form: useful inside
		// a macro body where retagMacroBody has already pointed every
		// node to the macro CALL site, so `sourcepos()` returns the
		// caller's position without needing an arg to thread through.
		if fn.Name == "sourcepos" && len(e.Args) <= 1 {
			if _, shadowed := cg.curScope.lookup("sourcepos"); !shadowed {
				var arg ast.Node
				if len(e.Args) == 1 {
					arg = e.Args[0]
				}
				// fn.Pos() rather than e.Pos(): the parser doesn't
				// always tag the CallExpr itself, but the function-name
				// identifier is reliably tagged by the lexer. After
				// macro retag the identifier reflects the macro CALL
				// site, which is exactly what sourcepos() needs.
				return cg.genBuiltinSourcepos(block, arg, fn.Pos())
			}
		}
		// Built-in: stacktrace([cap [, opts]]) - returns [atom] of the
		// live call stack, top-of-stack first. Optional cap clamps the
		// trace length (runtime saturates to [1, 1024]); optional opts
		// is a literal `[atom]` of TIN_ST_HIDE_* filter atoms (parsed
		// at codegen). Same shadow rule as sourcepos.
		if fn.Name == "stacktrace" && len(e.Args) <= 2 {
			if _, shadowed := cg.curScope.lookup("stacktrace"); !shadowed {
				var capArg, optsArg ast.Node
				if len(e.Args) >= 1 {
					capArg = e.Args[0]
				}

				if len(e.Args) == 2 {
					optsArg = e.Args[1]
				}

				return cg.genBuiltinStacktrace(block, capArg, optsArg, e.Pos())
			}
		}
		// ADT constructor call: `Some(42)`, `Ok(42)`, `Rgb(r, g, b)`.
		// Only intercept when the name is a known variant AND is not shadowed
		// by a local binding or regular function of the same name.
		if cg.isDataVariant(fn.Name) {
			if _, shadowed := cg.curScope.lookup(fn.Name); !shadowed {
				if v, err := cg.genDataConstructorCall(block, fn.Name, e.Args); err != nil {
					return nil, err
				} else if v != nil {
					return v, nil
				}
			}
		}
		// Check if this is a generic or constrained function call - monomorphize it.
		{
			var gTmpl *ast.FuncDecl
			if t, ok2 := cg.constrainedFuncs[fn.Name]; ok2 {
				gTmpl = t
			} else if t, ok2 := cg.genericFuncs[fn.Name]; ok2 {
				// Prefer a concrete compiled version over the template when one exists in
				// scope (e.g. the non-generic parse() inside json::parse[T]).
				// Also skip the generic when concrete overloads exist for this name:
				// overloaded functions are registered with mangled names (e.g. parse__string)
				// so the bare name lookup below would fail, but the overload resolution
				// path handles them correctly. Using the generic template in this case
				// causes infinite self-recursion (parse[T] calling parse[T] again).
				_, concreteOk := cg.curScope.lookup(fn.Name)
				_, hasOverloads := cg.overloads[fn.Name]

				if !concreteOk && !hasOverloads {
					gTmpl = t
				}
			}

			if gTmpl != nil {
				tmpl := gTmpl
				// Evaluate arguments first to infer concrete types.
				argVals := make([]value.Value, 0, len(e.Args))
				for _, arg := range e.Args {
					av, err2 := cg.genExpr(block, arg)
					if err2 != nil {
						return nil, err2
					}

					argVals = append(argVals, av)
				}

				typeSubst := cg.inferTypeArgs(tmpl, argVals)
				// Build instance key from substituted types.
				instKey := ""

				for i, tp := range tmpl.TypeParams {
					if i > 0 {
						instKey += "__"
					}

					if name, found := typeSubst[tp]; found {
						instKey += name
					} else {
						instKey += tp
					}
				}

				concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
				if err2 != nil {
					return nil, err2
				}
				// Constant compatibility check: reject literals that can't be
				// represented in the inferred target type (e.g. negative value -> unsigned).
				for i, argVal := range argVals {
					if i >= len(concreteFunc.Sig.Params) {
						break
					}

					constVal, isConst := argVal.(constant.Constant)
					if !isConst {
						continue
					}

					if err3 := checkConstantCompatible(constVal, concreteFunc.Sig.Params[i]); err3 != nil {
						return nil, err3
					}
				}
				// Adapt args if needed and call.
				preCoerceVals := argVals
				argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
				result := block.NewCall(concreteFunc, argVals...)
				// ARC: release temporary RC-tracked arguments (same as regular call path).
				for i, astArg := range e.Args {
					if i >= len(preCoerceVals) {
						break
					}

					cg.emitCallArgRelease(block, astArg, preCoerceVals[i], argVals[i])
				}

				if irtypes.IsVoid(result.Type()) {
					return nil, nil
				}

				return result, nil
			}
		}
		// Overload resolution: if this name has multiple variants, evaluate args
		// first to pick the best match by type, then call it directly.
		if variants, hasOverloads := cg.overloads[fn.Name]; hasOverloads {
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

			// Prefer overload variants that exist in the most local scope to avoid
			// cross-package interference: e.g. yaml's internal parse() should call
			// yaml's own parse, not json's, even though both are in cg.overloads["parse"].
			// Traverse the scope chain from innermost outward; stop at the first level
			// that contains any matching variant (by irName in that scope's own vars).
			localVariants := variants

			for s := cg.curScope; s != nil; s = s.parent {
				var found []*overloadEntry

				for _, v := range variants {
					entry, ok := s.vars[v.irName]
					if !ok {
						continue
					}

					// Skip the self-reference that genFuncDeclAs registers in the
					// body scope (line "cg.curScope.set(scopeName, ...)").  Stopping
					// at a self-entry would hide sibling overloads in the parent scope
					// (e.g. fnv1a_32(*u8,i64) hidden from inside fnv1a_32(string)).
					if cg.curFn != nil {
						if fn, isFunc := entry.val.(*ir.Func); isFunc && fn == cg.curFn {
							continue
						}
					}

					found = append(found, v)
				}

				if len(found) > 0 {
					localVariants = found

					break
				}
			}

			best := cg.resolveOverload(localVariants, argVals)
			if best == nil {
				return nil, cg.nodeErr(e, "no matching overload for %s (got %d arg(s))", fn.Name, len(argVals))
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

			argValsPreCoerce := append([]value.Value(nil), argVals...)
			if f, ok2 := ovCallee.(*ir.Func); ok2 {
				argVals = cg.adaptArgs(block, argVals, f.Sig)
			}

			result := block.NewCall(ovCallee, argVals...)

			for i, astArg := range e.Args {
				if i >= len(argValsPreCoerce) {
					break
				}

				cg.emitCallArgRelease(block, astArg, argValsPreCoerce[i], argVals[i])
			}

			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}

		entry, ok := cg.curScope.lookup(fn.Name)
		if !ok {
			return nil, cg.nodeErr(fn, "undefined function: %s", fn.Name)
		}
		// Warn when a {#blocking} extern is called inside an {#async} function.
		if cg.curCoroHdl != nil {
			if origDecl, found := cg.funcDecls[fn.Name]; found {
				if origDecl.IsExtern != "" && hasTag(origDecl.Tags, "blocking") {
					cg.warn("blocking-in-async", e.Pos(),
						"calling blocking extern %q inside an {#async} function; use async_read/async_write instead",
						fn.Name)
				}
			}
		}

		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			loaded := block.NewLoad(ptrType.ElemType, entry.val)
			// If it's a closure fat pointer, call through it.
			if isFatFnPtr(loaded.Type()) {
				return cg.callFatFn(block, loaded, e.Args)
			}

			callee = loaded
		} else {
			callee = entry.val
		}

	case *ast.FieldAccess:
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
				if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
					if _, isGeneric := cg.genericStructsByArity[staticName]; isGeneric {
						synthDecl := &ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: staticName, TypeParams: typeArgTEs},
						}
						if mErr := cg.genTypeDecl(synthDecl); mErr != nil {
							return nil, cg.nodeErr(e, "instantiating %s: %v", prettyStructName(concreteName), mErr)
						}
					}
				}

				if _, exists := cg.structTypes[concreteName]; exists {
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
					typeName := baseStaticName
					if typeArgStr != "" {
						typeName = baseStaticName + "[" + strings.ReplaceAll(typeArgStr, ",", ", ") + "]"
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

					cg.emitCallArgRelease(block, astArg, preCoerceVals[i], llArgs[i])
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

					result := block.NewCall(f, llArgs...)

					for i, astArg := range e.Args {
						if i >= len(preCoerceVals) || i >= len(llArgs) {
							break
						}

						cg.emitCallArgRelease(block, astArg, preCoerceVals[i], llArgs[i])
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
				return nil, cg.nodeErr(e, "no matching overload for %s.%s (got %d arg(s))", structName, fn.Field, len(argVals))
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

				cg.emitCallArgRelease(block, astArg, argVals[i], llArgs[i+thisOff])
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
			if tmpl, isGenericMethod := cg.genericMethodTemplates[methodName]; isGenericMethod {
				// Evaluate call arguments.
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
				// Build arg list for type inference: this + call args.
				inferArgs := make([]value.Value, 0, len(callArgs)+1)
				inferArgs = append(inferArgs, objVal)
				inferArgs = append(inferArgs, callArgs...)
				typeSubst := cg.inferTypeArgs(tmpl, inferArgs)
				instKey := ""

				for i, tp := range tmpl.TypeParams {
					if i > 0 {
						instKey += "__"
					}

					if name, found := typeSubst[tp]; found {
						instKey += name
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
				result := block.NewCall(concreteFunc, llArgs...)
				// ARC: release temporary RC-tracked call arguments (index 1+ in llArgs; 0 is this).
				for i, astArg := range e.Args {
					if i >= len(callArgs) || i+1 >= len(llArgs) {
						break
					}

					cg.emitCallArgRelease(block, astArg, callArgs[i], llArgs[i+1])
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
			// Adapt arg types to function signature.
			if f, ok2 := callee.(*ir.Func); ok2 {
				calleeType = f.Sig
				llArgs = cg.adaptArgs(block, llArgs, calleeType)
			}

			// Auto-yield before calling a heavy or recursive method.
			block = cg.genCallSiteYieldFor(block, methodName)

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

				cg.emitCallArgRelease(block, astArg, llArgsPreCoerce[i], llArgs[i+thisOff])
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
		if _, isStruct := objVal.Type().(*irtypes.StructType); isStruct {
			alloca := block.NewAlloca(objVal.Type())
			block.NewStore(objVal, alloca)

			gep := cg.emitFieldGEP(block, alloca, structName, fn.Field)
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
				prettyStructName(structName), fn.Field, formatStripWitnesses(witnesses))
		}

		if _, isPtr := objLookupType.(*irtypes.PointerType); isPtr {
			return nil, cg.nodeErr(e, "undefined method: %s.%s (possible missing dereference)", structName, fn.Field)
		}

		return nil, cg.nodeErr(e, "undefined method: %s.%s", structName, fn.Field)

	case *ast.ScopeAccess:
		// Macro call through a qualified path (e.g. `log::info!(l, "x")`,
		// `std::log::info!(l, "x")`, etc.). The parser stores the trailing
		// `!` on the LAST path element. cg.macros is populated by
		// packages.go's pass-5 with the immediate `pkg::name!` keys, and
		// pass-6 cascades those under every re-export alias's namespace,
		// so a single lookup on the literal joined path resolves at any
		// re-export depth.
		if len(fn.Path) >= 2 {
			fullKey := strings.Join(fn.Path, "::")

			altKey := strings.Join(fn.Path, ".")
			for _, key := range []string{fullKey, altKey} {
				if macro, ok := cg.macros[key]; ok {
					return cg.expandMacro(block, macro, e.Args, fn.Pos())
				}

				if strings.HasSuffix(key, "!") {
					if macro, ok := cg.macros[key[:len(key)-1]]; ok {
						return cg.expandMacro(block, macro, e.Args, fn.Pos())
					}
				}
			}
		}

		// ADT constructor call: `Option::Some(42)` or `Option[i32]::Some(42)`
		// and similarly `Result[i32, string]::Ok(42)`.
		if v, handled, err := cg.genDataScopeCtorCall(block, fn, e.Args); handled {
			return v, err
		}

		// Static method call on a generic struct: Type[K,V]::method(args) or
		// pkg::Type[K,V]::method(args).  The ScopeAccess path looks like
		// ["collections::HashMap[string,string]", "make"].
		// Resolve the concrete name and apply overload resolution when needed.
		if len(fn.Path) >= 2 {
			methodField := fn.Path[len(fn.Path)-1]
			typePart := fn.Path[0]

			if len(fn.Path) == 3 {
				typePart = fn.Path[1]
			}

			typeParamStr := ""
			if i := strings.Index(typePart, "["); i >= 0 {
				typeParamStr = strings.TrimSuffix(typePart[i+1:], "]")
				typePart = typePart[:i]
			}

			bareBaseName := typePart
			if idx2 := strings.LastIndex(bareBaseName, "::"); idx2 >= 0 {
				bareBaseName = bareBaseName[idx2+2:]
			}

			if typeParamStr != "" {
				if _, isGeneric := cg.genericStructsByArity[bareBaseName]; isGeneric {
					// Each piece of typeParamStr can be a nested generic
					// (`*rc::Cell[i64]`), a type alias, or a qualified
					// name. Parse to a TypeExpr first so the canonical-
					// key step handles all shapes (alias chains, pointers,
					// packages) uniformly.
					rawParts := splitTopLevelTypeArgs(typeParamStr)
					resolvedParts := make([]string, len(rawParts))
					resolvedTEs := make([]ast.TypeExpr, len(rawParts))

					for i, te := range rawParts {
						resolvedParts[i] = cg.typeExprCanonicalKey(te)
						resolvedTEs[i] = te
					}

					concreteName := bareBaseName + "__" + strings.Join(resolvedParts, "__")
					if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
						if mErr := cg.genTypeDecl(&ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: bareBaseName, TypeParams: resolvedTEs},
						}); mErr != nil {
							return nil, cg.nodeErr(e, "instantiating %s: %v", prettyStructName(concreteName), mErr)
						}
					}

					concreteMethodKey := concreteName + "_" + methodField
					if variants, hasOL := cg.overloads[concreteMethodKey]; hasOL {
						olArgs := make([]value.Value, 0, len(e.Args))
						for _, arg := range e.Args {
							av, err2 := cg.genExpr(block, arg)
							if err2 != nil {
								return nil, err2
							}

							olArgs = append(olArgs, av)

							if cg.curBlock != nil && cg.curBlock != block {
								block = cg.curBlock
							}
						}

						best := cg.resolveOverload(variants, olArgs)
						if best == nil {
							return nil, cg.nodeErr(e, "no matching overload for %s[%s]::%s (got %d arg(s))",
								bareBaseName, strings.Join(resolvedParts, ", "), methodField, len(olArgs))
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

						preCoerceVals := append([]value.Value(nil), olArgs...)
						if f2, ok2 := ovCallee.(*ir.Func); ok2 {
							olArgs = cg.adaptArgs(block, olArgs, f2.Sig)
						}

						result := block.NewCall(ovCallee, olArgs...)
						// ARC: release temporary RC-tracked arguments (boxed-to-any temps,
						// fresh string concats, etc.) so generic static methods don't leak
						// the values their callers passed in. Mirrors the other call paths.
						for i, astArg := range e.Args {
							if i >= len(preCoerceVals) || i >= len(olArgs) {
								break
							}

							cg.emitCallArgRelease(block, astArg, preCoerceVals[i], olArgs[i])
						}

						if irtypes.IsVoid(result.Type()) {
							return nil, nil
						}

						return result, nil
					}
				}
			}
		}

		// Overload resolution for cross-package calls: pkg::overloadedFn(args).
		bareName := fn.Path[len(fn.Path)-1]
		if variants, hasOverloads := cg.overloads[bareName]; hasOverloads {
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

			// When the call is qualified (pkg::fn), restrict candidates to overloads
			// that belong to that package so that identically-signed functions from
			// different packages (e.g. json::parse vs yaml::parse) don't interfere.
			filteredVariants := variants

			if len(fn.Path) > 1 {
				pkgPrefix := strings.Join(fn.Path[:len(fn.Path)-1], "__") + "__"

				var pkg []*overloadEntry

				for _, v := range variants {
					if strings.HasPrefix(v.irName, pkgPrefix) {
						pkg = append(pkg, v)
					}
				}

				if len(pkg) > 0 {
					filteredVariants = pkg
				}
			}

			best := cg.resolveOverload(filteredVariants, argVals)
			if best != nil {
				if oEntry, oOk := cg.curScope.lookup(best.irName); oOk {
					var ovCallee value.Value

					if oEntry.isAlloc {
						ptrType := oEntry.val.Type().(*irtypes.PointerType)
						ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
					} else {
						ovCallee = oEntry.val
					}

					argValsPreCoerce := append([]value.Value(nil), argVals...)
					if f, ok2 := ovCallee.(*ir.Func); ok2 {
						argVals = cg.adaptArgs(block, argVals, f.Sig)
					}

					result := block.NewCall(ovCallee, argVals...)

					for i, astArg := range e.Args {
						if i >= len(argValsPreCoerce) {
							break
						}

						cg.emitCallArgRelease(block, astArg, argValsPreCoerce[i], argVals[i])
					}

					if irtypes.IsVoid(result.Type()) {
						return nil, nil
					}

					return result, nil
				}
			}
		}
		// Generic function call without explicit type arg: infer type and monomorphize.
		// When multiple packages export identically-named generics (e.g. json::encode and
		// yaml::encode both map to bare "encode"), prefer the qualified key pkg__fn so the
		// correct package's template is used.
		qualBareName := strings.Join(fn.Path, "__")
		if qualBareName != bareName {
			for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
				if tmplQ, ok := m[qualBareName]; ok {
					// monomorphizeFunc looks up the home scope by tmpl.Name (the bare function
					// name), but the bare-name entry may point to a different package's scope
					// (e.g. yaml overwrote json's "encode" home scope). Temporarily fix this
					// by setting the bare-name home scope to the qualified-key home scope
					// before calling into the generic dispatch machinery.
					qualHome := cg.genericFuncHomeScopes[qualBareName]
					prevHome := cg.genericFuncHomeScopes[tmplQ.Name]

					if qualHome != nil {
						cg.genericFuncHomeScopes[tmplQ.Name] = qualHome
					}

					result, _, found, err2 := cg.callGenericFromMap(block, e.Args, qualBareName, m)

					cg.genericFuncHomeScopes[tmplQ.Name] = prevHome

					if err2 != nil {
						return nil, err2
					}

					if found {
						return result, nil
					}
				}
			}
		}

		// Bare-name fallback for genericFuncs / constrainedFuncs.
		// Skipped when the call is qualified (len(fn.Path) > 1): a qualified path
		// uniquely names the package, so we must not fall back to a same-bare-name
		// template from a different package (e.g. assert::ok(bool) silently
		// monomorphizing result::ok[t,e](Result[t,e])).
		if len(fn.Path) <= 1 {
			for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
				result, _, found, err2 := cg.callGenericFromMap(block, e.Args, bareName, m)
				if err2 != nil {
					return nil, err2
				}

				if found {
					return result, nil
				}
			}
		}
		// e.g. weather.sunny used as function - probably an error, but handle gracefully.
		v, err := cg.genScopeAccess(block, fn)
		if err != nil {
			return nil, err
		}

		callee = v

	case *ast.IndexExpr:
		// Explicit generic instantiation: fn[TypeArg](args) or pkg::fn[TypeArg](args)
		// The parser represents decode[person](src) as
		//   CallExpr{Func: IndexExpr{Expr: decode_or_scope, Index: type_ident}, Args: [src]}
		if typeArgID, ok := fn.Index.(*ast.Identifier); ok {
			// Get the function name (bare or scope-qualified)
			var funcName string

			var qualFuncName string

			switch inner := fn.Expr.(type) {
			case *ast.Identifier:
				funcName = inner.Name
			case *ast.ScopeAccess:
				funcName = inner.Path[len(inner.Path)-1]
				// Build qualified key for disambiguation when multiple packages export
				// identically-named generics (e.g. json::parse[T] vs yaml::parse[T]).
				qualFuncName = strings.Join(inner.Path, "__")
				if qualFuncName == funcName {
					qualFuncName = ""
				}
			}

			typeArgName := typeArgID.Name
			// If the explicit type argument refers to a type parameter that has been
			// substituted (e.g. a recursive call like _encode_any[T](...) inside
			// _encode_any__jt_rect), resolve it to the concrete type so we don't
			// create a self-referential alias ("T" -> "T") that causes infinite recursion.
			if alias, ok := cg.typeAliases[typeArgName]; ok {
				if st, ok2 := alias.(*ast.SimpleType); ok2 && st.Name != typeArgName {
					typeArgName = st.Name
				}
			}

			if funcName != "" {
				// Generic struct positional construction: StructName[T](field1, field2, ...)
				// e.g. fooStruct[i32](42) where fooStruct is a generic struct template.
				if _, isStruct := cg.genericStructsByArity[funcName]; isStruct {
					synthLit := &ast.StructLit{
						TypeName:   funcName,
						TypeArgs:   []ast.TypeExpr{&ast.SimpleType{Name: typeArgName}},
						Positional: e.Args,
					}

					return cg.genStructLit(block, synthLit)
				}

				// Look up the generic function template; prefer the qualified key
				// (pkg__fn) when available so that identically-named generics in
				// different packages don't shadow each other.
				var tmpl *ast.FuncDecl

				var isGeneric bool

				usedQual := false

				if qualFuncName != "" {
					tmpl, isGeneric = cg.genericFuncs[qualFuncName]
					if isGeneric {
						usedQual = true
					}
				}

				if !isGeneric {
					tmpl, isGeneric = cg.genericFuncs[funcName]
				}

				if !isGeneric {
					tmpl, isGeneric = cg.constrainedFuncs[funcName]
				}

				// Fix home scope: monomorphizeFunc looks up by tmpl.Name (bare name),
				// but the bare-name entry may point to a different package's scope when
				// two packages export identically-named generics. When we found the
				// template via the qualified key, temporarily redirect the bare-name
				// home scope entry to the correct package's scope.
				var savedHome *scope

				if usedQual {
					if qualHome := cg.genericFuncHomeScopes[qualFuncName]; qualHome != nil {
						savedHome = cg.genericFuncHomeScopes[tmpl.Name]
						cg.genericFuncHomeScopes[tmpl.Name] = qualHome
					}
				}

				if isGeneric && len(tmpl.TypeParams) > 0 {
					typeSubst := map[string]string{tmpl.TypeParams[0]: typeArgName}
					instKey := typeArgName

					concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
					if usedQual && savedHome != nil {
						cg.genericFuncHomeScopes[tmpl.Name] = savedHome
					}

					if err2 != nil {
						return nil, err2
					}
					// Build argument list and call
					argVals := make([]value.Value, 0, len(e.Args))
					for _, arg := range e.Args {
						av, err3 := cg.genExpr(block, arg)
						if err3 != nil {
							return nil, err3
						}

						argVals = append(argVals, av)

						if cg.curBlock != nil && cg.curBlock != block {
							block = cg.curBlock
						}
					}

					preCoerceVals := append([]value.Value(nil), argVals...)
					argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
					result2 := block.NewCall(concreteFunc, argVals...)
					// ARC: release temporary RC-tracked arguments.
					for i, astArg := range e.Args {
						if i >= len(preCoerceVals) {
							break
						}

						cg.emitCallArgRelease(block, astArg, preCoerceVals[i], argVals[i])
					}

					if irtypes.IsVoid(result2.Type()) {
						return nil, nil
					}

					return result2, nil
				}
			}
		}
		// Fallthrough: evaluate as regular index expression used as function
		var err error

		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}

		if callee != nil && isFatFnPtr(callee.Type()) {
			result, err2 := cg.callFatFn(block, callee, e.Args)
			// ARC: release a temporary callee closure after the call.
			if isRCTrackedType(callee.Type()) && !isCopyExpr(e.Func) {
				cg.emitRelease(block, callee)
			}

			return result, err2
		}

	default:
		var err error

		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}
		// If the expression evaluated to a fat fn pointer, call through it.
		if callee != nil && isFatFnPtr(callee.Type()) {
			result, err2 := cg.callFatFn(block, callee, e.Args)
			// ARC: release a temporary callee closure after the call.
			if isRCTrackedType(callee.Type()) && !isCopyExpr(e.Func) {
				cg.emitRelease(block, callee)
			}

			return result, err2
		}
	}

	if callee == nil {
		return nil, fmt.Errorf("nil callee")
	}

	// Build arguments. Keep pre-coercion values for ARC temporary release.
	// When the callee's signature is already resolved (common case for bare
	// function names), pass each parameter's type down to arg codegen so
	// ArrayLits are generated at the target element type directly. Without
	// this plumbing an integer array literal takes its inferred (usually i64)
	// element type and the call-site coerce has no way to fix up the mismatch
	// without silent narrowing - which `coerce` now refuses.
	var preCalleeSig *irtypes.FuncType

	if f, ok := callee.(*ir.Func); ok {
		preCalleeSig = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			preCalleeSig = ft
		}
	}

	llArgs := make([]value.Value, 0, len(e.Args))

	llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
	for i, arg := range e.Args {
		var tType irtypes.Type
		if preCalleeSig != nil && i < len(preCalleeSig.Params) {
			tType = preCalleeSig.Params[i]
		}

		av, err := cg.genArgWithTargetType(block, arg, tType)
		if err != nil {
			return nil, err
		}

		if av != nil {
			llArgs = append(llArgs, av)
			llArgsPreCoerce = append(llArgsPreCoerce, av)
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	// Validate callee: must be a function pointer type.  A non-function value
	// (e.g. calling an integer variable) should be a compile error, not a panic.
	if f, ok := callee.(*ir.Func); ok {
		calleeType = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			calleeType = ft
		} else {
			return nil, cg.nodeErr(e, "cannot call non-function value (type %s)", pt.ElemType)
		}
	} else {
		return nil, cg.nodeErr(e, "cannot call non-function value (type %s)", fmtArgType(callee.Type()))
	}

	// Arity check: non-variadic functions must receive exactly the declared number of args.
	if calleeType != nil && !calleeType.Variadic && len(llArgs) != len(calleeType.Params) {
		calleeName := ""

		if f, ok := callee.(*ir.Func); ok {
			// Convert internal IR name (pkg__fn or pkg__fn__sig) to source form (pkg::fn).
			// Strip any overload signature suffix (last __<sig> segment) first, then
			// replace remaining __ separators with ::.
			name := f.Name()
			if idx := strings.Index(name, "__"); idx >= 0 {
				base := name[:idx] + "__" + strings.SplitN(name[idx+2:], "__", 2)[0]
				calleeName = strings.ReplaceAll(base, "__", "::")
			} else {
				calleeName = name
			}
		}

		// Anchor the diagnostic on the function name (e.Func) so the
		// underline starts at the identifier, not the open-paren. Falls
		// back to the call expression's position when Func has no Pos.
		anchor := ast.Node(e)
		if e.Func != nil && e.Func.Pos().Line > 0 {
			anchor = e.Func
		}

		endCol := cg.sourceLineEndCol(anchor.Pos().Line)
		if calleeName != "" {
			return nil, cg.nodeErrSpan(anchor, endCol, "wrong number of arguments to %q: got %d, want %d",
				calleeName, len(llArgs), len(calleeType.Params))
		}

		return nil, cg.nodeErrSpan(anchor, endCol, "wrong number of arguments: got %d, want %d",
			len(llArgs), len(calleeType.Params))
	}

	if calleeType != nil {
		llArgs = cg.adaptArgs(block, llArgs, calleeType)

		// Strict per-argument type check. Fires for clear mismatches
		// (passing a string where an int is expected, etc.) and reports
		// them with file:line and human-readable Tin type names rather
		// than letting them fall through to LLVM-level panics.
		//
		// Generic monomorphizations are not specially exempted any more:
		// dead branches in `fn enc[T](v T) string` whose runtime guard
		// `if typeof(v) == 'string` is statically false for the current
		// instantiation are now elided by the if-condition folder
		// (codegen/fold.go) BEFORE this check sees them, so the type-
		// incorrect calls in those branches no longer exist in IR.
		//
		// Fat-array element-type mismatches get the explicit
		// `arg as [T]` cast hint; everything else falls through to the
		// generic implicit-coercion allowlist in argTypeImplicitlyOK.
		for i, arg := range llArgs {
			if i >= len(calleeType.Params) {
				break
			}

			pt := calleeType.Params[i]
			if arg.Type().Equal(pt) {
				continue
			}

			if isFatArrayPtr(arg.Type()) && isFatArrayPtr(pt) {
				srcEl := arg.Type().(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType
				tgtEl := pt.(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType

				if !srcEl.Equal(tgtEl) && !srcEl.Equal(irtypes.I8) {
					return nil, cg.nodeErr(e,
						"argument %d: cannot pass [%s] where [%s] is expected; use %q to convert",
						i+1, fmtArgType(srcEl), fmtArgType(tgtEl),
						"arg as ["+fmtArgType(tgtEl)+"]")
				}

				continue
			}

			if cg.argTypeImplicitlyOK(arg.Type(), pt) {
				continue
			}

			return nil, cg.nodeErr(e,
				"argument %d: cannot pass %s where %s is expected",
				i+1, fmtArgType(arg.Type()), fmtArgType(pt))
		}
	}

	// Auto-yield before calling a heavy or recursive Tin function.
	// Uses the IR function name (matches funcDecls keys for user-defined Tin fns).
	if f, ok := callee.(*ir.Func); ok {
		block = cg.genCallSiteYieldFor(block, f.Name())
	}

	result := block.NewCall(callee, llArgs...)

	// ARC: release temporary RC-tracked arguments.  Fresh allocations (array
	// literals, concat results, function-call return values, etc.) that are
	// passed directly without being stored in a named variable have nobody to
	// release them after the callee finishes.  The callee retains on entry and
	// releases on exit, so the net rc after the call is still 1.  We drop our
	// owning reference here to reach rc=0 and free the block.
	for i, astArg := range e.Args {
		if i >= len(llArgsPreCoerce) {
			break
		}

		cg.emitCallArgRelease(block, astArg, llArgsPreCoerce[i], llArgs[i])
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// argTypeImplicitlyOK reports whether passing a value of type src where the
// callee expects pt is one of the runtime-compatible-but-LLVM-unequal
// shapes the codegen tolerates. Used by the post-coerce strict type check
// at concrete call sites.
//
// Allowed without further conversion (these are wrapped at runtime by the
// callee or by emitted shim code):
//   - target is `any`: arbitrary src is boxed by the callee glue
//   - target is a trait fat-pointer: registered impls produce the iface
//   - target or source is a raw C pointer (i8*) when the other side is a
//     fat-ptr (string/byte-slice extraction for extern calls)
//   - target accepts an implicit-conversion fn registered for src
//   - target is a fat-fn-ptr and src is a function pointer (closure shim)
//   - both sides are pointer types of compatible underlying shape (ABI is
//     the pointer width either way)
func (cg *CodeGen) argTypeImplicitlyOK(src, pt irtypes.Type) bool {
	if isAnyType(pt) {
		return true
	}

	if _, ok := cg.isTraitFatPtr(pt); ok {
		return true
	}

	if isFatFnPtr(pt) {
		return true
	}
	// Implicit-conversion functions registered for the target type.
	if name := cg.typeNameOf(pt); name != "" {
		for _, e := range cg.implicitConvFns[name] {
			if e.srcLLVM.Equal(src) {
				return true
			}
		}
	}
	// Raw C-pointer / fat-ptr extraction shims.
	if _, srcIsPtr := src.(*irtypes.PointerType); srcIsPtr {
		if _, tgtIsPtr := pt.(*irtypes.PointerType); tgtIsPtr {
			return true
		}
	}

	if isFatPtrType(src) {
		if _, tgtIsPtr := pt.(*irtypes.PointerType); tgtIsPtr {
			return true
		}
	}

	if isFatPtrType(pt) {
		if _, srcIsPtr := src.(*irtypes.PointerType); srcIsPtr {
			return true
		}
	}
	// Same-size integer types (e.g. i32 vs u32 / char vs i8): coerce
	// returned the value unchanged because the bit width matches and the
	// runtime ABI passes them identically.
	if srcInt, ok := src.(*irtypes.IntType); ok {
		if tgtInt, ok2 := pt.(*irtypes.IntType); ok2 && srcInt.BitSize == tgtInt.BitSize {
			return true
		}
	}

	return false
}

func (cg *CodeGen) adaptArgs(block *ir.Block, args []value.Value, sig *irtypes.FuncType) []value.Value {
	if sig == nil {
		return args
	}

	result := make([]value.Value, len(args))
	for i, arg := range args {
		if i < len(sig.Params) {
			result[i] = cg.coerce(block, arg, sig.Params[i])
		} else if sig.Variadic && arg != nil && isAtomType(arg.Type()) {
			// Variadic position: atoms must become i8* (the atom string rep).
			code := cg.extractAtomCode(block, arg)
			strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
			result[i] = cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
		} else if sig.Variadic && arg != nil && isFatPtrType(arg.Type()) {
			// Variadic position: fat-ptrs are not valid C varargs - unwrap to
			// the underlying raw pointer so printf-style calls work correctly.
			result[i] = cg.extractFatPtrData(block, arg, arg.Type().(*irtypes.StructType))
		} else {
			result[i] = arg
		}
	}

	return result
}

func (cg *CodeGen) genFieldAccess(block *ir.Block, e *ast.FieldAccess) (value.Value, error) {
	if _, isNil := e.Expr.(*ast.NilLit); isNil {
		return nil, cg.nodeErr(e, "field access on nil literal")
	}

	// Check if this is an enum member access: EnumName.Member or pkg::EnumName.Member
	var enumBaseName string

	switch base := e.Expr.(type) {
	case *ast.Identifier:
		enumBaseName = base.Name
	case *ast.ScopeAccess:
		// pkg::EnumName.Member - use the last path element as the enum name.
		if len(base.Path) > 0 {
			enumBaseName = base.Path[len(base.Path)-1]
		}
	}

	if enumBaseName != "" {
		key := enumBaseName + "." + e.Field
		if val, ok2 := cg.enumValues[key]; ok2 {
			baseType := cg.enumTypes[enumBaseName]
			if it, ok3 := baseType.(*irtypes.IntType); ok3 {
				return constant.NewInt(it, val), nil
			}
			// Atom enum: wrap i32 code in %__atom struct.
			if isAtomType(baseType) {
				return cg.atomConstant(int32(val)), nil
			}

			return constant.NewInt(irtypes.I32, val), nil
		}
	}

	obj, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	if obj == nil {
		return nil, nil
	}

	// If pointer, dereference first.
	objType := obj.Type()
	if e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			obj = block.NewLoad(pt.ElemType, obj)
			objType = pt.ElemType
		}
	}
	// Auto-deref: when obj is a pointer-to-named-struct, dereference it even
	// without the -> operator.  This handles pointer receiver methods where
	// `this *Foo` fields are accessed with `this.field` rather than `this->field`.
	if !e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			if cg.typeNameOf(pt.ElemType) != "" {
				obj = block.NewLoad(pt.ElemType, obj)
				objType = pt.ElemType
			}
		}
	}

	// Handle .len on dynamic arrays {T*, i64} and strings {i8*, i64}.
	if e.Field == "len" && (isFatArrayPtr(objType) || isStringType(objType)) {
		return block.NewExtractValue(obj, 1), nil
	}

	structName := cg.typeNameOf(objType)

	// Native union field access: bitcast storage to member type and load.
	if ud, isNative := cg.nativeUnionDecls[structName]; isNative {
		for _, m := range ud.Members {
			if m.FieldName == e.Field {
				memberLLVM, err2 := cg.tinTypeToLLVM(m.Type)
				if err2 != nil {
					return nil, err2
				}

				alloca := block.NewAlloca(objType)
				block.NewStore(obj, alloca)
				storageGEP := block.NewGetElementPtr(objType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				memberPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(memberLLVM))

				return block.NewLoad(memberLLVM, memberPtr), nil
			}
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", structName, e.Field)
	}

	// Handle field access on %S.native values: embedded cLayoutStruct fields.
	// These arise when reading a cLayoutStruct field that itself is a cLayoutStruct
	// (e.g. outer_t.a where a is inner_t, both cLayoutStructs). We already have the
	// native value; GEP directly without going through c_data_ptr.
	if strings.HasSuffix(structName, ".native") {
		baseName := strings.TrimSuffix(structName, ".native")

		fieldIdx := cg.nativeFieldIndex(baseName, e.Field)
		if fieldIdx < 0 {
			return nil, cg.nodeErr(e, "unknown field %s.%s", structName, e.Field)
		}

		nativeSt := cg.nativeStructTypes[baseName]
		if nativeSt != nil {
			alloca := block.NewAlloca(nativeSt)
			block.NewStore(obj, alloca)

			gep := block.NewGetElementPtr(nativeSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			if fieldIdx < len(nativeSt.Fields) {
				return block.NewLoad(nativeSt.Fields[fieldIdx], gep), nil
			}
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", structName, e.Field)
	}

	if cg.cLayoutStructs[structName] {
		// cLayoutStruct: store to alloca then access through c_data_ptr.
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)

		fieldIdx := cg.nativeFieldIndex(structName, e.Field)
		if fieldIdx < 0 {
			return nil, cg.nodeErr(e, "unknown field %s.%s", structName, e.Field)
		}

		gep := cg.emitCLayoutFieldPtr(block, alloca, structName, fieldIdx)

		nativeSt := cg.nativeStructTypes[structName]
		if nativeSt != nil && fieldIdx < len(nativeSt.Fields) {
			return block.NewLoad(nativeSt.Fields[fieldIdx], gep), nil
		}

		return block.NewLoad(irtypes.I64, gep), nil
	}

	fieldIdx := cg.fieldIndex(structName, e.Field)
	if fieldIdx < 0 {
		// Not a struct field -- check if it is a bound method reference.
		// `f.method` where f is of struct type Foo synthesizes a closure that
		// captures the receiver and calls Foo_method(receiver, args...).
		if bm, err2 := cg.genBoundMethod(block, e.Expr, obj, structName, e.Field); err2 == nil && bm != nil {
			return bm, nil
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", structName, e.Field)
	}

	// We need a pointer to the struct to do GEP.
	alloca := block.NewAlloca(objType)
	block.NewStore(obj, alloca)
	gep := block.NewGetElementPtr(objType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	// Load the field.
	if st, ok := objType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
		return block.NewLoad(st.Fields[fieldIdx], gep), nil
	}

	return block.NewLoad(irtypes.I64, gep), nil
}

func (cg *CodeGen) genIndexExpr(block *ir.Block, e *ast.IndexExpr) (value.Value, error) {
	// ptr[lo..hi] - range slice on a raw pointer: produce a fat-pointer [T].
	if bin, ok := e.Index.(*ast.BinExpr); ok && bin.Op == ".." {
		return cg.genPtrRangeSlice(block, e.Expr, bin.Left, bin.Right)
	}

	if length, ok := cg.staticArrayLen(e.Expr); ok {
		cg.checkConstIndexBounds(e, length)
	}

	// For addressable fixed-size arrays: GEP directly into the original alloca
	// without loading/copying the entire array. This is critical for arrays
	// accessed inside loops - the load+alloca+store path allocates N*sizeof(T)
	// bytes on the stack on every iteration, which is never freed until the
	// function returns, causing a stack overflow over time.
	if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil && arrPtr != nil {
		if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
			if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 {
				idx, err3 := cg.genExpr(block, e.Index)
				if err3 != nil {
					return nil, err3
				}

				if idx == nil {
					return nil, nil
				}

				idx = cg.coerce(block, idx, irtypes.I64)
				gep := block.NewGetElementPtr(at, arrPtr,
					constant.NewInt(irtypes.I32, 0), idx)

				return block.NewLoad(at.ElemType, gep), nil
			}
		}
	}

	arr, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	idx, err := cg.genExpr(block, e.Index)
	if err != nil {
		return nil, err
	}

	if arr == nil || idx == nil {
		return nil, nil
	}

	idx = cg.coerce(block, idx, irtypes.I64)

	// Check if it's a fat-ptr (dynamic array) or regular array.
	arrType := arr.Type()

	// SIMD vector: extractelement
	if _, ok := arrType.(*irtypes.VectorType); ok {
		idx32 := cg.coerce(block, idx, irtypes.I32)

		return block.NewExtractElement(arr, idx32), nil
	}

	switch at := arrType.(type) {
	case *irtypes.StructType:
		if len(at.Fields) == 2 {
			// Fat pointer: {T*, i64} - extract data pointer directly without alloca.
			elemPtrType := at.Fields[0]

			dataPtr := block.NewExtractValue(arr, 0)
			if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
				elemGep := block.NewGetElementPtr(pt.ElemType, dataPtr, idx)

				return block.NewLoad(pt.ElemType, elemGep), nil
			}
		}
	case *irtypes.ArrayType:
		alloca := block.NewAlloca(arrType)
		block.NewStore(arr, alloca)
		gep := block.NewGetElementPtr(arrType, alloca,
			constant.NewInt(irtypes.I32, 0), idx)

		return block.NewLoad(at.ElemType, gep), nil
	case *irtypes.PointerType:
		gep := block.NewGetElementPtr(at.ElemType, arr, idx)

		return block.NewLoad(at.ElemType, gep), nil
	}

	// User struct (or *Struct) receiver: dispatch to ::index trait method
	// when the struct implements index[K, R]. Mirror of dispatchBinOp's
	// path — look up an op-trait impl keyed by (structName, "index")
	// whose param type matches the index value, then emit the call.
	//
	// Comma-ok return convention (recommended): impls return a 2-tuple
	// `(V, bool)` where the bool reports whether the key was present.
	// At a tuple-destructure call site (`let (v, ok) = t[k]`) the raw
	// tuple is passed through. At any other call site, codegen auto-
	// unwraps: emits `if !ok: panic("...")` and substitutes V. Impls
	// that return plain V (no comma-ok) are also accepted — value
	// flows through unchanged.
	if structName := cg.structNameForReceiver(arrType); structName != "" {
		if fn := cg.lookupOpMethod(structName, "index", []irtypes.Type{idx.Type()}); fn != nil {
			result, derr := cg.emitOpDispatch(block, fn, arr, []value.Value{idx})
			if derr != nil {
				return nil, derr
			}

			return cg.maybeUnwrapIndexTuple(block, e, result)
		}

		return nil, cg.nodeErr(e,
			"type %s has no `::index` impl for index of type %s; declare `fn ::index(this %s, k %s) (V, bool)`",
			cg.tinTypeDisplay(arrType), cg.tinTypeDisplay(idx.Type()),
			cg.tinTypeDisplay(arrType), cg.tinTypeDisplay(idx.Type()))
	}

	return nil, cg.nodeErr(e, "type %s does not support index expressions", arrType)
}

// maybeUnwrapIndexTuple handles the comma-ok return convention from a
// user `::index` impl. If `result` is a 2-field struct shaped like
// `(V, bool)`, the function:
//
//   - returns it as-is when codegen is currently inside a tuple-
//     destructure VarDecl (cg.indexExprRawTuple); the destructure
//     step will bind both halves.
//   - otherwise extracts field 0 (V) and field 1 (bool), emits a
//     branch that panics with a descriptive message when the bool
//     is false, and returns V on the success path.
//
// If `result` is not shaped like `(V, bool)` (e.g. an impl that
// returns plain V without comma-ok), it's returned unchanged.
func (cg *CodeGen) maybeUnwrapIndexTuple(block *ir.Block, e *ast.IndexExpr, result value.Value) (value.Value, error) {
	if result == nil {
		return nil, nil
	}

	// Tin tuples are `{ i32 type_tag, T1, T2 }` — the (V, bool) pair
	// lives at fields 1 and 2.
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 3 {
		return result, nil
	}

	okField := st.Fields[2]
	if it, isInt := okField.(*irtypes.IntType); !isInt || it.BitSize != 1 {
		return result, nil
	}

	if cg.indexExprRawTuple {
		return result, nil
	}

	val := block.NewExtractValue(result, 1)
	okVal := block.NewExtractValue(result, 2)

	panicBlock := cg.newBlock("idx.miss")
	contBlock := cg.newBlock("idx.ok")

	block.NewCondBr(okVal, contBlock, panicBlock)

	msgPtr := cg.newGlobalString(indexMissMessage(e))
	panicBlock.NewCall(cg.ensurePanicFn(), msgPtr)
	panicBlock.NewUnreachable()

	cg.curBlock = contBlock

	return val, nil
}

// indexMissMessage formats the panic string for an unwrapped index miss.
// Includes the AST source position to make the source line obvious.
func indexMissMessage(e *ast.IndexExpr) string {
	pos := e.Pos()

	return fmt.Sprintf("index miss at %d:%d (no `(_, ok)` destructure to handle absent key)",
		pos.Line, pos.Col)
}

// structNameForReceiver returns the named-struct identifier when t is a
// struct or *Struct. Returns "" for any other shape. Used to drive
// op-trait dispatch (::index, ::index_set) on receivers that can be
// either a value-form struct or a pointer-to-struct.
func (cg *CodeGen) structNameForReceiver(t irtypes.Type) string {
	if isStructType(t) {
		return cg.typeNameOf(t)
	}

	if pt, ok := t.(*irtypes.PointerType); ok && isStructType(pt.ElemType) {
		return cg.typeNameOf(pt.ElemType)
	}

	return ""
}
