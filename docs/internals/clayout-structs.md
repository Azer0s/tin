# cLayoutStructs - wrapper + native layout

## Overview

Structs that appear as `*S` in any extern function signature are called
**cLayoutStructs**. They use a two-type LLVM representation that enables:

- **Live C mutation visibility** - field reads always observe current C memory
- **ARC compatibility** - type-ID and vtable pointers are preserved for dynamic
  dispatch and reflection
- **No unnecessary copies** - non-handover pointers store only the raw C pointer

## LLVM types

For each cLayoutStruct `S`, two LLVM types are declared:

```
%S        = { i32 type_id, vtable_ptr_types..., i8* c_data_ptr }
%S.native = { field_0_type, field_1_type, ... }   ; C layout, no metadata
```

`%S.native` exactly matches the C struct memory layout. `%S` is the Tin
wrapper that holds the ARC/dispatch metadata plus a raw pointer to the C data.

### Field index layout in `%S`

| Index | Field |
|-------|-------|
| 0 | `i32 type_id` |
| 1 .. vtableOffset(S)-1 | vtable pointers |
| `userFieldOffset(S)` | `i8* c_data_ptr` |

`userFieldOffset(S) = 1 + number_of_vtable_slots`.

There are **no inline user fields** in `%S`. The wrapper is minimal.

## c_data_ptr semantics

`c_data_ptr` always points to a valid `%S.native`-compatible memory region:

| Source                | c_data_ptr value                                  | Who owns the memory        |
|-----------------------|---------------------------------------------------|----------------------------|
| Non-handover C return | raw C pointer                                     | C owns; never freed by Tin |
| `#handover` C return  | pointer into RC alloc (after `sizeof(%S)` offset) | Tin owns via ARC           |
| Struct literal        | pointer to stack-allocated `%S.native` alloca     | stack frame                |

Because all field accesses go through `c_data_ptr`, C mutations to the
original struct are visible immediately without re-fetching the pointer.

## Field access

`emitCLayoutFieldPtr(block, wrapperPtr, structName, fieldIdx)` returns a GEP
pointer to native field `fieldIdx`:

```
1. GEP into %S at index cDataPtrIndex(S) -> loads i8* c_data_ptr
2. bitcast i8* -> %S.native*
3. GEP into %S.native at index fieldIdx
```

`emitFieldGEP(block, ptr, structName, fieldName)` dispatches:
- cLayoutStructs -> `emitCLayoutFieldPtr` (native 0-based index)
- regular structs -> direct GEP at `userFieldOffset + fieldIdx`

For embedded nested cLayoutStructs (e.g., `outer.a.x` where `a: inner_t`
and `inner_t` is also a cLayoutStruct), the field read of `a` produces an
`%inner_t.native` value loaded from C memory. The subsequent `.x` access
recognizes the `.native` suffix in the LLVM type name and GEPs directly
into the native value without another c_data_ptr indirection.

## Non-handover allocation (`emitStructPtrBorrow`)

```
allocSize = sizeof(%S)
wrapper   = _tin_rc_alloc(allocSize)    ; RC=1
store type_id at wrapper[0]
zero-init vtable slots
store rawI8Ptr at wrapper[cDataPtrIndex] ; c_data_ptr = raw C pointer
return wrapper
```

No data copy. C mutations are visible through every subsequent field read.

## Handover allocation (`emitStructPtrHandover`)

The GEP+1 trick allocates `sizeof(%S) + sizeof(%S.native)` bytes in a single
RC block:

```
allocSize = sizeof(%S) + sizeof(%S.native)
wrapper   = _tin_rc_alloc(allocSize)    ; RC=1
store type_id
zero-init vtable slots
native    = GEP(wrapper, 1)             ; points just past the wrapper struct
memcpy(native, rawI8Ptr, sizeof(%S.native))
free rawI8Ptr if heap-allocated
store native as c_data_ptr
return wrapper
```

`c_data_ptr` points inside the same RC allocation. When the RC count drops to
zero, the entire block (wrapper + native copy) is freed in one `_tin_release`.

## Struct literals

```
wrapperAlloca = alloca(%S)
nativeAlloca  = alloca(%S.native)
zero-init both
store type_id
store nativeAlloca as c_data_ptr
for each field: GEP into nativeAlloca, store value
return load(%S, wrapperAlloca)
```

The wrapper lives on the stack of the current function. `c_data_ptr` points
to the adjacent `nativeAlloca`. Stack lifetime is sufficient for the common
case of constructing a literal and immediately passing it to a C function.

## Passing *S to C

`extractCSrcPtr(block, wrapperPtr, te, cTargetType)` loads `c_data_ptr` from
the wrapper and casts it to `cTargetType` (`%S.native*`). This is the pointer
C receives - the raw pointer to live data regardless of whether the wrapper is
a non-handover borrow or a handover/literal copy.

## Reflection and dynamic dispatch

`type_id` is at field 0 of `%S`, matching the layout expected by `typeof`,
`traitof`, and vtable lookup. The wrapper is pointer-compatible with `*any`
(`{ i32*, ... }`). No changes to the ARC or reflection runtime are needed.

## Propagation to nested field types

When struct `A` has a field of type `B` and `A` is a cLayoutStruct,
`B` is also marked as a cLayoutStruct transitively (in `scanExternPtrStructs`).
This ensures `%B.native` is declared and that `%A.native` uses `%B.native`
for its field type (not the `%B` wrapper), preserving the exact C memory
layout for nested structs.
