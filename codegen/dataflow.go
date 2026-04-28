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
//   - Nil tracking for pointer / RC-typed locals, an Andersen's-style
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
	"math/big"
	"strconv"

	"github.com/Azer0s/tin/ast"
)

// floatPair holds a float-valued constant in two parallel forms: the IEEE
// 754 result (what runtime arithmetic produces) and the exact rational
// (what arbitrary-precision arithmetic produces). Used to detect comparisons
// where the two disagree, like 0.1 + 0.2 == 0.3.
type floatPair struct {
	ieee  float64
	exact *big.Rat
}

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

// interval is a closed integer range [lo, hi]. `set` distinguishes "no
// information" from a real range. The lattice meet is union; narrowing
// happens through dedicated helpers driven by branch conditions.
type interval struct {
	lo, hi int64
	set    bool
}

// dfState captures the abstract state at one program point.
type dfState struct {
	nil    map[string]nilFact
	cnst   map[string]constFact
	freed  map[string]bool       // variable was passed to deinit()
	intv   map[string]interval   // integer interval per variable
	floats map[string]*floatPair // exact + IEEE float pair per variable
	dead   bool                  // true means control-flow can't reach this point
}

func newDFState() *dfState {
	return &dfState{
		nil:    map[string]nilFact{},
		cnst:   map[string]constFact{},
		freed:  map[string]bool{},
		intv:   map[string]interval{},
		floats: map[string]*floatPair{},
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

	for k, v := range s.freed {
		out.freed[k] = v
	}

	for k, v := range s.intv {
		out.intv[k] = v
	}

	for k, v := range s.floats {
		out.floats[k] = v
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
	// freed: if either branch freed the var, treat as freed at the join.
	// This is the "may-be-freed" semantics that catches use-after-deinit
	// even when only one branch did the deinit.
	for k, v := range a.freed {
		if v {
			out.freed[k] = true
		}
	}

	for k, v := range b.freed {
		if v {
			out.freed[k] = true
		}
	}
	// Interval: union the two ranges. A variable known only in one branch
	// is unknown after the join (union with the implicit unknown side).
	for k, v := range a.intv {
		if w, ok := b.intv[k]; ok {
			out.intv[k] = unionInterval(v, w)
		}
	}
	// Floats: keep only when both branches agree on the exact rational.
	// Disagreement means the join can't honestly claim either value.
	for k, v := range a.floats {
		if w, ok := b.floats[k]; ok && v.exact.Cmp(w.exact) == 0 && v.ieee == w.ieee {
			out.floats[k] = v
		}
	}

	return out
}

// unionInterval is the lattice meet for the interval analysis: the
// smallest range containing both inputs. Either operand being unset means
// the result is unset (we lost information).
func unionInterval(a, b interval) interval {
	if !a.set || !b.set {
		return interval{}
	}

	out := interval{set: true, lo: a.lo, hi: a.hi}
	if b.lo < out.lo {
		out.lo = b.lo
	}

	if b.hi > out.hi {
		out.hi = b.hi
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

	if len(a.intv) != len(b.intv) {
		return false
	}

	for k, v := range a.intv {
		w, ok := b.intv[k]
		if !ok || w != v {
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
	// Their interval is seeded from the declared type, so a `u8` parameter
	// starts in [0, 255] - that's how `u8 < 0` falls out as always-false
	// without any further analysis.
	st := newDFState()

	for _, p := range fn.Params {
		if p.Name == "" || p.Name == "_" {
			continue
		}

		st.nil[p.Name] = nilBottom
		st.cnst[p.Name] = cBotFact()

		if iv := intervalForTinType(p.Type); iv.set {
			st.intv[p.Name] = iv
		}
	}

	cg.dfWalkAny(fn.Body, st)
}

// intervalForTinType returns the integer range that a variable of type t
// is guaranteed to fall in. Returns the unset interval for non-integer or
// 64-bit-unsigned types (the latter doesn't fit in a Go int64).
func intervalForTinType(t ast.TypeExpr) interval {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return interval{}
	}

	switch st.Name {
	case "i8":
		return interval{set: true, lo: -128, hi: 127}
	case "u8", "byte", "char":
		return interval{set: true, lo: 0, hi: 255}
	case "i16":
		return interval{set: true, lo: -32768, hi: 32767}
	case "u16":
		return interval{set: true, lo: 0, hi: 65535}
	case "i32":
		return interval{set: true, lo: -2147483648, hi: 2147483647}
	case "u32":
		return interval{set: true, lo: 0, hi: 4294967295}
	case "i64":
		return interval{set: true, lo: -9223372036854775808, hi: 9223372036854775807}
	}

	return interval{}
}

// dfIntervalForBinding chooses the interval for `let <name> [type] = value`.
// Prefers the value's interval (it's a tighter bound), falling back to the
// declared type's range when the value's interval is unknown.
func (cg *CodeGen) dfIntervalForBinding(declType ast.TypeExpr, value ast.Node, st *dfState) interval {
	if iv := cg.intervalOf(value, st); iv.set {
		return iv
	}

	return intervalForTinType(declType)
}

// intervalOf returns the abstract interval of expr under st.
func (cg *CodeGen) intervalOf(expr ast.Node, st *dfState) interval {
	if expr == nil {
		return interval{}
	}

	switch e := expr.(type) {
	case *ast.IntLit:
		return interval{set: true, lo: e.Value, hi: e.Value}
	case *ast.Identifier:
		if v, ok := st.intv[e.Name]; ok {
			return v
		}
	}

	return interval{}
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

		if iv := cg.dfIntervalForBinding(v.Type, v.Value, st); iv.set {
			st.intv[v.Name] = iv
		}

		if fp := dfFoldFloat(v.Value, st); fp != nil {
			st.floats[v.Name] = fp
		}

		return st

	case *ast.AssignStmt:
		cg.dfCheckExpr(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			st = st.clone()
			st.nil[id.Name] = cg.dfNilOf(v.Value, st)
			st.cnst[id.Name] = cg.dfEval(v.Value, st)
			delete(st.freed, id.Name) // reassign clears freed state

			if iv := cg.intervalOf(v.Value, st); iv.set {
				st.intv[id.Name] = iv
			} else {
				delete(st.intv, id.Name)
			}

			if fp := dfFoldFloat(v.Value, st); fp != nil {
				st.floats[id.Name] = fp
			} else {
				delete(st.floats, id.Name)
			}
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

			delete(st.intv, id.Name)
			delete(st.floats, id.Name)
		}

		return st

	case *ast.ExprStmt:
		if name, ok := isDeinitCall(v.Expr); ok {
			st = st.clone()

			if st.freed[name] {
				cg.warn(DiagDoubleDeinit, v.Expr.Pos(),
					"deinit on %q which has already been deinitialised on this path", name)
			}

			st.freed[name] = true

			return st
		}

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

	cg.checkImpossibleCmp(s.Cond, st)

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

// dfCheckExpr looks for nil-deref, nil-field-access, and use-after-deinit
// patterns inside expr using the current state. The walk is non-state-
// modifying.
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

				if st.freed[id.Name] {
					cg.warn(DiagUseAfterDeinit, e.Pos(),
						"dereference of %q after deinit on this path", id.Name)
				}
			}
		case *ast.FieldAccess:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"field access on %q which is statically nil at this point", id.Name)
				}

				if st.freed[id.Name] {
					cg.warn(DiagUseAfterDeinit, e.Pos(),
						"field access on %q after deinit on this path", id.Name)
				}
			}
		case *ast.BinExpr:
			cg.dfCheckFloatPrecision(e, st)
		}
	})
}

