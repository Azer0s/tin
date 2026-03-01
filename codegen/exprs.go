package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/parser"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Expression generation

// genExpr generates code for an expression and returns the resulting value.
func (cg *CodeGen) genExpr(block *ir.Block, node ast.Node) (value.Value, error) {
	if node == nil {
		return nil, nil
	}
	switch e := node.(type) {
	case *ast.IntLit:
		return constant.NewInt(irtypes.I64, e.Value), nil

	case *ast.FloatLit:
		return constant.NewFloat(irtypes.Double, e.Value), nil

	case *ast.BoolLit:
		if e.Value {
			return constant.NewInt(irtypes.I1, 1), nil
		}
		return constant.NewInt(irtypes.I1, 0), nil

	case *ast.CharLit:
		return constant.NewInt(irtypes.I8, int64(e.Value)), nil

	case *ast.NoneLit:
		return constant.NewInt(irtypes.I64, 0), nil

	case *ast.AtomLit:
		// Emit atom as %__atom { i32 CRC32(name) } constant.
		return cg.atomConstant(cg.registerAtom(e.Name)), nil

	case *ast.StringLit:
		return cg.buildStringFatPtr(block, e.Value), nil

	case *ast.BacktickLit:
		// Backtick literal: compile as string with backtick delimiters.
		// If the content contains {expr} interpolations (used in CTFE macro bodies),
		// expand them so that variable values are substituted at runtime.
		// In non-CTFE macro context the expander unwraps this before codegen (see expandMacro).
		if strings.Contains(e.Content, "{") {
			node, err := parser.ParseStringInterp(e.Content)
			if err == nil {
				if interp, ok := node.(*ast.InterpolatedString); ok {
					// Wrap interpolated parts with backtick delimiters.
					parts := make([]ast.StringPart, 0, len(interp.Parts)+2)
					parts = append(parts, ast.StringPart{Str: "`"})
					parts = append(parts, interp.Parts...)
					parts = append(parts, ast.StringPart{Str: "`"})
					return cg.genInterpolatedString(block, &ast.InterpolatedString{Parts: parts})
				}
			}
		}
		return cg.buildStringFatPtr(block, "`"+e.Content+"`"), nil

	case *ast.InterpolatedString:
		return cg.genInterpolatedString(block, e)

	case *ast.Identifier:
		return cg.genIdentifier(block, e)

	case *ast.BinExpr:
		return cg.genBinExpr(block, e)

	case *ast.UnaryExpr:
		return cg.genUnaryExpr(block, e)

	case *ast.CallExpr:
		return cg.genCallExpr(block, e)

	case *ast.FieldAccess:
		return cg.genFieldAccess(block, e)

	case *ast.IndexExpr:
		return cg.genIndexExpr(block, e)

	case *ast.ScopeAccess:
		return cg.genScopeAccess(block, e)

	case *ast.ArrayLit:
		return cg.genArrayLit(block, e)

	case *ast.StructLit:
		return cg.genStructLit(block, e)

	case *ast.AsExpr:
		return cg.genAsExpr(block, e)

	case *ast.AddrExpr:
		return cg.genAddrExpr(block, e)

	case *ast.AddressOfExpr:
		return cg.genAddrOfExpr(block, e)

	case *ast.DerefExpr:
		return cg.genDerefExpr(block, e)

	case *ast.PipeExpr:
		return cg.genPipeExpr(block, e)

	case *ast.TernaryExpr:
		return cg.genTernaryExpr(block, e)

	case *ast.IsExpr:
		return cg.genIsExpr(block, e)

	case *ast.RangeExpr:
		// RangeExpr in expression context returns start value.
		return cg.genExpr(block, e.Start)

	case *ast.LambdaExpr:
		return cg.genLambdaExpr(block, e)

	case *ast.WildcardExpr:
		return constant.NewInt(irtypes.I1, 1), nil

	case *ast.DefaultExpr:
		if e.OfExpr != nil {
			// default(typeof(expr)): get LLVM type of inner expression, return zero for it.
			// e.OfExpr is the TypeofExpr node; we evaluate its inner Expr to get the type.
			inner := e.OfExpr
			if te, ok := inner.(*ast.TypeofExpr); ok {
				inner = te.Expr
			}
			val, err := cg.genExpr(block, inner)
			if err != nil {
				return nil, err
			}
			if val != nil {
				return cg.zeroValue(val.Type()), nil
			}
		}
		if e.Type != nil {
			lt, err := cg.tinTypeToLLVM(e.Type)
			if err != nil {
				return nil, err
			}
			return cg.zeroValue(lt), nil
		}
		return constant.NewInt(irtypes.I64, 0), nil

	case *ast.Block:
		// Block expression: (let x = ...; ...; last_expr) — produced by CTFE macro splices.
		// Generate all statements and return the value of the last expression.
		curBlock := block
		var lastVal value.Value = constant.NewInt(irtypes.I64, 0)
		for i, stmt := range e.Stmts {
			isLast := i == len(e.Stmts)-1
			if isLast {
				if es, ok := stmt.(*ast.ExprStmt); ok {
					v, err := cg.genExpr(curBlock, es.Expr)
					if err != nil {
						return nil, err
					}
					if v != nil {
						lastVal = v
					}
					continue
				}
			}
			newBlock, _, err := cg.genStmt(curBlock, stmt)
			if err != nil {
				return nil, err
			}
			if newBlock != nil {
				curBlock = newBlock
			}
		}
		return lastVal, nil

	case *ast.SizeofExpr:
		if e.Type == nil {
			return constant.NewInt(irtypes.I64, 0), nil
		}
		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}
		if irtypes.IsVoid(lt) {
			return constant.NewInt(irtypes.I64, 0), nil
		}
		// GEP trick: sizeof(T) = (i64) &((T*)null)[1]
		nullPtr := constant.NewNull(irtypes.NewPointer(lt))
		gepOne := block.NewGetElementPtr(lt, nullPtr, constant.NewInt(irtypes.I32, 1))
		return block.NewPtrToInt(gepOne, irtypes.I64), nil

	case *ast.TypeAssertExpr:
		inner, err := cg.genExpr(block, e.Expr)
		if err != nil || inner == nil || e.Type == nil {
			return inner, err
		}
		// Native union type cast: b.(string) — bitcast storage to target type.
		innerName := cg.typeNameOf(inner.Type())
		if _, isNative := cg.nativeUnionDecls[innerName]; isNative {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}
			st := inner.Type().(*irtypes.StructType)
			alloca := block.NewAlloca(st)
			block.NewStore(inner, alloca)
			storageGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			memberPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(targetLLVM))
			return block.NewLoad(targetLLVM, memberPtr), nil
		}
		return inner, nil

	case *ast.TypeofExpr:
		return cg.genTypeof(block, e)

	case *ast.TraitofExpr:
		return cg.genTraitof(block, e)

	case *ast.FieldnamesExpr:
		return cg.genFieldnames(block, e)

	case *ast.FieldtypesExpr:
		return cg.genFieldtypes(block, e)

	case *ast.FieldtagExpr:
		return cg.genFieldtag(block, e)

	case *ast.GetfieldExpr:
		return cg.genGetfield(block, e)

	case *ast.SetfieldExpr:
		return cg.genSetfield(block, e)

	case *ast.VarDecl:
		_, err := cg.genVarDecl(block, e)
		if err != nil {
			return nil, err
		}
		// Return the alloca'd value.
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, nil
		}
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil

	default:
		return nil, nil
	}
}

func (cg *CodeGen) genIdentifier(block *ir.Block, e *ast.Identifier) (value.Value, error) {
	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		return nil, fmt.Errorf("undefined identifier: %s", e.Name)
	}
	if entry.isAlloc {
		ptrType := entry.val.Type().(*irtypes.PointerType)
		return block.NewLoad(ptrType.ElemType, entry.val), nil
	}
	return entry.val, nil
}

