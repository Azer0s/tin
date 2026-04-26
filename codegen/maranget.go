package codegen

// Pattern-matching usefulness and exhaustiveness analysis based on Luc
// Maranget's "Warnings for pattern matching" (Journal of Functional
// Programming, vol. 17 no. 3, 2007, pp. 387-421, doi:10.1017/S0956796807006223,
// available at http://moscova.inria.fr/~maranget/papers/warn/warn.pdf).
//
// We implement the paper's three primitives over a pattern matrix M:
//
//   Specialize(M, c) -- restrict M to rows whose head pattern can match the
//   constructor c, expanding the head into c's argument columns.
//
//   Default(M)       -- drop rows whose head is a concrete constructor; on
//   the remaining (wildcard-headed) rows, drop the head column.
//
//   Useful(M, q)     -- decide whether the pattern row q matches some value
//   that no row of M matches. By construction, an exhaustive match is one
//   where Useful(M, [_, _, ..., _]) is false.
//
// We use Useful for two purposes:
//   1. Reachability: arm i of an arm list is unreachable iff
//      Useful(M_<i, arm_i) is false, where M_<i is the matrix of the prior
//      arms. The user is warned per arm. Suppression: -Wno-unused-match-arms.
//   2. Exhaustiveness: a where-list or match is non-exhaustive iff
//      Useful(M_all, [_, _, ..., _]) is true, in which case the recursion's
//      final witness is a concrete value the arms fail to cover. We surface
//      that witness in the error message.
//
// The pattern shapes Tin currently expresses are:
//   - literals (i32/i64/.../f32/f64/string/bool/atom)         -> nullary constructors
//   - identifier binders, including `_` wildcard               -> Wild
//   - ArrayPattern with `regularCount` slots and an optional
//     `...rest` slot binding >= 1 element                       -> ArrShape ctor
//   - StructPattern Type{field: pat, ...}                       -> Struct ctor
//   - top-level TuplePattern for multi-arg dispatch             -> matrix columns
//
// Type-driven completeness: a column's "signature" (set of constructors that
// have appeared) is "complete" iff it covers every value the column's type
// can take. For finite domains we know completeness structurally:
//   - bool: complete iff both `true` and `false` literals appear.
//   - struct: single-constructor type, complete iff a struct pattern of that
//     name appears AND the fields are exhaustively covered.
//   - arrays: complete iff the union of exact-length arms and rest-arms
//     covers every length in {0, 1, 2, ...} (equivalently, there is some
//     ArrRest with prefix N and exact arms cover every length 0..N-1).
// For open domains (i32, f64, string, atom) the signature is never complete;
// we fall through to the Default case in the algorithm. (Tin atoms are
// observationally infinite even when only a few are used in practice; if
// closed-set atom completeness is desired in the future, attach a
// per-`use` enum table here.)
//
// Witness generation reconstructs a concrete uncovered value from the
// recursion: when the algorithm decides a wildcard is useful at a column
// because the signature is incomplete, the witness adopts a "missing"
// constructor for that column; otherwise it adopts the constructor whose
// specialised submatrix produced the witness. See witnessFor.

