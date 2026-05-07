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
//   - Blank lines: a run of blank lines at depth 0 collapses to one.
//
// Out of scope (deliberately): naming rewrites (those are -Wstyle's job),
// binop spacing, expression wrapping, comment reflowing.
package format

import (
	"strings"
)

// Format returns the formatted form of src. The bool reports whether the
// output differs from the input -- callers in --check mode use it to set
// the exit code.
func Format(src string) (string, bool) {
	out := formatSource(src)

	return out, out != src
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

		// Continuation of an open (, [, { from a prior line: leave the
		// indent alone so hand-aligned multi-line literals like
		//   u8x16{ @'A', @'B',
		//          @'A', @'B' }
		// don't get squashed to the canonical block indent. We still
		// strip trailing whitespace.
		if bracketDepth > 0 {
			out = append(out, stripped)
			bracketDepth += bracketDelta(body)

			continue
		}

		depth := updateIndentStack(&indentStack, lead)

		// Re-indent canonically. Comment-only lines participate in
		// indentation just like code lines so a `// note` indented to
		// match its block stays aligned with the surrounding code.
		out = append(out, strings.Repeat("  ", depth)+body)

		bracketDepth += bracketDelta(body)

		// A `/*` without a matching `*/` on the same line opens a
		// multi-line block comment. We detect this AFTER re-indenting
		// the opening line so the start sits at the canonical depth.
		if opensBlockComment(body) {
			inBlock = true
		}
	}

	out = collapseBlankRuns(out, indentStack)

	return ensureTrailingNewline(joinLines(out))
}

// bracketDelta returns the net change in bracket depth contributed by
// body: +1 for each (, [, {, -1 for each ), ], }, ignoring brackets that
// appear inside string literals or after a // line comment. Tin's lexer
// uses this same notion to suppress NEWLINE tokens inside open
// delimiters; we mirror it here so multi-line literals are recognized
// as continuations rather than fresh statements.
func bracketDelta(body string) int {
	delta := 0
	state := 0 // 0 = code, 1 = inside "

	for i := 0; i < len(body); i++ {
		c := body[i]

		switch state {
		case 0:
			switch c {
			case '"':
				state = 1
			case '/':
				if i+1 < len(body) && body[i+1] == '/' {
					return delta
				}
			case '(', '[', '{':
				delta++
			case ')', ']', '}':
				delta--
			}
		case 1:
			if c == '\\' && i+1 < len(body) {
				i++

				continue
			}

			if c == '"' {
				state = 0
			}
		}
	}

	return delta
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
