// Package format implements `tin fmt`, a line-oriented source formatter.
//
// Why line-oriented? The Tin lexer strips comments before producing tokens,
// so a token-driven formatter would lose every comment in the source. We
// work on the source bytes directly, with just enough state to recognize
// string literals and block comments so we don't mangle their contents.
//
// Current normalizations:
//
//   - Indent: every block-leading line picks up a 2-space-per-depth
//     indent. The depth is tracked by a stack of "absolute indent" values
//     so a source written with 4-space indents reformats correctly.
//   - Trailing whitespace: stripped on every line.
//   - Trailing newline: ensured (single newline at EOF).
//   - Blank lines: runs of two-or-more collapse to one (every depth).
//
// Semantic blank-line insertion (idempotent — running on already-formatted
// source produces no change):
//
//  1. Blank line BEFORE a block-opener (`if`/`for`/`loop`/`match`/`where`/
//     `defer`/`spawn`-as-stmt) when the previous statement at the same
//     depth is neither blank, a comment attached to this line, nor itself
//     a block-opener. Skipped when the block-opener is the first
//     statement in its enclosing body.
//  2. Blank line AFTER a block-closer (when depth has just dropped back
//     to a block-opener's depth and the next non-blank line is a real
//     statement, not an `else`/`else if`/`case`/`default` continuation
//     and not another block-opener which Rule 1 would handle).
//  3. Blank line BEFORE the final `return` of a multi-statement body
//     (≥3 non-comment statements). Skipped for short bodies and one-
//     liner returns.
//  4. Blank line AFTER a "setup section" — a maximal run of `let`
//     bindings plus bare-name reassignments to in-section names — when
//     the next line is none of those.
//
// Comment attachment: when a rule wants a blank line before a code line
// that has `//`-comments immediately above it (no blank between), the
// blank is inserted ABOVE the comment block instead, so the comment
// stays glued to the code it documents.
//
// Out of scope (deliberately): naming rewrites (those are -Wstyle's
// job), binop spacing, expression wrapping, comment reflowing,
// alignment.
package format

import (
	"strings"
	"unicode"
)

// Format returns the formatted form of src. The bool reports whether the
// output differs from the input -- callers in --check mode use it to set
// the exit code.
//
// A file may opt out of formatting by including the directive
// `//!-Wno-format` on a line by itself anywhere in the file. The
// formatter returns src unchanged. Mirrors the existing per-file
// warning-suppression syntax (`//!-Wno-<diag>`) used elsewhere in the
// codebase. Useful for files where tests pin to exact source positions
// (e.g. examples/sourcepos.tin, stdlib/source/source_test.tin).
func Format(src string) (string, bool) {
	if hasFmtOffDirective(src) {
		return src, false
	}

	out := formatSource(src)

	return out, out != src
}

// hasFmtOffDirective reports whether src opts out of formatting via a
// `//!-Wno-format` line. The marker may be followed by any explanatory
// text on the same line (separated by whitespace), so authors can
// document why the file is exempt; matching is by prefix.
func hasFmtOffDirective(src string) bool {
	const marker = "//!-Wno-format"

	for _, line := range splitLines(src) {
		t := strings.TrimSpace(line)
		if t == marker || strings.HasPrefix(t, marker+" ") {
			return true
		}
	}

	return false
}

