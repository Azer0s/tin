package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

const usage = `tin - the tin language compiler

Usage:
  tin run         <file.tin>               compile and execute
  tin build       <file.tin> [-o out]      compile to native binary
  tin build       -lib <file.tin> [-o out] compile to object file (library)
  tin ir          <file.tin> [-o out]      emit LLVM IR (default: stdout)
  tin ir-test     <file.tin> [-o out]      emit test-mode LLVM IR
  tin test        <file.tin|dir|dir/...>   run test blocks and report results
  tin build-test  <file.tin> [-o out]      compile test binary without running
  tin preprocess  <file.tin>               expand macros and print source to stdout

Link flags (passed after the source file):
  -lNAME           link with libNAME (e.g. -lm for libmath)
  -LDIR            add DIR to the library search path
  file.o / file.a  link with extra object or archive file
  -cflag FLAG      pass FLAG to clang (repeatable, e.g. -cflag -fsanitize=address)

Warning flags:
  -Wno-async-main          suppress "main() uses spawn/await but is not async" warning
  -Wno-await-match-guards  suppress warning about guards in await-match arms

Stdlib/libs flags:
  --stdlib PATH    override the standard library path (default: <execDir>/stdlib)
  --lib-root PATH  add an additional package root (before default <execDir>/libs); repeatable

Run/test flags:
  -v-valgrind      run binary under valgrind --leak-check=full (run, test)
  -v-leaks         run binary under leaks --atExit (run, test; macOS only)

In-source directives (at the top of the .tin file):
  //!-lNAME                    link with libNAME
  //!+file.c                   compile C source file alongside the tin module
  //!+file.c -- FLAGS          compile C source with extra clang flags
  //!+file.c [arch]            compile only on matching arch
  //!+file.c [arch] -- FLAGS   arch-specific file with extra flags
  //!-lNAME [arch]             arch-specific linker flag

  Arch tokens: x86_64, aarch64, darwin, linux  (comma = AND, e.g. [aarch64,darwin])
  Variables: $TIN_RUNTIME expands to <execDir>/runtime, $TIN_STDLIB expands to <execDir>/stdlib
`

// cSource represents a C source file to compile alongside the tin module,
// optionally with extra clang flags (from //!+file.c -- -DFOO directives).
type cSource struct {
	path  string
	flags []string
}

// tinRuntimeDir returns the path of the runtime/ directory that ships next to
// the tin binary.  Used to expand $TIN_RUNTIME in //!+file.c flag directives.
func tinRuntimeDir() string {
	ex, err := os.Executable()
	if err != nil {
		return "runtime"
	}

	return filepath.Join(filepath.Dir(ex), "runtime")
}

// stdlibDirForDirectives returns the effective stdlib path for $TIN_STDLIB expansion.
// Uses override if provided; otherwise falls back to <execDir>/stdlib.
func stdlibDirForDirectives(override string) string {
	if override != "" {
		return override
	}

	ex, err := os.Executable()
	if err != nil {
		return "stdlib"
	}

	return filepath.Join(filepath.Dir(ex), "stdlib")
}

// archMatches reports whether the optional [arch] qualifier in a directive
// matches the current platform. qualifier is the raw bracket content, e.g.
// "x86_64" or "aarch64,darwin". Returns true when qualifier is empty (no
// constraint) or every comma-separated token matches.
//
// Supported tokens:
//
//	x86_64   - runtime.GOARCH == "amd64"
//	aarch64  - runtime.GOARCH == "arm64"
//	darwin   - runtime.GOOS  == "darwin"
//	linux    - runtime.GOOS  == "linux"
func archMatches(qualifier string) bool {
	if qualifier == "" {
		return true
	}

	for _, tok := range strings.Split(qualifier, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "x86_64":
			if runtime.GOARCH != "amd64" {
				return false
			}
		case "aarch64":
			if runtime.GOARCH != "arm64" {
				return false
			}
		case "darwin":
			if runtime.GOOS != "darwin" {
				return false
			}
		case "linux":
			if runtime.GOOS != "linux" {
				return false
			}
		}
	}

	return true
}

// extractArchQualifier splits a directive token (file path or linker flag) into
// the base value and an optional arch qualifier. The qualifier is the content
// inside trailing [...], e.g.:
//
//	"helper.c [x86_64]"  -> ("helper.c", "x86_64")
//	"-lm [darwin]"       -> ("-lm", "darwin")
//	"helper.c"           -> ("helper.c", "")
func extractArchQualifier(s string) (base, qualifier string) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "["); i >= 0 && strings.HasSuffix(s, "]") {
		qualifier = strings.TrimSpace(s[i+1 : len(s)-1])
		base = strings.TrimSpace(s[:i])

		return
	}

	return s, ""
}

