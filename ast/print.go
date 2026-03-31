package ast

import (
	"fmt"
	"strings"
)

// PrintExpr serializes an expression AST node to tin source (no indentation).
// Used by the CTFE macro system to embed argument values into generated source.
func PrintExpr(n Node) string { return printNode(n, 0) }

// PrintStmt serializes a statement AST node with the given indent depth.
// Each level uses 2 spaces of indentation.
func PrintStmt(n Node, indent int) string { return printNode(n, indent) }

func ind(n int) string { return strings.Repeat("  ", n) }

func printNode(n Node, depth int) string {
	if n == nil {
		return ""
	}
	switch v := n.(type) {
	// Literals
	case *IntLit:

		return fmt.Sprintf("%d", v.Value)
	case *FloatLit:

		return fmt.Sprintf("%g", v.Value)
	case *StringLit:

		return fmt.Sprintf("%q", v.Value)
	case *BacktickLit:

		return "`" + v.Content + "`"
	case *BoolLit:
		if v.Value {
			return "true"
		}

		return "false"
	case *AtomLit:

		return "'" + v.Name
	case *CharLit:

		return fmt.Sprintf("%d", v.Value) // emit as integer literal
	case *Identifier:

		return v.Name
	case *NilLit:

		return "nil"
	case *WildcardExpr:

		return "_"

	// Expressions
	case *BinExpr:

		return fmt.Sprintf("(%s %s %s)", printNode(v.Left, depth), v.Op, printNode(v.Right, depth))
	case *UnaryExpr:
		if v.Post {
			return fmt.Sprintf("(%s%s)", printNode(v.Expr, depth), v.Op)
		}

		return fmt.Sprintf("(%s%s)", v.Op, printNode(v.Expr, depth))
	case *CallExpr:
		args := make([]string, len(v.Args))
		for i, a := range v.Args {
			args[i] = printNode(a, depth)
		}

		return fmt.Sprintf("%s(%s)", printNode(v.Func, depth), strings.Join(args, ", "))
	case *FieldAccess:
		op := "."
		if v.IsPtr {
			op = "->"
		}

		return fmt.Sprintf("%s%s%s", printNode(v.Expr, depth), op, v.Field)
	case *ScopeAccess:

		return strings.Join(v.Path, "::")
	case *IndexExpr:

		return fmt.Sprintf("%s[%s]", printNode(v.Expr, depth), printNode(v.Index, depth))
	case *SliceExpr:
		s, e := "", ""
		if v.Start != nil {
			s = printNode(v.Start, depth)
		}
		if v.End != nil {
			e = printNode(v.End, depth)
		}

		return fmt.Sprintf("%s[%s:%s]", printNode(v.Expr, depth), s, e)
	case *TupleLit:
		parts := make([]string, len(v.Elems))
		for i, elem := range v.Elems {
			parts[i] = printNode(elem, depth)
		}

		return "(" + strings.Join(parts, ", ") + ")"
	case *TernaryExpr:

		return fmt.Sprintf("(%s ? %s : %s)", printNode(v.Cond, depth), printNode(v.Then, depth), printNode(v.Else, depth))
	case *AsExpr:

		return fmt.Sprintf("(%s as %s)", printNode(v.Expr, depth), v.Type.String())
	case *AddrExpr:

		return fmt.Sprintf("(&%s)", printNode(v.Val, depth))
	case *AddressOfExpr:

		return fmt.Sprintf("(&%s)", printNode(v.Expr, depth))
	case *DerefExpr:

		return fmt.Sprintf("(*%s)", printNode(v.Expr, depth))
	case *PipeExpr:

		return fmt.Sprintf("(%s |> %s)", printNode(v.Left, depth), printNode(v.Right, depth))
	case *RangeExpr:

		return fmt.Sprintf("(%s..%s)", printNode(v.Start, depth), printNode(v.End, depth))
	case *IsExpr:
		if v.VarName != "" {
			return fmt.Sprintf("(%s is %s %s)", printNode(v.Expr, depth), v.VarName, v.Type.String())
		}

		return fmt.Sprintf("(%s is %s)", printNode(v.Expr, depth), v.Type.String())
	case *TypeAssertExpr:
		if v.IsType {
			return fmt.Sprintf("%s.(type)", printNode(v.Expr, depth))
		}

		return fmt.Sprintf("%s.(%s)", printNode(v.Expr, depth), v.Type.String())
	case *SizeofExpr:

		return fmt.Sprintf("sizeof(%s)", v.Type.String())
	case *TypeofExpr:

		return fmt.Sprintf("typeof(%s)", printNode(v.Expr, depth))
	case *InterpolatedString:
		var sb strings.Builder
		sb.WriteString("\"")
		for _, part := range v.Parts {
			if !part.IsExpr {
				sb.WriteString(part.Str)
			} else if part.Format != "" {
				sb.WriteString("{")
				sb.WriteString(printNode(part.Expr, 0))
				sb.WriteString(":")
				sb.WriteString(part.Format)
				sb.WriteString("}")
			} else {
				sb.WriteString("{")
				sb.WriteString(printNode(part.Expr, 0))
				sb.WriteString("}")
			}
		}
		sb.WriteString("\"")

		return sb.String()
	case *ArrayLit:
		elems := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			elems[i] = printNode(e, depth)
		}

		return fmt.Sprintf("[%s]", strings.Join(elems, ", "))

	// Top-level declarations
	case *FuncDecl:
		params := make([]string, len(v.Params))
		for i, p := range v.Params {
			if p.IsVarArgs {
				params[i] = "..."
			} else if p.Type != nil {
				params[i] = p.Name + " " + p.Type.String()
			} else {
				params[i] = p.Name
			}
		}
		ret := ""
		if v.RetType != nil {
			ret = " " + v.RetType.String()
		}
		sig := fmt.Sprintf("%sfn %s(%s)%s", ind(depth), v.Name, strings.Join(params, ", "), ret)
		if v.IsExtern != "" {
			return sig + " = extern(\"" + v.IsExtern + "\")"
		}
		if v.Body != nil {
			return sig + " =\n" + printNode(v.Body, depth+1)
		}

		return sig
	case *UseDecl:
		if v.FromSyntax {
			return fmt.Sprintf("%suse { %s } from %s", ind(depth), strings.Join(v.Names, ", "), v.Path)
		}

		return fmt.Sprintf("%suse %s", ind(depth), v.Path)
	case *ExportDecl:

		return fmt.Sprintf("%sexport { %s } as %s", ind(depth), strings.Join(v.Names, ", "), v.AsName)
	case *MacroDecl:
		body := printNode(v.Body, depth+1)
		// Name already includes trailing "!" (e.g. "square!")
		macroName := v.Name
		if !strings.HasSuffix(macroName, "!") {
			macroName += "!"
		}
		paramStrs := make([]string, len(v.Params))
		for i, p := range v.Params {
			if i < len(v.ParamTypes) && v.ParamTypes[i] != "" {
				paramStrs[i] = p + " " + v.ParamTypes[i]
			} else {
				paramStrs[i] = p
			}
		}

		return fmt.Sprintf("%smacro %s(%s) =\n%s", ind(depth), macroName, strings.Join(paramStrs, ", "), body)
	case *StructDecl:
		var lines []string
		lines = append(lines, fmt.Sprintf("%sstruct %s =", ind(depth), v.Name))
		for _, f := range v.Fields {
			if f.Type != nil {
				lines = append(lines, fmt.Sprintf("%s%s %s", ind(depth+1), f.Name, f.Type.String()))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s", ind(depth+1), f.Name))
			}
		}
		for _, m := range v.Methods {
			lines = append(lines, printNode(m, depth+1))
		}

		return strings.Join(lines, "\n")
	case *EnumDecl:
		var lines []string
		lines = append(lines, fmt.Sprintf("%senum %s =", ind(depth), v.Name))
		for _, m := range v.Members {
			lines = append(lines, fmt.Sprintf("%s%s", ind(depth+1), m.Name))
		}

		return strings.Join(lines, "\n")
	case *TypeDecl:

		return fmt.Sprintf("%stype %s = %s", ind(depth), v.Name, v.Type.String())
	case *TestDecl:
		body := printNode(v.Body, depth+1)

		return fmt.Sprintf("%stest %q =\n%s", ind(depth), v.Desc, body)
	case *LambdaExpr:
		params := make([]string, len(v.Params))
		for i, p := range v.Params {
			if p.IsVarArgs {
				params[i] = "..."
			} else if p.Type != nil {
				params[i] = p.Name + " " + p.Type.String()
			} else {
				params[i] = p.Name
			}
		}
		ret := ""
		if v.RetType != nil {
			ret = " " + v.RetType.String()
		}
		sig := fmt.Sprintf("fn(%s)%s", strings.Join(params, ", "), ret)
		if v.Body != nil {
			return sig + " => " + printNode(v.Body, 0)
		}

		return sig

	// Statements
	case *ExprStmt:

		return printNode(v.Expr, depth)
	case *EchoStmt:

		return fmt.Sprintf("echo %s", printNode(v.Value, depth))
	case *ReturnStmt:
		if v.Value == nil {
			return "return"
		}

		return fmt.Sprintf("return %s", printNode(v.Value, depth))
	case *BreakStmt:

		return "break"
	case *DeferStmt:

		return fmt.Sprintf("defer %s", printNode(v.Call, depth))
	case *AssignStmt:

		return fmt.Sprintf("%s = %s", printNode(v.Target, depth), printNode(v.Value, depth))
	case *AugAssignStmt:

		return fmt.Sprintf("%s %s %s", printNode(v.Target, depth), v.Op, printNode(v.Value, depth))
	case *PostfixStmt:

		return fmt.Sprintf("%s%s", printNode(v.Expr, depth), v.Op)
	case *VarDecl:
		if v.Type != nil && v.Value != nil {
			return fmt.Sprintf("let %s %s = %s", v.Name, v.Type.String(), printNode(v.Value, depth))
		} else if v.Type != nil {
			return fmt.Sprintf("let %s %s", v.Name, v.Type.String())
		} else if v.Value != nil {
			return fmt.Sprintf("let %s = %s", v.Name, printNode(v.Value, depth))
		}

		return "let " + v.Name
	case *TupleDestructDecl:

		return fmt.Sprintf("let (%s) = %s", strings.Join(v.Names, ", "), printNode(v.Value, depth))
	case *IfStmt:

		return printIfStmt(v, depth)
	case *ForStmt:

		return printForStmt(v, depth)
	case *Block:

		return printBlockInline(v, depth)
	// Phase 0 - top-level var
	case *TopLevelVar:
		if v.Value != nil {
			return fmt.Sprintf("var %s %s = %s", v.Name, v.Type.String(), printNode(v.Value, depth))
		}

		return fmt.Sprintf("var %s %s", v.Name, v.Type.String())
	// Fiber nodes
	case *SpawnExpr:
		if v.DoBlock != nil {
			return fmt.Sprintf("spawn do:\n%s", printBlockInline(v.DoBlock, depth+1))
		}

		return fmt.Sprintf("spawn %s", printNode(v.Call, depth))
	case *AwaitExpr:

		return fmt.Sprintf("await %s", printNode(v.Future, depth))
	case *YieldStmt:

		return "yield"
	default:

		return fmt.Sprintf("/* unhandled: %T */", n)
	}
}