// formatSource is the workhorse: walks the source line-by-line, tracking
// whether we're inside a block comment, and rewrites indent and trailing
// whitespace. Lines inside block comments are emitted verbatim so we
// don't accidentally rewrite an embedded `/*` or `*/`.
func formatSource(src string) string {
	lines := splitLines(src)

	var (
		out          []string
		indentStack  = []int{0}
		inBlock      bool // inside a /* ... */ that spans lines
		bracketDepth int  // unclosed (, [, { carried over from prior lines
		opContinue   bool // previous code line ended with a continuation op
	)

	for _, line := range lines {
		stripped := trimTrailingWS(line)

		if inBlock {
			out = append(out, stripped)

			if strings.Contains(stripped, "*/") {
				inBlock = false
			}

			continue
		}

		body, lead := splitIndent(stripped)
		if body == "" {
			// Blank line: emit empty (no leading whitespace at all).
			// Blank-line collapsing is a separate post-pass.
			out = append(out, "")

			continue
		}

		// Continuation of an open (, [, { from a prior line OR of a
		// trailing binary operator (&&, ||, +, etc.): leave the indent
		// alone so hand-aligned multi-line literals and operator
		// continuations like
		//   if a && b &&
		//      c && d:
		// don't get squashed to the canonical block indent. We still
		// strip trailing whitespace.
		if bracketDepth > 0 || opContinue {
			out = append(out, stripped)
			bracketDepth += bracketDelta(body)
			opContinue = endsInContinuationOp(body)

			continue
		}

		depth := updateIndentStack(&indentStack, lead)

		// Re-indent canonically. Comment-only lines participate in
		// indentation just like code lines so a `// note` indented to
		// match its block stays aligned with the surrounding code.
		out = append(out, strings.Repeat("  ", depth)+body)

		bracketDepth += bracketDelta(body)
		opContinue = endsInContinuationOp(body)

		// A `/*` without a matching `*/` on the same line opens a
		// multi-line block comment. We detect this AFTER re-indenting
		// the opening line so the start sits at the canonical depth.
		if opensBlockComment(body) {
			inBlock = true
		}
	}

	out = insertSemanticBlanks(out)
	out = collapseBlankRuns(out, indentStack)

	return ensureTrailingNewline(joinLines(out))
}

// lineKind classifies a normalized output line for the semantic-blank pass.
type lineKind int

const (
	kindBlank             lineKind = iota
	kindComment                    // line whose first non-whitespace is `//` or inside a /* */
	kindMultilineCont              // bracket-continuation of an open ( [ { from above
	kindLetDecl                    // first token is `let`
	kindBareAssign                 // pattern `IDENT (=|+=|-=|*=|/=|%=|++=) ...` (no `.` or `[` before op)
	kindBlockOpener                // first token in {if for loop match where defer spawn}
	kindBlockContinuation          // first token in {else case default} (sticks to its preceding block)
	kindReturn                     // first token is `return`
	kindOther                      // any other code line
)

// classifiedLine is a per-line record used by the semantic-blank pass.
// It captures the canonicalized indent depth, kind, body, and -- for let
// declarations / bare assignments -- the name that's bound or assigned.
type classifiedLine struct {
	raw   string
	kind  lineKind
	depth int
	body  string // body without leading whitespace
	name  string // bound name (let) or LHS (bare assign), "" otherwise
}

