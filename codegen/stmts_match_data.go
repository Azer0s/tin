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
	// `match await f:` lowers the await into fresh blocks; continue
	// scrutinee handling on the post-await block, not the pre-await
	// one (which is already terminated by the await's br).
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
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

	outerSt := cg.structTypeFor(CanonKey(adtName))
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
	tagI64 := block.NewLoad(irtypes.I64, tagGEP)

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

		return nil, fmt.Errorf("%d:%d: non-exhaustive match on %s: no arm matches %s; add the missing variant or a \"default:\" arm",
			pos.Line, pos.Col, adtName, witness)
	}

	defaultBlock := cg.newBlock("match.default")

	var cases []*ir.Case

	seenTags := make(map[int64]bool, len(s.Cases))

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
			constant.NewInt(irtypes.I64, vi.Tag), caseBlock))

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
				// For embedded named structs whose payload itself
				// contains RC fields (e.g. Result.Ok(event_with_time)
				// where event_with_time has a string), the binding
				// must take its own retain regardless of scrutinee
				// ownership.  A chained method call on a Result
				// temporary (e.g. `verify(...).unwrap()`), or a free
				// fn that consumes one (`result::unwrap(make())`),
				// releases the temporary right after the call
				// returns, so without an extra retain the returned
				// struct's RC fields are read by the caller after
				// the temporary's release has freed them.  The retain
				// pairs with `entry.ownsIfaceData`-style scope-exit
				// bookkeeping below so the net RC delta stays zero
				// across every match shape (named scrutinee,
				// temporary scrutinee, function-arg scrutinee).
				rcTracked := !f.IsWeak && isRCTrackedType(fieldTy)
				owningStruct := !f.IsWeak && wantScrutRelease &&
					!isRCTrackedType(fieldTy) && cg.elemNeedsRelease(fieldTy)
				transferredFromBorrow := !f.IsWeak && !wantScrutRelease &&
					!isRCTrackedType(fieldTy) && cg.elemNeedsRelease(fieldTy)

				if rcTracked {
					// emitRetain is a no-op for trait fat-ptr values
					// (extractRCDataPtr doesn't know about iface
					// shape, walkRCStructFields skips synthetic iface
					// structs).  Retain the data ptr directly so
					// `case Err(e): return Err(e)` keeps the heap rc
					// balanced against the source scrutinee's release;
					// pairs with the wrapDataVariant retainMask path
					// for the rewrap construction.  Gated on
					// `wantScrutRelease`: the retain only makes sense
					// when the source scrutinee is about to be
					// released (owned rvalue or temp call result);
					// for a borrow-shaped scrutinee (Identifier /
					// FieldAccess) the source's own scope-exit keeps
					// the data alive, and an extra retain would leak
					// once per match-arm fire because no matching
					// release runs on the iface struct value at arm
					// scope exit.
					if isTraitFatPtrShape(fieldVal.Type()) {
						if wantScrutRelease {
							dataPtr := caseBlock.NewExtractValue(fieldVal, 0)
							caseBlock.NewCall(cg.ensureRetain(), dataPtr)
						}
					} else {
						cg.emitRetain(caseBlock, fieldVal)
					}
				} else if owningStruct {
					cg.emitStructFieldRetain(caseBlock, fieldVal)
				} else if transferredFromBorrow {
					// Borrow-shaped scrutinee (param/function-arg/copy
					// expression) whose payload is a struct with RC
					// content.  Retain so the binding keeps the RC
					// fields alive past whatever the caller does with
					// the borrow.  Pairs with the caller-side ADT
					// rvalue release in emitCallArgReleaseForRet so
					// `let f = unwrap(pipe())` (temp scrutinee) and
					// `let c = vr.method()` (live local scrutinee)
					// both stay rc-balanced.
					cg.emitStructFieldRetain(caseBlock, fieldVal)
				}

				alloca := caseBlock.NewAlloca(fieldVal.Type())
				caseBlock.NewStore(fieldVal, alloca)

				entry := &scopeEntry{val: alloca, isAlloc: true}
				// noRelease semantics:
				//   - rcTracked (string/array/any/fn closure): retained on
				//     extract; the binding owns its own RC contribution and
				//     must release at scope exit.
				//   - owningStruct: retained because the scrutinee owns
				//     the payload and will release it; the binding owns
				//     its own retain, must release symmetrically.
				//   - transferredFromBorrow: retained because the caller
				//     may release the (temporary) scrutinee right after
				//     the enclosing call returns -- the retain protects
				//     the returned struct's RC fields long enough for the
				//     receiving binding to take ownership.  The binding
				//     itself must release at scope exit to balance the
				//     retain when the arm body doesn't transfer v
				//     out (e.g. `case Ok(v): claims_val = v` plus a
				//     `return Ok(...)` later -- the assignment retains
				//     independently, but our retain still needs a pair).
				//   - else: binding is a pure borrow of scrutinee-owned
				//     storage; suppress the scope-exit release.
				if !rcTracked && !owningStruct && !transferredFromBorrow {
					entry.noRelease = true
				}
				// Iface fat-ptr with borrow scrutinee: the retain above is
				// gated on wantScrutRelease, so when the scrutinee owns the
				// data (Identifier / FieldAccess source) no retain fired.
				// The scope-exit release would then decrement an rc the
				// binding never bumped, frees the iface block while the
				// scrutinee still references it, and the next allocator
				// reuse trips on the dangling pointer.  Skip the release
				// to mirror the retain gate.
				if rcTracked && isTraitFatPtrShape(fieldVal.Type()) && !wantScrutRelease {
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
			caseBlock.NewCondBr(cg.toBoolImplicit(caseBlock, guardVal), guardedBody, defaultBlock)

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

// genTupleMatch handles `match (e1, e2, ...): case (p1, p2, ...):` shapes.
// Each slot pattern is checked independently against the corresponding
// tuple slot; the AND of every check selects the arm.  Supported slot
// patterns: bare identifier (binds), `_` (wildcard), integer / string /
// bool / char literal (equality), ADT ctor (e.g. Ok(x), None, Some(v)).
// Nested tuple patterns also flow through this path recursively.
func (cg *CodeGen) genTupleMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	scrutType := scrutinee.Type()
	if pt, ok := scrutType.(*irtypes.PointerType); ok {
		scrutinee = block.NewLoad(pt.ElemType, scrutinee)
		scrutType = pt.ElemType
	}

	tupSt, isStruct := scrutType.(*irtypes.StructType)
	if !isStruct {
		return nil, fmt.Errorf("genTupleMatch: scrutinee type %s is not a tuple struct", cg.fmtArgType(scrutType))
	}

	tupName := tupSt.Name()
	if tupName == "" || !strings.HasPrefix(tupName, "Tuple__") {
		return nil, fmt.Errorf("genTupleMatch: scrutinee type %s is not a Tuple__... monomorphization", cg.fmtArgType(scrutType))
	}

	scrutAlloca := block.NewAlloca(scrutType)
	block.NewStore(scrutinee, scrutAlloca)

	// User-visible slots start after the type_id + any vtable
	// pointers prefix; tuple struct layout matches the regular
	// struct layout used by userFieldOffset.
	slotOffset := cg.userFieldOffset(tupName)
	slotCount := len(tupSt.Fields) - slotOffset

	afterBlock := cg.newBlock("match.after")
	anyFallthrough := false

	curCheckBlock := block

	for i, c := range s.Cases {
		nextCaseBlock := cg.newBlock(fmt.Sprintf("tuple.next.%d", i))
		bodyBlock := cg.newBlock(fmt.Sprintf("tuple.case.%d", i))

		// Open a fresh scope so per-arm pattern bindings don't leak.
		cg.curScope = newScope(cg.curScope)

		checkBlock := curCheckBlock

		// Wildcard catch-all: identifier "_" or a plain identifier with
		// no slot decomposition - treat as default arm.
		if id, ok := c.Pattern.(*ast.Identifier); ok && id.Name == "_" {
			checkBlock.NewBr(bodyBlock)

			_, bodyErr := cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
			cg.curScope = cg.curScope.parent

			if bodyErr != nil {
				return nil, bodyErr
			}

			nextCaseBlock.NewUnreachable()

			curCheckBlock = nextCaseBlock

			continue
		}

		tp, isTuple := c.Pattern.(*ast.TupleLit)
		if !isTuple {
			cg.curScope = cg.curScope.parent

			return nil, fmt.Errorf("genTupleMatch: case %d pattern is not a tuple literal (got %T)", i, c.Pattern)
		}

		if len(tp.Elems) != slotCount {
			cg.curScope = cg.curScope.parent

			return nil, fmt.Errorf("genTupleMatch: case %d has %d tuple elements, scrutinee has %d", i, len(tp.Elems), slotCount)
		}

		var checkErr error

		for j, elemPat := range tp.Elems {
			fieldIdx := slotOffset + j
			slotGEP := checkBlock.NewGetElementPtr(scrutType, scrutAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			slotType := tupSt.Fields[fieldIdx]

			checkBlock, checkErr = cg.emitTupleSlotPatternCheck(checkBlock, nextCaseBlock, slotGEP, slotType, elemPat)
			if checkErr != nil {
				cg.curScope = cg.curScope.parent

				return nil, checkErr
			}
		}

		if c.Guard != nil {
			guardVal, gErr := cg.genExpr(checkBlock, c.Guard)
			if gErr != nil {
				cg.curScope = cg.curScope.parent

				return nil, gErr
			}

			checkBlock.NewCondBr(cg.toBoolImplicit(checkBlock, guardVal), bodyBlock, nextCaseBlock)
		} else {
			checkBlock.NewBr(bodyBlock)
		}

		_, bodyErr := cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
		cg.curScope = cg.curScope.parent

		if bodyErr != nil {
			return nil, bodyErr
		}

		curCheckBlock = nextCaseBlock
	}

	// Default arm or fallthrough.
	_, defaultErr := cg.emitMatchDefaultArm(s, curCheckBlock, afterBlock, resAlloca, &anyFallthrough, false)
	if defaultErr != nil {
		return nil, defaultErr
	}

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

// emitTupleSlotPatternCheck dispatches a pattern check against one
// tuple slot.  The slot pointer is `slotGEP` (a pointer to slotType in
// the scrutinee alloca).  Failure branches to `failBlock`; success
// returns the block to continue checking in (callers chain calls so
// later slot checks join the success path).
//
// Supported patterns:
//   - Identifier "_": wildcard, no check, no bind.
//   - Identifier <other>: bind slot value to the name.
//   - IntLit / FloatLit / StringLit / CharLit / BoolLit: equality.
//   - CallExpr Variant(args): ADT variant tag check + sub-pattern
//     bindings via genDataMatch-equivalent inline logic.
//   - Identifier matching a nullary ADT variant: tag check.
func (cg *CodeGen) emitTupleSlotPatternCheck(block *ir.Block, failBlock *ir.Block, slotGEP value.Value, slotType irtypes.Type, pat ast.Node) (*ir.Block, error) {
	switch p := pat.(type) {
	case *ast.Identifier:
		if p.Name == "_" {
			return block, nil
		}
		// ADT nullary variant identifier match (e.g. `case (None, x):`).
		if cg.isDataMatchPattern(p) {
			return cg.emitTupleSlotAdtCheck(block, failBlock, slotGEP, slotType, p)
		}
		// Plain binder: load and bind.
		val := block.NewLoad(slotType, slotGEP)
		alloca := block.NewAlloca(slotType)
		block.NewStore(val, alloca)

		if isRCTrackedType(slotType) {
			cg.emitRetain(block, val)
		}

		entry := &scopeEntry{val: alloca, isAlloc: true}
		if !isRCTrackedType(slotType) {
			entry.noRelease = true
		}

		cg.curScope.set(p.Name, entry)

		return block, nil
	case *ast.CallExpr:
		if cg.isDataMatchPattern(p) {
			return cg.emitTupleSlotAdtCheck(block, failBlock, slotGEP, slotType, p)
		}

		return nil, fmt.Errorf("tuple-slot pattern: call expression that is not an ADT variant constructor")
	case *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.CharLit, *ast.BoolLit:
		val := block.NewLoad(slotType, slotGEP)

		litVal, err := cg.genExpr(block, p)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		litVal = cg.coerce(block, litVal, slotType)

		var cmp value.Value

		switch slotType.(type) {
		case *irtypes.IntType:
			cmp = block.NewICmp(enum.IPredEQ, val, litVal)
		case *irtypes.FloatType:
			cmp = block.NewFCmp(enum.FPredOEQ, val, litVal)
		default:
			return nil, fmt.Errorf("tuple-slot literal pattern: unsupported slot type %s", cg.fmtArgType(slotType))
		}

		ok := cg.newBlock("tuple.lit.ok")
		block.NewCondBr(cmp, ok, failBlock)

		return ok, nil
	case *ast.TupleLit:
		// Nested tuple pattern: recurse.
		innerSt, isStruct := slotType.(*irtypes.StructType)
		if !isStruct || !strings.HasPrefix(innerSt.Name(), "Tuple__") {
			return nil, fmt.Errorf("nested tuple pattern in slot of non-tuple type %s", cg.fmtArgType(slotType))
		}

		if len(p.Elems) != len(innerSt.Fields) {
			return nil, fmt.Errorf("nested tuple pattern: %d elements vs slot type's %d", len(p.Elems), len(innerSt.Fields))
		}

		for j, sub := range p.Elems {
			subGEP := block.NewGetElementPtr(slotType, slotGEP,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))

			var err error

			block, err = cg.emitTupleSlotPatternCheck(block, failBlock, subGEP, innerSt.Fields[j], sub)
			if err != nil {
				return nil, err
			}
		}

		return block, nil
	}

	return nil, fmt.Errorf("tuple-slot pattern: unsupported pattern kind %T", pat)
}

// emitTupleSlotAdtCheck verifies that the slot (a Result/Option/etc.
// ADT value) matches an ADT variant pattern.  Decomposes the slot
// into discriminant + payload like genDataMatch does, but inline so
// the per-arm failure path threads through the next-case block
// rather than a per-data switch.
func (cg *CodeGen) emitTupleSlotAdtCheck(block *ir.Block, failBlock *ir.Block, slotGEP value.Value, slotType irtypes.Type, pat ast.Node) (*ir.Block, error) {
	adtSt, isStruct := slotType.(*irtypes.StructType)
	if !isStruct {
		return nil, fmt.Errorf("ADT pattern on non-ADT slot type %s", cg.fmtArgType(slotType))
	}

	adtName := adtSt.Name()
	if adtName == "" {
		return nil, fmt.Errorf("ADT pattern on anonymous struct slot %s", cg.fmtArgType(slotType))
	}

	variantName := dataPatternVariantName(pat)

	vi := cg.dataVariantInfoFor(adtName, variantName)
	if vi == nil {
		return nil, fmt.Errorf("data %s has no variant %q", adtName, variantName)
	}

	tagGEP := block.NewGetElementPtr(adtSt, slotGEP,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := block.NewLoad(irtypes.I64, tagGEP)
	expectedTag := constant.NewInt(irtypes.I64, vi.Tag)
	cmp := block.NewICmp(enum.IPredEQ, tagI64, expectedTag)

	matchBlk := cg.newBlock("tuple.adt.match")
	block.NewCondBr(cmp, matchBlk, failBlock)

	// Bind payload fields.
	binders := dataPatternBinders(pat)
	if len(binders) != len(vi.Fields) {
		return nil, fmt.Errorf("data %s.%s expects %d binding(s), got %d", adtName, variantName, len(vi.Fields), len(binders))
	}

	if len(vi.Fields) > 0 {
		payloadGEP := matchBlk.NewGetElementPtr(adtSt, slotGEP,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := matchBlk.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			name := binders[fi]
			if name == "" || name == "_" {
				continue
			}

			fieldPtr := matchBlk.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldTy := vi.PayloadType.Fields[fi]
			fieldVal := matchBlk.NewLoad(fieldTy, fieldPtr)

			if !f.IsWeak && isRCTrackedType(fieldTy) {
				cg.emitRetain(matchBlk, fieldVal)
			}

			alloca := matchBlk.NewAlloca(fieldVal.Type())
			matchBlk.NewStore(fieldVal, alloca)

			entry := &scopeEntry{val: alloca, isAlloc: true}
			if !isRCTrackedType(fieldTy) {
				entry.noRelease = true
			}

			cg.curScope.set(name, entry)
		}
	}

	return matchBlk, nil
}
