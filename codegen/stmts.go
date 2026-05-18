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

			if decl, ok := cg.funcDecls[fnName]; ok {
				return cg.nodeErr(decl, "fn %s: not all code paths return a value", fnName)
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
		// Tail-expression match: when the body block contains exactly one
		// statement and that statement is a MatchStmt whose arms each yield
		// a single expression, compile the match in expression mode so the
		// arm values propagate as the function's return value.
		if !irtypes.IsVoid(retType) && len(b.Stmts) == 1 {
			if ms, ok := b.Stmts[0].(*ast.MatchStmt); ok && tailMatchUsableAsExpr(ms) {
				val, err := cg.genMatchAsExpr(block, ms)
				if err != nil {
					return false, err
				}

				curBlock := cg.curBlock
				if curBlock == nil {
					curBlock = block
				}

				if val != nil {
					val = cg.coerce(curBlock, val, retType)
					emitTerminator(curBlock, val, "")
				} else {
					if err := addDefaultRet(curBlock); err != nil {
						return false, err
					}
				}

				return true, nil
			}
		}

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
			// genExpr may have advanced `cg.curBlock` past the original
			// `block` -- e.g. `await spawn ...` finishes in the synthetic
			// `await.ok` block, not the entry block.  The terminator
			// must land where the result is live, so prefer cg.curBlock
			// when it differs.  Pre-fix this emitted on the (already
			// terminated) entry block, leaving `await.ok` dangling and
			// triggering "missing terminator" during IR serialization.
			curBlock := cg.curBlock
			if curBlock == nil {
				curBlock = block
			}

			if val != nil {
				val = cg.coerce(curBlock, val, retType)

				retSkip := ""
				if ident, ok := inner.(*ast.Identifier); ok {
					retSkip = ident.Name
				}

				emitTerminator(curBlock, val, retSkip)
			} else {
				if err := addDefaultRet(curBlock); err != nil {
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
		// Tail match in a value-returning function: each arm provides the
		// result expression, e.g. `fn f(x) T = match x: case A: 1 case B: 2`.
		// Compile in expression mode so the arm bodies materialize the
		// function result rather than running as statements with no value.
		if ms, ok := b.(*ast.MatchStmt); ok && !irtypes.IsVoid(retType) && tailMatchUsableAsExpr(ms) {
			val, err := cg.genMatchAsExpr(block, ms)
			if err != nil {
				return false, err
			}

			curBlock := cg.curBlock
			if curBlock == nil {
				curBlock = block
			}

			if val != nil {
				val = cg.coerce(curBlock, val, retType)
				emitTerminator(curBlock, val, "")
			} else {
				if err := addDefaultRet(curBlock); err != nil {
					return false, err
				}
			}

			return true, nil
		}
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

// tailMatchUsableAsExpr reports whether a MatchStmt can be compiled via
// genMatchAsExpr as the tail of a value-returning function body. Each arm
// (and the default) must contain exactly one *expression* statement (an
// ast.ExprStmt) whose value is the arm's result. Arms that use explicit
// `return`, nested matches, conditionals, etc. fall through to the regular
// statement path so those statements keep working as before.
func tailMatchUsableAsExpr(s *ast.MatchStmt) bool {
	if s == nil {
		return false
	}

	if s.IsType {
		// `match v.(type):` arms commonly use explicit return; keep the
		// statement-mode compilation path for tagged-union dispatch.
		return false
	}

	if len(s.Cases) == 0 {
		return false
	}

	armIsBareExpr := func(body *ast.Block) bool {
		if body == nil || len(body.Stmts) != 1 {
			return false
		}

		_, ok := body.Stmts[0].(*ast.ExprStmt)

		return ok
	}

	for _, c := range s.Cases {
		if !armIsBareExpr(c.Body) {
			return false
		}
	}

	if s.Default != nil && !armIsBareExpr(s.Default) {
		return false
	}

	return true
}

// genBlock generates a sequence of statements in the given block.
// Returns (currentBlock, terminated, error). currentBlock is the block that
// should receive the next instruction after the block's statements; it may
// differ from the incoming block when nested control-flow (if/for/match)
// creates new merge blocks.
func (cg *CodeGen) genBlock(block *ir.Block, b *ast.Block) (*ir.Block, bool, error) {
	// Each { } block gets its own DWARF lexical scope so the debugger knows
	// the extent of variables declared inside it.
	restoreDbgScope := cg.pushLexicalBlock(b.Pos().Line)
	defer restoreDbgScope()

	var err error

	for i, stmt := range b.Stmts {
		if block == nil {
			panic(fmt.Sprintf("genBlock: block nil before stmt %T", stmt))
		}

		var terminated bool

		block, terminated, err = cg.genStmt(block, stmt)
		if err != nil {
			return nil, false, err
		}

		if terminated || block == nil {
			// Warn about any statements following an explicit terminator
			// (return / break / panic-style call). We deliberately skip the
			// warning when the terminator is structural - an `if` chain that
			// the analyzer/folder discovered always returns, a `for` whose
			// condition collapsed away, a match that exhausts every arm -
			// because the source code is still branching as written; "dead"
			// is a property of the monomorphized callsite (e.g. typeof(v) ==
			// 'i64 in encode[T]) rather than user-visible mistake.
			if i+1 < len(b.Stmts) && (isExplicitTerminator(stmt) || isFullyTerminatingStructural(stmt)) {
				cg.warn(DiagUnreachableCode, b.Stmts[i+1].Pos(),
					"unreachable code after %s", terminatorKind(stmt))
			}

			return nil, true, nil
		}
	}

	return block, false, nil
}

// isExplicitTerminator reports whether stmt is a syntactic control-flow
// terminator (return, break, panic-style call). Structural constructs like
// if / for / match that the analyzer happens to discover always-terminate
// after monomorphization don't count - issuing -Wunreachable-code on
// "the rest of an if/elif chain whose typeof(v) ==' branches were folded
// down to one live path" is noise, not a useful diagnostic.
func isExplicitTerminator(stmt ast.Node) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt, *ast.BreakStmt:
		return true
	case *ast.ExprStmt:
		if call, ok := s.Expr.(*ast.CallExpr); ok {
			if id, ok2 := call.Func.(*ast.Identifier); ok2 && id.Name == "panic" {
				return true
			}
		}
	}

	return false
}

// terminatorKind returns a short human-readable name for a control-flow
// terminator statement, used in the unreachable-code warning.
func terminatorKind(stmt ast.Node) string {
	switch stmt.(type) {
	case *ast.ReturnStmt:
		return "return"
	case *ast.BreakStmt:
		return "break"
	case *ast.IfStmt:
		return "if/else where every branch returns"
	case *ast.MatchStmt:
		return "match where every arm returns"
	}

	if call, ok := stmt.(*ast.ExprStmt); ok {
		if c, ok2 := call.Expr.(*ast.CallExpr); ok2 {
			if id, ok3 := c.Func.(*ast.Identifier); ok3 && id.Name == "panic" {
				return "panic"
			}
		}
	}

	return "terminator"
}

// isFullyTerminatingStructural reports whether a structural statement
// (if-chain, match, block) terminates control flow on every path it
// can take. Used by the unreachable-code check to flag dead statements
// after a complete `if cond: return; else: return` (or a match whose
// every arm returns), since those are syntactically distinguishable
// from the monomorphization-driven structural-fold case the existing
// isExplicitTerminator carve-out worries about.
func isFullyTerminatingStructural(stmt ast.Node) bool {
	switch s := stmt.(type) {
	case *ast.Block:
		return blockEndsTerminating(s)
	case *ast.IfStmt:
		// Every arm including else must terminate; missing else means
		// the no-arm-taken path falls through.
		if s.Else == nil {
			return false
		}

		if !blockEndsTerminating(s.Then) {
			return false
		}

		for _, ei := range s.ElseIfs {
			if !blockEndsTerminating(ei.Body) {
				return false
			}
		}

		return blockEndsTerminating(s.Else)
	case *ast.MatchStmt:
		// Every case body must terminate. Default is required because
		// without it the match falls through on no-match.
		if s.Default == nil {
			return false
		}

		for _, c := range s.Cases {
			if !nodeEndsTerminating(c.Body) {
				return false
			}
		}

		return nodeEndsTerminating(s.Default)
	}

	return false
}

func blockEndsTerminating(b *ast.Block) bool {
	if b == nil || len(b.Stmts) == 0 {
		return false
	}

	return nodeEndsTerminating(b.Stmts[len(b.Stmts)-1])
}

func nodeEndsTerminating(n ast.Node) bool {
	if n == nil {
		return false
	}

	if b, ok := n.(*ast.Block); ok {
		return blockEndsTerminating(b)
	}

	return isExplicitTerminator(n) || isFullyTerminatingStructural(n)
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
			cg.emitAllScopeReleases(newBlock, "")
			newBlock.NewRet(nil)
		}

		return nil
	}

	// TCO: intercept a bare self-call expression body and rewrite as a loop-back.
	if cg.tcoFuncName != "" {
		if ce, ok := body.(*ast.CallExpr); ok {
			if ident, ok2 := ce.Func.(*ast.Identifier); ok2 && ident.Name == cg.tcoFuncName {
				return cg.emitTCOLoopBack(block, ce)
			}
		}
	}

	// Expression body: evaluate and return value.
	// Seed currentPos from the body node's position so the ret gets the
	// correct source line in debug builds.
	if p := body.Pos(); p.Line != 0 {
		cg.currentPos = p
	}

	bodyVal, err := cg.genExpr(block, body)
	if err != nil {
		return err
	}

	// Sync up with cg.curBlock in case genExpr redirected emission to a new block.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	// ARC: release scope variables (parameters) before returning.
	// Parameters are retained on function entry; the return terminator must
	// balance that retain.  Skip releasing the variable being directly
	// returned (to transfer ownership to the caller without an extra retain).
	skipName := ""
	if ident, ok := body.(*ast.Identifier); ok {
		skipName = ident.Name
	}

	_ = cg.emitDefers(block)
	if !irtypes.IsVoid(retType) && bodyVal != nil {
		bodyVal = cg.coerce(block, bodyVal, retType)
		cg.emitAllScopeReleases(block, skipName)

		if cg.inCoroFn {
			cg.emitCoroComplete(block, bodyVal)
			cg.emitFinalSuspend(block, cg.curCoroFrame)
		} else {
			retInst := block.NewRet(bodyVal)
			cg.attachCurrentDbgLocToTerm(retInst)
		}
	} else {
		cg.emitAllScopeReleases(block, "")

		if cg.inCoroFn {
			cg.emitCoroComplete(block, nil)
			cg.emitFinalSuspend(block, cg.curCoroFrame)
		} else {
			retInst := block.NewRet(nil)
			cg.attachCurrentDbgLocToTerm(retInst)
		}
	}

	return nil
}

