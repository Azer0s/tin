#pragma once
// tin runtime - shared types and function declarations
//
// Include this header from any C file that needs to call into the tin runtime
// (e.g. extern C files compiled via //!+file.c -- -I /path/to/runtime).

#include <stdint.h>
#include <stdbool.h>

// Sentinel reference-count value for immortal (static/literal) objects
#define TIN_IMMORTAL_RC ((int64_t)-1)

// -- Core types

// Fat-pointer string: { i8* ptr, i64 len }
typedef struct { const char *ptr; int64_t len; } TinString;

// Dynamic array: { T* ptr, i64 len }
typedef struct { void *ptr; int64_t len; } TinSlice;

// ARC header prepended before every heap-managed block
typedef struct { int64_t rc; } TinRCHdr;

// Defer chain node - allocated on the registering frame's stack
typedef struct TinDeferEntry {
    struct TinDeferEntry *prev;
    void (*fn)(void *env);
    void *env;
} TinDeferEntry;

// Array of TinStrings returned by _tin_reflect_fn_params
typedef struct { TinString *ptr; int64_t len; } TinStringArray;

// -- ARC
void *_tin_rc_alloc(int64_t size);
void  _tin_retain(void *ptr);
void  _tin_release(void *ptr);

// -- Echo / print
void _tin_echo_i64(int64_t v);
void _tin_echo_u64(uint64_t v);
void _tin_echo_f64(double v);
void _tin_echo_bool(int32_t v);
void _tin_echo_char(uint8_t v);
void _tin_echo_string(TinString s);
void _tin_print_i64(int64_t v);
void _tin_print_u64(uint64_t v);
void _tin_print_f64(double v);
void _tin_print_bool(int32_t v);
void _tin_print_char(uint8_t v);
void _tin_print_string(TinString s);
void _tin_print_newline(void);

// -- Strings
TinString _tin_str_concat(TinString a, TinString b);
TinString _tin_str_from_cstr(const char *s);
int32_t   _tin_str_eq(TinString a, TinString b);

// -- Slices
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size);
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size);
int64_t  _tin_slice_len(TinSlice s);
void    *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size);
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size);

// -- Memory
void *_tin_malloc(int64_t size);
void  _tin_free(void *p);

// -- Defer / panic
void      _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *), void *env);
void      _tin_defer_pop(int64_t n);
void      _tin_panic(const char *msg);
void      _tin_assert(int32_t cond, const char *msg);
TinString _tin_recover(void);

// -- Length builtins
int64_t _tin_len_string(TinString s);
int64_t _tin_len_slice(TinSlice s);

// -- Test runner
void    _tin_run_test(TinString desc, void *fn);
int64_t _tin_test_finish(int64_t total);
void    _tin_assert_abort(const char *msg);

// -- Runtime atom table
int32_t     _tin_learn_atom(const char *str);
const char *_tin_rt_atom_to_str(int32_t code);

// -- Any equality
typedef struct { int32_t tag; void *ptr; } _TinAny;
int64_t _tin_any_eq(_TinAny a, _TinAny b);

// -- Reflect (stdlib/reflect/reflect.c - linked in when reflect module is used)
const char   *_tin_reflect_kind(const char *atom);
int64_t       _tin_reflect_is_ptr(const char *atom);
int64_t       _tin_reflect_is_array(const char *atom);
int64_t       _tin_reflect_is_fn(const char *atom);
int64_t       _tin_reflect_is_primitive(const char *atom);
const char   *_tin_reflect_elem(const char *atom);
const char   *_tin_reflect_fn_ret(const char *atom);
int64_t       _tin_reflect_fn_arity(const char *atom);
const char   *_tin_reflect_fn_param(const char *atom, int64_t idx);
TinStringArray _tin_reflect_fn_params(const char *atom);
