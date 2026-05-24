package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genPipeExpr(block *ir.Block, e *ast.PipeExpr) (value.Value, error) {
	// a |> f(args) = f(args)(a)  - curried style: call f(args) first, then call
	// the returned function with a.
	// a |> f         = f(a)      - plain function value on the right.

	// Bare RHS naming a generic (or otherwise non-evaluable as a value)
	// function: desugar `a |> pkg::fn` (or `a |> fn`) to `pkg::fn(a)` so
	// the regular call path handles generic-type inference from the
	// left-hand expression's type.  Without this, `genExpr(e.Right)`
	// would try to materialize `seq::reverse` as a value and fail with
	// "undefined: seq::reverse" because the generic has no concrete
	// instantiation to pick.
	if sa, ok := e.Right.(*ast.ScopeAccess); ok && cg.isBareGenericFuncRef(sa) {
		synth := &ast.CallExpr{Func: sa, Args: []ast.Node{e.Left}}

		return cg.genCallExpr(block, synth)
	}

	if id, ok := e.Right.(*ast.Identifier); ok && cg.isBareGenericFuncRefIdent(id) {
		synth := &ast.CallExpr{Func: id, Args: []ast.Node{e.Left}}

		return cg.genCallExpr(block, synth)
	}

	leftVal, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	// `a |> Adt::method` (or `pkg::Adt::method`) -- the RHS names an
	// instance method on a generic struct/ADT.  Evaluating the RHS via
	// genExpr would dispatch through genDataScopeCtorCall and fail
	// because `method` is not a variant; route directly to the method-
	// call form `Adt::method(a)` which is what the pipe sugar means.
	if sa, ok := e.Right.(*ast.ScopeAccess); ok {
		if v, handled, mErr := cg.tryPipeToStaticMethod(block, sa, leftVal); handled {
			return v, mErr
		}
	}

	// Evaluate the right-hand side completely (including any call arguments),
	// yielding the function to apply to leftVal.  Stash leftVal's type as
	// a curried-return hint so overload picking inside the RHS (e.g.
	// `map(f)` with both `[t]`-eager and `*Seq[t]`-lazy overloads) can
	// pick the variant whose returned closure consumes leftVal.
	prevHint := cg.pipeCurriedRetHint
	cg.pipeCurriedRetHint = leftVal.Type()

	rightFn, err := cg.genExpr(block, e.Right)
	cg.pipeCurriedRetHint = prevHint

	if err != nil {
		return nil, err
	}

	if rightFn == nil {
		return leftVal, nil
	}
	// Call through the function (fat-pointer or plain).
	var result value.Value

	if isFatFnPtr(rightFn.Type()) {
		// Pick the sync variant: slot 1 (colored) when the pipe sits
		// inside cooperative context ($coro or $colored body),
		// otherwise slot 0 (non-colored).  Mirrors callFatFn.
		slot := 0
		if cg.inCoroFn || cg.curFnColoredSync {
			slot = 1
		}

		fnPtr := block.NewExtractValue(rightFn, uint64(slot))
		envPtr := block.NewExtractValue(rightFn, 3)
		fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		llArgs := cg.adaptArgs(block, []value.Value{envPtr, leftVal}, fnType)
		result = block.NewCall(fnPtr, llArgs...)
	} else {
		result = block.NewCall(rightFn, leftVal)
	}
	// ARC: release the left-hand value if it is a temporary RC allocation.
	if isRCTrackedType(leftVal.Type()) && !isCopyExpr(e.Left) {
		cg.emitRelease(block, leftVal)
	}
	// ARC: release the right-hand closure if it is a temporary RC allocation
	// (e.g. `nums |> filter(fn)` where filter returns a fresh closure).
	if isRCTrackedType(rightFn.Type()) && !isCopyExpr(e.Right) {
		cg.emitRelease(block, rightFn)
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// isBareGenericFuncRef reports whether sa names a free function that
// CAN'T be materialized as a plain LLVM function value at the RHS of a
// pipe -- i.e. a generic template with no concrete instantiation, OR a
// non-generic overload set whose disambiguation depends on the LHS
// type.  In either case the pipe should desugar `a |> pkg::f` to
// `pkg::f(a)` so the normal call-resolution path handles it.
func (cg *CodeGen) isBareGenericFuncRef(sa *ast.ScopeAccess) bool {
	if len(sa.Path) < 2 {
		return false
	}

	qual := strings.Join(sa.Path, "::")
	bare := sa.Path[len(sa.Path)-1]

	for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
		if _, ok := m[qual]; ok {
			return true
		}

		if _, ok := m[bare]; ok {
			return true
		}
	}

	if ovs, ok := cg.overloads[bare]; ok && len(ovs) > 1 {
		return true
	}

	return false
}

func (cg *CodeGen) isBareGenericFuncRefIdent(id *ast.Identifier) bool {
	for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
		if _, ok := m[id.Name]; ok {
			return true
		}
	}

	if ovs, ok := cg.overloads[id.Name]; ok && len(ovs) > 1 {
		return true
	}

	return false
}

