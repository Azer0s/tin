package main

// Build pipeline -- LLVM tools (opt + ld.lld) only.
//
// IR-touching steps: opt for the optimizer + ThinLTO pre-link prep,
// ld.lld for the link (which natively runs ThinLTO at link when handed
// bitcode inputs). Coroutine lowering runs through opt -O1 -S.
//
// C source compilation still uses clang -- that path needs a frontend
// and is aggressively cached so it only re-runs on source change.
//
// Tooling probes (clang --version for cache invalidation, clang -x c -
// for target-triple detection) also still use clang because there's no
// equivalent path through the LLVM tools.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Compile / link options that must be threaded down into the backend.
// Kept minimal and additive so call sites can default-zero fields they
// don't need. ltoMode is "thin" or "" (no LTO).
type compileIROpts struct {
	optLevel    string // -O0/-O1/-O2/-O3
	ltoMode     string // "thin" or ""
	targetFlags []string
}

type linkOpts struct {
	optLevel             string
	ltoMode              string // "thin" or ""
	debug                bool
	standaloneDebugMacOS bool
	functionSecs         bool
	dataSecs             bool
	gcSections           bool // -Wl,--gc-sections / -Wl,-dead_strip
	useLld               bool // -fuse-ld=lld (cross-compile)
	rdynamic             bool
	targetGOOS           string
	targetFlags          []string
	cLinkerFlags         []string
	extraCFlags          []string
}

// optThinLtoPreLinkPass picks the right opt pipeline name for the
// requested optimization level. Mirrors clang's `-c -flto=thin -O<N>`
// per-TU prep work.
func optThinLtoPreLinkPass(optLevel string) string {
	switch optLevel {
	case "-O0":
		return "thinlto-pre-link<O0>"
	case "-O1":
		return "thinlto-pre-link<O1>"
	case "-O3":
		return "thinlto-pre-link<O3>"
	case "-Os":
		return "thinlto-pre-link<Os>"
	case "-Oz":
		return "thinlto-pre-link<Oz>"
	default:
		return "thinlto-pre-link<O2>"
	}
}

// compileIRToObj compiles an LLVM IR `.ll` input into ThinLTO bitcode.
// The actual codegen happens in ld.lld at link time -- matches what
// `clang -c -flto=thin` does internally, just bypassing the clang
// frontend overhead.
func compileIRToObj(llPath, outPath string, opts compileIROpts) error {
	args := []string{
		"-passes=" + optThinLtoPreLinkPass(opts.optLevel),
		"--module-summary",
		llPath,
		"-o",
		outPath,
	}
	cmd := exec.Command("opt", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opt %s: %w", llPath, err)
	}

	return nil
}

// coroSplit runs the LLVM coroutine split pass over an `.ll` input and
// writes the split `.ll` to outPath. opt -O1 runs the same pass set
// clang does at -O1, which includes coro splitting. This works around
// LLVM 22's -O2 corrupting coro yield paths -- we split at -O1 first
// and let the -O2 link stage run on the split IR.
func coroSplit(inLL, outLL string, opts compileIROpts) error {
	cmd := exec.Command("opt", "-O1", "-S", inLL, "-o", outLL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opt -O1 (coro split) %s: %w", inLL, err)
	}

	return nil
}

// linkBinary runs the system linker with the right crt files /
// library search paths discovered via the linker probe. The probe
// captures whatever clang would invoke -- Apple's `ld` on native
// macOS, GNU `ld` / `ld.lld` on Linux, `ld64.lld` for Linux ->
// Darwin cross-compile -- so the active platform always gets its
// idiomatic linker.
//
// On ELF targets, forces `--build-id=sha1` so two cold builds of the
// same source produce byte-identical binaries (lld's default
// `--build-id` uses a timestamp hash). The flag is silently dropped
// on Mach-O where it doesn't apply.
func linkBinary(inputs []string, outBin string, opts linkOpts) error {
	entry := lldArgvFor(opts)
	if entry.err != nil {
		return entry.err
	}

	prefix := entry.prefix
	if opts.targetGOOS != "darwin" {
		prefix = forceBuildIDSha1(prefix)
	}

	argv := append([]string{}, prefix...)
	argv = append(argv, "-o", outBin)
	argv = append(argv, inputs...)
	argv = append(argv, opts.cLinkerFlags...)
	argv = append(argv, entry.suffix...)

	if opts.rdynamic && opts.targetGOOS != "darwin" {
		argv = append(argv, "--export-dynamic")
	}

	cmd := exec.Command(entry.linker, argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", entry.linker, err)
	}

	return nil
}

