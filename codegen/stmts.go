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

// Body generation

// genBody generates a function body from a node (Block, WhereList, or expression).
// Returns whether the block was terminated.
func (cg *CodeGen) genBody(block *ir.Block, body ast.Node, retType irtypes.Type) (bool, error) {
	// checkDeferRetOverride checks if a deferred thunk wrote an override return value
	// into curFnDeferRetAlloca and returns it (or val unchanged) if not set.
	checkDeferRetOverride := func(b *ir.Block, val value.Value) value.Value {
		if cg.curDeferRetSlotParam != nil {
			return val // inside a thunk - no override slot
		}

		if cg.curFnDeferRetAlloca == nil || irtypes.IsVoid(retType) || val == nil {
			return val
		}

		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotPtr := b.NewBitCast(cg.curFnDeferRetAlloca, irtypes.NewPointer(slotType))
		validGep := b.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		valid := b.NewLoad(irtypes.I8, validGep)
		isValid := b.NewICmp(enum.IPredNE, valid, constant.NewInt(irtypes.I8, 0))
		valGep := b.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		overrideVal := b.NewLoad(retType, valGep)

		return b.NewSelect(isValid, overrideVal, val)
	}

	// emitTerminator emits the appropriate block terminator.
	// In a coroutine context, it emits _tin_fiber_complete + final coro.suspend
	// instead of a plain ret instruction.
	emitTerminator := func(b *ir.Block, val value.Value, skipName string) {
		if b == nil || b.Term != nil {
			return
		}

		_ = cg.emitDefers(b)
		if val != nil && !irtypes.IsVoid(retType) {
			val = checkDeferRetOverride(b, val)
		}

		cg.emitAllScopeReleases(b, skipName)

		if cg.inCoroFn {
			cg.emitCoroComplete(b, val)
			cg.emitFinalSuspend(b, cg.curCoroFrame)
		} else if irtypes.IsVoid(retType) || val == nil {
			b.NewRet(nil)
		} else {
			b.NewRet(val)
		}
	}

	addDefaultRet := func(b *ir.Block) error {
		if b == nil || b.Term != nil {
			return nil
		}

		if !irtypes.IsVoid(retType) {
			fnName := ""
			if cg.curFn != nil {
				fnName = cg.curFn.Name()
			}

			return fmt.Errorf("fn %s: not all code paths return a value", fnName)
		}

		if cg.inCoroFn {
			emitTerminator(b, nil, "")
		} else {
			_ = cg.emitDefers(b)
			cg.emitAllScopeReleases(b, "")
			b.NewRet(nil)
		}

		return nil
	}
	switch b := body.(type) {
	case *ast.Block:
		newBlock, term, err := cg.genBlock(block, b)
		if err != nil {
			return false, err
		}

		if !term {
			if err := addDefaultRet(newBlock); err != nil {
				return false, err
			}
		}

		return true, nil
	case *ast.WhereList:
		return cg.genWhereList(block, b, retType)
	case nil:
		return false, nil
	case *ast.ExprStmt:
		// Single expression-statement body (e.g. fn foo() = someCall())
		// For void functions, generate the call and add a default return.
		// For value-returning functions, unwrap and treat as an expression.
		inner := b.Expr
		if !irtypes.IsVoid(retType) {
			val, err := cg.genExpr(block, inner)
			if err != nil {
				return false, err
			}

			if val != nil {
				val = cg.coerce(block, val, retType)

				retSkip := ""
				if ident, ok := inner.(*ast.Identifier); ok {
					retSkip = ident.Name
				}

				emitTerminator(block, val, retSkip)
			} else {
				if err := addDefaultRet(block); err != nil {
					return false, err
				}
			}

			return true, nil
		}
		// Void: generate as statement.
		newBlock, terminated, err := cg.genStmt(block, b)
		if err != nil {
			return false, err
		}

		if !terminated {
			if err := addDefaultRet(newBlock); err != nil {
				return false, err
			}
		}

		return true, nil
	case *ast.ReturnStmt, *ast.EchoStmt, *ast.AssignStmt, *ast.PostfixStmt,
		*ast.VarDecl, *ast.IfStmt, *ast.ForStmt, *ast.MatchStmt, *ast.DeferStmt,
		*ast.AwaitMatchStmt:
		// Single statement body (e.g. fn foo() T = return expr)
		newBlock, terminated, err := cg.genStmt(block, body)
		if err != nil {
			return false, err
		}

		if !terminated {
			if err := addDefaultRet(newBlock); err != nil {
				return false, err
			}
		}

		return true, nil
	default:
		// Single expression body (e.g. fn foo() = expr)
		val, err := cg.genExpr(block, body)
		if err != nil {
			return false, err
		}

		if !irtypes.IsVoid(retType) && val != nil {
			val = cg.coerce(block, val, retType)

			retSkip := ""
			if ident, ok := body.(*ast.Identifier); ok {
				retSkip = ident.Name
			}

			emitTerminator(block, val, retSkip)
		} else {
			emitTerminator(block, nil, "")
		}

		return true, nil
	}
}

// genBlock generates a sequence of statements in the given block.
// Returns (currentBlock, terminated, error). currentBlock is the block that
// should receive the next instruction after the block's statements; it may
// differ from the incoming block when nested control-flow (if/for/match)
// creates new merge blocks.
func (cg *CodeGen) genBlock(block *ir.Block, b *ast.Block) (*ir.Block, bool, error) {
	var err error

	for _, stmt := range b.Stmts {
		if block == nil {
			panic(fmt.Sprintf("genBlock: block nil before stmt %T", stmt))
		}

		var terminated bool

		block, terminated, err = cg.genStmt(block, stmt)
		if err != nil {
			return nil, false, err
		}

		if terminated || block == nil {
			return nil, true, nil
		}
	}

	return block, false, nil
}

// isStmtNode reports whether an AST node is inherently a statement (not an
// expression that also appears as a statement).
func isStmtNode(node ast.Node) bool {
	switch node.(type) {
	case *ast.Block, *ast.ReturnStmt, *ast.EchoStmt, *ast.AssignStmt,
		*ast.AugAssignStmt, *ast.PostfixStmt, *ast.VarDecl,
		*ast.IfStmt, *ast.ForStmt, *ast.MatchStmt, *ast.DeferStmt,
		*ast.BreakStmt, *ast.FuncDecl, *ast.TaggedBlock, *ast.AwaitMatchStmt:
		return true
	}

	return false
}

// genWhereBody generates the body of a where clause (which may be an
// expression, a statement, or a block) and emits an appropriate terminator.
func (cg *CodeGen) genWhereBody(block *ir.Block, body ast.Node, retType irtypes.Type) error {
	// If the body is an ExprStmt wrapping an expression, unwrap it so we
	// can capture the return value.
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}

	if isStmtNode(body) {
		newBlock, terminated, err := cg.genStmt(block, body)
		if err != nil {
			return err
		}

		if !terminated && newBlock != nil && newBlock.Term == nil {
			_ = cg.emitDefers(newBlock)
			newBlock.NewRet(nil)
		}

		return nil
	}

	// Expression body: evaluate and return value.
	bodyVal, err := cg.genExpr(block, body)
	if err != nil {
		return err
	}

	if !irtypes.IsVoid(retType) && bodyVal != nil {
		bodyVal = cg.coerce(block, bodyVal, retType)
		_ = cg.emitDefers(block)
		block.NewRet(bodyVal)
	} else {
		_ = cg.emitDefers(block)
		block.NewRet(nil)
	}

	return nil
}

// genWhereCondition generates an i1 condition for a where clause condition.
// When the condition is an AtomLit and a match subject is set, it emits a
// comparison against the subject.
func (cg *CodeGen) genWhereCondition(block *ir.Block, condNode ast.Node) (value.Value, error) {
	if atomNode, ok := condNode.(*ast.AtomLit); ok && cg.matchSubject != nil {
		subjectType := cg.matchSubject.Type()
		if isAtomType(subjectType) {
			// atom subject: compare CRC32 codes directly.
			code := cg.registerAtom(atomNode.Name)
			subjectCode := cg.extractAtomCode(block, cg.matchSubject)

			return block.NewICmp(enum.IPredEQ, subjectCode, constant.NewInt(irtypes.I32, int64(code))), nil
		}
		// string subject: compare via strcmp.
		atomStr := cg.buildStringFatPtr(block, "'"+atomNode.Name)
		subjectPtr := cg.extractStringPtr(block, cg.matchSubject)
		atomPtr := cg.extractStringPtr(block, atomStr)
		cmpResult := block.NewCall(cg.ensureStrcmp(), subjectPtr, atomPtr)

		return block.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0)), nil
	}

	cond, err := cg.genExpr(block, condNode)
	if err != nil {
		return nil, err
	}

	return cg.toBool(block, cond), nil
}

// genWhereList generates a chain of if/else blocks for where clauses.
func (cg *CodeGen) genWhereList(block *ir.Block, wl *ast.WhereList, retType irtypes.Type) (bool, error) {
	// mergeBlock is created lazily so it only gets added to the function
	// if actually needed (when no wildcard catches everything).
	var mergeBlock *ir.Block

	getMerge := func() *ir.Block {
		if mergeBlock == nil {
			mergeBlock = cg.newBlock("where.merge")
		}

		return mergeBlock
	}

	for i, clause := range wl.Clauses {
		if clause.Cond == nil {
			// Wildcard: always executes. Ensure merge has a terminator if it exists.
			if mergeBlock != nil && mergeBlock.Term == nil {
				mergeBlock.NewUnreachable()
			}
			// Reset cg.curBlock so that any stale value from a previous clause's
			// condition evaluation does not misdirect body code generation.
			cg.curBlock = nil
			if err := cg.genWhereBody(block, clause.Body, retType); err != nil {
				return false, err
			}

			return true, nil
		}

		// Evaluate condition.
		cond, err := cg.genWhereCondition(block, clause.Cond)
		if err != nil {
			return false, err
		}
		// Reset cg.curBlock after condition evaluation.  genBinExpr (which
		// evaluates the condition) sets cg.curBlock = block (the condition's
		// entry block) as a baseline for its internal block-refresh logic.
		// That stale value must not leak into the then/else body code-gen,
		// where block-refresh checks would misfire and redirect instruction
		// emission to the condition's block instead of the branch block.
		cg.curBlock = nil

		thenBlock := cg.newBlock(fmt.Sprintf("where.then.%d", i))

		var elseBlock *ir.Block
		if i == len(wl.Clauses)-1 {
			elseBlock = getMerge()
		} else {
			elseBlock = cg.newBlock(fmt.Sprintf("where.else.%d", i))
		}

		block.NewCondBr(cond, thenBlock, elseBlock)

		// Generate then body.
		if err := cg.genWhereBody(thenBlock, clause.Body, retType); err != nil {
			return false, err
		}

		block = elseBlock
	}

	// Fallthrough: unreachable.
	m := getMerge()
	if m.Term == nil {
		m.NewUnreachable()
	}

	return true, nil
}

// Statement generation

