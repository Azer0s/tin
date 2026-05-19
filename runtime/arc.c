// tin runtime - ARC (Automatic Reference Counting)
//
// Every heap block managed by ARC is preceded by a TinRCHdr.
// The public pointer points past the header to the actual data.
// Static string literals use TIN_IMMORTAL_RC (-1) so retain/release skip them.

#include "runtime.h"
#include <stdlib.h>
#include <stdio.h>
#include <string.h>

static inline TinRCHdr *_rc_hdr(void *ptr) {
    return (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
}

// Allocate an ARC-managed block of `size` bytes; starts with rc = 1
void *_tin_rc_alloc(int64_t size) {
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + (size_t)size);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc = 1;
    return hdr + 1;
}

// Increment reference count; skips immortal (rc == -1) and NULL pointers.
// Uses RELAXED ordering: only the increment itself needs to be atomic;
// no happens-before relationship is required for retain.
void _tin_retain(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    __atomic_fetch_add(&hdr->rc, 1, __ATOMIC_RELAXED);
}

// Decrement reference count; frees the block when it reaches zero.
// Uses ACQ_REL ordering so all prior accesses to the object are visible
// before the free (preventing use-after-free on concurrent release).
void _tin_release(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) free(hdr);
}

// Decrement reference count and free; returns 1 if the block was freed (prev==1).
// Callers use the return value to decide whether child fields need recursive release.
// The struct value MUST be loaded before calling this function, since the block is
// freed before returning 1.
int64_t _tin_release_struct(void *ptr) {
    if (!ptr) return 0;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return 0;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) { free(hdr); return 1; }
    return 0;
}

// Release a fat array whose elements are fat-ptr ARC objects (strings, fat arrays).
// Decrements the outer RC; only when RC reaches 0 does it release each element
// and free the outer block.  This ensures element release happens exactly once
// (when the last owner drops it), preventing double-free when the array is shared.
void _tin_release_fat_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        // Mirrors the fat-array element layout `{T* ptr, i64 len, i64
        // cap}` -- struct size must match so the per-element stride
        // is right.  Only ptr is read; len/cap are unused here.
        typedef struct { void *ptr; int64_t len; int64_t cap; } FatElem;
        FatElem *elems = (FatElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i].ptr);
        free(hdr);
    }
}

// Forward declaration; full definition appears below.  Required because
// _tin_release_any_elem_array dispatches each element through it.
void _tin_release_any(int32_t tag, void *data);

// Release a fat array whose elements are `any` {i32 tag, void* ptr}.
// Same RC-0 semantics as _tin_release_fat_elem_array.  Dispatches each
// element through _tin_release_any (tag-aware) so inner ARC-tracked
// content (string i8*, fat-array T*, fat-fn-ptr env) gets released
// when its any data block hits RC=0.  Without this dispatch, the
// inner content of every any element would leak when its block frees.
void _tin_release_any_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        typedef struct { int32_t tag; void *ptr; } AnyElem;
        AnyElem *elems = (AnyElem *)data;
        for (int64_t i = 0; i < count; i++) {
            _tin_release_any(elems[i].tag, elems[i].ptr);
        }
        free(hdr);
    }
}

// Release a closure env block. Layout: { void(*dtor)(void*), capture_0, ... }
// Decrements the env RC; when RC reaches 0 calls the dtor (if non-null) to
// release RC-tracked captures, then frees the block.  Safe to call with NULL.
void _tin_release_closure(void *env) {
    if (!env) return;
    TinRCHdr *hdr = _rc_hdr(env);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        typedef void(*DtorFn)(void*);
        DtorFn dtor = *(DtorFn *)env;
        if (dtor) dtor(env);
        free(hdr);
    }
}

// Release a fat array whose elements are 4-slot fat-fn-ptrs
// {coro*, colored*, sync*, i8* env}.  Decrements the outer RC; when RC=0,
// releases each element's env via _tin_release_closure then frees the outer
// block.
void _tin_release_fn_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        typedef struct { void *coro; void *colored; void *sync; void *env; } FnElem;
        FnElem *elems = (FnElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release_closure(elems[i].env);
        free(hdr);
    }
}

