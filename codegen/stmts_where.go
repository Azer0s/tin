package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genPatternWhereList lowers a pattern-where-list as a chain of if-else blocks.
// Each clause tests the function's argument(s) against the clause pattern; on
// a match the body runs (and terminates via genWhereBody); on a mismatch
// control falls through to the next clause. A bare `_` clause is the final
// catch-all.
//
// Pattern kinds supported in Slice 1:
//   - integer / float / string / bool / atom literals (exact-match)
//   - identifier (binder; `_` is a discarding wildcard)
//   - ArrayPattern  (`[]`, `[x]`, `[x, ...xs]`, `[_, ...]`, ...)
//   - TuplePattern  (multi-arg dispatch; each elem may itself be any of the
//     above but not another TuplePattern)
//
// Struct patterns (`Type{field: ...}`) are not yet supported in where -
// they're planned for Slice 2. A clear error is emitted when one is encountered.
func (cg *CodeGen) genPatternWhereList(block *ir.Block, wl *ast.WhereList, retType irtypes.Type) (bool, error) {
	if cg.curFn == nil {
		return false, fmt.Errorf("pattern where-clauses are only supported inside function bodies")
	}

	// Collect the fn argument values (from their allocas registered in scope
	// at fn entry). In a coro fn the $coro variant has the same arg names
	// bound in the coroutine frame; the scope lookup finds them either way.
	args, argErr := cg.collectCurFnArgs(block)
	if argErr != nil {
		return false, argErr
	}

	arity := len(args)

	// Validate clause shapes: arity of each pattern must match the function
	// arity. Single-arg functions may use a bare pattern; multi-arg functions
	// must use a TuplePattern with the right number of components.
	for _, c := range wl.Clauses {
		if c.Pattern == nil {
			continue // bare `_`
		}

		got, gotNode := whereClauseArity(c.Pattern)
		if arity == 1 {
			if _, isTuple := c.Pattern.(*ast.TuplePattern); isTuple {
				return false, fmt.Errorf("%d:%d: function takes 1 argument but clause has a %d-element tuple pattern",
					c.Pos.Line, c.Pos.Col, got)
			}
		} else {
			if _, isTuple := c.Pattern.(*ast.TuplePattern); !isTuple {
				return false, fmt.Errorf("%d:%d: function takes %d arguments but clause has a single-pattern (expected a %d-element tuple like `where (p1, p2, ...):`)",
					c.Pos.Line, c.Pos.Col, arity, arity)
			}

			if got != arity {
				return false, fmt.Errorf("%d:%d: function takes %d arguments but clause pattern has %d components",
					c.Pos.Line, c.Pos.Col, arity, got)
			}

			_ = gotNode
		}
	}

	cg.scanWhereForUnreachable(wl)

	// Exhaustiveness: full Maranget check (codegen/maranget.go). When the
	// arms structurally cover every input, no explicit catch-all is needed;
	// otherwise we surface a witness value the arms fail to match.
	if ok, witness := marangetCheckWhereExhaustive(wl, arity); !ok {
		pos := wl.Pos()
		if len(wl.Clauses) > 0 {
			pos = wl.Clauses[0].Pos
		}

		return false, fmt.Errorf("%d:%d: non-exhaustive where: no clause matches %s; add the missing case or a catch-all `where _:`",
			pos.Line, pos.Col, witness)
	}

	// Emit the dispatch chain.
	curBlock := block
	anyFallthrough := false

	for i, c := range wl.Clauses {
		if isCatchAllWhereClause(c) {
			// Bare `_` wildcard: body runs unconditionally here. Any clauses
			// after this are dead code; a redundancy warning could go here.
			cg.curBlock = nil

			if err := cg.genWhereBody(curBlock, c.Body, retType); err != nil {
				return false, err
			}

			return true, nil
		}

		isLast := i == len(wl.Clauses)-1

		var nextBlock *ir.Block

		if !isLast {
			nextBlock = cg.newBlock(fmt.Sprintf("where.pat.next.%d", i))
		} else {
			// The last clause must be a structural catch-all for
			// exhaustiveness. On match it falls into the body. On
			// mismatch we emit unreachable (the compiler proved this
			// cannot happen).
			nextBlock = cg.newBlock(fmt.Sprintf("where.pat.unreach.%d", i))
			nextBlock.NewUnreachable()
		}

		bodyBlock := cg.newBlock(fmt.Sprintf("where.pat.body.%d", i))

		// Open a fresh scope for pattern bindings.
		cg.curScope = newScope(cg.curScope)

		successBlock, patErr := cg.emitWherePatternTest(curBlock, c.Pattern, args, nextBlock)
		if patErr != nil {
			cg.curScope = cg.curScope.parent

			return false, patErr
		}

		// Guard (optional): evaluate in the success block; false guard routes
		// to nextBlock just like a failed pattern.
		if c.Guard != nil {
			guardVal, gerr := cg.genExpr(successBlock, c.Guard)
			if gerr != nil {
				cg.curScope = cg.curScope.parent

				return false, gerr
			}

			if cg.curBlock != nil && cg.curBlock != successBlock {
				successBlock = cg.curBlock
			}

			successBlock.NewCondBr(cg.toBool(successBlock, guardVal), bodyBlock, nextBlock)
		} else {
			successBlock.NewBr(bodyBlock)
		}

		cg.curBlock = nil

		if err := cg.genWhereBody(bodyBlock, c.Body, retType); err != nil {
			cg.curScope = cg.curScope.parent

			return false, err
		}

		cg.curScope = cg.curScope.parent

		if isLast {
			anyFallthrough = true

			break
		}

		curBlock = nextBlock
	}

	_ = anyFallthrough

	return true, nil
}

