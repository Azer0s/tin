package codegen

import (
	"fmt"
	"os"

	irtypes "github.com/llir/llvm/ir/types"

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
)

// diagState tracks the user's preferences for one named warning.
type diagState struct {
	suppressed bool // -Wno-<name>
	asError    bool // -Werror=<name>
}

// SetWarnSuppress silences the named warning (-Wno-<name>).
func (cg *CodeGen) SetWarnSuppress(name string) {
	cg.ensureDiag(name).suppressed = true
}

// SetWarnAsError escalates the named warning to a hard error (-Werror=<name>).
func (cg *CodeGen) SetWarnAsError(name string) {
	cg.ensureDiag(name).asError = true
}

// SetAllWarnsAsErrors escalates every warning to a hard error (-Werror).
func (cg *CodeGen) SetAllWarnsAsErrors() { cg.allWarnsAsErrors = true }

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

	asError := cg.allWarnsAsErrors
	if s := cg.diags[name]; s != nil && s.asError {
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
