// tin runtime - pclntab reader (PC -> file:line:col)
//
// Replaces libdw / DWARF entirely for stacktrace symbol resolution. The
// codegen post-pass (codegen/pclntab.go) emits one TinPclnFnHdr per Tin
// function into a custom binary section ("tin_pclntab" on ELF, "__TIN,
// __pclntab" on Mach-O) and a per-image __attribute__((constructor))
// that calls _tin_pclntab_register_self() at load time.
//
// Why a custom table instead of DWARF:
//   - libdw / libdwfl pulls in elfutils, which links against libcurl /
//     libssl through debuginfod (~40 KiB of "still reachable" allocs
//     under valgrind even with debuginfod disabled).
//   - DWARF is the wrong format for a runtime-resolved table: it's
//     designed for offline tools (lldb, gdb), not for programs that
//     need millisecond-grade lookups during exception capture.
//   - Mach-O has no equivalent of libdwfl; runtime resolution on macOS
//     would need a separate DWARF reader, doubling the surface area.
//   - Go's pclntab proves the design works at scale; we adopt the
//     same structure (per-fn header + per-call PC delta table).
//
// Process-wide table:
//   Each loaded image (main binary + every dlopen'd REPL cell) appends
//   its (start, end) section range to a global vector under a mutex.
//   First lookup sorts the cumulative header list by fn_start; further
//   reads are lock-free via atomic seqlock-style versioning.
//
// ASLR safety:
//   The fn_start pointers are absolute addresses, patched at load time
//   by ld.so / dyld. The pc_off entries are constant 32-bit offsets
//   computed at link time as `blockaddress(@fn,%bb) - @fn`, which the
//   linker resolves to a fixed `.long X-Y` because both symbols land
//   in the same .text section (`-ffunction-sections` keeps them within
//   the per-fn section, but the relative distance is invariant under
//   ASLR — the kernel slides every text mapping by the same delta).
//
// Concurrency:
//   - Registration: takes _tin_pclntab_mu.
//   - First lookup: takes _tin_pclntab_mu, sorts, publishes via
//     atomic_store(_release, ...) on _sorted.
//   - Steady-state lookup: atomic_load(_acquire, ...) on _sorted, then
//     binary search the (immutable until next dlopen) array.
//   - dlopen mid-lookup: serializes on _tin_pclntab_mu. The new image's
//     constructor runs to completion (registering its hdrs), then
//     re-sorts. In-flight lookups against the old array are correct
//     for any IP that was resolvable before the dlopen; new IPs from
//     the just-loaded image become resolvable after the next sort.

#include "runtime.h"

#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <stdatomic.h>

// -- Per-image discovery
//
// Each Tin-compiled image (main binary + every dlopen'd .so) gets a
// codegen-emitted constructor (codegen/pclntab.go emitPclntabConstructor)
// that calls _tin_pclntab_register_image with PER-IMAGE args:
//
//   ELF (Linux / FreeBSD): the constructor's IR carries extern_weak
//     declarations of __start_/__stop_tin_pclntab; the linker resolves
//     each declaration to THIS image's section bounds. The constructor
//     passes those addresses to _tin_pclntab_register_image.
//
//   Mach-O (macOS): the IR can't synthesize __start_/__stop_, so the
//     constructor passes its own address as a marker; this file uses
//     dladdr + getsectiondata to find the section.
//
// This pattern works whether the runtime is linked into the same image
// or only into the main binary — every image's constructor passes its
// OWN section data, so the registrar resolves correctly per call even
// when called from a dlopen'd .so back into the main binary's runtime.
#if defined(__APPLE__)
#  include <mach-o/dyld.h>
#  include <mach-o/getsect.h>
#  include <dlfcn.h>
#endif

// -- Process-wide registry of (image_start, image_end) ranges.
//
// Ranges are kept separately from the flattened header array because the
// flattening pass may re-run on each new dlopen and the original ranges
// give us a stable input. Storage grows dynamically — no silent drop
// when a long REPL session loads many cells.
typedef struct {
    const TinPclnFnHdr *start;
    const TinPclnFnHdr *end;
} PclnRange;

