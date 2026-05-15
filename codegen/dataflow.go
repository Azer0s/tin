package codegen

// Intraprocedural dataflow analysis. The pass is syntax-directed: it walks
// the AST recursively, threading an abstract state through each statement
// and merging at join points. Loops iterate to a small fixed bound.
//
// The lattice combines three pieces:
//
//   - SCCP-style constant propagation for integer and boolean locals.
//     Lattice elements are TOP (uninitialized), CONST(c), or BOTTOM (varying).
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
	"fmt"
	"math"
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
	nilTop    nilFact = iota // unreachable / uninitialized
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

// interval is a strided integer range [lo, hi] step `stride` (Reps et
// al.'s strided intervals). Members are {lo, lo+stride, lo+2*stride,
// ..., hi} with the invariant `(hi - lo) % stride == 0` whenever
// stride > 0. `set` distinguishes "no information" from a real range.
//
// stride conventions:
//
//	stride == 0 : singleton or stride-unconstrained (lo == hi acts as
//	              a single value; treated as gcd-identity element by
//	              unionInterval so a singleton does not pin down the
//	              stride of an unrelated peer).
//	stride == 1 : ordinary contiguous range.
//	stride >  1 : every (stride-1) values out of `stride` are excluded;
//	              used to track loop induction variables stepped by a
//	              constant != 1, so `for i = 0; i < n; i += k` lets
//	              the analyzer prove `i % k == 0` always-true inside
//	              the body.
//
// The lattice meet (unionInterval) widens bounds and shrinks stride by
// gcd. Narrowing happens through dedicated helpers driven by branch
// conditions.
type interval struct {
	lo, hi int64
	stride int64
	set    bool
}

// singletonIv returns a strided interval containing exactly one value.
// stride=0 lets the value join freely with any other interval without
// pinning down a stride.
func singletonIv(v int64) interval {
	return interval{set: true, lo: v, hi: v, stride: 0}
}

// rangeIv returns the contiguous range [lo, hi] (stride 1).
func rangeIv(lo, hi int64) interval {
	if lo > hi {
		return interval{}
	}

	if lo == hi {
		return singletonIv(lo)
	}

	return interval{set: true, lo: lo, hi: hi, stride: 1}
}

// effectiveStride returns 1 for arbitrary ranges and stride 0 for
// singletons -- the value to use in arithmetic ops where stride 0 means
// "no constraint".
func (iv interval) effectiveStride() int64 {
	if !iv.set || iv.lo == iv.hi {
		return 0
	}

	if iv.stride <= 0 {
		return 1
	}

	return iv.stride
}

// gcd64 returns greatest common divisor on absolute values. gcd(0, x) =
// |x|; gcd(0, 0) = 0. Used for stride lubs.
func gcd64(a, b int64) int64 {
	if a < 0 {
		a = -a
	}

	if b < 0 {
		b = -b
	}

	for b != 0 {
		a, b = b, a%b
	}

	return a
}

// dfState captures the abstract state at one program point.
type dfState struct {
	nil    map[string]nilFact
	cnst   map[string]constFact
	freed  map[string]bool       // variable was passed to deinit()
	intv   map[string]interval   // integer interval per variable
	floats map[string]*floatPair // exact + IEEE float pair per variable
	// uninit holds names of locals declared without an initializer that
	// have NOT yet been explicitly assigned on this path. A read while
	// the name is in this set fires DiagUseBeforeAssign. Tin
	// auto-zero-inits primitives, so the read is well-defined; the
	// warning is about programmer intent, not memory safety.
	uninit map[string]bool
	// notZero holds integer names proven != 0 on this path via a
	// guard like `if x != 0:` or `if x > 0:`.  Used by
	// dfCheckUncheckedDiv to silence pedantic warnings on guarded
	// divisions where the interval-narrowing can't represent the
	// punctured range (i.e. arbitrary i64 minus the singleton 0).
	// Cleared on assignment to the name.
	notZero map[string]bool
	// boundsChecked holds integer names whose upper bound has been
	// established on this path -- either via `if i < positiveConst:`
	// (narrows interval, also recorded here), `if i < len(arr):`
	// (recorded against the specific arr name), or by being the
	// induction variable of a `for i in arr` loop.  Used by
	// dfCheckUncheckedIndex to silence pedantic warnings on guarded
	// array accesses.  A name is in the set iff *some* upper bound was
	// established; we don't track WHICH array, only that the user
	// proved an upper bound.  Cleared on assignment.
	boundsChecked map[string]bool
	// types tracks the declared AST type of every binding visible on
	// this path -- parameters seeded at fn entry, locals seeded by
	// VarDecl.  Used by dfCheckUncheckedIndex to distinguish built-in
	// array indexing (i64 bounds-check semantics) from user `::index`
	// overloads (ok-destructure semantics).  Not used for value
	// inference -- types are static, not flow-sensitive -- so the map
	// is shared by clone (same pointers) rather than copied.
	types map[string]ast.TypeExpr
	// manualAlloc tracks names bound to `mem::malloc/calloc/realloc`
	// results.  Live: held but not yet freed on this path.  Freed:
	// `mem::free` consumed it.  Escaped: the pointer left the fn
	// (returned, stored, passed to another fn) so the check stops
	// tracking it.  Drives -Wmanual-alloc-leak (on scope-exit),
	// -Wmanual-double-free, -Wmanual-use-after-free.  Counterpart to
	// `freed` (ARC deinit) but tracks the C-interop malloc/free
	// world separately so the two diagnostics don't cross-fire.
	manualAlloc map[string]manualAllocState
	dead        bool // true means control-flow can't reach this point
}

// manualAllocState is the per-name lattice for mem::malloc/free
// tracking.  Live < Freed < Escaped (Bottom): any join collapses to
// the dominant state, but a Freed path "wins" over a missing free
// (MAY-be-leaked semantics is captured by Live surviving to a scope
// exit where it would have been Freed had ALL paths freed).
type manualAllocState int

const (
	manualAllocLive    manualAllocState = 1
	manualAllocFreed   manualAllocState = 2
	manualAllocEscaped manualAllocState = 3
)