// genWhereCondition generates an i1 condition for a where clause condition.
// When the condition is an AtomLit and a match subject is set, it emits a
// comparison against the subject.
func (cg *CodeGen) genWhereCondition(block *ir.Block, condNode ast.Node) (value.Value, error) {
	// Establish the post-call invariant up front: if a path below doesn't
	// advance control flow, the caller's `cg.curBlock != nil` check will
	// correctly resolve to `block` rather than picking up a stale value
	// from a prior call.
	cg.curBlock = block

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

	end := block
	if cg.curBlock != nil {
		end = cg.curBlock
	}

	return cg.toBoolImplicit(end, cond), nil
}

// whereMode is the kind of dispatch a where-list uses.
type whereMode int

const (
	whereModeBool    whereMode = iota // bool guards (+ bare `_` catch-all)
	whereModePattern                  // pattern clauses (+ bare `_` catch-all)
)

// classifyWhereList determines the dispatch mode for a where-list and
// enforces that bool clauses and pattern clauses are not mixed. A bare `_`
// wildcard clause (Cond == nil, Pattern == nil) is compatible with both
// modes; it does not pin the mode.
//
// Returns a descriptive error when mixing is detected, pointing the user at
// the `where (pat) if cond:` form that replaces inline bool clauses in
// pattern mode.
func classifyWhereList(wl *ast.WhereList) (whereMode, error) {
	hasBool := false
	hasPattern := false

	var firstBoolPos, firstPatPos ast.Pos

	for _, c := range wl.Clauses {
		if c.Pattern != nil {
			if !hasPattern {
				firstPatPos = c.Pos
			}

			hasPattern = true

			continue
		}

		if c.Cond != nil {
			if !hasBool {
				firstBoolPos = c.Pos
			}

			hasBool = true
		}
	}

	if hasBool && hasPattern {
		return 0, fmt.Errorf("%d:%d: cannot mix bool clauses and pattern clauses in the same where-list (bool clause here conflicts with pattern clause at %d:%d); use \"where (pat) if %s:\" or split into separate where-lists",
			firstBoolPos.Line, firstBoolPos.Col,
			firstPatPos.Line, firstPatPos.Col,
			"cond")
	}

	if hasPattern {
		return whereModePattern, nil
	}

	return whereModeBool, nil
}

