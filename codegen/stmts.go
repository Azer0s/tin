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
			// calls fire the always-on -Wunused-must-use diagnostic; pure
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
		// Nested function declaration - hoist to top level.
		if err := cg.genFuncDecl(s); err != nil {
			return nil, false, err
		}

		return block, false, nil

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
		} else if fillLit, ok := s.Value.(*ast.ArrayFillLit); ok {
			if _, isStaticLLVM := llType.(*irtypes.ArrayType); isStaticLLVM {
				// Static fixed-size target: fill emitted after alloca creation.
				_ = fillLit // handled in post-alloca block below
			} else {
				initVal, err = cg.genArrayFillLit(block, fillLit)
			}
		} else if arrLit, ok := s.Value.(*ast.ArrayLit); ok && s.Type != nil {
			// ArrayLit with declared element type: coerce each element to the declared type.
			// Handles e.g. let fns [fn{#async}(i64) i64] = [double] where elements need wrapping.
			if _, isStaticLLVM := llType.(*irtypes.ArrayType); isStaticLLVM {
				// Static fixed-size target: fill emitted after alloca creation.
				_ = arrLit // handled in post-alloca block below
			} else {
				var targetElemType irtypes.Type
				if at, ok2 := s.Type.(*ast.ArrayType); ok2 && at.Elem != nil {
					targetElemType, _ = cg.tinTypeToLLVM(at.Elem)
				}

				initVal, err = cg.genArrayLitWithElemType(block, arrLit, targetElemType)
			}
		} else {
			// When an explicit type annotation is present, propagate it as a hint
			// so overload resolution can prefer the variant whose return type matches
			// (let binding type > concrete arg types > constant arg types).
			if llType != nil {
				cg.returnTypeHint = llType
			}

			initVal, err = cg.genExpr(block, s.Value)
			cg.returnTypeHint = nil
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

	// Track whether we ever had a declared annotation. The struct-fallback
	// override below must NOT fire for user-declared `i64` -- only for
	// the implicit i64-fallback case where no annotation was given.
	hadDeclaredType := s.Type != nil

	if llType == nil {
		llType = irtypes.I64
	}

	// Generic-alias resolution: when the declared type is missing AND
	// the init value has a concrete struct type, use that. This handles
	// `let t = expr` where expr returns a Generic[T] resolved struct.
	// Skip when an explicit annotation is present -- the type-mismatch
	// check below should fire instead.
	if !hadDeclaredType && initVal != nil && llType.Equal(irtypes.I64) {
		if _, isStruct := initVal.Type().(*irtypes.StructType); isStruct {
			llType = initVal.Type()
		}
	}

	if block == nil {
		panic(fmt.Sprintf("genVarDecl: block is nil for var %q (llType=%v, curBlock=%v, curFn=%v)", s.Name, llType, cg.curBlock, cg.curFn))
	}

	// #no_copy enforcement: a let-binding cannot hold a value of a #no_copy
	// struct, since a subsequent reference would alias the underlying cell.
	// `*S` is fine -- pointer copies just retain.
	if name := cg.typeNameOf(llType); name != "" && cg.noCopyStructs[name] {
		return nil, cg.nodeErr(s,
			"%s is #no_copy: bind a *%s instead -- value-form let aliases the cell and double-frees on scope exit",
			prettyStructName(name), prettyStructName(name))
	}

	// REPL mode: promote top-level `let` bindings in the cell function to LLVM
	// global variables so their values persist across subsequent cells.
	// Static-array fill/literal allocas are skipped (they need alloca semantics).
	isReplCellFn := cg.curFn != nil && (cg.curFn.Name() == cg.replCellFuncName ||
		cg.curFn.Name() == cg.replCellFuncName+"$coro")
	if cg.replMode && !s.IsConst && isReplCellFn {
		_, isStaticArray := llType.(*irtypes.ArrayType)

		if !isStaticArray {
			if llType == nil {
				llType = irtypes.I64
			}
			// Check the scope for a previous-cell external global first.
			if existing, ok := cg.curScope.lookup(s.Name); ok && existing.isGlobal {
				if g, ok2 := existing.val.(*ir.Global); ok2 && initVal != nil {
					initVal = cg.coerce(block, initVal, g.ContentType)
					cg.emitRetain(block, initVal)
					block.NewStore(initVal, g)
				}

				return block, nil
			}
			// Check the persistent cell-globals map so the $coro variant of the
			// cell function can find the global created by the non-coro variant,
			// even after the non-coro function scope was popped.
			if g, ok := cg.replCellGlobals[s.Name]; ok {
				cg.curScope.set(s.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(g.ContentType), isGlobal: true})

				if initVal != nil {
					initVal = cg.coerce(block, initVal, g.ContentType)
					cg.emitRetain(block, initVal)
					block.NewStore(initVal, g)
				}

				return block, nil
			}

			g := cg.mod.NewGlobal(s.Name, llType)
			g.Init = cg.zeroConstant(llType)
			isRC := isRCTrackedType(llType)
			cg.curScope.set(s.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRC, isGlobal: true})

			cg.replCellGlobals[s.Name] = g
			if initVal != nil {
				initVal = cg.coerce(block, initVal, llType)
				cg.emitRetain(block, initVal)
				block.NewStore(initVal, g)
			}

			cg.replNewGlobals = append(cg.replNewGlobals, ReplGlobal{Name: s.Name, TinType: s.Type, LLVMType: llType})

			return block, nil
		}
	}

	// Local variables are stack-allocated by default. When escape analysis
	// (cg.curFnEscapingVars) flagged this binding as having `&x` reach an
	// escape sink -- return, struct-field of escaping struct, *Trait coerce,
	// channel send, spawn arg, etc. -- heap-allocate it via _tin_rc_alloc
	// instead so &x is a stable pointer outliving the frame. entry.val
	// becomes the heap pointer directly (same LLVM type as a stack alloca:
	// `*T`), so every later `genLValue(Ident)` returns the heap pointer
	// without extra indirection. Scope-exit emits _tin_release on entry.val
	// (see emitScopeRelease's isEarlyHeap branch).
	earlyHeap := cg.curFnEscapingVars[s.Name]

	var alloca value.Value

	if earlyHeap {
		sz := cg.llvmSizeOf(block, llType)
		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		alloca = block.NewBitCast(heapI8, irtypes.NewPointer(llType))
		// Zero-init the heap block so reads of unwritten fields aren't
		// uninitialized (mirrors what alloca's caller relies on).
		block.NewStore(cg.zeroValue(llType), alloca)
	} else {
		alloca = block.NewAlloca(llType)
	}

	// Emit dbg.declare for debug builds. Stack allocas only -- heap-promoted
	// vars don't have an alloca to attach the dbg.declare intrinsic to.
	if stackAlloca, ok := alloca.(*ir.InstAlloca); ok {
		cg.emitDbgDeclare(block, stackAlloca, s.Name, s.Pos().Line, 0, s.Type, llType)
	}

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
		case *ast.ScopeAccess:
			calleeName = strings.Join(fn.Path, "__")
		case *ast.FieldAccess:
			// Static method call written with dot syntax -- `S.alloc(...)` or
			// `Generic[T].alloc(...)` (via FieldAccess on Identifier or
			// IndexExpr respectively). Resolve to the IR function name so
			// heapPromotingFns lookup matches.
			if name := cg.staticCallIRName(fn); name != "" {
				calleeName = name
			}
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
			// Resolve generic static method scope access, e.g. "tree_node[i64]__branch"
			// -> "tree_node__i64_branch" (the key stored in heapPromotingFns).
			if !isHeapFn {
				if sa, ok := callExpr.Func.(*ast.ScopeAccess); ok && len(sa.Path) >= 2 {
					baseName := sa.Path[0]
					last := sa.Path[len(sa.Path)-1]

					if i := strings.Index(baseName, "["); i >= 0 {
						typeParam := strings.TrimSuffix(baseName[i+1:], "]")
						base := baseName[:i]
						concreteName := base + "__" + strings.ReplaceAll(typeParam, ",", "__")
						concreteKey := concreteName + "_" + last
						isHeapFn = cg.heapPromotingFns[concreteKey]
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
		isHeapAlloc := false

		if _, isStructLit := addrOf.Expr.(*ast.StructLit); isStructLit {
			isHeapAlloc = true
		} else if call, isCall := addrOf.Expr.(*ast.CallExpr); isCall {
			if id, ok := call.Func.(*ast.Identifier); ok && cg.isDataVariant(id.Name) {
				isHeapAlloc = true
			}
		}

		if isHeapAlloc && llType != nil {
			depth := pointerChainDepth(llType)
			if depth > 0 {
				isHeapOwned = true
				heapOwnedDepth = depth
			}
		}
	} else if _, isAwait := s.Value.(*ast.AwaitExpr); isAwait && llType != nil {
		// `let c = await expr` where expr returns a *NamedStruct
		// always transfers RC ownership to the caller -- the producer
		// (channel/atomic/whatever) retained a slot, the await
		// dequeues + clears the slot, and the awaiter is now the
		// owner. Without this isHeapOwned flag the binding would skip
		// scope-exit release_ptr and leak the dequeued value.
		if pt, isPtr := llType.(*irtypes.PointerType); isPtr {
			if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
				if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
					isHeapOwned = true
					heapOwnedDepth = pointerChainDepth(llType)
				}
			}
		}
	}

	isRC := isRCTrackedType(llType)

	// Static array initializers: fill the stack alloca directly.
	// Handles both [v; N] fill literals and [e0, e1, ...] literals when the
	// declared type is a fixed-size [T; N] array.
	if at, isStaticAt := llType.(*irtypes.ArrayType); isStaticAt && s.Value != nil {
		if fillLit, isFill := s.Value.(*ast.ArrayFillLit); isFill {
			fillVal, ferr := cg.genExpr(block, fillLit.Value)
			if ferr != nil {
				return nil, ferr
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			// Zero fill: use memset for efficiency.
			isZeroFill := false

			if ic, isConst := fillLit.Value.(*ast.IntLit); isConst && ic.Value == 0 {
				isZeroFill = true
			}

			if ic, isConst := fillLit.Value.(*ast.CharLit); isConst && ic.Value == '\000' {
				isZeroFill = true
			}

			if isZeroFill {
				elemBytes := llvmElemByteSize(at.ElemType)
				totalBytes := constant.NewInt(irtypes.I64, int64(at.Len)*elemBytes)
				dstPtr := block.NewBitCast(alloca, irtypes.I8Ptr)
				block.NewCall(cg.ensureMemset(), dstPtr,
					constant.NewInt(irtypes.I8, 0), totalBytes,
					constant.NewInt(irtypes.I1, 0))
			} else {
				fillCoerced := cg.coerce(block, fillVal, at.ElemType)
				for i := uint64(0); i < at.Len; i++ {
					gep := block.NewGetElementPtr(llType, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
					block.NewStore(fillCoerced, gep)
				}
			}
		} else if arrLit, isArr := s.Value.(*ast.ArrayLit); isArr {
			// Static array from element list: [e0, e1, ..., eN].
			for i, elem := range arrLit.Elems {
				v, verr := cg.genExpr(block, elem)
				if verr != nil {
					return nil, verr
				}

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}

				v = cg.coerce(block, v, at.ElemType)
				gep := block.NewGetElementPtr(llType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
				block.NewStore(v, gep)
			}

			// Zero-initialize any trailing elements beyond what was specified.
			if uint64(len(arrLit.Elems)) < at.Len {
				for i := uint64(len(arrLit.Elems)); i < at.Len; i++ {
					gep := block.NewGetElementPtr(llType, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
					block.NewStore(cg.zeroValue(at.ElemType), gep)
				}
			}
		}
		// Non-fill, non-ArrayLit static target: fall through to initVal path below.
	}

	// ownsIfaceData: trait-iface let-bindings always own a fresh
	// _tin_rc_alloc'd data ptr from coerceToTrait (both value-source and
	// pointer-source coerceToTrait branches heap-copy the source struct).
	// emitScopeRelease (runtime.go) uses the flag to emit the matching
	// _tin_release at scope exit so the iface storage is reclaimed.
	var ownsIfaceData bool

	if initVal != nil {
		// If the init value is an empty array literal `[]` (constant
		// `{null, 0}`) but the declared type is a typed fat array
		// `{T*, i64}`, use a properly-typed zero value.  Strings and
		// non-empty slices share the {i8*, i64} shape, so we discriminate
		// on the source value being a constant null-data struct -- the
		// LLVM artifact genArrayLit emits for the empty-literal form
		// only.  Real strings reach coerce/store-time checks unchanged
		// so the user gets a precise type-mismatch diagnostic.
		if !initVal.Type().Equal(llType) {
			if isFatArrayPtr(initVal.Type()) && isFatArrayPtr(llType) {
				if cs, ok := initVal.(*constant.Struct); ok && len(cs.Fields) == 2 {
					if _, isNull := cs.Fields[0].(*constant.Null); isNull {
						initVal = cg.zeroValue(llType)
					}
				}
			}
		}

		srcType := initVal.Type()

		ownsIfaceData = isTraitFatPtrShape(llType) && !isTraitFatPtrShape(srcType)

		// Suppress coerceToTrait's deferred scope-exit release when this
		// let-binding will own the iface and emit its own release via
		// the scope entry's ownsIfaceData flag (see emitScopeRelease).
		prevSuppress := cg.suppressIfaceScopeRelease
		if ownsIfaceData {
			cg.suppressIfaceScopeRelease = true
		}

		cg.coerceLastErr = nil
		initVal = cg.coerce(block, initVal, llType)
		cg.suppressIfaceScopeRelease = prevSuppress

		// If coerce stashed a richer diagnostic (e.g. trait
		// pointer-receiver-vs-value-source rejection), surface that
		// instead of the generic type-mismatch fall-through.
		if cg.coerceLastErr != nil {
			return nil, cg.nodeErr(s, "%v", cg.coerceLastErr)
		}
		// Coerce returns the value unchanged when no conversion path applies;
		// guard NewStore so a real type mismatch produces a clean diagnostic
		// instead of a Go panic from llir's incompatible-operand check.
		if !initVal.Type().Equal(llType) {
			return nil, cg.nodeErr(s,
				"cannot assign value of type %s to %q (declared type %s)",
				fmtArgType(initVal.Type()), s.Name, fmtArgType(llType))
		}

		block.NewStore(initVal, alloca)

		// ARC: retain when copying from an existing variable (identifier).
		// emitRetain handles RC-tracked values (fat arrays, strings, any) and
		// named structs with RC-tracked fields, and is a no-op for everything else.
		//
		// EXCEPTION: if coerce just boxed a non-any value into `any`, the new
		// box block is a fresh _tin_rc_alloc (rc=1) - it is already owned, so
		// an extra retain would over-count and cause a leak.
		//
		// EXCEPTION: a bound method (FieldAccess -> genBoundMethod) or capturing
		// lambda allocates a fresh env via _tin_rc_alloc (rc=1). Retaining would
		// over-count: the single scope-exit release_closure must be the only decrement.
		isFreshFatFn := isFatFnPtr(llType) && cg.lastLambdaHadCaptures

		boxedToAny := isAnyType(llType) && !isAnyType(srcType)
		// Trait coercion already minted a fresh _tin_rc_alloc'd data ptr
		// (rc=1) inside coerceToTrait; the let-binding owns it via
		// ownsIfaceData. An emitRetain here would over-count and leak.
		freshIface := ownsIfaceData
		if isCopyExpr(s.Value) && !boxedToAny && !isFreshBytesAlloc(initVal) && !isFreshFatFn && !freshIface {
			cg.emitRetain(block, initVal)
		}
	} else if s.Value == nil {
		// No initializer: zero-initialize.
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
	// else: s.Value != nil && initVal == nil means a static-array fill was handled
	// directly above (ArrayFillLit or ArrayLit targeting [T; N] alloca).

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
	// Also handles bound methods (FieldAccess -> genBoundMethod): they set
	// lastLambdaHadCaptures=true so we must not skip the scope-exit release.
	noReleaseClosureEnv := false

	if isFatFnPtr(llType) {
		_, isLambda := s.Value.(*ast.LambdaExpr)
		_, isBound := s.Value.(*ast.FieldAccess)

		if isLambda || isBound {
			noReleaseClosureEnv = !cg.lastLambdaHadCaptures
			cg.lastLambdaHadCaptures = false // consume
		}
	}

	// Determine the byte-array element kind: prefer the explicit declared type,
	// then fall back to the RHS AsExpr type (covers `let x = expr as [byte]`).
	bae := byteArrayElemType(s.Type)
	if bae == "" {
		if asExpr, ok := s.Value.(*ast.AsExpr); ok {
			bae = byteArrayElemType(asExpr.Type)
		}
	}

	// scalarTypeName covers 8-bit types ("char","byte","u8","i8") and 128-bit types
	// ("i128","u128","f128") for echo/interpolation dispatch.
	stn := scalar8BitTypeName(s.Type)
	if stn == "" {
		stn = scalar128BitTypeName(s.Type)
	}

	entry := &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, basePtr: sliceBase, isUnsigned: isUnsignedTinType(s.Type), byteArrayElem: bae, scalarTypeName: stn, isHeapOwned: isHeapOwned, heapOwnedDepth: heapOwnedDepth, noRelease: noReleaseClosureEnv, tinType: s.Type, ownsIfaceData: ownsIfaceData, isEarlyHeap: earlyHeap, ownsHeapIfaceData: cg.bindingOwnsHeapIfaceData(s), declaredConst: s.IsConst, declaredLet: !s.IsConst}

	// Capture the init expression for compile-time folding (codegen/fold.go).
	// Subsequent assignments to the same name clear constInitExpr in
	// genAssign / aug-assign so a mutated variable can never be folded.
	if isFoldableInitExpr(s.Value) {
		entry.constInitExpr = s.Value
	}

	// Record the literal length of array / string initializers so the
	// array-bounds checker can warn on out-of-range constant indices into
	// `let xs = [1, 2, 3]; xs[5]` and similar.
	switch v := s.Value.(type) {
	case *ast.ArrayLit:
		entry.staticArrayLen = int64(len(v.Elems))
	case *ast.ArrayFillLit:
		if v.Count >= 0 {
			entry.staticArrayLen = int64(v.Count)
		}
	case *ast.StringLit:
		entry.staticArrayLen = int64(len(v.Value))
	}

	entry.declPos = s.Pos()
	cg.curScope.set(s.Name, entry)
	cg.warnIfBuiltinShadow("let", s.Name, s.Pos())

	return block, nil
}

// isFoldableInitExpr returns true for AST shapes the constant folder in
// fold.go can handle. Used to decide whether to capture an init expr on
// the scope entry. Conservative: false is always safe (just disables
// folding for that binding).
func isFoldableInitExpr(n ast.Node) bool {
	switch e := n.(type) {
	case nil:
		return false
	case *ast.BoolLit, *ast.AtomLit, *ast.IntLit, *ast.TypeofExpr:
		return true
	case *ast.Identifier:
		return true
	case *ast.BinExpr:
		return isFoldableInitExpr(e.Left) && isFoldableInitExpr(e.Right)
	case *ast.UnaryExpr:
		return isFoldableInitExpr(e.Expr)
	}

	return false
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
	// Propagate "owning iface" up the call graph: if we're returning a
	// binding that we know carries an escape-promoted iface data block,
	// flag this function so callers' let-bindings inherit
	// ownsHeapIfaceData (see bindingOwnsHeapIfaceData).
	if cg.curFn != nil && s.Value != nil {
		if id, ok := s.Value.(*ast.Identifier); ok {
			if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.ownsHeapIfaceData {
				cg.fnReturnsOwningIface[cg.curFn.Name()] = true
			}
		}
	}

	// In a coroutine body, return is replaced by _tin_fiber_complete + final suspend.
	if cg.inCoroFn {
		return cg.genCoroReturn(block, s)
	}

	// Self-TCO: intercept `return name(args...)` and rewrite as a loop-back.
	if cg.tcoFuncName != "" && s.Value != nil {
		if ce, ok := s.Value.(*ast.CallExpr); ok {
			if ident, ok2 := ce.Func.(*ast.Identifier); ok2 && ident.Name == cg.tcoFuncName {
				return cg.emitTCOLoopBack(block, ce)
			}
		}
	}

	// Mutual TCO: `return g(args...)` where g is a different Tin function with a
	// compatible non-RC return type. Emit a musttail call so LLVM turns it into
	// a sibling call, preventing stack growth in mutually-recursive cycles.
	if cg.mutualTCOEligible && s.Value != nil &&
		cg.curFnDeferRetAlloca == nil && len(cg.pendingDeferFnI8s) == 0 {
		if ce, ok := s.Value.(*ast.CallExpr); ok {
			if ident, ok2 := ce.Func.(*ast.Identifier); ok2 {
				if callee, eligible := cg.resolveMutualTCOCallee(ident.Name); eligible {
					return cg.emitMutualTCO(block, ce, callee)
				}
			}
		}
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
		// Bare `return` in a non-void function: emit a Tin diagnostic
		// here instead of letting NewRet(nil) reach LLVM and surface as
		// a clang IR-level "value doesn't match function result type"
		// error from a temp .ll file.
		if cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
			return cg.nodeErr(s, "function returns %s but the return statement has no value",
				fmtArgType(cg.curFn.Sig.RetType))
		}

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
		// Set returnTypeHint so ADT bare-constructor calls like `return Ok(x)`
		// can resolve against the declared return type. Restore after.
		prevHint := cg.returnTypeHint

		if cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
			cg.returnTypeHint = cg.curFn.Sig.RetType
		}

		var err2 error

		val, err2 = cg.genExpr(block, s.Value)
		cg.returnTypeHint = prevHint

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
		if irtypes.IsVoid(retType) {
			if val != nil {
				return cg.nodeErr(s, "void function cannot return a value")
			}
		} else {
			// Bare `return` in a non-void function: catch here with a
			// targeted Tin diagnostic instead of letting LLVM emit
			// `ret void` and surface a clang IR-level error.
			if val == nil {
				return cg.nodeErr(s, "function returns %s but the return statement has no value", fmtArgType(retType))
			}

			val = cg.coerce(block, val, retType)
			if !val.Type().Equal(retType) {
				// Render in user-facing source syntax (Foo[i64], not
				// the LLVM-mangled %Foo__i64). fmtArgType handles every
				// common shape; fall back to prettyStructName via the
				// raw struct name only when fmtArgType yields nothing.
				gotName := fmtArgType(val.Type())
				if gotName == "" || gotName == "<nil>" {
					gotName = prettyStructName(cg.typeNameOf(val.Type()))
				}

				wantName := fmtArgType(retType)
				if wantName == "" || wantName == "<nil>" {
					wantName = prettyStructName(cg.typeNameOf(retType))
				}

				if astDecl, ok := cg.funcDecls[cg.curFn.Name()]; ok && astDecl.RetType != nil {
					wantName = astDecl.RetType.String()
				}

				return cg.nodeErr(s, "cannot return value of type %s as %s", gotName, wantName)
			}
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
	} else if isCopyExpr(s.Value) && !isFreshBytesAlloc(val) {
		// Returning a borrowed value (field access, index) whose RC lifetime is
		// tied to a local/parameter that will be released by emitAllScopeReleases.
		// Retain first so the caller gets one owned reference, then scope cleanup
		// decrements the RC back to a net-neutral result.
		// Exception: [T;N] as string calls _tin_bytes_from_buf which already
		// allocates with RC=1 - no extra retain needed.
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
		// TupleLit: hand the coroutine's *original* return type to
		// the tuple generator so trait-pointer fields keep their
		// real LLVM type instead of being silently widened to i64
		// during inference (the LLVM coro signature is i8*, but the
		// stored payload uses the user-declared shape).
		if tup, isTup := s.Value.(*ast.TupleLit); isTup && cg.curCoroRetType != nil {
			retVal, err = cg.genTupleLit(block, tup, cg.curCoroRetType)
		} else {
			retVal, err = cg.genExpr(block, s.Value)
		}

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
// Returns the continuation block where execution resumes after the panic check.
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

	doneBlk := cg.newBlock("yield.done")
	cg.emitPanicCheck(resumeBlk, doneBlk, "yield")

	// Track doneBlk so genYieldAutoAt suppresses the redundant auto-yield when
	// the loop backedge lands on this continuation block.
	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[doneBlk] = true
	}

	return doneBlk, nil
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

	var length value.Value

	// String fat-ptr {i8*, i64}: extract field 1.
	if isStringType(t) {
		length = cg.extractStringLen(block, val)
	} else if isFatArrayPtr(t) {
		// Dynamic array fat-ptr {T*, i64}: extract field 1.
		st := t.(*irtypes.StructType)
		alloca := block.NewAlloca(st)
		block.NewStore(val, alloca)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		length = block.NewLoad(irtypes.I64, gep)
	} else if at, ok := t.(*irtypes.ArrayType); ok {
		// Static array [N x T]: constant length (no RC).
		return constant.NewInt(irtypes.I64, int64(at.Len)), nil
	} else {
		return nil, fmt.Errorf("len() not supported for type %s", t)
	}

	// ARC: release the argument if it is a temporary RC allocation
	// (e.g. len(a |> filter(f)) where the filtered array is a fresh allocation).
	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(block, val)
	}

	return length, nil
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
//
// callPos is the source position of the macro CALL site - used to retag
// macro-body nodes so codegen-time pos lookups (sourcepos in particular)
// report the caller's location, not the macro definition line.
func (cg *CodeGen) expandMacro(block *ir.Block, macro *ast.MacroDecl, args []ast.Node, callPos ast.Pos) (value.Value, error) {
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

		retagMacroBody(resultNode, args, callPos)

		return cg.genExpr(block, resultNode)
	}
	// Simple expression macros: AST substitution (fast path).
	cg.progress("macro " + strings.TrimSuffix(macro.Name, "!"))

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
		retagMacroBody(node, args, callPos)

		return cg.genExpr(block, node)
	}

	retagMacroBody(expanded, args, callPos)

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
	case *ast.Block:
		// Copy the original to preserve the embedded `base` (position
		// info).  Building from scratch with a struct literal would
		// drop the span and any diagnostic later raised against the
		// substituted body would point at (0,0).
		out := *n
		out.Stmts = make([]ast.Node, len(n.Stmts))

		for i, s := range n.Stmts {
			out.Stmts[i] = substituteMacroNode(s, subst)
		}

		return &out
	case *ast.MatchStmt:
		out := *n
		out.Expr = substituteMacroNode(n.Expr, subst)
		out.Cases = make([]ast.MatchCase, len(n.Cases))

		for i, c := range n.Cases {
			var body *ast.Block
			if b, ok := substituteMacroNode(c.Body, subst).(*ast.Block); ok {
				body = b
			}

			out.Cases[i] = ast.MatchCase{
				Pattern: substituteMacroNode(c.Pattern, subst),
				Guard:   substituteMacroNode(c.Guard, subst),
				VarName: c.VarName,
				Body:    body,
			}
		}

		if n.Default != nil {
			if b, ok := substituteMacroNode(n.Default, subst).(*ast.Block); ok {
				out.Default = b
			}
		}

		return &out
	case *ast.IfStmt:
		out := *n
		out.Cond = substituteMacroNode(n.Cond, subst)

		if b, ok := substituteMacroNode(n.Then, subst).(*ast.Block); ok {
			out.Then = b
		}

		if n.Else != nil {
			if b, ok := substituteMacroNode(n.Else, subst).(*ast.Block); ok {
				out.Else = b
			}
		}

		out.ElseIfs = make([]ast.ElseIfClause, len(n.ElseIfs))

		for i, ei := range n.ElseIfs {
			var body *ast.Block
			if b, ok := substituteMacroNode(ei.Body, subst).(*ast.Block); ok {
				body = b
			}

			out.ElseIfs[i] = ast.ElseIfClause{
				Cond: substituteMacroNode(ei.Cond, subst),
				Body: body,
			}
		}

		return &out
	case *ast.VarDecl:
		out := *n
		out.Value = substituteMacroNode(n.Value, subst)

		return &out
	case *ast.AssignStmt:
		out := *n
		out.Target = substituteMacroNode(n.Target, subst)
		out.Value = substituteMacroNode(n.Value, subst)

		return &out
	}

	return node
}

