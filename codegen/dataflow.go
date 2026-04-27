package codegen

// Intraprocedural dataflow analysis. The pass is syntax-directed: it walks
// the AST recursively, threading an abstract state through each statement
// and merging at join points. Loops iterate to a small fixed bound.
//
// The lattice combines three pieces:
//
//   - SCCP-style constant propagation for integer and boolean locals.
//     Lattice elements are TOP (uninitialised), CONST(c), or BOTTOM (varying).
//
//   - Nil tracking for pointer / RC-typed locals, an Andersen-style
//     "may-point-to" set restricted to two abstract addresses: NIL and
//     NON_NIL. Per-variable lattice: TOP, NIL, NON_NIL, BOTTOM (could be
//     either).
//
//   - Branch narrowing: an `if p == nil { ... } else { ... }` propagates
//     `p == NIL` into the then-branch and `p == NON_NIL` into the else;
//     `if p != nil` is the inverse.
//
// Findings drive two warning sources:
//
//   - flow-sensitive `deref-nil`: dereferencing or field-accessing a name
//     that's provably nil at that program point.
//   - flow-sensitive `bool-analysis`: an `if`/`elif`/`while` whose
//     condition folds to a constant after substituting locals.

import (
	"github.com/Azer0s/tin/ast"
)

// nilFact is the per-variable abstract value for nil tracking.
type nilFact int

const (
	nilTop    nilFact = iota // unreachable / uninitialised
	nilIsNil                 // statically nil
	nilNonNil                // statically non-nil
	nilBottom                // could be either
)

func mergeNil(a, b nilFact) nilFact {
	if a == nilTop {
		return b
	}

	if b == nilTop {
		return a
	}

	if a == b {
		return a
	}

	return nilBottom
}

// constFact is the per-variable lattice for SCCP. Only int and bool
// constants are tracked; everything else is BOTTOM.
type constFact struct {
	kind    constKind
	intVal  int64
	boolVal bool
}

type constKind int

const (
	cTop constKind = iota
	cInt
	cBool
	cBottom
)

func cTopFact() constFact        { return constFact{kind: cTop} }
func cBotFact() constFact        { return constFact{kind: cBottom} }
func cIntFact(v int64) constFact { return constFact{kind: cInt, intVal: v} }
func cBoolFact(v bool) constFact { return constFact{kind: cBool, boolVal: v} }

func mergeConst(a, b constFact) constFact {
	if a.kind == cTop {
		return b
	}

	if b.kind == cTop {
		return a
	}

	if a.kind == cBottom || b.kind == cBottom {
		return cBotFact()
	}

	if a.kind != b.kind {
		return cBotFact()
	}

	if a.kind == cInt && a.intVal == b.intVal {
		return a
	}

	if a.kind == cBool && a.boolVal == b.boolVal {
		return a
	}

	return cBotFact()
}

// dfState captures the abstract state at one program point.
type dfState struct {
	nil  map[string]nilFact
	cnst map[string]constFact
	dead bool // true means control-flow can't reach this point
}

func newDFState() *dfState {
	return &dfState{
		nil:  map[string]nilFact{},
		cnst: map[string]constFact{},
	}
}

func (s *dfState) clone() *dfState {
	out := newDFState()
	out.dead = s.dead

	for k, v := range s.nil {
		out.nil[k] = v
	}

	for k, v := range s.cnst {
		out.cnst[k] = v
	}

	return out
}

func mergeStates(a, b *dfState) *dfState {
	if a == nil || a.dead {
		return b
	}

	if b == nil || b.dead {
		return a
	}

	out := newDFState()

	for k, v := range a.nil {
		if w, ok := b.nil[k]; ok {
			out.nil[k] = mergeNil(v, w)
		} else {
			out.nil[k] = v
		}
	}

	for k, v := range b.nil {
		if _, ok := out.nil[k]; !ok {
			out.nil[k] = v
		}
	}

	for k, v := range a.cnst {
		if w, ok := b.cnst[k]; ok {
			out.cnst[k] = mergeConst(v, w)
		} else {
			out.cnst[k] = v
		}
	}

	for k, v := range b.cnst {
		if _, ok := out.cnst[k]; !ok {
			out.cnst[k] = v
		}
	}

	return out
}

