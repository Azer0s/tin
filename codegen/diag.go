package codegen

import (
	"fmt"
	"os"
	"strings"

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
	// DiagWriteToConst fires when a write reaches a top-level const
	// through a pointer alias. Top-level consts are placed in read-only
	// storage; writing through them is undefined behavior the way
	// modifying a `const` global in C is. The check tracks bindings
	// derived from `&const_name` (and aliases of those bindings), and
	// warns when the program later assigns through the deref or passes
	// the pointer to a function that would mutate it.
	DiagWriteToConst = "write-to-const"
	// DiagUseBeforeAssign fires when a local declared without an
	// initializer (e.g. `let x i64`) is read before the program
	// explicitly assigns to it on every path reaching the read. Tin
	// auto-zero-inits primitives at runtime, so this is a style /
	// correctness check rather than UB detection: the read returns 0,
	// "", or nil, but the value almost certainly isn't what the
	// programmer intended. Default-off; opt in via -Wall.
	DiagUseBeforeAssign = "use-before-assign"
	// DiagLoopInvariant fires on a pure expression inside a loop body
	// whose operands are never assigned (or address-taken) within that
	// body. Hoisting it to before the loop avoids redoing the work each
	// iteration. The check is conservative: it only considers
	// arithmetic/bitwise/comparison/boolean trees over identifiers,
	// literals, field accesses, and casts -- never function calls,
	// pointer derefs, or indexed reads, since those may observe
	// side-effecting state we don't track.
	DiagLoopInvariant = "loop-invariant"
	// DiagMagicNumber fires on int/float literals embedded in code where
	// a named constant would convey intent better. Default-off; opt in via
	// -Wpedantic or -Wmagic-number. Universally exempt: -1, 0, 1, 2 (and
	// their float counterparts). Context-exempt: const initializers,
	// array index positions, comparison RHS, and bitwise-op operands when
	// the value is a power of two or a 2^N-1 mask.
	DiagMagicNumber = "magic-number"
	// DiagStyle fires on naming conventions that don't match Tin's house
	// style: functions, parameters, and locals should be snake_case;
	// types (struct, trait, enum, type, union, data) should be
	// PascalCase. Also flags trailing whitespace and a missing
	// end-of-file newline. Default-off; opt in via -Wstyle or -Wall.
	DiagStyle = "style"
	// DiagUnusedMustUse fires when a statement-level call returns a
	// `Result[t, e]` (or another #must_use-tagged type) and the result is
	// silently dropped. Result is the canonical Tin error channel; ignoring
	// it elides the error path entirely. The fix is to bind the result and
	// match on it, propagate it via try!, or take it via unwrap_or / unwrap.
	// To intentionally drop it, write `let _ = call(...)`. Default-on; use
	// -Wno-unused-must-use to silence.
	DiagUnusedMustUse = "unused-must-use"
	// DiagUnclosedCloseable fires when a local binding's value is produced
	// by a function whose return type implements `io::Closeable` (or
	// another resource trait) and the binding leaves scope without a
	// `.close()` call AND without being transferred (returned, stored in
	// a struct, passed to a fn). With Tin's default rc::Cell-backed
	// resources, auto-cleanup makes explicit close optional, so this
	// warning is default-off - opt in via -Wpedantic or
	// -Wunclosed-closeable for codebases that want explicit close
	// discipline (e.g. for graceful TLS close_notify, deterministic fd
	// release before a long compute, etc.).
	DiagUnclosedCloseable = "unclosed-closeable"
	// DiagFiber catches misuse of fiber/channel/mutex primitives:
	// double-close of a channel on a single path; send to a channel that
	// has already been closed on this path; mutex .lock() with no matching
	// .unlock() before function return; mutex declared but never locked
	// anywhere in the function. The check is intra-function and walks
	// each branch independently, so cross-branch interactions are
	// intentionally ignored to avoid false positives.
	DiagFiber = "fiber-misuse"
	// DiagUnguardedTraitDowncast fires when `expr as *Concrete` is used
	// to downcast a trait pointer (`*Trait`) to a concrete struct
	// pointer without a preceding `expr is *Concrete` guard in the
	// enclosing control-flow path.  The cast is inherently unchecked --
	// if the dynamic type does not match, the resulting pointer aliases
	// arbitrary memory -- so a guard is the canonical safe pattern.
	// Default-on; suppress with -Wno-unguarded-trait-downcast or by
	// adding the matching `is` check.
	DiagUnguardedTraitDowncast = "unguarded-trait-downcast"
	// DiagRedundantTypeCast fires on `<lit> as T` when the surrounding
	// context already pins the slot to T and the literal would
	// auto-coerce on its own.  Most commonly inside an array literal:
	//
	//   let k [u32; 64] = [0x428a2f98 as u32, ...]
	//                                ^^^^^^ redundant
	//
	// Removing the cast is a pure cleanup; the resulting program is
	// type-identical and shorter.  Default-on.
	DiagRedundantTypeCast = "redundant-type-cast"
)