func (cg *CodeGen) genBinExpr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// Short-circuit for && and ||.
	switch e.Op {
	case "&&":
		return cg.genLogicalAnd(block, e)
	case "||":
		return cg.genLogicalOr(block, e)
	}

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}

	if left == nil || right == nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	// Unify types.
	lt := left.Type()
	rt := right.Type()

	// Type promotion.
	if irtypes.IsInt(lt) && irtypes.IsInt(rt) {
		lBits := lt.(*irtypes.IntType).BitSize
		rBits := rt.(*irtypes.IntType).BitSize
		if lBits < rBits {
			left = block.NewSExt(left, rt)
			lt = rt
		} else if rBits < lBits {
			right = block.NewSExt(right, lt)
		}
	} else if irtypes.IsFloat(lt) && irtypes.IsInt(rt) {
		right = block.NewSIToFP(right, lt)
	} else if irtypes.IsInt(lt) && irtypes.IsFloat(rt) {
		left = block.NewSIToFP(left, rt)
		lt = rt
	}

	isFloat := irtypes.IsFloat(lt)

	switch e.Op {
	case "+":
		if isFloat {
			return block.NewFAdd(left, right), nil
		}
		return block.NewAdd(left, right), nil
	case "-":
		if isFloat {
			return block.NewFSub(left, right), nil
		}
		return block.NewSub(left, right), nil
	case "*":
		if isFloat {
			return block.NewFMul(left, right), nil
		}
		return block.NewMul(left, right), nil
	case "/":
		if isFloat {
			return block.NewFDiv(left, right), nil
		}
		return block.NewSDiv(left, right), nil
	case "%":
		return block.NewSRem(left, right), nil
	case "==":
		if isFloat {
			return block.NewFCmp(enum.FPredOEQ, left, right), nil
		}
		// any equality: dynamically dispatched by runtime.
		if isAnyType(lt) || isAnyType(rt) {
			if !isAnyType(lt) {
				left = cg.boxToAny(block, left)
			}
			if !isAnyType(rt) {
				right = cg.boxToAny(block, right)
			}
			cmp := block.NewCall(cg.ensureAnyEq(), left, right)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I64, 0)), nil
		}
		// atom == atom: compare CRC32 codes directly.
		if isAtomType(lt) && isAtomType(rt) {
			lcode := cg.extractAtomCode(block, left)
			rcode := cg.extractAtomCode(block, right)
			return block.NewICmp(enum.IPredEQ, lcode, rcode), nil
		}
		// atom == string or string == atom: convert atom to string, then strcmp.
		if isAtomType(lt) && isFatPtrType(rt) {
			strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, left))
			lptr := cg.extractStringPtr(block, strVal)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		if isFatPtrType(lt) && isAtomType(rt) {
			strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, right))
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, strVal)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		// String equality: compare via strcmp.
		if isFatPtrType(lt) {
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		return block.NewICmp(enum.IPredEQ, left, right), nil
	case "!=":
		if isFloat {
			return block.NewFCmp(enum.FPredONE, left, right), nil
		}
		// any inequality: dynamically dispatched by runtime.
		if isAnyType(lt) || isAnyType(rt) {
			if !isAnyType(lt) {
				left = cg.boxToAny(block, left)
			}
			if !isAnyType(rt) {
				right = cg.boxToAny(block, right)
			}
			cmp := block.NewCall(cg.ensureAnyEq(), left, right)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I64, 0)), nil
		}
		// atom != atom
		if isAtomType(lt) && isAtomType(rt) {
			lcode := cg.extractAtomCode(block, left)
			rcode := cg.extractAtomCode(block, right)
			return block.NewICmp(enum.IPredNE, lcode, rcode), nil
		}
		// atom != string or string != atom
		if isAtomType(lt) && isFatPtrType(rt) {
			strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, left))
			lptr := cg.extractStringPtr(block, strVal)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		if isFatPtrType(lt) && isAtomType(rt) {
			strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, right))
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, strVal)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		// String inequality: compare via strcmp.
		if isFatPtrType(lt) {
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		return block.NewICmp(enum.IPredNE, left, right), nil
	case "<":
		if isFloat {
			return block.NewFCmp(enum.FPredOLT, left, right), nil
		}
		return block.NewICmp(enum.IPredSLT, left, right), nil
	case "<=":
		if isFloat {
			return block.NewFCmp(enum.FPredOLE, left, right), nil
		}
		return block.NewICmp(enum.IPredSLE, left, right), nil
	case ">":
		if isFloat {
			return block.NewFCmp(enum.FPredOGT, left, right), nil
		}
		return block.NewICmp(enum.IPredSGT, left, right), nil
	case ">=":
		if isFloat {
			return block.NewFCmp(enum.FPredOGE, left, right), nil
		}
		return block.NewICmp(enum.IPredSGE, left, right), nil
	case "&":
		return block.NewAnd(left, right), nil
	case "|":
		return block.NewOr(left, right), nil
	case "^":
		return block.NewXor(left, right), nil
	case "<<":
		return block.NewShl(left, right), nil
	case ">>":
		return block.NewAShr(left, right), nil
	case "++":
		// Typed array concatenation: {T*, i64} ++ {T*, i64} -> {T*, i64}
		// (strings {i8*, i64} are handled by the string path below)
		if isFatArrayPtr(left.Type()) && !isStringType(left.Type()) {
			fatType := left.Type().(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			leftDataPtr := block.NewExtractValue(left, 0)
			leftLen := block.NewExtractValue(left, 1)
			rightDataPtr := block.NewExtractValue(right, 0)
			rightLen := block.NewExtractValue(right, 1)
			totalLen := block.NewAdd(leftLen, rightLen)

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)

			// new_ptr = _tin_rc_alloc(totalLen * elemSize)
			totalBytes := block.NewMul(totalLen, elemSize)
			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), totalBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy left data
			leftBytes := block.NewMul(leftLen, elemSize)
			leftI8Ptr := block.NewBitCast(leftDataPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, leftI8Ptr, leftBytes, constant.NewInt(irtypes.I1, 0))

			// memcpy right data at offset leftLen*elemSize
			rightOffset := block.NewMul(leftLen, elemSize)
			rightDst := block.NewGetElementPtr(irtypes.I8, newI8Ptr, rightOffset)
			rightI8Ptr := block.NewBitCast(rightDataPtr, irtypes.I8Ptr)
			rightBytes := block.NewMul(rightLen, elemSize)
			block.NewCall(cg.ensureMemcpy(), rightDst, rightI8Ptr, rightBytes, constant.NewInt(irtypes.I1, 0))

			// Build new fat ptr {T*, i64}
			fatAlloca := block.NewAlloca(fatType)
			ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(newPtr, ptrGep)
			lenGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(totalLen, lenGep)
			return block.NewLoad(fatType, fatAlloca), nil
		}

		// String concatenation: both operands are {i8*, i64} fat-ptrs.
		leftPtr := cg.extractStringPtr(block, left)
		leftLen := cg.extractStringLen(block, left)
		rightPtr := cg.extractStringPtr(block, right)
		rightLen := cg.extractStringLen(block, right)
		totalLen := block.NewAdd(leftLen, rightLen)
		// rc_alloc(totalLen + 1) for null terminator; ARC manages the result.
		allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
		buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
		// memcpy(buf, leftPtr, leftLen)
		block.NewCall(cg.ensureMemcpy(), buf, leftPtr, leftLen, constant.NewInt(irtypes.I1, 0))
		// memcpy(buf + leftLen, rightPtr, rightLen)
		rightDst := block.NewGetElementPtr(irtypes.I8, buf, leftLen)
		block.NewCall(cg.ensureMemcpy(), rightDst, rightPtr, rightLen, constant.NewInt(irtypes.I1, 0))
		// null-terminate
		nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
		block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)
		// build {i8*, i64} fat-ptr result
		fatPtrType := stringFatPtrType()
		alloca := block.NewAlloca(fatPtrType)
		gep0 := block.NewGetElementPtr(fatPtrType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(buf, gep0)
		gep1 := block.NewGetElementPtr(fatPtrType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(totalLen, gep1)
		return block.NewLoad(fatPtrType, alloca), nil
	}

	return constant.NewInt(irtypes.I64, 0), nil
}