// tryPipeToStaticMethod handles `a |> Adt::method` and
// `a |> pkg::Adt::method` by converting it to `Adt::method(a)`.  Returns
// (val, true, err) when the form was recognized so genPipeExpr can stop
// before falling through to the value-as-callable path.
func (cg *CodeGen) tryPipeToStaticMethod(block *ir.Block, sa *ast.ScopeAccess, leftVal value.Value) (value.Value, bool, error) {
	if len(sa.Path) < 2 {
		return nil, false, nil
	}
	// Last segment is the method name; everything before is the type
	// path (`Adt`, `Adt[T,U]`, `pkg::Adt`, `pkg::Adt[T,U]`).
	method := sa.Path[len(sa.Path)-1]
	typeSeg := sa.Path[len(sa.Path)-2]
	// Strip any package qualifier the parser folded into the leading
	// segment, e.g. `result::Result[i64,string]` -> just `Result`.
	if idx := strings.Index(typeSeg, "::"); idx >= 0 {
		typeSeg = typeSeg[idx+2:]
	}
	// Strip the `[T,U]` type-arg suffix when present; we recover the
	// concrete args from leftVal's type instead.
	if i := strings.IndexByte(typeSeg, '['); i >= 0 {
		typeSeg = typeSeg[:i]
	}
	// Bail out if the type isn't an ADT or generic struct -- let the
	// regular pipe path handle it (e.g. user wrote `a |> mod::fn`).
	if cg.dataDeclFor(CanonKey(typeSeg)) == nil {
		if _, isGenericStruct := cg.genericStructsByArity[typeSeg]; !isGenericStruct {
			return nil, false, nil
		}
	}
	// Build a synthetic call `Adt::method(leftVal)`.  We construct a
	// throwaway Identifier node carrying a marker we can recognize
	// downstream so the call goes through the regular method-resolution
	// path; the easier route is to call cg.genMethodCall directly with
	// leftVal as the receiver and `method` as the name.
	concreteName := structNameFromValue(leftVal)
	if concreteName == "" {
		return nil, false, nil
	}

	scopeKey := concreteName + "_" + method

	entry, ok := cg.curScope.lookup(scopeKey)
	if !ok {
		return nil, false, nil
	}

	fn, ok := entry.val.(*ir.Func)
	if !ok {
		return nil, false, nil
	}

	args := cg.adaptArgs(block, []value.Value{leftVal}, fn.Sig)
	result := block.NewCall(fn, args...)

	if irtypes.IsVoid(result.Type()) {
		return nil, true, nil
	}

	return result, true, nil
}

