package codegen

// Cheap, syntax-directed checks. Each one walks the AST once and emits a
// diagnostic on a recognizable shape - no dataflow, no type inference
// beyond what the existing scope already knows.

import (
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// runAstChecks runs every syntax-level check over the program.
func (cg *CodeGen) runAstChecks(prog *ast.Program) {
	if cg.replMode {
		return
	}

	// AST-time struct registry.  cg.structDeclsByName populates during
	// codegen, *after* runAstChecks, so the redundant-cast check on
	// struct literals would otherwise see an empty map.  Walk the
	// program here to build a name -> decl map independent of codegen
	// ordering.  Imported structs are not visible here, but the bulk
	// of struct literals reference user-declared structs in the same
	// translation unit, so the check still has high coverage.
	astStructDecls := map[string]*ast.StructDecl{}

	for _, stmt := range prog.Stmts {
		walkAST(stmt, func(n ast.Node) {
			if sd, ok := n.(*ast.StructDecl); ok {
				astStructDecls[sd.Name] = sd
			}
		})
	}

	// -Wbare-parking-async-call / -Wbare-async-call: collect CallExpr
	// pointers that sit directly under `await` or `spawn` so the
	// per-call check below can distinguish legitimate uses from the
	// demoted bare-call form.
	awaitedOrSpawned := cg.collectAwaitedOrSpawnedCalls(prog)

	for _, n := range prog.Stmts {
		if fd, ok := n.(*ast.FuncDecl); ok {
			cg.checkInfiniteRecursion(fd)
			cg.checkFiberMisuse(fd)
			cg.checkUnguardedTraitDowncast(fd)
			cg.checkRedundantReturnCast(fd)
			cg.checkSyncUsesAwait(fd)
			cg.checkNonTinThread(fd)
			cg.checkPtrTraitInFuncSig(fd)
		}

		if sd, ok := n.(*ast.StructDecl); ok {
			cg.checkPtrTraitInStruct(sd)
		}
		// Track whether the immediately-enclosing FuncDecl declares
		// a Result-shaped return type. The match-as-try lint only
		// fires when the enclosing fn returns Result, since `try`
		// otherwise can't compile inside it.
		prevReturnsResult := cg.curFnReturnsResult

		// Track the enclosing FuncDecl so call-site lints can consult
		// caller-side tags (e.g. -Winterop-self-call skips when the
		// enclosing fn is itself `#interop`).  Nested function decls
		// are uncommon at this AST level but the outer fd remains the
		// closest match for diagnostic intent.
		var enclosingFn *ast.FuncDecl
		if fd, ok := n.(*ast.FuncDecl); ok {
			enclosingFn = fd

			if astReturnTypeIsResult(fd.RetType) {
				cg.curFnReturnsResult++
			}
		}

		walkAST(n, func(node ast.Node) {
			switch e := node.(type) {
			case *ast.BinExpr:
				cg.checkIdenticalOperands(e)
				cg.checkArithIdentity(e)
				cg.checkFloatEqual(e)
			case *ast.AsExpr:
				cg.checkUselessCast(e)
			case *ast.IfStmt:
				cg.checkEmptyIfBody(e)
			case *ast.ForStmt:
				cg.checkLoopInvariant(e)
			case *ast.VarDecl:
				cg.checkRedundantArrayElemCasts(e)
				cg.checkSyncFnCoercedToAsync(e)
				cg.checkTraitSnapshotMutation(e)
			case *ast.CallExpr:
				cg.checkRedundantArgCasts(e)
				cg.checkBareAsyncCall(e, awaitedOrSpawned)
				cg.checkSyncFnCoercedToAsync(e)
				cg.checkInteropSelfCall(e, enclosingFn)
			case *ast.ExprStmt:
				cg.checkDroppableFiber(e)
			case *ast.StructLit:
				cg.checkRedundantStructFieldCasts(e, astStructDecls)
			case *ast.MatchStmt:
				cg.checkResultMatchAntipattern(e)
			}
		})

		cg.curFnReturnsResult = prevReturnsResult
	}

	cg.checkMagicNumbers(prog)
	cg.checkStyle(prog)
}

// checkRedundantReturnCast warns when a function with declared return
// type T returns `<literal> as T`.  The implicit return-coercion
// would auto-coerce the literal already; the explicit cast adds
// nothing.  Mirrors checkRedundantArrayElemCasts at the return-stmt
// granularity.

func (cg *CodeGen) checkEmptyIfBody(s *ast.IfStmt) {
	if s.Then != nil && len(s.Then.Stmts) == 0 && !s.Then.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty if body")
	}

	if s.Else != nil && len(s.Else.Stmts) == 0 && !s.Else.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty else body")
	}
}

// exprIsFloat reports whether expr's static type is f32 or f64. Conservative
// (returns false on unknown), so the check never fires when in doubt.
func (cg *CodeGen) exprIsFloat(expr ast.Node) bool {
	t := cg.staticTypeOf(expr)
	if t == nil {
		return false
	}

	return irtypes.IsFloat(t)
}

// checkLoopInvariant flags pure expressions inside a loop body whose
// operands are never written (or address-taken) within the body or post
// statements. The hint suggests hoisting the expression before the loop.
//
// Only arithmetic/bitwise/comparison/boolean/unary/field/cast trees over
// identifiers and literals are considered: anything reached through a
// call, pointer deref, or indexed read may observe state we don't
// statically prove invariant, so we leave those alone.
//
// Emits at the maximal invariant subtree -- if `(a + b) * c` is fully
// invariant, fires once on the whole expression rather than separately on
// each subterm.