// parseFileDirectives scans the leading lines of src for //! directives and
// returns linker flags and C source files to compile in.
//
//	//!-lm                         -> linker flag -lm
//	//!-lm [x86_64]                -> linker flag -lm, x86_64 only
//	//!+helper.c                   -> compile helper.c alongside the module
//	//!+src/foo.c -- -DDEBUG       -> compile src/foo.c with extra flag -DDEBUG
//	//!+src/foo.c [arch]           -> compile only on matching arch
//	//!+src/foo.c [arch] -- FLAGS  -> arch-specific file with extra flags
//
// srcDir is the directory of the .tin file; relative C source paths are
// resolved against it. Scanning stops at the first non-comment, non-blank line.
func parseFileDirectives(src, srcDir, stdlibDir string) (linkerFlags []string, cSources []cSource) {
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
				// Split on " -- " to separate file+qualifier from extra flags.
				parts := strings.SplitN(spec, " -- ", 2)
				fileAndQualifier, archQualifier := extractArchQualifier(strings.TrimSpace(parts[0]))

				if !archMatches(archQualifier) {
					continue
				}

				cpath := filepath.Join(srcDir, fileAndQualifier)

				var extraFlags []string

				if len(parts) == 2 {
					rtDir := tinRuntimeDir()

					fields := strings.Fields(parts[1])
					for i := 0; i < len(fields); i++ {
						f := strings.ReplaceAll(fields[i], "$TIN_RUNTIME", rtDir)
						f = strings.ReplaceAll(f, "$TIN_STDLIB", stdlibDir)

						var iPath string

						if f == "-I" && i+1 < len(fields) {
							// "-I path" (space-separated)
							i++
							iPath = strings.ReplaceAll(fields[i], "$TIN_RUNTIME", rtDir)
							iPath = strings.ReplaceAll(iPath, "$TIN_STDLIB", stdlibDir)
						} else if strings.HasPrefix(f, "-I") && len(f) > 2 {
							// "-Ipath" (no space)
							iPath = f[2:]
						}

						if iPath != "" {
							if !filepath.IsAbs(iPath) {
								iPath = filepath.Join(srcDir, iPath)
							}

							extraFlags = append(extraFlags, "-I"+iPath)
						} else {
							extraFlags = append(extraFlags, f)
						}
					}
				}

				cSources = append(cSources, cSource{path: cpath, flags: extraFlags})
			} else {
				// Linker flag: check for optional arch qualifier.
				flagAndQualifier, archQualifier := extractArchQualifier(rest)
				if archMatches(archQualifier) {
					linkerFlags = append(linkerFlags, flagAndQualifier)
				}
			}

			continue
		}

		break
	}

	return
}

