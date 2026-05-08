package codegen

// runtime.go - ARC (automatic reference counting) helpers, string builders,
// global string constants, and lazily-declared runtime/C functions.

import (
	"crypto/sha1"
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Basic C runtime declarations

// ensurePrintf declares printf if not already done.
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

// ensureRCAlloc lazily declares _tin_rc_alloc(size i64) i8*.
func (cg *CodeGen) ensureRCAlloc() *ir.Func {
	if cg.rcAllocFn != nil {
		return cg.rcAllocFn
	}

	cg.rcAllocFn = cg.ensureExternDecl("_tin_rc_alloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	return cg.rcAllocFn
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

// staticCallIRName collapses a FieldAccess of the shape `Type.method` or
// `Type[T,U].method` (or qualified `pkg::Type[T].method`) into the IR
// function name the static method was emitted under, so callers can
// look it up in registries like heapPromotingFns.
func (cg *CodeGen) staticCallIRName(fn *ast.FieldAccess) string {
	bareName, typeArg := cg.tryResolveStructTypeName(fn.Expr)
	if bareName == "" {
		return ""
	}

	concrete := bareName

	if typeArg != "" {
		parts := strings.Split(typeArg, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}

		concrete = bareName + "__" + strings.Join(parts, "__")
	}

	return concrete + "_" + fn.Field
}

// bindingOwnsHeapIfaceData reports whether s's let-binding holds a *Trait
// fat-ptr value whose `data` field is an escape-promoted heap block. Two
// shapes qualify:
//
//  1. Direct: `let p *Trait = &b` where b is in cg.curFnEscapingVars.
//     buildPtrToTraitBorrow heap-allocs the iface here; b's heap block
//     becomes iface.data.
//  2. Forwarded: `let s = make()` where make() was recorded as
//     fnReturnsOwningIface -- make's body created an owning iface and
//     returned it. The flag must hop across the call so the caller's
//     scope-exit can release both the iface and its data.
//
// Returns true on either shape so emitScopeRelease cascades through the
// data field on drop.
//
// The let's type annotation is intentionally NOT consulted: type
// inference often leaves s.Type nil for `let s = make()` so we instead
// rely on the value's shape (AddressOfExpr -> declared trait, CallExpr ->
// callee return type lookup).
func (cg *CodeGen) bindingOwnsHeapIfaceData(s *ast.VarDecl) bool {
	if s == nil || s.Value == nil {
		return false
	}

	switch v := s.Value.(type) {
	case *ast.AddressOfExpr:
		// `let p *Trait = &b`: only meaningful if the declared type is
		// *Trait. Without that we'd be guessing about iface coercion.
		if !cg.declTypeIsTraitPtr(s.Type) {
			return false
		}

		if id, ok := v.Expr.(*ast.Identifier); ok {
			return cg.curFnEscapingVars[id.Name]
		}
	case *ast.CallExpr:
		name := resolveCalleeName(v)
		if name == "" {
			return false
		}

		bare := name
		if idx := strings.LastIndex(bare, "::"); idx >= 0 {
			bare = bare[idx+2:]
		}

		if cg.fnReturnsOwningIface[bare] {
			return true
		}
		// IR-name match (mangled): tries the function as registered in
		// scope to recover the irName the callee was emitted under.
		if cg.curScope != nil {
			if entry, ok := cg.curScope.lookup(bare); ok {
				if f, ok2 := entry.val.(interface{ Name() string }); ok2 {
					if cg.fnReturnsOwningIface[f.Name()] {
						return true
					}
				}
			}
		}
	}

	return false
}

// declTypeIsTraitPtr reports whether te names `*Trait` for some declared
// trait. Used to decide whether the binding's value can sensibly be an
// owning-iface fat ptr.
func (cg *CodeGen) declTypeIsTraitPtr(te ast.TypeExpr) bool {
	pt, ok := te.(*ast.PointerType)
	if !ok {
		return false
	}

	st, ok := pt.Elem.(*ast.SimpleType)
	if !ok {
		return false
	}

	name := st.Name
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		name = name[idx+2:]
	}

	_, isTrait := cg.traits[name]

	return isTrait
}

// markOwningRawPtrField records that fieldName on structName receives an
// owning heap pointer. Triggered by genStructLit (and assignment paths) when
// the value being stored is `&Identifier` and the identifier is in
// cg.curFnEscapingVars -- i.e. the local was already heap-promoted by escape
// analysis and the receiving struct is now the sole owner. The struct's
// release helper consults this map to cascade _tin_release through the
// field on drop.
//
// Only `*T` raw pointer fields where T is NOT itself a Tin struct are
// recorded -- Tin's existing per-struct release machinery already cascades
// through `*TinStruct` fields, RC-tracked fat ptrs (string, fat array,
// any, fn closure), and nested structs.
func (cg *CodeGen) markOwningRawPtrField(structName, fieldName string, valueExpr ast.Node, valueLLType irtypes.Type) {
	if structName == "" || fieldName == "" {
		return
	}

	addr, ok := valueExpr.(*ast.AddressOfExpr)
	if !ok {
		return
	}

	id, ok := addr.Expr.(*ast.Identifier)
	if !ok {
		return
	}

	if !cg.curFnEscapingVars[id.Name] {
		return
	}

	pt, ok := valueLLType.(*irtypes.PointerType)
	if !ok {
		return
	}
	// Tin struct pointer: existing structPtrReleaseFn already cascades.
	if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
		if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
			return
		}
	}

	if cg.structOwningRawPtrFields[structName] == nil {
		cg.structOwningRawPtrFields[structName] = make(map[string]bool)
	}

	cg.structOwningRawPtrFields[structName][fieldName] = true
}

// curFnOwnsStruct reports whether the current function being emitted is a
// method of structName (template or any of its monomorphized instances).
// Used to gate the #closed struct-literal check: only the struct's own
// methods may construct it directly.
func (cg *CodeGen) curFnOwnsStruct(structName string) bool {
	if cg.curFn == nil {
		return false
	}

	fnName := cg.curFn.Name()
	// The IR-name produced by methodScopeKey is "<StructName>_<methodName>"
	// for plain methods or "<StructName>_<traitKey>_<methodName>" for
	// trait-qualified ones. After monomorphization the struct name is
	// "<Bare>__<typeArgs>" (Bare carries the same #closed tag in noCopyStructs/
	// closedStructs since genStructDecl is re-run on the concrete decl).
	// "<StructName>$coro" is the async-method coro variant; trim the suffix.
	fnName = strings.TrimSuffix(fnName, "$coro")

	prefix := structName + "_"
	if strings.HasPrefix(fnName, prefix) {
		return true
	}
	// Bare (template) name match: e.g. fn "RcCell_alloc" inside a still-
	// generic body, before monomorphization renames it.
	if idx := strings.Index(structName, "__"); idx >= 0 {
		bare := structName[:idx]
		if strings.HasPrefix(fnName, bare+"_") {
			return true
		}
	}

	return false
}

