package codegen

import (
	"strings"
	"testing"

	"github.com/Azer0s/tin/ast"
)

// TestCtfeFnFingerprintStable: identical AST shapes hash to identical text.
func TestCtfeFnFingerprintStable(t *testing.T) {
	fd := makeFuncDecl("square", "i64",
		[]ast.Param{{Name: "n", Type: simpleType("i64")}},
		&ast.Block{Stmts: []ast.Node{
			&ast.ReturnStmt{Value: &ast.BinExpr{
				Op:    "*",
				Left:  &ast.Identifier{Name: "n"},
				Right: &ast.Identifier{Name: "n"},
			}},
		}},
	)

	a := ctfeFnFingerprint(fd)
	b := ctfeFnFingerprint(fd)

	if a != b {
		t.Fatalf("fingerprint not deterministic:\nA=%q\nB=%q", a, b)
	}

	if !strings.Contains(a, "fn square") {
		t.Fatalf("fingerprint missing fn name; got %q", a)
	}

	if !strings.Contains(a, "Bin *") {
		t.Fatalf("fingerprint missing operator; got %q", a)
	}
}

// TestCtfeFnFingerprintBodyChange: changing the body changes the fingerprint.
func TestCtfeFnFingerprintBodyChange(t *testing.T) {
	mul := makeFuncDecl("foo", "i64",
		[]ast.Param{{Name: "n", Type: simpleType("i64")}},
		&ast.Block{Stmts: []ast.Node{
			&ast.ReturnStmt{Value: &ast.BinExpr{
				Op:    "*",
				Left:  &ast.Identifier{Name: "n"},
				Right: &ast.Identifier{Name: "n"},
			}},
		}},
	)

	add := makeFuncDecl("foo", "i64",
		[]ast.Param{{Name: "n", Type: simpleType("i64")}},
		&ast.Block{Stmts: []ast.Node{
			&ast.ReturnStmt{Value: &ast.BinExpr{
				Op:    "+",
				Left:  &ast.Identifier{Name: "n"},
				Right: &ast.Identifier{Name: "n"},
			}},
		}},
	)

	if ctfeFnFingerprint(mul) == ctfeFnFingerprint(add) {
		t.Fatalf("fingerprint did not differentiate * from +")
	}
}

// TestCtfeFnHashMerkle: changing a direct dep flips the caller's hash.
func TestCtfeFnHashMerkle(t *testing.T) {
	cg := &CodeGen{funcDecls: map[string]*ast.FuncDecl{}}

	depV1 := makeFuncDecl("dep", "i64", nil, &ast.Block{Stmts: []ast.Node{
		&ast.ReturnStmt{Value: &ast.IntLit{Value: 1}},
	}})
	caller := makeFuncDecl("caller", "i64", nil, &ast.Block{Stmts: []ast.Node{
		&ast.ReturnStmt{Value: &ast.CallExpr{Func: &ast.Identifier{Name: "dep"}}},
	}})

	cg.funcDecls["dep"] = depV1
	cg.funcDecls["caller"] = caller
	cg.ctfeFnHashes = ctfeFnHashCache{}
	hashV1 := cg.ctfeFnHash(caller)

	if hashV1 == "" {
		t.Fatalf("expected non-empty hash for caller, got empty")
	}

	depV2 := makeFuncDecl("dep", "i64", nil, &ast.Block{Stmts: []ast.Node{
		&ast.ReturnStmt{Value: &ast.IntLit{Value: 2}},
	}})

	cg2 := &CodeGen{funcDecls: map[string]*ast.FuncDecl{
		"dep":    depV2,
		"caller": caller,
	}}
	cg2.ctfeFnHashes = ctfeFnHashCache{}
	hashV2 := cg2.ctfeFnHash(caller)

	if hashV1 == hashV2 {
		t.Fatalf("Merkle hash did not change when dep body changed: %s", hashV1)
	}
}

