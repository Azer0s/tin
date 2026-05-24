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
    return (TinString){ buf, len, len };
}

// Wrap a C string literal in a TinString (no copy).  cap = -1 marks
// it as a borrowed view -- the literal lives in .rodata and must not
// be released or appended in-place.
TinString _tin_str_from_cstr(const char *s) {
    int64_t len = (int64_t)strlen(s);
    return (TinString){ s, len, -1 };
}

// NULL-safe strlen used by the codegen extern-return wrap path.  C string-
// returning APIs (getenv, ttyname, readline, ...) often signal "absent" with
// NULL; calling bare strlen on NULL is UB and segfaults on glibc / Darwin
// libSystem.  Returns 0 for NULL so the wrap site can build an empty fat
// string {NULL, 0} without crashing.
int64_t _tin_extern_cstr_len(const char *s) {
    return s == NULL ? 0 : (int64_t)strlen(s);
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
    if (!buf) return (TinString){NULL, 0, 0};
    if (ptr && len > 0) memcpy(buf, ptr, (size_t)len);
    buf[len] = '\0';
    return (TinString){buf, len, len};
}

// Return the raw char pointer from a TinString.
//
// Tin's user-extern lowering passes `string` as `i8*` (the data pointer),
// not the full TinString struct -- see codegen/extern.go:tinTypeToExternLLVM.
// So the C parameter must be `const char*`, not `TinString`, otherwise the
// 24-byte struct ABI mismatches what the caller put in registers and we
// read garbage off the stack.
const char *_tin_string_data(const char *ptr) {
    return ptr;
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
    return (TinSlice){buf, len, len};
}
