package codegen

import (
	"reflect"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkLoopInvariant(loop *ast.ForStmt) {
	if loop.Body == nil || len(loop.Body.Stmts) == 0 {
		return
	}

	mutated := map[string]bool{}

	collectLoopMutations(loop.Body, mutated)

	if loop.Post != nil {
		collectLoopMutations(loop.Post, mutated)
	}

	if loop.VarName != "" {
		mutated[loop.VarName] = true
	}

	cg.walkLicm(loop.Body, false, mutated)
}

// walkLicm descends through n's children. When it finds a BinExpr or
// UnaryExpr that is fully loop-invariant and references at least one
// identifier (so the optimizer can't trivially fold it), it warns -- but
// only at the outermost such subtree, by passing parentInv=true to
// children of an emitted node so they suppress their own emit.
//
// Nested ForStmt and LambdaExpr bodies are skipped: nested loops get
// their own checkLoopInvariant pass, and lambdas may be invoked outside
// the loop where "loop-invariant" no longer applies.
func (cg *CodeGen) walkLicm(n ast.Node, parentInv bool, mutated map[string]bool) {
	if n == nil {
		return
	}
	// Same typed-nil guard as walkAST: an interface value can wrap a nil
	// concrete pointer (e.g. *ast.Block) which `n == nil` misses, and
	// dereferencing it (n.Pos(), or any field access) would segfault.
	if rv := reflect.ValueOf(n); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return
	}

	switch n.(type) {
	case *ast.ForStmt, *ast.LambdaExpr:
		return
	}

	isInv := false

	switch n.(type) {
	case *ast.BinExpr, *ast.UnaryExpr:
		if isLoopInvariantExpr(n, mutated) && containsIdentifier(n) {
			isInv = true

			if !parentInv {
				cg.warn(DiagLoopInvariant, n.Pos(),
					"expression does not depend on loop state; consider hoisting it before the loop")
			}
		}
	}

	switch e := n.(type) {
	case *ast.Block:
		for _, s := range e.Stmts {
			cg.walkLicm(s, false, mutated)
		}
	case *ast.IfStmt:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)

		for _, ei := range e.ElseIfs {
			cg.walkLicm(ei.Cond, false, mutated)
			cg.walkLicm(ei.Body, false, mutated)
		}

		cg.walkLicm(e.Else, false, mutated)
	case *ast.MatchStmt:
		cg.walkLicm(e.Expr, false, mutated)

		for _, c := range e.Cases {
			cg.walkLicm(c.Pattern, false, mutated)
			cg.walkLicm(c.Guard, false, mutated)
			cg.walkLicm(c.Body, false, mutated)
		}

		cg.walkLicm(e.Default, false, mutated)
	case *ast.AssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.AugAssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.PostfixStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.VarDecl:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ReturnStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.EchoStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ExprStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.DeferStmt:
		cg.walkLicm(e.Call, false, mutated)
	case *ast.BinExpr:
		cg.walkLicm(e.Left, isInv, mutated)
		cg.walkLicm(e.Right, isInv, mutated)
	case *ast.UnaryExpr:
		cg.walkLicm(e.Expr, isInv, mutated)
	case *ast.CallExpr:
		cg.walkLicm(e.Func, false, mutated)

		for _, a := range e.Args {
			cg.walkLicm(a, false, mutated)
		}
	case *ast.IndexExpr:
		cg.walkLicm(e.Expr, false, mutated)
		cg.walkLicm(e.Index, false, mutated)
	case *ast.FieldAccess:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AsExpr:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AwaitExpr:
		cg.walkLicm(e.Future, false, mutated)
	case *ast.SpawnExpr:
		cg.walkLicm(e.Call, false, mutated)
		cg.walkLicm(e.DoBlock, false, mutated)
	case *ast.TernaryExpr:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)
		cg.walkLicm(e.Else, false, mutated)
	case *ast.TaggedBlock:
		cg.walkLicm(e.Body, false, mutated)
	}
}

