package codegen

import (
	"fmt"
	"math/big"
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
//
// Contract: on return, cg.curBlock points to the block where the next
// instruction should be emitted. If the expression contains control flow
// (await / yield / short-circuit && / ||) it may differ from `block`.
// Callers that emit follow-up instructions (toBool, NewCondBr, NewStore,
// etc.) must use cg.curBlock, not the input `block`.
func (cg *CodeGen) genExpr(block *ir.Block, node ast.Node) (value.Value, error) {
	if node == nil {
		return nil, nil
	}

	// Establish the post-call invariant: if the expression doesn't advance
	// control flow, cg.curBlock equals the input block on return; if it
	// does (await/yield/&&/||), the handler updates it.
	cg.curBlock = block

	// Track source position for error messages produced deeper in the call stack.
	if p := node.Pos(); p.Line != 0 {
		cg.currentPos = p
	}

	switch e := node.(type) {
	case *ast.IntLit:
		if e.Big != nil {
			// 128 bits = at most u128 range; values above that don't fit
			// either i128 or u128 and would silently wrap inside LLVM.
			if e.Big.BitLen() > 128 {
				return nil, cg.nodeErr(e,
					"integer literal %s exceeds i128/u128 range; use a string-based bignum library for larger values",
					e.Big.String())
			}

			return &constant.Int{Typ: irtypes.I128, X: new(big.Int).Set(e.Big)}, nil
		}

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

	case *ast.ArrayFillLit:
		return cg.genArrayFillLit(block, e)

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
		//   await asyncFn(args)       - inline drive: no fiber allocation (inCoroFn only)
		//
		// Type rule: await is valid iff expr : Awaitable[T] (i.e. Future[T]).
		// Calling a {#async} fn directly within a coroutine uses inline drive.
		// {#async} direct call handling: `await asyncFn(args)` (no explicit spawn).
		//
		// Two cases based on calling context:
		//   inCoroFn == true  -> inline drive: runs inner coro in this fiber's frame,
		//                        no fiber allocation, direct park/unpark via runnext.
		//   inCoroFn == false -> auto-spawn: wrap in synthetic SpawnExpr so the regular
		//                        await-Future path takes over (e.g. sync wrapper body,
		//                        or main() in non-async context).
		futureExpr := e.Future
		if callNode, ok := e.Future.(*ast.CallExpr); ok {
			if cg.inCoroFn {
				// Inside a coroutine: try zero-cost inline drive.
				result, driveErr := cg.genInlineAsyncDrive(block, callNode)
				if driveErr != nil {
					return nil, driveErr
				}

				if result != nil {
					return result, nil
				}
				// (nil, nil) -> callee $coro not in scope yet; fall through to auto-spawn.
			}
			// Not in coroutine (or inline drive not available): auto-spawn if async.
			if cg.directCallHasCoroVariant(callNode) {
				futureExpr = &ast.SpawnExpr{Call: callNode}
			} else {
				// Check whether the callee evaluates to an async fat-ptr.
				// Handles `await x(args)` and `await fns[i](args)`.
				isAsyncFatPtrCallee := false

				switch calleeNode := callNode.Func.(type) {
				case *ast.Identifier:
					// Variable of type fn{#async}(...): check scope.
					if se, seOk := cg.curScope.lookup(calleeNode.Name); seOk && se.isAlloc {
						if pt, ptOk := se.val.Type().(*irtypes.PointerType); ptOk && isAsyncFatFnPtr(pt.ElemType) {
							isAsyncFatPtrCallee = true
						}
					}
				case *ast.IndexExpr:
					// fns[i](args) where fns: [fn{#async}(...)].
					// Let genSpawnExpr evaluate and decide; mark as candidate.
					isAsyncFatPtrCallee = true
				}

				if isAsyncFatPtrCallee {
					futureExpr = &ast.SpawnExpr{Call: callNode}
				}
			}
		}

		val, err := cg.genExpr(block, futureExpr)
		if err != nil {
			return nil, err
		}

		if val == nil {
			return nil, cg.nodeErr(e, "await: expression produced no value")
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
					return nil, fmt.Errorf("await: sync package failed to load so spawn returned a raw pid.\n"+
						"  Ensure the tin executable is alongside the stdlib/ directory.\n"+
						"  Load error: %w", cg.syncLoadErr)
				}

				return nil, cg.nodeErr(e, "await: expression is a raw i64, not a Future[t]; use \"await spawn fn(args)\" which returns Future[t]")
			}

			return nil, cg.nodeErr(e, "await: expression (type %s) does not implement Awaitable[t]; use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] (e.g. fn f() Future[t] = spawn ...)",
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

			return nil, cg.nodeErr(e, "await: expression (type %q) does not implement Awaitable[t]; use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] directly", structName)
		}

		// Extract pid from Future[T] using extractvalue (no alloca -> safe inside loops).
		cg.ensureFiberRuntime()

		pid := block.NewExtractValue(val, uint64(pidIdx))

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
		// Do NOT release pmsg here.  _tin_fiber_get_panic_msg retained it for the
		// caller, and the defer thunk balances that retain: either the thunk
		// releases the discarded recover() result directly (consuming the retain),
		// or it retains pmsg for a captured variable (e.g. "caught = msg").  In
		// the latter case emitAllScopeReleases below releases the captured variable,
		// which decrements the same ref.  Adding an explicit release here would
		// cause a double-free for the discard pattern.

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
			// Release all ARC-tracked scope variables.  The defer thunk has
			// already run via _tin_panic; any variable updated by the thunk
			// (e.g. "caught = msg") now holds an extra ARC reference that must
			// be released before the function returns.  This mirrors the
			// emitAllScopeReleases call in the normal return path.
			cg.emitAllScopeReleases(panicBlk, "")
			// Free any malloc'd defer closure envs.  _tin_panic already called
			// the thunks via the runtime defer chain; only the env allocations
			// remain.  This mirrors emitDefers' env-free loop on the normal path.
			freeFn := cg.ensureFree()
			for i := len(cg.pendingDeferEnvs) - 1; i >= 0; i-- {
				env := cg.pendingDeferEnvs[i]
				if _, isNull := env.(*constant.Null); !isNull {
					panicBlk.NewCall(freeFn, env)
				}
			}

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

		// Use parseTypeParamStr so that pointer-type params like "*my_val" (from
		// Future__*my_val) resolve to the correct LLVM pointer type instead of i64.
		retLLVM, resolveErr := cg.tinTypeToLLVM(parseTypeParamStr(retTypeName))
		if resolveErr != nil || retLLVM == nil || irtypes.IsVoid(retLLVM) {
			return constant.NewInt(irtypes.I1, 1), nil
		}

		// Get the boxed result pointer, unbox it, then free the heap buffer.
		// _tin_fiber_get_result transfers ownership of the malloc'd result box
		// to the caller; the caller must free it after loading the value.
		rawPtr := block.NewCall(cg.fiberGetResultFn, pid)
		typedPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(retLLVM))
		result := block.NewLoad(retLLVM, typedPtr)
		block.NewCall(cg.ensureFree(), rawPtr)
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
		// Block expression: (stmt1; stmt2; ...; last_expr) - produced by CTFE macro splices.
		// Generate all statements and return the value of the last expression.
		// A new scope is pushed so let bindings do not leak into the outer function scope.
		curBlock := block

		cg.curScope = newScope(cg.curScope)

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

		cg.emitScopeRelease(curBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

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
		// Compile-time RC kind for T. Encodes both whether T needs ARC
		// management and where in T's bytes the retainable pointer sits, so
		// the C runtime (Channel, Atomic) can dispatch without knowing the
		// Tin type.
		//
		//   0 = not RC
		//   1 = leading pointer at offset 0 (string, fat array, trait fat ptr)
		//   2 = any: {i32 tag, i8* ptr} -- ptr at offset 8, release with
		//       _tin_release_any so closure-typed `any` values free their env
		//   3 = fn fat ptr: {fn*, env*} -- env at offset 8, release with
		//       _tin_release_closure
		if e.Type == nil {
			return constant.NewInt(irtypes.I32, int64(rcKindNone)), nil
		}

		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}

		return constant.NewInt(irtypes.I32, int64(channelRCKindOf(lt))), nil

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
		// Pointer type cast: p.(*T) bitcasts between pointer types (e.g. *void -> *i64).
		if irtypes.IsPointer(inner.Type()) {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 == nil && irtypes.IsPointer(targetLLVM) && targetLLVM != inner.Type() {
				return block.NewBitCast(inner, targetLLVM), nil
			}
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

	case *ast.MatchStmt:
		return cg.genMatchAsExpr(block, e)

	default:
		return nil, nil
	}
}

