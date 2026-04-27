package repl

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

// opTraitRegistry tracks which built-in operator traits the user has
// implemented in this REPL session. The highlighter consults it to color
// operator tokens only when their corresponding trait is overloaded.
//
// Written by the session (main goroutine, between readline calls) and read
// by the highlighter (also main goroutine, inside readline).
type opTraitRegistry struct {
	overloaded map[string]bool // keys: trait names ("add", "sub", ...)
}

func newOpTraitRegistry() *opTraitRegistry {
	return &opTraitRegistry{overloaded: map[string]bool{}}
}

// builtinOpTraits is the set of trait names for which the highlighter colors
// the corresponding operator token. Mirrors registerBuiltinOpTraits in
// codegen/codegen.go.
var builtinOpTraits = map[string]bool{
	"add":       true,
	"sub":       true,
	"mul":       true,
	"div":       true,
	"mod":       true,
	"neg":       true,
	"pos":       true,
	"not":       true,
	"comp":      true,
	"ord":       true,
	"index":     true,
	"index_set": true,
	"concat":    true,
}

// recordImpls scans a struct's Implements list and registers any operator
// trait names it finds.
func (r *opTraitRegistry) recordImpls(impls []ast.TypeExpr) {
	for _, t := range impls {
		name := traitNameOf(t)
		if builtinOpTraits[name] {
			r.overloaded[name] = true
		}
	}
}

// isOverloaded reports whether traitName has been implemented in the current
// REPL session.
func (r *opTraitRegistry) isOverloaded(traitName string) bool {
	if r == nil {
		return false
	}

	return r.overloaded[traitName]
}

// traitNameOf extracts the bare trait name from a TypeExpr in a struct's
// Implements list, stripping any module qualifier ("io::Reader" -> "Reader").
func traitNameOf(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return stripModule(t.Name)
	case *ast.GenericType:
		return stripModule(t.Name)
	}

	return ""
}

func stripModule(name string) string {
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		return name[idx+2:]
	}

	return name
}
