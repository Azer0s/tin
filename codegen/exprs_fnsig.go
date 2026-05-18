package codegen

import (
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func fnSigName(ft *irtypes.FuncType, skipFirstEnv bool) string {
	var sb strings.Builder
	sb.WriteString("fn(")

	start := 0
	if skipFirstEnv && len(ft.Params) > 0 {
		start = 1
	}

	for i := start; i < len(ft.Params); i++ {
		if i > start {
			sb.WriteString(",")
		}

		sb.WriteString(llvmTypeName(ft.Params[i]))
	}

	sb.WriteString(")")

	if ft.RetType != nil && !irtypes.IsVoid(ft.RetType) {
		sb.WriteString(llvmTypeName(ft.RetType))
	}

	return sb.String()
}

// ensureFnTypeID assigns a unique compile-time type ID to a function signature
// string, reusing the existing ID if the same signature was seen before.
func (cg *CodeGen) ensureFnTypeID(sig string) int32 {
	if id, ok := cg.fnTypeIDs[sig]; ok {
		return id
	}

	id := cg.nextTypeID
	cg.nextTypeID++
	cg.fnTypeIDs[sig] = id

	return id
}

// collectFreeVars walks body and returns the names of Identifier nodes that are
// not already in localNames. VarDecl nodes add their names to localNames as they
// are encountered. Nested LambdaExpr nodes are not recursed into (they have their
// own scope and will capture independently).
func collectFreeVars(body ast.Node, localNames map[string]bool) []string {
	seen := map[string]bool{}

	var (
		result []string
		walk   func(ast.Node)
	)

	walk = func(n ast.Node) {
		if n == nil {
			return
		}

		switch v := n.(type) {
		case *ast.Identifier:
			if !localNames[v.Name] && !seen[v.Name] {
				seen[v.Name] = true
				result = append(result, v.Name)
			}
		case *ast.VarDecl:
			walk(v.Value)
			localNames[v.Name] = true
		case *ast.LambdaExpr:
			// Collect free vars of nested lambda that the current lambda needs to
			// capture so they're available in scope when the nested lambda is compiled.
			// Example: fn(b) = return fn(c) = return a+b+c
			// The outer lambda must capture 'a' even though 'a' only appears in the inner lambda.
			nestedLocals := map[string]bool{}
			for _, p := range v.Params {
				nestedLocals[p.Name] = true
			}

			for _, nf := range collectFreeVars(v.Body, nestedLocals) {
				if !localNames[nf] && !seen[nf] {
					seen[nf] = true
					result = append(result, nf)
				}
			}
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.ReturnStmt:
			walk(v.Value)
		case *ast.EchoStmt:
			walk(v.Value)
		case *ast.AssignStmt:
			walk(v.Target)
			walk(v.Value)
		case *ast.AugAssignStmt:
			walk(v.Target)
			walk(v.Value)
		case *ast.ExprStmt:
			walk(v.Expr)
		case *ast.BinExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			walk(v.Expr)
		case *ast.CallExpr:
			walk(v.Func)

			for _, a := range v.Args {
				walk(a)
			}
		case *ast.FieldAccess:
			walk(v.Expr)
		case *ast.IndexExpr:
			walk(v.Expr)
			walk(v.Index)
		case *ast.IfStmt:
			walk(v.Cond)
			walk(v.Then)

			for _, ei := range v.ElseIfs {
				walk(ei.Cond)
				walk(ei.Body)
			}

			if v.Else != nil {
				walk(v.Else)
			}
		case *ast.TernaryExpr:
			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.StructLit:
			for _, f := range v.Fields {
				walk(f.Value)
			}
		case *ast.ArrayLit:
			for _, el := range v.Elems {
				walk(el)
			}
		case *ast.ArrayFillLit:
			walk(v.Value)
		case *ast.TupleLit:
			for _, el := range v.Elems {
				walk(el)
			}
		case *ast.SliceExpr:
			walk(v.Expr)
			walk(v.Start)
			walk(v.End)
		case *ast.AddrExpr:
			walk(v.Val)
		case *ast.AddressOfExpr:
			walk(v.Expr)
		case *ast.DerefExpr:
			walk(v.Expr)
		case *ast.AsExpr:
			walk(v.Expr)
		case *ast.PipeExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.WhereList:
			for _, c := range v.Clauses {
				walk(c.Cond)
				walk(c.Body)
			}
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

			if v.Default != nil {
				walk(v.Default)
			}
		case *ast.InterpolatedString:
			for _, p := range v.Parts {
				if p.IsExpr {
					walk(p.Expr)
				}
			}
		case *ast.IsExpr:
			walk(v.Expr)
		case *ast.TypeAssertExpr:
			walk(v.Expr)
		case *ast.AwaitExpr:
			walk(v.Future)
		case *ast.SpawnExpr:
			if v.Call != nil {
				walk(v.Call)
			}
			// Don't descend into DoBlock of nested spawn do: blocks; they capture independently.
		}
	}
	walk(body)

	return result
}

// callTraitMethod dispatches x.method(args) where x is a trait fat pointer
// {i8* data, vtable*}.  It looks up the method slot index in the vtable,
// loads the function pointer, and calls it with (data, args...).
// instKey may be "named" or "iter_i64" etc.
