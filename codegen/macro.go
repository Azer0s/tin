package codegen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

// macroTimeout is the maximum wall-clock time allowed for one CTFE macro
// execution (compilation + run).  Defaults to 5 s; override via the
// TINMACRO_TIMEOUT environment variable (e.g. "30s", "2m").
// The env var is read once at package init so the value is stable across
// concurrent compilations.
var macroTimeout = func() time.Duration {
	if v := os.Getenv("TINMACRO_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}

	return 5 * time.Second
}()

// isMacroComplex returns true if the macro body is a block (requires CTFE).
func isMacroComplex(m *ast.MacroDecl) bool {
	_, ok := m.Body.(*ast.Block)

	return ok
}

// checkMacroSideEffects returns an error if the macro body contains echo or I/O.
// Recursive macros are allowed - the 5-second timeout handles runaway recursion.
func checkMacroSideEffects(m *ast.MacroDecl) error {
	if nodeHasSideEffects(m.Body) {
		return fmt.Errorf("macro %s: macros must be pure (no echo or I/O statements)", m.Name)
	}

	return nil
}

func nodeHasSideEffects(node ast.Node) bool {
	if node == nil {
		return false
	}

	switch v := node.(type) {
	case *ast.EchoStmt:
		return true
	case *ast.Block:
		for _, s := range v.Stmts {
			if nodeHasSideEffects(s) {
				return true
			}
		}
	case *ast.IfStmt:
		if nodeHasSideEffects(v.Cond) || blockHasSideEffects(v.Then) {
			return true
		}

		for _, ei := range v.ElseIfs {
			if blockHasSideEffects(ei.Body) {
				return true
			}
		}

		return blockHasSideEffects(v.Else)
	case *ast.ForStmt:
		return nodeHasSideEffects(v.Cond) || blockHasSideEffects(v.Body)
	case *ast.ReturnStmt:
		return nodeHasSideEffects(v.Value)
	case *ast.AssignStmt:
		return nodeHasSideEffects(v.Value)
	case *ast.VarDecl:
		return nodeHasSideEffects(v.Value)
	case *ast.ExprStmt:
		return nodeHasSideEffects(v.Expr)
	case *ast.BinExpr:
		return nodeHasSideEffects(v.Left) || nodeHasSideEffects(v.Right)
	}

	return false
}

func blockHasSideEffects(b *ast.Block) bool {
	if b == nil {
		return false
	}

	for _, s := range b.Stmts {
		if nodeHasSideEffects(s) {
			return true
		}
	}

	return false
}

// ctfeExpandMacro compiles and runs the macro body at compile time,
// wrapped in a typed function so that `return` works correctly.
func (cg *CodeGen) ctfeExpandMacro(m *ast.MacroDecl, args []ast.Node) (ast.Node, error) {
	cg.progress("ctfe macro " + strings.TrimSuffix(m.Name, "!"))

	retType := inferReturnType(m, args)
	src := buildMacroSource(m, args, cg.macros, cg.importedPkgs)

	tmpFile, err := os.CreateTemp("", "tin_macro_*.tin")
	if err != nil {
		return nil, fmt.Errorf("macro %s: %w", m.Name, err)
	}
	defer func(name string) {
		_ = os.Remove(name)
	}(tmpFile.Name())

	if _, err := tmpFile.WriteString(src); err != nil {
		_ = tmpFile.Close()

		return nil, err
	}

	_ = tmpFile.Close()

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("macro %s: cannot locate tin binary: %w", m.Name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), macroTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "run", tmpFile.Name())
	out, err := cmd.Output()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("macro %s: timed out after %v\nGenerated source:\n%s", m.Name, macroTimeout, src)
	}

	if err != nil {
		stderr := ""

		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}

		return nil, fmt.Errorf("macro %s: execution failed: %w\nStderr: %s\nGenerated source:\n%s",
			m.Name, err, stderr, src)
	}

	result := strings.TrimSpace(string(out))
	if result == "" {
		return nil, fmt.Errorf("macro %s: produced no output\nGenerated source:\n%s", m.Name, src)
	}

	return parseCtfeResult(result, retType, m.Name, src)
}

