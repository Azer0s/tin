package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) ensureDeferChain() {
	if cg.deferPushFn != nil {
		return
	}
	// { i8* prev, i8* fn, i8* env, i8* ret_slot }  mirrors TinDeferEntry in runtime.c
	cg.deferEntryType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr)
	cg.deferPushFn = cg.mod.NewFunc("_tin_defer_push", irtypes.Void,
		ir.NewParam("entry", irtypes.I8Ptr),
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("env", irtypes.I8Ptr),
		ir.NewParam("ret_slot", irtypes.I8Ptr),
	)
	cg.deferPopFn = cg.mod.NewFunc("_tin_defer_pop", irtypes.Void,
		ir.NewParam("n", irtypes.I64),
	)
}

// genDeferThunk generates a zero-param thunk function that, when called,
// executes the deferred call expression.  Free variables referenced by the
// call are captured by reference (alloca pointer) into a heap-allocated env
// struct so that mutations inside the thunk propagate back to the outer scope.
// Returns (fn as i8*, env as i8*).
func (cg *CodeGen) genDeferThunk(block *ir.Block, call ast.Node) (value.Value, value.Value, error) {
	// Handles "defer (fn() = body)()" and "defer do: body" (both parsed as
	// CallExpr{Func: LambdaExpr, Args: nil}).
	if callExpr, ok := call.(*ast.CallExpr); ok && len(callExpr.Args) == 0 {
		if _, isLambda := callExpr.Func.(*ast.LambdaExpr); isLambda {
			return cg.genDeferLambdaThunk(block, callExpr.Func)
		}
	}

	name := fmt.Sprintf("defer.thunk.%d", cg.strCount)
	cg.strCount++

	// Step 1: collect free variables
	freeNames := collectFreeVars(call, map[string]bool{})

	var captures []closureCapture

	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}

		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
		}

		if entry.isAlloc {
			// Capture by reference so mutations inside the thunk are visible outside.
			captures = append(captures, closureCapture{name: n, val: entry.val, llvmTy: entry.val.Type(), byRef: true})
		} else {
			captures = append(captures, closureCapture{name: n, val: entry.val, llvmTy: entry.val.Type(), byRef: false})
		}
	}

	// Step 2: build env struct and heap-allocate it
	envI8, envStructType := cg.buildEnv(block, captures)

	// Step 3: create the thunk IR function void(i8* env, i8* ret_slot)
	f := cg.mod.NewFunc(name, irtypes.Void,
		ir.NewParam("env", irtypes.I8Ptr),
		ir.NewParam("ret_slot", irtypes.I8Ptr),
	)
	entryBlock := f.NewBlock("entry")

	prevCtx := cg.pushClosureCtx(f)
	cg.curDeferRetSlotParam = f.Params[1]

	// Step 4: unpack captures from env (defer thunks run once; env persists during execution)
	cg.unpackEnv(entryBlock, f, envStructType, captures, false)

	// Step 5: emit the deferred call
	if _, err := cg.genExpr(entryBlock, call); err != nil {
		return nil, nil, err
	}

	if entryBlock.Term == nil {
		entryBlock.NewRet(nil)
	}

	// Restore context.
	cg.popClosureCtx(prevCtx)

	// Return fn as i8* and env as i8*.
	fnI8 := block.NewBitCast(f, irtypes.I8Ptr)

	return fnI8, envI8, nil
}

// genDeferLambdaThunk handles "defer fn() = body".
// The lambda's free variables are captured by reference (alloca pointer) into
// a heap-allocated env struct so that mutations inside the thunk propagate back
// to the outer function's locals. This is safe because the thunk always runs
// before the outer function returns (either inline via emitDefers or via
// _tin_panic's defer chain while the outer stack frame is still live).
func (cg *CodeGen) genDeferLambdaThunk(block *ir.Block, lambdaNode ast.Node) (value.Value, value.Value, error) {
	lambda := lambdaNode.(*ast.LambdaExpr)
	name := fmt.Sprintf("defer.lambda.thunk.%d", cg.strCount)
	cg.strCount++

	// Collect free variables from the lambda BODY (skip lambda params).
	localNames := map[string]bool{}
	for _, p := range lambda.Params {
		localNames[p.Name] = true
	}

	freeNames := collectFreeVars(lambda.Body, localNames)

	var captures []closureCapture

	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}

		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue
		}

		if entry.isAlloc {
			captures = append(captures, closureCapture{name: n, val: entry.val, llvmTy: entry.val.Type(), byRef: true})
		} else {
			captures = append(captures, closureCapture{name: n, val: entry.val, llvmTy: entry.val.Type(), byRef: false})
		}
	}

	// Build heap-allocated env struct.
	envI8, envStructType := cg.buildEnv(block, captures)

	// Create the thunk function: void(i8* env, i8* ret_slot)
	f := cg.mod.NewFunc(name, irtypes.Void,
		ir.NewParam("env", irtypes.I8Ptr),
		ir.NewParam("ret_slot", irtypes.I8Ptr),
	)
	entryBlock := f.NewBlock("entry")

	prevCtx := cg.pushClosureCtx(f)
	cg.curDeferRetSlotParam = f.Params[1]
	// Scope the cLayout escape walker to the thunk body so any
	// let-decl inside the defer thunk's lambda body is analyzed
	// against its own scope, not the outer fn's.
	cg.curFnAstBody = lambda.Body

	// Set the lambda's declared return type so genReturn can coerce values correctly.
	// e.g. for `fn() *i64 = return None`, curDeferThunkRetType = *i64.
	var lambdaRetType irtypes.Type = irtypes.Void

	if lambda.RetType != nil {
		if rt, err2 := cg.tinTypeToLLVM(lambda.RetType); err2 == nil {
			lambdaRetType = rt
			cg.curDeferThunkRetType = rt
		}
	}

	// Unpack captures from env (defer thunk runs once; env persists during execution).
	cg.unpackEnv(entryBlock, f, envStructType, captures, false)

	// Register lambda params (none for "defer fn() void = body", but support them for completeness).
	for i, p := range lambda.Params {
		// Lambda thunks take no user params - these would be zero-valued placeholders.
		_ = i

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err == nil {
			alloca := entryBlock.NewAlloca(pt)
			cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true})
		}
	}

	// Emit the lambda body.
	if _, err := cg.genBody(entryBlock, lambda.Body, lambdaRetType); err != nil {
		cg.popClosureCtx(prevCtx)

		return nil, nil, err
	}

	cg.popClosureCtx(prevCtx)

	fnI8 := block.NewBitCast(f, irtypes.I8Ptr)

	return fnI8, envI8, nil
}

// genBuiltinDefault implements default(TypeName) - returns the zero/null value
// for the given type. Works on numeric types, booleans, floats, pointers, and
// struct types. Used in generic code: `default(t)` where `t` is a type param
// that has been monomorphized to a concrete type before this is called.
