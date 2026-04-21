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

Target flags:
  -target os/arch  cross-compile for the given target (e.g. linux/amd64, darwin/arm64)
                   Supported: linux/amd64, linux/arm64, linux/386,
                              darwin/amd64, darwin/arm64

Stdlib/libs flags:
  --stdlib PATH    override the standard library path (default: <execDir>/stdlib)
  --lib-root PATH  add an additional package root (before default <execDir>/libs); repeatable

Run/test flags:
  -v-valgrind      run binary under valgrind --leak-check=full (run, test)
  -v-leaks         run binary under leaks --atExit (run, test; macOS only)
Compiler output flags:
  -v               print compilation stages (lex, parse, codegen, link, ...)
  -v-heuristics    print auto-yield heuristics for every function to stderr
  -v-tco           print tail call optimizations (self-TCO and mutual TCO) to stderr

Debug flags:
  -f-debug-fiber-slots  print fiber struct pool ramp/decay events to stderr

In-source directives (at the top of the .tin file):
  //!-lNAME                    link with libNAME
  //!+file.c                   compile C source file alongside the tin module
  //!+file.c -- FLAGS          compile C source with extra clang flags
  //!+file.c [arch]            compile only on matching arch
  //!+file.c [arch] -- FLAGS   arch-specific file with extra flags
  //!-lNAME [arch]             arch-specific linker flag

  Arch tokens: x86_64, aarch64, 386, darwin, linux  (comma = AND, e.g. [aarch64,darwin])
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

// targetGOOS and targetGOARCH reflect the compilation target platform.
// They default to the host and are overridden by the -target os/arch flag.
var (
	targetGOOS     = runtime.GOOS
	targetGOARCH   = runtime.GOARCH
	explicitTarget bool
)

// verbose flags are package-level so directory-mode test runners can use them.
var (
	verboseProgress   bool
	verboseHeuristics bool
	verboseTCO        bool
)

// clangTripleForTarget returns the canonical LLVM target triple for the
// current targetGOOS/targetGOARCH pair.
func clangTripleForTarget() string {
	switch targetGOOS + "/" + targetGOARCH {
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	case "linux/386":
		return "i386-unknown-linux-gnu"
	case "darwin/amd64":
		return "x86_64-apple-macosx11.0.0"
	case "darwin/arm64":
		return "arm64-apple-macosx11.0.0"
	default:
		return ""
	}
}

// clangTargetFlag returns {"-target", triple} when -target was explicitly
// given, otherwise nil (host triple is used implicitly).
func clangTargetFlag() []string {
	if !explicitTarget {
		return nil
	}

	t := clangTripleForTarget()
	if t == "" {
		return nil
	}

	return []string{"-target", t}
}

