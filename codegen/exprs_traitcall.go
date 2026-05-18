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

func (cg *CodeGen) callTraitMethod(block *ir.Block, ifaceVal value.Value, instKey, methodName string, argNodes []ast.Node) (value.Value, error) {
	// Method order is stored by base trait name.
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	methodOrder := cg.traitMethodOrder[baseTrait]
	slotIdx := -1

	for i, n := range methodOrder {
		if n == methodName {
			slotIdx = i

			break
		}
	}

	if slotIdx < 0 {
		return nil, fmt.Errorf("trait %s has no method %s", cg.traitDisplayName(instKey), methodName)
	}

	// Extract data pointer and vtable pointer from iface fat ptr.
	dataPtr := block.NewExtractValue(ifaceVal, 0)
	vtablePtr := block.NewExtractValue(ifaceVal, 1)

	// Load function pointer from vtable[slotIdx].
	vtableSt := cg.vtableFor(CanonKey(instKey))
	fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slotIdx)))
	fnSlotType := vtableSt.Fields[slotIdx].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	fnPtr := block.NewLoad(irtypes.NewPointer(fnSlotType), fnPtrGep)

	// Build call args: (data_ptr, extra_args...).
	llArgs := []value.Value{dataPtr}

	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}

		llArgs = append(llArgs, av)
	}

	llArgs = cg.adaptArgs(block, llArgs, fnSlotType)

	result := block.NewCall(fnPtr, llArgs...)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// isAsyncTraitMethod reports whether methodName is a {#async} virtual method
// of the trait identified by instKey.
func (cg *CodeGen) isAsyncTraitMethod(instKey, methodName string) bool {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	for _, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return true
		}
	}

	return false
}

// asyncCoroSlotIndex returns the vtable field index of the $coro slot for
// methodName in instKey's vtable.  Returns -1 if not found.
// $coro slots are appended after all sync slots:
//
//	index = len(syncMethods) + position_in_asyncMethods_list
func (cg *CodeGen) asyncCoroSlotIndex(instKey, methodName string) int {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	syncCount := len(cg.traitMethodOrder[baseTrait])
	for i, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return syncCount + i
		}
	}

	return -1
}

// traitMethodRetType returns the sync LLVM return type for methodName in
// instKey's vtable (slot 0..N-1).  Returns nil if not found.
func (cg *CodeGen) traitMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	methodOrder := cg.traitMethodOrder[baseTrait]

	vtableSt := cg.vtableFor(CanonKey(instKey))
	if vtableSt == nil {
		return nil
	}

	for i, name := range methodOrder {
		if name == methodName {
			fnPtr, ok := vtableSt.Fields[i].(*irtypes.PointerType)
			if !ok {
				return nil
			}

			ft, ok := fnPtr.ElemType.(*irtypes.FuncType)
			if !ok {
				return nil
			}

			return ft.RetType
		}
	}

	return nil
}

// traitAsyncMethodRetType returns the LLVM return type for an {#async} virtual method
// by looking up the trait declaration.  Used when the method has no sync vtable slot
// (async-only traits like io::AsyncReader).
func (cg *CodeGen) traitAsyncMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	td := cg.traitFor(CanonKey(baseTrait))
	if td == nil {
		return nil
	}

	for _, m := range td.Methods {
		if m.Name != methodName || !isAsyncTag(m.Tags) {
			continue
		}

		if m.RetType == nil {
			return irtypes.Void
		}

		lt, err := cg.tinTypeToLLVM(m.RetType)
		if err != nil {
			return nil
		}

		return lt
	}

	return nil
}

// wrapFnAsFatPtr wraps a named or extern function pointer into the
// 4-slot fat-fn-ptr.  Builds an env-dropping sync shim from the
// source fn, then routes through buildFatFnPtrValue to fill slots
// 0/1/2/3 (coro wrapper / colored / non-colored / env).  Shims are
// cached per source-fn name to avoid duplicate definitions.
func (cg *CodeGen) wrapFnAsFatPtr(block *ir.Block, fnVal value.Value, targetFatType irtypes.Type) value.Value {
	fatSt := targetFatType.(*irtypes.StructType)
	// Slot 0 holds the non-colored sync variant -- the shim adapts the
	// bare fn to this env-first sync signature.  Slot 2 (coro ramp) is
	// synthesized later by buildFatFnPtrValue via ensureCoroWrapperFor.
	wrapperFnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Get the original function's type (without the env param).
	srcFnType, ok := fnVal.Type().(*irtypes.PointerType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}

	origFnType, ok := srcFnType.ElemType.(*irtypes.FuncType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}

	// Build a cache key from the function's name.
	shimName := ""
	if named, ok := fnVal.(interface{ Name() string }); ok {
		shimName = "__shim_" + named.Name()
	} else {
		shimName = fmt.Sprintf("__shim_%d", cg.strCount)
		cg.strCount++
	}

	// Reuse cached shim if already generated.
	var shim *ir.Func

	for _, fn := range cg.allFuncs() {
		if fn.Name() == shimName {
			shim = fn

			break
		}
	}

	if shim == nil {
		// The shim's signature must match wrapperFnType (the fat-fn-ptr's expected
		// function type): (i8* env, tin_param_0, tin_param_1, ...).
		// wrapperFnType.Params[0] is i8* (env); Params[1..] are the tin-level types.
		shimParams := make([]*ir.Param, len(wrapperFnType.Params))
		for i, pt := range wrapperFnType.Params {
			name := "env"
			if i > 0 {
				name = fmt.Sprintf("p%d", i-1)
			}

			shimParams[i] = ir.NewParam(name, pt)
		}

		shim = cg.mod.NewFunc(shimName, wrapperFnType.RetType, shimParams...)
		entry := shim.NewBlock("entry")
		// Forward call: skip env (index 0), adapt remaining args to orig signature.
		callArgs := make([]value.Value, len(origFnType.Params))
		for i := range origFnType.Params {
			callArgs[i] = shim.Params[i+1]
		}

		callArgs = cg.adaptArgs(entry, callArgs, origFnType)

		result := entry.NewCall(fnVal, callArgs...)
		if irtypes.IsVoid(wrapperFnType.RetType) {
			entry.NewRet(nil)
		} else {
			// Wrap return value if needed (e.g., raw i8* -> string fat-ptr).
			ret := cg.wrapFromExtern(entry, result, wrapperFnType.RetType, false)
			entry.NewRet(ret)
		}
	}

	// Build the 4-slot fat-fn-ptr value: slot 0 is a coro wrapper
	// around the sync shim, slots 1 and 2 share the shim, slot 3 is
	// null env.  Centralized via buildFatFnPtrValue so the layout
	// only lives in one place.
	return cg.buildFatFnPtrValue(block, shim, constant.NewNull(irtypes.I8Ptr))
}

