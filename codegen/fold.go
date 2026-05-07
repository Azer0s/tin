package codegen

// Compile-time constant folding for if/elif conditions.
//
// Motivation: Tin generic functions are monomorphized into one concrete
// function per type argument. The body often uses `typeof(v) == 'foo` to
// dispatch on the type at "runtime", but for any fixed monomorphization
// the result is statically knowable. Without folding, the compiler emits
// every dispatch arm including the type-incorrect ones (e.g. `v as string`
// when v is bool); these only survive because the runtime guard prevents
// the wrong branch from executing. They also defeat the strict per-arg
// type check at call sites, since the bad-typed calls do exist in IR even
// though they are dynamically unreachable.
//
// This file walks the AST of an if condition (and a few related shapes)
// and attempts to evaluate to a literal value. genIf consults
// foldedBoolCondition before generating both branches; when the answer is
// known, only the live branch is emitted.
//
// What we recognize today:
//
//   - bool literals
//   - atom literals
//   - integer literals
//   - typeof(<expr>) where <expr>'s static type can be resolved by
//     genExpr-equivalent type analysis without emitting any IR
//   - identifier references to non-mutated `let` bindings whose initializer
//     is one of the above (recursively foldable)
//   - BinExpr with == / != between two foldable values
//   - BinExpr with && / || between two foldable bools (short-circuiting)
//   - UnaryExpr with `not` on a foldable bool
//
// Anything outside this set returns "unknown" and the caller falls back to
// runtime evaluation. Folding is a best-effort optimization: returning
// unknown is always safe.

