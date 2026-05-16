package codegen

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/parser"
)

// genTryExpr lowers `try expr` into:
//
//	let __t = expr
//	if __t.is_err():
//	  return __t.err_value()
//	__t.ok_value()
//
// The methods are looked up by the plain alias the impl registered
// (typename + "_" + method). Trait-qualified bodies are reachable
// through that alias for any type implementing tryable, since
// registerPlainMethodAliases surfaces them under the bare name.
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
			if dd, ok := cg.dataDecls[typeName]; ok {
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
	if cg.curFn != nil {
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

	monoFn, monoOK := cg.ensureWildcardMono(typeName, "err_value", srcType, monoTarget)
	if monoOK {
		thisArg := errBlock.NewLoad(srcType, tempStorage)
		rewrapped := errBlock.NewCall(monoFn, thisArg)
		emitTempRelease(errBlock)
		// Run scope releases before the propagating return.  The
		// surrounding function's RC locals (string params, closure
		// envs, etc.) need to be cleaned up exactly as on a normal
		// `return <expr>` path; skipping them here was leaking those
		// allocations on any `try expr` early return.
		cg.emitAllScopeReleases(errBlock, "")
		errBlock.NewRet(rewrapped)
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
			pretty := prettyStructName(typeName)

			return nil, cg.nodeErr(e,
				"`try`: cannot propagate %s through a function returning %s. The impl of tryable on %s did not declare a wildcard slot in its trait bound, so the success type cannot be re-bound at this call site. Add the wildcard (e.g. change the trait bound to `tryable[V, %s[_, ...]]`) or convert the value explicitly with .map / .map_err.",
				fmtArgType(errVal.Type()), fmtArgType(monoTarget), pretty, pretty)
		}

		emitTempRelease(errBlock)
		cg.emitAllScopeReleases(errBlock, "")
		errBlock.NewRet(errVal)
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
		if dd, ok := cg.dataDecls[typeName]; ok {
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

	if d := cg.dataDecls[adtName]; d != nil {
		decls = append(decls, d)
	}

	if idx := strings.Index(adtName, "__"); idx > 0 {
		if d := cg.dataDecls[adtName[:idx]]; d != nil {
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

	innerOuterSt := cg.structTypes[innerName]
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
func (cg *CodeGen) genExpr(block *ir.Block, node ast.Node) (value.Value, error) {
	if node == nil {
		return nil, nil
	}

	// Establish the post-call invariant: if the expression doesn't advance
	// control flow, cg.curBlock equals the input block on return; if it
	// does (await/yield/&&/||), the handler updates it.
	cg.curBlock = block

	// Track source position for error messages produced deeper in the call stack.
	if p := node.Pos(); p.Line != 0 {
		cg.currentPos = p
	}

	switch e := node.(type) {
	case *ast.IntLit:
		if e.Big != nil {
			// 128 bits = at most u128 range; values above that don't fit
			// either i128 or u128 and would silently wrap inside LLVM.
			if e.Big.BitLen() > 128 {
				return nil, cg.nodeErr(e,
					"integer literal %s exceeds i128/u128 range; use a string-based bignum library for larger values",
					e.Big.String())
			}

			return &constant.Int{Typ: irtypes.I128, X: new(big.Int).Set(e.Big)}, nil
		}

		return constant.NewInt(irtypes.I64, e.Value), nil

	case *ast.FloatLit:
		return constant.NewFloat(irtypes.Double, e.Value), nil

	case *ast.BoolLit:
		if e.Value {
			return constant.NewInt(irtypes.I1, 1), nil
		}

		return constant.NewInt(irtypes.I1, 0), nil

	case *ast.CharLit:
		return constant.NewInt(irtypes.I8, int64(e.Value)), nil

	case *ast.NilLit:
		return constant.NewNull(irtypes.I8Ptr), nil

	case *ast.AtomLit:
		// Emit atom as %__atom { i32 CRC32(name) } constant.
		return cg.atomConstant(cg.registerAtom(e.Name)), nil

	case *ast.StringLit:
		return cg.buildStringFatPtr(block, e.Value), nil

	case *ast.BacktickLit:
		// Backtick literal: compile as string with backtick delimiters.
		// If the content contains {expr} interpolations (used in CTFE macro bodies),
		// expand them so that variable values are substituted at runtime.
		// In non-CTFE macro context the expander unwraps this before codegen (see expandMacro).
		if strings.Contains(e.Content, "{") {
			node, err := parser.ParseStringInterp(e.Content)
			if err == nil {
				if interp, ok := node.(*ast.InterpolatedString); ok {
					// Wrap interpolated parts with backtick delimiters.
					parts := make([]ast.StringPart, 0, len(interp.Parts)+2)
					parts = append(parts, ast.StringPart{Str: "`"})
					parts = append(parts, interp.Parts...)
					parts = append(parts, ast.StringPart{Str: "`"})

					return cg.genInterpolatedString(block, &ast.InterpolatedString{Parts: parts})
				}
			}
		}

		return cg.buildStringFatPtr(block, "`"+e.Content+"`"), nil

	case *ast.InterpolatedString:
		return cg.genInterpolatedString(block, e)

	case *ast.Identifier:
		return cg.genIdentifier(block, e)

	case *ast.BinExpr:
		return cg.genBinExpr(block, e)

	case *ast.UnaryExpr:
		return cg.genUnaryExpr(block, e)

	case *ast.CallExpr:
		return cg.genCallExpr(block, e)

	case *ast.FieldAccess:
		return cg.genFieldAccess(block, e)

	case *ast.IndexExpr:
		return cg.genIndexExpr(block, e)

	case *ast.ScopeAccess:
		return cg.genScopeAccess(block, e)

	case *ast.ArrayLit:
		return cg.genArrayLit(block, e)

	case *ast.ArrayFillLit:
		return cg.genArrayFillLit(block, e)

	case *ast.StructLit:
		return cg.genStructLit(block, e)

	case *ast.TupleLit:
		// Pass the current returnTypeHint as the expected type so
		// elements can disambiguate ADT constructors against the
		// tuple's element types (set by genArgWithTargetType,
		// let-bindings with annotation, etc.).
		return cg.genTupleLit(block, e, cg.returnTypeHint)

	case *ast.SliceExpr:
		return cg.genSliceExpr(block, e)

	case *ast.AsExpr:
		return cg.genAsExpr(block, e)

	case *ast.AddrExpr:
		return cg.genAddrExpr(block, e)

	case *ast.AddressOfExpr:
		return cg.genAddrOfExpr(block, e)

	case *ast.DerefExpr:
		return cg.genDerefExpr(block, e)

	case *ast.PipeExpr:
		return cg.genPipeExpr(block, e)

	case *ast.TernaryExpr:
		return cg.genTernaryExpr(block, e)

	case *ast.IsExpr:
		return cg.genIsExpr(block, e)

	case *ast.RangeExpr:
		// RangeExpr in expression context returns start value.
		return cg.genExpr(block, e.Start)

	case *ast.LambdaExpr:
		return cg.genLambdaExpr(block, e)

	case *ast.SpawnExpr:
		return cg.genSpawnExpr(block, e)

	case *ast.TryExpr:
		return cg.genTryExpr(block, e)

	case *ast.AwaitExpr:
		// await expr -- evaluates e.Future, which must implement awaitable[t]
		// (typically `Future[t]`).  Strict: the operand's type must be an
		// awaitable; there is no auto-spawn-under-await sugar.  To wait on a
		// user-declared `fn{#async} f() T`, write `await spawn f(args)` --
		// `spawn` produces `Future[T]` and the await unwraps to T.
		//
		// Optimisations that fire under coro context (cg.inCoroFn == true):
		//   - Channel fast path: `await ch.recv()` / `await ch.send(v)` emit
		//     the inline channel direct op, bypassing the sync wrapper and
		//     its spawn.
		//   - Inline-drive: `await spawn fn(args)` where fn has a `$coro`
		//     variant drives the inner coroutine in the caller's own frame
		//     instead of allocating a fresh fiber.
		futureExpr := e.Future
		if cg.inCoroFn {
			// Channel fast path: `await ch.recv()` / `await ch.send(v)`
			// short-circuit the outer sync wrapper (which returns a
			// `Future[T]` constructed via `spawn this.{recv,send}_impl`)
			// and emit the inline channel op directly, returning T to
			// the caller's coro frame.
			if callNode, ok := e.Future.(*ast.CallExpr); ok {
				if result, ok2, driveErr := cg.tryChannelWrapperFastPath(block, callNode); ok2 {
					if driveErr != nil {
						return nil, driveErr
					}

					return result, nil
				}
			}

			// Inline-drive: `await spawn fn(args)` runs the inner coroutine
			// in this fiber's own frame without allocating a fresh fiber.
			// Gated on cg.stacktraceUsed: when the program reaches
			// stacktrace(), inline-drive collapses two spawn-chain levels
			// into one and breaks the parent-gen walk -- so we use the
			// real spawn path (which captures caller IP + parent pid+gen
			// at the spawn site) whenever stacktrace is observable.  In
			// stacktrace-free programs the optimisation is sound and
			// saves the fiber-frame allocation + scheduler handoff.
			if !cg.stacktraceUsed {
				if spawnNode, ok := e.Future.(*ast.SpawnExpr); ok {
					if callNode, ok2 := spawnNode.Call.(*ast.CallExpr); ok2 {
						result, driveErr := cg.genInlineAsyncDrive(block, callNode)
						if driveErr != nil {
							return nil, driveErr
						}

						if result != nil {
							return result, nil
						}
						// (nil, nil) -> callee $coro not in scope; fall through
						// to the standard spawn+await path below.
					}
				}
			}
		}

		val, err := cg.genExpr(block, futureExpr)
		if err != nil {
			return nil, err
		}

		if val == nil {
			return nil, cg.nodeErr(e, "await: expression produced no value")
		}
		// Refresh block in case evaluating the future expression advanced the IR
		// insertion point (e.g. `await spawn fn(await spawn other())` where the
		// inner await moved to a new block via cg.curBlock signaling).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		// Verify the value is a Future[T] struct and extract its PID + result type.
		structName := structNameFromValue(val)
		if structName == "" {
			if val.Type().Equal(irtypes.I64) {
				if cg.syncLoadErr != nil {
					return nil, fmt.Errorf("await: sync package failed to load so spawn returned a raw pid.\n"+
						"  Ensure the tin executable is alongside the stdlib/ directory.\n"+
						"  Load error: %w", cg.syncLoadErr)
				}

				return nil, cg.nodeErr(e, "await: expression is a raw i64, not a Future[t]; use \"await spawn fn(args)\" which returns Future[t]")
			}

			return nil, cg.nodeErr(e, "await: expression (type %s) does not implement awaitable[t]; use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] (e.g. fn f() Future[t] = spawn ...)",
				val.Type())
		}

		// The value must be a Future[T] struct.  Extract .pid field (field index 0).
		pidIdx := cg.fieldIndex(structName, "pid")
		if pidIdx < 0 {
			// Not a Future struct: lower `await x` against the
			// awaitable[t] trait as
			//
			//   loop:
			//     if x.ready(): break
			//     yield
			//   x.result()
			//
			// The runtime drives the spin loop, so user impls
			// only have to answer "ready?" and "result". If
			// either method is missing report which one - the
			// most common cause is forgetting to migrate from
			// the old single-method shape.
			readyName := structName + "_ready"
			resultName := structName + "_result"

			readyEntry, hasReady := cg.curScope.lookup(readyName)
			resultEntry, hasResult := cg.curScope.lookup(resultName)

			if hasReady && hasResult {
				if readyFn, rOk := readyEntry.val.(*ir.Func); rOk {
					if resultFn, sOk := resultEntry.val.(*ir.Func); sOk {
						// `await <name>` (Identifier / DerefExpr of a
						// named pointer) is a borrow of the outer scope's
						// binding -- the original binding's scope exit
						// will release it.  Only release inside the loop
						// when the futureExpr is a temp producer
						// (CallExpr, BinExpr concat, etc.) and no other
						// owner exists.
						release := !isCopyExpr(futureExpr)

						return cg.emitAwaitableLoop(block, val, readyFn, resultFn, release)
					}
				}
			}

			return nil, cg.nodeErr(e, "await: expression (type %q) does not implement awaitable[t] (need fn awaitable::ready and fn awaitable::result); use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] directly", structName)
		}

		// Extract pid from Future[T] using extractvalue (no alloca -> safe inside loops).
		cg.ensureFiberRuntime()

		pid := block.NewExtractValue(val, uint64(pidIdx))

		// Properly suspend the calling fiber (or block main) until pid completes.
		resumeBlk, awaitErr := cg.genAwaitStmt(block, pid)
		if awaitErr != nil {
			return nil, awaitErr
		}

		if resumeBlk != nil {
			block = resumeBlk
			cg.curBlock = block
		}

		// Check whether the awaited fiber panicked.
		// We emit the _tin_panic call inline (not inside a C helper) so that
		// the panic unwinds in the calling Tin function's context - making it
		// catchable via defer + recover() in that function.
		//
		// Emitted IR pattern:
		//   %pmsg = call i8* @_tin_fiber_get_panic_msg(pid)
		//   %panicked = icmp ne i8* %pmsg, null
		//   br i1 %panicked, label %await.panic, label %await.ok
		// await.panic:
		//   call void @_tin_panic(i8* %pmsg)
		//   ret <zero>     ; if recovered by defer, return zero value
		// await.ok:
		//   ... get and unbox result ...
		pmsg := block.NewCall(cg.fiberGetPanicMsgFn, pid)
		panicked := block.NewICmp(enum.IPredNE, pmsg, constant.NewNull(irtypes.I8Ptr))
		panicBlk := cg.newBlock("await.panic")
		okBlk := cg.newBlock("await.ok")
		block.NewCondBr(panicked, panicBlk, okBlk)

		// Panic block: call _tin_panic then emit a valid terminator.
		// Inside a coroutine body we must use the coro completion path so that
		// _tin_fiber_complete is called and llvm.coro.end sees a valid IR shape.
		// (A bare ret in a presplit coro body bypasses coro.end and leaves the
		// frame in an undefined state.)  This mirrors the fix in genBuiltinPanic.
		panicBlk.NewCall(cg.ensurePanicFn(), pmsg)
		// Do NOT release pmsg here.  _tin_fiber_get_panic_msg retained it for the
		// caller, and the defer thunk balances that retain: either the thunk
		// releases the discarded recover() result directly (consuming the retain),
		// or it retains pmsg for a captured variable (e.g. "caught = msg").  In
		// the latter case emitAllScopeReleases below releases the captured variable,
		// which decrements the same ref.  Adding an explicit release here would
		// cause a double-free for the discard pattern.

		if cg.inCoroFn {
			cg.ensureFiberRuntime()
			// If _tin_panic returns (panic was caught by defer+recover in this
			// coro), complete with the defer-override value if a thunk set one,
			// otherwise the zero value of the declared return type.  Passing nil
			// would leave the fiber result as NULL, causing a null-pointer
			// dereference in the outer awaiter's okBlk.
			cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
			cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)
		} else {
			// Release all ARC-tracked scope variables.  The defer thunk has
			// already run via _tin_panic; any variable updated by the thunk
			// (e.g. "caught = msg") now holds an extra ARC reference that must
			// be released before the function returns.  This mirrors the
			// emitAllScopeReleases call in the normal return path.
			cg.emitAllScopeReleases(panicBlk, "")
			// Free any malloc'd defer closure envs.  _tin_panic already called
			// the thunks via the runtime defer chain; only the env allocations
			// remain.  This mirrors emitDefers' env-free loop on the normal path.
			freeFn := cg.ensureFree()
			for i := len(cg.pendingDeferEnvs) - 1; i >= 0; i-- {
				env := cg.pendingDeferEnvs[i]
				if _, isNull := env.(*constant.Null); !isNull {
					panicBlk.NewCall(freeFn, env)
				}
			}

			retType := cg.curFn.Sig.RetType
			if irtypes.IsVoid(retType) {
				panicBlk.NewRet(nil)
			} else {
				panicBlk.NewRet(cg.zeroValue(retType))
			}
		}

		block = okBlk
		cg.curBlock = block

		// Determine the Future's type parameter T so we can unbox the result.
		// Future__i64 -> retType=i64; Future__Unit -> retType=Unit(void).
		retTypeName := ""
		if len(structName) > 8 && structName[:8] == "Future__" {
			retTypeName = structName[8:]
		}

		if retTypeName == "" || retTypeName == "Unit" {
			// void result - return a sentinel i1 true so callers don't see nil.
			return constant.NewInt(irtypes.I1, 1), nil
		}

		// Use parseTypeParamStr so that pointer-type params like "*my_val" (from
		// Future__*my_val) resolve to the correct LLVM pointer type instead of i64.
		retLLVM, resolveErr := cg.tinTypeToLLVM(parseTypeParamStr(retTypeName))
		if resolveErr != nil || retLLVM == nil || irtypes.IsVoid(retLLVM) {
			return constant.NewInt(irtypes.I1, 1), nil
		}

		// Get the boxed result pointer, unbox it, then free the heap buffer.
		// _tin_fiber_get_result transfers ownership of the malloc'd result box
		// to the caller; the caller must free it after loading the value.
		rawPtr := block.NewCall(cg.fiberGetResultFn, pid)
		typedPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(retLLVM))
		result := block.NewLoad(retLLVM, typedPtr)
		block.NewCall(cg.ensureFree(), rawPtr)
		cg.curBlock = block

		return result, nil

	case *ast.YieldStmt:
		// yield used in expression context (e.g., let _ = yield): treat as statement.
		newBlk, err := cg.genYieldStmt(block)
		if err != nil {
			return nil, err
		}

		cg.curBlock = newBlk

		return constant.NewInt(irtypes.I1, 0), nil

	case *ast.WildcardExpr:
		return constant.NewInt(irtypes.I1, 1), nil

	case *ast.DefaultExpr:
		if e.OfExpr != nil {
			// default(typeof(expr)): get LLVM type of inner expression, return zero for it.
			// e.OfExpr is the TypeofExpr node; we evaluate its inner Expr to get the type.
			inner := e.OfExpr
			if te, ok := inner.(*ast.TypeofExpr); ok {
				inner = te.Expr
			}

			val, err := cg.genExpr(block, inner)
			if err != nil {
				return nil, err
			}

			if val != nil {
				return cg.zeroValue(val.Type()), nil
			}
		}

		if e.Type != nil {
			lt, err := cg.tinTypeToLLVM(e.Type)
			if err != nil {
				return nil, err
			}

			return cg.zeroValue(lt), nil
		}

		return constant.NewInt(irtypes.I64, 0), nil

	case *ast.Block:
		// Block expression: (stmt1; stmt2; ...; last_expr) - produced by CTFE macro splices.
		// Generate all statements and return the value of the last expression.
		// A new scope is pushed so let bindings do not leak into the outer function scope.
		curBlock := block

		cg.curScope = newScope(cg.curScope)

		var lastVal value.Value = constant.NewInt(irtypes.I64, 0)

		for i, stmt := range e.Stmts {
			isLast := i == len(e.Stmts)-1
			if isLast {
				if es, ok := stmt.(*ast.ExprStmt); ok {
					v, err := cg.genExpr(curBlock, es.Expr)
					if err != nil {
						return nil, err
					}

					if v != nil {
						lastVal = v
					}

					continue
				}
			}

			newBlock, _, err := cg.genStmt(curBlock, stmt)
			if err != nil {
				return nil, err
			}

			if newBlock != nil {
				curBlock = newBlock
			}
		}

		cg.emitScopeRelease(curBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		return lastVal, nil

	case *ast.SizeofExpr:
		if e.Type == nil {
			return constant.NewInt(irtypes.I64, 0), nil
		}

		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}

		if irtypes.IsVoid(lt) {
			return constant.NewInt(irtypes.I64, 0), nil
		}
		// GEP trick: sizeof(T) = (i64) &((T*)null)[1]
		nullPtr := constant.NewNull(irtypes.NewPointer(lt))
		gepOne := block.NewGetElementPtr(lt, nullPtr, constant.NewInt(irtypes.I32, 1))

		return block.NewPtrToInt(gepOne, irtypes.I64), nil

	case *ast.IsRCExpr:
		// Compile-time RC kind for T. Encodes both whether T needs ARC
		// management and where in T's bytes the retainable pointer sits, so
		// the C runtime (Channel, Atomic) can dispatch without knowing the
		// Tin type.
		//
		//   0 = not RC
		//   1 = leading pointer at offset 0 (string, fat array, trait fat ptr)
		//   2 = any: {i32 tag, i8* ptr} -- ptr at offset 8, release with
		//       _tin_release_any so closure-typed `any` values free their env
		//   3 = fn fat ptr: {fn*, env*} -- env at offset 8, release with
		//       _tin_release_closure
		if e.Type == nil {
			return constant.NewInt(irtypes.I32, int64(rcKindNone)), nil
		}

		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}

		return constant.NewInt(irtypes.I32, int64(channelRCKindOf(lt))), nil

	case *ast.TypeAssertExpr:
		inner, err := cg.genExpr(block, e.Expr)
		if err != nil || inner == nil || e.Type == nil {
			return inner, err
		}
		// Native union type cast: b.(string) - bitcast storage to target type.
		innerName := cg.typeNameOf(inner.Type())
		if _, isNative := cg.nativeUnionDecls[innerName]; isNative {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}

			st := inner.Type().(*irtypes.StructType)
			alloca := block.NewAlloca(st)
			block.NewStore(inner, alloca)
			storageGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			memberPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(targetLLVM))

			return block.NewLoad(targetLLVM, memberPtr), nil
		}
		// Pointer type cast: p.(*T) bitcasts between pointer types (e.g. *void -> *i64).
		if irtypes.IsPointer(inner.Type()) {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 == nil && irtypes.IsPointer(targetLLVM) && targetLLVM != inner.Type() {
				return block.NewBitCast(inner, targetLLVM), nil
			}
		}

		return inner, nil

	case *ast.TypeofExpr:
		return cg.genTypeof(block, e)

	case *ast.TraitofExpr:
		return cg.genTraitof(block, e)

	case *ast.FieldnamesExpr:
		return cg.genFieldnames(block, e)

	case *ast.FieldtypesExpr:
		return cg.genFieldtypes(block, e)

	case *ast.FieldtagExpr:
		return cg.genFieldtag(block, e)

	case *ast.GetfieldExpr:
		return cg.genGetfield(block, e)

	case *ast.SetfieldExpr:
		return cg.genSetfield(block, e)

	case *ast.VarDecl:
		_, err := cg.genVarDecl(block, e)
		if err != nil {
			return nil, err
		}
		// Return the alloca'd value.
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, nil
		}

		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)

			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}

		return entry.val, nil

	case *ast.MatchStmt:
		return cg.genMatchAsExpr(block, e)

	default:
		return nil, nil
	}
}