func newDFState() *dfState {
	return &dfState{
		nil:           map[string]nilFact{},
		cnst:          map[string]constFact{},
		freed:         map[string]bool{},
		intv:          map[string]interval{},
		floats:        map[string]*floatPair{},
		uninit:        map[string]bool{},
		notZero:       map[string]bool{},
		boundsChecked: map[string]bool{},
		types:         map[string]ast.TypeExpr{},
		manualAlloc:   map[string]manualAllocState{},
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

	for k, v := range s.uninit {
		out.uninit[k] = v
	}

	for k, v := range s.notZero {
		out.notZero[k] = v
	}

	for k, v := range s.boundsChecked {
		out.boundsChecked[k] = v
	}

	for k, v := range s.types {
		out.types[k] = v
	}

	for k, v := range s.manualAlloc {
		out.manualAlloc[k] = v
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
	// Uninit: union (i.e., "definitely assigned" is intersection). A name
	// is still uninit at the join if either incoming path hadn't yet
	// assigned to it -- a later read could come from that path.
	for k, v := range a.uninit {
		if v {
			out.uninit[k] = true
		}
	}

	for k, v := range b.uninit {
		if v {
			out.uninit[k] = true
		}
	}
	// notZero: only retain when BOTH branches proved non-zero.  Either
	// path that didn't prove it leaves the merge unable to make the
	// claim either.  (Intersection: the stricter of the two facts.)
	for k, v := range a.notZero {
		if v && b.notZero[k] {
			out.notZero[k] = true
		}
	}
	// boundsChecked: intersection (same shape as notZero).  A name
	// remains proven bounds-checked at the join only when every
	// incoming path established an upper bound.
	for k, v := range a.boundsChecked {
		if v && b.boundsChecked[k] {
			out.boundsChecked[k] = true
		}
	}
	// types: union; static type info, never differs between paths,
	// so we just take whichever side has the binding.
	for k, v := range a.types {
		out.types[k] = v
	}

	for k, v := range b.types {
		if _, ok := out.types[k]; !ok {
			out.types[k] = v
		}
	}

	// manualAlloc merge: Live AND Live -> Live (still might leak);
	// Freed AND Freed -> Freed (consistent); mix -> Live (the
	// not-freed path leaks); Escaped on either side -> Escaped
	// (binding has left the fn, don't track further).
	for k, va := range a.manualAlloc {
		vb, hasB := b.manualAlloc[k]
		if !hasB {
			out.manualAlloc[k] = va

			continue
		}

		switch {
		case va == manualAllocEscaped || vb == manualAllocEscaped:
			out.manualAlloc[k] = manualAllocEscaped
		case va == manualAllocLive || vb == manualAllocLive:
			// Either path didn't free -- treat as Live so the
			// post-loop / post-if scope-exit check flags it.
			out.manualAlloc[k] = manualAllocLive
		case va == manualAllocFreed && vb == manualAllocFreed:
			out.manualAlloc[k] = manualAllocFreed
		}
	}

	for k, vb := range b.manualAlloc {
		if _, ok := out.manualAlloc[k]; !ok {
			out.manualAlloc[k] = vb
		}
	}

	return out
}

// unionInterval is the lattice meet for the strided-interval analysis:
// the smallest strided interval containing both inputs. Either operand
// being unset means the result is unset (we lost information).
//
// Bounds use min/max. Stride is gcd of the inputs' effective strides
// and the gap between their lows -- the standard strided-interval lub
// (Reps, Improved Memory-Access Analysis 2006). Singletons contribute
// stride 0 (gcd identity) so a single value does not over-constrain
// the merge.
func unionInterval(a, b interval) interval {
	if !a.set || !b.set {
		return interval{}
	}

	lo := a.lo
	if b.lo < lo {
		lo = b.lo
	}

	hi := a.hi
	if b.hi > hi {
		hi = b.hi
	}

	if lo == hi {
		return singletonIv(lo)
	}

	gap := a.lo - b.lo
	if gap < 0 {
		gap = -gap
	}

	s := gcd64(a.effectiveStride(), b.effectiveStride())
	s = gcd64(s, gap)

	if s == 0 {
		s = 1
	}

	if (hi-lo)%s != 0 {
		s = 1
	}

	return interval{set: true, lo: lo, hi: hi, stride: s}
}

// widenInterval is the Cousot-Cousot widening operator restricted to a
// single variable: when `cur` extends beyond `prev` on either bound, the
// extending side is pushed to ±∞ (int64 saturation) so the fixpoint
// can stabilize on a non-trivial loop counter without taking thousands
// of iterations. Stride is preserved by snapping the widened bound back
// to the nearest multiple-of-stride offset from `cur.lo`, keeping the
// interval invariant `(hi-lo) % stride == 0` intact for downstream
// modulo folding (`x % stride == 0` provably 0 inside the loop body).
func widenInterval(prev, cur interval) interval {
	if !prev.set || !cur.set {
		return cur
	}

	wlo, whi := cur.lo, cur.hi

	if cur.lo < prev.lo {
		wlo = math.MinInt64
	}

	if cur.hi > prev.hi {
		whi = math.MaxInt64
	}

	if wlo == cur.lo && whi == cur.hi {
		return cur
	}

	s := cur.effectiveStride()
	if s > 1 {
		if whi == math.MaxInt64 {
			d := uint64(whi) - uint64(cur.lo)
			whi -= int64(d % uint64(s))
		}

		if wlo == math.MinInt64 {
			d := uint64(cur.lo) - uint64(wlo)
			wlo += int64(d % uint64(s))
		}
	}

	if wlo > whi {
		return cur
	}

	if wlo == whi {
		return singletonIv(wlo)
	}

	return interval{set: true, lo: wlo, hi: whi, stride: cur.stride}
}

// widenStates applies widenInterval per-variable to the freshly-merged
// state `cur` against the previous iteration's `prev`. Other lattice
// pieces (consts, nil-state, freed) are kept as-is -- they don't grow
// monotonically the way intervals do.
func widenStates(prev, cur *dfState) *dfState {
	if prev == nil || cur == nil {
		return cur
	}

	out := cur.clone()

	for k, nv := range cur.intv {
		if pv, ok := prev.intv[k]; ok && pv.set && nv.set {
			out.intv[k] = widenInterval(pv, nv)
		}
	}

	return out
}

// shiftIv returns iv shifted by a constant `delta`. Stride and width
// are preserved. Used when an augmented assign or postfix op adds a
// known constant to an induction variable.  Bottoms out (returns the
// empty interval) on i64 overflow rather than producing wrapped bounds
// that downstream narrowing would treat as a real range.
func shiftIv(iv interval, delta int64) interval {
	if !iv.set {
		return interval{}
	}

	lo, ok1 := addOverflow(iv.lo, delta)
	hi, ok2 := addOverflow(iv.hi, delta)

	if !ok1 || !ok2 {
		return interval{}
	}

	return interval{set: true, lo: lo, hi: hi, stride: iv.stride}
}

// allMembersDivisibleBy returns true when every value the strided
// interval can hold is an exact multiple of n. Requires the stride to
// divide n's modulus AND the base lo to already be a multiple of n.
// Used to fold `x % n` to 0 when the loop variable steps by a multiple
// of n from a multiple of n (e.g. `for i = 0; ...; i += 500` makes
// `i % 500 == 0` provably always-true).
func allMembersDivisibleBy(iv interval, n int64) bool {
	if !iv.set || n <= 0 {
		return false
	}

	if iv.lo%n != 0 {
		return false
	}

	if iv.lo == iv.hi {
		return true
	}

	s := iv.effectiveStride()
	if s == 0 {
		return false
	}

	return s%n == 0
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

	if len(a.uninit) != len(b.uninit) {
		return false
	}

	for k, v := range a.uninit {
		if w, ok := b.uninit[k]; !ok || w != v {
			return false
		}
	}

	return true
}

// runDataflow runs the intraprocedural dataflow pass over every top-level
// FuncDecl (and struct method). Each function gets its own analysis run.
//
// Pre-pass: compute the interprocedural manual-alloc summaries
// (paramFrees, returnsAlloc) so the dataflow's call-site handler
// can make precise escape decisions instead of conservatively
// marking every call arg as Escaped.
func (cg *CodeGen) runDataflow(prog *ast.Program) {
	cg.computeManualAllocSummaries(prog)

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

// computeManualAllocSummaries walks every top-level fn (and struct
// method) twice:
//
//  1. Direct pass: for each fn F:
//     - paramFrees[F] gets every param index i such that the body
//     contains `mem::free(p_i)` directly.
//     - returnsAlloc[F] is true when any return value is a direct
//     call to mem::malloc / calloc / realloc / alloc.
//
//  2. Fixpoint iteration: walk all calls.  If callee C has
//     paramFrees[C] containing index j, and the call's arg at
//     position j is the caller's own param p_k, propagate by
//     adding k to paramFrees[F].  Similarly, when `return f(...)`
//     and f is in returnsAlloc, set returnsAlloc[F] = true.
//     Iterate until no changes.
//
// Conservative: parameters that flow through complex expressions
// (struct field, address-of, etc.) are not tracked.  The
// dataflow's call-site handler treats such bindings as Escaped --
// which is the existing fallback -- so the summary just refines
// the common direct-pass shape.
func (cg *CodeGen) computeManualAllocSummaries(prog *ast.Program) {
	if cg.paramFrees == nil {
		cg.paramFrees = map[string]map[int]bool{}
	}

	if cg.returnsAlloc == nil {
		cg.returnsAlloc = map[string]bool{}
	}

	type fnEntry struct {
		decl       *ast.FuncDecl
		paramIndex map[string]int // param name -> position
	}

	fns := map[string]fnEntry{}

	addFn := func(fd *ast.FuncDecl) {
		if fd == nil || fd.Body == nil || fd.IsExtern != "" || fd.IsVirtual {
			return
		}

		idx := map[string]int{}
		pos := 0

		for _, p := range fd.Params {
			if p.IsVarArgs {
				continue
			}

			if p.Name != "" && p.Name != "_" {
				idx[p.Name] = pos
			}

			pos++
		}

		fns[fd.Name] = fnEntry{decl: fd, paramIndex: idx}
	}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			addFn(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				addFn(m)
			}
		}
	}

	// Phase 1: direct summary.  Seed an empty entry for every
	// analyzed fn so the dataflow's call-site handler can tell
	// "analyzed callee, frees nothing" from "unanalyzed callee
	// (extern / indirect)".  Without the seed, both cases looked
	// identical (paramFrees lookup returned nil) and a Live alloc
	// passed to a read-only helper got marked Escaped, silently
	// suppressing the leak warning.
	for name := range fns {
		if cg.paramFrees[name] == nil {
			cg.paramFrees[name] = map[int]bool{}
		}
	}

	for name, entry := range fns {
		walkAST(entry.decl.Body, func(n ast.Node) {
			switch s := n.(type) {
			case *ast.ExprStmt:
				if argName, ok := isManualFreeCall(s.Expr); ok {
					if i, here := entry.paramIndex[argName]; here {
						cg.paramFrees[name][i] = true
					}
				}
			case *ast.ReturnStmt:
				if s.Value != nil && isManualAllocCall(s.Value) {
					cg.returnsAlloc[name] = true
				}
			}
		})
	}

	// Phase 2: fixpoint over call edges.
	for changed := true; changed; {
		changed = false

		for name, entry := range fns {
			walkAST(entry.decl.Body, func(n ast.Node) {
				switch s := n.(type) {
				case *ast.CallExpr:
					id, ok := s.Func.(*ast.Identifier)
					if !ok {
						return
					}

					calleeFrees, hasFrees := cg.paramFrees[id.Name]
					if !hasFrees {
						return
					}

					for i, arg := range s.Args {
						if !calleeFrees[i] {
							continue
						}

						argID, ok2 := arg.(*ast.Identifier)
						if !ok2 {
							continue
						}

						pidx, here := entry.paramIndex[argID.Name]
						if !here {
							continue
						}

						if cg.paramFrees[name] == nil {
							cg.paramFrees[name] = map[int]bool{}
						}

						if !cg.paramFrees[name][pidx] {
							cg.paramFrees[name][pidx] = true
							changed = true
						}
					}
				case *ast.ReturnStmt:
					if s.Value == nil {
						return
					}

					call, ok := s.Value.(*ast.CallExpr)
					if !ok {
						return
					}

					id, ok2 := call.Func.(*ast.Identifier)
					if !ok2 {
						return
					}

					if cg.returnsAlloc[id.Name] && !cg.returnsAlloc[name] {
						cg.returnsAlloc[name] = true
						changed = true
					}
				}
			})
		}
	}
}

func (cg *CodeGen) dfAnalyzeFunc(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Stash the current fn name for the Andersen lookup in
	// dfCheckExpr -- ptVarFor keys on the (fn, var) pair, and the
	// dataflow walker doesn't otherwise know which function it is
	// in.  Cleared at exit so a later subagent / monomorphization
	// step doesn't accidentally key into the wrong fn.
	prevDfFn := cg.dfCurFnName
	cg.dfCurFnName = fn.Name

	defer func() { cg.dfCurFnName = prevDfFn }()

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

		if p.Type != nil {
			st.types[p.Name] = p.Type
		}
	}

	cg.manualAllocSites = map[string]ast.Pos{}

	finalSt := cg.dfWalkAny(fn.Body, st)
	cg.dfCheckManualAllocLeaks(finalSt)
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
		return rangeIv(-128, 127)
	case "u8", "byte", "char":
		return rangeIv(0, 255)
	case "i16":
		return rangeIv(-32768, 32767)
	case "u16":
		return rangeIv(0, 65535)
	case "i32":
		return rangeIv(-2147483648, 2147483647)
	case "u32":
		return rangeIv(0, 4294967295)
	case "i64":
		return rangeIv(-9223372036854775808, 9223372036854775807)
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
		return singletonIv(e.Value)
	case *ast.Identifier:
		if v, ok := st.intv[e.Name]; ok {
			return v
		}
	case *ast.UnaryExpr:
		if e.Op == "-" {
			if iv := cg.intervalOf(e.Expr, st); iv.set {
				return interval{set: true, lo: -iv.hi, hi: -iv.lo, stride: iv.effectiveStride()}
			}
		}
	case *ast.BinExpr:
		l := cg.intervalOf(e.Left, st)
		r := cg.intervalOf(e.Right, st)

		return intervalArith(e.Op, l, r)
	}

	return interval{}
}

