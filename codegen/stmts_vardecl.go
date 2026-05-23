package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genVarDecl(block *ir.Block, s *ast.VarDecl) (*ir.Block, error) {
	// `let _ = expr`: evaluate expr as a statement and drop the result.
	// Without this short-circuit, two `let _ = call()` in the same scope
	// both bind to scope entry "_", and the second's scope.set silently
	// overwrites the first's alloca pointer; the first value's RC fields
	// are then unreachable for scope-exit release and leak.  Treating
	// the underscore form as a statement-level discard releases RC-
	// tracked values eagerly and walks struct values for their inner
	// RC fields, so every drop is balanced regardless of how many the
	// user writes back-to-back.  The new `discard <expr>` macro
	// expands to `let _ = <expr>`, so this also fixes every leak that
	// the macro would have inherited.
	if s.Name == "_" && s.Value != nil {
		cg.curBlock = block

		val, err := cg.genExpr(block, s.Value)
		if err != nil {
			return block, err
		}

		if cg.curBlock != block {
			block = cg.curBlock
		}

		if val != nil && !isVoidType(val.Type()) {
			cg.emitDiscardedValueRelease(block, val, s.Value)
		}

		return block, nil
	}
	// Top-level constants are preregistered in the preregister pass as direct
	// constant values. Skip re-emitting them as stack allocas.
	if s.IsConst {
		if e, ok := cg.curScope.lookup(s.Name); ok && !e.isAlloc {
			return block, nil
		}
	}

	var (
		llType irtypes.Type
		err    error
	)

	if s.Type != nil {
		llType, err = cg.tinTypeToLLVM(s.Type)
		if err != nil {
			return nil, err
		}
	}

	var initVal value.Value

	if s.Value != nil {
		cg.curBlock = block
		// Clear any stale lastSliceBase left over from a slice expr that
		// fired in a non-let context (e.g. `return xs[0:m]` inside a
		// callee's body).  Without this, the next genVarDecl would
		// retain/release a value from a foreign function's block,
		// producing a forward-reference IR.
		cg.lastSliceBase = nil
		// TupleLit: pass the declared type so fields get the right LLVM types.
		if tup, ok := s.Value.(*ast.TupleLit); ok && llType != nil {
			initVal, err = cg.genTupleLit(block, tup, llType)
		} else if fillLit, ok := s.Value.(*ast.ArrayFillLit); ok {
			if _, isStaticLLVM := llType.(*irtypes.ArrayType); isStaticLLVM {
				// Static fixed-size target: fill emitted after alloca creation.
				_ = fillLit // handled in post-alloca block below
			} else {
				initVal, err = cg.genArrayFillLit(block, fillLit)
			}
		} else if arrLit, ok := s.Value.(*ast.ArrayLit); ok && s.Type != nil {
			// ArrayLit with declared element type: coerce each element to the declared type.
			// Handles e.g. let fns [fn{#async}(i64) i64] = [double] where elements need wrapping.
			if _, isStaticLLVM := llType.(*irtypes.ArrayType); isStaticLLVM {
				// Static fixed-size target: fill emitted after alloca creation.
				_ = arrLit // handled in post-alloca block below
			} else {
				var targetElemType irtypes.Type
				if at, ok2 := s.Type.(*ast.ArrayType); ok2 && at.Elem != nil {
					targetElemType, _ = cg.tinTypeToLLVM(at.Elem)
				}

				initVal, err = cg.genArrayLitWithElemType(block, arrLit, targetElemType)
			}
		} else {
			// When an explicit type annotation is present, propagate it as a hint
			// so overload resolution can prefer the variant whose return type matches
			// (let binding type > concrete arg types > constant arg types).
			if llType != nil {
				cg.returnTypeHint = llType
			}
			// Recursive lambda support: if the RHS is a lambda with an
			// explicit retType, plumb the let-binding name through so
			// genLambdaExpr can pre-register a self-reference in the
			// lambda's body scope.  Required for `let fact = fn(n i64)
			// i64 = ... fact(n-1) ...`.  retType-less lambdas can't
			// recurse here because the signature isn't known up front.
			if lam, ok := s.Value.(*ast.LambdaExpr); ok && lam.RetType != nil && s.Name != "" && s.Name != "_" {
				cg.lambdaSelfName = s.Name
			}

			cg.maybeMarkCLayoutStackBind(s)

			initVal, err = cg.genExpr(block, s.Value)
			cg.returnTypeHint = nil
			cg.lambdaSelfName = ""
			cg.nextCLayoutStackBind = ""
		}

		if err != nil {
			return nil, err
		}
		// Pick up any block change from await/yield inside the init expression.
		if cg.curBlock != block {
			block = cg.curBlock
		}

		if llType == nil && initVal != nil {
			llType = initVal.Type()
		}
	}

	// Track whether we ever had a declared annotation. The struct-fallback
	// override below must NOT fire for user-declared `i64` -- only for
	// the implicit i64-fallback case where no annotation was given.
	hadDeclaredType := s.Type != nil

	if llType == nil {
		llType = irtypes.I64
	}

	// Generic-alias resolution: when the declared type is missing AND
	// the init value has a concrete struct type, use that. This handles
	// `let t = expr` where expr returns a Generic[T] resolved struct.
	// Skip when an explicit annotation is present -- the type-mismatch
	// check below should fire instead.
	if !hadDeclaredType && initVal != nil && llType.Equal(irtypes.I64) {
		if _, isStruct := initVal.Type().(*irtypes.StructType); isStruct {
			llType = initVal.Type()
		}
	}

	if block == nil {
		panic(fmt.Sprintf("genVarDecl: block is nil for var %q (llType=%v, curBlock=%v, curFn=%v)", s.Name, llType, cg.curBlock, cg.curFn))
	}

	// #no_copy enforcement: a let-binding cannot hold a value of a #no_copy
	// struct, since a subsequent reference would alias the underlying cell.
	// `*S` is fine -- pointer copies just retain.
	if name := cg.typeNameOf(llType); name != "" && cg.noCopyStructs[name] {
		return nil, cg.nodeErr(s,
			"%s is #no_copy: bind a *%s instead - value-form let aliases the cell and double-frees on scope exit",
			cg.diagStructName(name), cg.diagStructName(name))
	}

	// REPL mode: promote top-level `let` bindings in the cell function to LLVM
	// global variables so their values persist across subsequent cells.
	// Static-array fill/literal allocas are skipped (they need alloca semantics).
	isReplCellFn := cg.curFn != nil && (cg.curFn.Name() == cg.replCellFuncName ||
		cg.curFn.Name() == cg.replCellFuncName+"$coro")
	if cg.replMode && !s.IsConst && isReplCellFn {
		_, isStaticArray := llType.(*irtypes.ArrayType)

		if !isStaticArray {
			if llType == nil {
				llType = irtypes.I64
			}
			// Check the scope for a previous-cell external global first.
			if existing, ok := cg.curScope.lookup(s.Name); ok && existing.isGlobal {
				if g, ok2 := existing.val.(*ir.Global); ok2 && initVal != nil {
					initVal = cg.coerce(block, initVal, g.ContentType)
					cg.emitRetain(block, initVal)
					block.NewStore(initVal, g)
				}

				return block, nil
			}
			// Check the persistent cell-globals map so the $coro variant of the
			// cell function can find the global created by the non-coro variant,
			// even after the non-coro function scope was popped.
			if g, ok := cg.replCellGlobals[s.Name]; ok {
				cg.curScope.set(s.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(g.ContentType), isGlobal: true})

				if initVal != nil {
					initVal = cg.coerce(block, initVal, g.ContentType)
					cg.emitRetain(block, initVal)
					block.NewStore(initVal, g)
				}

				return block, nil
			}

			g := cg.mod.NewGlobal(s.Name, llType)
			g.Init = cg.zeroConstant(llType)
			isRC := isRCTrackedType(llType)
			cg.curScope.set(s.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRC, isGlobal: true})

			cg.replCellGlobals[s.Name] = g
			if initVal != nil {
				initVal = cg.coerce(block, initVal, llType)
				cg.emitRetain(block, initVal)
				block.NewStore(initVal, g)
			}

			cg.replNewGlobals = append(cg.replNewGlobals, ReplGlobal{Name: s.Name, TinType: s.Type, LLVMType: llType})

			return block, nil
		}
	}

	// Local variables are stack-allocated by default. When escape analysis
	// (cg.curFnEscapingVars) flagged this binding as having `&x` reach an
	// escape sink -- return, struct-field of escaping struct, *Trait coerce,
	// channel send, spawn arg, etc. -- heap-allocate it via _tin_rc_alloc
	// instead so &x is a stable pointer outliving the frame. entry.val
	// becomes the heap pointer directly (same LLVM type as a stack alloca:
	// `*T`), so every later `genLValue(Ident)` returns the heap pointer
	// without extra indirection. Scope-exit emits _tin_release on entry.val
	// (see emitScopeRelease's isEarlyHeap branch).
	earlyHeap := cg.curFnEscapingVars[s.Name]

	var alloca value.Value

	if earlyHeap {
		sz := cg.llvmSizeOf(block, llType)
		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		alloca = block.NewBitCast(heapI8, irtypes.NewPointer(llType))
		// Zero-init the heap block so reads of unwritten fields aren't
		// uninitialized (mirrors what alloca's caller relies on).
		block.NewStore(cg.zeroValue(llType), alloca)
	} else {
		// Hoist the alloca to function entry so a let-binding inside a
		// loop body doesn't grow the stack one slot per iteration.
		// LLVM-canonical: mem2reg promotes entry allocas to SSA
		// registers, and the slot is reused across iterations.
		alloca = cg.hoistAlloca(block, llType)
	}

	// Emit dbg.declare for debug builds. Stack allocas only -- heap-promoted
	// vars don't have an alloca to attach the dbg.declare intrinsic to.
	if stackAlloca, ok := alloca.(*ir.InstAlloca); ok {
		cg.emitDbgDeclare(block, stackAlloca, s.Name, s.Pos().Line, 0, s.Type, llType)
	}

	// isHeapOwned: this variable receives the return value of a heap-promoting
	// function (one that uses _tin_rc_alloc to return *T), or a &StructLit{} that
	// was RC-alloc'd inline.  Scope-exit performs a chain release rather than the
	// normal ARC release.
	isHeapOwned := false
	heapOwnedDepth := 0
	pointsToBorrowedStorage := false

	defer func() {
		cg.nextCLayoutStackBind = ""
		cg.curStructLitOuterIsLocal = false
	}()

	if callExpr, isCall := s.Value.(*ast.CallExpr); isCall {
		calleeName := ""

		switch fn := callExpr.Func.(type) {
		case *ast.Identifier:
			calleeName = fn.Name
		case *ast.ScopeAccess:
			calleeName = strings.Join(fn.Path, "__")
		case *ast.FieldAccess:
			// Static method call written with dot syntax -- `S.alloc(...)` or
			// `Generic[T].alloc(...)` (via FieldAccess on Identifier or
			// IndexExpr respectively). Resolve to the IR function name so
			// heapPromotingFns lookup matches.
			if name := cg.staticCallIRName(fn); name != "" {
				calleeName = name
			}
		}

		if calleeName != "" && llType != nil {
			// Check both the raw AST name and the scope-resolved IR name (e.g.
			// "parse_value" AST name vs "json__parse_value" IR name) so that
			// package-qualified functions are detected correctly.
			isHeapFn := cg.heapPromotingFns[calleeName]
			if !isHeapFn {
				if entry, ok := cg.curScope.lookup(calleeName); ok {
					if f, ok2 := entry.val.(*ir.Func); ok2 {
						isHeapFn = cg.heapPromotingFns[f.Name()]
					}
				}
			}
			// Resolve generic static method scope access, e.g. "tree_node[i64]__branch"
			// -> "tree_node__i64_branch" (the key stored in heapPromotingFns).
			if !isHeapFn {
				if sa, ok := callExpr.Func.(*ast.ScopeAccess); ok && len(sa.Path) >= 2 {
					baseName := sa.Path[0]
					last := sa.Path[len(sa.Path)-1]

					if i := strings.Index(baseName, "["); i >= 0 {
						typeParam := strings.TrimSuffix(baseName[i+1:], "]")
						base := baseName[:i]
						concreteName := base + "__" + strings.ReplaceAll(typeParam, ",", "__")
						concreteKey := concreteName + "_" + last
						isHeapFn = cg.heapPromotingFns[concreteKey]
					}
				}
			}

			if isHeapFn {
				depth := pointerChainDepth(llType)
				if depth > 0 {
					isHeapOwned = true
					heapOwnedDepth = depth
				}
			}
		}
	} else if addrOf, isAddrOf := s.Value.(*ast.AddressOfExpr); isAddrOf {
		isHeapAlloc := false

		if _, isStructLit := addrOf.Expr.(*ast.StructLit); isStructLit {
			isHeapAlloc = true
		} else if call, isCall := addrOf.Expr.(*ast.CallExpr); isCall {
			if id, ok := call.Func.(*ast.Identifier); ok && cg.isDataVariant(id.Name) {
				isHeapAlloc = true
			}
		}

		if isHeapAlloc && llType != nil {
			depth := pointerChainDepth(llType)
			if depth > 0 {
				isHeapOwned = true
				heapOwnedDepth = depth
			}
		}
		// `let q = &localIdent` -- propagate the source's storage class.
		// If the source binding lives on the stack (no TinRCHdr prefix), q
		// must NOT participate in RC retain/release ops when later coerced
		// to `*Trait` (see buildPtrToTraitBorrow).  Same applies to
		// module-level globals.  We deliberately leave this flag UNSET when
		// the source itself owns a heap reference -- the existing
		// isHeapOwned / isEarlyHeap pipeline handles that case correctly.
		if id, isIdent := addrOf.Expr.(*ast.Identifier); isIdent {
			if srcEntry, ok := cg.curScope.lookup(id.Name); ok && srcEntry != nil {
				if !srcEntry.isHeapOwned && !srcEntry.isEarlyHeap && !srcEntry.ownsPtrViaRetain {
					pointsToBorrowedStorage = true
				}
			}
		}
	} else if _, isAwait := s.Value.(*ast.AwaitExpr); isAwait && llType != nil {
		// `let c = await expr` where expr returns a *NamedStruct
		// always transfers RC ownership to the caller -- the producer
		// (channel/atomic/whatever) retained a slot, the await
		// dequeues + clears the slot, and the awaiter is now the
		// owner. Without this isHeapOwned flag the binding would skip
		// scope-exit release_ptr and leak the dequeued value.
		if pt, isPtr := llType.(*irtypes.PointerType); isPtr {
			if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					isHeapOwned = true
					heapOwnedDepth = pointerChainDepth(llType)
				}
			}
		}
	}

	isRC := isRCTrackedType(llType)

	// Static array initializers: fill the stack alloca directly.
	// Handles both [v; N] fill literals and [e0, e1, ...] literals when the
	// declared type is a fixed-size [T; N] array.
	if at, isStaticAt := llType.(*irtypes.ArrayType); isStaticAt && s.Value != nil {
		if fillLit, isFill := s.Value.(*ast.ArrayFillLit); isFill {
			fillVal, ferr := cg.genExpr(block, fillLit.Value)
			if ferr != nil {
				return nil, ferr
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			// Zero fill: use memset for efficiency.
			isZeroFill := false

			if ic, isConst := fillLit.Value.(*ast.IntLit); isConst && ic.Value == 0 {
				isZeroFill = true
			}

			if ic, isConst := fillLit.Value.(*ast.CharLit); isConst && ic.Value == '\000' {
				isZeroFill = true
			}

			if isZeroFill {
				elemBytes := llvmElemByteSize(at.ElemType)
				totalBytes := constant.NewInt(irtypes.I64, int64(at.Len)*elemBytes)
				dstPtr := block.NewBitCast(alloca, irtypes.I8Ptr)
				block.NewCall(cg.ensureMemset(), dstPtr,
					constant.NewInt(irtypes.I8, 0), totalBytes,
					constant.NewInt(irtypes.I1, 0))
			} else {
				fillCoerced := cg.coerce(block, fillVal, at.ElemType)
				for i := uint64(0); i < at.Len; i++ {
					gep := block.NewGetElementPtr(llType, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
					block.NewStore(fillCoerced, gep)
				}
			}
		} else if arrLit, isArr := s.Value.(*ast.ArrayLit); isArr {
			// Static array from element list: [e0, e1, ..., eN].
			for i, elem := range arrLit.Elems {
				v, verr := cg.genExpr(block, elem)
				if verr != nil {
					return nil, verr
				}

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}

				v = cg.coerce(block, v, at.ElemType)
				gep := block.NewGetElementPtr(llType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
				block.NewStore(v, gep)
			}

			// Zero-initialize any trailing elements beyond what was specified.
			if uint64(len(arrLit.Elems)) < at.Len {
				for i := uint64(len(arrLit.Elems)); i < at.Len; i++ {
					gep := block.NewGetElementPtr(llType, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
					block.NewStore(cg.zeroValue(at.ElemType), gep)
				}
			}
		}
		// Non-fill, non-ArrayLit static target: fall through to initVal path below.
	}

	// ownsIfaceData: trait-iface let-bindings always own a fresh
	// _tin_rc_alloc'd data ptr from coerceToTrait (both value-source and
	// pointer-source coerceToTrait branches heap-copy the source struct).
	// emitScopeRelease (runtime.go) uses the flag to emit the matching
	// _tin_release at scope exit so the iface storage is reclaimed.
	var ownsIfaceData bool
	// preCoerceSrcType captures initVal's type BEFORE coerce reshapes
	// it (e.g. boxing into `any`).  The borrow-classification check at
	// the bottom of this function needs the pre-coerce type to spot
	// implicit boxing -- `let x any = n` whose post-coerce initVal is
	// already `any` but whose source was something else.
	var preCoerceSrcType irtypes.Type

	if initVal != nil {
		// If the init value is an empty array literal `[]` (constant
		// `{null, 0}`) but the declared type is a typed fat array
		// `{T*, i64}`, use a properly-typed zero value.  Strings and
		// non-empty slices share the {i8*, i64} shape, so we discriminate
		// on the source value being a constant null-data struct -- the
		// LLVM artifact genArrayLit emits for the empty-literal form
		// only.  Real strings reach coerce/store-time checks unchanged
		// so the user gets a precise type-mismatch diagnostic.
		if !initVal.Type().Equal(llType) {
			if isFatArrayPtr(initVal.Type()) && isFatArrayPtr(llType) {
				if cs, ok := initVal.(*constant.Struct); ok && len(cs.Fields) == 3 {
					if _, isNull := cs.Fields[0].(*constant.Null); isNull {
						initVal = cg.zeroValue(llType)
					}
				}
			}
		}

		srcType := initVal.Type()
		preCoerceSrcType = srcType

		// A let-binding "owns its iface" when the declared type is the
		// trait fat-ptr shape (or pointer-to-it) and the source value
		// isn't already that exact shape -- meaning coerce will run
		// either coerceToTrait (value source) or buildPtrToTraitBorrow
		// (pointer source) and produce a freshly RC-allocated iface.
		// The pre-fix only checked the value-shape direction, leaving
		// the pointer-shape case (`let a *Animal = &Cat{...}`,
		// `let a *Animal = c` where c is *Cat) un-flagged so genVarDecl's
		// post-coerce retain branch over-counted the fresh iface and
		// leaked it.
		isPtrToIface := func(t irtypes.Type) bool {
			pt, ok := t.(*irtypes.PointerType)
			if !ok {
				return false
			}

			innerSt, ok2 := pt.ElemType.(*irtypes.StructType)
			if !ok2 {
				return false
			}

			return isTraitFatPtrShape(innerSt)
		}
		ownsIfaceData = (isTraitFatPtrShape(llType) && !isTraitFatPtrShape(srcType)) ||
			(isPtrToIface(llType) && !isPtrToIface(srcType))

		// Suppress coerceToTrait's deferred scope-exit release when this
		// let-binding will own the iface and emit its own release via
		// the scope entry's ownsIfaceData flag (see emitScopeRelease).
		prevSuppress := cg.suppressIfaceScopeRelease
		if ownsIfaceData {
			cg.suppressIfaceScopeRelease = true
		}

		cg.coerceLastErr = nil
		initVal = cg.coerce(block, initVal, llType)
		cg.suppressIfaceScopeRelease = prevSuppress

		// If coerce stashed a richer diagnostic (e.g. trait
		// pointer-receiver-vs-value-source rejection), surface that
		// instead of the generic type-mismatch fall-through.
		if cg.coerceLastErr != nil {
			return nil, cg.nodeErr(s, "%v", cg.coerceLastErr)
		}
		// Coerce returns the value unchanged when no conversion path applies;
		// guard NewStore so a real type mismatch produces a clean diagnostic
		// instead of a Go panic from llir's incompatible-operand check.
		if !initVal.Type().Equal(llType) {
			return nil, cg.nodeErr(s,
				"cannot assign value of type %s to %q (declared type %s)",
				cg.fmtArgType(initVal.Type()), s.Name, cg.fmtArgType(llType))
		}

		block.NewStore(initVal, alloca)

		// ARC: retain when copying from an existing variable (identifier).
		// emitRetain handles RC-tracked values (fat arrays, strings, any) and
		// named structs with RC-tracked fields, and is a no-op for everything else.
		//
		// EXCEPTION: if coerce just boxed a non-any value into `any`, the new
		// box block is a fresh _tin_rc_alloc (rc=1) - it is already owned, so
		// an extra retain would over-count and cause a leak.
		//
		// EXCEPTION: a bound method (FieldAccess -> genBoundMethod) or capturing
		// lambda allocates a fresh env via _tin_rc_alloc (rc=1). Retaining would
		// over-count: the single scope-exit release_closure must be the only decrement.
		isFreshFatFn := isFatFnPtr(llType) && cg.lastLambdaHadCaptures

		boxedToAny := isAnyType(llType) && !isAnyType(srcType)
		// Trait coercion already minted a fresh _tin_rc_alloc'd data ptr
		// (rc=1) inside coerceToTrait; the let-binding owns it via
		// ownsIfaceData. An emitRetain here would over-count and leak.
		freshIface := ownsIfaceData
		// Heap-promoted call return: the callee already returned rc=1
		// (the heap promotion itself), so retaining here would push it
		// to rc=2 while the scope exit only decrements once -- leaking
		// rc=1. The chain-release on isHeapOwned is the matching dec.
		freshHeapPromoted := isHeapOwned
		// `let s = expr as string/Trait/...` lowers to a coerce[T] call
		// returning rc=1; treating it as a borrow and retaining would
		// over-count.  Same logic as freshHeapPromoted but generalized
		// for any RC-tracked call result.
		freshCallResult := cg.isFreshCallResult(initVal)

		// `let v t = *(raw as *t)` where `raw : *void` -- ownership-
		// transfer load out of opaque scratch storage that the caller
		// frees immediately afterwards (sync::wait's get_result + free
		// sequence; channel.recv's per-thread buffer; ...).  The void-
		// ptr-cast tag tells us no other tracked binding still owns
		// these inner fields, so retaining here would leave a +1 with
		// no matching release.  Same exception the return-stmt path
		// already applies.
		derefOfRawVoid := cg.isDerefOfRawVoidPtrCast(s.Value)
		// `let s = a[lo..hi]` lowers via genIndexExpr to
		// genPtrRangeSlice, which always returns a freshly
		// `_tin_rc_alloc`'d owned buffer.  Without this exception,
		// isCopyExpr fires the genVarDecl retain and the binding's
		// scope-exit release leaves the buffer at rc=1 forever --
		// one leaked slice per binding.
		freshSliceExpr := isFreshSliceExpr(s.Value)
		if isCopyExpr(s.Value) && !boxedToAny && !cg.isFreshBytesAlloc(initVal) && !isFreshFatFn && !freshIface && !freshHeapPromoted && !freshCallResult && !derefOfRawVoid && !freshSliceExpr {
			// Owning-pointer borrow case: see emitOwningPtrRetainIfApplicable.
			// emitRetain skips bare *<struct> / *<iface> by design (param
			// borrow convention).  When the let-binding takes ownership of
			// such a value (e.g. `let r = h.iface_field`), bump the heap
			// block's RC explicitly so the binding's scope-exit release is
			// matched.
			//
			// Skip for AsExpr (`let back = widened as *dom`): the cast
			// extracts a raw pointer view from the source iface; the
			// cast result is a borrow whose lifetime is tied to the
			// source binding, NOT a new owning reference.  Retaining
			// here would unbalance the source binding's own scope-exit
			// release of the underlying heap block.
			_, isAs := s.Value.(*ast.AsExpr)
			// Only treat *TinStruct copy bindings as ownership
			// transfers when the source is an IndexExpr (array
			// element: `let p = items[idx]`). For FieldAccess /
			// Identifier / DerefExpr the source still owns the +1
			// RC for the binding's life, so retaining here would
			// over-count and the matching scope-exit release_ptr
			// would free a node the parent still references -
			// breaking LinkedList.get and similar field-chase
			// patterns. Iface ptrs continue to retain unconditionally
			// (emitOwningPtrRetainIfApplicable's iface arm).
			treatAsOwning := false

			if !isAs {
				if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
					treatAsOwning = true
				}

				if pt, isPtr := initVal.Type().(*irtypes.PointerType); isPtr {
					if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && isTraitFatPtrShape(innerSt) {
						treatAsOwning = true
					}
				}
			}

			// `let p = expr as *u8` / `as *char` / `as *void`: raw-
			// pointer view extracted from a fat-ptr or other owner.
			// emitRetain would route through _tin_retain_ptr (bumping
			// the underlying heap block's RC) but scope-exit skips
			// release_ptr for raw primitive pointers (elemNeedsRelease
			// returns false), so the +1 leaks.  Treat as a borrow
			// view: no retain, no release.
			//
			// Twin cases:
			//   - `let p = &s[i]` (AddressOf into IndexExpr) -- raw
			//     ptr aliasing into a fat-ptr's data.
			//   - `let p = *pp` (DerefExpr loading an inner pointer
			//     out of a `**T` heap chain) -- inner ptr is borrow
			//     view, pp's chain release frees the underlying block.
			// collectBorrowCandidates records the &s[i] shape but the
			// analyzer rejects whenever the source is a parameter,
			// capture, or other non-let binding, so currentFnBorrowSet
			// misses these.  Without an explicit exemption here the
			// emitRetain on the let-init leaves an unbalanced +1 on
			// the source's heap block.
			_, isAddrOf := s.Value.(*ast.AddressOfExpr)
			_, isDeref := s.Value.(*ast.DerefExpr)
			viewToRawPtr := (isAs || isAddrOf || isDeref) && isPrimitivePtr(initVal.Type())
			asToRawPtr := viewToRawPtr

			if treatAsOwning && cg.emitOwningPtrRetainIfApplicable(block, initVal) {
				// Tag the entry so emitScopeRelease's *TinStruct
				// release path knows this binding actually owns a
				// heap RC slot.  Plain `let p = &local_var` does
				// NOT come through here, so its scope exit skips
				// release (correct - there's no heap to release).
				cg.pendingOwnsPtrViaRetain = true
			} else if !cg.currentFnBorrowSet[s.Name] && !asToRawPtr {
				// Skip the entry retain for bindings the borrow
				// analyzer classified as Borrowed.  The scope-exit
				// release path checks the same classification (via
				// entry.ownership) and skips matching releases so
				// the pair stays balanced.
				cg.emitRetain(block, initVal)
			}
		}
	} else if s.Value == nil {
		// No initializer: zero-initialize.
		// For fixed-size arrays >= 128 bytes, use llvm.memset rather than
		// storing a huge aggregate constant: large aggregate value stores
		// (e.g. [65536 x i8] zeroinitializer) crash LLVM's instruction selector.
		if at, ok := llType.(*irtypes.ArrayType); ok {
			elemBytes := llvmElemByteSize(at.ElemType)
			if elemBytes > 0 && int64(at.Len)*elemBytes >= 128 {
				totalBytes := constant.NewInt(irtypes.I64, int64(at.Len)*elemBytes)
				dstPtr := block.NewBitCast(alloca, irtypes.I8Ptr)
				block.NewCall(cg.ensureMemset(), dstPtr,
					constant.NewInt(irtypes.I8, 0), totalBytes,
					constant.NewInt(irtypes.I1, 0))
			} else {
				block.NewStore(cg.zeroValue(llType), alloca)
			}
		} else {
			block.NewStore(cg.zeroValue(llType), alloca)
		}
	}
	// else: s.Value != nil && initVal == nil means a static-array fill was handled
	// directly above (ArrayFillLit or ArrayLit targeting [T; N] alloca).

	// Consume lastSliceBase: genSliceExpr sets it to the base allocation pointer
	// (before any GEP offset) so that ARC retain/release works on the real ARC
	// header rather than a possibly-interior fat-ptr field-0 pointer.
	// We read it here (after genExpr returns) so that any nested expression that
	// also calls genSliceExpr doesn't clobber our value.
	// genSliceExpr emits its own +1 retain on the base now, so we just
	// record the base pointer for scope-exit release -- the matching
	// retain is no longer required here.
	sliceBase := cg.lastSliceBase

	cg.lastSliceBase = nil

	// If there is already an RC-tracked variable with the same name in the CURRENT
	// (not parent) scope, release it before overwriting the entry.  This handles
	// re-declarations inside loop bodies (e.g. `for ...: let x = recv()`)
	// where the same name is declared on every iteration and the old value would
	// otherwise be orphaned.
	if existing, ok := cg.curScope.vars[s.Name]; ok && existing.isAlloc && existing.isRC {
		if existing.basePtr != nil {
			block.NewCall(cg.ensureRelease(), existing.basePtr)
		} else {
			existingPtrType, isPtrType := existing.val.Type().(*irtypes.PointerType)
			if isPtrType {
				oldVal := block.NewLoad(existingPtrType.ElemType, existing.val)
				if existing.noDeinit {
					cg.emitReleaseNoDeinit(block, oldVal)
				} else {
					cg.emitRelease(block, oldVal)
				}
			}
		}
	}

	// Non-capturing closures (null env): scope-exit would emit _tin_release_closure(null)
	// which is a no-op in the runtime.  Set noRelease to skip it entirely.
	// Also handles bound methods (FieldAccess -> genBoundMethod): they set
	// lastLambdaHadCaptures=true so we must not skip the scope-exit release.
	noReleaseClosureEnv := false

	if isFatFnPtr(llType) {
		_, isLambda := s.Value.(*ast.LambdaExpr)
		_, isBound := s.Value.(*ast.FieldAccess)

		if isLambda || isBound {
			noReleaseClosureEnv = !cg.lastLambdaHadCaptures
			cg.lastLambdaHadCaptures = false // consume
		}
	}

	// Determine the byte-array element kind: prefer the explicit declared type,
	// then fall back to the RHS AsExpr type (covers `let x = expr as [byte]`).
	bae := byteArrayElemType(s.Type)
	if bae == "" {
		if asExpr, ok := s.Value.(*ast.AsExpr); ok {
			bae = byteArrayElemType(asExpr.Type)
		}
	}

	// scalarTypeName covers 8-bit types ("char","byte","u8","i8") and 128-bit types
	// ("i128","u128","f128") for echo/interpolation dispatch.
	stn := scalar8BitTypeName(s.Type)
	if stn == "" {
		stn = scalar128BitTypeName(s.Type)
	}

	entry := &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, basePtr: sliceBase, isUnsigned: isUnsignedTinType(s.Type), byteArrayElem: bae, scalarTypeName: stn, isHeapOwned: isHeapOwned, heapOwnedDepth: heapOwnedDepth, noRelease: noReleaseClosureEnv, tinType: s.Type, ownsIfaceData: ownsIfaceData, isEarlyHeap: earlyHeap, ownsHeapIfaceData: cg.bindingOwnsHeapIfaceData(s), ownsHeapPromotedFields: cg.bindingHeapPromotedFields(s), declaredConst: s.IsConst, declaredLet: !s.IsConst, ownsPtrViaRetain: cg.pendingOwnsPtrViaRetain, pointsToBorrowedStorage: pointsToBorrowedStorage}

	// Tag the binding with its alias source when the initializer is a bare
	// identifier resolving to another fat-pointer binding.  Used by the
	// -Walias-mutation check at later indexed-write / `++=` sites.
	if src, ok := s.Value.(*ast.Identifier); ok && src != nil {
		t := alloca.Type().(*irtypes.PointerType).ElemType
		if isStringType(t) || isFatArrayPtr(t) {
			if srcEntry, ok2 := cg.curScope.lookup(src.Name); ok2 && srcEntry.isAlloc {
				entry.aliasedFromName = src.Name
			}
		}
	}

	cg.pendingOwnsPtrViaRetain = false

	// Capture the init expression for compile-time folding (codegen/fold.go).
	// Subsequent assignments to the same name clear constInitExpr in
	// genAssign / aug-assign so a mutated variable can never be folded.
	if isFoldableInitExpr(s.Value) {
		entry.constInitExpr = s.Value
	}

	// Record the literal length of array / string initializers so the
	// array-bounds checker can warn on out-of-range constant indices into
	// `let xs = [1, 2, 3]; xs[5]` and similar.
	switch v := s.Value.(type) {
	case *ast.ArrayLit:
		entry.staticArrayLen = int64(len(v.Elems))
	case *ast.ArrayFillLit:
		if v.Count >= 0 {
			entry.staticArrayLen = int64(v.Count)
		}
	case *ast.StringLit:
		entry.staticArrayLen = int64(len(v.Value))
	}

	entry.declPos = s.Pos()
	// Bindings the borrow analyzer flagged as borrow-safe get
	// classified as Borrowed.  emitScopeRelease reads this field to
	// decide whether to drop the scope-exit release.
	//
	// EXCEPTION: a let-binding whose coerce minted a fresh rc=1
	// allocation -- `let x any = n` (boxedToAny) or
	// `let m Trait = s` (ownsIfaceData).  The borrow analyzer's
	// "let target = source-Ident" shape misses the coercion-induced
	// allocation; flagging as Borrowed would suppress the
	// scope-exit release of the freshly-allocated block.
	coerceAllocates := false

	if preCoerceSrcType != nil {
		if isAnyType(llType) && !isAnyType(preCoerceSrcType) {
			coerceAllocates = true
		}

		if ownsIfaceData {
			coerceAllocates = true
		}
	}

	if cg.currentFnBorrowSet[s.Name] && !coerceAllocates {
		entry.ownership = ownershipBorrowed
	}

	cg.curScope.set(s.Name, entry)
	cg.warnIfBuiltinShadow("let", s.Name, s.Pos())
	// Record the binding for --explain-ownership: Borrowed for
	// analyzer-classified bindings, Owned otherwise.  No-op when
	// --explain-ownership is off.
	cg.recordOwnership(s.Name, entry.ownership, "")

	return block, nil
}

