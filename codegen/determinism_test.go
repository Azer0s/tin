package codegen

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestIRDeterminism guards against codegen non-determinism by compiling
// a representative set of source files to LLVM IR twice in fresh
// subprocesses (so Go map iteration randomization fires anew each run)
// and asserting the output is byte-identical.
//
// The mono cache (incremental-compilation Phase 5 / decision D2) is
// content-addressed, so any non-deterministic IR emission would silently
// blow the cache hit rate. Lock the property in here.
//
// Skipped if the tin binary isn't present at the repo root - go test ./...
// runs in CI where main.go has been built into ./tin already.
func TestIRDeterminism(t *testing.T) {
	repoRoot := findRepoRoot(t)

	tinBin := filepath.Join(repoRoot, "tin")
	if !fileExists(tinBin) {
		t.Skipf("tin binary not found at %s; build with `go build -o tin .` first", tinBin)
	}

	// One file per category of codegen path that has historically harbored
	// non-determinism: scope release order, reflection table iteration,
	// struct method dispatch, async/coro transforms, generics, macros.
	cases := []string{
		"examples/sourcepos.tin",
		"examples/reflection.tin",
		"examples/map_filter_reduce.tin",
		"examples/echo_server/echo_server.tin",
		"examples/await_match.tin",
		"examples/macros_ctfe.tin",
		"examples/stacktrace_filter_pipe.tin",
		"examples/stress_tests/union_type_stress.tin",
		"examples/clayout_fiber.tin",
		"examples/test_http_client.tin",
	}

	for _, rel := range cases {
		rel := rel

		t.Run(rel, func(t *testing.T) {
			path := filepath.Join(repoRoot, rel)
			if !fileExists(path) {
				t.Skipf("missing fixture: %s", path)
			}

			a := runIR(t, tinBin, path)
			b := runIR(t, tinBin, path)

			if !bytes.Equal(a, b) {
				// Truncate the diff snapshot so a flake doesn't dump megabytes.
				const maxDump = 4096

				snip := func(buf []byte) string {
					if len(buf) <= maxDump {
						return string(buf)
					}

					return string(buf[:maxDump]) + "\n... (truncated)"
				}

				t.Errorf("IR not byte-identical across two runs of %s\n--- run A (first %d bytes) ---\n%s\n--- run B (first %d bytes) ---\n%s",
					rel, maxDump, snip(a), maxDump, snip(b))
			}
		})
	}
}

func runIR(t *testing.T, tinBin, src string) []byte {
	t.Helper()

	cmd := exec.Command(tinBin, "ir", src)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("`tin ir %s` failed: %v", src, err)
	}

	return out
}

func fileExists(p string) bool {
	cmd := exec.Command("test", "-f", p)

	if runtime.GOOS == "windows" {
		// "test" isn't on PATH; fall back to a stat via os in real impl.
		// Skipping windows for now - the codegen test suite isn't run there.
		return true
	}

	return cmd.Run() == nil
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v", err)
	}

	return string(bytes.TrimSpace(out))
}
