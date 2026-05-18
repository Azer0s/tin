package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Azer0s/tin/codegen"
)

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

	msg := fmt.Sprintf(format, args...)

	// The legacy form was `tin: <format>` -- e.g. `tin: codegen error:
	// foo.tin:5:3: undefined identifier`. The snippet renderer parses
	// `file:line:col: ...` into a Rust-style block, so strip the
	// repeated `tin:` prefix before handing it over and re-emit only
	// the prefix when no snippet pattern matched (= raw render).
	rendered := codegen.RenderDiagnostic(stripTinPrefix(msg))
	if rendered == stripTinPrefix(msg) {
		_, _ = fmt.Fprintf(os.Stderr, "tin: %s\n", msg)
	} else {
		_, _ = fmt.Fprintln(os.Stderr, rendered)
	}

	os.Exit(1)
}

// stripTinPrefix removes the legacy "tin: <kind> error: " preamble so
// the snippet renderer sees the bare `file:line:col: ...` shape it
// expects. Returns the input unchanged when no preamble is present.
func stripTinPrefix(s string) string {
	for _, p := range []string{
		"codegen error: ",
		"parse error: ",
		"compile error: ",
		"link error: ",
	} {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}

	return s
}

// defaultBuildOutPath mirrors the implicit `tin build` output naming:
// `foo/bar.tin` -> `foo/bar`. In --lib mode the suffix depends on the
// link configuration: `bar.o` with -static (single relocatable object
// produced by `ld -r`), `bar.so` on Linux, `bar.dylib` on macOS.
func defaultBuildOutPath(file string, libMode bool) string {
	out := strings.TrimSuffix(file, filepath.Ext(file))
	if libMode {
		out += libOutSuffix()
	}

	return out
}

// libOutSuffix returns the platform-appropriate library suffix for the
// current build configuration. -static produces a relocatable object;
// without -static, we emit a shared library (`.so` on ELF targets,
// `.dylib` on Mach-O).
func libOutSuffix() string {
	if staticLink {
		return ".o"
	}

	if targetGOOS == "darwin" {
		return ".dylib"
	}

	return ".so"
}

// lookupOArg scans the trailing argv (everything after the source-file
// arg index) for `-o PATH` and returns PATH, or "" if absent. Used by
// the build-cache pre-check to know the user's output path before the
// main build switch parses it itself.
func lookupOArg(fileArgIdx int) string {
	for i := fileArgIdx + 1; i < len(os.Args); i++ {
		if os.Args[i] == "-o" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}

	return ""
}

// copyAndChmodExec copies src to dst and marks dst executable. Used by
// the `tin build` cache hit path to materialize the cached binary at
// the user's chosen -o path. The explicit Chmod handles the case where
// dst already exists at non-exec perms - os.WriteFile honors the mode
// arg only when CREATING a new file, so without the follow-up Chmod a
// stale 0644 dst from an earlier run would silently keep its perms.
func copyAndChmodExec(src, dst string) error {
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	if err := os.WriteFile(dst, body, 0o755); err != nil {
		return err
	}

	return os.Chmod(dst, 0o755)
}

// cacheBinDir returns ".build/<mode>/<dunder>_<srcmd5>_<flagshash>" under
// CWD, where <dunder> is the cleaned source path with `/` replaced by
// `__`, <srcmd5> hashes the source bytes, and <flagshash> hashes the
// invocation-time settings that change the produced binary (opt level,
// debug, target triple, extra cflags). Mode is "run", "test", or "build".
//
// The cache dir is the lookup key. Inside it lives `bin` (the compiled
// binary) and `sbom.txt` (an SBOM listing every dep file with its MD5 -
// see writeBuildSBOM / sbomMatches). Without the flagshash, an `-O0`
// rebuild would silently reuse an `-O2` cached binary.
func cacheBinDir(mode, file string, src []byte) string {
	cleaned := filepath.ToSlash(filepath.Clean(file))
	cleaned = strings.TrimPrefix(cleaned, "/")
	dunder := strings.ReplaceAll(cleaned, "/", "__")

	sum := md5.Sum(src)

	return filepath.Join(".build", mode, fmt.Sprintf("%s_%s_%s", dunder, hex.EncodeToString(sum[:]), buildFlagsHash()))
}

// buildFlagsHash hashes the invocation-time settings that change the
// produced binary's identity but aren't part of the source bytes.
// Memoized at first call (per process). Inputs:
//   - optLevel (e.g. "-O0", "-O2")
//   - debugBuild (-g)
//   - target os/arch
//   - extraCFlags (--cflag forwards)
//
// Returned as 8 hex chars for a short, stable suffix on the cache path.
var (
	buildFlagsHashCache string
	buildFlagsHashOnce  sync.Once
)

