package codegen

import (
	"fmt"
	"os"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/parser"
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

	case *ast.NilLit:

		return constant.NewNull(irtypes.I8Ptr), nil

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

	case *ast.TupleLit:

		return cg.genTupleLit(block, e, nil)

	case *ast.SliceExpr:

		return cg.genSliceExpr(block, e)

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

	case *ast.SpawnExpr:

		return cg.genSpawnExpr(block, e)

	case *ast.AwaitExpr:
		// await expr - evaluates e.Future (which must be a Future[t] / Awaitable[t]).
		//
		// Any rvalue that evaluates to an Awaitable[T] can be awaited:
		//   await spawn fn(args)      - spawn returns Future[T]
		//   await fetch()             - fetch() returns Future[T]
		//   await f                   - f : Future[T] variable
		//
		// Type rule: await is valid iff expr : Awaitable[T] (i.e. Future[T]).
		// Calling a {#async} fn directly returns T (not Future[T]) - not awaitable.
		val, err := cg.genExpr(block, e.Future)
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, fmt.Errorf("await: expression produced no value")
		}
		// Refresh block in case evaluating the future expression advanced the IR
		// insertion point (e.g. `await spawn fn(await spawn other())` where the
		// inner await moved to a new block via cg.curBlock signaling).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		// Verify the value is a Future[T] struct and extract its PID + result type.
		structName := structNameFromValue(val)
		if structName == "" {
			if val.Type().Equal(irtypes.I64) {
				if cg.syncLoadErr != nil {
					return nil, fmt.Errorf("await: stdlib/sync failed to load so spawn returned a raw pid.\n"+
						"  Ensure the tin executable is alongside the stdlib/ directory.\n"+
						"  Load error: %w", cg.syncLoadErr)
				}

				return nil, fmt.Errorf("await: expression is a raw i64, not a Future[t]; use `await spawn fn(args)` which returns Future[t]")
			}

			return nil, fmt.Errorf("await: expression (type %s) does not implement Awaitable[t]; use `await spawn fn(args)` to run fn as a fiber, or have the function return Future[t] (e.g. fn f() Future[t] = spawn ...)",
				val.Type())
		}

		// The value must be a Future[T] struct.  Extract .pid field (field index 0).
		pidIdx := cg.fieldIndex(structName, "pid")
		if pidIdx < 0 {
			// Not a Future struct - check if it implements Awaitable via await_result.
			methodName := structName + "_await_result"
			if se, ok := cg.curScope.lookup(methodName); ok {
				if fn, ok2 := se.val.(*ir.Func); ok2 {
					args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
					result := block.NewCall(fn, args...)
					cg.curBlock = block

					return result, nil
				}
			}

			return nil, fmt.Errorf("await: expression (type %q) does not implement Awaitable[t]; use `await spawn fn(args)` to run fn as a fiber, or have the function return Future[t] directly", structName)
		}

		// Extract pid from Future[T].
		cg.ensureFiberRuntime()
		alloca := block.NewAlloca(val.Type())
		block.NewStore(val, alloca)
		pidGEP := block.NewGetElementPtr(val.Type(), alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(pidIdx)))
		pid := block.NewLoad(irtypes.I64, pidGEP)

		// Properly suspend the calling fiber (or block main) until pid completes.
		resumeBlk, awaitErr := cg.genAwaitStmt(block, pid)
		if awaitErr != nil {
			return nil, awaitErr
		}
		if resumeBlk != nil {
			block = resumeBlk
			cg.curBlock = block
		}

		// Check whether the awaited fiber panicked.
		// We emit the _tin_panic call inline (not inside a C helper) so that
		// the panic unwinds in the calling Tin function's context - making it
		// catchable via defer + recover() in that function.
		//
		// Emitted IR pattern:
		//   %pmsg = call i8* @_tin_fiber_get_panic_msg(pid)
		//   %panicked = icmp ne i8* %pmsg, null
		//   br i1 %panicked, label %await.panic, label %await.ok
		// await.panic:
		//   call void @_tin_panic(i8* %pmsg)
		//   ret <zero>     ; if recovered by defer, return zero value
		// await.ok:
		//   ... get and unbox result ...
		pmsg := block.NewCall(cg.fiberGetPanicMsgFn, pid)
		panicked := block.NewICmp(enum.IPredNE, pmsg, constant.NewNull(irtypes.I8Ptr))
		panicBlk := cg.newBlock("await.panic")
		okBlk := cg.newBlock("await.ok")
		block.NewCondBr(panicked, panicBlk, okBlk)

		// Panic block: call _tin_panic then emit a valid terminator.
		// Inside a coroutine body we must use the coro completion path so that
		// _tin_fiber_complete is called and llvm.coro.end sees a valid IR shape.
		// (A bare ret in a presplit coro body bypasses coro.end and leaves the
		// frame in an undefined state.)  This mirrors the fix in genBuiltinPanic.
		panicBlk.NewCall(cg.ensurePanicFn(), pmsg)
		if cg.inCoroFn {
			cg.ensureFiberRuntime()
			// If _tin_panic returns (panic was caught by defer+recover in this
			// coro), complete with the defer-override value if a thunk set one,
			// otherwise the zero value of the declared return type.  Passing nil
			// would leave the fiber result as NULL, causing a null-pointer
			// dereference in the outer awaiter's okBlk.
			cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
			cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)
		} else {
			retType := cg.curFn.Sig.RetType
			if irtypes.IsVoid(retType) {
				panicBlk.NewRet(nil)
			} else {
				panicBlk.NewRet(cg.zeroValue(retType))
			}
		}

		block = okBlk
		cg.curBlock = block

		// Determine the Future's type parameter T so we can unbox the result.
		// Future__i64 -> retType=i64; Future__Unit -> retType=Unit(void).
		retTypeName := ""
		if len(structName) > 8 && structName[:8] == "Future__" {
			retTypeName = structName[8:]
		}
		if retTypeName == "" || retTypeName == "Unit" {
			// void result - return a sentinel i1 true so callers don't see nil.
			return constant.NewInt(irtypes.I1, 1), nil
		}
		retLLVM, resolveErr := cg.resolveSimpleType(retTypeName)
		if resolveErr != nil || retLLVM == nil || irtypes.IsVoid(retLLVM) {
			return constant.NewInt(irtypes.I1, 1), nil
		}

		// Get the boxed result pointer and unbox it.
		rawPtr := block.NewCall(cg.fiberGetResultFn, pid)
		typedPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(retLLVM))
		result := block.NewLoad(retLLVM, typedPtr)
		cg.curBlock = block

		return result, nil

	case *ast.YieldStmt:
		// yield used in expression context (e.g., let _ = yield): treat as statement.
		newBlk, err := cg.genYieldStmt(block)
		if err != nil {
			return nil, err
		}
		cg.curBlock = newBlk

		return constant.NewInt(irtypes.I1, 0), nil

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
		// Block expression: (let x = ...; ...; last_expr) - produced by CTFE macro splices.
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

	case *ast.IsRCExpr:
		// Compile-time constant: 1 if T is ARC-tracked (string / array / any), 0 otherwise.
		if e.Type == nil {
			return constant.NewInt(irtypes.I32, 0), nil
		}
		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}
		if isRCTrackedType(lt) {
			return constant.NewInt(irtypes.I32, 1), nil
		}

		return constant.NewInt(irtypes.I32, 0), nil

	case *ast.TypeAssertExpr:
		inner, err := cg.genExpr(block, e.Expr)
		if err != nil || inner == nil || e.Type == nil {
			return inner, err
		}
		// Native union type cast: b.(string) - bitcast storage to target type.
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

	cg.curBlock = block
	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}
	cg.curBlock = block
	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
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

	// Pointer arithmetic: ptr + int -> getelementptr; ptr - int -> getelementptr with negation.
	if ptrType, isPtr := lt.(*irtypes.PointerType); isPtr && irtypes.IsInt(rt) {
		// Ensure the index is i64.
		if rt.(*irtypes.IntType).BitSize < 64 {
			right = block.NewSExt(right, irtypes.I64)
		}
		switch e.Op {
		case "+":

			return block.NewGetElementPtr(ptrType.ElemType, left, right), nil
		case "-":
			negIdx := block.NewSub(constant.NewInt(irtypes.I64, 0), right)

			return block.NewGetElementPtr(ptrType.ElemType, left, negIdx), nil
		}
	}

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

		return cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, false), nil
	case "!=":

		return cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, true), nil
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
			result := block.NewLoad(fatType, fatAlloca)
			// Release sub-expression temporaries now that the result is built.
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}
			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}

			return result, nil
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
		result := block.NewLoad(fatPtrType, alloca)
		// Release sub-expression temporaries now that the result is built.
		if isTemporaryProducer(e.Left) {
			cg.emitRelease(block, left)
		}
		if isTemporaryProducer(e.Right) {
			cg.emitRelease(block, right)
		}

		return result, nil
	}

	return constant.NewInt(irtypes.I64, 0), nil
}

