// tin runtime - heap arena segregation.
//
// Reserves a large virtual address range at startup and registers it
// with mimalloc as an exclusive arena.  Every Tin rc-block (and
// nothing else) is allocated from heaps pinned to this arena, so the
// runtime can decide "is this pointer one of ours?" via a single
// range check (_tin_is_managed).
//
// The virtual reservation is essentially free: mmap registers a
// hole in the process's VMA list, no RAM is committed until pages
// are touched.  Pages commit lazily as mimalloc grows into the arena,
// and madvise(MADV_DONTNEED) returns RSS to the OS on shrink.
//
// Per-thread heaps: mimalloc heaps are not safe to allocate from
// concurrently, so each thread that runs ARC allocations gets its
// own heap pinned to the shared arena.  The TLS init is contention-
// free -- a thread only ever creates its own heap, never another
// thread's -- so no mutex is involved.
//
// Configuration via TINMAXHEAP env var (default 16 GiB):
//   TINMAXHEAP=64G   -> reserve 64 GiB virtual.
//   TINMAXHEAP=2048M -> reserve 2 GiB.
//   TINMAXHEAP=0     -> disable the arena; ARC allocations fall back
//                       to mimalloc's default heap and _tin_is_managed
//                       returns 1 unconditionally (sanitizer builds,
//                       debugging).

#include "runtime.h"

#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if !TIN_USE_MIMALLOC
#  error "heap_arena.c requires TIN_USE_MIMALLOC=1; mimalloc is now a hard dependency"
#endif

#include <mimalloc.h>

static mi_arena_id_t _tin_arena_id;
static void         *_tin_arena_base;
static size_t        _tin_arena_size;
static int           _tin_arena_active;

// One mi_heap per thread, lazily created on first ARC alloc.  Pinned
// to _tin_arena_id so allocations from this heap land in our reserved
// range.  Cross-thread free is safe (mi_free routes back to the
// owning heap internally).
static __thread mi_heap_t *_tin_thread_heap;

// Per-thread heaps are tracked in a lock-free singly-linked list so
// the atexit handler can destroy them all at clean shutdown.  Each
// thread atomically prepends its own node on first heap creation;
// the list is read-only after main() returns (workers have joined
// by then), so the destroy walk needs no synchronization.
typedef struct _tin_heap_node {
    mi_heap_t            *heap;
    struct _tin_heap_node *next;
} _tin_heap_node;

static _Atomic(_tin_heap_node *) _tin_heap_list_head;

#define TIN_DEFAULT_HEAP_GB 16

// Parse TINMAXHEAP env var with K / M / G / T suffixes.  Returns
// size in bytes, or 0 to disable the arena.
static size_t _tin_parse_heap_size(const char *s) {
    if (!s || !*s) return (size_t)TIN_DEFAULT_HEAP_GB * (1ULL << 30);

    char  *end = NULL;
    unsigned long long n = strtoull(s, &end, 10);
    if (end == s) return (size_t)TIN_DEFAULT_HEAP_GB * (1ULL << 30);

    while (*end == ' ' || *end == '\t') end++;

    size_t mult = 1;
    switch (*end) {
        case 'K': case 'k': mult = 1ULL << 10; break;
        case 'M': case 'm': mult = 1ULL << 20; break;
        case 'G': case 'g': mult = 1ULL << 30; break;
        case 'T': case 't': mult = 1ULL << 40; break;
        case '\0':          mult = 1; break;
        default:
            fprintf(stderr, "tin: unrecognized TINMAXHEAP suffix '%c'; expected K/M/G/T\n", *end);
            exit(1);
    }

    return (size_t)n * mult;
}

// One-shot init.  Fires at constructor priority 101 -- before any
// user-priority constructor in the rest of the runtime touches the
// allocator, after libc's own setup (priorities 0-100).
void _tin_heap_init(void) {
    if (_tin_arena_active) return;

    size_t want = _tin_parse_heap_size(getenv("TINMAXHEAP"));
    if (want == 0) {
        return; // degraded mode; _tin_is_managed returns 1
    }

    // Reserve our arena with exclusive=true so ONLY heaps explicitly
    // bound to it (via mi_heap_new_in_arena) draw from it.  Default
    // mi_malloc on any thread goes to mimalloc's other heaps, OUTSIDE
    // our arena, so the range check in _tin_is_managed truly identifies
    // rc-blocks vs everything else.
    //
    // macOS / aarch64 caps large virtual reservations even with
    // unlimited ulimit -v; halve and retry down to 256 MiB.
    int err = 0;
    while (want >= (256ULL << 20)) {
        err = mi_reserve_os_memory_ex(want,
                                      /* commit       */ false,
                                      /* allow_large  */ false,
                                      /* exclusive    */ true,
                                      &_tin_arena_id);
        if (err == 0) break;
        want /= 2;
    }
    if (err != 0) {
        fprintf(stderr, "tin: mi_reserve_os_memory_ex failed at every fallback size; "
                        "set TINMAXHEAP=0 to disable arena segregation\n");
        exit(1);
    }

    size_t actual_size = 0;
    void *base = mi_arena_area(_tin_arena_id, &actual_size);
    if (!base || actual_size == 0) {
        fprintf(stderr, "tin: mi_arena_area returned NULL after reservation\n");
        exit(1);
    }

    _tin_arena_base = base;
    _tin_arena_size = actual_size;
    _tin_arena_active = 1;
}

