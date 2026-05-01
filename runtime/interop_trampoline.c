// tin runtime - per-instance trampoline allocator for closure returns.
//
// When a `#interop` function returns `fn(args) ret`, the wrapper hands
// the C caller a stable function pointer. Behind that pointer is a
// small (~22 byte) hand-written stub mmap'd into an executable page;
// the stub loads the address of its closure-data block into a
// scratch register (%r10 on x86_64, x16 on ARM64) and tail-jumps to a
// per-signature dispatcher emitted at codegen time. The dispatcher's
// first IR statement reads that scratch register via inline asm,
// recovering the closure data without ever passing it as a regular
// arg - so the user's args stay in their natural positions, and the
// LLVM-emitted dispatcher does the (env, args...) shift via a normal
// indirect call.
//
// Design constraints:
//   * No JIT compiler - we only memcpy short, fixed templates.
//   * No libffi - one external dep we're avoiding.
//   * No restrictions on the number of concurrent live closures.
//   * Per-instance trampoline = unique C function pointer = OK to
//     hand off to any C API that wants a callback.
//
// Page / slot layout (4 KB page):
//
//   [TrampPage header (32 B)] [slot 0 (32 B)] [slot 1] ... [slot 126]
//
// Each slot holds one TinClosureData (16 B = fn + env) followed by
// the trampoline machine code (up to 16 B). The slot's address IS
// the function pointer handed to the C caller; the leading 16 bytes
// are read by the dispatcher (via the scratch-register pointer) to
// recover (fn, env).

#include <stdatomic.h>
#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <pthread.h>

// Forward decl from arc.c. Closure envs created by Tin are ARC blocks
// with a destructor at offset 0; _tin_release_closure invokes the
// destructor when rc reaches 0 to release captured RC values, then
// frees the block. NULL-safe.
extern void _tin_release_closure(void *env);

// Page / slot bookkeeping.

#define TRAMP_PAGE_SIZE  4096
#define TRAMP_SLOT_SIZE  64

// Slot bytes: 16 for closure data (read by dispatcher), 48 for code
// (more than enough for x86_64's 22-byte body or ARM64's 28-byte body
// plus literal pool).
#define TRAMP_DATA_BYTES 16
#define TRAMP_CODE_BYTES 48

typedef struct TrampPage {
    struct TrampPage *next;       // free-list chain (pages with >= 1 free slot)
    int32_t free_head;            // index of first free slot, or -1
    int32_t free_count;           // count of free slots in this page
    uint32_t magic;               // identifies our pages on tin_release_callback
    uint64_t in_use;              // bit i set when slot i is allocated; used
                                  // to detect double-free without scanning
                                  // the free-list. TRAMP_SLOTS <= 63 today,
                                  // so a single uint64_t suffices.
    uint8_t  body[];              // TRAMP_SLOTS slots, each TRAMP_SLOT_SIZE bytes
} TrampPage;

// Number of slots that fit after the (now slot-aligned) header.
#define TRAMP_SLOTS \
    ((TRAMP_PAGE_SIZE - offsetof(TrampPage, body)) / TRAMP_SLOT_SIZE)

#define TRAMP_PAGE_MAGIC 0x54436250u // "TCbp" - tin callback page

// The double-free guard uses one bit per slot in `in_use`; if a future
// page-size bump or slot-size shrink lifts TRAMP_SLOTS past 64, the
// bitmap silently truncates and slots beyond 63 lose double-free
// protection. Catch that at compile time.
_Static_assert(TRAMP_SLOTS <= 64,
               "in_use bitmap is uint64_t; TRAMP_SLOTS must fit in 64 bits");

static inline uint8_t *slot_addr_in_page(TrampPage *p, int32_t idx) {
    return p->body + idx * TRAMP_SLOT_SIZE;
}

// When a slot is FREE its first 4 bytes encode the next free index
// (-1 = end of free list within this page).
static inline int32_t slot_free_next(TrampPage *p, int32_t idx) {
    int32_t v;
    memcpy(&v, slot_addr_in_page(p, idx), sizeof v);
    return v;
}

static inline void slot_set_free_next(TrampPage *p, int32_t idx, int32_t next) {
    memcpy(slot_addr_in_page(p, idx), &next, sizeof next);
}