// genEqNeqExpr implements shared handling for == and != operators.
func (cg *CodeGen) genEqNeqExpr(block *ir.Block, left, right value.Value, lt, rt irtypes.Type, isFloat bool, notEqual bool) value.Value {
	if isFloat {
		if notEqual {
			return block.NewFCmp(enum.FPredONE, left, right)
		}

		return block.NewFCmp(enum.FPredOEQ, left, right)
	}

	pred := enum.IPredEQ
	if notEqual {
		pred = enum.IPredNE
	}

	// any equality/inequality: dynamically dispatched by runtime.
	if isAnyType(lt) || isAnyType(rt) {
		if !isAnyType(lt) {
			left = cg.boxToAny(block, left)
		}
		if !isAnyType(rt) {
			right = cg.boxToAny(block, right)
		}
		cmp := block.NewCall(cg.ensureAnyEq(), left, right)
		if notEqual {
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I64, 0))
		}

		return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I64, 0))
	}

	// atom ==/!= atom: compare CRC32 codes directly.
	if isAtomType(lt) && isAtomType(rt) {
		lcode := cg.extractAtomCode(block, left)
		rcode := cg.extractAtomCode(block, right)

		return block.NewICmp(pred, lcode, rcode)
	}

	// atom <-> string: convert atom to string, then strcmp.
	if isAtomType(lt) && isFatPtrType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, left))
		lptr := cg.extractStringPtr(block, strVal)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}
	if isFatPtrType(lt) && isAtomType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, right))
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, strVal)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// String equality/inequality: compare via strcmp.
	if isFatPtrType(lt) {
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// Pointer vs integer-zero (None): coerce i64(0) to typed null pointer.
	if irtypes.IsPointer(lt) && !irtypes.IsPointer(rt) {
		right = constant.NewNull(lt.(*irtypes.PointerType))
	} else if irtypes.IsPointer(rt) && !irtypes.IsPointer(lt) {
		left = constant.NewNull(rt.(*irtypes.PointerType))
	}

	return block.NewICmp(pred, left, right)
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
		// #no_excl: allow calling macro! as plain function name (without !).
		// Only applies when the macro has the "no_excl" tag.
		if !strings.HasSuffix(fn.Name, "!") {
			if macro, ok := cg.macros[fn.Name+"!"]; ok && macroHasTag(macro, "no_excl") {
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
		// Built-in: recover() - retrieve panic message from deferred function.
		if fn.Name == "recover" && len(e.Args) == 0 {
			return cg.genBuiltinRecover(block)
		}
		// Built-in: default(TypeName) - returns the zero value for a type.
		// Used in generic code to produce a typed zero without knowing the concrete type.
		if fn.Name == "default" && len(e.Args) == 1 {
			return cg.genBuiltinDefault(block, e.Args[0])
		}
		// Check if this is a constrained generic function call - monomorphize it.
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
		// Overload resolution: if this name has multiple variants, evaluate args
		// first to pick the best match by type, then call it directly.
		if variants, hasOverloads := cg.overloads[fn.Name]; hasOverloads {
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}
				argVals = append(argVals, av)
				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
			best := cg.resolveOverload(variants, argVals)
			if best == nil {
				return nil, fmt.Errorf("no matching overload for %s (got %d arg(s))", fn.Name, len(argVals))
			}
			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, fmt.Errorf("overload %s not found in scope", best.irName)
			}
			var ovCallee value.Value
			if oEntry.isAlloc {
				ptrType := oEntry.val.Type().(*irtypes.PointerType)
				ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
			} else {
				ovCallee = oEntry.val
			}
			argValsPreCoerce := append([]value.Value(nil), argVals...)
			if f, ok2 := ovCallee.(*ir.Func); ok2 {
				argVals = cg.adaptArgs(block, argVals, f.Sig)
			}
			result := block.NewCall(ovCallee, argVals...)
			for i, astArg := range e.Args {
				if i >= len(argValsPreCoerce) {
					break
				}
				preCoerce := argValsPreCoerce[i]
				postCoerce := argVals[i]
				if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
					cg.emitRelease(block, postCoerce)

					continue
				}
				if !isRCTrackedType(preCoerce.Type()) {
					continue
				}
				if isCopyExpr(astArg) {
					continue
				}
				cg.emitRelease(block, preCoerce)
			}
			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}
		entry, ok := cg.curScope.lookup(fn.Name)
		if !ok {
			return nil, fmt.Errorf("undefined function: %s", fn.Name)
		}
		// Warn when a {#blocking} extern is called inside an {#async} function.
		if cg.curCoroHdl != nil {
			if origDecl, found := cg.funcDecls[fn.Name]; found {
				if origDecl.IsExtern != "" && hasTag(origDecl.Tags, "blocking") {
					_, _ = fmt.Fprintf(os.Stderr,
						"warning: calling blocking extern %q inside an {#async} function; "+
							"use async_read/async_write instead\n", fn.Name)
				}
			}
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
		// Static dispatch: TypeName.method() where TypeName is a struct type, not a variable.
		// Must be checked BEFORE trying to evaluate fn.Expr as a value, because type names
		// are not in scope as values and would cause "undefined identifier" errors.
		if staticName, typeArgStr := cg.tryResolveStructTypeName(fn.Expr); staticName != "" {
			methodKey := staticName + "_" + fn.Field
			// Also try the concrete monomorphized key when a type arg is present.
			if typeArgStr != "" {
				concreteName := staticName + "__" + typeArgStr
				if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
					if _, isGeneric := cg.genericStructsByArity[staticName]; isGeneric {
						synthDecl := &ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: staticName, TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: typeArgStr}}},
						}
						_ = cg.genTypeDecl(synthDecl)
					}
				}
				if _, exists := cg.structTypes[concreteName]; exists {
					methodKey = concreteName + "_" + fn.Field
					staticName = concreteName
				}
			}
			if entry, ok := cg.curScope.lookup(methodKey); ok {
				if f, isFn := entry.val.(*ir.Func); isFn && cg.isStaticMethodIR(f, staticName) {
					llArgs := make([]value.Value, 0, len(e.Args))
					for _, arg := range e.Args {
						av, err2 := cg.genExpr(block, arg)
						if err2 != nil {
							return nil, err2
						}
						llArgs = append(llArgs, av)
						if cg.curBlock != nil && cg.curBlock != block {
							block = cg.curBlock
						}
					}
					llArgs = cg.adaptArgs(block, llArgs, f.Sig)

					return block.NewCall(f, llArgs...), nil
				}
			}
		}

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
		// When obj is a pointer-to-struct (*T), use the pointee's name for method
		// lookup but keep objVal as the pointer (the thisArg logic below handles it).
		objLookupType := objVal.Type()
		if pt, ok := objLookupType.(*irtypes.PointerType); ok {
			if cg.typeNameOf(pt.ElemType) != "" {
				objLookupType = pt.ElemType
			}
		}
		structName := cg.typeNameOf(objLookupType)
		methodName := structName + "_" + fn.Field

		// Overloaded method: evaluate args first to pick the best variant.
		if variants, hasOverloads := cg.overloads[methodName]; hasOverloads {
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}
				argVals = append(argVals, av)
				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
			best := cg.resolveOverload(variants, argVals)
			if best == nil {
				return nil, fmt.Errorf("no matching overload for %s.%s (got %d arg(s))", structName, fn.Field, len(argVals))
			}
			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, fmt.Errorf("overload %s not found in scope", best.irName)
			}
			var ovCallee value.Value
			if oEntry.isAlloc {
				ptrType := oEntry.val.Type().(*irtypes.PointerType)
				ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
			} else {
				ovCallee = oEntry.val
			}
			// Static method called on an instance: don't pass the instance as receiver.
			ovIsStatic := false
			if f, ok2 := ovCallee.(*ir.Func); ok2 {
				ovIsStatic = cg.isStaticMethodIR(f, structName)
			}
			var llArgs []value.Value
			if ovIsStatic {
				llArgs = make([]value.Value, 0, len(argVals))
				llArgs = append(llArgs, argVals...)
			} else {
				// Build thisArg (pointer receiver if needed).
				thisArg := objVal
				if f, ok2 := ovCallee.(*ir.Func); ok2 && len(f.Sig.Params) > 0 {
					firstParam := f.Sig.Params[0]
					if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr {
						if pt.ElemType.Equal(objVal.Type()) {
							if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
								thisArg = lv
							} else {
								tmp := block.NewAlloca(objVal.Type())
								block.NewStore(objVal, tmp)
								thisArg = tmp
							}
						}
					}
				}
				llArgs = make([]value.Value, 0, len(argVals)+1)
				llArgs = append(llArgs, thisArg)
				llArgs = append(llArgs, argVals...)
			}
			if f, ok2 := ovCallee.(*ir.Func); ok2 {
				llArgs = cg.adaptArgs(block, llArgs, f.Sig)
			}
			result := block.NewCall(ovCallee, llArgs...)
			// ARC: release temporary RC-tracked args (same logic as genCallExpr bottom).
			thisOff := 1
			if ovIsStatic {
				thisOff = 0
			}
			for i, astArg := range e.Args {
				if i >= len(argVals) || i+thisOff >= len(llArgs) {
					break
				}
				preCoerce := argVals[i]
				postCoerce := llArgs[i+thisOff]
				if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
					cg.emitRelease(block, postCoerce)

					continue
				}
				if !isRCTrackedType(preCoerce.Type()) {
					continue
				}
				if isCopyExpr(astArg) {
					continue
				}
				cg.emitRelease(block, preCoerce)
			}
			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}

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
			// Static method called on an instance: skip the instance receiver.
			instIsStatic := false
			if f, ok2 := callee.(*ir.Func); ok2 {
				instIsStatic = cg.isStaticMethodIR(f, structName)
			}
			var llArgs []value.Value
			llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
			if instIsStatic {
				llArgs = make([]value.Value, 0, len(e.Args))
				for _, arg := range e.Args {
					av, err := cg.genExpr(block, arg)
					if err != nil {
						return nil, err
					}
					llArgs = append(llArgs, av)
					llArgsPreCoerce = append(llArgsPreCoerce, av)
					if cg.curBlock != nil && cg.curBlock != block {
						block = cg.curBlock
					}
				}
			} else {
				// Determine the first argument: if the method expects a pointer
				// receiver (*Struct), pass the address of the object rather than
				// its value so that mutations through `this` are visible to the caller.
				thisArg := objVal
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
				llArgs = make([]value.Value, 0, len(e.Args)+1)
				llArgs = append(llArgs, thisArg)
				for _, arg := range e.Args {
					av, err := cg.genExpr(block, arg)
					if err != nil {
						return nil, err
					}
					llArgs = append(llArgs, av)
					llArgsPreCoerce = append(llArgsPreCoerce, av)
					if cg.curBlock != nil && cg.curBlock != block {
						block = cg.curBlock
					}
				}
			}
			// Adapt arg types to function signature.
			if f, ok2 := callee.(*ir.Func); ok2 {
				calleeType = f.Sig
				llArgs = cg.adaptArgs(block, llArgs, calleeType)
			}
			result := block.NewCall(callee, llArgs...)
			// ARC: release temporary RC-tracked args (same logic as genCallExpr bottom).
			thisOff := 1
			if instIsStatic {
				thisOff = 0
			}
			for i, astArg := range e.Args {
				if i >= len(llArgsPreCoerce) || i+thisOff >= len(llArgs) {
					break
				}
				preCoerce := llArgsPreCoerce[i]
				postCoerce := llArgs[i+thisOff]
				if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
					cg.emitRelease(block, postCoerce)

					continue
				}
				if !isRCTrackedType(preCoerce.Type()) {
					continue
				}
				if isCopyExpr(astArg) {
					continue
				}
				cg.emitRelease(block, preCoerce)
			}
			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}
		_ = objVal

		return nil, fmt.Errorf("undefined method: %s.%s", structName, fn.Field)

	case *ast.ScopeAccess:
		// Overload resolution for cross-package calls: pkg::overloadedFn(args).
		bareName := fn.Path[len(fn.Path)-1]
		if variants, hasOverloads := cg.overloads[bareName]; hasOverloads {
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}
				argVals = append(argVals, av)
				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}
			best := cg.resolveOverload(variants, argVals)
			if best != nil {
				if oEntry, oOk := cg.curScope.lookup(best.irName); oOk {
					var ovCallee value.Value
					if oEntry.isAlloc {
						ptrType := oEntry.val.Type().(*irtypes.PointerType)
						ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
					} else {
						ovCallee = oEntry.val
					}
					argValsPreCoerce := append([]value.Value(nil), argVals...)
					if f, ok2 := ovCallee.(*ir.Func); ok2 {
						argVals = cg.adaptArgs(block, argVals, f.Sig)
					}
					result := block.NewCall(ovCallee, argVals...)
					for i, astArg := range e.Args {
						if i >= len(argValsPreCoerce) {
							break
						}
						preCoerce := argValsPreCoerce[i]
						postCoerce := argVals[i]
						if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
							cg.emitRelease(block, postCoerce)

							continue
						}
						if !isRCTrackedType(preCoerce.Type()) {
							continue
						}
						if isCopyExpr(astArg) {
							continue
						}
						cg.emitRelease(block, preCoerce)
					}
					if irtypes.IsVoid(result.Type()) {
						return nil, nil
					}

					return result, nil
				}
			}
		}
		// Generic function call without explicit type arg: infer type and monomorphize.
		// Check genericFuncs first, then constrainedFuncs for cross-package generic calls.
		for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
			result, _, found, err2 := cg.callGenericFromMap(block, e.Args, bareName, m)
			if err2 != nil {
				return nil, err2
			}
			if found {

				return result, nil
			}
		}
		// e.g. weather.sunny used as function - probably an error, but handle gracefully.
		v, err := cg.genScopeAccess(block, fn)
		if err != nil {
			return nil, err
		}
		callee = v

	case *ast.IndexExpr:
		// Explicit generic instantiation: fn[TypeArg](args) or pkg::fn[TypeArg](args)
		// The parser represents decode[person](src) as
		//   CallExpr{Func: IndexExpr{Expr: decode_or_scope, Index: type_ident}, Args: [src]}
		if typeArgID, ok := fn.Index.(*ast.Identifier); ok {
			// Get the function name (bare or scope-qualified)
			var funcName string
			switch inner := fn.Expr.(type) {
			case *ast.Identifier:
				funcName = inner.Name
			case *ast.ScopeAccess:
				funcName = inner.Path[len(inner.Path)-1]
			}
			typeArgName := typeArgID.Name
			// If the explicit type argument refers to a type parameter that has been
			// substituted (e.g. a recursive call like _encode_any[T](...) inside
			// _encode_any__jt_rect), resolve it to the concrete type so we don't
			// create a self-referential alias ("T" -> "T") that causes infinite recursion.
			if alias, ok := cg.typeAliases[typeArgName]; ok {
				if st, ok2 := alias.(*ast.SimpleType); ok2 && st.Name != typeArgName {
					typeArgName = st.Name
				}
			}
			if funcName != "" {
				// Look up the generic function template
				tmpl, isGeneric := cg.genericFuncs[funcName]
				if !isGeneric {
					tmpl, isGeneric = cg.constrainedFuncs[funcName]
				}
				if isGeneric && len(tmpl.TypeParams) > 0 {
					typeSubst := map[string]string{tmpl.TypeParams[0]: typeArgName}
					instKey := typeArgName
					concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
					if err2 != nil {
						return nil, err2
					}
					// Build argument list and call
					argVals := make([]value.Value, 0, len(e.Args))
					for _, arg := range e.Args {
						av, err3 := cg.genExpr(block, arg)
						if err3 != nil {
							return nil, err3
						}
						argVals = append(argVals, av)
						if cg.curBlock != nil && cg.curBlock != block {
							block = cg.curBlock
						}
					}
					argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)

					return block.NewCall(concreteFunc, argVals...), nil
				}
			}
		}
		// Fallthrough: evaluate as regular index expression used as function
		var err error
		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}
		if callee != nil && isFatFnPtr(callee.Type()) {
			return cg.callFatFn(block, callee, e.Args)
		}

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
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
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
		preCoerce := llArgsPreCoerce[argIdx]
		postCoerce := llArgs[argIdx]
		argIdx++

		// Case 1: adaptArgs boxed a non-any value to any (fresh _tin_rc_alloc).
		// The box is now owned by us; release it after the call regardless of
		// whether the source expression was a copy (identifier) or a temporary.
		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}

		// Case 2: pre-coerce value is RC-tracked and the argument is a temporary.
		if !isRCTrackedType(preCoerce.Type()) {
			continue
		}
		if isCopyExpr(astArg) {
			// Named variable: its scope entry will release it at scope exit.

			continue
		}
		// Temporary fresh allocation: release our reference.
		cg.emitRelease(block, preCoerce)
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
			// Variadic position: fat-ptrs are not valid C varargs - unwrap to
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
			if _, isGeneric := cg.genericStructsByArity[baseName]; isGeneric {
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

// tryResolveStructTypeName tries to interpret expr as a struct (or generic struct)
// type name, returning (structName, typeArgStr). structName is the base struct
// name registered in cg.structTypes or cg.genericStructsByArity; typeArgStr is the
// concrete type parameter (e.g. "i64" for Channel[i64]) or "" for non-generic.
// Returns ("", "") when expr does not resolve to a known struct type.
func (cg *CodeGen) tryResolveStructTypeName(expr ast.Node) (string, string) {
	switch e := expr.(type) {
	case *ast.Identifier:
		if _, ok := cg.structTypes[e.Name]; ok {
			return e.Name, ""
		}
		if _, ok := cg.genericStructsByArity[e.Name]; ok {
			return e.Name, ""
		}
		// Check type alias.
		if ta, ok := cg.typeAliases[e.Name]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}
				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.ScopeAccess:
		// pkg.Type or pkg::Type - resolve via type alias.
		key := strings.Join(e.Path, ".")
		if ta, ok := cg.typeAliases[key]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}
				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
		key2 := strings.Join(e.Path, "::")
		if ta, ok := cg.typeAliases[key2]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}
				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.IndexExpr:
		// Generic instantiation: Channel[i64] or sync::Channel[i64]
		base, _ := cg.tryResolveStructTypeName(e.Expr)
		if base == "" {
			return "", ""
		}
		if typeArgID, ok := e.Index.(*ast.Identifier); ok {
			return base, typeArgID.Name
		}

		return base, ""
	}

	return "", ""
}