// Release a fat array whose elements are raw ARC-managed pointers (e.g. [*T]).
// Decrements the outer buffer RC; when RC reaches 0, calls _tin_release on
// each pointer element (treating the pointer value as the ARC data ptr) and
// frees the outer buffer.  Every pointer in such an array must be heap-
// allocated (via _tin_rc_alloc) - see compiler enforcement in genAugAssign.
void _tin_release_ptr_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        void **elems = (void **)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i]);
        free(hdr);
    }
}

// Release a fat array whose elements are named structs with RC fields.
// Decrements the outer RC; when RC reaches 0, calls release_fn on each element
// (passing a pointer to the element in-place) and frees the outer block.
// release_fn is a compiler-generated per-struct helper that releases RC fields.
typedef void (*TinElemReleaseFn)(void *elem);
void _tin_foreach_struct_elem_release(
    void *data, int64_t count, int64_t elem_size, TinElemReleaseFn release_fn
) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        for (int64_t i = 0; i < count; i++) {
            release_fn((char *)data + i * elem_size);
        }
        free(hdr);
    }
}

// Release each element of an inline (stack/struct-field) fixed-size array
// [T; N] whose backing storage is NOT an RC block.  Unlike its sibling above,
// this does NOT decrement an outer RC or free anything: the buffer's lifetime
// is owned by the enclosing scope or struct.  Used for scope-exit and per-
// struct release of [T; N] fields whose elements own RC heap blocks
// (e.g. [errors::Err; 4], [string; N]).
void _tin_foreach_fixed_elem_release(
    void *data, int64_t count, int64_t elem_size, TinElemReleaseFn release_fn
) {
    if (!data) return;
    for (int64_t i = 0; i < count; i++) {
        release_fn((char *)data + i * elem_size);
    }
}

// Retain helpers for array concatenation: when `b = a ++ [item]` and `a` is a
// non-temporary source, the new buffer `b` holds copies of `a`'s element
// pointers.  Since `a` and `b` both hold those pointers, each element's RC
// must be incremented so that releasing either array does not free elements
// still held by the other.  The retain helpers below loop over the copied
// portion of the new buffer and increment each element's RC.

// Retain each pointer element in a [*T] array slice (count elements at data).
void _tin_retain_ptr_elems(void *data, int64_t count) {
    if (!data || count <= 0) return;
    void **elems = (void **)data;
    for (int64_t i = 0; i < count; i++) _tin_retain(elems[i]);
}

// Retain each fat-pointer element in a [string] or [[T]] slice.
// Fat pointers are `{void* ptr, i64 len, i64 cap}`; field 0 is the
// ARC-managed data pointer.  Struct size must match the array stride.
void _tin_retain_fat_elems(void *data, int64_t count) {
    if (!data || count <= 0) return;
    typedef struct { void *ptr; int64_t len; int64_t cap; } FatElem;
    FatElem *elems = (FatElem *)data;
    for (int64_t i = 0; i < count; i++) _tin_retain(elems[i].ptr);
}

// Retain each closure fat-pointer element in a [fn] slice.
// Fat-fn-ptrs are {sync*, colored*, coro*, i8* env}; env (slot 3) is ARC-managed.
void _tin_retain_fn_elems(void *data, int64_t count) {
    if (!data || count <= 0) return;
    typedef struct { void *sync; void *colored; void *coro; void *env; } FnElem;
    FnElem *elems = (FnElem *)data;
    for (int64_t i = 0; i < count; i++) _tin_retain(elems[i].env);
}

// Retain each `any` element in a [any] slice.
// `any` is {i32 tag, void* ptr}; field 1 is the ARC-managed data pointer.
void _tin_retain_any_elems(void *data, int64_t count) {
    if (!data || count <= 0) return;
    typedef struct { int32_t tag; void *ptr; } AnyElem;
    AnyElem *elems = (AnyElem *)data;
    for (int64_t i = 0; i < count; i++) _tin_retain(elems[i].ptr);
}

// Retain each named-struct element in a slice using a per-type helper.
// retain_fn is a compiler-generated function that retains RC fields in one element.
typedef void (*TinElemRetainFn)(void *elem);
void _tin_foreach_struct_elem_retain(
    void *data, int64_t count, int64_t elem_size, TinElemRetainFn retain_fn
) {
    if (!data || count <= 0) return;
    for (int64_t i = 0; i < count; i++) {
        retain_fn((char *)data + i * elem_size);
    }
}

// Free ptr if it was allocated via malloc (usable_size > 0).
// No-op for static strings, stack variables, or NULL.
void _tin_handover_free(void *ptr) {
    if (!ptr) return;
    if (_tin_usable_size(ptr) > 0) free(ptr);
}

