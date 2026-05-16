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
	"crypto/sha256"
	"encoding/hex"
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
	// static drops the link line into a fully-static configuration.
	// Honored on Linux as `-static` (libc and friends pulled in as
	// archives); silently dropped on macOS because libSystem cannot be
	// linked statically. The flag is reflected in the lld-probe cache
	// key so static and dynamic argvs don't share a slot.
	static bool
	// useMimalloc routes runtime malloc/free through mimalloc's mi_*
	// API.  When true: the runtime.c TU is compiled with
	// -DTIN_USE_MIMALLOC=1 (which activates the macro shim that
	// redirects malloc/free/realloc/calloc to mi_*), and the link
	// line is augmented with -lmimalloc.  On by default; opt out
	// via the top-level `--no-mimalloc` flag.
	//
	// We hard-error at link time when this is true but libmimalloc
	// can't be located -- silent fall-back to libc would produce
	// confusing perf cliffs that look like regressions.
	useMimalloc  bool
	targetGOOS   string
	targetFlags  []string
	cLinkerFlags []string
	extraCFlags  []string
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

// compileIRToNativeObj compiles an LLVM IR `.ll` to a native ELF/Mach-O
// relocatable object without LTO. Used by --lib mode where the caller
// wants a plain .o for ld -r, not ThinLTO bitcode.
// Pipeline: opt <optLevel> (middle-end) -> llc -filetype=obj (codegen).
func compileIRToNativeObj(llPath, outPath, optLevel string) error {
	bcFile, err := os.CreateTemp("", "tin-lib-*.bc")
	if err != nil {
		return fmt.Errorf("cannot create temp bitcode file: %w", err)
	}

	bcPath := bcFile.Name()

	_ = bcFile.Close()

	defer func() { _ = os.Remove(bcPath) }()

	optCmd := exec.Command("opt", optLevel, llPath, "-o", bcPath)
	optCmd.Stdout = os.Stdout

	optCmd.Stderr = os.Stderr

	if err := optCmd.Run(); err != nil {
		return fmt.Errorf("opt %s: %w", llPath, err)
	}

	llcCmd := exec.Command("llc", "-filetype=obj", "--relocation-model=pic", bcPath, "-o", outPath)
	llcCmd.Stdout = os.Stdout

	llcCmd.Stderr = os.Stderr

	if err := llcCmd.Run(); err != nil {
		return fmt.Errorf("llc %s: %w", bcPath, err)
	}

	return nil
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

	// mimalloc: link before the entry.suffix so the libc that lives in
	// the suffix doesn't win the malloc/free symbol resolution race.
	// On dynamic links (the common path), `-lmimalloc` adds a DT_NEEDED
	// to libmimalloc; the runtime.c TU was compiled with the macro shim
	// (-DTIN_USE_MIMALLOC=1) so its internal calls go to mi_malloc /
	// mi_free directly, bypassing any libc-side resolution entirely.
	if opts.useMimalloc {
		libDir, _, ok := findMimallocInstall(opts.targetGOOS)
		if !ok {
			return fmt.Errorf("tin: --no-mimalloc was not passed but libmimalloc was not found at any of the standard paths. Install via `brew install mimalloc` (macOS) or your distro's package manager (pacman -S mimalloc / apt install libmimalloc-dev / dnf install mimalloc-devel), or pass --no-mimalloc to fall back to libc malloc")
		}

		argv = append(argv, "-L"+libDir, "-lmimalloc")
	}

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
	triple        string
	optLevel      string
	debug         bool
	ltoMode       string
	macOS         bool
	useLld        bool
	static        bool   // -static at link time pulls a different argv (libc.a, etc.)
	standaloneDbg bool   // -fstandalone-debug (darwin debug builds)
	mimalloc      bool   // -lmimalloc at link adds a libmimalloc.so/.dylib reference -> distinct lld probe
	extraHash     string // sha256(extraCFlags) so e.g. -fsanitize=address gets its own probe slot
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
		triple:        tripleFromTargetGOOS(opts.targetGOOS, opts.targetFlags),
		optLevel:      opts.optLevel,
		debug:         opts.debug,
		ltoMode:       opts.ltoMode,
		macOS:         opts.targetGOOS == "darwin",
		useLld:        opts.useLld,
		static:        opts.static,
		standaloneDbg: opts.standaloneDebugMacOS,
		mimalloc:      opts.useMimalloc,
		extraHash:     hashExtraCFlags(opts.extraCFlags),
	}

	if entry, ok := lookupLldArgv(key); ok {
		return entry
	}

	// Per-key lock so concurrent goroutines hitting the same key only
	// run the probe once (the second arrival re-checks the cache after
	// acquiring the lock and finds the first arrival's result).
	probeOnce(key).Do(func() {
		if disk := readLldArgvFromDisk(key); disk != nil {
			storeLldArgv(key, disk)

			return
		}

		entry := probeLldArgv(opts)
		if entry.err == nil {
			writeLldArgvToDisk(key, entry)
		}

		storeLldArgv(key, entry)
	})

	if entry, ok := lookupLldArgv(key); ok {
		return entry
	}

	return &lldArgvEntry{err: fmt.Errorf("lld probe: lookup miss after probe (internal)")}
}