// statesEqual reports whether two states agree on every tracked variable.
// Used to detect fixpoint convergence in loops.
func statesEqual(a, b *dfState) bool {
	if a == nil || b == nil {
		return a == b
	}

	if a.dead != b.dead {
		return false
	}

	if len(a.nil) != len(b.nil) || len(a.cnst) != len(b.cnst) {
		return false
	}

	for k, v := range a.nil {
		if w, ok := b.nil[k]; !ok || w != v {
			return false
		}
	}

	for k, v := range a.cnst {
		w, ok := b.cnst[k]
		if !ok {
			return false
		}

		if w.kind != v.kind || w.intVal != v.intVal || w.boolVal != v.boolVal {
			return false
		}
	}

	return true
}

// runDataflow runs the intraprocedural dataflow pass over every top-level
// FuncDecl (and struct method). Each function gets its own analysis run.
func (cg *CodeGen) runDataflow(prog *ast.Program) {
	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			cg.dfAnalyzeFunc(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.dfAnalyzeFunc(m)
			}
		}
	}
}

func (cg *CodeGen) dfAnalyzeFunc(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Entry state: parameters are BOTTOM (could be anything from caller).
	st := newDFState()

	for _, p := range fn.Params {
		if p.Name != "" && p.Name != "_" {
			st.nil[p.Name] = nilBottom
			st.cnst[p.Name] = cBotFact()
		}
	}

	cg.dfWalkAny(fn.Body, st)
}

// dfWalkAny dispatches a node to dfWalkBlock for *Block or dfWalkStmt for
// any other statement-shaped node. Used at function-body roots that are
// typed as ast.Node so they can carry expressions or where-clauses.
func (cg *CodeGen) dfWalkAny(n ast.Node, st *dfState) *dfState {
	if n == nil {
		return st
	}

	if b, ok := n.(*ast.Block); ok {
		return cg.dfWalkBlock(b, st)
	}

	return cg.dfWalkStmt(n, st)
}

// dfWalkBlock threads state through a block's statements. Returns the
// outgoing state, or a dead state if every path terminated.
func (cg *CodeGen) dfWalkBlock(b *ast.Block, st *dfState) *dfState {
	if b == nil {
		return st
	}

	for _, s := range b.Stmts {
		if st == nil || st.dead {
			return st
		}

		st = cg.dfWalkStmt(s, st)
	}

	return st
}

func (cg *CodeGen) dfWalkStmt(stmt ast.Node, st *dfState) *dfState {
	switch v := stmt.(type) {
	case *ast.VarDecl:
		cg.dfCheckExpr(v.Value, st)

		st = st.clone()
		st.nil[v.Name] = cg.dfNilOf(v.Value, st)
		st.cnst[v.Name] = cg.dfEval(v.Value, st)

		return st

	case *ast.AssignStmt:
		cg.dfCheckExpr(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			st = st.clone()
			st.nil[id.Name] = cg.dfNilOf(v.Value, st)
			st.cnst[id.Name] = cg.dfEval(v.Value, st)
		}

		return st

	case *ast.AugAssignStmt:
		cg.dfCheckExpr(v.Value, st)
		// Augmented assigns invalidate the variable's known value: in the
		// general case x += y is x = x + y; the new value is BOTTOM unless
		// we did full arithmetic propagation.
		if id, ok := v.Target.(*ast.Identifier); ok {
			st = st.clone()
			st.cnst[id.Name] = cBotFact()
			st.nil[id.Name] = nilBottom
		}

		return st

	case *ast.ExprStmt:
		cg.dfCheckExpr(v.Expr, st)

		return st

	case *ast.EchoStmt:
		cg.dfCheckExpr(v.Value, st)

		return st

	case *ast.ReturnStmt:
		if v.Value != nil {
			cg.dfCheckExpr(v.Value, st)
		}

		dead := st.clone()
		dead.dead = true

		return dead

	case *ast.IfStmt:
		return cg.dfWalkIf(v, st)

	case *ast.ForStmt:
		return cg.dfWalkLoop(v.Init, v.Cond, v.Post, v.Body, st)

	case *ast.MatchStmt:
		return cg.dfWalkMatch(v, st)

	case *ast.Block:
		return cg.dfWalkBlock(v, st)

	case *ast.DeferStmt:
		// Defers run on every exit; for the analysis treat them as a
		// side-effecting statement at the defer site.
		cg.dfCheckExpr(v.Call, st)

		return st

	case *ast.BreakStmt:
		dead := st.clone()
		dead.dead = true

		return dead

	default:
		return st
	}
}

