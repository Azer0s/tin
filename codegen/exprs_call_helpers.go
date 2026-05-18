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

func (cg *CodeGen) resolveColoredCallee(name string, fallback value.Value) value.Value {
	// Cooperative context covers both $coro bodies (inCoroFn) and
	// $colored bodies (curFnColoredSync).  Plain sync bodies (neither
	// flag set) stay on the plain callee -- they aren't running on a
	// fiber and yields would have nowhere to suspend to.
	if !cg.inCoroFn && !cg.curFnColoredSync {
		return fallback
	}
	// Don't redirect calls to coroutine variants (they have a
	// different signature -- return i8* hdl) or other special
	// symbols.
	if strings.HasSuffix(name, "$coro") || strings.HasSuffix(name, "$colored") {
		return fallback
	}

	coloredName := coloredVersionName(name)
	if entry, ok := cg.curScope.lookup(coloredName); ok {
		// Body-presence check (`len(f.Blocks) > 0`) -- a declaration-only
		// stub would link to nothing.  Package fns are codegenned before
		// colorCallGraph runs, so their `<name>$colored` may be
		// predeclared but never gain a body.  Falling back to the plain
		// sync callee preserves correctness (cooperation is lost for
		// that specific call, but no link failure).
		if f, isFn := entry.val.(*ir.Func); isFn && len(f.Blocks) > 0 {
			return f
		}
	}

	return fallback
}