// armExprNode returns the expression node from a single-statement arm body.
// It handles both *ast.ExprStmt (bare expression) and *ast.MatchStmt (nested
// match expression used as arm value). Returns nil for anything else.
func armExprNode(stmt ast.Node) ast.Node {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return s.Expr
	case *ast.MatchStmt:
		return s // genExpr handles *ast.MatchStmt directly
	}

	return nil
}

// astInferTypeWithPattern infers the type of node like astInferType but first
// pushes a temporary scope that maps pattern-bound names to their field types,
// so that renamed bindings (e.g. "x: px") are visible when node is "px".
func (cg *CodeGen) astInferTypeWithPattern(node ast.Node, pattern ast.Node) irtypes.Type {
	sp, ok := pattern.(*ast.StructPattern)
	if !ok {
		return cg.astInferType(node)
	}

	// Collect bindings: field name -> LLVM field type from the struct.
	bindings := map[string]irtypes.Type{}
	cg.collectPatternBindingTypes(sp, bindings)

	if len(bindings) == 0 {
		return cg.astInferType(node)
	}

	// Push a temporary scope with those bindings as non-alloc entries.
	cg.curScope = newScope(cg.curScope)

	for varName, llvmType := range bindings {
		cg.curScope.set(varName, &scopeEntry{val: &syntheticValue{t: llvmType}})
	}

	t := cg.astInferType(node)
	cg.curScope = cg.curScope.parent

	return t
}