// isCatchAllWhereClause reports whether a clause matches every input (the
// bare `_` wildcard). Guarded wildcards are refutable and don't count.
func isCatchAllWhereClause(c ast.WhereClause) bool {
	return c.Cond == nil && c.Pattern == nil && c.Guard == nil
}

// genWhereList generates code for a where-list. A where-list is either all
// bool clauses (classic chained if-else lowering) or all pattern clauses
// (lowered as a match on the function's arg or tuple-of-args). A bare `_`
// wildcard clause is compatible with both modes and always serves as the
// final catch-all.
//
// Mixing bool clauses and pattern clauses in the same where-list is a compile
// error: `where (x) if cond: ...` covers every case a bool clause could.
func (cg *CodeGen) genWhereList(block *ir.Block, wl *ast.WhereList, retType irtypes.Type) (bool, error) {
	mode, modeErr := classifyWhereList(wl)
	if modeErr != nil {
		return false, modeErr
	}

	if mode == whereModePattern {
		return cg.genPatternWhereList(block, wl, retType)
	}

	// Simplify each bool clause's condition first; an "always true/false"
	// warning is emitted when the result folds to a constant. Clauses with
	// no Cond (bare `where _:` wildcard) are left alone.
	for i := range wl.Clauses {
		if wl.Clauses[i].Cond != nil {
			wl.Clauses[i].Cond = cg.prepareBoolCond(wl.Clauses[i].Cond, "where", false)
		}
	}

	cg.scanWhereForUnreachable(wl)

	// Bool mode: a where-list must include a catch-all clause - either a bare
	// `_` wildcard, or a bind-all `where (name):` pattern (treated as wildcard
	// here since it would match any value).
	hasWildcard := false

	for _, c := range wl.Clauses {
		if isCatchAllWhereClause(c) {
			hasWildcard = true

			break
		}
	}

	if !hasWildcard {
		pos := wl.Pos()
		if len(wl.Clauses) > 0 {
			pos = wl.Clauses[0].Pos
		}

		return false, fmt.Errorf("%d:%d: non-exhaustive where: missing wildcard clause \"where _: ...\"",
			pos.Line, pos.Col)
	}

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

		// Seed currentPos from the clause condition's position so that the
		// condition instructions and branch get tagged with the right line.
		if p := clause.Cond.Pos(); p.Line != 0 {
			cg.currentPos = p
		}

		// Evaluate condition.
		cond, err := cg.genWhereCondition(block, clause.Cond)
		if err != nil {
			return false, err
		}
		// Pick up curBlock so the cond-branch goes into the post-cond
		// block (advanced when the cond contained short-circuit && / ||).
		condEnd := block
		if cg.curBlock != nil {
			condEnd = cg.curBlock
		}
		// Reset cg.curBlock so the then/else body code-gen starts fresh
		// from its own block; otherwise stale curBlock from the condition
		// would leak into the body's block-refresh logic.
		cg.curBlock = nil

		thenBlock := cg.newBlock(fmt.Sprintf("where.then.%d", i))

		var elseBlock *ir.Block
		if i == len(wl.Clauses)-1 {
			elseBlock = getMerge()
		} else {
			elseBlock = cg.newBlock(fmt.Sprintf("where.else.%d", i))
		}

		condBr := condEnd.NewCondBr(cond, thenBlock, elseBlock)
		cg.attachCurrentDbgLocToTerm(condBr)

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
	// Update current source position and record block state for !dbg attachment.
	if pos := node.Pos(); pos.Line != 0 {
		cg.currentPos = pos
	}

	var dbgInstBefore int

	wantPosTrack := (cg.debugMode || cg.pclntabUsed) && block != nil
	if wantPosTrack {
		dbgInstBefore = len(block.Insts)
	}

	outBlock, term, err := cg.genStmtInner(block, node)

	// Attach source-position info to every new instruction emitted by
	// this statement. Two consumers:
	//   - debug builds (-g): attachCurrentDbgLoc adds !dbg DILocation
	//     metadata, materialized as DWARF .debug_line for lldb / gdb.
	//   - pclntab (always when stacktrace is reachable): the same call
	//     populates cg.instLineCol; pclntab.go's post-pass reads from
	//     there to anchor per-call PC entries.
	// Both paths share the attach helper but are independently gated
	// inside attachCurrentDbgLoc, so release-with-stacktrace gets the
	// side map only and emits no DWARF.
	if wantPosTrack && !cg.emittingARC && err == nil {
		for i := dbgInstBefore; i < len(block.Insts); i++ {
			cg.attachCurrentDbgLoc(block.Insts[i])
		}
	}

	return outBlock, term, err
}

