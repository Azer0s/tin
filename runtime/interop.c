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
#include <stdlib.h>
#include <stddef.h>
#include <string.h>

extern void _tin_fiber_init(void);
extern void *_tin_rc_alloc(int64_t size);

typedef void *(*tin_alloc_fn)(size_t);

static atomic_intptr_t _tin_extern_alloc_fn = (intptr_t)0;

void tin_set_extern_alloc(tin_alloc_fn fn) {
    atomic_store_explicit(&_tin_extern_alloc_fn,
                          (intptr_t)(void *)fn, memory_order_release);
}

void *tin_extern_alloc(size_t n) {
    intptr_t slot = atomic_load_explicit(&_tin_extern_alloc_fn,
                                         memory_order_acquire);
    if (slot == 0) {
        return malloc(n);
    }

    return ((tin_alloc_fn)(void *)slot)(n);
}

// String marshaling helpers used by the C wrappers emitted for #interop
// functions whose signature involves Tin strings.

// Marshal a C string into a fresh ARC-managed Tin string. Caller is
// responsible for releasing the resulting buffer after the internal
// call returns.
TinString tin_interop_str_in(const char *cstr) {
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
char *tin_interop_str_out(TinString s) {
    char *out = (char *)tin_extern_alloc((size_t)(s.len + 1));
    if (!out) return NULL;

    if (s.len > 0) {
        memcpy(out, s.ptr, (size_t)s.len);
    }

    out[s.len] = '\0';

    return out;
}

// Marshal a C array (data + len) into a fresh ARC-managed Tin slice.
// elem_size is the bytewidth of the element type; it is supplied by
// the wrapper at codegen time because Tin's slice carries no runtime
// type tag.
TinSlice tin_interop_slice_in(const void *data, int64_t len, int64_t elem_size) {
    if (len <= 0) {
        char *buf = (char *)_tin_rc_alloc(1);
        if (buf) buf[0] = '\0';

        return (TinSlice){buf, 0};
    }

    int64_t bytes = len * elem_size;
    void *buf = _tin_rc_alloc(bytes);
    if (!buf) return (TinSlice){NULL, 0};

    if (data) {
        memcpy(buf, data, (size_t)bytes);
    }

    return (TinSlice){buf, len};
}

// Marshal a Tin slice out to the C side via tin_extern_alloc. The
// caller passes pointers to the data and length out-slots; the
// function fills them and returns 0 on success or 1 on OOM. On OOM
// *out_data is NULL and *out_len is 0 so the caller has well-defined
// state.
int tin_interop_slice_out(TinSlice s, int64_t elem_size,
                          void **out_data, int64_t *out_len) {
    if (out_len) *out_len = s.len;

    if (s.len <= 0) {
        if (out_data) *out_data = NULL;
        return 0;
    }

    int64_t bytes = s.len * elem_size;
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

void _tin_runtime_init_once(void) {
    int s = atomic_load_explicit(&_tin_rt_initialized, memory_order_acquire);
    if (s == 2) {
        return;
    }

    int expected = 0;
    if (atomic_compare_exchange_strong_explicit(&_tin_rt_initialized,
                                                &expected, 1,
                                                memory_order_acq_rel,
                                                memory_order_acquire)) {
        // Winner: do the actual init.
        _tin_fiber_init();
        atomic_store_explicit(&_tin_rt_initialized, 2, memory_order_release);

        return;
    }
    // Loser: another thread is initializing. Spin until done. Cheap
    // because init runs once per process and is fast.
    while (atomic_load_explicit(&_tin_rt_initialized, memory_order_acquire) != 2) {
        // intentionally tight; init is brief
    }
}