// dfCheckFloatPrecision flags `==` / `!=` whose two sides are float
// expressions that disagree under IEEE 754 but agree under exact
// arithmetic (the 0.1 + 0.2 == 0.3 trap), threading let-bindings through
// the dataflow state so it works on variables, not just literals.
func (cg *CodeGen) dfCheckFloatPrecision(e *ast.BinExpr, st *dfState) {
	if e.Op != "==" && e.Op != "!=" {
		return
	}

	lhs := dfFoldFloat(e.Left, st)
	if lhs == nil {
		return
	}

	rhs := dfFoldFloat(e.Right, st)
	if rhs == nil {
		return
	}

	floatEq := lhs.ieee == rhs.ieee
	exactEq := lhs.exact.Cmp(rhs.exact) == 0

	if floatEq == exactEq {
		return
	}

	ieeeResult := floatEq
	exactResult := exactEq

	if e.Op == "!=" {
		ieeeResult = !ieeeResult
		exactResult = !exactResult
	}

	cg.warn(DiagFloatPrecision, e.Pos(),
		"%q evaluates to %v under IEEE 754 but %v under exact arithmetic; "+
			"use `abs(a - b) < eps` instead",
		e.Op, ieeeResult, exactResult)
}

// dfFoldFloat folds an expression to a (IEEE, exact) float pair under the
// given state. Handles FloatLit, IntLit, the four arithmetic ops, unary
// minus, and Identifier (via the state's tracked floats). Returns nil for
// anything we can't statically resolve.
func dfFoldFloat(n ast.Node, st *dfState) *floatPair {
	switch e := n.(type) {
	case *ast.FloatLit:
		return floatPairFromLit(e.Value)
	case *ast.IntLit:
		// Integer literals participate in mixed expressions like `x + 1`.
		return &floatPair{
			ieee:  float64(e.Value),
			exact: new(big.Rat).SetInt64(e.Value),
		}
	case *ast.Identifier:
		if fp, ok := st.floats[e.Name]; ok {
			return fp
		}
	case *ast.UnaryExpr:
		if e.Op != "-" {
			return nil
		}

		inner := dfFoldFloat(e.Expr, st)
		if inner == nil {
			return nil
		}

		return &floatPair{
			ieee:  -inner.ieee,
			exact: new(big.Rat).Neg(inner.exact),
		}
	case *ast.BinExpr:
		l := dfFoldFloat(e.Left, st)
		if l == nil {
			return nil
		}

		r := dfFoldFloat(e.Right, st)
		if r == nil {
			return nil
		}

		out := new(big.Rat)

		switch e.Op {
		case "+":
			return &floatPair{ieee: l.ieee + r.ieee, exact: out.Add(l.exact, r.exact)}
		case "-":
			return &floatPair{ieee: l.ieee - r.ieee, exact: out.Sub(l.exact, r.exact)}
		case "*":
			return &floatPair{ieee: l.ieee * r.ieee, exact: out.Mul(l.exact, r.exact)}
		case "/":
			if r.ieee == 0 || r.exact.Sign() == 0 {
				return nil
			}

			return &floatPair{ieee: l.ieee / r.ieee, exact: out.Quo(l.exact, r.exact)}
		}
	}

	return nil
}

