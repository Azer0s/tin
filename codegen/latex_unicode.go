package codegen

import "strings"

// latexToUnicodeReplacer expands LaTeX-style command names into their unicode
// glyphs at diagnostic render time. Lets the compiler keep its `cg.warn` /
// `cg.nodeErr` strings ASCII-safe (per the codebase-hygiene rule) while still
// showing the user a pretty `\pm \infty` rendering.
//
// strings.NewReplacer picks the earliest-listed match at each position (NOT
// the longest), so any prefix that shares a leading segment with a longer
// command (e.g. `\in` vs `\infty`, `\sum` vs `\subseteq`/`\subset`/`\sigma`)
// must be listed AFTER its longer cousins. Keep that invariant when adding
// new entries -- the test in latex_unicode_test.go covers the common traps.
var latexToUnicodeReplacer = strings.NewReplacer(
	// --- longer prefixes first within each shared family ---

	// `\in*` family: `\infty` and `\int` before `\in`.
	`\infty`, "∞",
	`\int`, "∫",
	`\in`, "∈",

	// `\sub*` / `\sup*` families.
	`\subseteq`, "⊆",
	`\subset`, "⊂",
	`\supseteq`, "⊇",
	`\supset`, "⊃",

	// `\not*` family.
	`\notin`, "∉",

	// `\Sigma` vs `\sigma` and `\sum` (different glyphs entirely, no prefix
	// collision -- but listed together for grouping).
	`\Sigma`, "Σ",
	`\sigma`, "σ",
	`\sum`, "∑",

	// `\prod` vs `\pi` -- no collision but kept together.
	`\prod`, "∏",
	`\pi`, "π",

	// `\partial` before `\pm` would matter if we had `\pmp...` etc., but the
	// `\p*` keys here are disjoint at char 2 (a/m vs r/h/s/i).
	`\partial`, "∂",
	`\pm`, "±",
	`\mp`, "∓",
	`\phi`, "φ",
	`\psi`, "ψ",
	`\Phi`, "Φ",
	`\Psi`, "Ψ",
	`\Pi`, "Π",

	// `\Lambda` vs `\lambda`.
	`\Lambda`, "Λ",
	`\lambda`, "λ",

	// `\Leftrightarrow` before `\Leftarrow`, `\Leftarrow` before `\leq`.
	`\Leftrightarrow`, "⇔",
	`\Leftarrow`, "⇐",
	`\leq`, "≤",
	`\Rightarrow`, "⇒",
	`\rightarrow`, "→",
	`\leftarrow`, "←",
	`\mapsto`, "↦",
	`\to`, "→",

	// `\geq` is solitary in its family.
	`\geq`, "≥",
	`\neq`, "≠",
	`\equiv`, "≡",
	`\approx`, "≈",

	// Set operations and quantifiers.
	`\cap`, "∩",
	`\cup`, "∪",
	`\forall`, "∀",
	`\exists`, "∃",

	// Arithmetic glyphs.
	`\times`, "×",
	`\cdot`, "·",
	`\div`, "÷",

	// `\Delta` vs `\delta`.
	`\Delta`, "Δ",
	`\delta`, "δ",

	// `\Theta` vs `\theta`.
	`\Theta`, "Θ",
	`\theta`, "θ",

	// `\Gamma` vs `\gamma`.
	`\Gamma`, "Γ",
	`\gamma`, "γ",

	// `\Omega` vs `\omega`.
	`\Omega`, "Ω",
	`\omega`, "ω",

	// `\Xi` vs `\xi`.
	`\Xi`, "Ξ",
	`\xi`, "ξ",

	// `\epsilon` -- solitary.
	`\epsilon`, "ε",
	`\zeta`, "ζ",
	`\eta`, "η",
	`\iota`, "ι",
	`\kappa`, "κ",
	`\mu`, "μ",
	`\nu`, "ν",
	`\rho`, "ρ",
	`\tau`, "τ",
	`\chi`, "χ",
	`\alpha`, "α",
	`\beta`, "β",

	// Remaining math operators.
	`\nabla`, "∇",
	`\sqrt`, "√",
)

// latexToUnicode expands a small set of LaTeX command names (`\pm`, `\infty`,
// Greek letters, etc.) into the corresponding unicode glyph. Used to render
// math symbols in compiler diagnostics without forcing the message strings in
// Go source to contain non-ASCII bytes.
func latexToUnicode(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}

	return latexToUnicodeReplacer.Replace(s)
}
