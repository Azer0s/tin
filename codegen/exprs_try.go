package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genTryExpr(block *ir.Block, e *ast.TryExpr) (value.Value, error) {
	innerVal, err := cg.genExpr(block, e.Inner)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if innerVal == nil {
		return nil, cg.nodeErr(e, "try: inner expression produced no value")
	}

	// Resolve type name. For a pointer to T, peek through to T so we look up
	// methods on the pointee just like dot-call does.
	srcType := innerVal.Type()

	lookupType := srcType
	if pt, ok := lookupType.(*irtypes.PointerType); ok {
		if cg.typeNameOf(pt.ElemType) != "" {
			lookupType = pt.ElemType
		}
	}

	typeName := cg.typeNameOf(lookupType)
	if typeName == "" {
		return nil, cg.nodeErr(e, "try: cannot determine concrete type of expression (got %s); only types implementing tryable are supported", srcType)
	}

	// Allocate a stable home for the inner value so we can call three
	// methods on the same value.
	tempStorage := block.NewAlloca(srcType)
	block.NewStore(innerVal, tempStorage)

	callMethod := func(blk *ir.Block, methodName string) (*ir.Block, value.Value, error) {
		key := typeName + "_" + methodName

		var fn *ir.Func

		if entry, ok := cg.curScope.lookup(key); ok {
			var fnVal value.Value

			if entry.isAlloc {
				ptrTy := entry.val.Type().(*irtypes.PointerType)
				fnVal = blk.NewLoad(ptrTy.ElemType, entry.val)
			} else {
				fnVal = entry.val
			}

			f, ok2 := fnVal.(*ir.Func)
			if !ok2 {
				return blk, nil, cg.nodeErr(e, "try: %s is not callable", key)
			}

			fn = f
		} else {
			// Cross-package fallback: when Result is first monomorphized
			// inside a foreign package (e.g. time.tin's parse_rfc3339
			// returns Result[Instant, errors::Err]), the trait-qualified
			// -> plain-name alias for is_err / ok_value / err_value
			// registers in that package's scope, which is discarded once
			// the package finishes loading. The IR functions live on in
			// the module though, so look them up by their fully-qualified
			// IR name via dataDecls[typeName].Methods.
			if dd := cg.dataDeclFor(CanonKey(typeName)); dd != nil {
				for _, m := range dd.Methods {
					if m.Name != methodName {
						continue
					}

					irName := methodScopeName(typeName, m)
					for _, f := range cg.allFuncs() {
						if f.Name() == irName {
							fn = f

							break
						}
					}

					if fn != nil {
						break
					}
				}
			}

			if fn == nil {
				return blk, nil, cg.nodeErr(e, "try: %s does not implement tryable (missing %s)", typeName, methodName)
			}
		}

		if len(fn.Sig.Params) == 0 {
			return blk, nil, cg.nodeErr(e, "try: %s expects no receiver", key)
		}

		firstParam := fn.Sig.Params[0]

		var thisArg value.Value
		if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(srcType) {
			thisArg = tempStorage
		} else if firstParam.Equal(srcType) {
			thisArg = blk.NewLoad(srcType, tempStorage)
		} else {
			// Type mismatch on receiver: fall back to a load.
			thisArg = blk.NewLoad(srcType, tempStorage)
		}

		args := cg.adaptArgs(blk, []value.Value{thisArg}, fn.Sig)
		result := blk.NewCall(fn, args...)

		return blk, result, nil
	}

	// Call is_err.
	block, isErrVal, err := callMethod(block, "is_err")
	if err != nil {
		return nil, err
	}

	// Branch.
	parentFn := blockOwner(block)
	if parentFn == nil {
		return nil, cg.nodeErr(e, "try: not inside a function body")
	}

	// Pair the two labels with a function-local counter (`try.0.err`,
	// `try.0.ok`, `try.1.err`, ...) so two `try` sites in the same
	// fn don't collide on the same block name (LLVM IR rejects
	// duplicates with `opt: input module is broken`).  Counting
	// existing `try.` blocks in this fn keeps numbering local --
	// cg.labelCount alone would produce `try.err.500` style labels
	// pulled from a CodeGen-global counter, which is ugly and
	// shuffles when unrelated code elsewhere emits more blocks.
	tryIdx := 0

	for _, b := range parentFn.Blocks {
		if strings.HasPrefix(b.LocalName, "try.") && strings.HasSuffix(b.LocalName, ".err") {
			tryIdx++
		}
	}

	errBlock := parentFn.NewBlock(fmt.Sprintf("try.%d.err", tryIdx))
	okBlock := parentFn.NewBlock(fmt.Sprintf("try.%d.ok", tryIdx))

	// is_err returns bool which Tin lowers to i1. CondBr expects i1.
	cond := isErrVal
	if !cond.Type().Equal(irtypes.I1) {
		cond = block.NewTrunc(cond, irtypes.I1)
	}

	block.NewCondBr(cond, errBlock, okBlock)

	// Err branch: when err_value's declared return type doesn't match
	// the enclosing fn's return type, dispatch to a per-target
	// monomorphization that takes the receiver, calls the impl's
	// err_value internally, and reconstructs the result in the target
	// type. Each unique target gets its own LLVM symbol via
	// ensureWildcardMono. When the types do match (or the impl didn't
	// opt in via a wildcard bound), fall back to the direct call.
	monoTarget := irtypes.Type(nil)
	// Inside a coro-transformed async fn, curFn.Sig.RetType is the
	// continuation-frame i8*, not the user-visible Tin return type.
	// Prefer curCoroRetType (set by coro.go around the body emit) so
	// `try` against an async fn returning Result[T, Err] resolves the
	// wildcard mono against the real Result type instead of i8*.
	if cg.curCoroRetType != nil && !irtypes.IsVoid(cg.curCoroRetType) {
		monoTarget = cg.curCoroRetType
	} else if cg.curFn != nil {
		monoTarget = cg.curFn.Sig.RetType
	}

	// Release tempStorage's content before returning from the try.err
	// path.  The receiver was passed by value to err_value via a load, so
	// tempStorage's bytes still reference the same Err payload;
	// err_value's entry-retain bumped that payload's inner RC fields by
	// +1 with no balancing release on the Err-branch `return this` path,
	// so the rc would stay at +1 past return and leak.  Decrementing
	// tempStorage here balances err_value's entry retain; the returned
	// value still owns rc=1 for the caller.
	emitTempRelease := func(blk *ir.Block) {
		loaded := blk.NewLoad(srcType, tempStorage)
		cg.emitRelease(blk, loaded)
	}

	// emitPropagatingReturn finalizes the err-branch by returning retVal.
	// Inside a coro fn the LLVM signature is i8* and direct NewRet won't
	// type-check, so the value has to flow through emitCoroComplete +
	// emitFinalSuspend the same way an explicit `return X` does in
	// genCoroReturn.
	emitPropagatingReturn := func(blk *ir.Block, retVal value.Value) {
		emitTempRelease(blk)
		cg.emitAllScopeReleases(blk, "")

		if cg.inCoroFn {
			cg.emitCoroComplete(blk, retVal)
			cg.emitFinalSuspend(blk, cg.curCoroFrame)

			return
		}

		blk.NewRet(retVal)
	}

	monoFn, monoOK := cg.ensureWildcardMono(typeName, "err_value", srcType, monoTarget)
	if monoOK {
		thisArg := errBlock.NewLoad(srcType, tempStorage)
		rewrapped := errBlock.NewCall(monoFn, thisArg)
		emitPropagatingReturn(errBlock, rewrapped)
	} else {
		_, errVal, err := callMethod(errBlock, "err_value")
		if err != nil {
			return nil, err
		}

		// Tin-level diagnostic: when err_value's return type doesn't
		// match the enclosing fn's return type and we couldn't
		// monomorphize (impl didn't opt in via a wildcard slot, or
		// no payload-compatible variants), error here instead of
		// letting the LLVM verifier surface a mangled-typename
		// mismatch from the temp .ll file.
		if monoTarget != nil && !errVal.Type().Equal(monoTarget) {
			pretty := cg.diagStructName(typeName)

			return nil, cg.nodeErr(e,
				"`try`: cannot propagate %s through a function returning %s. The impl of tryable on %s did not declare a wildcard slot in its trait bound, so the success type cannot be re-bound at this call site. Add the wildcard (e.g. change the trait bound to `tryable[V, %s[_, ...]]`) or convert the value explicitly with .map / .map_err.",
				cg.fmtArgType(errVal.Type()), cg.fmtArgType(monoTarget), pretty, pretty)
		}

		emitPropagatingReturn(errBlock, errVal)
	}

	// Ok branch: call ok_value, that's the value of the try expression.
	_, okVal, err := callMethod(okBlock, "ok_value")
	if err != nil {
		return nil, err
	}

	emitTempRelease(okBlock)

	cg.curBlock = okBlock

	return okVal, nil
}