// Recover the TrampPage* a given slot belongs to. The slot is
// guaranteed to lie inside the page's body region; subtract the
// offset to land back on the header.
static inline TrampPage *page_of_slot(uint8_t *slot) {
    uintptr_t addr = (uintptr_t)slot;
    uintptr_t base = addr & ~(uintptr_t)(TRAMP_PAGE_SIZE - 1);
    return (TrampPage *)base;
}

// Per-arch trampoline body emitter.
//
// Writes the machine bytes for a trampoline that:
//   1. Loads `closure_data_ptr` into the scratch register the
//      per-signature dispatcher will read.
//   2. Tail-jumps to `dispatcher`.
//
// Returns the number of bytes written. The dispatcher contract
// (which scratch reg to use) is documented per-arch in the codegen
// counterpart.

static size_t emit_trampoline_x86_64(uint8_t *code,
                                     void *closure_data_ptr,
                                     void *dispatcher) {
    // movabs $imm64, %r10:        49 BA <8-byte little-endian imm>
    code[0] = 0x49;
    code[1] = 0xBA;
    memcpy(code + 2, &closure_data_ptr, 8);

    // movabs $imm64, %rax:        48 B8 <8-byte little-endian imm>
    code[10] = 0x48;
    code[11] = 0xB8;
    memcpy(code + 12, &dispatcher, 8);

    // jmp *%rax:                  FF E0
    code[20] = 0xFF;
    code[21] = 0xE0;

    return 22;
}

static size_t emit_trampoline_aarch64(uint8_t *code,
                                      void *closure_data_ptr,
                                      void *dispatcher) {
    // ARM64 layout (28 bytes total):
    //   code+0  ldr x16, [pc, #12]   ; load closure ptr from literal #1
    //   code+4  ldr x17, [pc, #12]   ; load dispatcher from literal #2
    //   code+8  br  x17              ; tail-jump to dispatcher
    //   code+12 .quad closure_data_ptr   (literal #1)
    //   code+20 .quad dispatcher         (literal #2)
    //
    // ldr (literal) encoding for 64-bit reg:
    //   opcode = 0x58000000 | (imm19 << 5) | Rt
    //   imm19  = (target_addr - pc_of_ldr) / 4
    //
    // For ldr x16 at code+0 reading from code+12 -> imm19 = 12/4 = 3.
    // For ldr x17 at code+4 reading from code+20 -> imm19 = 16/4 = 4.
    //
    // x16/x17 are AAPCS64 IP0/IP1 ("intra-procedure-call scratch"),
    // never used to pass arguments, so they survive the BR into the
    // dispatcher. The dispatcher reads x16 via inline asm to recover
    // the closure-data pointer.

    uint32_t inst[3];
    inst[0] = 0x58000000 | ((uint32_t)3 << 5) | 16; // ldr x16, [pc, #12]
    inst[1] = 0x58000000 | ((uint32_t)4 << 5) | 17; // ldr x17, [pc, #16]
    inst[2] = 0xd61f0220;                            // br x17

    memcpy(code + 0,  &inst[0], 4);
    memcpy(code + 4,  &inst[1], 4);
    memcpy(code + 8,  &inst[2], 4);
    memcpy(code + 12, &closure_data_ptr, 8);
    memcpy(code + 20, &dispatcher, 8);

    return 28;
}

static size_t emit_trampoline(uint8_t *code, void *closure_data_ptr, void *dispatcher) {
#if defined(__x86_64__)
    return emit_trampoline_x86_64(code, closure_data_ptr, dispatcher);
#elif defined(__aarch64__)
    return emit_trampoline_aarch64(code, closure_data_ptr, dispatcher);
#else
    (void)code; (void)closure_data_ptr; (void)dispatcher;
    fputs("tin: trampoline emitter does not support this architecture\n", stderr);
    exit(1);
#endif
}

// Page allocator.

static pthread_mutex_t _tramp_mu = PTHREAD_MUTEX_INITIALIZER;
static TrampPage *_tramp_freelist;
// Tracks every page we have ever allocated so the atexit handler can
// munmap them. Pages move on/off _tramp_freelist as slots fill and
// drain; this list is append-only and never shrinks.
static TrampPage **_tramp_all_pages;
static size_t      _tramp_all_pages_len;
static size_t      _tramp_all_pages_cap;
static int         _tramp_atexit_installed;