// isStaticMethodIR returns true when fn's first parameter is NOT a receiver
// of structName's type (meaning the method is a static/constructor method).
func (cg *CodeGen) isStaticMethodIR(fn *ir.Func, structName string) bool {
	if len(fn.Sig.Params) == 0 {
		return true
	}
	st, ok := cg.structTypes[structName]
	if !ok {
		return true
	}
	first := fn.Sig.Params[0]
	if first.Equal(st) {
		return false // instance method: first param is the struct value
	}
	if pt, isPtr := first.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(st) {
		return false // pointer receiver
	}

	return true
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
	typeName := e.TypeName
	// Generic struct literal with explicit type args: Name[T1, T2]{...}
	// Monomorphize to the concrete name Name__T1__T2 (resolving type aliases).
	if len(e.TypeArgs) > 0 {
		parts := make([]string, len(e.TypeArgs))
		resolvedTypeArgs := make([]ast.TypeExpr, len(e.TypeArgs))
		for i, ta := range e.TypeArgs {
			// Resolve through type aliases (handles generic function type params like t->i64).
			lt, err2 := cg.tinTypeToLLVM(ta)
			if err2 == nil {
				parts[i] = llvmTypeToTinName(lt)
			} else {
				parts[i] = ta.String()
			}
			resolvedTypeArgs[i] = &ast.SimpleType{Name: parts[i]}
		}
		concreteName := typeName + "__" + strings.Join(parts, "__")
		if _, done := cg.structTypes[concreteName]; !done {
			synthDecl := &ast.TypeDecl{
				Name: concreteName,
				Type: &ast.GenericType{Name: typeName, TypeParams: resolvedTypeArgs},
			}
			_ = cg.genTypeDecl(synthDecl)
		}
		typeName = concreteName
		// Rewrite the StructLit to use the concrete name for the rest of genStructLit.
		e = &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
	}
	// Resolve through type aliases to the canonical struct name
	// (e.g., bare "Mutex" -> "sync__Mutex" after canonical naming).
	if _, exists := cg.structTypes[typeName]; !exists {
		if alias, ok2 := cg.typeAliases[typeName]; ok2 {
			if simple, ok3 := alias.(*ast.SimpleType); ok3 {
				typeName = simple.Name
				e = &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
			}
		}
	}
	st, ok := cg.structTypes[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown struct type: %s", typeName)
	}

	alloca := block.NewAlloca(st)
	// Zero-initialize the struct so unspecified fields start as 0/nil.
	// Without this, ARC retain/release on array or string fields (which are
	// fat-ptrs) would operate on garbage stack values and segfault.
	block.NewStore(constant.NewZeroInitializer(st), alloca)
	fieldNames := cg.structFields[e.TypeName]
	vtableOff := cg.vtableOffset(e.TypeName)
	// userOff = 1 (type_id) + vtable fields
	userOff := 1 + vtableOff

	// Initialize the leading i32 type_id field (index 0).
	if typeID, ok := cg.structTypeIDs[e.TypeName]; ok {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	// Initialize embedded vtable pointer fields (indices 1 ... vtableOff).
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
			// ARC: retain RC-tracked values that are copied from existing owners
			// (variables, field accesses, index expressions).  Without this, the
			// original owner's scope-release would free the buffer while the struct
			// still holds a reference to it.
			if isCopyExpr(v) {
				cg.emitRetain(block, val)
			}
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
			// ARC: retain RC-tracked values copied from existing owners.
			if isCopyExpr(f.Value) {
				cg.emitRetain(block, val)
			}
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

	// Call chained trait init methods (for traits that also define fn init).
	for _, traitInitFn := range cg.traitChainedInits[e.TypeName] {
		args := cg.adaptArgs(block, []value.Value{result}, traitInitFn.Sig)
		block.NewCall(traitInitFn, args...)
	}

	return result, nil
}

// genTupleLit generates code for a tuple literal (e1, e2, ...).
// expectedType is the LLVM struct type of the target if known (e.g. from a
// variable declaration or return type); pass nil to infer from element types.
func (cg *CodeGen) genTupleLit(block *ir.Block, tup *ast.TupleLit, expectedType irtypes.Type) (value.Value, error) {
	if len(tup.Elems) < 2 {
		return nil, fmt.Errorf("tuple literal requires at least 2 elements")
	}

	// Evaluate all element expressions.
	vals := make([]value.Value, len(tup.Elems))
	for i, elem := range tup.Elems {
		v, err := cg.genExpr(block, elem)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}

	// Determine concrete Tuple struct name.
	// If expectedType is a known named struct, use it directly.
	var concreteName string
	if expectedType != nil {
		if st, ok := expectedType.(*irtypes.StructType); ok {
			if n := st.Name(); n != "" {
				if _, known := cg.structTypes[n]; known {
					concreteName = n
				}
			}
		}
	}
	if concreteName == "" {
		// Infer from element LLVM types.
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = llvmTypeToTinName(v.Type())
		}
		concreteName = "Tuple__" + strings.Join(parts, "__")
		// Trigger monomorphization for this concrete name.
		if _, done := cg.structTypes[concreteName]; !done {
			typeParams := make([]ast.TypeExpr, len(parts))
			for i, p := range parts {
				typeParams[i] = &ast.SimpleType{Name: p}
			}
			synthDecl := &ast.TypeDecl{
				Name: concreteName,
				Type: &ast.GenericType{Name: "Tuple", TypeParams: typeParams},
			}
			_ = cg.genTypeDecl(synthDecl)
		}
	}

	st, ok := cg.structTypes[concreteName]
	if !ok {
		return nil, fmt.Errorf("failed to monomorphize Tuple type %q", concreteName)
	}

	alloca := block.NewAlloca(st)
	block.NewStore(constant.NewZeroInitializer(st), alloca)

	// Set type_id field (index 0).
	if typeID, has := cg.structTypeIDs[concreteName]; has {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	// userOff = 1 (type_id) + vtable fields (Tuple has none).
	userOff := 1 + cg.vtableOffset(concreteName)

	// Store each element positionally into fields a, b, c, ...
	for i, v := range vals {
		idx := userOff + i
		if idx >= len(st.Fields) {
			break
		}
		fieldType := st.Fields[idx]
		v = cg.coerce(block, v, fieldType)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		block.NewStore(v, gep)
		// ARC: retain any RC-tracked elements.
		if isCopyExpr(tup.Elems[i]) {
			cg.emitRetain(block, v)
		}
	}

	return block.NewLoad(st, alloca), nil
}

// genSliceExpr generates code for a slice expression arr[start:end].
func (cg *CodeGen) genSliceExpr(block *ir.Block, e *ast.SliceExpr) (value.Value, error) {
	arrVal, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// Only fat-pointer arrays {T*, i64} are supported for slicing.
	arrType, ok := arrVal.Type().(*irtypes.StructType)
	if !ok || len(arrType.Fields) < 2 {
		return nil, fmt.Errorf("slice expression requires a fat-array type, got %s", arrVal.Type())
	}

	ptrField := arrType.Fields[0]
	ptrType, isPtrType := ptrField.(*irtypes.PointerType)
	if !isPtrType {
		return nil, fmt.Errorf("slice expression: first field must be a pointer, got %s", ptrField)
	}
	elemType := ptrType.ElemType

	alloca := block.NewAlloca(arrType)
	block.NewStore(arrVal, alloca)

	// Extract data pointer and length from fat-array.
	dataGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	lenGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataPtr := block.NewLoad(ptrType, dataGep)
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	var startVal, endVal value.Value

	if e.Start != nil {
		sv, err := cg.genExpr(block, e.Start)
		if err != nil {
			return nil, err
		}
		startVal = cg.coerce(block, sv, irtypes.I64)
	} else {
		startVal = constant.NewInt(irtypes.I64, 0)
	}

	if e.End != nil {
		ev, err := cg.genExpr(block, e.End)
		if err != nil {
			return nil, err
		}
		endVal = cg.coerce(block, ev, irtypes.I64)
	} else {
		endVal = arrLen
	}

	// newDataPtr = GEP(elemType, dataPtr, startVal)
	newDataPtr := block.NewGetElementPtr(elemType, dataPtr, startVal)
	// newLen = endVal - startVal
	newLen := block.NewSub(endVal, startVal)

	// Build new fat-array {T*, i64}.
	resultAlloca := block.NewAlloca(arrType)
	newDataGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	newLenGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(newDataPtr, newDataGep)
	block.NewStore(newLen, newLenGep)

	// Expose the BASE allocation pointer (before the GEP offset) so that genVarDecl
	// can retain/release the actual ARC block rather than a possibly-interior pointer.
	// For start==0 newDataPtr==dataPtr; for start>0 newDataPtr is interior.
	cg.lastSliceBase = block.NewBitCast(dataPtr, irtypes.I8Ptr)

	return block.NewLoad(arrType, resultAlloca), nil
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
	// a |> f(args) = f(args)(a)  - curried style: call f(args) first, then call
	// the returned function with a.
	// a |> f         = f(a)      - plain function value on the right.
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
	// Call through the function (fat-pointer or plain).
	var result value.Value
	if isFatFnPtr(rightFn.Type()) {
		fnPtr := block.NewExtractValue(rightFn, 0)
		envPtr := block.NewExtractValue(rightFn, 1)
		fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		llArgs := cg.adaptArgs(block, []value.Value{envPtr, leftVal}, fnType)
		result = block.NewCall(fnPtr, llArgs...)
	} else {
		result = block.NewCall(rightFn, leftVal)
	}
	// ARC: release the left-hand value if it is a temporary RC allocation.
	if isRCTrackedType(leftVal.Type()) && !isCopyExpr(e.Left) {
		cg.emitRelease(block, leftVal)
	}
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

	// Typed is-check: "x is v T" - check the tag and optionally bind the payload.
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

	}
	// any type check: "x is dog" where x is any - compare type_id (field 0).
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

// isAsyncTraitMethod reports whether methodName is a {#async} virtual method
// of the trait identified by instKey.
func (cg *CodeGen) isAsyncTraitMethod(instKey, methodName string) bool {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	for _, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return true
		}
	}

	return false
}

