// Package repl implements the interactive REPL for the Tin language.
package repl

import (
	"fmt"
	"os"
	"strings"
)

// Run starts the interactive REPL. runtimeDir is the path to runtime/,
// stdlibOverride is an optional stdlib path override, and libsRoots are
// additional package roots.
func Run(runtimeDir, stdlibOverride string, libsRoots []string) {
	macros := newMacroRegistry()

	s, err := newSession(runtimeDir, stdlibOverride, libsRoots, macros)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "repl: init failed: %v\n", err)

		os.Exit(1)
	}
	defer s.close()

	in, err := newInputReader(macros)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "repl: readline init failed: %v\n", err)

		os.Exit(1)
	}
	defer in.close()

	fmt.Println("tin repl - type :help for commands, Ctrl-D to exit")

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
