package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// ensureStructDeepCopyFn returns a cached or freshly-emitted helper
// that produces a deep copy of a struct value: each scalar/pointer
// field is bit-copied (pointers get a retain so the shared block's
// rc accounts for both the original and the copy), and every
// string/fat-array/nested-struct field gets a fresh buffer.
//
// Signature: `{struct} @{name}__deep_copy({struct} %src)`.
//
// Used by the call-site auto-copy dispatch: when a struct arg flows
// into a callee that mutates its parameter and the caller still
// needs the value after the call, the dispatch calls this helper to
// produce an isolated temp so the callee's mutations (including
// through shared-buffer fields) never propagate back to the caller's
// binding.
func (cg *CodeGen) ensureStructDeepCopyFn(structName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structDeepCopyFns[structName]; ok {
		return fn
	}

	fnName := structName + "__deep_copy"
	srcParam := ir.NewParam("src", st)
	fn := cg.activeModule().NewFunc(fnName, st, srcParam)
	fn.Linkage = enum.LinkageWeakODR
	// Cache before generating the body so a recursive nested-struct
	// field can refer back to us without infinite re-entry.
	cg.structDeepCopyFns[structName] = fn

	entry := fn.NewBlock("entry")
	src := fn.Params[0]

	var result value.Value = constant.NewUndef(st)
	for i, fieldType := range st.Fields {
		fieldVal := entry.NewExtractValue(src, uint64(i))
		copied := cg.deepCopyFieldValue(entry, fieldVal, fieldType)
		result = entry.NewInsertValue(result, copied, uint64(i))
	}

	entry.NewRet(result)

	return fn
}

// deepCopyFieldValue emits IR that produces an isolated copy of a
// single struct field value.  Per field kind:
//
//   - Scalar (int / float / bool / etc): direct bit-copy, no rc work.
//   - Tin string fat-ptr `{i8*, i64, i64}`: alloc a fresh buffer
//     of length bytes via _tin_rc_alloc, memcpy the contents, build
//     a new fat-ptr pointing at the fresh buffer.
//   - Tin `[T]` fat-array `{T*, i64, i64}` where T is a scalar:
//     same as string but with elem size = sizeof(T).
//   - Tin `[T]` where T is RC-tracked: clone the array buffer
//     (memcpy of pointers) and retain each element, so the new
//     array owns a +1 share of each shared element.  Element-level
//     deep copy (cloning each string/struct element's buffer) is
//     left as a follow-up; the current behavior matches what `let b
//     = a` does for an `[string]` binding.
//   - Tin `any` `{i32, i8*}`: retain the boxed pointer; the boxed
//     value is shared.  Element-level deep clone is a follow-up.
//   - Trait iface fat-ptr: retain + copy; the iface block is itself
//     rc-managed and shared.
//   - Pointer (`*T`): copy the pointer + retain via _tin_retain_ptr;
//     pointer fields preserve their by-reference semantics by
//     design (the caller wrote `*T`, not `T`, when they wanted
//     sharing).
//   - Nested struct: recurse via ensureStructDeepCopyFn.
//
// The signal that a struct deep-copy is "fully isolated" today: no
// shared buffer remains for fields handled by the cloning paths
// (string, scalar [T]).  Fields whose shape we can't yet recurse
// into safely (RC-elem arrays, any, iface) stay shared with a
// matching retain so the rc math stays balanced; they are documented
// as a known gap.
func (cg *CodeGen) deepCopyFieldValue(block *ir.Block, val value.Value, t irtypes.Type) value.Value {
	switch ft := t.(type) {
	case *irtypes.IntType, *irtypes.FloatType:
		return val
	case *irtypes.ArrayType:
		// Fixed-size array `[N]T` field: stored inline in the struct
		// layout.  For rc-tracked element types each slot owns +1 of
		// shared content, so without a matching retain the autocopy
		// temp's scope-exit emitFixedArrayRelease would underflow the
		// shared rc and corrupt the original.  Element-wise deep copy
		// (loop unroll or runtime loop over the inline slots) is a
		// follow-up; today we retain each slot via emitRetain (which
		// walks the array via emitFixedArrayRetain) so the math
		// balances at the shared-rc level, matching `let b = a` for
		// these field types.
		if cg.elemNeedsRelease(ft.ElemType) {
			cg.emitRetain(block, val)
		}

		return val
	case *irtypes.PointerType:
		_ = ft

		ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
		block.NewCall(cg.ensureRetainPtr(), ptrI8)

		return val
	case *irtypes.StructType:
		switch {
		case isStringType(ft):
			return cg.deepCopyStringValue(block, val, ft)
		case isFatArrayPtr(ft):
			elemT, _ := ft.Fields[0].(*irtypes.PointerType)
			if elemT == nil {
				cg.emitRetain(block, val)

				return val
			}

			return cg.deepCopyArrayValue(block, val, ft, elemT.ElemType)
		case isAnyType(ft):
			// any {i32 tag, i8* data}: dispatch through
			// _tin_any_deepcopy.  The runtime looks up the per-
			// type-id thunk registered at module init; thunks
			// allocate a fresh data block and deep-copy the
			// boxed value into it.  When no thunk is registered
			// for the tag, the runtime falls back to retain-and-
			// share (legacy behavior).  Tag is preserved; only
			// the data field is replaced.
			tag := block.NewExtractValue(val, 0)
			data := block.NewExtractValue(val, 1)
			newData := block.NewCall(cg.ensureAnyDeepCopyFn(), tag, data)

			var result value.Value = constant.NewUndef(ft)
			result = block.NewInsertValue(result, tag, 0)
			result = block.NewInsertValue(result, newData, 1)

			return result
		case isTraitFatPtrShape(ft):
			// Trait iface fat-ptr: the iface block is rc-managed
			// and shared between the original and the copy.
			// Future work could deep-copy through the vtable
			// when the concrete impl supports it.
			cg.emitRetain(block, val)

			return val
		default:
			if name := ft.Name(); name != "" && cg.structTypeFor(CanonKey(name)) != nil {
				// ADT fields: route through the variant-tag-dispatched
				// deep-copy helper.  Falls back to retain-and-share
				// when the helper returns nil (no variant has any
				// deep-copyable field).
				if cg.isDataType(ft) {
					if fn := cg.ensureDataValueDeepCopyFn(name, ft); fn != nil {
						return block.NewCall(fn, val)
					}

					cg.emitRetain(block, val)

					return val
				}

				// cLayoutStruct fields: the wrapper-block invariants
				// don't fit the generic field-walking model.  Retain
				// + share matches `let b = a` semantics.
				if cg.cLayoutStructs[name] {
					cg.emitRetain(block, val)

					return val
				}

				inner := cg.ensureStructDeepCopyFn(name, ft)

				return block.NewCall(inner, val)
			}

			cg.emitRetain(block, val)

			return val
		}
	default:
		return val
	}
}