func (cg *CodeGen) genLogicalAnd(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// genExpr does not thread the current block through return values, so we
	// cannot use real branches here (the caller would keep using the original
	// block which is already terminated, leaving the merge block without a
	// terminator).  Use `select` instead: semantics are identical for pure
	// operands, and side-effectful short-circuit can be revisited later.
	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	leftBool := cg.toBool(block, left)

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	rightBool := cg.toBool(block, right)

	// true && x = x;  false && _ = false
	return block.NewSelect(leftBool, rightBool, constant.NewInt(irtypes.I1, 0)), nil
}

func (cg *CodeGen) genLogicalOr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	leftBool := cg.toBool(block, left)

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	rightBool := cg.toBool(block, right)

	// false || x = x;  true || _ = true
	return block.NewSelect(leftBool, constant.NewInt(irtypes.I1, 1), rightBool), nil
}

func (cg *CodeGen) genUnaryExpr(block *ir.Block, e *ast.UnaryExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	switch e.Op {
	case "-":
		if irtypes.IsFloat(val.Type()) {
			return block.NewFNeg(val), nil
		}
		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), val.Type())
		return block.NewSub(zero, val), nil
	case "!":
		b := cg.toBool(block, val)
		return block.NewXor(b, constant.NewInt(irtypes.I1, 1)), nil
	case "~":
		minusOne := cg.coerce(block, constant.NewInt(irtypes.I64, -1), val.Type())
		return block.NewXor(val, minusOne), nil
	case "*":
		// Dereference
		if pt, ok := val.Type().(*irtypes.PointerType); ok {
			return block.NewLoad(pt.ElemType, val), nil
		}
		return val, nil
	}
	return val, nil
}


func (cg *CodeGen) genCallExpr(block *ir.Block, e *ast.CallExpr) (value.Value, error) {
	// Resolve callee.
	var callee value.Value
	var calleeType *irtypes.FuncType

	switch fn := e.Func.(type) {
	case *ast.Identifier:
		// CTFE: evaluate #pure #no_recurse calls with constant arguments at compile time.
		if ctfeResult, err := cg.tryEvalPureCall(e); err != nil {
			return nil, err
		} else if ctfeResult != nil {
			return ctfeResult, nil
		}
		// Macro expansion: check before scope lookup.
		macroName := fn.Name
		if macro, ok := cg.macros[macroName]; ok {
			return cg.expandMacro(block, macro, e.Args)
		}
		// Also check with trailing ! stripped (for macro! call syntax).
		if strings.HasSuffix(fn.Name, "!") {
			baseName := fn.Name[:len(fn.Name)-1]
			if macro, ok := cg.macros[baseName+"!"]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}
			if macro, ok := cg.macros[baseName]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}
		}
		// Built-in: len(expr)
		if fn.Name == "len" && len(e.Args) == 1 {
			return cg.genBuiltinLen(block, e.Args[0])
		}
		// Built-in: panic(msg)
		if fn.Name == "panic" && len(e.Args) == 1 {
			return cg.genBuiltinPanic(block, e.Args[0])
		}
		// Built-in: default(TypeName) — returns the zero value for a type.
		// Used in generic code to produce a typed zero without knowing the concrete type.
		if fn.Name == "default" && len(e.Args) == 1 {
			return cg.genBuiltinDefault(block, e.Args[0])
		}
		// Check if this is a constrained generic function call — monomorphize it.
		if tmpl, ok := cg.constrainedFuncs[fn.Name]; ok {
			// Evaluate arguments first to infer concrete types.
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}
				argVals = append(argVals, av)
			}
			typeSubst := cg.inferTypeArgs(tmpl, argVals)
			// Build instance key from substituted types.
			instKey := ""
			for i, tp := range tmpl.TypeParams {
				if i > 0 {
					instKey += "__"
				}
				if name, found := typeSubst[tp]; found {
					instKey += name
				} else {
					instKey += tp
				}
			}
			concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
			if err2 != nil {
				return nil, err2
			}
			// Adapt args if needed and call.
			argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
			return block.NewCall(concreteFunc, argVals...), nil
		}
		entry, ok := cg.curScope.lookup(fn.Name)
		if !ok {
			return nil, fmt.Errorf("undefined function: %s", fn.Name)
		}
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			loaded := block.NewLoad(ptrType.ElemType, entry.val)
			// If it's a closure fat pointer, call through it.
			if isFatFnPtr(loaded.Type()) {
				return cg.callFatFn(block, loaded, e.Args)
			}
			callee = loaded
		} else {
			callee = entry.val
		}

	case *ast.FieldAccess:
		// Method call: obj.method(args...) or ptr->method(args...)
		objVal, err := cg.genExpr(block, fn.Expr)
		if err != nil {
			return nil, err
		}

		// -> operator: dereference the pointer-to-struct to get the struct value.
		if fn.IsPtr {
			if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
				objVal = block.NewLoad(pt.ElemType, objVal)
			}
		}

		// Trait fat-pointer dispatch: if obj is {i8*, vtable*}, use vtable.
		if traitName, ok := cg.isTraitFatPtr(objVal.Type()); ok {
			return cg.callTraitMethod(block, objVal, traitName, fn.Field, e.Args)
		}

		// Concrete struct method: resolve as StructName_method.
		structName := cg.typeNameOf(objVal.Type())
		methodName := structName + "_" + fn.Field
		entry, ok := cg.curScope.lookup(methodName)
		if !ok {
			// Also check without prefix.
			entry, ok = cg.curScope.lookup(fn.Field)
		}
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)
				callee = block.NewLoad(ptrType.ElemType, entry.val)
			} else {
				callee = entry.val
			}
			// Determine the first argument: if the method expects a pointer
			// receiver (*Struct), pass the address of the object rather than
			// its value so that mutations through `this` are visible to the caller.
			var thisArg value.Value = objVal
			if f, ok2 := callee.(*ir.Func); ok2 && len(f.Sig.Params) > 0 {
				firstParam := f.Sig.Params[0]
				if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr {
					if pt.ElemType.Equal(objVal.Type()) {
						// Try to get the lvalue (alloca) for the receiver expression.
						if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
							thisArg = lv
						} else {
							// Fallback: store to a temp alloca (mutations are lost,
							// but this keeps the call type-correct).
							tmp := block.NewAlloca(objVal.Type())
							block.NewStore(objVal, tmp)
							thisArg = tmp
						}
					}
				}
			}
			// Build args with obj first.
			llArgs := make([]value.Value, 0, len(e.Args)+1)
			llArgs = append(llArgs, thisArg)
			for _, arg := range e.Args {
				av, err := cg.genExpr(block, arg)
				if err != nil {
					return nil, err
				}
				llArgs = append(llArgs, av)
			}
			// Adapt arg types to function signature.
			if f, ok2 := callee.(*ir.Func); ok2 {
				calleeType = f.Sig
				llArgs = cg.adaptArgs(block, llArgs, calleeType)
			}
			return block.NewCall(callee, llArgs...), nil
		}
		_ = objVal
		return nil, fmt.Errorf("undefined method: %s.%s", structName, fn.Field)

	case *ast.ScopeAccess:
		// e.g. weather.sunny used as function - probably an error, but handle gracefully.
		v, err := cg.genScopeAccess(block, fn)
		if err != nil {
			return nil, err
		}
		callee = v

	default:
		var err error
		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}
		// If the expression evaluated to a fat fn pointer, call through it.
		if callee != nil && isFatFnPtr(callee.Type()) {
			return cg.callFatFn(block, callee, e.Args)
		}
	}

	if callee == nil {
		return nil, fmt.Errorf("nil callee")
	}

	// Build arguments. Keep pre-coercion values for ARC temporary release.
	llArgs := make([]value.Value, 0, len(e.Args))
	llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
	for _, arg := range e.Args {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		if av != nil {
			llArgs = append(llArgs, av)
			llArgsPreCoerce = append(llArgsPreCoerce, av)
		}
	}

	// Adapt argument types.
	if f, ok := callee.(*ir.Func); ok {
		calleeType = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			calleeType = ft
		}
	}
	if calleeType != nil {
		llArgs = cg.adaptArgs(block, llArgs, calleeType)
	}

	result := block.NewCall(callee, llArgs...)

	// ARC: release temporary RC-tracked arguments.  Fresh allocations (array
	// literals, concat results, function-call return values, etc.) that are
	// passed directly without being stored in a named variable have nobody to
	// release them after the callee finishes.  The callee retains on entry and
	// releases on exit, so the net rc after the call is still 1.  We drop our
	// owning reference here to reach rc=0 and free the block.
	argIdx := 0
	for _, astArg := range e.Args {
		if argIdx >= len(llArgsPreCoerce) {
			break
		}
		av := llArgsPreCoerce[argIdx]
		argIdx++
		if !isRCTrackedType(av.Type()) {
			continue
		}
		if isCopyExpr(astArg) {
			// Named variable: its scope entry will release it at scope exit.
			continue
		}
		// Temporary fresh allocation: release our reference.
		cg.emitRelease(block, av)
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) adaptArgs(block *ir.Block, args []value.Value, sig *irtypes.FuncType) []value.Value {
	if sig == nil {
		return args
	}
	result := make([]value.Value, len(args))
	for i, arg := range args {
		if i < len(sig.Params) {
			result[i] = cg.coerce(block, arg, sig.Params[i])
		} else if sig.Variadic && arg != nil && isAtomType(arg.Type()) {
			// Variadic position: atoms must become i8* (the atom string rep).
			code := cg.extractAtomCode(block, arg)
			strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
			result[i] = cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
		} else if sig.Variadic && arg != nil && isFatPtrType(arg.Type()) {
			// Variadic position: fat-ptrs are not valid C varargs — unwrap to
			// the underlying raw pointer so printf-style calls work correctly.
			result[i] = cg.extractFatPtrData(block, arg, arg.Type().(*irtypes.StructType))
		} else {
			result[i] = arg
		}
	}
	return result
}

