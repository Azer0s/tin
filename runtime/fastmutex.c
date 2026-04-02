// TinFastMutex - coroutine-aware spinlock with direct fiber resume.
//
// See fastmutex.h for the design rationale, state encoding, and usage guide.

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
    m->wcnt = 0;
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

    if (m->wcnt >= FMUTEX_MAX_WAITERS) {
        _wl_unlock(m);
        return 0;  // list full: yield-retry
    }

    m->wpid[m->wcnt] = pid;
    m->whdl[m->wcnt] = hdl;
    m->wcnt++;

    // Atomically transition state 1→2 to tell the unlocker "there are waiters".
    // If this CAS fails, the mutex was released while we were adding ourselves
    // (state became 0).  Remove the entry we just added and tell the caller to
    // retry — the lock is now free so the next CAS will succeed.
    expected = 1;
    int marked = atomic_compare_exchange_strong_explicit(
        &m->state, &expected, 2u,
        memory_order_acq_rel, memory_order_relaxed);
    if (!marked) {
        // Unlock already fired; undo the waiter registration.
        m->wcnt--;
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
    // Pop one waiter under wl_lock, update state, then directly resume it.
    _wl_lock(m);

    int64_t pid = -1;
    void   *hdl = NULL;
    if (m->wcnt > 0) {
        m->wcnt--;
        pid = m->wpid[m->wcnt];
        hdl = m->whdl[m->wcnt];
        // If no more waiters: transition 2→0 (unlock + clear waiters flag).
        // If more waiters remain: stay at 2 (still locked, still has waiters).
        // The woken waiter will contend for the lock again after re-acquiring.
        atomic_store_explicit(&m->state, m->wcnt > 0 ? 2u : 0u,
                              memory_order_release);
    } else {
        // Shouldn't happen (state==2 with wcnt==0), but be defensive.
        atomic_store_explicit(&m->state, 0u, memory_order_release);
    }

    _wl_unlock(m);

    if (pid >= 0) _tin_fiber_unpark_hdl(pid, hdl);
}
