package codegen

// diag_snippet.go -- Rust-style diagnostic renderer.
//
// Takes a diagnostic of the form "file:line:col: level: message" and
// re-renders it with the offending source line and a caret pointing at
// the column. Color is applied via ANSI escapes when the output is a
// terminal; the plain-text fallback matches the legacy format byte-for-
// byte so CI logs and grep-driven tools keep working.
//
// The renderer is intentionally string-in / string-out so it can wrap
// any error path -- parser errors (string-formatted), codegen errors
// (cg.nodeErr), and direct stderr writes (cg.warnInFile) all flow
// through `RenderDiagnostic`. No call sites need to migrate to a
// structured Diagnostic type to benefit.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// AnsiEnabled reports whether Render* helpers should emit color escape
// sequences. Set by the CLI based on isatty + the `--color={auto,
// always, never}` flag. Default false so tests / CI keep clean output.
var AnsiEnabled bool

// SnippetEnabled reports whether to expand a "file:line:col: ..."
// header into a multi-line snippet. Default true; set false by the CLI
// for `--error-format=plain` users (or any non-interactive caller that
// wants to grep the line-and-column without the source body).
var SnippetEnabled = true

// Color codes. Bright red for errors, bright yellow for warnings,
// cyan for source-quoting headers, dim grey for the line gutter, bold
// for the message. All wrapped in helpers so disabling color is one
// flag flip rather than a global s/replace.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func ansi(code, s string) string {
	if !AnsiEnabled {
		return s
	}

	return code + s + ansiReset
}

func ansiBoldText(s string) string  { return ansi(ansiBold, s) }
func ansiRedText(s string) string   { return ansi(ansiRed, s) }
func ansiYellowText(s string) string { return ansi(ansiYellow, s) }
func ansiCyanText(s string) string  { return ansi(ansiCyan, s) }
func ansiDimText(s string) string   { return ansi(ansiDim, s) }

// diagHeader matches the standard `file:line:col[-endcol]: ...`
// prefix produced by every codegen / parser error. The file group
// accepts paths with colons (uncommon on POSIX but legal on Windows
// volumes after we strip the drive letter); it also accepts the
// bare-relative form. The optional `-endcol` lets a producer specify
// the underline span explicitly when it knows where the offending
// region ends; without it the renderer falls back to identifier-scan
// heuristics.
var diagHeader = regexp.MustCompile(`^([^:\n]+(?::[^:\n]*)*?):(\d+):(\d+)(?:-(\d+))?:\s*(.*)$`)

// labelPattern catches the optional "warning:" / "error:" tag that
// follows the position when the diagnostic came through warnInFile.
var labelPattern = regexp.MustCompile(`^(warning|error|note):\s*(.*)$`)

// trailingFlag captures the `[-Wname]` tag that warnInFile appends
// to the end of warning messages.
var trailingFlag = regexp.MustCompile(`\s*\[\-W[^\]]+\]\s*$`)

// RenderDiagnostic takes a raw stderr line (or multi-line block) and,
// if it parses as a Tin diagnostic, returns the snippet-formatted
// Rust-style version. Falls through verbatim for anything that doesn't
// match. Multi-line input is split on '\n' so an error message that
// already contains its own bullet list (e.g. ambiguous-overload
// candidates) renders correctly with each bullet on its own line.
func RenderDiagnostic(raw string) string {
	if !SnippetEnabled {
		return raw
	}

	// Process line-by-line so multi-line diagnostics (ambiguity
	// candidates, where-guard witnesses) keep their structure.
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		out = append(out, renderOneLine(line))
	}

	return strings.Join(out, "\n")
}

// renderOneLine produces the rendered form of a single log line.
// Returns the line unchanged when it doesn't match the diagnostic
// header pattern -- that keeps progress bars, plain echoes, and other
// non-diagnostic output intact.
func renderOneLine(line string) string {
	m := diagHeader.FindStringSubmatch(line)
	if m == nil {
		return line
	}

	file := m[1]

	lineNum, err := strconv.Atoi(m[2])
	if err != nil {
		return line
	}

	col, err := strconv.Atoi(m[3])
	if err != nil {
		return line
	}

	endCol := 0

	if m[4] != "" {
		if v, e := strconv.Atoi(m[4]); e == nil {
			endCol = v
		}
	}

	rest := m[5]

	// Pull the level and possible -W flag out of the message body so
	// we can color them separately from the user-visible text.
	level := "error"
	msg := rest

	if lm := labelPattern.FindStringSubmatch(rest); lm != nil {
		level = lm[1]
		msg = lm[2]
	}

	flag := ""
	if loc := trailingFlag.FindStringIndex(msg); loc != nil {
		flag = strings.TrimSpace(msg[loc[0]:loc[1]])
		msg = strings.TrimSpace(msg[:loc[0]])
	}

	source, ok := readSourceLine(file, lineNum)
	if !ok {
		// Couldn't read the file (e.g. REPL, generated input). Render
		// the header line cleanly without the snippet block.
		return formatHeader(level, msg, flag, file, lineNum, col)
	}

	return formatSnippet(level, msg, flag, file, lineNum, col, endCol, source)
}

