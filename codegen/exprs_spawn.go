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

func (cg *CodeGen) genSpawnExpr(block *ir.Block, e *ast.SpawnExpr) (value.Value, error) {
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// spawn do: block -> synthesize an anonymous {#async} function and spawn it.
	if e.DoBlock != nil {
		return cg.genSpawnDoBlock(block, e.DoBlock)
	}

	// Determine the call node and callee name.
	callNode, ok := e.Call.(*ast.CallExpr)
	if !ok {
		return nil, fmt.Errorf("spawn: expected function call expression")
	}

	// Handle method calls: spawn obj.method(args)
	if fa, ok2 := callNode.Func.(*ast.FieldAccess); ok2 {
		return cg.genSpawnMethodExpr(block, callNode, fa)
	}

	var (
		calleeName string
		scopeKey   string
	)

	switch fn := callNode.Func.(type) {
	case *ast.Identifier:
		calleeName = fn.Name
		scopeKey = fn.Name
	case *ast.ScopeAccess:
		// e.g. io::async_write -> bareName="async_write", scopeKey="io.async_write"
		calleeName = fn.Path[len(fn.Path)-1]
		scopeKey = strings.Join(fn.Path, ".")
	}

	if calleeName == "" {
		// Callee is not a simple name (e.g. fns[0], obj.field, a closure variable).
		// Evaluate it and check if it's an async fat-fn-ptr.
		fatVal, evalErr := cg.genExpr(block, callNode.Func)
		if evalErr != nil {
			return nil, fmt.Errorf("spawn: cannot determine callee name; only named function calls are supported")
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if fatVal != nil && isAsyncFatFnPtr(fatVal.Type()) {
			// Try to recover the Tin FuncType for proper Future[T] wrapping.
			// For fns[i](args) where fns: [fn{#async}(T) R], look up fns's tinType.
			var tinFnType ast.TypeExpr

			if ie, ok2 := callNode.Func.(*ast.IndexExpr); ok2 {
				if id, ok3 := ie.Expr.(*ast.Identifier); ok3 {
					if se2, ok4 := cg.curScope.lookup(id.Name); ok4 {
						if at, ok5 := se2.tinType.(*ast.ArrayType); ok5 {
							tinFnType = at.Elem
						}
					}
				}
			}

			return cg.genSpawnAsyncFatPtr(block, fatVal, callNode.Args, tinFnType)
		}

		return nil, fmt.Errorf("spawn: cannot determine callee name; only named function calls are supported")
	}

	// Evaluate arguments first so we can do overload resolution if needed.
	var callArgs []value.Value

	for _, arg := range callNode.Args {
		val, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}

		callArgs = append(callArgs, val)

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	// Look up the sync function first to derive its IR name (which may differ from calleeName)
	// and to get its return type for wrapPidInFuture.
	// e.g. for bare "async_write" inside io.tin, the scope entry points to "io__async_write".
	var (
		syncIRName    string
		syncFnRetType irtypes.Type
	)

	for _, key := range []string{scopeKey, calleeName} {
		if se2, ok3 := cg.curScope.lookup(key); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				syncIRName = fn2.Name()
				syncFnRetType = fn2.Sig.RetType

				break
			}
		}
	}

	// Look up the $coro variant of the callee.
	// Try bare name, scope-qualified name, and sync IR name (for cross-package).
	var coroFn *ir.Func

	coroKeys := []string{calleeName + "$coro", scopeKey + "$coro"}
	if syncIRName != "" && syncIRName != calleeName && syncIRName != scopeKey {
		coroKeys = append(coroKeys, syncIRName+"$coro")
	}

	for _, coroKey := range coroKeys {
		if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				coroFn = fn2

				break
			}
		}
	}

	// resolvedCalleeName is the key for funcDecls (always the bare name for return-type lookup).
	resolvedCalleeName := calleeName

	// Try overload resolution if direct lookup failed.
	if coroFn == nil && len(cg.overloads[calleeName]) > 0 {
		best := cg.resolveOverload(cg.overloads[calleeName], callArgs)
		if best != nil {
			// Also capture the sync function's return type for wrapPidInFuture.
			if se3, ok3 := cg.curScope.lookup(best.irName); ok3 {
				if fn3, ok4 := se3.val.(*ir.Func); ok4 && syncFnRetType == nil {
					syncFnRetType = fn3.Sig.RetType
				}
			}

			for _, coroKey := range []string{best.irName + "$coro", calleeName + "$coro"} {
				if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
					if fn2, ok4 := se2.val.(*ir.Func); ok4 {
						coroFn = fn2
						resolvedCalleeName = best.irName

						break
					}
				}
			}
		}
	}

	// If direct lookup failed, try monomorphizing a generic async template.
	if coroFn == nil {
		if tmpl, isGeneric := cg.genericFuncs[calleeName]; isGeneric && hasTag(tmpl.Tags, "async") {
			typeSubst := cg.inferTypeArgs(tmpl, callArgs)
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

			monoName := tmpl.Name + "__" + instKey
			coroName := monoName + "$coro"
			// Monomorphize the concrete variant. genFuncDeclAs will call
			// predeclareCoroVariant + genCoroFuncBody for async functions
			// (because no $coro stub exists for the monomorphized name yet).
			if concreteFn, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst); err2 == nil {
				if syncFnRetType == nil {
					syncFnRetType = concreteFn.Sig.RetType
				}

				resolvedCalleeName = monoName
				// Find the $coro variant in the module (generated as side effect).
				for _, f := range cg.allFuncs() {
					if f.Name() == coroName {
						coroFn = f
						cg.curScope.set(coroName, &scopeEntry{val: f, isAlloc: false})

						break
					}
				}
			}
		}
	}

	if coroFn == nil {
		// Last resort: check if calleeName is a variable whose type is an async
		// fat-fn-ptr.  This handles `spawn x(args)` where x: fn{#async}(...).
		if se, ok2 := cg.curScope.lookup(calleeName); ok2 && se.isAlloc {
			if pt, ok3 := se.val.Type().(*irtypes.PointerType); ok3 && isAsyncFatFnPtr(pt.ElemType) {
				loaded := block.NewLoad(pt.ElemType, se.val)
				fnPtr := block.NewExtractValue(loaded, 2)  // slot 2: coro ramp
				envPtr := block.NewExtractValue(loaded, 3) // slot 3: env
				fatFnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

				// Build args: env first, then actual params.
				spawnArgs := []value.Value{envPtr}

				for i, val := range callArgs {
					// Params[0] is env; i-th tin arg maps to Params[i+1].
					if i+1 < len(fatFnType.Params) {
						spawnArgs = append(spawnArgs, cg.coerce(block, val, fatFnType.Params[i+1]))
					} else {
						spawnArgs = append(spawnArgs, val)
					}
				}

				hdl := block.NewCall(fnPtr, spawnArgs...)
				pid := cg.emitSpawnCall(block, hdl)
				retType := cg.asyncFatPtrRetType(se.tinType)
				// Fallback: if se.tinType wasn't annotated (typical for
				// `let f = fn{#async}(...) T = ...`), recover T from slot
				// 0's LLVM function type.  Slot 0 returns T (slot 2
				// returns i8*, so we can't use it directly).
				if retType == nil {
					slot0Ty := pt.ElemType.(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
					if !irtypes.IsVoid(slot0Ty.RetType) {
						retType = slot0Ty.RetType
					}
				}

				return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
			}
		}

		return nil, fmt.Errorf("spawn: function %q does not have an {#async} variant; add {#async} tag", calleeName)
	}

	// Coerce arguments to match coro function params.
	// Note: no ARC retain here - the $coro ramp block retains RC-tracked
	// params before the initial suspend (see genCoroFuncBody).  A caller-side
	// retain would double-count and produce a leak.
	preCoerceCallArgs := append([]value.Value(nil), callArgs...)
	for i, val := range callArgs {
		if i < len(coroFn.Params) {
			callArgs[i] = cg.coerce(block, val, coroFn.Params[i].Type())
		}
	}

	// Call the ramp function: hdl = callee$coro(args...)
	hdl := block.NewCall(coroFn, callArgs...)

	// Spawn the fiber: pid = _tin_fiber_spawn(hdl)
	pid := cg.emitSpawnCall(block, hdl)

	// Release temporary RC-tracked arguments after spawning.  The $coro ramp
	// retains them before the initial suspend, so the caller's own reference
	// (RC=1 from construction) must be dropped after spawn.  Named variable
	// references are skipped via isCopyExpr - they are owned by their
	// declaration scope and must not be released by the call site.
	//
	// Placed AFTER _tin_fiber_spawn so that LLVM's optimizer does not pair
	// the ramp's retain with this release and eliminate both (which would
	// produce a use-after-free in the fiber if the array is freed before the
	// fiber ever reads it).
	for i, astArg := range callNode.Args {
		if i >= len(preCoerceCallArgs) {
			break
		}

		pre := preCoerceCallArgs[i]
		post := callArgs[i]

		if isCopyExpr(astArg) {
			continue
		}

		if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
			cg.emitRelease(block, post)

			continue
		}

		if isRCTrackedType(pre.Type()) {
			cg.emitRelease(block, pre)
		} else if cg.typeNameOf(pre.Type()) != "" {
			// Named struct value: the coro ramp retained its RC-tracked fields via
			// walkRCStructFields.  Release those fields here (without deinit - the
			// fiber still owns the struct and will call deinit at scope exit).
			cg.emitReleaseNoDeinit(block, pre)
		}
	}

	// Wrap pid in Future[t] where t is the original function's return type.
	// Prefer the funcDecl lookup (bare name), fall back to sync function's LLVM return type.
	if _, hasFuncDecl := cg.funcDecls[resolvedCalleeName]; hasFuncDecl {
		return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
	}

	if syncFnRetType != nil {
		return cg.wrapPidInFutureWithLLVMType(block, pid, syncFnRetType)
	}

	return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
}

