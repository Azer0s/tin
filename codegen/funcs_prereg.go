package codegen

import (
	"math/big"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) preregister(node ast.Node) error {
	switch n := node.(type) {
	case *ast.StructDecl:
		// Tag-driven side maps must populate during preregister (pass 1)
		// so later passes can consult them. genStructLayout (pass 3 phase
		// A) used to be the only writer, but predeclareFuncAs (pass 2)
		// also needs to see #no_copy to reject by-value parameters and
		// returns -- previously those slipped through and only let-binding
		// rejection caught them. Mirror the writes here for both bare and
		// pkg-qualified keys so cross-package lookups work.
		if hasTag(n.Tags, "no_copy") {
			cg.noCopyStructs[n.Name] = true
			cg.noCopyStructs[cg.pkgStructKey(n.Name)] = true
		}

		if hasTag(n.Tags, "closed") {
			cg.closedStructs[n.Name] = true
			cg.closedStructs[cg.pkgStructKey(n.Name)] = true
		}

		if len(n.TypeParams) > 0 {
			// Generic struct - store as template keyed by arity; concrete types
			// are created when a "type X = GenericStruct[T, R]" alias is processed.
			// Templates are always keyed by bare name so that tinTypeToLLVM can look
			// them up using the stripped (bare) name component.
			if cg.genericStructsByArity[n.Name] == nil {
				cg.genericStructsByArity[n.Name] = make(map[int]*ast.StructDecl)
			}

			cg.genericStructsByArity[n.Name][len(n.TypeParams)] = n

			// Tag the template's source file so monomorphizations can
			// inherit it for `//!-Wno-` lookup. Without this, every
			// `Channel[T]` instantiated in user code would lose the
			// suppression that lives in stdlib/sync/channel.tin.
			if cg.filename != "" {
				cg.genericStructTmplFiles[n.Name] = cg.filename
				if cg.currentPkg != "" {
					cg.genericStructTmplFiles[cg.currentPkg+"__"+n.Name] = cg.filename
				}
			}
		} else {
			// Register an opaque struct so recursive types work.
			// Use the canonical package-prefixed name as both the map key and the
			// LLVM IR struct name so that structs from different packages never
			// collide (e.g. sync__Unit, io__Reader).
			structKey := cg.pkgStructKey(n.Name)
			st := irtypes.NewStruct()
			st.SetName(structKey)
			cg.recordLLVM(CanonKey(structKey), st)
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			// Register a bare-name alias so that code referencing the short form
			// (e.g. "Parser" inside yaml, "Unit" inside sync) resolves to the
			// canonical name.  Always overwrite so the currently-compiling package's
			// definition takes precedence over a same-named type from an earlier
			// package loaded in the same scope.
			if cg.currentPkg != "" {
				cg.recordAliasType(CanonKey(n.Name), &ast.SimpleType{Name: structKey})
				cg.recordAlias(CanonKey(structKey), n.Name)
			}
		}

		cg.curScope.markTypeVisible(n.Name)
	case *ast.EnumDecl:
		// Register enum values early so they are available during on-demand
		// struct monomorphization triggered from pass 2 (predeclare).
		if err := cg.genEnumDecl(n); err != nil {
			return err
		}

		cg.curScope.markTypeVisible(n.Name)
	case *ast.UnionDecl:
		// Register an opaque struct so forward references work.
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.recordLLVM(CanonKey(n.Name), st)
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	case *ast.DataDecl:
		// Register an opaque struct for non-generic ADTs so forward references
		// in function signatures resolve. Layout is filled in by genDataDecl
		// during pre-pass 3. Generic ADTs are monomorphized on demand; their
		// variant names are registered against the concrete instance at
		// monomorphization time (see monomorphizeDataDecl).
		if len(n.TypeParams) == 0 {
			st := irtypes.NewStruct()
			st.SetName(n.Name)
			cg.recordLLVM(CanonKey(n.Name), st)
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)

			for _, v := range n.Variants {
				cg.dataVariantLookup[v.Name] = appendUnique(cg.dataVariantLookup[v.Name], n.Name)
			}
		}

		cg.recordData(CanonKey(n.Name), n)
		cg.curScope.markTypeVisible(n.Name)
	case *ast.TypeDecl:
		// Simple type aliases (type char = u8) go straight into typeAliases.
		// Tagged union aliases (type u = i8 | string) get a placeholder struct so
		// forward references work; full layout is filled in genTypeDecl.
		// Struct-monomorphization aliases (type point = tuple[f32]) are handled
		// in genTypeDecl so that all struct templates are known first.
		if ut, isUnion := n.Type.(*ast.UnionTypeExpr); isUnion {
			if cg.structTypeFor(CanonKey(n.Name)) == nil {
				st := irtypes.NewStruct()
				st.SetName(n.Name)
				cg.recordLLVM(CanonKey(n.Name), st)
				cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			}
			// Populate unionTypeMembers here so that downstream constraint
			// checks (`where t is X` against a tagged-union alias) work
			// even when the TypeDecl was declared in an imported package
			// -- packages.go doesn't run pass-2's genTypeDecl, so without
			// this preregister write the membership is invisible across
			// package boundaries and method-level where guards silently
			// dead-strip every method.
			if _, already := cg.unionTypeMembers[n.Name]; !already {
				cg.unionTypeMembers[n.Name] = ut.Types
			}
		} else if _, isGeneric := n.Type.(*ast.GenericType); !isGeneric {
			cg.recordAliasType(CanonKey(n.Name), n.Type)
		}

		cg.curScope.markTypeVisible(n.Name)
	case *ast.TraitDecl:
		if err := cg.checkTraitDefaultMutationForms(n); err != nil {
			return err
		}

		cg.recordTrait(CanonKey(n.Name), n)

		if cg.currentPkg != "" {
			qualInstKey := cg.currentPkg + "__" + n.Name
			cg.traitBareToQualInstKey[n.Name] = qualInstKey

			displayPkg := cg.currentPkgPath
			if displayPkg == "" {
				displayPkg = cg.currentPkg
			}
			// Register the qualified form so callers asking for the
			// trait by `pkg__Trait` canonical key get the source-form
			// `pkg::Trait` display back without any reconstruction.
			cg.recordDisplay(CanonKey(qualInstKey), displayPkg+"::"+n.Name)
			cg.recordTrait(CanonKey(qualInstKey), n)
		}

		cg.curScope.markTypeVisible(n.Name)
	case *ast.MacroDecl:
		cg.macros[n.Name] = n
	case *ast.VarDecl:
		if !n.IsConst || n.Value == nil {
			break
		}
		// Preregister top-level constants so test-block scopes can see them.
		// Only simple literal values are evaluated here; complex expressions
		// are left to the normal genVarDecl pass.
		var cv value.Value

		switch lit := n.Value.(type) {
		case *ast.IntLit:
			if lit.Big != nil {
				cv = &constant.Int{Typ: irtypes.I128, X: new(big.Int).Set(lit.Big)}
			} else {
				cv = constant.NewInt(irtypes.I64, lit.Value)
			}
		case *ast.FloatLit:
			cv = constant.NewFloat(irtypes.Double, lit.Value)
		case *ast.BoolLit:
			if lit.Value {
				cv = constant.NewInt(irtypes.I1, 1)
			} else {
				cv = constant.NewInt(irtypes.I1, 0)
			}
		case *ast.StringLit:
			raw := cg.newGlobalString(lit.Value).(constant.Constant)
			strType := stringFatPtrType()
			lenVal := constant.NewInt(irtypes.I64, int64(len(lit.Value)))
			cv = constant.NewStruct(strType, raw, lenVal)
		case *ast.AtomLit:
			cv = cg.atomConstant(cg.registerAtom(lit.Name))
		}

		if cv != nil {
			if n.Type != nil {
				if lt, err := cg.tinTypeToLLVM(n.Type); err == nil {
					cv = cg.constCoerce(cv, lt)
				}
			}

			cg.curScope.set(n.Name, &scopeEntry{val: cv, isAlloc: false})
		}
	}

	return nil
}

