// tin runtime - ARC (Automatic Reference Counting)
//
// Every heap block managed by ARC is preceded by a TinRCHdr.
// The public pointer points past the header to the actual data.
// Static string literals use TIN_IMMORTAL_RC (-1) so retain/release skip them.

#include "runtime.h"
#include <stdlib.h>
#include <stdio.h>

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
        typedef struct { void *ptr; int64_t dummy; } FatElem;
        FatElem *elems = (FatElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i].ptr);
        free(hdr);
    }
}

// Release a fat array whose elements are `any` {i32 tag, void* ptr}.
// Same RC-0 semantics as _tin_release_fat_elem_array.
void _tin_release_any_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        typedef struct { int32_t tag; void *ptr; } AnyElem;
        AnyElem *elems = (AnyElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i].ptr);
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

// Release a fat array whose elements are closure fat pointers {fn_ptr*, i8* env}.
// Decrements the outer RC; when RC=0, releases each element's env via
// _tin_release_closure then frees the outer block.
void _tin_release_fn_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        typedef struct { void *fn_ptr; void *env; } FnElem;
        FnElem *elems = (FnElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release_closure(elems[i].env);
        free(hdr);
    }
}

// Release an `any` value whose tag is anyTagFn (5): the data block holds a
// closure fat pointer {fn_ptr*, i8* env}.  Decrements the data RC; when RC
// reaches 0 releases the env via _tin_release_closure then frees the block.
// For all other tags, behaves like _tin_release(data).
// anyTagFn = 5 matches the constant in codegen/types.go.
void _tin_release_any(int32_t tag, void *data) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    if (__atomic_load_n(&hdr->rc, __ATOMIC_ACQUIRE) == TIN_IMMORTAL_RC) return;
    int64_t prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    if (prev == 1) {
        if (tag == 5) { /* anyTagFn */
            void *env = *((void **)data + 1);
            _tin_release_closure(env);
        }
        free(hdr);
    }
}