// blockOwner returns the function a block belongs to.
func blockOwner(b *ir.Block) *ir.Func { return b.Parent }

// ensureWildcardMono returns (or generates) a per-target wrapper
// function that wraps an ADT's wildcard-return method. The wrapper
// takes the same receiver as the impl method, internally invokes
// the impl's method, reconstructs the result in `target`, and
// returns. Each unique target produces a distinct LLVM symbol so
// every call site dispatches directly - no inline rewrap, no
// runtime tag-walk at the caller.
//
// Returns (monoFn, true) when monomorphization applies: the impl
// declared a wildcard slot in its trait bound and the inner/target
// types share at least one payload-compatible variant.
// Returns (nil, false) when the caller should fall back to the
// direct (un-rewrapped) call.
//
// The wrapper itself uses the existing rewrapTryable for the
// reconstruction codegen, so the variant-walk logic stays in one
// place. Future evolution (per the call-site-generics design): the
// wrapper body becomes the impl's body re-typed under the wildcard
// substitution, with the rewrap inserted automatically by the
// return-statement coerce path. The wrapper-around-impl form is the
// migration step.
func (cg *CodeGen) ensureWildcardMono(typeName, methodName string, srcType, target irtypes.Type) (*ir.Func, bool) {
	if target == nil {
		return nil, false
	}

	if srcType.Equal(target) {
		return nil, false
	}

	if !cg.adtImplHasWildcardBound(typeName, "tryable") {
		return nil, false
	}

	targetName := cg.typeNameOf(target)
	if targetName == "" {
		return nil, false
	}

	monoName := typeName + "_" + methodName + "__W_" + targetName
	if fn, ok := cg.wildcardMonos[monoName]; ok {
		return fn, true
	}

	origKey := typeName + "_" + methodName

	var origFn *ir.Func

	if origEntry, ok := cg.curScope.lookup(origKey); ok {
		fn, ok2 := origEntry.val.(*ir.Func)
		if !ok2 {
			return nil, false
		}

		origFn = fn
	} else {
		// Cross-package fallback: same situation as callMethod's
		// fallback above - the plain alias died with the foreign
		// package's scope, but the impl's IR func survives in the
		// module under its trait-qualified name.
		if dd := cg.dataDeclFor(CanonKey(typeName)); dd != nil {
			for _, m := range dd.Methods {
				if m.Name != methodName {
					continue
				}

				irName := methodScopeName(typeName, m)
				for _, f := range cg.allFuncs() {
					if f.Name() == irName {
						origFn = f

						break
					}
				}

				if origFn != nil {
					break
				}
			}
		}

		if origFn == nil {
			return nil, false
		}
	}

	// Build the wrapper: takes the impl's receiver, returns target.
	monoFn := cg.activeModule().NewFunc(monoName, target, ir.NewParam("this", srcType))
	cg.wildcardMonos[monoName] = monoFn

	prevBlock := cg.curBlock

	defer func() { cg.curBlock = prevBlock }()

	entry := monoFn.NewBlock("entry")

	// Adapt receiver if the impl method uses a pointer receiver.
	args := cg.adaptArgs(entry, []value.Value{monoFn.Params[0]}, origFn.Sig)
	innerVal := entry.NewCall(origFn, args...)

	rewrapped, joinBlock, rewrapOK := cg.rewrapTryable(entry, innerVal, target)
	if !rewrapOK {
		// No compatible overlap. Drop the half-built mono so callers
		// fall back to the direct path; the LLVM verifier flags the
		// type mismatch on the original call.
		entry.NewUnreachable()

		delete(cg.wildcardMonos, monoName)

		return nil, false
	}

	joinBlock.NewRet(rewrapped)

	return monoFn, true
}