// noCopyValueTypeName resolves te through type aliases and reports the bare
// struct name when te names a #no_copy struct in *value* (non-pointer) form.
// Pointer-to-no-copy is fine (pointer copies are RC-tracked retains), so a
// PointerType immediately returns "". Used to reject #no_copy values in
// let-bindings, function params, return types, and struct fields.
func (cg *CodeGen) noCopyValueTypeName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case nil:
		return ""
	case *ast.PointerType:
		return ""
	case *ast.ArrayType:
		return ""
	case *ast.SimpleType:
		name := t.Name
		// Walk alias chain.
		for i := 0; i < 32; i++ {
			if cg.noCopyStructs[name] {
				return name
			}

			alias, ok := cg.typeAliases[name]
			if !ok {
				break
			}

			st, ok2 := alias.(*ast.SimpleType)
			if !ok2 {
				return cg.noCopyValueTypeName(alias)
			}

			if st.Name == name {
				break
			}

			name = st.Name
		}
		// Qualified package name (foo::Bar): strip prefix and try again.
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			return cg.noCopyValueTypeName(&ast.SimpleType{Name: name[idx+2:]})
		}

		return ""
	case *ast.GenericType:
		concrete := cg.typeExprCanonicalKey(t)
		if cg.noCopyStructs[concrete] {
			return concrete
		}
		// Template name is registered under multiple keys depending on
		// the package context where the decl was processed. Try the
		// bare name first (covers user-level decls), then the package-
		// qualified key, then sweep for any "<pkg>__<bare>" entry as
		// a final fallback so pkg::Generic[T] field types in OTHER
		// packages still trip the check.
		bare := t.Name
		if idx := strings.LastIndex(bare, "::"); idx >= 0 {
			bare = bare[idx+2:]
		}

		if cg.noCopyStructs[bare] {
			return concrete
		}

		if pkgKey := cg.pkgStructKey(bare); pkgKey != bare && cg.noCopyStructs[pkgKey] {
			return concrete
		}

		suffix := "__" + bare
		for k := range cg.noCopyStructs {
			if strings.HasSuffix(k, suffix) {
				return concrete
			}
		}
	}

	return ""
}

// isBadFatPtrArithmetic reports whether op applied to operands of types lt/rt
// would silently fall through to an integer arith on a fat-pointer struct
// -- `string + string` and the like. The fat-pointer types are LLVM-anonymous
// structs (`{i8*, i64}`) so they slip past isStructType's user-struct check
// and get fed to NewAdd, which clang rejects at the IR level. Catching the
// shape here turns it into a positioned Tin diagnostic.
func (cg *CodeGen) isBadFatPtrArithmetic(op string, lt, rt irtypes.Type) bool {
	switch op {
	case "+", "-", "*", "/", "%", "&", "|", "^", "<<", ">>":
	default:
		return false
	}

	bad := func(t irtypes.Type) bool {
		return isStringType(t) || isFatArrayPtr(t) || isAnyType(t) || isFatFnPtr(t)
	}

	return bad(lt) || bad(rt)
}

// isRCTrackedType returns true for types whose heap data is ARC-managed:
//   - strings      {i8*, i64}           - ptr is either immortal (-1 sentinel) or rc-alloc'd
//   - fat arrays   {T*,  i64}           - ptr is always rc-alloc'd
//   - any          {i32, i8*}           - ptr is rc-alloc'd (boxed value)
//   - fat fn ptrs  {fn(i8*,...)*, i8*}  - env (field 1) is rc-alloc'd (null for named-fn wrappers)
func isRCTrackedType(t irtypes.Type) bool {
	return rcKindOf(t) != rcKindNone
}

// RC-tracking kinds emitted by `isrc(T)`. The C runtime (Channel,
// Atomic) reads this to decide where the retainable pointer sits inside
// each value of T, and which release entry-point to use. Keeping the
// kinds here mirrored in runtime/arc.h would be ideal but the runtime C
// uses bare ints; the values are part of the ABI between the compiler
// and the runtime so they MUST NOT be renumbered.
type rcKind int32

const (
	rcKindNone       rcKind = 0 // no RC management needed
	rcKindLeadingPtr rcKind = 1 // string / fat array / trait fat ptr / named struct ptr -- retain ptr at offset 0
	rcKindAny        rcKind = 2 // any: {i32 tag, i8* ptr} -- release via _tin_release_any(tag, ptr@8)
	rcKindFn         rcKind = 3 // fat fn ptr: {fn*, env*} -- release via _tin_release_closure(env@8)
)

// rcKindOf classifies an LLVM type by where its retainable pointer
// (if any) sits inside the value. See rcKind comments.
func rcKindOf(t irtypes.Type) rcKind {
	switch {
	case t == nil:
		return rcKindNone
	case isAnyType(t):
		return rcKindAny
	case isFatFnPtr(t):
		return rcKindFn
	case isStringType(t), isFatArrayPtr(t), isTraitFatPtrShape(t):
		return rcKindLeadingPtr
	}

	return rcKindNone
}

// channelRCKindOf is the channel/atomic-specific variant of rcKindOf.
// It additionally classifies pointer-to-named-struct as leading-ptr so
// Channel[*S] / Atomic[*S] retain the slot on enqueue. The non-channel
// rcKindOf must NOT do this -- many other callers (struct-field ARC
// machinery, scope release) already treat *S correctly via per-struct
// release helpers and would double-free if marked as leading-ptr here.
func channelRCKindOf(t irtypes.Type) rcKind {
	if k := rcKindOf(t); k != rcKindNone {
		return k
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			return rcKindLeadingPtr
		}
	}

	return rcKindNone
}

// isTraitFatPtrShape detects the universal trait fat-pointer struct shape
// `{i8*, ptr-to-named-struct}` whose second field's pointee struct name ends
// in `_vtable`. Used by codegen sites that need to release iface storage
// without access to the full *CodeGen state (e.g. genForIterTrait emitting
// the iter loop's exit-block release).
func isTraitFatPtrShape(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}

	if st.Fields[0] != irtypes.I8Ptr {
		return false
	}

	pt, ok := st.Fields[1].(*irtypes.PointerType)
	if !ok {
		return false
	}

	innerSt, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return false
	}

	return innerSt.Name() != "" && strings.HasSuffix(innerSt.Name(), "_vtable")
}

