package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) ensurePrintf() *ir.Func {
	if cg.printfFn != nil {
		return cg.printfFn
	}

	cg.printfFn = cg.ensureExternDecl("printf", irtypes.I32,
		[]*ir.Param{ir.NewParam("format", irtypes.I8Ptr)}, true)

	return cg.printfFn
}

// ensureMalloc declares malloc if not already done.
func (cg *CodeGen) ensureMalloc() *ir.Func {
	if cg.mallocFn != nil {
		return cg.mallocFn
	}

	cg.mallocFn = cg.ensureExternDecl("malloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.mallocFn
}

// ensureFree declares free if not already done.
func (cg *CodeGen) ensureFree() *ir.Func {
	if cg.freeFn != nil {
		return cg.freeFn
	}

	cg.freeFn = cg.ensureExternDecl("free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.freeFn
}

// ARC helpers

// ensureRCAlloc returns the allocator the current function should use.
// When curFnSyncLocal is set (the call-graph analyzer proved the
// enclosing function is not reachable from any {#async} root) it
// routes to _tin_rc_alloc_local which starts blocks at shared=0 so
// the biased retain/release path can use non-atomic ops.  Otherwise
// it falls back to the conservative _tin_rc_alloc which starts at
// shared=1.  The runtime path is identical from the codegen's
// perspective (same i8* return shape); the difference shows up only
// in the runtime's biased-RC fast path.
func (cg *CodeGen) ensureRCAlloc() *ir.Func {
	if cg.curFnSyncLocal {
		return cg.ensureRCAllocLocal()
	}

	if cg.rcAllocFn != nil {
		return cg.rcAllocFn
	}

	cg.rcAllocFn = cg.ensureExternDecl("_tin_rc_alloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.rcAllocFn
}

// ensureRCAllocLocal lazily declares _tin_rc_alloc_local(size i64) i8*.
// Allocates with shared=0 -- caller must be confident the block stays
// confined to the current fiber.
func (cg *CodeGen) ensureRCAllocLocal() *ir.Func {
	if cg.rcAllocLocalFn != nil {
		return cg.rcAllocLocalFn
	}

	cg.rcAllocLocalFn = cg.ensureExternDecl("_tin_rc_alloc_local", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.rcAllocLocalFn
}

// ensureMakeShared lazily declares _tin_make_shared(ptr i8*).
// Emitted at any site where a previously-local block crosses a
// fiber boundary -- spawn capture, channel send, global store, etc.
func (cg *CodeGen) ensureMakeShared() *ir.Func {
	if cg.makeSharedFn != nil {
		return cg.makeSharedFn
	}

	cg.makeSharedFn = cg.ensureExternDecl("_tin_make_shared", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.makeSharedFn
}

// ensureIsManaged lazily declares _tin_is_managed(ptr i8*) -> i32.
// Returns nonzero when the pointer was allocated through Tin's arena.
// Used by per-struct release helpers (`Foo__release_ptr`) and by
// codegen's _tin_retain_ptr / _tin_release_ptr emission sites to
// short-circuit ARC ops on pointers from outside the Tin runtime.
func (cg *CodeGen) ensureIsManaged() *ir.Func {
	if cg.isManagedFn != nil {
		return cg.isManagedFn
	}

	cg.isManagedFn = cg.ensureExternDecl("_tin_is_managed", irtypes.I32,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.isManagedFn
}

// ensureRetainPtr lazily declares _tin_retain_ptr(ptr i8*).
// Provenance-aware retain entry point.  Codegen routes ARC ops on
// user-pointer types (*T) here; ops on data pointers continue using
// _tin_retain directly because those callers know their source.
func (cg *CodeGen) ensureRetainPtr() *ir.Func {
	if cg.retainPtrFn != nil {
		return cg.retainPtrFn
	}

	cg.retainPtrFn = cg.ensureExternDecl("_tin_retain_ptr", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.retainPtrFn
}

// ensureReleasePtr lazily declares _tin_release_ptr(ptr i8*).
// Provenance-aware release; mirror of ensureRetainPtr.
func (cg *CodeGen) ensureReleasePtr() *ir.Func {
	if cg.releasePtrFn != nil {
		return cg.releasePtrFn
	}

	cg.releasePtrFn = cg.ensureExternDecl("_tin_release_ptr", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.releasePtrFn
}

// ensureRetain lazily declares _tin_retain(ptr i8*).
func (cg *CodeGen) ensureRetain() *ir.Func {
	if cg.retainFn != nil {
		return cg.retainFn
	}

	cg.retainFn = cg.ensureExternDecl("_tin_retain", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.retainFn
}

// ensureRelease lazily declares _tin_release(ptr i8*).
func (cg *CodeGen) ensureRelease() *ir.Func {
	if cg.releaseFn != nil {
		return cg.releaseFn
	}

	cg.releaseFn = cg.ensureExternDecl("_tin_release", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.releaseFn
}

// ensureCLayoutRetain lazily declares _tin_retain_clayout(ptr i8*, flags i32).
// Wraps _tin_retain with a flags check: if bit 0 of flags is set, the
// wrapper is borrowed (c_data_ptr points outside its rc-block) and the
// call no-ops without touching memory at ptr.
func (cg *CodeGen) ensureCLayoutRetain() *ir.Func {
	if cg.clayoutRetainFn != nil {
		return cg.clayoutRetainFn
	}

	cg.clayoutRetainFn = cg.ensureExternDecl("_tin_retain_clayout", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("ptr", irtypes.I8Ptr),
			ir.NewParam("flags", irtypes.I32),
		}, false)

	return cg.clayoutRetainFn
}

// ensureCLayoutRelease lazily declares _tin_release_clayout(ptr i8*, flags i32).
// Borrowed-flag mirror of ensureCLayoutRetain for the release side.
func (cg *CodeGen) ensureCLayoutRelease() *ir.Func {
	if cg.clayoutReleaseFn != nil {
		return cg.clayoutReleaseFn
	}

	cg.clayoutReleaseFn = cg.ensureExternDecl("_tin_release_clayout", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("ptr", irtypes.I8Ptr),
			ir.NewParam("flags", irtypes.I32),
		}, false)

	return cg.clayoutReleaseFn
}

// ensureReleaseStruct lazily declares _tin_release_struct(ptr i8*) i64.
// Returns 1 if the block was freed (was the last reference), 0 otherwise.
func (cg *CodeGen) ensureReleaseStruct() *ir.Func {
	if cg.releaseStructFn != nil {
		return cg.releaseStructFn
	}

	cg.releaseStructFn = cg.ensureExternDecl("_tin_release_struct", irtypes.I64,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)

	return cg.releaseStructFn
}

// ensureReleaseFatElemArray lazily declares _tin_release_fat_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each fat-ptr element then frees.
func (cg *CodeGen) ensureReleaseFatElemArray() *ir.Func {
	if cg.releaseFatElemArrayFn != nil {
		return cg.releaseFatElemArrayFn
	}

	cg.releaseFatElemArrayFn = cg.ensureExternDecl("_tin_release_fat_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseFatElemArrayFn
}

// ensureReleaseAnyElemArray lazily declares _tin_release_any_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each `any` element then frees.
func (cg *CodeGen) ensureReleaseAnyElemArray() *ir.Func {
	if cg.releaseAnyElemArrayFn != nil {
		return cg.releaseAnyElemArrayFn
	}

	cg.releaseAnyElemArrayFn = cg.ensureExternDecl("_tin_release_any_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseAnyElemArrayFn
}

// ensureReleaseFnElemArray lazily declares _tin_release_fn_elem_array(data i8*, count i64).
// Decrements the outer array RC; when RC hits 0, releases each closure env then frees.
func (cg *CodeGen) ensureReleaseFnElemArray() *ir.Func {
	if cg.releaseFnElemArrayFn != nil {
		return cg.releaseFnElemArrayFn
	}

	cg.releaseFnElemArrayFn = cg.ensureExternDecl("_tin_release_fn_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releaseFnElemArrayFn
}

// ensureReleaseClosure lazily declares _tin_release_closure(env i8*).
// Decrements the closure env RC; when RC hits 0, calls the per-closure dtor
// (stored at env field 0) to release RC-tracked captures, then frees the block.
func (cg *CodeGen) ensureReleaseClosure() *ir.Func {
	if cg.releaseClosureFn != nil {
		return cg.releaseClosureFn
	}

	cg.releaseClosureFn = cg.ensureExternDecl("_tin_release_closure", irtypes.Void,
		[]*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}, false)

	return cg.releaseClosureFn
}

// ensureReleaseAny lazily declares _tin_release_any(tag i32, data i8*).
// For anyTagFn (5): also releases the closure env before freeing the data block.
// For all other tags: equivalent to _tin_release(data).
func (cg *CodeGen) ensureReleaseAny() *ir.Func {
	if cg.releaseAnyFn != nil {
		return cg.releaseAnyFn
	}

	cg.releaseAnyFn = cg.ensureExternDecl("_tin_release_any", irtypes.Void,
		[]*ir.Param{ir.NewParam("tag", irtypes.I32), ir.NewParam("data", irtypes.I8Ptr)}, false)

	return cg.releaseAnyFn
}

// ensureForeachStructElemRelease lazily declares
// _tin_foreach_struct_elem_release(data i8*, count i64, elem_size i64, release_fn i8*).
// The C function atomically decrements the outer array RC; when RC hits 0, calls
// release_fn on each element (in-place pointer) then frees the buffer.
func (cg *CodeGen) ensureForeachStructElemRelease() *ir.Func {
	if cg.foreachStructElemReleaseFn != nil {
		return cg.foreachStructElemReleaseFn
	}

	cg.foreachStructElemReleaseFn = cg.ensureExternDecl("_tin_foreach_struct_elem_release", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("data", irtypes.I8Ptr),
			ir.NewParam("count", irtypes.I64),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("release_fn", irtypes.I8Ptr),
		}, false)

	return cg.foreachStructElemReleaseFn
}

// ensureForeachFixedElemRelease lazily declares
// _tin_foreach_fixed_elem_release(data i8*, count i64, elem_size i64, release_fn i8*).
// Inline-array sibling of the struct-elem helper: the buffer is NOT an RC
// block, so the function unconditionally walks each slot and calls release_fn.
// Used for scope-exit / struct-field release of [T; N] whose elements own RC.
func (cg *CodeGen) ensureForeachFixedElemRelease() *ir.Func {
	if cg.foreachFixedElemReleaseFn != nil {
		return cg.foreachFixedElemReleaseFn
	}

	cg.foreachFixedElemReleaseFn = cg.ensureExternDecl("_tin_foreach_fixed_elem_release", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("data", irtypes.I8Ptr),
			ir.NewParam("count", irtypes.I64),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("release_fn", irtypes.I8Ptr),
		}, false)

	return cg.foreachFixedElemReleaseFn
}

// ensureReleasePtrElemArray lazily declares _tin_release_ptr_elem_array(data i8*, count i64).
// Decrements the outer buffer RC; when RC hits 0, calls _tin_release on each pointer
// element and frees the buffer.  All pointer elements must be heap-allocated.
func (cg *CodeGen) ensureReleasePtrElemArray() *ir.Func {
	if cg.releasePtrElemArrayFn != nil {
		return cg.releasePtrElemArrayFn
	}

	cg.releasePtrElemArrayFn = cg.ensureExternDecl("_tin_release_ptr_elem_array", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.releasePtrElemArrayFn
}

// Element retain helpers (for ++ concat from non-temporary sources)

func (cg *CodeGen) ensureRetainPtrElems() *ir.Func {
	if cg.retainPtrElemsFn != nil {
		return cg.retainPtrElemsFn
	}

	cg.retainPtrElemsFn = cg.ensureExternDecl("_tin_retain_ptr_elems", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.retainPtrElemsFn
}

func (cg *CodeGen) ensureRetainFatElems() *ir.Func {
	if cg.retainFatElemsFn != nil {
		return cg.retainFatElemsFn
	}

	cg.retainFatElemsFn = cg.ensureExternDecl("_tin_retain_fat_elems", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.retainFatElemsFn
}

func (cg *CodeGen) ensureRetainFnElems() *ir.Func {
	if cg.retainFnElemsFn != nil {
		return cg.retainFnElemsFn
	}

	cg.retainFnElemsFn = cg.ensureExternDecl("_tin_retain_fn_elems", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.retainFnElemsFn
}

func (cg *CodeGen) ensureRetainAnyElems() *ir.Func {
	if cg.retainAnyElemsFn != nil {
		return cg.retainAnyElemsFn
	}

	cg.retainAnyElemsFn = cg.ensureExternDecl("_tin_retain_any_elems", irtypes.Void,
		[]*ir.Param{ir.NewParam("data", irtypes.I8Ptr), ir.NewParam("count", irtypes.I64)}, false)

	return cg.retainAnyElemsFn
}

func (cg *CodeGen) ensureForeachStructElemRetain() *ir.Func {
	if cg.foreachStructElemRetainFn != nil {
		return cg.foreachStructElemRetainFn
	}

	cg.foreachStructElemRetainFn = cg.ensureExternDecl("_tin_foreach_struct_elem_retain", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("data", irtypes.I8Ptr),
			ir.NewParam("count", irtypes.I64),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("retain_fn", irtypes.I8Ptr),
		}, false)

	return cg.foreachStructElemRetainFn
}

// ensureElemRetainHelper returns (or generates) a private IR function that,
// given a pointer to one element of type t, retains its ARC-tracked fields.
// This mirrors ensureElemReleaseHelper but calls retain instead of release.
func (cg *CodeGen) ensureElemRetainHelper(t irtypes.Type) *ir.Func {
	key := cg.elemTypeKey(t)
	if fn, ok := cg.elemRetainHelpers[key]; ok {
		return fn
	}

	name := "__tin_retain_elem_" + key
	param := ir.NewParam("elem", irtypes.I8Ptr)
	fn := cg.activeModule().NewFunc(name, irtypes.Void, param)
	fn.Linkage = enum.LinkageWeakODR
	// Pre-register to handle recursive types.
	cg.elemRetainHelpers[key] = fn

	entryBlock := fn.NewBlock("entry")
	elemI8 := fn.Params[0]

	savedScope := cg.curScope
	cg.curScope = cg.moduleScope

	if _, isPtr := t.(*irtypes.PointerType); isPtr {
		// Pointer element: retain the pointer value itself (it's an ARC heap ptr).
		entryBlock.NewCall(cg.ensureRetain(), elemI8)
	} else {
		// Load the element value and call emitRetain on it.
		typedPtr := entryBlock.NewBitCast(elemI8, irtypes.NewPointer(t))
		elemVal := entryBlock.NewLoad(t, typedPtr)
		cg.emitRetain(entryBlock, elemVal)
	}

	cg.curScope = savedScope

	entryBlock.NewRet(nil)

	return fn
}

// emitRetainElemSlice emits calls to retain each element in [data, data+count)
// for arrays shared by a non-temporary source after ++ concatenation.
// dataI8Ptr must point to the start of the element slice in the new buffer.
func (cg *CodeGen) emitRetainElemSlice(block *ir.Block, dataI8Ptr value.Value, count value.Value, elemT irtypes.Type) {
	if isAnyType(elemT) {
		block.NewCall(cg.ensureRetainAnyElems(), dataI8Ptr, count)

		return
	}

	if isFatFnPtr(elemT) {
		block.NewCall(cg.ensureRetainFnElems(), dataI8Ptr, count)

		return
	}

	if isStringType(elemT) || isFatArrayPtr(elemT) {
		block.NewCall(cg.ensureRetainFatElems(), dataI8Ptr, count)

		return
	}

	if _, isPtr := elemT.(*irtypes.PointerType); isPtr {
		block.NewCall(cg.ensureRetainPtrElems(), dataI8Ptr, count)

		return
	}

	if cg.elemNeedsRelease(elemT) {
		helper := cg.ensureElemRetainHelper(elemT)
		elemSize := cg.llvmSizeOf(block, elemT)
		helperI8 := block.NewBitCast(helper, irtypes.I8Ptr)
		block.NewCall(cg.ensureForeachStructElemRetain(), dataI8Ptr, count, elemSize, helperI8)
	}
}

// elemTypeKey returns a stable unique string key for any LLVM type, used to
// cache per-type element release helpers.
func (cg *CodeGen) elemTypeKey(t irtypes.Type) string {
	switch v := t.(type) {
	case *irtypes.StructType:
		if v.Name() != "" {
			return v.Name()
		}

		if isFatArrayPtr(t) {
			pt := v.Fields[0].(*irtypes.PointerType)

			return "fatarray__" + cg.elemTypeKey(pt.ElemType)
		}

		return fmt.Sprintf("anon_%p", t)
	case *irtypes.PointerType:
		return "ptr__" + cg.elemTypeKey(v.ElemType)
	case *irtypes.IntType:
		return fmt.Sprintf("i%d", v.BitSize)
	case *irtypes.FloatType:
		return v.Kind.String()
	default:
		return fmt.Sprintf("t%p", t)
	}
}

// ensureElemReleaseHelper lazily generates (or returns) a private IR function
// `__tin_release_<key>_elem(i8* elem_ptr)` that loads an element from the
// given pointer and calls emitRelease on it.  Works for any element type:
// named structs, fat arrays (nested arrays like [[T]]), and raw pointers (*T).
//
// The helper is registered before its body is generated so that self-referential
// types (e.g. json::Value containing [Value]) don't produce infinite recursion.
func (cg *CodeGen) ensureElemReleaseHelper(t irtypes.Type) *ir.Func {
	key := cg.elemTypeKey(t)
	if fn, ok := cg.elemReleaseHelpers[key]; ok {
		return fn
	}

	helperName := "__tin_release_" + key + "_elem"
	param := ir.NewParam("elem_ptr", irtypes.I8Ptr)
	fn := cg.activeModule().NewFunc(helperName, irtypes.Void, param)
	fn.Linkage = enum.LinkageWeakODR

	// Register BEFORE generating the body to break potential recursion.
	cg.elemReleaseHelpers[key] = fn

	entry := fn.NewBlock("entry")

	typedPtr := entry.NewBitCast(param, irtypes.NewPointer(t))

	if pt, isPtr := t.(*irtypes.PointerType); isPtr {
		// Pointer element: the stored value is the ARC data ptr itself.
		// Load the pointer value, then release the inner struct's RC fields
		// (if any) before freeing the outer block.
		ptrVal := entry.NewLoad(t, typedPtr)

		innerType := pt.ElemType
		if cg.elemNeedsRelease(innerType) {
			// Inner struct has RC fields: load it and release them first.
			innerVal := entry.NewLoad(innerType, ptrVal)

			savedFn := cg.curFn
			savedScope := cg.curScope
			cg.curFn = fn
			cg.curScope = cg.moduleScope

			cg.emitRelease(entry, innerVal)

			cg.curFn = savedFn
			cg.curScope = savedScope
		}

		ptrI8 := entry.NewBitCast(ptrVal, irtypes.I8Ptr)
		entry.NewCall(cg.ensureRelease(), ptrI8)
	} else {
		val := entry.NewLoad(t, typedPtr)

		savedFn := cg.curFn
		savedScope := cg.curScope
		cg.curFn = fn
		cg.curScope = cg.moduleScope

		cg.emitRelease(entry, val)

		cg.curFn = savedFn
		cg.curScope = savedScope
	}

	entry.NewRet(nil)

	return fn
}

// emitGenericFatArrayRelease releases a fat array whose elements require
// element-level ARC cleanup beyond a simple _tin_release of their data pointer.
// This covers: named structs with RC fields, nested fat arrays ([[T]]), and
// raw ARC-managed pointers ([*T]).  It calls _tin_foreach_struct_elem_release
// with a compiler-generated per-type helper so each element is properly
// decremented when the last owner drops the array.
func (cg *CodeGen) emitGenericFatArrayRelease(block *ir.Block, val value.Value, elemType irtypes.Type) {
	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)
	dataPtrI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
	elemSize := cg.llvmSizeOf(block, elemType)
	releaseFn := cg.ensureElemReleaseHelper(elemType)
	releaseFnI8 := block.NewBitCast(releaseFn, irtypes.I8Ptr)
	block.NewCall(cg.ensureForeachStructElemRelease(), dataPtrI8, length, elemSize, releaseFnI8)
}

// emitFixedArrayRelease releases each element of a fixed-size array [T; N]
// whose backing storage lives at arrPtr (type *[N x T]).  Used at scope exit
// and inside per-struct release helpers when an outer struct carries an
// inline `[T; N]` field whose elements own RC blocks (e.g. [errors::Err; 4]
// or [string; N]).  Defers to _tin_foreach_fixed_elem_release so the loop is
// shared rather than unrolled per N; that helper, unlike its struct-elem
// sibling, does NOT decrement an outer RC -- the buffer lives in stack /
// struct-field storage owned by the enclosing scope.
func (cg *CodeGen) emitFixedArrayRelease(block *ir.Block, arrPtr value.Value, at *irtypes.ArrayType) {
	dataPtrI8 := block.NewBitCast(arrPtr, irtypes.I8Ptr)
	length := constant.NewInt(irtypes.I64, int64(at.Len))
	elemSize := cg.llvmSizeOf(block, at.ElemType)
	releaseFn := cg.ensureElemReleaseHelper(at.ElemType)
	releaseFnI8 := block.NewBitCast(releaseFn, irtypes.I8Ptr)
	block.NewCall(cg.ensureForeachFixedElemRelease(), dataPtrI8, length, elemSize, releaseFnI8)
}

// emitFixedArrayRetain mirrors emitFixedArrayRelease for retain.  Used when
// a struct value carrying an inline `[T; N]` field is copied (struct param
// entry, struct lit copy) so each owning element slot bumps its RC and the
// caller's matching release stays balanced.  _tin_foreach_struct_elem_retain
// already ignores outer RC headers, so the same runtime helper works for
// both heap and inline storage.
func (cg *CodeGen) emitFixedArrayRetain(block *ir.Block, arrPtr value.Value, at *irtypes.ArrayType) {
	dataPtrI8 := block.NewBitCast(arrPtr, irtypes.I8Ptr)
	length := constant.NewInt(irtypes.I64, int64(at.Len))
	elemSize := cg.llvmSizeOf(block, at.ElemType)
	retainFn := cg.ensureElemRetainHelper(at.ElemType)
	retainFnI8 := block.NewBitCast(retainFn, irtypes.I8Ptr)
	block.NewCall(cg.ensureForeachStructElemRetain(), dataPtrI8, length, elemSize, retainFnI8)
}

// staticCallIRName collapses a FieldAccess of the shape `Type.method` or
// `Type[T,U].method` (or qualified `pkg::Type[T].method`) into the IR
// function name the static method was emitted under, so callers can
// look it up in registries like heapPromotingFns.
