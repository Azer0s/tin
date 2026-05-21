// tin runtime - heap arena segregation for biased RC.
//
// Reserves a large virtual address range at startup and registers it
// with mimalloc as an exclusive arena.  Tin's `_tin_rc_alloc` routes
// every managed allocation through this arena, so the runtime can
// distinguish Tin-managed pointers from C-managed ones via a cheap
// range check (`_tin_is_managed`).
//
// The virtual reservation is essentially free: `mmap(PROT_NONE)` only
// registers a hole in the process's VMA list, no RAM is committed
// until pages are touched.  Pages commit lazily as mimalloc grows
// into the arena; `madvise(MADV_DONTNEED)` returns RSS to the OS when
// regions go cold.
//
// Configuration via `TINMAXHEAP` env var (default 16 GB):
//   TINMAXHEAP=64G  -> reserve 64 GiB virtual.
//   TINMAXHEAP=2048M -> reserve 2 GiB.
//   TINMAXHEAP=0    -> disable the arena; allocations come from
//                       generic mimalloc heaps and `_tin_is_managed`
//                       always returns 1 (degraded mode; legacy
//                       behavior, useful for debugging only).

#include "runtime.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <sys/mman.h>

#if !TIN_USE_MIMALLOC
#  error "heap_arena.c requires TIN_USE_MIMALLOC=1; mimalloc is now a hard dependency"
#endif

#include <mimalloc.h>

// Single-arena configuration.  Multi-arena fallback (when the initial
// reservation exhausts) is a future extension; for now a single
// generous reservation covers every realistic workload.
//
// The arena_id and bounds are process-global (write-once at init).
// The HEAP is per-thread because mimalloc heaps are not thread-safe
// for concurrent allocations -- each thread that calls _tin_rc_alloc
// lazily creates its own heap pinned to our arena, so all Tin
// allocations land in the segregated address range.
static mi_arena_id_t _tin_arena_id;
static void         *_tin_arena_base;
static size_t        _tin_arena_size;
static int           _tin_arena_active;

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

// One-shot init.  Called from the C-main prologue before any user
// code runs, before any `_tin_rc_alloc` call.  Idempotent under
// repeated calls (re-entries are no-ops).
void _tin_heap_init(void) {
    if (_tin_arena_active) return;

    size_t want = _tin_parse_heap_size(getenv("TINMAXHEAP"));
    if (want == 0) {
        // Degraded mode: skip arena setup.  _tin_is_managed will
        // return 1 for any non-null pointer; retain/release falls
        // back to the legacy "trust the caller" behavior.  Useful
        // for sanitizer-instrumented builds where mimalloc arenas
        // interfere with shadow memory.
        return;
    }

    // Reserve our arena via mimalloc.  `exclusive=false` adds the arena
    // to mimalloc's global pool: the default (per-thread, thread-safe)
    // heap allocates from it preferentially, so all `mi_malloc` calls
    // through the runtime's shim land in the Tin range.  This avoids
    // the need for explicit per-thread heaps -- those introduce a
    // race in `mi_heap_new_in_arena` on first concurrent allocation
    // that manifested as fmutex deadlocks in fiber-heavy tests.
    //   commit=false:       reserve virtually; commit pages on demand.
    //   allow_large=false:  no huge pages (avoids host-specific config).
    //
    // macOS and some Linux configurations cap large virtual reservations
    // (Apple silicon hosts reject ~16GB+ reservations even with
    // unlimited ulimit -v).  Halve the requested size on failure and
    // retry down to 256 MiB before giving up.
    int err = 0;
    while (want >= (256ULL << 20)) {
        err = mi_reserve_os_memory_ex(want,
                                      /* commit       */ false,
                                      /* allow_large  */ false,
                                      /* exclusive    */ false,
                                      &_tin_arena_id);
        if (err == 0) break;
        want /= 2;
    }
    if (err != 0) {
        fprintf(stderr, "tin: mi_reserve_os_memory_ex failed at every fallback size; "
                        "set TINMAXHEAP=0 to disable arena segregation\n");
        exit(1);
    }

    // Pull back the actual address mimalloc reserved so _tin_is_managed
    // can range-check against it.
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

// Returns 1 when `ptr` is an address Tin's allocator handed out, 0
// otherwise.  The cheap range check covers the entire reserved
// arena; mimalloc may not have committed every page yet, but a Tin
// allocation that produced `ptr` has by definition committed the
// containing region.  Pointers that escaped from C / static data /
// stack / foreign mmap'd regions land outside the arena range and
// return 0.
//
// Degraded mode (`TINMAXHEAP=0`): `_tin_arena_active == 0`, the
// function returns 1 unconditionally for non-null pointers --
// equivalent to today's pre-segregation behavior.
int _tin_is_managed(void *ptr) {
    if (!ptr) return 0;
    if (!_tin_arena_active) return 1;
    uintptr_t addr = (uintptr_t)ptr;
    uintptr_t base = (uintptr_t)_tin_arena_base;
    return addr - base < _tin_arena_size;
}

// Internal accessor used by arc.c::_tin_rc_alloc to route managed
// allocations through the arena's dedicated heap.  When the arena is
// inactive (degraded mode), returns NULL and the caller falls back
// to mi_malloc.
//
// Each calling thread gets its own heap pinned to our arena -- mimalloc
// heaps are not thread-safe for concurrent allocations, but threads
// allocating from heaps in the same arena all land in our segregated
// virtual range.  The arena membership is what _tin_is_managed checks;
// the per-heap split is just so concurrent allocators don't trample
// each other's free lists.
// With exclusive=false the runtime's default mi_malloc allocates from
// our arena automatically, so no Tin-specific heap is needed.
// `_tin_managed_heap` always returns NULL; arc.c's _tin_arena_alloc
// falls through to plain malloc (the macro shim routes to mi_malloc).
mi_heap_t *_tin_managed_heap(void) {
    return NULL;
}

// Initialize the arena as soon as the binary loads -- before any
// constructor in the rest of the runtime touches the allocator.
// Constructor priority 101 is the lowest non-reserved value, so this
// fires before any user-priority constructor; libc's allocator setup
// runs even earlier via priority 0-100.
__attribute__((constructor(101))) static void _tin_heap_arena_ctor(void) {
    _tin_heap_init();
}
