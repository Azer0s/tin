package parser

import (
	"testing"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// parseTypeFromSrc lexes src and parses it as a single type expression.
// Helper for the wildcard-in-trait-bound parsing tests.
func parseTypeFromSrc(t *testing.T, src string) ast.TypeExpr {
	t.Helper()

	tokens, err := lexer.New(src).Tokenize()
	if err != nil {
		t.Fatalf("lex %q: %v", src, err)
	}

	p := New(tokens, "<test>")

	te, err := p.parseTypeExpr()
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}

	return te
}

func TestParseWildcardAnonymous(t *testing.T) {
	te := parseTypeFromSrc(t, "_")

	w, ok := te.(*ast.WildcardType)
	if !ok {
		t.Fatalf("expected *WildcardType, got %T", te)
	}

	if w.Name != "" {
		t.Fatalf("expected empty Name for anonymous wildcard, got %q", w.Name)
	}
}

func TestParseWildcardNamed(t *testing.T) {
	te := parseTypeFromSrc(t, "_: T")

	w, ok := te.(*ast.WildcardType)
	if !ok {
		t.Fatalf("expected *WildcardType, got %T", te)
	}

	if w.Name != "T" {
		t.Fatalf("expected Name == T, got %q", w.Name)
	}
}

func TestParseWildcardInsideGeneric(t *testing.T) {
	te := parseTypeFromSrc(t, "Result[_, E]")

	g, ok := te.(*ast.GenericType)
	if !ok {
		t.Fatalf("expected *GenericType, got %T", te)
	}

	if g.Name != "Result" {
		t.Fatalf("expected Name == Result, got %q", g.Name)
	}

	if len(g.TypeParams) != 2 {
		t.Fatalf("expected 2 type params, got %d", len(g.TypeParams))
	}

	if _, ok := g.TypeParams[0].(*ast.WildcardType); !ok {
		t.Fatalf("first param: expected *WildcardType, got %T", g.TypeParams[0])
	}

	if s, ok := g.TypeParams[1].(*ast.SimpleType); !ok || s.Name != "E" {
		t.Fatalf("second param: expected SimpleType{E}, got %T %v", g.TypeParams[1], g.TypeParams[1])
	}
}

func TestParseWildcardMultipleAnonymous(t *testing.T) {
	te := parseTypeFromSrc(t, "Map[_, _]")

	g, ok := te.(*ast.GenericType)
	if !ok {
		t.Fatalf("expected *GenericType, got %T", te)
	}

	if len(g.TypeParams) != 2 {
		t.Fatalf("expected 2 type params, got %d", len(g.TypeParams))
	}

	for i, tp := range g.TypeParams {
		if _, ok := tp.(*ast.WildcardType); !ok {
			t.Fatalf("param %d: expected *WildcardType, got %T", i, tp)
		}
	}
}

func TestParseWildcardNamedMixed(t *testing.T) {
	te := parseTypeFromSrc(t, "Map[_: K, _: V]")

	g, ok := te.(*ast.GenericType)
	if !ok {
		t.Fatalf("expected *GenericType, got %T", te)
	}

	if len(g.TypeParams) != 2 {
		t.Fatalf("expected 2 type params, got %d", len(g.TypeParams))
	}

	w0, ok := g.TypeParams[0].(*ast.WildcardType)
	if !ok || w0.Name != "K" {
		t.Fatalf("first param: expected WildcardType{K}, got %T %v", g.TypeParams[0], g.TypeParams[0])
	}

	w1, ok := g.TypeParams[1].(*ast.WildcardType)
	if !ok || w1.Name != "V" {
		t.Fatalf("second param: expected WildcardType{V}, got %T %v", g.TypeParams[1], g.TypeParams[1])
	}
}

func TestWildcardTypeString(t *testing.T) {
	cases := []struct {
		in   *ast.WildcardType
		want string
	}{
		{&ast.WildcardType{}, "_"},
		{&ast.WildcardType{Name: "T"}, "_: T"},
	}

	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Fatalf("String(%+v): want %q got %q", c.in, c.want, got)
		}
	}
}
