#pragma once
// TinFastMutex - coroutine-aware spinlock with direct fiber resume.
//
// A fast mutex for short critical sections in the Tin runtime.
// Uses a single atomic state word; no OS primitives in the fast path.
//
// State encoding (uint32_t):
//   0  - UNLOCKED, no waiters
//   1  - LOCKED, no waiters
//   2  - LOCKED, at least one waiter in the embedded array
//
// Fast paths (uncontended, common case):
//   lock:   CAS(0→1)  — single atomic op, no OS call, no wl_lock
//   unlock: CAS(1→0)  — single atomic op, no OS call, no wl_lock
//
// Slow paths (contended, rare):
//   lock:   brief spin, then add {pid,hdl} to waiter array, CAS(1→2),
//           call _tin_fiber_park, return 0 (caller yields)
//   unlock: CAS(1→0) fails because state==2; acquire wl_lock, pop one
//           waiter, update state, call _tin_fiber_unpark_hdl (runnext)
//
// Usage in blocking channel operations:
//
//   void *hdl = _tin_current_coro_hdl();
//   if (!tin_fmutex_lock_coro(&ch->fmu, pid, hdl))
//       return TIN_CHAN_BLOCKED;  // parked; coro yields and retries
//   ... critical section ...
//   tin_fmutex_unlock(&ch->fmu);
//
// Usage in non-blocking / non-coro contexts (close, free):
//
//   tin_fmutex_lock_spin(&ch->fmu);
//   ... critical section ...
//   tin_fmutex_unlock(&ch->fmu);

#include <stdint.h>
#include <stdatomic.h>

// Maximum number of coroutines that can be parked in one mutex's wait queue.
// Excess waiters fall back to the yield-retry loop (correct, just slower).
// Keep this small so TinFastMutex fits in ≤2 cache lines (hot lock state
// + hot channel fields must share early cache lines in TinChannel).
#define FMUTEX_MAX_WAITERS 4

typedef struct {
    _Atomic(uint32_t) state;    // 0=unlocked, 1=locked no waiters, 2=locked has waiters
    // Waiter array: only accessed when state == 2.
    // Protected by wl_lock (acquired only in the slow path — never in fast unlock).
    _Atomic(uint32_t) wl_lock;
    int64_t wpid[FMUTEX_MAX_WAITERS];
    void   *whdl[FMUTEX_MAX_WAITERS];
    int     wcnt;
} TinFastMutex;

// Zero-initialise a TinFastMutex (equivalent to = {0} but explicit).
void tin_fmutex_init(TinFastMutex *m);

// Attempt to acquire the lock with a single CAS (0→1).
// Returns 1 if locked, 0 if already held.
static inline int tin_fmutex_trylock(TinFastMutex *m) {
    uint32_t expected = 0;
    return atomic_compare_exchange_strong_explicit(
        &m->state, &expected, 1u,
        memory_order_acquire, memory_order_relaxed);
}

// Slow-path helpers — only called when the fast CAS fails.
// Defined in fastmutex.c; not meant to be called directly.
int  _tin_fmutex_lock_coro_slow(TinFastMutex *m, int64_t pid, void *hdl);
void _tin_fmutex_unlock_slow(TinFastMutex *m);

// Acquire the lock for a coroutine context.
//
// Fast path (uncontended, state == 0): single inlined CAS(0→1).
// Slow path (contended): brief spin, then register {pid,hdl} in the waiter
//   array, atomically mark state = 2 ("has waiters"), call _tin_fiber_park,
//   and return 0 so the caller can yield.
//
// Returns 1 if the lock was acquired, 0 if parked (caller must yield).
static inline int tin_fmutex_lock_coro(TinFastMutex *m, int64_t pid, void *hdl) {
    uint32_t expected = 0;
    if (__builtin_expect(
            atomic_compare_exchange_strong_explicit(&m->state, &expected, 1u,
                memory_order_acquire, memory_order_relaxed), 1))
        return 1;
    return _tin_fmutex_lock_coro_slow(m, pid, hdl);
}

// Acquire the lock by busy-spinning.  For non-coro callers.
void tin_fmutex_lock_spin(TinFastMutex *m);

// Release the lock.
//
// Fast path (state == 1, no waiters): single inlined CAS(1→0) — no wl_lock.
// Slow path (state == 2, has waiters): pop one waiter under wl_lock, update
//   state, then call _tin_fiber_unpark_hdl (runnext on worker threads).
static inline void tin_fmutex_unlock(TinFastMutex *m) {
    uint32_t expected = 1;
    if (__builtin_expect(
            atomic_compare_exchange_strong_explicit(&m->state, &expected, 0u,
                memory_order_release, memory_order_relaxed), 1))
        return;
    _tin_fmutex_unlock_slow(m);
}