// At process exit munmap every page we ever allocated so leak
// detectors stay quiet. Also releases the captured env of any
// still-live trampoline the C caller forgot to free, so leaked
// closures get their dtors run and any RC-tracked captures hit zero.
// Called via atexit() the first time we allocate a page.
static void atexit_release_all_pages(void) {
    // Snapshot under the lock, then release env / munmap outside the
    // lock: env dtors are user-defined and may take other locks or
    // even allocate (which would deadlock if we held _tramp_mu).
    pthread_mutex_lock(&_tramp_mu);

    TrampPage **pages = _tramp_all_pages;
    size_t      n     = _tramp_all_pages_len;

    _tramp_all_pages = NULL;
    _tramp_all_pages_len = 0;
    _tramp_all_pages_cap = 0;
    _tramp_freelist = NULL;

    pthread_mutex_unlock(&_tramp_mu);

    for (size_t i = 0; i < n; i++) {
        TrampPage *p = pages[i];

        for (uint32_t idx = 0; idx < TRAMP_SLOTS; idx++) {
            if ((p->in_use & (((uint64_t)1) << idx)) == 0) continue;

            void **data = (void **)slot_addr_in_page(p, (int32_t)idx);
            void  *env  = data[1];
            // Scrub before release in case the dtor somehow re-enters
            // and re-walks this slot.
            data[0] = NULL;
            data[1] = NULL;
            _tin_release_closure(env);
        }

        munmap(p, TRAMP_PAGE_SIZE);
    }

    free(pages);
}