func (cg *CodeGen) genTernaryExpr(block *ir.Block, e *ast.TernaryExpr) (value.Value, error) {
	cond, err := cg.genExpr(block, e.Cond)
	if err != nil {
		return nil, err
	}

	cond = cg.toBoolImplicit(block, cond)

	thenVal, err := cg.genExpr(block, e.Then)
	if err != nil {
		return nil, err
	}

	elseVal, err := cg.genExpr(block, e.Else)
	if err != nil {
		return nil, err
	}

	if thenVal == nil {
		thenVal = constant.NewInt(irtypes.I64, 0)
	}

	if elseVal == nil {
		elseVal = constant.NewInt(irtypes.I64, 0)
	}

	// Unify types.
	elseVal = cg.coerce(block, elseVal, thenVal.Type())

	result := block.NewSelect(cond, thenVal, elseVal)

	// ARC: both branches are evaluated eagerly before select.  If a branch
	// produces a fresh RC-tracked value (call, concat, etc.) that is not
	// selected, it must be released.  Use a second select to identify the
	// discarded value at runtime without actual conditional branching.
	// Releasing a zero-initialized fat struct is safe: extractRCDataPtr returns
	// a null ptr, and _tin_release(null) is a no-op.
	t := result.Type()
	if isRCTrackedType(t) {
		zero := cg.zeroValue(t)
		thenIsTemp := isTemporaryProducer(e.Then)
		elseIsTemp := isTemporaryProducer(e.Else)

		if thenIsTemp {
			// Release thenVal when the else branch was selected (cond == false).
			discarded := block.NewSelect(cond, zero, thenVal)
			cg.emitRelease(block, discarded)
		}

		if elseIsTemp {
			// Release elseVal when the then branch was selected (cond == true).
			discarded := block.NewSelect(cond, elseVal, zero)
			cg.emitRelease(block, discarded)
		}
	}

	return result, nil
}