// parseCtfeResult converts raw CTFE stdout into an AST literal node.
// Special cases:
//   - backtick-wrapped output " `expr` " -> parse inner content as tin source (code splice)
//   - "string" return type -> wrap bare output in StringLit
//   - "f64" return type -> promote integer-looking output to FloatLit
func parseCtfeResult(result, retType, macroName, src string) (ast.Node, error) {
	// Backtick splice: the macro returned a backtick literal; echo printed `content`
	if len(result) >= 2 && result[0] == '`' && result[len(result)-1] == '`' {
		inner := result[1 : len(result)-1]

		node, err := parseExprString(inner)
		if err != nil {
			return nil, fmt.Errorf("macro %s: backtick splice parse error %q: %w\nGenerated source:\n%s",
				macroName, inner, err, src)
		}

		return node, nil
	}

	switch retType {
	case "string":
		// echo prints bare string content - wrap it back into a StringLit directly
		return &ast.StringLit{Value: result}, nil
	case "f64":
		// Try normal parse first (handles "3.14", "-1.5", etc.)
		node, err := parseExprString(result)
		if err != nil {
			return nil, fmt.Errorf("macro %s: cannot parse output %q: %w\nGenerated source:\n%s",
				macroName, result, err, src)
		}
		// If we got an IntLit, promote it to FloatLit (echo 1.0 -> "1")
		if il, ok := node.(*ast.IntLit); ok {
			return &ast.FloatLit{Value: float64(il.Value)}, nil
		}
		// UnaryExpr(-IntLit) -> promote to negative FloatLit
		if ue, ok := node.(*ast.UnaryExpr); ok && ue.Op == "-" {
			if il, ok2 := ue.Expr.(*ast.IntLit); ok2 {
				return &ast.FloatLit{Value: -float64(il.Value)}, nil
			}
		}

		return node, nil
	default:
		node, err := parseExprString(result)
		if err != nil {
			return nil, fmt.Errorf("macro %s: cannot parse output %q: %w\nGenerated source:\n%s",
				macroName, result, err, src)
		}

		return node, nil
	}
}

// inferArgType returns the tin type name for an argument expression.
// Used to generate typed function parameters in the CTFE wrapper.
func inferArgType(arg ast.Node) string {
	switch arg.(type) {
	case *ast.IntLit:
		return "i64"
	case *ast.FloatLit:
		return "f64"
	case *ast.BoolLit:
		return "bool"
	case *ast.StringLit:
		return "string"
	default:
		// Complex expressions (calls, field accesses, etc.) are code fragments:
		// pass them as strings containing the source text so they can be spliced
		// into backtick templates via string interpolation.
		return "string"
	}
}

// isCodeFragmentArg returns true when the arg is not a simple literal and
// should therefore be passed to the CTFE subprocess as a quoted source string.
func isCodeFragmentArg(arg ast.Node) bool {
	switch arg.(type) {
	case *ast.IntLit, *ast.FloatLit, *ast.BoolLit, *ast.StringLit:
		return false
	default:
		return true
	}
}

// inferReturnType infers the tin return type for a CTFE macro.
// It looks at return statements in the block body for type hints,
// falling back to the type of the first argument.
func inferReturnType(m *ast.MacroDecl, args []ast.Node) string {
	block, ok := m.Body.(*ast.Block)
	if !ok {
		return "i64"
	}
	// Walk the body looking for return statements
	if t := findReturnType(block, m, args); t != "" {
		return t
	}
	// Fall back to first argument type
	if len(args) > 0 {
		return inferArgType(args[0])
	}

	return "i64"
}

func findReturnType(node ast.Node, m *ast.MacroDecl, args []ast.Node) string {
	if node == nil {
		return ""
	}
	// Build param->type map for lookup; prefer explicit types over inferred
	paramTypes := make(map[string]string, len(m.Params))
	for i, p := range m.Params {
		paramTypes[p] = paramType(m, i, args)
	}

	return findReturnTypeInNode(node, paramTypes)
}