// asyncCoroSlotIndex returns the vtable field index of the $coro slot for
// methodName in instKey's vtable.  Returns -1 if not found.
// $coro slots are appended after all sync slots:
//
//	index = len(syncMethods) + position_in_asyncMethods_list
func (cg *CodeGen) asyncCoroSlotIndex(instKey, methodName string) int {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	syncCount := len(cg.traitMethodOrder[baseTrait])
	for i, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return syncCount + i
		}
	}

	return -1
}

// traitMethodRetType returns the sync LLVM return type for methodName in
// instKey's vtable (slot 0..N-1).  Returns nil if not found.
func (cg *CodeGen) traitMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	methodOrder := cg.traitMethodOrder[baseTrait]
	vtableSt := cg.traitVtableStructTypes[instKey]
	if vtableSt == nil {
		return nil
	}
	for i, name := range methodOrder {
		if name == methodName {
			fnPtr, ok := vtableSt.Fields[i].(*irtypes.PointerType)
			if !ok {
				return nil
			}
			ft, ok := fnPtr.ElemType.(*irtypes.FuncType)
			if !ok {
				return nil
			}

			return ft.RetType
		}
	}

	return nil
}

// traitAsyncMethodRetType returns the LLVM return type for an {#async} virtual method
// by looking up the trait declaration.  Used when the method has no sync vtable slot
// (async-only traits like io::AsyncReader).
func (cg *CodeGen) traitAsyncMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	td, ok := cg.traits[baseTrait]
	if !ok {
		return nil
	}
	for _, m := range td.Methods {
		if m.Name != methodName || !isAsyncTag(m.Tags) {
			continue
		}
		if m.RetType == nil {
			return irtypes.Void
		}
		lt, err := cg.tinTypeToLLVM(m.RetType)
		if err != nil {
			return nil
		}

		return lt
	}

	return nil
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

	// Build args (index 0 = env, indices 1..N = actual params).
	llArgs := []value.Value{envPtr}
	llArgsPreCoerce := []value.Value{envPtr}
	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		llArgs = append(llArgs, av)
		llArgsPreCoerce = append(llArgsPreCoerce, av)
	}

	// Adapt args to the underlying function's signature.
	fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	llArgs = cg.adaptArgs(block, llArgs, fnType)

	result := block.NewCall(fnPtr, llArgs...)

	// ARC: release temporary RC-tracked arguments (skip index 0 = env).
	for i, astArg := range argNodes {
		argIdx := i + 1 // offset by 1 for the env slot
		preCoerce := llArgsPreCoerce[argIdx]
		postCoerce := llArgs[argIdx]

		// Case 1: adaptArgs boxed a non-any value to any.
		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}
		// Case 2: RC-tracked temporary argument.
		if !isRCTrackedType(preCoerce.Type()) {
			continue
		}
		if isCopyExpr(astArg) {
			continue
		}
		cg.emitRelease(block, preCoerce)
	}

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
	// resolve to module-level IR functions (not allocas) - those are callable
	// directly by name and don't need capturing.
	var captures []closureCapture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
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
		captures = append(captures, closureCapture{name: n, val: val, llvmTy: ty})
	}

	// Step 2: build env struct and malloc it (if there are captures)
	envI8Ptr, envStructType := cg.buildEnv(block, captures)

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

	prevCtx := cg.pushClosureCtx(f)

	// Step 4: unpack captures from env inside the lambda body
	cg.unpackEnv(entry, f, envStructType, captures)

	// Register lambda params (skip index 0 = env).
	for i, p := range e.Params {
		param := f.Params[i+1]
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}
		alloca := entry.NewAlloca(pt)
		entry.NewStore(param, alloca)
		// ARC: retain RC-tracked params so scope-exit release is balanced.
		// Same convention as genFuncDeclAs: callee owns a reference.
		cg.emitRetain(entry, param)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRCTrackedType(pt)})
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

	cg.popClosureCtx(prevCtx)

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
				// Unknown format specifier - fall through to default handling
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
	//   1. snprintf(NULL, 0, fmt, ...) -> returns the required length (excluding NUL).
	//   2. _tin_rc_alloc(len+1) -> allocate exact buffer with ARC header.
	//   3. snprintf(buf, len+1, fmt, ...) -> fill buffer.
	// This avoids a fixed-size buffer and handles arbitrarily long interpolations.
	// IMPORTANT: must use _tin_rc_alloc (not malloc) so that the result is ARC-tracked
	// and _tin_release can safely read the RC header 8 bytes before the returned ptr.
	fmtStr := strings.Join(fmtParts, "")
	fmtPtr := cg.newGlobalString(fmtStr)
	snprintfFn := cg.ensureSnprintf()

	// Pass 1: measure required length.
	nullBuf := constant.NewNull(irtypes.I8Ptr)
	sizeZero := constant.NewInt(irtypes.I64, 0)
	measureArgs := []value.Value{nullBuf, sizeZero, fmtPtr}
	measureArgs = append(measureArgs, args...)
	needed := block.NewCall(snprintfFn, measureArgs...) // i32
	neededI64 := block.NewSExt(needed, irtypes.I64)
	allocSize := block.NewAdd(neededI64, constant.NewInt(irtypes.I64, 1)) // +1 for NUL

	// Pass 2: allocate with ARC header and fill.
	buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
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

