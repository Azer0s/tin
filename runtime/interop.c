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

extern void _tin_fiber_init(void);

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