// genStmt generates a single statement. Returns (currentBlock, terminated, error).
// If the block was terminated (ret/br), currentBlock may be nil.
func (cg *CodeGen) genStmt(block *ir.Block, node ast.Node) (*ir.Block, bool, error) {
	switch s := node.(type) {
	case *ast.Block:
		newBlock, term, err := cg.genBlock(block, s)
		if err != nil {
			return nil, false, err
		}

		if term {
			return nil, true, nil
		}

		return newBlock, false, nil

	case *ast.VarDecl:
		block, err := cg.genVarDecl(block, s)

		return block, false, err

	case *ast.ArrayDestructDecl:
		newBlock, err := cg.genArrayDestructDecl(block, s)

		return newBlock, false, err

	case *ast.StructDestructDecl:
		newBlock, err := cg.genStructDestructDecl(block, s)

		return newBlock, false, err

	case *ast.TupleDestructDecl:
		newBlock, err := cg.genTupleDestructDecl(block, s)

		return newBlock, false, err

	case *ast.ReturnStmt:
		if err := cg.genReturn(block, s); err != nil {
			return nil, false, err
		}

		return nil, true, nil

	case *ast.BreakStmt:
		// Emit an unconditional branch to the innermost loop's after-block.
		// First release any RC-tracked variables declared inside the loop body
		// (from the current scope up to, but not including, the scope that was
		// active before the loop body was entered).
		if target := cg.currentBreakTarget(); target != nil {
			outerScope := cg.currentBreakScope()
			for s := cg.curScope; s != nil && s != outerScope && !s.isFunctionBoundary; s = s.parent {
				cg.emitScopeRelease(block, s)
			}

			block.NewBr(target)
			cg.markBreakUsed()
		}

		return nil, true, nil

	case *ast.EchoStmt:
		var err error

		block, err = cg.genEcho(block, s)

		return block, false, err

	case *ast.YieldStmt:
		newBlock, err := cg.genYieldStmt(block)

		return newBlock, false, err

	case *ast.ExprStmt:
		// Special-case: `await expr` as a statement.
		// Dispatch via genExpr (which calls genAwaitExpr) so that Future[t] and
		// other Awaitable[t] implementations are handled correctly via trait dispatch.
		// The result is discarded (it may be Future[Unit] = void).
		if _, ok := s.Expr.(*ast.AwaitExpr); ok {
			// Mirror the pattern used by genVarDecl: align cg.curBlock with the
			// current block before calling genExpr so we can detect if genExpr
			// advanced to a new block (e.g. coroutine chaining creates new blocks).
			cg.curBlock = block

			_, err := cg.genExpr(block, s.Expr)
			if err != nil {
				return nil, false, err
			}

			if cg.curBlock != nil && cg.curBlock != block {
				return cg.curBlock, false, nil
			}

			return block, false, nil
		}

		cg.curBlock = block
		val, err := cg.genExpr(block, s.Expr)
		// If genExpr advanced the current block (e.g. an await arg created new
		// blocks), use the continuation block for any subsequent emission.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if err == nil && val != nil && isRCTrackedType(val.Type()) && isTemporaryProducer(s.Expr) {
			// Discarded RC-tracked value from a call/concat/etc.: release our ref.
			cg.emitRelease(block, val)
		}

		return block, false, err

	case *ast.AssignStmt:
		newBlock, err := cg.genAssign(block, s)

		return newBlock, false, err

	case *ast.AugAssignStmt:
		newBlock, err := cg.genAugAssign(block, s)

		return newBlock, false, err

	case *ast.PostfixStmt:
		err := cg.genPostfix(block, s)

		return block, false, err

	case *ast.IfStmt:
		newBlock, term, err := cg.genIf(block, s)

		return newBlock, term, err

	case *ast.ForStmt:
		newBlock, err := cg.genFor(block, s)
		if err != nil {
			return nil, false, err
		}
		// genFor returns nil when the loop is unconditionally infinite (for true)
		// and has no reachable after-block (no break statement).  Signal to the
		// caller that this path is terminated so addDefaultRet is not called.
		if newBlock == nil {
			return nil, true, nil
		}

		return newBlock, false, nil

	case *ast.MatchStmt:
		newBlock, err := cg.genMatch(block, s)

		return newBlock, false, err

	case *ast.AwaitMatchStmt:
		newBlock, err := cg.genAwaitMatch(block, s)

		return newBlock, false, err

	case *ast.DeferStmt:
		// 1. Generate a zero-param thunk that captures free variables from the
		//    current scope by value (same semantics as a closure).
		fnI8, envI8, err := cg.genDeferThunk(block, s.Call)
		if err != nil {
			return nil, false, err
		}
		// 2. Push thunk + env onto the runtime defer chain so that _tin_panic
		//    can run it during cross-frame stack unwinding.
		cg.ensureDeferChain()
		entryAlloca := block.NewAlloca(cg.deferEntryType)
		entryI8 := block.NewBitCast(entryAlloca, irtypes.I8Ptr)
		// Pass curFnDeferRetAlloca as the ret_slot so a defer thunk can override the return value.
		retSlotArg := cg.curFnDeferRetAlloca
		if retSlotArg == nil {
			retSlotArg = constant.NewNull(irtypes.I8Ptr)
		}

		block.NewCall(cg.deferPushFn, entryI8, fnI8, envI8, retSlotArg)
		cg.pendingDeferFrames = append(cg.pendingDeferFrames, entryI8)
		// 3. Record the thunk fn and its env for inline LIFO emission on normal
		//    return: emitDefers calls thunk(env) then frees env.
		cg.pendingDeferFnI8s = append(cg.pendingDeferFnI8s, fnI8)
		cg.pendingDeferEnvs = append(cg.pendingDeferEnvs, envI8)

		return block, false, nil

	case *ast.StructDecl:
		// Struct declared inside a function/test body: register it so struct
		// literals using its name can resolve it.
		if err := cg.genStructDecl(s); err != nil {
			return nil, false, err
		}

		return block, false, nil

	case *ast.TypeDecl:
		// Local type alias or generic struct instantiation inside a function/test body.
		if err := cg.genTypeDecl(s); err != nil {
			return nil, false, err
		}

		return block, false, nil

	case *ast.FuncDecl:
		// Nested function declaration - hoist to top level.
		if err := cg.genFuncDecl(s); err != nil {
			return nil, false, err
		}

		return block, false, nil

	case *ast.TaggedBlock:
		return cg.genStmt(block, s.Body)

	default:
		// Unknown statement - try as expression.
		_, err := cg.genExpr(block, node)
		if err != nil {
			return nil, false, err
		}
		// If genExpr terminated the block (e.g. via panic builtin), signal that.
		if block.Term != nil {
			return nil, true, nil
		}

		return block, false, nil
	}
}

func (cg *CodeGen) genVarDecl(block *ir.Block, s *ast.VarDecl) (*ir.Block, error) {
	// Top-level constants are preregistered in the preregister pass as direct
	// constant values. Skip re-emitting them as stack allocas.
	if s.IsConst {
		if e, ok := cg.curScope.lookup(s.Name); ok && !e.isAlloc {
			return block, nil
		}
	}

	var (
		llType irtypes.Type
		err    error
	)

	if s.Type != nil {
		llType, err = cg.tinTypeToLLVM(s.Type)
		if err != nil {
			return nil, err
		}
	}

	var initVal value.Value

	if s.Value != nil {
		cg.curBlock = block
		// TupleLit: pass the declared type so fields get the right LLVM types.
		if tup, ok := s.Value.(*ast.TupleLit); ok && llType != nil {
			initVal, err = cg.genTupleLit(block, tup, llType)
		} else {
			initVal, err = cg.genExpr(block, s.Value)
		}

		if err != nil {
			return nil, err
		}
		// Pick up any block change from await/yield inside the init expression.
		if cg.curBlock != block {
			block = cg.curBlock
		}

		if llType == nil && initVal != nil {
			llType = initVal.Type()
		}
	}

	if llType == nil {
		llType = irtypes.I64
	}

	// If llType is the i64 fallback (unresolved generic/alias) and the init
	// value has a concrete struct type, use the init value's type instead.
	// This handles: let t GenericType = expr  where GenericType resolves to a concrete struct.
	if initVal != nil && llType.Equal(irtypes.I64) {
		if _, isStruct := initVal.Type().(*irtypes.StructType); isStruct {
			llType = initVal.Type()
		}
	}

	if block == nil {
		panic(fmt.Sprintf("genVarDecl: block is nil for var %q (llType=%v, curBlock=%v, curFn=%v)", s.Name, llType, cg.curBlock, cg.curFn))
	}

	// All local variables are stack-allocated. Heap promotion happens lazily at
	// the return site (genLatePromotedReturn) for variables whose addresses escape.
	alloca := block.NewAlloca(llType)

	// isHeapOwned: this variable receives the return value of a heap-promoting
	// function (one that uses _tin_rc_alloc to return *T), or a &StructLit{} that
	// was RC-alloc'd inline.  Scope-exit performs a chain release rather than the
	// normal ARC release.
	isHeapOwned := false
	heapOwnedDepth := 0

	if callExpr, isCall := s.Value.(*ast.CallExpr); isCall {
		calleeName := ""

		switch fn := callExpr.Func.(type) {
		case *ast.Identifier:
			calleeName = fn.Name
		}

		if calleeName != "" && llType != nil {
			// Check both the raw AST name and the scope-resolved IR name (e.g.
			// "parse_value" AST name vs "json__parse_value" IR name) so that
			// package-qualified functions are detected correctly.
			isHeapFn := cg.heapPromotingFns[calleeName]
			if !isHeapFn {
				if entry, ok := cg.curScope.lookup(calleeName); ok {
					if f, ok2 := entry.val.(*ir.Func); ok2 {
						isHeapFn = cg.heapPromotingFns[f.Name()]
					}
				}
			}

			if isHeapFn {
				depth := pointerChainDepth(llType)
				if depth > 0 {
					isHeapOwned = true
					heapOwnedDepth = depth
				}
			}
		}
	} else if addrOf, isAddrOf := s.Value.(*ast.AddressOfExpr); isAddrOf {
		if _, isStructLit := addrOf.Expr.(*ast.StructLit); isStructLit && llType != nil {
			depth := pointerChainDepth(llType)
			if depth > 0 {
				isHeapOwned = true
				heapOwnedDepth = depth
			}
		}
	}

	isRC := isRCTrackedType(llType)
	if initVal != nil {
		// If the init value is an empty array {i8*, i64} but the declared type
		// is a typed fat array {T*, i64}, use a properly-typed zero value.
		if !initVal.Type().Equal(llType) {
			if isFatArrayPtr(initVal.Type()) && isFatArrayPtr(llType) {
				initVal = cg.zeroValue(llType)
			}
		}

		srcType := initVal.Type()
		initVal = cg.coerce(block, initVal, llType)
		block.NewStore(initVal, alloca)

		// ARC: retain when copying from an existing variable (identifier).
		// emitRetain handles RC-tracked values (fat arrays, strings, any) and
		// named structs with RC-tracked fields, and is a no-op for everything else.
		//
		// EXCEPTION: if coerce just boxed a non-any value into `any`, the new
		// box block is a fresh _tin_rc_alloc (rc=1) - it is already owned, so
		// an extra retain would over-count and cause a leak.
		boxedToAny := isAnyType(llType) && !isAnyType(srcType)
		if isCopyExpr(s.Value) && !boxedToAny {
			cg.emitRetain(block, initVal)
		}
	} else {
		// Zero-initialize.
		// For fixed-size arrays >= 128 bytes, use llvm.memset rather than
		// storing a huge aggregate constant: large aggregate value stores
		// (e.g. [65536 x i8] zeroinitializer) crash LLVM's instruction selector.
		if at, ok := llType.(*irtypes.ArrayType); ok {
			elemBytes := llvmElemByteSize(at.ElemType)
			if elemBytes > 0 && int64(at.Len)*elemBytes >= 128 {
				totalBytes := constant.NewInt(irtypes.I64, int64(at.Len)*elemBytes)
				dstPtr := block.NewBitCast(alloca, irtypes.I8Ptr)
				block.NewCall(cg.ensureMemset(), dstPtr,
					constant.NewInt(irtypes.I8, 0), totalBytes,
					constant.NewInt(irtypes.I1, 0))
			} else {
				block.NewStore(cg.zeroValue(llType), alloca)
			}
		} else {
			block.NewStore(cg.zeroValue(llType), alloca)
		}
	}

	// Consume lastSliceBase: genSliceExpr sets it to the base allocation pointer
	// (before any GEP offset) so that ARC retain/release works on the real ARC
	// header rather than a possibly-interior fat-ptr field-0 pointer.
	// We read it here (after genExpr returns) so that any nested expression that
	// also calls genSliceExpr doesn't clobber our value.
	sliceBase := cg.lastSliceBase

	cg.lastSliceBase = nil
	if sliceBase != nil {
		// Retain the base pointer once to balance the scope-exit release below.
		block.NewCall(cg.ensureRetain(), sliceBase)
	}

	// If there is already an RC-tracked variable with the same name in the CURRENT
	// (not parent) scope, release it before overwriting the entry.  This handles
	// re-declarations inside loop bodies (e.g. `for ...: let x = recv()`)
	// where the same name is declared on every iteration and the old value would
	// otherwise be orphaned.
	if existing, ok := cg.curScope.vars[s.Name]; ok && existing.isAlloc && existing.isRC {
		if existing.basePtr != nil {
			block.NewCall(cg.ensureRelease(), existing.basePtr)
		} else {
			existingPtrType, isPtrType := existing.val.Type().(*irtypes.PointerType)
			if isPtrType {
				oldVal := block.NewLoad(existingPtrType.ElemType, existing.val)
				if existing.noDeinit {
					cg.emitReleaseNoDeinit(block, oldVal)
				} else {
					cg.emitRelease(block, oldVal)
				}
			}
		}
	}

	// Non-capturing closures (null env): scope-exit would emit _tin_release_closure(null)
	// which is a no-op in the runtime.  Set noRelease to skip it entirely.
	noReleaseClosureEnv := false
	if _, isLambda := s.Value.(*ast.LambdaExpr); isLambda && isFatFnPtr(llType) {
		noReleaseClosureEnv = !cg.lastLambdaHadCaptures
		cg.lastLambdaHadCaptures = false // consume
	}

	// Determine the byte-array element kind: prefer the explicit declared type,
	// then fall back to the RHS AsExpr type (covers `let x = expr as [byte]`).
	bae := byteArrayElemType(s.Type)
	if bae == "" {
		if asExpr, ok := s.Value.(*ast.AsExpr); ok {
			bae = byteArrayElemType(asExpr.Type)
		}
	}

	cg.curScope.set(s.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, basePtr: sliceBase, isUnsigned: isUnsignedTinType(s.Type), byteArrayElem: bae, scalarTypeName: scalar8BitTypeName(s.Type), isHeapOwned: isHeapOwned, heapOwnedDepth: heapOwnedDepth, noRelease: noReleaseClosureEnv})

	return block, nil
}

