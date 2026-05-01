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
typedef struct { int64_t rc; int64_t _pad; } TinRCHdr;  // 16 bytes for 128-bit SIMD alignment

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
void _tin_echo_string_escaped(const char *ptr, int64_t len);
void _tin_print_string_escaped(const char *ptr, int64_t len);
void _tin_print_i64(int64_t v);
void _tin_print_u64(uint64_t v);
void _tin_print_f64(double v);
void _tin_print_bool(int32_t v);
void _tin_print_char(uint8_t v);
void _tin_print_string(TinString s);
void _tin_print_newline(void);
// 128-bit helpers.
// __int128 / unsigned __int128 are universally available on 64-bit targets.
// For fp128 we use mode(TF) - GCC/Clang's TFmode - which maps directly to
// LLVM fp128 on all supported architectures (x86-64, Linux AArch64) without
// needing __float128 (x86-only extension) or _Float128 (requires glibc header).
// Apple clang (both x86_64 and arm64) does not support TF mode at all
// - long double is 80-bit on Intel macOS and 64-bit on arm64 macOS,
// neither of which is the 128-bit IEEE binary128 the rest of the
// runtime expects. Fall back to long double on every Apple target;
// the f128 echo / cstr helpers degrade to lower precision but link
// cleanly. (TF mode works fine on Linux/glibc and FreeBSD with both
// gcc and clang, where binary128 is implemented via compiler-rt /
// libgcc soft-float helpers.)
#if defined(__APPLE__)
typedef long double tin_fp128_t;
#else
typedef float tin_fp128_t __attribute__((mode(TF)));
#endif
void             _tin_echo_i128(__int128 v);
void             _tin_echo_u128(unsigned __int128 v);
void             _tin_echo_f128(tin_fp128_t v);
const char      *_tin_i128_to_cstr(__int128 v);
const char      *_tin_u128_to_cstr(unsigned __int128 v);
const char      *_tin_f128_to_cstr(tin_fp128_t v);

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
TinStringArray _tin_argv_to_slice(int32_t argc, char **argv);
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size);
int64_t  _tin_slice_len(TinSlice s);
void    *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size);
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size);
TinSlice _tin_slice_convert_int(TinSlice s, int64_t src_sz, int64_t tgt_sz, int32_t src_signed);

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
int64_t _tin_fiber_spawn_joinable(void *hdl);
// Stacktrace-aware spawn variants (Phase 4 of docs/plans/stacktrace-libunwind.md).
// caller_ip is the spawn-site llvm.returnaddress(0); the runtime captures
// the current fiber's pid+generation as the new fiber's parent for safe
// chain walks. Programs without stacktrace() never call these.
int64_t _tin_fiber_spawn_chain(void *hdl, uintptr_t caller_ip);
int64_t _tin_fiber_spawn_joinable_chain(void *hdl, uintptr_t caller_ip);
// Spawn-chain walk helper used by tin_capture_stacktrace. When pid == 0
// (and expected_gen == 0): reads the CURRENT fiber's spawn info; the gen
// check is skipped because the running fiber's slot can't be reclaimed
// while it's executing. When pid > 0: looks up _fibers[pid] and compares
// generation; mismatch terminates the chain so we never deref a recycled
// fiber's data as if it were the original. Returns 1 when out_* are valid,
// 0 to stop walking. Walking iteratively with this helper keeps the
// TinFiber layout private to fiber.c.
int     _tin_fiber_spawn_info(int64_t pid, int64_t expected_gen,
                              uintptr_t *out_caller_ip,
                              int64_t   *out_parent_pid,
                              int64_t   *out_parent_gen);
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
int64_t _tin_now_ns(void);   // monotonic nanoseconds
int64_t _tin_now_ns_real(void);  // wall-clock (REALTIME) nanoseconds since Unix epoch
int     _tin_instant_rfc3339(int64_t ns, char *buf);  // format ns as RFC3339 (buf >= 32)
int     _tin_from_rfc3339(const char *s, int64_t *ns_out);  // parse RFC3339 to ns

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
int32_t     _tin_learn_atom_handover(char *str); // like _tin_learn_atom but frees str when done

// -- Link-time reflection table (impl trait for struct).
// Materialized by the compiler: each `impl T for S` becomes one entry in the
// `tin_impl` (ELF) / `__DATA,__tin_impl` (Mach-O) custom section. Runtime
// walks the section once on first call and folds each entry into the
// (type_id -> [trait_atom]) table queried by traitof and friends.
void    _tin_build_impl_table(void);
int32_t _tin_impl_count_for_type(int32_t type_id);
int32_t _tin_impl_atom_for_type(int32_t type_id, int32_t idx);
int32_t _tin_impl_total_entries(void); // sum across all type_ids; for tests

// -- Stacktrace capture (frame-pointer walker; see docs/plans/stacktrace-libunwind.md)
// Writes up to `cap` interned atom codes into `out` (must be cap*sizeof(int32_t)
// bytes); returns the number actually written. Never panics; on total failure
// (NULL out, cap < 1, or unsupported arch) returns 0.
//
// `flags` is a bitfield of TIN_ST_HIDE_* constants (see below). Frames matching
// any active filter are dropped before the cap is applied, so a filtered call
// returns up to `cap` frames that survived filtering. flags=0 disables all
// filtering and gives the raw walk.
#define TIN_ST_HIDE_LIBC    0x1   // drop frames in libc / libpthread / libsystem
#define TIN_ST_HIDE_UNKNOWN 0x2   // drop frames that resolved to "??+0x<addr>"
#define TIN_ST_HIDE_RUNTIME 0x4   // drop frames whose symbol starts with "_tin_"
#define TIN_ST_HIDE_MAIN    0x8   // drop the main() / _start / __libc_start_* tail
int32_t     tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags);

