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

// genTryExpr lowers `try expr` into:
//
//	let __t = expr
//	if __t.is_err():
//	  return __t.err_value()
//	__t.ok_value()
//
// The methods are looked up by the plain alias the impl registered
// (typename + "_" + method). Trait-qualified bodies are reachable
// through that alias for any type implementing tryable, since
// registerPlainMethodAliases surfaces them under the bare name.

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
	case *ast.SuffixCallExpr:
		return cg.genSuffixCall(block, e)

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
		// Pass the current returnTypeHint as the expected type so
		// elements can disambiguate ADT constructors against the
		// tuple's element types (set by genArgWithTargetType,
		// let-bindings with annotation, etc.).
		return cg.genTupleLit(block, e, cg.returnTypeHint)

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

	case *ast.MoveExpr:
		return cg.genMoveExpr(block, e)

	case *ast.RefExpr:
		return cg.genRefExpr(block, e)

	case *ast.TryExpr:
		return cg.genTryExpr(block, e)

	case *ast.AwaitExpr:
		// await expr -- evaluates e.Future, which must implement awaitable[t]
		// (typically `Future[t]`).  Strict: the operand's type must be an
		// awaitable; there is no auto-spawn-under-await sugar.  To wait on a
		// user-declared `fn{#async} f() T`, write `await spawn f(args)` --
		// `spawn` produces `Future[T]` and the await unwraps to T.
		//
		// Optimisations that fire under coro context (cg.inCoroFn == true):
		//   - Channel fast path: `await ch.recv()` / `await ch.send(v)` emit
		//     the inline channel direct op, bypassing the sync wrapper and
		//     its spawn.
		//   - Inline-drive: `await spawn fn(args)` where fn has a `$coro`
		//     variant drives the inner coroutine in the caller's own frame
		//     instead of allocating a fresh fiber.
		futureExpr := e.Future
		if cg.inCoroFn {
			// Channel fast path: `await ch.recv()` / `await ch.send(v)`
			// short-circuit the outer sync wrapper (which returns a
			// `Future[T]` constructed via `spawn this.{recv,send}_impl`)
			// and emit the inline channel op directly, returning T to
			// the caller's coro frame.
			if callNode, ok := e.Future.(*ast.CallExpr); ok {
				if result, ok2, driveErr := cg.tryChannelWrapperFastPath(block, callNode); ok2 {
					if driveErr != nil {
						return nil, driveErr
					}

					return result, nil
				}
			}

			// Inline-drive: `await spawn fn(args)` runs the inner coroutine
			// in this fiber's own frame without allocating a fresh fiber.
			// Gated on cg.stacktraceUsed: when the program reaches
			// stacktrace(), inline-drive collapses two spawn-chain levels
			// into one and breaks the parent-gen walk -- so we use the
			// real spawn path (which captures caller IP + parent pid+gen
			// at the spawn site) whenever stacktrace is observable.  In
			// stacktrace-free programs the optimisation is sound and
			// saves the fiber-frame allocation + scheduler handoff.
			if !cg.stacktraceUsed {
				if spawnNode, ok := e.Future.(*ast.SpawnExpr); ok {
					if callNode, ok2 := spawnNode.Call.(*ast.CallExpr); ok2 {
						result, driveErr := cg.genInlineAsyncDrive(block, callNode)
						if driveErr != nil {
							return nil, driveErr
						}

						if result != nil {
							return result, nil
						}
						// (nil, nil) -> callee $coro not in scope; fall through
						// to the standard spawn+await path below.
					}
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

			return nil, cg.nodeErr(e, "await: expression (type %s) does not implement awaitable[t]; use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] (e.g. fn f() Future[t] = spawn ...)",
				val.Type())
		}

		// The value must be a Future[T] struct.  Extract .pid field (field index 0).
		pidIdx := cg.fieldIndex(structName, "pid")
		if pidIdx < 0 {
			// Not a Future struct: lower `await x` against the
			// awaitable[t] trait as
			//
			//   loop:
			//     if x.ready(): break
			//     yield
			//   x.result()
			//
			// The runtime drives the spin loop, so user impls
			// only have to answer "ready?" and "result". If
			// either method is missing report which one - the
			// most common cause is forgetting to migrate from
			// the old single-method shape.
			readyName := structName + "_ready"
			resultName := structName + "_result"

			readyEntry, hasReady := cg.curScope.lookup(readyName)
			resultEntry, hasResult := cg.curScope.lookup(resultName)

			if hasReady && hasResult {
				if readyFn, rOk := readyEntry.val.(*ir.Func); rOk {
					if resultFn, sOk := resultEntry.val.(*ir.Func); sOk {
						// `await <name>` (Identifier / DerefExpr of a
						// named pointer) is a borrow of the outer scope's
						// binding -- the original binding's scope exit
						// will release it.  Only release inside the loop
						// when the futureExpr is a temp producer
						// (CallExpr, BinExpr concat, etc.) and no other
						// owner exists.
						release := !isCopyExpr(futureExpr)

						return cg.emitAwaitableLoop(block, val, readyFn, resultFn, release)
					}
				}
			}

			return nil, cg.nodeErr(e, "await: expression (type %q) does not implement awaitable[t] (need fn awaitable::ready and fn awaitable::result); use \"await spawn fn(args)\" to run fn as a fiber, or have the function return Future[t] directly", structName)
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
