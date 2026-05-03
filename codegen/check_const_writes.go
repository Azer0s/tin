package codegen

// check_const_writes.go - static analysis pass that warns when a
// program takes the address of a top-level `const` and then writes
// through that pointer.
//
// Top-level const is placed in read-only storage by codegen (the
// global is emitted with g.Immutable=true so it lives in `.rodata`).
// Writing through an alias is undefined behavior - the LLVM
// optimizer can elide the store entirely or the OS can segfault on
// the access. Either way the program does not behave as the source
// suggests.
//
// The pass runs once per program, after parse and before codegen
// emits IR. It is two-phase:
//
//   1. Pre-pass: scan every function body to compute which parameter
//      indices are written through inside the body. This builds a
//      "may-mutate" set per function name.
//
//   2. Body pass: for each function, mark every binding tainted that
//      is initialised from `&top_level_const` or aliases another
//      tainted binding. Warn when:
//      - `*p = ...`, `*p += ...`, `*p++` is performed on a tainted p,
//        or
//      - a tainted p is passed at a call site to a parameter index
//        the callee writes through.
//
// Inter-procedural propagation stops at one level of indirection:
// if `helper` calls `inner_helper(p)` and inner_helper writes
// through the param, we mark `helper`'s corresponding param as
// write-through too via the pre-pass fixpoint, so the chain is
// captured. This is a closed flow analysis on user-defined Tin
// functions; extern / FFI calls are conservatively treated as
// "doesn't write" to avoid noise (the user is on their own once a
// pointer crosses into C).

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkAllWritesToTopLevelConst(prog *ast.Program) {
	if cg.diagSuppressed(DiagWriteToConst) {
		return
	}

	consts := collectTopLevelConsts(prog)
	if len(consts) == 0 {
		return
	}

	mutators := computeMutatingParams(prog)

	for _, n := range prog.Stmts {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		cg.checkFuncForConstWrites(fn, consts, mutators)
	}
}

// collectTopLevelConsts returns the set of top-level decl names that
// were declared with `const`. Only TopLevelVar decls qualify - block-
// scope const is local and never escapes through a global pointer.
func collectTopLevelConsts(prog *ast.Program) map[string]bool {
	consts := map[string]bool{}

	for _, n := range prog.Stmts {
		if tv, ok := n.(*ast.TopLevelVar); ok && tv.IsConst {
			consts[tv.Name] = true
		}
	}

	return consts
}

// computeMutatingParams returns a map fn-name -> set of parameter
// indices that the function writes through (either directly via
// `*param = ...` / `*param += ...` / `*param++`, or transitively by
// passing param to another mutating function at a mutating slot).
//
// Implements a fixpoint: keep widening per-fn sets until nothing
// changes. The state is small (map[string]map[int]bool); for normal
// programs this converges in 2-3 iterations.
func computeMutatingParams(prog *ast.Program) map[string]map[int]bool {
	// fns: bare-name -> *FuncDecl. Used to resolve call targets.
	fns := map[string]*ast.FuncDecl{}

	for _, n := range prog.Stmts {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Body != nil {
			fns[fn.Name] = fn
		}
	}

	mutators := map[string]map[int]bool{}

	changed := true
	for changed {
		changed = false

		for name, fn := range fns {
			set := scanMutatingParams(fn, mutators)

			before := mutators[name]
			if !sameIntSet(before, set) {
				mutators[name] = set
				changed = true
			}
		}
	}

	return mutators
}