func (cg *CodeGen) genFieldAccess(block *ir.Block, e *ast.FieldAccess) (value.Value, error) {
	// Check if this is an enum member access: EnumName.Member
	if id, ok := e.Expr.(*ast.Identifier); ok {
		key := id.Name + "." + e.Field
		if val, ok2 := cg.enumValues[key]; ok2 {
			baseType := cg.enumTypes[id.Name]
			if it, ok3 := baseType.(*irtypes.IntType); ok3 {
				return constant.NewInt(it, val), nil
			}
			// Atom enum: wrap i32 code in %__atom struct.
			if isAtomType(baseType) {
				return cg.atomConstant(int32(val)), nil
			}
			return constant.NewInt(irtypes.I32, val), nil
		}
	}

	obj, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}

	// If pointer, dereference first.
	objType := obj.Type()
	if e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			obj = block.NewLoad(pt.ElemType, obj)
			objType = pt.ElemType
		}
	}
	// Auto-deref: when obj is a pointer-to-named-struct, dereference it even
	// without the -> operator.  This handles pointer receiver methods where
	// `this *Foo` fields are accessed with `this.field` rather than `this->field`.
	if !e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			if cg.typeNameOf(pt.ElemType) != "" {
				obj = block.NewLoad(pt.ElemType, obj)
				objType = pt.ElemType
			}
		}
	}

	// Handle .len on dynamic arrays {T*, i64} and strings {i8*, i64}.
	if e.Field == "len" && (isFatArrayPtr(objType) || isStringType(objType)) {
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)
		gep := block.NewGetElementPtr(objType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		return block.NewLoad(irtypes.I64, gep), nil
	}

	structName := cg.typeNameOf(objType)

	// Native union field access: bitcast storage to member type and load.
	if ud, isNative := cg.nativeUnionDecls[structName]; isNative {
		for _, m := range ud.Members {
			if m.FieldName == e.Field {
				memberLLVM, err2 := cg.tinTypeToLLVM(m.Type)
				if err2 != nil {
					return nil, err2
				}
				alloca := block.NewAlloca(objType)
				block.NewStore(obj, alloca)
				storageGEP := block.NewGetElementPtr(objType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				memberPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(memberLLVM))
				return block.NewLoad(memberLLVM, memberPtr), nil
			}
		}
		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
	}

	fieldIdx := cg.fieldIndex(structName, e.Field)
	if fieldIdx < 0 {
		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
	}

	// We need a pointer to the struct to do GEP.
	alloca := block.NewAlloca(objType)
	block.NewStore(obj, alloca)
	gep := block.NewGetElementPtr(objType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	// Load the field.
	if st, ok := objType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
		return block.NewLoad(st.Fields[fieldIdx], gep), nil
	}
	return block.NewLoad(irtypes.I64, gep), nil
}

func (cg *CodeGen) genIndexExpr(block *ir.Block, e *ast.IndexExpr) (value.Value, error) {
	arr, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	idx, err := cg.genExpr(block, e.Index)
	if err != nil {
		return nil, err
	}
	if arr == nil || idx == nil {
		return nil, nil
	}

	idx = cg.coerce(block, idx, irtypes.I64)

	// Check if it's a fat-ptr (dynamic array) or regular array.
	arrType := arr.Type()
	switch at := arrType.(type) {
	case *irtypes.StructType:
		if len(at.Fields) == 2 {
			// Fat pointer: {T*, i64}
			elemPtrType := at.Fields[0]
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)
			ptrGep := block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			dataPtr := block.NewLoad(elemPtrType, ptrGep)
			if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
				elemGep := block.NewGetElementPtr(pt.ElemType, dataPtr, idx)
				return block.NewLoad(pt.ElemType, elemGep), nil
			}
		}
	case *irtypes.ArrayType:
		alloca := block.NewAlloca(arrType)
		block.NewStore(arr, alloca)
		gep := block.NewGetElementPtr(arrType, alloca,
			constant.NewInt(irtypes.I32, 0), idx)
		return block.NewLoad(at.ElemType, gep), nil
	case *irtypes.PointerType:
		gep := block.NewGetElementPtr(at.ElemType, arr, idx)
		return block.NewLoad(at.ElemType, gep), nil
	}
	return nil, nil
}

func (cg *CodeGen) genScopeAccess(block *ir.Block, e *ast.ScopeAccess) (value.Value, error) {
	// e.g. weather.sunny -> look up "weather.sunny" in enum registry.
	if len(e.Path) == 2 {
		key := e.Path[0] + "." + e.Path[1]
		if val, ok := cg.enumValues[key]; ok {
			baseType := cg.enumTypes[e.Path[0]]
			if it, ok2 := baseType.(*irtypes.IntType); ok2 {
				return constant.NewInt(it, val), nil
			}
			return constant.NewInt(irtypes.I32, val), nil
		}
	}
	// Try identifier lookup.
	joined := strings.Join(e.Path, ".")
	entry, ok := cg.curScope.lookup(joined)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil
	}
	// For 3+ segment paths like std::math::floor, try dropping the first segment:
	// "math.floor" after failing "std.math.floor".
	if len(e.Path) >= 3 {
		tail := strings.Join(e.Path[1:], ".")
		entry, ok = cg.curScope.lookup(tail)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)
				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}
			return entry.val, nil
		}
	}
	// Try last element.
	last := e.Path[len(e.Path)-1]
	entry, ok = cg.curScope.lookup(last)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil
	}
	// Try struct static method: TypeName::method or TypeName[T]::method
	// Scope key is "TypeName_method" (set when struct is compiled with static methods).
	if len(e.Path) >= 2 {
		baseName := e.Path[0]
		typeParamStr := ""
		if i := strings.Index(baseName, "["); i >= 0 {
			typeParamStr = strings.TrimSuffix(baseName[i+1:], "]")
			baseName = baseName[:i]
		}
		staticKey := baseName + "_" + last
		entry, ok = cg.curScope.lookup(staticKey)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)
				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}
			return entry.val, nil
		}
		// On-demand monomorphization: if baseName is a generic struct template and
		// we have a concrete type param, monomorphize now and retry.
		if typeParamStr != "" {
			if _, isGeneric := cg.genericStructs[baseName]; isGeneric {
				concreteName := baseName + "__" + typeParamStr
				if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
					typeParamTE := &ast.SimpleType{Name: typeParamStr}
					synthDecl := &ast.TypeDecl{
						Name: concreteName,
						Type: &ast.GenericType{Name: baseName, TypeParams: []ast.TypeExpr{typeParamTE}},
					}
					_ = cg.genTypeDecl(synthDecl) // ignore error; best-effort
				}
				concreteStaticKey := concreteName + "_" + last
				entry, ok = cg.curScope.lookup(concreteStaticKey)
				if ok {
					if entry.isAlloc {
						ptrType := entry.val.Type().(*irtypes.PointerType)
						return block.NewLoad(ptrType.ElemType, entry.val), nil
					}
					return entry.val, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("undefined: %s", strings.Join(e.Path, "::"))
}

func (cg *CodeGen) genArrayLit(block *ir.Block, e *ast.ArrayLit) (value.Value, error) {
	if len(e.Elems) == 0 {
		// Empty dynamic array: {null, 0}
		fat := stringFatPtrType() // {i8*, i64} - reuse structure
		alloca := block.NewAlloca(fat)
		ptrGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewNull(irtypes.I8Ptr), ptrGep)
		lenGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenGep)
		return block.NewLoad(fat, alloca), nil
	}

	vals := make([]value.Value, len(e.Elems))
	for i, elem := range e.Elems {
		v, err := cg.genExpr(block, elem)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}

	elemType := vals[0].Type()
	n := int64(len(vals))

	// Compute element size via GEP trick: sizeof(elemType) = gep(null, 1) as i64.
	nullPtr := constant.NewNull(irtypes.NewPointer(elemType))
	sizeGep := block.NewGetElementPtr(elemType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSize := block.NewMul(elemSize, constant.NewInt(irtypes.I64, n))

	// Heap-allocate array data (ARC-managed so rc=1 initially).
	mallocI8 := block.NewCall(cg.ensureRCAlloc(), totalSize)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(elemType))

	// Store elements into heap memory.
	for i, v := range vals {
		gep := block.NewGetElementPtr(elemType, dataPtr, constant.NewInt(irtypes.I64, int64(i)))
		block.NewStore(v, gep)
	}

	// Return as fat pointer {T*, i64}.
	fatType := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)
	fatAlloca := block.NewAlloca(fatType)
	ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, ptrGep)
	lenGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, n), lenGep)
	return block.NewLoad(fatType, fatAlloca), nil
}

