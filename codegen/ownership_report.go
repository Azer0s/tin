package codegen

// ownership_report.go - per-binding ownership classification report
// emitted by --explain-ownership.  Records every let-decl /
// parameter and the ownership state codegen committed to (owned,
// borrow, or move), grouped by function.

import (
	"fmt"
	"os"
	"strings"
)

// ownershipReportEntry is one row in --explain-ownership output.
type ownershipReportEntry struct {
	fnName      string    // qualified function name (e.g. "handle_request")
	fileName    string    // source file (for filtering)
	bindingName string    // let-binding name
	ownership   ownership // classification at codegen time
	note        string    // short reason (e.g. "returned to caller")
}

// recordOwnership stores one binding's ownership classification for the
// --explain-ownership report. No-op when the flag is off (the common
// case), so the call site can fire unconditionally without measurable
// overhead.
func (cg *CodeGen) recordOwnership(bindingName string, own ownership, note string) {
	if cg.explainOwnershipSpec == "" {
		return
	}

	if bindingName == "" {
		return
	}

	fnName := ""
	if cg.curFn != nil {
		fnName = cg.curFn.Name()
	}
	// Skip the auto-generated $colored / $coro variants: they reuse the
	// same source body so each binding would appear N times in the
	// report, one per variant. The sync (bare-name) variant is enough.
	if strings.HasSuffix(fnName, "$colored") || strings.HasSuffix(fnName, "$coro") {
		return
	}

	cg.explainOwnershipReport = append(cg.explainOwnershipReport, ownershipReportEntry{
		fnName:      fnName,
		fileName:    cg.filename,
		bindingName: bindingName,
		ownership:   own,
		note:        note,
	})
}

// fnNameMatches returns true when the user-supplied fn filter matches
// the actual emitted function name. Matches the bare source name plus
// Tin's internal `_tin_user_<name>` prefix so users can write `main`
// instead of `_tin_user_main`.
func fnNameMatches(emitted, want string) bool {
	if emitted == want {
		return true
	}

	if strings.HasPrefix(emitted, "_tin_user_") {
		return strings.TrimPrefix(emitted, "_tin_user_") == want
	}

	return false
}

// FinalizeExplainOwnership emits the per-binding ownership report to
// stderr.  Called by the driver after codegen completes for the
// current compilation unit.  No-op when --explain-ownership was not
// passed.
//
// Format matches docs/15-ownership-borrowing.md:
//
//	fn handle_request:
//	  let req       owned   (returned to caller)
//	  let path      borrow  (read-only, dropped at scope exit)
func (cg *CodeGen) FinalizeExplainOwnership() {
	if cg.explainOwnershipSpec == "" || len(cg.explainOwnershipReport) == 0 {
		return
	}

	spec := cg.explainOwnershipSpec
	wantFile, wantFn := "", ""

	if spec != "*" {
		if idx := strings.LastIndex(spec, ":"); idx >= 0 {
			wantFile = spec[:idx]
			wantFn = spec[idx+1:]
		} else {
			wantFn = spec
		}
	}

	groups := map[string][]ownershipReportEntry{}
	order := []string{}

	for _, e := range cg.explainOwnershipReport {
		if wantFn != "" && !fnNameMatches(e.fnName, wantFn) {
			continue
		}

		if wantFile != "" && !strings.HasSuffix(e.fileName, wantFile) {
			continue
		}

		if _, seen := groups[e.fnName]; !seen {
			order = append(order, e.fnName)
		}

		groups[e.fnName] = append(groups[e.fnName], e)
	}

	if len(order) == 0 {
		return
	}

	fmt.Fprintln(os.Stderr, "--explain-ownership:")

	maxName := 0

	for _, fn := range order {
		for _, e := range groups[fn] {
			if n := len(e.bindingName); n > maxName {
				maxName = n
			}
		}
	}

	for _, fn := range order {
		fmt.Fprintf(os.Stderr, "fn %s:\n", cg.displayFnName(fn))

		for _, e := range groups[fn] {
			padding := strings.Repeat(" ", maxName-len(e.bindingName))
			// "parameter" notes get a `param` prefix so the user can
			// distinguish function parameters from let-decls at a
			// glance; the prefix conveys the kind, so we drop the
			// `(parameter)` note that would otherwise duplicate it.
			kind := "let  "
			note := e.note

			if e.note == "parameter" {
				kind = "param"
				note = ""
			}

			if note == "" {
				fmt.Fprintf(os.Stderr, "  %s %s%s  %s\n", kind, e.bindingName, padding, e.ownership)
			} else {
				fmt.Fprintf(os.Stderr, "  %s %s%s  %-7s (%s)\n", kind, e.bindingName, padding, e.ownership, note)
			}
		}
	}
}

// displayFnName converts an emitted IR function name to its
// user-visible Tin source name. Falls through to fnDisplayNames for
// names the codegen has explicitly registered (lambdas, monomorphs,
// `_tin_user_main` -> `main`, etc.); otherwise strips the
// `_tin_user_` prefix that wraps every top-level fn.
func (cg *CodeGen) displayFnName(emitted string) string {
	if dn, ok := cg.fnDisplayNames[emitted]; ok && dn != "" {
		return dn
	}

	if strings.HasPrefix(emitted, "_tin_user_") {
		return strings.TrimPrefix(emitted, "_tin_user_")
	}

	return emitted
}