// scanMutatingParams walks fn's body and returns the set of param
// indices that are written through. `mutators` is the current
// fixpoint state for transitive propagation.
func scanMutatingParams(fn *ast.FuncDecl, mutators map[string]map[int]bool) map[int]bool {
	paramIdx := map[string]int{}
	for i, p := range fn.Params {
		paramIdx[p.Name] = i
	}

	out := map[int]bool{}

	// A binding is "param-tainted" when it aliases a function param
	// directly. We track only this kind of taint here -- the goal is
	// to discover which PARAMETERS get written through. const-taint
	// propagation is handled by the body-pass walker.
	aliasOf := map[string]string{}

	for n, p := range paramIdx {
		_ = p
		aliasOf[n] = n // identity
	}

	mark := func(rootName string) {
		if i, ok := paramIdx[rootName]; ok {
			out[i] = true
		}
	}

	rootOf := func(name string) (string, bool) {
		seen := map[string]bool{}

		cur := name
		for !seen[cur] {
			seen[cur] = true

			next, ok := aliasOf[cur]
			if !ok {
				return cur, false
			}

			if next == cur {
				return cur, true
			}

			cur = next
		}

		return cur, false
	}

	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		switch v := node.(type) {
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.VarDecl:
			if v.Value != nil {
				if id, ok := v.Value.(*ast.Identifier); ok {
					if _, isParam := paramIdx[id.Name]; isParam {
						aliasOf[v.Name] = id.Name
					} else if root, ok2 := aliasOf[id.Name]; ok2 {
						aliasOf[v.Name] = root
					}
				}

				walk(v.Value)
			}
		case *ast.AssignStmt:
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 {
					if root, ok3 := rootOf(id.Name); ok3 {
						mark(root)
					}
				}
			}

			walk(v.Value)
			walk(v.Target)
		case *ast.AugAssignStmt:
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 {
					if root, ok3 := rootOf(id.Name); ok3 {
						mark(root)
					}
				}
			}

			walk(v.Value)
		case *ast.PostfixStmt:
			if d, ok := v.Expr.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 {
					if root, ok3 := rootOf(id.Name); ok3 {
						mark(root)
					}
				}
			}
		case *ast.IfStmt:
			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.ForStmt:
			walk(v.Init)
			walk(v.Cond)
			walk(v.Post)
			walk(v.Iter)
			walk(v.Body)
		case *ast.MatchStmt:
			walk(v.Expr)
			for _, c := range v.Cases {
				walk(c.Body)
			}

			walk(v.Default)
		case *ast.ReturnStmt:
			walk(v.Value)
		case *ast.ExprStmt:
			walk(v.Expr)
		case *ast.CallExpr:
			// Transitive: pass at slot i to a callee whose i-th param
			// is mutating means our param is also mutating.
			calleeName := callExprName(v.Func)
			if calleeName != "" {
				if calleeMut, ok := mutators[calleeName]; ok {
					for argIdx, arg := range v.Args {
						if !calleeMut[argIdx] {
							continue
						}

						if id, ok2 := arg.(*ast.Identifier); ok2 {
							if root, ok3 := rootOf(id.Name); ok3 {
								mark(root)
							}
						}
					}
				}
			}

			for _, a := range v.Args {
				walk(a)
			}
		case *ast.BinExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			walk(v.Expr)
		case *ast.AddressOfExpr:
			walk(v.Expr)
		case *ast.DerefExpr:
			walk(v.Expr)
		}
	}

	walk(fn.Body)

	return out
}

// callExprName returns the bare callee name when the call target is a
// plain identifier (the only form we resolve through the per-program
// fns map). Returns "" for method calls, indirect calls, or anything
// that doesn't reduce to a top-level fn name.
func callExprName(target ast.Node) string {
	if id, ok := target.(*ast.Identifier); ok {
		return id.Name
	}

	return ""
}

// sameIntSet reports whether two int-sets are equal. nil and empty
// are treated as the same.
func sameIntSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}

	for k := range a {
		if !b[k] {
			return false
		}
	}

	return true
}