// markOutParamVarsHeapOwned marks variables passed as &varName (address-of) to
// a call that has N*S (N>=2) write-back parameters. After such a call, varName
// may hold a heap-allocated borrow wrapper (depth-1 chain), so it must be
// released at scope exit. We mark isHeapOwned=true and heapOwnedDepth=N-1.
//
// This fixes the leak where a void-returning extern with **S out-params writes
// a new RC-allocated borrow wrapper into the caller's *S variable, but the scope
// release would skip it because the void function was never in heapPromotingFns.
func (cg *CodeGen) markOutParamVarsHeapOwned(call *ast.CallExpr) {
	for _, arg := range call.Args {
		addrOf, ok := arg.(*ast.AddressOfExpr)
		if !ok {
			continue
		}

		ident, ok2 := addrOf.Expr.(*ast.Identifier)
		if !ok2 {
			continue
		}

		entry, ok3 := cg.curScope.lookup(ident.Name)
		if !ok3 || entry.tinType == nil {
			continue
		}
		// Count pointer levels and find cLayoutStruct base.
		depth := 0
		cur := entry.tinType

		for {
			pt, ptOk := cur.(*ast.PointerType)
			if !ptOk {
				break
			}

			depth++

			if st, stOk := pt.Elem.(*ast.SimpleType); stOk {
				if cg.cLayoutStructs[st.Name] {
					// varName has type (depth)*S where S is cLayoutStruct.
					// After a write-back from a (depth+1)*S param, varName holds
					// a heap chain of depth levels.
					entry.isHeapOwned = true
					entry.heapOwnedDepth = depth
				}

				break
			}

			cur = pt.Elem
		}
	}
}

