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

#include <setjmp.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// Valgrind client requests for marking the magic probe as intentional
// out-of-bounds.  Compiled in ONLY when the user passes --valgrind to
// `tin run`/`tin test`; the build driver injects -DTIN_VALGRIND=1 and
// the csrc cache produces a distinct runtime.o that carries this code.
// Default builds get a clean fast path with no rolq sequences, no
// RUNNING_ON_VALGRIND probes, and zero runtime overhead from valgrind.
#ifdef TIN_VALGRIND
#  include <valgrind/memcheck.h>
#endif

// A foreign pointer can sit within sizeof(TinRCHdr) bytes of a page
// boundary; reading 8 bytes at ptr-8 then crosses into the prior page,
// which may be unmapped (different VMA from a separate mmap region).
// A SIGBUS handler with a sigsetjmp landing pad catches the fault and
// returns "not managed" instead of crashing the process.  The handler
// is installed once at startup; the per-call cost is a thread-local
// load + sigsetjmp (~10 ns) and fires only on the page-boundary slow
// path -- the common, page-interior case stays a plain load.
static __thread sigjmp_buf _tin_probe_jmpbuf;
static __thread int        _tin_probe_active;

static struct sigaction _tin_prev_sigsegv;
static struct sigaction _tin_prev_sigbus;

static void _tin_probe_signal_handler(int sig, siginfo_t *si, void *ctx) {
    if (_tin_probe_active) {
        // The fault came from inside a probe; jump back to the safe
        // pad in _tin_probe_magic_slow so we can report "not managed".
        siglongjmp(_tin_probe_jmpbuf, 1);
    }
    // Genuine SIGSEGV / SIGBUS from user code: forward to whatever was
    // installed before us (debugger, the program's own handler, or the
    // default which terminates the process with the same signal).
    struct sigaction *prev = (sig == SIGBUS) ? &_tin_prev_sigbus : &_tin_prev_sigsegv;
    if (prev->sa_flags & SA_SIGINFO) {
        if (prev->sa_sigaction != NULL) {
            prev->sa_sigaction(sig, si, ctx);
            return;
        }
    } else if (prev->sa_handler != SIG_DFL && prev->sa_handler != SIG_IGN) {
        prev->sa_handler(sig);
        return;
    }
    // Default: re-raise with SIG_DFL so the kernel can produce the crash report.
    struct sigaction dfl = {0};
    dfl.sa_handler = SIG_DFL;
    sigaction(sig, &dfl, NULL);
    raise(sig);
}

__attribute__((constructor(95)))
static void _tin_install_probe_handler(void) {
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = _tin_probe_signal_handler;
    sa.sa_flags = SA_SIGINFO | SA_NODEFER;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGSEGV, &sa, &_tin_prev_sigsegv);
    sigaction(SIGBUS,  &sa, &_tin_prev_sigbus);
}

static uint64_t _tin_probe_magic_slow(const TinRCHdr *hdr) {
    uint64_t pad = 0;
    _tin_probe_active = 1;
    if (sigsetjmp(_tin_probe_jmpbuf, 1) == 0) {
        pad = hdr->_pad;
    } // else: fault path; pad stays 0, _tin_is_managed returns 0
    _tin_probe_active = 0;
    return pad;
}

// _tin_probe_magic reads the 8-byte _pad slot in the would-be
// TinRCHdr at `ptr - sizeof(TinRCHdr)`.  When running under valgrind
// the surrounding bytes may be NOACCESS (allocator metadata) or even
// in a freed region; the probe is intentional and known to land
// outside the user's allocation for foreign pointers, so save the
// vbits, mark the slot defined for the read, then restore the vbits
// so memcheck still flags genuine use-after-frees of nearby memory
// from elsewhere.  The valgrind dance is only needed when the read
// might touch foreign bytes; for the in-page interior fast path the
// callers below do a direct load and reach this function only on the
// page-boundary slow path.
// Page-boundary slow path: ptr's hdr might straddle into an unmapped
// prior page (foreign mmap region).  Under valgrind we save the vbits
// of the probe slot, mark it defined for the read, then restore the
// vbits so memcheck still flags genuine use-after-frees of nearby
// memory from elsewhere.  This matches the pre-gated machinery the
// magic-probe shipped with before TIN_VALGRIND existed -- vbits cover
// uninit reads of allocator metadata AND silence the "invalid read"
// flag on foreign blocks, both of which are intentional for the
// foreign-pointer probe.
static uint64_t _tin_probe_magic_slow_path(const TinRCHdr *hdr) {
#ifdef TIN_VALGRIND
    unsigned char vbits[sizeof(hdr->_pad)];
    int saved = (VALGRIND_GET_VBITS(&hdr->_pad, vbits, sizeof(hdr->_pad)) == 0);
    (void)VALGRIND_MAKE_MEM_DEFINED(&hdr->_pad, sizeof(hdr->_pad));
    uint64_t pad = _tin_probe_magic_slow(hdr);
    if (saved) (void)VALGRIND_SET_VBITS(&hdr->_pad, vbits, sizeof(hdr->_pad));
    return pad;
#else
    return _tin_probe_magic_slow(hdr);
#endif
}

// _tin_probe_might_cross_page: 1 iff the would-be TinRCHdr (16 bytes
// ending at ptr) might straddle a page boundary into a possibly-unmapped
// prior page.  4 KiB is the smaller of the host page sizes we target
// (linux 4K, macOS arm64 16K) -- if 4K covers the read, 16K covers it
// too.  Branch is heavily biased toward "no" (page interior = ~99% of
// pointers), so callers use __builtin_expect to keep the slow path off
// the hot ifetch.
#define _tin_probe_might_cross_page(ptr) \
    (((uintptr_t)(ptr) & 0xFFFULL) < sizeof(TinRCHdr))

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
    // Fast path: ptr's page is the same as hdr's page, so the read can
    // never fault.  In a default (non-valgrind) build this lowers to a
    // single load + compare with no signal-handler dance and no
    // RUNNING_ON_VALGRIND inline asm.
#ifdef TIN_VALGRIND
    // Valgrind build: even the in-page read of a foreign pointer would
    // flag "conditional jump on uninitialised value", so route every
    // probe through the vbits-protected slow path.
    return _tin_probe_magic_slow_path(hdr) == TIN_RC_HDR_MAGIC;
#else
    if (__builtin_expect(!_tin_probe_might_cross_page(ptr), 1)) {
        return hdr->_pad == TIN_RC_HDR_MAGIC;
    }
    return _tin_probe_magic_slow_path(hdr) == TIN_RC_HDR_MAGIC;
#endif
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
#ifdef TIN_VALGRIND
    return _tin_probe_magic_slow_path(hdr) == TIN_RC_HDR_MAGIC;
#else
    if (__builtin_expect(!_tin_probe_might_cross_page(ptr), 1)) {
        return hdr->_pad == TIN_RC_HDR_MAGIC;
    }
    return _tin_probe_magic_slow_path(hdr) == TIN_RC_HDR_MAGIC;
#endif
}

#endif // TIN_USE_MIMALLOC