// hashExtraCFlags returns a stable short hash of the user-supplied
// --cflag values so two link targets with different extras (e.g.
// -fsanitize=address vs nothing) get distinct probe cache slots. The
// link argv depends on these flags (ASan adds libasan to the link
// line); without this in the key, a second link with different extras
// would silently reuse the wrong cached argv.
func hashExtraCFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}

	h := sha256.New()
	for _, f := range flags {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))[:16]
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

// probeOnce returns the per-key sync.Once used to coalesce concurrent
// probes for the same lookup key. The Once value is keyed by the full
// lldArgvKey so distinct (target, opt-level, ...) combos still probe
// in parallel; only redundant probes for the SAME key block.
var (
	probeOnceMap = map[lldArgvKey]*sync.Once{}
	probeOnceMu  sync.Mutex
)

func probeOnce(key lldArgvKey) *sync.Once {
	probeOnceMu.Lock()
	defer probeOnceMu.Unlock()

	if o, ok := probeOnceMap[key]; ok {
		return o
	}

	o := &sync.Once{}
	probeOnceMap[key] = o

	return o
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
	// Every keying field of lldArgvKey must appear in the filename, or
	// two builds that differ only in (e.g.) `static` will collide on
	// disk and the second build picks up the first's argv -- silently
	// linking dynamically when -static was requested or vice versa.
	return fmt.Sprintf("%s_%s_%s_%v_%v_%s_%v_%v_%v_%s.argv",
		clangVersion(), k.triple, k.optLevel, k.debug, k.macOS, k.ltoMode, k.useLld,
		k.static, k.standaloneDbg, k.extraHash)
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

	if opts.debug {
		args = append(args, "-g")
	}

	if opts.standaloneDebugMacOS && opts.targetGOOS == "darwin" {
		args = append(args, "-fstandalone-debug")
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

	// extraCFlags is the user's --cflag passthrough. Some entries
	// (e.g. -fsanitize=address) inject runtime libraries into the
	// link line, so they MUST be in the probe argv -- otherwise the
	// cached result would omit those libs and the link command tin
	// later issues would fail to find them.
	args = append(args, opts.extraCFlags...)

	// -static on Linux pulls libc.a / libpthread.a / libm.a / libdl.a
	// into the link line in place of their shared counterparts. macOS
	// doesn't support a fully static link (no static libSystem), so
	// the flag is silently dropped there -- the toolchain is already
	// "as static as possible" by default on Mach-O.
	if opts.static && opts.targetGOOS != "darwin" {
		args = append(args, "-static")
	}

	// Probe the link line WITH -lmimalloc so the cached argv prefix
	// already accounts for any extra search paths or runtime symbols
	// clang's driver expects.  Skipped when the lib isn't present
	// (linkBinary will surface the hard error before reaching here on
	// the real link path; we just need the probe to succeed for the
	// build-flag fingerprint).
	if opts.useMimalloc {
		if libDir, _, ok := findMimallocInstall(opts.targetGOOS); ok {
			args = append(args, "-L"+libDir, "-lmimalloc")
		}
	}

	args = append(args, tmpPath, "-o", dummyOutPath, "-###")
	out, _ := exec.Command("clang", args...).CombinedOutput()

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return &lldArgvEntry{err: fmt.Errorf("lld probe: empty clang -### output")}
	}

	last := lines[len(lines)-1]

	toks := splitShellLine(last)

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
// Inside a quoted run, only `\\` and `\"` are recognized as escape
// sequences (they unwrap to `\` and `"` respectively); a stray `\`
// followed by anything else stays literal. Outside quotes a `\\` quotes
// the next character verbatim.
func splitShellLine(line string) []string {
	var (
		toks []string
		buf  strings.Builder
		in   bool
	)

	for i := 0; i < len(line); i++ {
		c := line[i]

		// Escape sequences: handle them BEFORE the quote toggle so
		// `\"` inside a quoted span stays a literal quote rather than
		// closing the run.
		if c == '\\' && i+1 < len(line) {
			next := line[i+1]

			if in {
				if next == '"' || next == '\\' {
					i++

					buf.WriteByte(next)

					continue
				}
				// Unknown escape inside quotes: keep both bytes
				// literal (matches POSIX shell behavior for
				// double-quoted strings).
				buf.WriteByte(c)

				continue
			}

			// Outside quotes: unconditional escape of the next byte.
			i++

			buf.WriteByte(next)

			continue
		}

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

		buf.WriteByte(c)
	}

	if buf.Len() > 0 {
		toks = append(toks, buf.String())
	}

	return toks
}

// isClangTempForBase reports whether t looks like clang's renamed
// temp object for an input whose basename (without extension) equals
// `base`. Clang produces `/tmp/<base>-<hex>.o` under -flto=thin where
// the hex run is always at least six characters (`mkstemp`-style).
// We require the same minimum so a foreign `.o` named `dummy-1.o`
// can't be mistaken for the dummy.
const clangTempMinHex = 6

func isClangTempForBase(t, base string) bool {
	if !strings.HasSuffix(t, ".o") {
		return false
	}

	bt := filepath.Base(t)
	if !strings.HasPrefix(bt, base+"-") {
		return false
	}

	rest := strings.TrimSuffix(strings.TrimPrefix(bt, base+"-"), ".o")
	if len(rest) < clangTempMinHex {
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

// findMimallocInstall locates a libmimalloc shared library on the host.
// Returns (libDir, includeDir, ok).  Checks the standard Homebrew prefix
// on Darwin and the common Linux library paths.  Used to (1) reject the
// build loudly when --no-mimalloc was NOT passed but the library is
// missing, and (2) extend the link line with -L<libDir> -lmimalloc plus
// the runtime.c compile flags with -I<includeDir>.
//
// Caller is responsible for checking the linkOpts.useMimalloc gate
// before consulting this function.  We do not auto-disable on miss --
// the user opted in to mimalloc (default), so a missing lib is an
// installation error we should surface, not paper over.
func findMimallocInstall(targetGOOS string) (libDir, incDir string, ok bool) {
	var libCandidates []string

	var incCandidates []string

	if targetGOOS == "darwin" {
		// Apple Silicon Homebrew prefix; the older /usr/local layout
		// is a fallback for Intel Macs or non-standard Homebrew installs.
		libCandidates = []string{"/opt/homebrew/lib", "/usr/local/lib"}
		incCandidates = []string{"/opt/homebrew/include", "/usr/local/include"}
	} else {
		// Linux: Arch / Fedora / RHEL ship in /usr/lib64; Debian / Ubuntu
		// in /usr/lib/x86_64-linux-gnu; some distros use /usr/lib directly.
		libCandidates = []string{"/usr/lib64", "/usr/lib/x86_64-linux-gnu", "/usr/lib", "/usr/local/lib"}
		incCandidates = []string{"/usr/include", "/usr/local/include"}
	}

	libNames := []string{"libmimalloc.so", "libmimalloc.dylib", "libmimalloc.a"}

	for _, d := range libCandidates {
		for _, n := range libNames {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				libDir = d

				break
			}
		}

		if libDir != "" {
			break
		}
	}

	for _, d := range incCandidates {
		if _, err := os.Stat(filepath.Join(d, "mimalloc.h")); err == nil {
			incDir = d

			break
		}
	}

	ok = libDir != "" && incDir != ""

	return
}
