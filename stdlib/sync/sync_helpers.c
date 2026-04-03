// tin stdlib/sync - Fiber-aware synchronization primitives + AtomicI64
//
// Mutex, RWMutex, and Cond are built on TinFastMutex (runtime/fastmutex.h)
// rather than pthreads.  TinFastMutex parks contended fibers via
// _tin_fiber_park / _tin_fiber_unpark_hdl instead of blocking the OS thread,
// so these primitives are safe to use from fiber-scheduled coroutines.
//
// AtomicI64 uses C11 __atomic builtins; no OS primitives needed.

#include <stdlib.h>
#include <stdio.h>
#include <stdint.h>
#include <stdatomic.h>
#include <string.h>

// Fiber park / unpark (runtime/fiber.c)
void    _tin_fiber_park(int64_t pid);
void    _tin_fiber_unpark_hdl(int64_t pid, void *hdl);

// Current coroutine handle (TLS, runtime/fiber.c)
extern __thread void *_current_hdl;
static inline void *_sync_coro_hdl(void) { return _current_hdl; }

// TinFastMutex: coroutine-aware spinlock (runtime/fastmutex.h/.c)
#include "../../runtime/fastmutex.h"

// ---------------------------------------------------------------------------
// Mutex  (exclusive lock)
// ---------------------------------------------------------------------------

typedef struct { TinFastMutex fmu; } TinMutexFA;

void *_tin_fmutex2_new(void) {
    TinMutexFA *m = (TinMutexFA *)malloc(sizeof(TinMutexFA));
    if (!m) { fputs("tin: mutex alloc failed\n", stderr); exit(1); }
    tin_fmutex_init(&m->fmu);
    return m;
}

// Try to acquire. Returns 1 if locked, 0 if parked (caller must yield+retry).
int _tin_fmutex2_try_lock(void *m, int64_t pid) {
    return tin_fmutex_lock_coro(&((TinMutexFA *)m)->fmu, pid, _sync_coro_hdl());
}

void _tin_fmutex2_unlock(void *m) { tin_fmutex_unlock(&((TinMutexFA *)m)->fmu); }
void _tin_fmutex2_free(void *m)   { free(m); }

// ---------------------------------------------------------------------------
// RWMutex  (reader-writer lock)
//
// State (atomic int32):
//   0   - unlocked
//   N>0 - N concurrent readers
//  -1   - write-locked
// ---------------------------------------------------------------------------

#define RWMU_MAX_WAITERS 8

typedef struct {
    _Atomic(int32_t)  state;
    _Atomic(uint32_t) wl_lock;              // protects waiter arrays
    int64_t rw_pid[RWMU_MAX_WAITERS];       // reader waiters
    void   *rw_hdl[RWMU_MAX_WAITERS];
    int     rw_cnt;
    int64_t ww_pid[RWMU_MAX_WAITERS];       // writer waiters
    void   *ww_hdl[RWMU_MAX_WAITERS];
    int     ww_cnt;
} TinRWMutexFA;

static void _rwmu_lock(TinRWMutexFA *m) {
    uint32_t z = 0;
    while (!atomic_compare_exchange_weak_explicit(&m->wl_lock, &z, 1u,
               memory_order_acquire, memory_order_relaxed))
        z = 0;
}
static void _rwmu_unlock(TinRWMutexFA *m) {
    atomic_store_explicit(&m->wl_lock, 0u, memory_order_release);
}

void *_tin_frwmutex2_new(void) {
    TinRWMutexFA *m = (TinRWMutexFA *)calloc(1, sizeof(TinRWMutexFA));
    if (!m) { fputs("tin: rwmutex alloc failed\n", stderr); exit(1); }
    return m;
}

// Read-lock: returns 1 if acquired, 0 if parked (write lock held).
int _tin_frwmutex2_rlock_try(void *p, int64_t pid) {
    TinRWMutexFA *m = (TinRWMutexFA *)p;
    int32_t s = atomic_load_explicit(&m->state, memory_order_acquire);
    while (s >= 0) {
        if (atomic_compare_exchange_weak_explicit(&m->state, &s, s + 1,
                memory_order_acquire, memory_order_relaxed))
            return 1;
    }
    void *hdl = _sync_coro_hdl();
    if (!pid || !hdl) return 0;
    _rwmu_lock(m);
    if (m->rw_cnt < RWMU_MAX_WAITERS) {
        m->rw_pid[m->rw_cnt] = pid;
        m->rw_hdl[m->rw_cnt] = hdl;
        m->rw_cnt++;
    }
    _rwmu_unlock(m);
    _tin_fiber_park(pid);
    return 0;
}

void _tin_frwmutex2_runlock(void *p) {
    TinRWMutexFA *m = (TinRWMutexFA *)p;
    int32_t old = atomic_fetch_sub_explicit(&m->state, 1, memory_order_acq_rel);
    if (old != 1) return;
    // Last reader: wake one write waiter.
    _rwmu_lock(m);
    int64_t pid = -1; void *hdl = NULL;
    if (m->ww_cnt > 0) {
        m->ww_cnt--;
        pid = m->ww_pid[m->ww_cnt];
        hdl = m->ww_hdl[m->ww_cnt];
    }
    _rwmu_unlock(m);
    if (pid >= 0) _tin_fiber_unpark_hdl(pid, hdl);
}

