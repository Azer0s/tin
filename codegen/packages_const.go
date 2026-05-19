package codegen

import (
	"math/big"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) evalConstExprTyped(expr ast.Node, declType ast.TypeExpr) constant.Constant {
	if declType != nil {
		if llType, err := cg.tinTypeToLLVM(declType); err == nil {
			switch lt := llType.(type) {
			case *irtypes.IntType:
				if intTyp, bigVal := cg.evalConstExprInt(expr, lt); intTyp != nil && bigVal != nil {
					return &constant.Int{Typ: intTyp, X: bigVal}
				}

			case *irtypes.StructType:
				if lit, ok := expr.(*ast.StructLit); ok {
					return cg.evalStructLitConst(lit)
				}
			}
		}
	}

	return cg.evalConstExpr(expr)
}

// evalStructLitConst builds a compile-time LLVM constant for a struct literal
// whose fields are all constant integers. Handles the full Tin struct layout:
// { i32 type_id, vtable_ptrs..., user_field_0, ... }.
// Returns nil if any field is non-constant or non-integer.
func (cg *CodeGen) evalStructLitConst(lit *ast.StructLit) constant.Constant {
	typeName := lit.TypeName
	if typeName == "" {
		return nil
	}

	// Resolve type alias to canonical struct name.
	canonicalName := typeName
	if alias := cg.aliasTypeFor(CanonKey(typeName)); alias != nil {
		if st, ok2 := alias.(*ast.SimpleType); ok2 {
			canonicalName = st.Name
		}
	}

	st := cg.structTypeFor(CanonKey(canonicalName))
	if st == nil {
		return nil
	}

	fieldNames := cg.structFields[canonicalName]
	fieldLLVMTypes := cg.structFieldLLVMTypes[canonicalName]
	typeID := cg.structTypeIDs[canonicalName]
	numVtable := len(cg.structVtableOrder[canonicalName])
	userOff := 1 + numVtable

	// Start with all-zero constant fields matching the LLVM struct layout.
	fields := make([]constant.Constant, len(st.Fields))
	for i, ft := range st.Fields {
		fields[i] = cg.zeroConstant(ft)
	}

	// Slot 0: i32 type_id.
	fields[0] = constant.NewInt(irtypes.I32, int64(typeID))

	// Evaluate each positional field from the literal. Positional and
	// named are mutually exclusive (parser enforces it), but mirror the
	// runtime genStructLit shape either way -- silently zero-fill missing
	// trailing fields and silently drop args past the field count.
	for i, elem := range lit.Positional {
		if i >= len(fieldLLVMTypes) {
			break
		}

		llIdx := userOff + i
		if llIdx >= len(st.Fields) {
			break
		}

		fc := cg.evalConstFieldExpr(elem, fieldLLVMTypes[i])
		if fc == nil {
			return nil
		}

		fields[llIdx] = fc
	}

	// Evaluate each named field from the literal.
	for _, f := range lit.Fields {
		rawIdx := -1

		for i, fn := range fieldNames {
			if fn == f.Name {
				rawIdx = i

				break
			}
		}

		if rawIdx < 0 || rawIdx >= len(fieldLLVMTypes) {
			continue
		}

		llIdx := userOff + rawIdx
		if llIdx >= len(st.Fields) {
			continue
		}

		fc := cg.evalConstFieldExpr(f.Value, fieldLLVMTypes[rawIdx])
		if fc == nil {
			return nil
		}

		fields[llIdx] = fc
	}

	return constant.NewStruct(st, fields...)
}

// evalConstFieldExpr evaluates a single struct-field initializer to
// an LLVM constant of the field's declared type.  Handles int, float
// (f32 / f64), and `-<float-literal>` shapes -- enough to make
// `const I Complex = Complex{re: 0.0, im: 1.0}` fold without falling
// back to a runtime initializer.
func (cg *CodeGen) evalConstFieldExpr(expr ast.Node, fieldType irtypes.Type) constant.Constant {
	if ft, ok := fieldType.(*irtypes.FloatType); ok {
		if v, ok2 := evalConstFloat(expr); ok2 {
			return constant.NewFloat(ft, v)
		}

		return nil
	}

	if it, ok := fieldType.(*irtypes.IntType); ok {
		intTyp, bigVal := cg.evalConstExprInt(expr, it)
		if intTyp == nil {
			return nil
		}

		return &constant.Int{Typ: intTyp, X: bigVal}
	}

	return nil
}

// evalConstFloat reads a float literal (optionally negated) into a
// raw float64.  Used by evalConstFieldExpr for struct-const fields.
func evalConstFloat(expr ast.Node) (float64, bool) {
	switch e := expr.(type) {
	case *ast.FloatLit:
		return e.Value, true
	case *ast.IntLit:
		return float64(e.Value), true
	case *ast.UnaryExpr:
		if e.Op != "-" {
			return 0, false
		}

		inner, ok := evalConstFloat(e.Expr)
		if !ok {
			return 0, false
		}

		return -inner, true
	}

	return 0, false
}

