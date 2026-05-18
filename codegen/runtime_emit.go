package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) extractRCDataPtr(block *ir.Block, val value.Value, t irtypes.Type) value.Value {
	if isStringType(t) {
		// String {i8*, i64}: field 0 is already i8* - no bitcast needed
		return block.NewExtractValue(val, 0)
	}

	if isFatArrayPtr(t) {
		// Fat array {T*, i64}: field 0 is T* - bitcast to i8* for _tin_retain/release
		dataPtr := block.NewExtractValue(val, 0)

		return block.NewBitCast(dataPtr, irtypes.I8Ptr)
	}

	if isAnyType(t) {
		// any {i32, i8*}: field 1 is the i8* data pointer
		return block.NewExtractValue(val, 1)
	}
	// Trait fat-pointer value: {i8* data, vtable*}.  The first field is the
	// heap pointer to the underlying concrete struct (allocated by
	// coerceToTrait / buildPtrToTraitBorrow), so _tin_retain/release on
	// that pointer is what balances ARC for an iface-VALUE field embedded
	// in another struct/ADT.  Without this, copies of a Result whose Err
	// payload is `errors::Err` (a trait value) would forget to bump the
	// iface block's RC -- the original's drop then frees the block while
	// the copy still holds a reference, producing the tcache double-free
	// we caught under valgrind.
	if st, ok := t.(*irtypes.StructType); ok && isTraitFatPtrShape(st) {
		dataPtr := block.NewExtractValue(val, 0)

		return dataPtr
	}

	return nil
}

// walkRCStructFieldsEx is the array-aware variant of walkRCStructFields.
// arrayVisit (if non-nil) receives a pointer to each inline `[T; N]` field
// whose element type carries owning state -- callers use it to emit per-
// element retain/release passes that the scalar `visit` callback cannot
// handle (the array-as-value load has no useful release path).
func (cg *CodeGen) walkRCStructFieldsEx(
	block *ir.Block,
	val value.Value,
	visit func(value.Value),
	arrayVisit func(fieldPtr value.Value, at *irtypes.ArrayType),
) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	structName := cg.typeNameOf(val.Type())
	if structName == "" {
		return
	}

	// cLayoutStructs: user fields live in C-owned native memory via c_data_ptr
	// (non-handover borrow) or in the RC block overflow (handover, freed with the
	// block). In both cases Tin does not independently own those fields; releasing
	// them would call _tin_release on raw C pointers, corrupting memory.
	if cg.cLayoutStructs[structName] {
		return
	}

	fieldTypes := cg.structFieldLLVMTypes[structName]
	offset := cg.userFieldOffset(structName)
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)

	fieldNames := cg.structFields[structName]
	weakSet := cg.structWeakFields[structName]

	owningRawPtrFields := cg.structOwningRawPtrFields[structName]

	for i, ft := range fieldTypes {
		_, isNestedStruct := ft.(*irtypes.StructType)

		// Owning pointer to a known Tin struct: must be recursively released/retained
		// just like an inline nested struct.  Only non-weak fields qualify.  Trait
		// fat-ptr iface structs live in cg.traitFatPtrTypes (not cg.structTypes), so
		// detect them via shape too -- otherwise a struct field of type *Trait
		// (e.g. `e *errors::Err`) would skip retain/release entirely and the iface
		// block leaks on outer struct copy or use-after-frees on outer struct drop
		// (caller's drop releases the iface, callee's parameter copy never
		// retained).
		isTinStructPtr := false

		if pt, ok2 := ft.(*irtypes.PointerType); ok2 {
			if innerSt, ok3 := pt.ElemType.(*irtypes.StructType); ok3 && innerSt.Name() != "" {
				if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
					isTinStructPtr = true
				} else if isTraitFatPtrShape(innerSt) {
					isTinStructPtr = true
				}
			}
		}
		// Owning raw pointer field -- registered when escape-promoted local
		// flowed in here. Walk it like any other RC-tracked field so the
		// per-struct release helper cascades _tin_release through it.
		isOwningRawPtr := false

		if i < len(fieldNames) && owningRawPtrFields[fieldNames[i]] {
			if _, isPtr := ft.(*irtypes.PointerType); isPtr {
				isOwningRawPtr = true
			}
		}

		// Fixed-size array field [T; N] whose element type carries owning
		// state: dispatch to arrayVisit with the field's address so the
		// caller can iterate per-element.  An `[errors::Err; 4]` field, for
		// instance, holds N iface heap blocks the struct's release helper
		// must walk -- the scalar visit path can't, since loading the array
		// as a value yields a `[N x T]` that emitRelease has no handler for.
		if at, isArr := ft.(*irtypes.ArrayType); isArr {
			if arrayVisit != nil && cg.elemNeedsRelease(at.ElemType) {
				if i < len(fieldNames) && weakSet[fieldNames[i]] {
					continue
				}

				var fieldPtr value.Value

				if cg.cLayoutStructs[structName] {
					fieldPtr = cg.emitCLayoutFieldPtr(block, alloca, structName, i)
				} else {
					fieldPtr = block.NewGetElementPtr(st, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(offset+i)))
				}

				arrayVisit(fieldPtr, at)
			}

			continue
		}

		if !isRCTrackedType(ft) && !isNestedStruct && !isTinStructPtr && !isOwningRawPtr {
			continue
		}
		// Weak fields are non-owning: skip retain/release entirely.
		if i < len(fieldNames) && weakSet[fieldNames[i]] {
			continue
		}

		var fieldVal value.Value

		if cg.cLayoutStructs[structName] {
			// cLayoutStructs: user fields live in native memory via c_data_ptr.
			fieldGep := cg.emitCLayoutFieldPtr(block, alloca, structName, i)
			fieldVal = block.NewLoad(ft, fieldGep)
		} else {
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(offset+i)))
			fieldVal = block.NewLoad(ft, gep)
		}

		// Owning raw pointer to a non-Tin-struct heap block (escape-promoted
		// local recorded via markOwningRawPtrField). Decrement RC directly;
		// the standard emitRelease path doesn't know how to release a bare
		// `*T` field because Tin treats *T as a borrow elsewhere.
		if isOwningRawPtr {
			ptrI8 := block.NewBitCast(fieldVal, irtypes.I8Ptr)
			block.NewCall(cg.ensureRelease(), ptrI8)

			continue
		}

		visit(fieldVal)
	}
}

