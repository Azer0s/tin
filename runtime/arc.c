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