func findReturnTypeInNode(node ast.Node, paramTypes map[string]string) string {
	if node == nil {
		return ""
	}

	switch v := node.(type) {
	case *ast.ReturnStmt:
		if v == nil {
			return ""
		}

		if v.Value != nil {
			return typeOfExpr(v.Value, paramTypes)
		}
	case *ast.Block:
		if v == nil {
			return ""
		}

		for _, s := range v.Stmts {
			if t := findReturnTypeInNode(s, paramTypes); t != "" {
				return t
			}
		}
	case *ast.IfStmt:
		if v.Then != nil {
			if t := findReturnTypeInNode(v.Then, paramTypes); t != "" {
				return t
			}
		}

		for _, ei := range v.ElseIfs {
			if ei.Body != nil {
				if t := findReturnTypeInNode(ei.Body, paramTypes); t != "" {
					return t
				}
			}
		}

		if v.Else != nil {
			if t := findReturnTypeInNode(v.Else, paramTypes); t != "" {
				return t
			}
		}
	case *ast.ForStmt:
		if v.Body != nil {
			return findReturnTypeInNode(v.Body, paramTypes)
		}
	}

	return ""
}

// typeOfExpr returns a tin type string for a simple expression node.
func typeOfExpr(n ast.Node, paramTypes map[string]string) string {
	switch v := n.(type) {
	case *ast.IntLit:
		return "i64"
	case *ast.FloatLit:
		return "f64"
	case *ast.BoolLit:
		return "bool"
	case *ast.StringLit:
		return "string"
	case *ast.BacktickLit:
		return "string" // backtick literals compile to string at runtime
	case *ast.Identifier:
		if t, ok := paramTypes[v.Name]; ok {
			return t
		}
	case *ast.BinExpr:
		if t := typeOfExpr(v.Left, paramTypes); t != "" {
			return t
		}

		return typeOfExpr(v.Right, paramTypes)
	case *ast.UnaryExpr:
		return typeOfExpr(v.Expr, paramTypes)
	case *ast.AsExpr:
		if v.Type != nil {
			return v.Type.String()
		}
	}

	return "i64" // conservative default
}

// paramType returns the resolved type for the i-th macro parameter.
// If an explicit type is declared in the macro signature, that is used.
// Otherwise the type is inferred from the call-site argument.
func paramType(m *ast.MacroDecl, i int, args []ast.Node) string {
	if i < len(m.ParamTypes) && m.ParamTypes[i] != "" {
		return m.ParamTypes[i]
	}

	if i < len(args) {
		return inferArgType(args[i])
	}

	return "i64"
}

