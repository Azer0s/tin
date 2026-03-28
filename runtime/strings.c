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
