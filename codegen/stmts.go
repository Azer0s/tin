package codegen

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Body generation

// genBody generates a function body from a node (Block, WhereList, or expression).
// Returns whether the block was terminated.
func (cg *CodeGen) genBody(block *ir.Block, body ast.Node, retType irtypes.Type) (bool, error) {
	addDefaultRet := func(b *ir.Block) {
		if b != nil && b.Term == nil {
			_ = cg.emitDefers(b)
			cg.emitAllScopeReleases(b, "")
			if irtypes.IsVoid(retType) {
				b.NewRet(nil)
			} else {
				b.NewRet(cg.zeroValue(retType))
			}
		}
	}
	switch b := body.(type) {
	case *ast.Block:
		newBlock, term, err := cg.genBlock(block, b)
		if err != nil {
			return false, err
		}
		if !term {
			addDefaultRet(newBlock)
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
				_ = cg.emitDefers(block)
				retSkip := ""
				if ident, ok := inner.(*ast.Identifier); ok {
					retSkip = ident.Name
				}
				cg.emitAllScopeReleases(block, retSkip)
				block.NewRet(val)
			} else {
				addDefaultRet(block)
			}
			return true, nil
		}
		// Void: generate as statement.
		newBlock, terminated, err := cg.genStmt(block, b)
		if err != nil {
			return false, err
		}
		if !terminated {
			addDefaultRet(newBlock)
		}
		return true, nil
	case *ast.ReturnStmt, *ast.EchoStmt, *ast.AssignStmt, *ast.PostfixStmt,
		*ast.VarDecl, *ast.IfStmt, *ast.ForStmt, *ast.MatchStmt, *ast.DeferStmt:
		// Single statement body (e.g. fn foo() T = return expr)
		newBlock, terminated, err := cg.genStmt(block, body)
		if err != nil {
			return false, err
		}
		if !terminated {
			addDefaultRet(newBlock)
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
			_ = cg.emitDefers(block)
			retSkip := ""
			if ident, ok := body.(*ast.Identifier); ok {
				retSkip = ident.Name
			}
			cg.emitAllScopeReleases(block, retSkip)
			block.NewRet(val)
		} else {
			_ = cg.emitDefers(block)
			cg.emitAllScopeReleases(block, "")
			block.NewRet(nil)
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
		*ast.BreakStmt, *ast.FuncDecl, *ast.TaggedBlock:
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

	case *ast.ReturnStmt:
		if err := cg.genReturn(block, s); err != nil {
			return nil, false, err
		}
		return nil, true, nil

	case *ast.BreakStmt:
		// We handle break by returning nil; the loop handler deals with it.
		return nil, true, nil

	case *ast.EchoStmt:
		var err error
		block, err = cg.genEcho(block, s)
		return block, false, err

	case *ast.ExprStmt:
		_, err := cg.genExpr(block, s.Expr)
		return block, false, err

	case *ast.AssignStmt:
		err := cg.genAssign(block, s)
		return block, false, err

	case *ast.AugAssignStmt:
		err := cg.genAugAssign(block, s)
		return block, false, err

	case *ast.PostfixStmt:
		err := cg.genPostfix(block, s)
		return block, false, err

	case *ast.IfStmt:
		newBlock, term, err := cg.genIf(block, s)
		return newBlock, term, err

	case *ast.ForStmt:
		newBlock, err := cg.genFor(block, s)
		return newBlock, false, err

	case *ast.MatchStmt:
		newBlock, err := cg.genMatch(block, s)
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
		block.NewCall(cg.deferPushFn, entryI8, fnI8, envI8)
		cg.pendingDeferFrames = append(cg.pendingDeferFrames, entryI8)
		// 3. Also record the original call for inline LIFO emission on normal return.
		cg.pendingDefers = append(cg.pendingDefers, s.Call)
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
	var llType irtypes.Type
	var err error

	if s.Type != nil {
		llType, err = cg.tinTypeToLLVM(s.Type)
		if err != nil {
			return nil, err
		}
	}

	var initVal value.Value
	if s.Value != nil {
		// Special-case: `let m maybe[T] = None` -> zero-tagged struct.
		if _, isNone := s.Value.(*ast.NoneLit); isNone && llType != nil {
			if noneVal := cg.makeNoneValue(block, llType); noneVal != nil {
				alloca := block.NewAlloca(llType)
				block.NewStore(noneVal, alloca)
				cg.curScope.set(s.Name, &scopeEntry{val: alloca, isAlloc: true})
				return block, nil
			}
		}
		initVal, err = cg.genExpr(block, s.Value)
		if err != nil {
			return nil, err
		}
		if llType == nil {
			llType = initVal.Type()
		}
	}

	if llType == nil {
		llType = irtypes.I64
	}

	alloca := block.NewAlloca(llType)
	isRC := isRCTrackedType(llType)
	if initVal != nil {
		// If the init value is an empty array {i8*, i64} but the declared type
		// is a typed fat array {T*, i64}, use a properly-typed zero value.
		if !initVal.Type().Equal(llType) {
			if isFatArrayPtr(initVal.Type()) && isFatArrayPtr(llType) {
				initVal = cg.zeroValue(llType)
			}
		}
		initVal = cg.coerce(block, initVal, llType)
		block.NewStore(initVal, alloca)
		// ARC: retain when copying from an existing variable (identifier).
		// emitRetain handles RC-tracked values (fat arrays, strings, any) and
		// named structs with RC-tracked fields, and is a no-op for everything else.
		if isCopyExpr(s.Value) {
			cg.emitRetain(block, initVal)
		}
	} else {
		// Zero-initialize.
		block.NewStore(cg.zeroValue(llType), alloca)
	}
	cg.curScope.set(s.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC})
	return block, nil
}