// forceBuildIDSha1 rewrites any `--build-id` (default: timestamp hash)
// into `--build-id=sha1` so the note is content-derived and stable
// across rebuilds. Leaves an explicit `--build-id=...` choice alone.
// ELF-only -- callers skip this on Mach-O.
func forceBuildIDSha1(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--build-id" {
			out = append(out, "--build-id=sha1")

			continue
		}

		out = append(out, a)
	}

	return out
}

// ============================================================
// Linker-args probe: discover the crt files / library search paths /
// libgcc + libc trailers that ld.lld needs but clang's driver normally
// computes. Cached by (target triple, opt level, debug, ltoMode).
// ============================================================

type lldArgvKey struct {
	triple   string
	optLevel string
	debug    bool
	ltoMode  string
	macOS    bool
	useLld   bool
}

type lldArgvEntry struct {
	linker string   // path to the linker binary (ld.lld / ld / ld64.lld / ...)
	prefix []string // crt-begin + library-paths + plugin opts (no linker name, no -o)
	suffix []string // libgcc/libc trailer + crt-end
	err    error
}

var (
	lldArgvCache   = map[lldArgvKey]*lldArgvEntry{}
	lldArgvCacheMu sync.Mutex
)

// lldArgvFor returns the cached linker-name + argv segments for the
// given link options. The cache lives in memory for the process AND
// on disk under .build/lld-probe so the probe (~40 ms of clang work)
// only runs once per (triple, opt level, debug, lto) tuple per
// machine, not per tin invocation.
func lldArgvFor(opts linkOpts) *lldArgvEntry {
	key := lldArgvKey{
		triple:   tripleFromTargetGOOS(opts.targetGOOS, opts.targetFlags),
		optLevel: opts.optLevel,
		debug:    opts.debug,
		ltoMode:  opts.ltoMode,
		macOS:    opts.targetGOOS == "darwin",
		useLld:   opts.useLld,
	}

	if entry, ok := lookupLldArgv(key); ok {
		return entry
	}

	if disk := readLldArgvFromDisk(key); disk != nil {
		storeLldArgv(key, disk)

		return disk
	}

	entry := probeLldArgv(opts)
	if entry.err == nil {
		writeLldArgvToDisk(key, entry)
	}

	storeLldArgv(key, entry)

	return entry
}

func lookupLldArgv(key lldArgvKey) (*lldArgvEntry, bool) {
	lldArgvCacheMu.Lock()
	defer lldArgvCacheMu.Unlock()

	e, ok := lldArgvCache[key]

	return e, ok
}

func storeLldArgv(key lldArgvKey, entry *lldArgvEntry) {
	lldArgvCacheMu.Lock()
	defer lldArgvCacheMu.Unlock()

	lldArgvCache[key] = entry
}

// lldProbeCacheDir is the on-disk cache root for linker-argv probe
// results. Lives under .build/host-info alongside the host clang
// metadata so a single `rm -rf .build/host-info` clears every host-
// toolchain probe at once.
const lldProbeCacheDir = ".build/host-info/lld-probe"

// lldProbeCacheKey returns the disk filename (relative to
// lldProbeCacheDir) for a given lookup key. Mixes in the clang version
// so a clang upgrade (which can change the produced argv) busts the
// cache.
func lldProbeCacheKey(k lldArgvKey) string {
	return fmt.Sprintf("%s_%s_%s_%v_%v_%s_%v.argv",
		clangVersion(), k.triple, k.optLevel, k.debug, k.macOS, k.ltoMode, k.useLld)
}

