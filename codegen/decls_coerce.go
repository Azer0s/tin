package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkTraitDefaultMutationForms(td *ast.TraitDecl) error {
	if td == nil {
		return nil
	}

	forwardFieldNames := map[string]bool{}
	for _, ff := range td.ForwardFields {
		forwardFieldNames[ff.Name] = true
	}

	if len(forwardFieldNames) == 0 {
		return nil
	}

	for _, m := range td.Methods {
		if m.IsVirtual || m.Body == nil {
			continue
		}

		if len(m.Params) == 0 || m.Params[0].Name != "this" {
			continue
		}

		if _, isPtr := m.Params[0].Type.(*ast.PointerType); isPtr {
			continue // pointer receiver: mutation through it is fine
		}

		mutatedField := firstAssignedForwardField(m.Body, forwardFieldNames)
		if mutatedField == "" {
			continue
		}

		return cg.nodeErr(m,
			"trait %s: default method %s has a value receiver (`this %s`) "+
				"but its body assigns to forward field `%s`; that mutation would "+
				"land on a copy.  Declare the receiver as `this *%s` so the "+
				"default body and every implementing struct share a pointer "+
				"receiver",
			td.Name, m.Name, td.Name, mutatedField, td.Name)
	}

	return nil
}

// firstAssignedForwardField walks body and returns the name of the
// first forward-field assignment target it finds (`this.<name> = ...`
// or augmented-assign / postfix), or "" when nothing matches.
func firstAssignedForwardField(body ast.Node, forwardFieldNames map[string]bool) string {
	var found string

	walkAST(body, func(n ast.Node) {
		if found != "" {
			return
		}

		switch s := n.(type) {
		case *ast.AssignStmt:
			if name := forwardFieldTarget(s.Target, forwardFieldNames); name != "" {
				found = name
			}
		case *ast.AugAssignStmt:
			if name := forwardFieldTarget(s.Target, forwardFieldNames); name != "" {
				found = name
			}
		case *ast.PostfixStmt:
			if name := forwardFieldTarget(s.Expr, forwardFieldNames); name != "" {
				found = name
			}
		}
	})

	return found
}

// forwardFieldTarget returns the field name when target is a
// `this.<name>` field access whose `<name>` matches a forward field.
// Returns "" otherwise.
func forwardFieldTarget(target ast.Node, forwardFieldNames map[string]bool) string {
	fa, ok := target.(*ast.FieldAccess)
	if !ok {
		return ""
	}

	id, ok := fa.Expr.(*ast.Identifier)
	if !ok || id.Name != "this" {
		return ""
	}

	if forwardFieldNames[fa.Field] {
		return fa.Field
	}

	return ""
}

// pointerProvenanceIsRCAlloc walks back through bitcasts, GEPs, and
// load instructions to find the originating call that produced the
// pointer.  Returns true when the originator is `_tin_rc_alloc` (or
// a similar Tin RC allocator wrapper).  Used by coerceToTrait's
// pointer-source aliasing path to decide whether the source has an
// ARC header that retain/release can safely touch.  External /
// raw-pointer sources (mem::malloc, FFI returns, casts of i64-as-
// pointer) return false so the iface uses the borrow vtable instead.
func (cg *CodeGen) pointerProvenanceIsRCAlloc(v value.Value) bool {
	for i := 0; i < 32; i++ {
		switch n := v.(type) {
		case *ir.InstBitCast:
			v = n.From
		case *ir.InstLoad:
			pt, ok := n.Src.Type().(*irtypes.PointerType)
			if !ok {
				return false
			}

			if alloca, isAlloca := n.Src.(*ir.InstAlloca); isAlloca {
				_ = pt
				_ = alloca
			}
			// A loaded *T came from somewhere -- usually a let
			// binding's alloca.  We can't trace the binding's
			// stored value without scope info, so be conservative:
			// don't claim RC.  The let-binding's own coerce sites
			// would have set the alloca through a fresh `&T{...}`
			// path, but proving that here is out of scope.
			return false
		case *ir.InstGetElementPtr:
			v = n.Src
		case *ir.InstCall:
			calleeName := ""
			if f, ok := n.Callee.(*ir.Func); ok {
				calleeName = f.Name()
			}

			switch calleeName {
			case "_tin_rc_alloc", "_tin_rc_alloc_atomic", "_tin_rc_alloc_typed":
				return true
			}

			return false
		default:
			return false
		}
	}

	return false
}

// traitDisplayName renders an instKey ("Reader", "Awaitable__i64") as
// the user-visible source form ("Reader", "Awaitable[i64]"). Same
// shape as prettyStructName but kept separate because traits use a
// different mangling pipeline.
func (cg *CodeGen) traitDisplayName(instKey string) string {
	return cg.diagStructName(instKey)
}