// emitCallArgRelease releases a temporary call argument after a call returns.
// pre is the argument value before coercion; post is the value after coercion.
// astArg is the corresponding AST expression.
//
// Rules:
//   - boxed-to-any: release the boxed value (fresh _tin_rc_alloc)
//   - copy expressions (identifier, field access, etc.): skip (scope exit owns it)
//   - RC-tracked temporaries (string, array, any, fn): release
//   - *TinStruct pointer temporaries: release via ensureStructPtrReleaseFn to
//     balance any retain performed inside the callee (e.g. storing the pointer
//     in a struct field via a struct literal).
func (cg *CodeGen) emitCallArgRelease(block *ir.Block, astArg ast.Node, pre, post value.Value) {
	if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
		cg.emitRelease(block, post)

		return
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
				if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
					cg.emitRelease(block, pre)
				}
			}
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

// elemNeedsRelease reports whether a scope variable with element type elemType
// requires any ARC or deinit processing at scope exit.  Returns false for
// primitive types (int, float, raw pointers) and for named structs with no RC
// fields, no deinit, and no nested structs - so emitScopeRelease can skip the
// load entirely rather than loading and then emitting nothing.
func (cg *CodeGen) elemNeedsRelease(elemType irtypes.Type) bool {
	switch elemType.(type) {
	case *irtypes.IntType, *irtypes.FloatType, *irtypes.PointerType, *irtypes.ArrayType:
		// Fixed-size arrays ([byte; N] etc.) are value types: never RC-tracked,
		// never need a scope release.
		// Pointer types (*T) are raw addresses; the pointed-to value is released
		// by its owner, not by every scope that borrows the pointer.
		return false
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

		// Owning pointer to a known Tin struct: needs recursive release.
		if pt, ok := ft.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
					return true
				}
			}
		}
	}

	return false
}

// extractRCDataPtr extracts the ARC heap data pointer (i8*) from a
// string, fat-array, or any value.  Returns nil for non-ARC types.
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

	return nil
}

func (cg *CodeGen) walkRCStructFields(block *ir.Block, val value.Value, visit func(value.Value)) {
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
		// just like an inline nested struct.  Only non-weak fields qualify.
		isTinStructPtr := false

		if pt, ok2 := ft.(*irtypes.PointerType); ok2 {
			if innerSt, ok3 := pt.ElemType.(*irtypes.StructType); ok3 && innerSt.Name() != "" {
				_, isTinStructPtr = cg.structTypes[innerSt.Name()]
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

// emitRetain emits a _tin_retain call for an ARC-tracked value.
// For named structs, it also retains any RC-tracked fields.
func (cg *CodeGen) emitRetain(block *ir.Block, val value.Value) {
	cg.emittingARC = true

	defer func() { cg.emittingARC = false }()

	t := val.Type()
	// Closure fat pointer: retain the env field (i8*). _tin_retain handles null env.
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureRetain(), envField)

		return
	}

	rcPtr := cg.extractRCDataPtr(block, val, t)
	if rcPtr != nil {
		block.NewCall(cg.ensureRetain(), rcPtr)

		return
	}
	// ADT value: tag-dispatched retain walks the active variant's payload.
	// The outer struct's declared fields {i32, i8, [N x i8]} hide the real
	// payload layout from walkRCStructFields, so we emit a dispatch here.
	if cg.isDataType(t) {
		cg.emitDataValueRetain(block, val)

		return
	}
	// Named struct: retain RC-tracked fields so copies are independent.
	// Use emitStructFieldRetain for each field to also handle *TinStruct pointer fields.
	cg.walkRCStructFields(block, val, func(fieldVal value.Value) {
		cg.emitStructFieldRetain(block, fieldVal)
	})
}

// emitStructFieldRetain retains a single struct field value.
// Unlike emitRetain (which is also called for function parameter pointers), this
// function is only called for fields loaded from a struct value being copied.
// It explicitly handles owning *TinStruct pointer fields that emitRetain skips.
func (cg *CodeGen) emitStructFieldRetain(block *ir.Block, fieldVal value.Value) {
	t := fieldVal.Type()
	// Owning pointer to a known Tin struct: retain via _tin_retain.
	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
				ptrI8 := block.NewBitCast(fieldVal, irtypes.I8Ptr)
				block.NewCall(cg.ensureRetain(), ptrI8)

				return
			}
		}
	}

	cg.emitRetain(block, fieldVal)
}

// emitRelease emits a _tin_release call for an ARC-tracked value.
// For named structs it first calls deinit (if defined), then releases any
// RC-tracked fields and recurses into nested struct fields.
func (cg *CodeGen) emitRelease(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, false)
}

// emitReleaseNoDeinit is like emitRelease but suppresses the deinit call for
// the top-level value.  Used when releasing the `this` parameter of a deinit
// method itself to prevent infinite recursion.  Field releases still recurse
// normally (nested struct fields call their own deinit as usual).
func (cg *CodeGen) emitReleaseNoDeinit(block *ir.Block, val value.Value) {
	cg.emitReleaseInner(block, val, true)
}