// deepCopyStringValue clones a string fat-pointer's underlying byte
// buffer: alloc len bytes via _tin_rc_alloc, memcpy, and build a
// fresh fat-pointer.  The cap field of the new fat-ptr is set to len
// (the copy is exactly sized; the first ++= or growth path will
// allocate as usual).  An empty string (len=0) still calls
// _tin_rc_alloc(0) for shape consistency -- the allocator returns a
// valid 16-byte header that the standard release path handles.
func (cg *CodeGen) deepCopyStringValue(block *ir.Block, val value.Value, t *irtypes.StructType) value.Value {
	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	newI8 := block.NewCall(cg.ensureRCAlloc(), length)
	srcI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureMemcpy(), newI8, srcI8, length, constant.NewInt(irtypes.I1, 0))

	var result value.Value = constant.NewUndef(t)
	result = block.NewInsertValue(result, newI8, 0)
	result = block.NewInsertValue(result, length, 1)
	result = block.NewInsertValue(result, length, 2)

	return result
}

// deepCopyArrayValue clones a fat-array's underlying buffer.  Bytes
// are memcpy'd wholesale to seed the new buffer with the original
// element layout, then per-element handling makes the copy
// independent:
//
//   - For scalar element types (no rc) the memcpy alone is enough.
//   - For raw `*T` pointer elements the existing emitRetainElemSlice
//     bumps each element's pointed-to rc; pointers are intentionally
//     shared per the language spec.
//   - For struct-shaped RC elements (string fat-ptrs, fat arrays,
//     `any`, iface, named structs) the foreach helper walks the new
//     buffer and replaces each element in place with a deep-copied
//     version, so the new array shares no underlying buffer with the
//     source: callee mutations against `c.items[i].field` (a buffer
//     write through an element field) cannot reach back to the
//     caller's array.
func (cg *CodeGen) deepCopyArrayValue(block *ir.Block, val value.Value, t *irtypes.StructType, elemT irtypes.Type) value.Value {
	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
	sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalBytes := block.NewMul(length, elemSize)

	newI8 := block.NewCall(cg.ensureRCAlloc(), totalBytes)
	newDataPtr := block.NewBitCast(newI8, irtypes.NewPointer(elemT))
	srcI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureMemcpy(), newI8, srcI8, totalBytes, constant.NewInt(irtypes.I1, 0))

	// `isRCTrackedType` is too narrow here: it only catches the
	// fat-ptr-shaped kinds (string, fat array, any, iface, fat fn,
	// primitive ptr) and misses named structs whose RC lives in
	// their fields.  Use `elemNeedsRelease` so `[Entry]` where
	// `Entry { label string }` correctly routes through the per-
	// element deep-copy helper instead of leaving every element's
	// string field shared without a matching retain (which would
	// double-free as soon as either the original or the copy went
	// out of scope).
	if cg.elemNeedsRelease(elemT) {
		if cg.elemNeedsDeepClone(elemT) {
			helper := cg.ensureElemDeepCopyHelper(elemT)
			helperI8 := block.NewBitCast(helper, irtypes.I8Ptr)
			block.NewCall(cg.ensureForeachStructElemRetain(), newI8, length, elemSize, helperI8)
		} else {
			cg.emitRetainElemSlice(block, newDataPtr, length, elemT)
		}
	}

	var result value.Value = constant.NewUndef(t)
	result = block.NewInsertValue(result, newDataPtr, 0)
	result = block.NewInsertValue(result, length, 1)
	result = block.NewInsertValue(result, length, 2)

	return result
}