// buildPtrToTraitBorrow lowers `let a *Trait = &b` (or any other coerce
// where target is `*FatPtr` and source is `*Struct`). Builds a stack-
// temporary fat ptr `{cast(structPtr, i8*), vtable}` and returns its
// address -- a true borrow, mutations via *a propagate to *structPtr.
//
// Returns nil when the struct doesn't implement the trait or the fat-
// ptr type isn't registered; the caller falls through to the generic
// coerce path (which currently emits a wrong bitcast -- that's the
// fallback we're trying to replace).
//
// Lifetime: stack-temp lives for the enclosing function frame. Same
// gotcha as returning `*T` of any local -- Tin does not statically
// prevent it, but it doesn't catch fire on common in-frame uses
// (which is what the user's `let a *T = &b; (*a).foo(); echo b` test
// exercises).
func (cg *CodeGen) buildPtrToTraitBorrow(block *ir.Block, structPtr value.Value, traitName string, fatPtrType irtypes.Type) value.Value {
	pt, ok := structPtr.Type().(*irtypes.PointerType)
	if !ok {
		return nil
	}

	structName := cg.typeNameOf(pt.ElemType)
	if structName == "" {
		return nil
	}

	vtableKey := structName + "__" + traitName

	vtableGlobal, ok := cg.traitVtableGlobals[vtableKey]
	if !ok {
		return nil
	}

	fatPtrSt, ok := fatPtrType.(*irtypes.StructType)
	if !ok {
		return nil
	}

	// Is the source a stack alloca (or a bitcast/GEP rooted in one)?  If
	// so, there is no RC header at (data - 16) -- the iface's standard
	// data-release thunk would treat the stack bytes as a TinRCHdr and
	// read uninit memory before the alloca, then conditionally free a
	// stack pointer.  Swap in the borrow vtable (no-op release) and skip
	// the balancing retain, since borrows neither own nor decrement.
	isStackBorrow := isStackAllocaRoot(structPtr)
	// Indirect borrow: `let q = &b; let a *Trait = q`.  structPtr is a
	// Load whose source alloca was initialized with the address of a
	// stack/global binding (we recorded this via pointsToBorrowedStorage
	// at the let-decl site).  The loaded pointer still names a stack /
	// global cell, not an RC block, so apply the same borrow-vtable
	// treatment.
	if !isStackBorrow {
		if loadInst, isLoad := structPtr.(*ir.InstLoad); isLoad {
			if cg.sourceBindingPointsToBorrowedStorage(loadInst.Src) {
				isStackBorrow = true
			}
		}
	}

	// The fat ptr struct lives on the heap so a `*Trait` value can outlive
	// the frame that built it (e.g. returned, sent down a channel, captured
	// by an escaping closure). Stack-allocating here would dangle on every
	// such use; the few extra bytes per coerce are noise. data field points
	// at the source struct directly (no copy of the underlying value).
	sz := cg.llvmSizeOf(block, fatPtrSt)
	heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
	temp := block.NewBitCast(heapI8, irtypes.NewPointer(fatPtrSt))
	// Record that the current function returns a *Trait whose data is an
	// escape-promoted heap block. The flag rides up to callers via
	// fnReturnsOwningIface so their let-binding releases data on drop.
	// (We don't have the source AST here, so the most conservative
	// option is "every buildPtrToTraitBorrow caller in a fn with any
	// escaping var owns its iface data" -- which is the common case.)
	//
	// A borrow iface owns nothing past the iface block itself, so the
	// owning-iface signal does not apply to it.
	if !isStackBorrow && cg.curFn != nil && len(cg.curFnEscapingVars) > 0 {
		cg.fnReturnsOwningIface[cg.curFn.Name()] = true
	}

	dataGEP := block.NewGetElementPtr(fatPtrSt, temp,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dataI8 := block.NewBitCast(structPtr, irtypes.I8Ptr)
	block.NewStore(dataI8, dataGEP)

	// Retain the data when the source comes from a binding whose own
	// scope-exit release will fire and decrement the data's RC.
	// Without this retain, the iface's release_ptr (via the vtable's
	// data-release thunk) and the source binding's release would both
	// decrement -- a double-free.
	//
	// Skip the retain when:
	//   - the source is a stack alloca (no RC header to touch and the
	//     borrow vtable's release will not decrement either)
	//   - the source binding is early-heap-promoted (its scope-exit
	//     release is SUPPRESSED -- only the iface would release, and
	//     a retain here would strand a +1 reference)
	//   - the caller set cg.coerceTransfersSource (e.g. genReturn
	//     using retSkipName to transfer ownership; same suppression)
	if !isStackBorrow {
		if loadInst, isLoad := structPtr.(*ir.InstLoad); isLoad {
			if !cg.coerceTransfersSource && !cg.sourceBindingIsEarlyHeap(loadInst.Src) {
				block.NewCall(cg.ensureRetain(), dataI8)
			}
		}
	}

	vtableGEP := block.NewGetElementPtr(fatPtrSt, temp,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	chosenVtable := value.Value(vtableGlobal)

	if isStackBorrow {
		if borrow := cg.ensureTraitBorrowVtable(vtableKey); borrow != nil {
			chosenVtable = borrow
		}
	}

	block.NewStore(chosenVtable, vtableGEP)

	return temp
}

// isStackAllocaRoot reports whether v ultimately points at an LLVM
// alloca in the current frame OR an LLVM global -- i.e. the source has
// no TinRCHdr prefix and must not be touched by iface release/retain
// paths.  Walks through bitcasts, zero-offset GEPs, and constant-expr
// wrappers since structPtr is often a downcast of the originating
// alloca rather than the alloca instruction itself.
//
// Globals (module-scope `var X Foo = ...`) live for the whole program
// runtime and are not heap-allocated, so releasing them as if they were
// an `_tin_rc_alloc` block would read uninit bytes before the global
// (whatever lay in .bss/.data padding) and decrement that as if it were
// an RC count.  Treating globals the same as stack allocas -- borrow
// vtable, no retain -- keeps the iface block lifecycle correct without
// touching the borrowed storage.
func isStackAllocaRoot(v value.Value) bool {
	for i := 0; i < 8; i++ {
		switch n := v.(type) {
		case *ir.InstAlloca:
			return true
		case *ir.Global:
			return true
		case *ir.InstBitCast:
			v = n.From
		case *ir.InstGetElementPtr:
			v = n.Src
		default:
			return false
		}
	}

	return false
}

func (cg *CodeGen) coerceToTrait(block *ir.Block, structVal value.Value, instKey string) (value.Value, error) {
	structType := structVal.Type()

	// Rule 1 (docs/06-traits.md): the source's receiver form must
	// match the form the impl methods expect.  `traitImplForm` walks
	// the struct's impl methods (and falls back to the trait def) and
	// returns "pointer" or "value"; the source is `*T` iff structType
	// is a PointerType.  Mismatches are rejected with a positioned
	// diagnostic stashed on cg.coerceLastErr (coerce() itself has no
	// error channel and is called from 87+ sites we don't want to
	// touch).
	// Coerce semantics: value source -> trait owns a heap-copy of the
	// value (snapshot); pointer source -> trait fat-ptr aliases the
	// source directly.  Both legitimate, documented in
	// docs/06-traits.md.  Mutations through pointer-receiver methods
	// land on whatever data points to (the heap copy or the source);
	// value-receiver methods receive a stack-local copy at the vtable
	// adapter regardless.  The `&x` vs `x` distinction at the coerce
	// site is the user's signal for "alias me" vs "snapshot me" --
	// the compiler doesn't second-guess it.

	var (
		dataPtr      value.Value
		concreteType irtypes.Type
	)

	if pt, ok := structType.(*irtypes.PointerType); ok {
		// Pointer source: alias the source pointer directly so that
		// mutations through *Self impl methods propagate to *structVal.
		// The trait fat-ptr's data field is exactly the source pointer.
		// Lifetime is split three ways:
		//   1. source provenance traces back to `_tin_rc_alloc`
		//      (`&T{...}`, ensureRCAlloc, etc.): retain to give the
		//      iface its own RC slot, scope-exit release balances.
		//   2. source is a stack/global borrow (`&local` or alloca
		//      bitcast): swap in the trait's borrow vtable so the
		//      iface's release is a no-op, don't retain.
		//   3. source is an external/raw pointer (mem::malloc + cast,
		//      C-interop returns): borrow vtable + no retain; the
		//      caller owns lifetime exactly like any *T pointer cast.
		stackBorrowSrc := isStackAllocaRoot(structVal)
		if !stackBorrowSrc {
			if loadInst, isLoad := structVal.(*ir.InstLoad); isLoad {
				if cg.sourceBindingPointsToBorrowedStorage(loadInst.Src) {
					stackBorrowSrc = true
				}
			}
		}

		hasRCHeader := !stackBorrowSrc && cg.pointerProvenanceIsRCAlloc(structVal)

		dataPtr = block.NewBitCast(structVal, irtypes.I8Ptr)
		concreteType = pt.ElemType

		if hasRCHeader && !cg.coerceTransfersSource {
			skipRetain := false

			if loadInst, isLoad := structVal.(*ir.InstLoad); isLoad {
				if cg.sourceBindingIsEarlyHeap(loadInst.Src) {
					skipRetain = true
				}
			}

			if !skipRetain {
				block.NewCall(cg.ensureRetain(), dataPtr)
			}
		}
		// Stack/global borrow OR external pointer: swap in the
		// trait's borrow vtable so the iface's scope-exit release
		// won't decrement a header that may not exist.
		if !hasRCHeader {
			structName := cg.typeNameOf(pt.ElemType)
			vtableKey := structName + "__" + instKey

			if borrow := cg.ensureTraitBorrowVtable(vtableKey); borrow != nil {
				cg.lastAliasBorrowVtable = borrow
			}
		}
	} else {
		// Value-source coerce: the trait fat-ptr owns its own
		// heap-allocated snapshot of the value.  Mutations through
		// *Self impl methods land on the snapshot, not on the
		// caller's storage.  The -Wtrait-snapshot-mutation warning
		// fires from astchecks (not here) when the source struct's
		// impl has pointer-receiver methods, so it has the right
		// source position and doesn't double-fire on $coro variants.
		//
		// Heap-allocate the source struct so the iface's `this` pointer
		// survives across coroutine suspends. A stack alloca here would die
		// the moment the constructing coroutine suspends (the resume
		// function's stack frame is freed on suspend), and any spawned
		// fiber that captured the iface would later read freed memory --
		// which on AArch64 reliably corrupts (the next worker-stack frame
		// overwrites it), and on AMD64 happens to look intact under most
		// scheduling but isn't guaranteed.
		//
		// _tin_rc_alloc gives us an ARC header so existing release paths
		// reclaim the storage when the iface goes out of scope.
		szGEP := block.NewGetElementPtr(structType,
			constant.NewNull(irtypes.NewPointer(structType)),
			constant.NewInt(irtypes.I32, 1))
		szInt := block.NewPtrToInt(szGEP, irtypes.I64)
		heapPtr := block.NewCall(cg.ensureRCAlloc(), szInt)
		typedPtr := block.NewBitCast(heapPtr, irtypes.NewPointer(structType))
		block.NewStore(structVal, typedPtr)

		dataPtr = heapPtr
		concreteType = structType
	}

	// The vtable global is a compile-time constant that is always correct for
	// the (struct, trait) pair - including malloc'd structs whose embedded
	// vtable field has not yet been initialized.
	structName := cg.typeNameOf(concreteType)
	vtableKey := structName + "__" + instKey

	vtableGlobal, ok := cg.traitVtableGlobals[vtableKey]
	if !ok {
		return nil, fmt.Errorf("no vtable for %s implementing %s", cg.diagStructName(structName), cg.traitDisplayName(instKey))
	}

	fatPtrType := cg.ifaceFor(CanonKey(instKey))
	if fatPtrType == nil {
		return nil, fmt.Errorf("no fat-ptr type for trait %s", cg.traitDisplayName(instKey))
	}

	// Borrow vtable swap for non-RC pointer sources (set above).
	chosenVtable := value.Value(vtableGlobal)

	if cg.lastAliasBorrowVtable != nil {
		chosenVtable = cg.lastAliasBorrowVtable
		cg.lastAliasBorrowVtable = nil
	}

	// Build fat pointer {i8* data, vtable*}.
	ifaceAlloca := block.NewAlloca(fatPtrType)
	dataGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, dataGep)
	vtableGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(chosenVtable, vtableGep)

	// Defer the heap-block release to the enclosing scope's exit. We
	// can't release immediately after this call returns because spawn'd
	// fibers in the callee may still be using the data ptr at that
	// point; we can't release at iface-temporary-end because temporaries
	// have no precise scope. Tying the release to the calling scope
	// handles both: the scope's `await` must complete before the scope
	// exits, which guarantees every captured ptr has been read by then.
	//
	// Skip the synthetic entry when the iface is being constructed for
	// a let-binding (the let entry's ownsIfaceData flag handles release
	// directly), for a for-iter loop (genForIterTrait emits its own
	// release at loop exit), or for a trait init/deinit chain call
	// (genStructLit / emitReleaseInner emit their own tighter release).
	// Those callers pre-mark cg.suppressIfaceScopeRelease.  Also skip
	// for stack-borrow pointer sources -- the borrow vtable's release
	// is a no-op and we don't own the storage either way.
	if cg.curScope != nil && !cg.suppressIfaceScopeRelease {
		ptrSlot := block.NewAlloca(irtypes.I8Ptr)
		block.NewStore(dataPtr, ptrSlot)

		name := fmt.Sprintf(".iface_data_%d", cg.strCount)
		cg.strCount++
		cg.curScope.set(name, &scopeEntry{
			val: ptrSlot, isAlloc: true, releaseRawPtr: true,
		})
	}

	return block.NewLoad(fatPtrType, ifaceAlloca), nil
}
