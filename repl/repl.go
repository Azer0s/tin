// Package repl implements the interactive REPL for the Tin language.
package repl

import (
	"fmt"
	"os"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// Run starts the interactive REPL. runtimeDir is the path to runtime/,
// stdlibOverride is an optional stdlib path override, libsRoots are
// additional package roots, and preloadFile (if non-empty) is a .tin file
// whose declarations are loaded into the session and whose top-level
// statements (plus `main()` if defined) run before the interactive prompt.
func Run(runtimeDir, stdlibOverride string, libsRoots []string, preloadFile string) {
	macros := newMacroRegistry()
	opTraits := newOpTraitRegistry()

	s, err := newSession(runtimeDir, stdlibOverride, libsRoots, macros, opTraits)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "repl: init failed: %v\n", err)

		os.Exit(1)
	}
	defer s.close()

	in, err := newInputReader(macros, opTraits, s)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "repl: readline init failed: %v\n", err)

		os.Exit(1)
	}
	defer in.close()

	fmt.Println("tin repl - type :help for commands, Ctrl-D to exit")

	if preloadFile != "" {
		if err := preload(s, preloadFile); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "repl: preload %s: %v\n", preloadFile, err)
		}
	}

	for {
		src, eof := in.readCell()
		if eof {
			fmt.Println()

			break
		}

		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}

		// Handle REPL meta-commands.
		if strings.HasPrefix(src, ":") {
			if quit := handleCommand(s, src); quit {
				break
			}

			continue
		}

		if err := s.evalCell(src); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		}
	}

	fiberRun()
}

func handleCommand(s *session, cmd string) (quit bool) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case ":quit", ":q":
		return true
	case ":reset":
		s.reset()
		fmt.Println("session reset")
	case ":list":
		if len(s.declOrder) == 0 && len(s.globalsOrder) == 0 {
			fmt.Println("(nothing declared)")

			return
		}

		for _, k := range s.globalsOrder {
			fmt.Printf("  %s\n", s.prevGlobals[k])
		}

		for _, k := range s.declOrder {
			fmt.Printf("  %s\n", firstLine(s.declMap[k]))
		}
	case ":help":
		fmt.Print(helpText)
	default:
		fmt.Printf("unknown command %q - type :help for commands\n", parts[0])
	}

	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

const helpText = `REPL commands:
  :help        show this help
  :list        list accumulated declarations and globals
  :reset       clear all session state (loaded libraries remain in memory)
  :quit / :q   exit the REPL

Input:
  End a line with ':' or '=' to enter multi-line mode.
  Press Enter twice on an empty line to submit a multi-line cell.
  Ctrl-D exits the REPL.
`

// preload reads a .tin file, registers its declarations in the session,
// and runs its top-level statements as the first cell. If the file defines
// `fn main()`, a follow-up `main()` cell is appended so the binary-style
// entry point also runs (matching `tin run`).
//
// Test blocks are filtered out — they only run under `tin test`.
func preload(s *session, path string) error {
	src, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return err
	}

	if err := s.evalCell(string(src)); err != nil {
		return err
	}

	// Run main() if the file defined one and there were no top-level stmts
	// that already drove the program.
	prog, perr := parseSrc(string(src))
	if perr != nil {
		return nil
	}

	hasMain := false

	for _, n := range prog.Stmts {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name == "main" {
			hasMain = true

			break
		}
	}

	if hasMain {
		if err := s.evalCell("main()"); err != nil {
			return err
		}
	}

	return nil
}
