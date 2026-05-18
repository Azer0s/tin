package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) ensureDataPtrReleaseFn(adtName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structPtrReleaseFns[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	ptrType := irtypes.NewPointer(st)
	fnName := adtName + "__data_release_ptr"
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("ptr", ptrType))

	cg.structPtrReleaseFns[adtName] = fn

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	exit := fn.NewBlock("exit")

	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load the full struct onto the stack BEFORE decrementing RC so that
	// payload reads remain valid even if _tin_release_struct frees the block.
	loadedVal := doRelease.NewLoad(st, fn.Params[0])
	stackCopy := doRelease.NewAlloca(st)
	doRelease.NewStore(loadedVal, stackCopy)

	tagGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := doRelease.NewLoad(irtypes.I64, tagGEP)

	payloadGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	// Decrement RC on the outer block; proceed to descend into children only
	// when we were the last reference (RC hit 0).
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)

	dispatch := fn.NewBlock("dispatch")
	doRelease.NewCondBr(isOne, dispatch, exit)

	var switchCases []*ir.Case

	for _, e := range sortedVariants(variants) {
		variantName, vi := e.Name, e.Info
		if !cg.variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, vi.Tag), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			cg.emitRelease(caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	dispatch.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// variantHasReleasableField returns true if any of the variant's fields carry
// an owning reference that needs release (RC-tracked type, owning pointer
// to a registered struct/ADT, or embedded named struct that itself has
// RC-tracked fields).
//
// The embedded-struct branch is what catches `Result.Ok(event_with_time)`
// where event_with_time is `struct { name string ... }`: the variant's
// payload field is the struct by value, not a pointer, so the
// pointer-to-struct branch above doesn't fire. Without this check the
// match scrutinee for `match parse(...): Ok(ev) -> ...` is judged
// "no owning fields" and the inner string leaks at scope exit.
func (cg *CodeGen) variantHasReleasableField(vi *dataVariantInfo) bool {
	for i, f := range vi.Fields {
		if f.IsWeak {
			continue
		}

		t := vi.PayloadType.Fields[i]
		if isRCTrackedType(t) {
			return true
		}

		if pt, ok := t.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				return true
			}
		}

		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			if cg.elemNeedsRelease(t) {
				return true
			}
		}
	}

	return false
}

// fieldNeedsOwningRelease returns true when a payload field type represents an
// owning reference (RC-tracked fat type, pointer to a named struct, or
// an embedded named struct with RC fields). See variantHasReleasableField
// for the embedded-struct rationale.
func (cg *CodeGen) fieldNeedsOwningRelease(t irtypes.Type) bool {
	if isRCTrackedType(t) {
		return true
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			return true
		}
	}

	if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
		if cg.elemNeedsRelease(t) {
			return true
		}
	}

	return false
}