// insertSemanticBlanks walks the canonicalized line stream and inserts
// blank lines before/after control-flow blocks, around setup-sections,
// and before the final return of multi-statement bodies. Idempotent: a
// second invocation on the same input produces no further insertions
// because every rule checks "is there already a blank above me" before
// firing.
func insertSemanticBlanks(lines []string) []string {
	infos := classifyLines(lines)

	// Rules 1, 2, 6 are co-operative: each decides whether the line at
	// position i needs a blank above it. We compute "needs blank above"
	// in one forward walk, then apply it.
	wantBlankAbove := make([]bool, len(lines))

	// Index of the most recent non-blank, non-comment, non-multiline-cont
	// line. -1 before we've seen any.
	prevCodeIdx := -1

	// Open setup-section bound-name set, per indent depth. A depth's
	// section is opened by the first `let` at that depth, extended by
	// further lets and by bare-name reassignments to names in the set.
	// Cleared when a non-extending line is seen at that depth (Rule 6
	// fires) or when control leaves that depth structurally (no blank).
	setupNames := map[int]map[string]bool{}

	// Per-depth count of "real" statements seen since this depth was
	// last entered. Used by Rule 2 to skip the blank-after-block when
	// the just-closed block had only one statement (e.g. a one-line
	// `if cond: return X` guard inside a loop — inserting a blank
	// above the rest of the loop body would be visually wrong).
	stmtCount := map[int]int{}

	for i, li := range infos {
		if li.kind == kindBlank || li.kind == kindComment || li.kind == kindMultilineCont {
			continue
		}

		if prevCodeIdx >= 0 {
			pi := infos[prevCodeIdx]

			// Rule 2: blank after block-closer. Detect via "previous
			// code line was at a deeper depth than current" — meaning
			// at least one block just closed. Only fire when the
			// just-closed block (the one at depth = li.depth+1) had
			// ≥2 statements; otherwise the block was a one-line guard
			// and inserting a blank below it visually fragments its
			// parent body. Skip for `else`/`else if`/`case`/`default`
			// continuations (part of the same construct).
			if pi.depth > li.depth && li.kind != kindBlockContinuation {
				if stmtCount[li.depth+1] >= 2 {
					wantBlankAbove[i] = true
				}
			}

			// Rule 1: blank before block-opener. Fires when the
			// previous line is at the SAME depth and is not itself a
			// block-opener / block-continuation. Cases where the
			// previous line is at a DIFFERENT depth are handled by
			// Rule 2 (deeper → block closed) or by the "first stmt of
			// a new block" case (shallower → entering a new body, no
			// blank).
			if li.kind == kindBlockOpener && pi.depth == li.depth &&
				pi.kind != kindBlockOpener && pi.kind != kindBlockContinuation {
				wantBlankAbove[i] = true
			}

			// Rule 6: setup-section close. Only consult the section
			// at li.depth — sections at OTHER depths either belong to
			// outer scopes (still open) or inner scopes (already
			// implicitly closed when we left them; their blank-line
			// is supplied by Rule 2's block-closer detection).
			//
			// Don't fire when the closing line is a return: a body
			// shaped like `let x = ...; return x + 1` is too short
			// to deserve a blank, and longer bodies are covered by
			// Rule 3 (blank before the final return when there are
			// ≥3 sibling statements). Suppressing the blank here
			// also keeps line-number-sensitive tests like
			// `examples/sourcepos.tin` stable when the formatter
			// runs over them.
			if openSet, sectionOpen := setupNames[li.depth]; sectionOpen {
				if !isStillInSetupSection(li, openSet) {
					if li.kind != kindReturn {
						wantBlankAbove[i] = true
					}

					delete(setupNames, li.depth)
				}
			}
		}

		// Update setup-section state for li's depth. Cases:
		//   bare-name let (`let x = ...`): open or extend section,
		//     adding the bound name.
		//   destructure let (`let (a, b) = ...`, `let [x, ...] = ...`):
		//     extends an open section without adding to the name set,
		//     but does NOT open a fresh one. Reason: lets that don't
		//     introduce a single bare name often appear inline with
		//     "use the result" code, and inserting a Rule-6 blank
		//     between them and the next stmt reads as overformatting.
		//   bare reassign to an in-section name: keep section open
		//     unchanged (the in-section check ran above).
		//   anything else: close the section (the blank was already
		//     requested above; here we just clear the state).
		if li.kind == kindLetDecl && li.name != "" {
			if setupNames[li.depth] == nil {
				setupNames[li.depth] = map[string]bool{}
			}

			setupNames[li.depth][li.name] = true
		} else if li.kind != kindBareAssign && li.kind != kindLetDecl {
			delete(setupNames, li.depth)
		}

		// When li's depth is shallower than some open setup-section's
		// depth, those deeper sections are no longer reachable — drop
		// them so they don't fire stale blanks later.
		for d := range setupNames {
			if d > li.depth {
				delete(setupNames, d)
			}
		}

		// Reset stmt counts for any depth deeper than li.depth: those
		// blocks have closed, so their counts are stale. li.depth's
		// own count gets bumped after this for the current line.
		for d := range stmtCount {
			if d > li.depth {
				delete(stmtCount, d)
			}
		}

		stmtCount[li.depth]++

		prevCodeIdx = i
	}

	// Rule 3: blank before final return in multi-statement bodies.
	// Walk again, this time looking for kindReturn lines and counting
	// the number of "real" (non-blank, non-comment) lines in the body
	// the return belongs to. The body's start is the line immediately
	// following the opening of the deepest enclosing block (or top of
	// file). We approximate "body" as "the contiguous run of lines at
	// >= return's depth ending at the return line."
	for i, li := range infos {
		if li.kind != kindReturn {
			continue
		}

		if i == 0 {
			continue
		}
		// Already requested by another rule? Don't double-up the
		// flag (it's a bool anyway, but the comment makes the intent
		// clear that Rule 3 is purely additive).
		if wantBlankAbove[i] {
			continue
		}

		hasBlankAbove := false

		for j := i - 1; j >= 0; j-- {
			pj := infos[j]
			if pj.kind == kindBlank {
				hasBlankAbove = true

				break
			}

			if pj.kind == kindComment || pj.kind == kindMultilineCont {
				continue
			}

			break
		}

		if hasBlankAbove {
			continue
		}

		// Count siblings at the same depth in the same body. Walk
		// backward until depth drops below li.depth, counting code
		// lines at exactly li.depth.
		siblings := 1 // count the return itself

		for j := i - 1; j >= 0; j-- {
			pj := infos[j]
			if pj.depth < li.depth {
				break
			}

			if pj.depth == li.depth &&
				pj.kind != kindBlank &&
				pj.kind != kindComment &&
				pj.kind != kindMultilineCont {
				siblings++
			}
		}

		if siblings >= 3 {
			wantBlankAbove[i] = true
		}
	}

	// Apply: walk lines and insert blanks. When a line wants a blank
	// above and has comment(s) immediately above (with no blank), the
	// blank goes ABOVE the topmost attached comment instead -- so the
	// comment stays glued to the code it documents.
	out := make([]string, 0, len(lines)+8)
	for i, line := range lines {
		if wantBlankAbove[i] {
			// Walk back over attached comments to find insertion point.
			insertAt := len(out)
			for insertAt > 0 {
				prev := out[insertAt-1]
				if strings.TrimSpace(prev) == "" {
					break // already a blank above; don't double-insert
				}

				if !lineIsLineComment(prev) {
					break
				}
				// It's a comment immediately above. Walk further up
				// to find the start of the comment block.
				insertAt--
			}
			// Only insert if the slot above isn't already blank.
			if insertAt == 0 || strings.TrimSpace(out[insertAt-1]) != "" {
				// Insert blank at insertAt.
				out = append(out, "") // grow by one
				copy(out[insertAt+1:], out[insertAt:])
				out[insertAt] = ""
			}
		}

		out = append(out, line)
	}

	return out
}