import (
	"reflect"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// foldedKind tags a foldedValue's payload.
type foldedKind int

const (
	foldUnknown foldedKind = iota
	foldBool
	foldInt
	foldAtom
)

// foldedValue is a successfully evaluated constant. Only one of the
// payload fields is meaningful, indicated by kind.
type foldedValue struct {
	kind    foldedKind
	boolVal bool
	intVal  int64
	atomVal string
}

func unknownFold() foldedValue { return foldedValue{kind: foldUnknown} }

// foldedBoolCondition tries to reduce an if-condition to a known bool. On
// success returns the bool and true; otherwise returns false. Use for
// dead-branch elimination at codegen.
func (cg *CodeGen) foldedBoolCondition(e ast.Node) (bool, bool) {
	v := cg.tryFoldExpr(e)
	if v.kind != foldBool {
		return false, false
	}

	return v.boolVal, true
}

// boolCondConstResult is like foldedBoolCondition but allowed to ignore
// side effects in connective operands. Only safe for warning emission,
// NOT for dead-branch elimination: `a && false` returns (false, true)
// here but `a` must still execute at runtime to preserve side effects.
//
// Extends the strict fold with two patterns that the rewriter
// deliberately leaves alone so operand side effects are preserved:
//
//	<unknown> && false  -> false
//	<unknown> || true   -> true
func (cg *CodeGen) boolCondConstResult(e ast.Node) (bool, bool) {
	v := cg.tryFoldExprForWarning(e)
	if v.kind != foldBool {
		return false, false
	}

	return v.boolVal, true
}

// tryFoldExprForWarning mirrors tryFoldExpr but accepts the side-effect-
// discarding connective patterns described on boolCondConstResult. It
// recurses through itself so nested compositions like `(x && false) ||
// (y && false)` still reduce.
func (cg *CodeGen) tryFoldExprForWarning(n ast.Node) foldedValue {
	if v := cg.tryFoldExpr(n); v.kind != foldUnknown {
		return v
	}

	switch e := n.(type) {
	case *ast.BinExpr:
		switch e.Op {
		case "&&", "and":
			l := cg.tryFoldExprForWarning(e.Left)
			if l.kind == foldBool && !l.boolVal {
				return foldedValue{kind: foldBool, boolVal: false}
			}

			r := cg.tryFoldExprForWarning(e.Right)
			if r.kind == foldBool && !r.boolVal {
				return foldedValue{kind: foldBool, boolVal: false}
			}

			if l.kind == foldBool && r.kind == foldBool {
				return foldedValue{kind: foldBool, boolVal: l.boolVal && r.boolVal}
			}
		case "||", "or":
			l := cg.tryFoldExprForWarning(e.Left)
			if l.kind == foldBool && l.boolVal {
				return foldedValue{kind: foldBool, boolVal: true}
			}

			r := cg.tryFoldExprForWarning(e.Right)
			if r.kind == foldBool && r.boolVal {
				return foldedValue{kind: foldBool, boolVal: true}
			}

			if l.kind == foldBool && r.kind == foldBool {
				return foldedValue{kind: foldBool, boolVal: l.boolVal || r.boolVal}
			}
		}
	case *ast.UnaryExpr:
		if e.Op == "!" || e.Op == "not" {
			v := cg.tryFoldExprForWarning(e.Expr)
			if v.kind == foldBool {
				return foldedValue{kind: foldBool, boolVal: !v.boolVal}
			}
		}
	}

	return unknownFold()
}

// tryFoldExpr is the recursive folding driver.
func (cg *CodeGen) tryFoldExpr(n ast.Node) foldedValue {
	switch e := n.(type) {
	case *ast.BoolLit:
		return foldedValue{kind: foldBool, boolVal: e.Value}
	case *ast.AtomLit:
		return foldedValue{kind: foldAtom, atomVal: e.Name}
	case *ast.IntLit:
		if e.Big != nil {
			// foldedValue stores i64; bail rather than silently truncate.
			return unknownFold()
		}

		return foldedValue{kind: foldInt, intVal: e.Value}

	case *ast.TypeofExpr:
		return cg.tryFoldTypeof(e)

	case *ast.Identifier:
		return cg.tryFoldIdent(e)

	case *ast.BinExpr:
		return cg.tryFoldBinExpr(e)

	case *ast.UnaryExpr:
		return cg.tryFoldUnaryExpr(e)

	case *ast.CallExpr:
		return cg.tryFoldPureCall(e)
	}

	return unknownFold()
}

// tryFoldPureCall reuses the AST evaluator behind tryEvalPureCall to fold
// a call to a `#pure #no_recurse` function whose arguments are themselves
// constants. Returns unknownFold() for any case the evaluator can't handle
// (non-pure callee, runtime args, unsupported body shape).
func (cg *CodeGen) tryFoldPureCall(call *ast.CallExpr) foldedValue {
	val, _, ok := cg.tryEvalPureCallToCtfeVal(call)
	if !ok {
		return unknownFold()
	}

	switch val.kind {
	case "i64":
		return foldedValue{kind: foldInt, intVal: val.i}
	case "bool":
		return foldedValue{kind: foldBool, boolVal: val.b}
	}
	// f64 and string can't ride in foldedValue today; ignore.
	return unknownFold()
}

// tryFoldTypeof statically resolves typeof(<expr>) when expr's type can be
// determined without emitting code.
func (cg *CodeGen) tryFoldTypeof(e *ast.TypeofExpr) foldedValue {
	t := cg.staticTypeOf(e.Expr)
	if t == nil {
		return unknownFold()
	}
	// `any` typed values dispatch at runtime; can't fold.
	if isAnyType(t) {
		return unknownFold()
	}

	name := cg.displayStructName(llvmTypeName(t))

	return foldedValue{kind: foldAtom, atomVal: name}
}

// staticTypeOf returns the LLVM type of a value-bearing expression without
// emitting IR. Only handles patterns common in fold contexts:
//   - identifier referencing a scope entry (read its tinType / alloca type)
//   - typeof returns atom (handled by caller)
func (cg *CodeGen) staticTypeOf(n ast.Node) irtypes.Type {
	switch e := n.(type) {
	case *ast.Identifier:
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil
		}
		// alloca pointer -> element type
		if entry.isAlloc {
			pt, ok := entry.val.Type().(*irtypes.PointerType)
			if !ok {
				return nil
			}

			return pt.ElemType
		}

		return entry.val.Type()
	}

	return nil
}