// elemNeedsDeepClone reports whether the element type benefits from
// per-element deep copy (struct-shaped value with shared buffer
// fields) versus a flat retain-each pointer bump.  Pointers (`*T`)
// are intentionally shared per the language spec, so they fall
// through to the retain path.  Scalars never reach here because the
// caller gates on isRCTrackedType.
func (cg *CodeGen) elemNeedsDeepClone(elemT irtypes.Type) bool {
	if _, isPtr := elemT.(*irtypes.PointerType); isPtr {
		return false
	}

	st, isStruct := elemT.(*irtypes.StructType)
	if !isStruct {
		return false
	}

	if isStringType(st) || isFatArrayPtr(st) || isAnyType(st) || isTraitFatPtrShape(st) {
		return true
	}

	if name := st.Name(); name != "" && cg.structTypeFor(CanonKey(name)) != nil {
		// ADT / cLayoutStruct elements share the same payload-walking
		// gap deepCopyFieldValue documents at the field level.  Fall
		// through to the retain-each path so `[Result[T]]` clone keeps
		// elements shared with the original (matches `let b = a` for
		// these element types) instead of provoking a shallow deep-
		// copy whose scope-exit would corrupt shared variant rc.
		if cg.isDataType(st) || cg.cLayoutStructs[name] {
			return false
		}

		return true
	}

	return false
}

// ensureElemDeepCopyHelper lazily emits the per-element deep-copy
// helper that the array deep-copy loop calls for every slot in the
// freshly-allocated buffer.  The helper signature matches
// _tin_foreach_struct_elem_retain's expected callback: a single
// `i8* elem_ptr` argument, void return.  Inside the helper we cast
// to the typed element pointer, load the value, deep-copy it via
// deepCopyFieldValue (the same dispatch tree the per-struct
// generator uses for fields), and store the isolated value back.
// Cached by element-type key so [Inner][3] in one struct and
// [Inner] in another share the same helper definition.
func (cg *CodeGen) ensureElemDeepCopyHelper(elemT irtypes.Type) *ir.Func {
	key := cg.elemTypeKey(elemT)
	if fn, ok := cg.elemDeepCopyHelpers[key]; ok {
		return fn
	}

	fnName := "__tin_deepcopy_" + key + "_elem"
	param := ir.NewParam("elem_ptr", irtypes.I8Ptr)
	fn := cg.activeModule().NewFunc(fnName, irtypes.Void, param)
	fn.Linkage = enum.LinkageWeakODR
	// Register before generating body so a self-referential element
	// type (e.g. `[Tree]` field of `struct Tree`) terminates on the
	// cached lookup instead of recursing forever in the generator.
	cg.elemDeepCopyHelpers[key] = fn

	entry := fn.NewBlock("entry")
	typedPtr := entry.NewBitCast(param, irtypes.NewPointer(elemT))
	loaded := entry.NewLoad(elemT, typedPtr)
	copied := cg.deepCopyFieldValue(entry, loaded, elemT)
	entry.NewStore(copied, typedPtr)
	entry.NewRet(nil)

	return fn
}
