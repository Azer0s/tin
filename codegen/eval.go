package codegen

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Compile-time value type

// ctfeVal holds a single compile-time constant value produced by CTFE.
//
// Slice-typed CTFE values use kind="slice"; the elemKind names the element
// scalar (one of i64/f64/bool/string) and elems carries the homogeneous
// per-element ctfeVal payload. Slices flow into tier-2 dispatch via the
// libffi shim; expansion to the wrapper's (T*, i64) C-ABI shape happens at
// the marshal boundary using cg.classifyInteropParam.
type ctfeVal struct {
	kind     string // "i64", "f64", "bool", "string", "slice"
	i        int64
	f        float64
	b        bool
	s        string
	elemKind string    // for kind=="slice": element scalar kind
	elems    []ctfeVal // for kind=="slice": homogeneous element payload
}

// ctfeReturn is a sentinel used to carry a return value out of evalBody.
type ctfeReturn struct{ val ctfeVal }

func (r ctfeReturn) Error() string { return "ctfe-return" }

// errNotConst signals that an expression is not a compile-time constant.
var errNotConst = errors.New("not a compile-time constant")

// errCTFEBudget signals the per-call evaluation budget was exhausted. The
// caller treats it identically to errNotConst (the call falls back to
// runtime emission); the distinct value lets internal call sites tell
// "structurally not foldable" from "complexity ceiling reached" when
// surfacing diagnostics.
var errCTFEBudget = errors.New("CTFE budget exhausted")

// maxCTFEIter is the maximum number of loop iterations during CTFE to prevent
// infinite loops when the loop bound is not statically knowable.
const maxCTFEIter = 100_000

// maxCTFEDepth is the maximum call-stack depth during CTFE.
const maxCTFEDepth = 256

// defaultPureFoldBudget is the default per-call node-evaluation budget.
// One million covers any reasonable compile-time computation (factorial of
// 20, fib(30), small interpreter loops) while keeping a runaway #pure body
// from hanging the compiler. Override via cg.SetPureFoldBudget or the
// `--pure-fold-budget=N` CLI flag.
const defaultPureFoldBudget = 1_000_000

// Entry point

// tryEvalPureCall attempts to evaluate a call to a #pure #no_recurse function
// whose arguments are all compile-time constants. On success it returns the
// constant LLVM value that replaces the call instruction. Returns nil (and no
// error) if the call cannot be evaluated at compile time.
//
// Honors the cg.pureFoldDisabled flag: a `--no-pure-fold` build skips the
// evaluator entirely, leaving the original CallExpr to codegen as a
// runtime call.
func (cg *CodeGen) tryEvalPureCall(call *ast.CallExpr) (value.Value, error) {
	if cg.pureFoldDisabled {
		return nil, nil
	}

	val, fd, ok := cg.tryEvalPureCallToCtfeVal(call)
	if !ok {
		return nil, nil
	}

	llVal, llErr := ctfeValToLLVM(val, fd, cg)
	if llErr == nil && llVal != nil {
		cg.progress("ctfe " + fd.Name + "()")
	}

	return llVal, llErr
}

// ctfeMemoEntry caches a CTFE result. ok=false means the call was attempted
// and failed; we cache that too so repeated bail-outs are cheap.
type ctfeMemoEntry struct {
	val ctfeVal
	fd  *ast.FuncDecl
	ok  bool
}