// emitOwningPtrRetainIfApplicable emits a _tin_retain on val if val is an
// owning pointer to a named Tin struct or to a trait fat-ptr iface --
// the cases where ownership transfer requires bumping the heap block's
// RC.  Returns true when a retain was emitted.  Used by genReturn's
// "return s.field" copy-expr branch and genVarDecl's
// "let r = s.field" branch where the caller takes ownership; emitRetain
// itself doesn't handle these because emitRelease's matching path
// (ensureStructPtrReleaseFn) is keyed on scope-exit, not on every
// retain site.
func (cg *CodeGen) emitOwningPtrRetainIfApplicable(block *ir.Block, val value.Value) bool {
	pt, ok := val.Type().(*irtypes.PointerType)
	if !ok {
		return false
	}

	innerSt, ok2 := pt.ElemType.(*irtypes.StructType)
	if !ok2 || innerSt.Name() == "" {
		return false
	}

	isTinStruct := cg.structTypeFor(CanonKey(innerSt.Name())) != nil
	if !isTinStruct && !isTraitFatPtrShape(innerSt) {
		return false
	}

	ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
	block.NewCall(cg.ensureRetain(), ptrI8)

	return true
}

// emitRetain emits a _tin_retain call for an ARC-tracked value.
// For named structs, it also retains any RC-tracked fields.
//
// NOTE: bare *<named struct> / *<iface> values are NOT retained here.
// Tin's calling convention treats those as borrows -- the caller's
// retain at the call site or struct-copy path is what keeps the
// pointee alive, and callee scope exit must not release.  Mirroring
// that asymmetry at function entry would unbalance scope releases.
// When ownership transfer IS required (genReturn's "return s.field"
// path, genVarDecl's "let r = s.field" path), the caller emits
// _tin_retain on the i8* representation explicitly via
// emitOwningPtrRetain.
