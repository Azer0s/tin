package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

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
  tin build       --lib <file.tin> [-o out]   compile to shared library (.so / .dylib)
                                              add -static to emit a relocatable .o instead
  tin test        <file.tin|dir|dir/...>      run test blocks and report results
  tin build-test  <file.tin> [-o out]         compile test binary without running
  tin ir          <file.tin> [-o out]         emit LLVM IR (default: stdout)
  tin ir-test     <file.tin> [-o out]         emit test-mode LLVM IR
  tin preprocess  <file.tin>                  expand macros and print source to stdout
  tin repl        [--stdlib PATH] [file.tin]  interactive REPL (preloads file)
  tin fmt         [--check] <file.tin>...     normalize indent / blank lines / EOF;
                                              --check prints unformatted files and exits non-zero
  tin clean                                   delete the local .build/ cache

Output:
  -o PATH                  write binary / object file to PATH
  -g                       emit debug info (-O0 + -g)
  -static                  link executables fully statically (Linux: -static against libc.a
                           and friends; macOS: accepted but is a no-op since libSystem cannot
                           be linked statically). For --lib, swaps the default shared library
                           (.so / .dylib) for a single relocatable object (.o) merged via ld -r.
  --emit-header=PATH       emit a C header for #interop functions

Source / library:
  --stdlib PATH            override the stdlib path (default: <execDir>/stdlib)
  --lib-root PATH          add a package root, repeatable (before default <execDir>/libs)
  --cflag FLAG             pass FLAG to clang, repeatable (e.g. --cflag -fsanitize=address)
  --mimalloc               link with mimalloc (DEFAULT; kept for back-compat).
                           Tin's arena-segregated ARC requires mimalloc;
                           install libmimalloc (brew/pacman/apt) before
                           building. fails at link if it can't be found.
  --no-mimalloc            disable mimalloc linkage. builds will fail
                           at link time; reserved for future degraded-mode
                           work (sanitizers, bare-metal targets).
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
  --no-runtime-checks      strip emission of runtime safety checks (currently: the ++=
                           borrowed-view / immortal-cap panic). Trades the icmp-and-branch per
                           write site for the chance of silently mutating shared storage; only
                           use for tight loops audited to ++= owned slices.