// TestCtfeFnHashCycle: recursive functions produce a stable hash.
func TestCtfeFnHashCycle(t *testing.T) {
	cg := &CodeGen{funcDecls: map[string]*ast.FuncDecl{}}

	fact := makeFuncDecl("fact", "i64",
		[]ast.Param{{Name: "n", Type: simpleType("i64")}},
		&ast.Block{Stmts: []ast.Node{
			&ast.IfStmt{
				Cond: &ast.BinExpr{Op: "<=", Left: &ast.Identifier{Name: "n"}, Right: &ast.IntLit{Value: 1}},
				Then: &ast.Block{Stmts: []ast.Node{
					&ast.ReturnStmt{Value: &ast.IntLit{Value: 1}},
				}},
			},
			&ast.ReturnStmt{Value: &ast.BinExpr{
				Op:   "*",
				Left: &ast.Identifier{Name: "n"},
				Right: &ast.CallExpr{
					Func: &ast.Identifier{Name: "fact"},
					Args: []ast.Node{&ast.BinExpr{Op: "-", Left: &ast.Identifier{Name: "n"}, Right: &ast.IntLit{Value: 1}}},
				},
			}},
		}},
	)
	cg.funcDecls["fact"] = fact
	cg.ctfeFnHashes = ctfeFnHashCache{}
	h1 := cg.ctfeFnHash(fact)

	cg2 := &CodeGen{funcDecls: map[string]*ast.FuncDecl{"fact": fact}}
	cg2.ctfeFnHashes = ctfeFnHashCache{}
	h2 := cg2.ctfeFnHash(fact)

	if h1 == "" || h1 != h2 {
		t.Fatalf("recursive function hash unstable: %q vs %q", h1, h2)
	}
}

// TestSliceIRForFunc: target define is preserved, every other define becomes a declare.
func TestSliceIRForFunc(t *testing.T) {
	full := `target triple = "x86_64-pc-linux-gnu"

%struct.Foo = type { i64, i8* }

@global = constant i64 42

declare void @runtime_release(i8* %p)

define i64 @other(i64 %n) alwaysinline readnone nounwind {
entry:
	%0 = mul i64 %n, 3
	ret i64 %0
}

define i64 @target(i64 %x) {
entry:
	%0 = call i64 @other(i64 %x)
	ret i64 %0
}
`

	out := sliceIRForFunc(full, "target")
	if out == "" {
		t.Fatalf("slice returned empty for present function")
	}

	mustContain(t, out, "define i64 @target(i64 %x)")
	mustContain(t, out, "declare i64 @other(i64 %n) alwaysinline readnone nounwind")
	mustContain(t, out, "@global = constant")
	mustContain(t, out, "%struct.Foo = type")
	mustContain(t, out, `target triple = "x86_64-pc-linux-gnu"`)
	mustContain(t, out, "declare void @runtime_release")
	// the *original* `define i64 @other` body must NOT be in the slice.
	if strings.Contains(out, "%0 = mul i64 %n, 3") {
		t.Fatalf("non-target define body leaked into slice:\n%s", out)
	}
}

// TestSliceIRForFuncMissing: returns "" when the target define is absent.
func TestSliceIRForFuncMissing(t *testing.T) {
	full := `target triple = "x86_64-pc-linux-gnu"

define i64 @other(i64 %n) {
entry:
	ret i64 %n
}
`

	if out := sliceIRForFunc(full, "missing"); out != "" {
		t.Fatalf("expected empty slice for missing function, got:\n%s", out)
	}
}

// TestExtractDefineName covers a few real shapes seen in Tin's IR.
func TestExtractDefineName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"define i64 @foo(i64 %n) {", "foo"},
		{"define void @assert__ok(i1 %cond) {", "assert__ok"},
		{"define i64 @safe_add(i64 %a, i64 %b) alwaysinline readnone nounwind {", "safe_add"},
		{"define { i8*, i64 } @returns_string() {", "returns_string"},
		{"; comment line", ""},
	}

	for _, c := range cases {
		if got := extractDefineName(c.in); got != c.want {
			t.Errorf("extractDefineName(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected slice to contain %q\n--- slice ---\n%s", needle, haystack)
	}
}

// helpers

func makeFuncDecl(name, ret string, params []ast.Param, body ast.Node) *ast.FuncDecl {
	fd := &ast.FuncDecl{Name: name, Params: params, Body: body, Tags: []string{"pure"}}
	if ret != "" {
		fd.RetType = simpleType(ret)
	}

	return fd
}

func simpleType(name string) ast.TypeExpr {
	return &ast.SimpleType{Name: name}
}