func main() {
	if v := clangMajorVersion(); v > 0 && v < 15 {
		_, _ = fmt.Fprintf(os.Stderr,
			"error: clang version %d is too old; tin requires clang >= 15 (the presplitcoroutine attribute was added in LLVM 15)\n", v)

		os.Exit(1)
	}

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

	// Collect -cflag values and warning-suppression flags from anywhere after the file arg.
	var extraCFlags []string

	var stdlibOverride string

	var extraLibsRoots []string

	noWarnAsyncMain := false
	noWarnAwaitMatchGuards := false

	for i := fileArgIdx + 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-cflag":
			if i+1 < len(os.Args) {
				i++
				extraCFlags = append(extraCFlags, os.Args[i])
			}
		case "--stdlib":
			if i+1 < len(os.Args) {
				i++
				stdlibOverride = os.Args[i]
			}
		case "--lib-root":
			if i+1 < len(os.Args) {
				i++
				extraLibsRoots = append(extraLibsRoots, os.Args[i])
			}
		case "-Wno-async-main":
			noWarnAsyncMain = true
		case "-Wno-await-match-guards":
			noWarnAwaitMatchGuards = true
		}
	}

	// Directory mode: tin test <dir> runs all test files in a directory.
	// tin test <dir>/... recurses into all subdirectories (Go-style wildcard).
	if cmd == "test" {
		recursive := filepath.Base(file) == "..."
		if recursive {
			file = filepath.Dir(file)
		}

		if fi, statErr := os.Stat(file); statErr == nil && fi.IsDir() {
			// Collect extra link flags and memory-checker flag from remaining args.
			var extraFlags []string

			memcheck := ""

			for i := fileArgIdx + 1; i < len(os.Args); i++ {
				a := os.Args[i]
				if a == "-v-valgrind" {
					memcheck = "valgrind"
				} else if a == "-v-leaks" {
					memcheck = "leaks"
				} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") ||
					strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
					extraFlags = append(extraFlags, a)
				}
			}

			validateMemcheck(memcheck)

			if recursive {
				runDirTestsRecursive(file, extraFlags, memcheck)
			} else {
				runDirTests(file, extraFlags, memcheck)
			}

			return
		}
	}

	src, err := os.ReadFile(file)
	if err != nil {
		die("error reading file: %v", err)
	}

	// Collect directives declared in the source file via //! lines
	fileLinkerFlags, fileCSources := parseFileDirectives(string(src), filepath.Dir(file), stdlibDirForDirectives(stdlibOverride))

	// Lex
	l := lexer.New(string(src))

	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		die("lex error: %v", lexErr)
	}

	// Parse
	// Pre-scan for #no_parens macros from `use { } from` imports so the parser
	// can do token substitution for them before parsing begins.
	p := parser.New(tokens)
	for name, expansion := range codegen.ScanImportedNoParensMacros(file, tokens, stdlibDirForDirectives(stdlibOverride), nil) {
		p.RegisterNoParensMacro(name, expansion)
	}

	if noWarnAwaitMatchGuards {
		p.SetNoWarnAwaitMatchGuards(true)
	}

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

	if noWarnAsyncMain {
		cg.SetNoWarnAsyncMain(true)
	}

	// On Apple arm64, long double == double and compiler-rt has no fp128 routines.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		cg.SetUseDoubleForF128(true)
	}

	if stdlibOverride != "" {
		cg.SetStdlibOverride(stdlibOverride)
	}

	for _, r := range extraLibsRoots {
		cg.AddLibsRoot(r)
	}

	mod, cgErr := cg.Generate(prog)
	if cgErr != nil {
		die("codegen error: %v", cgErr)
	}

	irText := fixCoroAttrs(mod.String())

	// Collect C sources and linker flags from loaded package source files.
	// Packages may declare //!+file.c directives that need to be compiled in.
	for _, pkgSrc := range cg.PackageSrcPaths() {
		src, readErr := os.ReadFile(pkgSrc)
		if readErr != nil {
			continue
		}

		pkgLinkFlags, pkgCSources := parseFileDirectives(string(src), filepath.Dir(pkgSrc), stdlibDirForDirectives(stdlibOverride))
		fileLinkerFlags = append(fileLinkerFlags, pkgLinkFlags...)
		fileCSources = append(fileCSources, pkgCSources...)
	}

	// Deduplicate C sources by path (a single .c helper may be referenced by
	// multiple .tin files via //!+file.c; only compile it once).
	{
		seen := map[string]bool{}

		deduped := fileCSources[:0]
		for _, cs := range fileCSources {
			if !seen[cs.path] {
				seen[cs.path] = true
				deduped = append(deduped, cs)
			}
		}

		fileCSources = deduped
	}
	// Deduplicate linker flags too.
	{
		seen := map[string]bool{}

		deduped := fileLinkerFlags[:0]
		for _, f := range fileLinkerFlags {
			if !seen[f] {
				seen[f] = true
				deduped = append(deduped, f)
			}
		}

		fileLinkerFlags = deduped
	}

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
			} else if a == "-cflag" {
				i++ // value already collected above
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}

		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, out, libMode, extraObjs, fileCSources, extraCFlags); err != nil {
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
			} else if a == "-cflag" {
				i++ // value already collected above
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}

		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, out, false, extraObjs, fileCSources, extraCFlags); err != nil {
			die("compile error: %v", err)
		}

	case "run", "test":
		tmpRel := strings.TrimSuffix(file, filepath.Ext(file)) + ".tin.out"
		tmp, _ := filepath.Abs(tmpRel)
		// Collect extra link inputs and memory-checker flag for run/test mode.
		var extraObjs []string

		memcheck := ""

		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if a == "-v-valgrind" {
				memcheck = "valgrind"
			} else if a == "-v-leaks" {
				memcheck = "leaks"
			} else if a == "-cflag" {
				i++ // value already collected above
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}

		extraObjs = append(srcLinkFlags, extraObjs...)

		if err := compileIR(irText, tmp, false, extraObjs, fileCSources, extraCFlags); err != nil {
			die("compile error: %v", err)
		}
		defer func(name string) {
			_ = os.Remove(name)
		}(tmp)

		validateMemcheck(memcheck)

		run := memcheckCmd(memcheck, tmp)
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