func (cg *CodeGen) genAssign(block *ir.Block, s *ast.AssignStmt) (*ir.Block, error) {
	// `_ = expr` is the explicit discard form: evaluate expr for its side
	// effects and throw away the result. Acts like an ExprStmt without
	// triggering the discarded-result warning.
	if id, ok := s.Target.(*ast.Identifier); ok && id.Name == "_" {
		if _, err := cg.genExpr(block, s.Value); err != nil {
			return block, err
		}

		return block, nil
	}
	// Detect `x = x` self-assign: same identifier on both sides.
	if tid, ok := s.Target.(*ast.Identifier); ok {
		if vid, ok2 := s.Value.(*ast.Identifier); ok2 && tid.Name == vid.Name {
			cg.warn(DiagSelfAssign, s.Pos(),
				"self-assignment %q has no effect", tid.Name)
		}
	}

	if err := cg.checkFieldWritable(s.Target); err != nil {
		return block, err
	}
	// Reject direct assignment to a `const` binding, top-level or block-
	// scope. Without this, `const X i64 = 10; X = 99` silently compiles
	// and writes to the read-only global -- LLVM's `constant` qualifier
	// then makes the store unreachable, so the original value persists
	// AND the user is silently lied to about whether the write happened.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if cg.topLevelConstNames[id.Name] {
			return block, cg.nodeErr(s, "cannot assign to top-level const %q (immutable storage)", id.Name)
		}

		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.declaredConst {
			return block, cg.nodeErr(s, "cannot assign to const %q; drop the const if you need to mutate", id.Name)
		}
	}
	// Mutating an identifier invalidates any captured constant init (used
	// by the if-condition folder). Clear it before emitting the store so
	// later folds don't see stale information.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}
	// User struct (or *Struct) target: dispatch to ::index_set trait
	// method when the receiver struct implements index_set[K, V], or
	// emit a SIMD insertelement when the receiver is a vector.  Both
	// branches need the rvalue of the receiver, so emit it once here
	// rather than once per branch -- emitting twice double-runs any
	// side effects in the receiver expression and silently swallowed
	// the genExpr error on the second go.  The third evaluation
	// (genLValue for the SIMD store-back) is unavoidable but only
	// fires for genuinely-addressable LHS expressions.
	if idxExpr, ok := s.Target.(*ast.IndexExpr); ok {
		recv, err2 := cg.genExpr(block, idxExpr.Expr)
		if err2 != nil {
			return block, err2
		}

		if recv != nil {
			if structName := cg.structNameForReceiver(recv.Type()); structName != "" {
				idx, err3 := cg.genExpr(block, idxExpr.Index)
				if err3 != nil {
					return block, err3
				}

				val, err4 := cg.genExpr(block, s.Value)
				if err4 != nil {
					return block, err4
				}

				if fn := cg.lookupOpMethod(structName, "index_set",
					[]irtypes.Type{idx.Type(), val.Type()}); fn != nil {
					_, dErr := cg.emitOpDispatch(block, fn, recv, []value.Value{idx, val})
					if dErr != nil {
						return block, dErr
					}

					return block, nil
				}

				return block, cg.nodeErr(s,
					"type %s has no `::index_set` impl for (key %s, value %s); declare `fn ::index_set(this %s, k %s, v %s)`",
					cg.tinTypeDisplay(recv.Type()), cg.tinTypeDisplay(idx.Type()), cg.tinTypeDisplay(val.Type()),
					cg.tinTypeDisplay(recv.Type()), cg.tinTypeDisplay(idx.Type()), cg.tinTypeDisplay(val.Type()))
			}

			if vecType, isVec := recv.Type().(*irtypes.VectorType); isVec {
				idxVal, err3 := cg.genExpr(block, idxExpr.Index)
				if err3 != nil {
					return block, err3
				}

				newElem, err4 := cg.genExpr(block, s.Value)
				if err4 != nil {
					return block, err4
				}

				newElem = cg.coerce(block, newElem, vecType.ElemType)
				idx32 := cg.coerce(block, idxVal, irtypes.I32)
				updated := block.NewInsertElement(recv, newElem, idx32)

				vecPtr, err5 := cg.genLValue(block, idxExpr.Expr)
				if err5 != nil {
					return block, err5
				}

				block.NewStore(updated, vecPtr)

				return block, nil
			}
		}
	}

	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return block, err
	}

	cg.curBlock = block

	// Plumb the target's element type so that callee-side generators
	// (notably empty-array literals `[]`) can pick the right shape up
	// front instead of leaving a `{i8*, i64}` for coerce to massage.
	ptrType := ptr.Type().(*irtypes.PointerType)

	val, err := cg.genArgWithTargetType(block, s.Value, ptrType.ElemType)
	if err != nil {
		return block, err
	}
	// If genExpr advanced the current block (e.g. await inside rhs), use
	// the continuation block for all subsequent emissions.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	srcType := val.Type()

	val = cg.coerce(block, val, ptrType.ElemType)
	if !val.Type().Equal(ptrType.ElemType) {
		return block, cg.nodeErr(s,
			"cannot assign value of type %s (declared type %s)",
			cg.tinTypeDisplay(srcType), cg.tinTypeDisplay(ptrType.ElemType))
	}
	// ARC: for RC-tracked types, retain new value (if copy) then release old.
	// Skip retain if coerce just boxed a non-any value to any: the new box is
	// a fresh _tin_rc_alloc (rc=1) and is already owned.
	// Weak field targets are non-owning: skip both retain and release.
	isWeakTarget := false

	if fa, ok2 := s.Target.(*ast.FieldAccess); ok2 {
		// Unwrap an explicit dereference: (*x).field -> look up x.
		innerExpr := fa.Expr
		if de, ok3 := innerExpr.(*ast.DerefExpr); ok3 {
			innerExpr = de.Expr
		}

		if ident, ok3 := innerExpr.(*ast.Identifier); ok3 {
			if se, ok4 := cg.curScope.lookup(ident.Name); ok4 {
				if pt, ok5 := se.val.Type().(*irtypes.PointerType); ok5 {
					parentName := cg.typeNameOf(pt.ElemType)
					// pt.ElemType is the variable's declared type (e.g. *Node).
					// If it is itself a pointer, unwrap one more level to reach the struct.
					if parentName == "" {
						if pt2, ok6 := pt.ElemType.(*irtypes.PointerType); ok6 {
							parentName = cg.typeNameOf(pt2.ElemType)
						}
					}

					if parentName != "" {
						isWeakTarget = cg.structWeakFields[parentName][fa.Field]
					}
				}
			}
		} else if innerFA, ok3 := innerExpr.(*ast.FieldAccess); ok3 {
			// Handle chained field access like (*this.head).prev:
			// innerExpr = FieldAccess{this, "head"} -> resolve this -> get head's type.
			baseIdent, ok4 := innerFA.Expr.(*ast.Identifier)
			if !ok4 {
				if de2, ok5 := innerFA.Expr.(*ast.DerefExpr); ok5 {
					baseIdent, ok4 = de2.Expr.(*ast.Identifier)
				}
			}

			if ok4 {
				if se, ok5 := cg.curScope.lookup(baseIdent.Name); ok5 {
					// se.val is *ParentStruct or **ParentStruct; unwrap to get ParentStruct name.
					baseType := se.val.Type()
					if pt, ok6 := baseType.(*irtypes.PointerType); ok6 {
						baseType = pt.ElemType
						if pt2, ok7 := baseType.(*irtypes.PointerType); ok7 {
							baseType = pt2.ElemType
						}
					}

					if baseSt, ok6 := baseType.(*irtypes.StructType); ok6 && baseSt.Name() != "" {
						// Now look up the type of innerFA.Field within baseSt.
						fieldIdx := cg.fieldIndex(baseSt.Name(), innerFA.Field)
						if fieldIdx >= 0 && fieldIdx < len(baseSt.Fields) {
							fieldType := baseSt.Fields[fieldIdx]
							// Unwrap pointer to get the struct pointed to by this field.
							if fpt, ok7 := fieldType.(*irtypes.PointerType); ok7 {
								if innerSt, ok8 := fpt.ElemType.(*irtypes.StructType); ok8 && innerSt.Name() != "" {
									isWeakTarget = cg.structWeakFields[innerSt.Name()][fa.Field]
								}
							}
						}
					}
				}
			}
		}
	}

	// Heap-owned pointer reassign: when the target is an Identifier whose
	// scope entry was marked isHeapOwned (e.g. `let head = make_chain(...)`
	// returning *Node), the binding owns the chain and reassigning must
	// release the prior chain before the new value overwrites it. Mirrors
	// what emitScopeRelease does at scope exit.
	//
	// Only applied to Identifier targets: FieldAccess writes are handled by
	// the isTinStructPtrElem branch below (with its own retain logic), and
	// pointer dereferences are raw stores by design.
	if id, isID := s.Target.(*ast.Identifier); isID {
		if entry, ok := cg.curScope.lookup(id.Name); ok && entry.isHeapOwned {
			oldVal := block.NewLoad(ptrType.ElemType, ptr)

			if entry.heapOwnedDepth > 1 {
				structName := cLayoutStructBaseName(entry.tinType)
				if structName != "" {
					relFn := cg.ensureHeapChainReleaseFn(structName, entry.heapOwnedDepth)
					block.NewCall(relFn, oldVal)
				} else {
					cg.emitHeapChainRelease(block, oldVal, entry.heapOwnedDepth)
				}
			} else {
				cg.emitHeapChainRelease(block, oldVal, entry.heapOwnedDepth)
			}
		}
	}

	// Check if the element is a pointer to a known Tin struct (ARC-managed
	// via &Struct{} allocation).  Only for struct FIELD assignments (e.g.
	// this.head = n), not for arbitrary pointer dereferences (*pp = target)
	// which are raw pointer stores, not ownership transfers.
	isTinStructPtrElem := false

	if _, isFieldTarget := s.Target.(*ast.FieldAccess); isFieldTarget {
		if ept, ok6 := ptrType.ElemType.(*irtypes.PointerType); ok6 {
			if innerSt, ok7 := ept.ElemType.(*irtypes.StructType); ok7 && innerSt.Name() != "" {
				_, isTinStructPtrElem = cg.structTypes[innerSt.Name()]
			}
		}
	}

	if (isRCTrackedType(ptrType.ElemType) || isTinStructPtrElem) && !isWeakTarget {
		boxedToAny := isAnyType(ptrType.ElemType) && !isAnyType(srcType)
		if isCopyExpr(s.Value) && !boxedToAny && !isFreshBytesAlloc(val) {
			if isTinStructPtrElem {
				// Direct _tin_retain for *TinStruct pointers (emitRetain doesn't
				// handle these to avoid retaining borrowed parameters).
				ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
				block.NewCall(cg.ensureRetain(), ptrI8)
			} else {
				cg.emitRetain(block, val)
			}
		}

		oldVal := block.NewLoad(ptrType.ElemType, ptr)
		cg.emitRelease(block, oldVal)
	} else if !isWeakTarget {
		// Struct values: release the previous value if it has any RC-tracked
		// fields (string, [T], any, fn, nested struct) or an explicit deinit.
		// Without this the old value's RC fields leak whenever a struct is
		// reassigned. Mirrors the gate used by emitScopeRelease.
		//
		// On the same gate, retain the NEW value's RC fields when it's a
		// borrowed copy of another binding (isCopyExpr): without the
		// retain, both the source binding and this slot release the same
		// underlying buffers when their scopes exit. Fresh callee-returned
		// structs (isFreshBytesAlloc) already carry an unbalanced retain
		// from the callee, so we move ownership instead of retaining.
		if cg.typeNameOf(ptrType.ElemType) != "" && cg.elemNeedsRelease(ptrType.ElemType) {
			if isCopyExpr(s.Value) && !isFreshBytesAlloc(val) {
				cg.emitRetain(block, val)
			}

			oldVal := block.NewLoad(ptrType.ElemType, ptr)
			cg.emitRelease(block, oldVal)
		}
	}

	block.NewStore(val, ptr)

	return block, nil
}