// hasDeferStmt reports whether body contains any DeferStmt (recursively).
// Nested fn/lambda declarations are not descended into.
// NOTE: concrete nil pointers (e.g. (*ast.Block)(nil)) passed as ast.Node are
// non-nil interfaces; all concrete-pointer cases must guard against nil n.
func hasDeferStmt(body ast.Node) bool {
	if body == nil {
		return false
	}

	switch n := body.(type) {
	case *ast.DeferStmt:
		return n != nil
	case *ast.FuncDecl, *ast.LambdaExpr:
		return false // defers inside nested fns don't affect outer's ret slot
	case *ast.Block:
		if n == nil {
			return false
		}

		for _, s := range n.Stmts {
			if hasDeferStmt(s) {
				return true
			}
		}
	case *ast.ExprStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Expr)
	case *ast.VarDecl:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.AssignStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.ReturnStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.IfStmt:
		if n == nil {
			return false
		}

		if n.Then != nil && hasDeferStmt(n.Then) {
			return true
		}

		if n.Else != nil && hasDeferStmt(n.Else) {
			return true
		}

		for _, elif := range n.ElseIfs {
			if elif.Body != nil && hasDeferStmt(elif.Body) {
				return true
			}
		}
	case *ast.ForStmt:
		if n == nil {
			return false
		}

		return n.Body != nil && hasDeferStmt(n.Body)
	case *ast.MatchStmt:
		if n == nil {
			return false
		}

		for _, arm := range n.Cases {
			if arm.Body != nil && hasDeferStmt(arm.Body) {
				return true
			}
		}
	}

	return false
}