// Take ownership of a C string returned by an extern #handover function.
// If src was malloc'd (usable_size > 0): uses the usable block size for the
// copy, then frees the original.
// If src was not malloc'd (static/literal): falls back to strlen to determine
// the copy length and heap-promotes (no free).
// Returns the Tin RC string data pointer (i8* after RC header), RC = 1.
char *_tin_string_handover(char *src) {
    if (!src) return NULL;
    size_t sz = _tin_usable_size(src);
    size_t copy_len = (sz > 0) ? sz : (strlen(src) + 1);
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + copy_len);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc = 1;
    char *dst = (char *)(hdr + 1);
    memcpy(dst, src, copy_len);
    if (sz > 0) free(src);
    return dst;
}

// Take ownership of an arbitrary C pointer from an extern #handover function.
// elem_size is the fallback copy length for non-malloc'd (static/stack) sources.
// If src was malloc'd (usable_size > 0): uses the usable block size.
// If src was not malloc'd: uses elem_size (0 = unknown, returns src unchanged).
// Returns the Tin RC data pointer (after RC header), RC = 1.
void *_tin_ptr_handover(void *src, size_t elem_size) {
    if (!src) return NULL;
    size_t sz = _tin_usable_size(src);
    size_t copy_len = (sz > 0) ? sz : elem_size;
    if (copy_len == 0) return src;
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + copy_len);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc = 1;
    void *dst = (void *)(hdr + 1);
    memcpy(dst, src, copy_len);
    if (sz > 0) free(src);
    return dst;
}

// Release an `any` value whose tag is anyTagFn (5): the data block holds a
// 4-slot fat-fn-ptr {sync*, colored*, coro*, i8* env}.  Decrements the data
// RC; when RC reaches 0 releases env (slot 3) via _tin_release_closure then
// frees the block.
// For all other tags, behaves like _tin_release(data).
// anyTagFn = 5 matches the constant in codegen/types.go.

// Per-type-id deinit dispatch. Codegen calls _tin_register_any_release at
// startup for every struct that has a deinit (or owns RC fields), so any-
// boxed structs route through their per-struct release helper. Without
// this the heap block was freed but the struct's deinit was skipped --
// e.g. an rc::Cell stored in `any` would silently leak its underlying C
// resource.
typedef void (*TinAnyRelease)(void *);
#define TIN_ANY_DISPATCH_MAX 4096
static TinAnyRelease _tin_any_release_table[TIN_ANY_DISPATCH_MAX];

void _tin_register_any_release(int32_t type_id, TinAnyRelease fn) {
    if (type_id >= 0 && type_id < TIN_ANY_DISPATCH_MAX) {
        _tin_any_release_table[type_id] = fn;
    }
}

void _tin_release_any(int32_t tag, void *data) {
    if (!data) return;

    // Per-type-id dispatch: the registered helper handles the full
    // teardown (RC dec + deinit + field release + free) so we return
    // before falling through to the generic free below.
    if (tag >= 6 && tag < TIN_ANY_DISPATCH_MAX) {
        TinAnyRelease fn = _tin_any_release_table[tag];
        if (fn) {
            fn(data);
            return;
        }
    }

    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        // The data block "owns" one reference to its inner ARC-tracked
        // content (the box-as-any path retains the inner before storing
        // it).  When the block hits RC=0 we release that owned ref so
        // strings / closure envs nested inside an any don't leak.  Tag
        // layout (matches codegen/types.go):
        //   0=int, 1=float, 2=string, 3=bool, 4=ptr, 5=fn, 6+=struct
        // Only tag=2 (string) and tag=5 (fn) carry an ARC-tracked
        // inner that needs explicit release here -- int/float/bool/
        // ptr/atom either carry no heap (primitives) or store a raw
        // pointer that the caller's emitRetain/emitRelease handles via
        // generic _tin_retain/_tin_release on the data ptr itself.
        switch (tag) {
        case 2: /* anyTagString -- release the i8* string ptr inside */
            _tin_release(*(void **)data);
            break;
        case 5: { /* anyTagFn -- release the env (slot 3 of fat-fn-ptr) */
            void *env = *((void **)data + 3);
            _tin_release_closure(env);
            break;
        }
        default:
            break;
        }
        free(hdr);
    }
}
