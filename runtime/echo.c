// tin runtime - typed echo and print helpers

#include "runtime.h"
#include <stdio.h>

void _tin_echo_i64(int64_t v)  { printf("%lld\n",  (long long)v);  fflush(stdout); }
void _tin_echo_u64(uint64_t v) { printf("%llu\n",  (unsigned long long)v); fflush(stdout); }
void _tin_echo_f64(double v)   { printf("%g\n",    v);             fflush(stdout); }
void _tin_echo_bool(int32_t v) { puts(v ? "true" : "false");       fflush(stdout); }
void _tin_echo_char(uint8_t v) { printf("%c\n",    v);             fflush(stdout); }

void _tin_echo_string(TinString s) {
    printf("%.*s\n", (int)s.len, s.ptr);
    fflush(stdout);
}

// Print without trailing newline (used for string interpolation pieces)
void _tin_print_i64(int64_t v)  { printf("%lld",  (long long)v); }
void _tin_print_u64(uint64_t v) { printf("%llu",  (unsigned long long)v); }
void _tin_print_f64(double v)   { printf("%g",    v); }
void _tin_print_bool(int32_t v) { fputs(v ? "true" : "false", stdout); }
void _tin_print_char(uint8_t v) { putchar(v); }
void _tin_print_string(TinString s) { printf("%.*s", (int)s.len, s.ptr); }
void _tin_print_newline(void)   { putchar('\n'); }
