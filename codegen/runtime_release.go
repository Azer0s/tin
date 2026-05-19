package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) emitCallArgReleaseForRet(block *ir.Block, astArg ast.Node, pre, post value.Value, retType irtypes.Type) {
	if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
		cg.emitRelease(block, post)

		return
	}

	// Trait-coerce / fresh-iface call argument: the parameter slot is
	// `*Trait_iface` and the value flowing in is a freshly allocated
	// iface block (heap-allocated by buildPtrToTraitBorrow or returned
	// fresh from a callee that itself returns *Trait).  Without
	// releasing the block after the callee returns, the iface leaks
	// because *Trait_iface pointers are not classified as
	// rc-tracked-leading-ptr -- isRCTrackedType / isFreshCallResult
	// match on the iface STRUCT type, not the *iface_struct pointer
	// shape we get when a fn returns or accepts a `*Trait`.
	//
	// Two arrival shapes trigger the release:
	//
	//   1. Call-site implicit coerce: pre's type is *Struct (or other
	//      non-iface pointer), post's type is *Trait_iface.
	//      buildPtrToTraitBorrow allocated the iface on the heap.
	//   2. Same-iface-type but pre is a fresh SSA producer (an InstCall
	//      returning *Trait, an InstBitCast of an _tin_rc_alloc, ...) --
	//      i.e. NOT a load from a binding's alloca.  Loads from
	//      bindings denote borrows; the binding's own scope-exit
	//      release handles the iface.
	if cg.isTraitFatPtrPtrType(post.Type()) {
		if !cg.isTraitFatPtrPtrType(pre.Type()) {
			cg.emitRelease(block, post)

			return
		}
		// Same iface type: distinguish fresh allocation from a
		// borrow.  Loads of an *iface pointer carry the borrow
		// semantics; anything else (call result, bitcast of
		// rc_alloc, ...) is fresh and must be released here.
		if _, isLoad := pre.(*ir.InstLoad); !isLoad {
			cg.emitRelease(block, post)

			return
		}
	}

	// `expr as Trait/string/...` lowers to a coerce[T] call returning
	// rc=1; isCopyExpr returns true for the AsExpr (it walks through
	// to the inner identifier), but the cast result is a fresh
	// allocation, not a borrow.  Without this exemption the call-arg
	// release is skipped, leaking the cast result on every call site.
	if isFreshCallResult(pre) {
		cg.emitRelease(block, pre)

		return
	}

	if isCopyExpr(astArg) {
		return
	}

	if isRCTrackedType(pre.Type()) {
		// Lambda temporaries passed as arguments: defer the release to scope exit
		// rather than immediately after the call. This keeps the closure env alive
		// for the duration of the enclosing scope, which is necessary when a C
		// function stashes the fat-fn pointer (C doesn't participate in ARC, so it
		// cannot increment the RC; if we release here the env is freed before any
		// subsequent call through the stashed pointer).
		if _, isLambda := astArg.(*ast.LambdaExpr); isLambda && isFatFnPtr(pre.Type()) && cg.curScope != nil {
			alloca := block.NewAlloca(pre.Type())
			block.NewStore(pre, alloca)

			name := fmt.Sprintf(".tmpfn_%d", cg.strCount)
			cg.strCount++
			cg.curScope.set(name, &scopeEntry{val: alloca, isAlloc: true, isRC: true})

			return
		}

		cg.emitRelease(block, pre)

		return
	}
	// *TinStruct pointer: only release if this is a heap temporary (e.g. the
	// result of a function call passed directly as an argument). Do NOT release
	// for &stackVar arguments - those are borrows, not ownership transfers, and
	// reading 8 bytes before a stack alloca as a TinRCHdr is invalid.
	if isTemporaryProducer(astArg) {
		if pt, ok := pre.Type().(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					cg.emitRelease(block, pre)
				}
			}
		}
		// ADT-by-value rvalue (e.g. `is_err(make())` where make
		// returns Result by value): the callee's entry retain +
		// epilogue release nets to zero, but the rvalue itself
		// still owns rc=1 of any heap-allocated active-variant
		// fields (strings, byte slices, freshly-coerced ifaces,
		// rc::Cell pointers).  Release here pairs with the match-
		// site `transferredFromBorrow` retain inside the callee:
		// the callee retains the extracted field for the caller's
		// receiving binding, the caller decrements the rvalue's
		// own share, and the net rc seen by the receiving binding
		// is exactly 1 -- as if the rvalue had been moved.
		if cg.isDataType(pre.Type()) {
			cg.emitDataValueRelease(block, pre)
		}
	}
}