// emitDataValueRetain tag-dispatches retain over an ADT value's payload.
func (cg *CodeGen) emitDataValueRetain(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueRetainFn(st.Name(), st)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// emitDataValueRelease releases the active variant's owning fields for an
// ADT value. Implemented as a single call to a per-ADT helper function so
// that the caller's basic block is not split.
func (cg *CodeGen) emitDataValueRelease(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueFieldFn(st.Name(), st,
		"__data_release_val", cg.dataValueReleaseFns,
		(*CodeGen).emitRelease)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// ensureDataValueRetainFn generates a per-ADT helper that retains all owning
// fields of the active variant's payload. The releaser counterpart is inlined
// directly into emitDataValueRelease via ensureDataValueFieldFn.
func (cg *CodeGen) ensureDataValueRetainFn(adtName string, st *irtypes.StructType) *ir.Func {
	return cg.ensureDataValueFieldFn(adtName, st,
		"__data_retain_val", cg.dataValueRetainFns,
		(*CodeGen).emitStructFieldRetain)
}

// ensureDataValueFieldFn is the common skeleton: lookup cache, precompute the
// "any variant has a releasable field" short-circuit, emit the tag-dispatch
// switch, and for each releasable field in each variant call the supplied
// emitField method (a pointer-to-method so the caller can pick retain vs
// release). All owning fields are processed (pointer-to-struct and
// RC-tracked fat types); weak fields are skipped.
func (cg *CodeGen) ensureDataValueFieldFn(
	adtName string,
	st *irtypes.StructType,
	suffix string,
	cache map[string]*ir.Func,
	emitField func(*CodeGen, *ir.Block, value.Value),
) *ir.Func {
	if fn, ok := cache[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	any := false

	for _, vi := range variants {
		if cg.variantHasReleasableField(vi) {
			any = true

			break
		}
	}

	if !any {
		cache[adtName] = nil

		return nil
	}

	fnName := adtName + suffix
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("val", st))
	cache[adtName] = fn

	entry := fn.NewBlock("entry")
	exit := fn.NewBlock("exit")

	stackCopy := entry.NewAlloca(st)
	entry.NewStore(fn.Params[0], stackCopy)

	tagGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI64 := entry.NewLoad(irtypes.I64, tagGEP)

	payloadGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	var switchCases []*ir.Case

	for _, e := range sortedVariants(variants) {
		variantName, vi := e.Name, e.Info
		if !cg.variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, vi.Tag), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			// Trait fat-ptr value fields embedded in an ADT variant
			// payload need value-form retain/release directly.
			// Going through emitField (emitRelease /
			// emitStructFieldRetain) hits walkRCStructFields which
			// doesn't know iface struct shape; pre-fix the iface
			// block leaked on release and aliased without a +1 RC
			// on retain. Both must be paired or copies double-free.
			if ft, ok := vi.PayloadType.Fields[fi].(*irtypes.StructType); ok && isTraitFatPtrShape(ft) {
				emitFatPtrRetainOrRelease(cg, caseBlock, fieldVal, ft, suffix == "__data_retain_val")

				continue
			}

			emitField(cg, caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	entry.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// emitFatPtrRetainOrRelease emits inline retain/release for a value-form
// trait fat-ptr {i8* data, vtable*} embedded in an ADT variant
// payload.
//
// Retain: `_tin_retain(data)` - the iface data block was alloc'd by
// coerceToTrait via _tin_rc_alloc, so retain is always safe (the RC
// header is at data - sizeof(TinRCHdr)).
//
// Release: dispatch through the vtable's data-release thunk (last
// slot) - the thunk decrements the data block's RC and walks nested
// RC fields when the block hits 0. We do NOT additionally
// _tin_release: the thunk already releases the block.
func emitFatPtrRetainOrRelease(cg *CodeGen, block *ir.Block, val value.Value, st *irtypes.StructType, retain bool) {
	dataField := block.NewExtractValue(val, 0)

	if retain {
		block.NewCall(cg.ensureRetain(), dataField)

		return
	}

	vtableField := block.NewExtractValue(val, 1)

	vtablePtrType, ok := st.Fields[1].(*irtypes.PointerType)
	if !ok {
		return
	}

	vtableSt, ok2 := vtablePtrType.ElemType.(*irtypes.StructType)
	if !ok2 || len(vtableSt.Fields) == 0 {
		return
	}

	lastIdx := len(vtableSt.Fields) - 1
	lastFieldType := vtableSt.Fields[lastIdx]

	lastPt, ok3 := lastFieldType.(*irtypes.PointerType)
	if !ok3 {
		return
	}

	lastFnType, ok4 := lastPt.ElemType.(*irtypes.FuncType)
	if !ok4 || len(lastFnType.Params) != 1 ||
		!lastFnType.Params[0].Equal(irtypes.I8Ptr) ||
		!irtypes.IsVoid(lastFnType.RetType) {
		return
	}

	releaseFnSlot := block.NewGetElementPtr(vtableSt, vtableField,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(lastIdx)))
	releaseFn := block.NewLoad(lastFieldType, releaseFnSlot)
	block.NewCall(releaseFn, dataField)
}

// adtHasFatPtrField reports whether any variant of the given ADT
// type carries a trait fat-ptr value field. Used by genReturn to
// decide whether to suppress synthetic iface-data scope-releases for
// the function exit.
func (cg *CodeGen) adtHasFatPtrField(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok {
		return false
	}

	variants := cg.dataVariants[st.Name()]
	if variants == nil {
		return false
	}

	for _, vi := range variants {
		for fi := range vi.Fields {
			if vi.Fields[fi].IsWeak {
				continue
			}

			if ft, ok := vi.PayloadType.Fields[fi].(*irtypes.StructType); ok && isTraitFatPtrShape(ft) {
				return true
			}
		}
	}

	return false
}

// hasLiveIfaceDataScopeEntry reports whether the current function scope
// chain holds any synthetic `.iface_data_*` entry that has not been marked
// noRelease.  Used by genReturn to gate the trait fat-ptr ownership retain:
// the retain only compensates for a scope release that's actually going to
// fire, so a pass-through wrapper / borrow-deref return must not over-retain.
func (cg *CodeGen) hasLiveIfaceDataScopeEntry() bool {
	if cg.curScope == nil {
		return false
	}

	found := false

	s := cg.curScope
	for s != nil {
		s.each(func(name string, e *scopeEntry) {
			if e.releaseRawPtr && !e.noRelease {
				found = true
			}
		})

		if found || s.isFunctionBoundary {
			break
		}

		s = s.parent
	}

	return found
}

// suppressIfaceDataScopeReleases marks every synthetic
// `.iface_data_*` scope entry in the current function scope as
// noRelease, so emitAllScopeReleases skips its _tin_release call.
// Used by genReturn when the return value is an ADT whose payload
// transferred an iface to the caller - the caller's data_release_val
// becomes the sole owner.
func (cg *CodeGen) suppressIfaceDataScopeReleases() {
	if cg.curScope == nil {
		return
	}

	s := cg.curScope
	for s != nil {
		s.each(func(name string, e *scopeEntry) {
			if e.releaseRawPtr {
				e.noRelease = true
			}
		})

		if s.isFunctionBoundary {
			break
		}

		s = s.parent
	}
}

// genAdtIsExpr handles `x is Ctor(bindings...)` and `x is NullaryVariant` on
// an ADT-typed scrutinee. Returns (value, handled=true, err) when it
