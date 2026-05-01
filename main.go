package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
	"github.com/Azer0s/tin/repl"
)

const usage = `tin - the tin language compiler

Subcommands:
  tin run         <file.tin>                  compile and execute
  tin build       <file.tin> [-o out]         compile to native binary
  tin build       --lib <file.tin> [-o out]   compile to object file (library)
  tin test        <file.tin|dir|dir/...>      run test blocks and report results
  tin build-test  <file.tin> [-o out]         compile test binary without running
  tin ir          <file.tin> [-o out]         emit LLVM IR (default: stdout)
  tin ir-test     <file.tin> [-o out]         emit test-mode LLVM IR
  tin preprocess  <file.tin>                  expand macros and print source to stdout
  tin repl        [--stdlib PATH] [file.tin]  interactive REPL (preloads file)
  tin clean                                   delete the local .build/ cache

Output:
  -o PATH                  write binary / object file to PATH
  -g                       emit debug info (-O0 + -g)
  --emit-header=PATH       emit a C header for #interop functions

Source / library:
  --stdlib PATH            override the stdlib path (default: <execDir>/stdlib)
  --lib-root PATH          add a package root, repeatable (before default <execDir>/libs)
  --cflag FLAG             pass FLAG to clang, repeatable (e.g. --cflag -fsanitize=address)
  -lNAME / -LDIR           link with libNAME / add DIR to lib search path
  file.o / file.a          link with extra object or archive file

Target:
  -target os/arch          cross-compile (linux/{amd64,arm64,386}, darwin/{amd64,arm64})
  --macos-sdk PATH         macOS SDK root (auto-detected from xcrun /
                           ~/.darling / Xcode CLT; required for -target darwin
                           when the host is not macOS)
  --linux-sysroot PATH     Linux rootfs (with usr/include + usr/lib for the
                           target arch); required for -target linux when the
                           host is not Linux

Run / test:
  --valgrind               run binary under valgrind --leak-check=full
  --leaks                  run binary under leaks --atExit (macOS only)
  -j N                     parallel TUs for clang compile (default GOMAXPROCS)
  -O0|-O1|-O2|-O3|-Os|-Oz  override clang optimization level (default -O2; -g implies -O0)
  --fast                   shortcut for -O0 - useful for tin test when the suite is bottlenecked
                           on optimization passes. Explicit -O<n> takes precedence.
  --no-pure-fold           disable compile-time evaluation of #pure calls; emit them as runtime
                           invocations. Same as -fno-pure-fold. Useful when a faulty #pure body
                           hangs the evaluator or when comparing folded vs unfolded codegen.
  --pure-fold-budget=N     cap node visits per top-level #pure call (default 1_000_000). On
                           exhaustion the call falls back to runtime emission. 0 = use default.

Warnings (all warnings carry a name; -Werror=<name> escalates one):
  -Wall                    enable hygiene checks: unused-let, unused-result
  -Wpedantic               enable -Wall plus unused-param
  -W<name>                 enable a default-off warning (e.g. -Wunused-let)
  -Wno-<name>              silence a warning entirely
  -Werror                  treat every warning as an error
  -Werror=<name>           treat the named warning as an error

  Default-on:
    array-bounds                index out of bounds for known-length array
    async-main                  main() uses spawn/await but is not #async
    await-match-guards          guard clauses in await-match arms
    bool-analysis               condition that folds to true/false at compile time
    deref-nil                   dereference of literal nil
    div-by-zero / shift-overflow  arithmetic that's UB at runtime
    self-assign                 x = x
    tautological-pointer-cmp    comparing a non-nil pointer against nil
    unreachable-code            statements after return / panic / infinite loop
    unused-match-arms           unreachable match case / where clause

  Default-off (opt in via -W<name>, -Wall, or -Wpedantic):
    unused-let                  let-binding that is never read
    unused-result               discarded result of a non-void call
    unused-param                fn parameter that is never read
    builtin-shadow              local binding masks a compile-time builtin (typeof, sourcepos, ...)

Diagnostic dumps (debug aids; output to stderr):
  -v                       print compilation stages (lex, parse, codegen, link, ...)
  -fdump-match-info        Maranget pattern matrix and per-arm reachability
  -fdump-demorgan          each De Morgan / boolean simplification rewrite
  -fdump-heuristics        auto-yield heuristics for every function
  -fdump-tco               tail-call optimizations (self-TCO and mutual TCO)
  -fdebug-fiber-slots      fiber struct pool ramp/decay events

In-source directives (at the top of the .tin file):
  //!-lNAME                    link with libNAME
  //!+file.c                   compile C source file alongside the tin module
  //!+file.c -- FLAGS          compile C source with extra clang flags
  //!+file.c [arch]            compile only on matching arch
  //!+file.c [arch] -- FLAGS   arch-specific file with extra flags
  //!-lNAME [arch]             arch-specific linker flag

  Arch tokens: x86_64, aarch64, 386, darwin, linux  (comma = AND, e.g. [aarch64,darwin])
  Variables: $TIN_RUNTIME expands to <execDir>/runtime, $TIN_STDLIB to <execDir>/stdlib
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

// macosSDKOverride is the explicit `--macos-sdk PATH` value. Empty
// means "auto-detect" - see macosSDKPath() for the resolution chain.
// Required when cross-compiling Linux -> Darwin so clang can find the
// Darwin SDK headers (malloc/malloc.h, libunwind.h, system frameworks).
var macosSDKOverride string

// linuxSysrootOverride is the explicit `--linux-sysroot PATH` value.
// Required when cross-compiling Darwin -> Linux so clang can find
// glibc/musl headers, ld-linux*, and libdw / libunwind for stacktrace.
// Auto-detection picks well-known paths (Homebrew x86_64-linux-gnu /
// Docker rootfs mounts) but on macOS hosts the user almost always has
// to provide one.
var linuxSysrootOverride string

// verbose flags are package-level so directory-mode test runners can use them.
var (
	verboseProgress   bool
	verboseHeuristics bool
	verboseTCO        bool
)

// jobs controls per-fn parallel compilation in the pure-fn .so cache pipeline.
// 0 means "use runtime.GOMAXPROCS(0)"; -j 1 forces serial execution.
var jobs int

// optLevelOverride is the -O flag value (0/1/2/3/s) supplied on the command
// line, or "" when the user did not pass -O. When non-empty it overrides the
// default optLevel chosen by compileIR.
var optLevelOverride string

// testFastCompile is reserved for an opt-in "fast tests" mode that defaults
// `tin test` to -O0 (~10x suite speedup). Currently off because the win was
// largely subsumed by the internal-linkage DCE change - clang at -O2 now
// drops dead stdlib early in compile rather than carrying it through every
// optimizer pass. Users wanting -O0 can pass it explicitly.
var testFastCompile bool

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

// clangTargetFlag returns the cross-compile flags for an explicit
// -target invocation: `-target <triple>` plus, on darwin, an
// `-isysroot <macOS SDK path>` so clang can find the Darwin headers
// (malloc/malloc.h, libunwind.h, etc). Returns nil when no
// cross-compile is requested (host triple is used implicitly).
//
// The SDK path is taken from -macos-sdk if given, then $TIN_MACOS_SDK,
// then well-known locations (Xcode CommandLineTools, Darling install).
// If none is found we fall through to bare `-target` and let clang
// produce a clear "header not found" error rather than guessing.
func clangTargetFlag() []string {
	if !explicitTarget {
		return nil
	}

	t := clangTripleForTarget()
	if t == "" {
		return nil
	}

	flags := []string{"-target", t}

	switch targetGOOS {
	case "darwin":
		// Cross-compiling to Darwin from any host: clang needs the
		// macOS SDK so it can find malloc/malloc.h, libunwind.h, and
		// the Mach-O system frameworks. Native Apple clang on macOS
		// finds it automatically via xcrun, so this is mostly a
		// concern when host != Darwin.
		if sdk := macosSDKPath(); sdk != "" {
			flags = append(flags, "-isysroot", sdk)
		}
	case "linux":
		// Cross-compiling to Linux from a non-Linux host (typically
		// macOS) needs a sysroot with glibc/musl headers + the
		// dynamic linker. Skip when host is already Linux - clang
		// already knows where /usr/include lives.
		if runtime.GOOS != "linux" {
			if sysroot := linuxSysrootPath(); sysroot != "" {
				flags = append(flags, "--sysroot", sysroot)
			}
		}
	}

	return flags
}

// hostClangTargetFlag returns the cross-compile flags for a HOST-arch
// build, deliberately ignoring -target. Used by emitPureFnCache: per-fn
// .so files are dlopen'd by the running tin process during CTFE, so they
// must match the host's ABI even when the user asked for a Darwin
// cross-compile. Returns nil so clang uses its built-in default triple
// (which IS the host triple).
func hostClangTargetFlag() []string { return nil }

// macosSDKPath returns the path to a macOS SDK (the directory whose
// usr/include holds Darwin headers). Resolved in this order:
//  1. -macos-sdk CLI flag
//  2. $TIN_MACOS_SDK env var
//  3. xcrun --show-sdk-path on Darwin hosts (or via Darling on Linux)
//  4. Well-known locations: Xcode CommandLineTools, Darling install
//
// Returns "" when no SDK is found; clang will then error with a clear
// "header not found" message rather than producing weird link failures.
func macosSDKPath() string {
	if macosSDKOverride != "" {
		return macosSDKOverride
	}

	if env := os.Getenv("TIN_MACOS_SDK"); env != "" {
		return env
	}

	if out, err := exec.Command("xcrun", "--show-sdk-path").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if _, statErr := os.Stat(p); statErr == nil {
				return p
			}
		}
	}

	candidates := []string{
		"/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk",
		"/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk",
	}
	if home, err := os.UserHomeDir(); err == nil {
		// Darling mounts the macOS / at $HOME/.darling/. The SDK shows
		// up at the same path inside that prefix when CommandLineTools
		// is installed via `darling shell -- xcode-select --install`.
		candidates = append(candidates,
			filepath.Join(home, ".darling/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk"))
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	return ""
}

// linuxSysrootPath returns the path to a Linux rootfs (the directory
// whose usr/include holds glibc/musl headers and whose usr/lib has the
// target-arch dynamic linker). Resolution order:
//  1. --linux-sysroot CLI flag
//  2. $TIN_LINUX_SYSROOT env var
//  3. Well-known Homebrew cross-toolchain paths (x86_64-linux-gnu)
//  4. /opt/cross/<arch>-linux-gnu (osxcross-style)
//
// Returns "" when no sysroot is found; clang will then error with
// "stdio.h: not found" or similar, which is clear enough.
func linuxSysrootPath() string {
	if linuxSysrootOverride != "" {
		return linuxSysrootOverride
	}

	if env := os.Getenv("TIN_LINUX_SYSROOT"); env != "" {
		return env
	}

	arch := targetGOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}

	candidates := []string{
		"/opt/cross/" + arch + "-linux-gnu",
		"/usr/local/" + arch + "-linux-gnu",
		"/opt/homebrew/" + arch + "-linux-gnu",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	return ""
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

	if len(os.Args) >= 2 && os.Args[1] == "repl" {
		runtimeDir := tinRuntimeDir()

		var (
			stdlibOverride string
			libsRoots      []string
			preloadFile    string
		)

		for i := 2; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--stdlib":
				if i+1 < len(os.Args) {
					i++
					stdlibOverride = os.Args[i]
				}
			case "--lib-root":
				if i+1 < len(os.Args) {
					i++
					libsRoots = append(libsRoots, os.Args[i])
				}
			default:
				// First non-flag positional arg is a .tin file to preload:
				// its declarations are loaded into the session and any top-
				// level statements (plus `main()` if defined) execute before
				// the interactive prompt appears.
				if preloadFile == "" && !strings.HasPrefix(os.Args[i], "-") {
					preloadFile = os.Args[i]
				}
			}
		}

		repl.Run(runtimeDir, stdlibOverride, libsRoots, preloadFile)

		return
	}

	if len(os.Args) >= 2 && os.Args[1] == "clean" {
		runClean()

		return
	}

	if len(os.Args) < 3 {
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(1)
	}

	cmd := os.Args[1]

	// Parse flags: --lib means compile to object file, not a binary.
	// Scan forward from position 2 to find the first non-flag argument (the source file).
	// Known pre-file flags: --lib (build only), -g.  Two-word flags (--stdlib PATH) are skipped.
	libMode := false
	fileArgIdx := 2

	if cmd == "build" && len(os.Args) > 2 && os.Args[2] == "--lib" {
		libMode = true
		fileArgIdx = 3
	}

	// Skip any flags that appear before the file argument.
	for fileArgIdx < len(os.Args) {
		a := os.Args[fileArgIdx]
		switch a {
		case "-g", "--fast", "--no-pure-fold", "-fno-pure-fold":
			fileArgIdx++
		case "--stdlib", "--lib-root", "-target", "-j":
			fileArgIdx += 2
		default:
			// -O0..-O3, -Os, -Oz are single-token flags.
			if a == "-O0" || a == "-O1" || a == "-O2" || a == "-O3" || a == "-Os" || a == "-Oz" {
				fileArgIdx++

				continue
			}

			// `--pure-fold-budget=N` is a single-token "key=value" flag.
			if strings.HasPrefix(a, "--pure-fold-budget=") {
				fileArgIdx++

				continue
			}

			goto doneFlags
		}
	}

doneFlags:

	if fileArgIdx >= len(os.Args) {
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(1)
	}

	file := os.Args[fileArgIdx]

	// Collect --cflag values and warning-suppression flags from anywhere after the file arg.
	var extraCFlags []string

	var stdlibOverride string

	var extraLibsRoots []string

	noWarnAwaitMatchGuards := false
	verboseMatchInfo := false
	verboseDemorgan := false
	debugBuild := false
	emitHeaderPath := ""
	allWarnsAsErrors := false
	wAll := false
	wPedantic := false
	noPureFold := false
	pureFoldBudget := 0 // 0 = use codegen default

	var (
		warnSuppress []string // -Wno-<name> targets
		warnAsErrors []string // -Werror=<name> targets
		warnEnable   []string // -W<name> opt-ins for default-off diags
	)

	// Scan all args (including those before the file) for flags.
	for i := 2; i < len(os.Args); i++ {
		a := os.Args[i]
		switch a {
		case "--cflag":
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
		case "-Werror":
			allWarnsAsErrors = true
		case "-Wall":
			wAll = true
		case "-Wpedantic":
			wPedantic = true
		case "-fdump-match-info":
			verboseMatchInfo = true
		case "-fdump-demorgan":
			verboseDemorgan = true
		case "-fdump-heuristics":
			verboseHeuristics = true
		case "-v":
			verboseProgress = true
		case "-fdump-tco":
			verboseTCO = true
		case "-fdebug-fiber-slots":
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
		case "--macos-sdk":
			if i+1 < len(os.Args) {
				i++
				macosSDKOverride = os.Args[i]
			}
		case "--linux-sysroot":
			if i+1 < len(os.Args) {
				i++
				linuxSysrootOverride = os.Args[i]
			}
		case "-j":
			if i+1 < len(os.Args) {
				i++

				n, err := strconv.Atoi(os.Args[i])
				if err != nil || n < 1 {
					die("-j: expected positive integer (got %q)", os.Args[i])
				}

				jobs = n
			}
		case "-O0", "-O1", "-O2", "-O3", "-Os", "-Oz":
			optLevelOverride = a
		case "--no-pure-fold", "-fno-pure-fold":
			noPureFold = true
		case "--fast":
			// Shortcut for `tin test`: drop the optimization level so the
			// LLVM passes that dominate compile time on rtti-heavy /
			// fiber-heavy IR don't run. Verified ~10x suite speedup at the
			// cost of slower test-runtime execution. Explicit -O<n> wins.
			if optLevelOverride == "" {
				optLevelOverride = "-O0"
			}
		default:
			switch {
			case strings.HasPrefix(a, "-Wno-"):
				name := strings.TrimPrefix(a, "-Wno-")
				warnSuppress = append(warnSuppress, name)

				if name == "await-match-guards" {
					noWarnAwaitMatchGuards = true
				}
			case strings.HasPrefix(a, "-Werror="):
				warnAsErrors = append(warnAsErrors, strings.TrimPrefix(a, "-Werror="))
			case strings.HasPrefix(a, "-W") && len(a) > 2:
				// `-W<name>` opts in to a default-off warning.
				warnEnable = append(warnEnable, strings.TrimPrefix(a, "-W"))
			case strings.HasPrefix(a, "--emit-header="):
				emitHeaderPath = strings.TrimPrefix(a, "--emit-header=")
			case strings.HasPrefix(a, "--pure-fold-budget="):
				raw := strings.TrimPrefix(a, "--pure-fold-budget=")

				n, perr := strconv.Atoi(raw)
				if perr != nil || n < 0 {
					die("--pure-fold-budget: expected non-negative integer (got %q)", raw)
				}

				pureFoldBudget = n
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
				if a == "--valgrind" {
					memcheck = "valgrind"
				} else if a == "--leaks" {
					memcheck = "leaks"
				} else if a == "-fdebug-fiber-slots" {
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

	// `tin run` and single-file `tin test` cache the compiled binary at
	// .build/<mode>/<dunder>_<md5>/bin (relative to CWD). On a cache hit -
	// the source MD5 names an existing dir AND every file recorded in its
	// sbom.txt still hashes the same - we skip lex/parse/codegen entirely
	// and exec the cached binary.
	var (
		runCacheDir     string
		runCacheBinPath string
	)

	if cmd == "run" || cmd == "test" {
		runCacheDir = cacheBinDir(cmd, file, src)
		runCacheBinPath = filepath.Join(runCacheDir, "bin")

		if _, statErr := os.Stat(runCacheBinPath); statErr == nil && sbomMatches(runCacheDir) {
			memcheck, binArgs := parseRunArgs(fileArgIdx)
			validateMemcheck(memcheck)
			execRunBinary(runCacheBinPath, memcheck, binArgs)

			return
		}
	}

	// Collect directives declared in the source file via //! lines
	fileLinkerFlags, fileCSources := parseFileDirectives(string(src), filepath.Dir(file), stdlibDirForDirectives(stdlibOverride))

	// Estimate total stages for progress display. Mirrors the actual
	// step shape so the post-codegen setTotal call refines without
	// jumping. Default (binary) mode: lex+parse+codegen + batched
	// compile + link = 5. --lib mode: lex+parse+codegen + per-TU
	// compile + ld -r merge.
	//
	// Coro detection here is conservative: any explicit spawn/await OR
	// any `use` declaration triggers it, because imported stdlib packages
	// (sync, ioutil, http, ...) very commonly emit coroutine IR even
	// when the user's own code never says spawn. Without this, every
	// `use std` program would see /5 jump to /6 mid-progress.
	hasPotentialCoro := strings.Contains(string(src), "spawn ") ||
		strings.Contains(string(src), "await ") ||
		strings.Contains(string(src), "\nuse ") ||
		strings.HasPrefix(string(src), "use ")

	var prelimTotal int
	if libMode {
		prelimTotal = 3 + len(fileCSources) + 1
	} else {
		prelimTotal = 3 + 1 + 1
	}

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

	if noPureFold {
		cg.SetPureFoldDisabled(true)
	}

	if pureFoldBudget > 0 {
		cg.SetPureFoldBudget(pureFoldBudget)
	}

	if wPedantic {
		cg.SetWPedantic()
	} else if wAll {
		cg.SetWAll()
	}

	for _, name := range warnEnable {
		cg.SetWarnEnable(name)
	}

	for _, name := range warnSuppress {
		cg.SetWarnSuppress(name)
	}

	for _, name := range warnAsErrors {
		cg.SetWarnAsError(name)
	}

	if allWarnsAsErrors {
		cg.SetAllWarnsAsErrors()
	}

	if verboseMatchInfo {
		cg.SetVerboseMatchInfo(true)
	}

	if verboseDemorgan {
		cg.SetVerboseDemorgan(true)
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

	if emitHeaderPath != "" {
		cg.SetEmitHeaderPath(emitHeaderPath)
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

	if cg.HadWarnError() {
		die("warnings treated as errors")
	}

	// Latch the stacktrace flag for the upcoming compileIR call. Phase 6
	// of docs/plans/stacktrace-libunwind.md gates `-lunwind` / `-rdynamic`
	// / `-DTIN_STACKTRACE` on this; programs that never reference
	// stacktrace() get the unmodified clang argv (and a smaller binary).
	stacktraceLinkActive = cg.StacktraceUsed()

	irText := fixCoroAttrs(mod.String())

	// Per-pkg IRs: each imported package compiled into its own .ll/.o
	// in parallel. finalizePerPkgModules ran inside cg.Generate to add
	// cross-module declares + shared TypeDefs, so each pkg's .ll is a
	// self-contained LLVM IR module.
	pkgIRTexts := collectPkgIRs(cg)

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
	// check whether a coroutine split pass is needed. The shape of the
	// remaining pipeline differs between modes:
	//
	//   - default (binary):  one batched "compile (N TUs)" step, one link
	//   - --lib (object):    one step per C source, one ld -r merge
	//
	// The earlier prelimTotal estimate could only see file-level //!+ C
	// sources (zero for most programs); after codegen we also have stdlib
	// runtime C sources, so the count would shoot up. Mirror the shape of
	// the actual step calls so the denominator stops jumping mid-progress.
	{
		hasCoro := strings.Contains(irText, "llvm.coro.")

		var actualTotal int
		if libMode {
			actualTotal = 3 + len(fileCSources) + 1 // lex+parse+codegen + per-TU + ld -r
		} else {
			actualTotal = 3 + 1 + 1 // lex+parse+codegen + batched compile + link
		}

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
			} else if a == "--cflag" {
				i++ // value already collected above
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}

		extraObjs = append(srcLinkFlags, extraObjs...)

		if err := compileIRWithPkgs(irText, pkgIRTexts, out, libMode, extraObjs, fileCSources, extraCFlags, cprog, debugBuild); err != nil {
			die("compile error: %v", err)
		}

		// Phase C2/C4: opt-in per-fn .so cache. Drives nothing in the user
		// binary itself (the wrapper symbols stay internal-linkage and clang
		// DCEs them); the cache is for tier-2 CTFE dispatch on rebuilds.
		if pureFnCacheEnabled() {
			if err := emitPureFnCache(cg, cprog); err != nil {
				die("pure-fn cache: %v", err)
			}
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
			} else if a == "--cflag" {
				i++ // value already collected above
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}

		extraObjs = append(srcLinkFlags, extraObjs...)

		if err := compileIRWithPkgs(irText, pkgIRTexts, out, false, extraObjs, fileCSources, extraCFlags, cprog); err != nil {
			die("compile error: %v", err)
		}

		cprog.clear()

	case "run", "test":
		extraObjs := collectExtraObjs(fileArgIdx)
		extraObjs = append(srcLinkFlags, extraObjs...)
		memcheck, binArgs := parseRunArgs(fileArgIdx)

		cleanStaleCacheEntries(cmd, file)

		if err := os.MkdirAll(runCacheDir, 0o755); err != nil {
			die("cache dir: %v", err)
		}

		if err := compileIRWithPkgs(irText, pkgIRTexts, runCacheBinPath, false, extraObjs, fileCSources, extraCFlags, cprog, debugBuild); err != nil {
			die("compile error: %v", err)
		}

		if err := writeBuildSBOM(runCacheDir, file, src, buildDeps(cg, fileCSources)); err != nil {
			die("sbom write: %v", err)
		}

		// Phase C2: emit per-fn .so files for #pure functions (opt-in via env)
		// so the cache is ready for the dlopen-based dispatch path that lands
		// in Phase C3. Off by default to keep default builds fast.
		if pureFnCacheEnabled() {
			if err := emitPureFnCache(cg, cprog); err != nil {
				die("pure-fn cache: %v", err)
			}
		}

		cprog.clear()

		validateMemcheck(memcheck)
		execRunBinary(runCacheBinPath, memcheck, binArgs)

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
// produce valid IR for the installed clang version.
// "presplitcoroutine" must be a keyword attribute, not a string attribute.
// llvm.coro.end changed signature at LLVM 22: <= 21 uses i1 return + ptr arg,
// >= 22 uses void return + ptr arg. llir emits the old void + i8* form; LLVM 22
// accepts that and auto-upgrades i8* to ptr. LLVM 21 expects i1, so we patch.
func fixCoroAttrs(ir string) string {
	ir = strings.ReplaceAll(ir, `"presplitcoroutine"`, "presplitcoroutine")
	if v := clangMajorVersion(); v > 0 && v <= 21 {
		ir = strings.ReplaceAll(ir,
			"declare void @llvm.coro.end(i8*",
			"declare i1 @llvm.coro.end(ptr")
		ir = strings.ReplaceAll(ir,
			"call void @llvm.coro.end(i8*",
			"%_coro_end = call i1 @llvm.coro.end(ptr")
	}

	return ir
}

// stacktraceLinkFlag is the link-time toggle that promotes user fns to
// the dynsym (Linux ELF only) and pulls in libunwind. Phase 6 of
// docs/plans/stacktrace-libunwind.md gates these on cg.StacktraceUsed();
// the global below is set in main() before each compileIR call.
//
// We can't thread a bool param through compileIR without rewriting its
// signature in five places, so a process-global suffices: the compiler
// is invoked once per build and the flag is decided after Generate()
// returns.
var stacktraceLinkActive bool

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// namedIR pairs a stable label (used in temp-file names + progress
// reporting) with the LLVM IR text for one compilation unit. Used by
// compileIR's per-pkg path: the entry IR (cg.mod) is one TU, and each
// imported package's *ir.Module is an additional TU compiled in
// parallel into its own `.o` and linked alongside.
type namedIR struct {
	label  string
	irText string
}

// collectPkgIRs serializes every per-pkg LLVM module the codegen built
// (excluding cg.mod itself, which is serialized separately) and returns
// them as namedIR entries for compileIRWithPkgs. Sanitizes pkg names
// for use in temp filenames (`::` -> `_`, etc.).
func collectPkgIRs(cg *codegen.CodeGen) []namedIR {
	mods := cg.PkgModules()
	if len(mods) == 0 {
		return nil
	}

	names := cg.PkgModuleNames()

	out := make([]namedIR, 0, len(mods))
	for i, m := range mods {
		label := strings.NewReplacer("::", "_", "/", "_", " ", "_").Replace(names[i])
		out = append(out, namedIR{
			label:  label,
			irText: fixCoroAttrs(m.String()),
		})
	}

	return out
}

// If libMode is true, compile to an object file with -c (no linking).
// extraObjs are additional .o/.a files and -l/-L flags to pass to the linker.
// cSources are C source files to compile in alongside the IR.
// prog is the optional progress tracker (nil = silent).
// debugMode switches the final compile from -O2 to -O0 and adds -g.
func compileIR(ir, outBin string, libMode bool, extraObjs []string, cSources []cSource, extraCFlags []string, prog *compileProgress, debugMode ...bool) error {
	return compileIRWithPkgs(ir, nil, outBin, libMode, extraObjs, cSources, extraCFlags, prog, debugMode...)
}

// compileIRWithPkgs is the multi-IR variant of compileIR. `pkgIRs` is
// one IR text per imported package; each is written to its own `.ll`,
// compiled to a `.o` in parallel with the entry IR + runtime.c, and
// added to the link inputs. When pkgIRs is empty this is identical to
// the legacy single-IR path.
func compileIRWithPkgs(ir string, pkgIRs []namedIR, outBin string, libMode bool, extraObjs []string, cSources []cSource, extraCFlags []string, prog *compileProgress, debugMode ...bool) error {
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

	finalOpt := chooseOptLevel(isDebug)

	// LLVM 22's -O2 optimizer breaks coroutine yield paths: the "suspended"
	// default arm of coro.suspend in the resume function is marked unreachable,
	// causing backward DCE to remove the `store index; ret void` that the
	// scheduler depends on.  Work around by splitting coroutines at -O1 first
	// (which produces correct yield paths), then running -O2 on the split IR.
	//
	// Only needed when the final compile goes through aggressive optimization
	// (-O2/-O3); at -O0/-O1 the broken pass is not exercised and we skip the
	// split, which can otherwise cost a minute or more on coroutine-heavy IR.
	needsCoroSplit := finalOpt == "-O2" || finalOpt == "-O3"
	if needsCoroSplit && strings.Contains(ir, "llvm.coro.") {
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
		// stacktraceLinkActive flips on `-gline-tables-only` for the IR
		// compile, so the patch also has to run in that mode even when the
		// user didn't pass -g explicitly.
		if isDebug || stacktraceLinkActive {
			if data, readErr := os.ReadFile(splitName); readErr == nil {
				if patched := patchMissingDILabelLine(string(data)); patched != string(data) {
					_ = os.WriteFile(splitName, []byte(patched), 0644)
				}
			}
		}

		llInputFile = splitName
	}

	optLevel := chooseOptLevel(isDebug)

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

	// Split the compile and link phases so each translation unit (IR, runtime.c,
	// any //!+file.c sources) compiles to its own .o in parallel; the final
	// clang invocation only links. This converts the dominant single-threaded
	// clang call into N parallel -c calls + one fast link, scaling with -j.
	var (
		tmpObjs      []string // every temp .o we own; cleaned up on return
		linkInputs   []string // .o files passed to the link step (in stable order)
		cLinkerFlags []string // -l/-L flags pulled out of //!+file.c directives
	)

	defer func() {
		for _, f := range tmpObjs {
			_ = os.Remove(f)
		}
	}()

	mkObj := func(prefix string) (string, error) {
		f, err := os.CreateTemp("", prefix+"-*.o")
		if err != nil {
			return "", fmt.Errorf("cannot create temp object file: %w", err)
		}

		name := f.Name()
		_ = f.Close()

		tmpObjs = append(tmpObjs, name)

		return name, nil
	}

	var jobsList []compileJob

	// IR -> ir.o
	irObjName, err := mkObj("tin-ir")
	if err != nil {
		return err
	}

	linkInputs = append(linkInputs, irObjName)
	{
		a := append([]string{optLevel, "-c", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			a = append(a, "-g")

			if targetGOOS == "darwin" {
				a = append(a, "-fstandalone-debug")
			}
		}

		// .eh_frame helps external tools (gdb, perf) walk Tin frames
		// even though Tin's own stacktrace() now uses an FP walker
		// (see runtime/stacktrace.c). Default builds keep paying the
		// 5-10% binary-size tax for unwind info nothing reads on Linux
		// x86_64, so emit the negative path explicitly when stacktrace
		// isn't reachable. (Phase 6, docs/plans/stacktrace-libunwind.md.)
		//
		// Source line resolution no longer goes through DWARF: the
		// codegen post-pass (codegen/pclntab.go) emits a custom
		// `tin_pclntab` section that runtime/pclntab.c reads directly,
		// so we don't need `-gline-tables-only` even when stacktrace
		// is reachable. -g still emits full DWARF (the explicit `-g`
		// branch above) for lldb / gdb consumers, but stacktrace itself
		// uses pclntab in every build.
		if stacktraceLinkActive {
			a = append(a, "-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			a = append(a, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		a = append(a, llInputFile, "-o", irObjName)
		jobsList = append(jobsList, compileJob{desc: filepath.Base(llInputFile), args: a})
	}

	// Per-pkg IR -> per-pkg .o. Each imported package's *ir.Module gets
	// its own .ll + clang -c invocation, run in parallel with the entry
	// IR / runtime.c jobs. The .o files are added to linkInputs so the
	// final clang link picks them up. With cross-module declares
	// (codegen/pkgmod.go addCrossModuleDeclares) and shared TypeDefs
	// (echoSharedTypeDefs), each pkg .ll is self-sufficient — its
	// cross-pkg references resolve at link time, not at compile time.
	for _, pkg := range pkgIRs {
		if dir := os.Getenv("TIN_DUMP_PKG_IR_DIR"); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, pkg.label+".ll"), []byte(pkg.irText), 0o644)
		}

		pkgLL, err := os.CreateTemp("", "tin-pkg-"+pkg.label+"-*.ll")
		if err != nil {
			return fmt.Errorf("cannot create temp pkg .ll: %w", err)
		}

		pkgLLName := pkgLL.Name()
		if _, err := pkgLL.WriteString(pkg.irText); err != nil {
			_ = pkgLL.Close()
			_ = os.Remove(pkgLLName)
			return err
		}

		_ = pkgLL.Close()

		defer func(name string) { _ = os.Remove(name) }(pkgLLName)

		pkgObj, err := mkObj("tin-pkg-" + pkg.label)
		if err != nil {
			return err
		}

		linkInputs = append(linkInputs, pkgObj)

		a := append([]string{optLevel, "-c", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			a = append(a, "-g")
			if targetGOOS == "darwin" {
				a = append(a, "-fstandalone-debug")
			}
		}

		if stacktraceLinkActive {
			a = append(a, "-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			a = append(a, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		a = append(a, pkgLLName, "-o", pkgObj)
		jobsList = append(jobsList, compileJob{desc: "pkg:" + pkg.label, args: a})
	}

	// runtime.c -> runtime.o (only if rtC exists alongside the tin binary).
	// Globally cached across every Tin compile on this platform: the runtime
	// is identical for every program, so compiling it once per content+flags
	// hash saves ~400ms per invocation when the suite of tests is rebuilt.
	if _, statErr := os.Stat(rtC); statErr == nil {
		rtArgs := append([]string{"-O2", "-c", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			rtArgs = append(rtArgs, "-g")

			if targetGOOS == "darwin" {
				rtArgs = append(rtArgs, "-fstandalone-debug")
			}
		}

		// runtime/stacktrace.c gates its FP-walker body on TIN_STACKTRACE
		// so programs that don't use stacktrace() don't incur the
		// resolver code or the pclntab section overhead. The csrc cache
		// key includes the canonical argv, so this define naturally
		// produces two distinct cached .o entries (one with the stub,
		// one with the real walk) instead of cross-contaminating a
		// single cache slot.
		//
		// Source-line resolution comes from runtime/pclntab.c (always
		// linked via the umbrella) reading the codegen-emitted
		// `tin_pclntab` section. No DWARF / libdw involved at runtime,
		// so no `-gline-tables-only` here either.
		if stacktraceLinkActive {
			// -fno-omit-frame-pointer is REQUIRED for the FP walker:
			// stacktrace.c reads rbp/x29 via inline asm and walks the
			// saved-fp chain. Without this flag clang's Linux x86_64
			// default omits the frame pointer setup, so rbp is
			// whatever the caller's general-purpose state is (often 0
			// from the kernel-cleared startup) and the walk dies on
			// the first iteration. Tin user code already gets
			// `frame-pointer="all"` via codegen.applyStacktracePostPass,
			// but the runtime C is compiled separately and needs the
			// equivalent here.
			rtArgs = append(rtArgs, "-DTIN_STACKTRACE=1",
				"-fno-omit-frame-pointer", "-mno-omit-leaf-frame-pointer",
				"-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			rtArgs = append(rtArgs, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		cachedPath, hit, err := csrcCacheLookup(rtC, rtArgs)
		if err != nil {
			return err
		}

		linkInputs = append(linkInputs, cachedPath)
		if !hit {
			tempPath := cachedPath + fmt.Sprintf(".tmp.%d", os.Getpid())

			rtArgs = append(rtArgs, rtC, "-o", tempPath)
			jobsList = append(jobsList, compileJob{
				desc: "runtime.c", args: rtArgs, renameTo: cachedPath,
			})
		}
	}

	// Each //!+file.c at -O2; -l/-L flags pulled out for the link step.
	// Cached globally (same .c content + flags == same .o, regardless of which
	// .tin program imports it).
	for _, cs := range cSources {
		var compileFlags []string

		for _, f := range cs.flags {
			if strings.HasPrefix(f, "-l") || strings.HasPrefix(f, "-L") {
				cLinkerFlags = append(cLinkerFlags, f)
			} else {
				compileFlags = append(compileFlags, f)
			}
		}

		baseArgs := append([]string{"-O2", "-c", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)
		baseArgs = append(baseArgs, compileFlags...)

		cachedPath, hit, err := csrcCacheLookup(cs.path, baseArgs)
		if err != nil {
			return err
		}

		linkInputs = append(linkInputs, cachedPath)

		if hit {
			continue
		}

		tempPath := cachedPath + fmt.Sprintf(".tmp.%d", os.Getpid())

		a := append([]string{}, baseArgs...)
		a = append(a, cs.path, "-o", tempPath)
		jobsList = append(jobsList, compileJob{
			desc: filepath.Base(cs.path), args: a, renameTo: cachedPath,
		})
	}

	if prog != nil {
		prog.step(outBin, fmt.Sprintf("compile (%d TUs)", len(jobsList)))
	}

	// Run all -c jobs in parallel. parallelJobs() honors -j; default is GOMAXPROCS.
	if err := runParallelClang(jobsList); err != nil {
		return err
	}

	// Link step: pull every compiled .o into one binary. The link itself is fast
	// because clang sees only object files and skips parsing/optimization.
	args := []string{optLevel}
	args = append(args, clangTargetFlag()...)

	if isDebug {
		args = append(args, "-g")

		if targetGOOS == "darwin" {
			args = append(args, "-fstandalone-debug")
		}
	}

	args = append(args, "-ffunction-sections", "-fdata-sections")
	// Linker dead-code-stripping flag (per-target) plus (for
	// cross-compile) the linker selection. lld is invoked via
	// `-fuse-ld=lld` and dispatches to ld64.lld for Mach-O / ld.lld
	// for ELF. The host's default linker only handles its native
	// format, so we explicitly opt into lld whenever host != target.
	if targetGOOS == "darwin" {
		args = append(args, "-Wl,-dead_strip")
	} else {
		args = append(args, "-Wl,--gc-sections")
	}

	if runtime.GOOS != targetGOOS {
		args = append(args, "-fuse-ld=lld")
	}

	args = append(args, linkInputs...)
	args = append(args, cLinkerFlags...)
	args = append(args, extraObjs...)
	args = append(args, extraCFlags...)
	// Conditional dynsym wiring. Only programs that reference
	// `stacktrace()` pay the binary-size cost of dynsym promotion;
	// default builds stay lean.
	//
	// `-rdynamic` promotes Tin user fns into the dynamic symbol table
	// so dladdr can recover symbol names for IPs that fall outside the
	// pclntab table (typically: runtime helpers, third-party C, libc).
	// pclntab itself doesn't need dynsym - it stores names directly in
	// .rodata and resolves via in-image section lookup - but the
	// dladdr fallback in resolve_frame still does, otherwise frames in
	// non-Tin code render as `??+0x<addr>` instead of `<lib>:sym+0x<off>`.
	//
	// macOS: dyld already keeps local symbols visible to dladdr until
	// `strip` removes them, so `-rdynamic` is unnecessary on the
	// Mach-O target.
	//
	// libdw / -ldw is GONE: the pclntab path in runtime/pclntab.c is
	// the sole source-line resolver in every build (release + -g).
	// -g still emits full DWARF for lldb / gdb, but stacktrace.c never
	// reads it.
	if stacktraceLinkActive && targetGOOS != "darwin" {
		args = append(args, "-rdynamic")
	}

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

// chooseOptLevel returns the clang optimization flag for this build. Order of
// precedence:
//
//  1. -O<n> on the command line wins unconditionally.
//  2. -g (debug) selects -O0 so the debugger sees source as written.
//  3. `tin test` / `tin build-test` / `tin ir-test` default to -O0 because
//     optimization buys nothing for test correctness and can take 100x longer
//     than codegen itself on large IR.
//  4. Otherwise -O2.
func chooseOptLevel(isDebug bool) string {
	if optLevelOverride != "" {
		return optLevelOverride
	}

	if isDebug {
		return "-O0"
	}

	if testFastCompile {
		return "-O0"
	}

	return "-O2"
}

// parallelJobs returns the per-process compile concurrency. Honors the -j flag
// when set; otherwise uses runtime.GOMAXPROCS(0).
func parallelJobs() int {
	if jobs > 0 {
		return jobs
	}

	return runtime.GOMAXPROCS(0)
}

// pureFnCacheRoot is the on-disk root for the per-fn .so cache populated by
// the Phase C2 pipeline. Mirrors the codegen-side ctfeCacheDir helper so
// main.go can read/write the cache without importing internal codegen state.
const pureFnCacheRoot = ".build/pure-fn"

// pureFnCacheEnabled reports whether the per-fn .so cache should be emitted
// for this build. Off by default; toggle with TIN_PURE_FN_CACHE=1 until the
// dispatch wiring (Phase C3) is in place and we can promote it to always-on.
func pureFnCacheEnabled() bool {
	return os.Getenv("TIN_PURE_FN_CACHE") == "1"
}

// emitPureFnCache walks the #pure artifacts produced by codegen, skips those
// already cached on disk, and compiles the rest in parallel via the same
// worker pool sized by -j.
func emitPureFnCache(cg *codegen.CodeGen, prog *compileProgress) error {
	artifacts := cg.PureFnsForCache()
	if len(artifacts) == 0 {
		return nil
	}

	type pendingFn struct {
		artifact codegen.PureFnArtifact
		llPath   string
		soPath   string
	}

	var pending []pendingFn

	for _, a := range artifacts {
		dir := filepath.Join(pureFnCacheRoot, a.Hash)

		soPath := filepath.Join(dir, "bin.so")
		if info, err := os.Stat(soPath); err == nil && !info.IsDir() && info.Size() > 0 {
			continue // already cached
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}

		llFile, err := os.CreateTemp("", "tin-purefn-*.ll")
		if err != nil {
			return fmt.Errorf("temp .ll: %w", err)
		}

		if _, err := llFile.WriteString(a.IRText); err != nil {
			_ = llFile.Close()
			_ = os.Remove(llFile.Name())

			return fmt.Errorf("write .ll: %w", err)
		}

		_ = llFile.Close()

		if dumpDir := os.Getenv("TIN_DUMP_SHIM"); dumpDir != "" {
			_ = os.MkdirAll(dumpDir, 0o755)
			_ = os.WriteFile(filepath.Join(dumpDir, a.Name+".ll"), []byte(a.IRText), 0o644)
		}

		pending = append(pending, pendingFn{
			artifact: a,
			llPath:   llFile.Name(),
			soPath:   soPath,
		})
	}

	if len(pending) == 0 {
		return nil
	}

	defer func() {
		for _, p := range pending {
			_ = os.Remove(p.llPath)
		}
	}()

	if prog != nil {
		prog.step("pure-fn cache", fmt.Sprintf("emit (%d fns)", len(pending)))
	}

	var jobsList []compileJob

	for i := range pending {
		// Write to a unique temp path then rename: prevents concurrent
		// `tin` processes (or this process's parallel jobs) from
		// half-writing the same final .so. The runner does the rename
		// atomically once the clang call succeeds.
		tempSo := pending[i].soPath + fmt.Sprintf(".tmp.%d", os.Getpid())

		// Per-fn .so files are dlopen'd by the running tin process
		// during CTFE evaluation. They MUST match the host's ABI even
		// when the user asked for a Darwin cross-compile - loading a
		// Mach-O .dylib into a Linux ELF process would fail at the
		// dlopen call. Use hostClangTargetFlag() (which returns nil)
		// instead of clangTargetFlag() so clang picks the host triple
		// regardless of -target.
		//
		// `-Wno-override-module` silences clang's complaint that the
		// IR module's target triple (set by codegen for the user's
		// requested target) doesn't match the host triple we're
		// compiling for. The override is intentional: CTFE shims live
		// in the host process, not in the produced binary.
		args := append([]string{
			"-shared", "-fPIC", "-O2",
			"-Wno-override-module",
		}, hostClangTargetFlag()...)
		args = append(args, pending[i].llPath, "-o", tempSo)
		jobsList = append(jobsList, compileJob{
			desc:     pending[i].artifact.Name,
			args:     args,
			renameTo: pending[i].soPath,
		})
	}

	if err := runParallelClang(jobsList); err != nil {
		return err
	}

	// Each .so is now in place - record its (hash -> shim name) manifest so
	// LoadPureFn can flag a future lookup whose Merkle hash matches but
	// whose expected shim symbol diverged (catches stale entries from a
	// hash-function change or a developer mistake).
	for i := range pending {
		shim := codegen.PureFnShimName(pending[i].artifact.Name)
		if err := codegen.WritePureFnCacheManifest(pending[i].artifact.Hash, shim); err != nil {
			return fmt.Errorf("manifest for %s: %w", pending[i].artifact.Name, err)
		}
	}

	return nil
}

// csrcCacheRoot is the directory holding cached .o files for runtime.c and
// for every //!+file.c source. Keyed by content+flags MD5 so that the same
// file compiled with the same flags reuses the .o across every Tin compile.
const csrcCacheRoot = ".build/csrc"

// csrcCacheLookup returns the cache path for compiling srcPath with the given
// args. The returned path always exists in the cache layout (the parent dir
// is created if needed); hit==true when a previously-built .o is already on
// disk and can be reused without recompiling, hit==false when the caller
// must produce the .o at the returned path.
//
// The cache key is sha256 of (file content + the canonical clang argv), so a
// flag change (e.g. -g, -fsanitize=address) produces a fresh entry rather
// than reusing a stale optimized .o.
func csrcCacheLookup(srcPath string, args []string) (string, bool, error) {
	body, err := os.ReadFile(srcPath)
	if err != nil {
		return "", false, fmt.Errorf("csrc cache: %w", err)
	}

	sum := md5.New()
	sum.Write(body)

	for _, a := range args {
		sum.Write([]byte{0})
		sum.Write([]byte(a))
	}

	key := hex.EncodeToString(sum.Sum(nil))
	dir := filepath.Join(csrcCacheRoot, key[:2])

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("csrc cache: %w", err)
	}

	objPath := filepath.Join(dir, key+".o")
	if info, err := os.Stat(objPath); err == nil && !info.IsDir() && info.Size() > 0 {
		return objPath, true, nil
	}

	return objPath, false, nil
}

// compileJob describes a single `clang ...` invocation that runParallelClang
// can fan out. desc is shown to the user via progress / error messages; args
// are passed verbatim (no shell escaping). When renameTo is non-empty, the
// runner renames the just-produced output (the path that appears as the -o
// target inside args) to renameTo on success - so concurrent `tin` processes
// can't half-write the same shared cache entry.
type compileJob struct {
	desc     string
	args     []string
	renameTo string
}

// runParallelClang fans out a list of independent `clang ...` invocations
// across a worker pool sized by parallelJobs(). It returns the first error it
// observes; remaining jobs are awaited so temp files have predictable lifetimes.
func runParallelClang(jobsList []compileJob) error {
	if len(jobsList) == 0 {
		return nil
	}

	workers := parallelJobs()
	if workers > len(jobsList) {
		workers = len(jobsList)
	}

	sem := make(chan struct{}, workers)
	errs := make([]error, len(jobsList))

	var wg sync.WaitGroup

	for i := range jobsList {
		wg.Add(1)

		sem <- struct{}{}

		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			cmd := exec.Command("clang", jobsList[i].args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			errs[i] = cmd.Run()
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("clang %s: %w", jobsList[i].desc, err)
		}
	}

	// Promote each job's temp output to its final cache path. Rename is
	// atomic on the same filesystem, so a second concurrent process
	// reading the cache either sees the previous file or the fresh one,
	// never a partial write.
	for _, j := range jobsList {
		if j.renameTo == "" {
			continue
		}

		tempPath := outputPathFromArgs(j.args)
		if tempPath == "" || tempPath == j.renameTo {
			continue
		}

		if err := os.Rename(tempPath, j.renameTo); err != nil {
			return fmt.Errorf("atomic rename %s -> %s: %w", tempPath, j.renameTo, err)
		}
	}

	return nil
}

// outputPathFromArgs scans a clang argv for the value of the -o flag and
// returns it (empty if no -o is present). Used by runParallelClang to find
// the file we just wrote so it can be renamed into the final cache slot.
func outputPathFromArgs(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" {
			return args[i+1]
		}
	}

	return ""
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
		if targetGOOS == "darwin" {
			if _, err := exec.LookPath("valgrind"); err != nil {
				die("valgrind is not supported on macOS; did you mean --leaks?")
			}
		}
	case "leaks":
		if runtime.GOOS != "darwin" {
			die("leaks is a macOS-only tool; did you mean --valgrind?")
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

		// Cache lookup: if the test binary is already built and every dep
		// recorded in its SBOM still hashes the same, run the cached binary
		// directly and skip lex/parse/codegen for this file.
		cacheDir := cacheBinDir("test", fpath, src)
		cachedBin := filepath.Join(cacheDir, "bin")

		if _, statErr := os.Stat(cachedBin); statErr == nil && sbomMatches(cacheDir) {
			fmt.Printf("%s\n\n", fname)

			run := memcheckCmd(memcheck, cachedBin)

			var outBuf bytes.Buffer

			run.Stdout = io.MultiWriter(os.Stdout, &outBuf)
			run.Stderr = os.Stderr

			passed := run.Run() == nil

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

		cleanStaleCacheEntries("test", fpath)

		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			cprog.clear()
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  error: %v\n", err)

			results = append(results, result{fname, false, false, fmt.Sprintf("error: %v", err), nil})

			continue
		}

		stacktraceLinkActive = cg.StacktraceUsed()

		irText := fixCoroAttrs(mod.String())
		pkgIRTexts := collectPkgIRs(cg)
		if compErr := compileIRWithPkgs(irText, pkgIRTexts, cachedBin, false, linkFlags, fCSources, extraCFlags, cprog); compErr != nil {
			cprog.clear()
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  compile error: %v\n", compErr)

			results = append(results, result{fname, false, false, fmt.Sprintf("compile error: %v", compErr), nil})

			continue
		}

		if err := writeBuildSBOM(cacheDir, fpath, src, buildDeps(cg, fCSources)); err != nil {
			cprog.clear()
			fmt.Printf("\n=== FAIL %s ===\n", fname)

			_, _ = fmt.Fprintf(os.Stderr, "  sbom write: %v\n", err)

			results = append(results, result{fname, false, false, fmt.Sprintf("sbom write: %v", err), nil})

			continue
		}

		cprog.clear()
		fmt.Printf("%s\n\n", fname)

		run := memcheckCmd(memcheck, cachedBin)

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

// cacheBinDir returns ".build/<mode>/<dunder>_<md5>" under CWD, where
// <dunder> is the cleaned source path with `/` replaced by `__` and <md5>
// is the hex MD5 of the source bytes. mode is "run" or "test".
//
// The cache dir is the lookup key. Inside it lives `bin` (the compiled
// binary) and `sbom.txt` (an SBOM listing every dep file with its MD5 -
// see writeBuildSBOM / sbomMatches).
func cacheBinDir(mode, file string, src []byte) string {
	cleaned := filepath.ToSlash(filepath.Clean(file))
	cleaned = strings.TrimPrefix(cleaned, "/")
	dunder := strings.ReplaceAll(cleaned, "/", "__")

	sum := md5.Sum(src)

	return filepath.Join(".build", mode, fmt.Sprintf("%s_%s", dunder, hex.EncodeToString(sum[:])))
}

// sbomMatches reports whether every file recorded in <cacheDir>/sbom.txt
// still hashes to the same MD5. A missing or unreadable SBOM means "no
// match" so the build is rerun.
func sbomMatches(cacheDir string) bool {
	data, err := os.ReadFile(filepath.Join(cacheDir, "sbom.txt"))
	if err != nil {
		return false
	}

	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		// "<md5>  <path>" - same shape as `md5sum` output.
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			return false
		}

		body, err := os.ReadFile(parts[1])
		if err != nil {
			return false
		}

		sum := md5.Sum(body)
		if hex.EncodeToString(sum[:]) != parts[0] {
			return false
		}
	}

	return true
}

// writeBuildSBOM records the entry source file plus every transitive
// dependency the build pulled in (imported package .tin sources and any
// `//!+file.c` C sources) under <cacheDir>/sbom.txt. On the next run
// sbomMatches re-hashes each path and refuses the cache if anything
// changed.
func writeBuildSBOM(cacheDir, entryFile string, entrySrc []byte, depPaths []string) error {
	seen := map[string]bool{entryFile: true}

	var sb strings.Builder

	entrySum := md5.Sum(entrySrc)
	fmt.Fprintf(&sb, "%s  %s\n", hex.EncodeToString(entrySum[:]), entryFile)

	for _, p := range depPaths {
		if seen[p] {
			continue
		}

		seen[p] = true

		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		sum := md5.Sum(body)
		fmt.Fprintf(&sb, "%s  %s\n", hex.EncodeToString(sum[:]), p)
	}

	return os.WriteFile(filepath.Join(cacheDir, "sbom.txt"), []byte(sb.String()), 0o644)
}

