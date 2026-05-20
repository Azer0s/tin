package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

func (cg *CodeGen) emitScopeRelease(block *ir.Block, s *scope) {
	if block == nil || block.Term != nil {
		return
	}

	// LIFO release in declaration order. eachReverse keeps the IR
	// deterministic across runs (Go map iteration is randomized) and
	// matches the natural "last declared, first torn down" ordering
	// for stack-allocated locals.
	s.eachReverse(func(_ string, entry *scopeEntry) {
		if !entry.isAlloc || entry.noRelease {
			return
		}

		ptrType, ok := entry.val.Type().(*irtypes.PointerType)
		if !ok {
			return
		}

		// Synthetic alloca holding a raw i8* heap ptr that needs releasing
		// (e.g. iface temporaries from coerceToTrait used inline as call
		// arguments). Load and release; no struct walk needed.
		if entry.releaseRawPtr {
			loadedPtr := block.NewLoad(ptrType.ElemType, entry.val)
			block.NewCall(cg.ensureRelease(), loadedPtr)

			return
		}

		// Heap-promoted struct fields: callee returned a struct value
		// whose raw-pointer field is a heap block.  Release each block
		// before the rest of the scope-exit logic runs (the per-struct
		// release below treats raw *T fields as borrows, so the cascade
		// would otherwise leak the cell).
		cg.releaseHeapPromotedFields(block, entry, ptrType)

		// Early-heap-promoted local: entry.val is the heap pointer itself
		// (allocated via _tin_rc_alloc at let-decl time because escape
		// analysis flagged the binding). The heap block now belongs to
		// whatever escape sink took the address -- return value, struct
		// field, *Trait coerce, channel send, spawn arg -- so the local
		// scope MUST NOT free it on exit. Caller-side release happens
		// through the receiving owner's drop chain.
		//
		// (Known follow-up: when the receiving owner is a raw `*T` field
		// of a Tin struct, Tin's per-struct release helper doesn't yet
		// cascade through it, so the heap block leaks. Tracked separately --
		// this branch only avoids the use-after-free.)
		if entry.isEarlyHeap {
			return
		}

		// (ownsHeapIfaceData previously emitted an explicit data-field
		// release here.  Now that ensureStructPtrReleaseFn releases the
		// data field automatically when the iface RC hits 0, that
		// explicit release would double-free.  Left as a no-op flag --
		// the chain-release below frees both iface and data.)
		_ = entry.ownsHeapIfaceData

		// isHeapOwned: variable holds a _tin_rc_alloc'd pointer returned by a
		// heap-promoting callee.  Use chain release to free all RC blocks.
		if entry.isHeapOwned {
			heapPtr := block.NewLoad(ptrType.ElemType, entry.val)
			if entry.heapOwnedDepth > 1 {
				// depth > 1: prefer the null-safe named-struct chain helper;
				// fall back to inline emitHeapChainRelease for non-struct chains
				// (e.g. **i64, ***i64).
				structName := cLayoutStructBaseName(entry.tinType)
				if structName != "" {
					relFn := cg.ensureHeapChainReleaseFn(structName, entry.heapOwnedDepth)
					block.NewCall(relFn, heapPtr)
				} else {
					cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)
				}
			} else {
				cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)
			}

			return
		}
		// Slice variables store the base allocation pointer separately so that
		// ARC release hits the real ARC header rather than an interior pointer.
		if entry.basePtr != nil {
			block.NewCall(cg.ensureRelease(), entry.basePtr)

			return
		}

		elemType := ptrType.ElemType

		// Trait-iface let-bindings whose data ptr we heap-allocated
		// during coerceToTrait need an explicit _tin_release on the
		// data field at scope exit. The struct itself has no other
		// RC-tracked fields and elemNeedsRelease would skip it.
		if entry.ownsIfaceData && isTraitFatPtrShape(elemType) {
			loaded := block.NewLoad(elemType, entry.val)
			dataField := block.NewExtractValue(loaded, 0)
			block.NewCall(cg.ensureRelease(), dataField)

			return
		}

		// *Trait or *TinStruct pointer binding without isHeapOwned:
		// elemNeedsRelease returns false for raw pointer types, so a
		// binding like `let g *Trait = &x as *Trait` or
		// `let p = items[idx]` (where items is [*Struct]) would
		// otherwise leak the heap block.  Call its release_ptr
		// explicitly; ensureStructPtrReleaseFn handles both the
		// iface and Tin-struct shapes.
		//
		// Trait fat-ptr branch: always release for let/const, since
		// these are bound from a coerce that minted a heap iface
		// block.  Tin-struct branch: ONLY release when the binding
		// actually took ownership (ownsPtrViaRetain).  Without that
		// guard, `let mb = &local_struct` would release a stack
		// pointer and infinite-loop / corrupt memory.
		if entry.declaredLet && !entry.noDeinit {
			if pt, isPtr := elemType.(*irtypes.PointerType); isPtr {
				if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
					isTinStruct := cg.structTypeFor(CanonKey(innerSt.Name())) != nil
					isIface := isTraitFatPtrShape(innerSt)

					shouldRelease := isIface || (isTinStruct && entry.ownsPtrViaRetain)
					if shouldRelease {
						loaded := block.NewLoad(elemType, entry.val)
						relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
						block.NewCall(relFn, loaded)

						return
					}
				}
			}
		}

		if !cg.elemNeedsRelease(elemType) {
			return
		}

		// Fixed-size array of releasable elements: iterate each slot rather
		// than loading the whole [N x T] (emitRelease has no path for it).
		if at, isArr := elemType.(*irtypes.ArrayType); isArr {
			cg.emitFixedArrayRelease(block, entry.val, at)

			return
		}

		loaded := block.NewLoad(elemType, entry.val)
		if entry.noDeinit {
			cg.emitReleaseNoDeinit(block, loaded)
		} else {
			cg.emitRelease(block, loaded)
		}
	})
}