// emitDefers emits all pending deferred calls in LIFO order into block.
// For each defer, it pops that single entry from the runtime chain before
// executing it inline.  This ensures that if a deferred call itself panics,
// the remaining (not-yet-run) defers are still in the chain and will be
// executed by _tin_panic.
//
// IMPORTANT: this function does NOT clear pendingDeferFnI8s.  Each return path in
// a function lives in its own basic block and independently emits the same set
// of defers.  Clearing here would cause the second (and later) return paths to
// see an empty list and silently skip their defers.  The list is naturally
// cleared when genFuncDeclAs restores the outer function's prevDefers state.
func (cg *CodeGen) emitDefers(block *ir.Block) error {
	n := len(cg.pendingDeferFnI8s)
	if n == 0 {
		return nil
	}
	// All thunks share the same signature: void(i8* env, i8* ret_slot).
	thunkFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr, irtypes.I8Ptr)

	retSlotArg := cg.curFnDeferRetAlloca
	if retSlotArg == nil {
		retSlotArg = constant.NewNull(irtypes.I8Ptr)
	}

	for i := n - 1; i >= 0; i-- {
		// Deregister this one entry before running it.
		if cg.deferPopFn != nil {
			block.NewCall(cg.deferPopFn, constant.NewInt(irtypes.I64, 1))
		}
		// Call the compiled thunk directly with its captured env.
		// This is correct for both plain-call defers and lambda defers because
		// the thunk captures all free variables by reference (alloca pointer),
		// and the allocas remain live until the enclosing function returns.
		fnI8 := cg.pendingDeferFnI8s[i]
		env := cg.pendingDeferEnvs[i]
		thunkFnPtr := block.NewBitCast(fnI8, irtypes.NewPointer(thunkFnType))
		block.NewCall(thunkFnPtr, env, retSlotArg)
		// Free the heap env that was malloc'd for the thunk.
		// Skip the null sentinel emitted when there were no captures.
		if _, isNull := env.(*constant.Null); !isNull {
			block.NewCall(cg.ensureFree(), env)
		}
	}

	return nil
}

func (cg *CodeGen) genReturn(block *ir.Block, s *ast.ReturnStmt) error {
	// In a coroutine body, return is replaced by _tin_fiber_complete + final suspend.
	if cg.inCoroFn {
		return cg.genCoroReturn(block, s)
	}

	// Inside a defer thunk: 'return val' overrides the outer function's return value
	// by writing to the ret_slot parameter.
	if cg.curDeferRetSlotParam != nil {
		if s.Value != nil {
			retVal, err := cg.genExpr(block, s.Value)
			if err != nil {
				return err
			}
			// Coerce to the lambda's declared return type (e.g. None -> null *i64).
			if cg.curDeferThunkRetType != nil && !irtypes.IsVoid(cg.curDeferThunkRetType) {
				retVal = cg.coerce(block, retVal, cg.curDeferThunkRetType)
			}
			// Two sub-cases:
			// (a) Lambda return type is *T (pointer-to-outer-retType): only override if non-nil.
			//     The ret_slot struct is typed for T, so load through the pointer.
			// (b) Direct value: write it directly to the slot.
			if ptrTy, isPtr := retVal.Type().(*irtypes.PointerType); isPtr {
				// Case (a): `defer (fn() *T = ...)()` - non-nil pointer overrides outer return.
				innerType := ptrTy.ElemType
				slotType := irtypes.NewStruct(irtypes.I8, innerType)
				slotPtr := block.NewBitCast(cg.curDeferRetSlotParam, irtypes.NewPointer(slotType))
				// Check non-nil.
				isNilPtr := block.NewICmp(enum.IPredEQ, retVal, constant.NewNull(ptrTy))
				nilBlock := cg.curFn.NewBlock(fmt.Sprintf("defer.ret.nil.%d", cg.labelCount))
				overrideBlock := cg.curFn.NewBlock(fmt.Sprintf("defer.ret.override.%d", cg.labelCount))
				cg.labelCount++

				block.NewCondBr(isNilPtr, nilBlock, overrideBlock)
				// Override branch: load *retVal and write to slot.
				derefVal := overrideBlock.NewLoad(innerType, retVal)
				validGep := overrideBlock.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				overrideBlock.NewStore(constant.NewInt(irtypes.I8, 1), validGep)
				valGep := overrideBlock.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
				overrideBlock.NewStore(derefVal, valGep)
				overrideBlock.NewRet(nil)
				// Nil branch: no override, just return.
				nilBlock.NewRet(nil)
			} else {
				// Case (b): plain `return val` in defer do: - write directly to slot.
				slotType := irtypes.NewStruct(irtypes.I8, retVal.Type())
				slotPtr := block.NewBitCast(cg.curDeferRetSlotParam, irtypes.NewPointer(slotType))
				validGep := block.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				block.NewStore(constant.NewInt(irtypes.I8, 1), validGep)
				valGep := block.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
				block.NewStore(retVal, valGep)
				block.NewRet(nil)
			}
		} else {
			block.NewRet(nil)
		}

		return nil
	}

	if s.Value == nil {
		if err := cg.emitDefers(block); err != nil {
			return err
		}

		cg.emitAllScopeReleases(block, "")
		block.NewRet(nil)

		return nil
	}

	// Late heap promotion: if this return involves escaping vars, defer evaluation
	// until after defers so the post-defer stack values are used for the RC blocks.
	if len(cg.curFnEscapingVars) > 0 {
		promoted := retainedHeapVars(s.Value, cg.curFnEscapingAliases, cg.curFnEscapingVars)
		if len(promoted) > 0 {
			return cg.genLatePromotedReturn(block, s, promoted)
		}
	}

	cg.curBlock = block // sync before genExpr so we can detect block advances
	// TupleLit: pass the declared return type so fields get the right types.
	var val value.Value

	if tup, ok := s.Value.(*ast.TupleLit); ok && cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
		var err2 error

		val, err2 = cg.genTupleLit(block, tup, cg.curFn.Sig.RetType)
		if err2 != nil {
			return err2
		}
	} else {
		var err2 error

		val, err2 = cg.genExpr(block, s.Value)
		if err2 != nil {
			return err2
		}
	}
	// If genExpr advanced the current block (e.g. via coro chain call), use it.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if cg.curFn != nil {
		retType := cg.curFn.Sig.RetType
		if !irtypes.IsVoid(retType) {
			val = cg.coerce(block, val, retType)
		}
	}

	if err := cg.emitDefers(block); err != nil {
		return err
	}
	// After running defers, check if any deferred function wrote an override return value.
	if cg.curFnDeferRetAlloca != nil && cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
		retType := cg.curFn.Sig.RetType
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotPtr := block.NewBitCast(cg.curFnDeferRetAlloca, irtypes.NewPointer(slotType))
		validGep := block.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		valid := block.NewLoad(irtypes.I8, validGep)
		isValid := block.NewICmp(enum.IPredNE, valid, constant.NewInt(irtypes.I8, 0))
		valGep := block.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		overrideVal := block.NewLoad(retType, valGep)
		val = block.NewSelect(isValid, overrideVal, val)
	}
	// ARC: release all RC locals except the one being returned
	// (to transfer its rc=1 ownership to the caller).
	retSkipName := ""
	if ident, ok := s.Value.(*ast.Identifier); ok {
		retSkipName = ident.Name
	} else if isCopyExpr(s.Value) {
		// Returning a borrowed value (field access, index) whose RC lifetime is
		// tied to a local/parameter that will be released by emitAllScopeReleases.
		// Retain first so the caller gets one owned reference, then scope cleanup
		// decrements the RC back to a net-neutral result.
		cg.emitRetain(block, val)
	}

	cg.emitAllScopeReleases(block, retSkipName)
	block.NewRet(val)

	return nil
}

// genCoroReturn generates the coroutine-specific terminator for a return statement.
// Instead of ret, it boxes the return value, calls _tin_fiber_complete, and
// emits the final coro.suspend which leads to cleanup.
func (cg *CodeGen) genCoroReturn(block *ir.Block, s *ast.ReturnStmt) error {
	var retVal value.Value

	if s.Value != nil {
		cg.curBlock = block // sync before genExpr so we can detect block advances

		var err error

		retVal, err = cg.genExpr(block, s.Value)
		if err != nil {
			return err
		}
		// If genExpr advanced the current block (e.g. via coro chain call), use it.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
		// Coerce to the original return type (not i8*).
		if cg.curCoroRetType != nil && !irtypes.IsVoid(cg.curCoroRetType) && retVal != nil {
			retVal = cg.coerce(block, retVal, cg.curCoroRetType)
		}
	}

	if err := cg.emitDefers(block); err != nil {
		return err
	}

	retSkipName := ""

	if s.Value != nil {
		if ident, ok := s.Value.(*ast.Identifier); ok {
			retSkipName = ident.Name
		}
	}

	cg.emitAllScopeReleases(block, retSkipName)
	cg.emitCoroComplete(block, retVal)
	cg.emitFinalSuspend(block, cg.curCoroFrame)

	return nil
}

// genYieldStmt emits a yield point inside an {#async} coroutine body.
// In the normal (non-coro) variant of the same function, yield is a no-op.
// Returns the resume block where execution continues after being scheduled.
func (cg *CodeGen) genYieldStmt(block *ir.Block) (*ir.Block, error) {
	if !cg.inCoroFn {
		// In the sync version of an {#async} function, yield is a no-op.
		return block, nil
	}

	cg.ensureFiberRuntime()
	// Notify the scheduler that we want to be re-enqueued.
	block.NewCall(cg.fiberYieldCoroFn, cg.curCoroHdl)
	// Suspend the coroutine; returns the resume block.
	resumeBlk := cg.emitSuspendPoint(block, cg.curCoroFrame)
	// Track yield-resume blocks so genYieldAutoAt can suppress the redundant
	// autoyield when the loop backedge lands on this resume block.
	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[resumeBlk] = true
	}

	return resumeBlk, nil
}