// classifyLines walks the canonicalized line stream and produces a
// classifiedLine record per input line. Tracks bracket depth so that
// continuation lines of multiline expressions/literals are tagged as
// kindMultilineCont and don't participate in the blank-line rules.
func classifyLines(lines []string) []classifiedLine {
	out := make([]classifiedLine, len(lines))

	var (
		bracketDepth int
		inBlockComm  bool
	)

	for i, line := range lines {
		body, lead := splitIndent(line)
		depth := lead / 2 // canonical 2-space per depth

		if inBlockComm {
			out[i] = classifiedLine{raw: line, kind: kindComment, depth: depth, body: body}
			if strings.Contains(body, "*/") {
				inBlockComm = false
			}

			continue
		}

		if body == "" {
			out[i] = classifiedLine{raw: line, kind: kindBlank, depth: depth}

			continue
		}

		if bracketDepth > 0 {
			out[i] = classifiedLine{raw: line, kind: kindMultilineCont, depth: depth, body: body}
			bracketDepth += bracketDelta(body)

			continue
		}

		kind, name := classifyBody(body)
		out[i] = classifiedLine{raw: line, kind: kind, depth: depth, body: body, name: name}

		bracketDepth += bracketDelta(body)

		if opensBlockComment(body) {
			inBlockComm = true
		}
	}

	return out
}