// hasSelfTailCall reports whether body contains at least one direct self-call
// to funcName in tail position (as the sole expression of a where clause, or
// as the value of an explicit return statement).  Nested fn/lambda bodies are
// not descended into.
func hasSelfTailCall(funcName string, body ast.Node) bool {
	if body == nil {
		return false
	}

	switch n := body.(type) {
	case *ast.ReturnStmt:
		if n == nil {
			return false
		}

		return isSelfCallExpr(funcName, n.Value)
	case *ast.WhereList:
		if n == nil {
			return false
		}

		for _, c := range n.Clauses {
			if hasSelfTailCall(funcName, c.Body) {
				return true
			}
		}
	case *ast.Block:
		if n == nil {
			return false
		}

		for _, s := range n.Stmts {
			if hasSelfTailCall(funcName, s) {
				return true
			}
		}
	case *ast.IfStmt:
		if n == nil {
			return false
		}

		if hasSelfTailCall(funcName, n.Then) {
			return true
		}

		if hasSelfTailCall(funcName, n.Else) {
			return true
		}

		for _, elif := range n.ElseIfs {
			if hasSelfTailCall(funcName, elif.Body) {
				return true
			}
		}
	case *ast.MatchStmt:
		if n == nil {
			return false
		}

		for _, arm := range n.Cases {
			if hasSelfTailCall(funcName, arm.Body) {
				return true
			}
		}
	case *ast.ExprStmt:
		if n == nil {
			return false
		}

		return isSelfCallExpr(funcName, n.Expr)
	case *ast.FuncDecl, *ast.LambdaExpr:
		return false // don't descend into nested functions
	default:
		return isSelfCallExpr(funcName, n)
	}

	return false
}

// isSelfCallExpr returns true if node is a direct call to funcName.
func isSelfCallExpr(funcName string, node ast.Node) bool {
	if node == nil {
		return false
	}

	ce, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}

	ident, ok := ce.Func.(*ast.Identifier)
	if !ok {
		return false
	}

	return ident.Name == funcName
}

// emitTCOLoopBack handles a tail self-call in a TCO-eligible function:
// it evaluates the new argument values, releases any in-scope RC locals,
// stores the new values into the parameter allocas, and branches back to
// the tco_loop block instead of emitting a recursive call + return.
func (cg *CodeGen) emitTCOLoopBack(block *ir.Block, ce *ast.CallExpr) error {
	// Evaluate all new argument values before touching any alloca so that
	// expressions like fact(n-1, n*acc) can safely read n and acc.
	newVals := make([]value.Value, len(cg.tcoParams))

	for i, astArg := range ce.Args {
		val, err := cg.genExpr(block, astArg)
		if err != nil {
			return err
		}
		// Sync block advance (e.g. coro chain calls can redirect cg.curBlock).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
		// Coerce to the alloca's element type.
		if e, ok := cg.curScope.lookup(cg.tcoParams[i]); ok && e.isAlloc {
			if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 {
				val = cg.coerce(block, val, alloca.ElemType)
			}
		}

		newVals[i] = val
	}

	// Release any RC-tracked locals that are live in the current scope
	// (non-RC params are skipped automatically by emitAllScopeReleases).
	cg.emitAllScopeReleases(block, "")

	// Update the parameter allocas with the new values.
	for i, paramName := range cg.tcoParams {
		if e, ok := cg.curScope.lookup(paramName); ok && e.isAlloc {
			if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 {
				block.NewStore(newVals[i], alloca)
			}
		}
	}

	// Branch back to the loop header.
	block.NewBr(cg.tcoLoopTop)

	return nil
}

// resolveMutualTCOCallee checks whether name refers to a Tin function that can
// receive a musttail call from the current function. Returns the IR function and
// true when eligible; false otherwise.
func (cg *CodeGen) resolveMutualTCOCallee(name string) (*ir.Func, bool) {
	if cg.curFn == nil {
		return nil, false
	}

	entry, ok := cg.curScope.lookup(name)
	if !ok || entry.isAlloc {
		return nil, false
	}

	callee, ok := entry.val.(*ir.Func)
	if !ok {
		return nil, false
	}

	// Exclude C extern symbols (they may use different calling conventions).
	if cg.externIRNames[callee.Name()] {
		return nil, false
	}

	// Coroutine functions have their IR signatures transformed; skip mutual TCO.
	if cg.inCoroFn {
		return nil, false
	}

	// musttail requires identical return types.
	if !callee.Sig.RetType.Equal(cg.curFn.Sig.RetType) {
		return nil, false
	}

	// No variadic callees.
	if callee.Sig.Variadic {
		return nil, false
	}

	// musttail requires matching parameter counts and types (sibling call constraint).
	if len(callee.Params) != len(cg.curFn.Params) {
		return nil, false
	}

	for i, cp := range callee.Params {
		if !cp.Type().Equal(cg.curFn.Params[i].Type()) {
			return nil, false
		}
	}

	// All callee params must be non-RC so scope cleanup before the call is safe.
	for _, p := range callee.Params {
		if isRCTrackedType(p.Type()) {
			return nil, false
		}
	}

	// LLVM's musttail tail-call elimination refuses any caller whose frame
	// still has live allocas at the call site. A trivial pass-through fn
	// like `fn from(v f64) Value = return from_f64_impl(v)` spills `v` to
	// an alloca first, which is enough to break musttail. Skip when we can
	// see allocas in the caller's entry block.
	if hasAllocaInsts(cg.curFn) {
		return nil, false
	}

	return callee, true
}

