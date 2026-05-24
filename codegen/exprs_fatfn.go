package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) buildFatFnPtrValue(block *ir.Block, syncFn *ir.Func, env value.Value) value.Value {
	slot1 := syncFn
	if colored := cg.lookupColoredVariant(syncFn); colored != nil {
		slot1 = colored
	}

	// Slot 2 prefers a real `<name>$coro` body when one was emitted
	// (e.g. for a `fn{#async}` lambda or any function the colored set
	// classifies as coro-callable).  Falls back to the synth wrapper
	// otherwise so a plain sync fn still has a working coro ramp.
	var coroSlot value.Value = cg.ensureCoroWrapperFor(syncFn)
	if realCoro := cg.lookupRealCoroVariant(syncFn); realCoro != nil {
		coroSlot = realCoro
	}

	fatType := irtypes.NewStruct(
		irtypes.NewPointer(syncFn.Sig), // slot 0: non-colored sync (canonical)
		irtypes.NewPointer(slot1.Sig),  // slot 1: colored sync (== slot 0 when no colored variant)
		irtypes.NewPointer(coroSlot.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)), // slot 2: coro ramp / real $coro
		irtypes.I8Ptr, // slot 3: env
	)

	v0 := block.NewInsertValue(constant.NewUndef(fatType), syncFn, 0)
	v1 := block.NewInsertValue(v0, slot1, 1)
	v2 := block.NewInsertValue(v1, coroSlot, 2)
	v3 := block.NewInsertValue(v2, env, 3)

	return v3
}

// lookupRealCoroVariant returns the `<syncFn>$coro` IR function if a
// real coroutine body was emitted (predeclared AND has blocks).
// Mirrors lookupColoredVariant: a stub without a body must not be
// wired into slot 2, since the linker would resolve it to a missing
// symbol.
func (cg *CodeGen) lookupRealCoroVariant(syncFn *ir.Func) *ir.Func {
	coroName := coroVersionName(syncFn.Name())

	entry, ok := cg.curScope.lookup(coroName)
	if !ok {
		return nil
	}

	f, ok := entry.val.(*ir.Func)
	if !ok || len(f.Blocks) == 0 {
		return nil
	}

	return f
}

// lookupColoredVariant returns the `<syncFn>$colored` IR function if
// one was emitted (predeclared AND has a body) during the coloring
// pass; nil otherwise.  Used by buildFatFnPtrValue to wire slot 1
// and by ensureCoroWrapperFor to pick the body the synth wrapper
// should target.
//
// Body-presence check (`len(f.Blocks) > 0`) is mandatory: a
// `<name>$colored` symbol with zero blocks is a declaration-only
// stub that would link to nothing.  Package fns are codegenned
// before colorCallGraph runs, so their colored stubs may be
// predeclared in a later pre-pass but never gain a body; the
// fallback path lets call sites route to the plain sync entry
// instead (cooperation is lost, but no link failure).
func (cg *CodeGen) lookupColoredVariant(syncFn *ir.Func) *ir.Func {
	if syncFn == nil {
		return nil
	}

	coloredName := syncFn.Name() + "$colored"
	if entry, ok := cg.curScope.lookup(coloredName); ok {
		if f, isFn := entry.val.(*ir.Func); isFn && len(f.Blocks) > 0 {
			// Sanity: signature must match the sync fn (slot 1 has
			// the same param / return shape as slot 0).
			if f.Sig.Equal(syncFn.Sig) {
				return f
			}
		}
	}

	return nil
}

// genClosureDtor generates a per-closure destructor IR function that releases
// any RC-tracked captures stored in the closure env (built by buildClosureEnv).
// The dtor signature is void(i8* env). The env layout matches buildClosureEnv:
// field 0 = i8* dtor_ptr, fields 1..N = captures.
func (cg *CodeGen) genClosureDtor(name string, captures []closureCapture) *ir.Func {
	// Reconstruct the env struct type (must match buildClosureEnv layout).
	fields := make([]irtypes.Type, len(captures)+1)

	fields[0] = irtypes.I8Ptr
	for i, c := range captures {
		fields[i+1] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)

	dtorFn := cg.mod.NewFunc(name, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))
	entry := dtorFn.NewBlock("entry")

	envTypedPtr := entry.NewBitCast(dtorFn.Params[0], irtypes.NewPointer(envStructType))

	for i, c := range captures {
		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))

		if isRCTrackedType(c.llvmTy) {
			fieldVal := entry.NewLoad(c.llvmTy, gep)
			cg.emitRelease(entry, fieldVal)

			continue
		}
		// Pointer-to-named-struct capture (e.g. bound method on a
		// heap-allocated receiver): route through the per-struct
		// release_ptr helper so the heap block is reclaimed when the
		// closure env drops.  isRCTrackedType deliberately excludes
		// *S to keep struct-field ARC machinery from double-freeing,
		// but a closure env that captures *S DOES own one reference
		// for the duration of the env's life.
		if pt, isPtr := c.llvmTy.(*irtypes.PointerType); isPtr {
			if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					fieldVal := entry.NewLoad(c.llvmTy, gep)
					relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
					entry.NewCall(relFn, fieldVal)
				}
			}
		}
	}

	entry.NewRet(nil)

	return dtorFn
}

