// tin runtime - ARC (Automatic Reference Counting) with biased RC.
//
// Every heap block managed by ARC is preceded by a TinRCHdr.  The
// public pointer points past the header to the actual data.  Static
// Static string literals carry TIN_RC_IMMORTAL in flags so retain/release skip them.
//
// Biased RC: each block carries a `shared` bit.  Blocks start shared=0
// (single-fiber/thread, retain/release uses non-atomic ops).  When the
// block escapes -- spawn capture, channel send, ... -- the bit is set
// via _tin_make_shared (release ordering) BEFORE the pointer becomes
// visible to another thread.  Subsequent retain/release on a shared
// block falls back to atomic ops.  See docs/15-ownership.md.

#include "runtime.h"
#include <stdlib.h>
#include <stdio.h>
#include <string.h>

#if TIN_USE_MIMALLOC
#  include <mimalloc.h>
#endif

static inline TinRCHdr *_rc_hdr(void *ptr) {
    return (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
}

#if TIN_USE_MIMALLOC

// Per-thread mimalloc heap, pinned to the Tin arena.  Defined in
// heap_arena.c; cached after first call so the lookup is one TLS load
// in the hot path.
extern mi_heap_t *_tin_managed_heap(void);

static inline void *_tin_arena_alloc(size_t bytes) {
    return mi_heap_malloc(_tin_managed_heap(), bytes);
}

// Free a block returned by _tin_arena_alloc.  Zero the header's _pad
// before mi_free so _tin_is_managed reliably rejects pointers into
// freed blocks (mimalloc's free-list link overwrites offset 0..7,
// but the pad slot at 8..15 would otherwise survive across the free
// and trick the magic check into accepting a freed pointer as live).
static inline void _tin_arena_free(void *hdr) {
    ((TinRCHdr *)hdr)->_pad = 0;
    mi_free(hdr);
}

#else // !TIN_USE_MIMALLOC

// --no-mimalloc fallback: rc-blocks come from libc malloc.  The
// header magic is what _tin_is_managed checks (heap_arena.c stub);
// zeroing _pad on free is still essential so a freed-then-recycled-
// by-libc block doesn't trick a stale *T retain/release.
static inline void *_tin_arena_alloc(size_t bytes) {
    return malloc(bytes);
}

static inline void _tin_arena_free(void *hdr) {
    ((TinRCHdr *)hdr)->_pad = 0;
    free(hdr);
}

#endif // TIN_USE_MIMALLOC

// Allocate an ARC-managed block of `size` bytes.  Starts with rc=1,
// SHARED flag set (atomic retain/release), UNIQUE clear (set only by
// release as rc transitions back to 1).  Use _tin_rc_alloc_local when
// escape analysis proved the block stays inside a single fiber.
void *_tin_rc_alloc(int64_t size) {
    TinRCHdr *hdr = (TinRCHdr *)_tin_arena_alloc(sizeof(TinRCHdr) + (size_t)size);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc    = 1;
    hdr->flags = TIN_RC_SHARED;
    hdr->_pad  = TIN_RC_HDR_MAGIC;
    return hdr + 1;
}

// Allocate a fiber-local block.  flags=0 means SHARED clear (non-atomic
// fast path) and UNIQUE clear (codegen consults UNIQUE only on the
// non-atomic path; we leave the initial value clear so the first
// observed rc-transition fills it in correctly).  If the block later
// escapes, _tin_make_shared flips SHARED and subsequent ops fall back
// to atomic.
void *_tin_rc_alloc_local(int64_t size) {
    TinRCHdr *hdr = (TinRCHdr *)_tin_arena_alloc(sizeof(TinRCHdr) + (size_t)size);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc    = 1;
    hdr->flags = TIN_RC_UNIQUE; // rc==1 with no other references
    hdr->_pad  = TIN_RC_HDR_MAGIC;
    return hdr + 1;
}

// Mark a block as shared across fiber/thread boundaries.  Atomic-OR
// keeps any other flag bits (UNIQUE, ARENA, ...) intact.  Release
// ordering pairs with the receiver's acquire on the channel/spawn
// machinery so every subsequent retain/release sees SHARED set.  No-op
// on NULL and on immortal blocks (retain/release short-circuit before
// the SHARED check anyway).
void _tin_make_shared(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_RELAXED);
    if (flags & TIN_RC_IMMORTAL) return;
    __atomic_or_fetch(&hdr->flags, TIN_RC_SHARED, __ATOMIC_RELEASE);
}

