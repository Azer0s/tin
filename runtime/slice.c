// tin runtime - dynamic array (slice) operations

#include "runtime.h"
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

// Append one element to a slice (elem_size bytes); returns a new slice.
// The old slice data is NOT freed - the caller manages it via ARC.
// Always allocates a fresh buffer (cap = new_len); the amortized
// in-place path lives in _tin_slice_concat_grow.
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size) {
    int64_t new_len = s.len + 1;
    void *new_ptr = _tin_rc_alloc(new_len * elem_size);
    if (s.ptr) memcpy(new_ptr, s.ptr, (size_t)(s.len * elem_size));
    memcpy((char *)new_ptr + s.len * elem_size, elem, (size_t)elem_size);
    return (TinSlice){ new_ptr, new_len, new_len };
}

// Concatenate two slices. Neither input is freed - the caller manages them via ARC.
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size) {
    int64_t new_len = a.len + b.len;
    void *buf = _tin_rc_alloc((int64_t)(new_len * elem_size));
    if (a.ptr) memcpy(buf,                             a.ptr, (size_t)(a.len * elem_size));
    if (b.ptr) memcpy((char *)buf + a.len * elem_size, b.ptr, (size_t)(b.len * elem_size));
    return (TinSlice){ buf, new_len, new_len };
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

// Build a TinStringArray from main()'s argc/argv.
// Each string is RC-allocated (via _tin_string_from_bytes) so ARC release works.
// The buffer is RC-allocated so _tin_release_fat_elem_array can free it.
TinStringArray _tin_argv_to_slice(int32_t argc, char **argv) {
    TinString *buf = (TinString *)_tin_rc_alloc((int64_t)argc * (int64_t)sizeof(TinString));
    for (int32_t i = 0; i < argc; i++) {
        const char *s = argv[i];
        buf[i] = _tin_string_from_bytes(s, (int64_t)strlen(s));
    }
    return (TinStringArray){ buf, (int64_t)argc, (int64_t)argc };
}

// Return a sub-slice starting at index `start`.
// Allocates a fresh ARC-managed copy (rc=1); caller owns one reference.
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size) {
    if (start >= s.len) return (TinSlice){NULL, 0, 0};
    int64_t new_len = s.len - start;
    size_t data_size = (size_t)(new_len * elem_size);
    void *buf = _tin_rc_alloc((int64_t)data_size);
    if (data_size > 0) memcpy(buf, (char *)s.ptr + start * elem_size, data_size);
    return (TinSlice){buf, new_len, new_len};
}

// Convert a slice of integer elements from src_sz bytes each to tgt_sz bytes
// each. Allocates a fresh ARC-managed buffer and truncates / sign-extends
// element-wise. src_signed!=0 selects sign extension on widening; zero
// extension otherwise. Only widths in {1,2,4,8} are supported.
// Used by the codegen when a fat-array `{T1*, i64}` is passed where a
// differently-sized `{T2*, i64}` is expected (e.g. `[1,2,3,4]` as `[i64]` -
// default for int literals - into a function parameter typed `[i32]`).
TinSlice _tin_slice_convert_int(TinSlice s, int64_t src_sz, int64_t tgt_sz, int32_t src_signed) {
    if (s.len == 0) return (TinSlice){NULL, 0, 0};
    int64_t total = s.len * tgt_sz;
    void *buf = _tin_rc_alloc(total);
    const char *src = (const char *)s.ptr;
    char *dst = (char *)buf;
    for (int64_t i = 0; i < s.len; i++) {
        int64_t v = 0;
        const char *sp = src + i * src_sz;
        switch (src_sz) {
            case 1: v = src_signed ? (int64_t)(*(const int8_t  *)sp) : (int64_t)(*(const uint8_t  *)sp); break;
            case 2: v = src_signed ? (int64_t)(*(const int16_t *)sp) : (int64_t)(*(const uint16_t *)sp); break;
            case 4: v = src_signed ? (int64_t)(*(const int32_t *)sp) : (int64_t)(*(const uint32_t *)sp); break;
            case 8: v =                          *(const int64_t *)sp;                                  break;
            default: v = 0; break;
        }
        char *dp = dst + i * tgt_sz;
        switch (tgt_sz) {
            case 1: *(int8_t  *)dp = (int8_t )v; break;
            case 2: *(int16_t *)dp = (int16_t)v; break;
            case 4: *(int32_t *)dp = (int32_t)v; break;
            case 8: *(int64_t *)dp =          v; break;
            default: break;
        }
    }
    return (TinSlice){buf, s.len, s.len};
}