// adtImplHasWildcardBound reports whether adtName declares an impl of
// the named trait whose bound contains at least one WildcardType slot.
// Used to gate cross-T rewrap on the impl actually opting in via the
// partial-bound syntax (see masterplan: "call-site generics").
//
// Both the concrete monomorphization and the template are checked.
// Monomorphization substitutes wildcards out of the concrete decl's
// Implements list, so the template is the source of truth for whether
// a wildcard was ever present in the impl bound.
// emitAwaitableLoop lowers `await x` where x is a non-Future awaitable
// to a runtime-driven spin loop:
//
//	loop:
//	  if x.ready(): goto done
//	  yield
//	done:
//	  x.result()
//
// The runtime owns the spin/yield, so user impls of awaitable[t]
// only have to answer "ready?" and "result". `yield` lowers to a
// no-op outside an {#async} body, so `await` from synchronous code
// turns into a busy-wait - which matches the legacy await_result
// behavior for direct (non-fiber) callers.
func (cg *CodeGen) emitAwaitableLoop(block *ir.Block, val value.Value, readyFn, resultFn *ir.Func, releaseVal bool) (value.Value, error) {
	condBlk := cg.newBlock("await.cond")
	bodyBlk := cg.newBlock("await.body")
	doneBlk := cg.newBlock("await.done")

	block.NewBr(condBlk)

	readyArgs := cg.adaptArgs(condBlk, []value.Value{val}, readyFn.Sig)
	readyVal := condBlk.NewCall(readyFn, readyArgs...)
	condBlk.NewCondBr(readyVal, doneBlk, bodyBlk)

	cg.curBlock = bodyBlk

	yieldedBlk, yieldErr := cg.genYieldStmt(bodyBlk)
	if yieldErr != nil {
		return nil, yieldErr
	}

	if yieldedBlk == nil {
		yieldedBlk = bodyBlk
	}

	yieldedBlk.NewBr(condBlk)

	resultArgs := cg.adaptArgs(doneBlk, []value.Value{val}, resultFn.Sig)
	res := doneBlk.NewCall(resultFn, resultArgs...)
	cg.curBlock = doneBlk

	// Release the awaitable struct value once its result has been
	// extracted -- but only when the value was a temp at the await
	// site (e.g. `await m.lock()`, where Mutex.lock()'s LockHandle
	// has no other owner).  For `await h` shapes the caller's
	// h binding still owns the awaitable and will release it on
	// scope exit; releasing here too would double-decrement the
	// underlying rc::Cell.
	if releaseVal && cg.elemNeedsRelease(val.Type()) {
		cg.emitRelease(doneBlk, val)
	}

	return res, nil
}

