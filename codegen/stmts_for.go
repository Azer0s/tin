package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genFor(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	switch s.Kind {
	case ast.ForCStyle:
		return cg.genForCStyle(block, s)
	case ast.ForIn:
		return cg.genForIn(block, s)
	case ast.ForWhile:
		return cg.genForWhile(block, s)
	}

	return block, nil
}

// genForWhile generates a condition-only while-style loop: for <cond>: body
func (cg *CodeGen) genForWhile(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	// Simplify the condition. Allow a bare `for true:` infinite-loop idiom
	// to stay quiet; any other fold-to-constant case emits the warning.
	s.Cond = cg.prepareBoolCond(s.Cond, "for", true)

	headerBlock := cg.newBlock("for.cond")
	bodyBlock := cg.newBlock("for.body")
	afterBlock := cg.newBlock("for.after")

	// headerBlock is the loop's stable back-edge target; condBlock may
	// advance through short-circuit && / || in the cond expression
	// (genShortCircuit moves cg.curBlock to its merge block). The
	// back-edge from the body must point at headerBlock, NOT the
	// advanced condBlock -- otherwise the body becomes a third
	// predecessor of the && merge block whose phi has no incoming for
	// it, producing undef cond on the next iteration (= infinite loop).
	condBlock := headerBlock

	brToCond := block.NewBr(headerBlock)

	// Condition - set curBlock so we can detect if await/yield changed it.
	cg.curBlock = condBlock

	cond, err := cg.genExpr(condBlock, s.Cond)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != condBlock {
		condBlock = cg.curBlock
	}

	cg.curBlock = nil
	cond = cg.toBoolImplicit(condBlock, cond)

	// Detect constant-true condition (e.g. `for true:` / `loop:`).
	// When the condition is always true, the loop only exits via break.
	// Emit an unconditional branch so the false-path to afterBlock has no
	// predecessor - this prevents dead-code blocks from being populated
	// with invalid SSA (e.g. loads of loop-body allocas) and avoids
	// duplicate coro.suspend(final=true) in coroutine functions.
	isConstTrue := false
	if ci, ok := cond.(*constant.Int); ok && ci.X.IsInt64() && ci.X.Int64() == 1 {
		isConstTrue = true

		condBlock.NewBr(bodyBlock)
	} else {
		condBlock.NewCondBr(cond, bodyBlock, afterBlock)
	}

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// Body - push a fresh scope so that `let` declarations inside the loop
	// body are tracked separately from the outer function scope.  This allows
	// emitScopeRelease to free RC-tracked locals at the end of each iteration.
	prevScope := cg.curScope
	cg.curScope = newScope(prevScope)
	cg.pushBreakTarget(afterBlock)

	var endBody *ir.Block

	endBody, _, err = cg.genStmt(bodyBlock, s.Body)

	breakUsed := cg.popBreakTarget()
	if err != nil {
		cg.curScope = prevScope

		return nil, err
	}

	if endBody != nil && endBody.Term == nil {
		// Release loop-body-local RC vars before jumping back to the condition.
		cg.emitScopeRelease(endBody, cg.curScope)

		// Back-edge to the stable loop header, not the (possibly advanced)
		// cond eval block -- see headerBlock comment above.
		if cg.curFnAutoYield {
			cg.genYieldAutoAt(endBody, headerBlock)
		} else {
			endBody.NewBr(headerBlock)
		}
	}

	cg.curScope = prevScope

	// For constant-true loops: if no break statement branched to afterBlock,
	// it is unreachable dead code.  Return nil so callers know the code path
	// is terminated and skip emitting a default return.
	if isConstTrue && !breakUsed {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) genForCStyle(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	headerBlock := cg.newBlock("for.cond")
	bodyBlock := cg.newBlock("for.body")
	postBlock := cg.newBlock("for.post")
	afterBlock := cg.newBlock("for.after")

	// headerBlock is the loop's stable back-edge target; condBlock may
	// advance through short-circuit && / || in the cond expression.
	condBlock := headerBlock

	// Init: push a scope so the loop variable is scoped to the loop.
	cg.curScope = newScope(cg.curScope)

	if s.Init != nil {
		var err error

		block, _, err = cg.genStmt(block, s.Init)
		if err != nil {
			return nil, err
		}
	} else if s.VarName != "" && s.VarType != nil {
		// C-style for without explicit initializer: `for let i T; cond; post:`
		// Declare the loop variable zero-initialized so it is in scope.
		zeroDecl := &ast.VarDecl{Name: s.VarName, Type: s.VarType}

		var err error

		block, err = cg.genVarDecl(block, zeroDecl)
		if err != nil {
			return nil, err
		}
	}

	if block.Term == nil {
		block.NewBr(condBlock)
	}

	// Cond
	if s.Cond != nil {
		s.Cond = cg.prepareBoolCond(s.Cond, "for", true)

		cg.curBlock = condBlock

		cond, err := cg.genExpr(condBlock, s.Cond)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != condBlock {
			condBlock = cg.curBlock
		}

		cg.curBlock = nil
		cond = cg.toBoolImplicit(condBlock, cond)
		condBlock.NewCondBr(cond, bodyBlock, afterBlock)
	} else {
		condBlock.NewBr(bodyBlock)
	}

	cg.attachForLoopDbg(s.Pos(), block.Term, condBlock)

	// Body
	cg.curScope = newScope(cg.curScope)

	var err error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, err = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	// ARC: release loop body scope vars before back-edge.
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if err != nil {
		return nil, err
	}

	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyBlock.NewBr(postBlock)
	}

	// Post
	if s.Post != nil {
		_, _, err = cg.genStmt(postBlock, s.Post)
		if err != nil {
			return nil, err
		}
	}

	if postBlock.Term == nil {
		// Back-edge to the stable loop header, not the (possibly advanced)
		// cond eval block.
		if cg.curFnAutoYield {
			cg.genYieldAutoAt(postBlock, headerBlock)
		} else {
			postBlock.NewBr(headerBlock)
		}
	}

	// ARC: release init scope vars (e.g. loop counter) in the after block.
	cg.emitScopeRelease(afterBlock, cg.curScope)
	cg.curScope = cg.curScope.parent // pop loop scope

	return afterBlock, nil
}