static PclnRange       *_pcln_ranges       = NULL;
static int              _pcln_range_count  = 0;
static int              _pcln_range_cap    = 0;
static pthread_mutex_t  _pcln_mu           = PTHREAD_MUTEX_INITIALIZER;

// Forward declaration so _sorted's element type is visible in
// declarations preceding the slot definition.
struct PclnFnSlot;

// Sorted-by-fn_start view of every header from every registered range.
// Built lazily on first resolve, rebuilt whenever a new range registers
// after the first build. atomic_load(_sorted) is the readers' fast path;
// when NULL, take the mutex and (re)build under exclusive ownership.
static _Atomic(struct PclnFnSlot *) _sorted       = NULL;
static _Atomic(int)                 _sorted_count = 0;

// Each fn's pcs needs runtime sorting by pc_addr: codegen emits them in
// IR-block order, but LLVM's layout optimizer reorders basic blocks
// (especially at -O2), so the IR order does NOT match the final binary
// layout. We can't write the rodata pcs in place, so we keep parallel
// sorted-pcs arrays in heap.
//
// The sorted-pcs heap allocation is reused across resorts via the
// per-hdr cache below. Each PclnFnSlot holds a pointer to the cache
// entry rather than owning its sorted_pcs — so re-registering doesn't
// re-allocate or re-sort already-known hdrs.
struct PclnHdrCache {
    const TinPclnFnHdr *hdr;
    TinPclnPC          *sorted_pcs;  // sorted copy (owned)
    uint32_t            npcs;
    const void         *max_pc_addr; // sorted_pcs[npcs-1].pc_addr or NULL
};
typedef struct PclnHdrCache PclnHdrCache;

// Process-wide cache of hdr -> sorted_pcs. Grows monotonically over the
// process lifetime. Each entry's sorted_pcs is allocated once on first
// resort containing this hdr and reused forever (hdrs are read-only and
// never unregistered).
//
// Indirect storage: the index array (`_hdr_cache`) stores POINTERS to
// individually-malloc'd PclnHdrCache structs. The struct addresses
// stay stable across cache growth, so PclnFnSlot.cache pointers remain
// valid even when concurrent readers hold them across a resort that
// triggered an index-array realloc.
static PclnHdrCache **_hdr_cache       = NULL;
static int            _hdr_cache_count = 0;
static int            _hdr_cache_cap   = 0;

struct PclnFnSlot {
    const PclnHdrCache *cache; // stable pointer to a malloc'd entry
};
typedef struct PclnFnSlot PclnFnSlot;

// _hdr_cache_find returns the existing cache entry for hdr, or NULL.
// Linear scan: hdr count is typically a few hundred; if profiling ever
// shows this dominating, swap for an open-addressed hash table keyed on
// the hdr pointer.
static PclnHdrCache *_hdr_cache_find(const TinPclnFnHdr *hdr) {
    for (int i = 0; i < _hdr_cache_count; i++) {
        if (_hdr_cache[i]->hdr == hdr) return _hdr_cache[i];
    }
    return NULL;
}