// Increment reference count; immortal, NULL and foreign pointers
// short-circuit.  Non-atomic when SHARED is clear; atomic when SHARED is
// set.  The non-atomic path also clears UNIQUE (rc is moving above 1).
// ACQUIRE on the flag load pairs with the RELEASE store in
// _tin_make_shared.
//
// The provenance check at the top makes this safe to emit from codegen
// for any pointer (libc malloc, addr(), static, stack, Tin arena alike):
// _tin_is_managed's arena range + header magic probe short-circuits
// before any hdr field is touched on foreign pointers, both fixing the
// latent corruption of allocator metadata AND silencing the matching
// valgrind "invalid read N bytes before block" reports.
void _tin_retain(void *ptr) {
    if (!_tin_is_managed(ptr)) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    if (flags & TIN_RC_SHARED) {
        __atomic_fetch_add(&hdr->rc, 1, __ATOMIC_RELAXED);
    } else {
        hdr->rc++;
        // Bumping rc above 1 means another owner exists -- UNIQUE no
        // longer holds.  Only safe to clear on the non-atomic path
        // because the SHARED path may race with another thread.
        if (flags & TIN_RC_UNIQUE) {
            hdr->flags = flags & ~TIN_RC_UNIQUE;
        }
    }
}

// Decrement reference count; frees the block when it reaches zero.
// Non-atomic on SHARED=0, ACQ_REL atomic on SHARED=1.  When the
// post-release rc lands at 1 on the non-atomic path, set UNIQUE so
// the single remaining owner can take the in-place mutation fast path
// at the next CoW site.  Skips free() when the ARENA bit is set --
// the block's storage is owned by an external arena/pool.  Foreign
// (non-Tin-managed) pointers short-circuit at _tin_is_managed -- see
// the matching comment in _tin_retain.
void _tin_release(void *ptr) {
    if (!_tin_is_managed(ptr)) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
        // rc dropping to 1 leaves us with the single remaining owner.
        // Re-enable UNIQUE so the next CoW site can mutate in place.
        if (prev == 2) {
            hdr->flags = flags | TIN_RC_UNIQUE;
        }
    }
    if (prev == 1 && !(flags & TIN_RC_ARENA)) _tin_arena_free(hdr);
}

// Provenance-aware retain.  Used by codegen for ARC ops on user-
// pointer values (*T, *void, *i64, etc.) whose source is unknown at
// the call site.  When the pointer falls outside Tin's arena (C-
// allocated, static, stack), the call is a no-op; otherwise it
// dispatches to the biased retain path.
//
// Splitting from _tin_retain keeps the data-pointer fast path (struct
// field walks, fat-string data releases) free of the range check --
// those callers know their pointer is Tin-managed by construction.
void _tin_retain_ptr(void *ptr) {
    if (!_tin_is_managed(ptr)) return;
    _tin_retain(ptr);
}

// Provenance-aware release.  Mirrors _tin_retain_ptr: short-circuits
// on foreign pointers, dispatches to _tin_release on managed ones.
void _tin_release_ptr(void *ptr) {
    if (!_tin_is_managed(ptr)) return;
    _tin_release(ptr);
}

// cLayoutStruct retain/release with the wrapper's borrow flag.
// flags bit 0 = "borrowed": c_data_ptr points outside the wrapper's
// rc-block (e.g. pointer-extern returns where C owns the lifetime), so
// the ptr the codegen passes via the c_data_ptr - sizeof(wrapper) trick
// would land in unrelated memory.  When the bit is set, skip the rc
// touch entirely; ownership is tracked through the original wrapper
// pointer that the caller (a separate rc-block) holds.
void _tin_retain_clayout(void *ptr, int32_t flags) {
    if (flags & 1) return;
    _tin_retain(ptr);
}

void _tin_release_clayout(void *ptr, int32_t flags) {
    if (flags & 1) return;
    _tin_release(ptr);
}

// Decrement reference count and free; returns 1 if the block was freed (prev==1).
// Callers use the return value to decide whether child fields need recursive release.
// The struct value MUST be loaded before calling this function, since the block is
// freed before returning 1.
int64_t _tin_release_struct(void *ptr) {
    if (!ptr) return 0;
    TinRCHdr *hdr = _rc_hdr(ptr);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return 0;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1 && !(flags & TIN_RC_ARENA)) { _tin_arena_free(hdr); return 1; }
    return 0;
}

// Release a fat array whose elements are fat-ptr ARC objects (strings, fat arrays).
// Decrements the outer RC; only when RC reaches 0 does it release each element
// and free the outer block.  This ensures element release happens exactly once
// (when the last owner drops it), preventing double-free when the array is shared.
void _tin_release_fat_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        // Mirrors the fat-array element layout `{T* ptr, i64 len, i64
        // cap}` -- struct size must match so the per-element stride
        // is right.  Only ptr is read; len/cap are unused here.
        typedef struct { void *ptr; int64_t len; int64_t cap; } FatElem;
        FatElem *elems = (FatElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i].ptr);
        _tin_arena_free(hdr);
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
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        typedef struct { int32_t tag; void *ptr; } AnyElem;
        AnyElem *elems = (AnyElem *)data;
        for (int64_t i = 0; i < count; i++) {
            _tin_release_any(elems[i].tag, elems[i].ptr);
        }
        _tin_arena_free(hdr);
    }
}