// readLldArgvFromDisk loads a cached probe result. Returns nil on miss
// or any read error -- the caller falls back to running the probe.
// Disk format is three sections separated by `\n---\n`:
//
//	linker-binary-path
//	---
//	prefix arg ...
//	---
//	suffix arg ...
func readLldArgvFromDisk(k lldArgvKey) *lldArgvEntry {
	path := filepath.Join(lldProbeCacheDir, lldProbeCacheKey(k))

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	parts := strings.Split(string(data), "\n---\n")
	if len(parts) != 3 {
		return nil
	}

	linker := strings.TrimSpace(parts[0])

	pf := strings.Split(strings.TrimRight(parts[1], "\n"), "\n")
	sf := strings.Split(strings.TrimRight(parts[2], "\n"), "\n")

	if len(pf) == 1 && pf[0] == "" {
		pf = nil
	}

	if len(sf) == 1 && sf[0] == "" {
		sf = nil
	}

	return &lldArgvEntry{linker: linker, prefix: pf, suffix: sf}
}

// writeLldArgvToDisk persists a probe result. Failures are silent --
// missing the cache slot just means we re-probe next time, which is
// cheap and correct.
func writeLldArgvToDisk(k lldArgvKey, entry *lldArgvEntry) {
	if err := os.MkdirAll(lldProbeCacheDir, 0o755); err != nil {
		return
	}

	path := filepath.Join(lldProbeCacheDir, lldProbeCacheKey(k))
	body := entry.linker + "\n---\n" +
		strings.Join(entry.prefix, "\n") + "\n---\n" +
		strings.Join(entry.suffix, "\n") + "\n"
	_ = os.WriteFile(path, []byte(body), 0o644)
}

// probeLldArgv asks clang to print the link command it would issue
// for our exact options, then captures the linker binary path and
// splits the rest of the argv into a prefix (everything up to the
// first input object) and suffix (everything after the last). The
// caller substitutes the actual `-o` and input list at the boundary.
//
// For native builds we let clang pick the system default linker
// (Apple `ld` on macOS, system `ld` on Linux). For cross-compile
// (opts.useLld) we force `-fuse-ld=lld` since the host's system
// linker rarely supports foreign target emulations.
func probeLldArgv(opts linkOpts) *lldArgvEntry {
	tmp, err := os.CreateTemp("", "tin-lld-probe-*.bc")
	if err != nil {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: temp: %w", err)}
	}

	tmpPath := tmp.Name()
	_ = tmp.Close()

	defer func() { _ = os.Remove(tmpPath) }()

	if err := writeDummyBitcode(tmpPath, opts); err != nil {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: dummy bc: %w", err)}
	}

	dummyOut, err := os.CreateTemp("", "tin-lld-probe-*.bin")
	if err != nil {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: temp out: %w", err)}
	}

	dummyOutPath := dummyOut.Name()
	_ = dummyOut.Close()

	defer func() { _ = os.Remove(dummyOutPath) }()

	args := []string{opts.optLevel}
	if opts.ltoMode == "thin" {
		args = append(args, "-flto=thin")
	}

	args = append(args, opts.targetFlags...)

	if opts.functionSecs {
		args = append(args, "-ffunction-sections")
	}

	if opts.dataSecs {
		args = append(args, "-fdata-sections")
	}

	if opts.gcSections {
		if opts.targetGOOS == "darwin" {
			args = append(args, "-Wl,-dead_strip")
		} else {
			args = append(args, "-Wl,--gc-sections")
		}
	}

	if opts.useLld {
		args = append(args, "-fuse-ld=lld")
	}

	args = append(args, tmpPath, "-o", dummyOutPath, "-###")
	out, _ := exec.Command("clang", args...).CombinedOutput()

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: empty clang -### output")}
	}

	last := lines[len(lines)-1]

	toks, err := splitShellLine(last)
	if err != nil {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: shlex: %w", err)}
	}

	if len(toks) == 0 {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: empty argv")}
	}

	// First token is the linker binary path -- whatever clang would
	// invoke (Apple's /usr/bin/ld on macOS, /usr/bin/ld.lld on
	// Linux-with-lld-default, etc.).
	linker := toks[0]
	toks = toks[1:]

	// Find the position of our dummy object in the argv. Clang
	// renames bitcode inputs to /tmp/<base>-XXXXXX.o under -flto=thin
	// before passing to ld.lld, so we search by basename prefix
	// rather than exact match.
	tmpBase := strings.TrimSuffix(filepath.Base(tmpPath), filepath.Ext(tmpPath))

	splitAt := -1

	for i, t := range toks {
		if t == tmpPath {
			splitAt = i

			break
		}

		if isClangTempForBase(t, tmpBase) {
			splitAt = i

			break
		}
	}

	if splitAt < 0 {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: dummy %q not in argv", tmpPath)}
	}

	prefix := append([]string{}, toks[:splitAt]...)
	suffix := append([]string{}, toks[splitAt+1:]...)

	// Strip the `-o dummyOutPath` pair from prefix (we always supply our
	// own -o at link time).
	cleaned := make([]string, 0, len(prefix))

	skip := false
	for _, t := range prefix {
		if skip {
			skip = false

			continue
		}

		if t == "-o" {
			skip = true

			continue
		}

		cleaned = append(cleaned, t)
	}

	prefix = cleaned

	return &lldArgvEntry{linker: linker, prefix: prefix, suffix: suffix}
}