// dfWalkIf processes an `if cond { then } elif (...) ... else { else }`.
// It folds the condition under the current state and emits a flow-sensitive
// bool-analysis warning when it's known. Branch-narrowing refines the
// per-arm state for the common `p == nil` / `p != nil` shape.
func (cg *CodeGen) dfWalkIf(s *ast.IfStmt, st *dfState) *dfState {
	cg.dfCheckExpr(s.Cond, st)

	// Flow-sensitive constant condition warning.
	if v := cg.dfEval(s.Cond, st); v.kind == cBool {
		// The non-flow-sensitive bool-analysis already fires on conditions
		// that fold without state context. Only emit here when the condition
		// folds *because* of flow facts that the AST-level fold misses.
		base := cg.tryFoldExpr(s.Cond)
		if base.kind != foldBool {
			cg.warn(DiagBoolAnalysis, s.Pos(),
				"if condition is always %v under current control flow", v.boolVal)
		}
	}

	thenSt, elseSt := narrowOnCond(s.Cond, st)

	thenSt = cg.dfWalkBlock(s.Then, thenSt)

	for _, ei := range s.ElseIfs {
		cg.dfCheckExpr(ei.Cond, elseSt)

		var inner *dfState

		inner, elseSt = narrowOnCond(ei.Cond, elseSt)
		inner = cg.dfWalkBlock(ei.Body, inner)
		thenSt = mergeStates(thenSt, inner)
	}

	if s.Else != nil {
		elseSt = cg.dfWalkBlock(s.Else, elseSt)
	}

	return mergeStates(thenSt, elseSt)
}

// dfWalkLoop runs the body to fixpoint (bounded). Init runs once; cond is
// re-checked each iteration; post runs at end of body. We cap iterations
// to avoid runaway loops on adversarial input.
func (cg *CodeGen) dfWalkLoop(init, cond, post ast.Node, body *ast.Block, st *dfState) *dfState {
	if init != nil {
		st = cg.dfWalkStmt(init, st)
	}

	const maxIter = 4

	prev := st

	for i := 0; i < maxIter; i++ {
		if cond != nil {
			cg.dfCheckExpr(cond, prev)
		}

		bodySt := prev.clone()
		bodySt = cg.dfWalkBlock(body, bodySt)

		if post != nil && bodySt != nil && !bodySt.dead {
			bodySt = cg.dfWalkStmt(post, bodySt)
		}

		merged := mergeStates(prev, bodySt)
		if statesEqual(prev, merged) {
			return merged
		}

		prev = merged
	}

	// Did not converge in maxIter; return BOTTOM-ised state for everything
	// the loop body might touch. Conservative: every tracked variable goes
	// to BOTTOM.
	out := newDFState()
	for k := range prev.nil {
		out.nil[k] = nilBottom
	}

	for k := range prev.cnst {
		out.cnst[k] = cBotFact()
	}

	return out
}

func (cg *CodeGen) dfWalkMatch(s *ast.MatchStmt, st *dfState) *dfState {
	cg.dfCheckExpr(s.Expr, st)

	var merged *dfState

	for _, c := range s.Cases {
		armSt := st.clone()
		armSt = cg.dfWalkStmt(c.Body, armSt)
		merged = mergeStates(merged, armSt)
	}

	if s.Default != nil {
		armSt := st.clone()
		armSt = cg.dfWalkStmt(s.Default, armSt)
		merged = mergeStates(merged, armSt)
	}

	if merged == nil {
		return st
	}

	return merged
}

// dfCheckExpr looks for nil-deref / nil-field-access patterns inside expr
// using the current state. The walk is non-state-modifying.
func (cg *CodeGen) dfCheckExpr(expr ast.Node, st *dfState) {
	if expr == nil || st == nil {
		return
	}

	walkAST(expr, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.DerefExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"dereferencing %q which is statically nil at this point", id.Name)
				}
			}
		case *ast.FieldAccess:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"field access on %q which is statically nil at this point", id.Name)
				}
			}
		}
	})
}

// dfNilOf returns the nil-state of an expression evaluated under st.
func (cg *CodeGen) dfNilOf(expr ast.Node, st *dfState) nilFact {
	if expr == nil {
		return nilTop
	}

	switch e := expr.(type) {
	case *ast.NilLit:
		return nilIsNil
	case *ast.AddressOfExpr:
		return nilNonNil
	case *ast.Identifier:
		if v, ok := st.nil[e.Name]; ok {
			return v
		}

		return nilBottom
	}

	return nilBottom
}