func (cg *CodeGen) genStructLit(block *ir.Block, e *ast.StructLit) (value.Value, error) {
	st, ok := cg.structTypes[e.TypeName]
	if !ok {
		return nil, fmt.Errorf("unknown struct type: %s", e.TypeName)
	}

	alloca := block.NewAlloca(st)
	fieldNames := cg.structFields[e.TypeName]
	vtableOff := cg.vtableOffset(e.TypeName)
	// userOff = 1 (type_id) + vtable fields
	userOff := 1 + vtableOff

	// Initialise the leading i32 type_id field (index 0).
	if typeID, ok := cg.structTypeIDs[e.TypeName]; ok {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	// Initialise embedded vtable pointer fields (indices 1 … vtableOff).
	for i, instKey := range cg.structVtableOrder[e.TypeName] {
		vtableKey := e.TypeName + "__" + instKey
		if vg, ok := cg.traitVtableGlobals[vtableKey]; ok {
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(1+i)))
			block.NewStore(vg, gep)
		}
	}

	if len(e.Positional) > 0 {
		for i, v := range e.Positional {
			idx := userOff + i
			if idx >= len(st.Fields) {
				break
			}
			val, err := cg.genExpr(block, v)
			if err != nil {
				return nil, err
			}
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
			val = cg.coerce(block, val, st.Fields[idx])
			block.NewStore(val, gep)
		}
	} else {
		for _, f := range e.Fields {
			rawIdx := -1
			for i, fn := range fieldNames {
				if fn == f.Name {
					rawIdx = i
					break
				}
			}
			if rawIdx < 0 {
				continue
			}
			idx := userOff + rawIdx
			val, err := cg.genExpr(block, f.Value)
			if err != nil {
				return nil, err
			}
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
			val = cg.coerce(block, val, st.Fields[idx])
			block.NewStore(val, gep)
		}
	}
	result := block.NewLoad(st, alloca)

	// Call the struct's init method (if defined) for side-effects.
	// Per spec: "fn init(this S) = ..." is called on every struct literal
	// except those created via malloc.
	initName := e.TypeName + "_init"
	if initFn, ok := cg.curScope.lookup(initName); ok {
		if fn, ok2 := initFn.val.(*ir.Func); ok2 {
			args := cg.adaptArgs(block, []value.Value{result}, fn.Sig)
			block.NewCall(fn, args...)
		}
	}

	return result, nil
}

func (cg *CodeGen) genAsExpr(block *ir.Block, e *ast.AsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	targetType, err := cg.tinTypeToLLVM(e.Type)
	if err != nil {
		return nil, err
	}
	return cg.coerce(block, val, targetType), nil
}

func (cg *CodeGen) genAddrExpr(block *ir.Block, e *ast.AddrExpr) (value.Value, error) {
	// addr(N) where N is an integer literal: treat as inttoptr cast (raw address).
	if il, ok := e.Val.(*ast.IntLit); ok {
		v := constant.NewInt(irtypes.I64, il.Value)
		return block.NewIntToPtr(v, irtypes.I8Ptr), nil
	}
	return cg.genLValue(block, e.Val)
}

func (cg *CodeGen) genAddrOfExpr(block *ir.Block, e *ast.AddressOfExpr) (value.Value, error) {
	return cg.genLValue(block, e.Expr)
}

func (cg *CodeGen) genDerefExpr(block *ir.Block, e *ast.DerefExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	if pt, ok := val.Type().(*irtypes.PointerType); ok {
		return block.NewLoad(pt.ElemType, val), nil
	}
	return val, nil
}

func (cg *CodeGen) genPipeExpr(block *ir.Block, e *ast.PipeExpr) (value.Value, error) {
	// a |> f(args) = f(args)(a)  — curried style: call f(args) first, then call
	// the returned function with a.
	// a |> f         = f(a)      — plain function value on the right.
	leftVal, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	// Evaluate the right-hand side completely (including any call arguments),
	// yielding the function to apply to leftVal.
	rightFn, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	if rightFn == nil {
		return leftVal, nil
	}
	// If rightFn is a closure fat pointer {fn*, i8*}, call through it.
	if isFatFnPtr(rightFn.Type()) {
		fnPtr := block.NewExtractValue(rightFn, 0)
		envPtr := block.NewExtractValue(rightFn, 1)
		fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		llArgs := cg.adaptArgs(block, []value.Value{envPtr, leftVal}, fnType)
		result := block.NewCall(fnPtr, llArgs...)
		if irtypes.IsVoid(result.Type()) {
			return nil, nil
		}
		return result, nil
	}
	// Plain function pointer.
	result := block.NewCall(rightFn, leftVal)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) genTernaryExpr(block *ir.Block, e *ast.TernaryExpr) (value.Value, error) {
	cond, err := cg.genExpr(block, e.Cond)
	if err != nil {
		return nil, err
	}
	cond = cg.toBool(block, cond)

	thenVal, err := cg.genExpr(block, e.Then)
	if err != nil {
		return nil, err
	}
	elseVal, err := cg.genExpr(block, e.Else)
	if err != nil {
		return nil, err
	}

	if thenVal == nil {
		thenVal = constant.NewInt(irtypes.I64, 0)
	}
	if elseVal == nil {
		elseVal = constant.NewInt(irtypes.I64, 0)
	}

	// Unify types.
	elseVal = cg.coerce(block, elseVal, thenVal.Type())
	return block.NewSelect(cond, thenVal, elseVal), nil
}