// emitDefers emits all pending deferred calls in LIFO order into block.
// For each defer, it pops that single entry from the runtime chain before
// executing it inline.  This ensures that if a deferred call itself panics,
// the remaining (not-yet-run) defers are still in the chain and will be
// executed by _tin_panic.
func (cg *CodeGen) emitDefers(block *ir.Block) error {
	n := len(cg.pendingDefers)
	if n == 0 {
		return nil
	}
	for i := n - 1; i >= 0; i-- {
		// Deregister this one entry before running it.
		if cg.deferPopFn != nil {
			block.NewCall(cg.deferPopFn, constant.NewInt(irtypes.I64, 1))
		}
		if _, err := cg.genExpr(block, cg.pendingDefers[i]); err != nil {
			return err
		}
	}
	cg.pendingDeferFrames = nil
	cg.pendingDefers = nil
	return nil
}

func (cg *CodeGen) genReturn(block *ir.Block, s *ast.ReturnStmt) error {
	if s.Value == nil {
		if err := cg.emitDefers(block); err != nil {
			return err
		}
		cg.emitAllScopeReleases(block, "")
		block.NewRet(nil)
		return nil
	}
	if cg.curFn != nil {
		retType := cg.curFn.Sig.RetType
		// Special-case: `return None` for a data-type function -> zero-tagged struct.
		if _, isNone := s.Value.(*ast.NoneLit); isNone {
			if noneVal := cg.makeNoneValue(block, retType); noneVal != nil {
				cg.emitAllScopeReleases(block, "")
				block.NewRet(noneVal)
				return nil
			}
		}
	}
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
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
	// ARC: release all RC locals except the one being returned
	// (to transfer its rc=1 ownership to the caller).
	retSkipName := ""
	if ident, ok := s.Value.(*ast.Identifier); ok {
		retSkipName = ident.Name
	}
	cg.emitAllScopeReleases(block, retSkipName)
	block.NewRet(val)
	return nil
}