func (cg *CodeGen) adtImplHasWildcardBound(adtName, traitBaseName string) bool {
	decls := []*ast.DataDecl{}

	if d := cg.dataDeclFor(CanonKey(adtName)); d != nil {
		decls = append(decls, d)
	}

	if idx := strings.Index(adtName, "__"); idx > 0 {
		if d := cg.dataDeclFor(CanonKey(adtName[:idx])); d != nil {
			decls = append(decls, d)
		}
	}

	if len(decls) == 0 {
		return false
	}

	for _, decl := range decls {
		for _, impl := range decl.Implements {
			if traitBaseImplName(impl) != traitBaseName {
				continue
			}

			if typeExprContainsWildcard(impl) {
				return true
			}
		}
	}

	return false
}

// traitBaseImplName extracts a trait's bare name from a trait-impl
// type expression. Mirrors traitBaseName but defined locally to avoid
// circular dependencies between exprs.go and decls.go for this usage.
func traitBaseImplName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	case *ast.GenericType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	}

	return ""
}

// typeExprContainsWildcard returns true if te is or contains a
// WildcardType anywhere in its tree.
func typeExprContainsWildcard(te ast.TypeExpr) bool {
	switch t := te.(type) {
	case *ast.WildcardType:
		return true
	case *ast.GenericType:
		for _, p := range t.TypeParams {
			if typeExprContainsWildcard(p) {
				return true
			}
		}
	case *ast.PointerType:
		return typeExprContainsWildcard(t.Elem)
	case *ast.ArrayType:
		return typeExprContainsWildcard(t.Elem)
	case *ast.UnionTypeExpr:
		for _, ut := range t.Types {
			if typeExprContainsWildcard(ut) {
				return true
			}
		}
	}

	return false
}