// ctfeCacheKey builds a stable string fingerprint of (callee, arg values).
// Args that aren't constants make the call un-cacheable; we return "" then
// and the caller skips the cache.
func ctfeCacheKey(calleeName string, args []ctfeVal) string {
	var b strings.Builder
	b.WriteString(calleeName)
	b.WriteByte('|')

	for _, a := range args {
		b.WriteString(a.kind)
		b.WriteByte(':')

		switch a.kind {
		case "i64":
			fmt.Fprintf(&b, "%d", a.i)
		case "f64":
			fmt.Fprintf(&b, "%v", a.f)
		case "bool":
			if a.b {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		case "string":
			b.WriteString(a.s)
		}

		b.WriteByte(',')
	}

	return b.String()
}

// tryEvalPureCallToCtfeVal is the shared core: it gates on #pure / #no_recurse,
// evaluates args through the AST interpreter, and runs the body. Returns the
// raw ctfeVal so callers that don't need an LLVM value (e.g. tryFoldExpr) can
// avoid round-tripping through the IR. Returns ok=false on any failure.
//
// Memoizes successful and failed evaluations in cg.ctfeCache keyed on
// (function name, argument fingerprint). A given pure call with literal args
// hits the body walker exactly once per compilation unit; subsequent calls
// reuse the cached result.
func (cg *CodeGen) tryEvalPureCallToCtfeVal(call *ast.CallExpr) (ctfeVal, *ast.FuncDecl, bool) {
	// Gate ALL fold paths (tryEvalPureCall, tryFoldPureCall, BinExpr/UnaryExpr
	// recursive folds) on the `--no-pure-fold` flag. The setter lives on the
	// shared core so a single check covers both the LLVM-emit fold driver
	// and the warning-only fold used by static analysis.
	if cg.pureFoldDisabled {
		return ctfeVal{}, nil, false
	}

	calleeName := resolveCalleeName(call)
	if calleeName == "" || strings.Contains(calleeName, "::") || strings.HasPrefix(calleeName, ".") {
		return ctfeVal{}, nil, false
	}

	fd, ok := cg.funcDecls[calleeName]
	if !ok {
		return ctfeVal{}, nil, false
	}

	// #pure is the user-facing contract; the evaluator's 256-frame depth
	// limit (see maxCTFEDepth) bounds recursion safely, so we no longer
	// require the explicit #no_recurse tag.
	if !hasTag(fd.Tags, "pure") {
		return ctfeVal{}, nil, false
	}

	// Functions with a {#allow_sideffect} block can't be CTFE'd: the block
	// must run for its side effects and the evaluator can't simulate them.
	if bodyHasAllowSideffect(fd.Body) {
		return ctfeVal{}, nil, false
	}

	if len(fd.TypeParams) > 0 {
		return ctfeVal{}, nil, false
	}

	env := make(map[string]ctfeVal)
	argVals := make([]ctfeVal, 0, len(call.Args))

	// Reset the per-call complexity budget. Argument evaluation participates
	// in the budget too: a caller passing a #pure(arg) whose `arg` itself
	// expands into a multi-million-node fold must not bypass the limit by
	// virtue of being an argument rather than a body.
	budget := cg.pureFoldBudget
	if budget <= 0 {
		budget = defaultPureFoldBudget
	}

	cg.pureFoldBudgetRemaining = budget

	for i, argNode := range call.Args {
		val, err := evalNode(argNode, env, cg, 0)
		if err != nil {
			return ctfeVal{}, nil, false
		}

		argVals = append(argVals, val)

		if i < len(fd.Params) {
			env[fd.Params[i].Name] = val
		}
	}

	cacheKey := ctfeCacheKey(calleeName, argVals)
	if cached, hit := cg.ctfeCache[cacheKey]; hit {
		return cached.val, cached.fd, cached.ok
	}

	result, err := evalBody(fd.Body, env, cg, 0)
	if err == nil {
		cg.ctfeCache[cacheKey] = ctfeMemoEntry{val: result, fd: fd, ok: true}

		return result, fd, true
	}

	// Tier-1 (AST evaluator) failed. Try tier-2 (dispatch through the
	// pre-compiled .so) before giving up. Only fires when the cache is
	// populated (TIN_PURE_FN_CACHE=1) AND the function's signature fits
	// the i64 marshal protocol - string/float/struct types need the
	// #interop wrapper machinery and are out of scope until we tie the
	// cache emit into emitInteropWrapperFor.
	if dispatchVal, ok := cg.tryDispatchPureCall(fd, argVals); ok {
		cg.ctfeCache[cacheKey] = ctfeMemoEntry{val: dispatchVal, fd: fd, ok: true}

		return dispatchVal, fd, true
	}

	cg.ctfeCache[cacheKey] = ctfeMemoEntry{ok: false}

	return ctfeVal{}, nil, false
}

// tryDispatchPureCall attempts the tier-2 fallback: dlopen the .so cached at
// .build/pure-fn/<merkle>/bin.so, dlsym its `__tin_pure_shim_<name>` entry,
// and invoke the function via libffi using the shim's actual signature.
// Returns ok=false silently when any step is unavailable (no cache, type
// outside the libffi marshal subset, hash unresolvable).
func (cg *CodeGen) tryDispatchPureCall(fd *ast.FuncDecl, argVals []ctfeVal) (ctfeVal, bool) {
	hash := cg.ctfeFnHash(fd)
	if hash == "" {
		return ctfeVal{}, false
	}

	if !ctfeCacheHit(hash) {
		return ctfeVal{}, false
	}

	h, err := LoadPureFn(hash, pureFnShimName(fd.Name))
	if err != nil {
		return ctfeVal{}, false
	}

	return InvokePureShim(h, fd, argVals)
}

// Body evaluator

// evalBody evaluates a function body (Block, WhereList, or expression) and
// returns the result. Returns errNotConst if the body cannot be fully evaluated.
//
// evalBody is the only frame that unwraps the ctfeReturn sentinel. evalBlock
// and evalIf propagate it upward so that an early `return` inside a nested
// if/for branch correctly aborts every enclosing block until reaching the
// function-body frame.
func evalBody(body ast.Node, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if depth > maxCTFEDepth {
		return ctfeVal{}, errNotConst
	}

	val, err := evalBodyRaw(body, env, cg, depth)
	if err != nil {
		var ret ctfeReturn
		if errors.As(err, &ret) {
			return ret.val, nil
		}

		return ctfeVal{}, err
	}

	return val, nil
}

// evalBodyRaw is evalBody without the ctfeReturn unwrap; it dispatches on the
// body shape and forwards every error (including ctfeReturn) to its caller.
func evalBodyRaw(body ast.Node, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	switch v := body.(type) {
	case *ast.Block:
		return evalBlock(v, copyEnv(env), cg, depth)
	case *ast.WhereList:
		return evalWhereList(v, env, cg, depth)
	default:
		return evalNode(body, env, cg, depth)
	}
}

// evalBranch runs a nested block (then/else arm of an IfStmt, body of a for
// loop, etc.) and merges mutations to outer-scope variables back into env.
// New bindings introduced inside the branch are discarded so they don't leak.
// ctfeReturn (and any other error) is propagated unchanged.
func evalBranch(blk *ast.Block, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	inner := copyEnv(env)

	val, err := evalBlock(blk, inner, cg, depth)
	for k := range env {
		if cv, ok := inner[k]; ok {
			env[k] = cv
		}
	}

	return val, err
}

// evalBlock runs a list of statements. A ctfeReturn sentinel from any
// statement aborts the block and is propagated unchanged to the caller; only
// evalBody unwraps it. This ensures `return` inside an if/for/where branch
// correctly short-circuits every enclosing block frame.
func evalBlock(blk *ast.Block, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if blk == nil {
		return ctfeVal{kind: "i64"}, nil
	}

	for _, stmt := range blk.Stmts {
		if _, err := evalNode(stmt, env, cg, depth); err != nil {
			return ctfeVal{}, err
		}
	}

	return ctfeVal{kind: "i64"}, nil
}

// evalWhereList evaluates a where-clause list, returning the first matching body.
func evalWhereList(wl *ast.WhereList, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	for _, clause := range wl.Clauses {
		if clause.Cond == nil {
			// Wildcard "_" - always matches.
			return evalNode(clause.Body, env, cg, depth)
		}

		cv, err := evalNode(clause.Cond, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		match := false

		switch cv.kind {
		case "bool":
			match = cv.b
		case "i64":
			match = cv.i != 0
		}

		if match {
			return evalNode(clause.Body, env, cg, depth)
		}
	}

	return ctfeVal{kind: "i64"}, nil
}

// Node evaluator

// evalNode evaluates any AST node as a statement or expression.
// For statement nodes (VarDecl, AssignStmt, etc.) it mutates env and returns zero.
// For expression nodes it returns the computed value.
// Return statements propagate via ctfeReturn error sentinel.
func evalNode(node ast.Node, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if node == nil {
		return ctfeVal{kind: "i64"}, nil
	}

	// Per-call complexity budget. The counter is set in
	// tryEvalPureCallToCtfeVal at top-level entry; nested calls share the
	// remaining budget so a recursive walk that explodes node-count gets
	// caught. When exhausted we unwind exactly like errNotConst, but with
	// the distinct errCTFEBudget so call sites can tell the two apart.
	if cg != nil {
		if cg.pureFoldBudgetRemaining <= 0 {
			return ctfeVal{}, errCTFEBudget
		}

		cg.pureFoldBudgetRemaining--
	}

	switch v := node.(type) {
	// Literals
	case *ast.IntLit:
		if v.Big != nil {
			// ctfeVal stores i64; cannot represent oversized literal.
			return ctfeVal{}, errNotConst
		}

		return ctfeVal{kind: "i64", i: v.Value}, nil
	case *ast.FloatLit:
		return ctfeVal{kind: "f64", f: v.Value}, nil
	case *ast.BoolLit:
		return ctfeVal{kind: "bool", b: v.Value}, nil
	case *ast.StringLit:
		return ctfeVal{kind: "string", s: v.Value}, nil
	case *ast.CharLit:
		return ctfeVal{kind: "i64", i: int64(v.Value)}, nil

	case *ast.ArrayLit:
		// Evaluate every element; require they share a single scalar kind so
		// the slice can be marshaled through the (T*, i64) wrapper boundary.
		// Mixed-kind arrays (e.g. union of i64+string) bail to AST-eval failure.
		if len(v.Elems) == 0 {
			return ctfeVal{kind: "slice", elemKind: "i64", elems: nil}, nil
		}

		elems := make([]ctfeVal, 0, len(v.Elems))

		var elemKind string

		for _, e := range v.Elems {
			ev, err := evalNode(e, env, cg, depth)
			if err != nil {
				return ctfeVal{}, err
			}

			if elemKind == "" {
				elemKind = ev.kind
			} else if ev.kind != elemKind {
				return ctfeVal{}, errNotConst
			}

			elems = append(elems, ev)
		}

		return ctfeVal{kind: "slice", elemKind: elemKind, elems: elems}, nil

	// Variables
	case *ast.Identifier:
		if cv, ok := env[v.Name]; ok {
			return cv, nil
		}

		return ctfeVal{}, errNotConst

	// Declarations / assignments (mutate env)
	case *ast.VarDecl:
		if v.Value != nil {
			val, err := evalNode(v.Value, env, cg, depth)
			if err != nil {
				return ctfeVal{}, err
			}

			env[v.Name] = val
		}

		return ctfeVal{kind: "i64"}, nil

	case *ast.AssignStmt:
		val, err := evalNode(v.Value, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		if id, ok := v.Target.(*ast.Identifier); ok {
			env[id.Name] = val
		} else {
			return ctfeVal{}, errNotConst // indexed or field assignment - bail
		}

		return ctfeVal{kind: "i64"}, nil

	case *ast.AugAssignStmt:
		id, ok := v.Target.(*ast.Identifier)
		if !ok {
			return ctfeVal{}, errNotConst
		}

		cur, ok2 := env[id.Name]
		if !ok2 {
			return ctfeVal{}, errNotConst
		}

		rhs, err := evalNode(v.Value, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}
		// Strip trailing '=' to get the operator (e.g. "+=" -> "+")
		baseOp := strings.TrimSuffix(v.Op, "=")

		result, err := evalBinOp(cur, baseOp, rhs)
		if err != nil {
			return ctfeVal{}, err
		}

		env[id.Name] = result

		return ctfeVal{kind: "i64"}, nil

	case *ast.PostfixStmt:
		if id, ok := v.Expr.(*ast.Identifier); ok {
			cur, ok2 := env[id.Name]
			if !ok2 {
				return ctfeVal{}, errNotConst
			}

			old := cur
			if v.Op == "++" {
				cur.i++
			} else {
				cur.i--
			}

			env[id.Name] = cur

			return old, nil
		}

		return ctfeVal{}, errNotConst

	case *ast.UnaryExpr:
		if v.Post {
			return ctfeVal{}, errNotConst // postfix handled by PostfixStmt
		}

		operand, err := evalNode(v.Expr, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		switch v.Op {
		case "-":
			switch operand.kind {
			case "i64":
				return ctfeVal{kind: "i64", i: -operand.i}, nil
			case "f64":
				return ctfeVal{kind: "f64", f: -operand.f}, nil
			}
		case "!":
			if operand.kind == "bool" {
				return ctfeVal{kind: "bool", b: !operand.b}, nil
			}
		case "~":
			if operand.kind == "i64" {
				return ctfeVal{kind: "i64", i: ^operand.i}, nil
			}
		}

		return ctfeVal{}, errNotConst

	// Binary expressions
	case *ast.BinExpr:
		// Short-circuit for &&/||
		if v.Op == "&&" || v.Op == "||" {
			left, err := evalNode(v.Left, env, cg, depth)
			if err != nil {
				return ctfeVal{}, err
			}

			if left.kind != "bool" {
				return ctfeVal{}, errNotConst
			}

			if v.Op == "&&" && !left.b {
				return ctfeVal{kind: "bool", b: false}, nil
			}

			if v.Op == "||" && left.b {
				return ctfeVal{kind: "bool", b: true}, nil
			}

			return evalNode(v.Right, env, cg, depth)
		}

		left, err := evalNode(v.Left, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		right, err := evalNode(v.Right, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		return evalBinOp(left, v.Op, right)

	// Ternary
	case *ast.TernaryExpr:
		cond, err := evalNode(v.Cond, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		if cond.kind != "bool" {
			return ctfeVal{}, errNotConst
		}

		if cond.b {
			return evalNode(v.Then, env, cg, depth)
		}

		return evalNode(v.Else, env, cg, depth)

	// Control flow
	case *ast.ReturnStmt:
		val, err := evalNode(v.Value, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		return ctfeVal{}, ctfeReturn{val: val}

	case *ast.IfStmt:
		cond, err := evalNode(v.Cond, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		if cond.kind != "bool" {
			return ctfeVal{}, errNotConst
		}

		if cond.b {
			return evalBranch(v.Then, env, cg, depth)
		}

		for _, ei := range v.ElseIfs {
			eCond, err2 := evalNode(ei.Cond, env, cg, depth)
			if err2 != nil {
				return ctfeVal{}, err2
			}

			if eCond.kind != "bool" {
				return ctfeVal{}, errNotConst
			}

			if eCond.b {
				return evalBranch(ei.Body, env, cg, depth)
			}
		}

		if v.Else != nil {
			return evalBranch(v.Else, env, cg, depth)
		}

		return ctfeVal{kind: "i64"}, nil

	case *ast.ForStmt:
		return evalForStmt(v, env, cg, depth)

	case *ast.MatchStmt:
		// Evaluate the discriminant, then walk cases looking for a literal-
		// pattern match. Type-pattern (`case _ T:`) and struct-pattern arms
		// can't be modeled without runtime type info; we bail to errNotConst
		// and let the runtime path handle them.
		disc, err := evalNode(v.Expr, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		for _, c := range v.Cases {
			matched, err := matchCtfePattern(c.Pattern, disc, env, cg, depth)
			if err != nil {
				return ctfeVal{}, err
			}

			if !matched {
				continue
			}

			if c.Guard != nil {
				gv, gerr := evalNode(c.Guard, env, cg, depth)
				if gerr != nil {
					return ctfeVal{}, gerr
				}

				if gv.kind != "bool" || !gv.b {
					continue
				}
			}

			if c.VarName != "" {
				env[c.VarName] = disc
			}

			return evalBranch(c.Body, env, cg, depth)
		}

		if v.Default != nil {
			return evalBranch(v.Default, env, cg, depth)
		}

		return ctfeVal{}, errNotConst

	// Block / tagged block
	case *ast.Block:
		return evalBlock(v, copyEnv(env), cg, depth)

	case *ast.TaggedBlock:
		// #allow_sideffect blocks may contain echo - bail on CTFE.
		if hasTag(v.Tags, "allow_sideffect") {
			return ctfeVal{}, errNotConst
		}

		return evalNode(v.Body, env, cg, depth)

	// Calls
	case *ast.CallExpr:
		return evalCallExpr(v, env, cg, depth)

	// Where list (expression-position where)
	case *ast.WhereList:
		return evalWhereList(v, env, cg, depth)

	// Wrapper statements
	case *ast.ExprStmt:
		return evalNode(v.Expr, env, cg, depth)

	// Echo: #pure functions cannot echo; bail anyway to be safe
	case *ast.EchoStmt:
		return ctfeVal{}, errNotConst

	default:
		// Unknown node type - cannot evaluate.
		return ctfeVal{}, errNotConst
	}
}

// For loop

func evalForStmt(v *ast.ForStmt, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	local := copyEnv(env)

	switch v.Kind {
	case ast.ForCStyle:
		// Init
		if v.Init != nil {
			if _, err := evalNode(v.Init, local, cg, depth); err != nil {
				return ctfeVal{}, err
			}
		}

		for iters := 0; iters < maxCTFEIter; iters++ {
			if v.Cond != nil {
				cv, err := evalNode(v.Cond, local, cg, depth)
				if err != nil {
					return ctfeVal{}, err
				}

				if cv.kind != "bool" || !cv.b {
					break
				}
			}

			_, err := evalBlock(v.Body, local, cg, depth)
			if err != nil {
				var ret ctfeReturn
				if errors.As(err, &ret) {
					return ctfeVal{}, err // propagate return
				}

				return ctfeVal{}, err
			}
			// Post
			if v.Post != nil {
				if _, err2 := evalNode(v.Post, local, cg, depth); err2 != nil {
					return ctfeVal{}, err2
				}
			}
		}
		// Copy mutations to outer-scope variables back into the caller's env.
		// Variables that did not exist before the loop (e.g. the loop index 'i',
		// temporaries declared inside the body) are not propagated.
		for k := range env {
			if cv, ok := local[k]; ok {
				env[k] = cv
			}
		}

		return ctfeVal{kind: "i64"}, nil

	case ast.ForIn:
		// For-in loops require range evaluation - not supported yet.
		return ctfeVal{}, errNotConst

	case ast.ForWhile:
		// While loops are not supported in CTFE yet.
		return ctfeVal{}, errNotConst
	}

	return ctfeVal{}, errNotConst
}

// Calls

func evalCallExpr(call *ast.CallExpr, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if depth >= maxCTFEDepth {
		return ctfeVal{}, errNotConst
	}

	calleeName := resolveCalleeName(call)
	if calleeName == "" || strings.Contains(calleeName, "::") || strings.HasPrefix(calleeName, ".") {
		return ctfeVal{}, errNotConst
	}

	fd, ok := cg.funcDecls[calleeName]
	if !ok {
		return ctfeVal{}, errNotConst
	}
	// Only pure functions can be called during CTFE.
	if !hasTag(fd.Tags, "pure") {
		return ctfeVal{}, errNotConst
	}

	// Bind arguments.
	callEnv := make(map[string]ctfeVal)

	for i, argNode := range call.Args {
		val, err := evalNode(argNode, env, cg, depth)
		if err != nil {
			return ctfeVal{}, err
		}

		if i < len(fd.Params) {
			callEnv[fd.Params[i].Name] = val
		}
	}

	return evalBody(fd.Body, callEnv, cg, depth+1)
}

// Binary operator evaluation

// matchCtfePattern reports whether pat matches disc at compile time.
// Supports literal patterns (IntLit / StringLit / BoolLit / FloatLit /
// CharLit) and the wildcard "_" pattern. Anything richer (struct
// destructuring, type-test arms, ADT variants) returns false silently -
// the caller treats it as "not foldable" and skips that case.
func matchCtfePattern(pat ast.Node, disc ctfeVal, env map[string]ctfeVal, cg *CodeGen, depth int) (bool, error) {
	if pat == nil {
		return true, nil
	}

	if _, ok := pat.(*ast.WildcardExpr); ok {
		return true, nil
	}

	pv, err := evalNode(pat, env, cg, depth)
	if err != nil {
		return false, err
	}

	if pv.kind != disc.kind {
		return false, nil
	}

	switch disc.kind {
	case "i64":
		return pv.i == disc.i, nil
	case "f64":
		return pv.f == disc.f, nil
	case "bool":
		return pv.b == disc.b, nil
	case "string":
		return pv.s == disc.s, nil
	}

	return false, nil
}

func evalBinOp(left ctfeVal, op string, right ctfeVal) (ctfeVal, error) {
	// Integer arithmetic.
	if left.kind == "i64" && right.kind == "i64" {
		l, r := left.i, right.i

		switch op {
		case "+":
			return ctfeVal{kind: "i64", i: l + r}, nil
		case "-":
			return ctfeVal{kind: "i64", i: l - r}, nil
		case "*":
			return ctfeVal{kind: "i64", i: l * r}, nil
		case "/":
			if r == 0 {
				return ctfeVal{}, fmt.Errorf("CTFE: division by zero")
			}

			return ctfeVal{kind: "i64", i: l / r}, nil
		case "%":
			if r == 0 {
				return ctfeVal{}, fmt.Errorf("CTFE: modulo by zero")
			}

			return ctfeVal{kind: "i64", i: l % r}, nil
		case "&":
			return ctfeVal{kind: "i64", i: l & r}, nil
		case "|":
			return ctfeVal{kind: "i64", i: l | r}, nil
		case "^":
			return ctfeVal{kind: "i64", i: l ^ r}, nil
		case "<<":
			return ctfeVal{kind: "i64", i: l << uint(r)}, nil
		case ">>":
			return ctfeVal{kind: "i64", i: l >> uint(r)}, nil
		case "==":
			return ctfeVal{kind: "bool", b: l == r}, nil
		case "!=":
			return ctfeVal{kind: "bool", b: l != r}, nil
		case "<":
			return ctfeVal{kind: "bool", b: l < r}, nil
		case "<=":
			return ctfeVal{kind: "bool", b: l <= r}, nil
		case ">":
			return ctfeVal{kind: "bool", b: l > r}, nil
		case ">=":
			return ctfeVal{kind: "bool", b: l >= r}, nil
		}
	}

	// Float arithmetic.
	if left.kind == "f64" && right.kind == "f64" {
		l, r := left.f, right.f

		switch op {
		case "+":
			return ctfeVal{kind: "f64", f: l + r}, nil
		case "-":
			return ctfeVal{kind: "f64", f: l - r}, nil
		case "*":
			return ctfeVal{kind: "f64", f: l * r}, nil
		case "/":
			return ctfeVal{kind: "f64", f: l / r}, nil
		case "**":
			return ctfeVal{kind: "f64", f: math.Pow(l, r)}, nil
		case "==":
			return ctfeVal{kind: "bool", b: l == r}, nil
		case "!=":
			return ctfeVal{kind: "bool", b: l != r}, nil
		case "<":
			return ctfeVal{kind: "bool", b: l < r}, nil
		case "<=":
			return ctfeVal{kind: "bool", b: l <= r}, nil
		case ">":
			return ctfeVal{kind: "bool", b: l > r}, nil
		case ">=":
			return ctfeVal{kind: "bool", b: l >= r}, nil
		}
	}

	// Integer + float coercion.
	if left.kind == "i64" && right.kind == "f64" {
		return evalBinOp(ctfeVal{kind: "f64", f: float64(left.i)}, op, right)
	}

	if left.kind == "f64" && right.kind == "i64" {
		return evalBinOp(left, op, ctfeVal{kind: "f64", f: float64(right.i)})
	}

	// String concatenation.
	if left.kind == "string" && right.kind == "string" && op == "++" {
		return ctfeVal{kind: "string", s: left.s + right.s}, nil
	}

	// Bool equality.
	if left.kind == "bool" && right.kind == "bool" {
		switch op {
		case "==":
			return ctfeVal{kind: "bool", b: left.b == right.b}, nil
		case "!=":
			return ctfeVal{kind: "bool", b: left.b != right.b}, nil
		}
	}

	return ctfeVal{}, errNotConst
}

// Helpers

// copyEnv copies an environment so inner scopes don't leak into outer scopes.
func copyEnv(env map[string]ctfeVal) map[string]ctfeVal {
	out := make(map[string]ctfeVal, len(env))
	for k, v := range env {
		out[k] = v
	}

	return out
}

// ctfeValToLLVM converts a ctfeVal to an LLVM constant value, using the
// function's declared return type to pick the right LLVM type.
func ctfeValToLLVM(v ctfeVal, fd *ast.FuncDecl, cg *CodeGen) (value.Value, error) {
	switch v.kind {
	case "i64":
		// Determine the exact LLVM integer type from the return type declaration.
		llvmType := cg.resolveReturnType(fd)
		if it, ok := llvmType.(*irtypes.IntType); ok {
			return constant.NewInt(it, v.i), nil
		}
		// Default to i64.

		return constant.NewInt(irtypes.I64, v.i), nil
	case "f64":
		return constant.NewFloat(irtypes.Double, v.f), nil
	case "bool":
		b := int64(0)
		if v.b {
			b = 1
		}

		return constant.NewInt(irtypes.I1, b), nil
	}
	// Strings and other types require fat-pointer construction - not a simple constant.

	return nil, nil
}

// resolveReturnType returns the LLVM type for the return value of fd, or nil.
func (cg *CodeGen) resolveReturnType(fd *ast.FuncDecl) irtypes.Type {
	if fd.RetType == nil {
		return irtypes.I64
	}

	switch t := fd.RetType.(type) {
	case *ast.SimpleType:
		switch t.Name {
		case "i8":
			return irtypes.I8
		case "i16":
			return irtypes.I16
		case "i32":
			return irtypes.I32
		case "i64":
			return irtypes.I64
		case "u8":
			return irtypes.I8
		case "u16":
			return irtypes.I16
		case "u32":
			return irtypes.I32
		case "u64":
			return irtypes.I64
		case "f32":
			return irtypes.Float
		case "f64":
			return irtypes.Double
		case "bool":
			return irtypes.I1
		}
	}

	return irtypes.I64
}