// intervalArith computes the strided interval of `a op b`. Handles `+`,
// `-`, `*` when at least one side narrows to useful bounds; returns the
// unset interval otherwise. Stride is preserved through shifts (single-
// constant `+`/`-`) and scaled through multiplications by a constant.
//
// Overflow at i64 boundaries collapses the result to the empty/unset
// interval rather than wrapping silently -- the wrapped bounds would
// drive narrowIntervalCmp to false-positive "impossible range" warnings.
func intervalArith(op string, a, b interval) interval {
	if !a.set || !b.set {
		return interval{}
	}

	aSingle := a.lo == a.hi
	bSingle := b.lo == b.hi

	switch op {
	case "+":
		if bSingle {
			return shiftIv(a, b.lo)
		}

		if aSingle {
			return shiftIv(b, a.lo)
		}

		lo, ok1 := addOverflow(a.lo, b.lo)
		hi, ok2 := addOverflow(a.hi, b.hi)

		if !ok1 || !ok2 {
			return interval{}
		}

		s := gcd64(a.effectiveStride(), b.effectiveStride())
		if s == 0 {
			s = 1
		}

		if (hi-lo)%s != 0 {
			s = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: s}
	case "-":
		if bSingle {
			neg, ok := subOverflow(0, b.lo)
			if !ok {
				return interval{}
			}

			return shiftIv(a, neg)
		}

		lo, ok1 := subOverflow(a.lo, b.hi)
		hi, ok2 := subOverflow(a.hi, b.lo)

		if !ok1 || !ok2 {
			return interval{}
		}

		s := gcd64(a.effectiveStride(), b.effectiveStride())
		if s == 0 {
			s = 1
		}

		if (hi-lo)%s != 0 {
			s = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: s}
	case "*":
		if bSingle {
			return scaleIv(a, b.lo)
		}

		if aSingle {
			return scaleIv(b, a.lo)
		}
	}

	return interval{}
}

// addOverflow returns a+b and a flag indicating whether the result fits
// in int64.  Used by intervalArith to bottom-out wraparound.
func addOverflow(a, b int64) (int64, bool) {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		return 0, false
	}

	return r, true
}

// subOverflow returns a-b with a fits-in-int64 flag.
func subOverflow(a, b int64) (int64, bool) {
	r := a - b
	if (b < 0 && r < a) || (b > 0 && r > a) {
		return 0, false
	}

	return r, true
}

