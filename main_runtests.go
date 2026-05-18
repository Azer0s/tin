package main

import (
	"bytes"
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
	"time"

	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

type compileJob struct {
	desc     string
	args     []string
	renameTo string
	runFn    func() error
}

// runParallelClang fans out a list of independent `clang ...` invocations
// across a worker pool sized by parallelJobs(). It returns the first error it
// observes; remaining jobs are awaited so temp files have predictable lifetimes.
//
// onJobEvent (optional) receives lifecycle notifications - one "start" when a
// worker picks a job up, one "done" with elapsed time when it finishes. The
// callback is invoked from worker goroutines and must be safe for concurrent
// use; compileProgress.parallelEvent serializes its writes via an internal
// mutex so call sites don't need to.
func runParallelClang(jobsList []compileJob, onJobEvent func(desc, kind string, elapsed time.Duration)) error {
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

			start := time.Now()

			if onJobEvent != nil {
				onJobEvent(jobsList[i].desc, "start", 0)
			}

			if jobsList[i].runFn != nil {
				errs[i] = jobsList[i].runFn()
			} else {
				cmd := exec.Command("clang", jobsList[i].args...)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				errs[i] = cmd.Run()
			}

			if onJobEvent != nil {
				onJobEvent(jobsList[i].desc, "done", time.Since(start))
			}
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

// memcheckCmdWithSuppressions builds the exec.Cmd to run binary under
// the requested checker.  binArgs are forwarded to the binary as its
// argv[1..].  vgSuppressions feeds `--suppressions=FILE` to valgrind
// from per-file `//!-suppressions=FILE` directives in the test source;
// the scope stays at the file that opted in -- a global suppression
// set would silently hide leaks in unrelated tests.
//
// $TIN_EXEC_WRAPPER prepends a runner (e.g. "qemu-aarch64") so cross-compiled
// foreign-arch binaries can be exercised on the host. Modeled on Go's GOEXEC
// and Cargo's CARGO_TARGET_<TRIPLE>_RUNNER.
func memcheckCmdWithSuppressions(memcheck, binary string, vgSuppressions []string, binArgs ...string) *exec.Cmd {
	switch memcheck {
	case "valgrind":
		vgArgs := []string{
			"--error-exitcode=1",
			"--leak-check=full",
			"--errors-for-leak-kinds=all",
		}
		for _, s := range vgSuppressions {
			vgArgs = append(vgArgs, "--suppressions="+s)
		}

		vgArgs = append(vgArgs, binary)
		vgArgs = append(vgArgs, binArgs...)

		return wrapExec("valgrind", vgArgs...)
	case "leaks":
		args := append([]string{"--atExit", "--", binary}, binArgs...)

		return wrapExec("leaks", args...)
	default:
		return wrapExec(binary, binArgs...)
	}
}

// runMemcheck starts cmd and waits for it to finish. For leaks, it enforces a
// per-test timeout: on macOS 15 CI, leaks --atExit injects a library into the
// binary that runs analysis on exit but then deadlocks inside the binary
// before returning control. The binary (child of the leaks process) is what
// actually hangs; killing only leaks orphans it. On timeout we first kill the
// binary children via pgrep, then kill leaks itself.
// Note: leaks requires the foreground process group (Setpgid breaks it).
func runMemcheck(memcheck string, cmd *exec.Cmd) error {
	const leaksTimeout = 15 * time.Second
	// After we give up on the leaks run and SIGKILL its children, the
	// binary's atexit-injected analysis library can deadlock inside its
	// own destructor and never let cmd.Wait() return.  Without an upper
	// bound here the leaks step on the macOS GHA runner stalls for the
	// full job timeout (we measured 4m+ on `Run example tests` with no
	// progress and no terminator).  Cap the post-kill drain so the
	// runner gives up after a bounded window and the test loop moves on.
	const leaksKillDrain = 5 * time.Second

	if memcheck != "leaks" {
		return cmd.Run()
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(leaksTimeout):
		killLeaksTree(cmd.Process.Pid)

		select {
		case <-done:
		case <-time.After(leaksKillDrain):
			// Children refused to die even after SIGKILL.  Leak the
			// goroutine -- it'll get GC'd on process exit -- and
			// return so the test runner can continue.
		}

		return nil
	}
}

// killLeaksTree kills the binary that leaks --atExit launched (its child),
// then kills the leaks process itself. Without killing the child first, the
// binary orphans and accumulates across tests because killing only the leaks
// parent leaves the binary running (the injection code deadlocks inside it).
func killLeaksTree(leaksPid int) {
	if out, err := exec.Command("pgrep", "-P", strconv.Itoa(leaksPid)).Output(); err == nil {
		for _, s := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	}

	if p, err := os.FindProcess(leaksPid); err == nil {
		_ = p.Kill()
	}
}

// wrapExec returns exec.Command(prog, args...) unless $TIN_EXEC_WRAPPER
// is set AND we're running a foreign binary (cross-OS or cross-arch),
// in which case it splits the wrapper on whitespace and prepends it.
// The cross-target guard prevents the wrapper from leaking onto host-
// platform tools (e.g. macro CTFE shims always build for the host).
//
// $TIN_EXEC_WRAPPER may contain whitespace-separated args (e.g.
// `qemu-aarch64 -L /usr/aarch64-linux-gnu`), but doesn't support
// quoted args with embedded whitespace -- matching Cargo's
// CARGO_TARGET_<TRIPLE>_RUNNER convention.
func wrapExec(prog string, args ...string) *exec.Cmd {
	wrapper := strings.TrimSpace(os.Getenv("TIN_EXEC_WRAPPER"))
	if wrapper == "" || (runtime.GOOS == targetGOOS && runtime.GOARCH == targetGOARCH) {
		return exec.Command(prog, args...)
	}

	parts := strings.Fields(wrapper)
	full := append(append([]string{}, parts[1:]...), prog)
	full = append(full, args...)

	return exec.Command(parts[0], full...)
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

	// Forward TTY state to child test binaries via env var, since their stdout
	// is piped through io.MultiWriter and isatty() in the child returns false.
	if isStdoutTTY() {
		_ = os.Setenv("TIN_TEST_COLOR", "1")
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

		// Parse //!-suppressions= directives up front so both the
		// cache-hit and the fresh-compile branches below can hand them
		// to memcheckCmdWithSuppressions; valgrind picks them up,
		// non-valgrind runs just ignore the list.
		_, _, fileVgSuppressions := parseFileDirectives(string(src), filepath.Dir(fpath), stdlibDirForDirectives(""))

		// Cache lookup: if the test binary is already built and every dep
		// recorded in its SBOM still hashes the same, run the cached binary
		// directly and skip lex/parse/codegen for this file.
		cacheDir := cacheBinDir("test", fpath, src)
		cachedBin := filepath.Join(cacheDir, "bin")

		if _, statErr := os.Stat(cachedBin); statErr == nil && sbomMatches(cacheDir) {
			fmt.Printf("%s\n\n", blueIfTTY(fname))

			run := memcheckCmdWithSuppressions(memcheck, cachedBin, fileVgSuppressions)

			var outBuf bytes.Buffer

			colorOut := newMemcheckColorWriter(os.Stdout)
			colorErr := newMemcheckColorWriter(os.Stderr)
			run.Stdout = io.MultiWriter(colorOut, &outBuf)
			run.Stderr = colorErr

			passed := runMemcheck(memcheck, run) == nil
			_ = colorOut.Flush()
			_ = colorErr.Flush()

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

		p := parser.New(tokens, fpath)
		for name, expansion := range codegen.ScanImportedNoParensMacros(fpath, tokens, "", nil) {
			p.RegisterNoParensMacro(name, expansion)
		}

		prog, parseErr := p.Parse()
		if parseErr != nil {
			cprog.clear()

			rendered := codegen.RenderDiagnostic(parseErr.Error())
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: parse error\n%s\n", fname, rendered)
			results = append(results, result{fname, false, true, fmt.Sprintf("parse error: %v", parseErr), nil})

			continue
		}

		for _, raw := range p.Warnings() {
			_, _ = fmt.Fprintln(os.Stderr, codegen.RenderDiagnostic(raw))
		}

		cprog.step(fpath, "codegen")

		cg := codegen.New(fpath)
		cg.SetTestMode(true)

		if explicitTarget {
			if triple := clangTripleForTarget(); triple != "" {
				cg.SetTargetTriple(triple)
			}
		}

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

		fileLinks, fCSources, _ := parseFileDirectives(string(src), filepath.Dir(fpath), stdlibDirForDirectives(""))

		srcLinks := append([]string{}, fileLinks...)
		for _, lib := range cg.LinkLibs() {
			srcLinks = append(srcLinks, "-l"+lib)
		}
		// Collect //!+file.c and //!-lNAME directives from imported packages,
		// just as the single-file build path does.  --valgrind suppression
		// directives stay scoped to the test file -- pulling them in from
		// every transitive package would silence checks they didn't opt
		// into.
		for _, pkgSrc := range cg.PackageSrcPaths() {
			pkgBytes, pkgReadErr := os.ReadFile(pkgSrc)
			if pkgReadErr != nil {
				continue
			}

			pkgLinks, pkgCSrcs, _ := parseFileDirectives(string(pkgBytes), filepath.Dir(pkgSrc), stdlibDirForDirectives(""))
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

		srcLinks = dedupLinkerFlags(srcLinks)

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
		fmt.Printf("%s\n\n", blueIfTTY(fname))

		run := memcheckCmdWithSuppressions(memcheck, cachedBin, fileVgSuppressions)

		var outBuf bytes.Buffer

		colorOut := newMemcheckColorWriter(os.Stdout)
		colorErr := newMemcheckColorWriter(os.Stderr)
		run.Stdout = io.MultiWriter(colorOut, &outBuf)
		run.Stderr = colorErr

		passed := true
		if runErr := runMemcheck(memcheck, run); runErr != nil {
			passed = false
		}

		_ = colorOut.Flush()
		_ = colorErr.Flush()

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
		fmt.Printf("%s\n", greenIfTTY(fmt.Sprintf("all %d test file(s) passed.", len(results))))

		return
	}

	passed := len(results) - failed - skipped

	failPart := fmt.Sprintf("%d failed", failed)
	if failed > 0 {
		failPart = redIfTTY(failPart)
	}

	fmt.Printf("%d passed, %s, %d skipped (%d total)\n", passed, failPart, skipped, len(results))

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