// clangMajorVersion runs `clang --version` and returns the major version number,
// or 0 if the version cannot be determined.
func clangMajorVersion() int {
	out, err := exec.Command("clang", "--version").Output()
	if err != nil {
		return 0
	}

	// Output looks like: "Ubuntu clang version 18.1.3" or "clang version 14.0.0"
	// Find "version " followed by a decimal major number.
	s := string(out)

	idx := strings.Index(s, "version ")
	if idx < 0 {
		return 0
	}

	s = s[idx+len("version "):]
	major := 0

	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			major = major*10 + int(ch-'0')
		} else {
			break
		}
	}

	return major
}

// fixCoroAttrs rewrites the LLVM IR string emitted by the llir library to
// produce valid IR for the installed clang version:
//
//  1. "presplitcoroutine" string attr -> keyword attr (required by coro-split).
//  2. On macOS Apple Silicon with clang 21, llvm.coro.end requires i1 return
//     type and ptr argument; patch the declaration and call sites there.
func fixCoroAttrs(ir string) string {
	ir = strings.ReplaceAll(ir, `"presplitcoroutine"`, "presplitcoroutine")

	// Observed on macOS Apple Silicon (arm64) with clang 21: llvm.coro.end
	// requires i1 return type and ptr argument (rather than void/i8*).
	// Use a named result to avoid shifting implicit SSA slot numbering.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && clangMajorVersion() == 21 {
		ir = strings.ReplaceAll(ir, "declare void @llvm.coro.end(i8*", "declare i1 @llvm.coro.end(ptr")
		ir = strings.ReplaceAll(ir, "call void @llvm.coro.end(i8*", "%_coroend = call i1 @llvm.coro.end(ptr")
	}

	return ir
}

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// If libMode is true, compile to an object file with -c (no linking).
// extraObjs are additional .o/.a files and -l/-L flags to pass to the linker.
// cSources are C source files to compile in alongside the IR.
func compileIR(ir, outBin string, libMode bool, extraObjs []string, cSources []cSource, extraCFlags []string) error {
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

	if dumpPath := os.Getenv("TIN_DUMP_IR"); dumpPath != "" {
		_ = os.WriteFile(dumpPath, []byte(ir), 0644)
	}

	llInputFile := llFile.Name()

	// LLVM 22's -O2 optimizer breaks coroutine yield paths: the "suspended"
	// default arm of coro.suspend in the resume function is marked unreachable,
	// causing backward DCE to remove the `store index; ret void` that the
	// scheduler depends on.  Work around by splitting coroutines at -O1 first
	// (which produces correct yield paths), then running -O2 on the split IR.
	if strings.Contains(ir, "llvm.coro.") {
		splitFile, err := os.CreateTemp("", "tin-split-*.ll")
		if err != nil {
			return fmt.Errorf("cannot create temp file for coro split: %w", err)
		}

		splitName := splitFile.Name()

		_ = splitFile.Close()

		defer func() { _ = os.Remove(splitName) }()

		split := exec.Command("clang", "-O1", "-S", "-emit-llvm", llInputFile, "-o", splitName)
		split.Stdout = os.Stdout

		split.Stderr = os.Stderr

		if err := split.Run(); err != nil {
			return fmt.Errorf("coro split pass failed: %w", err)
		}

		llInputFile = splitName
	}

	optLevel := "-O2"

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

		clangIR := exec.Command("clang", optLevel, "-c", llInputFile, "-o", irObjName)
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

	// Compile each cSource at -O2 (always safe: C files never contain coro
	// intrinsics, so -O2 is correct and avoids the -O1 penalty forced on the IR).
	// Linker flags (-l/-L) are separated out and passed only at link time.
	var (
		tmpCObjs     []string
		cObjPaths    []string
		cLinkerFlags []string
	)

	for _, cs := range cSources {
		var compileFlags []string

		for _, f := range cs.flags {
			if strings.HasPrefix(f, "-l") || strings.HasPrefix(f, "-L") {
				cLinkerFlags = append(cLinkerFlags, f)
			} else {
				compileFlags = append(compileFlags, f)
			}
		}

		cObj, tmpErr := os.CreateTemp("", "tin-c-*.o")
		if tmpErr != nil {
			return fmt.Errorf("cannot create temp object file: %w", tmpErr)
		}

		cObjName := cObj.Name()
		_ = cObj.Close()

		tmpCObjs = append(tmpCObjs, cObjName)
		cArgs := []string{"-O2", "-c"}
		cArgs = append(cArgs, compileFlags...)
		cArgs = append(cArgs, cs.path, "-o", cObjName)
		clangC := exec.Command("clang", cArgs...)
		clangC.Stdout = os.Stdout

		clangC.Stderr = os.Stderr
		if err := clangC.Run(); err != nil {
			for _, f := range tmpCObjs {
				_ = os.Remove(f)
			}

			return err
		}

		cObjPaths = append(cObjPaths, cObjName)
	}

	defer func() {
		for _, f := range tmpCObjs {
			_ = os.Remove(f)
		}
	}()

	args := []string{optLevel, llInputFile}
	if _, err := os.Stat(rtC); err == nil {
		args = append(args, rtC)
	}

	args = append(args, cObjPaths...)
	args = append(args, cLinkerFlags...)
	args = append(args, extraObjs...)
	args = append(args, extraCFlags...)
	args = append(args, "-o", outBin)

	clang := exec.Command("clang", args...)
	clang.Stdout = os.Stdout
	clang.Stderr = os.Stderr

	return clang.Run()
}

