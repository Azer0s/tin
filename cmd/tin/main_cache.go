package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/Azer0s/tin/codegen"
)

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
// cmd/tin/main.go can read/write the cache without importing internal codegen state.
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
		tempSo := pending[i].soPath + fmt.Sprintf(".tmp.%d.%d", os.Getpid(), atomic.AddUint64(&tempCacheCounter, 1))

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

	parStart := time.Now()

	var parCb func(string, string, time.Duration)

	if prog != nil {
		prog.parallelStart(len(jobsList))
		parCb = prog.parallelEvent
	}

	parErr := runParallelClang(jobsList, parCb)

	if prog != nil {
		prog.parallelEnd(time.Since(parStart))
	}

	if parErr != nil {
		return parErr
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

// clangVersion returns the first line of `clang --version` output.
// Mixed into pkg/csrc cache keys so a clang upgrade busts the cache.
// On probe failure returns a per-process sentinel (PID + start ns) so
// cache slots written under failure are never reused -- same
// defensive shape as tinBinaryHash.
//
// Backed by the disk-cached host-info record so subsequent tin
// invocations reuse the result without re-spawning clang.
func clangVersion() string {
	v := hostInfo().ClangVersion
	if v == "" {
		return fmt.Sprintf("UNAVAIL-pid%d-%d", os.Getpid(), time.Now().UnixNano())
	}

	return v
}

// pkgCacheRoot is the directory holding cached per-pkg .o files. Step 7
// of docs/plans/incremental-compilation.md: each pkg's IR text + its
// canonical clang argv produce a SHA-256 key under .build/pkg/<key>/pkg.o.
// On hit the clang -c invocation is skipped entirely. Unlike `.build/run`
// (whole-program-keyed), this caches PER pkg, so an edit to pkg A doesn't
// invalidate pkg B's .o. Adds one disk-I/O per pkg per build, in exchange
// for skipping clang -c on every cached pkg.
const pkgCacheRoot = ".build/pkg"

// pkgCacheLookup returns the cache path for compiling a per-pkg .ll
// content with the given args. Returns (path, hit, err): on hit, path
// already exists and the caller should add it to linkInputs and skip
// clang. On miss, the caller must produce the .o at path; the cache
// dir is pre-created.
//
// Key composition: SHA-256 of (irText || NUL-separated argv || host arch
// || clang version banner). Includes every clang flag so that an
// opt-level / target / debug change produces a fresh entry; includes
// the clang version so a clang upgrade (e.g. 22 -> 23 with codegen
// fixes) invalidates every cache slot rather than reusing .o produced
// by the older toolchain.
func pkgCacheLookup(irText string, args []string) (string, bool, error) {
	sum := sha256.New()
	sum.Write([]byte(irText))

	for _, a := range args {
		sum.Write([]byte{0})
		sum.Write([]byte(a))
	}

	// Include host arch in the key so an amd64 host and arm64 host (e.g.
	// running the same source tree via a docker --platform mount) don't
	// share the same .o cache slot. clangTargetFlag() returns nil for
	// native compiles, leaving the args identical across archs - without
	// this disambiguator, the second host would link the first host's .o
	// and ld would error on the architecture mismatch.
	sum.Write([]byte{0})
	sum.Write([]byte(runtime.GOOS + "-" + runtime.GOARCH))

	// Include the clang version banner so a toolchain upgrade busts
	// every prior cache slot. Without this, a clang 22 -> 23 upgrade
	// silently reuses .o files built by clang 22, missing any codegen
	// fixes the new clang would apply.
	sum.Write([]byte{0})
	sum.Write([]byte(clangVersion()))

	key := hex.EncodeToString(sum.Sum(nil))
	dir := filepath.Join(pkgCacheRoot, key[:2])

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("pkg cache: %w", err)
	}

	objPath := filepath.Join(dir, key+".o")
	if info, err := os.Stat(objPath); err == nil && !info.IsDir() && info.Size() > 0 {
		return objPath, true, nil
	}

	return objPath, false, nil
}

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

	// See pkgCacheLookup for rationale: native compiles omit -target so
	// amd64 and arm64 hosts produce identical args. Disambiguate by host
	// arch and clang version banner.
	sum.Write([]byte{0})
	sum.Write([]byte(runtime.GOOS + "-" + runtime.GOARCH))
	sum.Write([]byte{0})
	sum.Write([]byte(clangVersion()))

	// Include the tin binary identity so a tin rebuild (which can
	// change the IR/runtime contract -- e.g. new emitted symbols
	// arc.c didn't yet define) invalidates stale cached .o files.
	// `runtime.c` itself rarely changes by content, but its #includes
	// (arc.c, fiber.c, ...) do; keying off the running tin binary
	// catches every such transitive change in one stamp.
	sum.Write([]byte{0})
	sum.Write([]byte(tinBinaryHash()))

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

// compileJob describes a single compile invocation that runParallelClang
// can fan out. desc is shown to the user via progress / error messages.
//
// args is the canonical clang argv. It's used for cache-key generation
// even when the actual execution goes through a different backend (the
// argv shape is what the cache lookup hashes). When runFn is nil the
// worker runs `clang args...` directly; when runFn is set the worker
// invokes it instead -- that's how the lld backend supplies its own
// `opt` invocation while preserving the cache-key contract.
//
// renameTo (when non-empty) tells the runner to atomically rename the
// just-produced output to that path on success, so concurrent tin
// processes can't half-write a shared cache entry.