// --------------------------------------------------------------------------
// Fiber expression helpers
// --------------------------------------------------------------------------

// canonicalUnitStructName returns the LLVM struct name for the sync Unit type.
// After canonical naming, this is "sync__Unit" when sync was loaded from source.
// Falls back to "Unit" for pre-compiled .tin.mod scenarios.
func (cg *CodeGen) canonicalUnitStructName() string {
	// Prefer the canonical package-prefixed name.
	if _, ok := cg.structTypes["sync__Unit"]; ok {
		return "sync__Unit"
	}
	// Try the type alias (covers pre-compiled mod scenarios).
	if alias, ok := cg.typeAliases["sync::Unit"]; ok {
		if simple, ok2 := alias.(*ast.SimpleType); ok2 {
			return simple.Name
		}
	}

	return "Unit"
}

// wrapPidInFuture wraps a fiber PID (i64) in a Future[T] struct value.
// calleeName is used to look up the original return type; pass "" for void/do-block spawns.
// Returns an error if the sync module is not available or Future[T] cannot be instantiated.
func (cg *CodeGen) wrapPidInFuture(block *ir.Block, pid value.Value, calleeName string) (value.Value, error) {
	// Determine the type parameter: original function's return type, or Unit for void.
	var retTypeExpr ast.TypeExpr
	if calleeName != "" {
		if origDecl, ok := cg.funcDecls[calleeName]; ok && origDecl.RetType != nil {
			retTypeExpr = origDecl.RetType
		}
	}
	if retTypeExpr == nil {
		retTypeExpr = &ast.SimpleType{Name: "Unit"}
	}

	// Get string name for the concrete type parameter (e.g., "i64", "string").
	retTypeLLVM, err := cg.tinTypeToLLVM(retTypeExpr)
	if err != nil {
		return nil, fmt.Errorf("spawn: cannot determine return type for Future[T]: %w", err)
	}
	retTypeStr := llvmTypeName(retTypeLLVM)

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if _, exists := cg.structTypes[futureConcreteName]; !exists {
		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			return nil, fmt.Errorf("spawn: cannot instantiate Future[%s]: %w", retTypeStr, monoErr)
		}
	}

	// Call Future[T].make(pid) to construct the struct value properly
	// (sets type_id, vtable pointer, and pid field).
	makeFnName := futureConcreteName + "_make"
	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: stdlib/sync failed to load: %w; ensure the tin executable is in its installed location alongside the stdlib/ directory", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - stdlib/sync could not be loaded; ensure the tin executable is in its installed location alongside the stdlib/ directory, or add `use sync` explicitly before using spawn/await", retTypeStr)
	}
	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// genSpawnExpr generates code for `spawn callExpr`.
