package codegen

import "testing"

func TestLatexToUnicode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"no escapes here", "no escapes here"},
		{`\pm 1`, "± 1"},
		{`x \in [0, 1]`, "x ∈ [0, 1]"},
		{`+\infty`, "+∞"},
		{`\pm\infty`, "±∞"},
		{`pts(p) \supseteq pts(q)`, "pts(p) ⊇ pts(q)"},
		{`\alpha + \beta = \gamma`, "α + β = γ"},
		{`A \cap B \cup C`, "A ∩ B ∪ C"},
		{`\Sigma vs \sigma`, "Σ vs σ"},
		{`a \neq b`, "a ≠ b"},
		{`\forall x \in N`, "∀ x ∈ N"},
		// `\in` must not eat the start of `\infty`.
		{`x \in \infty`, "x ∈ ∞"},
		// Unknown command stays verbatim.
		{`\unknown stays`, `\unknown stays`},
	}

	for _, c := range cases {
		if got := latexToUnicode(c.in); got != c.want {
			t.Errorf("latexToUnicode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
