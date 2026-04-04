// tin runtime - string operations

#include "runtime.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Concatenate two strings; returns an ARC-managed TinString (rc=1).
// The caller owns one reference; use _tin_release(result.ptr) to free.
TinString _tin_str_concat(TinString a, TinString b) {
    int64_t len = a.len + b.len;
    char *buf = (char *)_tin_rc_alloc(len + 1);
    memcpy(buf, a.ptr, (size_t)a.len);
    memcpy(buf + a.len, b.ptr, (size_t)b.len);
    buf[len] = '\0';
    return (TinString){ buf, len };
}

// Wrap a C string literal in a TinString (no copy)
TinString _tin_str_from_cstr(const char *s) {
    return (TinString){ s, (int64_t)strlen(s) };
}

// Compare two strings
int32_t _tin_str_eq(TinString a, TinString b) {
    if (a.len != b.len) return 0;
    return memcmp(a.ptr, b.ptr, (size_t)a.len) == 0;
}

// Construct a TinString from a byte buffer (copies the bytes).
// Uses _tin_rc_alloc so the returned string is ARC-managed (like _tin_str_concat).
TinString _tin_string_from_bytes(const char *ptr, int64_t len) {
    char *buf = (char *)_tin_rc_alloc(len + 1);
    if (!buf) return (TinString){NULL, 0};
    if (ptr && len > 0) memcpy(buf, ptr, (size_t)len);
    buf[len] = '\0';
    return (TinString){buf, len};
}

// Return the raw char pointer from a TinString.
const char *_tin_string_data(TinString s) {
    return s.ptr;
}

// Heap buffer helpers for use in coroutines (stack allocas may not survive coro.split).
char *_tin_buf_alloc(int64_t n)            { return (char *)malloc((size_t)n); }
char *_tin_buf_realloc(char *p, int64_t n) { return (char *)realloc(p, (size_t)n); }
void  _tin_buf_free(char *p)               { free(p); }

// Create an ARC-managed TinSlice (byte array) from a raw buffer (copies the bytes).
// The returned slice's backing memory is managed by ARC: it is released when the
// last reference to the slice goes out of scope.  Pass len=0 and ptr=NULL for
// an empty slice.
TinSlice _tin_bytes_from_buf(const char *ptr, int64_t len) {
    char *buf = (char *)_tin_rc_alloc(len + 1);
    if (ptr && len > 0) memcpy(buf, ptr, (size_t)len);
    buf[len] = '\0';
    return (TinSlice){buf, len};
}