// The callee must be a function marked {#async} (in coroCallable).
// Returns Future[T] wrapping the fiber PID.
func (cg *CodeGen) genSpawnExpr(block *ir.Block, e *ast.SpawnExpr) (value.Value, error) {
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// spawn do: block -> synthesize an anonymous {#async} function and spawn it.
	if e.DoBlock != nil {
		return cg.genSpawnDoBlock(block, e.DoBlock)
	}

	// Determine the call node and callee name.
	callNode, ok := e.Call.(*ast.CallExpr)
	if !ok {
		return nil, fmt.Errorf("spawn: expected function call expression")
	}

	// Handle method calls: spawn obj.method(args)
	if fa, ok2 := callNode.Func.(*ast.FieldAccess); ok2 {
		return cg.genSpawnMethodExpr(block, callNode, fa)
	}

	var calleeName string
	var scopeKey string
	switch fn := callNode.Func.(type) {
	case *ast.Identifier:
		calleeName = fn.Name
		scopeKey = fn.Name
	case *ast.ScopeAccess:
		// e.g. io::async_write -> bareName="async_write", scopeKey="io.async_write"
		calleeName = fn.Path[len(fn.Path)-1]
		scopeKey = strings.Join(fn.Path, ".")
	}

	if calleeName == "" {
		return nil, fmt.Errorf("spawn: cannot determine callee name; only named function calls are supported")
	}

	// Evaluate arguments first so we can do overload resolution if needed.
	var callArgs []value.Value
	for _, arg := range callNode.Args {
		val, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		callArgs = append(callArgs, val)
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	// Look up the sync function first to derive its IR name (which may differ from calleeName)
	// and to get its return type for wrapPidInFuture.
	// e.g. for bare "async_write" inside io.tin, the scope entry points to "io__async_write".
	var syncIRName string
	var syncFnRetType irtypes.Type
	for _, key := range []string{scopeKey, calleeName} {
		if se2, ok3 := cg.curScope.lookup(key); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				syncIRName = fn2.Name()
				syncFnRetType = fn2.Sig.RetType

				break
			}
		}
	}

	// Look up the $coro variant of the callee.
	// Try bare name, scope-qualified name, and sync IR name (for cross-package).
	var coroFn *ir.Func
	coroKeys := []string{calleeName + "$coro", scopeKey + "$coro"}
	if syncIRName != "" && syncIRName != calleeName && syncIRName != scopeKey {
		coroKeys = append(coroKeys, syncIRName+"$coro")
	}
	for _, coroKey := range coroKeys {
		if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				coroFn = fn2

				break
			}
		}
	}

	// resolvedCalleeName is the key for funcDecls (always the bare name for return-type lookup).
	resolvedCalleeName := calleeName

	// Try overload resolution if direct lookup failed.
	if coroFn == nil && len(cg.overloads[calleeName]) > 0 {
		best := cg.resolveOverload(cg.overloads[calleeName], callArgs)
		if best != nil {
			// Also capture the sync function's return type for wrapPidInFuture.
			if se3, ok3 := cg.curScope.lookup(best.irName); ok3 {
				if fn3, ok4 := se3.val.(*ir.Func); ok4 && syncFnRetType == nil {
					syncFnRetType = fn3.Sig.RetType
				}
			}
			for _, coroKey := range []string{best.irName + "$coro", calleeName + "$coro"} {
				if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
					if fn2, ok4 := se2.val.(*ir.Func); ok4 {
						coroFn = fn2
						resolvedCalleeName = best.irName

						break
					}
				}
			}
		}
	}

	if coroFn == nil {
		return nil, fmt.Errorf("spawn: function %q does not have an {#async} variant; add {#async} tag", calleeName)
	}

	// Coerce arguments to match coro function params.
	// Note: no ARC retain here - the $coro ramp block retains RC-tracked
	// params before the initial suspend (see genCoroFuncBody).  A caller-side
	// retain would double-count and produce a leak.
	for i, val := range callArgs {
		if i < len(coroFn.Params) {
			callArgs[i] = cg.coerce(block, val, coroFn.Params[i].Type())
		}
	}

	// Call the ramp function: hdl = callee$coro(args...)
	hdl := block.NewCall(coroFn, callArgs...)

	// Spawn the fiber: pid = _tin_fiber_spawn(hdl)
	pid := block.NewCall(cg.fiberSpawnFn, hdl)

	// Wrap pid in Future[t] where t is the original function's return type.
	// Prefer the funcDecl lookup (bare name), fall back to sync function's LLVM return type.
	if _, hasFuncDecl := cg.funcDecls[resolvedCalleeName]; hasFuncDecl {
		return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
	}
	if syncFnRetType != nil {
		return cg.wrapPidInFutureWithLLVMType(block, pid, syncFnRetType)
	}

	return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
}