// scaleIv multiplies a strided interval by a constant. Stride scales
// with the constant; bounds flip when the constant is negative.
func scaleIv(iv interval, k int64) interval {
	if !iv.set {
		return interval{}
	}

	if k == 0 {
		return singletonIv(0)
	}

	lo := iv.lo * k
	hi := iv.hi * k

	if k < 0 {
		lo, hi = hi, lo
	}

	if lo == hi {
		return singletonIv(lo)
	}

	s := iv.effectiveStride() * k
	if s < 0 {
		s = -s
	}

	if s == 0 {
		s = 1
	}

	return interval{set: true, lo: lo, hi: hi, stride: s}
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
	case *ast.TupleDestructDecl:
		// Mark a `t[k]` RHS as ok-destructured so the
		// -Wunchecked-index pedantic check stays silent for the
		// canonical `let (v, ok) = t[k]` shape.  The IndexExpr's
		// visit consults cg.dfSkipIndexCheck.
		if ie, ok := v.Value.(*ast.IndexExpr); ok {
			if cg.dfSkipIndexCheck == nil {
				cg.dfSkipIndexCheck = map[*ast.IndexExpr]bool{}
			}

			cg.dfSkipIndexCheck[ie] = true
		}

		cg.dfCheckExpr(v.Value, st)

		// Seed types for the bound names.  Without type inference
		// from the destructured tuple we leave them untyped (no
		// downstream warning is silenced or fired incorrectly).
		st = cg.dfApplyEscapes(v.Value, st)
		st = st.clone()

		for _, name := range v.Names {
			if name == "" || name == "_" {
				continue
			}

			st.nil[name] = nilBottom
			st.cnst[name] = cBotFact()
			delete(st.uninit, name)
		}

		return st

	case *ast.VarDecl:
		cg.dfCheckExpr(v.Value, st)

		// Compute the binding's facts from the pre-escape state (the
		// RHS expression evaluates against the values held going in),
		// then bottom-ise any other names whose address escaped through
		// the RHS. Target gets overwritten next so escape on it is a
		// no-op.
		newNil := cg.dfNilOf(v.Value, st)
		newCnst := cg.dfEval(v.Value, st)
		newIv := cg.dfIntervalForBinding(v.Type, v.Value, st)
		newFp := dfFoldFloat(v.Value, st)

		st = cg.dfApplyEscapes(v.Value, st)
		st = st.clone()
		st.nil[v.Name] = newNil
		st.cnst[v.Name] = newCnst

		if newIv.set {
			st.intv[v.Name] = newIv
		}

		if newFp != nil {
			st.floats[v.Name] = newFp
		}
		// Track the declared type so dfCheckUncheckedIndex can tell
		// built-in arrays apart from user `::index` overloads.  When
		// the let binding is unannotated, infer from the RHS shape
		// where possible (array literals, len() calls produce known
		// types).
		if v.Name != "" && v.Name != "_" {
			if v.Type != nil {
				st.types[v.Name] = v.Type
			} else if rhsType := cg.dfInferTypeFromRHS(v.Value); rhsType != nil {
				st.types[v.Name] = rhsType
			}
			// Manual-alloc tracking: `let p = mem::malloc(...)`
			// (or calloc/realloc/alloc) starts a Live binding
			// that must be passed to `mem::free` before scope
			// exit.  ALSO: `let p = make()` where `make` was
			// proven by the interprocedural pre-pass to return
			// a freshly-allocated block starts a Live binding
			// too -- the caller now owns the allocation and
			// must free it.  Reassignment to anything else
			// clears the entry.
			if isManualAllocCall(v.Value) || cg.callReturnsAlloc(v.Value) {
				st.manualAlloc[v.Name] = manualAllocLive

				if cg.manualAllocSites == nil {
					cg.manualAllocSites = map[string]ast.Pos{}
				}

				cg.manualAllocSites[v.Name] = v.Pos()
			} else {
				delete(st.manualAlloc, v.Name)
			}
		}
		// Track the uninit-by-decl shape `let x T` (no initializer) so a
		// later read fires DiagUseBeforeAssign. `let x = expr` (or `let
		// x T = expr`) is initialized.
		if v.Value == nil && v.Name != "" && v.Name != "_" {
			st.uninit[v.Name] = true
		} else {
			delete(st.uninit, v.Name)
		}

		return st

	case *ast.AssignStmt:
		cg.dfCheckExpr(v.Value, st)

		newNil := cg.dfNilOf(v.Value, st)
		newCnst := cg.dfEval(v.Value, st)
		newIv := cg.intervalOf(v.Value, st)
		newFp := dfFoldFloat(v.Value, st)

		st = cg.dfApplyEscapes(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			st = st.clone()
			st.nil[id.Name] = newNil
			st.cnst[id.Name] = newCnst
			delete(st.freed, id.Name) // reassign clears freed state
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name) // reassign clears any prior non-zero proof
			// Manual-alloc reassign rules:
			//   - prior Live + RHS is mem::realloc(<same id>, ...):
			//       legal swap (realloc consumes the input
			//       pointer internally).  Stay Live, no warn.
			//   - prior Live + RHS is any OTHER alloc / call that
			//       returns alloc:  the prior block is dropped --
			//       warn leak, then transition to a fresh Live
			//       binding for the new allocation.
			//   - prior Live + RHS is a borrow (other identifier,
			//       field load, etc.):  same drop-warn, then
			//       clear the entry (no new alloc to track).
			//   - prior absent + RHS is an alloc: standard Live
			//       initialisation.
			//   - prior absent + RHS is anything else: no-op.
			rhsIsAlloc := isManualAllocCall(v.Value) || cg.callReturnsAlloc(v.Value)
			rhsIsReallocSelf := isReallocOfSelf(v.Value, id.Name)
			priorLive := st.manualAlloc[id.Name] == manualAllocLive

			switch {
			case priorLive && rhsIsReallocSelf:
				// Legal swap; nothing to do.
			case priorLive && rhsIsAlloc:
				cg.warn(DiagManualAllocLeak, v.Pos(),
					"reassigning %q drops the previous mem::malloc/calloc/realloc result without `mem::free`; add the free before the reassignment, or transfer ownership first",
					id.Name)

				st.manualAlloc[id.Name] = manualAllocLive
				cg.manualAllocSites[id.Name] = v.Pos()
			case priorLive:
				cg.warn(DiagManualAllocLeak, v.Pos(),
					"reassigning %q drops the previous mem::malloc/calloc/realloc result without `mem::free`; add the free before the reassignment, or transfer ownership first",
					id.Name)

				delete(st.manualAlloc, id.Name)
			case rhsIsAlloc:
				st.manualAlloc[id.Name] = manualAllocLive
				cg.manualAllocSites[id.Name] = v.Pos()
			default:
				delete(st.manualAlloc, id.Name)
			}

			if newIv.set {
				st.intv[id.Name] = newIv
			} else {
				delete(st.intv, id.Name)
			}

			if newFp != nil {
				st.floats[id.Name] = newFp
			} else {
				delete(st.floats, id.Name)
			}
		}

		return st

	case *ast.AugAssignStmt:
		cg.dfCheckExpr(v.Value, st)
		st = cg.dfApplyEscapes(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			// `x += y` reads x before writing, so uninit fires here.
			if st.uninit[id.Name] {
				cg.warn(DiagUseBeforeAssign, v.Pos(),
					"%q is read by %q before being explicitly assigned",
					id.Name, v.Op)
			}

			st = st.clone()
			st.nil[id.Name] = nilBottom
			delete(st.floats, id.Name)
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name)
			// `x += k` and `x -= k` for constant k shift the strided
			// interval. Preserving stride here is what lets the loop
			// fixpoint widen `for epoch = 0; ...; epoch += 500` to a
			// stride-500 interval rather than collapsing to BOTTOM.
			cur, hasIv := st.intv[id.Name]
			delta, isInt := dfConstInt(cg.dfEval(v.Value, st))

			switch {
			case isInt && (v.Op == "+=" || v.Op == "-=") && hasIv && cur.set:
				d := delta
				if v.Op == "-=" {
					d = -d
				}

				st.intv[id.Name] = shiftIv(cur, d)
			default:
				delete(st.intv, id.Name)
			}

			st.cnst[id.Name] = cBotFact()
		}

		return st

	case *ast.PostfixStmt:
		// `x++` / `x--` mutates the bound name. Treat as a strided shift
		// by ±1 so a loop counter's interval widens through the fixpoint
		// instead of collapsing -- e.g. `for i = 0; i < 5; i++` ends up
		// with i ∈ [0, 5] stride 1 at the join.
		cg.dfCheckExpr(v.Expr, st)

		if id, ok := v.Expr.(*ast.Identifier); ok {
			if st.uninit[id.Name] {
				cg.warn(DiagUseBeforeAssign, v.Pos(),
					"%q is read by %q before being explicitly assigned",
					id.Name, v.Op)
			}

			st = st.clone()
			st.nil[id.Name] = nilBottom
			delete(st.floats, id.Name)
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name)

			var d int64

			switch v.Op {
			case "++":
				d = 1
			case "--":
				d = -1
			}

			if cur, ok := st.intv[id.Name]; ok && cur.set && d != 0 {
				st.intv[id.Name] = shiftIv(cur, d)
			} else {
				delete(st.intv, id.Name)
			}

			st.cnst[id.Name] = cBotFact()
		}

		return st

	case *ast.ExprStmt:
		if name, ok := isDeinitCall(v.Expr); ok {
			st = st.clone()

			if st.freed[name] {
				cg.warn(DiagDoubleDeinit, v.Expr.Pos(),
					"deinit on %q which has already been deinitialized on this path", name)
			}

			st.freed[name] = true

			return st
		}
		// Manual-alloc tracking: detect `mem::free(p)` at the
		// statement level (the common shape).  Transitions p
		// from Live to Freed; warns on a re-free of an already
		// Freed binding.
		if name, ok := isManualFreeCall(v.Expr); ok {
			st = st.clone()

			if st.manualAlloc[name] == manualAllocFreed {
				cg.warn(DiagManualDoubleFree, v.Expr.Pos(),
					"mem::free(%q) but %q was already freed on this path", name, name)
			}

			st.manualAlloc[name] = manualAllocFreed

			return st
		}

		cg.dfCheckExpr(v.Expr, st)

		return cg.dfApplyEscapes(v.Expr, st)

	case *ast.EchoStmt:
		cg.dfCheckExpr(v.Value, st)

		return cg.dfApplyEscapes(v.Value, st)

	case *ast.ReturnStmt:
		if v.Value != nil {
			cg.dfCheckExpr(v.Value, st)
		}

		// Manual-alloc ownership transfer: returning a Live binding
		// transfers ownership to the caller; mark Escaped so the
		// scope-exit leak check stays silent.
		st = st.clone()
		dfEscapeManualAllocFromExpr(v.Value, st)
		// Early-return leak check: any binding still Live AND not
		// in the returned expression leaks via this exit point.
		// Without this the function-end leak check (which runs on
		// the joined non-dead state) misses leaks that escape via
		// early returns -- the dead branch's state is discarded.
		cg.dfCheckManualAllocLeaks(st)

		dead := st.clone()
		dead.dead = true

		return dead

	case *ast.IfStmt:
		return cg.dfWalkIf(v, st)

	case *ast.ForStmt:
		// For-in loops have nil Init/Cond/Post; piping them through
		// dfWalkLoop walks the body once with the entry state and the
		// induction variable is never registered, so any prior fact
		// about a name shadowed by VarName leaks into the body.  Drop
		// such facts before walking so the body sees a fresh slot for
		// the loop variable.
		if v.Kind == ast.ForIn {
			loopSt := st
			if v.VarName != "" {
				loopSt = st.clone()
				delete(loopSt.nil, v.VarName)
				delete(loopSt.cnst, v.VarName)
				delete(loopSt.intv, v.VarName)
				delete(loopSt.floats, v.VarName)
				delete(loopSt.uninit, v.VarName)
				delete(loopSt.freed, v.VarName)
				delete(loopSt.notZero, v.VarName)
				delete(loopSt.boundsChecked, v.VarName)
				// Range-form iteration (`for i in lo..hi`) makes the
				// loop variable an integer index bounded above by hi
				// (a typical bounds-check shape).  Mark it
				// boundsChecked so `arr[i]` inside the body skips the
				// -Wunchecked-index pedantic warning when the
				// surrounding code wrote the obvious `for i in
				// 0..len(arr): arr[i]` pattern.  We don't verify hi
				// matches the indexed array -- the warning is about
				// intent ("you wrote an upper bound"), not soundness.
				if bin, ok := v.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
					loopSt.boundsChecked[v.VarName] = true
				}
			}
			// Evaluate the iterable for side-effect facts (e.g. nil
			// guard on the iter expression).
			if v.Iter != nil {
				cg.dfCheckExpr(v.Iter, loopSt)
			}

			return cg.dfWalkLoop(nil, nil, nil, v.Body, loopSt)
		}

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

	// Phase 1: iterate to fixpoint silently. The first iterations see a
	// transient state where loop-modified locals still hold their init
	// values; warnings emitted then would be phantom (e.g. "epoch % 500 ==
	// 0 always true" on iter 0 of `for let epoch = 0; ...; epoch++`).
	cg.dfSuppressWarnings++

	converged := false

	for i := 0; i < maxIter; i++ {
		if cond != nil {
			cg.dfCheckExpr(cond, prev)
		}
		// Body sees the then-narrowed state from the loop condition
		// (`for i < len(arr): arr[i]` should treat `i` as
		// boundsChecked inside the body).  narrowOnCond returns
		// (thenSt, elseSt); we want thenSt for the body, the
		// elseSt is the post-loop continuation handled by the
		// outer caller via the un-narrowed `prev`.
		bodySt := prev.clone()
		if cond != nil {
			thenSt, _ := narrowOnCond(cond, prev)
			if thenSt != nil {
				bodySt = thenSt
			}
		}

		bodySt = cg.dfWalkBlock(body, bodySt)

		if post != nil && bodySt != nil && !bodySt.dead {
			bodySt = cg.dfWalkStmt(post, bodySt)
		}

		merged := mergeStates(prev, bodySt)
		if i >= 1 {
			// Apply widening from the second iteration onward so that
			// monotone-growing loop counters saturate to int64 bounds
			// (preserving stride). Without this, `for i = 0; i < N; i++`
			// adds 1 to the upper bound each iteration and never
			// converges within maxIter.
			merged = widenStates(prev, merged)
		}

		if statesEqual(prev, merged) {
			converged = true
			prev = merged

			break
		}

		prev = merged
	}

	cg.dfSuppressWarnings--

	if !converged {
		// Did not converge in maxIter; return BOTTOM-ised state for
		// everything the loop body might touch. Skip the emit pass --
		// flow-sensitive warnings on a state that didn't stabilize are
		// unreliable.
		//
		// `freed` and `uninit` are MAY-lattices: an entry means "the
		// variable might be freed / uninitialised on some path through
		// the loop."  Dropping them when the loop fails to converge
		// silently masks use-after-deinit and use-of-uninit on the
		// post-loop tail.  Carry them forward verbatim instead.
		out := newDFState()
		for k := range prev.nil {
			out.nil[k] = nilBottom
		}

		for k := range prev.cnst {
			out.cnst[k] = cBotFact()
		}

		for k, v := range prev.freed {
			out.freed[k] = v
		}

		for k, v := range prev.uninit {
			out.uninit[k] = v
		}

		return out
	}

	// Phase 2: walk the body once more with the converged input state so
	// flow-sensitive warnings fire against values that actually reflect
	// the loop's steady state. Any variable widened to BOTTOM during
	// fixpoint stays BOTTOM here, so phantom-constant conditions don't
	// fold; conditions that genuinely fold under the converged state
	// (e.g. a local re-assigned the same constant on every iteration)
	// still produce warnings. The walk's output state is discarded --
	// prev is already a fixpoint, so re-walking can't refine it.
	if cond != nil {
		cg.dfCheckExpr(cond, prev)
	}

	bodySt := prev.clone()
	if cond != nil {
		thenSt, _ := narrowOnCond(cond, prev)
		if thenSt != nil {
			bodySt = thenSt
		}
	}

	cg.dfWalkBlock(body, bodySt)

	return prev
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
		case *ast.Identifier:
			// Manual-alloc use-after-free: any read of a name
			// whose manualAlloc state is Freed fires the
			// diagnostic.  Skipped when the read is `mem::free`
			// dispatching (we want one warning per use site, not
			// recursively on the free arg itself -- isManualFreeCall
			// already handles the double-free message).
			if st.manualAlloc[e.Name] == manualAllocFreed {
				cg.warn(DiagManualUseAfterFree, e.Pos(),
					"use of %q after mem::free on this path", e.Name)
			}
		case *ast.DerefExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"dereferencing %q which is statically nil at this point", id.Name)
				} else if st.nil[id.Name] != nilNonNil {
					// Prefer the interprocedural diagnostic when
					// Andersen has more-specific info: "this came
					// from a fn that returns nil sometimes".  Falls
					// back to the generic intraprocedural pedantic
					// warning when no interprocedural source is
					// known (e.g. the value is a fresh param).
					if cg.andersenMayBeNil(cg.dfCurFnName, id.Name) {
						cg.warn(DiagUncheckedReturnedNil, e.Pos(),
							"dereferencing %q whose source function may return nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					} else {
						cg.warn(DiagUncheckedNilDeref, e.Pos(),
							"dereference of %q without proving non-nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					}
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
				} else if dfIsPointerLike(st.types[id.Name]) && st.nil[id.Name] != nilNonNil {
					// Same prefer-interprocedural logic as the
					// DerefExpr branch above: the Andersen warning
					// names the source function, which is more
					// actionable than the generic dataflow message.
					if cg.andersenMayBeNil(cg.dfCurFnName, id.Name) {
						cg.warn(DiagUncheckedReturnedNil, e.Pos(),
							"field access on %q whose source function may return nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					} else {
						cg.warn(DiagUncheckedNilDeref, e.Pos(),
							"field access on pointer %q without proving non-nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					}
				}

				if st.freed[id.Name] {
					cg.warn(DiagUseAfterDeinit, e.Pos(),
						"field access on %q after deinit on this path", id.Name)
				}
			}
		case *ast.IndexExpr:
			cg.dfCheckUncheckedIndex(e, st)
		case *ast.BinExpr:
			cg.dfCheckFloatPrecision(e, st)
			cg.dfCheckUncheckedDiv(e, st)
		}
	})

	cg.dfCheckUseBeforeAssign(expr, st)
}

// dfCheckUseBeforeAssign warns on every read of a name that's still in
// st.uninit. Excludes idents that appear directly under `&` (taking an
// address doesn't read the value) and the `name` half of FieldAccess
// (`s.field` -- field is a label, not a read of a binding called
// `field`). The expression is walked with custom recursion (rather than
// walkAST) so the skip rules can be applied site-locally.
func (cg *CodeGen) dfCheckUseBeforeAssign(expr ast.Node, st *dfState) {
	if expr == nil || st == nil || len(st.uninit) == 0 {
		return
	}

	cg.dfCheckUseBeforeAssignNode(expr, st)
}

func (cg *CodeGen) dfCheckUseBeforeAssignNode(n ast.Node, st *dfState) {
	if n == nil {
		return
	}

	switch e := n.(type) {
	case *ast.Identifier:
		if st.uninit[e.Name] {
			cg.warn(DiagUseBeforeAssign, e.Pos(),
				"%q is read before being explicitly assigned (zero-initialized at runtime)",
				e.Name)
		}
	case *ast.AddressOfExpr:
		// `&x` itself is a read of the storage location, not the value;
		// skip the inner identifier. But anything deeper (e.g. `&s.f`)
		// still walks normally so reads under field accesses are caught.
		if _, isIdent := e.Expr.(*ast.Identifier); isIdent {
			return
		}

		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.FieldAccess:
		// Recurse only into the receiver: `s.field` reads s, not a
		// binding called field.
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.BinExpr:
		cg.dfCheckUseBeforeAssignNode(e.Left, st)
		cg.dfCheckUseBeforeAssignNode(e.Right, st)
	case *ast.UnaryExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.CallExpr:
		cg.dfCheckUseBeforeAssignNode(e.Func, st)

		for _, a := range e.Args {
			cg.dfCheckUseBeforeAssignNode(a, st)
		}
	case *ast.DerefExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.IndexExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
		cg.dfCheckUseBeforeAssignNode(e.Index, st)
	case *ast.TernaryExpr:
		cg.dfCheckUseBeforeAssignNode(e.Cond, st)
		cg.dfCheckUseBeforeAssignNode(e.Then, st)
		cg.dfCheckUseBeforeAssignNode(e.Else, st)
	}
}

// dfApplyEscapes returns a copy of `st` with any name whose address is
// taken in `expr` invalidated to BOTTOM across every lattice piece. The
// callee receiving `&x` may store, mutate, or deinit through the
// pointer; we have no way to know without interprocedural info, so the
// safe assumption after the expression is that x's previous facts no
// longer hold.
func (cg *CodeGen) dfApplyEscapes(expr ast.Node, st *dfState) *dfState {
	if expr == nil || st == nil {
		return st
	}

	var (
		escaped      []string
		callArgNames []string
		callArgFreed []string
	)

	walkAST(expr, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.AddressOfExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
				escaped = append(escaped, id.Name)
			}
		case *ast.StructLit:
			// Storing a Live alloc into a struct field transfers
			// ownership to the struct: `Owner{buf: p}` makes the
			// Owner instance responsible for p, not the caller.
			// Mark p Escaped so the scope-exit leak check skips it.
			for _, f := range e.Fields {
				if id, ok := f.Value.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.ArrayLit:
			// Same logic for array element absorption:
			// `let xs = [p, q]` transfers ownership of p, q.
			for _, el := range e.Elems {
				if id, ok := el.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.TupleLit:
			for _, el := range e.Elems {
				if id, ok := el.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.CallExpr:
			// Skip mem::free -- handled separately in ExprStmt
			// (transitions Live -> Freed, not an escape).
			if _, isFree := isManualFreeCall(e); isFree {
				return
			}
			// Use the interprocedural paramFrees summary when
			// available.  Two cases:
			//   - hasSummary && calleeFrees[i] -> Live -> Freed
			//     (callee provably frees its own param at index i)
			//   - hasSummary && !calleeFrees[i] -> stays Live
			//     (callee provably does NOT free; the alloc is
			//     just borrowed for read/write through the pointer)
			//   - no summary (extern, indirect, unanalyzed) -> Escaped
			//     (conservative: callee MIGHT take ownership)
			var (
				calleeFrees map[int]bool
				hasSummary  bool
			)

			if id, ok := e.Func.(*ast.Identifier); ok {
				calleeFrees, hasSummary = cg.paramFrees[id.Name]
			}

			for i, a := range e.Args {
				argID, ok := a.(*ast.Identifier)
				if !ok || argID.Name == "" || argID.Name == "_" {
					continue
				}

				if calleeFrees != nil && calleeFrees[i] {
					// Caller's `helper(p)`-and-helper-frees-p
					// shape: transition Live -> Freed.
					callArgFreed = append(callArgFreed, argID.Name)

					continue
				}

				if hasSummary {
					// Analyzed callee provably does NOT free
					// this index -- the alloc remains the
					// caller's responsibility.
					continue
				}

				callArgNames = append(callArgNames, argID.Name)
			}
		}
	})

	if len(escaped) == 0 && len(callArgNames) == 0 && len(callArgFreed) == 0 {
		return st
	}

	out := st.clone()

	for _, name := range escaped {
		out.cnst[name] = cBotFact()
		out.nil[name] = nilBottom
		delete(out.intv, name)
		delete(out.floats, name)
		delete(out.freed, name)
		// An address taken doesn't tell us whether the callee assigned;
		// be lenient and clear uninit too so we don't flood the user
		// with warnings on idiomatic out-parameters (`fn fill(p *T)`).
		delete(out.uninit, name)
	}

	for _, name := range callArgNames {
		if out.manualAlloc[name] == manualAllocLive {
			out.manualAlloc[name] = manualAllocEscaped
		}
	}

	for _, name := range callArgFreed {
		// Interprocedural Live -> Freed (the callee's summary
		// proves the param is passed to mem::free).
		if out.manualAlloc[name] == manualAllocLive {
			out.manualAlloc[name] = manualAllocFreed
		}
	}

	return out
}

// dfEscapeManualAllocFromExpr walks `expr` and marks every manually
// allocated identifier that appears as Escaped in `st`.  Used by
// ReturnStmt to transfer ownership to the caller and by assignment
// to non-local destinations (struct field, array element, etc.) to
// signal that the binding now flows out of the current scope's
// responsibility.
func dfEscapeManualAllocFromExpr(expr ast.Node, st *dfState) {
	if expr == nil || st == nil {
		return
	}

	walkAST(expr, func(n ast.Node) {
		if id, ok := n.(*ast.Identifier); ok {
			if st.manualAlloc[id.Name] == manualAllocLive {
				st.manualAlloc[id.Name] = manualAllocEscaped
			}
		}
	})
}

// dfCheckManualAllocLeaks emits -Wmanual-alloc-leak for every name
// still in Live state at function exit.  Called once at the end of
// dfAnalyzeFunc with the joined post-body state -- intermediate
// scope exits don't fire (you're allowed to free in a later
// statement) but a path that reaches the function return without
// any `mem::free(p)` DOES fire.  Position is recovered from the
// scope entry where the binding was first seen; we keep that in a
// per-fn map populated during VarDecl walks.
func (cg *CodeGen) dfCheckManualAllocLeaks(st *dfState) {
	if st == nil {
		return
	}

	// Don't re-fire on a dead state: every path that ended in a
	// ReturnStmt already ran the leak check at that exit, so the
	// joined dead state would emit a second warning for the same
	// binding.  The fn-end caller and the ReturnStmt caller share
	// this helper; this guard keeps the second one silent.
	if st.dead {
		return
	}

	for name, state := range st.manualAlloc {
		if state != manualAllocLive {
			continue
		}

		pos := cg.manualAllocSites[name]
		cg.warn(DiagManualAllocLeak, pos,
			"%q holds an mem::malloc/calloc/realloc result that is not freed on every path; "+
				"add `mem::free(%s)` before scope exit, or transfer ownership by returning "+
				"the pointer / storing it in a field", name, name)
	}
}

// isReallocOfSelf reports whether `expr` is `mem::realloc(<name>, ...)`
// with the first argument being the identifier `name`.  The
// realloc convention is that the input pointer is consumed
// (potentially freed by the allocator), so reassigning the same
// binding to its realloc result is a LEGAL ownership transfer --
// not a dropped allocation.  Anything else (different name, no
// arg, different fn) returns false.
func isReallocOfSelf(expr ast.Node, name string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	callee := call.Func
	if ie, ok2 := callee.(*ast.IndexExpr); ok2 {
		callee = ie.Expr
	}

	sa, ok := callee.(*ast.ScopeAccess)
	if !ok {
		return false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" || sa.Path[1] != "realloc" {
		return false
	}

	if len(call.Args) < 1 {
		return false
	}

	id, ok := call.Args[0].(*ast.Identifier)

	return ok && id.Name == name
}

// callReturnsAlloc reports whether `expr` is a direct call to a fn
// that the pre-pass summary proves returns a freshly-allocated
// block.  Counterpart to isManualAllocCall but interprocedural --
// recognizes `let p = make()` shapes where `make` ultimately
// returns mem::malloc.  Conservative: only direct identifier-call
// shapes are tracked; method calls / scope-access calls / generic
// instantiations are out of scope.
func (cg *CodeGen) callReturnsAlloc(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok {
		return false
	}

	return cg.returnsAlloc[id.Name]
}

// isManualAllocCall reports whether `expr` is a call to one of the
// manual-allocation primitives in `mem::`:
//
//	mem::malloc(size)         // raw libc malloc
//	mem::calloc(n, size)      // raw libc calloc
//	mem::realloc(p, new_size) // raw libc realloc (already-owned block
//	                          // becomes Live again under the new var)
//	mem::alloc[T]()           // typed wrapper that allocates + zeros
//	mem::alloc[T](n)          // typed wrapper allocating n elements
//
// Each of these returns a freshly-owned block that must reach
// `mem::free` (or escape the fn) before scope exit.  Used by
// VarDecl/AssignStmt to seed the manualAlloc tracker.
func isManualAllocCall(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	// Type args wrap the callee in an IndexExpr:
	// `mem::alloc[*Foo]()` -> CallExpr{Func: IndexExpr{Expr:
	// ScopeAccess[mem, alloc], Index: <typeArg>}}.  No-type-args
	// callers have ScopeAccess directly.  Unwrap one IndexExpr
	// layer if present.
	callee := call.Func
	if ie, ok2 := callee.(*ast.IndexExpr); ok2 {
		callee = ie.Expr
	}

	sa, ok := callee.(*ast.ScopeAccess)
	if !ok {
		return false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" {
		return false
	}

	switch sa.Path[1] {
	case "malloc", "calloc", "realloc", "alloc":
		return true
	}

	return false
}

// isManualFreeCall reports whether `expr` is `mem::free(p)` with `p`
// a simple identifier; returns the identifier's name.  Anything more
// elaborate (`mem::free(struct.field)`, `mem::free(call())`) is left
// to the user to verify -- the lattice keyed on name can't track
// non-binding free targets.
func isManualFreeCall(expr ast.Node) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}

	sa, ok := call.Func.(*ast.ScopeAccess)
	if !ok {
		return "", false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" || sa.Path[1] != "free" {
		return "", false
	}

	if len(call.Args) != 1 {
		return "", false
	}

	id, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return "", false
	}

	return id.Name, true
}