func (cg *CodeGen) genForIn(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	// `for ref` rejects ranges -- a range produces values, not slots,
	// so there's nothing for ref to alias.
	if s.IsRef {
		if _, isRange := s.Iter.(*ast.RangeExpr); isRange {
			return nil, cg.nodeErr(s, "for ref: cannot ref-iterate a range; range produces values, not slots")
		}

		if bin, ok := s.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
			return nil, cg.nodeErr(s, "for ref: cannot ref-iterate a range; range produces values, not slots")
		}

		// Reject ref-iteration over a `const` array (top-level or
		// block-level). Top-level consts live in read-only storage;
		// block-level consts are immutable by language convention.
		// Mutable bindings (`let` block-level, `var` top-level) are
		// fine -- ref aliases their slots.
		if id, ok := s.Iter.(*ast.Identifier); ok {
			if cg.topLevelConstNames[id.Name] {
				return nil, cg.nodeErr(s, "for ref: cannot ref-iterate top-level const %q (immutable storage)", id.Name)
			}

			if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
				if entry.declaredConst {
					return nil, cg.nodeErr(s, "for ref: cannot ref-iterate %q because it was declared with const; drop the const if you need to mutate elements",
						id.Name)
				}
			}
		}
	}

	// Check if iter is a RangeExpr or a BinExpr with op ".." (start..end).
	if rng, ok := s.Iter.(*ast.RangeExpr); ok {
		return cg.genForRange(block, s, rng)
	}

	if bin, ok := s.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
		return cg.genForRange(block, s, &ast.RangeExpr{Start: bin.Left, End: bin.Right})
	}

	// Iterate over a dynamic array: {ptr*, len}.
	iterVal, err := cg.genExpr(block, s.Iter)
	if err != nil {
		return nil, err
	}

	// iter[t] trait: struct (or fat-ptr) implementing iter[T] - use vtable.
	if iterFatPtr, instKey, ok, ownsData := cg.tryCoerceToIter(block, iterVal); ok {
		return cg.genForIterTrait(block, s, iterFatPtr, instKey, ownsData)
	}

	// Rune iteration over strings: for r rune in s decodes UTF-8 codepoints.
	if s.VarType != nil {
		if st, ok := s.VarType.(*ast.SimpleType); ok && st.Name == "rune" {
			if isStringType(iterVal.Type()) {
				return cg.genForInStringRunes(block, s, iterVal)
			}
		}
	}

	// Get element type.
	// For string fat-pointers ({i8*, i64}), default element type is i8 (byte).
	// For fat arrays {T*, i64}, infer T from the pointer field.
	var elemType irtypes.Type = irtypes.I64
	if isStringType(iterVal.Type()) {
		elemType = irtypes.I8
	} else if isFatArrayPtr(iterVal.Type()) {
		if pt, ok := iterVal.Type().(*irtypes.StructType).Fields[0].(*irtypes.PointerType); ok {
			elemType = pt.ElemType
		}
	}

	if s.VarType != nil {
		elemType, err = cg.tinTypeToLLVM(s.VarType)
		if err != nil {
			return nil, err
		}
	}

	condBlock := cg.newBlock("forin.cond")
	bodyBlock := cg.newBlock("forin.body")
	afterBlock := cg.newBlock("forin.after")

	// Extract length and data pointer from fat ptr.  Fat-ptr is either
	// the 2-field string shape `{i8*, i64}` or the 3-field array shape
	// `{T*, i64, i64}`; the iteration code only touches fields 0 and 1
	// so it works for both.
	fatPtrType := fatArrayPtrType(elemType)

	// Alloca to store the fat ptr.
	fatAlloca := block.NewAlloca(iterVal.Type())
	block.NewStore(iterVal, fatAlloca)

	// Extract len.
	// Try to get it as struct.
	lenAlloca := block.NewAlloca(irtypes.I64)
	ptrAlloca := block.NewAlloca(irtypes.NewPointer(elemType))

	if st, ok := iterVal.Type().(*irtypes.StructType); ok && len(st.Fields) >= 2 {
		_ = fatPtrType
		dataGep := block.NewGetElementPtr(iterVal.Type(), fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		lenGep := block.NewGetElementPtr(iterVal.Type(), fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dataPtr := block.NewLoad(irtypes.NewPointer(elemType), dataGep)
		lenVal := block.NewLoad(irtypes.I64, lenGep)
		block.NewStore(dataPtr, ptrAlloca)
		block.NewStore(lenVal, lenAlloca)
	} else {
		// Unknown structure, store zero len.
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenAlloca)
		block.NewStore(constant.NewNull(irtypes.NewPointer(elemType)), ptrAlloca)
	}

	// Loop counter.
	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	brToCond := block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenVal := condBlock.NewLoad(irtypes.I64, lenAlloca)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenVal)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	bodyPtr := bodyBlock.NewLoad(irtypes.NewPointer(elemType), ptrAlloca)
	elemGep := bodyBlock.NewGetElementPtr(elemType, bodyPtr, bodyIdx)

	isElemRC := isRCTrackedType(elemType)

	if s.IsRef {
		// `for ref` registers the slot's GEP as the scope binding's
		// alloca. Reads through the binding load from the slot;
		// writes (`x = newval`, `x += 1`) store back, so the array
		// is mutated in place. genAssignStmt's release-old + retain-
		// new path handles RC fields correctly: the old slot value
		// gets released before the new one is written, matching the
		// invariant that the slot owns the RC for its element.
		if s.VarName != "" {
			cg.curScope.set(s.VarName, &scopeEntry{
				val: elemGep, isAlloc: true, isRC: isElemRC,
				declPos: s.Pos(),
				// noRelease=true: the slot lives in the source
				// array, not in scope-local storage. Releasing
				// here would double-free when the array drops.
				noRelease: true,
			})
			cg.warnIfBuiltinShadow("for-in", s.VarName, s.Pos())
		}
	} else {
		// Per-iteration COPY semantics (the historical default):
		// load the element, store into a fresh alloca, retain RC
		// to claim ownership, scope-exit releases.
		elemVal := bodyBlock.NewLoad(elemType, elemGep)
		elemAlloca := bodyBlock.NewAlloca(elemType)
		bodyBlock.NewStore(elemVal, elemAlloca)

		if isElemRC {
			cg.emitRetain(bodyBlock, elemVal)
		}

		if s.VarName != "" {
			cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true, isRC: isElemRC, declPos: s.Pos()})
			cg.warnIfBuiltinShadow("for-in", s.VarName, s.Pos())
		}
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	// ARC: release loop body scope before back-edge.
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	// ARC: release the iterator value if it was a temporary RC allocation
	// (e.g. `for x in qsort(arr):` where qsort returns a fresh array).
	if isRCTrackedType(iterVal.Type()) && !isCopyExpr(s.Iter) {
		cg.emitRelease(afterBlock, iterVal)
	}

	return afterBlock, nil
}

