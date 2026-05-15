package codegen

import (
	"github.com/Azer0s/tin/ast"
)

// emitUnreachableArmWarning prints a warning to stderr when an arm of a
// match or where list is provably unreachable. The message includes the
// source position of the offending arm and a short hint about the rule
// that fired. Suppressed by -Wno-unused-match-arms; escalatable via
// -Werror=unused-match-arms.
func (cg *CodeGen) emitUnreachableArmWarning(pos ast.Pos, kind string) {
	cg.warn(DiagUnusedMatchArms, pos,
		"unreachable %s: previous arms cover every value this arm matches", kind)
}

// scanWhereForUnreachable walks the where clauses in declaration order,
// emitting an unreachable warning for each clause that adds nothing new.
// Backed by the Maranget Useful() check (codegen/maranget.go).
func (cg *CodeGen) scanWhereForUnreachable(wl *ast.WhereList) {
	if wl == nil {
		return
	}

	if cg.verboseMatchInfo {
		cg.dumpWhereInfo(wl, "where-list")
	}

	if cg.diagSuppressed(DiagUnusedMatchArms) {
		return
	}

	arity := cg.whereArity(wl)
	for i := range wl.Clauses {
		if !cg.marangetWhereArmUseful(wl, i, arity) {
			cg.emitUnreachableArmWarning(wl.Clauses[i].Pos, "where clause")
		}
	}
}

// whereArity infers the arity of the where-list. Single-arg if every clause
// is non-tuple; otherwise the tuple width of the first non-`_` clause.
// Used by both the warning scanner and the Maranget reachability check.
func (cg *CodeGen) whereArity(wl *ast.WhereList) int {
	if cg.curFn != nil {
		return len(cg.curFn.Params)
	}

	for _, c := range wl.Clauses {
		if tp, ok := c.Pattern.(*ast.TuplePattern); ok {
			return len(tp.Elems)
		}
	}

	return 1
}

// scanMatchForUnreachable does the same for match cases. The default arm
// (s.Default != nil) is treated as the lowest-priority wildcard catch-all.
// Backed by the Maranget Useful() check.
func (cg *CodeGen) scanMatchForUnreachable(s *ast.MatchStmt) {
	if s == nil {
		return
	}

	if cg.verboseMatchInfo {
		cg.dumpMatchInfo(s, "match")
	}

	if cg.diagSuppressed(DiagUnusedMatchArms) {
		return
	}
	// Skip the unreachable scan when any case pattern is a bare
	// identifier naming a known type ("case T:" -- not valid pattern
	// syntax, the user wants `case x is T:` or `match scrutinee.(type)`).
	// Without this skip the Maranget walker sees the bare identifier
	// as a wildcard binder and falsely flags the subsequent arm as
	// unreachable, piling a confusing "unreachable" warning on top of
	// the real "undefined identifier" error that genIdentifier will
	// emit later for the same arm.
	for _, c := range s.Cases {
		if id, ok := c.Pattern.(*ast.Identifier); ok {
			if cg.isKnownTypeName(id.Name) {
				return
			}
		}
	}

	for i := range s.Cases {
		if !cg.marangetMatchArmUseful(s, i) {
			cg.emitUnreachableArmWarning(s.Cases[i].Pos, "match case")
		}
	}

	if s.Default != nil {
		if !cg.marangetMatchDefaultUseful(s) {
			cg.emitUnreachableArmWarning(s.Pos(), "match default")
		}
	}
}