// dfCheckUncheckedDiv warns on `a / b` and `a % b` when the dataflow
// pass cannot prove `b != 0` at the use site.  The default-on hard
// error for "division by zero" already rejects b == constant 0; this
// pedantic check covers the unproven case: b is a variable with an
// interval that may contain 0, or has no interval at all.  When b
// folds to a non-zero compile-time constant the check stays silent
// (the const case is proven safe).
func (cg *CodeGen) dfCheckUncheckedDiv(e *ast.BinExpr, st *dfState) {
	if e.Op != "/" && e.Op != "%" {
		return
	}

	// Constant RHS: zero already errored, non-zero is provably safe.
	if v := cg.tryFoldExpr(e.Right); v.kind == foldInt {
		return
	}

	// Identifier RHS with an interval that excludes zero, OR with a
	// flow-sensitive notZero proof, is provably safe.
	if id, ok := e.Right.(*ast.Identifier); ok {
		if iv, has := st.intv[id.Name]; has && iv.set && (iv.lo > 0 || iv.hi < 0) {
			return
		}

		if st.notZero[id.Name] {
			return
		}
	}

	// Float division by zero is well-defined IEEE (Inf/NaN); only warn
	// when the operands look integral.  We don't have type info here
	// cheap, so guard on op kind: `%` is integer-only in Tin, and `/`
	// on identifiers we conservatively warn about.
	rhsDesc := "divisor"
	if id, ok := e.Right.(*ast.Identifier); ok {
		rhsDesc = fmt.Sprintf("divisor %q", id.Name)
	}

	cg.warn(DiagUncheckedDiv, e.Pos(),
		"%q with unproven non-zero %s; guard with `if <divisor> != 0:` or narrow the value before this point",
		e.Op, rhsDesc)
}