// classifyBody returns the kind and (for letDecl/bareAssign) the bound
// or LHS name for a single line's body. Comments are recognized first;
// then keywords; then the assignment-pattern fallback.
func classifyBody(body string) (lineKind, string) {
	if strings.HasPrefix(body, "//") {
		return kindComment, ""
	}

	if strings.HasPrefix(body, "/*") {
		return kindComment, ""
	}

	tok := firstWord(body)
	switch tok {
	case "let":
		return kindLetDecl, letBoundName(body)
	case "if", "for", "loop", "match", "where", "defer":
		return kindBlockOpener, ""
	case "spawn":
		// spawn-as-statement (top-level `spawn foo()`); when bound
		// (`let f = spawn ...`) the line starts with `let` and goes
		// through the let path.
		return kindBlockOpener, ""
	case "else", "case", "default":
		return kindBlockContinuation, ""
	case "return":
		return kindReturn, ""
	}

	if name, ok := bareAssignTarget(body); ok {
		return kindBareAssign, name
	}

	return kindOther, ""
}

// firstWord returns the first whitespace-delimited token at the start of
// body. Strips trailing punctuation like `(` or `:` so `if(cond)` and
// `if cond:` both yield "if".
func firstWord(body string) string {
	end := 0
	for end < len(body) {
		c := rune(body[end])
		if !isIdentRune(c) {
			break
		}

		end++
	}

	return body[:end]
}

func isIdentRune(c rune) bool {
	return c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c)
}

// letBoundName extracts NAME from `let NAME ...` (with or without a type
// annotation, with or without `=`). Returns "" if the binding doesn't
// match the simple bare-name pattern (e.g. tuple destructure
// `let (a, b) = ...`, array pattern `let [x, ...rest] = ...`).
func letBoundName(body string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(body, "let"))
	if rest == "" {
		return ""
	}

	if rest[0] == '(' || rest[0] == '[' || rest[0] == '{' {
		return ""
	}

	end := 0
	for end < len(rest) {
		c := rune(rest[end])
		if !isIdentRune(c) {
			break
		}

		end++
	}

	if end == 0 {
		return ""
	}

	return rest[:end]
}

// bareAssignTarget recognizes `IDENT (=|+=|-=|*=|/=|%=|++=) ...` where the
// LHS is exactly a bare identifier (no `.` field access, no `[` index, no
// `*` deref). Returns the LHS name and ok=true on match. Used by Rule 6
// to extend a setup-section through reassignments.
func bareAssignTarget(body string) (string, bool) {
	end := 0
	for end < len(body) {
		c := rune(body[end])
		if !isIdentRune(c) {
			break
		}

		end++
	}

	if end == 0 {
		return "", false
	}

	name := body[:end]

	// Skip whitespace.
	for end < len(body) && (body[end] == ' ' || body[end] == '\t') {
		end++
	}

	if end >= len(body) {
		return "", false
	}
	// Match assignment operator. Reject `==`, `<=`, `>=`, `!=` (those
	// are comparisons, not assignments).
	switch body[end] {
	case '=':
		if end+1 < len(body) && body[end+1] == '=' {
			return "", false // `==`
		}

		return name, true
	case '+', '-', '*', '/', '%':
		if end+1 < len(body) && body[end+1] == '=' {
			return name, true
		}
		// `++=` (string concat-assign): looks like `+` then `+=`.
		if body[end] == '+' && end+2 < len(body) && body[end+1] == '+' && body[end+2] == '=' {
			return name, true
		}
	}

	return "", false
}

// isStillInSetupSection reports whether li can extend an open
// setup-section. Lets always extend; bare-name assignments extend only
// when the target is in the open set.
func isStillInSetupSection(li classifiedLine, openSet map[string]bool) bool {
	if li.kind == kindLetDecl {
		return true
	}

	if li.kind == kindBareAssign {
		return openSet[li.name]
	}

	return false
}

// lineIsLineComment returns true when line is a `//`-comment line at any
// indent. Used to walk back over attached comments when inserting a
// blank above.
func lineIsLineComment(line string) bool {
	body, _ := splitIndent(line)

	return strings.HasPrefix(body, "//")
}

