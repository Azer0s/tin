package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// newCodeGenForPkgModTest constructs a minimum-viable CodeGen that has
// just enough of cg.mod populated for the per-pkg helpers to function.
// Avoids the full Generate() pipeline so the test stays unit-scoped.
func newCodeGenForPkgModTest() *CodeGen {
	cg := &CodeGen{}
	cg.mod = ir.NewModule()
	cg.mod.TargetTriple = "x86_64-pc-linux-gnu"
	cg.mod.DataLayout = "e-m:e-i64:64-f80:128-n8:16:32:64-S128"

	return cg
}

func TestPkgMod_LazyCreatePerName(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	a1 := cg.pkgMod("io")
	a2 := cg.pkgMod("io")
	b := cg.pkgMod("net")

	if a1 == nil || b == nil {
		t.Fatalf("pkgMod returned nil for non-empty name")
	}

	if a1 != a2 {
		t.Errorf("pkgMod(\"io\") returned different modules across calls")
	}

	if a1 == b {
		t.Errorf("pkgMod(\"io\") and pkgMod(\"net\") returned the same module")
	}

	if a1.TargetTriple != cg.mod.TargetTriple {
		t.Errorf("pkg module triple = %q, want %q", a1.TargetTriple, cg.mod.TargetTriple)
	}

	if a1.DataLayout != cg.mod.DataLayout {
		t.Errorf("pkg module datalayout differs from cg.mod")
	}
}

func TestPkgMod_PanicsOnEmptyName(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("pkgMod(\"\") did not panic")
		}

		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "empty package name") {
			t.Errorf("panic message = %v, want text mentioning empty package name", r)
		}
	}()

	cg.pkgMod("")
}

func TestPkgMod_EntrySentinelAccepted(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	if EntryPkgName == "" {
		t.Fatalf("EntryPkgName must be a non-empty sentinel")
	}

	m := cg.pkgMod(EntryPkgName)
	if m == nil {
		t.Fatalf("pkgMod(EntryPkgName) returned nil")
	}
}

func TestEchoTypeInActive_NoOpWhenActiveIsCgMod(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	st := irtypes.NewStruct(irtypes.I32)
	st.SetName("MyStruct")

	got := cg.echoTypeInActive(st)
	if got != st {
		t.Errorf("echoTypeInActive must return the input type unchanged")
	}

	if len(cg.mod.TypeDefs) != 0 {
		t.Errorf("echoTypeInActive into cg.mod must not append TypeDefs (got %d)", len(cg.mod.TypeDefs))
	}
}

func TestEchoTypeInActive_AppendsToForeignActiveModule(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	st := irtypes.NewStruct(irtypes.I32, irtypes.I8)
	st.SetName("Foo")

	cg.activeMod = cg.pkgMod("io")

	defer func() { cg.activeMod = nil }()

	cg.echoTypeInActive(st)
	cg.echoTypeInActive(st)

	pkg := cg.pkgMods["io"]
	if len(pkg.TypeDefs) != 1 {
		t.Errorf("expected 1 typedef in foreign module after idempotent calls, got %d", len(pkg.TypeDefs))
	}

	if pkg.TypeDefs[0].Name() != "Foo" {
		t.Errorf("expected echoed typedef name %q, got %q", "Foo", pkg.TypeDefs[0].Name())
	}
}

func TestEchoTypeInActive_SkipsAnonymousAndNonStruct(t *testing.T) {
	cg := newCodeGenForPkgModTest()
	cg.activeMod = cg.pkgMod("io")

	defer func() { cg.activeMod = nil }()

	anon := irtypes.NewStruct(irtypes.I64)
	cg.echoTypeInActive(anon)
	cg.echoTypeInActive(irtypes.I32)

	if len(cg.pkgMods["io"].TypeDefs) != 0 {
		t.Errorf("anonymous and non-struct types must not produce typedefs (got %d)", len(cg.pkgMods["io"].TypeDefs))
	}
}

func TestPkgModNames_Sorted(t *testing.T) {
	cg := newCodeGenForPkgModTest()
	cg.pkgMod("net")
	cg.pkgMod("io")
	cg.pkgMod("encoding")

	names := cg.pkgModNames()
	want := []string{"encoding", "io", "net"}

	if len(names) != len(want) {
		t.Fatalf("pkgModNames len = %d, want %d", len(names), len(want))
	}

	for i, n := range want {
		if names[i] != n {
			t.Errorf("pkgModNames[%d] = %q, want %q", i, names[i], n)
		}
	}
}

func TestDebugPkgMods_EmptyAndPopulated(t *testing.T) {
	cg := newCodeGenForPkgModTest()

	if got := cg.debugPkgMods(); !strings.Contains(got, "no per-pkg") {
		t.Errorf("debugPkgMods() empty = %q, want text mentioning no per-pkg", got)
	}

	cg.pkgMod("io")

	if got := cg.debugPkgMods(); !strings.Contains(got, "io:") {
		t.Errorf("debugPkgMods() populated = %q, want mention of \"io:\"", got)
	}
}