// _hdr_cache_intern returns a cache entry for hdr, building one if
// missing. Builds the sorted-pcs copy and computes max_pc_addr. Returns
// NULL on OOM. Caller MUST hold _pcln_mu.
static PclnHdrCache *_hdr_cache_intern(const TinPclnFnHdr *hdr) {
    PclnHdrCache *e = _hdr_cache_find(hdr);
    if (e != NULL) return e;

    if (_hdr_cache_count == _hdr_cache_cap) {
        int new_cap = _hdr_cache_cap == 0 ? 64 : _hdr_cache_cap * 2;
        PclnHdrCache **new_arr = (PclnHdrCache **)realloc(
            _hdr_cache, (size_t)new_cap * sizeof(PclnHdrCache *));
        if (new_arr == NULL) return NULL;
        _hdr_cache = new_arr;
        _hdr_cache_cap = new_cap;
    }

    e = (PclnHdrCache *)malloc(sizeof(PclnHdrCache));
    if (e == NULL) return NULL;
    e->hdr = hdr;
    e->npcs = 0;
    e->sorted_pcs = NULL;
    e->max_pc_addr = NULL;

    if (hdr->npcs > 0 && hdr->pcs != NULL) {
        TinPclnPC *spcs = (TinPclnPC *)malloc((size_t)hdr->npcs * sizeof(TinPclnPC));
        if (spcs != NULL) {
            memcpy(spcs, hdr->pcs, (size_t)hdr->npcs * sizeof(TinPclnPC));
            // Insertion sort: per-fn npcs is typically tens.
            for (uint32_t a = 1; a < hdr->npcs; a++) {
                TinPclnPC cur = spcs[a];
                uint32_t b = a;
                while (b > 0 && spcs[b - 1].pc_addr > cur.pc_addr) {
                    spcs[b] = spcs[b - 1];
                    b--;
                }
                spcs[b] = cur;
            }
            e->sorted_pcs = spcs;
            e->npcs = hdr->npcs;
            e->max_pc_addr = spcs[hdr->npcs - 1].pc_addr;
        }
        // On OOM: leave sorted_pcs=NULL; resolver treats as marker-only.
    }

    _hdr_cache[_hdr_cache_count++] = e;
    return e;
}

// _pcln_resort_cmp: qsort comparator for PclnFnSlot, sorts by fn_start.
// Cast through uintptr_t to avoid signed-pointer-arithmetic UB on huge
// 64-bit address spaces.
static int _pcln_resort_cmp(const void *a, const void *b) {
    uintptr_t pa = (uintptr_t)((const PclnFnSlot *)a)->cache->hdr->fn_start;
    uintptr_t pb = (uintptr_t)((const PclnFnSlot *)b)->cache->hdr->fn_start;
    return (pa > pb) - (pa < pb);
}

// _pcln_retired holds the previous _sorted array between resorts. Two
// generations are kept alive at any time (current + retired); the
// generation BEFORE the retired one gets freed when a new resort runs.
// This bounds leakage to 2× the largest sort table, vs. unbounded under
// the previous "leak everything" scheme.
static PclnFnSlot *_pcln_retired = NULL;

// _pcln_resort rebuilds _sorted from _pcln_ranges. Caller MUST hold
// _pcln_mu.
//
// Reuses per-hdr sorted_pcs from _hdr_cache so re-registering doesn't
// re-allocate or re-sort already-known hdrs. Only the _sorted slot
// array (small: 8 bytes per fn) is allocated fresh.
static void _pcln_resort(void) {
    int total = 0;
    for (int i = 0; i < _pcln_range_count; i++) {
        total += (int)(_pcln_ranges[i].end - _pcln_ranges[i].start);
    }

    if (total <= 0) {
        atomic_store_explicit(&_sorted, NULL, memory_order_release);
        atomic_store_explicit(&_sorted_count, 0, memory_order_release);
        return;
    }

    PclnFnSlot *arr = (PclnFnSlot *)malloc((size_t)total * sizeof(PclnFnSlot));
    if (arr == NULL) {
        // OOM — leave previous _sorted in place; readers keep working
        // for already-resolvable IPs.
        return;
    }

    int k = 0;
    for (int i = 0; i < _pcln_range_count; i++) {
        for (const TinPclnFnHdr *h = _pcln_ranges[i].start; h < _pcln_ranges[i].end; h++) {
            PclnHdrCache *e = _hdr_cache_intern(h);
            if (e == NULL) continue;  // OOM — drop this entry
            arr[k++].cache = e;
        }
    }

    qsort(arr, (size_t)k, sizeof(PclnFnSlot), _pcln_resort_cmp);

    // Publish new sorted view. Free the prev-prev generation. The prev
    // (retired) generation may still be in use by readers between the
    // atomic_load and binary-search; we don't free it now. Next call
    // will free it (by which point any reader using it has finished —
    // resort happens only on dlopen, which is infrequent compared to
    // a binary search).
    PclnFnSlot *prev = atomic_load_explicit(&_sorted, memory_order_acquire);
    PclnFnSlot *to_free = _pcln_retired;
    _pcln_retired = prev;

    atomic_store_explicit(&_sorted, arr, memory_order_release);
    atomic_store_explicit(&_sorted_count, k, memory_order_release);

    if (to_free != NULL) free(to_free);
}