// makeNoneValue builds a None data-union struct value for the given target
// type. Returns nil if the target is not a data type.
// Layout: { i32 type_id, i8 variant_tag=0, [n x i8] payload=zeros }
func (cg *CodeGen) makeNoneValue(block *ir.Block, target irtypes.Type) value.Value {
	st, ok := target.(*irtypes.StructType)
	if !ok {
		return nil
	}
	name := cg.typeNameOf(target)
	if _, isData := cg.dataDecls[name]; !isData {
		return nil
	}
	alloca := block.NewAlloca(st)
	block.NewStore(cg.zeroValue(st), alloca)
	// Set the type_id field (field 0) to the data type's compile-time ID.
	if typeID, ok := cg.dataTypeIDs[name]; ok {
		typeIDGEP := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGEP)
	}
	// Variant tag (field 1) stays 0 = None.
	return block.NewLoad(st, alloca)
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
// initialises the TinDeferEntry LLVM struct type.
func (cg *CodeGen) ensureDeferChain() {
	if cg.deferPushFn != nil {
		return
	}
	// { i8* prev, i8* fn, i8* env }  mirrors TinDeferEntry in runtime.c
	cg.deferEntryType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr)
	cg.deferPushFn = cg.mod.NewFunc("_tin_defer_push", irtypes.Void,
		ir.NewParam("entry", irtypes.I8Ptr),
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("env", irtypes.I8Ptr),
	)
	cg.deferPopFn = cg.mod.NewFunc("_tin_defer_pop", irtypes.Void,
		ir.NewParam("n", irtypes.I64),
	)
}

// genDeferThunk generates a zero-param thunk function that, when called,
// executes the deferred call expression.  Free variables referenced by the
// call are captured by value into a heap-allocated env struct (same mechanics
// as genLambdaExpr).  Returns (fn as i8*, env as i8*).
func (cg *CodeGen) genDeferThunk(block *ir.Block, call ast.Node) (value.Value, value.Value, error) {
	name := fmt.Sprintf("defer.thunk.%d", cg.strCount)
	cg.strCount++

	// Step 1: collect free variables
	freeNames := collectFreeVars(call, map[string]bool{})

	type capture struct {
		name   string
		val    value.Value
		llvmTy irtypes.Type
	}
	var captures []capture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function — reachable by name, no capture needed
		}
		var val value.Value
		var ty irtypes.Type
		if entry.isAlloc {
			pt := entry.val.Type().(*irtypes.PointerType)
			ty = pt.ElemType
			val = block.NewLoad(ty, entry.val)
		} else {
			val = entry.val
			ty = val.Type()
		}
		captures = append(captures, capture{n, val, ty})
	}

	// Step 2: build env struct and heap-allocate it
	var envI8 value.Value = constant.NewNull(irtypes.I8Ptr)
	var envStructType *irtypes.StructType

	if len(captures) > 0 {
		fields := make([]irtypes.Type, len(captures))
		for i, c := range captures {
			fields[i] = c.llvmTy
		}
		envStructType = irtypes.NewStruct(fields...)

		nullEnvPtr := constant.NewNull(irtypes.NewPointer(envStructType))
		oneGEP := block.NewGetElementPtr(envStructType, nullEnvPtr, constant.NewInt(irtypes.I32, 1))
		envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
		envI8 = block.NewCall(cg.ensureMalloc(), envSize)

		envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := block.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			block.NewStore(c.val, gep)
		}
	}

	// Step 3: create the thunk IR function void(i8* env)
	f := cg.mod.NewFunc(name, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))
	entryBlock := f.NewBlock("entry")

	// Save and reset context so the thunk body doesn't inherit the caller's
	// pending defers or scope.
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevDefers := cg.pendingDefers
	prevDeferFrames := cg.pendingDeferFrames

	cg.curFn = f
	cg.pendingDefers = nil
	cg.pendingDeferFrames = nil

	// Root the scope at the global level so top-level functions remain
	// reachable, but local variables from the outer scope are NOT visible
	// (they are accessed exclusively through the env struct below).
	global := prevScope
	for global.parent != nil {
		global = global.parent
	}
	cg.curScope = newScope(global)

	// Step 4: unpack captures from env
	if len(captures) > 0 {
		envRaw := f.Params[0]
		envTypedPtr := entryBlock.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := entryBlock.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			alloca := entryBlock.NewAlloca(c.llvmTy)
			loaded := entryBlock.NewLoad(c.llvmTy, gep)
			entryBlock.NewStore(loaded, alloca)
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true})
		}
	}

	// Step 5: emit the deferred call
	if _, err := cg.genExpr(entryBlock, call); err != nil {
		return nil, nil, err
	}
	entryBlock.NewRet(nil)

	// Restore context.
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.pendingDefers = prevDefers
	cg.pendingDeferFrames = prevDeferFrames

	// Return fn as i8* and env as i8*.
	fnI8 := block.NewBitCast(f, irtypes.I8Ptr)
	return fnI8, envI8, nil
}

