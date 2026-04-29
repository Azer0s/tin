package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPureFnRoundTrip wires the full Phase C path end-to-end:
//  1. write a tiny IR module that exports `cube` with the native C ABI
//  2. compile it to .so via clang -shared -fPIC -O2
//  3. dlopen + dlsym + invoke through the shape-specific cgo bridge
//  4. verify the returned i64 matches what the function would compute natively
//
// Skipped when clang is unavailable on PATH (typical CI without LLVM toolchain).
func TestPureFnRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH; skipping cgo round-trip")
	}

	tmp := t.TempDir()
	llPath := filepath.Join(tmp, "fn.ll")
	soPath := filepath.Join(tmp, "fn.so")
	hash := "test_roundtrip_hash"

	const ir = `target triple = "x86_64-pc-linux-gnu"

define i64 @cube(i64 %n) {
entry:
	%0 = mul i64 %n, %n
	%1 = mul i64 %0, %n
	ret i64 %1
}
`

	if err := os.WriteFile(llPath, []byte(ir), 0o644); err != nil {
		t.Fatalf("write .ll: %v", err)
	}

	cmd := exec.Command("clang", "-shared", "-fPIC", "-O2", llPath, "-o", soPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang: %v\n%s", err, out)
	}

	// LoadPureFn looks up .build/pure-fn/<hash>/bin.so under the working
	// directory; chdir to tmp and stage the .so at the expected path.
	cacheDir := filepath.Join(tmp, ".build/pure-fn", hash)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dst := filepath.Join(cacheDir, "bin.so")
	if data, err := os.ReadFile(soPath); err == nil {
		_ = os.WriteFile(dst, data, 0o755)
	} else {
		t.Fatalf("read .so: %v", err)
	}

	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()

	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	h, err := LoadPureFn(hash, "cube")
	if err != nil {
		t.Fatalf("LoadPureFn: %v", err)
	}

	cases := []struct {
		in, want int64
	}{
		{0, 0},
		{1, 1},
		{2, 8},
		{7, 343},
		{10, 1000},
	}

	for _, c := range cases {
		got, err := InvokePureFn(h, []int64{c.in})
		if err != nil {
			t.Errorf("InvokePureFn(cube, %d): %v", c.in, err)
			continue
		}

		if got != c.want {
			t.Errorf("cube(%d) via dispatch = %d; want %d", c.in, got, c.want)
		}
	}
}