// Returns 1 only when ptr points at the start of a Tin rc-block --
// i.e. the public pointer of one of our allocations.  Two-stage check:
//
//   1. Range: ptr must fall inside [base, base+size).  Foreign
//      pointers (libc malloc / stack / rodata / mmap / extern) live
//      outside the exclusive arena and fail this.
//   2. Magic: the would-be header at (ptr - sizeof(TinRCHdr)) must
//      carry TIN_RC_HDR_MAGIC in its _pad slot.  _tin_rc_alloc and
//      friends stamp it at allocation; interior pointers (e.g.
//      &arr[5]) land inside block data where _pad mismatches and the
//      check fails, so _tin_retain_ptr / _tin_release_ptr safely
//      no-op rather than corrupting bytes 16 ahead of the pointer.
//
// Degraded mode (TINMAXHEAP=0): no arena, return 1 unconditionally
// for non-null pointers.  Retain/release on foreign pointers will then
// touch the header math; only useful in builds where the sanitizer
// already invalidates the arena trick.
int _tin_is_managed(void *ptr) {
    if (!ptr) return 0;
    if (!_tin_arena_active) return 1;
    uintptr_t addr = (uintptr_t)ptr;
    uintptr_t base = (uintptr_t)_tin_arena_base;
    if (addr - base >= _tin_arena_size) return 0;
    // ptr is in the arena.  The header sits at ptr - sizeof(TinRCHdr).
    // Reject pointers too close to the arena base for the subtract to
    // stay in-range -- there is no real rc-block whose header would
    // land before the arena, but the bounds-check keeps the read safe
    // under any future shifting allocator layouts.
    if (addr - base < sizeof(TinRCHdr)) return 0;
    TinRCHdr *hdr = (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
    return hdr->_pad == TIN_RC_HDR_MAGIC;
}

// Returns the calling thread's rc-heap, lazily creating it on the
// first ARC alloc this thread performs.  No locking: a thread only
// ever writes to its own TLS slot, so concurrent initialization on
// different threads doesn't share state.  Each newly-created heap is
// also enrolled in the global atomic list so atexit cleanup can
// destroy it.
mi_heap_t *_tin_managed_heap(void) {
    mi_heap_t *h = _tin_thread_heap;
    if (h != NULL) return h;

    if (!_tin_arena_active) {
        // Degraded mode: fall back to mimalloc's main heap (process-
        // wide, thread-safe via mi_malloc's TLS).  The atexit hook
        // does not destroy this -- mimalloc owns its lifetime.
        _tin_thread_heap = mi_heap_main();
        return _tin_thread_heap;
    }

    h = mi_heap_new_in_arena(_tin_arena_id);
    if (h == NULL) {
        // Arena exhausted or creation failed; fall back to the main
        // heap.  Subsequent allocs from this thread leak out of the
        // arena; _tin_is_managed will report those as foreign and
        // provenance-aware callers will skip them.  Not registered
        // for atexit destroy.
        _tin_thread_heap = mi_heap_main();
        return _tin_thread_heap;
    }

    // Register so atexit can destroy.  Uses libc malloc (not our
    // heap, obviously); the node leak on process exit is harmless.
    _tin_heap_node *node = (_tin_heap_node *)malloc(sizeof(_tin_heap_node));
    if (node != NULL) {
        node->heap = h;
        _tin_heap_node *head = atomic_load(&_tin_heap_list_head);
        for (;;) {
            node->next = head;
            if (atomic_compare_exchange_weak(&_tin_heap_list_head, &head, node)) break;
        }
    }

    _tin_thread_heap = h;
    return h;
}

// atexit hook: destroy every per-thread rc-heap we registered.  Frees
// all rc-blocks regardless of refcount -- intended for clean shutdown
// where the caller has guaranteed no live references remain (workers
// joined, runtime quiesced).  Used for leak-checker friendliness;
// uninstrumented builds would otherwise see every still-rc'd block
// reported as a leak on process exit.
static void _tin_arena_atexit(void) {
    // First let mimalloc reclaim its deferred free lists.  Then walk
    // the registered heap list and destroy each.  After this, any
    // _tin_release call would UB; the assumption is the program is
    // shutting down and no further runtime allocator calls happen.
    mi_collect(true);

    _tin_heap_node *node = atomic_load(&_tin_heap_list_head);
    while (node != NULL) {
        _tin_heap_node *next = node->next;
        mi_heap_destroy(node->heap);
        free(node);
        node = next;
    }
}

__attribute__((constructor(101))) static void _tin_heap_arena_ctor(void) {
    _tin_heap_init();
    atexit(_tin_arena_atexit);
}
