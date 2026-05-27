package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/format"
)

func buildDeps(cg *codegen.CodeGen, cSources []cSource) []string {
	out := append([]string{}, cg.PackageSrcPaths()...)

	for _, c := range cSources {
		out = append(out, c.path)
	}

	return out
}

// parseRunArgs scans os.Args after fileArgIdx for the run/test memcheck flag
// and the binary's argv (the part after `--`).
func parseRunArgs(fileArgIdx int) (memcheck string, binArgs []string) {
	for i := fileArgIdx + 1; i < len(os.Args); i++ {
		a := os.Args[i]
		if a == "--" {
			binArgs = append(binArgs, os.Args[i+1:]...)

			break
		} else if a == "--valgrind" {
			memcheck = "valgrind"
		} else if a == "--leaks" {
			memcheck = "leaks"
		}
	}

	return memcheck, binArgs
}

// collectExtraObjs scans os.Args after fileArgIdx for extra link inputs:
// .o / .a files and -l / -L flags.
func collectExtraObjs(fileArgIdx int) []string {
	var out []string

	for i := fileArgIdx + 1; i < len(os.Args); i++ {
		a := os.Args[i]
		if a == "--" {
			break
		} else if a == "--cflag" {
			i++ // value already collected upstream
		} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
			out = append(out, a)
		} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
			out = append(out, a)
		}
	}

	return out
}

// execRunBinary runs `bin` (under memcheck if set) and exits with its status.
func execRunBinary(bin, memcheck string, binArgs []string, vgSuppressions ...string) {
	run := memcheckCmdWithSuppressions(memcheck, bin, vgSuppressions, binArgs...)

	if memcheck != "" {
		colorOut := newMemcheckColorWriter(os.Stdout)
		colorErr := newMemcheckColorWriter(os.Stderr)
		run.Stdout = colorOut
		run.Stderr = colorErr

		defer func() {
			_ = colorOut.Flush()
			_ = colorErr.Flush()
		}()
	} else {
		run.Stdout = os.Stdout
		run.Stderr = os.Stderr
	}

	if isStdoutTTY() {
		_ = os.Setenv("TIN_TEST_COLOR", "1")
	}

	// Route through runMemcheck when a memcheck tool is active so the
	// leaks(1) deadlock guard (15 s + 5 s drain) also covers single-file
	// `tin run --leaks` invocations -- not just `tin test` ones.  Without
	// this, the macOS leaks --atExit injection that suspends the target
	// in T state and never SIGCONTs it would hang `tin run` forever, and
	// shell-out callers (test fixtures using run_tin_flags) inherit the
	// hang.
	var runErr error
	if memcheck != "" {
		runErr = runMemcheck(memcheck, run)
	} else {
		runErr = run.Run()
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}

		die("run error: %v", runErr)
	}
}

// runClean removes per-program cache directories under .build/ but preserves
// the two content-addressed caches that never go stale:
//
//	.build/csrc/    - runtime.c and stdlib //!+file.c objects keyed by
//	                  sha(content + flags); wiping forces a slow rebuild
//	                  of every C source for no observable correctness gain.
//	.build/pure-fn/ - per-function CTFE shared objects keyed by Merkle
//	                  hash of the #pure function and its dependencies;
//	                  wiping forces every #pure call to re-emit + re-link
//	                  a .so on the next compile.
//
// Both caches are immutable once written (their key encodes their inputs),
// so a stale entry is never possible - only orphaned entries from removed
// code, which cost tens of KB and are cheap to ignore. Silent on success;
// no-op if .build/ is missing.
func runClean() {
	entries, err := os.ReadDir(".build")
	if err != nil {
		if os.IsNotExist(err) {
			return
		}

		die("clean: %v", err)
	}

	for _, e := range entries {
		// Preserve content-addressed caches: csrc (runtime + //!+file.c
		// objects), pure-fn (CTFE per-fn .so), pkg (per-pkg .o under
		// step 7 of incremental compilation). These are pure caches:
		// hits never reuse stale entries because the key includes the
		// content + every flag, so leaving them speeds up subsequent
		// builds without correctness risk.
		//
		// Everything else gets wiped:
		//   .build/run/, .build/test/, .build/build/  -- whole-program artifacts
		//   .build/host-info/                         -- toolchain probes
		//                                                (clang version, target
		//                                                triple, lld argv) AND
		//                                                the per-tool presence
		//                                                markers under tools/.
		//                                                Cheap to redo, but
		//                                                clearing them is the
		//                                                only way to force a
		//                                                fresh probe if the
		//                                                user changed clang
		//                                                in-place.
		if e.Name() == "csrc" || e.Name() == "pure-fn" || e.Name() == "pkg" {
			continue
		}

		if err := os.RemoveAll(filepath.Join(".build", e.Name())); err != nil {
			die("clean: %v", err)
		}
	}
}

