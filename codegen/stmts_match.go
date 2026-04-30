package codegen

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// isExhaustiveEnumMatch returns true when every case pattern is a member of
// the same enum and all members of that enum are covered - making a default
// clause unnecessary for exhaustiveness.
// isExhaustiveStructMatch returns true when the struct match has at least one
// total pattern arm: a StructPattern with no literal constraints and no guard,
// or a default arm. Such an arm covers all remaining values.
func (cg *CodeGen) isExhaustiveStructMatch(s *ast.MatchStmt) bool {
	if s.Default != nil {
		return true
	}

	for _, c := range s.Cases {
		sp, ok := c.Pattern.(*ast.StructPattern)
		if !ok {
			continue
		}

		if c.Guard != nil {
			continue
		}

		total := true

		for _, f := range sp.Fields {
			if f.Literal != nil {
				total = false

				break
			}
		}

		if total {
			return true
		}
	}

	return false
}

// applyPatternChecks emits constraint checks for a struct pattern against the
// value stored in scrutAlloca. For each literal-constrained field it compares
// the loaded field value against the literal (or recursively checks a nested
// StructPattern) and branches to failBlock on mismatch. Returns the block
// where all constraints have passed. The current scope must already be open;
// free fields are NOT bound here - call bindPatternFree after all checks.
func (cg *CodeGen) applyPatternChecks(
	checkBlock *ir.Block,
	failBlock *ir.Block,
	scrutType irtypes.Type,
	scrutAlloca value.Value,
	structName string,
	sp *ast.StructPattern,
	caseIdx int,
	passSeq *int,
) (*ir.Block, error) {
	for _, field := range sp.Fields {
		if field.IsWild || field.Literal == nil {
			continue
		}

		fieldIdx := cg.fieldIndex(structName, field.Name)
		if fieldIdx < 0 {
			return nil, cg.nodeErr(sp, "struct pattern: unknown field %s.%s", structName, field.Name)
		}

		var fieldType irtypes.Type

		var gep value.Value

		if cg.cLayoutStructs[structName] {
			// fieldIdx is native 0-based; access through c_data_ptr.
			nativeSt := cg.nativeStructTypes[structName]
			if nativeSt != nil && fieldIdx < len(nativeSt.Fields) {
				fieldType = nativeSt.Fields[fieldIdx]
			} else {
				fieldType = irtypes.I64
			}

			gep = cg.emitCLayoutFieldPtr(checkBlock, scrutAlloca, structName, fieldIdx)
		} else {
			if st, ok := scrutType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
				fieldType = st.Fields[fieldIdx]
			} else {
				fieldType = irtypes.I64
			}

			gep = checkBlock.NewGetElementPtr(scrutType, scrutAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		}

		// Nested struct pattern: recurse.
		if nested, ok := field.Literal.(*ast.StructPattern); ok {
			subAlloca := checkBlock.NewAlloca(fieldType)
			subVal := checkBlock.NewLoad(fieldType, gep)
			checkBlock.NewStore(subVal, subAlloca)

			subName := cg.typeNameOf(fieldType)

			var err error

			checkBlock, err = cg.applyPatternChecks(checkBlock, failBlock, fieldType, subAlloca, subName, nested, caseIdx, passSeq)
			if err != nil {
				return nil, err
			}

			continue
		}

		// Scalar literal constraint.
		fieldVal := checkBlock.NewLoad(fieldType, gep)

		litVal, err := cg.genExpr(checkBlock, field.Literal)
		if err != nil {
			return nil, err
		}

		litVal = cg.coerce(checkBlock, litVal, fieldType)

		var cmp value.Value

		if irtypes.IsFloat(fieldType) {
			cmp = checkBlock.NewFCmp(enum.FPredOEQ, fieldVal, litVal)
		} else if isFatPtrType(fieldType) {
			// String/fat-pointer equality: use strcmp.
			lptr := cg.extractStringPtr(checkBlock, fieldVal)
			rptr := cg.extractStringPtr(checkBlock, litVal)
			strcmpResult := checkBlock.NewCall(cg.ensureStrcmp(), lptr, rptr)
			cmp = checkBlock.NewICmp(enum.IPredEQ, strcmpResult, constant.NewInt(irtypes.I32, 0))
		} else {
			cmp = checkBlock.NewICmp(enum.IPredEQ, fieldVal, litVal)
		}

		*passSeq++
		passBlock := cg.newBlock(fmt.Sprintf("match.case.%d.pass.%d", caseIdx, *passSeq))
		checkBlock.NewCondBr(cmp, passBlock, failBlock)
		checkBlock = passBlock
	}

	return checkBlock, nil
}

// bindPatternFree loads each free (unbound) field from scrutAlloca and creates
// an alloca in cg.curScope. For nested StructPattern fields it recurses.
func (cg *CodeGen) bindPatternFree(
	block *ir.Block,
	scrutType irtypes.Type,
	scrutAlloca value.Value,
	structName string,
	sp *ast.StructPattern,
) error {
	for _, field := range sp.Fields {
		if field.IsWild {
			continue
		}

		fieldIdx := cg.fieldIndex(structName, field.Name)
		if fieldIdx < 0 {
			return cg.nodeErr(sp, "struct pattern: unknown field %s.%s", structName, field.Name)
		}

		var (
			fieldType irtypes.Type
			gep       value.Value
		)

		if cg.cLayoutStructs[structName] {
			nativeSt := cg.nativeStructTypes[structName]
			if nativeSt != nil && fieldIdx < len(nativeSt.Fields) {
				fieldType = nativeSt.Fields[fieldIdx]
			} else {
				fieldType = irtypes.I64
			}

			gep = cg.emitCLayoutFieldPtr(block, scrutAlloca, structName, fieldIdx)
		} else {
			if st, ok := scrutType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
				fieldType = st.Fields[fieldIdx]
			} else {
				fieldType = irtypes.I64
			}

			gep = block.NewGetElementPtr(scrutType, scrutAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		}

		// Nested struct pattern: recurse into sub-fields.
		if nested, ok := field.Literal.(*ast.StructPattern); ok {
			subAlloca := block.NewAlloca(fieldType)
			subVal := block.NewLoad(fieldType, gep)
			block.NewStore(subVal, subAlloca)

			if err := cg.bindPatternFree(block, fieldType, subAlloca, cg.typeNameOf(fieldType), nested); err != nil {
				return err
			}

			continue
		}

		// Literal constraint: no binding needed.
		if field.Literal != nil {
			continue
		}

		// Free field: bind to scope under Name (or BindTo if a rename was specified).
		bindName := field.Name
		if field.BindTo != "" {
			bindName = field.BindTo
		}

		fv := block.NewLoad(fieldType, gep)
		fa := block.NewAlloca(fieldType)
		block.NewStore(fv, fa)
		cg.curScope.set(bindName, &scopeEntry{val: fa, isAlloc: true})
	}

	return nil
}