// checkFuncForConstWrites walks fn's body, tracking which bindings
// alias `&top_level_const` and emitting DiagWriteToConst when one is
// written through (directly via *p / *p+= / *p++) or passed to a
// callee that writes through that argument position.
func (cg *CodeGen) checkFuncForConstWrites(
	fn *ast.FuncDecl, consts map[string]bool, mutators map[string]map[int]bool,
) {
	tainted := map[string]bool{}

	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		switch v := node.(type) {
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.VarDecl:
			if v.Value != nil {
				walk(v.Value)

				if isConstAddr(v.Value, consts) || isTaintedAlias(v.Value, tainted) {
					tainted[v.Name] = true
				}
			}
		case *ast.AssignStmt:
			// Detect *p = ... where p is tainted.
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"writing through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior. Drop the alias, or change the const to var if mutation is required.",
						id.Name)
				}
				// Direct write: `*&CONST = ...`. Rare but trivial to catch.
				if ao, ok2 := d.Expr.(*ast.AddressOfExpr); ok2 {
					if id, ok3 := ao.Expr.(*ast.Identifier); ok3 && consts[id.Name] {
						cg.warn(DiagWriteToConst, v.Pos(),
							"writing to top-level const %q directly: "+
								"top-level consts live in read-only storage; "+
								"this is undefined behavior.",
							id.Name)
					}
				}
			}

			walk(v.Value)
			walk(v.Target)
		case *ast.AugAssignStmt:
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"compound-assign through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior.",
						id.Name)
				}
			}

			walk(v.Value)
			walk(v.Target)
		case *ast.PostfixStmt:
			if d, ok := v.Expr.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"%s through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior.",
						v.Op, id.Name)
				}
			}
		case *ast.IfStmt:
			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.ForStmt:
			walk(v.Init)
			walk(v.Cond)
			walk(v.Post)
			walk(v.Iter)
			walk(v.Body)
		case *ast.MatchStmt:
			walk(v.Expr)
			for _, c := range v.Cases {
				walk(c.Body)
			}

			walk(v.Default)
		case *ast.ReturnStmt:
			walk(v.Value)
		case *ast.ExprStmt:
			walk(v.Expr)
		case *ast.CallExpr:
			// Inter-procedural: passing a tainted pointer to a slot
			// the callee writes through is the same UB as writing
			// directly. The leading position points at the call, the
			// message names both the alias and the called fn.
			calleeName := callExprName(v.Func)
			if calleeName != "" {
				if calleeMut, ok := mutators[calleeName]; ok {
					for argIdx, arg := range v.Args {
						if !calleeMut[argIdx] {
							continue
						}

						if isConstAddr(arg, consts) {
							ao := arg.(*ast.AddressOfExpr)
							id := ao.Expr.(*ast.Identifier)
							cg.warn(DiagWriteToConst, v.Pos(),
								"passing &%s to %s at parameter %d, which writes through that pointer: "+
									"top-level consts live in read-only storage; "+
									"this is undefined behavior.",
								id.Name, calleeName, argIdx+1)

							continue
						}

						if id, ok2 := arg.(*ast.Identifier); ok2 && tainted[id.Name] {
							cg.warn(DiagWriteToConst, v.Pos(),
								"passing %q (a pointer aliased to top-level const) to %s at parameter %d, which writes through that pointer: "+
									"top-level consts live in read-only storage; "+
									"this is undefined behavior.",
								id.Name, calleeName, argIdx+1)
						}
					}
				}
			}

			for _, a := range v.Args {
				walk(a)
			}
		case *ast.BinExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			walk(v.Expr)
		case *ast.AddressOfExpr:
			walk(v.Expr)
		case *ast.DerefExpr:
			walk(v.Expr)
		case *ast.LambdaExpr:
			// Recurse into lambda bodies. The taint set is intentionally
			// not propagated across the closure boundary -- captured
			// outer bindings would need an explicit alias inside the
			// lambda to reactivate detection. This matches Go's vet:
			// pointer-to-const writes inside a closure are flagged when
			// the closure itself takes a pointer-to-const.
			walk(v.Body)
		}
	}

	walk(fn.Body)
}

// isConstAddr returns true when expr is `&IDENT` and IDENT names a
// top-level const declared in this program.
func isConstAddr(expr ast.Node, consts map[string]bool) bool {
	ao, ok := expr.(*ast.AddressOfExpr)
	if !ok {
		return false
	}

	id, ok := ao.Expr.(*ast.Identifier)
	if !ok {
		return false
	}

	return consts[id.Name]
}

// isTaintedAlias returns true when expr is an Identifier that names a
// previously-tainted binding (so `let p2 = p1` propagates the taint
// when p1 was already tainted).
func isTaintedAlias(expr ast.Node, tainted map[string]bool) bool {
	id, ok := expr.(*ast.Identifier)
	if !ok {
		return false
	}

	return tainted[id.Name]
}
