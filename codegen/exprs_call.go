package codegen

import (
	"fmt"
	"os"
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
			return cg.expandMacro(block, macro, e.Args)
		}
		// Also check with trailing ! stripped (for macro! call syntax).
		if strings.HasSuffix(fn.Name, "!") {
			baseName := fn.Name[:len(fn.Name)-1]
			if macro, ok := cg.macros[baseName+"!"]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}

			if macro, ok := cg.macros[baseName]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}
		}
		// #no_excl: allow calling macro! as plain function name (without !).
		// Only applies when the macro has the "no_excl" tag.
		if !strings.HasSuffix(fn.Name, "!") {
			if macro, ok := cg.macros[fn.Name+"!"]; ok && macroHasTag(macro, "no_excl") {
				return cg.expandMacro(block, macro, e.Args)
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
		// Check if this is a generic or constrained function call - monomorphize it.
		{
			var gTmpl *ast.FuncDecl
			if t, ok2 := cg.constrainedFuncs[fn.Name]; ok2 {
				gTmpl = t
			} else if t, ok2 := cg.genericFuncs[fn.Name]; ok2 {
				// Prefer a concrete compiled version over the template when one exists in
				// scope (e.g. the non-generic parse() inside json::parse[T]).
				if _, concreteOk := cg.curScope.lookup(fn.Name); !concreteOk {
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
				// represented in the inferred target type (e.g. negative value → unsigned).
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

			best := cg.resolveOverload(variants, argVals)
			if best == nil {
				return nil, fmt.Errorf("no matching overload for %s (got %d arg(s))", fn.Name, len(argVals))
			}

			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, fmt.Errorf("overload %s not found in scope", best.irName)
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
			return nil, fmt.Errorf("undefined function: %s", fn.Name)
		}
		// Warn when a {#blocking} extern is called inside an {#async} function.
		if cg.curCoroHdl != nil {
			if origDecl, found := cg.funcDecls[fn.Name]; found {
				if origDecl.IsExtern != "" && hasTag(origDecl.Tags, "blocking") {
					_, _ = fmt.Fprintf(os.Stderr,
						"warning: calling blocking extern %q inside an {#async} function; "+
							"use async_read/async_write instead\n", fn.Name)
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
			// Also try the concrete monomorphized key when a type arg is present.
			if typeArgStr != "" {
				// typeArgStr may be comma-separated for multi-param generics (e.g. "string,i64").
				// Build the canonical concrete name by joining parts with __.
				typeArgParts := strings.Split(typeArgStr, ",")

				concreteName := staticName + "__" + strings.Join(typeArgParts, "__")
				if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
					if _, isGeneric := cg.genericStructsByArity[staticName]; isGeneric {
						typeParams := make([]ast.TypeExpr, len(typeArgParts))
						for i, p := range typeArgParts {
							typeParams[i] = parseTypeParamStr(strings.TrimSpace(p))
						}

						synthDecl := &ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: staticName, TypeParams: typeParams},
						}
						_ = cg.genTypeDecl(synthDecl)
					}
				}

				if _, exists := cg.structTypes[concreteName]; exists {
					methodKey = concreteName + "_" + fn.Field
					staticName = concreteName
				}
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

					llArgs = cg.adaptArgs(block, llArgs, f.Sig)

					return block.NewCall(f, llArgs...), nil
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
			return cg.callTraitMethod(block, objVal, traitName, fn.Field, e.Args)
		}
		// Auto-deref: *TraitFatPtr -> load the fat pointer and dispatch through vtable.
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			if traitName, ok2 := cg.isTraitFatPtr(pt.ElemType); ok2 {
				loaded := block.NewLoad(pt.ElemType, objVal)

				return cg.callTraitMethod(block, loaded, traitName, fn.Field, e.Args)
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
				return nil, fmt.Errorf("no matching overload for %s.%s (got %d arg(s))", structName, fn.Field, len(argVals))
			}

			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, fmt.Errorf("overload %s not found in scope", best.irName)
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

		if _, isPtr := objLookupType.(*irtypes.PointerType); isPtr {
			return nil, fmt.Errorf("undefined method: %s.%s (possible missing dereference)", structName, fn.Field)
		}

		return nil, fmt.Errorf("undefined method: %s.%s", structName, fn.Field)

	case *ast.ScopeAccess:
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

			best := cg.resolveOverload(variants, argVals)
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
		// Check genericFuncs first, then constrainedFuncs for cross-package generic calls.
		for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
			result, _, found, err2 := cg.callGenericFromMap(block, e.Args, bareName, m)
			if err2 != nil {
				return nil, err2
			}

			if found {
				return result, nil
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

			switch inner := fn.Expr.(type) {
			case *ast.Identifier:
				funcName = inner.Name
			case *ast.ScopeAccess:
				funcName = inner.Path[len(inner.Path)-1]
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

				// Look up the generic function template
				tmpl, isGeneric := cg.genericFuncs[funcName]
				if !isGeneric {
					tmpl, isGeneric = cg.constrainedFuncs[funcName]
				}

				if isGeneric && len(tmpl.TypeParams) > 0 {
					typeSubst := map[string]string{tmpl.TypeParams[0]: typeArgName}
					instKey := typeArgName

					concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
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
	llArgs := make([]value.Value, 0, len(e.Args))

	llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
	for _, arg := range e.Args {
		av, err := cg.genExpr(block, arg)
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

	// Adapt argument types.
	if f, ok := callee.(*ir.Func); ok {
		calleeType = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			calleeType = ft
		}
	}

	if calleeType != nil {
		llArgs = cg.adaptArgs(block, llArgs, calleeType)
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
	// Check if this is an enum member access: EnumName.Member
	if id, ok := e.Expr.(*ast.Identifier); ok {
		key := id.Name + "." + e.Field
		if val, ok2 := cg.enumValues[key]; ok2 {
			baseType := cg.enumTypes[id.Name]
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
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)
		gep := block.NewGetElementPtr(objType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

		return block.NewLoad(irtypes.I64, gep), nil
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

		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
	}

	// Handle field access on %S.native values: embedded cLayoutStruct fields.
	// These arise when reading a cLayoutStruct field that itself is a cLayoutStruct
	// (e.g. outer_t.a where a is inner_t, both cLayoutStructs). We already have the
	// native value; GEP directly without going through c_data_ptr.
	if strings.HasSuffix(structName, ".native") {
		baseName := strings.TrimSuffix(structName, ".native")

		fieldIdx := cg.nativeFieldIndex(baseName, e.Field)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
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

		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
	}

	if cg.cLayoutStructs[structName] {
		// cLayoutStruct: store to alloca then access through c_data_ptr.
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)

		fieldIdx := cg.nativeFieldIndex(structName, e.Field)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
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
		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
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
			// Fat pointer: {T*, i64}
			elemPtrType := at.Fields[0]
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)
			ptrGep := block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

			dataPtr := block.NewLoad(elemPtrType, ptrGep)
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

	return nil, nil
}
