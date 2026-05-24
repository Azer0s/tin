package codegen

import (
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
)

func (cg *CodeGen) emitAnyDispatchRegistrations(block *ir.Block) *ir.Block {
	if block == nil {
		return block
	}

	// One-shot: only the first wrapper that calls in emits the
	// registrations. Subsequent calls (from genTestRunner /
	// genImplicitMain when both happen to fire) are no-ops.
	if cg.anyDispatchEmitted {
		return block
	}

	cg.anyDispatchEmitted = true

	regRelease := cg.ensureExternDecl("_tin_register_any_release", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("type_id", irtypes.I32),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	regDeepCopy := cg.ensureExternDecl("_tin_register_any_deepcopy", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("type_id", irtypes.I32),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	// Iterate in typeID order so the emitted register-call sequence is
	// deterministic across program runs (Go map iteration is randomized).
	type structEntry struct {
		Name   string
		TypeID int32
	}

	entries := make([]structEntry, 0, len(cg.structTypeIDs))

	for name, id := range cg.structTypeIDs {
		entries = append(entries, structEntry{Name: name, TypeID: id})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TypeID < entries[j].TypeID
	})

	for _, e := range entries {
		structName, typeID := e.Name, e.TypeID

		st := cg.structTypeFor(CanonKey(structName))
		if st == nil {
			continue
		}

		if cg.structEligibleForAnyDispatch(structName, st) {
			relFn := cg.ensureStructPtrReleaseFn(structName, st)
			if relFn != nil {
				fnI8 := block.NewBitCast(relFn, irtypes.I8Ptr)
				block.NewCall(regRelease, constant.NewInt(irtypes.I32, int64(typeID)), fnI8)
			}
		}

		// Any-deepcopy is independently eligible: we register a
		// thunk for every named struct that has a deep-copy fn,
		// not just the deinit / no-copy carve-out structEligibleFor
		// AnyDispatch applies to release.  Lets ordinary value-
		// types with rc fields get isolated deep-copy when boxed
		// in `any` and that any flows into a mutating callee.
		// ADT and cLayoutStructs go through their own paths; skip.
		if !cg.isDataType(st) && !cg.cLayoutStructs[structName] && cg.structHasRelease(structName, st) {
			thunk := cg.ensureAnyDeepCopyThunk(structName, st)
			if thunk != nil {
				thunkI8 := block.NewBitCast(thunk, irtypes.I8Ptr)
				block.NewCall(regDeepCopy, constant.NewInt(irtypes.I32, int64(typeID)), thunkI8)
			}
		}
	}

	return block
}

// ensureAnyDeepCopyThunk returns a cached or freshly-generated
// `i8* @{struct}__any_deepcopy(i8* %src)` thunk that the runtime
// dispatcher (_tin_any_deepcopy) calls when the type-id matches.
//
// The thunk loads the boxed struct from %src (a _tin_rc_alloc'd
// data block holding the struct value), deep-copies it via the
// per-struct deep-copy generator, allocates a new block of the
// same size, stores the deep-copy result, and returns the new
// block as i8*.  Result: callers get a fresh data slot with
// isolated buffer fields, matching how the box-to-any boxing
// originally allocated the data block.
//
// Cached in cg.anyDeepCopyThunks so successive call sites share
// the same registered thunk.
func (cg *CodeGen) ensureAnyDeepCopyThunk(structName string, st *irtypes.StructType) *ir.Func {
	if cg.anyDeepCopyThunks == nil {
		cg.anyDeepCopyThunks = map[string]*ir.Func{}
	}

	if fn, ok := cg.anyDeepCopyThunks[structName]; ok {
		return fn
	}

	deepFn := cg.ensureStructDeepCopyFn(structName, st)
	if deepFn == nil {
		return nil
	}

	fnName := structName + "__any_deepcopy"
	fn := cg.activeModule().NewFunc(fnName, irtypes.I8Ptr,
		ir.NewParam("src", irtypes.I8Ptr))
	cg.anyDeepCopyThunks[structName] = fn

	entry := fn.NewBlock("entry")

	typedSrc := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(st))
	srcVal := entry.NewLoad(st, typedSrc)
	copied := entry.NewCall(deepFn, srcVal)

	nullStructPtr := constant.NewNull(irtypes.NewPointer(st))
	sizeGep := entry.NewGetElementPtr(st, nullStructPtr, constant.NewInt(irtypes.I64, 1))
	size := entry.NewPtrToInt(sizeGep, irtypes.I64)

	newBlock := entry.NewCall(cg.ensureRCAlloc(), size)
	newTyped := entry.NewBitCast(newBlock, irtypes.NewPointer(st))
	entry.NewStore(copied, newTyped)
	entry.NewRet(newBlock)

	return fn
}

// structEligibleForAnyDispatch returns true when boxing a *NamedStruct
// of structName into `any` should run that struct's release_ptr (which
// fires deinit and releases child fields) when the any drops.
//
// Eligible:
//   - #no_copy wrappers (rc::Cell, etc.)
//   - structs that declare a `deinit` method (the user opted into
//     custom destruction; we run their deinit when an any drops the
//     last owner)
//
// Excluded:
//   - data types (ADTs) -- their release goes through a variant-
//     dispatched data_release_val path, not the struct release_ptr.
//   - structs with only auto-RC fields (no deinit) -- the field-ARC
//     machinery on the let-binding side already covers cleanup; the
//     any path falls through to the generic _tin_release which still
//     decrements RC correctly without re-running field releases.
//
// The boxToAny pointer-case must agree with this predicate so the
// retain-on-box and the dispatch registration stay in sync.
func (cg *CodeGen) structEligibleForAnyDispatch(structName string, st *irtypes.StructType) bool {
	if !cg.structHasRelease(structName, st) {
		return false
	}

	if cg.isDataType(st) {
		return false
	}

	bareName := structName
	if idx := strings.LastIndex(structName, "__"); idx > 0 {
		bareName = structName[idx+2:]
	}

	if cg.noCopyStructs[structName] || cg.noCopyStructs[bareName] {
		return true
	}

	return cg.structHasDeinitMethod(structName)
}

// structHasDeinitMethod reports whether structName declares a deinit
// method. Used by structEligibleForAnyDispatch to opt structs into
// per-type-id any-release dispatch when the user has explicitly
// requested custom destruction.
func (cg *CodeGen) structHasDeinitMethod(structName string) bool {
	if cg.curScope == nil {
		return false
	}

	_, ok := cg.curScope.lookup(structName + "_deinit")

	return ok
}

// structHasRelease reports whether struct named structName has a
// per-struct release helper worth dispatching to from the any-release
// path. True when the struct has a deinit or owns RC-tracked fields.
func (cg *CodeGen) structHasRelease(structName string, st *irtypes.StructType) bool {
	if cg.curScope != nil {
		if _, has := cg.curScope.lookup(structName + "_deinit"); has {
			return true
		}
	}

	for _, ft := range cg.structFieldLLVMTypes[structName] {
		if isRCTrackedType(ft) {
			return true
		}

		if pt, ok := ft.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					return true
				}
			}
		}
	}

	_ = st

	return false
}