// collectCurFnArgs returns (name, value, llvm-type) for each formal parameter
// of the current function, loading through the parameter's alloca when the
// scope entry is a slot (the common case). The returned value is the already-
// loaded rvalue for use in equality comparisons; for array patterns, callers
// will spill again to GEP into the fat-array fields.
type whereArg struct {
	name    string
	val     value.Value
	llvmTyp irtypes.Type
}

func (cg *CodeGen) collectCurFnArgs(block *ir.Block) ([]whereArg, error) {
	args := make([]whereArg, 0, len(cg.curFn.Params))

	for _, p := range cg.curFn.Params {
		name := p.Name()

		entry, ok := cg.curScope.lookup(name)
		if !ok {
			return nil, fmt.Errorf("internal: parameter %q not in scope for pattern-where dispatch", name)
		}

		if entry.isAlloc {
			pt, ok := entry.val.Type().(*irtypes.PointerType)
			if !ok {
				return nil, fmt.Errorf("internal: param %q alloca not a pointer", name)
			}

			args = append(args, whereArg{
				name:    name,
				val:     block.NewLoad(pt.ElemType, entry.val),
				llvmTyp: pt.ElemType,
			})
		} else {
			args = append(args, whereArg{
				name:    name,
				val:     entry.val,
				llvmTyp: entry.val.Type(),
			})
		}
	}

	return args, nil
}

// emitWherePatternTest emits tests for a top-level clause pattern against the
// function arg(s). On all-success execution continues in the returned block
// with pattern bindings in scope. On any failure, execution branches to
// failBlock.
//
// For TuplePattern: iterate elements positionally against args.
// For all other patterns: arity must be 1; test against args[0].
func (cg *CodeGen) emitWherePatternTest(block *ir.Block, pat ast.Node, args []whereArg, failBlock *ir.Block) (*ir.Block, error) {
	if tp, ok := pat.(*ast.TuplePattern); ok {
		cur := block

		for i, elem := range tp.Elems {
			next, err := cg.emitSingleArgPatternTest(cur, elem, args[i], failBlock)
			if err != nil {
				return nil, err
			}

			cur = next
		}

		return cur, nil
	}

	return cg.emitSingleArgPatternTest(block, pat, args[0], failBlock)
}