// genAwaitStmt emits an await point inside an {#async} coroutine body.
// In the normal (non-coro) variant, await is a no-op.
func (cg *CodeGen) genAwaitStmt(block *ir.Block, pidVal value.Value) (*ir.Block, error) {
	if !cg.inCoroFn {
		// Non-coroutine context (e.g., main): run scheduler until the fiber completes.
		cg.ensureFiberRuntime()

		if pidVal == nil {
			return block, nil
		}

		pid64 := cg.coerce(block, pidVal, irtypes.I64)
		syncAwaitFn := cg.ensureExternDecl("_tin_fiber_sync_await", irtypes.Void,
			[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)
		block.NewCall(syncAwaitFn, pid64)

		return block, nil
	}

	cg.ensureFiberRuntime()

	if pidVal != nil && !pidVal.Type().Equal(irtypes.I64) {
		pidVal = cg.coerce(block, pidVal, irtypes.I64)
	}

	if pidVal == nil {
		return block, nil
	}
	// Register this fiber as a waiter for pid.
	block.NewCall(cg.fiberJoinFn, pidVal, cg.curCoroHdl)
	// Suspend; the scheduler will re-enqueue us when pid completes.
	resumeBlk := cg.emitSuspendPoint(block, cg.curCoroFrame)

	return resumeBlk, nil
}

// genBuiltinLen implements the len(expr) built-in: returns the i64 length of
// strings, dynamic arrays, or the constant size of static arrays.
func (cg *CodeGen) genBuiltinLen(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	t := val.Type()
	// String fat-ptr {i8*, i64}: extract field 1.
	if isStringType(t) {
		return cg.extractStringLen(block, val), nil
	}
	// Dynamic array fat-ptr {T*, i64}: extract field 1.
	if isFatArrayPtr(t) {
		st := t.(*irtypes.StructType)
		alloca := block.NewAlloca(st)
		block.NewStore(val, alloca)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

		return block.NewLoad(irtypes.I64, gep), nil
	}
	// Static array [N x T]: constant length.
	if at, ok := t.(*irtypes.ArrayType); ok {
		return constant.NewInt(irtypes.I64, int64(at.Len)), nil
	}

	return nil, fmt.Errorf("len() not supported for type %s", t)
}

// Defer chain helpers

// ensureDeferChain lazily declares the runtime defer-chain functions and
// initializes the TinDeferEntry LLVM struct type.
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
			captures = append(captures, closureCapture{n, entry.val, entry.val.Type(), true})
		} else {
			captures = append(captures, closureCapture{n, entry.val, entry.val.Type(), false})
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
			captures = append(captures, closureCapture{n, entry.val, entry.val.Type(), true})
		} else {
			captures = append(captures, closureCapture{n, entry.val, entry.val.Type(), false})
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
func (cg *CodeGen) genBuiltinDefault(block *ir.Block, arg ast.Node) (value.Value, error) {
	// Handle default(typeof(expr)): generate expr to discover its LLVM type,
	// then return the zero value for that type. The generated code for the
	// inner expression is dead but LLVM will optimize it away.
	if call, ok := arg.(*ast.CallExpr); ok {
		if fnID, ok2 := call.Func.(*ast.Identifier); ok2 && fnID.Name == "typeof" && len(call.Args) == 1 {
			val, err := cg.genExpr(block, call.Args[0])
			if err != nil {
				return nil, err
			}

			return cg.zeroValue(val.Type()), nil
		}
	}

	// The argument is a type used as an expression - typically an Identifier.
	var typeExpr ast.TypeExpr

	switch a := arg.(type) {
	case *ast.Identifier:
		typeExpr = &ast.SimpleType{Name: a.Name}
	default:
		return constant.NewInt(irtypes.I64, 0), nil
	}

	llvmType, err := cg.tinTypeToLLVM(typeExpr)
	if err != nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	switch t := llvmType.(type) {
	case *irtypes.IntType:
		return constant.NewInt(t, 0), nil
	case *irtypes.FloatType:
		return constant.NewFloat(t, 0), nil
	case *irtypes.PointerType:
		return constant.NewNull(t), nil
	case *irtypes.StructType:
		// Zero-initialize each field.
		fields := make([]constant.Constant, len(t.Fields))
		for i, f := range t.Fields {
			switch ft := f.(type) {
			case *irtypes.IntType:
				fields[i] = constant.NewInt(ft, 0)
			case *irtypes.FloatType:
				fields[i] = constant.NewFloat(ft, 0)
			case *irtypes.PointerType:
				fields[i] = constant.NewNull(ft)
			default:
				fields[i] = constant.NewUndef(ft)
			}
		}

		return constant.NewStruct(t, fields...), nil
	default:
		return constant.NewUndef(llvmType), nil
	}
}

// panic builtin

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable

// expandMacro evaluates a macro call, choosing the appropriate strategy:
//   - Complex macros (block body): CTFE - compile to a temp binary, run with timeout,
//     parse stdout as the expansion result.
//   - Simple macros (expression body): AST substitution - fast, no subprocess.
func (cg *CodeGen) expandMacro(block *ir.Block, macro *ast.MacroDecl, args []ast.Node) (value.Value, error) {
	if len(args) != len(macro.Params) {
		return nil, fmt.Errorf("macro %s: expected %d args, got %d",
			macro.Name, len(macro.Params), len(args))
	}
	// Complex (block body) macros: compile and run at compile time.
	if isMacroComplex(macro) {
		resultNode, err := cg.ctfeExpandMacro(macro, args)
		if err != nil {
			return nil, err
		}

		return cg.genExpr(block, resultNode)
	}
	// Simple expression macros: AST substitution (fast path).
	subst := make(map[string]ast.Node, len(macro.Params))
	for i, p := range macro.Params {
		subst[p] = args[i]
	}

	body := macro.Body
	// Unwrap ExprStmt and ReturnStmt wrappers so the body is a bare expression.
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}

	if rs, ok := body.(*ast.ReturnStmt); ok && rs.Value != nil {
		body = rs.Value
	}

	expanded := substituteMacroNode(body, subst)
	// Backtick literal body: parse the content as tin source, substitute params, then codegen.
	if btl, ok := expanded.(*ast.BacktickLit); ok {
		node, err := parseExprString(btl.Content)
		if err != nil {
			return nil, fmt.Errorf("macro %s: backtick parse error: %w", macro.Name, err)
		}
		// Substitute params into the parsed tree (backtick was an opaque string).
		node = substituteMacroNode(node, subst)

		return cg.genExpr(block, node)
	}

	return cg.genExpr(block, expanded)
}

// substituteMacroNode replaces identifier nodes matching a macro parameter
// with the corresponding argument AST node.
func substituteMacroNode(node ast.Node, subst map[string]ast.Node) ast.Node {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.Identifier:
		if replacement, ok := subst[n.Name]; ok {
			return replacement
		}

		return n
	case *ast.BinExpr:
		return &ast.BinExpr{
			Left:  substituteMacroNode(n.Left, subst),
			Right: substituteMacroNode(n.Right, subst),
			Op:    n.Op,
		}
	case *ast.UnaryExpr:
		return &ast.UnaryExpr{
			Expr: substituteMacroNode(n.Expr, subst),
			Op:   n.Op,
		}
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substituteMacroNode(a, subst)
		}

		return &ast.CallExpr{
			Func:     substituteMacroNode(n.Func, subst),
			Args:     newArgs,
			TypeArgs: n.TypeArgs,
		}
	case *ast.FieldAccess:
		return &ast.FieldAccess{
			Expr:  substituteMacroNode(n.Expr, subst),
			Field: n.Field,
			IsPtr: n.IsPtr,
		}
	case *ast.IndexExpr:
		return &ast.IndexExpr{
			Expr:  substituteMacroNode(n.Expr, subst),
			Index: substituteMacroNode(n.Index, subst),
		}
	case *ast.TernaryExpr:
		return &ast.TernaryExpr{
			Cond: substituteMacroNode(n.Cond, subst),
			Then: substituteMacroNode(n.Then, subst),
			Else: substituteMacroNode(n.Else, subst),
		}
	case *ast.ExprStmt:
		return &ast.ExprStmt{Expr: substituteMacroNode(n.Expr, subst)}
	case *ast.ReturnStmt:
		if n.Value != nil {
			return &ast.ReturnStmt{Value: substituteMacroNode(n.Value, subst)}
		}

		return n
	}

	return node
}

// exprByteArrayElem returns the element type name ("byte", "u8", "char") when
// the AST expression is statically known to be a [byte]/[u8]/[char] fat array,
// and "" otherwise.
func (cg *CodeGen) exprByteArrayElem(node ast.Node) string {
	switch n := node.(type) {
	case *ast.AsExpr:
		return byteArrayElemType(n.Type)
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.byteArrayElem
		}
	}

	return ""
}

// exprByte8Type returns the Tin type name for an 8-bit scalar expression:
// "char", "byte", "u8", or "i8".  Returns "" for non-8-bit types.
// Handles identifiers (scope lookup), function parameters, and struct field
// accesses (e.g. this.age where age is declared u8).
func (cg *CodeGen) exprByte8Type(node ast.Node) string {
	switch n := node.(type) {
	case *ast.AsExpr:
		return scalar8BitTypeName(n.Type)
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.scalarTypeName
		}
	case *ast.FieldAccess:
		if ident, ok := n.Expr.(*ast.Identifier); ok {
			se, ok2 := cg.curScope.lookup(ident.Name)
			if !ok2 {
				break
			}

			// se.val is an alloca; its element type is the struct (possibly via *)
			var elemT irtypes.Type
			if pt, ok3 := se.val.Type().(*irtypes.PointerType); ok3 {
				elemT = pt.ElemType
				// Handle pointer-receiver: *Struct -> Struct
				if pt2, ok4 := elemT.(*irtypes.PointerType); ok4 {
					elemT = pt2.ElemType
				}
			}

			structName := cg.typeNameOf(elemT)
			if structName == "" {
				break
			}

			fields := cg.structFields[structName]
			tinTypes := cg.structFieldTinTypes[structName]

			for i, fname := range fields {
				if fname == n.Field && i < len(tinTypes) {
					return scalar8BitTypeName(tinTypes[i])
				}
			}
		}
	}

	return ""
}

func (cg *CodeGen) genEcho(block *ir.Block, s *ast.EchoStmt) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	cg.curBlock = block

	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if val == nil {
		return block, nil
	}

	t := val.Type()
	switch {
	case isAnyType(t):
		return cg.genEchoAny(block, val)

	case isAtomType(t):
		// Convert atom to its string representation then print.
		code := cg.extractAtomCode(block, val)
		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		ptr := cg.extractStringPtr(block, strFatPtr)
		fmtStr := cg.newGlobalString("'%s\n")
		block.NewCall(printf, fmtStr, ptr)

	case isStringType(t):
		// [byte]/[u8]/[char] arrays share {i8*, i64} layout with string.
		// Dispatch by element type: byte -> hex, u8 -> decimal, char -> %c.
		// Plain strings fall through to %s.
		if elem := cg.exprByteArrayElem(s.Value); elem != "" {
			var perElemFmt string

			switch elem {
			case "byte":
				perElemFmt = "%02x"
			case "u8":
				perElemFmt = "%u"
			default: // "char"
				perElemFmt = "%c"
			}

			var printErr error

			block, printErr = cg.genPrintByteArray(block, val, perElemFmt)
			if printErr != nil {
				return nil, printErr
			}

			block.NewCall(printf, cg.newGlobalString("\n"))

			break
		}
		// Extract data pointer and call printf("%s\n", ptr).
		ptr := cg.extractStringPtr(block, val)
		fmtStr := cg.newGlobalString("%s\n")
		block.NewCall(printf, fmtStr, ptr)

	case irtypes.IsInt(t):
		it := t.(*irtypes.IntType)

		var fmtStr value.Value
		if it.BitSize == 1 {
			// bool: print 0 or 1 via printf
			fmtStr = cg.newGlobalString("%d\n")
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, fmtStr, zext)

			return block, nil
		}

		if it.BitSize == 8 {
			// Dispatch format by Tin type: char->%c, byte->%02x, u8/i8->%d
			ext := block.NewZExt(val, irtypes.I32)

			switch cg.exprByte8Type(s.Value) {
			case "char":
				fmtStr = cg.newGlobalString("%c\n")
			case "byte":
				fmtStr = cg.newGlobalString("%02x\n")
			default: // "u8", "i8", ""
				fmtStr = cg.newGlobalString("%d\n")
			}

			block.NewCall(printf, fmtStr, ext)

			return block, nil
		}

		fmtStr = cg.newGlobalString("%lld\n")
		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, fmtStr, ext)

	case irtypes.IsFloat(t):
		fmtStr := cg.newGlobalString("%g\n")

		var ext value.Value
		if t == irtypes.Double {
			ext = val
		} else {
			ext = block.NewFPExt(val, irtypes.Double)
		}

		block.NewCall(printf, fmtStr, ext)

	case irtypes.IsPointer(t):
		fmtStr := cg.newGlobalString("%p\n")
		block.NewCall(printf, fmtStr, val)

	default:
		// print trait: struct or fat-pointer with a print() method.
		if strVal, ok := cg.callPrintTrait(block, val); ok {
			ptr := cg.extractStringPtr(block, strVal)
			fmtStr := cg.newGlobalString("%s\n")
			block.NewCall(printf, fmtStr, ptr)

			break
		}
		// Struct or array: Go-style formatting.
		var printErr error

		block, printErr = cg.genPrintValue(block, val)
		if printErr != nil {
			return nil, printErr
		}

		block.NewCall(printf, cg.newGlobalString("\n"))
	}

	// ARC: release fresh RC-tracked values produced by function calls or
	// concatenation that are not stored in a named variable (temporaries).
	// Named variables are released by their scope entry at scope exit.
	if isRCTrackedType(t) && isTemporaryProducer(s.Value) {
		cg.emitRelease(block, val)
	}

	return block, nil
}