// collectPatternBindingTypes walks a StructPattern and fills bindings with the
// LLVM type for each free or renamed field, recursing into nested patterns.
func (cg *CodeGen) collectPatternBindingTypes(sp *ast.StructPattern, bindings map[string]irtypes.Type) {
	llvmType, ok := cg.structTypes[sp.TypeName]
	if !ok {
		return
	}

	for _, field := range sp.Fields {
		if field.IsWild {
			continue
		}

		idx := cg.fieldIndex(sp.TypeName, field.Name)
		if idx < 0 {
			continue
		}

		var ft irtypes.Type

		if cg.cLayoutStructs[sp.TypeName] {
			if nativeSt := cg.nativeStructTypes[sp.TypeName]; nativeSt != nil && idx < len(nativeSt.Fields) {
				ft = nativeSt.Fields[idx]
			}
		} else if idx < len(llvmType.Fields) {
			ft = llvmType.Fields[idx]
		}

		if nested, ok2 := field.Literal.(*ast.StructPattern); ok2 {
			cg.collectPatternBindingTypes(nested, bindings)

			continue
		}

		if field.Literal != nil {
			continue
		}

		bindName := field.Name
		if field.BindTo != "" {
			bindName = field.BindTo
		}

		if ft != nil {
			bindings[bindName] = ft
		}
	}
}