// buildMacroSource generates a standalone tin program for CTFE execution.
// The macro body is wrapped in a typed function so `return` works correctly.
// Recursive self-calls in the body are renamed to the helper function name,
// converting recursive macro calls into ordinary function recursion without
// spawning additional compiler processes.
//
// allMacros is the full macro registry; simple (expression-body) macros other
// than m itself are emitted as `macro` declarations so that inter-macro calls
// inside m's body work correctly.
//
// Generated structure:
//
//	macro dep1!(p) = <expr>     // dependency simple macros
//	fn __macro_<name>(param1 type1, ...) = <body with self-calls renamed>
//	echo __macro_<name>(arg1, arg2, ...)
func buildMacroSource(m *ast.MacroDecl, args []ast.Node, allMacros map[string]*ast.MacroDecl, importedPkgs map[string]bool) string {
	baseName := strings.TrimSuffix(m.Name, "!")
	fnName := "__macro_" + baseName

	var sb strings.Builder

	// Emit use declarations so the macro body can call stdlib functions.
	for pkg := range importedPkgs {
		sb.WriteString("use " + pkg + "\n")
	}

	if len(importedPkgs) > 0 {
		sb.WriteString("\n")
	}

	// Emit simple (expression-body) macros that m might call
	for _, dep := range allMacros {
		if dep.Name == m.Name {
			continue // skip self; self-calls are handled by renameMacroCalls
		}

		if isMacroComplex(dep) {
			continue // only emit simple macros as declarations
		}

		sb.WriteString("macro " + dep.Name + "(")

		for i, p := range dep.Params {
			if i > 0 {
				sb.WriteString(", ")
			}

			sb.WriteString(p)
		}

		sb.WriteString(") = ")

		body := dep.Body
		if es, ok := body.(*ast.ExprStmt); ok {
			body = es.Expr
		}

		sb.WriteString(ast.PrintExpr(body))
		sb.WriteString("\n")
	}

	if len(allMacros) > 1 {
		sb.WriteString("\n")
	}

	retType := inferReturnType(m, args)

	// Generate function declaration with inferred/declared parameter types
	sb.WriteString("fn " + fnName + "(")

	for i, param := range m.Params {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString(param + " " + paramType(m, i, args))
	}

	sb.WriteString(") " + retType + " =\n")

	// Print body with self-calls renamed to fnName (enables recursion without nested processes)
	body := renameMacroCalls(m.Body, m.Name, baseName, fnName)
	sb.WriteString(ast.PrintStmt(body, 1))
	sb.WriteString("\n\n")

	// Entry point: echo the result.
	// Code-fragment args are passed as quoted source strings so the CTFE body
	// can embed them in backtick splices via {param} interpolation.
	sb.WriteString("echo " + fnName + "(")

	for i, arg := range args {
		if i > 0 {
			sb.WriteString(", ")
		}

		if isCodeFragmentArg(arg) {
			src := ast.PrintExpr(arg)
			// Escape backslashes and double quotes for embedding in a string literal.
			src = strings.ReplaceAll(src, `\`, `\\`)
			src = strings.ReplaceAll(src, `"`, `\"`)
			sb.WriteString(`"` + src + `"`)
		} else {
			sb.WriteString(ast.PrintExpr(arg))
		}
	}

	sb.WriteString(")\n")

	return sb.String()
}

// renameMacroCalls walks an AST node and replaces calls to the macro
// (identifiers matching macroName, baseName!, or baseName as a call target)
// with calls to fnName. This prevents recursive CTFE macros from spawning
// additional compiler processes by converting macro calls into plain function calls.
func renameMacroCalls(node ast.Node, macroName, baseName, fnName string) ast.Node {
	if node == nil {
		return nil
	}

	isMacroIdent := func(name string) bool {
		return name == macroName || name == baseName+"!" || name == baseName
	}

	switch v := node.(type) {
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(v.Args))
		for i, a := range v.Args {
			newArgs[i] = renameMacroCalls(a, macroName, baseName, fnName)
		}

		if id, ok := v.Func.(*ast.Identifier); ok && isMacroIdent(id.Name) {
			return &ast.CallExpr{Func: &ast.Identifier{Name: fnName}, Args: newArgs}
		}

		return &ast.CallExpr{
			Func:     renameMacroCalls(v.Func, macroName, baseName, fnName),
			TypeArgs: v.TypeArgs,
			Args:     newArgs,
		}

	case *ast.Block:
		stmts := make([]ast.Node, len(v.Stmts))
		for i, s := range v.Stmts {
			stmts[i] = renameMacroCalls(s, macroName, baseName, fnName)
		}

		return &ast.Block{Stmts: stmts}

	case *ast.IfStmt:
		elseIfs := make([]ast.ElseIfClause, len(v.ElseIfs))
		for i, ei := range v.ElseIfs {
			elseIfs[i] = ast.ElseIfClause{
				Cond: renameMacroCalls(ei.Cond, macroName, baseName, fnName),
				Body: renameMacroCalls(ei.Body, macroName, baseName, fnName).(*ast.Block),
			}
		}

		var elsePart *ast.Block
		if v.Else != nil {
			elsePart = renameMacroCalls(v.Else, macroName, baseName, fnName).(*ast.Block)
		}

		var thenPart *ast.Block
		if v.Then != nil {
			thenPart = renameMacroCalls(v.Then, macroName, baseName, fnName).(*ast.Block)
		}

		return &ast.IfStmt{
			Cond:    renameMacroCalls(v.Cond, macroName, baseName, fnName),
			Then:    thenPart,
			ElseIfs: elseIfs,
			Else:    elsePart,
		}

	case *ast.ForStmt:
		var body *ast.Block
		if v.Body != nil {
			body = renameMacroCalls(v.Body, macroName, baseName, fnName).(*ast.Block)
		}

		return &ast.ForStmt{
			Kind:    v.Kind,
			VarName: v.VarName,
			VarType: v.VarType,
			Init:    renameMacroCalls(v.Init, macroName, baseName, fnName),
			Cond:    renameMacroCalls(v.Cond, macroName, baseName, fnName),
			Post:    renameMacroCalls(v.Post, macroName, baseName, fnName),
			Iter:    renameMacroCalls(v.Iter, macroName, baseName, fnName),
			Body:    body,
		}

	case *ast.ReturnStmt:
		return &ast.ReturnStmt{Value: renameMacroCalls(v.Value, macroName, baseName, fnName)}

	case *ast.AssignStmt:
		return &ast.AssignStmt{
			Target: renameMacroCalls(v.Target, macroName, baseName, fnName),
			Value:  renameMacroCalls(v.Value, macroName, baseName, fnName),
		}

	case *ast.AugAssignStmt:
		return &ast.AugAssignStmt{
			Target: v.Target,
			Op:     v.Op,
			Value:  renameMacroCalls(v.Value, macroName, baseName, fnName),
		}

	case *ast.PostfixStmt:
		return &ast.PostfixStmt{
			Expr: renameMacroCalls(v.Expr, macroName, baseName, fnName),
			Op:   v.Op,
		}

	case *ast.VarDecl:
		return &ast.VarDecl{
			IsConst: v.IsConst,
			Name:    v.Name,
			Type:    v.Type,
			Value:   renameMacroCalls(v.Value, macroName, baseName, fnName),
		}

	case *ast.ExprStmt:
		return &ast.ExprStmt{Expr: renameMacroCalls(v.Expr, macroName, baseName, fnName)}

	case *ast.BinExpr:
		return &ast.BinExpr{
			Left:  renameMacroCalls(v.Left, macroName, baseName, fnName),
			Op:    v.Op,
			Right: renameMacroCalls(v.Right, macroName, baseName, fnName),
		}

	case *ast.UnaryExpr:
		return &ast.UnaryExpr{
			Op:   v.Op,
			Expr: renameMacroCalls(v.Expr, macroName, baseName, fnName),
			Post: v.Post,
		}

	case *ast.TernaryExpr:
		return &ast.TernaryExpr{
			Cond: renameMacroCalls(v.Cond, macroName, baseName, fnName),
			Then: renameMacroCalls(v.Then, macroName, baseName, fnName),
			Else: renameMacroCalls(v.Else, macroName, baseName, fnName),
		}

	case *ast.FieldAccess:
		return &ast.FieldAccess{
			Expr:  renameMacroCalls(v.Expr, macroName, baseName, fnName),
			Field: v.Field,
			IsPtr: v.IsPtr,
		}

	case *ast.IndexExpr:
		return &ast.IndexExpr{
			Expr:  renameMacroCalls(v.Expr, macroName, baseName, fnName),
			Index: renameMacroCalls(v.Index, macroName, baseName, fnName),
		}

	default:
		return node
	}
}

// parseExprString parses a single tin expression from a string (the CTFE output).
func parseExprString(s string) (ast.Node, error) {
	l := lexer.New(s)

	tokens, err := l.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("parse macro output %q: lex error: %w", s, err)
	}

	p := parser.New(tokens)

	return p.ParseExpr()
}
