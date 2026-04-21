package repl

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

// macroEntry holds metadata about a single macro.
// Name is always the bare name WITHOUT the trailing `!`.
type macroEntry struct {
	Name     string
	Params   []string
	HasBang  bool // original name ends in ! (e.g. todo!, min!)
	NoParens bool // #no_parens: invoked as bare identifier (e.g. loop)
	Decl     *ast.MacroDecl
}

// macroRegistry is a shared, non-concurrent registry of known macros.
// Written by the session (main goroutine, between readline calls) and read
// by the highlighter / autocompleter (also main goroutine, inside readline).
type macroRegistry struct {
	byName map[string]macroEntry // key = bare name without !
	noExcl map[string]bool       // bare names used without ! (includes no_parens)
}

func newMacroRegistry() *macroRegistry {
	return &macroRegistry{
		byName: make(map[string]macroEntry),
		noExcl: make(map[string]bool),
	}
}

func (r *macroRegistry) register(e macroEntry) {
	r.byName[e.Name] = e
	if !e.HasBang {
		r.noExcl[e.Name] = true
	}
}

// isMacroIdent returns true if name is a known macro invoked without !.
func (r *macroRegistry) isMacroIdent(name string) bool {
	return r.noExcl[name]
}

// lookup returns the entry for a bare macro name (without !).
func (r *macroRegistry) lookup(name string) (macroEntry, bool) {
	name = strings.TrimSuffix(name, "!")
	e, ok := r.byName[name]

	return e, ok
}
