package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

const usage = `tin - the tin language compiler

Usage:
  tin run   <file.tin>              compile and execute
  tin build <file.tin> [-o out]     compile to native binary
  tin build -lib <file.tin> [-o out] compile to object file (library)
  tin ir    <file.tin>              emit LLVM IR to stdout
  tin test  <file.tin>              run test blocks and report results
  tin build-test <file.tin> [-o out] compile test binary without running
  tin ir-test <file.tin>            emit test-mode LLVM IR to stdout

Linker flags (passed after the source file):
  -lNAME       link with libNAME (e.g. -lm for libmath)
  -LDIR        add DIR to the library search path
  file.o/.a    link with extra object/archive file
`

// parseFileLinkerFlags scans the leading lines of src for //! directives and
// returns the flags they specify.  Each directive line has the form:
//
//	//!-lm
//	//!-lraylib
//	//!-L/usr/local/lib
//
// Scanning stops at the first line that is not a comment or blank
func parseFileLinkerFlags(src string) []string {
	var flags []string
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "//!") {
			// blank or ordinary comment — keep scanning
			continue
		}
		if strings.HasPrefix(trimmed, "//!") {
			flag := strings.TrimSpace(trimmed[3:])
			if flag != "" {
				flags = append(flags, flag)
			}
			continue
		}
		break // first non-comment, non-blank line — stop
	}
	return flags
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	cmd := os.Args[1]

	// Parse flags: -lib means compile to object file, not a binary
	libMode := false
	fileArgIdx := 2
	if cmd == "build" && len(os.Args) > 2 && os.Args[2] == "-lib" {
		libMode = true
		fileArgIdx = 3
		if len(os.Args) <= fileArgIdx {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(1)
		}
	}
	file := os.Args[fileArgIdx]

	// Directory mode: tin test <dir> runs all test files in a directory
	if cmd == "test" {
		if fi, statErr := os.Stat(file); statErr == nil && fi.IsDir() {
			// Collect extra link flags from remaining args
			var extraFlags []string
			for i := fileArgIdx + 1; i < len(os.Args); i++ {
				a := os.Args[i]
				if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") ||
					strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
					extraFlags = append(extraFlags, a)
				}
			}
			runDirTests(file, extraFlags)
			return
		}
	}

	src, err := os.ReadFile(file)
	if err != nil {
		die("error reading file: %v", err)
	}

	// Collect linker flags declared in the source file via //! directives
	fileLinkerFlags := parseFileLinkerFlags(string(src))

	// ── Lex ──────────────────────────────────────────────────────────────────
	l := lexer.New(string(src))
	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		die("lex error: %v", lexErr)
	}

	// ── Parse ─────────────────────────────────────────────────────────────────
	p := parser.New(tokens)
	prog, parseErr := p.Parse()
	if parseErr != nil {
		die("parse error: %v", parseErr)
	}

	// ── Codegen ───────────────────────────────────────────────────────────────
	cg := codegen.New(file)
	if cmd == "test" || cmd == "build-test" || cmd == "ir-test" {
		cg.SetTestMode(true)
	}
	mod, cgErr := cg.Generate(prog)
	if cgErr != nil {
		die("codegen error: %v", cgErr)
	}

	irText := mod.String()

	// Collect linker flags: //! directives in the file + codegen link directives
	srcLinkFlags := append([]string{}, fileLinkerFlags...)
	for _, lib := range cg.LinkLibs() {
		srcLinkFlags = append(srcLinkFlags, "-l"+lib)
	}

	switch cmd {
	case "ir", "ir-test":
		fmt.Print(irText)

	case "build":
		out := strings.TrimSuffix(file, filepath.Ext(file))
		if libMode {
			out += ".o"
		}
		// Collect extra link inputs: .o/.a files, -lNAME, -LDIR, -o flag
		var extraObjs []string
		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if a == "-o" {
				i++
				if i < len(os.Args) {
					out = os.Args[i]
				}
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}
		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, out, false, libMode, extraObjs); err != nil {
			die("compile error: %v", err)
		}

	case "build-test":
		out := strings.TrimSuffix(file, filepath.Ext(file)) + ".test"
		var extraObjs []string
		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if a == "-o" {
				i++
				if i < len(os.Args) {
					out = os.Args[i]
				}
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}
		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, out, true, false, extraObjs); err != nil {
			die("compile error: %v", err)
		}

	case "run", "test":
		tmpRel := strings.TrimSuffix(file, filepath.Ext(file)) + ".tin.out"
		tmp, _ := filepath.Abs(tmpRel)
		// Collect extra link inputs for run/test mode too
		var extraObjs []string
		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}
		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, tmp, true, false, extraObjs); err != nil {
			die("compile error: %v", err)
		}
		defer os.Remove(tmp)
		run := exec.Command(tmp)
		run.Stdout = os.Stdout
		run.Stderr = os.Stderr
		if err := run.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			die("run error: %v", err)
		}

	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// compileIR writes the LLVM IR to a temp .ll file and invokes clang