// bracketDelta returns the net change in bracket depth contributed by
// body: +1 for each (, [, {, -1 for each ), ], }, ignoring brackets that
// appear inside string literals, char-coded atom literals (`@'X'`), or
// after a // line comment. Tin's lexer uses this same notion to
// suppress NEWLINE tokens inside open delimiters; we mirror it here so
// multi-line literals are recognized as continuations rather than fresh
// statements.
//
// Note: a bare `'X'` is NOT a char literal in Tin — `'foo` is an atom-
// by-name (no closing apostrophe), and `'"name"'` is a quoted atom.
// Char-coded atoms use the `@'X'` form. We only special-case `@'...'`;
// bare `'` is left as ordinary punctuation, which is fine because atom
// names (`'foo`) contain only identifier characters, never brackets.
func bracketDelta(body string) int {
	delta := 0

	for i := 0; i < len(body); i++ {
		c := body[i]

		switch c {
		case '"':
			// Skip to the matching closing quote, honoring backslash
			// escapes. Brackets inside the string don't count.
			i = skipQuoted(body, i+1, '"')
		case '@':
			// Atom-by-char `@'X'`: skip past the closing apostrophe so
			// the inner char (`,`, `}`, `]`, etc.) doesn't flip the
			// bracket count. Forms `@name` (bare-symbol atom) and
			// `@N` (numeric code) don't contain brackets, so they fall
			// through to normal scanning.
			if i+1 < len(body) && body[i+1] == '\'' {
				i = skipQuoted(body, i+2, '\'')
			}
		case '/':
			if i+1 < len(body) && body[i+1] == '/' {
				return delta
			}
		case '(', '[', '{':
			delta++
		case ')', ']', '}':
			delta--
		}
	}

	return delta
}

// endsInContinuationOp reports whether body, considered without trailing
// whitespace and `// line comments`, ends in a token that the parser will
// treat as "the expression continues on the next line." Tin's parser
// honors a NEWLINE after a binary operator as transparent (parser/exprs.go
// "Line-continuation: operator at end of line"), so the formatter must
// treat the following line as a wrap of the same statement and leave its
// indent alone — otherwise the indent stack picks up the deeper hanging
// indent as a "new block level" and subsequent siblings get garbage
// indents.
//
// Operators recognized:
//
//	&& ||                             (logical)
//	+ - * / %                         (arithmetic; bare `-` is unary on
//	                                   the next line — we still treat it
//	                                   as continuation, harmless)
//	== != < > <= >=                   (comparison)
//	+= -= *= /= %= ++=                (augmented assigns)
//	|>                                (pipe)
//	?                                 (ternary opener)
//	, .                               (separator / method chain)
//
// Trailing `:` is the block-opener marker, NOT a continuation, so it's
// excluded. Trailing `=` is excluded for the same reason most other tools
// do — `let x = expr` is the canonical complete form, and a bare `=`
// dangling at end of line is rare and ambiguous.
func endsInContinuationOp(body string) bool {
	// Strip a trailing line comment.
	if idx := indexOutsideString(body, "//"); idx >= 0 {
		body = body[:idx]
	}

	body = strings.TrimRight(body, " \t")
	if body == "" {
		return false
	}
	// Two-char operators first (so `&&` doesn't match `&`, `>=` doesn't
	// match `>`, etc.).
	twoChar := []string{
		"&&", "||",
		"==", "!=", "<=", ">=",
		"+=", "-=", "*=", "/=", "%=",
		"|>",
	}
	for _, op := range twoChar {
		if strings.HasSuffix(body, op) {
			return true
		}
	}

	if strings.HasSuffix(body, "++=") {
		return true
	}
	// Single-char operators. Watch out for `=` (excluded) and `:` (block
	// opener marker).
	switch body[len(body)-1] {
	case '+', '-', '*', '/', '%', '<', '>', '?', ',', '.':
		return true
	}

	return false
}