// isCopyExpr returns true when an AST expression produces a reference to
// existing heap data rather than a fresh allocation.  The caller must
// retain the result before storing it in a new alloca.
func isCopyExpr(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.Identifier:
		// Named variable - its scope entry owns the RC reference.
		return true
	case *ast.FieldAccess:
		// Borrowing a field from a struct - the struct retains ownership.
		// Do not release after use; the struct's scope release handles it.
		return true
	case *ast.IndexExpr:
		// Borrowing an element from an array - the array retains ownership.
		return true
	case *ast.AsExpr:
		// Casting to `any` boxes the source value into a fresh _tin_rc_alloc.
		// The resulting any value is a NEW allocation, not a copy of an existing
		// reference.  Treating it as a copy would emit an extra retain (RC=2) that
		// the single scope-exit release cannot balance, causing a leak.
		if st, ok2 := n.Type.(*ast.SimpleType); ok2 && st.Name == "any" {
			return false
		}
		// Other casts (sv as string, n as i64, etc.) do not allocate new memory;
		// the underlying value still owns the RC reference.  Propagate through so
		// that `sv as string` (where sv is a named variable) is treated as a copy
		// and not released after a call.  Without this, the ARC release loop would
		// drop the RC after the callee returns AND scope-exit would drop it again,
		// double-freeing the underlying string.
		return isCopyExpr(n.Expr)
	case *ast.DerefExpr:
		// Dereferencing a named pointer variable (*ptr): the pointee still owns
		// the RC references for all inner fields.  Retain so the caller gets an
		// independent copy; without a retain, both the new variable and the
		// original pointee would release the same inner RC pointers (double-free).
		//
		// Dereferencing a temporary (*call()): genDerefExpr freed the outer RC block
		// immediately after loading, transferring sole ownership of the inner fields
		// to the loaded value.  No retain is needed - the loaded value already owns
		// its fields at RC=1.  Retaining here would cause RC=2 with only one release,
		// leaking the inner allocations.
		return !isTemporaryProducer(n.Expr)
	}

	return false
}

// isDerefOfRawVoidPtrCast reports whether node is `*(ident as *T)` where
// `ident` was declared `*void` -- i.e. a load through a raw foreign-memory
// pointer.  The pattern marks "move out of opaque scratch storage" (the
// stdlib channel.recv loads from a per-thread buffer the channel transferred
// RC into); the regular copy-expr retain rule must skip it because no scope
// cleanup will balance the +1.
//
// Conservative on shape: only matches `*(<ident> as *T)`.  A future expansion
// could descend through nested AsExprs, but the recv() pattern is the only
// known caller and a tighter check minimizes the chance of accidentally
// skipping a retain we genuinely need.
func (cg *CodeGen) isDerefOfRawVoidPtrCast(node ast.Node) bool {
	de, ok := node.(*ast.DerefExpr)
	if !ok {
		return false
	}

	as, ok := de.Expr.(*ast.AsExpr)
	if !ok {
		return false
	}

	if _, isPtrTarget := as.Type.(*ast.PointerType); !isPtrTarget {
		return false
	}

	id, ok := as.Expr.(*ast.Identifier)
	if !ok {
		return false
	}

	if cg.curScope == nil {
		return false
	}

	entry, ok := cg.curScope.lookup(id.Name)
	if !ok || entry.val == nil {
		return false
	}

	pt, ok := entry.val.Type().(*irtypes.PointerType)
	if !ok {
		return false
	}

	innerPt, ok := pt.ElemType.(*irtypes.PointerType)
	if !ok {
		return false
	}

	// *void lowers to `i8*` in LLVM (no named element type).
	if it, isInt := innerPt.ElemType.(*irtypes.IntType); !isInt || it.BitSize != 8 {
		return false
	}

	return true
}

// isTemporaryProducer returns true when an expression is known to return a
// freshly heap-allocated RC-tracked value (rc = 1) that the caller owns.
// Used to release intermediates that are never stored in a named variable.
//
// Covered cases:
//   - CallExpr:           function may return a heap-allocated RC value
//   - BinExpr("++"):      string/array concat always creates a fresh allocation
//   - InterpolatedString: snprintf result is _tin_rc_alloc'd
//   - ArrayLit:           non-empty array literal is _tin_rc_alloc'd
func isTemporaryProducer(node ast.Node) bool {
	if _, ok := node.(*ast.CallExpr); ok {
		return true
	}

	if be, ok := node.(*ast.BinExpr); ok {
		return be.Op == "++"
	}

	if _, ok := node.(*ast.InterpolatedString); ok {
		return true
	}

	if al, ok := node.(*ast.ArrayLit); ok {
		return len(al.Elems) > 0 // empty [] has no heap block
	}

	return false
}

// isFreshBytesAlloc returns true when v is the direct result of _tin_bytes_from_buf.
// Such strings already carry RC=1 (freshly allocated, not borrowed from any
// existing variable) and must NOT receive an extra retain at assignment or return
// sites.  An extra retain would raise the RC to 2 while only one release is ever
// emitted, causing a permanent leak.
func isFreshBytesAlloc(v value.Value) bool {
	call, ok := v.(*ir.InstCall)
	if !ok {
		return false
	}

	fn, ok2 := call.Callee.(*ir.Func)
	if !ok2 {
		return false
	}

	return fn.Name() == "_tin_bytes_from_buf"
}

