package codegen

// Boolean expression simplification.
//
// simplifyBool rewrites boolean expressions via De Morgan's laws and a few
// related algebraic identities:
//
//   !true               -> false
//   !false              -> true
//   !!x                 -> x
//   !(a == b)           -> a != b
//   !(a != b)           -> a == b
//   !(a <  b)           -> a >= b
//   !(a <= b)           -> a >  b
//   !(a >  b)           -> a <= b
//   !(a >= b)           -> a <  b
//   !(a && b)           -> !a || !b
//   !(a || b)           -> !a && !b
//   a && true           -> a
//   a && false          -> false
//   true && a           -> a
//   false && a          -> false
//   a || true           -> true
//   a || false          -> a
//   true || a           -> true
//   false || a          -> a
//
// The pass is bottom-up: children are simplified before the current node, so
// `!!(a && true)` collapses to `a` in one sweep. The algorithm terminates
// because every rule either removes a node or pushes a `!` strictly inward
// towards a leaf that does not accept further pushes (an identifier, call,
// etc.).
//
// When cg.verboseDemorgan is true, each rewrite is logged to stderr with the
// source position, the rule name, and a pretty-printed before/after pair so
// the user can see what the optimizer changed.

import (
	"fmt"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// isNotOp reports whether s is a textual negation operator (either `!` or the
// keyword `not`).
func isNotOp(s string) bool { return s == "!" || s == "not" }

// isAndOp / isOrOp check both the symbol form and the keyword form of the
// short-circuit connectives Tin accepts.
func isAndOp(s string) bool { return s == "&&" || s == "and" }
func isOrOp(s string) bool  { return s == "||" || s == "or" }

// negatedCmpOp returns the opposite comparison operator, or "" when op is
// not a comparison we can negate algebraically.
func negatedCmpOp(op string) string {
	switch op {
	case "==":
		return "!="
	case "!=":
		return "=="
	case "<":
		return ">="
	case "<=":
		return ">"
	case ">":
		return "<="
	case ">=":
		return "<"
	}

	return ""
}

// simplifyBool returns a rewritten copy of e with De Morgan simplifications
// applied. The original AST is not mutated. When cg.verboseDemorgan is set
// each applied rule is recorded via logDemorgan.
func (cg *CodeGen) simplifyBool(e ast.Node) ast.Node {
	if e == nil {
		return nil
	}

	switch n := e.(type) {
	case *ast.UnaryExpr:
		if !isNotOp(n.Op) {
			return e
		}

		inner := cg.simplifyBool(n.Expr)

		return cg.pushNotInward(n, inner)

	case *ast.BinExpr:
		if isAndOp(n.Op) || isOrOp(n.Op) {
			left := cg.simplifyBool(n.Left)
			right := cg.simplifyBool(n.Right)

			return cg.simplifyConnective(n, left, right)
		}
	}

	return e
}

// pushNotInward implements all the `!(...)` -> ... rewrites. It assumes the
// inner operand has already been simplified.
func (cg *CodeGen) pushNotInward(orig *ast.UnaryExpr, inner ast.Node) ast.Node {
	switch x := inner.(type) {
	case *ast.BoolLit:
		// !true -> false  /  !false -> true
		out := &ast.BoolLit{Value: !x.Value}
		cg.logDemorgan(orig, "!bool", orig, out)

		return out

	case *ast.UnaryExpr:
		if isNotOp(x.Op) {
			// !!x -> x
			cg.logDemorgan(orig, "double-negation", orig, x.Expr)

			return x.Expr
		}

	case *ast.BinExpr:
		// !(a <cmp> b) -> a <negated-cmp> b.
		//
		// SOUNDNESS: this rewrite is only valid when neither operand can
		// be a float. For IEEE floats, both `a < b` and `a >= b` return
		// false when either operand is NaN, so `!(a < b)` (true for NaN)
		// is NOT equivalent to `a >= b` (false for NaN). For integers
		// there is no such discrepancy and the rewrite is a straight
		// IR-level swap of predicate.
		if neg := negatedCmpOp(x.Op); neg != "" && cg.canSafelyNegateCmp(x.Left, x.Right) {
			out := &ast.BinExpr{Left: x.Left, Op: neg, Right: x.Right}
			out.SetPos(x.Pos())
			cg.logDemorgan(orig, "negate-cmp("+x.Op+")", orig, out)

			return out
		}

		if isAndOp(x.Op) || isOrOp(x.Op) {
			// De Morgan: !(a && b) -> !a || !b ;  !(a || b) -> !a && !b
			leftNeg := &ast.UnaryExpr{Op: orig.Op, Expr: x.Left}
			leftNeg.SetPos(orig.Pos())
			rightNeg := &ast.UnaryExpr{Op: orig.Op, Expr: x.Right}
			rightNeg.SetPos(orig.Pos())
			notL := cg.simplifyBool(leftNeg)
			notR := cg.simplifyBool(rightNeg)

			newOp := "||"
			rule := "de-morgan(&&)"

			if isOrOp(x.Op) {
				newOp = "&&"
				rule = "de-morgan(||)"
			}

			out := &ast.BinExpr{Left: notL, Op: newOp, Right: notR}
			out.SetPos(orig.Pos())
			cg.logDemorgan(orig, rule, orig, out)

			return out
		}
	}
	// No rule applies; keep the negation with the simplified inner.
	return &ast.UnaryExpr{Op: orig.Op, Expr: inner}
}

// simplifyConnective applies the constant-absorption rules for && and ||.
// It assumes both operands are already simplified.
func (cg *CodeGen) simplifyConnective(orig *ast.BinExpr, left, right ast.Node) ast.Node {
	if isAndOp(orig.Op) {
		if bl, ok := left.(*ast.BoolLit); ok {
			if bl.Value {
				// true && a -> a
				cg.logDemorgan(orig, "absorb(true&&_)", orig, right)

				return right
			}
			// false && a -> false (short-circuits; note this discards
			// any side effects in `a`, which is acceptable for a
			// constant false LHS).
			cg.logDemorgan(orig, "absorb(false&&_)", orig, left)

			return left
		}

		if br, ok := right.(*ast.BoolLit); ok {
			if br.Value {
				// a && true -> a
				cg.logDemorgan(orig, "absorb(_&&true)", orig, left)

				return left
			}
			// a && false -> false (keeps `a`'s side effects by running
			// the LHS first; callers that care should have already
			// simplified `a`).
			out := &ast.BinExpr{Left: left, Op: orig.Op, Right: right}

			return out
		}

		return &ast.BinExpr{Left: left, Op: orig.Op, Right: right}
	}
	// Or connective.
	if bl, ok := left.(*ast.BoolLit); ok {
		if bl.Value {
			// true || a -> true
			cg.logDemorgan(orig, "absorb(true||_)", orig, left)

			return left
		}
		// false || a -> a
		cg.logDemorgan(orig, "absorb(false||_)", orig, right)

		return right
	}

	if br, ok := right.(*ast.BoolLit); ok && !br.Value {
		// a || false -> a
		cg.logDemorgan(orig, "absorb(_||false)", orig, left)

		return left
	}
	// a || true: NOT rewritten to `true` because `a` may have side
	// effects (function call, mutation) which the original program
	// evaluates before the constant-true short-circuits.

	return &ast.BinExpr{Left: left, Op: orig.Op, Right: right}
}

// logDemorgan records one rewrite to stderr when -v-demorgan is active.
// Positions come from the original node so the user can locate the site in
// their source.
func (cg *CodeGen) logDemorgan(origin ast.Node, rule string, before, after ast.Node) {
	if !cg.verboseDemorgan {
		return
	}

	pos := origin.Pos()
	file := cg.filenameForDiag()

	b := strings.TrimSpace(ast.PrintExpr(before))
	a := strings.TrimSpace(ast.PrintExpr(after))

	fmt.Fprintf(cg.matchInfoSink(), "[demorgan] %s:%d:%d %s: %s => %s\n",
		file, pos.Line, pos.Col, rule, b, a)
}

// emitBoolAnalysisWarning prints "condition is always true/false" to stderr.
// Suppressed by -Wno-bool-analysis.
func (cg *CodeGen) emitBoolAnalysisWarning(pos ast.Pos, value bool, loc string) {
	if cg.noWarnBoolAnalysis {
		return
	}

	truth := "true"
	if !value {
		truth = "false"
	}

	fmt.Fprintf(cg.matchInfoSink(),
		"%s:%d:%d: warning: %s condition is always %s\n",
		cg.filenameForDiag(), pos.Line, pos.Col, loc, truth)
}

// prepareBoolCond runs De Morgan simplification on a boolean condition and
// emits the always-true/false warning when the simplified form folds to a
// constant. loc is a short label used in the warning message ("if", "elif",
// "while", "where", "for"). When allowBareTrueLoop is true AND the original
// condition was a bare `true` literal, the warning is suppressed so the
// idiomatic `for true:` / `while true:` infinite loop stays quiet.
//
// Returns the simplified expression so the caller can use it for subsequent
// fold checks and IR generation. Simplified forms always have identical
// runtime semantics to the original.
func (cg *CodeGen) prepareBoolCond(e ast.Node, loc string, allowBareTrueLoop bool) ast.Node {
	if e == nil {
		return nil
	}

	simp := cg.simplifyBool(e)

	v, ok := cg.foldedBoolCondition(simp)
	if !ok {
		return simp
	}

	if allowBareTrueLoop {
		if bl, isBL := e.(*ast.BoolLit); isBL && bl.Value {
			return simp
		}
	}
	// Generic-dispatch pattern: inside a `fn f[T](...)` body we routinely
	// see `if typeof(v) == 'i64: ...` (or a let-bound alias of one) whose
	// result is decidable per instantiation. Those conditions are
	// intentional compile-time dispatches, not user errors, so suppress
	// the warning when the condition involves typeof() directly or via a
	// followed const-let binding.
	if cg.condInvolvesTypeof(e) {
		return simp
	}

	pos := firstPos(simp)
	if pos.Line == 0 {
		pos = firstPos(e)
	}

	cg.emitBoolAnalysisWarning(pos, v, loc)

	return simp
}

// canSafelyNegateCmp reports whether rewriting `!(a <cmp> b)` to the
// negated-predicate form is semantically equivalent. For IEEE floats the
// rewrite is unsound around NaN (both `a < b` and `a >= b` return false
// for NaN, so `!(a < b)` != `a >= b`), so the rewrite is only applied
// when both operands can be proven non-float.
func (cg *CodeGen) canSafelyNegateCmp(a, b ast.Node) bool {
	return cg.isNonFloatOperand(a) && cg.isNonFloatOperand(b)
}

// isNonFloatOperand reports whether n is guaranteed to evaluate to a
// non-float value at runtime. Unknown expressions conservatively return
// false so the rewrite stays on the safe side.
func (cg *CodeGen) isNonFloatOperand(n ast.Node) bool {
	switch v := n.(type) {
	case *ast.FloatLit:
		return false
	case *ast.IntLit, *ast.BoolLit, *ast.CharLit, *ast.AtomLit, *ast.StringLit:
		return true
	case *ast.Identifier:
		if t := cg.staticTypeOf(v); t != nil {
			return !irtypes.IsFloat(t)
		}
	case *ast.UnaryExpr:
		// `-x` / `+x` / `~x` preserve the operand's numeric domain; `!x`
		// / `not x` produce a bool (itself non-float). Recursing on the
		// operand gives the right answer in both cases even though the
		// reasoning differs - keep this comment so a future reader does
		// not split the arms incorrectly.
		return cg.isNonFloatOperand(v.Expr)
	case *ast.AsExpr:
		// A cast locks the target type; only allow the rewrite when the
		// annotation is a known non-float primitive. User aliases
		// (e.g. `type Temp = f64`) could resolve to float at later
		// stages, so treat unknown simple names as "could be float" and
		// skip the rewrite.
		if st, ok := v.Type.(*ast.SimpleType); ok {
			switch st.Name {
			case "i8", "i16", "i32", "i64", "i128",
				"u8", "u16", "u32", "u64", "u128",
				"bool", "byte", "char", "atom", "string":
				return true
			}
		}
	}

	return false
}

// condInvolvesTypeof reports whether the expression tree contains a
// typeof(...) call, either inline or reachable through an identifier whose
// constant-let initializer (set by genVarDecl for foldable `let`s) itself
// reaches a typeof. Used to skip always-true/false warnings for generic
// dispatch idioms like `let t = typeof(v); if t == 'i64: ...`.
func (cg *CodeGen) condInvolvesTypeof(n ast.Node) bool {
	if n == nil {
		return false
	}

	switch v := n.(type) {
	case *ast.TypeofExpr:
		return true
	case *ast.BinExpr:
		return cg.condInvolvesTypeof(v.Left) || cg.condInvolvesTypeof(v.Right)
	case *ast.UnaryExpr:
		return cg.condInvolvesTypeof(v.Expr)
	case *ast.Identifier:
		if entry, ok := cg.curScope.lookup(v.Name); ok && entry.constInitExpr != nil {
			return cg.condInvolvesTypeof(entry.constInitExpr)
		}
	}

	return false
}

// firstPos returns the earliest source position attached to any node in the
// expression tree. Many literal/binary nodes omit a pos field when parsed
// (e.g. `1 == 1` has no pos on the BinExpr itself); walking for a child
// token gives a usable diagnostic location even so.
func firstPos(n ast.Node) ast.Pos {
	if n == nil {
		return ast.Pos{}
	}

	if p := n.Pos(); p.Line != 0 {
		return p
	}

	switch v := n.(type) {
	case *ast.BinExpr:
		if p := firstPos(v.Left); p.Line != 0 {
			return p
		}

		return firstPos(v.Right)
	case *ast.UnaryExpr:
		return firstPos(v.Expr)
	}

	return ast.Pos{}
}