// isFoldableInitExpr returns true for AST shapes the constant folder in
// fold.go can handle. Used to decide whether to capture an init expr on
// the scope entry. Conservative: false is always safe (just disables
// folding for that binding).
func isFoldableInitExpr(n ast.Node) bool {
	switch e := n.(type) {
	case nil:
		return false
	case *ast.BoolLit, *ast.AtomLit, *ast.IntLit, *ast.TypeofExpr:
		return true
	case *ast.Identifier:
		return true
	case *ast.BinExpr:
		return isFoldableInitExpr(e.Left) && isFoldableInitExpr(e.Right)
	case *ast.UnaryExpr:
		return isFoldableInitExpr(e.Expr)
	}

	return false
}

// emitDefers emits all pending deferred calls in LIFO order into block.
// For each defer, it pops that single entry from the runtime chain before
// executing it inline.  This ensures that if a deferred call itself panics,
// the remaining (not-yet-run) defers are still in the chain and will be
// executed by _tin_panic.
//
// IMPORTANT: this function does NOT clear pendingDeferFnI8s.  Each return path in
// a function lives in its own basic block and independently emits the same set
// of defers.  Clearing here would cause the second (and later) return paths to
// see an empty list and silently skip their defers.  The list is naturally
// cleared when genFuncDeclAs restores the outer function's prevDefers state.
func (cg *CodeGen) emitDefers(block *ir.Block) error {
	n := len(cg.pendingDeferFnI8s)
	if n == 0 {
		return nil
	}
	// All thunks share the same signature: void(i8* env, i8* ret_slot).
	thunkFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr, irtypes.I8Ptr)

	retSlotArg := cg.curFnDeferRetAlloca
	if retSlotArg == nil {
		retSlotArg = constant.NewNull(irtypes.I8Ptr)
	}

	for i := n - 1; i >= 0; i-- {
		// Deregister this one entry before running it.
		if cg.deferPopFn != nil {
			block.NewCall(cg.deferPopFn, constant.NewInt(irtypes.I64, 1))
		}
		// Call the compiled thunk directly with its captured env.
		// This is correct for both plain-call defers and lambda defers because
		// the thunk captures all free variables by reference (alloca pointer),
		// and the allocas remain live until the enclosing function returns.
		fnI8 := cg.pendingDeferFnI8s[i]
		env := cg.pendingDeferEnvs[i]
		thunkFnPtr := block.NewBitCast(fnI8, irtypes.NewPointer(thunkFnType))
		block.NewCall(thunkFnPtr, env, retSlotArg)
		// Free the heap env that was malloc'd for the thunk.
		// Skip the null sentinel emitted when there were no captures.
		if _, isNull := env.(*constant.Null); !isNull {
			block.NewCall(cg.ensureFree(), env)
		}
	}

	return nil
}