// tryFoldIdent looks up the identifier in scope and follows
// constInitExpr (set by genVarDecl for foldable `let`s) to its constant
// value. Soundness: cg.mutatedNames is populated per function body before
// codegen and contains every name written to by any AssignStmt /
// AugAssignStmt / PostfixStmt anywhere in the body (including closures
// and defers). An identifier in that set is treated as non-constant even
// when its initializer was foldable, because a deferred or captured
// mutation could change its value before the if-condition is evaluated.
func (cg *CodeGen) tryFoldIdent(e *ast.Identifier) foldedValue {
	if cg.mutatedNames != nil && cg.mutatedNames[e.Name] {
		return unknownFold()
	}

	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		return unknownFold()
	}

	if entry.constInitExpr == nil {
		return unknownFold()
	}

	return cg.tryFoldExpr(entry.constInitExpr)
}

// tryFoldBinExpr handles the binary operators we care about for branch
// folding: equality on atoms / ints / bools, and logical combinators on
// bools.
func (cg *CodeGen) tryFoldBinExpr(e *ast.BinExpr) foldedValue {
	switch e.Op {
	case "==":
		l := cg.tryFoldExpr(e.Left)
		r := cg.tryFoldExpr(e.Right)

		if l.kind == foldUnknown || r.kind == foldUnknown {
			return unknownFold()
		}

		if l.kind != r.kind {
			return foldedValue{kind: foldBool, boolVal: false}
		}

		switch l.kind { //nolint:exhaustive // foldUnknown rejected above
		case foldAtom:
			return foldedValue{kind: foldBool, boolVal: l.atomVal == r.atomVal}
		case foldBool:
			return foldedValue{kind: foldBool, boolVal: l.boolVal == r.boolVal}
		case foldInt:
			return foldedValue{kind: foldBool, boolVal: l.intVal == r.intVal}
		}

	case "!=":
		l := cg.tryFoldExpr(e.Left)
		r := cg.tryFoldExpr(e.Right)

		if l.kind == foldUnknown || r.kind == foldUnknown {
			return unknownFold()
		}

		if l.kind != r.kind {
			return foldedValue{kind: foldBool, boolVal: true}
		}

		switch l.kind { //nolint:exhaustive // foldUnknown rejected above
		case foldAtom:
			return foldedValue{kind: foldBool, boolVal: l.atomVal != r.atomVal}
		case foldBool:
			return foldedValue{kind: foldBool, boolVal: l.boolVal != r.boolVal}
		case foldInt:
			return foldedValue{kind: foldBool, boolVal: l.intVal != r.intVal}
		}

	case "&&", "and":
		l := cg.tryFoldExpr(e.Left)
		if l.kind == foldBool && !l.boolVal {
			// Short-circuit: false && _ = false.
			return foldedValue{kind: foldBool, boolVal: false}
		}

		r := cg.tryFoldExpr(e.Right)
		if l.kind == foldBool && r.kind == foldBool {
			return foldedValue{kind: foldBool, boolVal: l.boolVal && r.boolVal}
		}

	case "||", "or":
		l := cg.tryFoldExpr(e.Left)
		if l.kind == foldBool && l.boolVal {
			return foldedValue{kind: foldBool, boolVal: true}
		}

		r := cg.tryFoldExpr(e.Right)
		if l.kind == foldBool && r.kind == foldBool {
			return foldedValue{kind: foldBool, boolVal: l.boolVal || r.boolVal}
		}
	}

	return unknownFold()
}

// tryFoldUnaryExpr handles `not <bool>` and `!`.
func (cg *CodeGen) tryFoldUnaryExpr(e *ast.UnaryExpr) foldedValue {
	if e.Op != "!" && e.Op != "not" {
		return unknownFold()
	}

	v := cg.tryFoldExpr(e.Expr)
	if v.kind != foldBool {
		return unknownFold()
	}

	return foldedValue{kind: foldBool, boolVal: !v.boolVal}
}

// collectMutatedNames walks an AST subtree (typically a function body)
// and returns the set of identifier names that appear as the LHS target
// of an AssignStmt, AugAssignStmt or PostfixStmt anywhere in the tree -
// including inside nested closures, defers, loops, ifs, where-bodies and
// match arms. Used by genFuncDeclAs to seed cg.mutatedNames before
// codegen so the if-condition folder can refuse to constant-fold
// identifiers whose value might change between binding and the fold
// site (e.g. a closure that mutates a captured variable).
func collectMutatedNames(root ast.Node) map[string]bool {
	out := map[string]bool{}
	collectMutatedNamesInto(root, out)

	return out
}