// Write-lock: returns 1 if acquired, 0 if parked.
int _tin_frwmutex2_lock_try(void *p, int64_t pid) {
    TinRWMutexFA *m = (TinRWMutexFA *)p;
    int32_t expected = 0;
    if (atomic_compare_exchange_strong_explicit(&m->state, &expected, -1,
            memory_order_acquire, memory_order_relaxed))
        return 1;
    void *hdl = _sync_coro_hdl();
    if (!pid || !hdl) return 0;
    _rwmu_lock(m);
    if (m->ww_cnt < RWMU_MAX_WAITERS) {
        m->ww_pid[m->ww_cnt] = pid;
        m->ww_hdl[m->ww_cnt] = hdl;
        m->ww_cnt++;
    }
    _rwmu_unlock(m);
    _tin_fiber_park(pid);
    return 0;
}

void _tin_frwmutex2_unlock(void *p) {
    TinRWMutexFA *m = (TinRWMutexFA *)p;
    atomic_store_explicit(&m->state, 0, memory_order_release);
    _rwmu_lock(m);
    // Prefer waking readers; fall back to one writer if no readers waiting.
    int rn = m->rw_cnt; m->rw_cnt = 0;
    int64_t rpid[RWMU_MAX_WAITERS]; void *rhdl[RWMU_MAX_WAITERS];
    memcpy(rpid, m->rw_pid, rn * sizeof(int64_t));
    memcpy(rhdl, m->rw_hdl, rn * sizeof(void *));
    int64_t wpid = -1; void *whdl = NULL;
    if (rn == 0 && m->ww_cnt > 0) {
        m->ww_cnt--;
        wpid = m->ww_pid[m->ww_cnt];
        whdl = m->ww_hdl[m->ww_cnt];
    }
    _rwmu_unlock(m);
    for (int i = 0; i < rn; i++) _tin_fiber_unpark_hdl(rpid[i], rhdl[i]);
    if (wpid >= 0) _tin_fiber_unpark_hdl(wpid, whdl);
}

void _tin_frwmutex2_free(void *p) { free(p); }

// ---------------------------------------------------------------------------
// Cond  (condition variable)
// ---------------------------------------------------------------------------

#define COND_MAX_WAITERS 16

typedef struct {
    _Atomic(uint32_t) wl_lock;
    int64_t wpid[COND_MAX_WAITERS];
    void   *whdl[COND_MAX_WAITERS];
    int     wcnt;
} TinCondFA;

static void _cond_lock(TinCondFA *c) {
    uint32_t z = 0;
    while (!atomic_compare_exchange_weak_explicit(&c->wl_lock, &z, 1u,
               memory_order_acquire, memory_order_relaxed))
        z = 0;
}
static void _cond_unlock(TinCondFA *c) {
    atomic_store_explicit(&c->wl_lock, 0u, memory_order_release);
}

void *_tin_fcond2_new(void) {
    TinCondFA *c = (TinCondFA *)calloc(1, sizeof(TinCondFA));
    if (!c) { fputs("tin: cond alloc failed\n", stderr); exit(1); }
    return c;
}

// Register the current fiber as a waiter and set pending_park.
// Called before releasing the associated mutex.
void _tin_fcond2_add_waiter(void *p, int64_t pid) {
    TinCondFA *c = (TinCondFA *)p;
    void *hdl = _sync_coro_hdl();
    _cond_lock(c);
    if (c->wcnt < COND_MAX_WAITERS) {
        c->wpid[c->wcnt] = pid;
        c->whdl[c->wcnt] = hdl;
        c->wcnt++;
    }
    _cond_unlock(c);
    if (pid > 0) _tin_fiber_park(pid);
}

void _tin_fcond2_signal(void *p) {
    TinCondFA *c = (TinCondFA *)p;
    _cond_lock(c);
    int64_t pid = -1; void *hdl = NULL;
    if (c->wcnt > 0) {
        c->wcnt--;
        pid = c->wpid[c->wcnt];
        hdl = c->whdl[c->wcnt];
    }
    _cond_unlock(c);
    if (pid >= 0) _tin_fiber_unpark_hdl(pid, hdl);
}

void _tin_fcond2_broadcast(void *p) {
    TinCondFA *c = (TinCondFA *)p;
    _cond_lock(c);
    int n = c->wcnt; c->wcnt = 0;
    int64_t pids[COND_MAX_WAITERS]; void *hdls[COND_MAX_WAITERS];
    memcpy(pids, c->wpid, n * sizeof(int64_t));
    memcpy(hdls, c->whdl, n * sizeof(void *));
    _cond_unlock(c);
    for (int i = 0; i < n; i++) _tin_fiber_unpark_hdl(pids[i], hdls[i]);
}

void _tin_fcond2_free(void *p) { free(p); }

// ---------------------------------------------------------------------------
// AtomicI64 (unchanged)
// ---------------------------------------------------------------------------

void *_tin_atomic_new_i64(int64_t v) {
    int64_t *p = (int64_t *)malloc(sizeof(int64_t));
    if (!p) { fputs("tin: atomic alloc failed\n", stderr); exit(1); }
    __atomic_store_n(p, v, __ATOMIC_RELAXED);
    return p;
}
int64_t _tin_atomic_load_i64(void *a) {
    return __atomic_load_n((int64_t *)a, __ATOMIC_ACQUIRE);
}
void _tin_atomic_store_i64(void *a, int64_t v) {
    __atomic_store_n((int64_t *)a, v, __ATOMIC_RELEASE);
}
int64_t _tin_atomic_add_i64(void *a, int64_t delta) {
    return __atomic_fetch_add((int64_t *)a, delta, __ATOMIC_ACQ_REL) + delta;
}
int64_t _tin_atomic_cas_i64(void *a, int64_t old_val, int64_t new_val) {
    __atomic_compare_exchange_n((int64_t *)a, &old_val, new_val,
        0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE);
    return old_val;
}
