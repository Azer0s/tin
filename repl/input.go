package repl

import (
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
)

// replListener handles Tab-driven macro expansion AND paste-aware
// auto-indent for continuation lines.
//
// On the first Readline call after `reset(prefill)` we set the buffer to
// `prefill` (via ReadlineWithDefault). Two failure modes follow:
//
//   - User types: characters arrive at typing speed, the prefill stays as
//     the user's leading indent, all good.
//   - User pastes: the terminal injects characters at sub-millisecond
//     intervals; the pasted lines often carry their own leading whitespace,
//     so the prefill double-indents the result.
//
// We disambiguate by timing. After two keystrokes arrive within a few
// milliseconds of each other (way faster than any human can type), we
// strip the prefill from the buffer's start so the pasted content's own
// indentation wins.
type replListener struct {
	macros *macroRegistry

	mu           sync.Mutex
	prefill      string
	lastKeyTime  time.Time
	rapidCount   int
	pasteHandled bool
}

const (
	pasteRapidThreshold = 5 * time.Millisecond
	pasteRapidNeeded    = 2 // 2 fast keys after the first => 3 keystrokes total
)

func (l *replListener) reset(prefill string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.prefill = prefill
	l.lastKeyTime = time.Time{}
	l.rapidCount = 0
	l.pasteHandled = false
}

// OnChange implements readline.Listener.
func (l *replListener) OnChange(line []rune, pos int, key rune) ([]rune, int, bool) {
	if key == '\t' && l.macros != nil {
		src := string(line[:pos])
		if expanded, ok := tryExpandMacro(src, l.macros); ok {
			suffix := line[pos:]
			newLine := append([]rune(expanded), suffix...)

			return newLine, len([]rune(expanded)), true
		}
	}

	if newLine, newPos, ok := l.handlePaste(line, pos, key); ok {
		return newLine, newPos, true
	}

	return nil, 0, false
}

func (l *replListener) handlePaste(line []rune, pos int, key rune) ([]rune, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.prefill == "" || l.pasteHandled || key == 0 {
		return nil, 0, false
	}

	now := time.Now()

	if !l.lastKeyTime.IsZero() && now.Sub(l.lastKeyTime) < pasteRapidThreshold {
		l.rapidCount++
	} else {
		l.rapidCount = 0
	}

	l.lastKeyTime = now

	if l.rapidCount < pasteRapidNeeded {
		return nil, 0, false
	}

	prefillRunes := []rune(l.prefill)
	if len(line) < len(prefillRunes) {
		return nil, 0, false
	}

	for i, r := range prefillRunes {
		if line[i] != r {
			return nil, 0, false
		}
	}

	l.pasteHandled = true

	newLine := append([]rune(nil), line[len(prefillRunes):]...)

	newPos := pos - len(prefillRunes)
	if newPos < 0 {
		newPos = 0
	}

	return newLine, newPos, true
}

// inputReader wraps readline to handle multi-line input.
// Multi-line mode is triggered when the last non-whitespace character of a
// submitted line is ':' or '='. It ends when two consecutive empty lines
// are entered. Continuation lines are auto-indented with the previous
// line's leading whitespace (plus 2 spaces if it ended with ':' or '=');
// the prefill is suppressed when we detect a paste burst so pasted blocks
// keep their own indentation.
type inputReader struct {
	rl       *readline.Instance
	macros   *macroRegistry
	listener *replListener
}

func newInputReader(macros *macroRegistry, opTraits *opTraitRegistry, s *session) (*inputReader, error) {
	listener := &replListener{macros: macros}

	cfg := &readline.Config{
		Prompt:                 "tin> ",
		HistoryFile:            "/tmp/tin-repl-history",
		DisableAutoSaveHistory: true,
		Painter:                &highlighter{macros: macros, opTraits: opTraits},
		AutoComplete:           &completer{s: s},
		Listener:               listener,
	}

	rl, err := readline.NewEx(cfg)
	if err != nil {
		return nil, err
	}

	return &inputReader{rl: rl, macros: macros, listener: listener}, nil
}

func (r *inputReader) close() {
	_ = r.rl.Close()
}

// readCell reads a complete cell from the user, handling multi-line input.
// Returns (source, false) on success, ("", true) when the user signals EOF.
func (r *inputReader) readCell() (string, bool) {
	r.rl.SetPrompt("tin> ")

	var lines []string

	multiLine := false
	emptyCount := 0
	nextIndent := ""

	for {
		var (
			line string
			err  error
		)

		r.listener.reset(nextIndent)

		if nextIndent != "" {
			line, err = r.rl.ReadlineWithDefault(nextIndent)
		} else {
			line, err = r.rl.Readline()
		}

		if err != nil {
			// EOF (Ctrl-D) or terminal closed. If we have accumulated content
			// in multi-line mode, submit it rather than discarding it.
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
					break
				}

				continue
			}

			emptyCount = 0

			lines = append(lines, line)
			nextIndent = autoIndentFor(trimmed)

			continue
		}

		if trimmed == "" {
			continue
		}

		last := trimmed[len(trimmed)-1]
		if last == ':' || last == '=' {
			multiLine = true

			r.rl.SetPrompt("... ")

			lines = append(lines, line)
			nextIndent = autoIndentFor(trimmed)

			continue
		}

		lines = append(lines, line)

		break
	}

	src := strings.Join(lines, "\n")
	if strings.TrimSpace(src) != "" {
		_ = r.rl.SaveHistory(src)
	}

	return src, false
}

// autoIndentFor returns the prefill for the next continuation line: the
// trimmed line's leading whitespace, plus 2 spaces when the line ends with
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

func leadingWhitespace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}

	return s[:i]
}
