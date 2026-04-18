package codegen

// autoyield.go - Compile-time auto-yield analysis for {#async} coroutines.
//
// Automatic preemption strategy
// ==============================
// The Tin async runtime uses cooperative scheduling: fibers yield control
// explicitly via suspend points, and the scheduler round-robins the ready queue.
// Without enough yield points a compute-bound coroutine monopolises its worker
// thread and starves other fibers.
//
// The auto-yield pass inserts suspend points at compile time, at two kinds of site:
//
//   1. Loop back-edges (existing).  Every iteration of a for-loop inside a $coro
//      function emits a coro.suspend (in genYieldAutoAt).  This covers the most
//      common stall source.
//
//   2. Call sites of "auto-yield" functions (new, this file).  Before each call to
//      a function that the heuristic analysis classifies as heavy or recursive, the
//      compiler inserts a coro.suspend.  This covers long computations that happen
//      to call out to a single expensive helper rather than looping themselves.
//
// "Over-yielding" is intentional: an extra round-trip through the scheduler costs
// microseconds; a missed yield can stall a thread for milliseconds or longer.
// Users who need tighter control can annotate hot inner loops with {#no_autoyield}.
//
// Heuristic classification
// ========================
// Each Tin function receives a ComplexScore:
//
//   score = loopCount      * loopWeight      (10 per loop)
//         + allocCount     * allocWeight      ( 5 per heap alloc)
//         + callCount      * callWeight       ( 2 per non-heavy call)
//         + heavyCallCount * heavyCallWeight  (20 per call to heavy/recursive fn)
//
// A function is classified as "heavy" when:
//   - It carries the {#heavy} user annotation, OR
//   - Its score >= heavyThreshold (30).
//
// A function is classified as "recursive" when it belongs to a back-edge cycle
// in the static call graph (direct or mutual recursion).
//
// AutoYield = IsHeavy || IsRecursive.
//
// The analysis runs in three passes over funcDecls (populated during predecl):
//   Pass 1: count raw loop/alloc/call stats; mark explicit {#heavy} functions.
//   Pass 2: re-count heavyCallCount using pass-1 results; finalize scores.
//   Pass 3: DFS on callGraph to detect recursive cycles.
//
// Suppressing auto-yield
// ======================
// Tagging a function {#no_autoyield} disables all auto-yield inside it:
//   - Loop back-edges (existing behavior in genYieldAutoAt).
//   - Call-site yields (checked via cg.curFnAutoYield in genCallSiteYieldFor).
//
// -v-heuristics output
// ====================
// When cg.verboseHeuristics is true (set via -v-heuristics CLI flag), one line
// is printed to stderr per function:
//
//   [autoyield] fn <name>  loops=N allocs=N calls=N heavyCalls=N  score=N  [label]
//
// where label is one of: heavy, recursive, auto-heavy, normal.

import (
	"fmt"
	"os"
	"sort"

	"github.com/Azer0s/tin/ast"
)

// Complexity scoring weights and auto-heavy threshold.
const (
	loopWeight      = 10 // points per loop statement (ForStmt)
	allocWeight     = 5  // points per heap allocation (AddressOfExpr)
	callWeight      = 2  // points per call to a non-heavy function
	heavyCallWeight = 20 // points per call to a heavy/recursive function
	heavyThreshold  = 30 // score >= this -> function is classified as auto-heavy
)

// FuncHeuristicInfo stores the heuristic analysis results for one function.
// Populated by computeAutoYieldHeuristics; consulted at call-site yield points.
type FuncHeuristicInfo struct {
	// Name is the function name as it appears in funcDecls (may include struct prefix,
	// e.g. "counter_add" for a method, or a bare name like "fibonacci").
	Name string

	// Raw AST metrics.
	LoopCount      int // number of ForStmt nodes in the body
	AllocCount     int // number of AddressOfExpr (heap) nodes in the body
	CallCount      int // number of CallExpr to non-heavy callees
	HeavyCallCount int // number of CallExpr to heavy/recursive callees

	// Derived.
	ComplexScore int  // loopCount*loopWeight + allocCount*allocWeight + ...
	IsHeavy      bool // {#heavy} tag present, OR ComplexScore >= heavyThreshold
	IsRecursive  bool // belongs to a recursive cycle in the static call graph
	AutoYield    bool // IsHeavy || IsRecursive; emit coro.suspend before calls to this fn
}

// bodyStats accumulates raw counts while walking a function body AST.
type bodyStats struct {
	loops  int
	allocs int
	calls  int
}

