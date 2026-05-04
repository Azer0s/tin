package main

// Host-toolchain info cache.
//
// Every tin invocation needs a few facts about the host clang/LLVM
// install: the version banner (used as a cache-key suffix), the LLVM
// target triple (baked into emitted IR modules), and the canonical
// linker argv prefix/suffix (used by the lld backend). Without
// caching, each fact costs one ~10-25 ms clang spawn per `tin`
// invocation -- on small programs that's a measurable fraction of the
// build.
//
// We cache the lot under `.build/host-info/<key>.json`, keyed by clang
// binary identity (path + size + mtime). If the binary moves or
// upgrades, the cached record is rejected and we re-probe.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const hostInfoCacheDir = ".build/host-info"

type hostToolchainInfo struct {
	// Identity stamp -- compared on read against the live clang binary
	// so an upgrade in-place busts the cache.
	ClangPath  string `json:"clang_path"`
	ClangSize  int64  `json:"clang_size"`
	ClangMtime int64  `json:"clang_mtime_unix_ns"`

	// Cached probe results.
	ClangVersion      string `json:"clang_version"` // first line of `clang --version`
	ClangMajorVersion int    `json:"clang_major"`   // parsed major number
	TargetTriple      string `json:"target_triple"` // from `clang -x c - -S -emit-llvm -o -`
	HostOS            string `json:"host_os"`
	HostArch          string `json:"host_arch"`
}

var (
	hostInfoCache *hostToolchainInfo
	hostInfoOnce  sync.Once
)

// hostInfo returns the cached host-toolchain probe results, running
// the underlying clang invocations (and writing the result to disk)
// only on first miss. Subsequent process startups load the JSON in a
// single open()+read().
//
// Returns a zero-value struct on probe failure so callers see "" for
// version / triple and can decide whether to keep going (most do --
// the version is only used as a cache-key suffix).
func hostInfo() *hostToolchainInfo {
	hostInfoOnce.Do(func() {
		hostInfoCache = loadOrProbeHostInfo()
	})

	return hostInfoCache
}

func loadOrProbeHostInfo() *hostToolchainInfo {
	clangPath, _ := exec.LookPath("clang")

	var (
		size  int64
		mtime int64
	)

	if clangPath != "" {
		if st, err := os.Stat(clangPath); err == nil {
			size = st.Size()
			mtime = st.ModTime().UnixNano()
		}
	}

	if disk := readHostInfoFromDisk(clangPath, size, mtime); disk != nil {
		return disk
	}

	info := &hostToolchainInfo{
		ClangPath:  clangPath,
		ClangSize:  size,
		ClangMtime: mtime,
		HostOS:     runtime.GOOS,
		HostArch:   runtime.GOARCH,
	}

	probeClangVersionInto(info)
	probeTargetTripleInto(info)

	writeHostInfoToDisk(info)

	return info
}

// readHostInfoFromDisk returns the cached record iff its binary
// identity matches the live clang. On any mismatch, IO error, or
// JSON parse failure, returns nil and the caller re-probes.
func readHostInfoFromDisk(clangPath string, size, mtime int64) *hostToolchainInfo {
	if clangPath == "" {
		return nil
	}

	path := filepath.Join(hostInfoCacheDir, "clang.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var rec hostToolchainInfo
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}

	if rec.ClangPath != clangPath || rec.ClangSize != size || rec.ClangMtime != mtime {
		return nil
	}

	if rec.HostOS != runtime.GOOS || rec.HostArch != runtime.GOARCH {
		return nil
	}

	return &rec
}

func writeHostInfoToDisk(info *hostToolchainInfo) {
	if err := os.MkdirAll(hostInfoCacheDir, 0o755); err != nil {
		return
	}

	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	// Atomic write so a concurrent reader never sees a half-written
	// file. Doesn't matter much in practice (the worst case is a
	// re-probe), but the cost is negligible.
	tmp := filepath.Join(hostInfoCacheDir, fmt.Sprintf("clang.json.tmp.%d.%d", os.Getpid(), time.Now().UnixNano()))

	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}

	final := filepath.Join(hostInfoCacheDir, "clang.json")
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
	}
}

// probeClangVersionInto runs `clang --version` and parses the first
// line + major version into info.
func probeClangVersionInto(info *hostToolchainInfo) {
	out, err := exec.Command("clang", "--version").Output()
	if err != nil {
		return
	}

	s := string(out)

	first := s
	if idx := indexByte(s, '\n'); idx >= 0 {
		first = s[:idx]
	}

	info.ClangVersion = trimSpace(first)

	// Parse "version <N>" then read the leading decimal run.
	if idx := indexSubstr(s, "version "); idx >= 0 {
		rest := s[idx+len("version "):]
		major := 0

		for _, c := range rest {
			if c >= '0' && c <= '9' {
				major = major*10 + int(c-'0')
			} else {
				break
			}
		}

		info.ClangMajorVersion = major
	}
}

// probeTargetTripleInto compiles a trivial empty C TU to LLVM IR and
// extracts the `target triple = "..."` line clang emits. This is the
// only reliable way to get the normalized macosx-style triple (rather
// than the darwin-style one from -dumpmachine).
func probeTargetTripleInto(info *hostToolchainInfo) {
	out, err := exec.Command("clang", "-x", "c", "-", "-S", "-emit-llvm", "-o", "-").Output()
	if err != nil {
		// Best-effort fallback by GOOS/GOARCH; matches the legacy
		// behavior in codegen.detectTargetTriple.
		switch runtime.GOOS + "/" + runtime.GOARCH {
		case "linux/amd64":
			info.TargetTriple = "x86_64-pc-linux-gnu"
		case "linux/arm64":
			info.TargetTriple = "aarch64-unknown-linux-gnu"
		case "darwin/amd64":
			info.TargetTriple = "x86_64-apple-macosx11.0.0"
		case "darwin/arm64":
			info.TargetTriple = "arm64-apple-macosx11.0.0"
		default:
			info.TargetTriple = "x86_64-pc-linux-gnu"
		}

		return
	}

	const prefix = `target triple = "`

	for _, line := range splitLines(string(out)) {
		if hasPrefix(line, prefix) {
			tr := line[len(prefix):]
			if idx := indexByte(tr, '"'); idx >= 0 {
				tr = tr[:idx]
			}

			if tr != "" {
				info.TargetTriple = tr

				return
			}
		}
	}
}

// Tiny string helpers kept local so this file doesn't need a strings
// import (avoids accidentally drifting back to the heavyweight stdlib
// API for one-line operations).
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}

	return -1
}

func indexSubstr(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}

	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}

	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}

		s = s[:len(s)-1]
	}

	return s
}

func splitLines(s string) []string {
	var out []string

	start := 0

	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		out = append(out, s[start:])
	}

	return out
}