func (cg *CodeGen) genIsExpr(block *ir.Block, e *ast.IsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// ADT variant is-check: `x is Ok(v)` or `x is None`. Produces an i1
	// (tag-equal) plus payload bindings into the current scope.
	if v, handled, err2 := cg.genAdtIsExpr(block, val, e); handled {
		return v, err2
	}

	// Typed is-check: "x is v T" - check the tag and optionally bind the payload.
	if st, ok := val.Type().(*irtypes.StructType); ok {
		typeName := cg.typeNameOf(val.Type())

		// Tagged union is-check: "a is i i8" where a is type u = i8 | string.
		if members, isUnion := cg.unionTypeMembers[typeName]; isUnion && e.Type != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}

			tag := int8(-1)

			for i, te := range members {
				lt, err3 := cg.tinTypeToLLVM(te)
				if err3 != nil {
					continue
				}

				if lt.Equal(targetLLVM) {
					tag = int8(i)

					break
				}
			}

			if tag < 0 {
				tag = 0
			}

			alloca := block.NewAlloca(st)
			block.NewStore(val, alloca)
			// Field 1 = i8 tag (field 0 is i32 type_id).
			tagGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			tagVal := block.NewLoad(irtypes.I8, tagGEP)

			cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I8, int64(tag)))
			if e.VarName != "" {
				// Field 2 = [N x i8] payload.
				payloadGEP := block.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := block.NewAlloca(targetLLVM)
				payloadVal := block.NewLoad(targetLLVM, payloadPtr)
				block.NewStore(payloadVal, payloadAlloca)
				// noRelease: the binding is a borrow from the union -- the union
				// owns the ARC reference.  The scope exit must not release it
				// because (a) no retain was performed and (b) in the non-match
				// path the alloca contains the union data interpreted as the
				// wrong type, so releasing it would corrupt memory.
				cg.curScope.set(e.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true, noRelease: true})
			}

			return cmp, nil
		}
	}
	// Trait-pointer downcast check: `expr is *Concrete` where expr is
	// of type `*Trait_iface`.  Compares the vtable in the iface against
	// the (Concrete, Trait) vtable global; returns i1.  Pairs with the
	// `as *Concrete` downcast so callers can guard the unsafe cast:
	//
	//   if e is *FlagError:
	//     let fe = e as *FlagError
	//     ...
	//
	// `astchecks.go:checkUnguardedTraitDowncast` warns when the `as`
	// happens without a same-type `is` guard in the enclosing block.
	if srcPt, ok := val.Type().(*irtypes.PointerType); ok {
		if traitInstKey, isTrait := cg.isTraitFatPtr(srcPt.ElemType); isTrait && e.Type != nil {
			if tt, ok2 := e.Type.(*ast.PointerType); ok2 {
				if st, ok3 := tt.Elem.(*ast.SimpleType); ok3 {
					structName := st.Name

					vtableKey := structName + "__" + traitInstKey
					if vtableGlobal, has := cg.traitVtableGlobals[vtableKey]; has {
						ifaceStructTy := srcPt.ElemType.(*irtypes.StructType)
						// `is` is supposed to be safe on a nil trait pointer.
						// A naive GEP+load would segfault; gate the load on
						// non-nil and return false for nil pointers.
						nilCheck := block.NewICmp(enum.IPredEQ, val,
							constant.NewNull(srcPt))
						// Use uniquified labels -- two `is *T` exprs in
						// the same fn would otherwise both produce blocks
						// labeled `is_load` / `is_merge`, and LLVM's phi
						// reference `%is_load` collapses to the first
						// match, silently rewiring the second `is`'s
						// incoming edge to the wrong block.
						loadBlk := cg.newBlock("is_load")
						mergeBlk := cg.newBlock("is_merge")
						block.NewCondBr(nilCheck, mergeBlk, loadBlk)

						vtableGep := loadBlk.NewGetElementPtr(ifaceStructTy, val,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
						vtablePtrType := ifaceStructTy.Fields[1]
						actual := loadBlk.NewLoad(vtablePtrType, vtableGep)
						expected := loadBlk.NewBitCast(vtableGlobal, vtablePtrType)
						eq := loadBlk.NewICmp(enum.IPredEQ, actual, expected)

						loadBlk.NewBr(mergeBlk)
						phi := mergeBlk.NewPhi(
							ir.NewIncoming(constant.NewBool(false), block),
							ir.NewIncoming(eq, loadBlk),
						)

						cg.curBlock = mergeBlk

						return phi, nil
					}
					// The struct does not implement this trait (or the
					// impl was misspelled).  A silent false would leave
					// the user's `if e is *X:` guard permanently dead;
					// report it instead.
					return nil, cg.nodeErr(e,
						"`is *%s` is unsatisfiable: struct %s does not implement the trait carried by %s",
						structName, structName, cg.tinTypeDisplay(srcPt.ElemType))
				}
			}
		}
	}

	// any type check: "x is dog" where x is any - compare type_id (field 0).
	if isAnyType(val.Type()) && e.Type != nil {
		targetName := ""

		switch t := e.Type.(type) {
		case *ast.SimpleType:
			targetName = t.Name
		}

		if targetName != "" {
			var (
				targetID int32
				found    bool
			)

			if id, ok := cg.structTypeIDs[targetName]; ok {
				targetID = id
				found = true
			}

			if found {
				anyType := anyFatPtrType()
				anyAlloca := block.NewAlloca(anyType)
				block.NewStore(val, anyAlloca)
				tagGep := block.NewGetElementPtr(anyType, anyAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				tag := block.NewLoad(irtypes.I32, tagGep)
				cmp := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I32, int64(targetID)))
				// Bind variable: extract data pointer and cast to the target type.
				if e.VarName != "" {
					targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
					if err2 == nil {
						ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
						dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
						typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(targetLLVM))
						typedVal := block.NewLoad(targetLLVM, typedPtr)
						typedAlloca := block.NewAlloca(targetLLVM)
						block.NewStore(typedVal, typedAlloca)
						cg.curScope.set(e.VarName, &scopeEntry{val: typedAlloca, isAlloc: true})
					}
				}

				return cmp, nil
			}
		}
	}
	// Fallback: just return true.

	return constant.NewInt(irtypes.I1, 1), nil
}

/// isFatArrayPtr returns true for anonymous {T*, i64} fat array pointer structs.
// Named structs (user-defined) are excluded to avoid false matches with

// fnSigName formats an LLVM FuncType as a Tin-style signature string such as
// "fn(i64,string)bool".  When skipFirstEnv is true the first parameter (the
// implicit i8* env of a fat-function-pointer) is omitted.
