// tin runtime - C-interop boundary helpers.
//
// Exposed to C callers (and to wrappers emitted for #interop functions):
//
//   void  tin_set_extern_alloc(tin_alloc_fn fn);
//   void* tin_extern_alloc(size_t n);
//   void  _tin_runtime_init_once(void);
//
// `tin_set_extern_alloc` swaps the callback used by #interop wrappers
// when they need to hand a freshly-allocated buffer to the C caller
// (e.g. when returning a Tin string). The default is malloc(3); pass
// NULL to reset.
//
// `_tin_runtime_init_once` is what the wrapper preamble calls before
// touching anything Tin-side. It guarantees one call to whatever
// runtime subsystems need bootstrapping (today: the fiber scheduler /
// worker pool). Subsequent calls are cheap atomic-load no-ops.

#include <stdatomic.h>
#include <stdint.h>
#include <stdlib.h>
#include <stddef.h>
#include <string.h>

extern void _tin_fiber_init(void);
extern void _tin_fiber_run(void);
extern void *_tin_rc_alloc(int64_t size);
extern void _tin_release(void *ptr);

// _tin_runtime_atexit drains the worker pool, joins the IO/timer
// threads, and frees the fiber/runqueue/IO/timer state that
// _tin_fiber_init lazily allocated. Registered from tin_runtime_init,
// which runs only on the #interop entry path: ordinary `tin run`
// programs reach _tin_fiber_run directly from the codegen-emitted
// main() and don't need an atexit shim. Calling _tin_fiber_run twice
// is safe (it bails immediately when _workers == NULL after the
// first teardown), so no extra coordination is needed if a future
// codegen change accidentally double-registers.
static void _tin_runtime_atexit(void) {
    _tin_fiber_run();
}

// TIN_API marks a symbol exported across the C-interop boundary.
// Surfaces past `-fvisibility=hidden` shared-library builds; on
// platforms without GCC-style attributes it expands to nothing.
#if defined(__GNUC__) || defined(__clang__)
#define TIN_API __attribute__((visibility("default")))
#else
#define TIN_API
#endif

typedef void *(*tin_alloc_fn)(size_t);
typedef void  (*tin_free_fn)(void *);

static atomic_intptr_t _tin_extern_alloc_fn = (intptr_t)0;
static atomic_intptr_t _tin_extern_free_fn  = (intptr_t)0;

TIN_API void tin_set_extern_alloc(tin_alloc_fn fn) {
    atomic_store_explicit(&_tin_extern_alloc_fn,
                          (intptr_t)(void *)fn, memory_order_release);
}

// Pair `tin_set_extern_alloc` so consumers that swap in a custom allocator
// can also swap in a matching deallocator. Callers of `tin_extern_alloc`
// that need to release the buffer (e.g. the CTFE compile-time dispatch
// when consuming a #pure #interop string return) MUST go through
// `tin_extern_free` rather than libc free; otherwise a non-malloc
// allocator's metadata gets corrupted.
TIN_API void tin_set_extern_free(tin_free_fn fn) {
    atomic_store_explicit(&_tin_extern_free_fn,
                          (intptr_t)(void *)fn, memory_order_release);
}

TIN_API void *tin_extern_alloc(size_t n) {
    intptr_t slot = atomic_load_explicit(&_tin_extern_alloc_fn,
                                         memory_order_acquire);
    if (slot == 0) {
        return malloc(n);
    }

    return ((tin_alloc_fn)(void *)slot)(n);
}

TIN_API void tin_extern_free(void *p) {
    if (!p) return;

    intptr_t slot = atomic_load_explicit(&_tin_extern_free_fn,
                                         memory_order_acquire);
    if (slot == 0) {
        free(p);
        return;
    }

    ((tin_free_fn)(void *)slot)(p);
}

// String marshaling helpers used by the C wrappers emitted for #interop
// functions whose signature involves Tin strings.

// Marshal a C string into a fresh ARC-managed Tin string. Caller is
// responsible for releasing the resulting buffer after the internal
// call returns.
TIN_API TinString tin_interop_str_in(const char *cstr) {
    if (!cstr) {
        char *buf = (char *)_tin_rc_alloc(1);
        if (buf) buf[0] = '\0';

        return (TinString){buf, 0};
    }

    int64_t len = (int64_t)strlen(cstr);
    char *buf = (char *)_tin_rc_alloc(len + 1);
    if (!buf) return (TinString){NULL, 0};

    memcpy(buf, cstr, (size_t)len);
    buf[len] = '\0';

    return (TinString){buf, len};
}