// formatHeader renders the no-source form: just the position +
// level + message. Used when the source file isn't readable.
func formatHeader(level, msg, flag string, file string, lineNum, col int) string {
	var b strings.Builder

	colorize := ansiRedText
	if level == "warning" {
		colorize = ansiYellowText
	} else if level == "note" {
		colorize = ansiCyanText
	}

	b.WriteString(ansiBoldText(colorize(level)))
	b.WriteString(ansiBoldText(": "))
	b.WriteString(ansiBoldText(msg))

	if flag != "" {
		b.WriteString(" ")
		b.WriteString(ansiDimText(flag))
	}

	b.WriteString("\n")
	b.WriteString(strings.Repeat(" ", numWidth(lineNum)+1))
	b.WriteString(ansiCyanText("--> "))
	b.WriteString(fmt.Sprintf("%s:%d:%d", file, lineNum, col))

	return b.String()
}

// formatSnippet is the Rust-style block: header, gutter, source line
// with caret. The gutter width is chosen to fit the line number plus
// one space padding. When endCol > col, the caret spans columns
// [col, endCol] (1-indexed, inclusive); otherwise the renderer falls
// back to identifier/operator/string heuristics for the underline
// width.
func formatSnippet(level, msg, flag, file string, lineNum, col, endCol int, source string) string {
	var b strings.Builder

	colorize := ansiRedText
	caretCol := ansiRedText

	switch level {
	case "warning":
		colorize = ansiYellowText
		caretCol = ansiYellowText
	case "note":
		colorize = ansiCyanText
		caretCol = ansiCyanText
	}

	b.WriteString(ansiBoldText(colorize(level)))
	b.WriteString(ansiBoldText(": "))
	b.WriteString(ansiBoldText(msg))

	if flag != "" {
		b.WriteString(" ")
		b.WriteString(ansiDimText(flag))
	}

	b.WriteString("\n")

	// Gutter is `numWidth + 1` spaces wide -- one slot for the line
	// number plus one trailing space -- so the `|` separators land in
	// the same column for the numbered, empty and caret lines.
	gutter := strings.Repeat(" ", numWidth(lineNum)+1)

	b.WriteString(gutter)
	b.WriteString(ansiCyanText("--> "))
	b.WriteString(fmt.Sprintf("%s:%d:%d\n", file, lineNum, col))

	b.WriteString(gutter)
	b.WriteString(ansiCyanText("|\n"))

	b.WriteString(ansiCyanText(fmt.Sprintf("%d | ", lineNum)))
	b.WriteString(source)
	b.WriteString("\n")

	// Caret line. Pad spaces by (col-1) characters using the same
	// width as the source so the caret aligns with the offending
	// column. Tabs in source count as one column for the caret too --
	// matching Rust's behavior, which trades exact alignment for not
	// having to know the user's tab width.
	//
	// Span: explicit endCol wins (the producer knows the offending
	// region's bounds). Otherwise, widen heuristically: identifier
	// scans the rest of the name; quote scans the string literal;
	// op-char scans the operator run.
	var span int
	if endCol >= col {
		span = endCol - col + 1
	} else {
		span = caretSpan(source, col)
	}

	caret := strings.Repeat(" ", col-1) + strings.Repeat("^", span)

	b.WriteString(gutter)
	b.WriteString(ansiCyanText("| "))
	b.WriteString(caretCol(caret))
	b.WriteString("\n")

	return b.String()
}

// caretSpan returns how many characters of `source` to underline,
// starting at 1-indexed column `col`. Lands on an identifier? underline
// the whole identifier. Lands on a string literal opener? underline the
// quoted run (closing quote inclusive). Anything else (operator,
// punctuation, end-of-line) gets a single-char caret.
//
// Returns at least 1 so the gutter math never produces an empty caret.
func caretSpan(source string, col int) int {
	if col < 1 || col > len(source) {
		return 1
	}

	c := source[col-1]
	if isIdentStart(c) {
		n := 1
		for col-1+n < len(source) && isIdentCont(source[col-1+n]) {
			n++
		}

		return n
	}

	if c == '"' || c == '\'' {
		quote := c
		n := 1

		for col-1+n < len(source) {
			ch := source[col-1+n]
			if ch == '\\' && col-1+n+1 < len(source) {
				n += 2 // skip the escaped char as a unit
				continue
			}

			n++
			if ch == quote {
				break
			}
		}

		return n
	}

	// Operator-class char: greedily consume the run so multi-char ops
	// like ++, ==, +=, <=, ..  get a caret across the whole token
	// instead of the first character only.
	if isOpChar(c) {
		n := 1
		for col-1+n < len(source) && isOpChar(source[col-1+n]) {
			n++
		}

		return n
	}

	return 1
}

func isOpChar(b byte) bool {
	switch b {
	case '+', '-', '*', '/', '%', '=', '<', '>', '!', '&', '|', '^', '~', '.', '?':
		return true
	}

	return false
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// numWidth returns the digit count of n. Used to size the gutter so
// the `|` separators line up regardless of whether the line is line 7
// or line 700.
func numWidth(n int) int {
	if n <= 0 {
		return 1
	}

	w := 0
	for n > 0 {
		w++
		n /= 10
	}

	return w
}

// sourceLineCache memoizes file reads. Diagnostics frequently fire
// many at a time over the same file; reading once is plenty.
var sourceLineCache = map[string][]string{}

// readSourceLine returns the 1-indexed line of `file`, or ("", false)
// when the file can't be read. The whole file is cached on first
// access -- diagnostic emission is bursty (several errors per file)
// and the cache wins after the second hit.
func readSourceLine(file string, lineNum int) (string, bool) {
	lines, ok := sourceLineCache[file]
	if !ok {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", false
		}

		lines = strings.Split(string(data), "\n")
		sourceLineCache[file] = lines
	}

	if lineNum < 1 || lineNum > len(lines) {
		return "", false
	}

	return strings.TrimRight(lines[lineNum-1], "\r"), true
}
