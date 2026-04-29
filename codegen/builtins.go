package codegen

// builtins.go - the single source of truth for which names the compiler
// recognises as compile-time / runtime builtins, plus the helper that
// raises DiagBuiltinShadow when a local binding masks one.
//
// Any new builtin recognised in genCallExpr (or anywhere else) MUST be
// added to compileTimeBuiltins so the shadow-warning pass and any future
// "list available builtins" tooling see it. Forgetting to add a name
// here doesn't break correctness — the warning is opt-in — but it does
// silently weaken -Wpedantic.

import (
	"github.com/Azer0s/tin/ast"
)

// compileTimeBuiltins is the set of identifiers genCallExpr or related
// passes special-case rather than treating as ordinary fn calls. Lookup
// is a hot path during decl processing (every let/var/param hits it), so
// a map beats a slice scan.
var compileTimeBuiltins = map[string]bool{
	"typeof":     true, // type-of via parser-level TypeofExpr; included for shadow detection on the textual form
	"default":    true,
	"panic":      true,
	"recover":    true,
	"len":        true,
	"echo":       true, // not a fn but reserved as a statement keyword; shadow still misleads
	"sourcepos":  true,
	"stacktrace": true, // reserved for the libunwind-backed runtime builtin (see docs/plans/stacktrace-libunwind.md)
}

// IsCompileTimeBuiltin reports whether name is one of the compiler's
// recognised builtins. Used by the shadow-warning hook and any future
// reflection/diagnostic tooling that needs to enumerate builtins.
func IsCompileTimeBuiltin(name string) bool {
	return compileTimeBuiltins[name]
}

// warnIfBuiltinShadow fires DiagBuiltinShadow when name is a builtin and
// the local binding sits inside a function body (i.e. has a non-module
// scope). Top-level shadowing already triggers earlier hard errors in
// most cases, so we don't re-warn there. Pass the binding-site Pos and
// a one-word `kind` describing what is shadowing ("let", "var", "param",
// "fn"); the rendered message is "<kind> '<name>' shadows builtin
// '<name>'".
func (cg *CodeGen) warnIfBuiltinShadow(kind, name string, pos ast.Pos) {
	if !compileTimeBuiltins[name] {
		return
	}
	// Skip the warning at module scope: top-level shadowing is either
	// already an error or is the user's deliberate redefinition; either
	// way it's NOT what -Wbuiltin-shadow targets.
	if cg.curScope == nil || cg.curScope == cg.moduleScope {
		return
	}

	cg.warn(DiagBuiltinShadow, pos, "%s %s shadows builtin %s", kind, name, name)
}