func (cg *CodeGen) genIsExpr(block *ir.Block, e *ast.IsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	if e.IsNone {
		// For a data-type tagged union: check variant_tag (field 1) == 0.
		if st, ok := val.Type().(*irtypes.StructType); ok {
			if dataName := cg.typeNameOf(val.Type()); dataName != "" {
				if _, isData := cg.dataDecls[dataName]; isData {
					alloca := block.NewAlloca(st)
					block.NewStore(val, alloca)
					// Field 0 = i32 type_id, field 1 = i8 variant_tag.
					tagGEP := block.NewGetElementPtr(st, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
					tag := block.NewLoad(irtypes.I8, tagGEP)
					return block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I8, 0)), nil
				}
			}
		}
		// Fallback: check for zero / null.
		zero := cg.zeroValue(val.Type())
		if irtypes.IsPointer(val.Type()) {
			null := constant.NewNull(val.Type().(*irtypes.PointerType))
			return block.NewICmp(enum.IPredEQ, val, null), nil
		}
		return block.NewICmp(enum.IPredEQ, val, zero), nil
	}

	// Typed is-check: "x is v T" — check the tag and optionally bind the payload.
	if st, ok := val.Type().(*irtypes.StructType); ok {
		typeName := cg.typeNameOf(val.Type())

		// Tagged union is-check: "a is i i8" where a is type u = i8 | string.
		if members, isUnion := cg.unionTypeMembers[typeName]; isUnion && e.Type != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}
			tag := int8(-1)
			for i, te := range members {
				lt, err3 := cg.tinTypeToLLVM(te)
				if err3 != nil {
					continue
				}
				if lt.Equal(targetLLVM) {
					tag = int8(i)
					break
				}
			}
			if tag < 0 {
				tag = 0
			}
			alloca := block.NewAlloca(st)
			block.NewStore(val, alloca)
			// Field 1 = i8 tag (field 0 is i32 type_id).
			tagGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			tagVal := block.NewLoad(irtypes.I8, tagGEP)
			cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I8, int64(tag)))
			if e.VarName != "" {
				// Field 2 = [N x i8] payload.
				payloadGEP := block.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := block.NewAlloca(targetLLVM)
				payloadVal := block.NewLoad(targetLLVM, payloadPtr)
				block.NewStore(payloadVal, payloadAlloca)
				cg.curScope.set(e.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true})
			}
			return cmp, nil
		}

		dataName := typeName
		if dd, isData := cg.dataDecls[dataName]; isData {
			// Find the variant matching e.Type.
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}
			variantTag := int8(-1)
			for i, v := range dd.Variants {
				if v.Type == nil {
					continue
				}
				lt, err3 := cg.tinTypeToLLVM(v.Type)
				if err3 != nil {
					continue
				}
				if lt.Equal(targetLLVM) || llvmTypeSize(lt) == llvmTypeSize(targetLLVM) {
					key := fmt.Sprintf("%s.%d", dataName, i)
					variantTag = cg.dataVariantTags[key]
					break
				}
			}
			if variantTag < 0 {
				variantTag = 1 // default first typed variant
			}
			alloca := block.NewAlloca(st)
			block.NewStore(val, alloca)
			// Field 0 = i32 type_id, field 1 = i8 variant_tag, field 2 = payload.
			tagGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			tag := block.NewLoad(irtypes.I8, tagGEP)
			cmp := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I8, int64(variantTag)))
			// Bind variable if requested.
			if e.VarName != "" {
				payloadGEP := block.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := block.NewAlloca(targetLLVM)
				payloadVal := block.NewLoad(targetLLVM, payloadPtr)
				block.NewStore(payloadVal, payloadAlloca)
				cg.curScope.set(e.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true})
			}
			return cmp, nil
		}
	}
	// any type check: "x is dog" where x is any — compare type_id (field 0).
	if isAnyType(val.Type()) && e.Type != nil {
		targetName := ""
		switch t := e.Type.(type) {
		case *ast.SimpleType:
			targetName = t.Name
		}
		if targetName != "" {
			var targetID int32
			var found bool
			if id, ok := cg.structTypeIDs[targetName]; ok {
				targetID = id
				found = true
			} else if id, ok := cg.dataTypeIDs[targetName]; ok {
				targetID = id
				found = true
			}
			if found {
				anyType := anyFatPtrType()
				anyAlloca := block.NewAlloca(anyType)
				block.NewStore(val, anyAlloca)
				tagGep := block.NewGetElementPtr(anyType, anyAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				tag := block.NewLoad(irtypes.I32, tagGep)
				cmp := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I32, int64(targetID)))
				// Bind variable: extract data pointer and cast to the target type.
				if e.VarName != "" {
					targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
					if err2 == nil {
						ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
						dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
						typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(targetLLVM))
						typedVal := block.NewLoad(targetLLVM, typedPtr)
						typedAlloca := block.NewAlloca(targetLLVM)
						block.NewStore(typedVal, typedAlloca)
						cg.curScope.set(e.VarName, &scopeEntry{val: typedAlloca, isAlloc: true})
					}
				}
				return cmp, nil
			}
		}
	}
	// Fallback: just return true.
	return constant.NewInt(irtypes.I1, 1), nil
}

/// isFatArrayPtr returns true for anonymous {T*, i64} fat array pointer structs.
// Named structs (user-defined) are excluded to avoid false matches with

// fnSigName formats an LLVM FuncType as a Tin-style signature string such as
// "fn(i64,string)bool".  When skipFirstEnv is true the first parameter (the
// implicit i8* env of a fat-function-pointer) is omitted.
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
	var result []string
	var walk func(ast.Node)
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
			// Don't descend into nested lambdas; they capture independently.
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
		}
	}
	walk(body)
	return result
}

// callTraitMethod dispatches x.method(args) where x is a trait fat pointer
// {i8* data, vtable*}.  It looks up the method slot index in the vtable,
// loads the function pointer, and calls it with (data, args...).
// instKey may be "named" or "iter_i64" etc.
func (cg *CodeGen) callTraitMethod(block *ir.Block, ifaceVal value.Value, instKey, methodName string, argNodes []ast.Node) (value.Value, error) {
	// Method order is stored by base trait name.
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	methodOrder := cg.traitMethodOrder[baseTrait]
	slotIdx := -1
	for i, n := range methodOrder {
		if n == methodName {
			slotIdx = i
			break
		}
	}
	if slotIdx < 0 {
		return nil, fmt.Errorf("trait %s has no method %s", instKey, methodName)
	}

	// Extract data pointer and vtable pointer from iface fat ptr.
	dataPtr := block.NewExtractValue(ifaceVal, 0)
	vtablePtr := block.NewExtractValue(ifaceVal, 1)

	// Load function pointer from vtable[slotIdx].
	vtableSt := cg.traitVtableStructTypes[instKey]
	fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slotIdx)))
	fnSlotType := vtableSt.Fields[slotIdx].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	fnPtr := block.NewLoad(irtypes.NewPointer(fnSlotType), fnPtrGep)

	// Build call args: (data_ptr, extra_args...).
	llArgs := []value.Value{dataPtr}
	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		llArgs = append(llArgs, av)
	}
	llArgs = cg.adaptArgs(block, llArgs, fnSlotType)
	result := block.NewCall(fnPtr, llArgs...)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