// emitAllScopeReleases emits _tin_release for all ARC-tracked variables in
// the current scope chain.  skipName, if non-empty, skips that variable
// (used to transfer ownership of a return value to the caller).
func (cg *CodeGen) emitAllScopeReleases(block *ir.Block, skipName string) {
	if block == nil || block.Term != nil {
		return
	}

	s := cg.curScope
	for s != nil {
		// LIFO across this scope's vars; same rationale as emitScopeRelease.
		s.eachReverse(func(name string, entry *scopeEntry) {
			if name == skipName || !entry.isAlloc || entry.isGlobal || entry.noRelease {
				return
			}

			ptrType, ok := entry.val.Type().(*irtypes.PointerType)
			if !ok {
				return
			}
			// Synthetic alloca holding a raw i8* heap ptr; see emitScopeRelease.
			if entry.releaseRawPtr {
				loadedPtr := block.NewLoad(ptrType.ElemType, entry.val)
				block.NewCall(cg.ensureRelease(), loadedPtr)

				return
			}
			// Heap-promoted struct fields cascade-release; twin of the
			// branch in emitScopeRelease.
			cg.releaseHeapPromotedFields(block, entry, ptrType)
			// (ownsHeapIfaceData no-op: the iface dtor in
			// ensureStructPtrReleaseFn handles data release now.  See
			// twin comment in emitScopeRelease.)
			_ = entry.ownsHeapIfaceData
			// isHeapOwned: chain release.
			if entry.isHeapOwned {
				heapPtr := block.NewLoad(ptrType.ElemType, entry.val)
				if entry.heapOwnedDepth > 1 {
					structName := cLayoutStructBaseName(entry.tinType)
					if structName != "" {
						relFn := cg.ensureHeapChainReleaseFn(structName, entry.heapOwnedDepth)
						block.NewCall(relFn, heapPtr)
					} else {
						cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)
					}
				} else {
					cg.emitHeapChainRelease(block, heapPtr, entry.heapOwnedDepth)
				}

				return
			}
			// Slice variables: release the base allocation pointer, not the fat-ptr.
			if entry.basePtr != nil {
				block.NewCall(cg.ensureRelease(), entry.basePtr)

				return
			}

			elemType := ptrType.ElemType

			// Trait-iface let-bindings owning their heap-allocated data:
			// release the iface's data ptr explicitly. See twin handling
			// in emitScopeRelease.
			if entry.ownsIfaceData && isTraitFatPtrShape(elemType) {
				loaded := block.NewLoad(elemType, entry.val)
				dataField := block.NewExtractValue(loaded, 0)
				block.NewCall(cg.ensureRelease(), dataField)

				return
			}

			// *Trait or *TinStruct pointer binding fallback; see twin
			// in emitScopeRelease.
			if entry.declaredLet && !entry.noDeinit {
				if pt, isPtr := elemType.(*irtypes.PointerType); isPtr {
					if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
						isTinStruct := cg.structTypeFor(CanonKey(innerSt.Name())) != nil
						isIface := isTraitFatPtrShape(innerSt)

						shouldRelease := isIface || (isTinStruct && entry.ownsPtrViaRetain)
						if shouldRelease {
							loaded := block.NewLoad(elemType, entry.val)
							relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
							block.NewCall(relFn, loaded)

							return
						}
					}
				}
			}

			if !cg.elemNeedsRelease(elemType) {
				return
			}

			// Fixed-size array of releasable elements: iterate each slot.
			if at, isArr := elemType.(*irtypes.ArrayType); isArr {
				cg.emitFixedArrayRelease(block, entry.val, at)

				return
			}

			loaded := block.NewLoad(elemType, entry.val)
			if entry.noDeinit {
				cg.emitReleaseNoDeinit(block, loaded)
			} else {
				cg.emitRelease(block, loaded)
			}
		})

		if s.isFunctionBoundary {
			break
		}

		s = s.parent
	}
}