// collectBodyStats recursively counts ForStmt, AddressOfExpr, and CallExpr nodes
// in the AST rooted at n. It does NOT recurse into nested lambda/spawn bodies
// because those are separate function scopes with their own heuristic entries.
func collectBodyStats(n ast.Node, s *bodyStats) {
	if n == nil {
		return
	}

	switch v := n.(type) {
	case *ast.ForStmt:
		s.loops++

		collectBodyStats(v.Init, s)
		collectBodyStats(v.Cond, s)
		collectBodyStats(v.Post, s)
		collectBodyStats(v.Iter, s)

		if v.Body != nil {
			for _, stmt := range v.Body.Stmts {
				collectBodyStats(stmt, s)
			}
		}
	case *ast.AddressOfExpr:
		s.allocs++

		collectBodyStats(v.Expr, s)
	case *ast.CallExpr:
		s.calls++

		collectBodyStats(v.Func, s)

		for _, a := range v.Args {
			collectBodyStats(a, s)
		}
	case *ast.Block:
		for _, stmt := range v.Stmts {
			collectBodyStats(stmt, s)
		}
	case *ast.ExprStmt:
		collectBodyStats(v.Expr, s)
	case *ast.VarDecl:
		collectBodyStats(v.Value, s)
	case *ast.ReturnStmt:
		collectBodyStats(v.Value, s)
	case *ast.AssignStmt:
		collectBodyStats(v.Target, s)
		collectBodyStats(v.Value, s)
	case *ast.AugAssignStmt:
		collectBodyStats(v.Target, s)
		collectBodyStats(v.Value, s)
	case *ast.PostfixStmt:
		collectBodyStats(v.Expr, s)
	case *ast.IfStmt:
		collectBodyStats(v.Cond, s)

		if v.Then != nil {
			for _, stmt := range v.Then.Stmts {
				collectBodyStats(stmt, s)
			}
		}

		for _, elif := range v.ElseIfs {
			collectBodyStats(elif.Cond, s)

			for _, stmt := range elif.Body.Stmts {
				collectBodyStats(stmt, s)
			}
		}

		if v.Else != nil {
			for _, stmt := range v.Else.Stmts {
				collectBodyStats(stmt, s)
			}
		}
	case *ast.MatchStmt:
		collectBodyStats(v.Expr, s)

		for _, c := range v.Cases {
			if c.Body != nil {
				for _, stmt := range c.Body.Stmts {
					collectBodyStats(stmt, s)
				}
			}
		}

		if v.Default != nil {
			for _, stmt := range v.Default.Stmts {
				collectBodyStats(stmt, s)
			}
		}
	case *ast.BinExpr:
		collectBodyStats(v.Left, s)
		collectBodyStats(v.Right, s)
	case *ast.UnaryExpr:
		collectBodyStats(v.Expr, s)
	case *ast.IndexExpr:
		collectBodyStats(v.Expr, s)
		collectBodyStats(v.Index, s)
	case *ast.FieldAccess:
		collectBodyStats(v.Expr, s)
	case *ast.PipeExpr:
		collectBodyStats(v.Left, s)
		collectBodyStats(v.Right, s)
	case *ast.TernaryExpr:
		collectBodyStats(v.Cond, s)
		collectBodyStats(v.Then, s)
		collectBodyStats(v.Else, s)
	case *ast.EchoStmt:
		collectBodyStats(v.Value, s)
	case *ast.DeferStmt:
		collectBodyStats(v.Call, s)
	case *ast.AwaitExpr:
		collectBodyStats(v.Future, s)
	case *ast.AddrExpr:
		collectBodyStats(v.Val, s)
	case *ast.DerefExpr:
		collectBodyStats(v.Expr, s)
	case *ast.TypeAssertExpr:
		collectBodyStats(v.Expr, s)
	case *ast.AsExpr:
		collectBodyStats(v.Expr, s)
	case *ast.SliceExpr:
		collectBodyStats(v.Expr, s)
		collectBodyStats(v.Start, s)
		collectBodyStats(v.End, s)
	case *ast.ArrayLit:
		for _, e := range v.Elems {
			collectBodyStats(e, s)
		}
	case *ast.StructLit:
		for _, f := range v.Fields {
			collectBodyStats(f.Value, s)
		}

		for _, p := range v.Positional {
			collectBodyStats(p, s)
		}
	case *ast.TupleLit:
		for _, e := range v.Elems {
			collectBodyStats(e, s)
		}
	case *ast.WhereList:
		for _, wc := range v.Clauses {
			collectBodyStats(wc.Body, s)
		}
	case *ast.InterpolatedString:
		for _, part := range v.Parts {
			if part.IsExpr {
				collectBodyStats(part.Expr, s)
			}
		}
	// SpawnExpr and LambdaExpr introduce separate function scopes; count the
	// spawn/lambda call itself but do NOT recurse into their bodies here.
	case *ast.SpawnExpr:
		s.calls++
	case *ast.LambdaExpr:
		// lambda bodies are their own scope; don't count them here
	}
}

// dfsState tracks DFS visiting state for cycle detection.
type dfsState int

const (
	dfsUnvisited dfsState = iota
	dfsActive             // currently on the DFS call stack
	dfsFinished           // fully processed
)