// genForInStringRunes generates a for-in loop over a string that decodes
// UTF-8 codepoints and yields each one as a rune (i32).  The loop variable
// type must be "rune".  Invalid byte sequences yield U+FFFD (0xFFFD).
func (cg *CodeGen) genForInStringRunes(block *ir.Block, s *ast.ForStmt, iterVal value.Value) (*ir.Block, error) {
	// Extract data pointer and byte length from the string fat-ptr.
	dataPtr := cg.extractStringPtr(block, iterVal)
	lenVal := cg.extractStringLen(block, iterVal)

	// Allocas that persist across loop iterations.
	idxAlloca := block.NewAlloca(irtypes.I64) // current byte index
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	strideAlloca := block.NewAlloca(irtypes.I64) // bytes consumed this iteration
	runeAlloca := block.NewAlloca(irtypes.I32)   // decoded codepoint (i32 = rune)

	condBlock := cg.newBlock("forin.rune.cond")
	decBlock := cg.newBlock("forin.rune.dec")
	b1Block := cg.newBlock("forin.rune.1b")
	mbBlock := cg.newBlock("forin.rune.mb")
	check2Block := cg.newBlock("forin.rune.chk2")
	seq2Block := cg.newBlock("forin.rune.2b")
	check3Block := cg.newBlock("forin.rune.chk3")
	bnd2Block := cg.newBlock("forin.rune.bnd2")
	seq3Block := cg.newBlock("forin.rune.3b")
	check4Block := cg.newBlock("forin.rune.chk4")
	bnd3Block := cg.newBlock("forin.rune.bnd3")
	seq4Block := cg.newBlock("forin.rune.4b")
	bnd4Block := cg.newBlock("forin.rune.bnd4")
	replBlock := cg.newBlock("forin.rune.repl")
	bodyBlock := cg.newBlock("forin.rune.body")
	afterBlock := cg.newBlock("forin.rune.after")

	brToCond := block.NewBr(condBlock)

	// cond: i < len
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenVal)
	condBlock.NewCondBr(cond, decBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// decode: read first byte, branch on sequence type
	b0ptr := decBlock.NewGetElementPtr(irtypes.I8, dataPtr, idx)
	b0i8 := decBlock.NewLoad(irtypes.I8, b0ptr)
	b0 := decBlock.NewZExt(b0i8, irtypes.I64)
	i1 := decBlock.NewAdd(idx, constant.NewInt(irtypes.I64, 1))
	i2 := decBlock.NewAdd(idx, constant.NewInt(irtypes.I64, 2))
	i3 := decBlock.NewAdd(idx, constant.NewInt(irtypes.I64, 3))
	top1 := decBlock.NewAnd(b0, constant.NewInt(irtypes.I64, 0x80))
	isASCII := decBlock.NewICmp(enum.IPredEQ, top1, constant.NewInt(irtypes.I64, 0))
	decBlock.NewCondBr(isASCII, b1Block, mbBlock)

	// 1-byte ASCII
	b1Block.NewStore(b1Block.NewTrunc(b0, irtypes.I32), runeAlloca)
	b1Block.NewStore(constant.NewInt(irtypes.I64, 1), strideAlloca)
	b1Block.NewBr(bodyBlock)

	// multi-byte: check for 2-byte (b0 & 0xE0) == 0xC0
	top3 := mbBlock.NewAnd(b0, constant.NewInt(irtypes.I64, 0xE0))
	is2 := mbBlock.NewICmp(enum.IPredEQ, top3, constant.NewInt(irtypes.I64, 0xC0))
	mbBlock.NewCondBr(is2, check2Block, check3Block)

	// bounds check for 2-byte
	has1 := check2Block.NewICmp(enum.IPredSLT, i1, lenVal)
	check2Block.NewCondBr(has1, seq2Block, replBlock)

	// decode 2-byte
	b1ptr2 := seq2Block.NewGetElementPtr(irtypes.I8, dataPtr, i1)
	b1i8 := seq2Block.NewLoad(irtypes.I8, b1ptr2)
	b1 := seq2Block.NewZExt(b1i8, irtypes.I64)
	hi2 := seq2Block.NewShl(seq2Block.NewAnd(b0, constant.NewInt(irtypes.I64, 0x1F)), constant.NewInt(irtypes.I64, 6))
	lo2 := seq2Block.NewAnd(b1, constant.NewInt(irtypes.I64, 0x3F))
	r2 := seq2Block.NewOr(hi2, lo2)
	seq2Block.NewStore(seq2Block.NewTrunc(r2, irtypes.I32), runeAlloca)
	seq2Block.NewStore(constant.NewInt(irtypes.I64, 2), strideAlloca)
	seq2Block.NewBr(bodyBlock)

	// check for 3-byte (b0 & 0xF0) == 0xE0
	top4 := check3Block.NewAnd(b0, constant.NewInt(irtypes.I64, 0xF0))
	is3 := check3Block.NewICmp(enum.IPredEQ, top4, constant.NewInt(irtypes.I64, 0xE0))
	check3Block.NewCondBr(is3, bnd2Block, check4Block)

	// bounds check for 3-byte
	has2 := bnd2Block.NewICmp(enum.IPredSLT, i2, lenVal)
	bnd2Block.NewCondBr(has2, seq3Block, replBlock)

	// decode 3-byte
	b1ptr3 := seq3Block.NewGetElementPtr(irtypes.I8, dataPtr, i1)
	b1i8_3 := seq3Block.NewLoad(irtypes.I8, b1ptr3)
	b1_3 := seq3Block.NewZExt(b1i8_3, irtypes.I64)
	b2ptr3 := seq3Block.NewGetElementPtr(irtypes.I8, dataPtr, i2)
	b2i8_3 := seq3Block.NewLoad(irtypes.I8, b2ptr3)
	b2_3 := seq3Block.NewZExt(b2i8_3, irtypes.I64)
	hi3 := seq3Block.NewShl(seq3Block.NewAnd(b0, constant.NewInt(irtypes.I64, 0x0F)), constant.NewInt(irtypes.I64, 12))
	mid3 := seq3Block.NewShl(seq3Block.NewAnd(b1_3, constant.NewInt(irtypes.I64, 0x3F)), constant.NewInt(irtypes.I64, 6))
	lo3 := seq3Block.NewAnd(b2_3, constant.NewInt(irtypes.I64, 0x3F))
	r3 := seq3Block.NewOr(seq3Block.NewOr(hi3, mid3), lo3)
	seq3Block.NewStore(seq3Block.NewTrunc(r3, irtypes.I32), runeAlloca)
	seq3Block.NewStore(constant.NewInt(irtypes.I64, 3), strideAlloca)
	seq3Block.NewBr(bodyBlock)

	// check for 4-byte (b0 & 0xF8) == 0xF0
	top5 := check4Block.NewAnd(b0, constant.NewInt(irtypes.I64, 0xF8))
	is4 := check4Block.NewICmp(enum.IPredEQ, top5, constant.NewInt(irtypes.I64, 0xF0))
	check4Block.NewCondBr(is4, bnd3Block, replBlock)

	// bounds check for 4-byte
	has3 := bnd3Block.NewICmp(enum.IPredSLT, i3, lenVal)
	bnd3Block.NewCondBr(has3, seq4Block, bnd4Block)

	// bnd4: i3 not < n means we're missing bytes for a 4-byte sequence
	bnd4Block.NewBr(replBlock)

	// decode 4-byte
	b1ptr4 := seq4Block.NewGetElementPtr(irtypes.I8, dataPtr, i1)
	b1i8_4 := seq4Block.NewLoad(irtypes.I8, b1ptr4)
	b1_4 := seq4Block.NewZExt(b1i8_4, irtypes.I64)
	b2ptr4 := seq4Block.NewGetElementPtr(irtypes.I8, dataPtr, i2)
	b2i8_4 := seq4Block.NewLoad(irtypes.I8, b2ptr4)
	b2_4 := seq4Block.NewZExt(b2i8_4, irtypes.I64)
	b3ptr4 := seq4Block.NewGetElementPtr(irtypes.I8, dataPtr, i3)
	b3i8_4 := seq4Block.NewLoad(irtypes.I8, b3ptr4)
	b3_4 := seq4Block.NewZExt(b3i8_4, irtypes.I64)
	hi4 := seq4Block.NewShl(seq4Block.NewAnd(b0, constant.NewInt(irtypes.I64, 0x07)), constant.NewInt(irtypes.I64, 18))
	m4a := seq4Block.NewShl(seq4Block.NewAnd(b1_4, constant.NewInt(irtypes.I64, 0x3F)), constant.NewInt(irtypes.I64, 12))
	m4b := seq4Block.NewShl(seq4Block.NewAnd(b2_4, constant.NewInt(irtypes.I64, 0x3F)), constant.NewInt(irtypes.I64, 6))
	lo4 := seq4Block.NewAnd(b3_4, constant.NewInt(irtypes.I64, 0x3F))
	r4 := seq4Block.NewOr(seq4Block.NewOr(hi4, m4a), seq4Block.NewOr(m4b, lo4))
	seq4Block.NewStore(seq4Block.NewTrunc(r4, irtypes.I32), runeAlloca)
	seq4Block.NewStore(constant.NewInt(irtypes.I64, 4), strideAlloca)
	seq4Block.NewBr(bodyBlock)

	// replacement character U+FFFD for invalid sequences
	replBlock.NewStore(constant.NewInt(irtypes.I32, 0xFFFD), runeAlloca)
	replBlock.NewStore(constant.NewInt(irtypes.I64, 1), strideAlloca)
	replBlock.NewBr(bodyBlock)

	// body: expose loop variable, run user statements
	cg.curScope = newScope(cg.curScope)
	cg.curScope.set(s.VarName, &scopeEntry{val: runeAlloca, isAlloc: true, isRC: false, declPos: s.Pos()})
	cg.warnIfBuiltinShadow("for-in", s.VarName, s.Pos())

	cg.pushBreakTarget(afterBlock)

	var bodyErr error

	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment byte index by stride.
	if bodyBlock != nil && bodyBlock.Term == nil {
		curIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		stride := bodyBlock.NewLoad(irtypes.I64, strideAlloca)
		newIdx := bodyBlock.NewAdd(curIdx, stride)
		bodyBlock.NewStore(newIdx, idxAlloca)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	// Release iterator if it was a temporary.
	if isRCTrackedType(iterVal.Type()) && !isCopyExpr(s.Iter) {
		cg.emitRelease(afterBlock, iterVal)
	}

	return afterBlock, nil
}

func (cg *CodeGen) genForRange(block *ir.Block, s *ast.ForStmt, rng *ast.RangeExpr) (*ir.Block, error) {
	start, err := cg.genExpr(block, rng.Start)
	if err != nil {
		return nil, err
	}

	end, err := cg.genExpr(block, rng.End)
	if err != nil {
		return nil, err
	}

	var varType irtypes.Type = irtypes.I64
	if s.VarType != nil {
		varType, err = cg.tinTypeToLLVM(s.VarType)
		if err != nil {
			return nil, err
		}
	}

	start = cg.coerce(block, start, varType)
	end = cg.coerce(block, end, varType)

	// Alloca loop var.
	loopVar := block.NewAlloca(varType)
	block.NewStore(start, loopVar)

	condBlock := cg.newBlock("range.cond")
	bodyBlock := cg.newBlock("range.body")
	afterBlock := cg.newBlock("range.after")

	brToCond := block.NewBr(condBlock)

	// Cond: i < end.
	iVal := condBlock.NewLoad(varType, loopVar)
	endLoad := cg.coerce(condBlock, end, varType)
	cond := condBlock.NewICmp(enum.IPredSLT, iVal, endLoad)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: loopVar, isAlloc: true, declPos: s.Pos()})
		cg.warnIfBuiltinShadow("for", s.VarName, s.Pos())
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()

	// ARC: release loop-body-local RC vars before the back-edge so per-
	// iteration `let` bindings don't leak. Mirrors genForIn / genForCStyle.
	// Must run BEFORE we restore cg.curScope so emitScopeRelease still
	// sees the body scope.
	if bodyBlock != nil && bodyBlock.Term == nil {
		cg.emitScopeRelease(bodyBlock, cg.curScope)
	}

	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		iVal2 := bodyBlock.NewLoad(varType, loopVar)
		one := cg.coerce(bodyBlock, constant.NewInt(irtypes.I64, 1), varType)
		newI := bodyBlock.NewAdd(iVal2, one)
		bodyBlock.NewStore(newI, loopVar)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	return afterBlock, nil
}
