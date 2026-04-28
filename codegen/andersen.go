package codegen

// Whole-program Andersen-style points-to analysis.
//
// The goal is interprocedural: an intraprocedural pass already knows that
// `let p *T = nil; *p` derefs nil, but it can't see through
// `let p = make_nothing(); *p` when the callee always returns nil. Andersen
// solves that.
//
// Algorithm (inclusion-based, context-insensitive):
//
//   1. Each variable becomes a node `<fn>::<name>`. Functions get a
//      synthetic `$ret` node.
//   2. Each `let p = <expr>` / `p = <expr>` / `return <expr>` produces
//      one or more inclusion constraints:
//         p = nil        ->  pts(p) ⊇ {NIL}
//         p = &x         ->  pts(p) ⊇ {ADDR(<fn>, x)}
//         p = q          ->  pts(p) ⊇ pts(q)
//         p = f(args...) ->  pts(p) ⊇ pts(f::$ret)
//                            and pts(f::param_i) ⊇ pts(args[i])
//      (Loads/stores through pointer arithmetic are not modeled here -
//      Tin already requires `{#unsafe}` to write them, and the static
//      analyzer is meant to surface obvious bugs, not exhaustively model
//      heap shape.)
//   3. Solve the constraint set to fixpoint with a simple worklist.
//   4. Use the result: when `pts(p) == {NIL}` at every program point, every
//      `*p` / `p.field` is provably nil and gets a hard warning.
//
// This complements the intraprocedural dataflow pass: that one is precise
// inside a single function's CFG; Andersen widens the picture across calls.

import (
	"github.com/Azer0s/tin/ast"
)

// ptToken is an abstract memory location: NIL, the address of a stack
// variable, or a heap site (`@heap:<fn>:<line>`). String typing keeps
// debug-printing trivial.
type ptToken string

const aTokNil ptToken = "@nil"

// ptVar names a points-to graph node: `<fn>::<name>`. Functions also get
// a synthetic `<fn>::$ret` for their return value.
type ptVar string

func ptVarFor(fn, name string) ptVar { return ptVar(fn + "::" + name) }

type ptcKind int

const (
	ptcAdd  ptcKind = iota // pts(dst) ⊇ {token}
	ptcCopy                // pts(dst) ⊇ pts(src)
)

type ptConstraint struct {
	kind  ptcKind
	dst   ptVar
	src   ptVar
	token ptToken
}

// runAndersen computes a program-wide points-to summary and uses it to
// surface interprocedural nil-deref findings. Runs after the
// intraprocedural pass so the two warnings can ride the same DiagDerefNil
// channel.
func (cg *CodeGen) runAndersen(prog *ast.Program) {
	funcByName := map[string]*ast.FuncDecl{}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			funcByName[v.Name] = v
		case *ast.StructDecl:
			for _, m := range v.Methods {
				funcByName[m.Name] = m
			}
		}
	}

	var constraints []ptConstraint

	for _, fd := range funcByName {
		cg.collectPtConstraints(fd, funcByName, &constraints)
	}

	pts := solveAndersen(constraints)

	for _, fd := range funcByName {
		cg.scanAndersenDerefs(fd, pts)
	}
}

// collectPtConstraints walks fn's body, emitting one constraint per
// reachable assignment, return, or call site.
func (cg *CodeGen) collectPtConstraints(fn *ast.FuncDecl, funcByName map[string]*ast.FuncDecl, out *[]ptConstraint) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	walkAST(fn.Body, func(n ast.Node) {
		switch s := n.(type) {
		case *ast.VarDecl:
			cg.assignConstraints(fn.Name, s.Name, s.Value, funcByName, out)
		case *ast.AssignStmt:
			if id, ok := s.Target.(*ast.Identifier); ok {
				cg.assignConstraints(fn.Name, id.Name, s.Value, funcByName, out)
			}
		case *ast.ReturnStmt:
			if s.Value != nil {
				cg.assignConstraints(fn.Name, "$ret", s.Value, funcByName, out)
			}
		case *ast.CallExpr:
			cg.callConstraints(fn.Name, s, funcByName, out)
		}
	})
}

