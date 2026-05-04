// tin runtime - typed echo and print helpers

#include "runtime.h"
#include <stdio.h>
#include <string.h>

void _tin_echo_i64(int64_t v)  { printf("%lld\n",  (long long)v);  fflush(stdout); }
void _tin_echo_u64(uint64_t v) { printf("%llu\n",  (unsigned long long)v); fflush(stdout); }
void _tin_echo_f64(double v)   { printf("%g\n",    v);             fflush(stdout); }
void _tin_echo_bool(int32_t v) { puts(v ? "true" : "false");       fflush(stdout); }
void _tin_echo_char(uint8_t v) { printf("%c\n",    v);             fflush(stdout); }

void _tin_echo_string(TinString s) {
    printf("%.*s\n", (int)s.len, s.ptr);
    fflush(stdout);
}

// _tin_echo_string_escaped / _tin_print_string_escaped: print a string escaping
// any byte >= 128 as \xNN so non-ASCII content is always human-readable.
// _tin_echo_string_escaped appends a trailing newline; _tin_print_string_escaped
// does not (used inside struct/array formatting).
void _tin_echo_string_escaped(const char *ptr, int64_t len) {
    for (int64_t i = 0; i < len; i++) {
        unsigned char c = (unsigned char)ptr[i];
        if (c >= 128) {
            printf("\\x%02x", c);
        } else {
            putchar(c);
        }
    }
    putchar('\n');
    fflush(stdout);
}

void _tin_print_string_escaped(const char *ptr, int64_t len) {
    for (int64_t i = 0; i < len; i++) {
        unsigned char c = (unsigned char)ptr[i];
        if (c >= 128) {
            printf("\\x%02x", c);
        } else {
            putchar(c);
        }
    }
}

// Print without trailing newline (used for string interpolation pieces)
void _tin_print_i64(int64_t v)  { printf("%lld",  (long long)v); }
void _tin_print_u64(uint64_t v) { printf("%llu",  (unsigned long long)v); }
void _tin_print_f64(double v)   { printf("%g",    v); }
void _tin_print_bool(int32_t v) { fputs(v ? "true" : "false", stdout); }
void _tin_print_char(uint8_t v) { putchar(v); }
void _tin_print_string(TinString s) { printf("%.*s", (int)s.len, s.ptr); }
void _tin_print_newline(void)   { putchar('\n'); }

// 128-bit integer helpers
//
// GCC and Clang both support __int128 / unsigned __int128 on x86-64.
// There is no standard printf format for these types, so we convert to
// decimal manually using a small stack buffer (at most 40 digits).

static void _i128_to_dec(__int128 v, char *out, int *len) {
    char buf[50];
    int i = 49;
    buf[i] = '\0';
    int neg = v < (__int128)0;
    // Handle INT128_MIN: negate via two's complement without overflow.
    unsigned __int128 u = neg
        ? (unsigned __int128)(-(v + 1)) + 1
        : (unsigned __int128)v;
    do { buf[--i] = '0' + (int)(u % 10); u /= 10; } while (u > 0);
    if (neg) buf[--i] = '-';
    *len = 49 - i;
    memcpy(out, buf + i, (size_t)(*len + 1)); // include NUL
}

static void _u128_to_dec(unsigned __int128 v, char *out, int *len) {
    char buf[50];
    int i = 49;
    buf[i] = '\0';
    do { buf[--i] = '0' + (int)(v % 10); v /= 10; } while (v > 0);
    *len = 49 - i;
    memcpy(out, buf + i, (size_t)(*len + 1));
}

void _tin_echo_i128(__int128 v) {
    char buf[50]; int n;
    _i128_to_dec(v, buf, &n);
    puts(buf);
    fflush(stdout);
}

void _tin_echo_u128(unsigned __int128 v) {
    char buf[50]; int n;
    _u128_to_dec(v, buf, &n);
    puts(buf);
    fflush(stdout);
}

// _tin_i128_to_cstr writes v as decimal into a thread-local 50-byte buffer
// and returns a pointer to it.  Valid until the next call on the same thread.
// Used by string interpolation (snprintf %s argument).
const char *_tin_i128_to_cstr(__int128 v) {
    static _Thread_local char buf[50];
    int n; _i128_to_dec(v, buf, &n);
    return buf;
}

const char *_tin_u128_to_cstr(unsigned __int128 v) {
    static _Thread_local char buf[50];
    int n; _u128_to_dec(v, buf, &n);
    return buf;
}

// f128 helpers
//
// tin_fp128_t is declared in runtime.h as __attribute__((mode(TF))), which
// matches LLVM fp128 on all targets (x86-64, AArch64, ...) without depending
// on __float128 (x86-only) or _Float128 (needs glibc header).
// We convert to long double for display (80-bit on x86, 64-bit on AArch64).

void _tin_echo_f128(tin_fp128_t v) {
    printf("%Lg\n", (long double)v);
    fflush(stdout);
}

const char *_tin_f128_to_cstr(tin_fp128_t v) {
    static _Thread_local char buf[64];
    snprintf(buf, sizeof(buf), "%Lg", (long double)v);
    return buf;
}