// isLoopInvariantExpr reports whether n is a pure expression all of
// whose identifier operands are not in the mutated set. Conservative:
// returns false for any node kind that could reach a side-effecting
// operation (calls, derefs, indexing, address-of, await, spawn).
func isLoopInvariantExpr(n ast.Node, mutated map[string]bool) bool {
	if n == nil {
		return true
	}

	switch e := n.(type) {
	case *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit, *ast.NilLit:
		return true
	case *ast.Identifier:
		return !mutated[e.Name]
	case *ast.FieldAccess:
		return isLoopInvariantExpr(e.Expr, mutated)
	case *ast.UnaryExpr:
		return isPureUnaryOp(e.Op) && isLoopInvariantExpr(e.Expr, mutated)
	case *ast.BinExpr:
		return isPureBinOp(e.Op) &&
			isLoopInvariantExpr(e.Left, mutated) &&
			isLoopInvariantExpr(e.Right, mutated)
	case *ast.AsExpr:
		return isLoopInvariantExpr(e.Expr, mutated)
	}

	return false
}

func isPureBinOp(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%",
		"&", "|", "^", "<<", ">>",
		"==", "!=", "<", "<=", ">", ">=",
		"&&", "||":
		return true
	}

	return false
}

func isPureUnaryOp(op string) bool {
	switch op {
	case "-", "+", "!", "~", "not":
		return true
	}

	return false
}

// containsIdentifier reports whether n's subtree contains at least one
// *ast.Identifier. Used by the LICM check so we don't emit on pure
// constant expressions like `1 + 2` -- the optimizer folds those without
// a programmer-visible improvement.
func containsIdentifier(n ast.Node) bool {
	found := false

	walkAST(n, func(c ast.Node) {
		if _, ok := c.(*ast.Identifier); ok {
			found = true
		}
	})

	return found
}

// collectLoopMutations records every name that is written, address-
// taken, or rebound inside the loop body. Conservative on every front:
// any `&x`, any assignment-target chain rooted at an Identifier, and any
// VarDecl introduces the bound name into the mutated set. CallExpr args
// of the form `&x` also mark x, since the callee can mutate through the
// address.
func collectLoopMutations(body ast.Node, mutated map[string]bool) {
	walkAST(body, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.AssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.AugAssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.PostfixStmt:
			markBaseIdent(e.Expr, mutated)
		case *ast.VarDecl:
			mutated[e.Name] = true
		case *ast.AddrExpr:
			markBaseIdent(e.Val, mutated)
		case *ast.AddressOfExpr:
			markBaseIdent(e.Expr, mutated)
		}
	})
}

// markBaseIdent walks an l-value chain (FieldAccess / IndexExpr /
// DerefExpr) down to its root identifier and records the name. A write
// through `obj.field`, `arr[i]`, or `*p` means we can no longer prove
// `obj`, `arr`, or `p` is loop-invariant, so the entire base name is
// marked.
func markBaseIdent(n ast.Node, m map[string]bool) {
	for n != nil {
		switch e := n.(type) {
		case *ast.Identifier:
			m[e.Name] = true

			return
		case *ast.FieldAccess:
			n = e.Expr
		case *ast.IndexExpr:
			n = e.Expr
		case *ast.DerefExpr:
			n = e.Expr
		default:
			return
		}
	}
}

// checkMagicNumbers flags int and float literals that aren't in the
// universal exempt set ({-1, 0, 1, 2}) and aren't in a context where
// embedding the literal directly is conventional.
//
// Two-pass design: the first walkAST classifies each node into context
// buckets (const-init descendants, array-index descendants, direct
// comparison/bitwise operands -- including those wrapped in unary +/-
// or `as` casts). The second walk inspects each literal against its
// bucket flags and emits when none of them apply.
//
// Default-off; gated through warn() but we also short-circuit the
// classification pass when the diagnostic is disabled to avoid building
// per-node maps for nothing.