func (cg *CodeGen) evalConstExpr(expr ast.Node) constant.Constant {
	// Delegate integer evaluation to the big.Int evaluator; float literals
	// are handled directly since float constants are always concrete values.
	switch e := expr.(type) {
	case *ast.FloatLit:
		return constant.NewFloat(irtypes.Double, e.Value)
	case *ast.StringLit:
		raw := cg.newGlobalString(e.Value).(constant.Constant)
		strType := stringFatPtrType()
		lenVal := constant.NewInt(irtypes.I64, int64(len(e.Value)))
		// Const strings live in `.rodata`; cap = -1 marks the
		// borrowed-view encoding (`++=` panics, drop skips release).
		borrowed := constant.NewInt(irtypes.I64, -1)

		return constant.NewStruct(strType, raw, lenVal, borrowed)
	}

	// Try integer path.
	if intTyp, bigVal := cg.evalConstExprInt(expr, nil); intTyp != nil && bigVal != nil {
		return &constant.Int{Typ: intTyp, X: bigVal}
	}

	return nil
}

// evalConstExprInt evaluates a Tin const expression as a (IntType, *big.Int)
// pair. declType provides the target integer type for top-level declarations
// (e.g. to resolve plain IntLit values that appear in a typed const); pass nil
// to infer type from the expression (defaults to i64 for bare literals).
// Returns (nil, nil) when the expression is not a constant integer.
func (cg *CodeGen) evalConstExprInt(expr ast.Node, hint *irtypes.IntType) (*irtypes.IntType, *big.Int) {
	switch e := expr.(type) {
	case *ast.IntLit:
		typ := hint
		if typ == nil {
			if e.Big != nil {
				typ = irtypes.I128
			} else {
				typ = irtypes.I64
			}
		}

		var raw *big.Int
		if e.Big != nil {
			raw = new(big.Int).Set(e.Big)
		} else {
			raw = big.NewInt(e.Value)
		}

		return typ, normIntBig(raw, uint(typ.BitSize))

	case *ast.UnaryExpr:
		it, inner := cg.evalConstExprInt(e.Expr, hint)
		if it == nil {
			return nil, nil
		}

		switch e.Op {
		case "-":
			result := new(big.Int).Neg(inner)

			return it, normIntBig(result, uint(it.BitSize))

		case "~":
			// bitwise NOT: ~x = -(x+1) in two's complement
			result := new(big.Int).Add(inner, big.NewInt(1))
			result.Neg(result)

			return it, normIntBig(result, uint(it.BitSize))
		}

		return nil, nil

	case *ast.AsExpr:
		targetLLVM, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, nil
		}

		toIt, toIsInt := targetLLVM.(*irtypes.IntType)
		if !toIsInt {
			return nil, nil
		}

		// Evaluate inner without hint so we get the raw value.
		_, inner := cg.evalConstExprInt(e.Expr, nil)
		if inner == nil {
			return nil, nil
		}

		return toIt, normIntBig(inner, uint(toIt.BitSize))

	case *ast.BinExpr:
		lt, left := cg.evalConstExprInt(e.Left, hint)
		if lt == nil {
			return nil, nil
		}

		// For shifts the right operand width can differ; use i64 as default.
		_, right := cg.evalConstExprInt(e.Right, irtypes.I64)
		if right == nil {
			return nil, nil
		}

		var result *big.Int

		switch e.Op {
		case "+":
			result = new(big.Int).Add(left, right)
		case "-":
			result = new(big.Int).Sub(left, right)
		case "<<":
			shift := uint(right.Uint64())
			result = new(big.Int).Lsh(left, shift)
		default:
			return nil, nil
		}

		return lt, normIntBig(result, uint(lt.BitSize))
	}

	return nil, nil
}

// normIntBig normalizes a *big.Int to the signed two's-complement range
// for an N-bit integer type: masks to N bits, then sign-extends from bit N-1.
// This ensures that e.g. (1 << 127) becomes -2^127 (I128_MIN) when bits==128.
func normIntBig(x *big.Int, bits uint) *big.Int {
	maxUnsigned := new(big.Int).Lsh(big.NewInt(1), bits) // 2^N
	maxSigned := new(big.Int).Rsh(maxUnsigned, 1)        // 2^(N-1)

	// Mask to N bits (unsigned mod 2^N).
	result := new(big.Int).And(x, new(big.Int).Sub(maxUnsigned, big.NewInt(1)))

	// Convert to signed: if result >= 2^(N-1), subtract 2^N.
	if result.Cmp(maxSigned) >= 0 {
		result.Sub(result, maxUnsigned)
	}

	return result
}
