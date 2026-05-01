package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Azer0s/tin/ast"
)

// TestPureFnRoundTrip wires the full Phase C path end-to-end:
//  1. write a tiny IR module that exports a `__tin_pure_shim_cube` C-callable
//  2. compile it to .so via clang -shared -fPIC -O2
//  3. dlopen + dlsym + invoke via libffi using a synthetic FuncDecl
//  4. verify the returned i64 matches the function's native result
//
// Skipped when clang is unavailable on PATH (typical CI without LLVM toolchain).
func TestPureFnRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH; skipping libffi round-trip")
	}

	tmp := t.TempDir()
	llPath := filepath.Join(tmp, "fn.ll")
	soPath := filepath.Join(tmp, "fn.so")
	hash := "test_roundtrip_hash"

	const ir = `target triple = "x86_64-pc-linux-gnu"

define i64 @__tin_pure_shim_cube(i64 %n) {
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

	h, err := LoadPureFn(hash, "__tin_pure_shim_cube")
	if err != nil {
		t.Fatalf("LoadPureFn: %v", err)
	}

	fd := &ast.FuncDecl{
		Name:    "cube",
		Params:  []ast.Param{{Name: "n", Type: &ast.SimpleType{Name: "i64"}}},
		RetType: &ast.SimpleType{Name: "i64"},
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
		got, ok := InvokePureShim(h, fd, []ctfeVal{{kind: "i64", i: c.in}})
		if !ok {
			t.Errorf("InvokePureShim(cube, %d): not ok", c.in)

			continue
		}

		if got.kind != "i64" || got.i != c.want {
			t.Errorf("cube(%d) via dispatch = %+v; want i64 %d", c.in, got, c.want)
		}
	}
}

// TestPureFnRoundTripMultiArg drives a 3-arg shim - exercises libffi
// argument vector marshaling beyond a single value.
func TestPureFnRoundTripMultiArg(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH")
	}

	tmp := t.TempDir()
	hash := "test_multiarg_hash"

	const ir = `target triple = "x86_64-pc-linux-gnu"

define i64 @__tin_pure_shim_clamp(i64 %v, i64 %lo, i64 %hi) {
entry:
	%c1 = icmp slt i64 %v, %lo
	%pick1 = select i1 %c1, i64 %lo, i64 %v
	%c2 = icmp sgt i64 %pick1, %hi
	%pick2 = select i1 %c2, i64 %hi, i64 %pick1
	ret i64 %pick2
}
`
	stageSO(t, tmp, hash, ir)

	wd, _ := os.Getwd()

	defer func() { _ = os.Chdir(wd) }()

	_ = os.Chdir(tmp)

	h, err := LoadPureFn(hash, "__tin_pure_shim_clamp")
	if err != nil {
		t.Fatalf("LoadPureFn: %v", err)
	}

	fd := &ast.FuncDecl{
		Name: "clamp",
		Params: []ast.Param{
			{Name: "v", Type: &ast.SimpleType{Name: "i64"}},
			{Name: "lo", Type: &ast.SimpleType{Name: "i64"}},
			{Name: "hi", Type: &ast.SimpleType{Name: "i64"}},
		},
		RetType: &ast.SimpleType{Name: "i64"},
	}

	cases := []struct {
		v, lo, hi, want int64
	}{
		{5, 0, 10, 5},
		{-3, 0, 10, 0},
		{99, 0, 10, 10},
		{7, 7, 7, 7},
	}

	for _, c := range cases {
		got, ok := InvokePureShim(h, fd, []ctfeVal{
			{kind: "i64", i: c.v},
			{kind: "i64", i: c.lo},
			{kind: "i64", i: c.hi},
		})
		if !ok {
			t.Errorf("clamp(%d,%d,%d): not ok", c.v, c.lo, c.hi)

			continue
		}

		if got.kind != "i64" || got.i != c.want {
			t.Errorf("clamp(%d,%d,%d) = %+v; want i64 %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

// TestPureFnRoundTripFloat exercises the f64 path through libffi (return
// value stored as a double, not an integer).
func TestPureFnRoundTripFloat(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH")
	}

	tmp := t.TempDir()
	hash := "test_float_hash"

	const ir = `target triple = "x86_64-pc-linux-gnu"

define double @__tin_pure_shim_dscale(double %x, double %k) {
entry:
	%0 = fmul double %x, %k
	ret double %0
}
`
	stageSO(t, tmp, hash, ir)

	wd, _ := os.Getwd()

	defer func() { _ = os.Chdir(wd) }()

	_ = os.Chdir(tmp)

	h, err := LoadPureFn(hash, "__tin_pure_shim_dscale")
	if err != nil {
		t.Fatalf("LoadPureFn: %v", err)
	}

	fd := &ast.FuncDecl{
		Name: "dscale",
		Params: []ast.Param{
			{Name: "x", Type: &ast.SimpleType{Name: "f64"}},
			{Name: "k", Type: &ast.SimpleType{Name: "f64"}},
		},
		RetType: &ast.SimpleType{Name: "f64"},
	}

	got, ok := InvokePureShim(h, fd, []ctfeVal{
		{kind: "f64", f: 2.5},
		{kind: "f64", f: 4.0},
	})
	if !ok {
		t.Fatal("InvokePureShim: not ok")
	}

	if got.kind != "f64" || got.f != 10.0 {
		t.Errorf("dscale(2.5, 4.0) = %+v; want f64 10.0", got)
	}
}

// stageSO writes ir to a temp .ll, compiles it via clang, and stages the
// resulting .so under tmp/.build/pure-fn/<hash>/bin.so where LoadPureFn
// expects to find it.
func stageSO(t *testing.T, tmp, hash, ir string) {
	t.Helper()

	llPath := filepath.Join(tmp, hash+".ll")
	soPath := filepath.Join(tmp, hash+".so")

	if err := os.WriteFile(llPath, []byte(ir), 0o644); err != nil {
		t.Fatalf("write .ll: %v", err)
	}

	cmd := exec.Command("clang", "-shared", "-fPIC", "-O2", llPath, "-o", soPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang: %v\n%s", err, out)
	}

	cacheDir := filepath.Join(tmp, ".build/pure-fn", hash)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	data, err := os.ReadFile(soPath)
	if err != nil {
		t.Fatalf("read .so: %v", err)
	}

	if err := os.WriteFile(filepath.Join(cacheDir, "bin.so"), data, 0o755); err != nil {
		t.Fatalf("stage .so: %v", err)
	}
}