// genStmtInner is the actual dispatch body for genStmt.
func (cg *CodeGen) genStmtInner(block *ir.Block, node ast.Node) (*ir.Block, bool, error) {
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

			val, err := cg.genExpr(block, s.Expr)
			if err != nil {
				return nil, false, err
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			// Release a discarded RC-tracked result (e.g.
			// `await ch.recv()` where ch is Channel[string]).  Without
			// this, every chan-recv'd RC value leaks: the await
			// produced an owned reference for the caller, and dropping
			// it without a binding has no other place to release it.
			if val != nil && !isVoidType(val.Type()) && isRCTrackedType(val.Type()) {
				cg.emitRelease(block, val)
			}

			return block, false, nil
		}

		cg.curBlock = block

		// Statement-level SpawnExpr: result is discarded (fire-and-forget).
		// Mark spawnFireForget so activeSpawnFn() uses fiberSpawnFn (prejoined=0),
		// allowing the fiber to be ff_reclaimed at completion for slot reuse.
		if _, ok := s.Expr.(*ast.SpawnExpr); ok {
			cg.spawnFireForget = true
		}

		val, err := cg.genExpr(block, s.Expr)
		cg.spawnFireForget = false

		// If genExpr advanced the current block (e.g. an await arg created new
		// blocks), use the continuation block for any subsequent emission.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		// After a call with N*S out-param write-backs, mark the written variables
		// as heap-owned so scope-exit releases the borrow wrapper(s).
		if callExpr, ok := s.Expr.(*ast.CallExpr); ok {
			cg.markOutParamVarsHeapOwned(callExpr)

			// Discarded result of a non-void call: warn. Result-returning
			// calls fire the always-on -Wmust-use diagnostic; pure
			// calls fire -Wdiscarded-pure-call (the call has no observable
			// effect at all). Everything else falls back to the default-off
			// -Wunused-result. Spawn/await results were already short-
			// circuited above; this only fires for plain calls.
			if val != nil && !isVoidType(val.Type()) {
				switch {
				case cg.calleeReturnsMustUse(callExpr):
					cg.warn(DiagUnusedMustUse, callExpr.Pos(),
						"%s",
						cg.mustUseMessage(callExpr))
				case cg.isCalleePure(callExpr):
					cg.warn(DiagDiscardedPureCall, callExpr.Pos(),
						"discarded result of pure call to %s has no effect",
						callDisplayName(callExpr))
				default:
					cg.warn(DiagUnusedResult, callExpr.Pos(),
						"discarded result of call to %s; use `_ = ...` to silence",
						callDisplayName(callExpr))
				}
			}
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
		// Tin doesn't support a `fn name(...) =` declaration inside another
		// function body.  The hoist-to-module-scope path that the old code
		// took here silently produced TWO `define @name` IR entries when the
		// outer fn was traversed by more than one codegen pass (e.g. the
		// $colored predeclare loop), and `opt` rejects the redefinition.
		// Reject at the source level with a redirect to the lambda form,
		// which is the supported way to bind a callable in a local scope:
		//
		//   let name = fn(p1, p2, ...) RetTy = ...body...
		//
		// The lambda binds an ordinary local variable so it composes with
		// the rest of the language (captures by closure, can be passed as
		// an arg, lives in the let-scope) and has none of the duplicate-
		// emission shape.
		return nil, false, cg.nodeErr(s,
			"`fn %s` inside a function body is not supported; "+
				"use a lambda bound to a let:  let %s = fn(...) RetTy = ...body...",
			s.Name, s.Name)

	case *ast.TaggedBlock:
		if hasTag(s.Tags, "unsafe") {
			cg.unsafeDepth++

			defer func() { cg.unsafeDepth-- }()
		}

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
