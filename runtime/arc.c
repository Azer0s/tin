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

// Increment reference count; skips immortal (rc == -1) and NULL pointers
void _tin_retain(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (hdr->rc == TIN_IMMORTAL_RC) return;
    hdr->rc++;
}

// Decrement reference count; frees the block when it reaches zero
void _tin_release(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (hdr->rc == TIN_IMMORTAL_RC) return;
    if (--hdr->rc == 0) free(hdr);
}