// recvExprIsHeapOwned reports whether the bound-method receiver
// expression resolves to a let-binding whose value is a heap-owned
// pointer (e.g. `let f = &Foo{}` or `let g = make_foo()` where
// make_foo returns *Foo).  Used by `genBoundMethod` to decide
// whether the closure env's dtor should release the captured *S
// pointer -- stack borrows must NOT be released (would read random
// data as an RC header and corrupt the surrounding frame).
//
// Conservative: returns true ONLY for Identifier sources whose
// scope entry has `isHeapOwned` set.  Other shapes (`&local`,
// `func_returning_borrowed_ptr()`) return false and the env is
// treated as borrowing the receiver for the duration of the
// closure -- the caller's binding outlives the closure or it
// would be a borrow-check error elsewhere.
func (cg *CodeGen) recvExprIsHeapOwned(recvExpr ast.Node) bool {
	switch v := recvExpr.(type) {
	case *ast.Identifier:
		if cg.curScope == nil {
			return false
		}

		entry, found := cg.curScope.lookup(v.Name)
		if !found || entry == nil {
			return false
		}

		return entry.isHeapOwned || entry.holdsFreshRCPtr

	case *ast.AddressOfExpr:
		// `&StructLit{...}` and `&Variant(...)` heap-allocate fresh
		// rc=1 blocks; anonymous receiver in `(&Foo{}).method` falls
		// through to here.  Other `&expr` shapes (`&ident` borrow,
		// `&field`, etc.) are borrows and stay false.
		switch v.Expr.(type) {
		case *ast.StructLit:
			return true
		case *ast.CallExpr:
			if id, ok := v.Expr.(*ast.CallExpr).Func.(*ast.Identifier); ok && cg.isDataVariant(id.Name) {
				return true
			}
		}
	}

	return false
}

// recvExprIsAnonHeap reports whether the bound-method receiver
// expression is an anonymous heap producer with no outer binding to
// scope-release the heap block.  Used to suppress the
// buildClosureEnv retain so the closure env takes the sole rc=1 and
// the dtor's release_ptr frees the block on drop -- a let-bound
// receiver always gets the retain because the source binding's own
// scope release balances the env's drop.
func recvExprIsAnonHeap(recvExpr ast.Node) bool {
	ao, ok := recvExpr.(*ast.AddressOfExpr)
	if !ok {
		return false
	}

	switch ao.Expr.(type) {
	case *ast.StructLit:
		return true
	case *ast.CallExpr:
		// Anon ADT-variant constructor: same shape as &StructLit.
		return true
	}

	return false
}