func (cg *CodeGen) emitReleaseInner(block *ir.Block, val value.Value, skipDeinit bool) {
	cg.emittingARC = true

	defer func() { cg.emittingARC = false }()

	t := val.Type()
	// Owning pointer to a known Tin struct: delegate to the null-safe per-struct
	// release helper so that its RC-tracked and nested-struct-pointer fields are
	// recursively released before the block itself is freed.
	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			if cg.isDataType(innerSt) {
				relFn := cg.ensureDataPtrReleaseFn(innerSt.Name(), innerSt)
				if relFn != nil {
					block.NewCall(relFn, val)

					return
				}
			}

			relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
			block.NewCall(relFn, val)

			return
		}
	}
	// Closure fat pointer: release the env via _tin_release_closure (null-safe).
	if isFatFnPtr(t) {
		envField := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureReleaseClosure(), envField)

		return
	}
	// any value: use tag-aware release that also handles closure envs inside.
	if isAnyType(t) {
		tag := block.NewExtractValue(val, 0)
		data := block.NewExtractValue(val, 1)
		block.NewCall(cg.ensureReleaseAny(), tag, data)

		return
	}
	// Fat array with RC-tracked element type: use a combined release that
	// decrements the outer RC and, only when it hits 0, releases each element
	// and frees the outer block.  This prevents double-free when the array is
	// shared (e.g. struct copy): the copy's release decrements RC without
	// touching elements; only the last owner's release triggers element cleanup.
	if isFatArrayPtr(t) {
		if st, ok := t.(*irtypes.StructType); ok {
			if pt, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
				elemType := pt.ElemType
				// Dispatch to the most specific release function for the element type.
				// All combined functions handle the outer buffer free - don't also
				// call _tin_release after returning.
				dataPtr := block.NewExtractValue(val, 0)
				length := block.NewExtractValue(val, 1)
				dataPtrI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)

				if isAnyType(elemType) {
					// [any]: tag-aware release handles closure envs
					block.NewCall(cg.ensureReleaseAnyElemArray(), dataPtrI8, length)

					return
				}

				if isFatFnPtr(elemType) {
					// [fn]: closure env is at field 1 (offset 8)
					block.NewCall(cg.ensureReleaseFnElemArray(), dataPtrI8, length)

					return
				}

				if isStringType(elemType) {
					// [string]: string data pointer is at field 0 - optimized path
					block.NewCall(cg.ensureReleaseFatElemArray(), dataPtrI8, length)

					return
				}

				if pt2, isPtr := elemType.(*irtypes.PointerType); isPtr {
					// [*T]: check if the inner type T has RC fields that need
					// deep release (load T, release its fields, free the block).
					if cg.elemNeedsRelease(pt2.ElemType) {
						cg.emitGenericFatArrayRelease(block, val, elemType)

						return
					}

					// T has no RC fields: raw pointer release suffices.
					block.NewCall(cg.ensureReleasePtrElemArray(), dataPtrI8, length)

					return
				}

				if cg.elemNeedsRelease(elemType) {
					// [[T]] nested arrays or structs with RC fields: per-element helper
					// correctly recurses into inner element types at any depth.
					cg.emitGenericFatArrayRelease(block, val, elemType)

					return
				}
			}
		}
	}

	rcPtr := cg.extractRCDataPtr(block, val, t)
	if rcPtr != nil {
		block.NewCall(cg.ensureRelease(), rcPtr)

		return
	}
	// Named struct: call deinit (if defined) before releasing RC fields so
	// the user-supplied cleanup runs while fields are still valid.
	if !skipDeinit {
		structName := cg.typeNameOf(val.Type())
		if structName != "" && cg.curScope != nil {
			deinitName := structName + "_deinit"
			if entry, ok := cg.curScope.lookup(deinitName); ok {
				if fn, ok2 := entry.val.(*ir.Func); ok2 {
					// Guard: the deinit's `this` param type must match
					// the value being released. Without this we silently
					// fall into a function that bit-reinterprets `val`
					// as a different struct (a real bug seen with
					// Atomic[t]'s dual-impl deinit when a field load
					// has type T but the struct has been monomorphized
					// to Atomic__T and the static lookup picks the
					// outer deinit).
					if len(fn.Sig.Params) > 0 && fn.Sig.Params[0].Equal(val.Type()) {
						args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
						block.NewCall(fn, args...)
					}
				}
			}
			// Call chained trait deinit methods (for traits that also define fn deinit).
			for _, traitDeinitFn := range cg.traitChainedDeinits[structName] {
				// Suppress coerceToTrait's deferred scope-exit release;
				// we emit a tighter release immediately after the call.
				prevSuppress := cg.suppressIfaceScopeRelease
				cg.suppressIfaceScopeRelease = true

				args := cg.adaptArgs(block, []value.Value{val}, traitDeinitFn.Sig)
				cg.suppressIfaceScopeRelease = prevSuppress

				block.NewCall(traitDeinitFn, args...)
				// Release iface temporaries adaptArgs constructed via
				// coerceToTrait; see twin comment in genStructLit.
				for _, a := range args {
					if isTraitFatPtrShape(a.Type()) {
						dataField := block.NewExtractValue(a, 0)
						block.NewCall(cg.ensureRelease(), dataField)
					}
				}
			}
		}
	}
	// ADT value: dispatch on tag and release the active variant's owning
	// payload fields. The outer struct holds only {i32, i8, [N x i8]} so the
	// generic walkRCStructFields below can't see inside the payload.
	if cg.isDataType(t) {
		cg.emitDataValueRelease(block, val)

		return
	}

	// Release RC-tracked fields and recurse into nested struct fields.
	// Propagate skipDeinit so that parameter-copy teardown does not call deinit
	// on nested struct fields (the caller's emitRelease already handles that).
	cg.walkRCStructFields(block, val, func(fieldVal value.Value) {
		cg.emitReleaseInner(block, fieldVal, skipDeinit)
	})
}

// ensureStructPtrReleaseFn lazily creates (or returns a cached) null-safe pointer
// release function for the named Tin struct:
//
//	define void @{structName}__release_ptr({struct}* %ptr) {
//	entry:
//	  %is_null = icmp eq {struct}* %ptr, null
//	  br i1 %is_null, label %exit, label %do_release
//	do_release:
//	  %val = load {struct}, {struct}* %ptr
//	  ; release RC-tracked fields
//	  %ptr_i8 = bitcast {struct}* %ptr to i8*
//	  call void @_tin_release(i8* %ptr_i8)
//	  br label %exit
//	exit:
//	  ret void
//	}
//
// This is used by emitScopeRelease / emitAllScopeReleases when a scope variable
// has type *T (pointer to a Tin struct) so that a safe release is emitted without
// splitting the caller's basic block.
func (cg *CodeGen) ensureStructPtrReleaseFn(structName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structPtrReleaseFns[structName]; ok {
		return fn
	}

	ptrType := irtypes.NewPointer(st)
	fnName := structName + "__release_ptr"
	fn := cg.activeModule().NewFunc(fnName, irtypes.Void, ir.NewParam("ptr", ptrType))
	// Cache before generating body to handle any hypothetical recursive reference.
	cg.structPtrReleaseFns[structName] = fn

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	releaseChildren := fn.NewBlock("release_children")
	exit := fn.NewBlock("exit")

	// Null guard.
	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load the struct value BEFORE decrementing RC (the block is still valid
	// since we hold a reference). Then atomically decrement RC. If the block
	// was freed (last ref), release child fields from the loaded value.
	structVal := doRelease.NewLoad(st, fn.Params[0])
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)
	doRelease.NewCondBr(isOne, releaseChildren, exit)

	// Block was freed (last reference). Release RC-tracked child fields
	// from the loaded struct value (which is on the stack, still valid).
	cg.emitRelease(releaseChildren, structVal)

	// Trait-iface fat ptr: dispatch via the vtable's data-release
	// thunk (last slot) to call the wrapped concrete struct's
	// release_ptr.  Raw _tin_release would only free the outer block
	// and leak any RC-tracked fields (string / fat-array / nested
	// structs).  Falls back to raw release when the vtable shape
	// doesn't match (defensive: shouldn't happen for well-formed
	// trait fat-ptrs).
	if isTraitFatPtrShape(st) {
		dataField := releaseChildren.NewExtractValue(structVal, 0)
		vtableField := releaseChildren.NewExtractValue(structVal, 1)

		if vtablePtrType, ok := st.Fields[1].(*irtypes.PointerType); ok {
			if vtableSt, ok2 := vtablePtrType.ElemType.(*irtypes.StructType); ok2 && len(vtableSt.Fields) > 0 {
				lastIdx := len(vtableSt.Fields) - 1
				lastFieldType := vtableSt.Fields[lastIdx]

				if lastPt, ok3 := lastFieldType.(*irtypes.PointerType); ok3 {
					if lastFnType, ok4 := lastPt.ElemType.(*irtypes.FuncType); ok4 && len(lastFnType.Params) == 1 && lastFnType.Params[0].Equal(irtypes.I8Ptr) && irtypes.IsVoid(lastFnType.RetType) {
						releaseFnSlot := releaseChildren.NewGetElementPtr(vtableSt, vtableField,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(lastIdx)))
						releaseFn := releaseChildren.NewLoad(lastFieldType, releaseFnSlot)

						releaseChildren.NewCall(releaseFn, dataField)
						releaseChildren.NewBr(exit)

						exit.NewRet(nil)

						return fn
					}
				}
			}
		}
		// Fallback: raw release if vtable shape doesn't match expectation.
		releaseChildren.NewCall(cg.ensureRelease(), dataField)
	}

	releaseChildren.NewBr(exit)

	exit.NewRet(nil)

	return fn
}