// collectTinFiles recursively collects all .tin file paths under root,
// skipping directories named "wip".
func collectTinFiles(root string) []string {
	var files []string

	var walk func(dir string)

	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}

		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".tin" {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}

		for _, e := range entries {
			if e.IsDir() && e.Name() != "wip" {
				walk(filepath.Join(dir, e.Name()))
			}
		}
	}

	walk(root)

	return files
}

// validateMemcheck checks that the requested memory checker is available on
// the current platform and exits with a helpful error if not.
func validateMemcheck(memcheck string) {
	switch memcheck {
	case "valgrind":
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("valgrind"); err != nil {
				die("valgrind is not supported on macOS; did you mean -v-leaks?")
			}
		}
	case "leaks":
		if runtime.GOOS != "darwin" {
			die("leaks is a macOS-only tool; did you mean -v-valgrind?")
		}
	}
}

// memcheckCmd builds the exec.Cmd to run binary under the requested checker.
func memcheckCmd(memcheck, binary string) *exec.Cmd {
	switch memcheck {
	case "valgrind":
		return exec.Command("valgrind", "--error-exitcode=1", "--leak-check=full", binary)
	case "leaks":
		return exec.Command("leaks", "--atExit", "--", binary)
	default:
		return exec.Command(binary)
	}
}

// runDirTestsRecursive collects all .tin files under root and runs them
// together as a single test batch with one combined summary.
func runDirTestsRecursive(root string, extraFlags []string, memcheck string) {
	files := collectTinFiles(root)
	runFileTests(files, extraFlags, memcheck)
}

// runDirTests runs all .tin files in dir that contain test blocks.
// It prints a per-file header and aggregate summary, then exits non-zero
// if any file has failing tests.
func runDirTests(dir string, extraFlags []string, memcheck string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		die("cannot read directory %s: %v", dir, err)
	}

	var files []string

	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".tin" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}

	runFileTests(files, extraFlags, memcheck)
}