// genBoundMethod synthesizes a closure fat-pointer for `obj.methodName` where
// obj is of struct type structName.  The closure captures the receiver value
// and, when called, passes it as the first argument to structName_methodName.
// Returns (nil, nil) if no matching method is found (caller falls through to error).
func (cg *CodeGen) genBoundMethod(block *ir.Block, recvExpr ast.Node, obj value.Value, structName, methodName string) (value.Value, error) {
	irName := structName + "_" + methodName

	entry, ok := cg.curScope.lookup(irName)
	if !ok {
		return nil, nil
	}

	irFunc, isFunc := entry.val.(*ir.Func)
	if !isFunc {
		return nil, nil
	}

	// irFunc.Sig.Params[0] is the receiver; Params[1..] are the user-visible params.
	sig := irFunc.Sig
	if len(sig.Params) == 0 {
		return nil, nil // unexpected: static method, no receiver
	}

	// Determine receiver value to store in env.
	recvType := sig.Params[0]

	var (
		recvVal      value.Value
		capturedHeap bool // true when the captured *S is heap-owned (else borrowed)
	)

	if pt, isPtr := recvType.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(obj.Type()) {
		// Pointer receiver, VALUE-typed source: take address of the
		// variable's alloca so mutations via the closure are visible
		// through the original binding.  The address may point into
		// the current stack frame (`let c = counter{}; c.add`) OR
		// to a fresh heap block (`(&counter{}).add`, `let f = &c{}`
		// followed by `f.add`).  Both shapes route here because the
		// receiver-method's `*S` matches obj's `S` after a load step
		// in genExpr -- we have to dispatch on the recvExpr itself
		// to decide whether the dtor should release_ptr.
		if lv, lvErr := cg.genLValue(block, recvExpr); lvErr == nil && lv != nil {
			recvVal = lv
			// When recvExpr is itself a pointer variable (e.g. `let f = &Foo{}`),
			// genLValue returns the alloca that holds *Foo (type **Foo).  The method
			// expects *Foo, so we must load through the extra indirection.
			if lvPt, isLvPtr := recvVal.Type().(*irtypes.PointerType); isLvPtr {
				if lvPt.ElemType.Equal(recvType) {
					recvVal = block.NewLoad(recvType, recvVal)
				}
			}

			capturedHeap = cg.recvExprIsHeapOwned(recvExpr)
		} else {
			// Fall back: fresh alloca copy (mutations won't propagate).
			alloca := block.NewAlloca(obj.Type())
			block.NewStore(obj, alloca)
			recvVal = alloca
		}
	} else {
		// Pointer-receiver, POINTER-typed source: obj is already the *S
		// value (e.g. `let f = &counter{}; f.add`).  Capture it as-is;
		// provenance follows the source binding.
		recvVal = obj
		capturedHeap = cg.recvExprIsHeapOwned(recvExpr)
	}

	// Build the closure env capturing just the receiver.  When the
	// receiver expression is an anonymous heap producer (e.g.
	// `(&Foo{}).method`) the env takes the sole rc=1 -- skip the
	// buildClosureEnv retain so the dtor's release_ptr drops to 0
	// instead of stalling at rc=1.  Let-bound heap receivers go
	// through the standard retain because the source binding's own
	// scope-exit release balances the env's later drop.
	recvCapture := closureCapture{
		name:       "__recv",
		val:        recvVal,
		llvmTy:     recvVal.Type(),
		skipRetain: recvExprIsAnonHeap(recvExpr),
	}

	var dtorFn *ir.Func
	// Pointer-to-named-struct receivers (`f.add` on a `*counter` for
	// instance) carry a heap reference the bound-method capture has to
	// release when the closure env drops; the generic isRCTrackedType
	// gate doesn't cover *S because most other callers route those
	// releases through per-struct helpers and would double-free if it
	// did.  Emit a dedicated dtor here that calls the struct's
	// release_ptr on the captured pointer -- but ONLY when the source
	// is heap-owned, since calling release_ptr on a stack-borrowed
	// address would corrupt the surrounding frame's RC header read.
	needsPtrStructRelease := false

	if capturedHeap {
		if pt, isPtr := recvCapture.llvmTy.(*irtypes.PointerType); isPtr {
			if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					needsPtrStructRelease = true
					// The pre-existing receiver-capture retain (emitted
					// upstream when the bound method's receiver was
					// materialized) is what gives the closure env its
					// own RC slot.  Without a matching release in the
					// env dtor that retain becomes a leak; adding
					// another retain here would shift the leak by one
					// instead of fixing it.
				}
			}
		}
	}

	if isRCTrackedType(recvCapture.llvmTy) || needsPtrStructRelease {
		dtorFn = cg.genClosureDtor(fmt.Sprintf("bound.%s.%s.%d.dtor", structName, methodName, cg.strCount), []closureCapture{recvCapture})
	}

	envI8Ptr, envStructType := cg.buildClosureEnv(block, []closureCapture{recvCapture}, dtorFn)

	// Build wrapper function: fn(i8* env, userParams...) retType
	wrapperName := fmt.Sprintf("bound.%s.%s.%d", structName, methodName, cg.strCount)
	cg.strCount++

	wrapperParams := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for i := 1; i < len(sig.Params); i++ {
		wrapperParams = append(wrapperParams, ir.NewParam(fmt.Sprintf("p%d", i), sig.Params[i]))
	}

	wrapFn := cg.mod.NewFunc(wrapperName, sig.RetType, wrapperParams...)
	wrapEntry := wrapFn.NewBlock("entry")

	// Unpack receiver from env.
	envTypedPtr := wrapEntry.NewBitCast(wrapFn.Params[0], irtypes.NewPointer(envStructType))
	recvGep := wrapEntry.NewGetElementPtr(envStructType, envTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	receiverArg := wrapEntry.NewLoad(recvVal.Type(), recvGep)

	// Call the original method with receiver + forwarded params.
	callArgs := make([]value.Value, 0, len(sig.Params))
	callArgs = append(callArgs, receiverArg)

	for i := 1; i < len(wrapFn.Params); i++ {
		callArgs = append(callArgs, wrapFn.Params[i])
	}

	result := wrapEntry.NewCall(irFunc, callArgs...)
	if irtypes.IsVoid(result.Type()) {
		wrapEntry.NewRet(nil)
	} else {
		wrapEntry.NewRet(result)
	}

	// Return 4-slot fat-fn-ptr {coro_ramp, colored_sync, non_colored_sync, env}.
	fatVal := cg.buildFatFnPtrValue(block, wrapFn, envI8Ptr)

	cg.lastLambdaHadCaptures = true

	return fatVal, nil
}

// Interpolated string
