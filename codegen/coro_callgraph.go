package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func coroVersionName(name string) string { return name + "$coro" }

// recordCallees walks an AST node tree and appends every directly-called
// function name to *out. This is used to build the call graph before coloring.
func recordCallees(n ast.Node, out *[]string) {
	if n == nil {
		return
	}

	switch v := n.(type) {
	case *ast.CallExpr:
		switch fn := v.Func.(type) {
		case *ast.Identifier:
			*out = append(*out, fn.Name)
		case *ast.ScopeAccess:
			// pkg::name(...) -> "pkg__name", matching the IR symbol
			// packages.go installs.  Multi-segment paths join with
			// the same double-underscore separator.
			if len(fn.Path) > 0 {
				name := fn.Path[0]
				for _, p := range fn.Path[1:] {
					name += "__" + p
				}
				*out = append(*out, name)
			}
		case *ast.FieldAccess:
			// Method / static call: type info is not available here
			// (recordCallees runs pre-typecheck on the raw AST), so
			// emit the bare field name.  callees consumed by
			// computeSpawnerReachable also alias funcDecls under
			// bare method names (see decls_struct.go), so the edge
			// resolves later.
			*out = append(*out, fn.Field)
		}

		recordCallees(v.Func, out)

		for _, a := range v.Args {
			recordCallees(a, out)
		}
	case *ast.Block:
		for _, s := range v.Stmts {
			recordCallees(s, out)
		}
	case *ast.ExprStmt:
		recordCallees(v.Expr, out)
	case *ast.VarDecl:
		recordCallees(v.Value, out)
	case *ast.ReturnStmt:
		recordCallees(v.Value, out)
	case *ast.AssignStmt:
		recordCallees(v.Target, out)
		recordCallees(v.Value, out)
	case *ast.AugAssignStmt:
		recordCallees(v.Target, out)
		recordCallees(v.Value, out)
	case *ast.IfStmt:
		recordCallees(v.Cond, out)

		for _, s := range v.Then.Stmts {
			recordCallees(s, out)
		}

		for _, elif := range v.ElseIfs {
			recordCallees(elif.Cond, out)

			for _, s := range elif.Body.Stmts {
				recordCallees(s, out)
			}
		}

		if v.Else != nil {
			for _, s := range v.Else.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.ForStmt:
		recordCallees(v.Cond, out)
		recordCallees(v.Init, out)
		recordCallees(v.Post, out)
		recordCallees(v.Iter, out)

		if v.Body != nil {
			for _, s := range v.Body.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.BinExpr:
		recordCallees(v.Left, out)
		recordCallees(v.Right, out)
	case *ast.UnaryExpr:
		recordCallees(v.Expr, out)
	case *ast.IndexExpr:
		recordCallees(v.Expr, out)
		recordCallees(v.Index, out)
	case *ast.FieldAccess:
		recordCallees(v.Expr, out)
	case *ast.PipeExpr:
		recordCallees(v.Left, out)
		recordCallees(v.Right, out)
	case *ast.TernaryExpr:
		recordCallees(v.Cond, out)
		recordCallees(v.Then, out)
		recordCallees(v.Else, out)
	case *ast.EchoStmt:
		recordCallees(v.Value, out)
	case *ast.DeferStmt:
		recordCallees(v.Call, out)
	case *ast.SpawnExpr:
		recordCallees(v.Call, out)

		if v.DoBlock != nil {
			for _, s := range v.DoBlock.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.AwaitExpr:
		recordCallees(v.Future, out)
	case *ast.LambdaExpr:
		if v.Body != nil {
			recordCallees(v.Body, out)
		}
	case *ast.WhereList:
		for _, wc := range v.Clauses {
			recordCallees(wc.Body, out)
		}
	}
}

// computeSpawnerReachable populates cg.spawnerReachable: the set of
// functions whose values might escape across a fiber boundary or
// through a global slot.  A function is in the set when its body
// directly contains a spawn/await OR a write to a module-level
// global -- both of which can hand the function's allocations to a
// different thread -- OR transitively calls any such function.
//
// recordCallees feeds ScopeAccess (`pkg::name(...)`) and FieldAccess
// (`obj.m(...)`) into the call graph alongside bare identifiers, so
// method dispatch and package-qualified call edges propagate
// through the fixpoint.  Cross-context callees (package fns, generic
// monomorphizations) whose funcDecls key would not match the IR
// scope name fall under nameLooksCrossContext in funcs.go and skip
// the biased-RC fast path conservatively.
//
// Idempotent: returns the cached map if already computed.  Walks
// cg.callGraph[caller] -> [callees] in a fixpoint until no new
// callers get tagged.
func (cg *CodeGen) computeSpawnerReachable() map[string]bool {
	if cg.spawnerReachable != nil {
		return cg.spawnerReachable
	}

	out := map[string]bool{}
	// Seed: any function whose body could publish an allocation past a
	// fiber boundary, into a global, or through an extern.  An extern
	// call is opaque to the analyzer -- the foreign code might stash
	// the pointer in thread-local state, hand it to another OS thread,
	// or otherwise share it -- so we treat any function that calls
	// extern as a publishing site.
	for name, decl := range cg.funcDecls {
		if decl == nil || decl.Body == nil {
			continue
		}

		if bodyCrossesFiberBoundary(decl.Body) || cg.bodyWritesToGlobal(decl.Body) || cg.bodyCallsExtern(decl.Body) {
			out[name] = true
		}
	}

	// Fixpoint: a caller of any tagged function is also tagged.  O(N*N)
	// worst case; in practice converges in a few rounds since most
	// call chains are shallow.
	changed := true
	for changed {
		changed = false

		for caller, callees := range cg.callGraph {
			if out[caller] {
				continue
			}

			for _, callee := range callees {
				if out[callee] {
					out[caller] = true
					changed = true

					break
				}
			}
		}
	}

	cg.spawnerReachable = out

	return out
}

// computeGlobalMutators populates cg.globalMutators: for each
// module-level global, the set of functions that may mutate it
// directly or transitively (via the call graph).  Idempotent.
//
// Used by the borrow analyzer to allow `let t = some_global` to be
// classified as Borrowed when no callee in the current body's call
// closure mutates that global.  Without this map the analyzer
// rejects every global alias for safety.
func (cg *CodeGen) computeGlobalMutators() map[string]map[string]bool {
	if cg.globalMutators != nil {
		return cg.globalMutators
	}

	out := map[string]map[string]bool{}
	// Seed: each fn's direct global writes.
	for fnName, decl := range cg.funcDecls {
		if decl == nil || decl.Body == nil {
			continue
		}

		walkAST(decl.Body, func(n ast.Node) {
			var target ast.Node

			switch v := n.(type) {
			case *ast.AssignStmt:
				if v == nil {
					return
				}

				target = v.Target
			case *ast.AugAssignStmt:
				if v == nil {
					return
				}

				target = v.Target
			default:
				return
			}

			name := rootIdentifierName(target)
			if name == "" {
				return
			}

			if _, isGlobal := cg.topLevelVarPos[name]; !isGlobal {
				return
			}

			if out[name] == nil {
				out[name] = map[string]bool{}
			}

			out[name][fnName] = true
		})
	}
	// Fixpoint upward via callGraph: a caller of a mutator of g is
	// also a mutator of g.
	changed := true
	for changed {
		changed = false

		for caller, callees := range cg.callGraph {
			for _, callee := range callees {
				for _, mutators := range out {
					if mutators[callee] && !mutators[caller] {
						mutators[caller] = true
						changed = true
					}
				}
			}
		}
	}

	cg.globalMutators = out

	return out
}

// buildCallGraphEntry builds call-graph entries for a single function declaration
// and all its struct methods.
func (cg *CodeGen) buildCallGraphEntry(name string, body ast.Node) {
	if body == nil {
		return
	}

	var callees []string
	recordCallees(body, &callees)
	// deduplicate
	seen := map[string]bool{}
	for _, c := range callees {
		if !seen[c] {
			seen[c] = true
			cg.callGraph[name] = append(cg.callGraph[name], c)
		}
	}
}

// colorCallGraph runs BFS from all {#async} roots and marks every reachable
// Tin function as needing a $coro duplicate.  Also populates
// cg.coloredCallable with the union of {#async}-reachable fns and boxed-fn
// reachable fns -- those need a $colored sync variant so callers in
// cooperative context can route to a yielding body without changing the
// sync signature.  See docs/internals/fn-coloring.md (Colored variants).
func (cg *CodeGen) colorCallGraph() {
	coroWorklist := make([]string, 0)

	for name, decl := range cg.funcDecls {
		if isAsyncTag(decl.Tags) {
			coroWorklist = append(coroWorklist, name)
		}
	}

	for len(coroWorklist) > 0 {
		name := coroWorklist[0]
		coroWorklist = coroWorklist[1:]

		if cg.coroCallable[name] {
			continue
		}

		cg.coroCallable[name] = true
		for _, callee := range cg.callGraph[name] {
			if _, ok := cg.funcDecls[callee]; ok && !cg.coroCallable[callee] {
				coroWorklist = append(coroWorklist, callee)
			}
		}
	}
	// fn main() is compiled to _tin_user_main at IR level.  If the user
	// marked it {#async}, colorCallGraph sets coroCallable["main"] but
	// genFuncDeclAs checks coroCallable["_tin_user_main"] (the IR name).
	// Sync the two so the $coro variant is actually generated.
	if cg.coroCallable["main"] {
		cg.coroCallable["_tin_user_main"] = true
	}
	// Build coloredCallable.  Roots: every coroCallable fn (so any sync
	// callee reached from {#async} is available in colored form, letting
	// the caller's $coro body route through the colored variant for
	// cooperation) plus every boxed fn (slot 1 of the fat-fn-ptr needs
	// a colored body).  BFS through cg.callGraph: a colored body routes
	// its sync callees to their colored variants, so the closure
	// captures every sync helper reachable from cooperative context.
	// isColorableFn returns true when name resolves to a Tin fn with a
	// real body we can emit a $colored variant from.  Skipped:
	//   - externs (no Tin body; the $colored stub would be declared
	//     but never defined, linker rejects).
	//   - #no_autoyield-tagged fns (yields are suppressed, so the
	//     colored body would be byte-identical to the sync entry --
	//     slot 1 falls back to slot 0 via lookupColoredVariant=nil).
	isColorableFn := func(name string) bool {
		decl, ok := cg.funcDecls[name]
		if !ok {
			return false
		}

		if decl.IsExtern != "" || decl.Body == nil {
			return false
		}

		return !hasTag(decl.Tags, "no_autoyield")
	}

	// Populate coloredCallable for EVERY colorable Tin fn (top-level,
	// struct method, package-loaded).  Method calls (`obj.foo()`) and
	// qualified calls (`pkg::foo()`) aren't recorded by recordCallees,
	// so a BFS-only approach would silently lose cooperation through
	// those call sites.  Over-emission cost is bounded: a $colored
	// body for a yield-free fn is byte-identical to its sync entry;
	// the linker / LLVM global-DCE drops unreferenced symbols.
	for name := range cg.funcDecls {
		if isColorableFn(name) {
			cg.coloredCallable[name] = true
		}
	}

	if cg.coloredCallable["main"] {
		cg.coloredCallable["_tin_user_main"] = true
	}
}

// collectBoxedFns walks the AST and populates cg.boxedFns with names
// of fns that appear as VALUES anywhere in the program (referenced
// without an immediate call).  These are roots for coloredCallable
// alongside {#async}-reachable fns: a boxed fn can be invoked via
// slot 1 of its fat-fn-ptr from any cooperative caller, so it needs a
// colored variant to yield at coloring points.
//
// Detection: every Identifier whose name resolves to a known fn AND
// which is NOT the .Func of a CallExpr is treated as boxed.  This
// over-approximates (the value may flow somewhere that never
// cooperatively invokes it) but errs in the safe direction: an unused
// colored emission is dead code the linker drops.
func (cg *CodeGen) collectBoxedFns(prog *ast.Program) {
	if cg.boxedFns == nil {
		cg.boxedFns = map[string]bool{}
	}

	for _, stmt := range prog.Stmts {
		cg.walkBoxedRefs(stmt, false)
	}
}

// walkBoxedRefs is the recursive worker for collectBoxedFns.  The
// `inCalleePos` flag is true when the current node sits in the .Func
// slot of a parent CallExpr -- an identifier in callee position is a
// direct call, not a value reference, and is suppressed.
func (cg *CodeGen) walkBoxedRefs(n ast.Node, inCalleePos bool) {
	if n == nil {
		return
	}

	switch v := n.(type) {
	case *ast.Identifier:
		if inCalleePos {
			return
		}

		if _, isFn := cg.funcDecls[v.Name]; isFn {
			cg.boxedFns[v.Name] = true
		}
	case *ast.CallExpr:
		cg.walkBoxedRefs(v.Func, true)

		for _, a := range v.Args {
			cg.walkBoxedRefs(a, false)
		}
	case *ast.Block:
		for _, s := range v.Stmts {
			cg.walkBoxedRefs(s, false)
		}
	case *ast.FuncDecl:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.TestDecl:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.StructDecl:
		for _, m := range v.Methods {
			if m.Body != nil {
				cg.walkBoxedRefs(m.Body, false)
			}
		}
	case *ast.BinExpr:
		cg.walkBoxedRefs(v.Left, false)
		cg.walkBoxedRefs(v.Right, false)
	case *ast.UnaryExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.FieldAccess:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.IndexExpr:
		cg.walkBoxedRefs(v.Expr, false)
		cg.walkBoxedRefs(v.Index, false)
	case *ast.AsExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.PipeExpr:
		cg.walkBoxedRefs(v.Left, false)
		cg.walkBoxedRefs(v.Right, false)
	case *ast.TernaryExpr:
		cg.walkBoxedRefs(v.Cond, false)
		cg.walkBoxedRefs(v.Then, false)
		cg.walkBoxedRefs(v.Else, false)
	case *ast.AwaitExpr:
		cg.walkBoxedRefs(v.Future, false)
	case *ast.SpawnExpr:
		cg.walkBoxedRefs(v.Call, false)

		if v.DoBlock != nil {
			for _, s := range v.DoBlock.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.LambdaExpr:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.ArrayLit:
		for _, e := range v.Elems {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.StructLit:
		for _, f := range v.Fields {
			cg.walkBoxedRefs(f.Value, false)
		}

		for _, e := range v.Positional {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.TupleLit:
		for _, e := range v.Elems {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.VarDecl:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.TopLevelVar:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.ReturnStmt:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AssignStmt:
		cg.walkBoxedRefs(v.Target, false)
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AugAssignStmt:
		cg.walkBoxedRefs(v.Target, false)
		cg.walkBoxedRefs(v.Value, false)
	case *ast.ExprStmt:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.IfStmt:
		cg.walkBoxedRefs(v.Cond, false)

		if v.Then != nil {
			for _, s := range v.Then.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}

		for _, ei := range v.ElseIfs {
			cg.walkBoxedRefs(ei.Cond, false)

			if ei.Body != nil {
				for _, s := range ei.Body.Stmts {
					cg.walkBoxedRefs(s, false)
				}
			}
		}

		if v.Else != nil {
			for _, s := range v.Else.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.ForStmt:
		cg.walkBoxedRefs(v.Cond, false)
		cg.walkBoxedRefs(v.Init, false)
		cg.walkBoxedRefs(v.Post, false)
		cg.walkBoxedRefs(v.Iter, false)

		if v.Body != nil {
			for _, s := range v.Body.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.DeferStmt:
		cg.walkBoxedRefs(v.Call, false)
	case *ast.EchoStmt:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.WhereList:
		for _, wc := range v.Clauses {
			cg.walkBoxedRefs(wc.Body, false)
		}
	case *ast.MatchStmt:
		cg.walkBoxedRefs(v.Expr, false)

		for _, c := range v.Cases {
			if c.Guard != nil {
				cg.walkBoxedRefs(c.Guard, false)
			}

			if c.Body != nil {
				cg.walkBoxedRefs(c.Body, false)
			}
		}
	case *ast.AwaitMatchStmt:
		for _, f := range v.Futures {
			cg.walkBoxedRefs(f, false)
		}

		for _, c := range v.Cases {
			if c.Guard != nil {
				cg.walkBoxedRefs(c.Guard, false)
			}

			if c.Body != nil {
				cg.walkBoxedRefs(c.Body, false)
			}
		}

		if v.Default != nil {
			cg.walkBoxedRefs(v.Default, false)
		}
	case *ast.TryExpr:
		cg.walkBoxedRefs(v.Inner, false)
	case *ast.SliceExpr:
		cg.walkBoxedRefs(v.Expr, false)
		cg.walkBoxedRefs(v.Start, false)
		cg.walkBoxedRefs(v.End, false)
	case *ast.RangeExpr:
		cg.walkBoxedRefs(v.Start, false)
		cg.walkBoxedRefs(v.End, false)
	case *ast.ArrayFillLit:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AddrExpr:
		cg.walkBoxedRefs(v.Val, false)
	case *ast.DerefExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.AddressOfExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.InterpolatedString:
		for _, p := range v.Parts {
			if p.IsExpr {
				cg.walkBoxedRefs(p.Expr, false)
			}
		}
	case *ast.TaggedBlock:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	}
}

// $coro variant predeclaration

// predeclareCoroVariant pre-declares "name$coro(...) i8*" on the module so
