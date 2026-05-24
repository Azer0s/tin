package codegen

import (
	"math"
	"math/big"

	"github.com/Azer0s/tin/ast"
)

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
// extending side is pushed to \pm\infty (int64 saturation) so the fixpoint
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