// genSpawnMethodExpr handles `spawn obj.method(args)` - spawns a method call as a fiber.
func (cg *CodeGen) genSpawnMethodExpr(block *ir.Block, callNode *ast.CallExpr, fa *ast.FieldAccess) (value.Value, error) {
	objVal, err := cg.genExpr(block, fa.Expr)
	if err != nil {
		return nil, err
	}

	// Trait fat-ptr method: spawn traitObj.method(args)
	if instKey, isTrait := cg.isTraitFatPtr(objVal.Type()); isTrait {
		if !cg.isAsyncTraitMethod(instKey, fa.Field) {
			return nil, fmt.Errorf("spawn: trait method %q is not {#async}", fa.Field)
		}
		coroSlotIdx := cg.asyncCoroSlotIndex(instKey, fa.Field)
		if coroSlotIdx < 0 {
			return nil, fmt.Errorf("spawn: no $coro slot for trait method %q", fa.Field)
		}

		dataPtr := block.NewExtractValue(objVal, 0)
		vtablePtr := block.NewExtractValue(objVal, 1)

		vtableSt := cg.traitVtableStructTypes[instKey]
		fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(coroSlotIdx)))
		coroSlotFnPtrType := vtableSt.Fields[coroSlotIdx].(*irtypes.PointerType)
		coroSlotFnType := coroSlotFnPtrType.ElemType.(*irtypes.FuncType)
		fnPtr := block.NewLoad(coroSlotFnPtrType, fnPtrGep)

		// Evaluate args
		llArgs := []value.Value{dataPtr}
		for _, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}
			llArgs = append(llArgs, av)
		}
		llArgs = cg.adaptArgs(block, llArgs, coroSlotFnType)

		hdl := block.NewCall(fnPtr, llArgs...)
		pid := block.NewCall(cg.fiberSpawnFn, hdl)

		// Get the actual return type of the async method (not the coro wrapper's i8*).
		// For async-only traits, traitMethodRetType returns nil (no sync slot), so we
		// fall back to looking up the method's return type from the trait declaration.
		retType := cg.traitMethodRetType(instKey, fa.Field)
		if retType == nil {
			retType = cg.traitAsyncMethodRetType(instKey, fa.Field)
		}

		return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
	}

	// Concrete struct method: look up structName_method$coro
	// Handle both value receivers (StructType) and pointer receivers (*StructType).
	structName := cg.typeNameOf(objVal.Type())
	if structName == "" {
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			structName = cg.typeNameOf(pt.ElemType)
		}
	}
	if structName == "" {
		return nil, fmt.Errorf("spawn: cannot determine struct type for method call on %s", objVal.Type())
	}
	coroName := structName + "_" + fa.Field + "$coro"
	se2, ok3 := cg.curScope.lookup(coroName)
	if !ok3 {
		return nil, fmt.Errorf("spawn: method %s.%s does not have a $coro variant; is it {#async}?", structName, fa.Field)
	}
	coroFn2, ok4 := se2.val.(*ir.Func)
	if !ok4 {
		return nil, fmt.Errorf("spawn: %s is not a function", coroName)
	}

	// Build call args: (obj, args...)
	coroArgs := []value.Value{objVal}
	for i, arg := range callNode.Args {
		av, err2 := cg.genExpr(block, arg)
		if err2 != nil {
			return nil, err2
		}
		if i+1 < len(coroFn2.Params) {
			av = cg.coerce(block, av, coroFn2.Params[i+1].Type())
		}
		coroArgs = append(coroArgs, av)
	}

	hdl2 := block.NewCall(coroFn2, coroArgs...)
	pid2 := block.NewCall(cg.fiberSpawnFn, hdl2)
	// Use the original method name for return type lookup.
	fnName := structName + "_" + fa.Field

	return cg.wrapPidInFuture(block, pid2, fnName)
}