func printBlockInline(b *Block, depth int) string {
	if b == nil || len(b.Stmts) == 0 {
		return ""
	}
	var lines []string
	for _, s := range b.Stmts {
		lines = append(lines, ind(depth)+printNode(s, depth))
	}

	return strings.Join(lines, "\n")
}

func printIfStmt(v *IfStmt, depth int) string {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "if %s:\n", printNode(v.Cond, depth))
	sb.WriteString(printBlockBody(v.Then, depth+1))
	for _, ei := range v.ElseIfs {
		_, _ = fmt.Fprintf(&sb, "\n%selif %s:\n", ind(depth), printNode(ei.Cond, depth))
		sb.WriteString(printBlockBody(ei.Body, depth+1))
	}
	if v.Else != nil {
		_, _ = fmt.Fprintf(&sb, "\n%selse:\n", ind(depth))
		sb.WriteString(printBlockBody(v.Else, depth+1))
	}

	return sb.String()
}

func printForStmt(v *ForStmt, depth int) string {
	var sb strings.Builder
	switch v.Kind {
	case ForCStyle:
		typeStr := ""
		if v.VarType != nil {
			typeStr = " " + v.VarType.String()
		}
		if v.Init != nil {
			// for let i T = init; cond; post:
			_, _ = fmt.Fprintf(&sb, "for %s; %s; %s:\n",
				printNode(v.Init, depth),
				printNode(v.Cond, depth),
				printNode(v.Post, depth))
		} else if v.VarName != "" {
			// for let i T; cond; post:  (no initializer)
			_, _ = fmt.Fprintf(&sb, "for let %s%s; %s; %s:\n",
				v.VarName, typeStr,
				printNode(v.Cond, depth),
				printNode(v.Post, depth))
		} else {
			// Condition-only fallback (not standard tin but handled gracefully)
			_, _ = fmt.Fprintf(&sb, "for %s:\n", printNode(v.Cond, depth))
		}
	case ForIn:
		typeStr := ""
		if v.VarType != nil {
			typeStr = " " + v.VarType.String()
		}
		_, _ = fmt.Fprintf(&sb, "for let %s%s in %s:\n", v.VarName, typeStr, printNode(v.Iter, depth))
	case ForWhile:
		_, _ = fmt.Fprintf(&sb, "for %s:\n", printNode(v.Cond, depth))
	}
	sb.WriteString(printBlockBody(v.Body, depth+1))

	return sb.String()
}

func printBlockBody(b *Block, depth int) string {
	if b == nil || len(b.Stmts) == 0 {
		return ind(depth) + "pass"
	}
	var lines []string
	for _, s := range b.Stmts {
		lines = append(lines, ind(depth)+printNode(s, depth))
	}

	return strings.Join(lines, "\n")
}