// wrapFnAsFatPtr wraps a named or extern function pointer into a fat-fn-ptr
// { fn(i8* env, params...)*, i8* } with a null environment.
// The shim ignores its env parameter and simply forwards to the wrapped function.
// Shims are cached per function name to avoid duplicate definitions.
func (cg *CodeGen) wrapFnAsFatPtr(block *ir.Block, fnVal value.Value, targetFatType irtypes.Type) value.Value {
	fatSt := targetFatType.(*irtypes.StructType)
	// The fat-fn-ptr stores fn(i8*, params...)* in field 0.
	wrapperFnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Get the original function's type (without the env param).
	srcFnType, ok := fnVal.Type().(*irtypes.PointerType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}
	origFnType, ok := srcFnType.ElemType.(*irtypes.FuncType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}

	// Build a cache key from the function's name.
	shimName := ""
	if named, ok := fnVal.(interface{ Name() string }); ok {
		shimName = "__shim_" + named.Name()
	} else {
		shimName = fmt.Sprintf("__shim_%d", cg.strCount)
		cg.strCount++
	}

	// Reuse cached shim if already generated.
	var shim *ir.Func
	for _, fn := range cg.mod.Funcs {
		if fn.Name() == shimName {
			shim = fn
			break
		}
	}

	if shim == nil {
		// The shim's signature must match wrapperFnType (the fat-fn-ptr's expected
		// function type): (i8* env, tin_param_0, tin_param_1, ...).
		// wrapperFnType.Params[0] is i8* (env); Params[1..] are the tin-level types.
		shimParams := make([]*ir.Param, len(wrapperFnType.Params))
		for i, pt := range wrapperFnType.Params {
			name := "env"
			if i > 0 {
				name = fmt.Sprintf("p%d", i-1)
			}
			shimParams[i] = ir.NewParam(name, pt)
		}
		shim = cg.mod.NewFunc(shimName, wrapperFnType.RetType, shimParams...)
		entry := shim.NewBlock("entry")
		// Forward call: skip env (index 0), adapt remaining args to orig signature.
		callArgs := make([]value.Value, len(origFnType.Params))
		for i := range origFnType.Params {
			callArgs[i] = shim.Params[i+1]
		}
		callArgs = cg.adaptArgs(entry, callArgs, origFnType)
		result := entry.NewCall(fnVal, callArgs...)
		if irtypes.IsVoid(wrapperFnType.RetType) {
			entry.NewRet(nil)
		} else {
			// Wrap return value if needed (e.g., raw i8* -> string fat-ptr).
			ret := cg.wrapFromExtern(entry, result, wrapperFnType.RetType)
			entry.NewRet(ret)
		}
	}

	// Return fat-fn-ptr { shim*, null }.
	alloca := block.NewAlloca(fatSt)
	gep0 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(shim, gep0)
	gep1 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewNull(irtypes.I8Ptr), gep1)
	return block.NewLoad(fatSt, alloca)
}

// callFatFn emits a call through a closure fat pointer { fn(i8*,params...)*, i8* }.
func (cg *CodeGen) callFatFn(block *ir.Block, fatPtr value.Value, argNodes []ast.Node) (value.Value, error) {
	fnPtr := block.NewExtractValue(fatPtr, 0)
	envPtr := block.NewExtractValue(fatPtr, 1)

	llArgs := []value.Value{envPtr}
	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		llArgs = append(llArgs, av)
	}

	// Adapt args to the underlying function's signature.
	fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	llArgs = cg.adaptArgs(block, llArgs, fnType)

	result := block.NewCall(fnPtr, llArgs...)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) genLambdaExpr(block *ir.Block, e *ast.LambdaExpr) (value.Value, error) {
	name := fmt.Sprintf("lambda.%d", cg.strCount)
	cg.strCount++

	// Step 1: identify free variables
	localNames := map[string]bool{}
	for _, p := range e.Params {
		localNames[p.Name] = true
	}
	freeNames := collectFreeVars(e.Body, localNames)

	// Resolve each free name in the current (outer) scope. Skip names that
	// resolve to module-level IR functions (not allocas) — those are callable
	// directly by name and don't need capturing.
	type capture struct {
		name    string
		val     value.Value // loaded value (not the alloca)
		llvmTy  irtypes.Type
	}
	var captures []capture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function – reachable by name, no capture needed
		}
		var val value.Value
		var ty irtypes.Type
		if entry.isAlloc {
			pt := entry.val.Type().(*irtypes.PointerType)
			ty = pt.ElemType
			val = block.NewLoad(ty, entry.val)
		} else {
			val = entry.val
			ty = val.Type()
		}
		captures = append(captures, capture{n, val, ty})
	}

	// Step 2: build env struct and malloc it (if there are captures)
	var envI8Ptr value.Value = constant.NewNull(irtypes.I8Ptr)
	var envStructType *irtypes.StructType

	if len(captures) > 0 {
		fields := make([]irtypes.Type, len(captures))
		for i, c := range captures {
			fields[i] = c.llvmTy
		}
		envStructType = irtypes.NewStruct(fields...)

		// sizeof(*envStructType): GEP trick — null + 1 element then ptrtoint.
		nullEnvPtr := constant.NewNull(irtypes.NewPointer(envStructType))
		oneGEP := block.NewGetElementPtr(envStructType, nullEnvPtr, constant.NewInt(irtypes.I32, 1))
		envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
		envI8Ptr = block.NewCall(cg.ensureMalloc(), envSize)

		// Store each captured value into the env struct.
		envTypedPtr := block.NewBitCast(envI8Ptr, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := block.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			block.NewStore(c.val, gep)
		}
	}

	// Step 3: create the lambda IR function with (i8* env, params...) sig
	llParams := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range e.Params {
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}
		llParams = append(llParams, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void
	if e.RetType != nil {
		var err error
		retType, err = cg.tinTypeToLLVM(e.RetType)
		if err != nil {
			return nil, err
		}
	}

	f := cg.mod.NewFunc(name, retType, llParams...)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	// Start a fresh scope (not inheriting outer scope — captured values are
	// explicitly loaded from the env struct below).
	cg.curScope = newScope(nil)

	// Register global scope so functions/enums remain accessible.
	// Walk up to the top-level scope and set it as the parent.
	global := prevScope
	for global.parent != nil {
		global = global.parent
	}
	cg.curScope = newScope(global)

	// Step 4: unpack captures from env inside the lambda body
	if len(captures) > 0 {
		envRaw := f.Params[0]
		envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			alloca := entry.NewAlloca(c.llvmTy)
			loaded := entry.NewLoad(c.llvmTy, gep)
			entry.NewStore(loaded, alloca)
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true})
		}
	}

	// Register lambda params (skip index 0 = env).
	for i, p := range e.Params {
		param := f.Params[i+1]
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}
		alloca := entry.NewAlloca(pt)
		entry.NewStore(param, alloca)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true})
	}

	// For where-list bodies, the match subject is the first parameter so that
	// atom and comparison conditions compare against it (mirroring genFuncDeclAs).
	prevMatchSubject := cg.matchSubject
	if _, isWhere := e.Body.(*ast.WhereList); isWhere && len(e.Params) > 0 {
		firstParamName := e.Params[0].Name
		if se, ok := cg.curScope.lookup(firstParamName); ok && se.isAlloc {
			pt := se.val.Type().(*irtypes.PointerType)
			cg.matchSubject = entry.NewLoad(pt.ElemType, se.val)
		}
	}

	term, err := cg.genBody(entry, e.Body, retType)
	cg.matchSubject = prevMatchSubject
	if err != nil {
		return nil, err
	}
	if !term {
		lastBlock := f.Blocks[len(f.Blocks)-1]
		if lastBlock.Term == nil {
			if irtypes.IsVoid(retType) {
				lastBlock.NewRet(nil)
			} else {
				lastBlock.NewRet(cg.zeroValue(retType))
			}
		}
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	// Step 5: build and return fat pointer { fn_ptr, env_i8_ptr }
	fatStructType := irtypes.NewStruct(irtypes.NewPointer(f.Sig), irtypes.I8Ptr)
	alloca := block.NewAlloca(fatStructType)
	gep0 := block.NewGetElementPtr(fatStructType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(f, gep0)
	gep1 := block.NewGetElementPtr(fatStructType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(envI8Ptr, gep1)
	return block.NewLoad(fatStructType, alloca), nil
}


// Interpolated string

func (cg *CodeGen) genInterpolatedString(block *ir.Block, e *ast.InterpolatedString) (value.Value, error) {
	// Build a format string and argument list for printf/sprintf.
	var fmtParts []string
	var args []value.Value

	for _, part := range e.Parts {
		if !part.IsExpr {
			// Escape % in literal parts.
			escaped := strings.ReplaceAll(part.Str, "%", "%%")
			fmtParts = append(fmtParts, escaped)
		} else {
			val, err := cg.genExpr(block, part.Expr)
			if err != nil {
				return nil, err
			}
			if val == nil {
				fmtParts = append(fmtParts, "(nil)")
				continue
			}
			t := val.Type()

			// If a format specifier was provided, use it directly.
			if part.Format != "" {
				fmtSpec := part.Format
				lastChar := fmtSpec[len(fmtSpec)-1]
				prefix := fmtSpec[:len(fmtSpec)-1]
				switch lastChar {
				case 'x', 'X', 'o', 'u':
					// Unsigned/hex/octal integer format
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))
							if it.BitSize < 32 {
								val = block.NewZExt(val, irtypes.I32)
							}
						}
						args = append(args, val)
						continue
					}
				case 'd', 'i':
					// Signed integer format
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))
							if it.BitSize < 32 {
								val = block.NewSExt(val, irtypes.I32)
							}
						}
						args = append(args, val)
						continue
					}
				case 'f', 'e', 'g', 'E', 'G':
					// Floating-point format
					fmtParts = append(fmtParts, "%"+fmtSpec)
					if irtypes.IsFloat(t) {
						if t != irtypes.Double {
							val = block.NewFPExt(val, irtypes.Double)
						}
					} else if irtypes.IsInt(t) {
						val = block.NewSIToFP(val, irtypes.Double)
					}
					args = append(args, val)
					continue
				case 's':
					// String format
					if isStringType(t) {
						fmtParts = append(fmtParts, "%"+fmtSpec)
						args = append(args, cg.extractStringPtr(block, val))
						continue
					}
				}
				// Unknown format specifier — fall through to default handling
			}

			switch {
			case isStringType(t):
				fmtParts = append(fmtParts, "%s")
				ptr := cg.extractStringPtr(block, val)
				args = append(args, ptr)
			case irtypes.IsInt(t):
				it := t.(*irtypes.IntType)
				if it.BitSize == 1 {
					fmtParts = append(fmtParts, "%d")
					val = block.NewZExt(val, irtypes.I32)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
				}
				args = append(args, val)
			case irtypes.IsFloat(t):
				fmtParts = append(fmtParts, "%g")
				if t != irtypes.Double {
					val = block.NewFPExt(val, irtypes.Double)
				}
				args = append(args, val)
			default:
				// print trait: struct or fat-pointer with a print() method.
				if strVal, ok := cg.callPrintTrait(block, val); ok {
					fmtParts = append(fmtParts, "%s")
					ptr := cg.extractStringPtr(block, strVal)
					args = append(args, ptr)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
					args = append(args, val)
				}
			}
		}
	}

	// Build result string using snprintf with a two-pass approach:
	//   1. snprintf(NULL, 0, fmt, ...) → returns the required length (excluding NUL).
	//   2. malloc(len+1) → allocate exact buffer.
	//   3. snprintf(buf, len+1, fmt, ...) → fill buffer.
	// This avoids a fixed-size buffer and handles arbitrarily long interpolations.
	fmtStr := strings.Join(fmtParts, "")
	fmtPtr := cg.newGlobalString(fmtStr)
	snprintfFn := cg.ensureSnprintf()
	malloc := cg.ensureMalloc()

	// Pass 1: measure required length.
	nullBuf := constant.NewNull(irtypes.I8Ptr)
	sizeZero := constant.NewInt(irtypes.I64, 0)
	measureArgs := []value.Value{nullBuf, sizeZero, fmtPtr}
	measureArgs = append(measureArgs, args...)
	needed := block.NewCall(snprintfFn, measureArgs...)     // i32
	neededI64 := block.NewSExt(needed, irtypes.I64)
	allocSize := block.NewAdd(neededI64, constant.NewInt(irtypes.I64, 1)) // +1 for NUL

	// Pass 2: allocate and fill.
	buf := block.NewCall(malloc, allocSize)
	fillArgs := []value.Value{buf, allocSize, fmtPtr}
	fillArgs = append(fillArgs, args...)
	block.NewCall(snprintfFn, fillArgs...)

	fatPtrType := stringFatPtrType()
	fatAlloca := block.NewAlloca(fatPtrType)
	ptrGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(buf, ptrGep)
	lenGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(neededI64, lenGep)
	return block.NewLoad(fatPtrType, fatAlloca), nil
}