// asyncFatPtrRetType extracts the actual Tin return type from a declared FuncType
// for use when wrapping a Future after spawning an async fat-fn-ptr.
// Returns nil if the type is unknown or not a FuncType.
func (cg *CodeGen) asyncFatPtrRetType(tinFnType ast.TypeExpr) irtypes.Type {
	if tinFnType == nil {
		return nil
	}

	ft, ok := tinFnType.(*ast.FuncType)
	if !ok || ft.RetType == nil {
		return nil
	}

	llRet, err := cg.tinTypeToLLVM(ft.RetType)
	if err != nil {
		return nil
	}

	return llRet
}

// genSpawnAsyncFatPtr spawns a fiber from an already-evaluated async fat-fn-ptr
// value.  It extracts the $coro fn-ptr and env, calls it with (env, args...)
// to get the coroutine handle, then spawns the fiber and returns Future[T].
// tinFnType is the declared Tin FuncType for the callee (may be nil, falls back to Future[Unit]).
func (cg *CodeGen) genSpawnAsyncFatPtr(block *ir.Block, fatVal value.Value, argNodes []ast.Node, tinFnType ast.TypeExpr) (value.Value, error) {
	fnPtr := block.NewExtractValue(fatVal, 2)  // slot 2: coro ramp
	envPtr := block.NewExtractValue(fatVal, 3) // slot 3: env
	fatFnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Build arg list: env first, then actual params (type-guided for fn values).
	llArgs := []value.Value{envPtr}

	for i, argNode := range argNodes {
		var targetType irtypes.Type
		// Params[0] is env; i-th tin arg maps to Params[i+1].
		if i+1 < len(fatFnType.Params) {
			targetType = fatFnType.Params[i+1]
		}

		av, err := cg.genArgWithTargetType(block, argNode, targetType)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if targetType != nil {
			av = cg.coerce(block, av, targetType)
		}

		llArgs = append(llArgs, av)
	}

	hdl := block.NewCall(fnPtr, llArgs...)
	pid := cg.emitSpawnCall(block, hdl)

	retType := cg.asyncFatPtrRetType(tinFnType)

	return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
}