// ensureHeapChainReleaseFn lazily generates a null-safe release function for a
// depth-N cLayoutStruct pointer chain written by N*S out-param write-backs.
//
// For depth=1: delegates to ensureStructPtrReleaseFn (existing null-safe helper).
// For depth>1: generates:
//
//	define void @structName__chain_N((N)*S.wrapper* %ptr) {
//	entry:
//	  br i1 (ptr==null), exit, do_release
//	do_release:
//	  inner = load (N-1)*S.wrapper, ptr
//	  call @structName__chain_{N-1}(inner)
//	  call _tin_release(bitcast ptr to i8*)
//	  br exit
//	exit:
//	  ret void
//	}
func (cg *CodeGen) ensureHeapChainReleaseFn(structName string, depth int) *ir.Func {
	if depth == 1 {
		wrapperSt := cg.structTypes[structName]

		return cg.ensureStructPtrReleaseFn(structName, wrapperSt)
	}

	key := fmt.Sprintf("%s__chain_%d", structName, depth)
	if fn, ok := cg.chainReleaseFns[key]; ok {
		return fn
	}

	// Build parameter type: (depth)*S.wrapper
	wrapperSt := cg.structTypes[structName]

	var paramType irtypes.Type = wrapperSt
	for i := 0; i < depth; i++ {
		paramType = irtypes.NewPointer(paramType)
	}

	fn := cg.activeModule().NewFunc(key, irtypes.Void, ir.NewParam("ptr", paramType))
	cg.chainReleaseFns[key] = fn // cache before generating body (handles recursive refs)

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	exit := fn.NewBlock("exit")

	// Null guard.
	isNull := entry.NewICmp(enum.IPredEQ,
		entry.NewBitCast(fn.Params[0], irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load inner (depth-1)*S.wrapper.
	innerType := paramType.(*irtypes.PointerType).ElemType
	innerPtr := doRelease.NewLoad(innerType, fn.Params[0])

	// Recursively release the inner chain.
	innerRelFn := cg.ensureHeapChainReleaseFn(structName, depth-1)
	doRelease.NewCall(innerRelFn, innerPtr)

	// Free this RC block.
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	doRelease.NewCall(cg.ensureRelease(), ptrI8)
	doRelease.NewBr(exit)

	exit.NewRet(nil)

	return fn
}

// cLayoutStructBaseName peels all pointer layers of a Tin AST type and returns
// the base struct name, or "" if the type is not a pointer-to-struct chain.
// E.g. *pvec2 -> "pvec2", **pvec2 -> "pvec2", ***pvec2 -> "pvec2".
func cLayoutStructBaseName(te ast.TypeExpr) string {
	cur := te

	for {
		pt, ok := cur.(*ast.PointerType)
		if !ok {
			return ""
		}

		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			return st.Name
		}

		cur = pt.Elem
	}
}

// emitHeapChainRelease releases a heap-promoted pointer chain of the given depth.
// For depth=1 (*T): loads T, releases T's ARC sub-fields, frees the RC block.
// For depth>1 (**T, ***T, ...): recursively releases inner chains before freeing
// the outer RC block.  This handles nested heap promotion (alloc_nested, etc.).
func (cg *CodeGen) emitHeapChainRelease(block *ir.Block, heapPtr value.Value, depth int) {
	ptrType, ok := heapPtr.Type().(*irtypes.PointerType)
	if !ok {
		return
	}

	elemType := ptrType.ElemType

	// For depth=1 with a named struct type: always use the null-safe per-struct
	// release helper. ensureStructPtrReleaseFn cascades through *TinStruct pointer
	// fields (e.g. a linked-list node's next field) even when elemNeedsRelease
	// returns false - elemNeedsRelease only checks for RC-tracked fat types and
	// nested struct values, not *TinStruct pointer fields.
	if depth == 1 {
		if st, ok2 := elemType.(*irtypes.StructType); ok2 && st.Name() != "" {
			if cg.isDataType(st) {
				if relFn := cg.ensureDataPtrReleaseFn(st.Name(), st); relFn != nil {
					block.NewCall(relFn, heapPtr)

					return
				}
			}

			relFn := cg.ensureStructPtrReleaseFn(st.Name(), st)
			block.NewCall(relFn, heapPtr)

			return
		}
	}

	// When the heap block holds a primitive leaf with no RC sub-fields, skip
	// the load entirely: loading from a potentially-NULL heapPtr would
	// segfault, and emitRelease would be a no-op anyway.  _tin_release is
	// null-safe, so call it directly.
	if depth == 1 && !cg.elemNeedsRelease(elemType) {
		rcI8 := block.NewBitCast(heapPtr, irtypes.I8Ptr)
		block.NewCall(cg.ensureRelease(), rcI8)

		return
	}

	// Load T from the heap block.
	tVal := block.NewLoad(elemType, heapPtr)

	if depth == 1 {
		// Leaf: release T's ARC sub-fields (handles strings/arrays inside structs).
		cg.emitRelease(block, tVal)
	} else {
		// Non-leaf: the loaded value is itself a heap-promoted pointer chain.
		cg.emitHeapChainRelease(block, tVal, depth-1)
	}

	// Free this RC block.
	rcI8 := block.NewBitCast(heapPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureRelease(), rcI8)
}

// emitScopeRelease emits _tin_release for all ARC-tracked variables in scope s
// whose block has not yet been terminated.  Named structs with RC-tracked
// fields are also cleaned up via emitRelease's recursive handling.
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

		// *Trait pointer binding without isHeapOwned: elemNeedsRelease
		// returns false for raw pointer types, so a binding like
		// `let g *Trait = &x as *Trait` would otherwise leak the iface
		// block.  Call its release_ptr explicitly; ensureStructPtr
		// ReleaseFn's iface arm handles the wrapped data on RC=0.
		// Restricted to let/const bindings: function parameters of
		// *Trait are borrows from the caller and must not be released.
		if (entry.declaredLet || entry.declaredConst) && !entry.noDeinit {
			if pt, isPtr := elemType.(*irtypes.PointerType); isPtr {
				if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && isTraitFatPtrShape(innerSt) && innerSt.Name() != "" {
					loaded := block.NewLoad(elemType, entry.val)
					relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
					block.NewCall(relFn, loaded)

					return
				}
			}
		}

		if !cg.elemNeedsRelease(elemType) {
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

			// *Trait pointer binding fallback; see twin in emitScopeRelease.
			if (entry.declaredLet || entry.declaredConst) && !entry.noDeinit {
				if pt, isPtr := elemType.(*irtypes.PointerType); isPtr {
					if innerSt, isStruct := pt.ElemType.(*irtypes.StructType); isStruct && isTraitFatPtrShape(innerSt) && innerSt.Name() != "" {
						loaded := block.NewLoad(elemType, entry.val)
						relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
						block.NewCall(relFn, loaded)

						return
					}
				}
			}

			if !cg.elemNeedsRelease(elemType) {
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

func (cg *CodeGen) ensureMemcpy() *ir.Func {
	if cg.memcpyFn != nil {
		return cg.memcpyFn
	}
	// LLVM intrinsic: declare void @llvm.memcpy.p0i8.p0i8.i64(i8*, i8*, i64, i1)
	f := cg.mod.NewFunc("llvm.memcpy.p0i8.p0i8.i64", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1),
	)
	f.Blocks = nil
	cg.memcpyFn = f

	return f
}

// ensureMemset lazily declares the llvm.memset.p0.i64 intrinsic.
// Used to zero-initialize large fixed-size arrays without generating huge
// aggregate-value stores that crash or hang LLVM's instruction selector.
func (cg *CodeGen) ensureMemset() *ir.Func {
	if cg.memsetFn != nil {
		return cg.memsetFn
	}

	f := cg.mod.NewFunc("llvm.memset.p0.i64", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("val", irtypes.I8),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1),
	)
	f.Blocks = nil
	cg.memsetFn = f

	return f
}

// ensureAnyEq declares _tin_any_eq if not already done.
// Signature: i64 _tin_any_eq({i32, i8*} a, {i32, i8*} b)
func (cg *CodeGen) ensureAnyEq() *ir.Func {
	if cg.anyEqFn != nil {
		return cg.anyEqFn
	}

	anyT := anyFatPtrType()
	cg.anyEqFn = cg.ensureExternDecl("_tin_any_eq", irtypes.I64,
		[]*ir.Param{ir.NewParam("a", anyT), ir.NewParam("b", anyT)}, false)

	return cg.anyEqFn
}

// ensureStrcmp declares strcmp if not already done.
func (cg *CodeGen) ensureStrcmp() *ir.Func {
	if cg.strcmpFn != nil {
		return cg.strcmpFn
	}

	cg.strcmpFn = cg.ensureExternDecl("strcmp", irtypes.I32,
		[]*ir.Param{ir.NewParam("s1", irtypes.I8Ptr), ir.NewParam("s2", irtypes.I8Ptr)}, false)

	return cg.strcmpFn
}

// newGlobalString creates a private unnamed_addr constant for a string,
// returning a pointer to its first byte.  The global is wrapped in a
// { i64, i64, [N x i8] } struct where the first i64 holds TIN_IMMORTAL_RC (-1)
// and the second i64 is padding to match the 16-byte TinRCHdr layout, so that
// _tin_retain / _tin_release treat it as an immortal, never-freed block and
// the data pointer is 16-byte aligned (needed for SIMD boxing).
//
//goland:noinspection GoSnakeCaseUsage
func (cg *CodeGen) newGlobalString(s string) value.Value {
	data := []byte(s)
	data = append(data, 0) // null terminator
	arrType := irtypes.NewArray(uint64(len(data)), irtypes.I8)
	ca := constant.NewCharArray(data)

	// Wrap in { i64, i64, [N x i8] } with immortal ARC header (rc = -1, pad = 0)
	immortalRC := constant.NewInt(irtypes.I64, -1)
	pad := constant.NewInt(irtypes.I64, 0)
	hdrStructType := irtypes.NewStruct(irtypes.I64, irtypes.I64, arrType)
	hdrConst := constant.NewStruct(hdrStructType, immortalRC, pad, ca)

	// Route through activeModule so the string lives in the same per-pkg
	// module that references it. linkonce_odr linkage with a content-
	// hashed symbol name means the LINKER deduplicates identical
	// strings across object boundaries. Without it, per-pkg compile
	// would either (a) link-fail when a fn moves modules between mono
	// instantiations and references a string defined elsewhere, or
	// (b) duplicate every string per pkg with no dedup.
	//
	// The hash is content-only: two `str.N` symbols with the same
	// payload across modules collide on the same symbol name and
	// linkonce_odr keeps one definition. unnamed_addr lets the
	// optimizer merge identical string globals within a module too.
	hash := sha1.Sum([]byte(s))
	symName := fmt.Sprintf("__tin_str_%x", hash[:8])

	if cg.stringPool == nil {
		cg.stringPool = map[*ir.Module]map[string]value.Value{}
	}

	mod := cg.activeModule()

	perMod, ok := cg.stringPool[mod]
	if !ok {
		perMod = map[string]value.Value{}
		cg.stringPool[mod] = perMod
	}

	if cached, ok := perMod[symName]; ok {
		return cached
	}

	g := mod.NewGlobalDef(symName, hdrConst)
	g.Immutable = true
	g.Linkage = enum.LinkageWeakODR
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr
	cg.strCount++

	// GEP: { i64, i64, [N x i8] }* -> [N x i8]* -> i8* (skipping the 16-byte ARC header)
	i32_0 := constant.NewInt(irtypes.I32, 0)
	i32_2 := constant.NewInt(irtypes.I32, 2)
	gep := constant.NewGetElementPtr(hdrStructType, g, i32_0, i32_2, i32_0)
	gep.InBounds = true

	perMod[symName] = gep

	return gep
}

// buildStringFatPtr creates a tin string fat-pointer {i8*, i64} from a literal string.
func (cg *CodeGen) buildStringFatPtr(block *ir.Block, s string) value.Value {
	ptr := cg.newGlobalString(s)
	length := constant.NewInt(irtypes.I64, int64(len(s)))
	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), ptr, 0)

	return block.NewInsertValue(v0, length, 1)
}