// armExprNode returns the expression node from a single-statement arm body.
// It handles both *ast.ExprStmt (bare expression) and *ast.MatchStmt (nested
// match expression used as arm value). Returns nil for anything else.
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
	llvmType, ok := cg.structTypes[sp.TypeName]
	if !ok {
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
func (cg *CodeGen) genMatchAsExpr(block *ir.Block, s *ast.MatchStmt) (value.Value, error) {
	// Validate: each arm must be a single expression OR a divergent
	// terminator (return / break / panic). Divergent arms don't produce a
	// value but unblock the common `let x = match ...: case Ok(v): v;
	// case Err(e): return Err(e)` propagation pattern.
	for i, c := range s.Cases {
		if c.Body == nil || len(c.Body.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: case %d has no body", i)
		}

		if len(c.Body.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: case %d has multiple statements; match expressions allow exactly one expression per arm", i)
		}

		if armExprNode(c.Body.Stmts[0]) == nil && !isExplicitTerminator(c.Body.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: case %d body is not an expression or terminator (use 'return match ...' for statement arms)", i)
		}
	}

	if s.Default != nil {
		if len(s.Default.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: default arm has no body")
		}

		if len(s.Default.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: default arm has multiple statements; match expressions allow exactly one expression per arm")
		}

		if armExprNode(s.Default.Stmts[0]) == nil && !isExplicitTerminator(s.Default.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: default arm body is not an expression or terminator (use 'return match ...' for statement arms)")
		}
	}

	// Determine result type from the first non-divergent arm. Divergent arms
	// (return/break/panic) don't yield a value, so they can't drive type
	// inference; they're skipped here and emitted without a result store.
	var resType irtypes.Type

	for _, c := range s.Cases {
		if isExplicitTerminator(c.Body.Stmts[0]) {
			continue
		}

		if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
			resType = cg.astInferTypeWithPattern(expr, c.Pattern)
		}

		if resType != nil {
			break
		}
	}

	if resType == nil && s.Default != nil && !isExplicitTerminator(s.Default.Stmts[0]) {
		if expr := armExprNode(s.Default.Stmts[0]); expr != nil {
			resType = cg.astInferType(expr)
		}
	}

	// Fall back to the caller's expected type (set by genVarDecl when the let
	// has an explicit annotation, or by genReturn for `return match ...`).
	// This rescues cases where every arm body refers to a pattern-bound name
	// the inference doesn't see (e.g. `case Ok(v): v` from an ADT pattern).
	if resType == nil {
		resType = cg.returnTypeHint
	}

	if resType == nil {
		return nil, cg.nodeErr(s, "match expression: cannot infer result type; annotate the variable or use 'return match ...'")
	}

	resAlloca := block.NewAlloca(resType)

	afterBlock, err := cg.genMatchWithResult(block, s, resAlloca)
	if err != nil {
		return nil, err
	}

	if afterBlock == nil {
		afterBlock = cg.newBlock("match.after")
	}

	cg.curBlock = afterBlock

	return afterBlock.NewLoad(resType, resAlloca), nil
}