// dfCheckUncheckedIndex warns on `arr[i]` when the dataflow pass
// cannot prove the access is safe on the current path.  Two
// regimes:
//
//   - Built-in arrays / slices / strings / pointers: index must be
//     bounds-checked.  Proof comes from a constant index that
//     fits, a flow-sensitive `boundsChecked` fact set by
//     narrowOnCond on `if i < expr:` / `if i <= expr:` guards.
//   - User structs with a `::index` overload: the impl returns
//     `(V, bool)` and the canonical safe access is the destructure
//     `let (v, ok) = t[k]` (the caller then checks `ok`).  A bare
//     `t[k]` at any other context relies on the codegen auto-
//     unwrap, which panics on miss -- that's the "unchecked" path.
//     The TupleDestructDecl pre-walk handler sets a transient
//     skip-flag so the access doesn't double-fire on the
//     destructured form.
func (cg *CodeGen) dfCheckUncheckedIndex(e *ast.IndexExpr, st *dfState) {
	// Constant index: handled by the default-on -Warray-bounds path.
	if v := cg.tryFoldExpr(e.Index); v.kind == foldInt {
		return
	}

	// Skip when this IndexExpr is the RHS of a tuple-destructure
	// (`let (v, ok) = t[k]`).  dfWalkStmt sets this transient flag
	// before walking the destructure's value so the IndexExpr's
	// visit knows it was unwrapped.  Applies to both array-like
	// receivers (rare; usually a fat-ptr-of-tuples access) and
	// custom `::index` receivers (the canonical safe form).
	if cg.dfSkipIndexCheck[e] {
		return
	}

	receiverIsArray := cg.indexReceiverTypeIsArrayLike(e.Expr, st)

	if receiverIsArray {
		// Identifier index with a flow-sensitive bounds-check proof
		// is provably safe.
		idIndex, isIdent := e.Index.(*ast.Identifier)
		if isIdent && st.boundsChecked[idIndex.Name] {
			return
		}

		idxDesc := "index"
		if isIdent {
			idxDesc = fmt.Sprintf("index %q", idIndex.Name)
		}

		cg.warn(DiagUncheckedIndex, e.Pos(),
			"array %s accessed without proven bounds check; guard with `if <index> < len(arr):` or narrow the value before this point",
			idxDesc)

		return
	}

	// Custom `::index` overload receiver: access without ok-
	// destructuring relies on the auto-unwrap panic to catch a
	// missing key at runtime.  Pedantic: warn so the user either
	// destructures `(v, ok) = t[k]` and checks `ok`, or accepts
	// the runtime panic by silencing the diagnostic explicitly.
	t := cg.dfResolveType(e.Expr, st)
	if t != nil && !isArrayLikeType(t) {
		recvDesc := dfExprDescription(e.Expr)
		cg.warn(DiagUncheckedIndex, e.Pos(),
			"indexed access on %s (custom `::index` impl) without `(v, ok)` destructure; pattern: `let (v, ok) = %s[...]; if ok: ...` or silence with `-Wno-unchecked-index`",
			recvDesc, recvDesc)
	}
}