// hasAllocaInsts reports whether fn currently contains any alloca
// instructions in any of its emitted blocks. Used to gate mutual TCO so
// musttail isn't requested from a frame that LLVM cannot pop.
//
// We scan every block, not just the entry, because allocas can be added
// past the call site (e.g. a deferred string interp builds an alloca in
// a successor block) and any live alloca anywhere in the function would
// keep LLVM from rewriting the musttail into a real tail jump.
func hasAllocaInsts(fn *ir.Func) bool {
	if fn == nil {
		return false
	}

	for _, blk := range fn.Blocks {
		for _, inst := range blk.Insts {
			if _, ok := inst.(*ir.InstAlloca); ok {
				return true
			}
		}
	}

	return false
}

// emitMutualTCO emits a musttail call to callee and returns its result,
// performing scope cleanup BEFORE the call so no instructions appear between
// the musttail call and the immediately following ret.
func (cg *CodeGen) emitMutualTCO(block *ir.Block, ce *ast.CallExpr, callee *ir.Func) error {
	// Evaluate all argument values before releasing scope.
	argVals := make([]value.Value, len(ce.Args))

	for i, arg := range ce.Args {
		cg.curBlock = block // sync before genExpr so stale values don't misdirect block updates

		v, err := cg.genExpr(block, arg)
		if err != nil {
			return err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if i < len(callee.Params) {
			v = cg.coerce(block, v, callee.Params[i].Type())
		}

		argVals[i] = v
	}

	// Release all RC-tracked locals before the tail call.
	cg.emitAllScopeReleases(block, "")

	// Emit musttail call.
	call := block.NewCall(callee, argVals...)
	call.Tail = enum.TailMustTail

	if cg.tcoReportFn != nil {
		cg.tcoReportFn(cg.curFn.Name(), callee.Name())
	}

	// Return the call result directly (no post-processing, satisfying musttail).
	if irtypes.IsVoid(call.Type()) {
		block.NewRet(nil)
	} else {
		block.NewRet(call)
	}

	return nil
}

// isFutureRetType reports whether t is a Future[T] generic type.
func isFutureRetType(t ast.TypeExpr) bool {
	g, ok := t.(*ast.GenericType)

	return ok && g.Name == "Future"
}

// bodyContainsSpawnOrAwait reports whether any node in the body (recursively)
// is a SpawnExpr or AwaitExpr. Nested fn declarations are not descended into.
func bodyContainsSpawnOrAwait(body []ast.Node) bool {
	var walk func(node ast.Node) bool

	walk = func(node ast.Node) bool {
		if node == nil {
			return false
		}

		switch n := node.(type) {
		case *ast.SpawnExpr, *ast.AwaitExpr:
			return true
		case *ast.FuncDecl:
			return false // don't descend into nested fn declarations
		case *ast.Block:
			for _, s := range n.Stmts {
				if walk(s) {
					return true
				}
			}
		case *ast.ExprStmt:
			return walk(n.Expr)
		case *ast.VarDecl:
			return walk(n.Value)
		case *ast.AssignStmt:
			return walk(n.Value)
		case *ast.AugAssignStmt:
			return walk(n.Value)
		case *ast.ReturnStmt:
			return walk(n.Value)
		case *ast.EchoStmt:
			return walk(n.Value)
		case *ast.IfStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Then != nil && walk(n.Then) {
				return true
			}

			if n.Else != nil && walk(n.Else) {
				return true
			}
		case *ast.ForStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Body != nil && walk(n.Body) {
				return true
			}
		case *ast.CallExpr:
			if walk(n.Func) {
				return true
			}

			for _, a := range n.Args {
				if walk(a) {
					return true
				}
			}
		case *ast.BinExpr:
			return walk(n.Left) || walk(n.Right)
		}

		return false
	}
	for _, s := range body {
		if walk(s) {
			return true
		}
	}

	return false
}
