package repl

import (
	"strings"

	"github.com/chzyer/readline"
)

func macroTabListener(macros *macroRegistry) readline.Listener {
	return readline.FuncListener(func(line []rune, pos int, key rune) ([]rune, int, bool) {
		if key != '\t' || macros == nil {
			return nil, 0, false
		}

		src := string(line[:pos])

		expanded, ok := tryExpandMacro(src, macros)
		if !ok {
			return nil, 0, false
		}

		suffix := line[pos:]
		newLine := append([]rune(expanded), suffix...)

		return newLine, len([]rune(expanded)), true
	})
}

// inputReader wraps readline to handle multi-line input.
// Multi-line mode is triggered when the last non-whitespace character of a
// submitted line is ':' or '='. It ends when two consecutive empty lines
// are entered.
type inputReader struct {
	rl     *readline.Instance
	macros *macroRegistry
}

func newInputReader(macros *macroRegistry, opTraits *opTraitRegistry) (*inputReader, error) {
	cfg := &readline.Config{
		Prompt:                 "tin> ",
		HistoryFile:            "/tmp/tin-repl-history",
		DisableAutoSaveHistory: true,
		Painter:                &highlighter{macros: macros, opTraits: opTraits},
		AutoComplete:           &tabGuard{},
		Listener:               macroTabListener(macros),
	}

	rl, err := readline.NewEx(cfg)
	if err != nil {
		return nil, err
	}

	return &inputReader{rl: rl, macros: macros}, nil
}

func (r *inputReader) close() {
	_ = r.rl.Close()
}

// readCell reads a complete cell from the user, handling multi-line input.
// Returns (source, false) on success, ("", true) when the user signals EOF.
//
// Continuation lines are auto-indented: the next line's input buffer is
// pre-filled with the previous line's leading whitespace, plus 2 extra
// spaces if the previous line ended with `:` or `=`. The user can edit
// (backspace to dedent, type more to indent further) before pressing Enter.
// This prevents the common pitfall of typing struct/fn bodies at column 0
// because the multiline prompt's trailing spaces look like indentation.
func (r *inputReader) readCell() (string, bool) {
	r.rl.SetPrompt("tin> ")

	var lines []string

	multiLine := false
	emptyCount := 0

	// nextIndent is the prefill for the next Readline call in multiline mode.
	nextIndent := ""

	for {
		var (
			line string
			err  error
		)

		if nextIndent != "" {
			line, err = r.rl.ReadlineWithDefault(nextIndent)
		} else {
			line, err = r.rl.Readline()
		}

		if err != nil {
			// EOF (Ctrl-D) or terminal closed.
			// If we have accumulated content in multi-line mode, submit it
			// rather than discarding it (supports piped/non-interactive input).
			if len(lines) > 0 {
				break
			}

			return "", true
		}

		trimmed := strings.TrimRight(line, " \t")

		if multiLine {
			if trimmed == "" {
				emptyCount++
				if emptyCount >= 2 {
					// Two consecutive empty lines end the multi-line cell.
					break
				}
				// Keep the previous non-blank line's indent so a blank
				// separator doesn't dedent everything that follows.
				continue
			}

			emptyCount = 0

			lines = append(lines, line)
			nextIndent = autoIndentFor(trimmed)

			continue
		}

		// Single-line mode: detect if we should enter multi-line.
		if trimmed == "" {
			continue
		}

		last := trimmed[len(trimmed)-1]
		if last == ':' || last == '=' {
			multiLine = true

			r.rl.SetPrompt("...   ")

			lines = append(lines, line)
			nextIndent = autoIndentFor(trimmed)

			continue
		}

		// Single complete line.
		lines = append(lines, line)

		break
	}

	src := strings.Join(lines, "\n")
	if strings.TrimSpace(src) != "" {
		_ = r.rl.SaveHistory(src)
	}

	return src, false
}

// autoIndentFor computes the prefill for the next continuation line: the
// current line's leading whitespace, plus 2 spaces when the line ends with
// `:` or `=` (entering a deeper block).
func autoIndentFor(trimmed string) string {
	indent := leadingWhitespace(trimmed)
	if trimmed == "" {
		return indent
	}

	last := trimmed[len(trimmed)-1]
	if last == ':' || last == '=' {
		return indent + "  "
	}

	return indent
}

// leadingWhitespace returns the run of spaces and tabs at the start of s.
func leadingWhitespace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}

	return s[:i]
}