// emitSingleArgPatternTest tests a single pattern against a single arg value.
// On success returns the success-continuation block with bindings in scope;
// on failure branches block to failBlock.
func (cg *CodeGen) emitSingleArgPatternTest(block *ir.Block, pat ast.Node, arg whereArg, failBlock *ir.Block) (*ir.Block, error) {
	switch p := pat.(type) {
	case *ast.Identifier:
		if p.Name == "_" {
			return block, nil
		}
		// Binder: allocate a slot, store arg value, bind in scope.
		alloca := block.NewAlloca(arg.llvmTyp)
		block.NewStore(arg.val, alloca)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRCTrackedType(arg.llvmTyp)})

		return block, nil

	case *ast.IntLit:
		// Only valid against integer argument types.
		if !irtypes.IsInt(arg.llvmTyp) {
			return nil, fmt.Errorf("%d:%d: integer literal pattern used against non-integer argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}

		cst := constant.NewInt(arg.llvmTyp.(*irtypes.IntType), p.Value)
		cond := block.NewICmp(enum.IPredEQ, arg.val, cst)
		next := cg.newBlock("where.pat.litok")
		block.NewCondBr(cond, next, failBlock)

		return next, nil

	case *ast.BoolLit:
		if it, ok := arg.llvmTyp.(*irtypes.IntType); !ok || it.BitSize != 1 {
			return nil, fmt.Errorf("%d:%d: bool literal pattern used against non-bool argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}

		v := int64(0)
		if p.Value {
			v = 1
		}

		cond := block.NewICmp(enum.IPredEQ, arg.val, constant.NewInt(irtypes.I1, v))
		next := cg.newBlock("where.pat.boolok")
		block.NewCondBr(cond, next, failBlock)

		return next, nil

	case *ast.StringLit:
		if !isFatArrayPtr(arg.llvmTyp) {
			return nil, fmt.Errorf("%d:%d: string literal pattern used against non-string argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}
		// Build the target string constant as a fat-ptr, strcmp == 0.
		strFat := cg.buildStringFatPtr(block, p.Value)
		argPtr := cg.extractStringPtr(block, arg.val)
		tgtPtr := cg.extractStringPtr(block, strFat)
		cmp := block.NewCall(cg.ensureStrcmp(), argPtr, tgtPtr)
		cond := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))
		next := cg.newBlock("where.pat.strok")
		block.NewCondBr(cond, next, failBlock)

		return next, nil

	case *ast.FloatLit:
		if _, ok := arg.llvmTyp.(*irtypes.FloatType); !ok {
			return nil, fmt.Errorf("%d:%d: float literal pattern used against non-float argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}

		cst := constant.NewFloat(arg.llvmTyp.(*irtypes.FloatType), p.Value)
		cond := block.NewFCmp(enum.FPredOEQ, arg.val, cst)
		next := cg.newBlock("where.pat.fok")
		block.NewCondBr(cond, next, failBlock)

		return next, nil

	case *ast.AtomLit:
		if !isAtomType(arg.llvmTyp) {
			return nil, fmt.Errorf("%d:%d: atom literal pattern used against non-atom argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}

		code := cg.registerAtom(p.Name)
		argCode := cg.extractAtomCode(block, arg.val)
		cond := block.NewICmp(enum.IPredEQ, argCode, constant.NewInt(irtypes.I32, int64(code)))
		next := cg.newBlock("where.pat.atomok")
		block.NewCondBr(cond, next, failBlock)

		return next, nil

	case *ast.ArrayPattern:
		if !isFatArrayPtr(arg.llvmTyp) {
			return nil, fmt.Errorf("%d:%d: array pattern used against non-array argument (arg %q has type %s)",
				p.Pos().Line, p.Pos().Col, arg.name, fmtArgType(arg.llvmTyp))
		}

		return cg.emitWhereArrayPatternTest(block, p, arg, failBlock)

	case *ast.StructPattern:
		return nil, fmt.Errorf("%d:%d: struct patterns in where-clauses are not yet supported (planned for slice 2); use `match` for now",
			p.Pos().Line, p.Pos().Col)

	case *ast.TuplePattern:
		return nil, fmt.Errorf("%d:%d: nested tuple patterns are not supported",
			p.Pos().Line, p.Pos().Col)

	case *ast.TupleLit:
		// Parenthesised comma expressions aren't patterns. This catches
		// accidental nested-tuple usage like `where ((0, 0), _):`.
		return nil, fmt.Errorf("%d:%d: nested tuple patterns are not supported (inner `(...)` with commas is not a pattern; flatten to a single top-level tuple)",
			p.Pos().Line, p.Pos().Col)
	}

	return nil, fmt.Errorf("%d:%d: unsupported pattern in where-clause: expressions like `%T` are not valid patterns (use a bool-guard `where <expr>:` clause instead, or rewrite as `where (pat) if <expr>:`)",
		pat.Pos().Line, pat.Pos().Col, pat)
}

// emitWhereArrayPatternTest emits length checks and element/rest bindings for
// an ArrayPattern matched against a fat-array arg. Structure mirrors the
// per-case core of genArrayMatch; extracted here so pattern-where can reuse
// it without the full match-statement shape.
func (cg *CodeGen) emitWhereArrayPatternTest(block *ir.Block, ap *ast.ArrayPattern, arg whereArg, failBlock *ir.Block) (*ir.Block, error) {
	arrType := arg.llvmTyp.(*irtypes.StructType)
	elemPtrType := arrType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	// Spill so we can GEP; load len and data pointer.
	arrAlloca := block.NewAlloca(arrType)
	block.NewStore(arg.val, arrAlloca)

	lenGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	ptrGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dataPtr := block.NewLoad(elemPtrType, ptrGep)

	// Count non-rest elements and locate the rest slot.
	regularCount := 0
	restIdx := -1

	for i, e := range ap.Elems {
		if e.IsRest {
			restIdx = i
		} else {
			regularCount++
		}
	}

	// Length check.
	var lenCond value.Value

	nConst := constant.NewInt(irtypes.I64, int64(regularCount))
	if restIdx >= 0 {
		// Rest semantics: see codegen/stmts_match.go for the full note.
		// `[x, ...xs]` matches lists where xs has at least one element, so
		// the total length must be strictly greater than regularCount.
		lenCond = block.NewICmp(enum.IPredSGT, arrLen, nConst)
	} else {
		lenCond = block.NewICmp(enum.IPredEQ, arrLen, nConst)
	}

	lenOkBlock := cg.newBlock("where.pat.arrlenok")
	block.NewCondBr(lenCond, lenOkBlock, failBlock)

	// Regular element bindings (skip `_` and rest slot).
	regIdx := 0

	for _, e := range ap.Elems {
		if e.IsRest {
			continue
		}

		if !e.IsWild && e.Name != "" {
			idxVal := constant.NewInt(irtypes.I64, int64(regIdx))
			elemGep := lenOkBlock.NewGetElementPtr(elemType, dataPtr, idxVal)
			loaded := lenOkBlock.NewLoad(elemType, elemGep)
			alloca := lenOkBlock.NewAlloca(elemType)
			lenOkBlock.NewStore(loaded, alloca)
			cg.curScope.set(e.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRCTrackedType(elemType)})
		}

		regIdx++
	}

	// Rest binding (when named and not wild).
	if restIdx >= 0 {
		e := ap.Elems[restIdx]
		if !e.IsWild && e.Name != "" {
			var elemSzBytes int64 = 8
			if sz := llvmTypeSize(elemType); sz > 0 {
				elemSzBytes = int64(sz)
			}

			sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
			rawAlloca := lenOkBlock.NewAlloca(sliceType)

			dataAsI8 := lenOkBlock.NewBitCast(dataPtr, irtypes.I8Ptr)
			rawPtrGep := lenOkBlock.NewGetElementPtr(sliceType, rawAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			lenOkBlock.NewStore(dataAsI8, rawPtrGep)
			rawLenGep := lenOkBlock.NewGetElementPtr(sliceType, rawAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			lenOkBlock.NewStore(arrLen, rawLenGep)
			rawSlice := lenOkBlock.NewLoad(sliceType, rawAlloca)

			subRes := lenOkBlock.NewCall(cg.ensureSliceSubslice(), rawSlice,
				constant.NewInt(irtypes.I64, int64(regularCount)),
				constant.NewInt(irtypes.I64, elemSzBytes))

			tmp := lenOkBlock.NewAlloca(sliceType)
			lenOkBlock.NewStore(subRes, tmp)
			castPtr := lenOkBlock.NewBitCast(tmp, irtypes.NewPointer(arrType))
			restVal := lenOkBlock.NewLoad(arrType, castPtr)

			restAlloca := lenOkBlock.NewAlloca(arrType)
			lenOkBlock.NewStore(restVal, restAlloca)
			cg.curScope.set(e.Name, &scopeEntry{val: restAlloca, isAlloc: true, isRC: isRCTrackedType(arrType)})
		}
	}

	return lenOkBlock, nil
}

// whereClauseArity returns the "width" of a pattern - how many function args
// it consumes. For TuplePattern it's the tuple size; for everything else it's
// 1. The second return is the inner pattern node (useful for error diagnostics).
func whereClauseArity(pat ast.Node) (int, ast.Node) {
	if tp, ok := pat.(*ast.TuplePattern); ok {
		return len(tp.Elems), tp
	}

	return 1, pat
}

// fmtArgType renders an LLVM type using Tin surface names where possible so
// error messages read naturally. Strings, bools, and ints/floats get their
// source-level names; everything else falls back to the LLVM form.
func fmtArgType(t irtypes.Type) string {
	if isFatArrayPtr(t) {
		st := t.(*irtypes.StructType)
		elem := st.Fields[0].(*irtypes.PointerType).ElemType

		if it, ok := elem.(*irtypes.IntType); ok && it.BitSize == 8 {
			return "string"
		}

		return "[" + fmtArgType(elem) + "]"
	}

	if isAtomType(t) {
		return "atom"
	}

	if it, ok := t.(*irtypes.IntType); ok {
		switch it.BitSize {
		case 1:
			return "bool"
		case 8, 16, 32, 64, 128:
			return fmt.Sprintf("i%d", it.BitSize)
		}
	}

	if ft, ok := t.(*irtypes.FloatType); ok {
		switch ft.Kind { //nolint:exhaustive // half/X86_FP80/PPC_FP128 are unused by tin
		case irtypes.FloatKindFloat:
			return "f32"
		case irtypes.FloatKindDouble:
			return "f64"
		case irtypes.FloatKindFP128:
			return "f128"
		}
	}

	return t.String()
}
