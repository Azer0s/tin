package codegen

import (
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func armExprNode(stmt ast.Node) ast.Node {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return s.Expr
	case *ast.MatchStmt:
		return s // genExpr handles *ast.MatchStmt directly
	}

	return nil
}

// astInferTypeWithPattern infers the type of node like astInferType but first
// pushes a temporary scope that maps pattern-bound names to their field types,
// so that renamed bindings (e.g. "x: px") are visible when node is "px".
func (cg *CodeGen) astInferTypeWithPattern(node ast.Node, pattern ast.Node) irtypes.Type {
	sp, ok := pattern.(*ast.StructPattern)
	if !ok {
		return cg.astInferType(node)
	}

	// Collect bindings: field name -> LLVM field type from the struct.
	bindings := map[string]irtypes.Type{}
	cg.collectPatternBindingTypes(sp, bindings)

	if len(bindings) == 0 {
		return cg.astInferType(node)
	}

	// Push a temporary scope with those bindings as non-alloc entries.
	cg.curScope = newScope(cg.curScope)

	for varName, llvmType := range bindings {
		cg.curScope.set(varName, &scopeEntry{val: &syntheticValue{t: llvmType}})
	}

	t := cg.astInferType(node)
	cg.curScope = cg.curScope.parent

	return t
}

// collectPatternBindingTypes walks a StructPattern and fills bindings with the
// LLVM type for each free or renamed field, recursing into nested patterns.
func (cg *CodeGen) collectPatternBindingTypes(sp *ast.StructPattern, bindings map[string]irtypes.Type) {
	llvmType := cg.structTypeFor(CanonKey(sp.TypeName))
	if llvmType == nil {
		return
	}

	for _, field := range sp.Fields {
		if field.IsWild {
			continue
		}

		idx := cg.fieldIndex(sp.TypeName, field.Name)
		if idx < 0 {
			continue
		}

		var ft irtypes.Type

		if cg.cLayoutStructs[sp.TypeName] {
			if nativeSt := cg.nativeStructTypes[sp.TypeName]; nativeSt != nil && idx < len(nativeSt.Fields) {
				ft = nativeSt.Fields[idx]
			}
		} else if idx < len(llvmType.Fields) {
			ft = llvmType.Fields[idx]
		}

		if nested, ok2 := field.Literal.(*ast.StructPattern); ok2 {
			cg.collectPatternBindingTypes(nested, bindings)

			continue
		}

		if field.Literal != nil {
			continue
		}

		bindName := field.Name
		if field.BindTo != "" {
			bindName = field.BindTo
		}

		if ft != nil {
			bindings[bindName] = ft
		}
	}
}

// syntheticValue is a zero-size placeholder value.Value used only to carry a
// type through astInferType's Identifier case without emitting any IR.
type syntheticValue struct{ t irtypes.Type }

func (s *syntheticValue) Type() irtypes.Type { return s.t }
func (s *syntheticValue) Ident() string      { return "%synthetic" }
func (s *syntheticValue) String() string     { return "%synthetic" }

// astInferType attempts to determine the LLVM type of a simple AST expression
// without generating any code. Returns nil when the type cannot be determined.
func (cg *CodeGen) astInferType(node ast.Node) irtypes.Type {
	switch e := node.(type) {
	case *ast.IntLit:
		if e.Big != nil {
			return irtypes.I128
		}

		return irtypes.I64
	case *ast.FloatLit:
		return irtypes.Double
	case *ast.BoolLit:
		return irtypes.I1
	case *ast.CharLit:
		return irtypes.I8
	case *ast.AtomLit:
		return cg.atomType
	case *ast.NilLit:
		return irtypes.I64
	case *ast.StringLit, *ast.InterpolatedString:
		return stringFatPtrType()
	case *ast.Identifier:
		en, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil
		}

		if en.isAlloc {
			return en.val.Type().(*irtypes.PointerType).ElemType
		}

		return en.val.Type()
	case *ast.BinExpr:
		switch e.Op {
		case "==", "!=", "<", ">", "<=", ">=", "&&", "||":
			return irtypes.I1
		default:
			return cg.astInferType(e.Left)
		}
	case *ast.AsExpr:
		t, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil
		}

		return t
	case *ast.UnaryExpr:
		return cg.astInferType(e.Expr)
	case *ast.FieldAccess:
		obj := cg.astInferType(e.Expr)
		if obj == nil {
			return nil
		}

		structName := cg.typeNameOf(obj)
		if structName == "" {
			return nil
		}

		idx := cg.fieldIndex(structName, e.Field)
		if idx < 0 {
			return nil
		}

		if st, ok := obj.(*irtypes.StructType); ok && idx < len(st.Fields) {
			return st.Fields[idx]
		}

		return nil
	case *ast.MatchStmt:
		// Infer from the first arm whose body is a single expression.
		for _, c := range e.Cases {
			if c.Body != nil && len(c.Body.Stmts) == 1 {
				if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
					if t := cg.astInferType(expr); t != nil {
						return t
					}
				}
			}
		}

		if e.Default != nil && len(e.Default.Stmts) == 1 {
			if expr := armExprNode(e.Default.Stmts[0]); expr != nil {
				return cg.astInferType(expr)
			}
		}

		return nil
	}

	return nil
}

// genMatchAsExpr runs a MatchStmt in expression mode: each arm body must be a
// single expression whose result is stored to a pre-allocated slot. The function
// updates cg.curBlock to the continuation block (afterBlock) so that callers
// using the cg.curBlock pattern (genVarDecl, genReturn, etc.) pick up the
// correct block for subsequent code emission.