// detectRecursiveFunctions runs DFS on the call graph and returns the set of
// function names that belong to at least one cycle (direct or mutual recursion).
//
// Only functions whose names appear in known are examined; calls to unknown names
// (stdlib, externs) are ignored.
func detectRecursiveFunctions(callGraph map[string][]string, known map[string]bool) map[string]bool {
	state := make(map[string]dfsState, len(known))
	recursive := make(map[string]bool)

	// stack holds the current DFS path for back-edge detection.
	var stack []string

	var dfs func(name string)

	dfs = func(name string) {
		state[name] = dfsActive
		stack = append(stack, name)

		for _, callee := range callGraph[name] {
			if !known[callee] {
				continue
			}

			switch state[callee] {
			case dfsActive:
				// Back-edge: callee is on the current stack -> cycle.
				// Mark every function on the stack from callee through name (the
				// current frame) as recursive.  The cycle path is:
				//   callee -> ... -> name -> callee
				// so both callee and name, plus everything between them, are part
				// of the cycle.
				inCycle := false

				for _, n := range stack {
					if n == callee {
						inCycle = true
					}

					if inCycle {
						recursive[n] = true
					}
				}
			case dfsUnvisited:
				dfs(callee)
			case dfsFinished:
				// Already fully explored; no action needed.
			}
		}

		stack = stack[:len(stack)-1]
		state[name] = dfsFinished
	}

	for name := range known {
		if state[name] == dfsUnvisited {
			dfs(name)
		}
	}

	return recursive
}

// computeAutoYieldHeuristics analyses all declared functions and populates
// cg.funcHeuristics. Must be called after colorCallGraph() so that callGraph
// and funcDecls are fully populated.
//
// The algorithm runs in three passes (see file header for details).
func (cg *CodeGen) computeAutoYieldHeuristics(prog *ast.Program) {
	// Collect names in sorted order for deterministic output.
	fnNames := make([]string, 0, len(cg.funcDecls))

	for name := range cg.funcDecls {
		fnNames = append(fnNames, name)
	}

	sort.Strings(fnNames)

	// Build a set for O(1) membership tests in detectRecursiveFunctions.
	knownFns := make(map[string]bool, len(fnNames))

	for _, name := range fnNames {
		knownFns[name] = true
	}

	// --- Pass 1: raw AST metrics + explicit {#heavy} detection ---

	type pass1Result struct {
		stats   bodyStats
		isHeavy bool // {#heavy} tag or score >= threshold
		noYield bool // {#no_autoyield} tag
	}

	pass1 := make(map[string]*pass1Result, len(fnNames))

	for _, name := range fnNames {
		decl := cg.funcDecls[name]
		r := &pass1Result{
			noYield: hasTag(decl.Tags, "no_autoyield"),
		}

		if decl.Body != nil {
			collectBodyStats(decl.Body, &r.stats)
		}

		r.isHeavy = hasTag(decl.Tags, "heavy")

		if !r.isHeavy {
			score := r.stats.loops*loopWeight +
				r.stats.allocs*allocWeight +
				r.stats.calls*callWeight

			if score >= heavyThreshold {
				r.isHeavy = true
			}
		}

		pass1[name] = r
	}

	// --- Pass 2: refine using caller/callee heaviness ---
	// Re-classify each function's calls as heavy vs normal based on pass-1 results.

	for _, name := range fnNames {
		r := pass1[name]
		heavyCalls := 0
		normalCalls := 0

		for _, callee := range cg.callGraph[name] {
			if ci, ok := pass1[callee]; ok && ci.isHeavy {
				heavyCalls++
			} else {
				normalCalls++
			}
		}

		r.stats.calls = normalCalls

		score := r.stats.loops*loopWeight +
			r.stats.allocs*allocWeight +
			r.stats.calls*callWeight +
			heavyCalls*heavyCallWeight

		if score >= heavyThreshold {
			r.isHeavy = true
		}

		pass1[name].stats.calls = normalCalls

		info := &FuncHeuristicInfo{
			Name:           name,
			LoopCount:      r.stats.loops,
			AllocCount:     r.stats.allocs,
			CallCount:      normalCalls,
			HeavyCallCount: heavyCalls,
			ComplexScore:   score,
			IsHeavy:        r.isHeavy,
		}

		cg.funcHeuristics[name] = info
	}

	// --- Pass 3: detect recursive cycles ---

	recursive := detectRecursiveFunctions(cg.callGraph, knownFns)

	for _, name := range fnNames {
		info := cg.funcHeuristics[name]
		info.IsRecursive = recursive[name]
		info.AutoYield = info.IsHeavy || info.IsRecursive
	}

	// --- Verbose output ---

	if !cg.verboseHeuristics {
		return
	}

	for _, name := range fnNames {
		info := cg.funcHeuristics[name]
		decl := cg.funcDecls[name]

		label := "normal"

		switch {
		case hasTag(decl.Tags, "heavy"):
			label = "heavy"
		case info.IsRecursive:
			label = "recursive"
		case info.IsHeavy:
			label = "auto-heavy"
		}

		fmt.Fprintf(os.Stderr,
			"[autoyield] fn %-40s  loops=%-3d allocs=%-3d calls=%-3d heavyCalls=%-3d  score=%-4d  [%s]\n",
			name,
			info.LoopCount,
			info.AllocCount,
			info.CallCount,
			info.HeavyCallCount,
			info.ComplexScore,
			label,
		)
	}
}