// genPrintValue emits printf calls to print val in Go-style format without a
// trailing newline. Structs print as {f1 f2 ...}, arrays as [e1 e2 ...].
func (cg *CodeGen) genPrintValue(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	t := val.Type()

	switch {
	case isStringType(t):
		ptr := cg.extractStringPtr(block, val)
		block.NewCall(printf, cg.newGlobalString("%s"), ptr)

	case isAtomType(t):
		code := cg.extractAtomCode(block, val)
		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		ptr := cg.extractStringPtr(block, strFatPtr)
		block.NewCall(printf, cg.newGlobalString("'%s"), ptr)

	case irtypes.IsInt(t):
		it := t.(*irtypes.IntType)
		switch it.BitSize {
		case 1:
			trueStr := cg.newGlobalString("true")
			falseStr := cg.newGlobalString("false")
			chosen := block.NewSelect(val, trueStr, falseStr)
			block.NewCall(printf, cg.newGlobalString("%s"), chosen)
		case 8:
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, cg.newGlobalString("%c"), zext)
		default:
			ext := cg.coerce(block, val, irtypes.I64)
			block.NewCall(printf, cg.newGlobalString("%lld"), ext)
		}

	case irtypes.IsFloat(t):
		var ext value.Value
		if t == irtypes.Double {
			ext = val
		} else {
			ext = block.NewFPExt(val, irtypes.Double)
		}

		block.NewCall(printf, cg.newGlobalString("%g"), ext)

	case irtypes.IsPointer(t):
		block.NewCall(printf, cg.newGlobalString("%p"), val)

	case isFatArrayPtr(t):
		var err error

		block, err = cg.genPrintArray(block, val)
		if err != nil {
			return nil, err
		}

	default:
		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			var err error

			block, err = cg.genPrintStruct(block, val, st)
			if err != nil {
				return nil, err
			}

			break
		}

		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, cg.newGlobalString("%lld"), ext)
	}

	return block, nil
}

// genPrintStruct emits printf calls to print a named struct value as {f1 f2 ...}.
func (cg *CodeGen) genPrintStruct(block *ir.Block, val value.Value, st *irtypes.StructType) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	name := st.Name()
	fieldNames := cg.structFields[name]
	userOff := 1 + cg.vtableOffset(name)

	block.NewCall(printf, cg.newGlobalString("{"))

	for i, fieldName := range fieldNames {
		if fieldName == "" {
			continue
		}

		llIdx := userOff + i
		if llIdx >= len(st.Fields) {
			break
		}

		if i > 0 {
			block.NewCall(printf, cg.newGlobalString(" "))
		}

		fieldVal := block.NewExtractValue(val, uint64(llIdx))

		var err error

		block, err = cg.genPrintValue(block, fieldVal)
		if err != nil {
			return nil, err
		}
	}

	block.NewCall(printf, cg.newGlobalString("}"))

	return block, nil
}

// genPrintArray emits a loop that prints a fat-array value as [e1 e2 ...].
func (cg *CodeGen) genPrintArray(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	fatType := val.Type().(*irtypes.StructType)
	elemPtrType := fatType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	// Alloca for loop counter.
	iAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	block.NewCall(printf, cg.newGlobalString("["))

	condBlock := cg.newBlock("print.arr.cond")
	bodyBlock := cg.newBlock("print.arr.body")
	endBlock := cg.newBlock("print.arr.end")

	block.NewBr(condBlock)

	// Condition: i < length
	iVal := condBlock.NewLoad(irtypes.I64, iAlloca)
	cmp := condBlock.NewICmp(enum.IPredSLT, iVal, length)
	condBlock.NewCondBr(cmp, bodyBlock, endBlock)

	// Body: print separator if i > 0, print element, increment i.
	iVal2 := bodyBlock.NewLoad(irtypes.I64, iAlloca)
	isFirst := bodyBlock.NewICmp(enum.IPredEQ, iVal2, constant.NewInt(irtypes.I64, 0))
	spaceStr := cg.newGlobalString(" ")
	emptyStr := cg.newGlobalString("")
	sepStr := bodyBlock.NewSelect(isFirst, emptyStr, spaceStr)
	bodyBlock.NewCall(printf, cg.newGlobalString("%s"), sepStr)

	elemPtr := bodyBlock.NewGetElementPtr(elemType, dataPtr, iVal2)
	elemVal := bodyBlock.NewLoad(elemType, elemPtr)

	var err error

	bodyBlock, err = cg.genPrintValue(bodyBlock, elemVal)
	if err != nil {
		return nil, err
	}

	iNext := bodyBlock.NewAdd(iVal2, constant.NewInt(irtypes.I64, 1))
	bodyBlock.NewStore(iNext, iAlloca)
	bodyBlock.NewBr(condBlock)

	endBlock.NewCall(printf, cg.newGlobalString("]"))

	return endBlock, nil
}

// genPrintByteArray emits a loop that prints a [byte]/[u8]/[char] fat-array as
// [e1 e2 ...] where each element is formatted with perElemFmt (e.g. "%02x",
// "%u", "%c").  The fat-array must have layout {i8*, i64}.
func (cg *CodeGen) genPrintByteArray(block *ir.Block, val value.Value, perElemFmt string) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	iAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	block.NewCall(printf, cg.newGlobalString("["))

	condBlock := cg.newBlock("print.bytes.cond")
	bodyBlock := cg.newBlock("print.bytes.body")
	endBlock := cg.newBlock("print.bytes.end")

	block.NewBr(condBlock)

	// Condition: i < length
	iVal := condBlock.NewLoad(irtypes.I64, iAlloca)
	cmp := condBlock.NewICmp(enum.IPredSLT, iVal, length)
	condBlock.NewCondBr(cmp, bodyBlock, endBlock)

	// Body: print space separator (except before first), print element, increment.
	iVal2 := bodyBlock.NewLoad(irtypes.I64, iAlloca)
	isFirst := bodyBlock.NewICmp(enum.IPredEQ, iVal2, constant.NewInt(irtypes.I64, 0))
	spaceStr := cg.newGlobalString(" ")
	emptyStr := cg.newGlobalString("")
	sepStr := bodyBlock.NewSelect(isFirst, emptyStr, spaceStr)
	bodyBlock.NewCall(printf, cg.newGlobalString("%s"), sepStr)

	elemPtr := bodyBlock.NewGetElementPtr(irtypes.I8, dataPtr, iVal2)
	elemVal := bodyBlock.NewLoad(irtypes.I8, elemPtr)
	zext := bodyBlock.NewZExt(elemVal, irtypes.I32)
	bodyBlock.NewCall(printf, cg.newGlobalString(perElemFmt), zext)

	iNext := bodyBlock.NewAdd(iVal2, constant.NewInt(irtypes.I64, 1))
	bodyBlock.NewStore(iNext, iAlloca)
	bodyBlock.NewBr(condBlock)

	endBlock.NewCall(printf, cg.newGlobalString("]"))

	return endBlock, nil
}

func (cg *CodeGen) genAssign(block *ir.Block, s *ast.AssignStmt) (*ir.Block, error) {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return block, err
	}

	cg.curBlock = block

	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return block, err
	}
	// If genExpr advanced the current block (e.g. await inside rhs), use
	// the continuation block for all subsequent emissions.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}
	// Get the element type of the pointer.
	ptrType := ptr.Type().(*irtypes.PointerType)
	srcType := val.Type()
	val = cg.coerce(block, val, ptrType.ElemType)
	// ARC: for RC-tracked types, retain new value (if copy) then release old.
	// Skip retain if coerce just boxed a non-any value to any: the new box is
	// a fresh _tin_rc_alloc (rc=1) and is already owned.
	// Weak field targets are non-owning: skip both retain and release.
	isWeakTarget := false

	if fa, ok2 := s.Target.(*ast.FieldAccess); ok2 {
		if ident, ok3 := fa.Expr.(*ast.Identifier); ok3 {
			if se, ok4 := cg.curScope.lookup(ident.Name); ok4 {
				if pt, ok5 := se.val.Type().(*irtypes.PointerType); ok5 {
					parentName := cg.typeNameOf(pt.ElemType)
					if parentName != "" {
						isWeakTarget = cg.structWeakFields[parentName][fa.Field]
					}
				}
			}
		}
	}

	if isRCTrackedType(ptrType.ElemType) && !isWeakTarget {
		boxedToAny := isAnyType(ptrType.ElemType) && !isAnyType(srcType)
		if isCopyExpr(s.Value) && !boxedToAny {
			cg.emitRetain(block, val)
		}

		oldVal := block.NewLoad(ptrType.ElemType, ptr)
		cg.emitRelease(block, oldVal)
	}

	block.NewStore(val, ptr)

	return block, nil
}

func (cg *CodeGen) genAugAssign(block *ir.Block, s *ast.AugAssignStmt) (*ir.Block, error) {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return block, err
	}

	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	cg.curBlock = block

	rhs, err := cg.genExpr(block, s.Value)
	if err != nil {
		return block, err
	}
	// If genExpr advanced the current block (e.g. await inside rhs), use
	// the continuation block for all subsequent emissions.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}
	// For ++= the rhs is an element to append, not the container type.
	// Save the raw rhs for use in the ++= case; other ops coerce rhs to
	// the container/element type (which is the same for scalar types).
	rhsRaw := rhs
	rhs = cg.coerce(block, rhs, elemType)

	var result value.Value

	switch s.Op {
	case "+=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			idx := cg.coerce(block, rhs, irtypes.I64)
			result = block.NewGetElementPtr(pt.ElemType, current, idx)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFAdd(current, rhs)
		} else {
			result = block.NewAdd(current, rhs)
		}
	case "-=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			idx := cg.coerce(block, rhs, irtypes.I64)
			neg := block.NewSub(constant.NewInt(irtypes.I64, 0), idx)
			result = block.NewGetElementPtr(pt.ElemType, current, neg)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFSub(current, rhs)
		} else {
			result = block.NewSub(current, rhs)
		}
	case "*=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFMul(current, rhs)
		} else {
			result = block.NewMul(current, rhs)
		}
	case "/=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFDiv(current, rhs)
		} else {
			result = block.NewSDiv(current, rhs)
		}
	case "++=":
		// Append element to fat array {T*, i64}.
		// current = {old_ptr, old_len}; rhs = new element of type T.
		// new_len = old_len + 1
		// new_ptr = malloc(new_len * sizeof(T))
		// memcpy(new_ptr, old_ptr, old_len * sizeof(T))
		// new_ptr[old_len] = rhs
		// result = {new_ptr, new_len}
		if isFatArrayPtr(elemType) {
			fatType := elemType.(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			oldPtr := block.NewExtractValue(current, 0)
			oldLen := block.NewExtractValue(current, 1)
			newLen := block.NewAdd(oldLen, constant.NewInt(irtypes.I64, 1))

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
			newBytes := block.NewMul(newLen, elemSize)

			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), newBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy old data.
			oldBytes := block.NewMul(oldLen, elemSize)
			oldI8Ptr := block.NewBitCast(oldPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, oldI8Ptr, oldBytes, constant.NewInt(irtypes.I1, 0))

			// Store new element at index old_len.
			// Use rhsRaw (the raw expression value) to avoid the earlier
			// coerce(rhs, elemType) which coerced to the container type rather
			// than the element type.  Re-coerce here to elemT (element type).
			newElemGep := block.NewGetElementPtr(elemT, newPtr, oldLen)
			newElem := cg.coerce(block, rhsRaw, elemT)

			// ARC for pointer-typed elements ([*T]):
			// The array co-owns every element pointer, so all elements must be
			// heap-allocated (ARC-managed) before being stored.
			if pt, isPtr := elemT.(*irtypes.PointerType); isPtr {
				if addrOf, ok := s.Value.(*ast.AddressOfExpr); ok {
					if _, isIdent := addrOf.Expr.(*ast.Identifier); isIdent {
						// &localVar: the pointer is to a stack alloca.  Heap-promote
						// it by copying the struct value into a fresh _tin_rc_alloc
						// block so the array holds a proper ARC-managed pointer.
						structVal := block.NewLoad(pt.ElemType, newElem)
						sz := cg.llvmSizeOf(block, pt.ElemType)
						heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
						typedHeapPtr := block.NewBitCast(heapI8, elemT)
						cg.emitRetain(block, structVal) // retain RC fields before copying
						block.NewStore(structVal, typedHeapPtr)
						newElem = typedHeapPtr
						// newElem is fresh (RC=1) - no additional retain below
					}
				}
			}

			block.NewStore(newElem, newElemGep)
			// ARC: retain element if it is copied from an existing owner (variable,
			// field, or index).  Without this, releasing the source variable frees
			// the element's data while the array still holds a reference.
			if isCopyExpr(s.Value) {
				if _, isPtr := elemT.(*irtypes.PointerType); isPtr {
					// For pointer elements: retain the pointed-to ARC block itself.
					ptrI8 := block.NewBitCast(newElem, irtypes.I8Ptr)
					block.NewCall(cg.ensureRetain(), ptrI8)
				} else {
					cg.emitRetain(block, newElem)
				}
			}

			// Build new fat ptr.
			fatAlloca := block.NewAlloca(fatType)
			ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(newPtr, ptrGep)
			lenGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(newLen, lenGep)
			result = block.NewLoad(fatType, fatAlloca)

			// ARC: release old array data (rc goes to 0 -> free) before
			// overwriting the alloca with the new fat ptr.
			block.NewCall(cg.ensureRelease(), oldI8Ptr)
		} else {
			result = rhs
		}
	default:
		result = rhs
	}

	block.NewStore(result, ptr)

	return block, nil
}

