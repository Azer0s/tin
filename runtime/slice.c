// tin runtime - dynamic array (slice) operations

#include "runtime.h"
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

// Append one element to a slice (elem_size bytes); returns a new slice.
// The old slice data is NOT freed - the caller manages it via ARC.
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size) {
    int64_t new_len = s.len + 1;
    void *new_ptr = _tin_rc_alloc(new_len * elem_size);
    if (s.ptr) memcpy(new_ptr, s.ptr, (size_t)(s.len * elem_size));
    memcpy((char *)new_ptr + s.len * elem_size, elem, (size_t)elem_size);
    return (TinSlice){ new_ptr, new_len };
}

// Concatenate two slices. Neither input is freed - the caller manages them via ARC.
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size) {
    int64_t new_len = a.len + b.len;
    void *buf = _tin_rc_alloc((int64_t)(new_len * elem_size));
    if (a.ptr) memcpy(buf,                             a.ptr, (size_t)(a.len * elem_size));
    if (b.ptr) memcpy((char *)buf + a.len * elem_size, b.ptr, (size_t)(b.len * elem_size));
    return (TinSlice){ buf, new_len };
}

// Return the length of a slice
int64_t _tin_slice_len(TinSlice s) { return s.len; }

// Bounds-checked index
void *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size) {
    if (i < 0 || i >= s.len) {
        fprintf(stderr, "tin: index %lld out of bounds (len=%lld)\n",
                (long long)i, (long long)s.len);
        exit(1);
    }
    return (char *)s.ptr + i * elem_size;
}

// Return a sub-slice starting at index `start`.
// Allocates a fresh ARC-managed copy (rc=1); caller owns one reference.
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size) {
    if (start >= s.len) return (TinSlice){NULL, 0};
    int64_t new_len = s.len - start;
    size_t data_size = (size_t)(new_len * elem_size);
    void *buf = _tin_rc_alloc((int64_t)data_size);
    if (data_size > 0) memcpy(buf, (char *)s.ptr + start * elem_size, data_size);
    return (TinSlice){buf, new_len};
}