// defaultOffWarnings lists diagnostics that are silent by default and only
// fire when the user opts in via -Wall, -Wpedantic, or -W<name>.
var defaultOffWarnings = map[string]bool{
	DiagUnusedLet:         true,
	DiagUnusedParam:       true,
	DiagUnusedResult:      true,
	DiagFloatEqual:        true,
	DiagBuiltinShadow:     true,
	DiagUseBeforeAssign:   true,
	DiagMagicNumber:       true,
	DiagStyle:             true,
	DiagUnclosedCloseable: true,
}

// wallWarnings is the set of diagnostics that -Wall enables on top of the
// always-on safety warnings. Mirrors the clang/gcc convention: useful
// hygiene checks that don't usually produce false positives in idiomatic
// code.
var wallWarnings = []string{DiagUnusedLet, DiagUnusedResult, DiagFloatEqual, DiagUseBeforeAssign, DiagStyle}

// wpedanticWarnings is the set that -Wpedantic enables on top of -Wall.
// These can produce noise in code that follows trait-conformance patterns
// (unused parameters required by an interface), so they're opt-in.
var wpedanticWarnings = []string{DiagUnusedParam, DiagBuiltinShadow, DiagMagicNumber, DiagUnclosedCloseable}

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

// localSuppression reports whether file:line carries a `//!-Wno-<name>`
// directive on the comment line(s) immediately preceding it. The map is
// built lazily from the on-disk source the first time a warning queries
// it, so files we never warn about cost nothing.
//
// Multiple comma-separated names per directive are supported:
//
//	//!-Wno-unwrapped-c-resource,no-copy
//	struct {#packed} ff = ...
func (cg *CodeGen) localSuppression(file string, line int, name string) bool {
	if file == "" || line <= 0 {
		return false
	}

	fileMap, ok := cg.localDiagSuppressions[file]
	if !ok {
		fileMap = scanLocalSuppressionsFromFile(file)

		if cg.localDiagSuppressions == nil {
			cg.localDiagSuppressions = map[string]map[int]map[string]bool{}
		}

		cg.localDiagSuppressions[file] = fileMap
	}

	if fileMap == nil {
		return false
	}

	return fileMap[line][name]
}

// scanLocalSuppressionsFromFile reads file once and returns a line ->
// suppressed-name set. Returns nil on read error so callers behave as if
// no suppressions exist.
func scanLocalSuppressionsFromFile(file string) map[int]map[string]bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}

	return scanLocalSuppressionsFromSource(string(data))
}

// scanLocalSuppressionsFromSource is the pure parser: walks src once,
// collects pending `//!-Wno-...` names, and attaches them to the line
// number of the next non-comment, non-blank line. Blank-line gaps and
// regular `//` comments don't break the chain.
func scanLocalSuppressionsFromSource(src string) map[int]map[string]bool {
	out := map[int]map[string]bool{}

	var pending []string

	const dirPrefix = "//!-Wno-"

	for i, line := range strings.Split(src, "\n") {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, dirPrefix) {
			body := strings.TrimSpace(trimmed[len(dirPrefix):])
			for _, raw := range strings.Split(body, ",") {
				if n := strings.TrimSpace(raw); n != "" {
					pending = append(pending, n)
				}
			}

			continue
		}

		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		if len(pending) > 0 {
			set := out[lineNum]
			if set == nil {
				set = map[string]bool{}
				out[lineNum] = set
			}

			for _, n := range pending {
				set[n] = true
			}

			pending = nil
		}
	}

	return out
}

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
	cg.warnInFile("", name, pos, format, args...)
}

// warnInFile is like warn but lets the caller pin the originating source
// file explicitly. Post-passes that walk decls from cross-package state
// must use this -- cg.filename moves around during compilation, so the
// implicit filenameForDiag may not match the file the directive lives
// in. Empty file falls back to cg.filenameForDiag().
func (cg *CodeGen) warnInFile(file, name string, pos ast.Pos, format string, args ...any) {
	if file == "" {
		file = cg.filenameForDiag()
	}

	// Suppressed during dataflow fixpoint iterations; see the comment on
	// CodeGen.dfSuppressWarnings.
	if cg.dfSuppressWarnings > 0 {
		return
	}

	if cg.diagSuppressed(name) {
		return
	}
	// Local suppression via `//!-Wno-<name>` on the line above the
	// warning's source position. Lets a struct field or fn declaration
	// opt out of one specific diagnostic without globally silencing it.
	if cg.localSuppression(file, pos.Line, name) {
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

	raw := fmt.Sprintf("%s:%d:%d: %s: %s [-W%s]",
		file, pos.Line, pos.Col, severity, msg, name)

	_, _ = fmt.Fprintln(os.Stderr, RenderDiagnostic(raw))
}