// _pcln_register_range adds (start, end) to the per-image ranges and
// invalidates _sorted so the next lookup rebuilds. Internal.
static void _pcln_register_range(const TinPclnFnHdr *start, const TinPclnFnHdr *end) {
    if (start == NULL || end == NULL || end <= start) return;

    pthread_mutex_lock(&_pcln_mu);

    // Dedup: skip if exact (start, end) is already registered. Each
    // image's constructor fires once but this is defensive against
    // double-registration if a future change adds another path.
    int seen = 0;
    for (int i = 0; i < _pcln_range_count; i++) {
        if (_pcln_ranges[i].start == start && _pcln_ranges[i].end == end) {
            seen = 1;
            break;
        }
    }

    if (!seen) {
        // Grow ranges dynamically. Doubling keeps amortized O(1) appends;
        // initial capacity 16 is enough for typical programs (1 main +
        // a few plugin .sos), and REPL sessions with hundreds of cells
        // grow without dropping.
        if (_pcln_range_count == _pcln_range_cap) {
            int new_cap = _pcln_range_cap == 0 ? 16 : _pcln_range_cap * 2;
            PclnRange *new_arr = (PclnRange *)realloc(
                _pcln_ranges, (size_t)new_cap * sizeof(PclnRange));
            if (new_arr == NULL) {
                // OOM: drop this registration silently. The previously
                // registered ranges keep working; the missing ones
                // render frames as `??+0x<addr>` (correct miss
                // attribution, not wrong attribution).
                pthread_mutex_unlock(&_pcln_mu);
                return;
            }
            _pcln_ranges = new_arr;
            _pcln_range_cap = new_cap;
        }

        _pcln_ranges[_pcln_range_count].start = start;
        _pcln_ranges[_pcln_range_count].end   = end;
        _pcln_range_count++;
        // Invalidate sorted view; next lookup rebuilds.
        atomic_store_explicit(&_sorted, NULL, memory_order_release);
        atomic_store_explicit(&_sorted_count, 0, memory_order_release);
    }

    pthread_mutex_unlock(&_pcln_mu);
}

// _tin_pclntab_register_image is called by the codegen-emitted
// __tin_pclntab_ctor in @llvm.global_ctors. Each loaded image (main
// binary + every dlopen'd REPL cell or plugin .so) calls this once at
// load time, passing per-image arguments so the same helper can
// resolve different sections per call.
//
// ELF (Linux / FreeBSD): the constructor passes start/end as references
// to its own image's linker-synthesized __start_/__stop_tin_pclntab.
// The references resolve correctly per image because each image gets
// its own section; the linker fills in the correct bounds.
//
// Mach-O (macOS): no __start_/__stop_ symbols exist. The constructor
// passes its own address as `marker`; we use dladdr to find the
// image and getsectiondata for the section bounds.
// _pcln_atexit frees process-wide pclntab state at shutdown so valgrind
// sees a clean exit. Not strictly required for correctness — the OS
// reclaims all memory when the process exits — but a clean valgrind
// report is the project convention.
//
// Idempotent: caller (atexit) runs it exactly once. We zero pointers
// after free defensively in case a future change retains them.
static void _pcln_atexit(void) {
    pthread_mutex_lock(&_pcln_mu);

    PclnFnSlot *cur = atomic_load_explicit(&_sorted, memory_order_acquire);
    if (cur != NULL) free(cur);
    atomic_store_explicit(&_sorted, NULL, memory_order_release);
    atomic_store_explicit(&_sorted_count, 0, memory_order_release);

    if (_pcln_retired != NULL) {
        free(_pcln_retired);
        _pcln_retired = NULL;
    }

    if (_hdr_cache != NULL) {
        for (int i = 0; i < _hdr_cache_count; i++) {
            if (_hdr_cache[i] != NULL) {
                if (_hdr_cache[i]->sorted_pcs != NULL) {
                    free(_hdr_cache[i]->sorted_pcs);
                }
                free(_hdr_cache[i]);
            }
        }
        free(_hdr_cache);
        _hdr_cache = NULL;
        _hdr_cache_count = 0;
        _hdr_cache_cap = 0;
    }

    if (_pcln_ranges != NULL) {
        free(_pcln_ranges);
        _pcln_ranges = NULL;
        _pcln_range_count = 0;
        _pcln_range_cap = 0;
    }

    pthread_mutex_unlock(&_pcln_mu);
}

