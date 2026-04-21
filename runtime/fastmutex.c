// TinFastMutex - coroutine-aware spinlock with direct fiber resume.
//
// See fastmutex.h for the design rationale, state encoding, and usage guide.

#include <stdlib.h>
#include "runtime.h"
#include "fastmutex.h"
#include "fiber.h"

// CPU-level spin hint: avoids cache-line thrashing on the atomic state word.
static inline void _fmu_relax(void) {
#if defined(__x86_64__) || defined(__i386__)
    __asm__ volatile("pause" ::: "memory");
#elif defined(__aarch64__) || defined(__arm__)
    __asm__ volatile("yield" ::: "memory");
#endif
}

void tin_fmutex_init(TinFastMutex *m) {
    atomic_store_explicit(&m->state,   0u, memory_order_relaxed);
    atomic_store_explicit(&m->wl_lock, 0u, memory_order_relaxed);
    m->wcnt     = 0;
    m->overflow = NULL;
}

// Acquire the waiter-list spinlock.  Only called in contended slow paths.
static inline void _wl_lock(TinFastMutex *m) {
    uint32_t zero = 0;
    while (!atomic_compare_exchange_weak_explicit(
               &m->wl_lock, &zero, 1u,
               memory_order_acquire, memory_order_relaxed)) {
        _fmu_relax();
        zero = 0;
    }
}

static inline void _wl_unlock(TinFastMutex *m) {
    atomic_store_explicit(&m->wl_lock, 0u, memory_order_release);
}

int _tin_fmutex_lock_coro_slow(TinFastMutex *m, int64_t pid, void *hdl) {
    // Fast CAS already failed in the inlined wrapper.  Brief spin first.
    uint32_t expected;
    for (int i = 0; i < 32; i++) {
        _fmu_relax();
        expected = 0;
        if (atomic_compare_exchange_weak_explicit(&m->state, &expected, 1u,
                memory_order_acquire, memory_order_relaxed))
            return 1;
    }

    // Still contended.  If we have a valid fiber identity, register as a waiter
    // and park; otherwise fall through to yield-retry.
    if (pid <= 0 || !hdl)
        return 0;  // no pid/hdl: yield-retry

    // Register under the waiter-list spinlock.
    _wl_lock(m);

    int used_overflow = 0;
    if (m->wcnt >= FMUTEX_MAX_WAITERS) {
        // Embedded array full: push to the overflow linked list so this fiber
        // is still properly parked rather than spinning in a yield-retry loop.
        TinFibWaiter *w = (TinFibWaiter *)malloc(sizeof(TinFibWaiter));
        if (!w) {
            // OOM: fall back to yield-retry rather than crashing.
            _wl_unlock(m);
            return 0;
        }
        w->pid      = pid;
        w->hdl      = hdl;
        w->next     = m->overflow;
        m->overflow = w;
        used_overflow = 1;
    } else {
        m->wpid[m->wcnt] = pid;
        m->whdl[m->wcnt] = hdl;
        m->wcnt++;
    }

    // Atomically transition state 1→2 to tell the unlocker "there are waiters".
    // If this CAS fails, the mutex was released while we were adding ourselves
    // (state became 0).  Remove the entry we just added and tell the caller to
    // retry - the lock is now free so the next CAS will succeed.
    expected = 1;
    int marked = atomic_compare_exchange_strong_explicit(
        &m->state, &expected, 2u,
        memory_order_acq_rel, memory_order_relaxed);
    if (!marked) {
        // Unlock already fired; undo the waiter registration.
        if (used_overflow) {
            // Pop the node we just prepended to the overflow list.
            TinFibWaiter *w = m->overflow;
            m->overflow = w->next;
            free(w);
        } else {
            m->wcnt--;
        }
        _wl_unlock(m);
        return 0;  // lock is now free; caller will retry (CAS will succeed)
    }
    _wl_unlock(m);

    // Park this fiber.  The worker loop defers FIBER_BLOCKED until after
    // coro.suspend fires (pending_park pattern), so tin_fmutex_unlock can
    // safely call _tin_fiber_unpark between here and the yield without
    // causing a double-resume.
    _tin_fiber_park(pid);
    return 0;  // parked; caller must yield
}

void tin_fmutex_lock_spin(TinFastMutex *m) {
    uint32_t expected = 0;
    while (!atomic_compare_exchange_weak_explicit(&m->state, &expected, 1u,
               memory_order_acquire, memory_order_relaxed)) {
        _fmu_relax();
        expected = 0;
    }
}

void _tin_fmutex_unlock_slow(TinFastMutex *m) {
    // Fast CAS already failed in the inlined wrapper.
    // Slow path: state must be 2 (has waiters).
    // Pop one waiter (embedded array first, then overflow list) under wl_lock,
    // update state, then directly resume the fiber.
    _wl_lock(m);

    int64_t pid = -1;
    void   *hdl = NULL;

    if (m->wcnt > 0) {
        // Pop from the embedded array (LIFO - cheapest to pop).
        m->wcnt--;
        pid = m->wpid[m->wcnt];
        hdl = m->whdl[m->wcnt];
    } else if (m->overflow) {
        // Embedded array empty, drain from the overflow linked list.
        TinFibWaiter *w = m->overflow;
        m->overflow = w->next;
        pid = w->pid;
        hdl = w->hdl;
        free(w);
    }

    // If more waiters remain (array or overflow), stay at 2.
    // Otherwise unlock: transition 2→0.
    int has_more = (m->wcnt > 0) || (m->overflow != NULL);
    atomic_store_explicit(&m->state, has_more ? 2u : 0u, memory_order_release);

    _wl_unlock(m);

    if (pid >= 0) _tin_fiber_unpark_hdl(pid, hdl);
}