// assignConstraints emits the one or two constraints implied by
// `<fn>::<dst> = <src>`.
func (cg *CodeGen) assignConstraints(fnName, dstName string, src ast.Node, funcByName map[string]*ast.FuncDecl, out *[]ptConstraint) {
	dst := ptVarFor(fnName, dstName)

	switch e := src.(type) {
	case nil:
		return
	case *ast.NilLit:
		*out = append(*out, ptConstraint{kind: ptcAdd, dst: dst, token: aTokNil})
	case *ast.AddressOfExpr:
		if id, ok := e.Expr.(*ast.Identifier); ok {
			tok := ptToken("@addr:" + fnName + "::" + id.Name)
			*out = append(*out, ptConstraint{kind: ptcAdd, dst: dst, token: tok})
		}
	case *ast.Identifier:
		*out = append(*out, ptConstraint{kind: ptcCopy, dst: dst, src: ptVarFor(fnName, e.Name)})
	case *ast.CallExpr:
		if id, ok := e.Func.(*ast.Identifier); ok {
			if callee, ok2 := funcByName[id.Name]; ok2 {
				*out = append(*out, ptConstraint{kind: ptcCopy, dst: dst, src: ptVarFor(callee.Name, "$ret")})
			}
		}
	}
}

// callConstraints emits parameter-binding constraints for a call site.
// pts(<callee>::param_i) ⊇ pts(<caller>::arg_i).
func (cg *CodeGen) callConstraints(callerFn string, c *ast.CallExpr, funcByName map[string]*ast.FuncDecl, out *[]ptConstraint) {
	id, ok := c.Func.(*ast.Identifier)
	if !ok {
		return
	}

	callee, ok := funcByName[id.Name]
	if !ok {
		return
	}

	for i, arg := range c.Args {
		if i >= len(callee.Params) {
			break
		}

		paramName := callee.Params[i].Name
		if paramName == "" || paramName == "_" {
			continue
		}

		cg.assignConstraints(callee.Name, paramName, arg, funcByName, out)
	}
}

// solveAndersen runs the worklist solver to fixpoint. Returns the pts map.
func solveAndersen(constraints []ptConstraint) map[ptVar]map[ptToken]bool {
	pts := map[ptVar]map[ptToken]bool{}

	add := func(v ptVar, tok ptToken) bool {
		if pts[v] == nil {
			pts[v] = map[ptToken]bool{}
		}

		if pts[v][tok] {
			return false
		}

		pts[v][tok] = true

		return true
	}

	for changed := true; changed; {
		changed = false

		for _, c := range constraints {
			switch c.kind {
			case ptcAdd:
				if add(c.dst, c.token) {
					changed = true
				}
			case ptcCopy:
				for tok := range pts[c.src] {
					if add(c.dst, tok) {
						changed = true
					}
				}
			}
		}
	}

	return pts
}

// scanAndersenDerefs reports `*p` / `p.field` sites where p's solved pts
// set is exactly {NIL}. Stronger conditions (e.g. p may sometimes be nil)
// are reserved for the intraprocedural pass to keep the false-positive
// rate low.
func (cg *CodeGen) scanAndersenDerefs(fn *ast.FuncDecl, pts map[ptVar]map[ptToken]bool) {
	if fn.Body == nil {
		return
	}

	provesNil := func(name string) bool {
		set, ok := pts[ptVarFor(fn.Name, name)]
		if !ok || len(set) == 0 {
			return false
		}

		return len(set) == 1 && set[aTokNil]
	}

	walkAST(fn.Body, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.DerefExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok && provesNil(id.Name) {
				cg.warn(DiagDerefNil, e.Pos(),
					"dereferencing %q which Andersen analysis proves only points to nil", id.Name)
			}
		case *ast.FieldAccess:
			if id, ok := e.Expr.(*ast.Identifier); ok && provesNil(id.Name) {
				cg.warn(DiagDerefNil, e.Pos(),
					"field access on %q which Andersen analysis proves only points to nil", id.Name)
			}
		}
	})
}