// skipQuoted advances past the body of a quoted literal that started at
// position `start` and ends at the next occurrence of `terminator`,
// respecting `\X` backslash escapes. Returns the index of the
// terminator character (or len(body)-1 when unterminated, which is the
// caller's responsibility to interpret as "rest of line is in literal").
func skipQuoted(body string, start int, terminator byte) int {
	for i := start; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			i++

			continue
		}

		if c == terminator {
			return i
		}
	}

	return len(body) - 1
}

// splitLines breaks src on '\n', preserving an empty trailing element if
// the file ends with a newline. We need that distinction so the EOF-
// newline post-pass knows whether to add one.
func splitLines(src string) []string {
	return strings.Split(src, "\n")
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}

// trimTrailingWS strips spaces and tabs from the end of a line. We never
// strip newlines (those are line separators handled elsewhere).
func trimTrailingWS(line string) string {
	return strings.TrimRight(line, " \t")
}

// splitIndent separates a line into (body, lead-width). The width is
// expressed in space-equivalent columns -- a tab counts as one column
// for stack-comparison purposes since the stack stores raw counts only.
// Mixed tabs and spaces in leading whitespace are tolerated; the body is
// what's left after the first non-whitespace character.
func splitIndent(line string) (string, int) {
	for i, r := range line {
		if r != ' ' && r != '\t' {
			return line[i:], i
		}
	}

	return "", 0
}

// updateIndentStack adjusts the indent stack to match the new line's
// absolute indent and returns the resulting depth (stack length minus
// one). The stack stores absolute indents seen so far; a deeper indent
// pushes a new level, a shallower one pops back to the matching slot.
//
// If the new indent doesn't exactly match any stack entry on the way
// down, we don't try to repair the source -- we accept the closest
// match. The formatter is not a parser; mismatched indents survive as
// the user wrote them so the next compile step can report the error.
func updateIndentStack(stack *[]int, lead int) int {
	top := (*stack)[len(*stack)-1]

	if lead > top {
		*stack = append(*stack, lead)

		return len(*stack) - 1
	}

	for len(*stack) > 1 && (*stack)[len(*stack)-1] > lead {
		*stack = (*stack)[:len(*stack)-1]
	}

	return len(*stack) - 1
}

// opensBlockComment reports whether body opens a `/* ... */` that does
// NOT close on the same line. Heuristic: look for `/*` outside string
// literals; if found, look for a matching `*/` after it. Doesn't handle
// nested block comments because Tin doesn't have them.
func opensBlockComment(body string) bool {
	idx := indexOutsideString(body, "/*")
	if idx < 0 {
		return false
	}

	tail := body[idx+2:]

	return !strings.Contains(tail, "*/")
}

// indexOutsideString finds the first occurrence of needle in s that
// isn't inside a "..." string. Returns -1 if not found. We only need
// this for `/*` detection, so a simple state machine over { CODE,
// STRING, ESCAPE } is enough -- char literals don't appear in this
// language's syntax.
func indexOutsideString(s, needle string) int {
	state := 0 // 0 = code, 1 = inside "

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch state {
		case 0:
			if c == '"' {
				state = 1

				continue
			}

			if c == '/' && i+1 < len(s) && s[i+1] == '/' {
				// Line comment: anything after this is not "in code".
				return -1
			}

			if i+len(needle) <= len(s) && s[i:i+len(needle)] == needle {
				return i
			}
		case 1:
			if c == '\\' && i+1 < len(s) {
				i++

				continue
			}

			if c == '"' {
				state = 0
			}
		}
	}

	return -1
}

// collapseBlankRuns rewrites runs of two-or-more blank lines down to a
// single blank line. Applied unconditionally so blank-line discipline is
// uniform across blocks and top-level decls.
func collapseBlankRuns(lines []string, _ []int) []string {
	out := make([]string, 0, len(lines))

	prevBlank := false

	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}

		out = append(out, line)
		prevBlank = blank
	}

	return out
}

// ensureTrailingNewline adds a single '\n' to s if it doesn't already end
// in one, and trims any extra trailing newlines down to one. An entirely
// empty source stays empty.
func ensureTrailingNewline(s string) string {
	if s == "" {
		return s
	}

	s = strings.TrimRight(s, "\n")

	return s + "\n"
}