// genStructMatch generates an if-else chain for match statements whose cases
// use struct destructuring patterns. resAlloca is non-nil in expression mode:
// each arm must consist of a single ExprStmt whose value is stored there.
// genDataMatch compiles `match x: case Ctor(bindings): ...` on an ADT value.
// Uses a tag-dispatch switch on the i8 tag field (offset 1 of the outer
// struct). In each case block, the payload is bitcast to the variant's
// packed struct and each field is bound into the arm scope.
//
// Exhaustiveness: when every variant is covered by some arm, the switch's
// default branch is `unreachable`. Otherwise it falls through to the
// user-supplied default, or (absent one) to afterBlock.
func (cg *CodeGen) genDataMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	scrutType := scrutinee.Type()
	if pt, ok := scrutType.(*irtypes.PointerType); ok {
		scrutinee = block.NewLoad(pt.ElemType, scrutinee)
		scrutType = pt.ElemType
	}

	adtName := cg.typeNameOf(scrutType)
	if adtName == "" {
		return nil, fmt.Errorf("genDataMatch: cannot resolve ADT name for match scrutinee (type=%v)", scrutType)
	}

	outerSt := cg.structTypes[adtName]
	if outerSt == nil {
		return nil, fmt.Errorf("genDataMatch: ADT %q is not registered", adtName)
	}

	scrutAlloca := block.NewAlloca(outerSt)
	block.NewStore(scrutinee, scrutAlloca)

	// Wrap arm processing in a synthetic "scrutinee scope" only when
	// the scrutinee was an OWNED expression (CallExpr, ++ concat,
	// interpolated string, fresh array literal) AND the ADT carries
	// owning fields. The owned-expression result was transferred to us
	// at RC=1 with nothing else holding it, so without a release-on-
	// exit it leaks (the original time_test bug:
	// `match json::parse(s): Ok(ev) -> ...`).
	//
	// For BORROWED scrutinees (Identifier / FieldAccess / DerefExpr of
	// a named variable / IndexExpr), the original owner still holds
	// the +1 RC and will release it at its own scope exit. Adding our
	// own release here would double-free; this is what regressed
	// adt_basics' `sum(t *IntTree)` recursion when we tried to retain
	// then release uniformly.
	wantScrutRelease := !isCopyExpr(s.Expr) && cg.elemNeedsRelease(outerSt)

	matchScrutScope := newScope(cg.curScope)
	if wantScrutRelease {
		matchScrutScope.set("__match_scrut", &scopeEntry{val: scrutAlloca, isAlloc: true})
	}
	cg.curScope = matchScrutScope

	tagGEP := block.NewGetElementPtr(outerSt, scrutAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI8 := block.NewLoad(irtypes.I8, tagGEP)
	tagI64 := block.NewZExt(tagI8, irtypes.I64)

	afterBlock := cg.newBlock("match.after")
	exhaustive := cg.isExhaustiveDataMatch(s, adtName)
	anyFallthrough := false

	if !exhaustive && s.Default == nil {
		_, witness := cg.marangetCheckMatchExhaustive(s)
		if witness == "" {
			witness = "<unknown variant>"
		}

		pos := s.Pos()
		if pos.Line == 0 && len(s.Cases) > 0 {
			pos = s.Cases[0].Pos
		}

		return nil, fmt.Errorf("%d:%d: non-exhaustive match on %s: no arm matches %s; add the missing variant or a `default:` arm",
			pos.Line, pos.Col, adtName, witness)
	}

	defaultBlock := cg.newBlock("match.default")

	var cases []*ir.Case

	seenTags := make(map[int8]bool, len(s.Cases))

	for i, c := range s.Cases {
		if !cg.isDataMatchPattern(c.Pattern) {
			return nil, fmt.Errorf("genDataMatch: non-ADT pattern in arm %d", i)
		}

		variantName := dataPatternVariantName(c.Pattern)

		vi := cg.dataVariantInfoFor(adtName, variantName)
		if vi == nil {
			return nil, fmt.Errorf("data %s: no variant %q", adtName, variantName)
		}

		binders := dataPatternBinders(c.Pattern)
		if len(binders) != len(vi.Fields) {
			return nil, fmt.Errorf("data %s: case %s expects %d binding(s), got %d",
				adtName, variantName, len(vi.Fields), len(binders))
		}

		if seenTags[vi.Tag] {
			// Arm is unreachable (Maranget already warned). Skip so we don't
			// emit a duplicate switch case and fail at llc.
			continue
		}

		seenTags[vi.Tag] = true

		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d.%s", i, variantName))
		cases = append(cases, ir.NewCase(
			constant.NewInt(irtypes.I64, int64(vi.Tag)), caseBlock))

		cg.curScope = newScope(cg.curScope)

		if len(vi.Fields) > 0 {
			payloadGEP := caseBlock.NewGetElementPtr(outerSt, scrutAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
			payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

			for fi, f := range vi.Fields {
				name := binders[fi]
				if name == "" || name == "_" {
					continue
				}

				fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
				fieldTy := vi.PayloadType.Fields[fi]
				fieldVal := caseBlock.NewLoad(fieldTy, fieldPtr)

				// For RC-tracked payload fields (string/array/any/fn closure),
				// retain the value on bind so the binding has an independent
				// RC contribution. The normal scope-exit release balances the
				// retain, and `return s` transfers ownership cleanly via the
				// `skipName` mechanism in genReturnStmt. Pointer-to-struct
				// fields (e.g. `own *Tree[t]`) are bound as borrows without
				// retain; they are shared with the scrutinee's payload.
				//
				// For embedded named structs whose payload itself contains
				// RC fields (e.g. Result.Ok(event_with_time) where
				// event_with_time has a string), the scope-exit semantics
				// are the same as a borrow only when the scrutinee will be
				// released by matchScrutScope (the OWNED scrutinee path).
				// In that case the per-arm release MUST fire so two paths
				// (binding scope + scrutinee scope) don't both decrement.
				// We mirror the predicate by retaining + releasing
				// symmetrically only when wantScrutRelease was set.
				rcTracked := !f.IsWeak && isRCTrackedType(fieldTy)
				owningStruct := !f.IsWeak && wantScrutRelease &&
					!isRCTrackedType(fieldTy) && cg.elemNeedsRelease(fieldTy)

				if rcTracked {
					cg.emitRetain(caseBlock, fieldVal)
				} else if owningStruct {
					cg.emitStructFieldRetain(caseBlock, fieldVal)
				}

				alloca := caseBlock.NewAlloca(fieldVal.Type())
				caseBlock.NewStore(fieldVal, alloca)

				entry := &scopeEntry{val: alloca, isAlloc: true}
				if !rcTracked && !owningStruct {
					entry.noRelease = true
				}

				cg.curScope.set(name, entry)
			}
		}

		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(caseBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			guardedBody := cg.newBlock(fmt.Sprintf("match.case.%d.%s.body", i, variantName))
			caseBlock.NewCondBr(cg.toBool(caseBlock, guardVal), guardedBody, defaultBlock)

			caseBlock = guardedBody
		}

		if _, err2 := cg.emitMatchArmBody(c, caseBlock, afterBlock, resAlloca, &anyFallthrough); err2 != nil {
			cg.curScope = cg.curScope.parent

			return nil, err2
		}

		cg.curScope = cg.curScope.parent
	}

	block.NewSwitch(tagI64, defaultBlock, cases...)

	if s.Default != nil {
		cg.curScope = newScope(cg.curScope)

		if _, err := cg.emitMatchArmBody(ast.MatchCase{Body: s.Default}, defaultBlock, afterBlock, resAlloca, &anyFallthrough); err != nil {
			cg.curScope = cg.curScope.parent

			return nil, err
		}

		cg.curScope = cg.curScope.parent
	} else if exhaustive {
		defaultBlock.NewUnreachable()
	} else {
		defaultBlock.NewBr(afterBlock)

		anyFallthrough = true
	}

	// Release the scrutinee's owned ARC fields on the merged exit
	// path. Returns inside an arm already drained matchScrutScope via
	// emitAllScopeReleases (the synthetic scope sits above each arm
	// scope), so this only fires when control falls through to
	// afterBlock. matchScrutScope is only populated when the scrutinee
	// was owned (see wantScrutRelease above); for borrowed scrutinees
	// the scope is empty and emitScopeRelease is a no-op.
	if anyFallthrough {
		cg.emitScopeRelease(afterBlock, matchScrutScope)
	}
	cg.curScope = matchScrutScope.parent

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) genStructMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	scrutType := scrutinee.Type()
	// Auto-deref pointer to named struct.
	if pt, ok := scrutType.(*irtypes.PointerType); ok {
		if cg.typeNameOf(pt.ElemType) != "" {
			scrutinee = block.NewLoad(pt.ElemType, scrutinee)
			scrutType = pt.ElemType
		}
	}

	scrutAlloca := block.NewAlloca(scrutType)
	block.NewStore(scrutinee, scrutAlloca)

	structName := cg.typeNameOf(scrutType)

	afterBlock := cg.newBlock("match.after")
	anyFallthrough := false

	curCheckBlock := block

	for i, c := range s.Cases {
		sp, ok := c.Pattern.(*ast.StructPattern)
		if !ok {
			return nil, fmt.Errorf("genStructMatch: non-struct pattern in struct match (case %d)", i)
		}

		nextCaseBlock := cg.newBlock(fmt.Sprintf("match.next.%d", i))
		bodyBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		checkBlock := curCheckBlock

		// Emit constraint checks for this pattern (recurses into nested StructPatterns).
		passSeq := 0

		var err2 error

		checkBlock, err2 = cg.applyPatternChecks(checkBlock, nextCaseBlock, scrutType, scrutAlloca, structName, sp, i, &passSeq)
		if err2 != nil {
			return nil, err2
		}

		// Bind free fields (including nested) before the guard so guard expressions
		// can reference them. Allocas in checkBlock dominate bodyBlock and nextCaseBlock.
		cg.curScope = newScope(cg.curScope)

		if err2 = cg.bindPatternFree(checkBlock, scrutType, scrutAlloca, structName, sp); err2 != nil {
			cg.curScope = cg.curScope.parent

			return nil, err2
		}

		// After field constraints (and bindings): apply guard if present.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(checkBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			checkBlock.NewCondBr(cg.toBool(checkBlock, guardVal), bodyBlock, nextCaseBlock)
		} else {
			checkBlock.NewBr(bodyBlock)
		}

		var bodyErr error

		_, bodyErr = cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
		cg.curScope = cg.curScope.parent

		if bodyErr != nil {
			return nil, bodyErr
		}

		curCheckBlock = nextCaseBlock
	}

	// Default or exhaustiveness fallthrough.
	var defaultErr error

	_, defaultErr = cg.emitMatchDefaultArm(s, curCheckBlock, afterBlock, resAlloca, &anyFallthrough, cg.isExhaustiveStructMatch(s))
	if defaultErr != nil {
		return nil, defaultErr
	}

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