// syntheticValue is a zero-size placeholder value.Value used only to carry a
// type through astInferType's Identifier case without emitting any IR.
type syntheticValue struct{ t irtypes.Type }

func (s *syntheticValue) Type() irtypes.Type { return s.t }
func (s *syntheticValue) Ident() string      { return "%synthetic" }
func (s *syntheticValue) String() string     { return "%synthetic" }

// astInferType attempts to determine the LLVM type of a simple AST expression
// without generating any code. Returns nil when the type cannot be determined.
func (cg *CodeGen) astInferType(node ast.Node) irtypes.Type {
	switch e := node.(type) {
	case *ast.IntLit:
		if e.Big != nil {
			return irtypes.I128
		}

		return irtypes.I64
	case *ast.FloatLit:
		return irtypes.Double
	case *ast.BoolLit:
		return irtypes.I1
	case *ast.CharLit:
		return irtypes.I8
	case *ast.AtomLit:
		return cg.atomType
	case *ast.NilLit:
		return irtypes.I64
	case *ast.StringLit, *ast.InterpolatedString:
		return stringFatPtrType()
	case *ast.Identifier:
		en, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil
		}

		if en.isAlloc {
			return en.val.Type().(*irtypes.PointerType).ElemType
		}

		return en.val.Type()
	case *ast.BinExpr:
		switch e.Op {
		case "==", "!=", "<", ">", "<=", ">=", "&&", "||":
			return irtypes.I1
		default:
			return cg.astInferType(e.Left)
		}
	case *ast.AsExpr:
		t, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil
		}

		return t
	case *ast.UnaryExpr:
		return cg.astInferType(e.Expr)
	case *ast.FieldAccess:
		obj := cg.astInferType(e.Expr)
		if obj == nil {
			return nil
		}

		structName := cg.typeNameOf(obj)
		if structName == "" {
			return nil
		}

		idx := cg.fieldIndex(structName, e.Field)
		if idx < 0 {
			return nil
		}

		if st, ok := obj.(*irtypes.StructType); ok && idx < len(st.Fields) {
			return st.Fields[idx]
		}

		return nil
	case *ast.MatchStmt:
		// Infer from the first arm whose body is a single expression.
		for _, c := range e.Cases {
			if c.Body != nil && len(c.Body.Stmts) == 1 {
				if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
					if t := cg.astInferType(expr); t != nil {
						return t
					}
				}
			}
		}

		if e.Default != nil && len(e.Default.Stmts) == 1 {
			if expr := armExprNode(e.Default.Stmts[0]); expr != nil {
				return cg.astInferType(expr)
			}
		}

		return nil
	}

	return nil
}

