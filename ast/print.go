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
	case *BoolLit:
		if v.Value {
			return "true"
		}
		return "false"
	case *AtomLit:
		return "'" + v.Name
	case *CharLit:
		return fmt.Sprintf("%d", v.Value) // emit as integer literal
	case *NoneLit:
		return "none"
	case *Identifier:
		return v.Name
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
		if v.IsNone {
			return fmt.Sprintf("(%s is none)", printNode(v.Expr, depth))
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
	case *ArrayLit:
		elems := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			elems[i] = printNode(e, depth)
		}
		return fmt.Sprintf("[%s]", strings.Join(elems, ", "))

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
	case *IfStmt:
		return printIfStmt(v, depth)
	case *ForStmt:
		return printForStmt(v, depth)
	case *Block:
		return printBlockInline(v, depth)
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
	sb.WriteString(fmt.Sprintf("if %s:\n", printNode(v.Cond, depth)))
	sb.WriteString(printBlockBody(v.Then, depth+1))
	for _, ei := range v.ElseIfs {
		sb.WriteString(fmt.Sprintf("\n%selif %s:\n", ind(depth), printNode(ei.Cond, depth)))
		sb.WriteString(printBlockBody(ei.Body, depth+1))
	}
	if v.Else != nil {
		sb.WriteString(fmt.Sprintf("\n%selse:\n", ind(depth)))
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
			sb.WriteString(fmt.Sprintf("for %s; %s; %s:\n",
				printNode(v.Init, depth),
				printNode(v.Cond, depth),
				printNode(v.Post, depth)))
		} else if v.VarName != "" {
			// for let i T; cond; post:  (no initializer)
			sb.WriteString(fmt.Sprintf("for let %s%s; %s; %s:\n",
				v.VarName, typeStr,
				printNode(v.Cond, depth),
				printNode(v.Post, depth)))
		} else {
			// Condition-only fallback (not standard tin but handled gracefully)
			sb.WriteString(fmt.Sprintf("for %s:\n", printNode(v.Cond, depth)))
		}
	case ForIn:
		typeStr := ""
		if v.VarType != nil {
			typeStr = " " + v.VarType.String()
		}
		sb.WriteString(fmt.Sprintf("for let %s%s in %s:\n", v.VarName, typeStr, printNode(v.Iter, depth)))
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
