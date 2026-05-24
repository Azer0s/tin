package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkMagicNumbers(prog *ast.Program) {
	if !cg.diagEnabled(DiagMagicNumber) {
		return
	}

	inConst := map[ast.Node]bool{}
	inIndex := map[ast.Node]bool{}
	cmpOperand := map[ast.Node]bool{}
	bitOperand := map[ast.Node]bool{}

	classify := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.VarDecl:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.TopLevelVar:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.IndexExpr:
			if e.Index != nil {
				walkAST(e.Index, func(c ast.Node) { inIndex[c] = true })
			}
		case *ast.BinExpr:
			switch e.Op {
			case "==", "!=", "<", "<=", ">", ">=":
				peelLitContext(e.Left, cmpOperand)
				peelLitContext(e.Right, cmpOperand)
			case "&", "|", "^", "<<", ">>":
				peelLitContext(e.Left, bitOperand)
				peelLitContext(e.Right, bitOperand)
			}
		}
	}

	flag := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.IntLit:
			if shouldFlagIntMagic(e, inConst[e], inIndex[e], cmpOperand[e], bitOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %d; consider naming it as a const", e.Value)
			}
		case *ast.FloatLit:
			if shouldFlagFloatMagic(e, inConst[e], inIndex[e], cmpOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %g; consider naming it as a const", e.Value)
			}
		}
	}

	for _, s := range prog.Stmts {
		walkAST(s, classify)
	}

	for _, s := range prog.Stmts {
		walkAST(s, flag)
	}
}

// peelLitContext records `n` and any literal-shaped descendant reached
// through unary +/- or an `as` cast. Captures `5`, `-5`, and `5 as i64`
// uniformly so the magic-number check exempts a literal regardless of
// the syntactic decoration around it.
func peelLitContext(n ast.Node, m map[ast.Node]bool) {
	for n != nil {
		m[n] = true

		switch e := n.(type) {
		case *ast.UnaryExpr:
			if e.Op == "-" || e.Op == "+" {
				n = e.Expr

				continue
			}
		case *ast.AsExpr:
			n = e.Expr

			continue
		}

		return
	}
}

// shouldFlagIntMagic decides whether an integer literal warrants a
// magic-number warning given its context flags.
func shouldFlagIntMagic(e *ast.IntLit, inConst, inIdx, cmpOp, bitOp bool) bool {
	if e.Big != nil {
		return false
	}

	if inConst || inIdx || cmpOp {
		return false
	}

	v := e.Value
	if v >= -1 && v <= 2 {
		return false
	}

	if bitOp && isBitOpExempt(v) {
		return false
	}

	return true
}

// shouldFlagFloatMagic decides whether a float literal warrants a
// magic-number warning. Floats don't get a bit-op exemption since
// bitwise ops aren't defined for them.
func shouldFlagFloatMagic(e *ast.FloatLit, inConst, inIdx, cmpOp bool) bool {
	if inConst || inIdx || cmpOp {
		return false
	}

	switch e.Value {
	case -1, 0, 0.5, 1, 2:
		return false
	}

	return true
}

// isBitOpExempt reports whether v is a recognizable bit pattern that
// commonly appears as a bitwise operand: power of two (single-bit set
// or shift count) or 2^N - 1 (an all-ones mask).
func isBitOpExempt(v int64) bool {
	if v <= 0 {
		return v == 0 || v == -1
	}

	if v&(v-1) == 0 {
		return true
	}

	if (v+1)&v == 0 {
		return true
	}

	return false
}

// diagEnabled reports whether a default-off diagnostic has been opted
// into via -W<name>, -Wpedantic, etc. Default-on diagnostics always
// return true.
func (cg *CodeGen) diagEnabled(name string) bool {
	if !defaultOffWarnings[name] {
		return true
	}

	if s := cg.diags[name]; s != nil {
		return s.enabled
	}

	return false
}

// astEqual reports whether two AST nodes are syntactically identical for
// the purposes of identical-operand detection. Conservative: only
// recognizes shapes that have no chance of side effects (identifiers,
// field access on the same target, integer literals).