// Release a closure env block. Layout: { void(*dtor)(void*), capture_0, ... }
// Decrements the env RC; when RC reaches 0 calls the dtor (if non-null) to
// release RC-tracked captures, then frees the block.  Safe to call with NULL.
void _tin_release_closure(void *env) {
    if (!env) return;
    TinRCHdr *hdr = _rc_hdr(env);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        typedef void(*DtorFn)(void*);
        DtorFn dtor = *(DtorFn *)env;
        if (dtor) dtor(env);
        _tin_arena_free(hdr);
    }
}

// Release a fat array whose elements are 4-slot fat-fn-ptrs
// {coro*, colored*, sync*, i8* env}.  Decrements the outer RC; when RC=0,
// releases each element's env via _tin_release_closure then frees the outer
// block.
void _tin_release_fn_elem_array(void *data, int64_t count) {
    if (!data) return;
    TinRCHdr *hdr = _rc_hdr(data);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        typedef struct { void *coro; void *colored; void *sync; void *env; } FnElem;
        FnElem *elems = (FnElem *)data;
        for (int64_t i = 0; i < count; i++) _tin_release_closure(elems[i].env);
        _tin_arena_free(hdr);
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
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        void **elems = (void **)data;
        for (int64_t i = 0; i < count; i++) _tin_release(elems[i]);
        _tin_arena_free(hdr);
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
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
    if (prev == 1) {
        for (int64_t i = 0; i < count; i++) {
            release_fn((char *)data + i * elem_size);
        }
        _tin_arena_free(hdr);
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
    TinRCHdr *hdr = (TinRCHdr *)_tin_arena_alloc(sizeof(TinRCHdr) + copy_len);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc    = 1;
    hdr->flags = TIN_RC_SHARED;
    hdr->_pad  = TIN_RC_HDR_MAGIC;
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
    TinRCHdr *hdr = (TinRCHdr *)_tin_arena_alloc(sizeof(TinRCHdr) + copy_len);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc    = 1;
    hdr->flags = TIN_RC_SHARED;
    hdr->_pad  = TIN_RC_HDR_MAGIC;
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

// Per-type-id deep-copy dispatch.  Parallel to the release table.
// Codegen registers a thunk for every struct it can deep-copy; the
// call site (struct field of type `any`, autocopy on an `any` arg)
// dispatches through this table to produce an isolated boxed value.
// Falls back to retain-and-share when no thunk is registered, so
// types that aren't registered keep today's sharing semantics
// (no regression, just no isolation).
typedef void *(*TinAnyDeepCopy)(void *);
static TinAnyDeepCopy _tin_any_deepcopy_table[TIN_ANY_DISPATCH_MAX];

void _tin_register_any_deepcopy(int32_t type_id, TinAnyDeepCopy fn) {
    if (type_id >= 0 && type_id < TIN_ANY_DISPATCH_MAX) {
        _tin_any_deepcopy_table[type_id] = fn;
    }
}

// _tin_any_deepcopy returns a freshly-allocated data block for the
// any value identified by (tag, data) when a per-type deep-copy
// thunk is registered.  When no thunk exists, falls back to bumping
// the rc on the existing block and returning the same pointer -
// callers treat the returned pointer as the new data slot for the
// boxed value.
void *_tin_any_deepcopy(int32_t tag, void *data) {
    if (!data) return NULL;

    if (tag >= 6 && tag < TIN_ANY_DISPATCH_MAX) {
        TinAnyDeepCopy fn = _tin_any_deepcopy_table[tag];
        if (fn) {
            return fn(data);
        }
    }

    TinRCHdr *hdr = _rc_hdr(data);
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (!(flags & TIN_RC_IMMORTAL)) {
        if (flags & TIN_RC_SHARED) {
            __atomic_fetch_add(&hdr->rc, 1, __ATOMIC_ACQ_REL);
        } else {
            hdr->rc++;
        }
    }
    return data;
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
    uint32_t flags = __atomic_load_n(&hdr->flags, __ATOMIC_ACQUIRE);
    if (flags & TIN_RC_IMMORTAL) return;
    uint32_t prev;
    if (flags & TIN_RC_SHARED) {
        prev = __atomic_fetch_sub(&hdr->rc, 1, __ATOMIC_ACQ_REL);
    } else {
        prev = hdr->rc;
        hdr->rc = prev - 1;
    }
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
        _tin_arena_free(hdr);
    }
}
