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

// ---------------------------------------------------------------------------
// Compile-time value type
// ---------------------------------------------------------------------------

// ctfeVal holds a single compile-time constant value produced by CTFE.
type ctfeVal struct {
	kind string // "i64", "f64", "bool", "string"
	i    int64
	f    float64
	b    bool
	s    string
}

// ctfeReturn is a sentinel used to carry a return value out of evalBody.
type ctfeReturn struct{ val ctfeVal }

func (r ctfeReturn) Error() string { return "ctfe-return" }

// errNotConst signals that an expression is not a compile-time constant.
var errNotConst = errors.New("not a compile-time constant")

// maxCTFEIter is the maximum number of loop iterations during CTFE to prevent
// infinite loops when the loop bound is not statically knowable.
const maxCTFEIter = 100_000

// maxCTFEDepth is the maximum call-stack depth during CTFE.
const maxCTFEDepth = 256

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// tryEvalPureCall attempts to evaluate a call to a #pure #no_recurse function
// whose arguments are all compile-time constants. On success it returns the
// constant LLVM value that replaces the call instruction. Returns nil (and no
// error) if the call cannot be evaluated at compile time.
func (cg *CodeGen) tryEvalPureCall(call *ast.CallExpr) (value.Value, error) {
	// Only simple identifier callees (no method/scope/ptr calls).
	calleeName := resolveCalleeName(call)
	if calleeName == "" || strings.Contains(calleeName, "::") || strings.HasPrefix(calleeName, ".") {
		return nil, nil
	}

	fd, ok := cg.funcDecls[calleeName]
	if !ok {
		return nil, nil
	}
	if !hasTag(fd.Tags, "pure") || !hasTag(fd.Tags, "no_recurse") {
		return nil, nil
	}
	// Generic functions are not evaluated (type substitution not handled).
	if len(fd.TypeParams) > 0 {
		return nil, nil
	}

	// Try to evaluate each argument.
	env := make(map[string]ctfeVal)
	for i, argNode := range call.Args {
		val, err := evalNode(argNode, env, cg, 0)
		if err != nil {
			return nil, nil // argument not constant - fall back to normal codegen
		}
		if i < len(fd.Params) {
			env[fd.Params[i].Name] = val
		}
	}

	// Evaluate the function body.
	result, err := evalBody(fd.Body, env, cg, 0)
	if err != nil {
		return nil, nil // evaluation failed - fall back to normal codegen
	}

	// Convert the result to an LLVM constant.
	return ctfeValToLLVM(result, fd, cg)
}

// ---------------------------------------------------------------------------
// Body evaluator
// ---------------------------------------------------------------------------

// evalBody evaluates a function body (Block, WhereList, or expression) and
// returns the result. Returns errNotConst if the body cannot be fully evaluated.
func evalBody(body ast.Node, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if depth > maxCTFEDepth {
		return ctfeVal{}, errNotConst
	}
	switch v := body.(type) {
	case *ast.Block:
		return evalBlock(v, copyEnv(env), cg, depth)
	case *ast.WhereList:
		return evalWhereList(v, env, cg, depth)
	default:
		// Single-expression body.
		return evalNode(body, env, cg, depth)
	}
}

// evalBlock runs a list of statements, propagating the first return value.
func evalBlock(blk *ast.Block, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if blk == nil {
		return ctfeVal{kind: "i64"}, nil
	}
	for _, stmt := range blk.Stmts {
		val, err := evalNode(stmt, env, cg, depth)
		if err != nil {
			// Unwrap return sentinel.
			var ret ctfeReturn
			if errors.As(err, &ret) {
				return ret.val, nil
			}
			return ctfeVal{}, err
		}
		_ = val
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

// ---------------------------------------------------------------------------
// Node evaluator
// ---------------------------------------------------------------------------

// evalNode evaluates any AST node as a statement or expression.
// For statement nodes (VarDecl, AssignStmt, etc.) it mutates env and returns zero.
// For expression nodes it returns the computed value.
// Return statements propagate via ctfeReturn error sentinel.
func evalNode(node ast.Node, env map[string]ctfeVal, cg *CodeGen, depth int) (ctfeVal, error) {
	if node == nil {
		return ctfeVal{kind: "i64"}, nil
	}
	switch v := node.(type) {

	// --- Literals ---
	case *ast.IntLit:
		return ctfeVal{kind: "i64", i: v.Value}, nil
	case *ast.FloatLit:
		return ctfeVal{kind: "f64", f: v.Value}, nil
	case *ast.BoolLit:
		return ctfeVal{kind: "bool", b: v.Value}, nil
	case *ast.StringLit:
		return ctfeVal{kind: "string", s: v.Value}, nil
	case *ast.CharLit:
		return ctfeVal{kind: "i64", i: int64(v.Value)}, nil

	// --- Variables ---
	case *ast.Identifier:
		if cv, ok := env[v.Name]; ok {
			return cv, nil
		}
		return ctfeVal{}, errNotConst

	// --- Declarations / assignments (mutate env) ---
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

	// --- Binary expressions ---
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

	// --- Ternary ---
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

	// --- Control flow ---
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
			return evalBlock(v.Then, copyEnv(env), cg, depth)
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
				return evalBlock(ei.Body, copyEnv(env), cg, depth)
			}
		}
		if v.Else != nil {
			return evalBlock(v.Else, copyEnv(env), cg, depth)
		}
		return ctfeVal{kind: "i64"}, nil

	case *ast.ForStmt:
		return evalForStmt(v, env, cg, depth)

	// --- Block / tagged block ---
	case *ast.Block:
		return evalBlock(v, copyEnv(env), cg, depth)

	case *ast.TaggedBlock:
		// #allow_sideffect blocks may contain echo - bail on CTFE.
		if hasTag(v.Tags, "allow_sideffect") {
			return ctfeVal{}, errNotConst
		}
		return evalNode(v.Body, env, cg, depth)

	// --- Calls ---
	case *ast.CallExpr:
		return evalCallExpr(v, env, cg, depth)

	// --- Where list (expression-position where) ---
	case *ast.WhereList:
		return evalWhereList(v, env, cg, depth)

	// --- Wrapper statements ---
	case *ast.ExprStmt:
		return evalNode(v.Expr, env, cg, depth)

	// --- Echo: #pure functions cannot echo; bail anyway to be safe ---
	case *ast.EchoStmt:
		return ctfeVal{}, errNotConst

	default:
		// Unknown node type - cannot evaluate.
		return ctfeVal{}, errNotConst
	}
}

// ---------------------------------------------------------------------------
// For loop
// ---------------------------------------------------------------------------

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
	}
	return ctfeVal{}, errNotConst
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Binary operator evaluation
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
		case "i64", "int":
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