// floatPairFromLit builds a (IEEE, exact) pair from a float literal value.
// Routes through shortest-decimal so 0.1 maps to the exact 1/10 rather
// than the IEEE-rounded approximation.
func floatPairFromLit(f float64) *floatPair {
	r := new(big.Rat)

	s := strconv.FormatFloat(f, 'g', -1, 64)
	if _, ok := r.SetString(s); !ok {
		return nil
	}

	return &floatPair{ieee: f, exact: r}
}

// isDeinitCall returns the variable name being deinitialised when expr
// matches `x.deinit()` or `(*x).deinit()`. The second form is what shows
// up after parsing the typical `(*ptr).deinit()` syntax.
func isDeinitCall(expr ast.Node) (string, bool) {
	c, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}

	fa, ok := c.Func.(*ast.FieldAccess)
	if !ok || fa.Field != "deinit" {
		return "", false
	}

	switch r := fa.Expr.(type) {
	case *ast.Identifier:
		return r.Name, true
	case *ast.DerefExpr:
		if id, ok := r.Expr.(*ast.Identifier); ok {
			return id.Name, true
		}
	}

	return "", false
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

	// Nil narrowing for `p == nil` / `p != nil`.
	if bin.Op == "==" || bin.Op == "!=" {
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

		if name != "" {
			if bin.Op == "==" {
				thenSt.nil[name] = nilIsNil
				elseSt.nil[name] = nilNonNil
			} else {
				thenSt.nil[name] = nilNonNil
				elseSt.nil[name] = nilIsNil
			}

			return
		}
	}

	// Integer-interval narrowing: `x op c` (or `c op x`) where one side is
	// an identifier we track and the other a constant.
	switch bin.Op {
	case "<", "<=", ">", ">=", "==", "!=":
	default:
		return
	}

	var (
		name string
		c    int64
		op   string
	)

	if id, ok := bin.Left.(*ast.Identifier); ok {
		iv := constIntOf(bin.Right)
		if !iv.set {
			return
		}

		name = id.Name
		c = iv.lo
		op = bin.Op
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		iv := constIntOf(bin.Left)
		if !iv.set {
			return
		}

		name = id.Name
		c = iv.lo
		op = flipOp(bin.Op)
	}

	if name == "" {
		return
	}

	cur, ok := st.intv[name]
	if !ok || !cur.set {
		return
	}

	thenIv, elseIv := narrowIntervalCmp(cur, op, c)
	if thenIv.set {
		thenSt.intv[name] = thenIv
	}

	if elseIv.set {
		elseSt.intv[name] = elseIv
	}

	return
}

// constIntOf returns the IntLit value of an expression as a single-point
// interval, or unset if it's not a literal int.
func constIntOf(n ast.Node) interval {
	if il, ok := n.(*ast.IntLit); ok {
		return interval{set: true, lo: il.Value, hi: il.Value}
	}

	return interval{}
}

// flipOp swaps a comparison operator so that `c op x` becomes `x op' c`.
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}

	return op
}