// writeDummyBitcode emits the smallest valid bitcode file we can hand
// clang for the link probe: a single empty `main` returning 0.
func writeDummyBitcode(path string, opts linkOpts) error {
	const dummyLL = `define i32 @main() { ret i32 0 }`

	llPath := path + ".ll"

	if err := os.WriteFile(llPath, []byte(dummyLL), 0644); err != nil {
		return err
	}

	defer func() { _ = os.Remove(llPath) }()

	args := []string{"-passes=" + optThinLtoPreLinkPass(opts.optLevel), "--module-summary", llPath, "-o", path}
	if err := exec.Command("opt", args...).Run(); err != nil {
		return err
	}

	return nil
}

// splitShellLine parses a shell-style command line into argv tokens,
// respecting double quotes (clang's -### uses them for every arg).
func splitShellLine(line string) ([]string, error) {
	var (
		toks []string
		buf  strings.Builder
		in   bool
	)

	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			in = !in

			continue
		}

		if !in && (c == ' ' || c == '\t') {
			if buf.Len() > 0 {
				toks = append(toks, buf.String())
				buf.Reset()
			}

			continue
		}

		if c == '\\' && i+1 < len(line) {
			i++
			buf.WriteByte(line[i])

			continue
		}

		buf.WriteByte(c)
	}

	if buf.Len() > 0 {
		toks = append(toks, buf.String())
	}

	return toks, nil
}

// isClangTempForBase reports whether t looks like clang's renamed
// temp object for an input whose basename (without extension) equals
// `base`. Clang produces `/tmp/<base>-<hex>.o` under -flto=thin.
func isClangTempForBase(t, base string) bool {
	if !strings.HasSuffix(t, ".o") {
		return false
	}

	bt := filepath.Base(t)
	if !strings.HasPrefix(bt, base+"-") {
		return false
	}

	rest := strings.TrimSuffix(strings.TrimPrefix(bt, base+"-"), ".o")
	if len(rest) == 0 {
		return false
	}

	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}

// tripleFromTargetGOOS produces a stable cache-key fragment from the
// target. Doesn't need to match the actual triple exactly -- only needs
// to differ between targets we'd want a separate probe for.
func tripleFromTargetGOOS(targetGOOS string, targetFlags []string) string {
	for _, f := range targetFlags {
		if strings.HasPrefix(f, "-target=") || strings.HasPrefix(f, "--target=") {
			return f
		}
	}

	for i, f := range targetFlags {
		if (f == "-target" || f == "--target") && i+1 < len(targetFlags) {
			return targetFlags[i+1]
		}
	}

	return targetGOOS + "-host"
}