// runFmt is the entry point for `tin fmt`. Without --check, it rewrites
// each file in place when the formatted output differs. With --check, it
// leaves files alone, prints the names of any that aren't formatted, and
// exits non-zero if at least one file would change.
func runFmt(args []string) {
	check := false

	var files []string

	for _, a := range args {
		switch a {
		case "--check", "-check":
			check = true
		case "-h", "--help":
			_, _ = fmt.Fprint(os.Stderr,
				"usage: tin fmt [--check] <file.tin> [more.tin ...]\n")

			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") {
				die("fmt: unknown flag %q", a)
			}

			files = append(files, a)
		}
	}

	if len(files) == 0 {
		_, _ = fmt.Fprint(os.Stderr,
			"usage: tin fmt [--check] <file.tin> [more.tin ...]\n")

		os.Exit(1)
	}

	anyDiff := false

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			die("fmt: %v", err)
		}

		out, changed := format.Format(string(src))

		if !changed {
			continue
		}

		anyDiff = true

		if check {
			fmt.Println(f)

			continue
		}

		if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
			die("fmt: writing %s: %v", f, err)
		}
	}

	if check && anyDiff {
		os.Exit(1)
	}
}

// isStderrTTY reports whether stderr is connected to a terminal.
// Used by --color=auto to enable ANSI escapes in the snippet
// renderer. golang.org/x/term would be more portable; the bare stat()
// check works on POSIX which is the only target Tin runs on today.
func isStderrTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}

	return (fi.Mode() & os.ModeCharDevice) != 0
}

func isStdoutTTY() bool {
	if _, hasNoColor := os.LookupEnv("NO_COLOR"); hasNoColor {
		return false
	}

	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}

	return (fi.Mode() & os.ModeCharDevice) != 0
}

func greenIfTTY(s string) string {
	if !isStdoutTTY() {
		return s
	}

	return "\x1b[32m" + s + "\x1b[0m"
}

func redIfTTY(s string) string {
	if !isStdoutTTY() {
		return s
	}

	return "\x1b[31m" + s + "\x1b[0m"
}

func blueIfTTY(s string) string {
	if !isStdoutTTY() {
		return s
	}

	return "\x1b[34m" + s + "\x1b[0m"
}

// memcheckColorWriter wraps an underlying writer (os.Stdout / os.Stderr) and
// colorizes recognized valgrind / macOS leaks summary lines. Buffers partial
// lines across Write calls so a colorize pattern split across writes is not
// missed.
type memcheckColorWriter struct {
	w   io.Writer
	buf []byte
	tty bool
}

// memcheckOKLine matches lines we want to highlight green: a clean
// valgrind run or a "0 leaks for 0 total leaked bytes" report.
var memcheckOKLine = regexp.MustCompile(`All heap blocks were freed -- no leaks are possible|\b0 leaks for 0 total leaked bytes\b`)

// memcheckErrLine matches lines we want to highlight red: any non-zero
// leak count from macOS leaks, or known valgrind error markers.
var memcheckErrLine = regexp.MustCompile(`\b[1-9][0-9]* leak(s)? for [0-9]+ total leaked bytes\b|ERROR SUMMARY: [1-9][0-9]* errors|definitely lost: [1-9][0-9,]* bytes|indirectly lost: [1-9][0-9,]* bytes|Invalid (read|write|free)|uninitialised value`)

func newMemcheckColorWriter(w *os.File) *memcheckColorWriter {
	if _, hasNoColor := os.LookupEnv("NO_COLOR"); hasNoColor {
		return &memcheckColorWriter{w: w, tty: false}
	}

	fi, err := w.Stat()
	tty := err == nil && (fi.Mode()&os.ModeCharDevice) != 0

	return &memcheckColorWriter{w: w, tty: tty}
}

func (m *memcheckColorWriter) Write(p []byte) (int, error) {
	if !m.tty {
		return m.w.Write(p)
	}

	m.buf = append(m.buf, p...)

	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}

		line := m.buf[:i+1]
		m.buf = m.buf[i+1:]

		if _, err := m.w.Write(colorizeMemcheckLine(line)); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func (m *memcheckColorWriter) Flush() error {
	if len(m.buf) == 0 {
		return nil
	}

	_, err := m.w.Write(colorizeMemcheckLine(m.buf))
	m.buf = nil

	return err
}

func colorizeMemcheckLine(line []byte) []byte {
	if memcheckErrLine.Match(line) {
		return memcheckErrLine.ReplaceAllFunc(line, func(m []byte) []byte {
			return append(append([]byte("\x1b[31m"), m...), "\x1b[0m"...)
		})
	}

	if memcheckOKLine.Match(line) {
		return memcheckOKLine.ReplaceAllFunc(line, func(m []byte) []byte {
			return append(append([]byte("\x1b[32m"), m...), "\x1b[0m"...)
		})
	}

	return line
}
