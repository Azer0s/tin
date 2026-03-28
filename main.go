package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azer0s/tin/ast"
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
  tin preprocess <file.tin>         expand macros and print source to stdout

Linker flags (passed after the source file):
  -lNAME       link with libNAME (e.g. -lm for libmath)
  -LDIR        add DIR to the library search path
  file.o/.a    link with extra object/archive file

In-source directives (at the top of the .tin file):
  //!-lNAME            link with libNAME
  //!+file.c           compile C source file alongside the tin module
  //!+file.c -- FLAGS  compile C source with extra clang flags
`

// cSource represents a C source file to compile alongside the tin module,
// optionally with extra clang flags (from //!+file.c -- -DFOO directives).
type cSource struct {
	path  string
	flags []string
}

// parseFileDirectives scans the leading lines of src for //! directives and
// returns linker flags and C source files to compile in.
//
//	//!-lm            → linker flag -lm
//	//!-lraylib       → linker flag -lraylib
//	//!-L/usr/local/lib → linker flag -L/usr/local/lib
//	//!+helper.c      → compile helper.c alongside the module
//	//!+src/foo.c -- -DDEBUG → compile src/foo.c with extra flag -DDEBUG
//
// srcDir is the directory of the .tin file; relative C source paths are
// resolved against it. Scanning stops at the first non-comment, non-blank line.
func parseFileDirectives(src, srcDir string) (linkerFlags []string, cSources []cSource) {
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "//!") {
			continue
		}
		if strings.HasPrefix(trimmed, "//!") {
			rest := strings.TrimSpace(trimmed[3:])
			if rest == "" {
				continue
			}
			if strings.HasPrefix(rest, "+") {
				spec := strings.TrimSpace(rest[1:])
				parts := strings.SplitN(spec, " -- ", 2)
				cpath := filepath.Join(srcDir, strings.TrimSpace(parts[0]))
				var extraFlags []string
				if len(parts) == 2 {
					for _, f := range strings.Fields(parts[1]) {
						extraFlags = append(extraFlags, f)
					}
				}
				cSources = append(cSources, cSource{path: cpath, flags: extraFlags})
			} else {
				linkerFlags = append(linkerFlags, rest)
			}
			continue
		}
		break
	}
	return
}

func main() {
	if len(os.Args) < 3 {
		_, _ = fmt.Fprint(os.Stderr, usage)
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
			_, _ = fmt.Fprint(os.Stderr, usage)
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

	// Collect directives declared in the source file via //! lines
	fileLinkerFlags, fileCSources := parseFileDirectives(string(src), filepath.Dir(file))

	// Lex
	l := lexer.New(string(src))
	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		die("lex error: %v", lexErr)
	}

	// Parse
	p := parser.New(tokens)
	prog, parseErr := p.Parse()
	if parseErr != nil {
		die("parse error: %v", parseErr)
	}

	// Preprocess: expand macros and print expanded source (no codegen/IR)
	if cmd == "preprocess" {
		cg := codegen.New(file)
		expanded, ppErr := cg.ExpandProgramMacros(prog)
		if ppErr != nil {
			die("preprocess error: %v", ppErr)
		}
		for _, stmt := range expanded.Stmts {
			if _, isMacro := stmt.(*ast.MacroDecl); isMacro {
				continue // macro decls are consumed; not printed
			}
			fmt.Println(ast.PrintStmt(stmt, 0))
		}
		return
	}

	// Codegen
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
		if err := compileIR(irText, out, libMode, extraObjs, fileCSources); err != nil {
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
		if err := compileIR(irText, out, false, extraObjs, fileCSources); err != nil {
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
		if err := compileIR(irText, tmp, false, extraObjs, fileCSources); err != nil {
			die("compile error: %v", err)
		}
		defer func(name string) {
			_ = os.Remove(name)
		}(tmp)
		run := exec.Command(tmp)
		run.Stdout = os.Stdout
		run.Stderr = os.Stderr
		if err := run.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			die("run error: %v", err)
		}

	default:
		_, _ = fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// If libMode is true, compile to an object file with -c (no linking).
// extraObjs are additional .o/.a files and -l/-L flags to pass to the linker.
// cSources are C source files to compile in alongside the IR.
func compileIR(ir, outBin string, libMode bool, extraObjs []string, cSources []cSource) error {
	// Write IR to temp file
	//goland:noinspection GoResourceLeak
	llFile, err := os.CreateTemp("", "tin-*.ll")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer func(name string) {
		_ = os.Remove(name)
	}(llFile.Name())
	if _, err := llFile.WriteString(ir); err != nil {
		return err
	}
	_ = llFile.Close()

	// Find runtime .c alongside the tin binary
	ex, _ := os.Executable()
	rtC := filepath.Join(filepath.Dir(ex), "runtime", "runtime.c")

	if libMode {
		// Library mode: compile to object file(s) with -c, then merge with ld -r.
		// clang -c cannot write multiple inputs to a single -o, so each source is
		// compiled separately and the results are partially linked together.
		irObj, err := os.CreateTemp("", "tin-ir-*.o")
		if err != nil {
			return fmt.Errorf("cannot create temp object file: %w", err)
		}
		irObjName := irObj.Name()
		_ = irObj.Close()
		defer func() { _ = os.Remove(irObjName) }()

		clangIR := exec.Command("clang", "-O2", "-c", llFile.Name(), "-o", irObjName)
		clangIR.Stdout = os.Stdout
		clangIR.Stderr = os.Stderr
		if err := clangIR.Run(); err != nil {
			return err
		}

		objs := []string{irObjName}
		var tmpObjs []string
		for _, cs := range cSources {
			cObj, err := os.CreateTemp("", "tin-c-*.o")
			if err != nil {
				return fmt.Errorf("cannot create temp object file: %w", err)
			}
			cObjName := cObj.Name()
			_ = cObj.Close()
			tmpObjs = append(tmpObjs, cObjName)
			cArgs := []string{"-O2", "-c"}
			cArgs = append(cArgs, cs.flags...)
			cArgs = append(cArgs, cs.path, "-o", cObjName)
			clangC := exec.Command("clang", cArgs...)
			clangC.Stdout = os.Stdout
			clangC.Stderr = os.Stderr
			if err := clangC.Run(); err != nil {
				for _, f := range tmpObjs {
					_ = os.Remove(f)
				}
				return err
			}
			objs = append(objs, cObjName)
		}
		defer func() {
			for _, f := range tmpObjs {
				_ = os.Remove(f)
			}
		}()

		// Merge all object files into the final output with ld -r (partial link)
		ldArgs := append([]string{"-r"}, objs...)
		ldArgs = append(ldArgs, "-o", outBin)
		ld := exec.Command("ld", ldArgs...)
		ld.Stdout = os.Stdout
		ld.Stderr = os.Stderr
		return ld.Run()
	}

	args := []string{"-O2", llFile.Name()}
	if _, err := os.Stat(rtC); err == nil {
		args = append(args, rtC)
	}
	for _, cs := range cSources {
		args = append(args, cs.flags...)
		args = append(args, cs.path)
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
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: lex error: %v\n", e.Name(), lexErr)
			continue
		}
		p := parser.New(tokens)
		prog, parseErr := p.Parse()
		if parseErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: parse error: %v\n", e.Name(), parseErr)
			continue
		}
		cg := codegen.New(fpath)
		cg.SetTestMode(true)
		mod, cgErr := cg.Generate(prog)
		if cgErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: codegen error: %v\n", e.Name(), cgErr)
			continue
		}
		if !cg.HasTests() {
			continue // no test blocks in this file
		}

		fileLinks, fCSources := parseFileDirectives(string(src), filepath.Dir(fpath))
		srcLinks := append([]string{}, fileLinks...)
		for _, lib := range cg.LinkLibs() {
			srcLinks = append(srcLinks, "-l"+lib)
		}
		linkFlags := append(srcLinks, extraFlags...)

		fmt.Printf("\n=== %s ===\n", e.Name())

		tmp, tmpErr := os.CreateTemp("", "tin-test-*.out")
		if tmpErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "  error: %v\n", tmpErr)
			results = append(results, result{e.Name(), false})
			continue
		}
		_ = tmp.Close()
		//goland:noinspection GoDeferInLoop
		defer func(name string) {
			_ = os.Remove(name)
		}(tmp.Name())

		irText := mod.String()
		if compErr := compileIR(irText, tmp.Name(), false, linkFlags, fCSources); compErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "  compile error: %v\n", compErr)
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
	_, _ = fmt.Fprintf(os.Stderr, "tin: "+format+"\n", args...)
	os.Exit(1)
}
