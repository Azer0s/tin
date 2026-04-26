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

// ---------------------------------------------------------------------------
// Page / slot bookkeeping.
// ---------------------------------------------------------------------------

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
    uint64_t _align;              // pad to a multiple of TRAMP_SLOT_SIZE
    uint8_t  body[];              // TRAMP_SLOTS slots, each TRAMP_SLOT_SIZE bytes
} TrampPage;

// Number of slots that fit after the (now slot-aligned) header.
#define TRAMP_SLOTS \
    ((TRAMP_PAGE_SIZE - offsetof(TrampPage, body)) / TRAMP_SLOT_SIZE)

#define TRAMP_PAGE_MAGIC 0x54436250u // "TCbp" - tin callback page

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

// ---------------------------------------------------------------------------
// Per-arch trampoline body emitter.
// ---------------------------------------------------------------------------
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
    // ARM64: load closure ptr into x16 (IP0, scratch), then dispatcher
    // address into x17, then BR x17.
    //
    // We use literal pools placed after the branch:
    //   ldr  x16, =closure_data_ptr   ; pc-relative load
    //   ldr  x17, =dispatcher
    //   br   x17
    //   .quad closure_data_ptr        ; literal #1
    //   .quad dispatcher              ; literal #2
    //
    // ldr x16, [pc, #8]:    58 00 00 50  (encoded little-endian)
    // ldr x17, [pc, #12]:   58 00 00 58
    // br  x17:              80 02 1f d6
    // -> 12 bytes of code, then 16 bytes of literals = 28 total.

    uint32_t inst[3];
    // ldr x16, [pc, #8]   -> opcode 58000050
    inst[0] = 0x58000050;
    // ldr x17, [pc, #12]  -> opcode 580000d1 (actually 0x58000071)
    // Encoding: 01 011 0 00 imm19 Rt; for ldr (literal) 64-bit:
    // opcode = 0x58000000 | (imm19 << 5) | Rt
    // imm19 = byte_offset / 4
    inst[0] = 0x58000000 | ((8 / 4) << 5) | 16; // ldr x16, [pc, #8]
    inst[1] = 0x58000000 | ((12 / 4) << 5) | 17; // ldr x17, [pc, #12]
    inst[2] = 0xd61f0220; // br x17 (encoded: D6 1F 02 20)

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

// ---------------------------------------------------------------------------
// Page allocator.
// ---------------------------------------------------------------------------

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
// detectors stay quiet. Called via atexit() the first time we
// allocate a page.
static void atexit_release_all_pages(void) {
    pthread_mutex_lock(&_tramp_mu);

    for (size_t i = 0; i < _tramp_all_pages_len; i++) {
        munmap(_tramp_all_pages[i], TRAMP_PAGE_SIZE);
    }

    free(_tramp_all_pages);
    _tramp_all_pages = NULL;
    _tramp_all_pages_len = 0;
    _tramp_all_pages_cap = 0;
    _tramp_freelist = NULL;

    pthread_mutex_unlock(&_tramp_mu);
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

    for (int32_t i = 0; i < TRAMP_SLOTS; i++) {
        slot_set_free_next(p, i, (i + 1 < TRAMP_SLOTS) ? (i + 1) : -1);
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
            return NULL;
        }

        _tramp_freelist = p;
    }

    TrampPage *p = _tramp_freelist;
    int32_t idx = p->free_head;
    p->free_head = slot_free_next(p, idx);
    p->free_count--;

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

void tin_interop_closure_free(void *tramp) {
    if (!tramp) return;

    // tramp points to the code area, which is at offset
    // TRAMP_DATA_BYTES inside the slot.
    uint8_t *slot = (uint8_t *)tramp - TRAMP_DATA_BYTES;
    TrampPage *p = page_of_slot(slot);
    if (p->magic != TRAMP_PAGE_MAGIC) {
        // Not one of our trampolines - silently ignore so the C
        // caller can route everything through this one entry point.
        return;
    }

    int32_t idx = (int32_t)((slot - p->body) / TRAMP_SLOT_SIZE);

    pthread_mutex_lock(&_tramp_mu);

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
}