// genMatchAsExpr runs a MatchStmt in expression mode: each arm body must be a
// single expression whose result is stored to a pre-allocated slot. The function
// updates cg.curBlock to the continuation block (afterBlock) so that callers
// using the cg.curBlock pattern (genVarDecl, genReturn, etc.) pick up the
// correct block for subsequent code emission.
func (cg *CodeGen) genMatchAsExpr(block *ir.Block, s *ast.MatchStmt) (value.Value, error) {
	// Validate: each arm must be a single expression OR a divergent
	// terminator (return / break / panic). Divergent arms don't produce a
	// value but unblock the common `let x = match ...: case Ok(v): v;
	// case Err(e): return Err(e)` propagation pattern.
	for i, c := range s.Cases {
		if c.Body == nil || len(c.Body.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: case %d has no body", i)
		}

		if len(c.Body.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: case %d has multiple statements; match expressions allow exactly one expression per arm", i)
		}

		if armExprNode(c.Body.Stmts[0]) == nil && !isExplicitTerminator(c.Body.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: case %d body is not an expression or terminator (use 'return match ...' for statement arms)", i)
		}
	}

	if s.Default != nil {
		if len(s.Default.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: default arm has no body")
		}

		if len(s.Default.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: default arm has multiple statements; match expressions allow exactly one expression per arm")
		}

		if armExprNode(s.Default.Stmts[0]) == nil && !isExplicitTerminator(s.Default.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: default arm body is not an expression or terminator (use 'return match ...' for statement arms)")
		}
	}

	// Determine result type from the first non-divergent arm. Divergent arms
	// (return/break/panic) don't yield a value, so they can't drive type
	// inference; they're skipped here and emitted without a result store.
	var resType irtypes.Type

	for _, c := range s.Cases {
		if isExplicitTerminator(c.Body.Stmts[0]) {
			continue
		}

		if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
			resType = cg.astInferTypeWithPattern(expr, c.Pattern)
		}

		if resType != nil {
			break
		}
	}

	if resType == nil && s.Default != nil && !isExplicitTerminator(s.Default.Stmts[0]) {
		if expr := armExprNode(s.Default.Stmts[0]); expr != nil {
			resType = cg.astInferType(expr)
		}
	}

	// Fall back to the caller's expected type (set by genVarDecl when the let
	// has an explicit annotation, or by genReturn for `return match ...`).
	// This rescues cases where every arm body refers to a pattern-bound name
	// the inference doesn't see (e.g. `case Ok(v): v` from an ADT pattern).
	if resType == nil {
		resType = cg.returnTypeHint
	}

	if resType == nil {
		return nil, cg.nodeErr(s, "match expression: cannot infer result type; annotate the variable or use 'return match ...'")
	}

	resAlloca := block.NewAlloca(resType)

	afterBlock, err := cg.genMatchWithResult(block, s, resAlloca)
	if err != nil {
		return nil, err
	}

	if afterBlock == nil {
		afterBlock = cg.newBlock("match.after")
	}

	cg.curBlock = afterBlock

	return afterBlock.NewLoad(resType, resAlloca), nil
}

// isKnownTypeName returns true when name resolves to a primitive,
// struct, trait, union, data, or generic type registered in this
// codegen.  Used by genIdentifier to emit a sharper error when the
// user wrote a type name in expression position (e.g. `case T:` in a
// match arm) -- the redirect points them at `match scrutinee.(type)`.
func (cg *CodeGen) isKnownTypeName(name string) bool {
	switch name {
	case "i8", "i16", "i32", "i64", "i128",
		"u8", "u16", "u32", "u64", "u128",
		"f32", "f64", "f128",
		"bool", "byte", "char", "string", "atom", "any", "void":
		return true
	}

	if _, ok := cg.structTypeIDs[name]; ok {
		return true
	}

	if _, ok := cg.unionTypeIDs[name]; ok {
		return true
	}

	if _, ok := cg.traitInstKeys[name]; ok {
		return true
	}

	if _, ok := cg.dataTypeIDs[name]; ok {
		return true
	}

	return false
}

func (cg *CodeGen) genIdentifier(block *ir.Block, e *ast.Identifier) (value.Value, error) {
	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		// Nullary ADT variant: bare `None`, `Leaf`, etc.
		if v, err := cg.genDataNullaryConstructor(block, e.Name); err != nil {
			return nil, err
		} else if v != nil {
			return v, nil
		}
		// Common confusion: the user wrote `case T:` in a match arm,
		// thinking T is a pattern that matches values of type T.  The
		// match parser treats T as an expression (since it has no `is`
		// keyword) so we end up here.  Detect known type names and
		// redirect to the `match a.(type)` form Tin uses for runtime
		// type matching.
		if cg.isKnownTypeName(e.Name) {
			return nil, cg.nodeErr(e,
				"`%s` is a type, not a value -- to match by type, write "+
					"`match <scrutinee>.(type)` (the case arms then bind the "+
					"matched type, e.g. `case x i64: ...`)", e.Name)
		}

		return nil, cg.nodeErr(e, "undefined identifier: %s", e.Name)
	}

	if entry.isAlloc {
		ptrType := entry.val.Type().(*irtypes.PointerType)

		return block.NewLoad(ptrType.ElemType, entry.val), nil
	}

	return entry.val, nil
}

// byteToStringFatPtr wraps a single i8 value in a {i8*, i64} fat-pointer so
// that it can be used on either side of a string ++ byte concatenation.
func byteToStringFatPtr(block *ir.Block, b value.Value) value.Value {
	byteAlloca := block.NewAlloca(irtypes.I8)
	block.NewStore(b, byteAlloca)

	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), byteAlloca, 0)

	return block.NewInsertValue(v0, constant.NewInt(irtypes.I64, 1), 1)
}

// isStringConcatNode reports whether node is a `++` BinExpr whose two sides
// are both string-typed (i.e. an internal node of a fusable string-concat
// chain).  Byte-coerced concats and array concats return false so the
// existing 2-way path handles them.
func (cg *CodeGen) isStringConcatNode(node ast.Node) bool {
	be, ok := node.(*ast.BinExpr)
	if !ok || be.Op != "++" {
		return false
	}

	lt := cg.astInferType(be.Left)
	rt := cg.astInferType(be.Right)

	return lt != nil && rt != nil && isStringType(lt) && isStringType(rt)
}

// flattenStringConcat walks a `++` chain on strings and returns the leaf AST
// nodes in left-to-right source order.  When node is not a string `++` it
// returns a single-element slice containing node itself.
func (cg *CodeGen) flattenStringConcat(node ast.Node) []ast.Node {
	if cg.isStringConcatNode(node) {
		be := node.(*ast.BinExpr)

		return append(cg.flattenStringConcat(be.Left), cg.flattenStringConcat(be.Right)...)
	}

	return []ast.Node{node}
}