// collectMutatedNamesFromStmts is the multi-root variant used by
// genImplicitMain, where the implicit main body is a flat slice of top-level
// statements rather than a single Block / FuncDecl.
func collectMutatedNamesFromStmts(stmts []ast.Node) map[string]bool {
	out := map[string]bool{}
	for _, s := range stmts {
		collectMutatedNamesInto(s, out)
	}

	return out
}

func collectMutatedNamesInto(root ast.Node, out map[string]bool) {
	walkAST(root, func(n ast.Node) {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if id, ok := s.Target.(*ast.Identifier); ok {
				out[id.Name] = true
			}
		case *ast.AugAssignStmt:
			if id, ok := s.Target.(*ast.Identifier); ok {
				out[id.Name] = true
			}
		case *ast.PostfixStmt:
			if id, ok := s.Expr.(*ast.Identifier); ok {
				out[id.Name] = true
			}
		case *ast.AddressOfExpr:
			// Taking the address of a variable exposes it to mutation via
			// the resulting pointer (e.g. `flip(&flag)` in a caller).
			// Treat the variable as mutated so the fold pass does not
			// constant-propagate its initializer.
			if id, ok := s.Expr.(*ast.Identifier); ok {
				out[id.Name] = true
			}
		case *ast.AddrExpr:
			if id, ok := s.Val.(*ast.Identifier); ok {
				out[id.Name] = true
			}
		}
	})
}