// archMatches reports whether the optional [arch] qualifier in a directive
// matches the compilation target. qualifier is the raw bracket content, e.g.
// "x86_64" or "aarch64,darwin". Returns true when qualifier is empty (no
// constraint) or every comma-separated token matches.
//
// Supported tokens:
//
//	x86_64   - targetGOARCH == "amd64"
//	aarch64  - targetGOARCH == "arm64"
//	386      - targetGOARCH == "386"
//	darwin   - targetGOOS  == "darwin"
//	linux    - targetGOOS  == "linux"
func archMatches(qualifier string) bool {
	if qualifier == "" {
		return true
	}

	for _, tok := range strings.Split(qualifier, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "x86_64":
			if targetGOARCH != "amd64" {
				return false
			}
		case "aarch64":
			if targetGOARCH != "arm64" {
				return false
			}
		case "386":
			if targetGOARCH != "386" {
				return false
			}
		case "darwin":
			if targetGOOS != "darwin" {
				return false
			}
		case "linux":
			if targetGOOS != "linux" {
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

// expandShellExprs replaces $(cmd args...) tokens in s with the trimmed stdout
// of running that command via the shell. Tokens whose command fails are left
// as empty strings so the caller surfaces a meaningful compiler error instead
// of a cryptic path.
func expandShellExprs(s string) string {
	for {
		start := strings.Index(s, "$(")
		if start == -1 {
			break
		}

		end := strings.Index(s[start+2:], ")")
		if end == -1 {
			break
		}

		end += start + 2
		cmd := s[start+2 : end]
		out, err := exec.Command("sh", "-c", cmd).Output()

		var val string
		if err == nil {
			val = strings.TrimSpace(string(out))
		}

		s = s[:start] + val + s[end+1:]
	}

	return s
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

					expandVars := func(s string) string {
						s = strings.ReplaceAll(s, "$TIN_RUNTIME", rtDir)
						s = strings.ReplaceAll(s, "$TIN_STDLIB", stdlibDir)
						// Expand any remaining $VAR tokens from the environment.
						return os.ExpandEnv(s)
					}

					// Expand $(cmd) expressions before field splitting so
					// commands with spaces (e.g. "brew --prefix foo") aren't
					// split across multiple fields.
					fields := strings.Fields(expandShellExprs(parts[1]))
					for i := 0; i < len(fields); i++ {
						f := expandVars(fields[i])

						var iPath string

						if f == "-I" && i+1 < len(fields) {
							// "-I path" (space-separated)
							i++
							iPath = expandVars(fields[i])
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
					linkerFlags = append(linkerFlags, os.ExpandEnv(expandShellExprs(flagAndQualifier)))
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

	// Parse flags: -lib means compile to object file, not a binary.
	// Scan forward from position 2 to find the first non-flag argument (the source file).
	// Known pre-file flags: -lib (build only), -g.  Two-word flags (--stdlib PATH) are skipped.
	libMode := false
	fileArgIdx := 2

	if cmd == "build" && len(os.Args) > 2 && os.Args[2] == "-lib" {
		libMode = true
		fileArgIdx = 3
	}

	// Skip any flags that appear before the file argument.
	for fileArgIdx < len(os.Args) {
		switch a := os.Args[fileArgIdx]; a {
		case "-g":
			fileArgIdx++
		case "--stdlib", "--lib-root", "-target":
			fileArgIdx += 2
		default:
			goto doneFlags
		}
	}

doneFlags:

	if fileArgIdx >= len(os.Args) {
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(1)
	}

	file := os.Args[fileArgIdx]

	// Collect -cflag values and warning-suppression flags from anywhere after the file arg.
	var extraCFlags []string

	var stdlibOverride string

	var extraLibsRoots []string

	noWarnAsyncMain := false
	noWarnAwaitMatchGuards := false
	debugBuild := false

	// Scan all args (including those before the file) for flags.
	for i := 2; i < len(os.Args); i++ {
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
		case "-v-heuristics":
			verboseHeuristics = true
		case "-v":
			verboseProgress = true
		case "-v-tco":
			verboseTCO = true
		case "-f-debug-fiber-slots":
			extraCFlags = append(extraCFlags, "-DTIN_DEBUG_FIBER_SLOTS=1")
		case "-g":
			debugBuild = true
		case "-target":
			if i+1 < len(os.Args) {
				i++

				parts := strings.SplitN(os.Args[i], "/", 2)
				if len(parts) != 2 {
					die("-target: expected os/arch (e.g. linux/amd64, darwin/arm64)")
				}

				targetGOOS = parts[0]
				targetGOARCH = parts[1]
				explicitTarget = true
			}
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

			var dirExtraCFlags []string

			memcheck := ""

			for i := fileArgIdx + 1; i < len(os.Args); i++ {
				a := os.Args[i]
				if a == "-v-valgrind" {
					memcheck = "valgrind"
				} else if a == "-v-leaks" {
					memcheck = "leaks"
				} else if a == "-f-debug-fiber-slots" {
					dirExtraCFlags = append(dirExtraCFlags, "-DTIN_DEBUG_FIBER_SLOTS=1")
				} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") ||
					strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
					extraFlags = append(extraFlags, a)
				}
			}

			validateMemcheck(memcheck)

			if recursive {
				runDirTestsRecursive(file, extraFlags, dirExtraCFlags, memcheck)
			} else {
				runDirTests(file, extraFlags, dirExtraCFlags, memcheck)
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

	// Estimate total stages for progress display.
	// Actual total is refined after codegen when package C sources are known.
	hasPotentialCoro := strings.Contains(string(src), "spawn ") || strings.Contains(string(src), "await ")

	prelimTotal := 3 + len(fileCSources) + 1 // lex+parse+codegen + C sources + link
	if hasPotentialCoro {
		prelimTotal++ // coro split pass
	}

	cprog := &compileProgress{
		verbose:    verboseProgress,
		total:      prelimTotal,
		sourceFile: file,
	}

	// Lex
	cprog.step(file, "lex")

	l := lexer.New(string(src))

	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		die("lex error: %v", lexErr)
	}

	// Parse
	// Pre-scan for #no_parens macros from `use { } from` imports so the parser
	// can do token substitution for them before parsing begins.
	cprog.step(file, "parse")

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
	cprog.step(file, "codegen")

	cg := codegen.New(file)
	if cmd == "test" || cmd == "build-test" || cmd == "ir-test" {
		cg.SetTestMode(true)
	}

	if noWarnAsyncMain {
		cg.SetNoWarnAsyncMain(true)
	}

	if verboseHeuristics {
		cg.SetVerboseHeuristics(true)
	}

	if debugBuild {
		cg.SetDebugMode(true)
	}

	// On Apple arm64, long double == double and compiler-rt has no fp128 routines.
	if targetGOOS == "darwin" && targetGOARCH == "arm64" {
		cg.SetUseDoubleForF128(true)
	}

	if explicitTarget {
		if triple := clangTripleForTarget(); triple != "" {
			cg.SetTargetTriple(triple)
		}
	}

	if stdlibOverride != "" {
		cg.SetStdlibOverride(stdlibOverride)
	}

	for _, r := range extraLibsRoots {
		cg.AddLibsRoot(r)
	}

	if verboseProgress {
		cg.SetProgressFunc(func(msg string) {
			cprog.detail(msg)
		})
	}

	if verboseTCO {
		cg.SetTCOReportFunc(func(caller, callee string) {
			if callee == "" {
				fmt.Fprintf(os.Stderr, "tco: %s (self)\n", caller)
			} else {
				fmt.Fprintf(os.Stderr, "tco: %s -> %s (mutual)\n", caller, callee)
			}
		})
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

	// Refine progress total now that package C sources are known and we can
	// check whether a coroutine split pass is needed.
	{
		hasCoro := strings.Contains(irText, "llvm.coro.")
		actualTotal := 3 + len(fileCSources) + 1

		if hasCoro {
			actualTotal++
		}

		cprog.setTotal(actualTotal)
	}

	// Collect linker flags: //! directives in the file + codegen link directives
	srcLinkFlags := append([]string{}, fileLinkerFlags...)
	for _, lib := range cg.LinkLibs() {
		srcLinkFlags = append(srcLinkFlags, "-l"+lib)
	}

	switch cmd {
	case "ir", "ir-test":
		cprog.clear()
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

		if err := compileIR(irText, out, libMode, extraObjs, fileCSources, extraCFlags, cprog, debugBuild); err != nil {
			die("compile error: %v", err)
		}

		cprog.clear()

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

		if err := compileIR(irText, out, false, extraObjs, fileCSources, extraCFlags, cprog); err != nil {
			die("compile error: %v", err)
		}

		cprog.clear()

	case "run", "test":
		tmpRel := strings.TrimSuffix(file, filepath.Ext(file)) + ".tin.out"
		tmp, _ := filepath.Abs(tmpRel)
		// Collect extra link inputs, memory-checker flag, and binary args (after --).
		var extraObjs []string

		var binArgs []string

		memcheck := ""

		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if a == "--" {
				binArgs = append(binArgs, os.Args[i+1:]...)

				break
			} else if a == "-v-valgrind" {
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

		if err := compileIR(irText, tmp, false, extraObjs, fileCSources, extraCFlags, cprog, debugBuild); err != nil {
			die("compile error: %v", err)
		}

		cprog.clear()

		defer func(name string) {
			_ = os.Remove(name)
		}(tmp)

		validateMemcheck(memcheck)

		run := memcheckCmd(memcheck, tmp, binArgs...)
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

// isAppleSilicon reports whether the current machine is Apple Silicon.
// This covers both macOS arm64 and Linux arm64 running on Apple hardware
// (e.g. Asahi Linux), but excludes generic arm64 (Graviton, RPi, etc.).
func isAppleSilicon() bool {
	if runtime.GOARCH != "arm64" {
		return false
	}

	if runtime.GOOS == "darwin" {
		return true
	}

	// On Linux, Apple CPUs report implementer code 0x61 in /proc/cpuinfo.
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}

	return strings.Contains(string(data), "CPU implementer\t: 0x61")
}

// fixCoroAttrs rewrites the LLVM IR string emitted by the llir library to
// produce valid IR for the installed clang version:
//
//  1. "presplitcoroutine" string attr -> keyword attr (required by coro-split).
//  2. On Apple Silicon (macOS arm64 and Asahi Linux), llvm.coro.end requires
//     i1 return type and ptr argument; patch the declaration and call sites.
func fixCoroAttrs(ir string) string {
	ir = strings.ReplaceAll(ir, `"presplitcoroutine"`, "presplitcoroutine")

	// Apple Silicon rejects llvm.coro.end declared as void/i8*: the intrinsic's
	// canonical signature there is i1(ptr, i1, token).  Patch the declaration
	// and call sites to match.  Use a named result (%_coroend) to avoid
	// shifting implicit SSA slot numbering.
	if isAppleSilicon() {
		ir = strings.ReplaceAll(ir, "declare void @llvm.coro.end(i8*", "declare i1 @llvm.coro.end(ptr")
		ir = strings.ReplaceAll(ir, "call void @llvm.coro.end(i8*", "%_coroend = call i1 @llvm.coro.end(ptr")
	}

	return ir
}

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// If libMode is true, compile to an object file with -c (no linking).
// extraObjs are additional .o/.a files and -l/-L flags to pass to the linker.
// cSources are C source files to compile in alongside the IR.
// prog is the optional progress tracker (nil = silent).
// debugMode switches the final compile from -O2 to -O0 and adds -g.
func compileIR(ir, outBin string, libMode bool, extraObjs []string, cSources []cSource, extraCFlags []string, prog *compileProgress, debugMode ...bool) error {
	isDebug := len(debugMode) > 0 && debugMode[0]
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

		sourceFile := outBin
		if prog != nil {
			sourceFile = prog.sourceFile
		}

		if prog != nil {
			prog.step(sourceFile, "coro split")
		}

		splitArgs := append([]string{"-O1", "-S", "-emit-llvm"}, clangTargetFlag()...)
		splitArgs = append(splitArgs, llInputFile, "-o", splitName)
		split := exec.Command("clang", splitArgs...)
		split.Stdout = os.Stdout

		split.Stderr = os.Stderr

		if err := split.Run(); err != nil {
			return fmt.Errorf("coro split pass failed: %w", err)
		}

		// LLVM 22's CoroSplitPass emits !DILabel nodes without the required
		// 'line' field when debug info is active. Patch them before the next
		// compile step to avoid "missing required field 'line'" errors.
		if isDebug {
			if data, readErr := os.ReadFile(splitName); readErr == nil {
				if patched := patchMissingDILabelLine(string(data)); patched != string(data) {
					_ = os.WriteFile(splitName, []byte(patched), 0644)
				}
			}
		}

		llInputFile = splitName
	}

	optLevel := "-O2"
	if isDebug {
		optLevel = "-O0"
	}

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

		irArgs := append([]string{optLevel, "-c"}, clangTargetFlag()...)
		irArgs = append(irArgs, llInputFile, "-o", irObjName)
		clangIR := exec.Command("clang", irArgs...)
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
			cArgs := append([]string{"-O2", "-c"}, clangTargetFlag()...)
			cArgs = append(cArgs, cs.flags...)
			cArgs = append(cArgs, cs.path, "-o", cObjName)

			if prog != nil {
				prog.step(cs.path, "compile")
			}

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
		if prog != nil {
			prog.step(outBin, "link")
		}

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
		cArgs := append([]string{"-O2", "-c"}, clangTargetFlag()...)
		cArgs = append(cArgs, compileFlags...)
		cArgs = append(cArgs, cs.path, "-o", cObjName)

		if prog != nil {
			prog.step(cs.path, "compile")
		}

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

	args := []string{optLevel}
	args = append(args, clangTargetFlag()...)

	if isDebug {
		args = append(args, "-g")

		// On macOS, clang -g emits a debug map that references temp .o files by
		// path. Those files are deleted before dsymutil can run, so LLDB sees no
		// debug info. -fstandalone-debug embeds full DWARF directly in the binary,
		// no debug map needed.
		if runtime.GOOS == "darwin" {
			args = append(args, "-fstandalone-debug")
		}
	}

	args = append(args, llInputFile)
	if _, err := os.Stat(rtC); err == nil {
		args = append(args, rtC)
	}

	args = append(args, cObjPaths...)
	args = append(args, cLinkerFlags...)
	args = append(args, extraObjs...)
	args = append(args, extraCFlags...)
	args = append(args, "-o", outBin)

	if prog != nil {
		prog.step(outBin, "link")
	}

	clang := exec.Command("clang", args...)
	clang.Stdout = os.Stdout
	clang.Stderr = os.Stderr

	if err := clang.Run(); err != nil {
		return err
	}

	return nil
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
// binArgs are forwarded to the binary as its argv[1..].
func memcheckCmd(memcheck, binary string, binArgs ...string) *exec.Cmd {
	switch memcheck {
	case "valgrind":
		args := append([]string{
			"--error-exitcode=1",
			"--leak-check=full",
			"--errors-for-leak-kinds=all",
			binary,
		}, binArgs...)

		return exec.Command("valgrind", args...)
	case "leaks":
		args := append([]string{"--atExit", "--", binary}, binArgs...)

		return exec.Command("leaks", args...)
	default:
		return exec.Command(binary, binArgs...)
	}
}

// runDirTestsRecursive collects all .tin files under root and runs them
// together as a single test batch with one combined summary.
func runDirTestsRecursive(root string, extraFlags []string, extraCFlags []string, memcheck string) {
	files := collectTinFiles(root)
	runFileTests(files, extraFlags, extraCFlags, memcheck)
}

// fileHasTestBlocks does a fast byte-level scan for 'test "' so we can skip
// files with no test blocks before attempting to lex/parse them.
func fileHasTestBlocks(src []byte) bool {
	needle := []byte(`test "`)
	for i := 0; i+len(needle) <= len(src); i++ {
		match := true

		for j, b := range needle {
			if src[i+j] != b {
				match = false

				break
			}
		}

		if match {
			return true
		}
	}

	return false
}

// runDirTests runs all .tin files in dir that contain test blocks.
// It prints a per-file header and aggregate summary, then exits non-zero
// if any file has failing tests.
func runDirTests(dir string, extraFlags []string, extraCFlags []string, memcheck string) {
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

	runFileTests(files, extraFlags, extraCFlags, memcheck)
}

// runFileTests runs the given .tin files that contain test blocks.
// It prints a per-file header and aggregate summary, then exits non-zero
// if any file has failing tests.  memcheck is "", "valgrind", or "leaks".
// reTestFailed matches lines like "test: my test name ... FAILED"
var reTestFailed = regexp.MustCompile(`^test: (.+) \.\.\. FAILED`)

func runFileTests(fpaths []string, extraFlags []string, extraCFlags []string, memcheck string) {
	type result struct {
		file        string
		passed      bool
		skipped     bool
		reason      string
		failedTests []string // individual test names that failed (empty = whole-file failure)
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

		// Fast pre-check: skip files with no test blocks at all.
		if !fileHasTestBlocks(src) {
			continue
		}

		cprog := &compileProgress{verbose: verboseProgress, total: 4, sourceFile: fpath}

		cprog.step(fpath, "lex")

		l := lexer.New(string(src))

		tokens, lexErr := l.Tokenize()
		if lexErr != nil {
			cprog.clear()

			_, _ = fmt.Fprintf(os.Stderr, "skip %s: lex error: %v\n", fname, lexErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("lex error: %v", lexErr), nil})

			continue
		}

		cprog.step(fpath, "parse")

		p := parser.New(tokens)
		for name, expansion := range codegen.ScanImportedNoParensMacros(fpath, tokens, "", nil) {
			p.RegisterNoParensMacro(name, expansion)
		}

		prog, parseErr := p.Parse()
		if parseErr != nil {
			cprog.clear()

			_, _ = fmt.Fprintf(os.Stderr, "skip %s: parse error: %v\n", fname, parseErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("parse error: %v", parseErr), nil})

			continue
		}

		cprog.step(fpath, "codegen")

		cg := codegen.New(fpath)
		cg.SetTestMode(true)

		if verboseHeuristics {
			cg.SetVerboseHeuristics(true)
		}

		if verboseProgress {
			cg.SetProgressFunc(func(msg string) { cprog.detail(msg) })
		}

		if verboseTCO {
			cg.SetTCOReportFunc(func(caller, callee string) {
				if callee == "" {
					_, _ = fmt.Fprintf(os.Stderr, "tco: %s (self)\n", caller)
				} else {
					_, _ = fmt.Fprintf(os.Stderr, "tco: %s -> %s (mutual)\n", caller, callee)
				}
			})
		}

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
			cprog.clear()

			_, _ = fmt.Fprintf(os.Stderr, "skip %s: codegen error: %v\n", fname, cgErr)
			results = append(results, result{fname, false, true, fmt.Sprintf("codegen error: %v", cgErr), nil})

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

		cprog.setTotal(3 + len(fCSources) + 1)

		tmp, tmpErr := os.CreateTemp("", "tin-test-*.out")
		if tmpErr != nil {
			cprog.clear()
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  error: %v\n", tmpErr)

			results = append(results, result{fname, false, false, fmt.Sprintf("error: %v", tmpErr), nil})

			continue
		}

		_ = tmp.Close()
		//goland:noinspection GoDeferInLoop
		defer func(name string) {
			_ = os.Remove(name)
		}(tmp.Name())

		irText := fixCoroAttrs(mod.String())
		if compErr := compileIR(irText, tmp.Name(), false, linkFlags, fCSources, extraCFlags, cprog); compErr != nil {
			cprog.clear()
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  compile error: %v\n", compErr)

			results = append(results, result{fname, false, false, fmt.Sprintf("compile error: %v", compErr), nil})

			continue
		}

		cprog.clear()
		fmt.Printf("%s\n\n", fname)

		run := memcheckCmd(memcheck, tmp.Name())

		var outBuf bytes.Buffer

		run.Stdout = io.MultiWriter(os.Stdout, &outBuf)
		run.Stderr = os.Stderr

		passed := true
		if runErr := run.Run(); runErr != nil {
			passed = false
		}

		fmt.Println("------------------------------------------------")

		var failedTests []string

		reason := ""

		if !passed {
			for _, line := range strings.Split(outBuf.String(), "\n") {
				if m := reTestFailed.FindStringSubmatch(line); m != nil {
					failedTests = append(failedTests, m[1])
				}
			}

			if len(failedTests) > 0 {
				reason = "test failures"
			} else {
				reason = "process error"
			}
		}

		results = append(results, result{fname, passed, false, reason, failedTests})
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
				if len(r.failedTests) > 0 {
					fmt.Printf("  %s:\n", r.file)

					for _, t := range r.failedTests {
						fmt.Printf("    - %s\n", t)
					}
				} else {
					fmt.Printf("  %s: %s\n", r.file, r.reason)
				}
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

// patchMissingDILabelLine adds 'line: 0' to any !DILabel(...) metadata node
// in the LLVM IR text that is missing the required 'line' field. LLVM 22's
// CoroSplitPass emits DILabel nodes for coro resume labels without line
// information, which causes subsequent clang invocations to fail.
func patchMissingDILabelLine(ir string) string {
	lines := strings.Split(ir, "\n")
	for i, line := range lines {
		if strings.Contains(line, "!DILabel(") && !strings.Contains(line, "line:") {
			// Insert ", line: 0" before the closing ")" of the DILabel node.
			idx := strings.LastIndex(line, ")")
			if idx >= 0 {
				lines[i] = line[:idx] + ", line: 0" + line[idx:]
			}
		}
	}

	return strings.Join(lines, "\n")
}

func die(format string, args ...any) {
	// Clear any in-progress progress line so the error message starts cleanly.
	_, _ = fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", progressLineWidth))
	_, _ = fmt.Fprintf(os.Stderr, "tin: "+format+"\n", args...)

	os.Exit(1)
}