// maybeMarkCLayoutStackBind sets cg.nextCLayoutStackBind when the let-binding's
// initializer is a direct CallExpr to a cLayoutStruct-value-returning wrapper
// AND the binding does not escape the enclosing function frame.  Called just
// before the initializer is emitted via genExpr so allocCLayoutReturnBuffer
// (consulted by genCallExpr while the wrapper call is being lowered) sees
// the flag and stack-allocates the out_native buffer with the IMMORTAL_RC
// sentinel.
func (cg *CodeGen) maybeMarkCLayoutStackBind(s *ast.VarDecl) {
	// Defensive: a previous let-decl's defer should have cleared the flag,
	// but if something interrupted it (panic in a sub-genExpr, early
	// return up the stack), stale state would silently apply to this
	// binding.  Reset before deciding.
	cg.nextCLayoutStackBind = ""
	cg.curStructLitOuterIsLocal = false

	if s == nil || s.Name == "" || s.Value == nil {
		return
	}

	// Peel through `as T` / `T.(type)` so `let d = c_call(...) as dyad`
	// still reaches the wrapper-call lookup.  The cast is a no-op at this
	// level -- the underlying value is the cLayoutStruct value we're
	// considering for stack-bind.
	peeled := peelTypeWrappers(s.Value)

	// `let h = Holder{field: c_call(...)}` -- the inner cLayout call's
	// result is stored into a field of h.  If h doesn't escape, the
	// inner call's stack composite can live in the same caller frame as
	// h's alloca; genStructLit will pick up the flag and stack-bind the
	// field's wrapper call.  Holder itself doesn't need any special
	// handling -- it's a regular Tin struct laid out by-value.
	//
	// Pass Holder's name to the escape walker so it treats reads of
	// `h.<cLayoutField>` as escape: those reads pull a cLayoutStruct
	// value out of h, and that value's c_data_ptr still points into h's
	// composite storage -- letting it flow into another binding / return
	// / call-arg would leak the storage reference past h's stack frame.
	if sl, isStructLit := peeled.(*ast.StructLit); isStructLit {
		body := cg.currentFnAstBody()
		if body == nil {
			return
		}

		// If the OUTER struct itself has any *this-receiver method,
		// calling `h.someMethod()` materializes `&h` -- and the method
		// body could store that pointer somewhere persistent, escaping
		// h's storage (and any cLayout fields embedded in it) past the
		// caller's frame.  The walker treats `h.someMethod()` as a
		// receiver-position FieldAccess (method name isn't a struct
		// field), so it doesn't catch this on its own.  Heap-allocate
		// the inner cLayout fields in that case.
		if cg.structHasPointerReceiverMethod(sl.TypeName) {
			return
		}

		if cg.cLayoutBindingEscapesForType(s.Name, sl.TypeName, body) {
			return
		}

		cg.curStructLitOuterIsLocal = true

		return
	}

	if _, isCall := peeled.(*ast.CallExpr); !isCall {
		return
	}

	structName := cg.resolveCLayoutWrapperCall(peeled)
	if structName == "" {
		return
	}

	// Pointer-receiver methods force heap mode: if any method on the
	// struct takes `this *S`, calling that method on the binding does
	// `&binding` under the hood.  A stack composite can't survive past
	// the callee's view, so we must heap-allocate the storage.
	if cg.structHasPointerReceiverMethod(structName) {
		return
	}

	body := cg.currentFnAstBody()
	if body == nil {
		return
	}

	// Pass the binding's cLayoutStruct name to the walker so that any
	// (currently hypothetical) nested cLayoutStruct fields trigger the
	// FieldAccess-of-cLayout-field escape rule.  Today's cLayoutStructs
	// only carry primitives, so the structName argument is functionally
	// no-op, but it's the right shape if cLayout-in-cLayout becomes a
	// thing later.
	if cg.cLayoutBindingEscapesForType(s.Name, structName, body) {
		return
	}

	cg.nextCLayoutStackBind = structName
}