// narrowIntervalCmp refines the interval of `x` given the branch
// `x op c` is taken (then) or not taken (else). Returns the refined
// (then, else) intervals; an unset interval means "no information".
func narrowIntervalCmp(x interval, op string, c int64) (thenIv, elseIv interval) {
	if !x.set {
		return interval{}, interval{}
	}

	switch op {
	case "<":
		thenIv = clipInterval(x, x.lo, c-1)
		elseIv = clipInterval(x, c, x.hi)
	case "<=":
		thenIv = clipInterval(x, x.lo, c)
		elseIv = clipInterval(x, c+1, x.hi)
	case ">":
		thenIv = clipInterval(x, c+1, x.hi)
		elseIv = clipInterval(x, x.lo, c)
	case ">=":
		thenIv = clipInterval(x, c, x.hi)
		elseIv = clipInterval(x, x.lo, c-1)
	case "==":
		thenIv = clipInterval(x, c, c)
		elseIv = x // can't refine the else side from a single point
	case "!=":
		thenIv = x
		elseIv = clipInterval(x, c, c)
	}

	return
}

// checkImpossibleCmp emits a warning when an integer comparison evaluates
// to a constant under the current interval state. Catches:
//
//	let x u8 = ... ; if x < 0:        // u8 is in [0,255], always false
//	if x >= 5 && x < 5:               // narrowed RHS is empty
//	if x == 100:  /* x ∈ [0, 50] */   // never holds
//
// For `A && B` the right conjunct is checked under the state where A is
// known to hold, so a contradiction with the narrowed state (the second
// example above) gets caught.
func (cg *CodeGen) checkImpossibleCmp(cond ast.Node, st *dfState) {
	cg.checkImpossibleCmpIn(cond, st)
}

func (cg *CodeGen) checkImpossibleCmpIn(cond ast.Node, st *dfState) *dfState {
	if bin, ok := cond.(*ast.BinExpr); ok {
		switch bin.Op {
		case "&&":
			leftThen := cg.checkImpossibleCmpIn(bin.Left, st)
			if leftThen == nil {
				leftThen = st
			}

			cg.checkImpossibleCmpIn(bin.Right, leftThen)

			return leftThen
		case "||":
			cg.checkImpossibleCmpIn(bin.Left, st)
			cg.checkImpossibleCmpIn(bin.Right, st)

			return st
		}
	}

	cg.checkSingleCmp(cond, st)

	thenSt, _ := narrowOnCond(cond, st)

	return thenSt
}

// checkSingleCmp handles a non-compound comparison. Emits the
// impossible-range warning if the result is determinate.
func (cg *CodeGen) checkSingleCmp(cond ast.Node, st *dfState) {
	bin, ok := cond.(*ast.BinExpr)
	if !ok {
		return
	}

	switch bin.Op {
	case "<", "<=", ">", ">=", "==", "!=":
	default:
		return
	}

	var (
		name string
		c    int64
		op   string
	)

	if id, ok := bin.Left.(*ast.Identifier); ok {
		iv := constIntOf(bin.Right)
		if !iv.set {
			return
		}

		name = id.Name
		c = iv.lo
		op = bin.Op
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		iv := constIntOf(bin.Left)
		if !iv.set {
			return
		}

		name = id.Name
		c = iv.lo
		op = flipOp(bin.Op)
	} else {
		return
	}

	cur, ok := st.intv[name]
	if !ok || !cur.set {
		return
	}

	if always, val := cmpAlwaysHolds(cur, op, c); always {
		cg.warn(DiagImpossibleRange, bin.Pos(),
			"comparison %q is always %v: %s ∈ [%d, %d]",
			op, val, name, cur.lo, cur.hi)
	}
}

// cmpAlwaysHolds reports whether `x op c` evaluates to a constant given
// x's interval. Returns (true, value) when fully determined.
func cmpAlwaysHolds(x interval, op string, c int64) (always, value bool) {
	switch op {
	case "<":
		if x.hi < c {
			return true, true
		}

		if x.lo >= c {
			return true, false
		}
	case "<=":
		if x.hi <= c {
			return true, true
		}

		if x.lo > c {
			return true, false
		}
	case ">":
		if x.lo > c {
			return true, true
		}

		if x.hi <= c {
			return true, false
		}
	case ">=":
		if x.lo >= c {
			return true, true
		}

		if x.hi < c {
			return true, false
		}
	case "==":
		if x.lo == c && x.hi == c {
			return true, true
		}

		if c < x.lo || c > x.hi {
			return true, false
		}
	case "!=":
		if c < x.lo || c > x.hi {
			return true, true
		}

		if x.lo == c && x.hi == c {
			return true, false
		}
	}

	return false, false
}

// clipInterval returns x ∩ [lo, hi], or unset if the result is empty.
func clipInterval(x interval, lo, hi int64) interval {
	if !x.set {
		return interval{}
	}

	if lo < x.lo {
		lo = x.lo
	}

	if hi > x.hi {
		hi = x.hi
	}

	if lo > hi {
		return interval{}
	}

	return interval{set: true, lo: lo, hi: hi}
}