// wrapPidInFutureWithLLVMType wraps a fiber PID in Future[T] using the LLVM type directly.
// Used when we have the concrete LLVM return type but no funcDecl entry (e.g., trait method spawns).
func (cg *CodeGen) wrapPidInFutureWithLLVMType(block *ir.Block, pid value.Value, retType irtypes.Type) (value.Value, error) {
	var retTypeStr string
	if retType == nil || retType.Equal(irtypes.Void) {
		// Resolve the canonical name of the Unit struct.  After the canonical
		// naming change, the Unit LLVM struct may be registered as "sync__Unit".
		retTypeStr = cg.canonicalUnitStructName()
	} else {
		retTypeStr = llvmTypeName(retType)
	}

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if _, exists := cg.structTypes[futureConcreteName]; !exists {
		retTypeExpr := &ast.SimpleType{Name: retTypeStr}
		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			// Try Unit as fallback (use canonical name)
			futureConcreteName = "Future__" + cg.canonicalUnitStructName()
		}
	}

	makeFnName := futureConcreteName + "_make"
	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: stdlib/sync failed to load: %w", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - stdlib/sync could not be loaded", retTypeStr)
	}
	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// genSpawnDoBlock synthesizes an anonymous {#async} function from a `spawn do:` body block,
// predeclares and generates its $coro variant, then spawns it as a fiber.
func (cg *CodeGen) genSpawnDoBlock(block *ir.Block, doBlock *ast.Block) (value.Value, error) {
	// Generate a unique name for the anonymous async function.
	anonName := fmt.Sprintf("__spawn_do_%d", cg.spawnDoCounter)
	cg.spawnDoCounter++

	// Collect free variables referenced in the do-block body that come from the
	// enclosing function's scope.  These need to be captured by value into an env
	// struct so that the synthesized $coro function can access them safely.
	freeNames := collectFreeVars(doBlock, map[string]bool{})
	var captures []closureCapture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
		}
		if entry.isGlobal {
			continue // module-level global - reachable directly
		}
		if !entry.isAlloc {
			continue // not an alloca - skip
		}
		pt, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			continue
		}
		ty := pt.ElemType
		val := block.NewLoad(ty, entry.val)
		captures = append(captures, closureCapture{name: n, val: val, llvmTy: ty})
	}

	// ARC: retain every RC-tracked capture before packing it into the env struct.
	// The coroutine runs asynchronously, after the parent scope's locals are
	// released.  Without this extra retain the captured strings could be freed
	// while the env still holds a reference to them.
	// The matching release happens in genCoroFuncBody after unpackEnv.
	for _, c := range captures {
		if isRCTrackedType(c.llvmTy) {
			cg.emitRetain(block, c.val)
		}
	}

	// Pack captured values into a heap-allocated env struct.  buildEnv returns a
	// null i8* and nil struct type when there are no captures.
	envI8Ptr, envStructType := cg.buildEnv(block, captures)

	// Synthesize an ast.FuncDecl with no params, void return, and {#async} tag.
	synth := &ast.FuncDecl{
		Name:   anonName,
		Params: nil,
		Tags:   []string{"async"},
		Body:   doBlock,
	}

	// Mark as coro-callable and predeclare the $coro variant (with env pointer
	// as the first parameter so unpackEnv can find it at coroFn.Params[0]).
	cg.coroCallable[anonName] = true
	if err := cg.predeclareCoroVariant(synth, anonName, true); err != nil {
		return nil, fmt.Errorf("spawn do: predeclare failed: %w", err)
	}

	// Generate the $coro body, passing the captures so unpackEnv can restore them.
	if err := cg.genCoroFuncBody(synth, coroVersionName(anonName), captures, envStructType); err != nil {
		return nil, fmt.Errorf("spawn do: coro body generation failed: %w", err)
	}

	// Look up the generated $coro function.
	coroName := coroVersionName(anonName)
	se, ok := cg.curScope.lookup(coroName)
	if !ok || se == nil {
		return nil, fmt.Errorf("spawn do: %s$coro not found after generation", anonName)
	}
	coroFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn do: %s$coro is not a function", anonName)
	}

	// Call the ramp function with the env pointer and spawn the fiber.
	hdl := block.NewCall(coroFn, envI8Ptr)
	pid := block.NewCall(cg.fiberSpawnFn, hdl)

	// Void do-block spawn: wrap in Future[Unit]

	return cg.wrapPidInFuture(block, pid, "")
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
			// Prefer to GEP into the *original* storage (alloca) so that writes
			// through the returned pointer actually mutate the source variable.
			// This is critical for patterns like: let buf [byte;N]; async_read(fd, &buf[0], N)
			if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
				if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok && pt.ElemType.Equal(arrType) {
					return block.NewGetElementPtr(arrType, arrPtr,
						constant.NewInt(irtypes.I32, 0), idx), nil
				}
			}
			// Fallback: copy into a temporary alloca (writes won't affect the original).
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
			// pre-existing behavior for such expressions.
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
			// e.Expr is a variable holding a *struct - dereference once.
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

	case *ast.StructLit:
		// &StructLit{...} - heap-allocate the struct and return a typed pointer.
		// The struct value is constructed normally (with init, field stores, and
		// ARC retains on RC-tracked fields), then stored into malloc'd memory.
		// The caller owns the raw memory; they must release RC fields and call
		// mem::free before the pointer goes out of scope.
		val, err := cg.genStructLit(block, e)
		if err != nil {
			return nil, err
		}
		st, ok2 := val.Type().(*irtypes.StructType)
		if !ok2 {
			return nil, fmt.Errorf("&struct{} requires a struct literal")
		}
		// sizeof(T) via GEP trick on null pointer.
		nullPtr := constant.NewNull(irtypes.NewPointer(st))
		gepOne := block.NewGetElementPtr(st, nullPtr, constant.NewInt(irtypes.I32, 1))
		sz := block.NewPtrToInt(gepOne, irtypes.I64)
		heapI8 := block.NewCall(cg.ensureMalloc(), sz)
		typedPtr := block.NewBitCast(heapI8, irtypes.NewPointer(st))
		block.NewStore(val, typedPtr)

		return typedPtr, nil
	}

	return nil, fmt.Errorf("not an lvalue: %T", node)
}

// callGenericFromMap looks up bareName in m (either genericFuncs or
// constrainedFuncs), evaluates args, infers type arguments, monomorphizes
// the template, and emits the call.  Returns (result, updatedBlock, found,
// error).  found is false when bareName is not in m.
func (cg *CodeGen) callGenericFromMap(
	block *ir.Block,
	args []ast.Node,
	bareName string,
	m map[string]*ast.FuncDecl,
) (value.Value, *ir.Block, bool, error) {
	tmpl, ok := m[bareName]
	if !ok {
		return nil, block, false, nil
	}

	argVals := make([]value.Value, 0, len(args))
	for _, arg := range args {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, block, true, err
		}
		argVals = append(argVals, av)
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	typeSubst := cg.inferTypeArgs(tmpl, argVals)
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

	concreteFunc, err := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
	if err != nil {
		return nil, block, true, err
	}
	argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)

	return block.NewCall(concreteFunc, argVals...), block, true, nil
}