// genFusedStringConcat lowers `a ++ b ++ ... ++ z` to a single _tin_rc_alloc
// + N memcpys.  Without fusion, codegen emits N-1 nested 2-way concats, each
// allocating an intermediate buffer that is immediately released by the next
// concat.  For workload-bench's `header ++ " | " ++ trailer` this saves one
// alloc per item (200k allocs on a 200k-item run).
//
// Each leaf is evaluated left-to-right via genExpr (preserving source-order
// side effects), then the fat-pointer's ptr+len is extracted.  After the
// memcpy phase, leaves whose AST is a temporary-producer (call result,
// interpolation, ...) are released; non-temp leaves are not -- their data
// has been copied into the new buffer, and their own RC remains untouched.
func (cg *CodeGen) genFusedStringConcat(block *ir.Block, leaves []ast.Node) (value.Value, error) {
	type part struct {
		node   ast.Node
		val    value.Value
		ptr    value.Value
		length value.Value
	}

	parts := make([]part, 0, len(leaves))

	for _, leaf := range leaves {
		cg.curBlock = block

		v, err := cg.genExpr(block, leaf)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if v == nil || !isStringType(v.Type()) {
			// Shouldn't happen given isStringConcatNode's type guard; fall
			// back to a clear error rather than emitting malformed IR.
			return nil, cg.nodeErr(leaf,
				"`++` chain leaf must be a string, got %s", fmtArgType(v.Type()))
		}

		parts = append(parts, part{
			node:   leaf,
			val:    v,
			ptr:    cg.extractStringPtr(block, v),
			length: cg.extractStringLen(block, v),
		})
	}

	totalLen := parts[0].length
	for i := 1; i < len(parts); i++ {
		totalLen = block.NewAdd(totalLen, parts[i].length)
	}

	allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
	buf := block.NewCall(cg.ensureRCAlloc(), allocSize)

	var offset value.Value = constant.NewInt(irtypes.I64, 0)

	for i, p := range parts {
		var dst value.Value
		if i == 0 {
			dst = buf
		} else {
			dst = block.NewGetElementPtr(irtypes.I8, buf, offset)
		}

		block.NewCall(cg.ensureMemcpy(), dst, p.ptr, p.length, constant.NewInt(irtypes.I1, 0))

		if i < len(parts)-1 {
			offset = block.NewAdd(offset, p.length)
		}
	}

	nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
	block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)

	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), buf, 0)
	result := block.NewInsertValue(v0, totalLen, 1)

	for _, p := range parts {
		if isTemporaryProducer(p.node) {
			cg.emitRelease(block, p.val)
		}
	}

	cg.curBlock = block

	return result, nil
}