// -- pclntab (PC -> file:line:col table; see runtime/pclntab.c)
//
// Replaces the libdw / DWARF dependency for stacktrace symbol resolution.
// Codegen emits one TinPclnFnHdr per Tin function into the `tin_pclntab`
// section (Linux) / `__TIN,__pclntab` (Mach-O). At image load time a
// __attribute__((constructor)) per image calls _tin_pclntab_register_self
// which finds its image's section bounds and adds them to the process-
// wide table. Lookup is done by tin_pclntab_resolve.
//
// Layout MUST match codegen/pclntab.go pclntabFnHdrType().
//
// pc_addr stores an absolute address (resolved by ld.so / dyld at load
// time under ASLR), not an offset. The runtime computes
// `pc_addr - fn_start` on demand. We store absolute pointers because
// PIC code (REPL cells / CTFE shims compiled with `-fPIC`) cannot
// represent the link-time `blockaddress(@fn,%bb) - @fn` subtraction;
// the assembler errors with "Cannot represent a difference across
// sections". Trading 4 extra bytes per entry for portability.
typedef struct {
    const void *pc_addr; // absolute address of the BB start
    uint32_t    line;
    uint32_t    col;
} TinPclnPC;

typedef struct {
    const void      *fn_start;   // bitcast of fn pointer at link time
    const char      *name;       // points into .rodata (NOT NUL-terminated)
    const char      *file;       // points into .rodata (NOT NUL-terminated)
    uint32_t         name_len;
    uint32_t         file_len;
    const TinPclnPC *pcs;        // NULL when this is a marker-only header
    uint32_t         npcs;       // 0 for marker-only headers
} TinPclnFnHdr;

// Register a per-image pclntab range. Invoked from each loaded image's
// codegen-emitted __tin_pclntab_ctor at load time. The args are
// per-image and let one helper resolve different sections per call:
//
//   ELF (Linux / FreeBSD): start/end are linker-synthesized
//     __start_/__stop_tin_pclntab references local to THIS image; the
//     marker arg is NULL.
//
//   Mach-O (macOS): start/end are NULL; the marker arg is any address
//     in this image (the constructor passes its own address). The
//     runtime uses dladdr+getsectiondata to find the section bounds.
//
// Idempotent; safe to call from any image's constructor.
void _tin_pclntab_register_image(const TinPclnFnHdr *start,
                                 const TinPclnFnHdr *end,
                                 const void *marker);

// Resolve an instruction pointer to (name, file, line, col). Returns 1 on
// match, 0 on miss. Output strings are NOT NUL-terminated; use the
// matching length fields. Safe to call from any thread; non-blocking after
// first call (sort happens once under a mutex; readers go lock-free).
int _tin_pclntab_resolve(uintptr_t ip,
                         const char **name, uint32_t *name_len,
                         const char **file, uint32_t *file_len,
                         uint32_t *line, uint32_t *col);

// -- #handover: take ownership of a C pointer returned by an extern function.
// Platform-specific malloc size detection used by arc.c / atom.c.
#ifdef __APPLE__
#  include <malloc/malloc.h>
#  define _tin_usable_size(p) ((size_t)malloc_size(p))
#else
#  include <malloc.h>
#  define _tin_usable_size(p) ((size_t)malloc_usable_size(p))
#endif
void  _tin_handover_free(void *ptr);      // free ptr if malloc'd, no-op otherwise
char *_tin_string_handover(char *src);    // copy src into RC alloc, free if malloc'd
void *_tin_ptr_handover(void *src, size_t elem_size); // RC-ify any C pointer

// -- Any equality
typedef struct { int32_t tag; void *ptr; } _TinAny;
int64_t _tin_any_eq(_TinAny a, _TinAny b);

// -- C-interop boundary helpers (runtime/interop.c)
typedef void *(*tin_alloc_fn)(size_t);
void  tin_set_extern_alloc(tin_alloc_fn fn);   // NULL resets to malloc
void *tin_extern_alloc(size_t n);
void  tin_runtime_init(void);                  // idempotent; safe to call concurrently
void  tin_release(void *ptr);                  // drop one ARC ref (for #interop *void returns)
TinString tin_interop_str_in(const char *cstr);  // C string -> ARC Tin string (caller releases)
char     *tin_interop_str_out(TinString s);     // Tin string -> C buffer via extern_alloc
TinSlice  tin_interop_slice_in(const void *data, int64_t len, int64_t elem_size);
int       tin_interop_slice_out(TinSlice s, int64_t elem_size,
                                void **out_data, int64_t *out_len);

// Closure-return trampoline allocator (runtime/interop_trampoline.c).
// tin_make_trampoline returns a stable C-callable function pointer
// that, when invoked, calls `fn(env, args...)`. Use
// tin_interop_closure_free to release the trampoline when the C side
// is done with it; any trampolines still alive at process exit are
// munmap'd via an atexit handler so leak detectors stay quiet.
void *tin_make_trampoline(void *fn, void *env, void *dispatcher);
void  tin_interop_closure_free(void *tramp);

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
