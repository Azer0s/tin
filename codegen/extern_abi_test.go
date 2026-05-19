package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// TestExternABIShape pins the IR shape ensureExternDecl emits for an
// extern with a >16-byte struct parameter and a >16-byte struct return.
// LLVM doesn't enforce caller/callee param-attribute consistency at IR
// link time, so a regression where the declaration's byval/sret falls
// out of sync with what cg.callExtern emits at the call site corrupts
// the parameter silently on Linux x86_64 (clang 18 expects byval) or
// AAPCS64 (composite-by-reference) -- this is what cratered every
// panic/recover/stacktrace test before the shim refactor.  Locking
// the shape down means a future refactor that drops byval/sret on
// either side fails the test immediately on the host arch.
//
// Covers both x86_64 and arm64 explicitly: each ABI has its own attr
// set (`byval` on x86_64, bare-pointer-with-noundef on arm64) and a
// regression on the not-currently-running arch would otherwise only
// show up in CI.
func TestExternABIShape(t *testing.T) {
	for _, tt := range []struct {
		name           string
		triple         string
		wantDecl       []string
		wantCallByval  []string
		wantCallSret   []string
		notWantDecl    []string
		notWantCall    []string
	}{
		{
			name:   "x86_64",
			triple: "x86_64-pc-linux-gnu",
			wantDecl: []string{
				"noundef byval({ i8*, i64, i64 })",
				"sret({ i8*, i64, i64 }) align 8",
			},
			wantCallByval: []string{
				"noundef byval({ i8*, i64, i64 }) align 8",
			},
			wantCallSret: []string{
				"sret({ i8*, i64, i64 }) align 8",
			},
		},
		{
			name:   "arm64-darwin",
			triple: "arm64-apple-macosx",
			wantDecl: []string{
				"noundef align 8",
				"sret({ i8*, i64, i64 }) align 8",
			},
			wantCallByval: []string{
				"noundef align 8",
			},
			wantCallSret: []string{
				"sret({ i8*, i64, i64 }) align 8",
			},
			notWantDecl: []string{"byval"},
			notWantCall: []string{"byval"},
		},
		{
			name:   "aarch64-linux",
			triple: "aarch64-unknown-linux-gnu",
			wantDecl: []string{
				"noundef align 8",
				"sret({ i8*, i64, i64 }) align 8",
			},
			notWantDecl: []string{"byval"},
			notWantCall: []string{"byval"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cg := &CodeGen{
				mod:           ir.NewModule(),
				externIRNames: map[string]bool{},
			}
			cg.mod.TargetTriple = tt.triple

			// Declare an extern over a 24-byte struct param + return,
			// matching the TinString / TinSlice / TinAtomArray shape.
			fatStruct := stringFatPtrType()
			fn := cg.ensureExternDecl(
				"test_extern",
				fatStruct,
				[]*ir.Param{ir.NewParam("desc", fatStruct)},
				false,
			)

			ctor := cg.mod.NewFunc("test_caller", irtypes.Void)
			block := ctor.NewBlock("entry")

			// Build a struct value to pass.
			zero := block.NewAlloca(fatStruct)
			val := block.NewLoad(fatStruct, zero)
			cg.callExtern(block, fn, val)
			block.NewRet(nil)

			modIR := cg.mod.String()

			declLine := lineContaining(modIR, "declare void @test_extern")
			callLine := lineContaining(modIR, "call void @test_extern")

			if declLine == "" {
				t.Fatalf("declare line for test_extern not found in:\n%s", modIR)
			}

			if callLine == "" {
				t.Fatalf("call line for test_extern not found in:\n%s", modIR)
			}

			for _, want := range tt.wantDecl {
				if !strings.Contains(declLine, want) {
					t.Errorf("declare line missing %q\nactual: %s", want, declLine)
				}
			}

			for _, want := range tt.wantCallByval {
				if !strings.Contains(callLine, want) {
					t.Errorf("call line missing byval attr %q\nactual: %s", want, callLine)
				}
			}

			for _, want := range tt.wantCallSret {
				if !strings.Contains(callLine, want) {
					t.Errorf("call line missing sret attr %q\nactual: %s", want, callLine)
				}
			}

			for _, no := range tt.notWantDecl {
				if strings.Contains(declLine, no) {
					t.Errorf("declare line wrongly contains %q\nactual: %s", no, declLine)
				}
			}

			for _, no := range tt.notWantCall {
				if strings.Contains(callLine, no) {
					t.Errorf("call line wrongly contains %q\nactual: %s", no, callLine)
				}
			}
		})
	}
}

// TestExternNoLoweringPassesThrough verifies that an extern whose
// signature fits in registers (no >16-byte struct param or return)
// does NOT get any byval/sret attributes -- this is the fast-path
// `cg.callExtern` falls through to plain `NewCall` for.
func TestExternNoLoweringPassesThrough(t *testing.T) {
	cg := &CodeGen{
		mod:           ir.NewModule(),
		externIRNames: map[string]bool{},
	}
	cg.mod.TargetTriple = "x86_64-pc-linux-gnu"

	fn := cg.ensureExternDecl(
		"small_extern",
		irtypes.I32,
		[]*ir.Param{ir.NewParam("x", irtypes.I64)},
		false,
	)

	if abi, ok := cg.externABIs[fn]; ok {
		t.Fatalf("small_extern wrongly recorded in externABIs: %+v", abi)
	}

	modIR := cg.mod.String()
	if strings.Contains(modIR, "byval") || strings.Contains(modIR, "sret") {
		t.Fatalf("small extern leaked byval/sret attrs:\n%s", modIR)
	}
}

func lineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}

	return ""
}