func (cg *CodeGen) genBinExpr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// Short-circuit for && and ||.
	switch e.Op {
	case "&&":
		return cg.genLogicalAnd(block, e)
	case "||":
		return cg.genLogicalOr(block, e)
	}

	// String concat fusion: detect `++` chains on strings of length >= 3
	// before evaluating operands.  Pairwise emission would alloc a temp
	// per intermediate node; the fused path does one alloc + N memcpys.
	// Length 2 falls through to the existing 2-way path (identical IR).
	if e.Op == "++" && cg.isStringConcatNode(e) {
		if chain := cg.flattenStringConcat(e); len(chain) >= 3 {
			return cg.genFusedStringConcat(block, chain)
		}
	}

	cg.curBlock = block

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	cg.curBlock = block

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if left == nil || right == nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	// Unify types.
	lt := left.Type()
	rt := right.Type()

	// Type promotion.
	if irtypes.IsInt(lt) && irtypes.IsInt(rt) {
		lBits := lt.(*irtypes.IntType).BitSize

		rBits := rt.(*irtypes.IntType).BitSize
		if lBits < rBits {
			if cg.exprElemIsUnsigned(e.Left) {
				left = block.NewZExt(left, rt)
			} else {
				left = block.NewSExt(left, rt)
			}

			lt = rt
		} else if rBits < lBits {
			if cg.exprElemIsUnsigned(e.Right) {
				right = block.NewZExt(right, lt)
			} else {
				right = block.NewSExt(right, lt)
			}
		}
	} else if irtypes.IsFloat(lt) && irtypes.IsInt(rt) {
		right = block.NewSIToFP(right, lt)
	} else if irtypes.IsInt(lt) && irtypes.IsFloat(rt) {
		left = block.NewSIToFP(left, rt)
		lt = rt
	} else if irtypes.IsFloat(lt) && irtypes.IsFloat(rt) {
		lBits := floatBits(lt.(*irtypes.FloatType))
		rBits := floatBits(rt.(*irtypes.FloatType))

		if lBits != rBits {
			if lfc, ok := left.(*constant.Float); ok {
				// Left is a float literal: reinterpret it as the right side's type.
				v, _ := lfc.X.Float64()
				left = constant.NewFloat(rt.(*irtypes.FloatType), v)
				lt = rt
			} else if rfc, ok := right.(*constant.Float); ok {
				// Right is a float literal: reinterpret it as the left side's type.
				v, _ := rfc.X.Float64()
				right = constant.NewFloat(lt.(*irtypes.FloatType), v)
			} else {
				// Two non-literal floats of different sizes: promote smaller to larger.
				if lBits < rBits {
					left = block.NewFPExt(left, rt)
					lt = rt
				} else {
					right = block.NewFPExt(right, lt)
				}
			}
		}
	}

	isFloat := irtypes.IsFloat(lt)
	// Also treat vectors of floats as float for operator selection.
	if !isFloat {
		if vt, ok := lt.(*irtypes.VectorType); ok {
			isFloat = irtypes.IsFloat(vt.ElemType)
		}
	}

	// Pointer arithmetic: ptr + int -> getelementptr; ptr - int -> getelementptr with negation.
	if ptrType, isPtr := lt.(*irtypes.PointerType); isPtr && irtypes.IsInt(rt) {
		switch e.Op {
		case "+", "-":
			if cg.unsafeDepth == 0 {
				return nil, cg.nodeErr(e,
					"pointer arithmetic requires an `{#unsafe}` block")
			}
		}
		// Ensure the index is i64.
		if rt.(*irtypes.IntType).BitSize < 64 {
			right = block.NewSExt(right, irtypes.I64)
		}

		switch e.Op {
		case "+":
			return block.NewGetElementPtr(ptrType.ElemType, left, right), nil
		case "-":
			negIdx := block.NewSub(constant.NewInt(irtypes.I64, 0), right)

			return block.NewGetElementPtr(ptrType.ElemType, left, negIdx), nil
		}
	}

	// Operator overloading dispatch (Phase 3): if either operand is a user
	// struct that implements the corresponding built-in operator trait, lower
	// to a method call. Falls through to the primitive path when neither
	// operand is a struct, and to the Phase 0 error gate when a struct
	// operand has no matching impl.
	if isStructType(lt) || isStructType(rt) {
		if res, dispatched, derr := cg.dispatchBinOp(block, e, left, right, lt, rt); dispatched {
			return res, derr
		}

		return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
	}

	// Reject arithmetic on string / fat-ptr operands before falling into the
	// integer add/sub paths below -- without this, `s1 + s2` would emit
	// `add { i8*, i64 }` which clang rejects with a confusing low-level
	// error instead of a Tin-level diagnostic. The right concat operator
	// for strings is `++`; surface that in the message.
	if cg.isBadFatPtrArithmetic(e.Op, lt, rt) {
		hint := ""
		if e.Op == "+" && isStringType(lt) && isStringType(rt) {
			hint = " (use %q to concatenate strings)"

			return nil, cg.nodeErr(e,
				"binary operator %q is not defined for operands of type %s and %s"+hint,
				e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt), "++")
		}

		return nil, cg.nodeErr(e,
			"binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
	}

	switch e.Op {
	case "+":
		if isFloat {
			return block.NewFAdd(left, right), nil
		}

		return block.NewAdd(left, right), nil
	case "-":
		if isFloat {
			return block.NewFSub(left, right), nil
		}

		return block.NewSub(left, right), nil
	case "*":
		if isFloat {
			return block.NewFMul(left, right), nil
		}

		return block.NewMul(left, right), nil
	case "/":
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "division by zero")
		}

		if isFloat {
			return block.NewFDiv(left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewUDiv(left, right), nil
		}

		return block.NewSDiv(left, right), nil
	case "%":
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "modulo by zero")
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewURem(left, right), nil
		}

		return block.NewSRem(left, right), nil
	case "==":
		cg.checkTautologicalNilCmp(e, false)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, false)
		// Release temporary string operands after comparison (e.g., fn() == fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "!=":
		cg.checkTautologicalNilCmp(e, true)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, true)
		// Release temporary string operands after comparison (e.g., fn() != fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "<":
		if isFloat {
			return block.NewFCmp(enum.FPredOLT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULT, left, right), nil
		}

		return block.NewICmp(enum.IPredSLT, left, right), nil
	case "<=":
		if isFloat {
			return block.NewFCmp(enum.FPredOLE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULE, left, right), nil
		}

		return block.NewICmp(enum.IPredSLE, left, right), nil
	case ">":
		if isFloat {
			return block.NewFCmp(enum.FPredOGT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGT, left, right), nil
		}

		return block.NewICmp(enum.IPredSGT, left, right), nil
	case ">=":
		if isFloat {
			return block.NewFCmp(enum.FPredOGE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGE, left, right), nil
		}

		return block.NewICmp(enum.IPredSGE, left, right), nil
	case "&":
		return block.NewAnd(left, right), nil
	case "|":
		return block.NewOr(left, right), nil
	case "^":
		return block.NewXor(left, right), nil
	case "<<":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}

		return block.NewShl(left, right), nil
	case ">>":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}
		// Use logical (zero-fill) right shift for unsigned types.
		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewLShr(left, right), nil
		}

		return block.NewAShr(left, right), nil
	case "++":
		// string ++ byte  /  byte ++ string: coerce the i8 operand to a 1-char string fat-ptr.
		// The byte is stored in a stack alloca; the memcpy inside the concat path happens in the
		// same basic block so the alloca lifetime is valid.
		// Track coercion so we skip ARC release on the coerced side (stack, not RC-managed).
		leftCoerced, rightCoerced := false, false

		if isStringType(left.Type()) && irtypes.IsInt(right.Type()) && right.Type().(*irtypes.IntType).BitSize == 8 {
			right = byteToStringFatPtr(block, right)
			rightCoerced = true
		} else if isStringType(right.Type()) && irtypes.IsInt(left.Type()) && left.Type().(*irtypes.IntType).BitSize == 8 {
			left = byteToStringFatPtr(block, left)
			leftCoerced = true
		}
		// `++` is slice-slice concat (mirrors `++=`).  String ++ byte is
		// handled above; everything else requires both sides to be the
		// same fat-array (or string) type.  Without this check,
		// `[1, 2] ++ 3` silently fell into the array path and produced
		// garbage IR (insertvalue/extractvalue with a non-fat-ptr RHS).
		leftIsArr := isFatArrayPtr(left.Type()) && !isStringType(left.Type())
		rightIsArr := isFatArrayPtr(right.Type()) && !isStringType(right.Type())

		if leftIsArr != rightIsArr {
			return nil, cg.nodeErr(e,
				"`++` is slice concat: both sides must be the same slice "+
					"type, got %s ++ %s. To prepend or append a single value, "+
					"wrap it as a one-element slice: `[v] ++ xs` or `xs ++ [v]`",
				fmtArgType(left.Type()), fmtArgType(right.Type()))
		}

		if leftIsArr && rightIsArr && !left.Type().Equal(right.Type()) {
			return nil, cg.nodeErr(e,
				"`++` requires matching slice element types, got %s ++ %s",
				fmtArgType(left.Type()), fmtArgType(right.Type()))
		}
		// Typed array concatenation: {T*, i64} ++ {T*, i64} -> {T*, i64}
		// (strings {i8*, i64} are handled by the string path below)
		if isFatArrayPtr(left.Type()) && !isStringType(left.Type()) {
			fatType := left.Type().(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			leftDataPtr := block.NewExtractValue(left, 0)
			leftLen := block.NewExtractValue(left, 1)
			rightDataPtr := block.NewExtractValue(right, 0)
			rightLen := block.NewExtractValue(right, 1)
			totalLen := block.NewAdd(leftLen, rightLen)

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)

			// new_ptr = _tin_rc_alloc(totalLen * elemSize)
			totalBytes := block.NewMul(totalLen, elemSize)
			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), totalBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy left data
			leftBytes := block.NewMul(leftLen, elemSize)
			leftI8Ptr := block.NewBitCast(leftDataPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, leftI8Ptr, leftBytes, constant.NewInt(irtypes.I1, 0))

			// memcpy right data at offset leftLen*elemSize
			rightOffset := block.NewMul(leftLen, elemSize)
			rightDst := block.NewGetElementPtr(irtypes.I8, newI8Ptr, rightOffset)
			rightI8Ptr := block.NewBitCast(rightDataPtr, irtypes.I8Ptr)
			rightBytes := block.NewMul(rightLen, elemSize)
			block.NewCall(cg.ensureMemcpy(), rightDst, rightI8Ptr, rightBytes, constant.NewInt(irtypes.I1, 0))

			// Build new fat ptr {T*, i64}
			v0 := block.NewInsertValue(constant.NewUndef(fatType), newPtr, 0)
			result := block.NewInsertValue(v0, totalLen, 1)
			// For non-temporary sources, the new buffer shares element pointers
			// with the source array.  Retain each shared element so that releasing
			// the source and the new buffer are independent: each holds its own RC
			// claim and can be released in any order without use-after-free.
			//
			// For temporary sources, the temp buffer is released below (buffer-only,
			// no element release), so elements are effectively transferred to the new
			// buffer without needing a retain.
			//
			// Note: elemNeedsRelease returns false for *irtypes.PointerType (pointer
			// variables don't need scope release), but pointer elements inside [*T]
			// arrays DO need retain/release so we check that case explicitly.
			_, elemIsPtr := elemT.(*irtypes.PointerType)
			needsElemRetain := cg.elemNeedsRelease(elemT) || isRCTrackedType(elemT) || elemIsPtr

			if !isTemporaryProducer(e.Left) && needsElemRetain {
				cg.emitRetainElemSlice(block, newI8Ptr, leftLen, elemT)
			}

			if !isTemporaryProducer(e.Right) && needsElemRetain {
				cg.emitRetainElemSlice(block, rightDst, rightLen, elemT)
			}

			// Release sub-expression temporaries: buffer-only release transfers
			// ownership of elements to the new buffer without a retain.
			if isTemporaryProducer(e.Left) {
				if rcPtr := cg.extractRCDataPtr(block, left, left.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
			}

			if isTemporaryProducer(e.Right) {
				if rcPtr := cg.extractRCDataPtr(block, right, right.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
			}

			return result, nil
		}

		// String concatenation: both operands are {i8*, i64} fat-ptrs.
		leftPtr := cg.extractStringPtr(block, left)
		leftLen := cg.extractStringLen(block, left)
		rightPtr := cg.extractStringPtr(block, right)
		rightLen := cg.extractStringLen(block, right)
		totalLen := block.NewAdd(leftLen, rightLen)
		// rc_alloc(totalLen + 1) for null terminator; ARC manages the result.
		allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
		buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
		// memcpy(buf, leftPtr, leftLen)
		block.NewCall(cg.ensureMemcpy(), buf, leftPtr, leftLen, constant.NewInt(irtypes.I1, 0))
		// memcpy(buf + leftLen, rightPtr, rightLen)
		rightDst := block.NewGetElementPtr(irtypes.I8, buf, leftLen)
		block.NewCall(cg.ensureMemcpy(), rightDst, rightPtr, rightLen, constant.NewInt(irtypes.I1, 0))
		// null-terminate
		nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
		block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)
		// build {i8*, i64} fat-ptr result
		fatPtrType := stringFatPtrType()
		v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), buf, 0)
		result := block.NewInsertValue(v0, totalLen, 1)
		// Release sub-expression temporaries now that the result is built.
		// Skip byte-to-string coerced operands: their ptr is a stack alloca, not ARC-managed.
		if isTemporaryProducer(e.Left) && !leftCoerced {
			cg.emitRelease(block, left)
		}

		if isTemporaryProducer(e.Right) && !rightCoerced {
			cg.emitRelease(block, right)
		}

		return result, nil
	}

	// No primitive / built-in lowering matched. Until operator overloading
	// lands (docs/plans/operator-overloading.md), there is no user hook
	// either; reject loudly instead of silently producing 0. Phase 0 of
	// that plan exists because the previous silent-zero fall-through hid
	// real bugs at every callsite.
	return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
		e.Op, cg.tinTypeDisplay(left.Type()), cg.tinTypeDisplay(right.Type()))
}