// rewrapTryable handles cross-T propagation for the `try` keyword
// - generically across any ADT pair where matching-named variants have
// compatible payload layouts. The keyword's err branch produces a value
// whose static type may not equal the enclosing fn's return type
// because the impl declared `err_value` with a wildcard slot
// (`Result[_, E]`); the runtime data is byte-compatible with the target
// when the active variant has a payload that doesn't depend on the
// wildcard slot.
//
// Strategy: dispatch on inner's runtime tag. For each tag:
//   - if target has a same-named variant whose payload type matches,
//     extract the inner's fields and reconstruct in target;
//   - otherwise panic - at runtime that branch is unreachable when
//     the impl's is_err honestly filters only payload-compatible
//     variants into the err branch (Result.Ok / Option.Some never
//     reach here because is_err returned false).
//
// Returns (rewrapped, joinBlock, true) when both ADTs share at least
// one payload-compatible variant; (nil, nil, false) when no overlap
// exists or either side isn't a registered ADT, so callers fall
// through to the direct-return path. joinBlock is where the caller
// should emit follow-up terminator instructions.
func (cg *CodeGen) rewrapTryable(block *ir.Block, inner value.Value, target irtypes.Type) (value.Value, *ir.Block, bool) {
	innerName := cg.typeNameOf(inner.Type())
	targetName := cg.typeNameOf(target)

	if innerName == "" || targetName == "" {
		return nil, nil, false
	}

	innerVariants := cg.dataVariants[innerName]
	targetVariants := cg.dataVariants[targetName]

	if innerVariants == nil || targetVariants == nil {
		return nil, nil, false
	}

	// Cross-type rewrap is only safe when the impl explicitly opted in
	// via a wildcard-bearing trait bound (e.g. `tryable[V, Result[_, E]]`).
	// Without the wildcard, the impl is committing to its concrete
	// container shape and rewrapping silently would coerce types the
	// user did not authorize. This gates "call-site generics" on the
	// declaration site spelling out the partial slot.
	if !cg.adtImplHasWildcardBound(innerName, "tryable") {
		return nil, nil, false
	}

	// Find variants that exist in both with payload-compatible shape.
	// Variants present only in inner, or whose payload differs, become
	// "panic" arms - runtime is supposed to never hit them since the
	// impl's is_err already filtered.
	type rewrapArm struct {
		name      string
		innerInfo *dataVariantInfo
		targetVI  *dataVariantInfo
	}

	var compatibleArms []rewrapArm

	for innerVName, innerVI := range innerVariants {
		targetVI, ok := targetVariants[innerVName]
		if !ok {
			continue
		}

		if len(innerVI.Fields) != len(targetVI.Fields) {
			continue
		}

		if !innerVI.PayloadType.Equal(targetVI.PayloadType) {
			continue
		}

		compatibleArms = append(compatibleArms, rewrapArm{
			name: innerVName, innerInfo: innerVI, targetVI: targetVI,
		})
	}

	if len(compatibleArms) == 0 {
		return nil, nil, false
	}

	// Stable order for deterministic IR output.
	sort.Slice(compatibleArms, func(i, j int) bool {
		return compatibleArms[i].innerInfo.Tag < compatibleArms[j].innerInfo.Tag
	})

	innerOuterSt := cg.structTypeFor(CanonKey(innerName))
	if innerOuterSt == nil {
		return nil, nil, false
	}

	parentFn := blockOwner(block)
	if parentFn == nil {
		return nil, nil, false
	}

	innerStorage := block.NewAlloca(inner.Type())
	block.NewStore(inner, innerStorage)

	tagGEP := block.NewGetElementPtr(innerOuterSt, innerStorage,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := block.NewLoad(irtypes.I64, tagGEP)

	// joinBlock collects each arm's rewrapped value via a phi.
	joinBlock := parentFn.NewBlock("try.rewrap.join")
	panicBlock := parentFn.NewBlock("try.rewrap.unreachable")
	panicBlock.NewUnreachable()

	var (
		switchCases []*ir.Case
		incomings   []*ir.Incoming
	)

	for _, arm := range compatibleArms {
		armBlock := parentFn.NewBlock("try.rewrap." + arm.name)

		args := make([]value.Value, 0, len(arm.innerInfo.Fields))

		if len(arm.innerInfo.Fields) > 0 {
			payloadGEP := armBlock.NewGetElementPtr(innerOuterSt, innerStorage,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
			payloadPtr := armBlock.NewBitCast(payloadGEP, irtypes.NewPointer(arm.innerInfo.PayloadType))

			for i := range arm.innerInfo.Fields {
				fieldPtr := armBlock.NewGetElementPtr(arm.innerInfo.PayloadType, payloadPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
				fieldVal := armBlock.NewLoad(arm.innerInfo.PayloadType.Fields[i], fieldPtr)

				args = append(args, fieldVal)
			}
		}

		rewrapped, err := cg.wrapDataVariant(armBlock, targetName, arm.name, args, nil)
		if err != nil {
			return nil, nil, false
		}

		armBlock.NewBr(joinBlock)

		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, arm.innerInfo.Tag), armBlock))
		incomings = append(incomings, ir.NewIncoming(rewrapped, armBlock))
	}

	block.NewSwitch(tagI64, panicBlock, switchCases...)

	phi := joinBlock.NewPhi(incomings...)

	return phi, joinBlock, true
}

// Expression generation

// genExpr generates code for an expression and returns the resulting value.
//
// Contract: on return, cg.curBlock points to the block where the next
// instruction should be emitted. If the expression contains control flow
// (await / yield / short-circuit && / ||) it may differ from `block`.
// Callers that emit follow-up instructions (toBool, NewCondBr, NewStore,
// etc.) must use cg.curBlock, not the input `block`.