// LValue generation

// genLValue returns a pointer to the storage location of an lvalue.
func (cg *CodeGen) genLValue(block *ir.Block, node ast.Node) (value.Value, error) {
	switch e := node.(type) {
	case *ast.Identifier:
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, fmt.Errorf("undefined identifier: %s", e.Name)
		}
		if entry.isAlloc {
			return entry.val, nil
		}
		// Not an alloca - wrap in alloca.
		alloca := block.NewAlloca(entry.val.Type())
		block.NewStore(entry.val, alloca)
		return alloca, nil

	case *ast.IndexExpr:
		arr, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}
		idx, err := cg.genExpr(block, e.Index)
		if err != nil {
			return nil, err
		}
		idx = cg.coerce(block, idx, irtypes.I64)

		arrType := arr.Type()
		switch at := arrType.(type) {
		case *irtypes.StructType:
			if len(at.Fields) == 2 {
				alloca := block.NewAlloca(arrType)
				block.NewStore(arr, alloca)
				ptrGep := block.NewGetElementPtr(arrType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				elemPtrType := at.Fields[0]
				dataPtr := block.NewLoad(elemPtrType, ptrGep)
				if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
					return block.NewGetElementPtr(pt.ElemType, dataPtr, idx), nil
				}
			}
		case *irtypes.ArrayType:
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)
			return block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), idx), nil
		case *irtypes.PointerType:
			return block.NewGetElementPtr(at.ElemType, arr, idx), nil
		}
		return nil, fmt.Errorf("cannot index type %s", arrType)

	case *ast.FieldAccess:
		// Use genLValue recursively so we obtain a pointer into the *original*
		// storage (alloca, heap, etc.) rather than a copy.  Writing through the
		// returned GEP pointer then actually mutates the variable.
		objPtr, err := cg.genLValue(block, e.Expr)
		if err != nil {
			// genLValue failed for the sub-expression (e.g. a non-lvalue like a
			// function call return value).  Fall back to a temporary alloca; this
			// means field-writes on temporaries are discarded, but that is the
			// pre-existing behaviour for such expressions.
			obj, err2 := cg.genExpr(block, e.Expr)
			if err2 != nil {
				return nil, err2
			}
			objType := obj.Type()
			if e.IsPtr {
				if pt, ok := objType.(*irtypes.PointerType); ok {
					structName := cg.typeNameOf(pt.ElemType)
					fieldIdx := cg.fieldIndex(structName, e.Field)
					if fieldIdx < 0 {
						return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
					}
					return block.NewGetElementPtr(pt.ElemType, obj,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
				}
			}
			alloca := block.NewAlloca(objType)
			block.NewStore(obj, alloca)
			structName := cg.typeNameOf(objType)
			fieldIdx := cg.fieldIndex(structName, e.Field)
			if fieldIdx < 0 {
				return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
			}
			return block.NewGetElementPtr(objType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
		}
		// objPtr is a pointer to the containing struct (or pointer-to-struct for IsPtr).
		objPtrType, ok := objPtr.Type().(*irtypes.PointerType)
		if !ok {
			return nil, fmt.Errorf("genLValue: expected pointer for field access")
		}
		objType := objPtrType.ElemType
		if e.IsPtr {
			// e.Expr is a variable holding a *struct — dereference once.
			structPtrVal := block.NewLoad(objType, objPtr)
			if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
				structName := cg.typeNameOf(pt.ElemType)
				fieldIdx := cg.fieldIndex(structName, e.Field)
				if fieldIdx < 0 {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}
				return block.NewGetElementPtr(pt.ElemType, structPtrVal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
			}
		}
		// Auto-deref: when the alloca holds a *struct (pointer receiver pattern),
		// dereference once so that `this.field` works the same as `this->field`.
		if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
			if cg.typeNameOf(pt.ElemType) != "" {
				structPtrVal := block.NewLoad(objType, objPtr)
				structName := cg.typeNameOf(pt.ElemType)
				fieldIdx := cg.fieldIndex(structName, e.Field)
				if fieldIdx < 0 {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}
				return block.NewGetElementPtr(pt.ElemType, structPtrVal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
			}
		}
		structName := cg.typeNameOf(objType)
		fieldIdx := cg.fieldIndex(structName, e.Field)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
		}
		return block.NewGetElementPtr(objType, objPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil

	case *ast.DerefExpr:
		val, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}
		if irtypes.IsPointer(val.Type()) {
			return val, nil
		}
		return nil, fmt.Errorf("cannot deref non-pointer")
	}
	return nil, fmt.Errorf("not an lvalue: %T", node)
}