// genSpawnMethodExpr handles `spawn obj.method(args)` - spawns a method call as a fiber.
func (cg *CodeGen) genSpawnMethodExpr(block *ir.Block, callNode *ast.CallExpr, fa *ast.FieldAccess) (value.Value, error) {
	objVal, err := cg.genExpr(block, fa.Expr)
	if err != nil {
		return nil, err
	}

	// Trait fat-ptr method: spawn traitObj.method(args)
	if instKey, isTrait := cg.isTraitFatPtr(objVal.Type()); isTrait {
		if !cg.isAsyncTraitMethod(instKey, fa.Field) {
			return nil, fmt.Errorf("spawn: trait method %q is not {#async}", fa.Field)
		}

		coroSlotIdx := cg.asyncCoroSlotIndex(instKey, fa.Field)
		if coroSlotIdx < 0 {
			return nil, fmt.Errorf("spawn: no $coro slot for trait method %q", fa.Field)
		}

		dataPtr := block.NewExtractValue(objVal, 0)
		vtablePtr := block.NewExtractValue(objVal, 1)

		vtableSt := cg.vtableFor(CanonKey(instKey))
		fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(coroSlotIdx)))
		coroSlotFnPtrType := vtableSt.Fields[coroSlotIdx].(*irtypes.PointerType)
		coroSlotFnType := coroSlotFnPtrType.ElemType.(*irtypes.FuncType)
		fnPtr := block.NewLoad(coroSlotFnPtrType, fnPtrGep)

		// Evaluate args
		llArgs := []value.Value{dataPtr}

		for _, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			llArgs = append(llArgs, av)
		}

		llArgs = cg.adaptArgs(block, llArgs, coroSlotFnType)

		hdl := block.NewCall(fnPtr, llArgs...)
		pid := cg.emitSpawnCall(block, hdl)

		// Get the actual return type of the async method (not the coro wrapper's i8*).
		// For async-only traits, traitMethodRetType returns nil (no sync slot), so we
		// fall back to looking up the method's return type from the trait declaration.
		retType := cg.traitMethodRetType(instKey, fa.Field)
		if retType == nil {
			retType = cg.traitAsyncMethodRetType(instKey, fa.Field)
		}

		return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
	}

	// Concrete struct method: look up structName_method$coro
	// Handle both value receivers (StructType) and pointer receivers (*StructType).
	structName := cg.typeNameOf(objVal.Type())
	if structName == "" {
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			structName = cg.typeNameOf(pt.ElemType)
		}
	}

	if structName == "" {
		return nil, fmt.Errorf("spawn: cannot determine struct type for method call on %s", cg.fmtArgType(objVal.Type()))
	}

	// Check if fa.Field is an async fat-fn-ptr struct field (not a method).
	// e.g. struct Handler = { handle fn{#async}(i64) i64 }; spawn h.handle(10)
	if fieldIdx := cg.fieldIndex(structName, fa.Field); fieldIdx >= 0 {
		// Determine the struct LLVM type.
		structLLVM := objVal.Type()
		if pt, ok := structLLVM.(*irtypes.PointerType); ok {
			structLLVM = pt.ElemType
		}

		if st, ok := structLLVM.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
			fieldTy := st.Fields[fieldIdx]

			if isAsyncFatFnPtr(fieldTy) {
				// Load the field value (need a pointer to the struct for GEP).
				var fieldVal value.Value

				if _, isPtr := objVal.Type().(*irtypes.PointerType); isPtr {
					gep := block.NewGetElementPtr(structLLVM, objVal,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fieldVal = block.NewLoad(fieldTy, gep)
				} else {
					alloca := block.NewAlloca(structLLVM)
					block.NewStore(objVal, alloca)
					gep := block.NewGetElementPtr(structLLVM, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fieldVal = block.NewLoad(fieldTy, gep)
				}

				// Recover the Tin FuncType from structFieldTinTypes for proper Future[T].
				var tinFnType ast.TypeExpr

				if tinFields, hasTF := cg.structFieldTinTypes[structName]; hasTF {
					fieldNames := cg.structFields[structName]

					for i, fn := range fieldNames {
						if fn == fa.Field && i < len(tinFields) {
							tinFnType = tinFields[i]

							break
						}
					}
				}

				return cg.genSpawnAsyncFatPtr(block, fieldVal, callNode.Args, tinFnType)
			}
		}
	}

	coroName := structName + "_" + fa.Field + "$coro"

	se2, ok3 := cg.curScope.lookup(coroName)
	if !ok3 {
		return nil, fmt.Errorf("spawn: method %s.%s does not have a $coro variant; is it {#async}?", structName, fa.Field)
	}

	coroFn2, ok4 := se2.val.(*ir.Func)
	if !ok4 {
		return nil, fmt.Errorf("spawn: %s is not a function", coroName)
	}

	// Build call args: (obj, args...).
	// If the $coro expects a pointer receiver (*T) but we have a value T,
	// use genLValue to get the address or fall back to a temp alloca.
	thisArg2 := objVal

	if len(coroFn2.Params) > 0 {
		firstParamTy2 := coroFn2.Params[0].Type()
		if pt2, isPtr2 := firstParamTy2.(*irtypes.PointerType); isPtr2 && pt2.ElemType.Equal(objVal.Type()) {
			if lv, err2 := cg.genLValue(block, fa.Expr); err2 == nil {
				thisArg2 = lv
			} else {
				tmp2 := block.NewAlloca(objVal.Type())
				block.NewStore(objVal, tmp2)
				thisArg2 = tmp2
			}
		}
	}

	coroArgs := []value.Value{thisArg2}
	preCoerceArgVals := make([]value.Value, 0, len(callNode.Args))

	for i, arg := range callNode.Args {
		av, err2 := cg.genExpr(block, arg)
		if err2 != nil {
			return nil, err2
		}

		preCoerceArgVals = append(preCoerceArgVals, av)

		if i+1 < len(coroFn2.Params) {
			av = cg.coerce(block, av, coroFn2.Params[i+1].Type())
		}

		coroArgs = append(coroArgs, av)
	}

	hdl2 := block.NewCall(coroFn2, coroArgs...)
	pid2 := cg.emitSpawnCall(block, hdl2)

	// Release temporary RC-tracked args after spawning (same as genSpawnExpr).
	// The receiver (coroArgs[0]) is handled separately below.
	for i, astArg := range callNode.Args {
		if i >= len(preCoerceArgVals) {
			break
		}

		pre := preCoerceArgVals[i]
		post := coroArgs[i+1] // coroArgs[0] is thisArg; user args start at 1

		if isCopyExpr(astArg) {
			continue
		}

		if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
			cg.emitRelease(block, post)

			continue
		}

		if isRCTrackedType(pre.Type()) {
			cg.emitRelease(block, pre)
		} else if cg.typeNameOf(pre.Type()) != "" {
			cg.emitReleaseNoDeinit(block, pre)
		}
	}
	// Release temporary receiver if it is not a named variable.
	if !isCopyExpr(fa.Expr) {
		if isRCTrackedType(objVal.Type()) {
			cg.emitRelease(block, objVal)
		} else if cg.typeNameOf(objVal.Type()) != "" {
			cg.emitReleaseNoDeinit(block, objVal)
		}
	}

	// Use the original method name for return type lookup.
	fnName := structName + "_" + fa.Field

	return cg.wrapPidInFuture(block, pid2, fnName)
}

