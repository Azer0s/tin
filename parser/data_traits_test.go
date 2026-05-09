package parser

import (
	"testing"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// parseProgram lexes src and returns the parsed program. Test helper.
func parseProgram(t *testing.T, src string) *ast.Program {
	t.Helper()

	tokens, err := lexer.New(src).Tokenize()
	if err != nil {
		t.Fatalf("lex: %v", err)
	}

	p := New(tokens, "<test>")

	prog, err := p.Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return prog
}

func findDataDecl(prog *ast.Program, name string) *ast.DataDecl {
	for _, d := range prog.Stmts {
		if dd, ok := d.(*ast.DataDecl); ok && dd.Name == name {
			return dd
		}
	}

	return nil
}

func TestParseDataDeclWithTraitHeader(t *testing.T) {
	src := `
data Foo[T](Bar, Baz[T]) =
  A(v T)
  B
`
	prog := parseProgram(t, src)

	d := findDataDecl(prog, "Foo")
	if d == nil {
		t.Fatalf("Foo not found")
	}

	if len(d.Implements) != 2 {
		t.Fatalf("expected 2 trait impls, got %d", len(d.Implements))
	}

	if s, ok := d.Implements[0].(*ast.SimpleType); !ok || s.Name != "Bar" {
		t.Fatalf("first impl: expected Bar, got %v", d.Implements[0])
	}

	if g, ok := d.Implements[1].(*ast.GenericType); !ok || g.Name != "Baz" {
		t.Fatalf("second impl: expected Baz[T], got %v", d.Implements[1])
	}

	if len(d.Variants) != 2 {
		t.Fatalf("expected 2 variants, got %d", len(d.Variants))
	}
}

func TestParseDataDeclWithMethods(t *testing.T) {
	src := `
data Foo[T](Bar) =
  A(v T)
  B

  fn Bar::name(this Foo[T]) string =
    return "foo"
`
	prog := parseProgram(t, src)

	d := findDataDecl(prog, "Foo")
	if d == nil {
		t.Fatalf("Foo not found")
	}

	if len(d.Methods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(d.Methods))
	}

	if d.Methods[0].Name != "name" {
		t.Fatalf("method name: got %q", d.Methods[0].Name)
	}
}

func TestParseDataDeclWithWildcardTraitBound(t *testing.T) {
	src := `
data Result[T, E](tryable[T, Result[_, E]]) =
  Ok(v T)
  Err(msg E)
`
	prog := parseProgram(t, src)

	d := findDataDecl(prog, "Result")
	if d == nil {
		t.Fatalf("Result not found")
	}

	if len(d.Implements) != 1 {
		t.Fatalf("expected 1 trait impl, got %d", len(d.Implements))
	}

	g, ok := d.Implements[0].(*ast.GenericType)
	if !ok || g.Name != "tryable" {
		t.Fatalf("expected tryable[..], got %v", d.Implements[0])
	}

	// tryable[T, Result[_, E]] - second slot is a GenericType containing
	// a WildcardType in its first param.
	inner, ok := g.TypeParams[1].(*ast.GenericType)
	if !ok || inner.Name != "Result" {
		t.Fatalf("expected Result[..] inner, got %v", g.TypeParams[1])
	}

	if _, ok := inner.TypeParams[0].(*ast.WildcardType); !ok {
		t.Fatalf("expected wildcard in Result's first slot, got %T", inner.TypeParams[0])
	}
}