import (
	"fmt"
	"os"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// ----------------------------------------------------------------------------
// Pattern representation
// ----------------------------------------------------------------------------

type mPat interface {
	mIsPat()
}

type mWild struct{} // identifier binder OR `_`; matches everything

type mLit struct {
	kind     mLitKind
	intVal   int64
	floatVal float64
	strVal   string
	boolVal  bool
	atomVal  string
}

// mOpaque is a tagged opaque pattern: the algorithm treats two opaques with
// the same tag as identical (for the duplicate-arm warning to fire) and
// otherwise as distinct (so two different opaque patterns are not falsely
// flagged unreachable). Used for AST shapes Tin can match on but that we
// don't structurally analyze: enum member access (`direction.north`),
// scope access, etc. The tag is the source-text rendering of the node.
type mOpaque struct {
	tag string
}

// mArr matches an array of either exact length len(elems) (when !hasRest) or
// length >= len(elems)+1 (when hasRest). The element patterns are stored;
// the rest sub-pattern is currently always treated as Wild for exhaustiveness
// purposes (Tin's `...xs` only binds the slice without further constraining
// it, beyond the >=1 length already encoded by hasRest).
type mArr struct {
	elems   []mPat
	hasRest bool
}

// mStruct matches a struct value of name typeName whose fields all match the
// listed field patterns. Field order is canonicalised to the struct's
// declared order so two arms that mention the same fields in different
// source orders produce identical column-shape ctors.
type mStruct struct {
	typeName string
	// fields is keyed by field name; missing fields are implicitly Wild.
	fields map[string]mPat
}

// mData matches an ADT variant (`case Ok(v):`, `case EmptyInput:`). args is
// the positional list of sub-patterns (nullary variants have len(args)==0).
// adtName is the concrete ADT type name (e.g. "Option__i64") and variant is
// the variant name ("Some", "Ok", ...).
type mData struct {
	adtName string
	variant string
	args    []mPat
}

func (mWild) mIsPat()    {}
func (*mLit) mIsPat()    {}
func (*mArr) mIsPat()    {}
func (*mStruct) mIsPat() {}
func (*mOpaque) mIsPat() {}
func (*mData) mIsPat()   {}

type mLitKind int

const (
	mLitInt mLitKind = iota
	mLitFloat
	mLitString
	mLitBool
	mLitAtom
)

// ----------------------------------------------------------------------------
// AST -> mPat conversion
// ----------------------------------------------------------------------------

// nodeToMPat converts an AST pattern node into the matrix-friendly
// representation. Unknown shapes become mWild conservatively (treats the
// arm as catch-everything, suppressing both unreachable and non-exhaustive
// reports for that column rather than producing false positives).
func (cg *CodeGen) nodeToMPat(n ast.Node) mPat {
	switch p := n.(type) {
	case nil:
		return mWild{}
	case *ast.Identifier:
		// A bare identifier is a binder (wildcard) UNLESS it names a
		// nullary ADT variant, in which case it is a data constructor.
		if cg != nil && cg.isDataVariant(p.Name) {
			adt := cg.adtForVariant(p.Name)

			return &mData{adtName: adt, variant: p.Name}
		}

		return mWild{}
	case *ast.IntLit:
		return &mLit{kind: mLitInt, intVal: p.Value}
	case *ast.FloatLit:
		return &mLit{kind: mLitFloat, floatVal: p.Value}
	case *ast.StringLit:
		return &mLit{kind: mLitString, strVal: p.Value}
	case *ast.BoolLit:
		return &mLit{kind: mLitBool, boolVal: p.Value}
	case *ast.AtomLit:
		return &mLit{kind: mLitAtom, atomVal: p.Name}
	case *ast.CallExpr:
		// ADT constructor pattern: `case Ok(v):`, `case Malformed(p, m):`.
		if id, ok := p.Func.(*ast.Identifier); ok && cg != nil && cg.isDataVariant(id.Name) {
			adt := cg.adtForVariant(id.Name)
			args := make([]mPat, len(p.Args))

			for i, a := range p.Args {
				args[i] = cg.nodeToMPat(a)
			}

			return &mData{adtName: adt, variant: id.Name, args: args}
		}
	case *ast.ArrayPattern:
		var elems []mPat

		hasRest := false

		for _, e := range p.Elems {
			if e.IsRest {
				hasRest = true

				continue
			}
			// Tin's array patterns currently only name slots, so each regular
			// element is a binder/wild. Treat as Wild for exhaustiveness.
			elems = append(elems, mWild{})
		}

		return &mArr{elems: elems, hasRest: hasRest}
	case *ast.StructPattern:
		// Struct patterns can mix named (field: pat) and positional (_)
		// slots. Resolving positionals to field names needs the struct's
		// declared field order, which the checker doesn't have access to
		// here. Treat as opaque-by-source-text so duplicates are still
		// detected but distinct patterns are NOT falsely flagged
		// unreachable. Improving struct pattern reasoning is tracked as
		// part of Slice 2 / future Maranget work.
		return &mOpaque{tag: opaqueTag(p)}
	}

	// Unrecognized pattern shape. Treat as an opaque tagged constructor so
	// distinct AST forms don't collapse into a single "always-matches"
	// wildcard (which would falsely flag enum-member arms as unreachable).
	return &mOpaque{tag: opaqueTag(n)}
}

// adtForVariant picks a concrete registered ADT name that declares the given
// variant. When a variant name is shared by multiple ADTs (e.g. a generic
// `Result` monomorphised into multiple instances), we prefer a variant lookup
// entry that looks concrete (no unresolved type-parameter suffix). The name
// is only used for completeness bookkeeping; codegen resolves the ADT from
// the scrutinee's type independently.
func (cg *CodeGen) adtForVariant(variant string) string {
	if cg == nil {
		return ""
	}

	adts := cg.dataVariantLookup[variant]
	if len(adts) == 0 {
		return ""
	}

	return adts[0]
}

// opaqueTag renders an AST node into a stable string used as the equality
// key for opaque constructors. Different concrete patterns must produce
// different strings.
func opaqueTag(n ast.Node) string {
	if n == nil {
		return "<nil>"
	}

	return ast.PrintExpr(n)
}

// armRow turns a where-clause / match-case into a single-row pattern row
// (one column for single-arg, N columns for multi-arg via top-level
// TuplePattern). guarded == true makes the row Wild^N for the priors-cover
// check (a guarded arm cannot be relied on to cover anything) but the arm
// itself is always considered useful (we cannot prove it dead).
func (cg *CodeGen) wherePatternToRow(c ast.WhereClause, arity int) (row []mPat, guarded bool) {
	if c.Guard != nil {
		guarded = true
	}

	if c.Pattern == nil {
		// Bare `where _:` or bool-guard. In both cases the arm has no
		// pattern; treat as Wild^arity. Bool guards are also flagged
		// guarded so they don't shadow subsequent arms.
		if c.Cond != nil && c.Pattern == nil {
			guarded = true
		}

		row = wildRow(arity)

		return
	}

	if tp, ok := c.Pattern.(*ast.TuplePattern); ok {
		if len(tp.Elems) != arity {
			row = wildRow(arity)

			return
		}

		row = make([]mPat, arity)
		for i, e := range tp.Elems {
			row[i] = cg.nodeToMPat(e)
		}

		return
	}

	if arity != 1 {
		// Arity mismatch; codegen reports this separately. Fall back to wilds.
		row = wildRow(arity)

		return
	}

	row = []mPat{cg.nodeToMPat(c.Pattern)}

	return
}

func (cg *CodeGen) matchCaseToRow(c ast.MatchCase) (row []mPat, guarded bool) {
	if c.Guard != nil {
		guarded = true
	}

	if c.VarType != nil {
		// case x T: -- runtime type test, not structurally analysable.
		row = []mPat{mWild{}}
		guarded = true

		return
	}

	if c.Pattern == nil {
		row = []mPat{mWild{}}

		return
	}

	if tp, ok := c.Pattern.(*ast.TuplePattern); ok {
		row = make([]mPat, len(tp.Elems))
		for i, e := range tp.Elems {
			row[i] = cg.nodeToMPat(e)
		}

		return
	}

	row = []mPat{cg.nodeToMPat(c.Pattern)}

	return
}

func wildRow(n int) []mPat {
	r := make([]mPat, n)
	for i := range r {
		r[i] = mWild{}
	}

	return r
}

// ----------------------------------------------------------------------------
// Constructors and column signatures
// ----------------------------------------------------------------------------

// mCtor identifies a constructor head for column 0 of a matrix.
//
// Literals carry their value so equal-valued literals are the same
// constructor (and thus a duplicate `case 0: case 0:` is detected).
//
// Array shapes are split: an exact-length array of N is a different
// constructor from an N-prefix-with-rest array (the latter additionally
// covers every longer length).
//
// Struct constructors are keyed by typeName; arity is the field count, and
// field names are recorded so Specialize can canonicalise field order.
type mCtor struct {
	kind      mCtorKind
	litKind   mLitKind
	intVal    int64
	floatVal  float64
	strVal    string
	boolVal   bool
	atomVal   string
	arrN      int  // for arrExact and arrRest: the regular slot count
	arrRest   bool // true iff this constructor includes a rest slot
	stName    string
	stFields  []string // canonical field-name order
	opaqueTag string   // for mcOpaque
	adtName   string   // for mcData: concrete ADT type name
	variant   string   // for mcData: variant name
	adtArity  int      // for mcData: number of positional payload fields
}

type mCtorKind int

const (
	mcLit mCtorKind = iota
	mcArrExact
	mcArrRest
	mcStruct
	mcOpaque
	mcData
)

// arity returns the number of columns Specialize will produce when
// expanding a row whose head matches this constructor.
func (c mCtor) arity() int {
	switch c.kind {
	case mcLit:
		return 0
	case mcArrExact:
		return c.arrN
	case mcArrRest:
		// Rest binding is opaque to the algorithm (its element patterns
		// are not currently constrained beyond the >= 1 length encoded by
		// the constructor itself), so we do NOT expose it as a column.
		return c.arrN
	case mcStruct:
		return len(c.stFields)
	case mcOpaque:
		return 0
	case mcData:
		return c.adtArity
	}

	return 0
}

func (c mCtor) eq(o mCtor) bool {
	if c.kind != o.kind {
		return false
	}

	switch c.kind {
	case mcLit:
		if c.litKind != o.litKind {
			return false
		}

		switch c.litKind {
		case mLitInt:
			return c.intVal == o.intVal
		case mLitFloat:
			return c.floatVal == o.floatVal
		case mLitString:
			return c.strVal == o.strVal
		case mLitBool:
			return c.boolVal == o.boolVal
		case mLitAtom:
			return c.atomVal == o.atomVal
		}
	case mcArrExact, mcArrRest:
		return c.arrN == o.arrN && c.arrRest == o.arrRest
	case mcStruct:
		return c.stName == o.stName
	case mcOpaque:
		return c.opaqueTag == o.opaqueTag
	case mcData:
		return c.adtName == o.adtName && c.variant == o.variant
	}

	return false
}

// headCtor returns the constructor for the head pattern of a row, or nil if
// the head is Wild. (Used to enumerate column-0 constructors of a matrix.)
func headCtor(p mPat) *mCtor {
	switch v := p.(type) {
	case mWild:
		return nil
	case *mLit:
		c := &mCtor{kind: mcLit, litKind: v.kind, intVal: v.intVal, floatVal: v.floatVal, strVal: v.strVal, boolVal: v.boolVal, atomVal: v.atomVal}

		return c
	case *mArr:
		if v.hasRest {
			return &mCtor{kind: mcArrRest, arrN: len(v.elems), arrRest: true}
		}

		return &mCtor{kind: mcArrExact, arrN: len(v.elems)}
	case *mStruct:
		names := make([]string, 0, len(v.fields))
		for k := range v.fields {
			names = append(names, k)
		}
		// Stable order: ascending field name. Witness pretty-printer uses
		// the same ordering so witnesses are deterministic.
		sortStrings(names)

		return &mCtor{kind: mcStruct, stName: v.typeName, stFields: names}
	case *mOpaque:
		return &mCtor{kind: mcOpaque, opaqueTag: v.tag}
	case *mData:
		return &mCtor{kind: mcData, adtName: v.adtName, variant: v.variant, adtArity: len(v.args)}
	}

	return nil
}

// columnCtors returns the distinct constructors appearing in column 0 of M.
func columnCtors(M [][]mPat) []mCtor {
	var out []mCtor

	for _, row := range M {
		c := headCtor(row[0])
		if c == nil {
			continue
		}

		dup := false

		for i := range out {
			if out[i].eq(*c) {
				dup = true

				break
			}
		}

		if !dup {
			out = append(out, *c)
		}
	}

	return out
}

// completeSignature decides whether the constructors in column 0 of M
// exhaust the column's domain. When incomplete, it returns a "missing"
// constructor that witness building uses to construct a concrete uncovered
// value at this column.
func (cg *CodeGen) completeSignature(ctors []mCtor) (complete bool, missing mCtor) {
	if len(ctors) == 0 {
		// No concrete head appears in column 0 - either the matrix is
		// empty, or every prior row has a wildcard at column 0. In either
		// case the column has no concrete coverage, so the signature is
		// INcomplete: the algorithm should fall through to Default(M),
		// which preserves correctness when M is empty (-> useful) or
		// wildcard-only (-> already covered by Default). Witness
		// placeholder is `_` (rendered as the empty witness).
		return false, mCtor{kind: mcLit, litKind: mLitInt, intVal: 0}
	}
	// Determine the kind family represented in column 0. Array exact and
	// array-with-rest are the same family for completeness purposes.
	family := func(k mCtorKind) mCtorKind {
		if k == mcArrRest {
			return mcArrExact
		}

		return k
	}

	first := ctors[0]
	mixed := false

	for _, c := range ctors[1:] {
		if family(c.kind) != family(first.kind) {
			mixed = true

			break
		}
	}

	if mixed {
		// Mixing scalar literals with array shapes etc. shouldn't happen
		// at the same column for well-typed code; treat as incomplete.
		return false, missingFor(first)
	}

	switch first.kind {
	case mcLit:
		// Bool is the only scalar literal kind that can be exhausted.
		if first.litKind == mLitBool {
			seenT := false
			seenF := false

			for _, c := range ctors {
				if c.litKind != mLitBool {
					return false, missingFor(first)
				}

				if c.boolVal {
					seenT = true
				} else {
					seenF = true
				}
			}

			if seenT && seenF {
				return true, missing
			}

			missing = mCtor{kind: mcLit, litKind: mLitBool, boolVal: !seenT}

			return false, missing
		}
		// Integer / float / string / atom: open domains.
		return false, missingFor(first)

	case mcArrExact, mcArrRest:
		exact := map[int]bool{}

		var restMins []int

		for _, c := range ctors {
			if c.kind == mcArrExact {
				exact[c.arrN] = true
			} else {
				restMins = append(restMins, c.arrN+1)
			}
		}

		if exhausted, witnessLen := arrayDomainExhaustedWithWitness(exact, restMins); exhausted {
			return true, missing
		} else {
			missing = mCtor{kind: mcArrExact, arrN: witnessLen}

			return false, missing
		}

	case mcStruct:
		// One constructor for the struct type. We're "complete" at this
		// column IF a struct pattern of this type appears (it does, since
		// `ctors` is non-empty here). Sub-completeness of the fields is
		// handled by the recursion on the expanded columns.
		return true, missing

	case mcOpaque:
		// Opaque tagged constructors come from AST shapes the algorithm
		// doesn't know how to enumerate (enum members, scope access, ...).
		// We can never prove the column is exhausted, so always treat as
		// incomplete; the witness reuses one of the seen tags as a
		// placeholder.
		return false, missingFor(first)

	case mcData:
		// ADT completeness: the column is complete iff the seen variants
		// cover every variant declared by the ADT. All ctors at this column
		// must refer to the same ADT type. cg.dataVariants[adtName] is the
		// authoritative variant set.
		adtName := first.adtName
		seen := make(map[string]bool, len(ctors))

		for _, c := range ctors {
			if c.kind != mcData {
				return false, missingFor(first)
			}

			if c.adtName != adtName {
				return false, missingFor(first)
			}

			seen[c.variant] = true
		}

		if cg == nil {
			return false, missingFor(first)
		}

		variants := cg.dataVariants[adtName]
		if variants == nil {
			return false, missingFor(first)
		}

		for vname := range variants {
			if !seen[vname] {
				missing = mCtor{
					kind: mcData, adtName: adtName,
					variant: vname, adtArity: len(variants[vname].Fields),
				}

				return false, missing
			}
		}

		return true, missing
	}

	return false, missingFor(first)
}

// missingFor returns a placeholder "missing" constructor for an open-domain
// column, used to build a sample witness value.
func missingFor(any mCtor) mCtor {
	switch any.kind {
	case mcLit:
		switch any.litKind {
		case mLitInt:
			return mCtor{kind: mcLit, litKind: mLitInt, intVal: any.intVal + 1}
		case mLitFloat:
			return mCtor{kind: mcLit, litKind: mLitFloat, floatVal: any.floatVal + 1}
		case mLitString:
			return mCtor{kind: mcLit, litKind: mLitString, strVal: "<other-string>"}
		case mLitAtom:
			return mCtor{kind: mcLit, litKind: mLitAtom, atomVal: "<other-atom>"}
		case mLitBool:
			return mCtor{kind: mcLit, litKind: mLitBool, boolVal: !any.boolVal}
		}
	case mcArrExact:
		return mCtor{kind: mcArrExact, arrN: any.arrN + 1}
	case mcArrRest:
		return mCtor{kind: mcArrExact, arrN: 0}
	case mcStruct:
		return mCtor{kind: mcStruct, stName: any.stName, stFields: any.stFields}
	case mcOpaque:
		return mCtor{kind: mcOpaque, opaqueTag: "<other-" + any.opaqueTag + ">"}
	case mcData:
		// Fallback when completeSignature couldn't resolve the ADT. The
		// witness-printer just needs some variant name that is clearly not
		// one of the seen ones.
		return mCtor{kind: mcData, adtName: any.adtName, variant: "<other-" + any.variant + ">"}
	}

	return mCtor{kind: mcLit, litKind: mLitInt}
}

// arrayDomainExhaustedWithWitness extends arrayDomainExhausted with a
// witness length: when the domain is incomplete, returns the smallest
// uncovered length.
func arrayDomainExhaustedWithWitness(exact map[int]bool, restMins []int) (bool, int) {
	if len(restMins) == 0 {
		// Without a rest, we cannot cover infinitely many lengths. The
		// witness is the smallest length not in `exact`.
		for n := 0; ; n++ {
			if !exact[n] {
				return false, n
			}
		}
	}

	smallestRest := restMins[0]
	for _, m := range restMins[1:] {
		if m < smallestRest {
			smallestRest = m
		}
	}
	// Every length below the smallest rest must be covered by an exact arm.
	for n := 0; n < smallestRest; n++ {
		if !exact[n] {
			return false, n
		}
	}

	return true, 0
}

// ----------------------------------------------------------------------------
// Specialize, Default
// ----------------------------------------------------------------------------

// specialize implements S(c, M) from Maranget §3.1.
func specialize(M [][]mPat, c mCtor) [][]mPat {
	a := c.arity()

	var out [][]mPat

	for _, row := range M {
		head, rest := row[0], row[1:]

		switch p := head.(type) {
		case mWild:
			expanded := append(wildRow(a), rest...)
			out = append(out, expanded)
		case *mLit:
			if c.kind != mcLit || c.litKind != p.kind {
				continue
			}

			match := false

			switch p.kind {
			case mLitInt:
				match = c.intVal == p.intVal
			case mLitFloat:
				match = c.floatVal == p.floatVal
			case mLitString:
				match = c.strVal == p.strVal
			case mLitBool:
				match = c.boolVal == p.boolVal
			case mLitAtom:
				match = c.atomVal == p.atomVal
			}

			if !match {
				continue
			}

			out = append(out, append([]mPat{}, rest...))
		case *mArr:
			// Match against ArrExact or ArrRest constructor. An exact-N row
			// matches an ArrExact-N constructor exactly. A rest-N row
			// matches ArrExact-K for K >= N+1 and any ArrRest-K for K <= N
			// or K == N. Following Maranget for inductive list shapes:
			switch c.kind { //nolint:exhaustive // mcLit/mcStruct/mcOpaque cannot match an *mArr row
			case mcArrExact:
				if !p.hasRest {
					if len(p.elems) != c.arrN {
						continue
					}
					// Expand: row's element wildcards become the new columns.
					exp := append([]mPat{}, p.elems...)
					out = append(out, append(exp, rest...))
				} else {
					// Rest-row matches ArrExact-K iff K >= len(p.elems)+1
					// (the rest captures the extra K - len(p.elems) >= 1
					// elements). Expand: regular elements + wildcards for
					// the tail elements.
					if c.arrN < len(p.elems)+1 {
						continue
					}

					exp := append([]mPat{}, p.elems...)
					for i := len(p.elems); i < c.arrN; i++ {
						exp = append(exp, mWild{})
					}

					out = append(out, append(exp, rest...))
				}
			case mcArrRest:
				if !p.hasRest {
					// Exact-K row matches ArrRest-N iff... it cannot. An
					// ArrRest-N constructor is "every length >= N+1"; an
					// exact-K row matches one specific length K. They
					// cannot be unified into a single constructor expansion
					// here (the algorithm would lose precision). Drop.
					//
					// Note: this is a known approximation; alternatively
					// the column could be split into one mcArrExact-K
					// constructor per discrete K and the rest-arms would
					// then specialise into each. For Tin's typical pattern
					// shapes this approximation only leaks usefulness in
					// arms that mix exact and rest patterns at the same
					// column, which is rare.
					continue
				}

				if len(p.elems) > c.arrN {
					continue
				}

				exp := append([]mPat{}, p.elems...)
				for i := len(p.elems); i < c.arrN; i++ {
					exp = append(exp, mWild{})
				}

				out = append(out, append(exp, rest...))
			}
		case *mStruct:
			if c.kind != mcStruct || c.stName != p.typeName {
				continue
			}
			// Expand fields in the constructor's canonical order.
			exp := make([]mPat, len(c.stFields))
			for i, fname := range c.stFields {
				if pat, ok := p.fields[fname]; ok {
					exp[i] = pat
				} else {
					exp[i] = mWild{}
				}
			}

			out = append(out, append(exp, rest...))
		case *mOpaque:
			if c.kind != mcOpaque || c.opaqueTag != p.tag {
				continue
			}

			out = append(out, append([]mPat{}, rest...))
		case *mData:
			if c.kind != mcData || c.adtName != p.adtName || c.variant != p.variant {
				continue
			}
			// Expand the variant's positional payload fields as new columns.
			exp := make([]mPat, c.adtArity)
			for i := 0; i < c.adtArity; i++ {
				if i < len(p.args) {
					exp[i] = p.args[i]
				} else {
					exp[i] = mWild{}
				}
			}

			out = append(out, append(exp, rest...))
		}
	}

	return out
}

// defaultMatrix implements D(M) from Maranget §3.1.
func defaultMatrix(M [][]mPat) [][]mPat {
	var out [][]mPat

	for _, row := range M {
		if _, ok := row[0].(mWild); ok {
			out = append(out, append([]mPat{}, row[1:]...))
		}
	}

	return out
}

// ----------------------------------------------------------------------------
// Useful + witness construction
// ----------------------------------------------------------------------------

// witnessVal is a recovered concrete value uncovered by a matrix; built
// during the Useful recursion so the caller can pretty-print it.
type witnessVal struct {
	ctor mCtor
	args []*witnessVal // children corresponding to the constructor's arity
}

// useful decides whether row q matches some value not covered by any row of
// M. When q is fully wild, this is the inverse of exhaustiveness. The
// witness, when q is found useful, is a concrete value q matches and no row
// of M does.
func (cg *CodeGen) useful(M [][]mPat, q []mPat) (bool, *witnessVal) {
	// Base cases following Maranget §3.1.
	if len(q) == 0 {
		// The matrix has no rows iff the empty pattern is useful. (No
		// remaining rows means none of them could "see" the empty value.)
		return len(M) == 0, &witnessVal{}
	}
	// Are all matrix rows already empty in column 0? That can happen only
	// if previous specializations consumed all columns; under Tin's usage
	// it shouldn't actually occur but we guard for shape safety.
	for _, row := range M {
		if len(row) == 0 {
			return false, nil
		}
	}

	head := q[0]
	rest := q[1:]

	switch p := head.(type) {
	case mWild:
		ctors := columnCtors(M)
		complete, missing := cg.completeSignature(ctors)

		if complete {
			// Try each constructor: q is useful iff any specialised
			// submatrix admits a useful wildcard row at the prepended
			// columns.
			for _, c := range ctors {
				a := c.arity()
				expanded := append(wildRow(a), rest...)

				ok, w := cg.useful(specialize(M, c), expanded)
				if ok {
					return true, prependChildren(c, w)
				}
			}

			return false, nil
		}
		// Incomplete signature: the witness uses the missing constructor
		// for column 0 and recurses on the default submatrix for the rest.
		ok, w := cg.useful(defaultMatrix(M), rest)
		if !ok {
			return false, nil
		}

		head := &witnessVal{ctor: missing}
		// missing constructor's args are unspecified - fill with wilds
		// (rendered as `_` in the witness pretty-print).
		for i := 0; i < missing.arity(); i++ {
			head.args = append(head.args, &witnessVal{})
		}

		w.ctor = head.ctor
		w.args = head.args

		// The remainder of the witness was built by the recursive call but
		// only describes columns 1..N. We rebuild a fresh witness of the
		// full shape: missing for column 0, then whatever the recursion
		// produced for columns 1..N. The recursion's witness IS the
		// columns 1..N value.
		return true, prependWitness(missing, w)

	case *mLit:
		c := mCtor{kind: mcLit, litKind: p.kind, intVal: p.intVal, floatVal: p.floatVal, strVal: p.strVal, boolVal: p.boolVal, atomVal: p.atomVal}

		ok, w := cg.useful(specialize(M, c), rest)
		if !ok {
			return false, nil
		}

		return true, prependWitness(c, w)

	case *mArr:
		var c mCtor

		if p.hasRest {
			c = mCtor{kind: mcArrRest, arrN: len(p.elems), arrRest: true}
		} else {
			c = mCtor{kind: mcArrExact, arrN: len(p.elems)}
		}

		expanded := append(append([]mPat{}, p.elems...), rest...)

		ok, w := cg.useful(specialize(M, c), expanded)
		if !ok {
			return false, nil
		}

		return true, prependWitness(c, w)

	case *mStruct:
		names := make([]string, 0, len(p.fields))
		for k := range p.fields {
			names = append(names, k)
		}

		sortStrings(names)

		c := mCtor{kind: mcStruct, stName: p.typeName, stFields: names}
		expanded := make([]mPat, 0, len(names)+len(rest))

		for _, fname := range names {
			expanded = append(expanded, p.fields[fname])
		}

		expanded = append(expanded, rest...)

		ok, w := cg.useful(specialize(M, c), expanded)
		if !ok {
			return false, nil
		}

		return true, prependWitness(c, w)

	case *mOpaque:
		c := mCtor{kind: mcOpaque, opaqueTag: p.tag}

		ok, w := cg.useful(specialize(M, c), rest)
		if !ok {
			return false, nil
		}

		return true, prependWitness(c, w)

	case *mData:
		c := mCtor{kind: mcData, adtName: p.adtName, variant: p.variant, adtArity: len(p.args)}
		expanded := append(append([]mPat{}, p.args...), rest...)

		ok, w := cg.useful(specialize(M, c), expanded)
		if !ok {
			return false, nil
		}

		return true, prependWitness(c, w)
	}

	return true, nil
}

// prependWitness constructs a witness for a row whose head matches
// constructor c, given the witness returned by the recursive call. We
// adopt the convention that the recursion returns args for the columns
// 0..arity(c)-1 first and the tail columns after; we slice off the leading
// `arity(c)` to attach as c's children.
func prependWitness(c mCtor, rest *witnessVal) *witnessVal {
	a := c.arity()
	w := &witnessVal{ctor: c}

	if rest == nil {
		for i := 0; i < a; i++ {
			w.args = append(w.args, &witnessVal{})
		}

		return w
	}

	if len(rest.args) >= a {
		w.args = append(w.args, rest.args[:a]...)
	} else {
		for i := 0; i < a; i++ {
			w.args = append(w.args, &witnessVal{})
		}
	}

	return w
}

// prependChildren is used when the algorithm specialised on every
// constructor of a complete signature (the wildcard branch). The witness
// returned by the recursive useful call already holds args for the
// specialised columns + the rest columns; we strip the first `a` of them
// and attach them as children of c, then keep the tail columns as siblings.
func prependChildren(c mCtor, rest *witnessVal) *witnessVal {
	a := c.arity()
	w := &witnessVal{ctor: c}

	if rest == nil {
		for i := 0; i < a; i++ {
			w.args = append(w.args, &witnessVal{})
		}

		return w
	}

	if len(rest.args) >= a {
		w.args = append(w.args, rest.args[:a]...)
	} else {
		for i := 0; i < a; i++ {
			w.args = append(w.args, &witnessVal{})
		}
	}

	return w
}

// ----------------------------------------------------------------------------
// Witness pretty-printing
// ----------------------------------------------------------------------------

func (w *witnessVal) String() string {
	if w == nil {
		return "_"
	}
	// Empty witness from the base case (no constructor selected) is the
	// wildcard placeholder.
	if w.ctor.kind == mcLit && w.ctor.litKind == mLitInt && w.ctor.intVal == 0 && len(w.args) == 0 {
		return "_"
	}

	switch w.ctor.kind {
	case mcLit:
		switch w.ctor.litKind {
		case mLitInt:
			return fmt.Sprintf("%d", w.ctor.intVal)
		case mLitFloat:
			return fmt.Sprintf("%g", w.ctor.floatVal)
		case mLitString:
			return fmt.Sprintf("%q", w.ctor.strVal)
		case mLitBool:
			if w.ctor.boolVal {
				return "true"
			}

			return "false"
		case mLitAtom:
			return "'" + w.ctor.atomVal
		}

		return "_"
	case mcArrExact:
		parts := make([]string, len(w.args))
		for i, a := range w.args {
			parts[i] = a.String()
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case mcArrRest:
		parts := make([]string, 0, len(w.args)+1)
		for _, a := range w.args {
			parts = append(parts, a.String())
		}

		parts = append(parts, "...")

		return "[" + strings.Join(parts, ", ") + "]"
	case mcStruct:
		fields := make([]string, len(w.ctor.stFields))
		for i, name := range w.ctor.stFields {
			val := "_"
			if i < len(w.args) {
				val = w.args[i].String()
			}

			fields[i] = name + ": " + val
		}

		return w.ctor.stName + "{" + strings.Join(fields, ", ") + "}"
	case mcOpaque:
		return w.ctor.opaqueTag
	case mcData:
		if len(w.args) == 0 {
			return w.ctor.variant
		}

		parts := make([]string, len(w.args))
		for i, a := range w.args {
			parts[i] = a.String()
		}

		return w.ctor.variant + "(" + strings.Join(parts, ", ") + ")"
	}

	return "_"
}

// ----------------------------------------------------------------------------
// Public entry points
// ----------------------------------------------------------------------------

// marangetCheckWhereExhaustive returns (ok, witnessString). When ok is false
// the witness describes a value the where-list fails to cover.
func (cg *CodeGen) marangetCheckWhereExhaustive(wl *ast.WhereList, arity int) (bool, string) {
	M := cg.buildWhereMatrix(wl, arity)

	q := wildRow(arity)

	ok, w := cg.useful(M, q)
	if !ok {
		return true, ""
	}

	return false, witnessRowString(w, arity)
}

// marangetCheckMatchExhaustive runs the same analysis on a match statement.
// arity is 1 for normal match (the scrutinee).
func (cg *CodeGen) marangetCheckMatchExhaustive(s *ast.MatchStmt) (bool, string) {
	if s == nil {
		return true, ""
	}

	if s.Default != nil {
		return true, ""
	}

	M := cg.buildMatchMatrix(s)

	q := wildRow(1)

	ok, w := cg.useful(M, q)
	if !ok {
		return true, ""
	}

	return false, witnessRowString(w, 1)
}

// marangetWhereArmUseful: is arm i of wl useful w.r.t. arms 0..i-1?
func (cg *CodeGen) marangetWhereArmUseful(wl *ast.WhereList, idx, arity int) bool {
	row, guarded := cg.wherePatternToRow(wl.Clauses[idx], arity)
	if guarded {
		return true
	}

	M := make([][]mPat, 0, idx)

	for i := 0; i < idx; i++ {
		r, g := cg.wherePatternToRow(wl.Clauses[i], arity)
		if g {
			continue // guarded prior cannot shadow anything
		}

		M = append(M, r)
	}

	ok, _ := cg.useful(M, row)

	return ok
}

func (cg *CodeGen) marangetMatchArmUseful(s *ast.MatchStmt, idx int) bool {
	row, guarded := cg.matchCaseToRow(s.Cases[idx])
	if guarded {
		return true
	}

	M := make([][]mPat, 0, idx)

	for i := 0; i < idx; i++ {
		r, g := cg.matchCaseToRow(s.Cases[i])
		if g {
			continue
		}

		M = append(M, r)
	}

	ok, _ := cg.useful(M, row)

	return ok
}

func (cg *CodeGen) marangetMatchDefaultUseful(s *ast.MatchStmt) bool {
	M := cg.buildMatchMatrix(s)

	q := wildRow(1)

	ok, _ := cg.useful(M, q)

	return ok
}

// buildWhereMatrix builds a matrix from a where-list, omitting guarded
// arms (they cannot be relied on to cover anything when assessing
// reachability of subsequent arms, and when assessing exhaustiveness they
// are similarly dropped because the catch-all rule already requires an
// unguarded catch-all).
func (cg *CodeGen) buildWhereMatrix(wl *ast.WhereList, arity int) [][]mPat {
	if wl == nil {
		return nil
	}

	var M [][]mPat

	for _, c := range wl.Clauses {
		row, guarded := cg.wherePatternToRow(c, arity)
		if guarded {
			continue
		}

		M = append(M, row)
	}

	return M
}

func (cg *CodeGen) buildMatchMatrix(s *ast.MatchStmt) [][]mPat {
	var M [][]mPat

	for _, c := range s.Cases {
		row, guarded := cg.matchCaseToRow(c)
		if guarded {
			continue
		}

		M = append(M, row)
	}

	return M
}

// witnessRowString renders a witness row of the given arity. For arity 1
// the witness is a single value (e.g. `[]`); for arity > 1 we render as a
// tuple (e.g. `(1, "hi")`).
func witnessRowString(w *witnessVal, arity int) string {
	if arity <= 1 {
		return w.String()
	}
	// w holds the witness for column 0; subsequent columns' witnesses are
	// chained via the rest. In our representation prependWitness puts
	// columns 1..N into rest.args[a:], but we don't carry them explicitly
	// across the recursion in this implementation - so multi-column
	// witnesses pretty-print as `(<col0>, ...)`.
	return "(" + w.String() + ", ...)"
}

// sortStrings is a small helper to avoid importing "sort" at top of file.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// ----------------------------------------------------------------------------
// Debug dumping (-v-match-info)
// ----------------------------------------------------------------------------

// patStr renders an mPat for debug dumps.
func patStr(p mPat) string {
	switch v := p.(type) {
	case mWild:
		return "_"
	case *mLit:
		switch v.kind {
		case mLitInt:
			return fmt.Sprintf("%d", v.intVal)
		case mLitFloat:
			return fmt.Sprintf("%g", v.floatVal)
		case mLitString:
			return fmt.Sprintf("%q", v.strVal)
		case mLitBool:
			if v.boolVal {
				return "true"
			}

			return "false"
		case mLitAtom:
			return "'" + v.atomVal
		}
	case *mArr:
		parts := make([]string, 0, len(v.elems)+1)
		for range v.elems {
			parts = append(parts, "_")
		}

		if v.hasRest {
			parts = append(parts, "...")
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case *mStruct:
		names := make([]string, 0, len(v.fields))
		for k := range v.fields {
			names = append(names, k)
		}

		sortStrings(names)

		fs := make([]string, len(names))
		for i, n := range names {
			fs[i] = n + ": " + patStr(v.fields[n])
		}

		return v.typeName + "{" + strings.Join(fs, ", ") + "}"
	case *mData:
		if len(v.args) == 0 {
			return v.variant
		}

		parts := make([]string, len(v.args))
		for i, a := range v.args {
			parts[i] = patStr(a)
		}

		return v.variant + "(" + strings.Join(parts, ", ") + ")"
	}

	return "?"
}

func rowStr(row []mPat) string {
	parts := make([]string, len(row))
	for i, p := range row {
		parts[i] = patStr(p)
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

// dumpMatchInfo writes the Maranget matrix + per-arm reachability + the
// final exhaustiveness verdict for a match statement.
func (cg *CodeGen) dumpMatchInfo(s *ast.MatchStmt, label string) {
	pos := s.Pos()
	_, _ = fmt.Fprintf(cg.matchInfoSink(), "[match-info] %s at %s:%d:%d\n", label, cg.filenameForDiag(), pos.Line, pos.Col)
	_, _ = fmt.Fprintf(cg.matchInfoSink(), "  arity=1, cases=%d, default=%v\n", len(s.Cases), s.Default != nil)

	for i, c := range s.Cases {
		row, guarded := cg.matchCaseToRow(c)
		marker := "ok"

		if guarded {
			marker = "guarded"
		} else if !cg.marangetMatchArmUseful(s, i) {
			marker = "UNREACHABLE"
		}

		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  case[%d] %-12s %s\n", i, marker, rowStr(row))
	}

	if s.Default != nil {
		marker := "ok"
		if !cg.marangetMatchDefaultUseful(s) {
			marker = "UNREACHABLE"
		}

		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  default     %s\n", marker)
	}

	if ok, w := cg.marangetCheckMatchExhaustive(s); !ok {
		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  exhaustive: NO   missing witness: %s\n", w)
	} else {
		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  exhaustive: YES\n")
	}
}

// dumpWhereInfo writes the same info for a where-list.
func (cg *CodeGen) dumpWhereInfo(wl *ast.WhereList, label string) {
	pos := wl.Pos()
	if len(wl.Clauses) > 0 {
		pos = wl.Clauses[0].Pos
	}

	_, _ = fmt.Fprintf(cg.matchInfoSink(), "[match-info] %s at %s:%d:%d\n", label, cg.filenameForDiag(), pos.Line, pos.Col)

	arity := cg.whereArity(wl)
	_, _ = fmt.Fprintf(cg.matchInfoSink(), "  arity=%d, clauses=%d\n", arity, len(wl.Clauses))

	for i, c := range wl.Clauses {
		row, guarded := cg.wherePatternToRow(c, arity)
		marker := "ok"

		if guarded {
			marker = "guarded"
		} else if !cg.marangetWhereArmUseful(wl, i, arity) {
			marker = "UNREACHABLE"
		}

		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  clause[%d] %-12s %s\n", i, marker, rowStr(row))
	}

	if ok, w := cg.marangetCheckWhereExhaustive(wl, arity); !ok {
		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  exhaustive: NO   missing witness: %s\n", w)
	} else {
		_, _ = fmt.Fprintf(cg.matchInfoSink(), "  exhaustive: YES\n")
	}
}

// matchInfoSink returns the writer used for -v-match-info output. Stderr
// keeps it cleanly separated from program stdout.
func (cg *CodeGen) matchInfoSink() *os.File { return os.Stderr }

func (cg *CodeGen) filenameForDiag() string {
	if cg.filename != "" {
		return cg.filename
	}

	return "<repl>"
}