func (cg *CodeGen) genIdentifier(block *ir.Block, e *ast.Identifier) (value.Value, error) {
	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		// Nullary ADT variant: bare `None`, `Leaf`, etc.
		if v, err := cg.genDataNullaryConstructor(block, e.Name); err != nil {
			return nil, err
		} else if v != nil {
			return v, nil
		}

		return nil, cg.nodeErr(e, "undefined identifier: %s", e.Name)
	}

	if entry.isAlloc {
		ptrType := entry.val.Type().(*irtypes.PointerType)

		return block.NewLoad(ptrType.ElemType, entry.val), nil
	}

	return entry.val, nil
}

// byteToStringFatPtr wraps a single i8 value in a {i8*, i64} fat-pointer so
// that it can be used on either side of a string ++ byte concatenation.
func byteToStringFatPtr(block *ir.Block, b value.Value) value.Value {
	byteAlloca := block.NewAlloca(irtypes.I8)
	block.NewStore(b, byteAlloca)

	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), byteAlloca, 0)

	return block.NewInsertValue(v0, constant.NewInt(irtypes.I64, 1), 1)
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
			if cg.exprElemIsUnsigned(e.Left) {
				left = block.NewZExt(left, rt)
			} else {
				left = block.NewSExt(left, rt)
			}

			lt = rt
		} else if rBits < lBits {
			if cg.exprElemIsUnsigned(e.Right) {
				right = block.NewZExt(right, lt)
			} else {
				right = block.NewSExt(right, lt)
			}
		}
	} else if irtypes.IsFloat(lt) && irtypes.IsInt(rt) {
		right = block.NewSIToFP(right, lt)
	} else if irtypes.IsInt(lt) && irtypes.IsFloat(rt) {
		left = block.NewSIToFP(left, rt)
		lt = rt
	} else if irtypes.IsFloat(lt) && irtypes.IsFloat(rt) {
		lBits := floatBits(lt.(*irtypes.FloatType))
		rBits := floatBits(rt.(*irtypes.FloatType))

		if lBits != rBits {
			if lfc, ok := left.(*constant.Float); ok {
				// Left is a float literal: reinterpret it as the right side's type.
				v, _ := lfc.X.Float64()
				left = constant.NewFloat(rt.(*irtypes.FloatType), v)
				lt = rt
			} else if rfc, ok := right.(*constant.Float); ok {
				// Right is a float literal: reinterpret it as the left side's type.
				v, _ := rfc.X.Float64()
				right = constant.NewFloat(lt.(*irtypes.FloatType), v)
			} else {
				// Two non-literal floats of different sizes: promote smaller to larger.
				if lBits < rBits {
					left = block.NewFPExt(left, rt)
					lt = rt
				} else {
					right = block.NewFPExt(right, lt)
				}
			}
		}
	}

	isFloat := irtypes.IsFloat(lt)
	// Also treat vectors of floats as float for operator selection.
	if !isFloat {
		if vt, ok := lt.(*irtypes.VectorType); ok {
			isFloat = irtypes.IsFloat(vt.ElemType)
		}
	}

	// Pointer arithmetic: ptr + int -> getelementptr; ptr - int -> getelementptr with negation.
	if ptrType, isPtr := lt.(*irtypes.PointerType); isPtr && irtypes.IsInt(rt) {
		switch e.Op {
		case "+", "-":
			if cg.unsafeDepth == 0 {
				return nil, cg.nodeErr(e,
					"pointer arithmetic requires an `{#unsafe}` block")
			}
		}
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

	// Operator overloading dispatch (Phase 3): if either operand is a user
	// struct that implements the corresponding built-in operator trait, lower
	// to a method call. Falls through to the primitive path when neither
	// operand is a struct, and to the Phase 0 error gate when a struct
	// operand has no matching impl.
	if isStructType(lt) || isStructType(rt) {
		if res, dispatched, derr := cg.dispatchBinOp(block, e, left, right, lt, rt); dispatched {
			return res, derr
		}

		return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
	}

	// Reject arithmetic on string / fat-ptr operands before falling into the
	// integer add/sub paths below -- without this, `s1 + s2` would emit
	// `add { i8*, i64 }` which clang rejects with a confusing low-level
	// error instead of a Tin-level diagnostic. The right concat operator
	// for strings is `++`; surface that in the message.
	if cg.isBadFatPtrArithmetic(e.Op, lt, rt) {
		hint := ""
		if e.Op == "+" && isStringType(lt) && isStringType(rt) {
			hint = " (use %q to concatenate strings)"

			return nil, cg.nodeErr(e,
				"binary operator %q is not defined for operands of type %s and %s"+hint,
				e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt), "++")
		}

		return nil, cg.nodeErr(e,
			"binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
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
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "division by zero")
		}

		if isFloat {
			return block.NewFDiv(left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewUDiv(left, right), nil
		}

		return block.NewSDiv(left, right), nil
	case "%":
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "modulo by zero")
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewURem(left, right), nil
		}

		return block.NewSRem(left, right), nil
	case "==":
		cg.checkTautologicalNilCmp(e, false)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, false)
		// Release temporary string operands after comparison (e.g., fn() == fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "!=":
		cg.checkTautologicalNilCmp(e, true)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, true)
		// Release temporary string operands after comparison (e.g., fn() != fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "<":
		if isFloat {
			return block.NewFCmp(enum.FPredOLT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULT, left, right), nil
		}

		return block.NewICmp(enum.IPredSLT, left, right), nil
	case "<=":
		if isFloat {
			return block.NewFCmp(enum.FPredOLE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULE, left, right), nil
		}

		return block.NewICmp(enum.IPredSLE, left, right), nil
	case ">":
		if isFloat {
			return block.NewFCmp(enum.FPredOGT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGT, left, right), nil
		}

		return block.NewICmp(enum.IPredSGT, left, right), nil
	case ">=":
		if isFloat {
			return block.NewFCmp(enum.FPredOGE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGE, left, right), nil
		}

		return block.NewICmp(enum.IPredSGE, left, right), nil
	case "&":
		return block.NewAnd(left, right), nil
	case "|":
		return block.NewOr(left, right), nil
	case "^":
		return block.NewXor(left, right), nil
	case "<<":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}

		return block.NewShl(left, right), nil
	case ">>":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}
		// Use logical (zero-fill) right shift for unsigned types.
		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewLShr(left, right), nil
		}

		return block.NewAShr(left, right), nil
	case "++":
		// string ++ byte  /  byte ++ string: coerce the i8 operand to a 1-char string fat-ptr.
		// The byte is stored in a stack alloca; the memcpy inside the concat path happens in the
		// same basic block so the alloca lifetime is valid.
		// Track coercion so we skip ARC release on the coerced side (stack, not RC-managed).
		leftCoerced, rightCoerced := false, false

		if isStringType(left.Type()) && irtypes.IsInt(right.Type()) && right.Type().(*irtypes.IntType).BitSize == 8 {
			right = byteToStringFatPtr(block, right)
			rightCoerced = true
		} else if isStringType(right.Type()) && irtypes.IsInt(left.Type()) && left.Type().(*irtypes.IntType).BitSize == 8 {
			left = byteToStringFatPtr(block, left)
			leftCoerced = true
		}
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
			v0 := block.NewInsertValue(constant.NewUndef(fatType), newPtr, 0)
			result := block.NewInsertValue(v0, totalLen, 1)
			// For non-temporary sources, the new buffer shares element pointers
			// with the source array.  Retain each shared element so that releasing
			// the source and the new buffer are independent: each holds its own RC
			// claim and can be released in any order without use-after-free.
			//
			// For temporary sources, the temp buffer is released below (buffer-only,
			// no element release), so elements are effectively transferred to the new
			// buffer without needing a retain.
			//
			// Note: elemNeedsRelease returns false for *irtypes.PointerType (pointer
			// variables don't need scope release), but pointer elements inside [*T]
			// arrays DO need retain/release so we check that case explicitly.
			_, elemIsPtr := elemT.(*irtypes.PointerType)
			needsElemRetain := cg.elemNeedsRelease(elemT) || isRCTrackedType(elemT) || elemIsPtr

			if !isTemporaryProducer(e.Left) && needsElemRetain {
				cg.emitRetainElemSlice(block, newI8Ptr, leftLen, elemT)
			}

			if !isTemporaryProducer(e.Right) && needsElemRetain {
				cg.emitRetainElemSlice(block, rightDst, rightLen, elemT)
			}

			// Release sub-expression temporaries: buffer-only release transfers
			// ownership of elements to the new buffer without a retain.
			if isTemporaryProducer(e.Left) {
				if rcPtr := cg.extractRCDataPtr(block, left, left.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
			}

			if isTemporaryProducer(e.Right) {
				if rcPtr := cg.extractRCDataPtr(block, right, right.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
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
		v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), buf, 0)
		result := block.NewInsertValue(v0, totalLen, 1)
		// Release sub-expression temporaries now that the result is built.
		// Skip byte-to-string coerced operands: their ptr is a stack alloca, not ARC-managed.
		if isTemporaryProducer(e.Left) && !leftCoerced {
			cg.emitRelease(block, left)
		}

		if isTemporaryProducer(e.Right) && !rightCoerced {
			cg.emitRelease(block, right)
		}

		return result, nil
	}

	// No primitive / built-in lowering matched. Until operator overloading
	// lands (docs/plans/operator-overloading.md), there is no user hook
	// either; reject loudly instead of silently producing 0. Phase 0 of
	// that plan exists because the previous silent-zero fall-through hid
	// real bugs at every callsite.
	return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
		e.Op, cg.tinTypeDisplay(left.Type()), cg.tinTypeDisplay(right.Type()))
}