func (cg *CodeGen) genAugAssign(block *ir.Block, s *ast.AugAssignStmt) (*ir.Block, error) {
	if err := cg.checkFieldWritable(s.Target); err != nil {
		return block, err
	}
	// Reject compound-assign to a `const` binding (same reason as
	// genAssign -- the underlying global lives in read-only storage).
	if id, ok := s.Target.(*ast.Identifier); ok {
		if cg.topLevelConstNames[id.Name] {
			return block, cg.nodeErr(s, "cannot %s top-level const %q (immutable storage)", s.Op, id.Name)
		}

		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.declaredConst {
			return block, cg.nodeErr(s, "cannot %s const %q; drop the const if you need to mutate", s.Op, id.Name)
		}
	}
	// Mutating an identifier invalidates any captured constant init.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}

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

	// Operator overloading: `a OP= b` on a user struct desugars to
	// `a = a.OP(b)` via the corresponding op trait. Falls through to the
	// primitive switch when the LHS is not a struct.
	if isStructType(elemType) {
		if traitName := compoundAssignTraitName(s.Op); traitName != "" {
			structName := cg.typeNameOf(elemType)
			if fn := cg.lookupOpMethod(structName, traitName, []irtypes.Type{rhs.Type()}); fn != nil {
				res, derr := cg.emitOpDispatch(block, fn, current, []value.Value{rhs})
				if derr != nil {
					return block, derr
				}

				if res != nil {
					// Release the previous value before overwriting so any
					// RC-tracked fields (strings, fat arrays, ...) are not
					// leaked. Mirrors the regular assign path above.
					if cg.elemNeedsRelease(elemType) {
						cg.emitRelease(block, current)
					}

					block.NewStore(cg.coerce(block, res, elemType), ptr)
				}

				return block, nil
			}

			return block, cg.nodeErr(s, "compound assignment %q is not defined for operands of type %s and %s",
				s.Op, elemType, rhs.Type())
		}
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
			// Exception: [T;N] as string via _tin_bytes_from_buf is already RC=1.
			if isCopyExpr(s.Value) && !isFreshBytesAlloc(newElem) {
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
	if err := cg.checkFieldWritable(s.Expr); err != nil {
		return err
	}
	// Mutation invalidates any captured constant init.
	if id, ok := s.Expr.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}

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

// genFoldedIf emits only the live branch of an if/elif/else chain whose
// initial cond was folded to known. The folded path is generated as a
// straight-line block; the dead branches are dropped entirely.
//
// When the initial condition is true, only the `then` block is emitted
// (subsequent elifs and else are dead).
//
// When the initial condition is false, the chain is "rotated": we step
// through elifs in declaration order, folding each. The first elif whose
// cond folds to true becomes the live branch; if any elif's cond is
// non-foldable we abandon folding and let the regular genIf path handle
// the remainder (rebuilding a fresh IfStmt with the unresolved tail).
// If every cond folds to false, we emit only the else branch (or
// fall-through when there isn't one).
func (cg *CodeGen) genFoldedIf(block *ir.Block, s *ast.IfStmt, takeThen bool) (*ir.Block, bool, error) {
	if takeThen {
		return cg.genFoldedBranch(block, s.Then)
	}
	// Initial cond is false; consider elifs.
	for i, elif := range s.ElseIfs {
		v, ok := cg.foldedBoolCondition(elif.Cond)
		if !ok {
			// Re-fall to the runtime path with the remaining (possibly
			// shorter) chain so the parts we did fold are still elided.
			rest := &ast.IfStmt{
				Cond:    elif.Cond,
				Then:    elif.Body,
				ElseIfs: s.ElseIfs[i+1:],
				Else:    s.Else,
			}

			return cg.genIfRuntime(block, rest)
		}

		if v {
			return cg.genFoldedBranch(block, elif.Body)
		}
	}
	// All conds folded false; emit else if present.
	if s.Else != nil {
		return cg.genFoldedBranch(block, s.Else)
	}
	// No live branch and no else: control falls through.
	return block, false, nil
}

// genFoldedBranch emits a body block that takes over from `block`, opens
// a fresh scope, releases it on exit (when not already terminated), and
// returns the post-body state in the same shape as genIf would.
func (cg *CodeGen) genFoldedBranch(block *ir.Block, body *ast.Block) (*ir.Block, bool, error) {
	cg.curScope = newScope(cg.curScope)

	cur, term, err := cg.genBlock(block, body)
	if cur == nil {
		cur = block
	}

	cg.emitScopeRelease(cur, cg.curScope)
	cg.curScope = cg.curScope.parent

	if err != nil {
		return nil, false, err
	}

	return cur, term || cur.Term != nil, nil
}

// genIfRuntime is the original genIf, used as a fallback by genFoldedIf
// when only a prefix of the chain folded.
func (cg *CodeGen) genIfRuntime(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	return cg.genIf(block, s)
}

func (cg *CodeGen) genIf(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	// Simplify the condition (De Morgan, double-negation, comparison
	// inversion, bool-literal absorption) and emit an "always true/false"
	// warning when the simplified form folds to a constant.
	s.Cond = cg.prepareBoolCond(s.Cond, "if", false)
	for i := range s.ElseIfs {
		s.ElseIfs[i].Cond = cg.prepareBoolCond(s.ElseIfs[i].Cond, "elif", false)
	}
	// Try to constant-fold the condition. When it folds we elide the
	// dead branch entirely so the strict per-arg type check at call sites
	// doesn't trip on type-incorrect code that would never execute (the
	// canonical case is `if typeof(v) == 'string: ... v as string` inside
	// a monomorphized generic where T isn't string). See codegen/fold.go.
	if v, ok := cg.foldedBoolCondition(s.Cond); ok {
		return cg.genFoldedIf(block, s, v)
	}

	mergeBlock := cg.newBlock("if.merge")

	// Reset cg.curBlock to the entry block before evaluating the condition.
	// Stale curBlock values left by prior statements (e.g. genBinExpr sets it
	// to the condition-check block of the PREVIOUS if) cause the arg-loop in
	// genCallExpr to emit the call into a non-dominating block while the arg
	// loads already went into the correct block, violating SSA dominance.
	// This mirrors the identical reset done for elif chains (see below).
	cg.curBlock = block

	cond, err := cg.genExpr(block, s.Cond)
	if err != nil {
		return nil, false, err
	}

	// genExpr may have advanced cg.curBlock through short-circuit && / ||
	// (genLogicalAnd/Or update curBlock to their merge block). Continue
	// emitting the cond-branch in that block, not the original.
	condEnd := block
	if cg.curBlock != nil {
		condEnd = cg.curBlock
	}

	cond = cg.toBoolImplicit(condEnd, cond)

	thenBlock := cg.newBlock("if.then")

	var elseStart *ir.Block
	if s.Else != nil || len(s.ElseIfs) > 0 {
		elseStart = cg.newBlock("if.else")
	} else {
		elseStart = mergeBlock
	}

	condEnd.NewCondBr(cond, thenBlock, elseStart)

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

		// Reset curBlock to the condition-check block before generating the
		// condition expression.  genBlock for the previous elif body may have
		// left curBlock pointing to a block inside that body, which would cause
		// genExpr to emit loads in the wrong (non-dominating) block.
		cg.curBlock = currentElse

		elifCond, err := cg.genExpr(currentElse, elif.Cond)
		if err != nil {
			return nil, false, err
		}

		// Pick up curBlock in case the elif's cond was short-circuited.
		elifCondEnd := currentElse
		if cg.curBlock != nil {
			elifCondEnd = cg.curBlock
		}

		elifCond = cg.toBoolImplicit(elifCondEnd, elifCond)
		elifThen := cg.newBlock("elif.then")
		elifCondEnd.NewCondBr(elifCond, elifThen, nextBlock)

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