// Marshal a Tin string out to the C side via the user-configurable
// allocator. The returned buffer is NUL-terminated and contains
// `s.len + 1` bytes. Returns NULL on OOM (allocator returned NULL).
TIN_API char *tin_interop_str_out(TinString s) {
    char *out = (char *)tin_extern_alloc((size_t)(s.len + 1));
    if (!out) return NULL;

    if (s.len > 0) {
        memcpy(out, s.ptr, (size_t)s.len);
    }

    out[s.len] = '\0';

    return out;
}

// safe_mul64 multiplies two non-negative int64s and reports overflow.
// Used to guard slice-allocation sizes against hostile or buggy inputs;
// silent overflow on the multiplication would translate into a too-small
// alloc and a buffer overrun in the subsequent memcpy.
static int safe_mul64(int64_t a, int64_t b, int64_t *out) {
    if (a < 0 || b < 0) return -1;
    if (a != 0 && b > INT64_MAX / a) return -1;
    *out = a * b;
    return 0;
}

// Marshal a C array (data + len) into a fresh ARC-managed Tin slice.
// elem_size is the bytewidth of the element type; it is supplied by
// the wrapper at codegen time because Tin's slice carries no runtime
// type tag.
TIN_API TinSlice tin_interop_slice_in(const void *data, int64_t len, int64_t elem_size) {
    if (len <= 0) {
        char *buf = (char *)_tin_rc_alloc(1);
        if (buf) buf[0] = '\0';

        return (TinSlice){buf, 0};
    }

    int64_t bytes;
    if (safe_mul64(len, elem_size, &bytes) != 0) {
        return (TinSlice){NULL, 0};
    }

    void *buf = _tin_rc_alloc(bytes);
    if (!buf) return (TinSlice){NULL, 0};

    if (data) {
        memcpy(buf, data, (size_t)bytes);
    }

    return (TinSlice){buf, len};
}

// Marshal a Tin slice out to the C side via tin_extern_alloc. The
// caller passes pointers to the data and length out-slots; the
// function fills them and returns 0 on success or 1 on OOM/overflow.
// On failure *out_data is NULL and *out_len is 0 so the caller has
// well-defined state.
TIN_API int tin_interop_slice_out(TinSlice s, int64_t elem_size,
                                  void **out_data, int64_t *out_len) {
    if (out_len) *out_len = s.len;

    if (s.len <= 0) {
        if (out_data) *out_data = NULL;
        return 0;
    }

    int64_t bytes;
    if (safe_mul64(s.len, elem_size, &bytes) != 0) {
        if (out_data) *out_data = NULL;
        if (out_len) *out_len = 0;
        return 1;
    }

    void *out = tin_extern_alloc((size_t)bytes);
    if (!out) {
        if (out_data) *out_data = NULL;
        if (out_len) *out_len = 0;
        return 1;
    }

    memcpy(out, s.ptr, (size_t)bytes);

    if (out_data) *out_data = out;

    return 0;
}

// Init state machine: 0 = uninit, 1 = in-progress, 2 = done. Single
// CAS from 0 -> 1 wins the race; the loser spins until state == 2.
static atomic_int _tin_rt_initialized = 0;

// tin_runtime_init brings the Tin runtime up if it has not been
// brought up yet. Idempotent and safe under concurrent first-callers.
// The wrapper preamble for every #interop function calls this on
// entry; C code that wants to control init timing (e.g., set up an
// allocator before any Tin code runs) can also call it directly.
// tin_release drops one ARC reference to a pointer returned from a
// `#interop` function (typically a `*void` opaque handle). Use this
// to reclaim Tin-allocated blocks the C side received from `#interop`
// returns. NULL-safe.
TIN_API void tin_release(void *ptr) {
    _tin_release(ptr);
}

TIN_API void tin_runtime_init(void) {
    int s = atomic_load_explicit(&_tin_rt_initialized, memory_order_acquire);
    if (s == 2) {
        return;
    }

    int expected = 0;
    if (atomic_compare_exchange_strong_explicit(&_tin_rt_initialized,
                                                &expected, 1,
                                                memory_order_acq_rel,
                                                memory_order_acquire)) {
        _tin_fiber_init();
        // Register the teardown hook only on the winning init path so
        // atexit isn't called twice if multiple threads race here. atexit
        // failure is non-fatal: we'd lose the runtime cleanup at exit
        // (still-reachable allocations on a dying process), but
        // functional behaviour is unaffected.
        (void)atexit(_tin_runtime_atexit);
        atomic_store_explicit(&_tin_rt_initialized, 2, memory_order_release);

        return;
    }
    // Loser of the init race: spin until the winner publishes state=2.
    // Init runs once per process and is fast; no syscalls in this loop.
    while (atomic_load_explicit(&_tin_rt_initialized, memory_order_acquire) != 2) {
        /* intentionally tight */
    }
}