// genEqNeqExpr implements shared handling for == and != operators.
func (cg *CodeGen) genEqNeqExpr(block *ir.Block, left, right value.Value, lt, rt irtypes.Type, isFloat bool, notEqual bool) value.Value {
	if isFloat {
		// IEEE 754 NaN: x == x is false, x != x is true. OEQ matches the
		// first (false on NaN); UNE the second (true on NaN). Using ONE
		// for != would silently fold `x != x` to false, breaking the
		// canonical NaN test pattern.
		if notEqual {
			return block.NewFCmp(enum.FPredUNE, left, right)
		}

		return block.NewFCmp(enum.FPredOEQ, left, right)
	}

	pred := enum.IPredEQ
	if notEqual {
		pred = enum.IPredNE
	}

	// any equality/inequality: dynamically dispatched by runtime.
	if isAnyType(lt) || isAnyType(rt) {
		var tempLeft, tempRight value.Value

		if !isAnyType(lt) {
			left = cg.boxToAny(block, left)
			tempLeft = left
		}

		if !isAnyType(rt) {
			right = cg.boxToAny(block, right)
			tempRight = right
		}

		cmp := block.NewCall(cg.ensureAnyEq(), left, right)

		// Release temporary boxes created by boxToAny - they are fresh RC=1
		// allocations that exist only for this comparison.
		if tempLeft != nil {
			cg.emitRelease(block, tempLeft)
		}

		if tempRight != nil {
			cg.emitRelease(block, tempRight)
		}

		result := cmp
		if notEqual {
			return block.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I64, 0))
		}

		return block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I64, 0))
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