func (cg *CodeGen) genPostfix(block *ir.Block, s *ast.PostfixStmt) error {
	ptr, err := cg.genLValue(block, s.Expr)
	if err != nil {
		return err
	}

	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	one := cg.coerce(block, constant.NewInt(irtypes.I64, 1), elemType)

	var result value.Value

	switch s.Op {
	case "++":
		result = block.NewAdd(current, one)
	case "--":
		result = block.NewSub(current, one)
	default:
		result = current
	}

	block.NewStore(result, ptr)

	return nil
}

func (cg *CodeGen) genIf(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	mergeBlock := cg.newBlock("if.merge")

	cond, err := cg.genExpr(block, s.Cond)
	if err != nil {
		return nil, false, err
	}

	cond = cg.toBool(block, cond)

	thenBlock := cg.newBlock("if.then")

	var elseStart *ir.Block
	if s.Else != nil || len(s.ElseIfs) > 0 {
		elseStart = cg.newBlock("if.else")
	} else {
		elseStart = mergeBlock
	}

	block.NewCondBr(cond, thenBlock, elseStart)

	// Then branch.
	cg.curScope = newScope(cg.curScope)

	thenCurBlock, thenTerm, err := cg.genBlock(thenBlock, s.Then)
	if thenCurBlock == nil {
		thenCurBlock = thenBlock
	}
	// ARC: release scope before popping (only when not already terminated).
	cg.emitScopeRelease(thenCurBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if err != nil {
		return nil, false, err
	}

	thenTerminated := thenTerm || thenCurBlock.Term != nil
	if !thenTerminated {
		thenCurBlock.NewBr(mergeBlock)
	}

	// ElseIf chains.
	allElifTerminated := true
	currentElse := elseStart

	for _, elif := range s.ElseIfs {
		nextBlock := cg.newBlock("elif.next")

		elifCond, err := cg.genExpr(currentElse, elif.Cond)
		if err != nil {
			return nil, false, err
		}

		elifCond = cg.toBool(currentElse, elifCond)
		elifThen := cg.newBlock("elif.then")
		currentElse.NewCondBr(elifCond, elifThen, nextBlock)

		cg.curScope = newScope(cg.curScope)

		elifCurBlock, elifTerm, err := cg.genBlock(elifThen, elif.Body)
		if elifCurBlock == nil {
			elifCurBlock = elifThen
		}

		cg.emitScopeRelease(elifCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, false, err
		}

		elifTerminated := elifTerm || elifCurBlock.Term != nil
		if !elifTerminated {
			elifCurBlock.NewBr(mergeBlock)

			allElifTerminated = false
		}

		currentElse = nextBlock
	}

	// Else branch.
	elseTerminated := false

	if s.Else != nil {
		cg.curScope = newScope(cg.curScope)

		elseCurBlock, elseTerm, err := cg.genBlock(currentElse, s.Else)
		if elseCurBlock == nil {
			elseCurBlock = currentElse
		}

		cg.emitScopeRelease(elseCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, false, err
		}

		elseTerminated = elseTerm || elseCurBlock.Term != nil
		if !elseTerminated {
			elseCurBlock.NewBr(mergeBlock)
		}
	} else if currentElse != mergeBlock && currentElse.Term == nil {
		currentElse.NewBr(mergeBlock)
	}

	// Only add unreachable to mergeBlock if ALL branches terminated (returned/
	// branched elsewhere). When there is no else clause, the false path always
	// reaches mergeBlock, so it can never be unreachable.
	allTerminated := thenTerminated && allElifTerminated && (s.Else != nil && elseTerminated)
	if mergeBlock.Term == nil && allTerminated {
		mergeBlock.NewUnreachable()
	}

	return mergeBlock, allTerminated, nil
}

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
	condBlock := cg.newBlock("for.cond")
	bodyBlock := cg.newBlock("for.body")
	afterBlock := cg.newBlock("for.after")

	block.NewBr(condBlock)

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
	cond = cg.toBool(condBlock, cond)

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

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(endBody, condBlock)
		} else {
			endBody.NewBr(condBlock)
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
	condBlock := cg.newBlock("for.cond")
	bodyBlock := cg.newBlock("for.body")
	postBlock := cg.newBlock("for.post")
	afterBlock := cg.newBlock("for.after")

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
		cg.curBlock = condBlock

		cond, err := cg.genExpr(condBlock, s.Cond)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != condBlock {
			condBlock = cg.curBlock
		}

		cg.curBlock = nil
		cond = cg.toBool(condBlock, cond)
		condBlock.NewCondBr(cond, bodyBlock, afterBlock)
	} else {
		condBlock.NewBr(bodyBlock)
	}

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
		if cg.curFnAutoYield {
			cg.genYieldAutoAt(postBlock, condBlock)
		} else {
			postBlock.NewBr(condBlock)
		}
	}

	// ARC: release init scope vars (e.g. loop counter) in the after block.
	cg.emitScopeRelease(afterBlock, cg.curScope)
	cg.curScope = cg.curScope.parent // pop loop scope

	return afterBlock, nil
}