// panic builtin

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable

// expandMacro evaluates a macro call, choosing the appropriate strategy:
//   - Complex macros (block body): CTFE — compile to a temp binary, run with timeout,
//     parse stdout as the expansion result.
//   - Simple macros (expression body): AST substitution — fast, no subprocess.
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
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}
	expanded := substituteMacroNode(body, subst)
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
			Func:    substituteMacroNode(n.Func, subst),
			Args:    newArgs,
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
	case *ast.ExprStmt:
		return &ast.ExprStmt{Expr: substituteMacroNode(n.Expr, subst)}
	}
	return node
}

func (cg *CodeGen) genEcho(block *ir.Block, s *ast.EchoStmt) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
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
			// char/u8: print as character
			fmtStr = cg.newGlobalString("%c\n")
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, fmtStr, zext)
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
		// Fallback: print as integer.
		fmtStr := cg.newGlobalString("%lld\n")
		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, fmtStr, ext)
	}

	// ARC: release fresh RC-tracked values produced by function calls or
	// concatenation that are not stored in a named variable (temporaries).
	// Named variables are released by their scope entry at scope exit.
	if isRCTrackedType(t) && isTemporaryProducer(s.Value) {
		cg.emitRelease(block, val)
	}

	return block, nil
}

func (cg *CodeGen) genAssign(block *ir.Block, s *ast.AssignStmt) error {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return err
	}
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
	}
	// Get the element type of the pointer.
	ptrType := ptr.Type().(*irtypes.PointerType)
	val = cg.coerce(block, val, ptrType.ElemType)
	// ARC: for RC-tracked types, retain new value (if copy) then release old.
	if isRCTrackedType(ptrType.ElemType) {
		if isCopyExpr(s.Value) {
			cg.emitRetain(block, val)
		}
		oldVal := block.NewLoad(ptrType.ElemType, ptr)
		cg.emitRelease(block, oldVal)
	}
	block.NewStore(val, ptr)
	return nil
}

func (cg *CodeGen) genAugAssign(block *ir.Block, s *ast.AugAssignStmt) error {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return err
	}
	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	rhs, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
	}
	rhs = cg.coerce(block, rhs, elemType)

	var result value.Value
	switch s.Op {
	case "+=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			result = block.NewGetElementPtr(pt.ElemType, current, rhs)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFAdd(current, rhs)
		} else {
			result = block.NewAdd(current, rhs)
		}
	case "-=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			neg := block.NewSub(constant.NewInt(irtypes.I64, 0), rhs)
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
			newElemGep := block.NewGetElementPtr(elemT, newPtr, oldLen)
			newElem := cg.coerce(block, rhs, elemT)
			block.NewStore(newElem, newElemGep)

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
	return nil
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

	return mergeBlock, false, nil
}


func (cg *CodeGen) genFor(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	f := cg.curFn

	switch s.Kind {
	case ast.ForCStyle:
		return cg.genForCStyle(block, s, f)
	case ast.ForIn:
		return cg.genForIn(block, s, f)
	}
	return block, nil
}