// If libMode is true, compile to an object file with -c (no linking)
// extraObjs are additional .o/.a files to link with
func compileIR(ir, outBin string, addRuntime bool, libMode bool, extraObjs []string) error {
	// Write IR to temp file
	llFile, err := os.CreateTemp("", "tin-*.ll")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer os.Remove(llFile.Name())
	if _, err := llFile.WriteString(ir); err != nil {
		return err
	}
	llFile.Close()

	// Find runtime .c alongside the tin binary
	ex, _ := os.Executable()
	rtC := filepath.Join(filepath.Dir(ex), "runtime", "runtime.c")

	if libMode {
		// Library mode: compile to object file only (-c), no runtime, no linking
		args := []string{"-O2", "-c", llFile.Name(), "-o", outBin}
		clang := exec.Command("clang", args...)
		clang.Stdout = os.Stdout
		clang.Stderr = os.Stderr
		return clang.Run()
	}

	args := []string{"-O2", llFile.Name()}
	if _, err := os.Stat(rtC); err == nil {
		args = append(args, rtC)
	}
	args = append(args, extraObjs...)
	args = append(args, "-o", outBin)

	clang := exec.Command("clang", args...)
	clang.Stdout = os.Stdout
	clang.Stderr = os.Stderr
	return clang.Run()
}

// runDirTests runs all .tin files in dir that contain test blocks
// It prints a per-file header and aggregate summary, then exits non-zero
// if any file has failing tests
func runDirTests(dir string, extraFlags []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		die("cannot read directory %s: %v", dir, err)
	}

	type result struct {
		file   string
		passed bool
	}
	var results []result

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tin" {
			continue
		}
		fpath := filepath.Join(dir, e.Name())

		src, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}

		l := lexer.New(string(src))
		tokens, lexErr := l.Tokenize()
		if lexErr != nil {
			fmt.Fprintf(os.Stderr, "skip %s: lex error: %v\n", e.Name(), lexErr)
			continue
		}
		p := parser.New(tokens)
		prog, parseErr := p.Parse()
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "skip %s: parse error: %v\n", e.Name(), parseErr)
			continue
		}
		cg := codegen.New(fpath)
		cg.SetTestMode(true)
		mod, cgErr := cg.Generate(prog)
		if cgErr != nil {
			fmt.Fprintf(os.Stderr, "skip %s: codegen error: %v\n", e.Name(), cgErr)
			continue
		}
		if !cg.HasTests() {
			continue // no test blocks in this file
		}

		srcLinks := append([]string{}, parseFileLinkerFlags(string(src))...)
		for _, lib := range cg.LinkLibs() {
			srcLinks = append(srcLinks, "-l"+lib)
		}
		linkFlags := append(srcLinks, extraFlags...)

		fmt.Printf("\n=== %s ===\n", e.Name())

		tmp, tmpErr := os.CreateTemp("", "tin-test-*.out")
		if tmpErr != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", tmpErr)
			results = append(results, result{e.Name(), false})
			continue
		}
		tmp.Close()
		defer os.Remove(tmp.Name())

		irText := mod.String()
		if compErr := compileIR(irText, tmp.Name(), true, false, linkFlags); compErr != nil {
			fmt.Fprintf(os.Stderr, "  compile error: %v\n", compErr)
			results = append(results, result{e.Name(), false})
			continue
		}

		run := exec.Command(tmp.Name())
		run.Stdout = os.Stdout
		run.Stderr = os.Stderr
		passed := true
		if runErr := run.Run(); runErr != nil {
			passed = false
		}
		results = append(results, result{e.Name(), passed})
	}

	if len(results) == 0 {
		fmt.Printf("no test files found in %s\n", dir)
		return
	}

	fmt.Printf("\n")
	failed := 0
	for _, r := range results {
		if !r.passed {
			failed++
		}
	}
	if failed == 0 {
		fmt.Printf("all %d test file(s) passed.\n", len(results))
	} else {
		fmt.Printf("%d/%d test file(s) failed.\n", failed, len(results))
		os.Exit(1)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tin: "+format+"\n", args...)
	os.Exit(1)
}