// wrapPidInFutureWithLLVMType wraps a fiber PID in Future[T] using the LLVM type directly.
// Used when we have the concrete LLVM return type but no funcDecl entry (e.g., trait method spawns).
func (cg *CodeGen) wrapPidInFutureWithLLVMType(block *ir.Block, pid value.Value, retType irtypes.Type) (value.Value, error) {
	var retTypeStr string
	if retType == nil || retType.Equal(irtypes.Void) {
		// Resolve the canonical name of the Unit struct.  After the canonical
		// naming change, the Unit LLVM struct may be registered as "sync__Unit".
		retTypeStr = cg.canonicalUnitStructName()
	} else {
		retTypeStr = llvmTypeName(retType)
	}

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if cg.structTypeFor(CanonKey(futureConcreteName)) == nil {
		retTypeExpr := &ast.SimpleType{Name: retTypeStr}

		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			// Try Unit as fallback (use canonical name)
			futureConcreteName = "Future__" + cg.canonicalUnitStructName()
		}
	}

	makeFnName := futureConcreteName + "_new"

	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: sync package failed to load: %w", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - sync package could not be loaded", retTypeStr)
	}

	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// genSpawnDoBlock synthesizes an anonymous {#async} function from a `spawn do:` body block,
// predeclares and generates its $coro variant, then spawns it as a fiber.
func (cg *CodeGen) genSpawnDoBlock(block *ir.Block, doBlock *ast.Block) (value.Value, error) {
	// Generate a unique name for the anonymous async function.
	anonName := fmt.Sprintf("__spawn_do_%d", cg.spawnDoCounter)
	cg.spawnDoCounter++

	// Collect free variables referenced in the do-block body that come from the
	// enclosing function's scope.  These need to be captured by value into an env
	// struct so that the synthesized $coro function can access them safely.
	freeNames := collectFreeVars(doBlock, map[string]bool{})

	var captures []closureCapture

	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}

		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
		}

		if entry.isGlobal {
			continue // module-level global - reachable directly
		}

		if !entry.isAlloc {
			continue // not an alloca - skip
		}

		pt, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			continue
		}

		ty := pt.ElemType
		val := block.NewLoad(ty, entry.val)
		captures = append(captures, closureCapture{name: n, val: val, llvmTy: ty})
	}

	// ARC: retain every RC-tracked capture before packing it into the env struct.
	// The coroutine runs asynchronously, after the parent scope's locals are
	// released.  Without this extra retain the captured strings could be freed
	// while the env still holds a reference to them.
	// The matching release happens in genCoroFuncBody after unpackEnv.
	for _, c := range captures {
		if isRCTrackedType(c.llvmTy) {
			cg.emitRetain(block, c.val)
		}
	}

	// Pack captured values into a heap-allocated env struct.  buildEnv returns a
	// null i8* and nil struct type when there are no captures.
	envI8Ptr, envStructType := cg.buildEnv(block, captures)

	// Synthesize an ast.FuncDecl with no params, void return, and {#async} tag.
	synth := &ast.FuncDecl{
		Name:   anonName,
		Params: nil,
		Tags:   []string{"async"},
		Body:   doBlock,
	}

	// Mark as coro-callable and predeclare the $coro variant (with env pointer
	// as the first parameter so unpackEnv can find it at coroFn.Params[0]).
	cg.coroCallable[anonName] = true
	if err := cg.predeclareCoroVariant(synth, anonName, true); err != nil {
		return nil, fmt.Errorf("spawn do: predeclare failed: %w", err)
	}

	// Generate the $coro body, passing the captures so unpackEnv can restore them.
	if err := cg.genCoroFuncBody(synth, coroVersionName(anonName), captures, envStructType); err != nil {
		return nil, fmt.Errorf("spawn do: coro body generation failed: %w", err)
	}

	// Look up the generated $coro function.
	coroName := coroVersionName(anonName)

	se, ok := cg.curScope.lookup(coroName)
	if !ok || se == nil {
		return nil, fmt.Errorf("spawn do: %s$coro not found after generation", anonName)
	}

	coroFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn do: %s$coro is not a function", anonName)
	}

	// Call the ramp function with the env pointer and spawn the fiber.
	hdl := block.NewCall(coroFn, envI8Ptr)
	pid := cg.emitSpawnCall(block, hdl)

	// Void do-block spawn: wrap in Future[Unit]

	return cg.wrapPidInFuture(block, pid, "")
}

// LValue generation

// genLValue returns a pointer to the storage location of an lvalue.