// genEqNeqExpr implements shared handling for == and != operators.
func (cg *CodeGen) genEqNeqExpr(block *ir.Block, left, right value.Value, lt, rt irtypes.Type, isFloat bool, notEqual bool) value.Value {
	if isFloat {
		// IEEE 754 NaN: x == x is false, x != x is true. OEQ matches the
		// first (false on NaN); UNE the second (true on NaN). Using ONE
		// for != would silently fold `x != x` to false, breaking the
		// canonical NaN test pattern.
		if notEqual {
			return block.NewFCmp(enum.FPredUNE, left, right)
		}

		return block.NewFCmp(enum.FPredOEQ, left, right)
	}

	pred := enum.IPredEQ
	if notEqual {
		pred = enum.IPredNE
	}

	// any equality/inequality: dynamically dispatched by runtime.
	if isAnyType(lt) || isAnyType(rt) {
		var tempLeft, tempRight value.Value

		if !isAnyType(lt) {
			left = cg.boxToAny(block, left)
			tempLeft = left
		}

		if !isAnyType(rt) {
			right = cg.boxToAny(block, right)
			tempRight = right
		}

		cmp := block.NewCall(cg.ensureAnyEq(), left, right)

		// Release temporary boxes created by boxToAny - they are fresh RC=1
		// allocations that exist only for this comparison.
		if tempLeft != nil {
			cg.emitRelease(block, tempLeft)
		}

		if tempRight != nil {
			cg.emitRelease(block, tempRight)
		}

		result := cmp
		if notEqual {
			return block.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I64, 0))
		}

		return block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I64, 0))
	}

	// atom ==/!= atom: compare CRC32 codes directly.
	if isAtomType(lt) && isAtomType(rt) {
		lcode := cg.extractAtomCode(block, left)
		rcode := cg.extractAtomCode(block, right)

		return block.NewICmp(pred, lcode, rcode)
	}

	// atom <-> string: convert atom to string, then strcmp.
	if isAtomType(lt) && isFatPtrType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, left))
		lptr := cg.extractStringPtr(block, strVal)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	if isFatPtrType(lt) && isAtomType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, right))
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, strVal)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// String equality/inequality: compare via strcmp.
	if isFatPtrType(lt) {
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// Pointer vs integer-zero (None): coerce i64(0) to typed null pointer.
	if irtypes.IsPointer(lt) && !irtypes.IsPointer(rt) {
		right = constant.NewNull(lt.(*irtypes.PointerType))
	} else if irtypes.IsPointer(rt) && !irtypes.IsPointer(lt) {
		left = constant.NewNull(rt.(*irtypes.PointerType))
	}

	return block.NewICmp(pred, left, right)
}

// genLogicalAnd emits short-circuit `A && B` as `if A { B } else { false }`.
// The RHS evaluates only when LHS is true. cg.curBlock is updated to the
// merge block on return so the caller continues emitting there. Callers that
// reference `block` (the input) post-call would target a terminated block;
// they must use cg.curBlock instead.
func (cg *CodeGen) genLogicalAnd(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, false)
}

// genLogicalOr emits short-circuit `A || B` as `if A { true } else { B }`.
// Symmetric to genLogicalAnd; see that function's note about cg.curBlock.
func (cg *CodeGen) genLogicalOr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, true)
}

// genShortCircuit lowers a logical && or || with proper short-circuit
// semantics. shortVal is the value the operator returns when the LHS
// already determines the result: false for &&, true for ||. The RHS
// is evaluated only when the LHS does NOT short-circuit.
func (cg *CodeGen) genShortCircuit(block *ir.Block, e *ast.BinExpr, shortVal bool) (value.Value, error) {
	cg.curBlock = block

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, left.Type()); err != nil {
		return nil, err
	}

	leftEnd := cg.curBlock
	leftBool := cg.toBool(leftEnd, left)

	var label string
	if shortVal {
		label = "or"
	} else {
		label = "and"
	}

	rhsBlock := cg.newBlock(label + ".rhs")
	mergeBlock := cg.newBlock(label + ".merge")

	if shortVal {
		// `A || B`: short-circuit to merge when A is true.
		leftEnd.NewCondBr(leftBool, mergeBlock, rhsBlock)
	} else {
		// `A && B`: short-circuit to merge when A is false.
		leftEnd.NewCondBr(leftBool, rhsBlock, mergeBlock)
	}

	cg.curBlock = rhsBlock

	right, err := cg.genExpr(rhsBlock, e.Right)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, right.Type()); err != nil {
		return nil, err
	}

	rightEnd := cg.curBlock
	rightBool := cg.toBool(rightEnd, right)
	rightEnd.NewBr(mergeBlock)

	var shortConst constant.Constant
	if shortVal {
		shortConst = constant.NewInt(irtypes.I1, 1)
	} else {
		shortConst = constant.NewInt(irtypes.I1, 0)
	}

	phi := mergeBlock.NewPhi(
		ir.NewIncoming(shortConst, leftEnd),
		ir.NewIncoming(rightBool, rightEnd),
	)
	cg.curBlock = mergeBlock

	return phi, nil
}
