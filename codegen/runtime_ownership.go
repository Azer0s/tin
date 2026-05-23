package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

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

// releaseHeapPromotedFields emits a _tin_release for each field
// offset recorded in entry.ownsHeapPromotedFields.  The binding's
// alloca holds the struct value by reference (entry.val is its
// `*Struct`); for each offset, GEP into the field, load the raw
// pointer, and release it.  Called from both emitScopeRelease and
// emitAllScopeReleases so the cascade fires on every scope-exit
// path.
func (cg *CodeGen) releaseHeapPromotedFields(block *ir.Block, entry *scopeEntry, ptrType *irtypes.PointerType) {
	if len(entry.ownsHeapPromotedFields) == 0 {
		return
	}

	st, ok := ptrType.ElemType.(*irtypes.StructType)
	if !ok {
		return
	}

	for _, idx := range entry.ownsHeapPromotedFields {
		if idx < 0 || idx >= len(st.Fields) {
			continue
		}

		fieldPtr := block.NewGetElementPtr(st, entry.val,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		fieldVal := block.NewLoad(st.Fields[idx], fieldPtr)
		ptrI8 := block.NewBitCast(fieldVal, irtypes.I8Ptr)

		block.NewCall(cg.ensureRelease(), ptrI8)
	}
}

// bindingHeapPromotedFields returns the LLVM field offsets whose
// pointer values are heap-promoted blocks owned by s's let-binding.
// Set when s = call(...) and the callee's body matched a
// `return Struct{field: &local}` shape, which was recorded into
// cg.fnReturnsHeapPromotedFields at codegen time of the callee.
// Lets the caller's scope exit release the heap blocks the per-
// struct release helper would otherwise skip as borrowed.
func (cg *CodeGen) bindingHeapPromotedFields(s *ast.VarDecl) []int {
	if s == nil || s.Value == nil {
		return nil
	}

	ce, ok := s.Value.(*ast.CallExpr)
	if !ok {
		return nil
	}

	name := resolveCalleeName(ce)
	if name == "" {
		return nil
	}

	bare := name
	if idx := strings.LastIndex(bare, "::"); idx >= 0 {
		bare = bare[idx+2:]
	}

	if list, ok := cg.fnReturnsHeapPromotedFields[bare]; ok && len(list) > 0 {
		out := make([]int, len(list))
		copy(out, list)

		return out
	}

	if cg.curScope != nil {
		if entry, ok := cg.curScope.lookup(bare); ok {
			if f, ok2 := entry.val.(interface{ Name() string }); ok2 {
				if list, ok3 := cg.fnReturnsHeapPromotedFields[f.Name()]; ok3 && len(list) > 0 {
					out := make([]int, len(list))
					copy(out, list)

					return out
				}
			}
		}
	}

	return nil
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

	return cg.traitFor(CanonKey(name)) != nil
}

// exprIsAddrOfEscapingLocal reports whether `e` is `&Identifier` whose
// target is a heap-promoted local in the current function (curFnEscapingVars).
func (cg *CodeGen) exprIsAddrOfEscapingLocal(e ast.Node) bool {
	addr, ok := e.(*ast.AddressOfExpr)
	if !ok {
		return false
	}

	id, ok := addr.Expr.(*ast.Identifier)
	if !ok {
		return false
	}

	return cg.curFnEscapingVars[id.Name]
}

// releaseHeapPromotedLocalsInStructLit emits a release for every
// `&heap_promoted_local` value used as a primitive *T field of `e`.
// Called from the `&StructLit` lowering after the heap-bound copy has
// been stored, to balance the unconditional field-store retain that
// genStructLit emitted for those slots.  Only the heap-bound path
// emits this -- bare `Box{...}` (by-value) balances via temp-copy
// access plus binding scope-exit, so a release here would UAF.
func (cg *CodeGen) releaseHeapPromotedLocalsInStructLit(block *ir.Block, e *ast.StructLit) {
	emit := func(node ast.Node) {
		if !cg.exprIsAddrOfEscapingLocal(node) {
			return
		}

		ao := node.(*ast.AddressOfExpr)
		id := ao.Expr.(*ast.Identifier)

		entry, ok := cg.curScope.lookup(id.Name)
		if !ok || !entry.isAlloc || !entry.isEarlyHeap {
			return
		}

		ptrType, ok := entry.val.Type().(*irtypes.PointerType)
		if !ok {
			return
		}
		// entry.val for early-heap'd locals is the heap pointer itself
		// (cast via bitcast on alloc); release it via the provenance-
		// aware _tin_release_ptr if it's a primitive *T, else via the
		// generic release for fat-ptr-shaped heap blocks.
		if isPrimitivePtr(ptrType) {
			i8p := block.NewBitCast(entry.val, irtypes.I8Ptr)
			block.NewCall(cg.ensureReleasePtr(), i8p)
		} else {
			i8p := block.NewBitCast(entry.val, irtypes.I8Ptr)
			block.NewCall(cg.ensureRelease(), i8p)
		}
	}

	for _, v := range e.Positional {
		emit(v)
	}

	for _, f := range e.Fields {
		emit(f.Value)
	}
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
		if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
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

			alias := cg.aliasTypeFor(CanonKey(name))
			if alias == nil {
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
//   - fat fn ptrs  {coro*, colored*, sync*, i8* env}  - env (field 3) is rc-alloc'd (null for named-fn wrappers)
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
	rcKindFn         rcKind = 3 // fat fn ptr: {coro*, colored*, sync*, env*} -- release via _tin_release_closure(env@24)
	rcKindRawPtr     rcKind = 4 // primitive *T (*i64, *void, ...) -- retain/release via _tin_{retain,release}_ptr; foreign + interior pointers no-op via arena-range + header-magic check
)

// rcKindOf classifies an LLVM type by where its retainable pointer
// (if any) sits inside the value. See rcKind comments.
func rcKindOf(t irtypes.Type) rcKind {
	switch {
	case t == nil:
		return rcKindNone
	case isVolatilePtr(t):
		// Raw bare-metal pointer (e.g. addr(0xDEAD)) -- skip all rc.
		return rcKindNone
	case isAnyType(t):
		return rcKindAny
	case isFatFnPtr(t):
		return rcKindFn
	case isStringType(t), isFatArrayPtr(t), isTraitFatPtrShape(t):
		return rcKindLeadingPtr
	case isPrimitivePtr(t):
		return rcKindRawPtr
	}

	return rcKindNone
}

// volatileAddrSpace is the LLVM address space tin uses for raw,
// bare-metal pointer values (`volatile *T`, e.g. the result of
// `addr(0xDEADBEEF)`).  Pointers in this space are explicitly opted
// out of all rc retain/release/is_managed machinery: their lifetimes
// are the user's responsibility.  Any other address space (0 today)
// is rc-tracked normally.
const volatileAddrSpace = 1

// isVolatilePtr reports whether t is an rc-opted-out raw pointer.
// Callers in the rc machinery (rcKindOf, isPrimitivePtr, scope
// release, assignment release, return release) all check this first
// and treat a volatile pointer as if it has no rc to manage.
func isVolatilePtr(t irtypes.Type) bool {
	pt, ok := t.(*irtypes.PointerType)

	return ok && pt.AddrSpace == volatileAddrSpace
}

// isPrimitivePtr reports whether t is a pointer whose element type is
// a scalar (*i64, *f64, *void, *byte, ...) -- NOT a pointer to a
// named struct (those have per-struct release helpers) and NOT a
// fat-ptr shape (string / fat array / trait / fat fn ptr, caught by
// their own predicates earlier in the switch).  Caller emits
// _tin_retain_ptr / _tin_release_ptr for these; the runtime's
// arena-range + header-magic check makes foreign and interior
// pointers safe no-ops.
//
// Volatile pointers (addrspace 1) are excluded -- they're the raw
// `addr(...)` escape hatch and must skip rc entirely.
func isPrimitivePtr(t irtypes.Type) bool {
	pt, ok := t.(*irtypes.PointerType)
	if !ok {
		return false
	}

	if pt.AddrSpace == volatileAddrSpace {
		return false
	}

	switch pt.ElemType.(type) {
	case *irtypes.IntType, *irtypes.FloatType:
		return true
	}

	return false
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

// emitCallArgReleaseForRet releases a temporary call argument after a
// call returns, taking the callee's return type into account.
//
// Rules:
//   - boxed-to-any: release the boxed value (fresh _tin_rc_alloc)
//   - copy expressions (identifier, field access, etc.): skip (scope exit owns it)
//   - RC-tracked temporaries (string, array, any, fn): release
//   - *TinStruct pointer temporaries: release via ensureStructPtrReleaseFn to
//     balance any retain performed inside the callee (e.g. storing the pointer
//     in a struct field via a struct literal).
//
// The ret-type arg lets the ADT-rvalue release
// path skip when the callee may have transferred the rvalue's inner
// contents through its return value (e.g. unwrap_or returns the
// inner Ok value). When ret type is nil (caller couldn't determine
// it), behave conservatively and skip the ADT rvalue release.