Warnings (all warnings carry a name; -Werror=<name> escalates one):
  -Wall                    Enable hygiene checks that almost never produce false
                           positives in idiomatic Tin code: unused-let, unused-result,
                           let-no-reassign, use-before-assign, float-equal, style.
                           Routine builds usually want this on.
  -Wpedantic               Issue diagnostics that are technically correct but often
                           acceptable in idiomatic Tin code: unused parameters
                           required by a trait conformance, magic-number literals,
                           bare calls to fn{#async}, calls to a #interop fn from
                           Tin itself, etc. Enable when auditing new code or
                           reviewing a module; routine builds usually do not want
                           them. Implies -Wall.
  -W<name>                 Enable a single default-off warning (e.g. -Wunused-let).
  -Wno-<name>              Silence a warning entirely.
  -Werror                  Treat every warning as an error.
  -Werror=<name>           Treat the named warning as an error.

  Default-on:
    array-bounds                index out of bounds for known-length array
    async-main                  main() uses spawn/await but is not #async
    await-match-guards          guard clauses in await-match arms
    bare-parking-async-call     calling an async fn whose body may park, without spawn/await
    bool-analysis               condition that folds to true/false at compile time
    cast-truncates              numeric cast that loses bits at runtime
    deref-nil                   dereference of literal nil
    discarded-pure-call         dropped result of a #pure call has no effect
    div-by-zero / shift-overflow  arithmetic that's UB at runtime
    double-deinit               value deinit'd twice on the same path
    empty-body                  if / for / while body that is empty
    fiber-misuse                double close, send-after-close, lock-without-unlock, unused mutex
    identical-operands          x == x, x - x, x & x, ...
    impossible-range            for-loop range bounds that can never produce values
    ineffective-allow-drop      "#allow_drop" on a non-must-use return type does nothing
    infinite-recursion          fn that always re-enters itself with no exit branch
    large-stack-alloc           local whose stack footprint exceeds the safe threshold
    loop-invariant              pure expression in a loop body that doesn't depend on loop state
    manual-alloc-leak           mem::malloc/calloc/realloc result leaks on some path (no free)
    manual-double-free          mem::free called twice on the same pointer along one path
    manual-use-after-free       deref/pass of an mem::malloc'd pointer after mem::free
    match-result-try            two-arm Result match that .unwrap / .expect /
                                .unwrap_or / .map / .map_err / try would replace
    must-use                    discarded #must_use value (Result, Future, etc.)
    ptr-trait                   "*Trait" in a fn signature or struct field (redundant
                                indirection on top of the trait fat-ptr; prefer "Trait")
    redundant-import-prefix     "pkg::sub::x" after "use pkg::sub" already binds "sub"
    redundant-type-cast         "<lit> as T" where T is already pinned by context
    return-try                  "return try expr" (yields the unwrapped V,
                                rarely what callers want)
    self-assign                 x = x
    tautological-int-cmp        integer comparison that always folds to true/false
    tautological-pointer-cmp    comparing a non-nil pointer against nil
    trait-snapshot-mutation     value-source coerce to a trait whose impl mutates through
                                *Self (mutations won't propagate; suggests "&Lit" alias form)
    unguarded-trait-downcast    "expr as *Concrete" without an "is *Concrete" guard
    unreachable-code            statements after return / panic / infinite loop
    unsafe-required             raw-pointer arithmetic outside a {#unsafe} block
    unused-import               imported package or name never referenced
    unused-match-arms           unreachable match case / where clause
    unused-wildcard             "_" pattern that shadows an in-scope binding
    unwrapped-c-resource        C-managed resource field not wrapped in *rc::Cell
    use-after-deinit            value used after explicit deinit
    useless-arith-identity      x + 0, x * 1, x | 0, ... that the optimizer drops
    useless-cast                cast whose source and target types are identical
    float-precision             float literal that loses precision at the chosen width
    write-to-const              write through a pointer alias to a top-level const

  Default-off (opt in via -W<name>, -Wall, or -Wpedantic):
    alias-mutation              Writing to a binding declared via 'let b = a' from
                                another fat-pointer; reaches through to the shared
                                buffer. Smell: writer assumed copy semantics; use
                                'let b = copy(a)' to break the alias.
    bare-async-call             Every bare call to an fn{#async}. Pedantic superset
                                of bare-parking-async-call (default-on, body-aware);
                                this one flags pure-compute async fns too.
                                Smell: forgot 'spawn' / 'await', call runs sync.
    builtin-shadow              Local binding masks a compile-time builtin (typeof,
                                sourcepos, ...). Smell: downstream calls to the
                                builtin silently break.
    droppable-fiber             spawn fn(args) whose Future is neither stored,
                                returned, nor awaited. Smell: fire-and-forget that
                                drops work at scope exit; awaitable was intended.
    float-equal                 == / != between floats. Smell: comparing IEEE 754
                                values exactly when an epsilon was intended.
    interop-self-call           Tin code calls a fn tagged #interop. Smell: the
                                tag does not change how Tin calls behave (they
                                bypass the C-export wrapper); the tag is dead
                                metadata at the call site. Drop the tag, or split
                                into 'fn foo_impl ... fn{#interop} foo = foo_impl(...)'.
    magic-number                int/float literal where a named const would convey
                                intent. Smell: tuning constants buried in
                                expressions with no name for reviewers.
    non-tin-thread              #interop fn body reaches 'await' or 'spawn'. Smell:
                                a C caller on a non-Tin pthread triggers scheduler
                                state the runtime never initialized there.
    style                       Naming conventions, trailing whitespace, missing
                                EOF newline. Smell: drift from project style.
    sync-fn-coerced-to-async    Sync fn coerced into a fn{#async} slot; bytes
                                match but the callee will not see await/spawn
                                coloring. Smell: callers expected the fn to yield.
    sync-uses-await             Sync fn body contains a literal 'await'. Smell:
                                hidden parking; prefer sync::wait(future) to make
                                the bridge explicit, or promote to fn{#async}.
    unchecked-div               a / b or a % b where the divisor is not proven
                                non-zero by dataflow (complement to default-on
                                div-by-zero hard error). Smell: code path the
                                analyzer cannot prove safe.
    unchecked-index             arr[i] where i is not bounds-checked, OR t[k] on
                                a custom ::index impl without (v, ok) destructure
                                (complement to default-on array-bounds).
                                Smell: index source is user input or unbounded.
    unchecked-nil-deref         *p or p.field where p is not proven non-nil by
                                dataflow (complement to default-on deref-nil).
                                Smell: deref before nil-check on the same path.
    unchecked-returned-nil      Deref of a value whose source function may return
                                nil (interprocedural via Andersen points-to).
                                Smell: caller never matched on the return.
    unclosed-closeable          io::Closeable binding leaves scope without close().
                                Smell: file/socket/handle leaked at scope exit.
    let-no-reassign             Mutable 'let' binding that is never reassigned.
                                Smell: 'const' communicates immutability better and
                                lets the optimizer fold.
    unused-let                  let-binding that is never read. Smell: dead
                                variable from a refactor.
    unused-param                fn parameter that is never read. Smell: signature
                                outdated, or required by a trait conformance.
    unused-result               Discarded result of a non-void call. Smell: the
                                call's status is being thrown away.
    use-before-assign           Local read before being assigned on every path.
                                Smell: forgot to initialize on one branch.

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

// tempCacheCounter disambiguates concurrent temp-file names. Multiple goroutines
// inside one `tin test` invocation race to populate the same content-hashed
// cache slot when two test files share a generic instantiation; the PID alone
// (os.Getpid()) is identical for all of them, so the .o.tmp.PID path collides
// and clang fails with "permission denied" on the partial file. Adding an
// atomic counter to the suffix makes each goroutine's temp path unique.
var tempCacheCounter uint64

// optLevelOverride is the -O flag value (0/1/2/3/s) supplied on the command
// line, or "" when the user did not pass -O. When non-empty it overrides the
// default optLevel chosen by compileIR.
var optLevelOverride string

// debugBuild and extraCFlags are populated by main() during arg parsing
// and read by buildFlagsHash() to mix the invocation-time settings into
// the binary cache key. Without these the cache would silently reuse a
// non-debug `-O2` build for a `-g` request, or vice versa.
var (
	debugBuild  bool
	useMimalloc bool
	extraCFlags []string
	// staticLink is set by -static. On Linux it forwards `-static` to
	// clang at link time so libc/libm/etc. are pulled in as archives.
	// On macOS the flag is accepted but nothing extra is forwarded --
	// libSystem can't be statically linked, so the toolchain is
	// already "as static as possible" by default.
	staticLink bool
	// Cache-invalidation: the runtime-check toggles change which
	// safety branches are compiled in, so they must mix into the
	// cache key. Otherwise toggling --check=NAME or --no-check=NAME on
	// a previously compiled source would return the stale binary
	// built with the prior flags. Package-level so buildFlagsHash()
	// can read them.
	cachedCheckFlagsKey string
)

// testFastCompile is reserved for an opt-in "fast tests" mode that defaults
// `tin test` to -O0 (~10x suite speedup). Currently off because the win was
// largely subsumed by the internal-linkage DCE change - clang at -O2 now
// drops dead stdlib early in compile rather than carrying it through every
// optimizer pass. Users wanting -O0 can pass it explicitly.
var testFastCompile bool

func main() {
	// Default ANSI color on when stderr is a terminal -- matches what
	// rustc and clang do. Overridable via --color={always,never,auto}
	// later in arg parsing.
	//
	// Honor the standard env-var conventions:
	//   NO_COLOR (any value)        -> disable colors unconditionally
	//                                  (https://no-color.org)
	//   CLICOLOR_FORCE=1            -> force colors even when piped
	//                                  (https://bixense.com/clicolors/)
	// The CLI flag --color=<auto|always|never> still wins over both.
	codegen.AnsiEnabled = isStderrTTY()
	if _, hasNoColor := os.LookupEnv("NO_COLOR"); hasNoColor {
		codegen.AnsiEnabled = false
	}

	if os.Getenv("CLICOLOR_FORCE") == "1" {
		codegen.AnsiEnabled = true
	}

	// Toolchain checks are deferred until we know the user is asking
	// for a compile (run/build/test/...). Subcommands that don't touch
	// the backend (clean, repl-help text, no-args usage) shouldn't
	// fail just because clang/opt aren't installed yet -- a fresh user
	// running `tin --help` deserves to see the help, not an install
	// hint. ensureBackendReady() below is called from each compile
	// path before any clang/opt invocation.

	if len(os.Args) >= 2 && os.Args[1] == "repl" {
		ensureBackendReady()

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

	if len(os.Args) >= 2 && os.Args[1] == "fmt" {
		runFmt(os.Args[2:])

		return
	}

	if len(os.Args) < 3 {
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(1)
	}

	ensureBackendReady()

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
		case "-g", "-static", "--fast", "--no-pure-fold", "-fno-pure-fold", "--no-runtime-checks", "--explain-ownership":
			fileArgIdx++
		case "--stdlib", "--lib-root", "-target", "-j", "--color", "--error-format":
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

			// `--explain-ownership=spec` is a single-token "key=value" flag.
			if strings.HasPrefix(a, "--explain-ownership=") {
				fileArgIdx++

				continue
			}

			// `--check=names` / `--no-check=names` are single-token
			// "key=value" flags.
			if strings.HasPrefix(a, "--check=") || strings.HasPrefix(a, "--no-check=") {
				fileArgIdx++

				continue
			}

			// -Wall / -Wpedantic / -W<name> / -Wno-<name> / -Werror[=name]
			// are single-token flags too -- they need to be skipped here
			// so the file argument lands correctly when callers write
			// `tin run -Wpedantic foo.tin`.
			if strings.HasPrefix(a, "-W") || strings.HasPrefix(a, "-Werror") {
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
	// extraCFlags and debugBuild are package-level (declared near
	// optLevelOverride) so buildFlagsHash() can mix them into the cache
	// key; main() resets them per-invocation here.
	extraCFlags = nil
	debugBuild = false
	// mimalloc is REQUIRED: Tin's arena-segregated ARC routes every
	// managed allocation through a dedicated mimalloc arena so the
	// runtime can distinguish Tin-managed pointers from C-allocated
	// ones (see runtime/heap_arena.c).  The flag toggle stays for
	// future degraded-mode work; users must install libmimalloc.
	useMimalloc = true

	var stdlibOverride string

	var extraLibsRoots []string

	noWarnAwaitMatchGuards := false
	verboseMatchInfo := false
	verboseDemorgan := false
	emitHeaderPath := ""
	allWarnsAsErrors := false
	wAll := false
	wPedantic := false
	noPureFold := false
	pureFoldBudget := 0 // 0 = use codegen default
	noRuntimeChecks := false
	explainOwnership := ""
	// --check=NAME[,NAME] selects which runtime safety checks to
	// compile into the binary.  Accumulated across repeated flags and
	// across comma lists; --check=all turns every check on; the
	// inverse --no-check=NAME individually disables.
	enabledChecks := map[string]bool{}
	disabledChecks := map[string]bool{}
	checkAll := false

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
		case "--mimalloc":
			// Kept as a no-op for backward compat with existing
			// scripts.  mimalloc is on by default since the arena-
			// segregated ARC infrastructure depends on it.
			useMimalloc = true
		case "--no-mimalloc":
			// Disables linking against mimalloc.  The runtime
			// expects mimalloc -- builds will fail at link time
			// with this flag.  Reserved for future degraded-mode
			// work (sanitizer builds, bare-metal).
			useMimalloc = false
		case "--color":
			// --color=<auto|always|never>. Defaults to `auto` which
			// turns ANSI escapes on when stderr is a terminal. The
			// snippet renderer is on by default; use --error-format=
			// plain to opt out of multi-line snippets entirely.
			if i+1 < len(os.Args) {
				i++

				switch os.Args[i] {
				case "always":
					codegen.AnsiEnabled = true
				case "never":
					codegen.AnsiEnabled = false
				case "auto":
					codegen.AnsiEnabled = isStderrTTY()
				}
			}
		case "--error-format":
			// --error-format=<rust|plain>. `plain` reverts to the
			// legacy single-line `file:line:col: msg` output, useful
			// for editor integrations that grep error positions.
			if i+1 < len(os.Args) {
				i++

				switch os.Args[i] {
				case "plain":
					codegen.SnippetEnabled = false
				case "rust":
					codegen.SnippetEnabled = true
				}
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
		case "-static":
			staticLink = true
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
		case "--no-runtime-checks":
			noRuntimeChecks = true
		case "--explain-ownership":
			// Bare form: print every binding in the program.
			explainOwnership = "*"
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
			case strings.HasPrefix(a, "--explain-ownership="):
				// Filter form: --explain-ownership=file.tin:fnName
				// (or --explain-ownership=fnName for all-files match).
				explainOwnership = strings.TrimPrefix(a, "--explain-ownership=")
			case strings.HasPrefix(a, "--check="):
				raw := strings.TrimPrefix(a, "--check=")
				for _, name := range strings.Split(raw, ",") {
					name = strings.TrimSpace(name)
					if name == "" {
						continue
					}

					if name == "all" {
						checkAll = true

						continue
					}

					enabledChecks[name] = true
				}
			case strings.HasPrefix(a, "--no-check="):
				raw := strings.TrimPrefix(a, "--no-check=")
				for _, name := range strings.Split(raw, ",") {
					name = strings.TrimSpace(name)
					if name != "" {
						disabledChecks[name] = true
					}
				}
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

	// Resolve --check= flags into the final enabled set, then build a
	// stable cache key BEFORE any cacheBinDir() call so toggling
	// --check / --no-check / --no-runtime-checks / --ownership-borrow
	// invalidates the per-source binary cache and forces re-compile.
	//
	// All runtime safety checks default OFF (the binary is
	// production-shaped out of the box); --check=NAME or --check=all
	// opts in.  The reserved names are: rc-balance, ownership-canary,
	// append-cap, cycle-detect, heap-sample, bounds.  Only append-cap
	// has a backing implementation today; the others register but
	// have no runtime effect until their backing checks land.
	resolvedChecks := map[string]bool{}

	if checkAll {
		for _, name := range []string{"append-cap", "rc-balance", "ownership-canary", "cycle-detect", "heap-sample", "bounds"} {
			resolvedChecks[name] = true
		}
	}

	for name := range enabledChecks {
		resolvedChecks[name] = true
	}

	for name := range disabledChecks {
		delete(resolvedChecks, name)
	}

	// --no-runtime-checks is the back-compat synonym for "strip
	// every runtime check"; since the default already does that,
	// the flag is a clearing operation on top of any --check= opts
	// that came earlier in argv.
	if noRuntimeChecks {
		resolvedChecks = map[string]bool{}
	}

	checkNames := make([]string, 0, len(resolvedChecks))
	for name := range resolvedChecks {
		checkNames = append(checkNames, name)
	}

	sort.Strings(checkNames)
	cachedCheckFlagsKey = strings.Join(checkNames, ",")

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
			// Pick up `//!-suppressions=` from the source so cached
			// single-file tests still hand them to valgrind.
			_, _, vgSupps := parseFileDirectives(string(src), filepath.Dir(file), stdlibDirForDirectives(stdlibOverride))
			execRunBinary(runCacheBinPath, memcheck, binArgs, vgSupps...)

			return
		}
	}

	// `tin build` shares the same content-addressed cache: when the SBOM
	// for this source still hashes the same as a prior build, copy the
	// cached binary to the user's -o path (or default name) and skip
	// codegen + clang + link entirely. Without this, every `tin build`
	// pays the full link cost even when nothing changed - which on
	// rtti_extern is ~3.5s of thinLTO link work per warm rebuild.
	if cmd == "build" && !libMode {
		buildOut := defaultBuildOutPath(file, libMode)
		if v := lookupOArg(fileArgIdx); v != "" {
			buildOut = v
		}

		buildCacheDir := cacheBinDir("build", file, src)
		buildCacheBin := filepath.Join(buildCacheDir, "bin")

		if _, statErr := os.Stat(buildCacheBin); statErr == nil && sbomMatches(buildCacheDir) {
			if err := copyAndChmodExec(buildCacheBin, buildOut); err == nil {
				return
			}
			// Copy failed (e.g. cross-device, perms): fall through to a
			// full rebuild rather than fail the user's build.
		}
	}

	// Collect directives declared in the source file via //! lines
	fileLinkerFlags, fileCSources, fileVgSuppressions := parseFileDirectives(string(src), filepath.Dir(file), stdlibDirForDirectives(stdlibOverride))

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

	p := parser.New(tokens, file)
	for name, expansion := range codegen.ScanImportedNoParensMacros(file, tokens, stdlibDirForDirectives(stdlibOverride), nil) {
		p.RegisterNoParensMacro(name, expansion)
	}

	if noWarnAwaitMatchGuards {
		p.SetNoWarnAwaitMatchGuards(true)
	}

	prog, parseErr := p.Parse()
	if parseErr != nil {
		die("parse error: %s", parseErr.Error())
	}

	for _, raw := range p.Warnings() {
		_, _ = fmt.Fprintln(os.Stderr, codegen.RenderDiagnostic(raw))
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

	// append-cap is the only check with a backing implementation
	// today; map it to the legacy noRuntimeChecks toggle (which is
	// inverted: true = strip, false = emit).  When append-cap is NOT
	// in the resolved set (computed during flag parsing above), strip
	// emission.
	cg.SetNoRuntimeChecks(!resolvedChecks["append-cap"])

	if explainOwnership != "" {
		cg.SetExplainOwnership(explainOwnership)
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

	cg.FinalizeExplainOwnership()

	// Latch the stacktrace flag for the upcoming compileIR call.  See
	// docs/plans/stacktrace-libunwind.md: `-lunwind` / `-rdynamic` /
	// `-DTIN_STACKTRACE` are gated on this.  `StacktraceUsed()` is
	// sticky-true once any codegen path called `ensurePanicFn`
	// (explicit panic, array bounds, cap-check, ADT mismatch...) so
	// an unrecovered panic dumps a backtrace; pure programs that
	// never touch panic still pay zero.
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

		pkgLinkFlags, pkgCSources, _ := parseFileDirectives(string(src), filepath.Dir(pkgSrc), stdlibDirForDirectives(stdlibOverride))
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

	fileLinkerFlags = dedupLinkerFlags(fileLinkerFlags)

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
			out += libOutSuffix()
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

		// Populate the build-cache slot so the next `tin build` of the
		// same source short-circuits via copy. Mirrors the run/test
		// cache write that fires after a successful binary build.
		if !libMode {
			buildCacheDir := cacheBinDir("build", file, src)
			if mkErr := os.MkdirAll(buildCacheDir, 0o755); mkErr == nil {
				_ = copyAndChmodExec(out, filepath.Join(buildCacheDir, "bin"))
				_ = writeBuildSBOM(buildCacheDir, file, src, buildDeps(cg, fileCSources))
			}
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

		// Write to a per-PID temp path then atomic-rename, so a
		// concurrent `tin run` of the same source can never observe
		// (or exec) a half-written binary. ETXTBSY otherwise: process
		// A holds the binary open for write while process B execs it.
		runCacheBinTmp := fmt.Sprintf("%s.tmp.%d", runCacheBinPath, os.Getpid())
		if err := compileIRWithPkgs(irText, pkgIRTexts, runCacheBinTmp, false, extraObjs, fileCSources, extraCFlags, cprog, debugBuild); err != nil {
			_ = os.Remove(runCacheBinTmp)

			die("compile error: %v", err)
		}

		if err := os.Rename(runCacheBinTmp, runCacheBinPath); err != nil {
			_ = os.Remove(runCacheBinTmp)

			die("cache rename: %v", err)
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
		execRunBinary(runCacheBinPath, memcheck, binArgs, fileVgSuppressions...)

	default:
		_, _ = fmt.Fprint(os.Stderr, usage)

		os.Exit(1)
	}
}

// clangMajorVersion returns the major clang version number, or 0 if
// the version cannot be determined. Backed by the disk-cached
// host-info record.