// walkAST visits every reachable node in the AST subtree, calling visit
// on each. Used by collectMutatedNames; intentionally light-touch (no
// type info, no scope tracking) since the only thing the caller needs is
// "does this name ever appear as an assignment target?"
//
// Defensive against typed-nil interface values (e.g. an *ast.Block field
// that's nil but stored in an ast.Node interface): a typed-nil compares
// non-nil to the bare `n == nil` check but its receiver methods would
// dereference, so we use reflect.IsNil to short-circuit.
func walkAST(n ast.Node, visit func(ast.Node)) {
	if n == nil {
		return
	}

	if rv := reflect.ValueOf(n); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return
	}

	visit(n)

	switch v := n.(type) {
	case *ast.Block:
		for _, s := range v.Stmts {
			walkAST(s, visit)
		}
	case *ast.IfStmt:
		walkAST(v.Cond, visit)
		walkAST(v.Then, visit)

		for _, ei := range v.ElseIfs {
			walkAST(ei.Cond, visit)
			walkAST(ei.Body, visit)
		}

		walkAST(v.Else, visit)
	case *ast.ForStmt:
		walkAST(v.Init, visit)
		walkAST(v.Cond, visit)
		walkAST(v.Post, visit)
		walkAST(v.Body, visit)
	case *ast.MatchStmt:
		walkAST(v.Expr, visit)

		for _, c := range v.Cases {
			walkAST(c.Pattern, visit)
			walkAST(c.Guard, visit)
			walkAST(c.Body, visit)
		}

		walkAST(v.Default, visit)
	case *ast.WhereList:
		for _, c := range v.Clauses {
			walkAST(c.Cond, visit)
			walkAST(c.Pattern, visit)
			walkAST(c.Guard, visit)
			walkAST(c.Body, visit)
		}
	case *ast.AssignStmt:
		walkAST(v.Target, visit)
		walkAST(v.Value, visit)
	case *ast.AugAssignStmt:
		walkAST(v.Target, visit)
		walkAST(v.Value, visit)
	case *ast.PostfixStmt:
		walkAST(v.Expr, visit)
	case *ast.VarDecl:
		walkAST(v.Value, visit)
	case *ast.TupleDestructDecl:
		walkAST(v.Value, visit)
	case *ast.ArrayDestructDecl:
		walkAST(v.Value, visit)
	case *ast.StructDestructDecl:
		walkAST(v.Value, visit)
	case *ast.ReturnStmt:
		walkAST(v.Value, visit)
	case *ast.EchoStmt:
		walkAST(v.Value, visit)
	case *ast.ExprStmt:
		walkAST(v.Expr, visit)
	case *ast.DeferStmt:
		walkAST(v.Call, visit)
	case *ast.BinExpr:
		walkAST(v.Left, visit)
		walkAST(v.Right, visit)
	case *ast.UnaryExpr:
		walkAST(v.Expr, visit)
	case *ast.CallExpr:
		walkAST(v.Func, visit)

		for _, a := range v.Args {
			walkAST(a, visit)
		}
	case *ast.TypeofExpr:
		walkAST(v.Expr, visit)
	case *ast.LambdaExpr:
		walkAST(v.Body, visit)
	case *ast.FuncDecl:
		walkAST(v.Body, visit)
	case *ast.IndexExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Index, visit)
	case *ast.FieldAccess:
		walkAST(v.Expr, visit)
	case *ast.TestDecl:
		walkAST(v.Body, visit)
	case *ast.StructDecl:
		for _, m := range v.Methods {
			walkAST(m, visit)
		}
	case *ast.TaggedBlock:
		walkAST(v.Body, visit)
	case *ast.AwaitExpr:
		walkAST(v.Future, visit)
	case *ast.SpawnExpr:
		walkAST(v.Call, visit)
		walkAST(v.DoBlock, visit)
	case *ast.AwaitMatchStmt:
		for _, fut := range v.Futures {
			walkAST(fut, visit)
		}

		for _, c := range v.Cases {
			walkAST(c.Guard, visit)
			walkAST(c.Body, visit)
		}

		walkAST(v.Default, visit)
	case *ast.TernaryExpr:
		walkAST(v.Cond, visit)
		walkAST(v.Then, visit)
		walkAST(v.Else, visit)
	case *ast.ArrayLit:
		for _, e := range v.Elems {
			walkAST(e, visit)
		}
	case *ast.ArrayFillLit:
		walkAST(v.Value, visit)
	case *ast.TupleLit:
		for _, e := range v.Elems {
			walkAST(e, visit)
		}
	case *ast.StructLit:
		for _, f := range v.Fields {
			walkAST(f.Value, visit)
		}

		for _, p := range v.Positional {
			walkAST(p, visit)
		}
	case *ast.RangeExpr:
		walkAST(v.Start, visit)
		walkAST(v.End, visit)
	case *ast.SliceExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Start, visit)
		walkAST(v.End, visit)
	case *ast.AsExpr:
		walkAST(v.Expr, visit)
	case *ast.IsExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Pattern, visit)
	case *ast.InterpolatedString:
		for _, p := range v.Parts {
			if p.IsExpr {
				walkAST(p.Expr, visit)
			}
		}
	case *ast.PipeExpr:
		walkAST(v.Left, visit)
		walkAST(v.Right, visit)
	// Address / deref / type-introspection / reflection-builtin nodes:
	// each wraps a single expression. Without these cases, callers
	// like detectStacktraceUsage and retagMacroBody would silently
	// miss a stacktrace() / sourcepos() call buried under e.g.
	// `&stacktrace()` (AddrExpr) or `sizeof(stacktrace())` (SizeofExpr).
	case *ast.TypeAssertExpr:
		walkAST(v.Expr, visit)
	// SizeofExpr / IsRCExpr take a TypeExpr (no Node child), so no
	// further recursion. Listed for completeness in this comment.
	case *ast.TraitofExpr:
		walkAST(v.Expr, visit)
	case *ast.FieldnamesExpr:
		walkAST(v.Expr, visit)
	case *ast.FieldtypesExpr:
		walkAST(v.Expr, visit)
	case *ast.FieldtagExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Field, visit)
	case *ast.GetfieldExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Field, visit)
	case *ast.SetfieldExpr:
		walkAST(v.Expr, visit)
		walkAST(v.Field, visit)
		walkAST(v.Val, visit)
	case *ast.AddrExpr:
		walkAST(v.Val, visit)
	case *ast.DerefExpr:
		walkAST(v.Expr, visit)
	case *ast.AddressOfExpr:
		walkAST(v.Expr, visit)
	// Top-level decls whose initializers can contain expression trees.
	// MacroDecl bodies need walking so detectStacktraceUsage finds
	// stacktrace() calls referenced ONLY through a macro body - without
	// this the gate stays off, linkage stays internal, and every Tin
	// frame in the eventual trace renders as ??+0x<addr>.
	case *ast.MacroDecl:
		walkAST(v.Body, visit)
	case *ast.TopLevelVar:
		walkAST(v.Value, visit)
	}
}