// dfExprDescription returns a short rendering of an expression for
// use in diagnostic messages.  Identifier names render as `name`;
// FieldAccess as `recv.field`; everything else as `<expr>`.
func dfExprDescription(expr ast.Node) string {
	switch e := expr.(type) {
	case *ast.Identifier:
		return fmt.Sprintf("%q", e.Name)
	case *ast.FieldAccess:
		return fmt.Sprintf("%s.%s", dfExprDescription(e.Expr), e.Field)
	}

	return "<expr>"
}

// indexReceiverTypeIsArrayLike reports whether the IndexExpr
// receiver `expr` resolves to a built-in indexable type (array
// literal, slice / fat-ptr `[T]`, string, or pointer) on the
// current dataflow state.  Handles identifiers, field access on
// struct-typed receivers, and chained index expressions
// (`rows[i][j]`).  For unresolvable shapes (CallExpr, etc.) it
// returns false so we don't false-warn on map accesses we can't
// see through.
func (cg *CodeGen) indexReceiverTypeIsArrayLike(expr ast.Node, st *dfState) bool {
	t := cg.dfResolveType(expr, st)
	if t == nil {
		return false
	}

	return isArrayLikeType(t)
}

// dfResolveType walks an expression and returns its declared AST
// type when we can resolve it statically, or nil otherwise.
// Supported shapes:
//
//   - Identifier  -> st.types[name] (param, local, captured)
//   - FieldAccess -> recursively resolve receiver to a struct, then
//     look up the named field in structDeclsByName
//   - IndexExpr   -> recursively resolve receiver, peel one `[T]`
//     or pointer layer (chained indexing)
//
// CallExpr / overloads / generic instantiation are NOT resolved
// (would require a full type checker rerun); callers get nil and
// fall back to "don't warn" so map-like / opaque receivers stay
// silent.
func (cg *CodeGen) dfResolveType(expr ast.Node, st *dfState) ast.TypeExpr {
	switch e := expr.(type) {
	case *ast.Identifier:
		if t, ok := st.types[e.Name]; ok {
			return t
		}

		return nil

	case *ast.FieldAccess:
		recvT := cg.dfResolveType(e.Expr, st)
		if recvT == nil {
			return nil
		}

		structName := dfStructNameOf(recvT)
		if structName == "" {
			return nil
		}

		decl, ok := cg.structDeclsByName[structName]
		if !ok || decl == nil {
			return nil
		}

		for _, f := range decl.Fields {
			if f.Name == e.Field {
				return f.Type
			}
		}

		return nil

	case *ast.IndexExpr:
		recvT := cg.dfResolveType(e.Expr, st)
		if recvT == nil {
			return nil
		}
		// Peel one layer of array / pointer / string.
		switch t := recvT.(type) {
		case *ast.ArrayType:
			return t.Elem
		case *ast.PointerType:
			return t.Elem
		}

		return nil
	}

	return nil
}

// dfStructNameOf returns the struct name a TypeExpr refers to, or
// "" if it does not name a struct.  Resolves through pointer
// wrappers (`*Foo` -> "Foo") and bare names.
func dfStructNameOf(t ast.TypeExpr) string {
	switch n := t.(type) {
	case *ast.SimpleType:
		return n.Name
	case *ast.PointerType:
		return dfStructNameOf(n.Elem)
	case *ast.GenericType:
		return n.Name
	}

	return ""
}

