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
    void (*fn)(void *env, void *ret_slot);
    void *env;
    void *ret_slot;  // pointer to outer function's return-override slot (may be NULL)
} TinDeferEntry;

// Array of TinStrings returned by _tin_reflect_fn_params (legacy, [string] variant)
typedef struct { TinString *ptr; int64_t len; } TinStringArray;

// Single atom value { i32 code } - matches %__atom LLVM type
typedef struct { int32_t code; } TinAtom;

// Array of TinAtoms returned by _tin_reflect_fn_params ([atom] variant)
typedef struct { TinAtom *ptr; int64_t len; } TinAtomArray;

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
TinString _tin_string_from_bytes(const char *ptr, int64_t len);
const char *_tin_string_data(TinString s);
char *_tin_buf_alloc(int64_t n);
char *_tin_buf_realloc(char *p, int64_t n);
void  _tin_buf_free(char *p);
TinSlice _tin_bytes_from_buf(const char *ptr, int64_t len);

// -- Slices
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size);
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size);
int64_t  _tin_slice_len(TinSlice s);
void    *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size);
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size);

// -- Memory
void *_tin_malloc(int64_t size);
void  _tin_free(void *p);

// -- Coroutine frame pool (hot path: no system malloc/free for reused frames)
void *_tin_coro_malloc(int64_t size);
void  _tin_coro_free(void *ptr);

// -- Defer / panic
void      _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *, void *), void *env, void *ret_slot);
void      _tin_defer_pop(int64_t n);
void      _tin_panic(const char *msg);
void      _tin_assert(int32_t cond, const char *msg);
TinString _tin_recover(void);
// Fiber-level panic interception (called by worker loop around _coro_resume).
void        _tin_panic_catch_begin(void);
const char *_tin_panic_catch_end(void);

// -- Length builtins
int64_t _tin_len_string(TinString s);
int64_t _tin_len_slice(TinSlice s);

// -- Test runner
void    _tin_run_test(TinString desc, void *fn);
int64_t _tin_test_finish(int64_t total);
void    _tin_assert_abort(const char *msg);

// -- Fiber scheduler (M:N, TINMAXPROCS worker threads)
void    _tin_fiber_init(void);
int64_t _tin_fiber_spawn(void *hdl);
void    _tin_fiber_complete(void *result);
void    _tin_fiber_join(int64_t pid, void *my_hdl);
void   *_tin_fiber_get_result(int64_t pid);
const char *_tin_fiber_get_panic_msg(int64_t pid);  // NULL if completed normally
void    _tin_fiber_yield_coro(void *hdl);
void    _tin_fiber_run(void);
int64_t _tin_current_pid(void);
void    _tin_fiber_unpark(int64_t pid);
void    _tin_fiber_park(int64_t pid);
void   *_tin_coro_take_result(void); // for coroutine-chaining drive loop

// -- Timer / sleep / clock
void    _tin_sleep_ms(int64_t ms);
int64_t _tin_now_ms(void);   // monotonic milliseconds
int64_t _tin_now_us(void);   // monotonic microseconds

// -- Async I/O (dedicated epoll/kqueue I/O thread)
void    _tin_io_init(void);
void    _tin_io_shutdown(void);
int64_t _tin_async_read(int fd, void *buf, int64_t n);
int64_t _tin_async_write(int fd, const void *buf, int64_t n);
int64_t _tin_io_blocked_val(void);

// -- fd close (generic; used by all socket stdlibs)
void    _tin_fd_close(int32_t fd);

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
TinAtomArray   _tin_reflect_fn_params(const char *atom);