// runFileTests runs the given .tin files that contain test blocks.
// It prints a per-file header and aggregate summary, then exits non-zero
// if any file has failing tests.  memcheck is "", "valgrind", or "leaks".
func runFileTests(fpaths []string, extraFlags []string, memcheck string) {
	type result struct {
		file    string
		passed  bool
		skipped bool
		reason  string
	}

	wd, _ := os.Getwd()

	var results []result

	for _, fpath := range fpaths {
		rel, relErr := filepath.Rel(wd, fpath)
		if relErr != nil {
			rel = fpath
		}

		fname := rel

		src, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}

		l := lexer.New(string(src))

		tokens, lexErr := l.Tokenize()
		if lexErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: lex error: %v\n", fname, lexErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("lex error: %v", lexErr)})

			continue
		}

		p := parser.New(tokens)
		for name, expansion := range codegen.ScanImportedNoParensMacros(fpath, tokens, "", nil) {
			p.RegisterNoParensMacro(name, expansion)
		}

		prog, parseErr := p.Parse()
		if parseErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: parse error: %v\n", fname, parseErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("parse error: %v", parseErr)})

			continue
		}

		cg := codegen.New(fpath)
		cg.SetTestMode(true)

		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			cg.SetUseDoubleForF128(true)
		}

		type modIface interface{ String() string }

		var mod modIface

		cgErr := func() (retErr error) {
			defer func() {
				if r := recover(); r != nil {
					retErr = fmt.Errorf("internal panic: %v", r)
				}
			}()

			m, err := cg.Generate(prog)
			mod = m

			return err
		}()
		if cgErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: codegen error: %v\n", fname, cgErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("codegen error: %v", cgErr)})

			continue
		}

		if !cg.HasTests() {
			continue // no test blocks in this file
		}

		fileLinks, fCSources := parseFileDirectives(string(src), filepath.Dir(fpath), stdlibDirForDirectives(""))

		srcLinks := append([]string{}, fileLinks...)
		for _, lib := range cg.LinkLibs() {
			srcLinks = append(srcLinks, "-l"+lib)
		}
		// Collect //!+file.c and //!-lNAME directives from imported packages,
		// just as the single-file build path does.
		for _, pkgSrc := range cg.PackageSrcPaths() {
			pkgBytes, pkgReadErr := os.ReadFile(pkgSrc)
			if pkgReadErr != nil {
				continue
			}

			pkgLinks, pkgCSrcs := parseFileDirectives(string(pkgBytes), filepath.Dir(pkgSrc), stdlibDirForDirectives(""))
			srcLinks = append(srcLinks, pkgLinks...)
			fCSources = append(fCSources, pkgCSrcs...)
		}
		// Deduplicate C sources (same helper may be pulled in by multiple
		// imported packages, e.g. sync_helpers.c from mutex/rwmutex/cond).
		{
			seen := map[string]bool{}

			deduped := fCSources[:0]
			for _, cs := range fCSources {
				if !seen[cs.path] {
					seen[cs.path] = true
					deduped = append(deduped, cs)
				}
			}

			fCSources = deduped
		}
		// Deduplicate link flags too.
		{
			seen := map[string]bool{}

			deduped := srcLinks[:0]
			for _, f := range srcLinks {
				if !seen[f] {
					seen[f] = true
					deduped = append(deduped, f)
				}
			}

			srcLinks = deduped
		}

		linkFlags := append(srcLinks, extraFlags...)

		tmp, tmpErr := os.CreateTemp("", "tin-test-*.out")
		if tmpErr != nil {
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  error: %v\n", tmpErr)

			results = append(results, result{fname, false, false, fmt.Sprintf("error: %v", tmpErr)})

			continue
		}

		_ = tmp.Close()
		//goland:noinspection GoDeferInLoop
		defer func(name string) {
			_ = os.Remove(name)
		}(tmp.Name())

		irText := fixCoroAttrs(mod.String())
		if compErr := compileIR(irText, tmp.Name(), false, linkFlags, fCSources, nil); compErr != nil {
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  compile error: %v\n", compErr)

			results = append(results, result{fname, false, false, fmt.Sprintf("compile error: %v", compErr)})

			continue
		}

		fmt.Printf("%s\n\n", fname)

		run := memcheckCmd(memcheck, tmp.Name())

		run.Stdout = os.Stdout
		run.Stderr = os.Stderr

		passed := true
		if runErr := run.Run(); runErr != nil {
			passed = false
		}

		fmt.Println("------------------------------------------------")

		reason := ""

		if !passed {
			reason = "test failures"
		}

		results = append(results, result{fname, passed, false, reason})
	}

	if len(results) == 0 {
		fmt.Printf("no test files found\n")

		return
	}

	fmt.Printf("\n")

	failed := 0
	skipped := 0

	for _, r := range results {
		if r.skipped {
			skipped++
		} else if !r.passed {
			failed++
		}
	}

	if failed == 0 && skipped == 0 {
		fmt.Printf("all %d test file(s) passed.\n", len(results))

		return
	}

	passed := len(results) - failed - skipped
	fmt.Printf("%d passed, %d failed, %d skipped (%d total)\n", passed, failed, skipped, len(results))

	if failed > 0 {
		fmt.Printf("\nFailed:\n")

		for _, r := range results {
			if !r.skipped && !r.passed {
				fmt.Printf("  %s: %s\n", r.file, r.reason)
			}
		}
	}

	if skipped > 0 {
		fmt.Printf("\nSkipped:\n")

		for _, r := range results {
			if r.skipped {
				fmt.Printf("  %s: %s\n", r.file, r.reason)
			}
		}
	}

	if failed > 0 || skipped > 0 {
		os.Exit(1)
	}
}

func die(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "tin: "+format+"\n", args...)

	os.Exit(1)
}