// cleanStaleCacheEntries removes every subdirectory of .build/<mode>/ whose
// name starts with "<dunder>_" - they're all stale candidates for the
// current source. Called before recreating the fresh cache dir on a miss
// so old binaries from prior builds don't pile up.
func cleanStaleCacheEntries(mode, file string) {
	cleaned := filepath.ToSlash(filepath.Clean(file))
	cleaned = strings.TrimPrefix(cleaned, "/")
	dunder := strings.ReplaceAll(cleaned, "/", "__")

	base := filepath.Join(".build", mode)

	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}

	prefix := dunder + "_"
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}

		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// buildDeps gathers the dep file paths that should appear in the SBOM,
// drawing from imported package source files and the //!+file.c C sources
// pulled in for this build. The entry file is added separately by
// writeBuildSBOM.
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
func execRunBinary(bin, memcheck string, binArgs []string) {
	run := memcheckCmd(memcheck, bin, binArgs...)
	run.Stdout = os.Stdout
	run.Stderr = os.Stderr

	if err := run.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}

		die("run error: %v", err)
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
		if e.Name() == "csrc" || e.Name() == "pure-fn" {
			continue
		}

		if err := os.RemoveAll(filepath.Join(".build", e.Name())); err != nil {
			die("clean: %v", err)
		}
	}
}