// isExhaustiveArrayMatch returns true when the match is guaranteed to cover
// every possible array length.  Exhaustive when:
//   - default: arm is present, or
//   - a guard-free [...xs] arm catches everything, or
//   - the union of exact-length arms (guard-free) and the minimum-length
//     intervals of rest arms (guard-free) covers all non-negative integers.
//
// Example: []  +  [x, ...xs]  ->  {0} union [1,inf) = [0,inf)  -> exhaustive.
func (cg *CodeGen) isExhaustiveArrayMatch(s *ast.MatchStmt) bool {
	if s.Default != nil {
		return true
	}

	exactLengths := make(map[int]bool)
	minRestCover := -1 // smallest min-length seen across rest arms; -1 = none

	for _, c := range s.Cases {
		ap, ok := c.Pattern.(*ast.ArrayPattern)
		if !ok || c.Guard != nil {
			continue
		}

		hasRest := false
		fixed := 0

		for _, e := range ap.Elems {
			if e.IsRest {
				hasRest = true
			} else {
				fixed++
			}
		}

		if hasRest {
			// This arm covers [fixed, inf).
			if minRestCover < 0 || fixed < minRestCover {
				minRestCover = fixed
			}
		} else {
			exactLengths[fixed] = true
		}
	}

	if minRestCover < 0 {
		return false // no rest arm
	}

	// Check that every integer 0 .. minRestCover-1 is covered by exact arms.
	for i := 0; i < minRestCover; i++ {
		if !exactLengths[i] {
			return false
		}
	}

	return true
}

// emitMatchDefaultArm emits the default arm (or exhaustiveness terminator) that
// is shared by genStructMatch and genArrayMatch.  It mutates anyFallthrough
// through the supplied pointer and returns the (possibly updated) curCheckBlock.
func (cg *CodeGen) emitMatchDefaultArm(
	s *ast.MatchStmt,
	curCheckBlock *ir.Block,
	afterBlock *ir.Block,
	resAlloca value.Value,
	anyFallthrough *bool,
	isExhaustive bool,
) (*ir.Block, error) {
	if s.Default != nil {
		if resAlloca != nil {
			if len(s.Default.Stmts) == 1 {
				if expr := armExprNode(s.Default.Stmts[0]); expr != nil {
					// Reset curBlock so a previous arm's inner-match advancement
					// doesn't pollute emission into curCheckBlock.
					cg.curBlock = curCheckBlock

					exprVal, err2 := cg.genExpr(curCheckBlock, expr)
					if err2 != nil {
						return nil, err2
					}

					if cg.curBlock != curCheckBlock {
						curCheckBlock = cg.curBlock
					}

					if exprVal != nil {
						resType := resAlloca.Type().(*irtypes.PointerType).ElemType
						curCheckBlock.NewStore(cg.coerce(curCheckBlock, exprVal, resType), resAlloca)
					}
				}
			}

			curCheckBlock.NewBr(afterBlock)

			*anyFallthrough = true
		} else {
			cg.curScope = newScope(cg.curScope)

			var err2 error

			curCheckBlock, _, err2 = cg.genStmt(curCheckBlock, s.Default)
			cg.curScope = cg.curScope.parent

			if err2 != nil {
				return nil, err2
			}

			if curCheckBlock != nil && curCheckBlock.Term == nil {
				curCheckBlock.NewBr(afterBlock)

				*anyFallthrough = true
			}
		}
	} else if isExhaustive {
		curCheckBlock.NewUnreachable()
	} else {
		curCheckBlock.NewBr(afterBlock)

		*anyFallthrough = true
	}

	return curCheckBlock, nil
}