// _pcln_atexit_armed ensures the atexit hook is installed at most once
// across all images. atexit hooks are process-wide and don't dedupe; if
// every cell.so installed one we'd run the cleanup multiple times and
// double-free.
static atomic_int _pcln_atexit_armed = 0;

void _tin_pclntab_register_image(const TinPclnFnHdr *start,
                                 const TinPclnFnHdr *end,
                                 const void *marker) {
    // Arm the atexit cleanup on first registration. Cmpxchg makes this
    // race-safe even if the first two cells' constructors happen to
    // race here (which they don't in practice — dlopen serializes — but
    // costs nothing to make airtight).
    int expected = 0;
    if (atomic_compare_exchange_strong(&_pcln_atexit_armed, &expected, 1)) {
        atexit(_pcln_atexit);
    }

#if defined(__APPLE__)
    if (marker == NULL) return;

    Dl_info info;
    if (dladdr(marker, &info) == 0) return;

    const struct mach_header *hdr = (const struct mach_header *)info.dli_fbase;
    if (hdr == NULL) return;

    unsigned long size = 0;
    const uint8_t *data = NULL;

#  ifdef __LP64__
    data = getsectiondata((const struct mach_header_64 *)hdr,
                          "__TIN", "__pclntab", &size);
#  else
    data = getsectiondata((const struct mach_header *)hdr,
                          "__TIN", "__pclntab", &size);
#  endif

    if (data == NULL || size == 0) return;

    const TinPclnFnHdr *s = (const TinPclnFnHdr *)data;
    const TinPclnFnHdr *e = (const TinPclnFnHdr *)(data + size);
    _pcln_register_range(s, e);

    (void)start; (void)end;
#else
    (void)marker;
    if (start == NULL || end == NULL || start == end) return;
    _pcln_register_range(start, end);
#endif
}

// Bound the per-fn IP range for misattribution defence: an IP that's
// past `max_pc_addr + PCLN_FN_TAIL_SLACK` is treated as not belonging
// to this Tin function (probably inter-fn padding or a foreign symbol
// the linker placed in the same section gap).
//
// The slack accounts for the post-call sled — between the last
// recorded BB and the function's last instruction, we can have ARC
// release sequences, an epilogue, alignment NOPs, etc. 4 KiB is
// generous; tighter bounds would risk false misattribution on
// functions with long post-call cleanup.
#define PCLN_FN_TAIL_SLACK 4096

// _pcln_lookup_slot binary-searches `arr` (length `n`) for the largest
// slot whose fn_start is <= ip. Returns NULL when no slot covers ip.
static const PclnFnSlot *_pcln_lookup_slot(PclnFnSlot *arr, int n,
                                           uintptr_t ip) {
    if (arr == NULL || n == 0) return NULL;

    int lo = 0;
    int hi = n;
    while (lo < hi) {
        int mid = (int)((unsigned)(lo + hi) >> 1);
        if ((uintptr_t)arr[mid].cache->hdr->fn_start <= ip) {
            lo = mid + 1;
        } else {
            hi = mid;
        }
    }

    if (lo == 0) return NULL;
    return &arr[lo - 1];
}