func (cg *CodeGen) genForIn(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
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
	if iterFatPtr, instKey, ok := cg.tryCoerceToIter(block, iterVal); ok {
		return cg.genForIterTrait(block, s, iterFatPtr, instKey)
	}

	// Get element type.
	// For string fat-pointers ({i8*, i64}), default element type is i8 (byte).
	var elemType irtypes.Type = irtypes.I64
	if isStringType(iterVal.Type()) {
		elemType = irtypes.I8
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

	// Extract length and data pointer from fat ptr.
	fatPtrType := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)

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
	block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenVal := condBlock.NewLoad(irtypes.I64, lenAlloca)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenVal)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	bodyPtr := bodyBlock.NewLoad(irtypes.NewPointer(elemType), ptrAlloca)
	elemGep := bodyBlock.NewGetElementPtr(elemType, bodyPtr, bodyIdx)
	elemVal := bodyBlock.NewLoad(elemType, elemGep)

	// Register loop variable.
	elemAlloca := bodyBlock.NewAlloca(elemType)
	bodyBlock.NewStore(elemVal, elemAlloca)

	isElemRC := isRCTrackedType(elemType)
	// ARC: each iteration copies an element - retain to claim ownership.
	if isElemRC {
		cg.emitRetain(bodyBlock, elemVal)
	}

	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true, isRC: isElemRC})
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

	block.NewBr(condBlock)

	// Cond: i < end.
	iVal := condBlock.NewLoad(varType, loopVar)
	endLoad := cg.coerce(condBlock, end, varType)
	cond := condBlock.NewICmp(enum.IPredSLT, iVal, endLoad)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: loopVar, isAlloc: true})
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
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
			return nil, fmt.Errorf("struct pattern: unknown field %s.%s", structName, field.Name)
		}

		var fieldType irtypes.Type

		if st, ok := scrutType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
			fieldType = st.Fields[fieldIdx]
		} else {
			fieldType = irtypes.I64
		}

		gep := checkBlock.NewGetElementPtr(scrutType, scrutAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

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
			return fmt.Errorf("struct pattern: unknown field %s.%s", structName, field.Name)
		}

		var fieldType irtypes.Type

		if st, ok := scrutType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
			fieldType = st.Fields[fieldIdx]
		} else {
			fieldType = irtypes.I64
		}

		gep := block.NewGetElementPtr(scrutType, scrutAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

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
// Example: []  +  [x, ...xs]  ->  {0} ∪ [1,∞) = [0,∞)  -> exhaustive.
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
			// This arm covers [fixed, ∞).
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

		bodyBlock.NewBr(afterBlock)

		*anyFallthrough = true
	} else {
		var err2 error

		bodyBlock, _, err2 = cg.genStmt(bodyBlock, c.Body)
		if err2 != nil {
			return nil, err2
		}

		if bodyBlock != nil && bodyBlock.Term == nil {
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
			// len >= regularCount
			lenCond = checkBlock.NewICmp(enum.IPredSGE, arrLen, nConst)
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
	if s.IsType {
		return cg.genMatchType(block, s)
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
		return nil, fmt.Errorf("match .(type): nil expression")
	}

	unionName := cg.typeNameOf(val.Type())

	members, isUnion := cg.unionTypeMembers[unionName]
	if !isUnion {
		return nil, fmt.Errorf("match .(type) requires a tagged union type, got %s", unionName)
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

// genArrayDestructDecl handles:
//
//	let [a, b] [T] = arr          - uniform typed (compile-time indexing)
//	let [a, b] [T1, T2] = arr     - per-slot typed from [any]  (runtime bounds check)
//	let [x, ...xs] [T] = arr      - rest split
//	let [a, b] res = arr          - named type alias resolved to per-slot types
func (cg *CodeGen) genArrayDestructDecl(block *ir.Block, s *ast.ArrayDestructDecl) (*ir.Block, error) {
	// Resolve named type alias (e.g. `type res = @[i32, bool]`)
	if s.NamedType != nil && len(s.ElemTypes) == 0 {
		// Look up the named type in typeAliases
		typeName := ""
		if st, ok := s.NamedType.(*ast.SimpleType); ok {
			typeName = st.Name
		}

		if typeName != "" {
			if aliasedTE, ok2 := cg.typeAliases[typeName]; ok2 {
				if tat, ok3 := aliasedTE.(*ast.TupleArrayType); ok3 {
					s = &ast.ArrayDestructDecl{
						Names:     s.Names,
						ElemTypes: tat.ElemTypes,
						IsAny:     true,
						Value:     s.Value,
					}
				}
			}
		}
	}

	arrVal, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if arrVal == nil {
		return block, nil
	}

	// Count regular (non-rest) names and find rest name index
	regularCount := 0
	restIdx := -1

	for i, n := range s.Names {
		if len(n) > 3 && n[:3] == "..." {
			restIdx = i
		} else {
			regularCount++
		}
	}

	// For [any] or per-slot typed: emit runtime bounds check
	if s.IsAny {
		arrAlloca := block.NewAlloca(arrVal.Type())
		block.NewStore(arrVal, arrAlloca)
		lenGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		arrLen := block.NewLoad(irtypes.I64, lenGep)

		needed := constant.NewInt(irtypes.I64, int64(regularCount))
		cond := block.NewICmp(enum.IPredSLT, arrLen, needed)

		id := cg.labelCount
		cg.labelCount++
		panicBlock := cg.curFn.NewBlock(fmt.Sprintf("destruct.panic.%d", id))
		okBlock := cg.curFn.NewBlock(fmt.Sprintf("destruct.ok.%d", id))
		block.NewCondBr(cond, panicBlock, okBlock)

		msg := cg.newGlobalString(fmt.Sprintf("array destructuring: need %d elements, got fewer", regularCount))
		panicBlock.NewCall(cg.ensurePanicFn(), msg)
		// Use a proper ret (not unreachable) so that recovered panics can return.
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			panicBlock.NewRet(nil)
		} else {
			panicBlock.NewRet(cg.zeroValue(retType))
		}

		block = okBlock
	}

	// Determine uniform element LLVM type (used when ElemTypes has 1 entry or is empty)
	var elemLLType irtypes.Type = anyFatPtrType()
	if len(s.ElemTypes) == 1 {
		elemLLType, err = cg.tinTypeToLLVM(s.ElemTypes[0])
		if err != nil {
			return nil, err
		}
	}

	// Extract data pointer from fat array {elemPtr*, i64}
	arrAlloca := block.NewAlloca(arrVal.Type())
	block.NewStore(arrVal, arrAlloca)
	ptrFieldGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ptrField := block.NewLoad(arrVal.Type().(*irtypes.StructType).Fields[0], ptrFieldGep)

	// Extract each regular element
	regIdx := 0

	for _, name := range s.Names {
		if len(name) > 3 && name[:3] == "..." {
			continue
		}

		// Per-slot type or uniform type
		var slotType irtypes.Type
		if len(s.ElemTypes) > 1 {
			slotType, err = cg.tinTypeToLLVM(s.ElemTypes[regIdx])
			if err != nil {
				return nil, err
			}
		} else {
			slotType = elemLLType
		}

		idxVal := constant.NewInt(irtypes.I64, int64(regIdx))
		if pt, ok := ptrField.Type().(*irtypes.PointerType); ok {
			elemGep := block.NewGetElementPtr(pt.ElemType, ptrField, idxVal)
			loaded := block.NewLoad(pt.ElemType, elemGep)
			coerced := cg.coerce(block, loaded, slotType)
			alloca := block.NewAlloca(slotType)
			block.NewStore(coerced, alloca)
			cg.curScope.set(name, &scopeEntry{val: alloca, isAlloc: true})
		}

		regIdx++
	}

	// Handle rest: create a sub-slice starting at regularCount
	if restIdx >= 0 {
		restName := s.Names[restIdx][3:] // strip "..."

		var elemSzBytes int64 = 8

		if pt, ok := ptrField.Type().(*irtypes.PointerType); ok {
			if sz := llvmTypeSize(pt.ElemType); sz > 0 {
				elemSzBytes = int64(sz)
			}
		}

		// Build a generic {i8*, i64} slice for _tin_slice_subslice
		sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
		rawAlloca := block.NewAlloca(sliceType)

		dataPtrAsI8 := block.NewBitCast(ptrField, irtypes.I8Ptr)
		rawPtrGep := block.NewGetElementPtr(sliceType, rawAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(dataPtrAsI8, rawPtrGep)

		lenGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		arrLen := block.NewLoad(irtypes.I64, lenGep)
		rawLenGep := block.NewGetElementPtr(sliceType, rawAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(arrLen, rawLenGep)
		rawSlice := block.NewLoad(sliceType, rawAlloca)

		subFn := cg.ensureSliceSubslice()
		subResult := block.NewCall(subFn, rawSlice,
			constant.NewInt(irtypes.I64, int64(regularCount)),
			constant.NewInt(irtypes.I64, elemSzBytes))

		// Cast the {i8*, i64} result back to the original fat-array type
		restType := arrVal.Type()
		tmpAlloca := block.NewAlloca(sliceType)
		block.NewStore(subResult, tmpAlloca)
		castPtr := block.NewBitCast(tmpAlloca, irtypes.NewPointer(restType))
		restVal := block.NewLoad(restType, castPtr)
		restAlloca := block.NewAlloca(restType)
		block.NewStore(restVal, restAlloca)
		cg.curScope.set(restName, &scopeEntry{val: restAlloca, isAlloc: true})
	}

	return block, nil
}

// genStructDestructDecl handles: let {x, y} TypeName = expr
func (cg *CodeGen) genStructDestructDecl(block *ir.Block, s *ast.StructDestructDecl) (*ir.Block, error) {
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return block, nil
	}

	// Resolve the struct type name
	typeName := ""

	switch t := s.StructType.(type) {
	case *ast.SimpleType:
		typeName = t.Name
	case *ast.GenericType:
		typeName = t.Name
	}

	if typeName == "" {
		return nil, fmt.Errorf("struct destructuring: cannot determine struct type name")
	}

	concreteName := typeName
	if aliasedType, ok := cg.typeAliases[typeName]; ok {
		if st, ok2 := aliasedType.(*ast.SimpleType); ok2 {
			concreteName = st.Name
		}
	}

	fields, ok := cg.structFields[concreteName]
	if !ok {
		return nil, fmt.Errorf("struct destructuring: unknown struct type '%s'", concreteName)
	}

	llType, err := cg.tinTypeToLLVM(s.StructType)
	if err != nil {
		return nil, err
	}

	structAlloca := block.NewAlloca(llType)
	block.NewStore(val, structAlloca)

	_ = fields // validated above; actual indices computed via fieldIndex (includes hidden fields)

	for i, fieldName := range s.Names {
		varName := fieldName
		if i < len(s.VarNames) && s.VarNames[i] != "" {
			varName = s.VarNames[i]
		}

		fieldIdx := cg.fieldIndex(concreteName, fieldName)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("struct destructuring: field '%s' not found in struct '%s'", fieldName, concreteName)
		}

		fieldGep := block.NewGetElementPtr(llType, structAlloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(fieldIdx)))

		if pt, ok := fieldGep.Type().(*irtypes.PointerType); ok {
			fieldVal := block.NewLoad(pt.ElemType, fieldGep)
			alloca := block.NewAlloca(pt.ElemType)
			block.NewStore(fieldVal, alloca)
			// Determine if this field's Tin type is unsigned so `as` casts zext.
			var fieldUnsigned bool

			if tinTypes, ok2 := cg.structFieldTinTypes[concreteName]; ok2 {
				// fieldIdx includes the leading i32 type-id; user fields start at offset 1+vtables.
				userOffset := 1 + len(cg.structVtableOrder[concreteName])

				userIdx := fieldIdx - userOffset
				if userIdx >= 0 && userIdx < len(tinTypes) {
					fieldUnsigned = isUnsignedTinType(tinTypes[userIdx])
				}
			}

			cg.curScope.set(varName, &scopeEntry{val: alloca, isAlloc: true, isUnsigned: fieldUnsigned})
		}
	}

	return block, nil
}

// genTupleDestructDecl handles: let (x, y, ...) = expr
// Extracts fields a, b, c, ... from a Tuple struct value by position.
func (cg *CodeGen) genTupleDestructDecl(block *ir.Block, s *ast.TupleDestructDecl) (*ir.Block, error) {
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}
	// Update block in case genExpr advanced it (e.g. await generates a new resume block).
	if cg.curBlock != nil {
		block = cg.curBlock
	}

	if val == nil {
		return block, nil
	}

	concreteName := structNameFromValue(val)
	if concreteName == "" {
		return nil, fmt.Errorf("tuple destructuring: expected a Tuple struct value, got %s", val.Type())
	}

	llType, ok := cg.structTypes[concreteName]
	if !ok {
		return nil, fmt.Errorf("tuple destructuring: unknown struct type '%s'", concreteName)
	}

	structAlloca := block.NewAlloca(llType)
	block.NewStore(val, structAlloca)

	// Detect whether the source is a call to a heap-promoting function.
	// If so, each *T field in the destructured tuple is itself a heap-owned RC block.
	heapPromotingSource := false

	if callExpr, isCall := s.Value.(*ast.CallExpr); isCall {
		if fnIdent, isIdent := callExpr.Func.(*ast.Identifier); isIdent {
			heapPromotingSource = cg.heapPromotingFns[fnIdent.Name]
		}
	}

	// Tuple fields are named a, b, c, ... (alphabet by position).
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	userOff := 1 + cg.vtableOffset(concreteName) // skip type_id + vtable fields

	for i, name := range s.Names {
		if i >= len(letters) {
			break
		}

		fieldName := letters[i]

		fieldIdx := cg.fieldIndex(concreteName, fieldName)
		if fieldIdx < 0 {
			// Fall back to positional
			fieldIdx = userOff + i
		}

		if fieldIdx >= len(llType.Fields) {
			break
		}

		fieldType := llType.Fields[fieldIdx]
		fieldGep := block.NewGetElementPtr(llType, structAlloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(fieldIdx)))
		fieldVal := block.NewLoad(fieldType, fieldGep)
		alloca := block.NewAlloca(fieldType)
		block.NewStore(fieldVal, alloca)

		isHeapOwned := false
		heapOwnedDepth := 0

		if heapPromotingSource {
			depth := pointerChainDepth(fieldType)
			if depth > 0 {
				isHeapOwned = true
				heapOwnedDepth = depth
			}
		}

		cg.curScope.set(name, &scopeEntry{val: alloca, isAlloc: true, isHeapOwned: isHeapOwned, heapOwnedDepth: heapOwnedDepth})
	}

	return block, nil
}

// findEscapingAddressTakenVars performs a lightweight escape analysis on a
// function body.  It returns the set of local variable names whose addresses
// escape the function frame - i.e. a pointer to them is returned from the
// function.  These variables will be heap-promoted: genVarDecl allocates them
// with malloc instead of alloca so the memory remains valid after the callee
// returns.
//
// Patterns detected:
//
//	return &varName            -- address of local returned directly
//	let alias = &varName
//	return alias               -- address returned via an alias variable
func findEscapingAddressTakenVars(body ast.Node) (map[string]bool, map[string]string) {
	if body == nil {
		return nil, nil
	}
	// Pass 1: collect address-of aliases.  aliases[alias] = source variable name.
	aliases := make(map[string]string)
	walkForAliases(body, aliases)

	// Pass 2: collect escaping variables by inspecting return statements.
	escaping := make(map[string]bool)
	walkForEscapes(body, aliases, escaping)

	if len(escaping) == 0 {
		return nil, nil
	}

	return escaping, aliases
}

// walkForAliases walks node and populates aliases: for every
//
//	let name = &ident
//
// it records aliases[name] = ident.Name.
func walkForAliases(node ast.Node, aliases map[string]string) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.VarDecl:
		if n.Value != nil {
			if addrOf, ok := n.Value.(*ast.AddressOfExpr); ok {
				if ident, ok2 := addrOf.Expr.(*ast.Identifier); ok2 {
					aliases[n.Name] = ident.Name
				}
			}
		}
	case *ast.Block:
		for _, s := range n.Stmts {
			walkForAliases(s, aliases)
		}
	case *ast.IfStmt:
		if n.Then != nil {
			walkForAliases(n.Then, aliases)
		}

		for _, elif := range n.ElseIfs {
			walkForAliases(elif.Body, aliases)
		}

		if n.Else != nil {
			walkForAliases(n.Else, aliases)
		}
	case *ast.ForStmt:
		if n.Body != nil {
			walkForAliases(n.Body, aliases)
		}
	case *ast.MatchStmt:
		for _, c := range n.Cases {
			walkForAliases(c.Body, aliases)
		}

		if n.Default != nil {
			walkForAliases(n.Default, aliases)
		}
	}
}

// walkForEscapes walks node and populates escaping: for every ReturnStmt
// whose value is &ident or an alias of &ident, marks the source variable.
func walkForEscapes(node ast.Node, aliases map[string]string, escaping map[string]bool) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.ReturnStmt:
		if n.Value == nil {
			return
		}

		markEscapeVal(n.Value, aliases, escaping)
	case *ast.Block:
		for _, s := range n.Stmts {
			walkForEscapes(s, aliases, escaping)
		}
	case *ast.IfStmt:
		if n.Then != nil {
			walkForEscapes(n.Then, aliases, escaping)
		}

		for _, elif := range n.ElseIfs {
			walkForEscapes(elif.Body, aliases, escaping)
		}

		if n.Else != nil {
			walkForEscapes(n.Else, aliases, escaping)
		}
	case *ast.ForStmt:
		if n.Body != nil {
			walkForEscapes(n.Body, aliases, escaping)
		}
	case *ast.MatchStmt:
		for _, c := range n.Cases {
			walkForEscapes(c.Body, aliases, escaping)
		}

		if n.Default != nil {
			walkForEscapes(n.Default, aliases, escaping)
		}
	}
}