static TrampPage *new_page(void) {
    void *raw = mmap(NULL, TRAMP_PAGE_SIZE,
                     PROT_READ | PROT_WRITE | PROT_EXEC,
                     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (raw == MAP_FAILED) {
        return NULL;
    }

    TrampPage *p = (TrampPage *)raw;
    p->next = NULL;
    p->free_head = 0;
    p->free_count = TRAMP_SLOTS;
    p->magic = TRAMP_PAGE_MAGIC;
    p->in_use = 0;

    for (uint32_t i = 0; i < TRAMP_SLOTS; i++) {
        slot_set_free_next(p, (int32_t)i,
                           ((i + 1) < TRAMP_SLOTS) ? (int32_t)(i + 1) : -1);
    }

    // Append to the all-pages registry for atexit cleanup.
    if (_tramp_all_pages_len == _tramp_all_pages_cap) {
        size_t new_cap = _tramp_all_pages_cap ? _tramp_all_pages_cap * 2 : 16;
        TrampPage **new_arr = (TrampPage **)realloc(_tramp_all_pages,
                                                    new_cap * sizeof(TrampPage *));
        if (!new_arr) {
            munmap(raw, TRAMP_PAGE_SIZE);
            return NULL;
        }

        _tramp_all_pages = new_arr;
        _tramp_all_pages_cap = new_cap;
    }

    _tramp_all_pages[_tramp_all_pages_len++] = p;

    if (!_tramp_atexit_installed) {
        atexit(atexit_release_all_pages);
        _tramp_atexit_installed = 1;
    }

    return p;
}

// Flush the icache for the slot we just wrote. On x86_64 the L1
// pipeline observes data writes coherently; on ARM64 we MUST issue
// a flush or the CPU may execute stale instructions.
static inline void icache_flush(void *start, size_t len) {
#if defined(__aarch64__)
    __builtin___clear_cache((char *)start, (char *)start + len);
#else
    (void)start; (void)len;
#endif
}

void *tin_make_trampoline(void *fn, void *env, void *dispatcher) {
    pthread_mutex_lock(&_tramp_mu);

    if (_tramp_freelist == NULL) {
        TrampPage *p = new_page();
        if (!p) {
            pthread_mutex_unlock(&_tramp_mu);
            // Drop the ref the caller transferred to us. Without this
            // the env block leaks for every OOM. NULL-safe.
            _tin_release_closure(env);
            return NULL;
        }

        _tramp_freelist = p;
    }

    TrampPage *p = _tramp_freelist;
    int32_t idx = p->free_head;
    p->free_head = slot_free_next(p, idx);
    p->free_count--;
    p->in_use |= ((uint64_t)1) << idx;

    if (p->free_head == -1) {
        // Page is now full; remove from free-list.
        _tramp_freelist = p->next;
        p->next = NULL;
    }

    pthread_mutex_unlock(&_tramp_mu);

    uint8_t *slot = slot_addr_in_page(p, idx);

    // Lay out the slot:
    //   slot+0..15   : TinClosureData = { fn, env }
    //   slot+16..31  : machine code
    void **data = (void **)slot;
    data[0] = fn;
    data[1] = env;

    void *closure_data_ptr = slot;     // dispatcher reads from here
    uint8_t *code = slot + TRAMP_DATA_BYTES;
    size_t n = emit_trampoline(code, closure_data_ptr, dispatcher);

    icache_flush(code, n);

    // The C caller's function pointer is the address of the CODE,
    // not of the data block.
    return code;
}

// is_known_page reports whether p is the base of a page we allocated.
// Linear scan over _tramp_all_pages: free is not on a hot path, the
// list is short (one entry per 4KB of trampolines, and even a heavy
// app rarely tops a few MB of trampolines), and skipping the scan
// would require dereferencing `p->magic` from an arbitrary
// page-aligned address - i.e. could SEGV on a guard page.
//
// Caller must hold _tramp_mu (the registry is mutated under the lock).
static int is_known_page(TrampPage *p) {
    for (size_t i = 0; i < _tramp_all_pages_len; i++) {
        if (_tramp_all_pages[i] == p) return 1;
    }

    return 0;
}

void tin_interop_closure_free(void *tramp) {
    if (!tramp) return;

    // tramp must point INTO the body region of a known page, at a
    // slot-aligned offset (TRAMP_DATA_BYTES into a slot). Anything
    // else - libc fn pointer, stack address, an interior byte of a
    // valid slot, a NULL+small-offset - is silently ignored so a
    // generic C-side cleanup routine can route everything through
    // this one entry point without first proving the pointer is ours.
    uint8_t *slot = (uint8_t *)tramp - TRAMP_DATA_BYTES;
    TrampPage *p = page_of_slot(slot);

    pthread_mutex_lock(&_tramp_mu);

    if (!is_known_page(p)) {
        // Not one of our pages. Bail without dereferencing p (which
        // could be an unmapped/guard page).
        pthread_mutex_unlock(&_tramp_mu);
        return;
    }

    if (p->magic != TRAMP_PAGE_MAGIC) {
        // Belt-and-braces: registered page should always have the
        // magic; if not, something has badly corrupted the page.
        pthread_mutex_unlock(&_tramp_mu);
        return;
    }

    // The slot must lie inside the body region at a SLOT_SIZE-aligned
    // offset, i.e. tramp = page + offsetof(body) + idx*SLOT + DATA_BYTES.
    if (slot < p->body) {
        pthread_mutex_unlock(&_tramp_mu);
        return;
    }

    size_t off = (size_t)(slot - p->body);
    if ((off % TRAMP_SLOT_SIZE) != 0 || (off / TRAMP_SLOT_SIZE) >= TRAMP_SLOTS) {
        pthread_mutex_unlock(&_tramp_mu);
        return;
    }

    int32_t idx = (int32_t)(off / TRAMP_SLOT_SIZE);
    uint64_t bit = ((uint64_t)1) << idx;

    if ((p->in_use & bit) == 0) {
        // Double-free or release of a never-allocated slot. Silently
        // ignore: pushing the slot onto the free-list a second time
        // would corrupt the list (next two allocs would return the
        // same address) and double-release env would corrupt ARC.
        pthread_mutex_unlock(&_tramp_mu);
        return;
    }

    p->in_use &= ~bit;

    // Snapshot env BEFORE handing the slot back to the free-list, but
    // AFTER confirming this is a first free. Released outside the
    // mutex: the closure dtor may run user-defined Tin deinit code
    // which can allocate, take other locks, or transitively allocate
    // another trampoline.
    void **data = (void **)slot;
    void *env = data[1];
    // Scrub so a use-after-free of the trampoline pointer crashes
    // loudly instead of jumping through stale bytes.
    data[0] = NULL;
    data[1] = NULL;

    if (p->free_count == 0) {
        // Page was full and not on the free-list; reinsert.
        p->next = _tramp_freelist;
        _tramp_freelist = p;
    }

    slot_set_free_next(p, idx, p->free_head);
    p->free_head = idx;
    p->free_count++;
    // Eager per-page reclamation is intentionally NOT done here. We
    // keep emptied pages on _tramp_freelist for reuse and rely on the
    // atexit handler (atexit_release_all_pages) to munmap everything
    // at process exit. This avoids the bookkeeping cost of removing
    // a page from _tramp_all_pages and keeps the per-call free path
    // lock-and-mutate only.

    pthread_mutex_unlock(&_tramp_mu);

    // Release the captured env after dropping the lock.
    _tin_release_closure(env);
}