// _pcln_lookup_pc binary-searches a sorted-by-pc_addr per-fn array for
// the largest entry whose pc_addr <= ip. Returns NULL when ip is
// below the first entry.
static const TinPclnPC *_pcln_lookup_pc(const TinPclnPC *pcs, uint32_t npcs,
                                        uintptr_t ip) {
    if (pcs == NULL || npcs == 0) return NULL;

    uint32_t lo = 0;
    uint32_t hi = npcs;
    while (lo < hi) {
        uint32_t mid = (lo + hi) >> 1;
        if ((uintptr_t)pcs[mid].pc_addr <= ip) {
            lo = mid + 1;
        } else {
            hi = mid;
        }
    }

    if (lo == 0) return NULL;
    return &pcs[lo - 1];
}

int _tin_pclntab_resolve(uintptr_t ip,
                         const char **name, uint32_t *name_len,
                         const char **file, uint32_t *file_len,
                         uint32_t *line, uint32_t *col) {
    if (ip == 0) return 0;

    PclnFnSlot *arr = atomic_load_explicit(&_sorted, memory_order_acquire);
    int         n   = atomic_load_explicit(&_sorted_count, memory_order_acquire);

    if (arr == NULL) {
        pthread_mutex_lock(&_pcln_mu);
        arr = atomic_load_explicit(&_sorted, memory_order_relaxed);
        if (arr == NULL) {
            _pcln_resort();
            arr = atomic_load_explicit(&_sorted, memory_order_relaxed);
            n   = atomic_load_explicit(&_sorted_count, memory_order_relaxed);
        } else {
            n = atomic_load_explicit(&_sorted_count, memory_order_relaxed);
        }
        pthread_mutex_unlock(&_pcln_mu);
    }

    const PclnFnSlot *slot = _pcln_lookup_slot(arr, n, ip);
    if (slot == NULL) return 0;

    const PclnHdrCache *e = slot->cache;

    // Marker-only header (npcs=0): codegen emits these for fns without
    // source context (compiler-generated wrappers, atom helpers, the
    // C-side `main` wrapper). They exist so the binary search lands on
    // the right fn for misattribution defence; on a hit we return 0 so
    // resolve_frame falls through to dladdr.
    if (e->sorted_pcs == NULL || e->npcs == 0) return 0;

    // Misattribution defence. The matched slot has the largest fn_start
    // <= ip, but ip may be past the matched fn's actual end (typical
    // case: ip is in code that's not in pclntab — a CTFE-emitted
    // helper, a non-Tin C trampoline, or the codegen-emitted
    // constructor). Two upper bounds:
    //   - max_pc_addr + slack: covers the trailing ARC release sled
    //     past the last call site within this fn
    //   - next slot's fn_start: tighter bound when fns are densely
    //     packed (typical at -O2 with -ffunction-sections)
    // We take the tighter of the two.
    uintptr_t upper = (uintptr_t)-1;
    if (e->max_pc_addr != NULL) {
        upper = (uintptr_t)e->max_pc_addr + PCLN_FN_TAIL_SLACK;
    }

    int slot_idx = (int)(slot - arr);
    if (slot_idx + 1 < n) {
        uintptr_t next_start = (uintptr_t)arr[slot_idx + 1].cache->hdr->fn_start;
        if (next_start < upper) upper = next_start;
    }

    if (ip >= upper) return 0;

    const TinPclnPC *pc = _pcln_lookup_pc(e->sorted_pcs, e->npcs, ip);
    if (pc == NULL) return 0;

    const TinPclnFnHdr *h = e->hdr;
    if (name)     *name     = h->name;
    if (name_len) *name_len = h->name_len;
    if (file)     *file     = h->file;
    if (file_len) *file_len = h->file_len;
    if (line)     *line     = pc->line;
    if (col)      *col      = pc->col;

    return 1;
}