// dfEval evaluates expr against the abstract state, returning a constFact.
// Recursively folds BinExpr/UnaryExpr using flow-sensitive identifier
// lookups, falling through to tryFoldExpr for everything else.
func (cg *CodeGen) dfEval(expr ast.Node, st *dfState) constFact {
	if expr == nil {
		return cTopFact()
	}

	switch e := expr.(type) {
	case *ast.IntLit:
		return cIntFact(e.Value)
	case *ast.BoolLit:
		return cBoolFact(e.Value)
	case *ast.Identifier:
		if v, ok := st.cnst[e.Name]; ok {
			return v
		}
	case *ast.BinExpr:
		l := cg.dfEval(e.Left, st)
		r := cg.dfEval(e.Right, st)

		return foldBinConst(e.Op, l, r)
	case *ast.UnaryExpr:
		v := cg.dfEval(e.Expr, st)

		return foldUnaryConst(e.Op, v)
	}

	v := cg.tryFoldExpr(expr)
	switch v.kind {
	case foldInt:
		return cIntFact(v.intVal)
	case foldBool:
		return cBoolFact(v.boolVal)
	case foldUnknown, foldAtom:
		return cBotFact()
	}

	return cBotFact()
}

// foldBinConst evaluates `a op b` when both operands are concrete
// constants, returning BOTTOM otherwise.
func foldBinConst(op string, a, b constFact) constFact {
	if a.kind == cBottom || b.kind == cBottom || a.kind == cTop || b.kind == cTop {
		return cBotFact()
	}

	if a.kind == cInt && b.kind == cInt {
		switch op {
		case "+":
			return cIntFact(a.intVal + b.intVal)
		case "-":
			return cIntFact(a.intVal - b.intVal)
		case "*":
			return cIntFact(a.intVal * b.intVal)
		case "/":
			if b.intVal == 0 {
				return cBotFact()
			}

			return cIntFact(a.intVal / b.intVal)
		case "%":
			if b.intVal == 0 {
				return cBotFact()
			}

			return cIntFact(a.intVal % b.intVal)
		case "==":
			return cBoolFact(a.intVal == b.intVal)
		case "!=":
			return cBoolFact(a.intVal != b.intVal)
		case "<":
			return cBoolFact(a.intVal < b.intVal)
		case "<=":
			return cBoolFact(a.intVal <= b.intVal)
		case ">":
			return cBoolFact(a.intVal > b.intVal)
		case ">=":
			return cBoolFact(a.intVal >= b.intVal)
		}
	}

	if a.kind == cBool && b.kind == cBool {
		switch op {
		case "==":
			return cBoolFact(a.boolVal == b.boolVal)
		case "!=":
			return cBoolFact(a.boolVal != b.boolVal)
		case "&&":
			return cBoolFact(a.boolVal && b.boolVal)
		case "||":
			return cBoolFact(a.boolVal || b.boolVal)
		}
	}

	return cBotFact()
}

func foldUnaryConst(op string, v constFact) constFact {
	if v.kind == cInt && op == "-" {
		return cIntFact(-v.intVal)
	}

	if v.kind == cBool && (op == "!" || op == "not") {
		return cBoolFact(!v.boolVal)
	}

	return cBotFact()
}

// narrowOnCond examines the test in `if cond { then } else { else }` and
// returns the (then, else) abstract states refined with whatever facts the
// branch establishes. The supported shapes are:
//
//	p == nil  -> then: p is NIL, else: p is NON_NIL
//	p != nil  -> then: p is NON_NIL, else: p is NIL
//
// Anything else falls back to the unrefined incoming state.
func narrowOnCond(cond ast.Node, st *dfState) (thenSt, elseSt *dfState) {
	thenSt = st.clone()
	elseSt = st.clone()

	bin, ok := cond.(*ast.BinExpr)
	if !ok {
		return
	}

	switch bin.Op {
	case "==", "!=":
	default:
		return
	}

	var name string

	if id, ok := bin.Left.(*ast.Identifier); ok {
		if _, isNil := bin.Right.(*ast.NilLit); isNil {
			name = id.Name
		}
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		if _, isNil := bin.Left.(*ast.NilLit); isNil {
			name = id.Name
		}
	}

	if name == "" {
		return
	}

	if bin.Op == "==" {
		thenSt.nil[name] = nilIsNil
		elseSt.nil[name] = nilNonNil
	} else {
		thenSt.nil[name] = nilNonNil
		elseSt.nil[name] = nilIsNil
	}

	return
}