// wrapAsyncFnAsFatPtr wraps an {#async} function's $coro variant into an async
// fat-fn-ptr { fn(i8* env, params...) i8* *, i8* } with a null environment.
// The shim ignores its env parameter and forwards to <name>$coro(params...).
// Falls back to wrapFnAsFatPtr (with a sync shim) when no $coro is found.
// Shims are cached per function name to avoid duplicate definitions.
func (cg *CodeGen) wrapAsyncFnAsFatPtr(block *ir.Block, fnVal value.Value, targetFatType irtypes.Type) value.Value {
	fatSt := targetFatType.(*irtypes.StructType)
	// Slot 2 is the coro-ramp slot; we need its signature to shape the
	// async shim that adapts source$coro to env-first.
	wrapperFnType := fatSt.Fields[2].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Derive the name of the function so we can look up its $coro variant.
	fnName := ""
	if named, ok := fnVal.(interface{ Name() string }); ok {
		fnName = named.Name()
	}

	if fnName == "" {
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	// Find the $coro variant in scope.
	coroName := fnName + "$coro"

	coroEntry, ok := cg.curScope.lookup(coroName)
	if !ok {
		// Also try stripping a package prefix (pkg__foo -> foo$coro).
		if idx := strings.Index(fnName, "__"); idx >= 0 {
			coroEntry, ok = cg.curScope.lookup(fnName[idx+2:] + "$coro")
		}
	}

	if !ok {
		// No $coro variant - fall back to sync shim (type mismatch at runtime).
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	coroFn, ok := coroEntry.val.(*ir.Func)
	if !ok {
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	shimName := "__ashim_" + fnName

	// Reuse cached shim.
	var shim *ir.Func

	for _, f := range cg.allFuncs() {
		if f.Name() == shimName {
			shim = f

			break
		}
	}

	if shim == nil {
		// Build shim: fn(i8* env, tin_param_0, ...) i8*
		// wrapperFnType.Params[0] is i8* (env); Params[1..] are actual types.
		shimParams := make([]*ir.Param, len(wrapperFnType.Params))
		for i, pt := range wrapperFnType.Params {
			name := "env"
			if i > 0 {
				name = fmt.Sprintf("p%d", i-1)
			}

			shimParams[i] = ir.NewParam(name, pt)
		}

		shim = cg.mod.NewFunc(shimName, irtypes.I8Ptr, shimParams...)
		entry := shim.NewBlock("entry")

		// Forward call to $coro: skip env (index 0), pass and adapt remaining args.
		n := len(coroFn.Params)
		if n > len(shim.Params)-1 {
			n = len(shim.Params) - 1
		}

		callArgs := make([]value.Value, n)
		for i := 0; i < n; i++ {
			callArgs[i] = cg.coerce(entry, shim.Params[i+1], coroFn.Params[i].Type())
		}

		hdl := entry.NewCall(coroFn, callArgs...)
		entry.NewRet(hdl)
	}

	// Build the 4-slot fat-fn-ptr {sync, colored, coro, env}.  The sync
	// slots (0, 1) need an env-first adapter -- the source's sync entry
	// has the bare user signature `fn(i64) i64` whereas the slot-0/1
	// type expects `fn(i8* env, i64) i64`.  Delegate to wrapFnAsFatPtr's
	// shim path for slots 0/1 + a synthesized coro wrapper for slot 2,
	// then OVERRIDE slot 2 with our async shim (which forwards to the
	// real $coro rather than a synth'd one).
	syncFatVal := cg.wrapFnAsFatPtr(block, fnVal, targetFatType)

	return block.NewInsertValue(syncFatVal, shim, 2)
}

// genArgWithTargetType evaluates an argument expression with a known target
// parameter type, enabling type-guided overload resolution for function-value
// arguments.  When the target is a fat-fn-ptr and the argument is a plain
// identifier that names overloaded functions, the overload whose arity matches
// the fat-ptr's parameter count is selected and wrapped appropriately.
// Falls through to a normal genExpr when the heuristic does not apply.