func buildFlagsHash() string {
	buildFlagsHashOnce.Do(func() {
		h := md5.New()
		h.Write([]byte(optLevelOverride))
		h.Write([]byte{0})

		if debugBuild {
			h.Write([]byte{1})
		}

		h.Write([]byte{0})

		if staticLink {
			h.Write([]byte{1})
		}

		h.Write([]byte{0})
		h.Write([]byte(targetGOOS))
		h.Write([]byte{0})
		h.Write([]byte(targetGOARCH))

		for _, f := range extraCFlags {
			h.Write([]byte{0})
			h.Write([]byte(f))
		}

		// Sysroot / SDK overrides change linkage and header search
		// paths, so they must invalidate the cache. Includes the
		// resolved path (auto-detected or explicit), not just the
		// override flag, so a TIN_MACOS_SDK env-var swap is also
		// detected.
		h.Write([]byte{0})
		h.Write([]byte(macosSDKPath()))
		h.Write([]byte{0})
		h.Write([]byte(linuxSysrootPath()))

		sum := h.Sum(nil)
		buildFlagsHashCache = hex.EncodeToString(sum[:4])
	})

	return buildFlagsHashCache
}

// sbomBinaryMarker is a sentinel "path" recorded in sbom.txt for the
// compiler binary itself. sbomMatches recognizes it and compares against
// tinBinaryHash() instead of trying to open the literal name as a file.
// Without this, a fresh tin binary with a codegen fix would silently
// reuse cached binaries built by the buggy compiler - the symptom that
// surfaced in the u64-mod fix audit.
const sbomBinaryMarker = "__tin_binary__"

var (
	tinBinaryHashCache string
	tinBinaryHashOnce  sync.Once
)

// tinBinaryHash returns the hex MD5 of the running compiler binary,
// memoized for the process lifetime. Used by writeBuildSBOM and
// sbomMatches to invalidate run/test cache entries when the compiler
// itself has changed.
//
// On unrecoverable error (no executable path, can't read it) returns a
// per-process sentinel that includes the PID and start time; the value
// changes every invocation so a cache slot written under a failed-hash
// build is never silently reused under another. A warning is printed.
func tinBinaryHash() string {
	tinBinaryHashOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			tinBinaryHashCache = fmt.Sprintf("UNAVAIL-pid%d-%d", os.Getpid(), time.Now().UnixNano())

			fmt.Fprintf(os.Stderr, "warning: tin: can't determine compiler executable for cache key (%v); cache invalidation on compiler change disabled, every build will miss\n", err)

			return
		}

		body, err := os.ReadFile(exe)
		if err != nil {
			tinBinaryHashCache = fmt.Sprintf("UNAVAIL-pid%d-%d", os.Getpid(), time.Now().UnixNano())

			fmt.Fprintf(os.Stderr, "warning: tin: can't read compiler binary %s for cache key (%v); cache invalidation on compiler change disabled, every build will miss\n", exe, err)

			return
		}

		sum := md5.Sum(body)
		tinBinaryHashCache = hex.EncodeToString(sum[:])
	})

	return tinBinaryHashCache
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

		if parts[1] == sbomBinaryMarker {
			if tinBinaryHash() != parts[0] {
				return false
			}

			continue
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
//
// The first line is a synthetic entry for the compiler binary
// (sbomBinaryMarker) so a rebuilt tin invalidates every cache entry it
// would otherwise have reused.
func writeBuildSBOM(cacheDir, entryFile string, entrySrc []byte, depPaths []string) error {
	seen := map[string]bool{entryFile: true}

	var sb strings.Builder

	if h := tinBinaryHash(); h != "" {
		fmt.Fprintf(&sb, "%s  %s\n", h, sbomBinaryMarker)
	}

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

// cleanStaleCacheEntries removes every subdirectory of .build/<mode>/
// whose name starts with "<dunder>_<srcmd5>_" -- those are stale
// flagshash slots for THIS source-content. Slots for other content
// MD5s are kept so unrelated entries don't get nuked. Slots for the
// SAME flagshash are also kept so toggling -O0/-O2/-O0 doesn't force
// a cold rebuild every time.
//
// Called before recreating the fresh cache dir on a miss so old
// binaries from prior builds (different content, same source path)
// don't pile up.
func cleanStaleCacheEntries(mode, file string) {
	src, err := os.ReadFile(file)
	if err != nil {
		return
	}

	cleaned := filepath.ToSlash(filepath.Clean(file))
	cleaned = strings.TrimPrefix(cleaned, "/")
	dunder := strings.ReplaceAll(cleaned, "/", "__")

	base := filepath.Join(".build", mode)

	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}

	srcSum := md5.Sum(src)
	srcHex := hex.EncodeToString(srcSum[:])
	keepBase := fmt.Sprintf("%s_%s_%s", dunder, srcHex, buildFlagsHash())

	// Drop slots for the same source path that have a DIFFERENT source
	// content MD5 (they came from prior file contents and will never be
	// reused). Keep flagshash siblings of the current source content so
	// users can toggle -O0/-O2 without paying cold-rebuild cost each time.
	prefix := dunder + "_"
	keepContentPrefix := dunder + "_" + srcHex + "_"

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		if name == keepBase || strings.HasPrefix(name, keepContentPrefix) {
			continue
		}

		_ = os.RemoveAll(filepath.Join(base, name))
	}
}

// buildDeps gathers the dep file paths that should appear in the SBOM,
// drawing from imported package source files and the //!+file.c C sources
// pulled in for this build. The entry file is added separately by
// writeBuildSBOM.