// dfIsPointerLike reports whether t is a pointer or pointer-like
// type (raw `*T`, `*Trait`).  Used by dfCheckExpr's FieldAccess
// case to decide whether `s.field` warrants the
// -Wunchecked-nil-deref pedantic check.  Slices and arrays return
// false: their bounds story is the -Wunchecked-index regime, not
// the nil-deref regime.
func dfIsPointerLike(t ast.TypeExpr) bool {
	_, ok := t.(*ast.PointerType)

	return ok
}

// isArrayLikeType reports whether t is a built-in indexable type
// (array literal `[T; N]`, slice / fat-pointer `[T]`, string, raw
// pointer `*T`).  Excludes user struct types -- those route through
// `::index` overloads with `(V, bool)` semantics.
func isArrayLikeType(t ast.TypeExpr) bool {
	if t == nil {
		return false
	}

	switch n := t.(type) {
	case *ast.ArrayType:
		return true
	case *ast.PointerType:
		return true
	case *ast.SimpleType:
		return n.Name == "string"
	}

	return false
}

// dfInferTypeFromRHS attempts to recover the type of a let-binding
// from its initializer when the user did not write an explicit
// annotation.  Supported shapes:
//   - `[a, b, c]` literal -> *ast.ArrayType
//   - `[T; N]` typed array literal -> *ast.ArrayType
//   - `fn(...)` direct call -> looked up in cg.funcDecls for RetType
//   - `&x` address-of -> *ast.PointerType (element type left blank)
//
// Returns nil when the type cannot be inferred locally; the binding
// is then left untyped in dfState.types and the
// -Wunchecked-{index,nil-deref} checks skip it (conservative).
func (cg *CodeGen) dfInferTypeFromRHS(rhs ast.Node) ast.TypeExpr {
	switch n := rhs.(type) {
	case *ast.ArrayLit:
		// Bare literal -- element type unknown without full inference,
		// but the SHAPE is an array, which is all we need to tell
		// array-like from custom-index.  Synthesize a placeholder
		// element type; isArrayLikeType only checks the outer node
		// kind.
		_ = n

		return &ast.ArrayType{Elem: &ast.SimpleType{Name: ""}}

	case *ast.AddressOfExpr:
		// `&x` is always a pointer to whatever x is.  Element type
		// is left blank -- the dataflow only checks the OUTER kind
		// of the inferred type, so the placeholder suffices.
		return &ast.PointerType{Elem: &ast.SimpleType{Name: ""}}

	case *ast.CallExpr:
		// Direct call by simple-name: look up the callee's
		// declared return type.  Falls back to nil for method
		// calls, scope-qualified calls, generic templates, etc.
		// -- those require more machinery the dataflow pass
		// doesn't carry.
		id, ok := n.Func.(*ast.Identifier)
		if !ok {
			return nil
		}

		decl, ok := cg.funcDecls[id.Name]
		if !ok || decl == nil {
			return nil
		}

		return decl.RetType
	}

	return nil
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

// isDeinitCall returns the variable name being deinitialized when expr
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

// dfConstInt extracts an int64 from a constFact when its kind is cInt.
func dfConstInt(f constFact) (int64, bool) {
	if f.kind != cInt {
		return 0, false
	}

	return f.intVal, true
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
		// Strided modulo fold: when the left side has a tracked strided
		// interval all of whose members are exact multiples of the right
		// side's constant divisor, the result is provably 0. This is
		// what makes `if epoch % 500 == 0` fire bool-analysis inside
		// `for epoch = 0; ...; epoch += 500`.
		if e.Op == "%" {
			rv := cg.dfEval(e.Right, st)
			if n, ok := dfConstInt(rv); ok && n > 0 {
				if iv := cg.intervalOf(e.Left, st); iv.set && allMembersDivisibleBy(iv, n) {
					return cIntFact(0)
				}
			}
		}

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
			// Non-constant RHS: still record a flow-sensitive
			// upper-bound proof for `i < expr` / `i <= expr`,
			// even though we can't narrow the interval.  Matches
			// the user's intent ("I wrote an upper-bound guard")
			// for the -Wunchecked-index pedantic check.  The
			// classic pattern is `if i < len(arr): arr[i]`,
			// where `len(arr)` is a call and not constIntOf-able.
			if bin.Op == "<" || bin.Op == "<=" {
				thenSt.boundsChecked[id.Name] = true
			}

			return
		}

		name = id.Name
		c = iv.lo
		op = bin.Op
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		iv := constIntOf(bin.Left)
		if !iv.set {
			// Non-constant LHS: mirror image of the above.
			// `expr > i` / `expr >= i` imply an upper bound on i.
			if bin.Op == ">" || bin.Op == ">=" {
				thenSt.boundsChecked[id.Name] = true
			}

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

	// notZero narrowing: x != 0 -> then proves non-zero; x == 0 -> else
	// proves non-zero.  Also covers strict-sign guards (x > 0, x < 0
	// imply non-zero on the then side) and !=0-style relations against
	// a non-zero constant (x != c with c != 0 doesn't imply x != 0, so
	// only handle c == 0 here).  Used by dfCheckUncheckedDiv to silence
	// guarded divisions where the residual interval can't represent the
	// punctured range.
	switch op {
	case "!=":
		if c == 0 {
			thenSt.notZero[name] = true
		}
	case "==":
		if c == 0 {
			elseSt.notZero[name] = true
		}
	case ">":
		if c >= 0 {
			thenSt.notZero[name] = true
		}
	case ">=":
		if c > 0 {
			thenSt.notZero[name] = true
		}
	case "<":
		if c <= 0 {
			thenSt.notZero[name] = true
		}
	case "<=":
		if c < 0 {
			thenSt.notZero[name] = true
		}
	}

	// boundsChecked narrowing: `x < c` or `x <= c` with c > 0 records
	// that x has been compared against an upper bound, so the
	// pedantic -Wunchecked-index check stays silent for `arr[x]`
	// inside the then branch.  We don't verify c <= len(arr) -- the
	// goal is "did the user write an upper-bound guard," not "is the
	// guard sound" (since c could be a const < len(arr) too).
	switch op {
	case "<", "<=":
		if c > 0 {
			thenSt.boundsChecked[name] = true
		}
	}

	return
}

// constIntOf returns the IntLit value of an expression as a single-point
// interval, or unset if it's not a literal int.
func constIntOf(n ast.Node) interval {
	if il, ok := n.(*ast.IntLit); ok {
		return singletonIv(il.Value)
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

	// c±1 underflow / overflow at i64::MIN / i64::MAX would feed bogus
	// bounds into clipInterval and produce spurious "impossible range"
	// warnings.  Clamp instead -- at the boundary the resulting clip is
	// trivially the original interval (or empty), which is semantically
	// the correct narrowing.
	cMinus1, okM := subOverflow(c, 1)
	if !okM {
		cMinus1 = c
	}

	cPlus1, okP := addOverflow(c, 1)
	if !okP {
		cPlus1 = c
	}

	switch op {
	case "<":
		thenIv = clipInterval(x, x.lo, cMinus1)
		elseIv = clipInterval(x, c, x.hi)
	case "<=":
		thenIv = clipInterval(x, x.lo, c)
		elseIv = clipInterval(x, cPlus1, x.hi)
	case ">":
		thenIv = clipInterval(x, cPlus1, x.hi)
		elseIv = clipInterval(x, x.lo, c)
	case ">=":
		thenIv = clipInterval(x, c, x.hi)
		elseIv = clipInterval(x, x.lo, cMinus1)
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
// When x has a non-trivial stride s > 1, the result is shrunk so that
// new lo and hi remain at exact multiples-of-s offsets from x.lo (Reps
// et al. 2006: clipping a strided interval to a bound). This keeps
// `if epoch < 5000` narrowing of `for epoch = 0; ...; epoch += 500`
// produce {0, 4500, 500} rather than {0, 4999} stride-1, so the
// downstream `epoch % 500 == 0` fold can prove always-true.
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

	s := x.stride
	if s <= 1 {
		if lo == hi {
			return singletonIv(lo)
		}

		stride := s
		if stride <= 0 {
			stride = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: stride}
	}

	if rem := (lo - x.lo) % s; rem != 0 {
		lo += s - rem
	}

	if rem := (hi - x.lo) % s; rem != 0 {
		hi -= rem
	}

	if lo > hi {
		return interval{}
	}

	if lo == hi {
		return singletonIv(lo)
	}

	return interval{set: true, lo: lo, hi: hi, stride: s}
}