// genLogicalAnd emits short-circuit `A && B` as `if A { B } else { false }`.
// The RHS evaluates only when LHS is true. cg.curBlock is updated to the
// merge block on return so the caller continues emitting there. Callers that
// reference `block` (the input) post-call would target a terminated block;
// they must use cg.curBlock instead.
func (cg *CodeGen) genLogicalAnd(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, false)
}

// genLogicalOr emits short-circuit `A || B` as `if A { true } else { B }`.
// Symmetric to genLogicalAnd; see that function's note about cg.curBlock.
func (cg *CodeGen) genLogicalOr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, true)
}

// genShortCircuit lowers a logical && or || with proper short-circuit
// semantics. shortVal is the value the operator returns when the LHS
// already determines the result: false for &&, true for ||. The RHS
// is evaluated only when the LHS does NOT short-circuit.
func (cg *CodeGen) genShortCircuit(block *ir.Block, e *ast.BinExpr, shortVal bool) (value.Value, error) {
	cg.curBlock = block

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, left.Type()); err != nil {
		return nil, err
	}

	leftEnd := cg.curBlock
	leftBool := cg.toBool(leftEnd, left)

	var label string
	if shortVal {
		label = "or"
	} else {
		label = "and"
	}

	rhsBlock := cg.newBlock(label + ".rhs")
	mergeBlock := cg.newBlock(label + ".merge")

	if shortVal {
		// `A || B`: short-circuit to merge when A is true.
		leftEnd.NewCondBr(leftBool, mergeBlock, rhsBlock)
	} else {
		// `A && B`: short-circuit to merge when A is false.
		leftEnd.NewCondBr(leftBool, rhsBlock, mergeBlock)
	}

	cg.curBlock = rhsBlock

	right, err := cg.genExpr(rhsBlock, e.Right)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, right.Type()); err != nil {
		return nil, err
	}

	rightEnd := cg.curBlock
	rightBool := cg.toBool(rightEnd, right)
	rightEnd.NewBr(mergeBlock)

	var shortConst constant.Constant
	if shortVal {
		shortConst = constant.NewInt(irtypes.I1, 1)
	} else {
		shortConst = constant.NewInt(irtypes.I1, 0)
	}

	phi := mergeBlock.NewPhi(
		ir.NewIncoming(shortConst, leftEnd),
		ir.NewIncoming(rightBool, rightEnd),
	)
	cg.curBlock = mergeBlock

	return phi, nil
}