// emitMatchArmBody emits the body of a single match arm in expression or
// statement mode.  Scope management (push/pop) is the caller's responsibility.
// On error the caller must still pop the scope before propagating.
func (cg *CodeGen) emitMatchArmBody(
	c ast.MatchCase,
	bodyBlock *ir.Block,
	afterBlock *ir.Block,
	resAlloca value.Value,
	anyFallthrough *bool,
) (*ir.Block, error) {
	if resAlloca != nil {
		// Expression mode: single-expression body (ExprStmt or nested MatchStmt).
		if c.Body != nil && len(c.Body.Stmts) == 1 {
			if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
				// Reset curBlock so a previous arm's inner-match advancement
				// doesn't pollute emission into bodyBlock.
				cg.curBlock = bodyBlock

				exprVal, err2 := cg.genExpr(bodyBlock, expr)
				if err2 != nil {
					return nil, err2
				}

				// genExpr may have advanced cg.curBlock (e.g. inner match expression).
				if cg.curBlock != bodyBlock {
					bodyBlock = cg.curBlock
				}

				if exprVal != nil {
					resType := resAlloca.Type().(*irtypes.PointerType).ElemType
					bodyBlock.NewStore(cg.coerce(bodyBlock, exprVal, resType), resAlloca)
				}
			}
		}

		// Release match-arm-bound variables (e.g. ...rest subslice) before
		// branching out of the arm.  This mirrors the scope-release emitted at
		// normal function exit but is scoped to only the arm's own bindings.
		cg.emitScopeRelease(bodyBlock, cg.curScope)
		bodyBlock.NewBr(afterBlock)

		*anyFallthrough = true
	} else {
		var err2 error

		bodyBlock, _, err2 = cg.genStmt(bodyBlock, c.Body)
		if err2 != nil {
			return nil, err2
		}

		if bodyBlock != nil && bodyBlock.Term == nil {
			// Release match-arm-bound variables (e.g. ...rest subslice) before
			// branching out of the arm.  Explicit returns inside the arm already
			// call emitAllScopeReleases, so this only runs when control falls
			// through to afterBlock.
			cg.emitScopeRelease(bodyBlock, cg.curScope)
			bodyBlock.NewBr(afterBlock)

			*anyFallthrough = true
		}
	}

	return bodyBlock, nil
}

// genArrayMatch generates an if-else chain for match statements whose cases
// use array destructuring patterns.  resAlloca is non-nil in expression mode.
//
// Each case checks:
//   - No rest: exact length match (len == n)
//   - Rest at end: minimum length match (len >= n-1)
//
// Variables are bound to individual elements (or a sub-slice for rest).
func (cg *CodeGen) genArrayMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if !isFatArrayPtr(scrutinee.Type()) {
		return nil, fmt.Errorf("array pattern match requires an array type, got %s", scrutinee.Type())
	}

	arrType := scrutinee.Type().(*irtypes.StructType)
	elemPtrType := arrType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	// Spill scrutinee to alloca so we can GEP into it.
	arrAlloca := block.NewAlloca(arrType)
	block.NewStore(scrutinee, arrAlloca)

	// Load length: GEP field 1.
	lenGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	// Load data pointer: GEP field 0.
	ptrGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dataPtr := block.NewLoad(elemPtrType, ptrGep)

	afterBlock := cg.newBlock("amatch.after")
	anyFallthrough := false
	curCheckBlock := block

	for i, c := range s.Cases {
		ap, ok := c.Pattern.(*ast.ArrayPattern)
		if !ok {
			return nil, fmt.Errorf("genArrayMatch: non-array pattern in case %d", i)
		}

		nextBlock := cg.newBlock(fmt.Sprintf("amatch.next.%d", i))
		bodyBlock := cg.newBlock(fmt.Sprintf("amatch.case.%d", i))

		// Count regular (non-rest) elements and find rest index.
		regularCount := 0
		restIdx := -1

		for j, e := range ap.Elems {
			if e.IsRest {
				restIdx = j
			} else {
				regularCount++
			}
		}

		// Emit length check.
		var lenCond value.Value

		checkBlock := curCheckBlock
		nConst := constant.NewInt(irtypes.I64, int64(regularCount))

		if restIdx >= 0 {
			// Rest semantics: the rest slot may bind zero or more elements, so
			// `[x, ...xs]` matches `[3]` (x=3, xs=[]). The only constraint is
			// that the array must have at least `regularCount` elements AND be
			// non-empty (so `[...xs]` does not overlap with `[]`).
			//
			// Use `[]` to match the empty array; use exact-length patterns
			// `[x]`, `[x, y]`, ... when no rest slot is needed.
			minLen := int64(regularCount)
			if minLen < 1 {
				minLen = 1
			}

			lenCond = checkBlock.NewICmp(enum.IPredSGE, arrLen, constant.NewInt(irtypes.I64, minLen))
		} else {
			// len == regularCount
			lenCond = checkBlock.NewICmp(enum.IPredEQ, arrLen, nConst)
		}

		afterLenCheck := cg.newBlock(fmt.Sprintf("amatch.lenok.%d", i))
		checkBlock.NewCondBr(lenCond, afterLenCheck, nextBlock)
		checkBlock = afterLenCheck

		// Open scope for bindings.
		cg.curScope = newScope(cg.curScope)

		// Bind regular elements.
		regIdx := 0

		for _, e := range ap.Elems {
			if e.IsRest {
				continue
			}

			if !e.IsWild && e.Name != "" {
				idxVal := constant.NewInt(irtypes.I64, int64(regIdx))
				elemGep := checkBlock.NewGetElementPtr(elemType, dataPtr, idxVal)
				loaded := checkBlock.NewLoad(elemType, elemGep)
				alloca := checkBlock.NewAlloca(elemType)
				checkBlock.NewStore(loaded, alloca)
				cg.curScope.set(e.Name, &scopeEntry{val: alloca, isAlloc: true})
			}

			regIdx++
		}

		// Bind rest element.
		if restIdx >= 0 {
			e := ap.Elems[restIdx]

			if !e.IsWild && e.Name != "" {
				// Build {i8*, i64} raw slice then subslice from regularCount.
				var elemSzBytes int64 = 8
				if sz := llvmTypeSize(elemType); sz > 0 {
					elemSzBytes = int64(sz)
				}

				sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
				rawAlloca := checkBlock.NewAlloca(sliceType)

				dataPtrAsI8 := checkBlock.NewBitCast(dataPtr, irtypes.I8Ptr)
				rawPtrGep := checkBlock.NewGetElementPtr(sliceType, rawAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				checkBlock.NewStore(dataPtrAsI8, rawPtrGep)
				rawLenGep := checkBlock.NewGetElementPtr(sliceType, rawAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
				checkBlock.NewStore(arrLen, rawLenGep)
				rawSlice := checkBlock.NewLoad(sliceType, rawAlloca)

				subFn := cg.ensureSliceSubslice()
				subResult := checkBlock.NewCall(subFn, rawSlice,
					constant.NewInt(irtypes.I64, int64(regularCount)),
					constant.NewInt(irtypes.I64, elemSzBytes))

				// Reinterpret {i8*, i64} as the original fat-array type.
				tmpAlloca := checkBlock.NewAlloca(sliceType)
				checkBlock.NewStore(subResult, tmpAlloca)
				castPtr := checkBlock.NewBitCast(tmpAlloca, irtypes.NewPointer(arrType))
				restVal := checkBlock.NewLoad(arrType, castPtr)
				restAlloca := checkBlock.NewAlloca(arrType)
				checkBlock.NewStore(restVal, restAlloca)
				cg.curScope.set(e.Name, &scopeEntry{val: restAlloca, isAlloc: true})
			}
		}

		// Optional guard.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(checkBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			checkBlock.NewCondBr(cg.toBool(checkBlock, guardVal), bodyBlock, nextBlock)
		} else {
			checkBlock.NewBr(bodyBlock)
		}

		// Emit body.
		var bodyErr error

		_, bodyErr = cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
		cg.curScope = cg.curScope.parent

		if bodyErr != nil {
			return nil, bodyErr
		}

		curCheckBlock = nextBlock
	}

	// Default arm or exhaustiveness.
	var defaultErr error

	_, defaultErr = cg.emitMatchDefaultArm(s, curCheckBlock, afterBlock, resAlloca, &anyFallthrough, cg.isExhaustiveArrayMatch(s))
	if defaultErr != nil {
		return nil, defaultErr
	}

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) isExhaustiveEnumMatch(s *ast.MatchStmt) bool {
	if len(s.Cases) == 0 {
		return false
	}

	var enumName string

	for _, c := range s.Cases {
		fa, ok := c.Pattern.(*ast.FieldAccess)
		if !ok {
			return false
		}

		id, ok := fa.Expr.(*ast.Identifier)
		if !ok {
			return false
		}

		key := id.Name + "." + fa.Field
		if _, isEnum := cg.enumValues[key]; !isEnum {
			return false
		}

		if enumName == "" {
			enumName = id.Name
		} else if enumName != id.Name {
			return false
		}
	}

	if enumName == "" {
		return false
	}
	// Count total members registered for this enum.
	prefix := enumName + "."
	total := 0

	for key := range cg.enumValues {
		if strings.HasPrefix(key, prefix) {
			total++
		}
	}

	return len(s.Cases) == total
}