// markEscapeVal marks variables in aliases that escape via the given return value.
// Handles identifiers, address-of expressions, and tuples containing those.
// Alias chains are followed transitively: if `ppx = &px` and `px = &x`, returning
// ppx marks both px and x as escaping.
func markEscapeVal(val ast.Node, aliases map[string]string, escaping map[string]bool) {
	if val == nil {
		return
	}

	switch rv := val.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok {
			markEscapeChain(ident.Name, aliases, escaping)
		}
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok {
			markEscapeChain(src, aliases, escaping)
		}
	case *ast.TupleLit:
		for _, elem := range rv.Elems {
			markEscapeVal(elem, aliases, escaping)
		}
	}
}

// markEscapeChain transitively marks name and all its alias sources as escaping.
func markEscapeChain(name string, aliases map[string]string, escaping map[string]bool) {
	for name != "" && !escaping[name] {
		escaping[name] = true
		name = aliases[name] // follow the chain: if px = &x, also mark x
	}
}

// hasDirectHeapReturn returns true if any return statement in body returns a
// freshly heap-allocated pointer without going through a named local variable.
// This covers two patterns not caught by findEscapingAddressTakenVars:
//
//	return &StructLit{...}        -- inline heap allocation in return position
//	return heap_fn(args...)       -- forwarding the result of a heap-promoting fn
//
// heapFns is the current heapPromotingFns map so that callee lookups work for
// functions already processed (defined before the current one in the same file).
func hasDirectHeapReturn(body ast.Node, heapFns map[string]bool) bool {
	if body == nil {
		return false
	}

	found := false

	var walk func(ast.Node)

	walk = func(node ast.Node) {
		if node == nil || found {
			return
		}

		switch n := node.(type) {
		case *ast.ReturnStmt:
			if n.Value == nil {
				return
			}

			switch rv := n.Value.(type) {
			case *ast.AddressOfExpr:
				if _, isStruct := rv.Expr.(*ast.StructLit); isStruct {
					found = true
				}
			case *ast.CallExpr:
				if fnIdent, ok := rv.Func.(*ast.Identifier); ok && heapFns[fnIdent.Name] {
					found = true
				}
			}
		case *ast.Block:
			for _, s := range n.Stmts {
				walk(s)
			}
		case *ast.IfStmt:
			if n.Then != nil {
				walk(n.Then)
			}

			for _, elif := range n.ElseIfs {
				if elif.Body != nil {
					walk(elif.Body)
				}
			}

			if n.Else != nil {
				walk(n.Else)
			}
		case *ast.ForStmt:
			if n.Body != nil {
				walk(n.Body)
			}
		case *ast.MatchStmt:
			for _, c := range n.Cases {
				if c.Body != nil {
					walk(c.Body)
				}
			}

			if n.Default != nil {
				walk(n.Default)
			}
		}
	}

	walk(body)

	return found
}

// retainedHeapVars returns the subset of escaping vars that are actually returned
// by retExpr.  Any heap-promoted var NOT in this set can be freed at the return site.
// Uses the same resolution logic as markEscapeVal/markEscapeChain.
func retainedHeapVars(retExpr ast.Node, aliases map[string]string, escaping map[string]bool) map[string]bool {
	kept := make(map[string]bool)
	collectRetained(retExpr, aliases, escaping, kept)

	return kept
}

func collectRetained(node ast.Node, aliases map[string]string, escaping map[string]bool, kept map[string]bool) {
	if node == nil {
		return
	}

	switch rv := node.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok {
			collectChain(ident.Name, aliases, escaping, kept)
		}
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok {
			collectChain(src, aliases, escaping, kept)
		}
	case *ast.TupleLit:
		for _, elem := range rv.Elems {
			collectRetained(elem, aliases, escaping, kept)
		}
	}
}

func collectChain(name string, aliases map[string]string, escaping map[string]bool, kept map[string]bool) {
	for name != "" && escaping[name] && !kept[name] {
		kept[name] = true
		name = aliases[name]
	}
}

// pointerChainDepth counts the number of consecutive pointer dereferences in t.
// Returns 0 for non-pointer types, 1 for *T, 2 for **T, etc.
func pointerChainDepth(t irtypes.Type) int {
	depth := 0

	for {
		pt, ok := t.(*irtypes.PointerType)
		if !ok {
			break
		}

		depth++
		t = pt.ElemType
	}

	return depth
}

// genLatePromotedReturn handles return statements in functions where one or more
// local variables escape via their address.  The key invariant: defers may modify
// the stack copies of promoted variables, so we run defers FIRST, then copy the
// post-defer values into fresh _tin_rc_alloc blocks and return those.
//
// For tuple returns, non-promoted elements are latched BEFORE defers run so that
// the caller sees the pre-defer values (the same semantics as early-promotion).
func (cg *CodeGen) genLatePromotedReturn(block *ir.Block, s *ast.ReturnStmt, promoted map[string]bool) error {
	retType := cg.curFn.Sig.RetType

	if tup, ok := s.Value.(*ast.TupleLit); ok {
		structType, ok2 := retType.(*irtypes.StructType)
		if !ok2 {
			return fmt.Errorf("genLatePromotedReturn: expected struct type for tuple return, got %v", retType)
		}

		concreteName := structType.Name()
		userOff := 1 + cg.vtableOffset(concreteName)

		// Phase 1: latch non-promoted elements BEFORE defers run.
		type latched struct {
			val      value.Value
			retained bool
		}

		preLatch := make([]latched, len(tup.Elems))
		for i, elem := range tup.Elems {
			if isPromotedTupleElem(elem, cg.curFnEscapingAliases, promoted) {
				continue
			}

			v, err := cg.genExpr(block, elem)
			if err != nil {
				return err
			}

			fi := userOff + i
			if v != nil && fi < len(structType.Fields) {
				v = cg.coerce(block, v, structType.Fields[fi])
			}

			retained := false

			if isCopyExpr(elem) {
				cg.emitRetain(block, v)

				retained = true
			}

			preLatch[i] = latched{val: v, retained: retained}
		}

		// Run defers (may modify stack copies of promoted vars).
		if err := cg.emitDefers(block); err != nil {
			return err
		}

		// Phase 2: build the result tuple.
		alloca := block.NewAlloca(structType)
		block.NewStore(constant.NewZeroInitializer(structType), alloca)

		if typeID, has := cg.structTypeIDs[concreteName]; has {
			typeIDGep := block.NewGetElementPtr(structType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
		}

		for i, elem := range tup.Elems {
			fi := userOff + i
			if fi >= len(structType.Fields) {
				break
			}

			var v value.Value

			if isPromotedTupleElem(elem, cg.curFnEscapingAliases, promoted) {
				rootVar := promotedTupleElemVar(elem, cg.curFnEscapingAliases, promoted)

				var err error

				v, err = cg.emitChainedHeapPromotion(block, rootVar)
				if err != nil {
					return err
				}
			} else {
				v = preLatch[i].val
			}

			if v == nil {
				continue
			}

			v = cg.coerce(block, v, structType.Fields[fi])
			gep := block.NewGetElementPtr(structType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			block.NewStore(v, gep)
		}

		retVal := block.NewLoad(structType, alloca)
		cg.emitAllScopeReleases(block, "")
		block.NewRet(retVal)

		return nil
	}

	// Non-tuple: `return &x` or `return p` (p = &x).
	// No pre-defer latching needed - just run defers and build the RC block.
	if err := cg.emitDefers(block); err != nil {
		return err
	}

	rootVar := latePromotionRootVar(s.Value, cg.curFnEscapingAliases, promoted)
	if rootVar == "" {
		return fmt.Errorf("genLatePromotedReturn: cannot find promoted root var in %T", s.Value)
	}

	retVal, err := cg.emitChainedHeapPromotion(block, rootVar)
	if err != nil {
		return err
	}

	if cg.curFn != nil && !irtypes.IsVoid(retType) {
		retVal = cg.coerce(block, retVal, retType)
	}

	cg.emitAllScopeReleases(block, "")
	block.NewRet(retVal)

	return nil
}

// emitChainedHeapPromotion promotes rootVar (and all variables in its alias chain
// that are in curFnEscapingVars) from stack to ARC heap blocks.  Inner variables
// (further down the alias chain) are promoted first; each parent's alloca is then
// updated to hold the child's heap pointer before the parent is promoted.  This
// ensures that returned pointer chains are fully heap-resident.
func (cg *CodeGen) emitChainedHeapPromotion(block *ir.Block, rootVar string) (value.Value, error) {
	aliases := cg.curFnEscapingAliases
	promoted := cg.curFnEscapingVars

	// Build the chain from rootVar following alias links in promoted.
	chain := []string{rootVar}
	for {
		cur := chain[len(chain)-1]

		next, ok := aliases[cur]
		if !ok || next == "" || !promoted[next] {
			break
		}

		chain = append(chain, next)
	}

	// heapPtrs maps varName -> its heap block pointer (typed as *T for T = element type).
	heapPtrs := make(map[string]value.Value)

	// Promote from leaf (last in chain) to root (first in chain).
	for i := len(chain) - 1; i >= 0; i-- {
		varName := chain[i]

		entry, ok := cg.curScope.lookup(varName)
		if !ok || !entry.isAlloc {
			return nil, fmt.Errorf("emitChainedHeapPromotion: var %q not found in scope", varName)
		}

		ptrType, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			return nil, fmt.Errorf("emitChainedHeapPromotion: var %q alloca not a pointer type", varName)
		}

		elemType := ptrType.ElemType

		// If this var points to a child that was just promoted, update the alloca
		// so it holds the child's heap pointer instead of the child's stack address.
		if i < len(chain)-1 {
			childHeapPtr := heapPtrs[chain[i+1]]
			childCast := block.NewBitCast(childHeapPtr, elemType)
			block.NewStore(childCast, entry.val)
		}

		// Load the (potentially updated) value from the stack alloca.
		stackVal := block.NewLoad(elemType, entry.val)

		// Allocate ARC block and copy the value into it.
		sz := cg.llvmSizeOf(block, elemType)
		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		heapPtr := block.NewBitCast(heapI8, irtypes.NewPointer(elemType))
		block.NewStore(stackVal, heapPtr)

		// Retain ARC sub-fields (strings, arrays) so scope cleanup on the stack
		// copy is balanced.  For plain i64/pointers this is a no-op.
		cg.emitRetain(block, stackVal)

		heapPtrs[varName] = heapPtr
	}

	return heapPtrs[rootVar], nil
}

// latePromotionRootVar extracts the name of the underlying escaping variable from
// a simple return expression: `return &x` -> "x", `return p` (p=&x) -> "x".
func latePromotionRootVar(node ast.Node, aliases map[string]string, promoted map[string]bool) string {
	switch rv := node.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok && promoted[ident.Name] {
			return ident.Name
		}
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok && promoted[src] {
			return src
		}
		// Also handle direct identifier that is itself the promoted var
		if promoted[rv.Name] {
			return rv.Name
		}
	}

	return ""
}

// isPromotedTupleElem reports whether a tuple element is a promoted pointer
// (either &x where x is promoted, or an alias identifier p where aliases[p] is promoted).
func isPromotedTupleElem(elem ast.Node, aliases map[string]string, promoted map[string]bool) bool {
	return promotedTupleElemVar(elem, aliases, promoted) != ""
}

// promotedTupleElemVar returns the root escaping variable name for a promoted
// tuple element, or "" if the element is not promoted.
func promotedTupleElemVar(elem ast.Node, aliases map[string]string, promoted map[string]bool) string {
	switch e := elem.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := e.Expr.(*ast.Identifier); ok && promoted[ident.Name] {
			return ident.Name
		}
	case *ast.Identifier:
		if src, ok := aliases[e.Name]; ok && promoted[src] {
			return src
		}
	}

	return ""
}