// extractStringPtr extracts the i8* data pointer from a tin string fat-ptr.
func (cg *CodeGen) extractStringPtr(block *ir.Block, fatPtr value.Value) value.Value {
	raw := block.NewExtractValue(fatPtr, 0)
	// Null-safety: a zero-initialized string has data=null; treat as empty string "".
	nullPtr := constant.NewNull(irtypes.I8Ptr)
	emptyPtr := cg.newGlobalString("")
	isNull := block.NewICmp(enum.IPredEQ, raw, nullPtr)

	return block.NewSelect(isNull, emptyPtr, raw)
}

// extractStringLen extracts the i64 length from a tin string fat-ptr.
func (cg *CodeGen) extractStringLen(block *ir.Block, fatPtr value.Value) value.Value {
	return block.NewExtractValue(fatPtr, 1)
}

// panic builtin

// ensurePanicFn lazily declares the _tin_panic external function.
func (cg *CodeGen) ensurePanicFn() *ir.Func {
	if cg.tinPanicFn != nil {
		return cg.tinPanicFn
	}

	cg.tinPanicFn = cg.mod.NewFunc("_tin_panic", irtypes.Void,
		ir.NewParam("msg", irtypes.I8Ptr),
	)

	return cg.tinPanicFn
}

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable
// terminator is appended so the block is valid LLVM IR.
func (cg *CodeGen) genBuiltinPanic(block *ir.Block, msgNode ast.Node) (value.Value, error) {
	msg, err := cg.genExpr(block, msgNode)
	if err != nil {
		return nil, err
	}

	var msgPtr value.Value

	t := msg.Type()

	switch {
	case isStringType(t):
		msgPtr = cg.extractStringPtr(block, msg)
	default:
		if strVal, ok := cg.callPrintTrait(block, msg); ok {
			msgPtr = cg.extractStringPtr(block, strVal)
		} else if t == irtypes.I1 {
			msgPtr = block.NewSelect(msg, cg.newGlobalString("true"), cg.newGlobalString("false"))
		} else if irtypes.IsInt(t) {
			arrTy := irtypes.NewArray(64, irtypes.I8)
			buf := block.NewAlloca(arrTy)
			bufPtr := block.NewGetElementPtr(arrTy, buf,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

			var wide value.Value

			if t == irtypes.I64 {
				wide = msg
			} else {
				wide = block.NewSExt(msg, irtypes.I64)
			}

			block.NewCall(cg.ensureSnprintf(), bufPtr, constant.NewInt(irtypes.I64, 64), cg.newGlobalString("%lld"), wide)
			msgPtr = bufPtr
		} else if irtypes.IsFloat(t) {
			arrTy := irtypes.NewArray(64, irtypes.I8)
			buf := block.NewAlloca(arrTy)
			bufPtr := block.NewGetElementPtr(arrTy, buf,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

			var fval value.Value

			if t == irtypes.Double {
				fval = msg
			} else {
				fval = block.NewFPExt(msg, irtypes.Double)
			}

			block.NewCall(cg.ensureSnprintf(), bufPtr, constant.NewInt(irtypes.I64, 64), cg.newGlobalString("%g"), fval)
			msgPtr = bufPtr
		} else {
			msgPtr = block.NewBitCast(msg, irtypes.I8Ptr)
		}
	}

	block.NewCall(cg.ensurePanicFn(), msgPtr)
	// If _tin_panic returns (a deferred function called recover()), all of this
	// function's defer lambdas have already run inside _tin_panic.  Their heap
	// envs were not freed by _tin_panic, so free them here.  This is the only
	// cleanup path for the panic branch; the normal exit path (emitDefers) is
	// never reached when panic() is called directly in the function body.
	for _, env := range cg.pendingDeferEnvs {
		if _, isNull := env.(*constant.Null); !isNull {
			block.NewCall(cg.ensureFree(), env)
		}
	}
	// Also release ARC-tracked scope variables so that e.g. a [any] array
	// allocated before the panic call is freed even when recover() is in play.
	cg.emitAllScopeReleases(block, "")
	// _tin_panic normally calls exit(1) and never returns.  However, when a
	// deferred function calls recover(), _tin_panic returns instead of exiting.
	// We must emit a valid terminator so the IR block is well-formed.
	//
	// Inside a coroutine body we emit the proper coro completion path so that
	// the fiber is marked as done and the coro frame is cleaned up correctly.
	// (A bare `ret` in a presplit coroutine body bypasses llvm.coro.end and
	// leaves the frame in an undefined state when the destroy path is called.)
	// If a subsequent explicit `return` statement is present (e.g. `return 0`
	// after `panic(...)`), its genCoroReturn call will overwrite block.Term with
	// the correct br->coro.final, which is harmless.
	if cg.inCoroFn {
		cg.ensureFiberRuntime()
		// If _tin_panic returns (panic was caught by defer+recover in this coro),
		// complete with the defer-override value if a thunk set one, otherwise
		// the zero value of the declared return type.  Passing nil would leave
		// the fiber result as NULL, causing a null dereference in the outer awaiter.
		cg.emitCoroComplete(block, cg.recoverRetVal(block))
		cg.emitFinalSuspend(block, cg.curCoroFrame)
	} else {
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			block.NewRet(nil)
		} else {
			block.NewRet(cg.zeroValue(retType))
		}
	}

	return nil, nil
}

// ensureSliceSubslice lazily declares _tin_slice_subslice(TinSlice s, i64 start, i64 elem_size) -> TinSlice.
// TinSlice has the same layout as a fat array: { i8*, i64 }.
func (cg *CodeGen) ensureSliceSubslice() *ir.Func {
	if cg.sliceSubsliceFn != nil {
		return cg.sliceSubsliceFn
	}

	sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
	cg.sliceSubsliceFn = cg.mod.NewFunc("_tin_slice_subslice", sliceType,
		ir.NewParam("s", sliceType),
		ir.NewParam("start", irtypes.I64),
		ir.NewParam("elem_size", irtypes.I64),
	)

	return cg.sliceSubsliceFn
}

// ensureSliceConvertInt lazily declares _tin_slice_convert_int(TinSlice s,
// i64 src_sz, i64 tgt_sz, i32 src_signed) -> TinSlice.
// Used by fat-array cross-type coercion to reallocate the buffer and convert
// integer elements from one width to another.
func (cg *CodeGen) ensureSliceConvertInt() *ir.Func {
	if cg.sliceConvertIntFn != nil {
		return cg.sliceConvertIntFn
	}

	sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
	cg.sliceConvertIntFn = cg.mod.NewFunc("_tin_slice_convert_int", sliceType,
		ir.NewParam("s", sliceType),
		ir.NewParam("src_sz", irtypes.I64),
		ir.NewParam("tgt_sz", irtypes.I64),
		ir.NewParam("src_signed", irtypes.I32),
	)

	return cg.sliceConvertIntFn
}

// ensureRecoverFn lazily declares the _tin_recover() -> TinString extern.
func (cg *CodeGen) ensureRecoverFn() *ir.Func {
	if cg.tinRecoverFn != nil {
		return cg.tinRecoverFn
	}

	cg.tinRecoverFn = cg.mod.NewFunc("_tin_recover", stringFatPtrType())

	return cg.tinRecoverFn
}

// genBuiltinRecover implements recover(): returns the panic message from a
// deferred function, or an empty string if not currently panicking.
func (cg *CodeGen) genBuiltinRecover(block *ir.Block) (value.Value, error) {
	return block.NewCall(cg.ensureRecoverFn()), nil
}

// ensureBytesFromBuf lazily declares _tin_bytes_from_buf(ptr *i8, len i64) {i8*, i64}.
// Copies len bytes from ptr into a new RC-allocated heap buffer and returns
// a fat [byte] slice.  Used to convert fixed-size stack arrays to [byte].
func (cg *CodeGen) ensureBytesFromBuf() *ir.Func {
	if cg.bytesFromBufFn != nil {
		return cg.bytesFromBufFn
	}

	// Reuse any existing declaration (e.g. from ioutil/os declaring the same
	// extern under a different Tin name) to avoid duplicate IR declarations.
	for _, f := range cg.mod.Funcs {
		if f.Name() == "_tin_bytes_from_buf" {
			cg.bytesFromBufFn = f

			return f
		}
	}

	sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
	cg.bytesFromBufFn = cg.mod.NewFunc("_tin_bytes_from_buf", sliceType,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
	)
	cg.bytesFromBufFn.Blocks = nil

	return cg.bytesFromBufFn
}

// ensureSnprintf lazily declares the snprintf external function.
// int snprintf(char* buf, size_t n, const char* format, ...)
func (cg *CodeGen) ensureSnprintf() *ir.Func {
	if cg.sprintfFn != nil {
		return cg.sprintfFn
	}

	cg.sprintfFn = cg.ensureExternDecl("snprintf", irtypes.I32,
		[]*ir.Param{ir.NewParam("buf", irtypes.I8Ptr), ir.NewParam("n", irtypes.I64), ir.NewParam("format", irtypes.I8Ptr)}, true)

	return cg.sprintfFn
}

// 128-bit echo / format helpers

// ensureEchoI128 lazily declares _tin_echo_i128(i128) void.
func (cg *CodeGen) ensureEchoI128() *ir.Func {
	if cg.echoI128Fn != nil {
		return cg.echoI128Fn
	}

	cg.echoI128Fn = cg.ensureExternDecl("_tin_echo_i128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.echoI128Fn
}

// ensureEchoU128 lazily declares _tin_echo_u128(i128) void.
func (cg *CodeGen) ensureEchoU128() *ir.Func {
	if cg.echoU128Fn != nil {
		return cg.echoU128Fn
	}

	cg.echoU128Fn = cg.ensureExternDecl("_tin_echo_u128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.echoU128Fn
}

// ensureEchoF128 lazily declares _tin_echo_f128(fp128) void.
func (cg *CodeGen) ensureEchoF128() *ir.Func {
	if cg.echoF128Fn != nil {
		return cg.echoF128Fn
	}

	cg.echoF128Fn = cg.ensureExternDecl("_tin_echo_f128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.FP128)}, false)

	return cg.echoF128Fn
}

// ensureEchoStringEscaped lazily declares _tin_echo_string_escaped(i8*, i64) void.
func (cg *CodeGen) ensureEchoStringEscaped() *ir.Func {
	if cg.echoStringEscapedFn != nil {
		return cg.echoStringEscapedFn
	}

	cg.echoStringEscapedFn = cg.ensureExternDecl("_tin_echo_string_escaped", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64)}, false)

	return cg.echoStringEscapedFn
}

// ensurePrintStringEscaped lazily declares _tin_print_string_escaped(i8*, i64) void.
func (cg *CodeGen) ensurePrintStringEscaped() *ir.Func {
	if cg.printStringEscapedFn != nil {
		return cg.printStringEscapedFn
	}

	cg.printStringEscapedFn = cg.ensureExternDecl("_tin_print_string_escaped", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64)}, false)

	return cg.printStringEscapedFn
}

