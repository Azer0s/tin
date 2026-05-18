package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genArgWithTargetType(block *ir.Block, argNode ast.Node, targetType irtypes.Type) (value.Value, error) {
	// Array literal with a known fat-array target: generate elements at the
	// target element type directly so we don't need a cross-type coercion
	// afterwards (which previously silently replaced non-empty arrays with
	// the zero fat-array and broke every match on element types <i64).
	if arrLit, ok := argNode.(*ast.ArrayLit); ok {
		if st, isStruct := targetType.(*irtypes.StructType); isStruct && isFatArrayPtr(st) {
			if pt, isPtr := st.Fields[0].(*irtypes.PointerType); isPtr {
				return cg.genArrayLitWithElemType(block, arrLit, pt.ElemType)
			}
		}

		// Fixed-size [N x T] target (e.g. struct field declared as
		// [errors::Err; 3]): build the aggregate in a fresh stack alloca and
		// load it as a value.  Without this branch the literal would default
		// to its fat-array form {T*, i64} and store-time type-checking would
		// reject the assignment, even though both shapes display as `[T]`.
		if at, isArr := targetType.(*irtypes.ArrayType); isArr {
			return cg.genArrayLitAsFixed(block, arrLit, at)
		}
	}

	// Tuple literal with a known Tuple-struct target: pick the
	// existing monomorphization rather than re-inferring from the
	// element LLVM types.  Inference flattens trait pointers into
	// SimpleType{"*errors__Err_iface"} which the type-resolver does
	// not recognize, so the synthesized struct ends up with i64
	// where `*Err` was expected and the tuple-store check rejects
	// the legitimate value.
	if tupLit, ok := argNode.(*ast.TupleLit); ok {
		if st, isStruct := targetType.(*irtypes.StructType); isStruct {
			// Tuple monomorphizations are mangled `Tuple__T1__T2__...`;
			// matching just `Tuple` prefix would also catch user
			// structs called `TupleHelper`, `Tuples`, etc.  Require the
			// `Tuple__` separator suffix to disambiguate.
			if name := st.Name(); name != "" && strings.HasPrefix(name, "Tuple__") {
				return cg.genTupleLit(block, tupLit, targetType)
			}
		}
	}

	if isFatFnPtr(targetType) {
		if id, ok := argNode.(*ast.Identifier); ok {
			if variants, hasOverloads := cg.overloads[id.Name]; hasOverloads {
				// Extract expected arity from the fat-ptr: Params[0] is env, rest are actual.
				fatSt := targetType.(*irtypes.StructType)
				fnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
				expectedArity := len(fnType.Params) - 1 // subtract env

				var best *overloadEntry

				for _, v := range variants {
					if v.arity == expectedArity {
						best = v

						break
					}
				}

				if best != nil {
					if se, seOk := cg.curScope.lookup(best.irName); seOk {
						var fnVal value.Value

						if se.isAlloc {
							pt := se.val.Type().(*irtypes.PointerType)
							fnVal = block.NewLoad(pt.ElemType, se.val)
						} else {
							fnVal = se.val
						}

						if isAsyncFatFnPtr(targetType) {
							return cg.wrapAsyncFnAsFatPtr(block, fnVal, targetType), nil
						}

						return cg.wrapFnAsFatPtr(block, fnVal, targetType), nil
					}
				}
			}
		}
	}
	// Set returnTypeHint so bare ADT constructor calls in argument
	// position disambiguate against the parameter's type. Covers
	// `f(Ok(x))` and `f(Err(e))` where f's parameter is a Result.
	// Restored after the recursive genExpr so siblings see the prior
	// (caller-supplied) hint.
	if targetType != nil {
		prevHint := cg.returnTypeHint
		cg.returnTypeHint = targetType

		v, err := cg.genExpr(block, argNode)
		cg.returnTypeHint = prevHint

		return v, err
	}

	return cg.genExpr(block, argNode)
}

// callFatFn emits a call through the 4-slot fat-fn-ptr ABI
// `{non_colored_sync*, colored_sync*, coro_ramp*, env}`.  Bare calls
// pick the slot by caller context: cooperative caller (inside $coro
// OR $colored body) -> slot 1 (colored), plain sync caller -> slot 0
// (non-colored).  Slot 2 (coro) is reserved for `spawn`, which routes
// through genSpawnAsyncFatPtr instead.
func (cg *CodeGen) callFatFn(block *ir.Block, fatPtr value.Value, argNodes []ast.Node) (value.Value, error) {
	slot := 0
	if cg.inCoroFn || cg.curFnColoredSync {
		slot = 1
	}

	fnPtr := block.NewExtractValue(fatPtr, uint64(slot))
	envPtr := block.NewExtractValue(fatPtr, 3)

	// Build args (index 0 = env, indices 1..N = actual params).
	llArgs := []value.Value{envPtr}
	llArgsPreCoerce := []value.Value{envPtr}

	// Derive target param types for type-guided resolution (Params[0] is env).
	fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	for i, arg := range argNodes {
		// Params[0] is env; the i-th tin arg maps to Params[i+1].
		var targetType irtypes.Type
		if i+1 < len(fnType.Params) {
			targetType = fnType.Params[i+1]
		}

		av, err := cg.genArgWithTargetType(block, arg, targetType)
		if err != nil {
			return nil, err
		}

		llArgs = append(llArgs, av)
		llArgsPreCoerce = append(llArgsPreCoerce, av)
	}

	llArgs = cg.adaptArgs(block, llArgs, fnType)

	result := block.NewCall(fnPtr, llArgs...)

	// ARC: release temporary RC-tracked arguments (skip index 0 = env).
	for i, astArg := range argNodes {
		argIdx := i + 1 // offset by 1 for the env slot
		preCoerce := llArgsPreCoerce[argIdx]
		postCoerce := llArgs[argIdx]

		// Case 1: adaptArgs boxed a non-any value to any.
		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}
		// Case 2: RC-tracked temporary argument.
		if !isRCTrackedType(preCoerce.Type()) {
			continue
		}

		if isCopyExpr(astArg) {
			continue
		}

		cg.emitRelease(block, preCoerce)
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// opTraitImplEntry records a single struct-method impl of a built-in
// operator trait, used by lookupOpMethod to pick a variant by parameter type.