func (cg *CodeGen) genForCStyle(block *ir.Block, s *ast.ForStmt, f *ir.Func) (*ir.Block, error) {
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
	}
	if block.Term == nil {
		block.NewBr(condBlock)
	}

	// Cond
	if s.Cond != nil {
		cond, err := cg.genExpr(condBlock, s.Cond)
		if err != nil {
			return nil, err
		}
		cond = cg.toBool(condBlock, cond)
		condBlock.NewCondBr(cond, bodyBlock, afterBlock)
	} else {
		condBlock.NewBr(bodyBlock)
	}

	// Body
	cg.curScope = newScope(cg.curScope)
	var err error
	bodyBlock, _, err = cg.genStmt(bodyBlock, s.Body)
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
		postBlock.NewBr(condBlock)
	}

	// ARC: release init scope vars (e.g. loop counter) in the after block.
	cg.emitScopeRelease(afterBlock, cg.curScope)
	cg.curScope = cg.curScope.parent // pop loop scope

	return afterBlock, nil
}

func (cg *CodeGen) genForIn(block *ir.Block, s *ast.ForStmt, f *ir.Func) (*ir.Block, error) {
	// Check if iter is a RangeExpr or a BinExpr with op ".." (start..end).
	if rng, ok := s.Iter.(*ast.RangeExpr); ok {
		return cg.genForRange(block, s, rng, f)
	}
	if bin, ok := s.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
		return cg.genForRange(block, s, &ast.RangeExpr{Start: bin.Left, End: bin.Right}, f)
	}

	// Iterate over a dynamic array: {ptr*, len}.
	iterVal, err := cg.genExpr(block, s.Iter)
	if err != nil {
		return nil, err
	}

	// iter[t] trait: struct (or fat-ptr) implementing iter[T] — use vtable.
	if iterFatPtr, instKey, ok := cg.tryCoerceToIter(block, iterVal); ok {
		return cg.genForIterTrait(block, s, iterFatPtr, instKey)
	}

	// Get element type.
	var elemType irtypes.Type = irtypes.I64
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
	// ARC: each iteration copies an element — retain to claim ownership.
	if isElemRC {
		cg.emitRetain(bodyBlock, elemVal)
	}
	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true, isRC: isElemRC})
	}

	var bodyErr error
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
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
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

func (cg *CodeGen) genForRange(block *ir.Block, s *ast.ForStmt, rng *ast.RangeExpr, f *ir.Func) (*ir.Block, error) {
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
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
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
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

func (cg *CodeGen) genMatch(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	if s.IsType {
		return cg.genMatchType(block, s)
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
	var cases []*ir.Case
	var caseBlocks []*ir.Block
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

	// Build switch.
	switchExpr := cg.coerce(block, expr, irtypes.I64)
	block.NewSwitch(switchExpr, defaultBlock, cases...)

	// Generate case bodies.
	for i, c := range s.Cases {
		var caseBlock *ir.Block
		if i < len(caseBlocks) {
			caseBlock = caseBlocks[i]
		} else {
			caseBlock = cg.newBlock(fmt.Sprintf("match.case.%d", i))
		}
		cg.curScope = newScope(cg.curScope)
		caseBlock, _, err = cg.genStmt(caseBlock, c.Body)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, err
		}
		if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)
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
		}
	}

	// If afterBlock was never jumped to (all arms terminated), mark unreachable.
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

	// Build cases: determine tag for each case from VarType.
	var cases []*ir.Case
	var caseBlocks []*ir.Block
	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)
		tag := int64(0)
		if c.VarType != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(c.VarType)
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
	for i, c := range s.Cases {
		caseBlock := caseBlocks[i]
		cg.curScope = newScope(cg.curScope)
		// Bind payload variable if specified.
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
		}
		caseBlock, _, err = cg.genStmt(caseBlock, c.Body)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, err
		}
		if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)
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
		}
	}

	return afterBlock, nil
}