func (cg *CodeGen) genMatch(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	return cg.genMatchWithResult(block, s, nil)
}

func (cg *CodeGen) genMatchWithResult(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	cg.prepareMatchGuards(s.Cases)

	if s.IsType {
		return cg.genMatchType(block, s)
	}

	cg.scanMatchForUnreachable(s)

	// ADT match: `case Ctor(bindings):` or bare nullary-variant identifier.
	for _, c := range s.Cases {
		if cg.isDataMatchPattern(c.Pattern) {
			return cg.genDataMatch(block, s, resAlloca)
		}
	}

	// Struct-pattern match: use if-else chain dispatch.
	for _, c := range s.Cases {
		if _, ok := c.Pattern.(*ast.StructPattern); ok {
			return cg.genStructMatch(block, s, resAlloca)
		}
	}

	// Array-pattern match: use if-else chain dispatch on array length.
	for _, c := range s.Cases {
		if _, ok := c.Pattern.(*ast.ArrayPattern); ok {
			return cg.genArrayMatch(block, s, resAlloca)
		}
	}

	expr, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	afterBlock := cg.newBlock("match.after")

	defaultBlock := afterBlock
	if s.Default != nil {
		defaultBlock = cg.newBlock("match.default")
	}

	// Build cases.
	var (
		cases      []*ir.Case
		caseBlocks []*ir.Block
	)

	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)

		pat, err := cg.genExpr(block, c.Pattern)
		if err != nil {
			return nil, err
		}

		if constPat, ok := pat.(constant.Constant); ok {
			intPat := cg.toConstInt(constPat, expr.Type())
			cases = append(cases, ir.NewCase(intPat, caseBlock))
		}
	}

	// Build switch. Use the natural type of the expression so case constants
	// always match the switch condition type (avoids i32 case vs i64 switch mismatch).
	switchExpr := expr
	switchType := expr.Type()
	// Re-build cases using the actual switch expression type so they match.
	for i, origCase := range cases {
		if constX, ok := origCase.X.(constant.Constant); ok {
			if target, ok2 := origCase.Target.(*ir.Block); ok2 {
				cases[i] = ir.NewCase(cg.toConstInt(constX, switchType), target)
			}
		}
	}

	block.NewSwitch(switchExpr, defaultBlock, cases...)

	// Generate case bodies. Track whether any arm fell through to afterBlock.
	// A match without a default is still exhaustive when every member of an
	// enum atom type is covered by an explicit case.
	anyFallthrough := s.Default == nil && !cg.isExhaustiveEnumMatch(s)

	genCaseBody := func(caseBlock *ir.Block, body *ast.Block) (*ir.Block, error) {
		if resAlloca != nil {
			// Expression mode: single-expression body (ExprStmt or nested MatchStmt).
			if body != nil && len(body.Stmts) == 1 {
				if expr := armExprNode(body.Stmts[0]); expr != nil {
					// Reset curBlock so a previous arm's inner-match advancement
					// doesn't pollute emission into caseBlock.
					cg.curBlock = caseBlock

					exprVal, err2 := cg.genExpr(caseBlock, expr)
					if err2 != nil {
						return nil, err2
					}

					if cg.curBlock != caseBlock {
						caseBlock = cg.curBlock
					}

					if exprVal != nil {
						resType := resAlloca.Type().(*irtypes.PointerType).ElemType
						caseBlock.NewStore(cg.coerce(caseBlock, exprVal, resType), resAlloca)
					}
				}
			}

			caseBlock.NewBr(afterBlock)

			return nil, nil
		}

		cg.curScope = newScope(cg.curScope)

		var err2 error

		caseBlock, _, err2 = cg.genStmt(caseBlock, body)

		// ARC: release case-body scope vars before falling through to afterBlock.
		cg.emitScopeRelease(caseBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		return caseBlock, err2
	}

	for i, c := range s.Cases {
		var caseBlock *ir.Block
		if i < len(caseBlocks) {
			caseBlock = caseBlocks[i]
		} else {
			caseBlock = cg.newBlock(fmt.Sprintf("match.case.%d", i))
		}

		caseBlock, err = genCaseBody(caseBlock, c.Body)
		if err != nil {
			return nil, err
		}

		if resAlloca != nil {
			anyFallthrough = true
		} else if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// Default.
	if s.Default != nil {
		defaultBlock, err = genCaseBody(defaultBlock, s.Default)
		if err != nil {
			return nil, err
		}

		if resAlloca != nil {
			anyFallthrough = true
		} else if defaultBlock != nil && defaultBlock.Term == nil {
			defaultBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// All arms terminated - afterBlock is unreachable; signal exhaustive termination.
	if !anyFallthrough {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) toConstInt(c constant.Constant, targetType irtypes.Type) *constant.Int {
	if ci, ok := c.(*constant.Int); ok {
		if it, ok2 := targetType.(*irtypes.IntType); ok2 {
			// Preserve the full big.Int magnitude when the target is at least
			// as wide as the source (e.g. i128 case against an i128 switch
			// expression). Calling X.Int64() here would silently truncate
			// 99999999999999999999 to its bottom 64 bits.
			if uint(it.BitSize) >= uint(ci.Typ.BitSize) {
				return &constant.Int{Typ: it, X: new(big.Int).Set(ci.X)}
			}

			return constant.NewInt(it, ci.X.Int64())
		}

		return ci
	}

	return constant.NewInt(irtypes.I64, 0)
}

// genAwaitMatch implements:
//
//	await match [a, b, c]:
//	  case [x, _, _] if guard: body
//	  case [_, y, _]: body
//	  default: body
//
// Without default: blocks until a future fires and a guard passes; re-blocks if
// a future fires but its guard fails; panics if all futures are exhausted with
// no guard passing.
//
// With default (Go select semantics): one non-blocking check; if nothing is
// actionable (no future done with a passing guard), runs the default body.
func (cg *CodeGen) genAwaitMatch(block *ir.Block, s *ast.AwaitMatchStmt) (*ir.Block, error) {
	cg.prepareAwaitMatchGuards(s.Cases)
	cg.ensureFiberRuntime()

	n := len(s.Futures)

	// Evaluate each future expression once and extract its PID.
	slots := make([]awMatchSlot, n)

	for i, fnode := range s.Futures {
		fval, err := cg.genExpr(block, fnode)
		if err != nil {
			return nil, fmt.Errorf("await match: future %d: %w", i, err)
		}

		sname := structNameFromValue(fval)
		if sname == "" || len(sname) <= 8 || sname[:8] != "Future__" {
			return nil, fmt.Errorf("await match: expression at index %d is not a Future[T] (got type %s)", i, fval.Type())
		}

		pidIdx := cg.fieldIndex(sname, "pid")
		if pidIdx < 0 {
			return nil, fmt.Errorf("await match: Future type %s has no pid field", sname)
		}

		pid := block.NewExtractValue(fval, uint64(pidIdx))

		retTypeName := sname[8:]

		var retLLVM irtypes.Type

		if retTypeName != "" && retTypeName != "Unit" {
			var rerr error

			retLLVM, rerr = cg.resolveSimpleType(retTypeName)
			if rerr != nil {
				retLLVM = nil
			}
		}

		slots[i] = awMatchSlot{val: fval, pid: pid, structName: sname, retType: retLLVM}
	}

	// Build a fixed-size [n x i64] PID array on the stack.
	pidArrayType := irtypes.NewArray(uint64(n), irtypes.I64)
	pidAlloca := block.NewAlloca(pidArrayType)

	for i, sl := range slots {
		gep := block.NewGetElementPtr(pidArrayType, pidAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(sl.pid, gep)
	}

	pidsPtr := block.NewGetElementPtr(pidArrayType, pidAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nConst := constant.NewInt(irtypes.I64, int64(n))

	// Ensure runtime function declarations.
	pollAnySkipFn := cg.ensureExternDecl("_tin_fiber_poll_any_skip", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
		}, false)

	afterBlock := cg.newBlock("awmatch.after")

	// skipAlloca: [n x i8] bitmask tracking slots whose guards failed.
	skipType := irtypes.NewArray(uint64(n), irtypes.I8)
	skipAlloca := block.NewAlloca(skipType)

	// Zero-initialize skip mask.
	for i := 0; i < n; i++ {
		gep := block.NewGetElementPtr(skipType, skipAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(constant.NewInt(irtypes.I8, 0), gep)
	}

	skipPtr := block.NewGetElementPtr(skipType, skipAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	// --- WITH default: one non-blocking poll pass ---
	if s.Default != nil {
		defaultBlock := cg.newBlock("awmatch.default")

		// Poll: find first done, non-skipped slot with a passing guard.
		// Linear scan through case arms; fall through to default if nothing actionable.
		checkBlock := block

		for i, c := range s.Cases {
			slotPid := slots[c.SlotIdx].pid
			doneCheckBlock := cg.newBlock(fmt.Sprintf("awmatch.donecheck.%d", i))
			nextArmBlock := cg.newBlock(fmt.Sprintf("awmatch.nextarm.%d", i))

			// Check FIBER_DONE for this slot via poll_any_skip on a single-element array.
			// Simpler: call _tin_fiber_poll_any_skip which already handles the table lock.
			// We build a 1-element pid array for the single-slot check.
			// Alternatively emit _tin_fiber_get_done(pid) - but we don't have that.
			// Use a temporary alloca with just this pid and a zero skip mask.
			singlePidAlloca := checkBlock.NewAlloca(irtypes.I64)
			checkBlock.NewStore(slotPid, singlePidAlloca)
			singleSkipAlloca := checkBlock.NewAlloca(irtypes.I8)
			checkBlock.NewStore(constant.NewInt(irtypes.I8, 0), singleSkipAlloca)

			idx := checkBlock.NewCall(pollAnySkipFn, singlePidAlloca,
				constant.NewInt(irtypes.I64, 1), singleSkipAlloca)
			isDone := checkBlock.NewICmp(enum.IPredEQ, idx, constant.NewInt(irtypes.I64, 0))
			checkBlock.NewCondBr(isDone, doneCheckBlock, nextArmBlock)

			checkBlock = doneCheckBlock

			// Slot is done. Bind result and check guard if present.
			cg.curScope = newScope(cg.curScope)

			okBlk, bindErr := cg.bindAwaitMatchSlot(checkBlock, c, slots[c.SlotIdx])
			if bindErr != nil {
				cg.curScope = cg.curScope.parent

				return nil, bindErr
			}

			armEntryBlock := okBlk

			if c.Guard != nil {
				guardVal, err := cg.genExpr(armEntryBlock, c.Guard)
				if err != nil {
					cg.curScope = cg.curScope.parent

					return nil, err
				}

				guardPassBlock := cg.newBlock(fmt.Sprintf("awmatch.guardpass.%d", i))
				armEntryBlock.NewCondBr(cg.toBool(armEntryBlock, guardVal), guardPassBlock, nextArmBlock)
				armEntryBlock = guardPassBlock
			}

			// Emit arm body.
			bodyBlock, _, err := cg.genStmt(armEntryBlock, c.Body)
			cg.curScope = cg.curScope.parent

			if err != nil {
				return nil, err
			}

			if bodyBlock != nil && bodyBlock.Term == nil {
				bodyBlock.NewBr(afterBlock)
			}

			checkBlock = nextArmBlock
		}

		// Nothing actionable: go to default.
		checkBlock.NewBr(defaultBlock)

		cg.curScope = newScope(cg.curScope)
		defBlock, _, err := cg.genStmt(defaultBlock, s.Default)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if defBlock != nil && defBlock.Term == nil {
			defBlock.NewBr(afterBlock)
		}

		return afterBlock, nil
	}

	// --- WITHOUT default: blocking loop ---
	// Loop: join_any -> poll -> dispatch; re-loop if guard fails; panic if exhausted.

	// anyWaiterType mirrors TinAnyWaiter in fiber.c:
	// { i64 waiter_pid, i32 fired (atomic), i32 pad, i64 result_idx, i64* pids, i64 n }
	anyWaiterType := irtypes.NewStruct(irtypes.I64, irtypes.I32, irtypes.I32, irtypes.I64,
		irtypes.NewPointer(irtypes.I64), irtypes.I64)
	anyWaiterAlloca := block.NewAlloca(anyWaiterType)
	_ = anyWaiterAlloca

	joinAnyFn := cg.ensureExternDecl("_tin_fiber_join_any", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
			ir.NewParam("my_hdl", irtypes.I8Ptr),
			ir.NewParam("aw", irtypes.I8Ptr),
		}, false)

	syncAwaitAnyFn := cg.ensureExternDecl("_tin_fiber_sync_await_any", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
		}, false)

	loopBlock := cg.newBlock("awmatch.loop")
	block.NewBr(loopBlock)

	// === loop body ===
	var resumeBlock *ir.Block

	if cg.inCoroFn {
		awPtr := loopBlock.NewBitCast(anyWaiterAlloca, irtypes.I8Ptr)
		loopBlock.NewCall(joinAnyFn, pidsPtr, nConst, skipPtr,
			cg.curCoroHdl, awPtr)
		resumeBlock = cg.emitSuspendPoint(loopBlock, cg.curCoroFrame)
	} else {
		// Non-async context: synchronous spin-wait.
		idx := loopBlock.NewCall(syncAwaitAnyFn, pidsPtr, nConst, skipPtr)
		_ = idx
		resumeBlock = loopBlock
	}

	// After resume: poll to find which slot fired.
	idx := resumeBlock.NewCall(pollAnySkipFn, pidsPtr, nConst, skipPtr)

	// Check exhaustion: idx == -1 means all slots skipped.
	exhaustedBlock := cg.newBlock("awmatch.exhausted")
	dispatchBlock := cg.newBlock("awmatch.dispatch")
	resumeBlock.NewCondBr(
		resumeBlock.NewICmp(enum.IPredEQ, idx, constant.NewInt(irtypes.I64, -1)),
		exhaustedBlock, dispatchBlock)

	// Exhaustion: panic.
	exhaustMsg := cg.newGlobalString("await match: all futures exhausted, no arm matched")
	exhaustedBlock.NewCall(cg.ensurePanicFn(), exhaustMsg)

	retType := cg.curFn.Sig.RetType
	if irtypes.IsVoid(retType) {
		exhaustedBlock.NewRet(nil)
	} else {
		exhaustedBlock.NewRet(cg.zeroValue(retType))
	}

	// Dispatch: if-else chain over case arms.
	checkBlock := dispatchBlock

	for i, c := range s.Cases {
		matchBlock := cg.newBlock(fmt.Sprintf("awmatch.arm.%d", i))
		noMatchBlock := cg.newBlock(fmt.Sprintf("awmatch.nomatch.%d", i))

		slotConst := constant.NewInt(irtypes.I64, int64(c.SlotIdx))
		isThisSlot := checkBlock.NewICmp(enum.IPredEQ, idx, slotConst)
		checkBlock.NewCondBr(isThisSlot, matchBlock, noMatchBlock)

		cg.curScope = newScope(cg.curScope)

		okBlk, bindErr := cg.bindAwaitMatchSlot(matchBlock, c, slots[c.SlotIdx])
		if bindErr != nil {
			cg.curScope = cg.curScope.parent

			return nil, bindErr
		}

		armEntry := okBlk

		// Guard check.
		if c.Guard != nil {
			guardVal, err := cg.genExpr(armEntry, c.Guard)
			if err != nil {
				cg.curScope = cg.curScope.parent

				return nil, err
			}

			guardPassBlock := cg.newBlock(fmt.Sprintf("awmatch.gpass.%d", i))
			guardFailBlock := cg.newBlock(fmt.Sprintf("awmatch.gfail.%d", i))
			armEntry.NewCondBr(cg.toBool(armEntry, guardVal), guardPassBlock, guardFailBlock)

			// Guard fail: mark slot as skipped, re-loop.
			skipGep := guardFailBlock.NewGetElementPtr(skipType, skipAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(c.SlotIdx)))
			guardFailBlock.NewStore(constant.NewInt(irtypes.I8, 1), skipGep)
			guardFailBlock.NewBr(loopBlock)

			armEntry = guardPassBlock
		}

		// Emit body.
		bodyBlock, _, err := cg.genStmt(armEntry, c.Body)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if bodyBlock != nil && bodyBlock.Term == nil {
			bodyBlock.NewBr(afterBlock)
		}

		checkBlock = noMatchBlock
	}

	// No arm matched this idx (shouldn't happen if patterns are exhaustive per slot,
	// but handle gracefully: re-loop).
	checkBlock.NewBr(loopBlock)

	return afterBlock, nil
}

// awMatchSlot holds per-slot data for genAwaitMatch.
type awMatchSlot struct {
	val        value.Value
	pid        value.Value
	structName string
	retType    irtypes.Type // nil for void/Unit futures
}

// bindAwaitMatchSlot emits panic-check + result unboxing for one await match arm.
// Returns the block to continue emitting into (the "ok" block after panic check).
func (cg *CodeGen) bindAwaitMatchSlot(block *ir.Block, c ast.AwaitMatchCase, sl awMatchSlot) (*ir.Block, error) {
	// Panic check (same pattern as single await).
	pmsg := block.NewCall(cg.fiberGetPanicMsgFn, sl.pid)
	panicked := block.NewICmp(enum.IPredNE, pmsg, constant.NewNull(irtypes.I8Ptr))
	panicBlk := cg.newBlock(fmt.Sprintf("awmatch.panic.s%d", c.SlotIdx))
	okBlk := cg.newBlock(fmt.Sprintf("awmatch.ok.s%d", c.SlotIdx))
	block.NewCondBr(panicked, panicBlk, okBlk)

	panicBlk.NewCall(cg.ensurePanicFn(), pmsg)

	retType := cg.curFn.Sig.RetType
	if irtypes.IsVoid(retType) {
		panicBlk.NewRet(nil)
	} else {
		panicBlk.NewRet(cg.zeroValue(retType))
	}

	// Unbox result and bind to BindName (if not wildcard / void).
	if sl.retType != nil && c.BindName != "" {
		rawPtr := okBlk.NewCall(cg.fiberGetResultFn, sl.pid)
		typedPtr := okBlk.NewBitCast(rawPtr, irtypes.NewPointer(sl.retType))
		result := okBlk.NewLoad(sl.retType, typedPtr)
		okBlk.NewCall(cg.ensureFree(), rawPtr)
		alloca := okBlk.NewAlloca(sl.retType)
		okBlk.NewStore(result, alloca)
		cg.curScope.set(c.BindName, &scopeEntry{val: alloca, isAlloc: true})
	}

	return okBlk, nil
}

// genMatchType handles "match a.(type):" dispatch for tagged unions.
// Each case "case i T:" extracts the payload as variable i of type T.
func (cg *CodeGen) genMatchType(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	// s.Expr is TypeAssertExpr{Expr: a, IsType: true}; genExpr just returns a.
	val, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, cg.nodeErr(s, "match .(type): nil expression")
	}

	unionName := cg.typeNameOf(val.Type())

	members, isUnion := cg.unionTypeMembers[unionName]
	if !isUnion {
		return nil, cg.nodeErr(s, "match .(type) requires a tagged union type, got %s", unionName)
	}

	st := val.Type().(*irtypes.StructType)
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)
	// Extract tag (field 1, i8; field 0 is i32 type_id), zero-extend to i64 for switch.
	tagGEP := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagVal := block.NewLoad(irtypes.I8, tagGEP)
	tagI64 := block.NewZExt(tagVal, irtypes.I64)

	afterBlock := cg.newBlock("match.after")

	defaultBlock := afterBlock
	if s.Default != nil {
		defaultBlock = cg.newBlock("match.default")
	}

	// Build cases: determine tag for each case from VarType or StructPattern.TypeName.
	var (
		cases      []*ir.Case
		caseBlocks []*ir.Block
	)

	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)
		tag := int64(0)

		// Determine the target type: from VarType or from StructPattern.TypeName.
		var targetType ast.TypeExpr

		if c.VarType != nil {
			targetType = c.VarType
		} else if sp, ok := c.Pattern.(*ast.StructPattern); ok {
			targetType = &ast.SimpleType{Name: sp.TypeName}
		}

		if targetType != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(targetType)
			if err2 == nil {
				for j, te := range members {
					lt, err3 := cg.tinTypeToLLVM(te)
					if err3 != nil {
						continue
					}

					if lt.Equal(targetLLVM) {
						tag = int64(j)

						break
					}
				}
			}
		}

		cases = append(cases, ir.NewCase(constant.NewInt(irtypes.I64, tag), caseBlock))
	}

	block.NewSwitch(tagI64, defaultBlock, cases...)

	// Generate case bodies.
	anyFallthrough := false

	for i, c := range s.Cases {
		caseBlock := caseBlocks[i]
		cg.curScope = newScope(cg.curScope)

		// Bind payload: either VarName+VarType or StructPattern fields.
		if c.VarName != "" && c.VarType != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(c.VarType)
			if err2 == nil {
				payloadGEP := caseBlock.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := caseBlock.NewAlloca(targetLLVM)
				payloadVal := caseBlock.NewLoad(targetLLVM, payloadPtr)
				caseBlock.NewStore(payloadVal, payloadAlloca)
				cg.curScope.set(c.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true})
			}
		} else if sp, ok := c.Pattern.(*ast.StructPattern); ok {
			structLLVM, err2 := cg.tinTypeToLLVM(&ast.SimpleType{Name: sp.TypeName})
			if err2 == nil {
				payloadGEP := caseBlock.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(structLLVM))
				payloadAlloca := caseBlock.NewAlloca(structLLVM)
				payloadVal := caseBlock.NewLoad(structLLVM, payloadPtr)
				caseBlock.NewStore(payloadVal, payloadAlloca)

				for _, field := range sp.Fields {
					if field.IsWild || field.Literal != nil {
						continue
					}

					fieldIdx := cg.fieldIndex(sp.TypeName, field.Name)
					if fieldIdx < 0 {
						continue
					}

					var fieldType irtypes.Type

					if st2, ok2 := structLLVM.(*irtypes.StructType); ok2 && fieldIdx < len(st2.Fields) {
						fieldType = st2.Fields[fieldIdx]
					} else {
						fieldType = irtypes.I64
					}

					gep := caseBlock.NewGetElementPtr(structLLVM, payloadAlloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fv := caseBlock.NewLoad(fieldType, gep)
					fa := caseBlock.NewAlloca(fieldType)
					caseBlock.NewStore(fv, fa)
					cg.curScope.set(field.Name, &scopeEntry{val: fa, isAlloc: true})
				}
			}
		}

		// Apply guard if present.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(caseBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			bodyBlock := cg.newBlock(fmt.Sprintf("match.case.%d.body", i))
			caseBlock.NewCondBr(cg.toBool(caseBlock, guardVal), bodyBlock, afterBlock)

			anyFallthrough = true // guard failure goes to afterBlock
			caseBlock = bodyBlock
		}

		caseBlock, _, err = cg.genStmt(caseBlock, c.Body)

		// ARC: release case-body scope vars before falling through to afterBlock.
		cg.emitScopeRelease(caseBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// Default.
	if s.Default != nil {
		cg.curScope = newScope(cg.curScope)
		defaultBlock, _, err = cg.genStmt(defaultBlock, s.Default)

		// ARC: release default-body scope vars before falling through.
		cg.emitScopeRelease(defaultBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if defaultBlock != nil && defaultBlock.Term == nil {
			defaultBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// All arms terminated - afterBlock is unreachable; signal exhaustive termination.
	if !anyFallthrough {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}