// ensureI128ToCstr lazily declares _tin_i128_to_cstr(i128) i8*.
func (cg *CodeGen) ensureI128ToCstr() *ir.Func {
	if cg.i128ToCstrFn != nil {
		return cg.i128ToCstrFn
	}

	cg.i128ToCstrFn = cg.ensureExternDecl("_tin_i128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.i128ToCstrFn
}

// ensureU128ToCstr lazily declares _tin_u128_to_cstr(i128) i8*.
func (cg *CodeGen) ensureU128ToCstr() *ir.Func {
	if cg.u128ToCstrFn != nil {
		return cg.u128ToCstrFn
	}

	cg.u128ToCstrFn = cg.ensureExternDecl("_tin_u128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.u128ToCstrFn
}

// ensureF128ToCstr lazily declares _tin_f128_to_cstr(fp128) i8*.
func (cg *CodeGen) ensureF128ToCstr() *ir.Func {
	if cg.f128ToCstrFn != nil {
		return cg.f128ToCstrFn
	}

	cg.f128ToCstrFn = cg.ensureExternDecl("_tin_f128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.FP128)}, false)

	return cg.f128ToCstrFn
}

// emitAnyDispatchRegistrations emits calls to _tin_register_any_release
// for every struct that has a release helper (deinit or RC-tracked
// fields). Without this, an any-boxed *Cell or other struct with a
// custom destructor would skip its deinit entirely on scope exit -- the
// generic _tin_release_any path only frees the heap block.
//
// Returns the (possibly new) block at which subsequent code should
// continue emitting.
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

	regFn := cg.ensureExternDecl("_tin_register_any_release", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("type_id", irtypes.I32),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	for structName, typeID := range cg.structTypeIDs {
		st, ok := cg.structTypes[structName]
		if !ok {
			continue
		}

		if !cg.structEligibleForAnyDispatch(structName, st) {
			continue
		}

		relFn := cg.ensureStructPtrReleaseFn(structName, st)
		if relFn == nil {
			continue
		}

		fnI8 := block.NewBitCast(relFn, irtypes.I8Ptr)
		block.NewCall(regFn, constant.NewInt(irtypes.I32, int64(typeID)), fnI8)
	}

	return block
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
				if _, isTinStruct := cg.structTypes[innerSt.Name()]; isTinStruct {
					return true
				}
			}
		}
	}

	_ = st

	return false
}
