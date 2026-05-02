package codegen

import (
	"fmt"
	"os"

	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Canonical diagnostic names used with the -W / -Wno- / -Werror= flag family.
// All compiler warnings should flow through CodeGen.warn() with one of these
// names so the user can scope suppression and escalation precisely.
const (
	DiagAsyncMain         = "async-main"
	DiagBoolAnalysis      = "bool-analysis"
	DiagUnusedMatchArms   = "unused-match-arms"
	DiagAwaitMatchGuards  = "await-match-guards"
	DiagArrayBounds       = "array-bounds"
	DiagDivByZero         = "div-by-zero"
	DiagShiftOverflow     = "shift-overflow"
	DiagDerefNil          = "deref-nil"
	DiagCastTrunc         = "cast-truncates"
	DiagUnreachableCode   = "unreachable-code"
	DiagTautologicalCmp   = "tautological-pointer-cmp"
	DiagSelfAssign        = "self-assign"
	DiagDiscardedPureCall = "discarded-pure-call"
	DiagUnsafeRequired    = "unsafe-required"
	DiagUnusedLet         = "unused-let"
	DiagUnusedParam       = "unused-param"
	DiagUnusedResult      = "unused-result"
	DiagUnusedImport      = "unused-import"
	DiagIdenticalOperands = "identical-operands"
	DiagUselessCast       = "useless-cast"
	DiagAlwaysTrueFalse   = "tautological-int-cmp"
	DiagLargeStackAlloc   = "large-stack-alloc"
	DiagUselessIdentity   = "useless-arith-identity"
	DiagFloatEqual        = "float-equal"
	DiagEmptyBody         = "empty-body"
	DiagInfiniteRecursion = "infinite-recursion"
	DiagImpossibleRange   = "impossible-range"
	DiagUseAfterDeinit    = "use-after-deinit"
	DiagDoubleDeinit      = "double-deinit"
	DiagFloatPrecision    = "float-precision"
	// DiagUnwrappedCResource fires on a struct field whose value transitively
	// touches an extern boundary (returned from / passed to a C function) and
	// has the shape of a C-managed resource (raw *void, pointer to an opaque
	// extern struct, i64 named like an fd, or i64 returned by a known fd-
	// returning POSIX function) when the field is not wrapped in *RcCell[T]
	// (or another #no_copy wrapper). Without the wrapper, copying the
	// containing struct would alias the resource and the second drop would
	// double-free / double-close.
	DiagUnwrappedCResource = "unwrapped-c-resource"
	// DiagBuiltinShadow fires when a local binding (let/var/param/nested-fn)
	// reuses the name of a recognized compile-time builtin. The shadow
	// itself is legal - `sourcepos` and friends are opted into by name and
	// the lexical scope wins as expected - but it can mask a typo and
	// silently disable the builtin in a region of code, which is hard to
	// debug after the fact. Default-off, opt-in via -W<name> or -Wpedantic.
	DiagBuiltinShadow = "builtin-shadow"
)

// defaultOffWarnings lists diagnostics that are silent by default and only
// fire when the user opts in via -Wall, -Wpedantic, or -W<name>.
var defaultOffWarnings = map[string]bool{
	DiagUnusedLet:     true,
	DiagUnusedParam:   true,
	DiagUnusedResult:  true,
	DiagFloatEqual:    true,
	DiagBuiltinShadow: true,
}

// wallWarnings is the set of diagnostics that -Wall enables on top of the
// always-on safety warnings. Mirrors the clang/gcc convention: useful
// hygiene checks that don't usually produce false positives in idiomatic
// code.
var wallWarnings = []string{DiagUnusedLet, DiagUnusedResult, DiagFloatEqual}

// wpedanticWarnings is the set that -Wpedantic enables on top of -Wall.
// These can produce noise in code that follows trait-conformance patterns
// (unused parameters required by an interface), so they're opt-in.
var wpedanticWarnings = []string{DiagUnusedParam, DiagBuiltinShadow}

// diagState tracks the user's preferences for one named warning.
type diagState struct {
	suppressed bool // -Wno-<name>
	asError    bool // -Werror=<name>
	enabled    bool // -W<name>, -Wall, -Wpedantic (opt-in for default-off diags)
}

// SetWarnSuppress silences the named warning (-Wno-<name>).
func (cg *CodeGen) SetWarnSuppress(name string) {
	cg.ensureDiag(name).suppressed = true
}

// SetWarnEnable opts in to a default-off warning (-W<name>).
func (cg *CodeGen) SetWarnEnable(name string) {
	cg.ensureDiag(name).enabled = true
}

// SetWarnAsError escalates the named warning to a hard error (-Werror=<name>).
// Also implicitly enables it if it was default-off.
func (cg *CodeGen) SetWarnAsError(name string) {
	d := cg.ensureDiag(name)
	d.asError = true
	d.enabled = true
}

// SetAllWarnsAsErrors escalates every warning to a hard error (-Werror).
func (cg *CodeGen) SetAllWarnsAsErrors() { cg.allWarnsAsErrors = true }

// SetWAll enables the -Wall family of warnings.
func (cg *CodeGen) SetWAll() {
	for _, n := range wallWarnings {
		cg.SetWarnEnable(n)
	}
}

// SetWPedantic enables -Wall plus the more pedantic checks.
func (cg *CodeGen) SetWPedantic() {
	cg.SetWAll()

	for _, n := range wpedanticWarnings {
		cg.SetWarnEnable(n)
	}
}

// HadWarnError reports whether any warning was promoted to an error during
// codegen. The caller should fail the build when this is true.
func (cg *CodeGen) HadWarnError() bool { return cg.hadWarnError }

func (cg *CodeGen) ensureDiag(name string) *diagState {
	if cg.diags == nil {
		cg.diags = map[string]*diagState{}
	}

	s := cg.diags[name]
	if s == nil {
		s = &diagState{}
		cg.diags[name] = s
	}

	return s
}

// diagSuppressed reports whether the named warning is silenced via -Wno-<name>.
func (cg *CodeGen) diagSuppressed(name string) bool {
	if s := cg.diags[name]; s != nil {
		return s.suppressed
	}

	return false
}

// warn emits a diagnostic for `name` at `pos`. The severity is:
//   - "warning" by default,
//   - "error" if -Werror or -Werror=<name> is in effect.
//
// On error escalation, hadWarnError is set so Generate() can return a
// terminal error after the codegen pass completes.
// checkConstIndexBounds emits the array-bounds warning when the IndexExpr's
// index folds to a known integer that lies outside [0, length).
func (cg *CodeGen) checkConstIndexBounds(e *ast.IndexExpr, length int64) {
	v := cg.tryFoldExpr(e.Index)
	if v.kind != foldInt {
		return
	}

	if v.intVal >= 0 && v.intVal < length {
		return
	}

	cg.warn(DiagArrayBounds, e.Pos(),
		"index %d is out of bounds for array of length %d", v.intVal, length)
}

// checkTautologicalNilCmp warns when one side of an `==` / `!=` is the
// literal `nil` and the other is an expression that's statically non-nil
// (currently: an &x address-of).
func (cg *CodeGen) checkTautologicalNilCmp(e *ast.BinExpr, notEqual bool) {
	_, lNil := e.Left.(*ast.NilLit)
	_, rNil := e.Right.(*ast.NilLit)

	var nonNil ast.Node

	switch {
	case lNil && !rNil:
		nonNil = e.Right
	case rNil && !lNil:
		nonNil = e.Left
	default:
		return
	}

	if !isStaticallyNonNil(nonNil) {
		return
	}

	truth := "false"
	if notEqual {
		truth = "true"
	}

	cg.warn(DiagTautologicalCmp, e.Pos(),
		"comparison is always %s: operand is statically non-nil", truth)
}

// isStaticallyNonNil reports whether n is an expression that the compiler
// can prove is never nil.
func isStaticallyNonNil(n ast.Node) bool {
	switch n.(type) {
	case *ast.AddressOfExpr:
		return true
	}

	return false
}

// checkCastTruncatesConst warns when an `as` cast narrows a constant
// integer that does not fit the destination's `tBits` width. Signed/unsigned
// is taken from the source expression.
func (cg *CodeGen) checkCastTruncatesConst(e *ast.AsExpr, tBits uint64, srcUnsigned bool) {
	v := cg.tryFoldExpr(e.Expr)
	if v.kind != foldInt {
		return
	}

	var (
		minOK int64
		maxOK int64
	)

	if srcUnsigned || tBits == 64 {
		minOK = 0
	} else {
		minOK = -(int64(1) << (tBits - 1))
	}

	if srcUnsigned {
		if tBits >= 64 {
			return
		}

		maxOK = (int64(1) << tBits) - 1
	} else {
		maxOK = (int64(1) << (tBits - 1)) - 1
	}

	if v.intVal >= minOK && v.intVal <= maxOK {
		return
	}

	prefix := "i"
	if srcUnsigned {
		prefix = "u"
	}

	cg.warn(DiagCastTrunc, e.Pos(),
		"constant %d does not fit in %s%d (range %d..%d)",
		v.intVal, prefix, tBits, minOK, maxOK)
}

// checkShiftAmount errors out when a shift's right-hand operand folds to a
// constant >= the bit width of the left operand. The hardware behavior is
// implementation-defined / UB so we refuse to lower it.
func (cg *CodeGen) checkShiftAmount(e *ast.BinExpr, left value.Value) error {
	v := cg.tryFoldExpr(e.Right)
	if v.kind != foldInt {
		return nil
	}

	it, ok := left.Type().(*irtypes.IntType)
	if !ok {
		return nil
	}

	if v.intVal < 0 {
		return cg.nodeErr(e, "shift by negative amount %d", v.intVal)
	}

	if v.intVal >= int64(it.BitSize) {
		return cg.nodeErr(e, "shift by %d is >= width of %s (%d bits)",
			v.intVal, it, it.BitSize)
	}

	return nil
}

// staticArrayLen tries to recover the compile-time length of `expr` as the
// receiver of an indexing op. It handles:
//   - ArrayLit: count of literal elements
//   - ArrayFillLit: fill count if it folds to a constant
//   - StringLit: byte length
//   - Identifier: look up the scope entry and read the alloca's array type
//
// Returns (length, true) on success.
func (cg *CodeGen) staticArrayLen(expr ast.Node) (int64, bool) {
	switch e := expr.(type) {
	case *ast.ArrayLit:
		return int64(len(e.Elems)), true
	case *ast.ArrayFillLit:
		if e.Count >= 0 {
			return int64(e.Count), true
		}
	case *ast.StringLit:
		return int64(len(e.Value)), true
	case *ast.Identifier:
		if entry, ok := cg.curScope.lookup(e.Name); ok {
			if entry.staticArrayLen > 0 {
				return entry.staticArrayLen, true
			}

			if entry.val != nil {
				if pt, isPtr := entry.val.Type().(*irtypes.PointerType); isPtr {
					if at, isArr := pt.ElemType.(*irtypes.ArrayType); isArr {
						return int64(at.Len), true
					}
				}
			}
		}
	}

	return 0, false
}

func (cg *CodeGen) warn(name string, pos ast.Pos, format string, args ...any) {
	if cg.diagSuppressed(name) {
		return
	}

	s := cg.diags[name]
	if defaultOffWarnings[name] {
		// Default-off warnings only fire when the user opted in via
		// -W<name>, -Wall, -Wpedantic, or -Werror=<name>.
		if s == nil || !s.enabled {
			return
		}
	}

	asError := cg.allWarnsAsErrors
	if s != nil && s.asError {
		asError = true
	}

	severity := "warning"
	if asError {
		severity = "error"

		cg.hadWarnError = true
	}

	msg := fmt.Sprintf(format, args...)

	_, _ = fmt.Fprintf(os.Stderr, "%s:%d:%d: %s: %s [-W%s]\n",
		cg.filenameForDiag(), pos.Line, pos.Col, severity, msg, name)
}