// resolveColoredFn is the *ir.Func-keyed version of resolveColoredCallee,
// used at method/static-call sites where the callee has already been
// resolved to an IR function rather than by AST identifier.  Returns the
// $colored variant when cooperative context + sig match; the original
// fn otherwise.
func (cg *CodeGen) resolveColoredFn(f *ir.Func) *ir.Func {
	if f == nil {
		return f
	}

	if !cg.inCoroFn && !cg.curFnColoredSync {
		return f
	}

	name := f.Name()
	if strings.HasSuffix(name, "$coro") || strings.HasSuffix(name, "$colored") {
		return f
	}

	if colored := cg.lookupColoredVariant(f); colored != nil {
		return colored
	}

	return f
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

// getOrCreateCFnShimFromLLVM returns (creating on first use) a
// per-signature shim that bridges a raw C function pointer into the
// Tin fat-fn-ptr ABI.  Shim signature matches the fat-fn-ptr's inner
// `fn(i8* env, params...) ret`; the env points at a small RC block
// laid out as {i8* dtor=null, i8* c_fn_ptr}.  The shim loads c_fn_ptr
// from env+8, bitcasts to the C function type, and calls through it
// (dropping the Tin-side env arg).  The env block is allocated via
// _tin_rc_alloc so the standard fat-fn-ptr release path
// (_tin_release_closure) frees it correctly.  Keyed by LLVM signature
// so multiple externs returning the same fn shape share one shim.
// Used by wrapFromExtern when an extern's `fn(...) T` return needs
// lifting back into a Tin fat-fn-ptr value.
func (cg *CodeGen) getOrCreateCFnShimFromLLVM(fatStruct *irtypes.StructType) *ir.Func {
	innerFnPtrTy := fatStruct.Fields[0].(*irtypes.PointerType)
	innerFnTy := innerFnPtrTy.ElemType.(*irtypes.FuncType)

	// The Tin inner fn has env as first param, then the actual params.
	// The C fn type drops env and keeps the rest.
	tinParamTypes := innerFnTy.Params[1:]

	key := callbackSigKey(innerFnTy.RetType, tinParamTypes)

	if cg.cFnShims == nil {
		cg.cFnShims = make(map[string]*ir.Func)
	}

	if f, ok := cg.cFnShims[key]; ok {
		return f
	}

	shimName := "__tin_c_fn_shim_" + key

	envParam := ir.NewParam("env", irtypes.I8Ptr)
	shimParams := []*ir.Param{envParam}

	for i, t := range tinParamTypes {
		shimParams = append(shimParams, ir.NewParam(fmt.Sprintf("a%d", i), t))
	}

	shim := cg.mod.NewFunc(shimName, innerFnTy.RetType, shimParams...)
	shim.Linkage = enum.LinkageInternal

	tb := shim.NewBlock("entry")
	// Load the C fn ptr from env+8 (env layout: {dtor=null, c_fn_ptr}).
	// Offset 0 is the destructor slot _tin_release_closure invokes when
	// the block's RC hits zero; null means no extra cleanup.
	envI8Slot := tb.NewGetElementPtr(irtypes.I8, envParam,
		constant.NewInt(irtypes.I32, 8))
	envCFnPtrSlot := tb.NewBitCast(envI8Slot, irtypes.NewPointer(irtypes.I8Ptr))
	cFnRaw := tb.NewLoad(irtypes.I8Ptr, envCFnPtrSlot)

	cFnTy := irtypes.NewFunc(innerFnTy.RetType, tinParamTypes...)
	cFnPtr := tb.NewBitCast(cFnRaw, irtypes.NewPointer(cFnTy))

	callArgs := make([]value.Value, 0, len(shimParams)-1)
	for _, p := range shimParams[1:] {
		callArgs = append(callArgs, p)
	}

	result := tb.NewCall(cFnPtr, callArgs...)

	if irtypes.IsVoid(innerFnTy.RetType) {
		tb.NewRet(nil)
	} else {
		tb.NewRet(result)
	}

	cg.cFnShims[key] = shim

	return shim
}

// wrapFatFnPtrAddrAsCCallbackPtr handles a Tin source like `&cb`
// (pointer to a fat-fn-ptr local) flowing into a C extern param
// typed `i8**` (pointer to a raw C fn ptr).  Loads the fat-fn-ptr,
// builds the trampoline, stores the trampoline `i8*` in a fresh
// stack slot, and returns the slot's address.  The C side calls
// `(*cbp)(args)` to deref the slot, get the trampoline, and call
// through it (env is baked in by tin_make_trampoline).
//
// Returns (nil, nil) when the wrap doesn't apply (e.g. callee AST
// unavailable, param shape not a `*fn(...)`).
func (cg *CodeGen) wrapFatFnPtrAddrAsCCallbackPtr(
	block *ir.Block,
	callee value.Value,
	paramIdx int,
	arg value.Value,
	callExpr *ast.CallExpr,
) (value.Value, error) {
	calleeFn, ok := callee.(*ir.Func)
	if !ok {
		return nil, nil
	}

	calleeName := calleeFn.Name()
	if stripped := strings.TrimPrefix(calleeName, "__tinwrap_"); stripped != calleeName {
		calleeName = stripped
	}

	decl, ok := cg.funcDecls[calleeName]
	if !ok || decl == nil || paramIdx >= len(decl.Params) {
		return nil, nil
	}

	pt, ok := decl.Params[paramIdx].Type.(*ast.PointerType)
	if !ok {
		return nil, nil
	}

	ft, ok := pt.Elem.(*ast.FuncType)
	if !ok {
		return nil, nil
	}

	disp, err := cg.getOrCreateClosureDispatcher(ft)
	if err != nil {
		return nil, cg.nodeErr(callExpr,
			"argument %d to extern %q: cannot build closure dispatcher for %s: %v",
			paramIdx+1, calleeFn.Name(), cg.fmtArgType(arg.Type()), err)
	}

	argPt := arg.Type().(*irtypes.PointerType)
	fatVal := block.NewLoad(argPt.ElemType, arg)

	// C trampolines can't run coros -- pull the non-colored sync variant (slot 0).
	fnRaw := block.NewExtractValue(fatVal, 0)
	envRaw := block.NewExtractValue(fatVal, 3)
	fnI8 := block.NewBitCast(fnRaw, irtypes.I8Ptr)
	dispI8 := block.NewBitCast(disp, irtypes.I8Ptr)

	// See `wrapFatFnPtrAsCCallback` for the retain rationale.
	block.NewCall(cg.ensureRetain(), envRaw)
	trampoline := block.NewCall(cg.ensureMakeTrampoline(), fnI8, envRaw, dispI8)

	slot := block.NewAlloca(irtypes.I8Ptr)
	block.NewStore(trampoline, slot)

	return slot, nil
}

// wrapFatFnPtrAsCCallback converts a Tin fat-fn-ptr argument value
// into a C-callable function pointer (i8*) suitable for an extern's
// raw-C-fn-ptr param.  Mirrors codegen/interop.go's #interop return-fn
// path: extract {fn, env} from the fat-ptr, get-or-create a
// per-signature dispatcher, hand off to `tin_make_trampoline` which
// returns a runtime-synthesized thunk that bakes env into x16/r10 at
// invocation time.
//
// Returns (nil, nil) when the callee's AST is unavailable or doesn't
// describe a fn-typed param at index i (e.g. variadic slot, generic);
// the caller falls back to its standard implicit-coerce check, which
// will produce a clean diagnostic if the shape really is wrong.
func (cg *CodeGen) wrapFatFnPtrAsCCallback(
	block *ir.Block,
	callee value.Value,
	paramIdx int,
	arg value.Value,
	callExpr *ast.CallExpr,
) (value.Value, error) {
	calleeFn, ok := callee.(*ir.Func)
	if !ok {
		return nil, nil
	}
	// Look up the Tin AST decl to recover the original `fn(...) T`
	// type of the param.  Tin-side extern call sites resolve to the
	// `__tinwrap_<name>` wrapper rather than the bare extern symbol
	// (see funcs.go:1847 -- the wrapper marshals struct / fn / string
	// arg types).  Strip the prefix so the funcDecls lookup hits.
	calleeName := calleeFn.Name()
	if stripped := strings.TrimPrefix(calleeName, "__tinwrap_"); stripped != calleeName {
		calleeName = stripped
	}

	decl, ok := cg.funcDecls[calleeName]
	if !ok || decl == nil || paramIdx >= len(decl.Params) {
		return nil, nil
	}

	ft, ok := decl.Params[paramIdx].Type.(*ast.FuncType)
	if !ok {
		return nil, nil
	}

	disp, err := cg.getOrCreateClosureDispatcher(ft)
	if err != nil {
		return nil, cg.nodeErr(callExpr,
			"argument %d to extern %q: cannot build closure dispatcher for %s: %v",
			paramIdx+1, calleeFn.Name(), cg.fmtArgType(arg.Type()), err)
	}

	// C trampolines can't run coros -- pull the non-colored sync variant (slot 0).
	fnRaw := block.NewExtractValue(arg, 0)
	envRaw := block.NewExtractValue(arg, 3)
	fnI8 := block.NewBitCast(fnRaw, irtypes.I8Ptr)
	dispI8 := block.NewBitCast(disp, irtypes.I8Ptr)

	// Retain env: `tin_make_trampoline` transfers one ARC ref to the
	// trampoline (released by `atexit_release_all_pages` at process
	// exit, or `tin_interop_closure_free` mid-life).  The source
	// fat-fn-ptr (an arg, not a return) still owns its own ref --
	// without a retain here, atexit's release would double-free.
	block.NewCall(cg.ensureRetain(), envRaw)

	return block.NewCall(cg.ensureMakeTrampoline(), fnI8, envRaw, dispI8), nil
}