// isFreshCallResult returns true when v is the direct result of a
// function call whose return value is a freshly RC-allocated value.
// Used at return-site retain decisions: a `return expr` where expr
// resolved to a call (e.g. `n as string` lowering to coerce[string])
// already gives the caller rc=1 ownership; an extra retain would
// raise rc to 2 and leak.  Conservatively true for any call returning
// an RC-tracked type (string/fat-array/iface/any/struct ptr).
func isFreshCallResult(v value.Value) bool {
	call, ok := v.(*ir.InstCall)
	if !ok {
		return false
	}

	rt := call.Type()

	return isRCTrackedType(rt)
}

// isFreshSliceExpr reports whether expr is a range-slice `a[lo..hi]`
// (an IndexExpr whose Index is a BinExpr with op ".."), optionally
// wrapped in an `as T` coerce.  genPtrRangeSlice always returns a
// freshly `_tin_rc_alloc`'d owned buffer for these; an extra retain
// at the let-binding or return-stmt site would raise rc to 2 with
// only one matching release, leaking one fat-array header per
// evaluation.
//
// AsExpr wrapping matters because `let s = buf[0..n] as string` is
// the dominant shape across the stdlib (read_file et al.): the
// coerce reuses the slice's data buffer in-place, so freshness is
// preserved through the cast.
func isFreshSliceExpr(n ast.Node) bool {
	if as, ok := n.(*ast.AsExpr); ok {
		n = as.Expr
	}

	ix, ok := n.(*ast.IndexExpr)
	if !ok {
		return false
	}

	bin, ok2 := ix.Index.(*ast.BinExpr)

	return ok2 && bin.Op == ".."
}

// elemNeedsRelease reports whether a scope variable with element type elemType
// requires any ARC or deinit processing at scope exit.  Returns false for
// primitive types (int, float, raw pointers) and for named structs with no RC
// fields, no deinit, and no nested structs - so emitScopeRelease can skip the
// load entirely rather than loading and then emitting nothing.
func (cg *CodeGen) elemNeedsRelease(elemType irtypes.Type) bool {
	switch t := elemType.(type) {
	case *irtypes.IntType, *irtypes.FloatType, *irtypes.PointerType:
		// Pointer types (*T) are raw addresses; the pointed-to value is released
		// by its owner, not by every scope that borrows the pointer.
		return false
	case *irtypes.ArrayType:
		// Fixed-size arrays [T; N] are value types: the array itself isn't
		// RC-tracked, but each slot carries a copy of T.  If T is releasable
		// (e.g. [errors::Err; 4] or [string; N]), the slots own heap blocks
		// that the array's scope-exit must release.
		return cg.elemNeedsRelease(t.ElemType)
	}
	// RC-tracked fat types (strings, arrays, closures, any): always need release.
	if isRCTrackedType(elemType) {
		return true
	}
	// Named struct: need release only if it has a deinit, RC fields, or nested structs.
	structName := cg.typeNameOf(elemType)
	if structName == "" {
		return true // anonymous struct (e.g. from external code) - be conservative
	}

	// ADT value: tag-dispatched release walks the active variant's payload.
	// Needs a release if any variant carries an owning/ARC field.
	if variants, ok := cg.dataVariants[structName]; ok {
		for _, vi := range variants {
			if cg.variantHasReleasableField(vi) {
				return true
			}
		}

		return false
	}

	if cg.curScope != nil {
		if _, hasDeinit := cg.curScope.lookup(structName + "_deinit"); hasDeinit {
			return true
		}
	}

	for _, ft := range cg.structFieldLLVMTypes[structName] {
		if isRCTrackedType(ft) {
			return true
		}

		if _, isNested := ft.(*irtypes.StructType); isNested {
			return true // may contain RC fields deeper in
		}

		// Inline `[T; N]` field whose element type owns RC: the struct's
		// per-struct release helper must walk it, so the enclosing scope
		// can't short-circuit.
		if at, isArr := ft.(*irtypes.ArrayType); isArr && cg.elemNeedsRelease(at.ElemType) {
			return true
		}

		// Owning pointer to a known Tin struct OR a trait fat-ptr iface
		// struct: needs recursive release.  Iface structs live in
		// cg.traitFatPtrTypes (not cg.structTypes), so detect them via
		// shape -- otherwise an outer struct holding a *Trait field
		// would be considered "no-release" and emitAllScopeReleases
		// would skip its scope-exit cleanup, leaking the iface block
		// (and any RC sub-fields the iface dtor would have torn down).
		if pt, ok := ft.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					return true
				}

				if isTraitFatPtrShape(innerSt) {
					return true
				}
			}
		}
	}

	return false
}

// extractRCDataPtr extracts the ARC heap data pointer (i8*) from a
// string, fat-array, or any value.  Returns nil for non-ARC types.
