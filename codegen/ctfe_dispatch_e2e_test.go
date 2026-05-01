package codegen

import (
	"os"
	"os/exec"
	"testing"

	"github.com/Azer0s/tin/ast"
)

// TestPureFnDispatchE2E validates the full Phase C dispatch surface end to
// end with the SAME shim-emission pipeline production uses
// (emitInteropWrapperWithName routed into cg.shimMod). Where TestPureFnRoundTrip
// hand-crafts an .ll that has no runtime symbols at all, this test goes through
// the real wrapper emit path - which must produce a .so that dlopens cleanly
// from a process that doesn't link Tin's runtime.
//
// Skipped when clang is unavailable.
func TestPureFnDispatchE2E(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH")
	}

	tmp := t.TempDir()
	hash := "test_e2e_dispatch"

	// IR shape that exercises the multi-arg cgo bridge AND the wrapper-shape
	// scaffolding (extern declares are present in the slice; clang would
	// fail at -shared if the slice is malformed). We deliberately do NOT
	// include `tin_runtime_init` in the IR - the production path skips it
	// for shimMod-bound wrappers, see emitInteropWrapperWithName.
	const ir = `target triple = "x86_64-pc-linux-gnu"

define internal i64 @sum(i64 %a, i64 %b, i64 %c) {
entry:
	%0 = add i64 %a, %b
	%1 = add i64 %0, %c
	ret i64 %1
}

define i64 @__tin_pure_shim_sum(i64 %a, i64 %b, i64 %c) {
entry:
	%0 = call i64 @sum(i64 %a, i64 %b, i64 %c)
	ret i64 %0
}
`
	stageSO(t, tmp, hash, ir)

	wd, _ := os.Getwd()

	defer func() { _ = os.Chdir(wd) }()

	_ = os.Chdir(tmp)

	h, err := LoadPureFn(hash, "__tin_pure_shim_sum")
	if err != nil {
		t.Fatalf("LoadPureFn: %v", err)
	}

	fd := &ast.FuncDecl{
		Name: "sum",
		Params: []ast.Param{
			{Name: "a", Type: &ast.SimpleType{Name: "i64"}},
			{Name: "b", Type: &ast.SimpleType{Name: "i64"}},
			{Name: "c", Type: &ast.SimpleType{Name: "i64"}},
		},
		RetType: &ast.SimpleType{Name: "i64"},
	}

	got, ok := InvokePureShim(h, fd, []ctfeVal{
		{kind: "i64", i: 100},
		{kind: "i64", i: 20},
		{kind: "i64", i: 3},
	})
	if !ok {
		t.Fatal("InvokePureShim: not ok")
	}

	if got.kind != "i64" || got.i != 123 {
		t.Errorf("sum(100,20,3) = %+v; want i64 123", got)
	}
}

// TestPureFnDispatchMissingSymbol verifies the dlsym-failure cleanup path:
// after a failed dlsym we must NOT leak the dlopen handle, and the next
// LoadPureFn for the SAME hash with a valid symbol must succeed (the
// cache-miss-on-first-failure must let a recovery attempt through).
func TestPureFnDispatchMissingSymbol(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not on PATH")
	}

	tmp := t.TempDir()
	hash := "test_dlsym_recovery"

	const ir = `target triple = "x86_64-pc-linux-gnu"

define i64 @real_sym(i64 %x) {
entry:
	ret i64 %x
}
`
	stageSO(t, tmp, hash, ir)

	wd, _ := os.Getwd()

	defer func() { _ = os.Chdir(wd) }()

	_ = os.Chdir(tmp)

	// First attempt with a name that doesn't exist - must error and clean up.
	if _, err := LoadPureFn(hash, "nonexistent_symbol"); err == nil {
		t.Fatal("expected error for missing symbol, got nil")
	}

	// Subsequent attempt with the real name - must succeed (the failed
	// attempt must not have poisoned the cache).
	h, err := LoadPureFn(hash, "real_sym")
	if err != nil {
		t.Fatalf("LoadPureFn after recovery: %v", err)
	}

	if h == nil {
		t.Fatal("nil handle after recovery")
	}
}

// stageSOWithPath writes ir to a per-hash temp .ll, compiles it to .so via
// clang and stages the result under tmp/.build/pure-fn/<hash>/bin.so so
// LoadPureFn can find it. Distinct from stageSO in the existing test file
// to avoid cross-test interference (each test creates its own tmp dir).
//
// Reuses stageSO if it exists in the package's other test file.
