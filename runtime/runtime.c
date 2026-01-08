/*
 * tin runtime library
 *
 * Provides helper functions that the LLVM IR emitted by the tin compiler
 * can call for operations that are easier to implement in C than in raw IR:
 *   - typed echo helpers
 *   - string/array operations
 *   - memory helpers
 *
 * All symbols are prefixed with _tin_ to avoid collisions.
 */

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdbool.h>

/* ── Typed echo helpers ──────────────────────────────────────────────────── */

void _tin_echo_i64(int64_t v)  { printf("%lld\n",  (long long)v); }
void _tin_echo_u64(uint64_t v) { printf("%llu\n",  (unsigned long long)v); }
void _tin_echo_f64(double v)   { printf("%g\n",    v); }
void _tin_echo_bool(int32_t v) { puts(v ? "true" : "false"); }
void _tin_echo_char(uint8_t v) { printf("%c\n",    v); }

/* string representation: { i8* ptr, i64 len } */
typedef struct { const char *ptr; int64_t len; } TinString;

void _tin_echo_string(TinString s) {
    printf("%.*s\n", (int)s.len, s.ptr);
}

/* Print without trailing newline (used for interpolation pieces) */
void _tin_print_i64(int64_t v)  { printf("%lld",  (long long)v); }
void _tin_print_u64(uint64_t v) { printf("%llu",  (unsigned long long)v); }
void _tin_print_f64(double v)   { printf("%g",    v); }
void _tin_print_bool(int32_t v) { fputs(v ? "true" : "false", stdout); }
void _tin_print_char(uint8_t v) { putchar(v); }
void _tin_print_string(TinString s) { printf("%.*s", (int)s.len, s.ptr); }
void _tin_print_newline(void)   { putchar('\n'); }

/* ── String operations ───────────────────────────────────────────────────── */

/* Concatenate two strings; returns a heap-allocated TinString */
TinString _tin_str_concat(TinString a, TinString b) {
    int64_t len = a.len + b.len;
    char *buf = (char *)malloc((size_t)(len + 1));
    if (!buf) { fputs("tin: out of memory\n", stderr); exit(1); }
    memcpy(buf, a.ptr, (size_t)a.len);
    memcpy(buf + a.len, b.ptr, (size_t)b.len);
    buf[len] = '\0';
    return (TinString){ buf, len };
}

/* Wrap a C string literal in a TinString (no copy) */
TinString _tin_str_from_cstr(const char *s) {
    return (TinString){ s, (int64_t)strlen(s) };
}

/* Compare two strings */
int32_t _tin_str_eq(TinString a, TinString b) {
    if (a.len != b.len) return 0;
    return memcmp(a.ptr, b.ptr, (size_t)a.len) == 0;
}

/* ── Dynamic array (slice) operations ────────────────────────────────────── */

/* Slice representation: { T* ptr, i64 len } – same layout as TinString */
typedef struct { void *ptr; int64_t len; } TinSlice;

/* Append one element (elem_size bytes) to a slice; reallocates */
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size) {
    int64_t new_len = s.len + 1;
    void *new_ptr = malloc((size_t)(new_len * elem_size));
    if (!new_ptr) { fputs("tin: out of memory\n", stderr); exit(1); }
    if (s.ptr) {
        memcpy(new_ptr, s.ptr, (size_t)(s.len * elem_size));
        free(s.ptr);
    }
    memcpy((char *)new_ptr + s.len * elem_size, elem, (size_t)elem_size);
    return (TinSlice){ new_ptr, new_len };
}

/* Concatenate two slices */
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size) {
    int64_t new_len = a.len + b.len;
    void *buf = malloc((size_t)(new_len * elem_size));
    if (!buf) { fputs("tin: out of memory\n", stderr); exit(1); }
    if (a.ptr) memcpy(buf,                             a.ptr, (size_t)(a.len * elem_size));
    if (b.ptr) memcpy((char *)buf + a.len * elem_size, b.ptr, (size_t)(b.len * elem_size));
    if (a.ptr) free(a.ptr);
    if (b.ptr) free(b.ptr);
    return (TinSlice){ buf, new_len };
}

/* Return the length of a slice */
int64_t _tin_slice_len(TinSlice s) { return s.len; }

/* Bounds-checked index */
void *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size) {
    if (i < 0 || i >= s.len) {
        fprintf(stderr, "tin: index %lld out of bounds (len=%lld)\n",
                (long long)i, (long long)s.len);
        exit(1);
    }
    return (char *)s.ptr + i * elem_size;
}

/* ── Memory helpers ──────────────────────────────────────────────────────── */

void *_tin_malloc(int64_t size) {
    void *p = malloc((size_t)size);
    if (!p) { fputs("tin: out of memory\n", stderr); exit(1); }
    return p;
}

void _tin_free(void *p) { free(p); }

/* ── Panic / assert ──────────────────────────────────────────────────────── */

void _tin_panic(const char *msg) {
    fprintf(stderr, "tin panic: %s\n", msg);
    exit(1);
}

void _tin_assert(int32_t cond, const char *msg) {
    if (!cond) _tin_panic(msg);
}

/* ── Length builtin ──────────────────────────────────────────────────────── */

int64_t _tin_len_string(TinString s) { return s.len; }
int64_t _tin_len_slice(TinSlice s)   { return s.len; }
