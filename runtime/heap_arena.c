// tin runtime - heap arena segregation (mimalloc) + magic-only fallback.
//
// Two build modes:
//
// 1. TIN_USE_MIMALLOC=1 (default): reserve a TINMAXHEAP-sized
//    exclusive mimalloc arena at startup.  Every rc-block is
//    allocated from a per-thread mi_heap pinned to the arena, so
//    _tin_is_managed has a cheap range check ("in arena") AND
//    a sanity check ("header carries TIN_RC_HDR_MAGIC").  Per-thread
//    heaps are tracked in a lock-free linked list; atexit destroys
//    them for leak-checker friendliness.
//
// 2. TIN_USE_MIMALLOC=0 (--no-mimalloc): no arena, no per-thread
//    heaps; rc-blocks come from libc malloc.  _tin_is_managed then
//    relies solely on the header magic at `ptr - sizeof(TinRCHdr)`.
//    Foreign-pointer false positives are statistically impossible
//    (64-bit nonce).  Reads of `ptr - 16` for non-rc pointers may
//    touch random memory; in practice this is safe (libc malloc
//    aligns to 16, stack/rodata reads succeed but magic mismatches),
//    but pointers near unmapped page boundaries could fault.  We
//    accept that for now -- the arena's range check is the more
//    robust answer if a workload hits the boundary case.
//
// Configuration via TINMAXHEAP env var (default 16 GiB, mimalloc mode
// only):
//   TINMAXHEAP=64G   -> reserve 64 GiB virtual.
//   TINMAXHEAP=2048M -> reserve 2 GiB.
//   TINMAXHEAP=0     -> disable the arena; rc-blocks fall back to
//                       mimalloc's main heap (still mimalloc, just
//                       not the exclusive arena).

#include "runtime.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// When valgrind/memcheck.h is available, expand to client requests
// that let memcheck distinguish the intentional out-of-bounds magic
// probe in _tin_is_managed from a real invalid read.  Outside
// valgrind the macros are zero-cost no-ops; outside the header the
// probe is unconditional and the wrappers below fall back to a
// plain read.
#if defined(__has_include)
#  if __has_include(<valgrind/memcheck.h>)
#    include <valgrind/memcheck.h>
#    define _TIN_VG_PRESENT 1
#  endif
#endif

// _tin_probe_magic reads the 8-byte _pad slot in the would-be
// TinRCHdr at `ptr - sizeof(TinRCHdr)`.  When running under valgrind
// the surrounding bytes may be NOACCESS (allocator metadata) or even
// in a freed region; the probe is intentional and known to land
// outside the user's allocation for foreign pointers, so save the
// vbits, mark the slot defined for the read, then restore the vbits
// so memcheck still flags genuine use-after-frees of nearby memory
// from elsewhere.
static inline uint64_t _tin_probe_magic(const TinRCHdr *hdr) {
#ifdef _TIN_VG_PRESENT
    unsigned char vbits[sizeof(hdr->_pad)];
    int saved = (VALGRIND_GET_VBITS(&hdr->_pad, vbits, sizeof(hdr->_pad)) == 0);
    (void)VALGRIND_MAKE_MEM_DEFINED(&hdr->_pad, sizeof(hdr->_pad));
    uint64_t pad = hdr->_pad;
    if (saved) (void)VALGRIND_SET_VBITS(&hdr->_pad, vbits, sizeof(hdr->_pad));
    return pad;
#else
    return hdr->_pad;
#endif
}

#if TIN_USE_MIMALLOC

#include <stdatomic.h>
#include <mimalloc.h>

static mi_arena_id_t _tin_arena_id;
static void         *_tin_arena_base;
static size_t        _tin_arena_size;
static int           _tin_arena_active;

static __thread mi_heap_t *_tin_thread_heap;

typedef struct _tin_heap_node {
    mi_heap_t            *heap;
    struct _tin_heap_node *next;
} _tin_heap_node;

static _Atomic(_tin_heap_node *) _tin_heap_list_head;

#define TIN_DEFAULT_HEAP_GB 16

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

void _tin_heap_init(void) {
    if (_tin_arena_active) return;

    size_t want = _tin_parse_heap_size(getenv("TINMAXHEAP"));
    if (want == 0) {
        return; // arena disabled; main heap fallback
    }

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

// Returns 1 only when ptr points at the start of a Tin rc-block that
// is currently allocated.
//
// With the arena active, two stages:
//   1. Range: ptr must fall inside [base, base+size).
//   2. Magic: header's _pad slot must equal TIN_RC_HDR_MAGIC.
//
// Without the arena (TINMAXHEAP=0), we have no range to check, so we
// fall through to the magic-only check used by the --no-mimalloc
// build.  Reading `ptr - sizeof(TinRCHdr)` may touch a few bytes
// outside foreign allocations; on every architecture we target the
// nearby memory is mapped and the read just returns bytes that don't
// match the nonce.
int _tin_is_managed(void *ptr) {
    if (!ptr) return 0;
    if (_tin_arena_active) {
        uintptr_t addr = (uintptr_t)ptr;
        uintptr_t base = (uintptr_t)_tin_arena_base;
        if (addr - base >= _tin_arena_size) return 0;
        if (addr - base < sizeof(TinRCHdr))  return 0;
    }
    TinRCHdr *hdr = (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
    return _tin_probe_magic(hdr) == TIN_RC_HDR_MAGIC;
}

// _tin_mi_default_heap returns a usable heap for allocations that
// don't need the per-arena slot.  mimalloc 3.x exposes mi_heap_main;
// mimalloc 2.x (Ubuntu 24.04 ships 2.1.2) renamed it to
// mi_heap_get_default.  The format of MI_MALLOC_VERSION differs
// across the rename (3-digit pre-3.0, 5-digit 3.0+), and the
// numeric threshold cleanly separates them.
static inline mi_heap_t *_tin_mi_default_heap(void) {
#if MI_MALLOC_VERSION >= 30000
    return mi_heap_main();
#else
    return mi_heap_get_default();
#endif
}

mi_heap_t *_tin_managed_heap(void) {
    mi_heap_t *h = _tin_thread_heap;
    if (h != NULL) return h;

    if (!_tin_arena_active) {
        _tin_thread_heap = _tin_mi_default_heap();
        return _tin_thread_heap;
    }

    h = mi_heap_new_in_arena(_tin_arena_id);
    if (h == NULL) {
        _tin_thread_heap = _tin_mi_default_heap();
        return _tin_thread_heap;
    }

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

static void _tin_arena_atexit(void) {
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

#else // !TIN_USE_MIMALLOC

// --no-mimalloc build: no arena, no per-thread heaps.  rc-blocks come
// from libc malloc.  _tin_is_managed relies entirely on the header
// magic stamped by _tin_rc_alloc into hdr->_pad.  Raw addresses from
// `addr(int_literal)` carry the `volatile *T` type which skips
// rc-tracking entirely in codegen, so they never reach this function.
int _tin_is_managed(void *ptr) {
    if (!ptr) return 0;
    TinRCHdr *hdr = (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
    return _tin_probe_magic(hdr) == TIN_RC_HDR_MAGIC;
}

#endif // TIN_USE_MIMALLOC
