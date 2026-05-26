// tin stdlib/sync - Fiber-aware synchronization primitives + Atomic[t]
//
// Mutex, RWMutex, and Cond are built on TinFastMutex (runtime/fastmutex.h)
// rather than pthreads.  TinFastMutex parks contended fibers via
// _tin_fiber_park / _tin_fiber_unpark_hdl instead of blocking the OS thread,
// so these primitives are safe to use from fiber-scheduled coroutines.
//
// Atomic[t] for primitive t uses C11 __atomic builtins; for non-primitive
// t it falls back to a TinFastMutex-protected heap copy. Selection happens
// at compile time via where guards on the Atomic struct's methods.

#include <stdlib.h>
#include <stdio.h>
#include <stdint.h>
#include <stdatomic.h>
#include <string.h>
#include <sched.h>
#include <pthread.h>

// Fiber park / unpark (runtime/fiber.c)
void    _tin_fiber_park(int64_t pid);
void    _tin_fiber_unpark_hdl(int64_t pid, void *hdl);
int64_t _tin_current_pid(void);

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
        // Re-check: if the write lock was released while we were registering,
        // remove ourselves and tell the caller to retry rather than parking.
        // Without this, the write-unlock's waiter snapshot may have already
        // run (seeing rw_cnt == 0) and this fiber would park forever.
        if (atomic_load_explicit(&m->state, memory_order_acquire) >= 0) {
            m->rw_cnt--;
            _rwmu_unlock(m);
            return 0;
        }
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
        // Re-check: if the lock is now free, remove ourselves and retry.
        // The last reader (or writer) may have unlocked and taken a snapshot
        // of ww_cnt == 0 before we registered, so no wakeup would arrive.
        if (atomic_load_explicit(&m->state, memory_order_acquire) == 0) {
            m->ww_cnt--;
            _rwmu_unlock(m);
            return 0;
        }
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
void _tin_atomic_free_i64(void *a) { free(a); }
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

// Per-width atomic helpers for primitive Atomic[t]. The slot allocator
// returns sizeof(t) bytes; the Tin side picks the right helper via
// where-guard dispatch so each call goes to a function whose argument
// types match the natural integer width -- no runtime size switch.

void *_tin_atomic_alloc(int64_t size) {
    void *p = calloc(1, (size_t)size);
    if (!p) { fputs("tin: atomic alloc failed\n", stderr); exit(1); }
    return p;
}
// Alloc + initial-store fused so the Tin side can chain the result
// straight into rc::Cell.new without naming a `let raw = ...` temp.
// Binding the foreign block to a local triggers a spurious _tin_retain_ptr
// on a non-arc pointer; threading the call through one expression keeps
// the value in the borrow path and matches the Mutex/Cond/RWMutex shape.
void *_tin_atomic_alloc_init(int64_t size, const void *initial) {
    void *p = calloc(1, (size_t)size);
    if (!p) { fputs("tin: atomic alloc failed\n", stderr); exit(1); }
    if (initial) memcpy(p, initial, (size_t)size);
    return p;
}
void _tin_atomic_free(void *a) { free(a); }

// load: __atomic_load_n on a typed pointer issues the right-width MOV
// with the requested memory order. The memcpy from the local temp to
// the user's `out` slot is a non-atomic byte copy -- the atomicity
// guarantee comes from the load itself.
void _tin_atomic_load_64(void *a, void *out) {
    int64_t v = __atomic_load_n((int64_t *)a, __ATOMIC_ACQUIRE);
    memcpy(out, &v, 8);
}
void _tin_atomic_load_32(void *a, void *out) {
    int32_t v = __atomic_load_n((int32_t *)a, __ATOMIC_ACQUIRE);
    memcpy(out, &v, 4);
}
void _tin_atomic_load_16(void *a, void *out) {
    int16_t v = __atomic_load_n((int16_t *)a, __ATOMIC_ACQUIRE);
    memcpy(out, &v, 2);
}
void _tin_atomic_load_8(void *a, void *out) {
    int8_t v = __atomic_load_n((int8_t *)a, __ATOMIC_ACQUIRE);
    memcpy(out, &v, 1);
}

void _tin_atomic_store_64(void *a, void *src) {
    int64_t v; memcpy(&v, src, 8);
    __atomic_store_n((int64_t *)a, v, __ATOMIC_RELEASE);
}
void _tin_atomic_store_32(void *a, void *src) {
    int32_t v; memcpy(&v, src, 4);
    __atomic_store_n((int32_t *)a, v, __ATOMIC_RELEASE);
}
void _tin_atomic_store_16(void *a, void *src) {
    int16_t v; memcpy(&v, src, 2);
    __atomic_store_n((int16_t *)a, v, __ATOMIC_RELEASE);
}
void _tin_atomic_store_8(void *a, void *src) {
    int8_t v; memcpy(&v, src, 1);
    __atomic_store_n((int8_t *)a, v, __ATOMIC_RELEASE);
}

// add: per-width fetch-add. Returns the new value (post-add), matching
// the existing _tin_atomic_add_i64 signature.
int32_t _tin_atomic_add_i32(void *a, int32_t d) {
    return __atomic_fetch_add((int32_t *)a, d, __ATOMIC_ACQ_REL) + d;
}
int16_t _tin_atomic_add_i16(void *a, int16_t d) {
    return __atomic_fetch_add((int16_t *)a, d, __ATOMIC_ACQ_REL) + d;
}
int8_t _tin_atomic_add_i8(void *a, int8_t d) {
    return __atomic_fetch_add((int8_t *)a, d, __ATOMIC_ACQ_REL) + d;
}

// cas: per-width compare-and-swap. Returns the previous value (so the
// caller can branch on prev == old to detect success).
int32_t _tin_atomic_cas_i32(void *a, int32_t old_val, int32_t new_val) {
    __atomic_compare_exchange_n((int32_t *)a, &old_val, new_val,
        0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE);
    return old_val;
}
int16_t _tin_atomic_cas_i16(void *a, int16_t old_val, int16_t new_val) {
    __atomic_compare_exchange_n((int16_t *)a, &old_val, new_val,
        0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE);
    return old_val;
}
int8_t _tin_atomic_cas_i8(void *a, int8_t old_val, int8_t new_val) {
    __atomic_compare_exchange_n((int8_t *)a, &old_val, new_val,
        0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE);
    return old_val;
}

// External ARC primitives (defined in runtime/runtime.c). Used by the
// non-primitive Atomic path when the protected payload is itself an
// ARC-tracked type -- store retains the new value and releases the old,
// load retains so the caller owns RC=1. The kind discriminator picks
// the right shape (string/array vs any vs fn) to find the retainable
// pointer inside the payload bytes.
void _tin_retain(void *ptr);
void _tin_release(void *ptr);
void _tin_release_any(int32_t tag, void *data);
void _tin_release_closure(void *env);

// Mirrors codegen/runtime.go rcKind. Keep in sync.
#define TIN_RC_NONE         0
#define TIN_RC_LEADING_PTR  1
#define TIN_RC_ANY          2
#define TIN_RC_FN           3

typedef struct { int32_t tag; void *ptr; } TinAnyVal;
// Fat-fn-ptr layout: {non_colored_sync*, colored_sync*, coro_ramp*, env}.
// Only env participates in ARC; the other three slots are code pointers.
typedef struct { void *sync; void *colored; void *coro; void *env; } TinFnVal;

// rc_retain_slot retains the payload pointer at the right offset for kind.
static inline void rc_retain_slot(void *slot, int kind) {
    if (kind == TIN_RC_NONE || !slot) return;

    switch (kind) {
    case TIN_RC_LEADING_PTR: {
        void *p; memcpy(&p, slot, sizeof(void *));
        if (p) _tin_retain(p);
        break;
    }
    case TIN_RC_ANY: {
        TinAnyVal a; memcpy(&a, slot, sizeof(a));
        if (a.ptr) _tin_retain(a.ptr);
        break;
    }
    case TIN_RC_FN: {
        TinFnVal f; memcpy(&f, slot, sizeof(f));
        if (f.env) _tin_retain(f.env);
        break;
    }
    }
}

// rc_release_slot releases the payload at the right offset for kind,
// using the type-specific release entry-point so any/fn cleanups run
// (e.g. closure dtors for captured RC values).
static inline void rc_release_slot(void *slot, int kind) {
    if (kind == TIN_RC_NONE || !slot) return;

    switch (kind) {
    case TIN_RC_LEADING_PTR: {
        void *p; memcpy(&p, slot, sizeof(void *));
        if (p) _tin_release(p);
        break;
    }
    case TIN_RC_ANY: {
        TinAnyVal a; memcpy(&a, slot, sizeof(a));
        if (a.ptr) _tin_release_any(a.tag, a.ptr);
        break;
    }
    case TIN_RC_FN: {
        TinFnVal f; memcpy(&f, slot, sizeof(f));
        if (f.env) _tin_release_closure(f.env);
        break;
    }
    }
}

// ---------------------------------------------------------------------------
// Atomic[t] for non-primitive t: spinlock-protected single-cell heap copy.
// Layout: { atomic_uint lock; int64_t size; uint8_t payload[size] }
// load/store memcpy under a CAS-based spinlock; the critical section is
// a single memcpy, so contention windows are tiny and a yield-free spin
// is fine. The payload is shallow-copied; ARC-tracked types keep their
// refcount stable because the compiler emits retain/release around the
// surrounding Tin-level assignment.
//
// Why a CAS spinlock and NOT TinFastMutex: this code path is reached
// from value-type contexts (let x = atomic.load()) that aren't always
// running on a fiber-scheduled coroutine. TinFastMutex's try_lock parks
// the caller as a fiber on contention, which deadlocks the test runner
// when no scheduler is pumping. A bare spinlock avoids that and
// matches the granularity (single struct copy).
// ---------------------------------------------------------------------------

// owner_id: 0 = unlocked, otherwise the atomic-lock owner. We mix fiber
// pid and OS-thread id into a single i64 so the lock works whether we
// are inside a fiber-scheduled coroutine (pid >= 0) or on a bare thread
// (pid == -1, fall back to pthread_self with the high bit set so the
// two id spaces never collide).
typedef struct {
    _Atomic int64_t      owner;
    int32_t              depth;
    int64_t              size;
    int                  rc_kind;
    char                 payload[];
} _tin_atomic_obj;

static inline int64_t _atomic_obj_self_id(void) {
    int64_t pid = _tin_current_pid();
    if (pid >= 0) return pid + 1;            // shift so pid 0 != unlocked
    return ((int64_t)1 << 62) | (int64_t)(uintptr_t)pthread_self();
}

// Lock allows reentry from the SAME fiber (or thread, when no fiber is
// scheduled): a re-entering call simply bumps depth and returns. Without
// this, calling for_locked from inside a for_locked callback on the same
// Atomic deadlocks. Cross-fiber contention spins (the critical section
// is a single memcpy on the fast path; nested user callbacks may run
// longer but those holders never block -- they yield via sched_yield).
static inline void _tin_atomic_obj_lock(_tin_atomic_obj *o) {
    int64_t self = _atomic_obj_self_id();
    int64_t expected;
    for (;;) {
        expected = 0;
        if (atomic_compare_exchange_weak_explicit(&o->owner, &expected, self,
                memory_order_acquire, memory_order_relaxed)) {
            o->depth = 1;
            return;
        }
        // Already locked. If WE own it, recurse.
        if (atomic_load_explicit(&o->owner, memory_order_relaxed) == self) {
            o->depth++;
            return;
        }
        sched_yield();
    }
}

static inline void _tin_atomic_obj_spin_unlock(_tin_atomic_obj *o) {
    if (--o->depth > 0) return;
    atomic_store_explicit(&o->owner, 0, memory_order_release);
}

void *_tin_atomic_obj_new(int64_t size, int rc_kind) {
    if (size <= 0) { fputs("tin: atomic_obj_new: non-positive size\n", stderr); exit(1); }
    _tin_atomic_obj *o = (_tin_atomic_obj *)malloc(sizeof(_tin_atomic_obj) + (size_t)size);
    if (!o) { fputs("tin: atomic_obj_new: alloc failed\n", stderr); exit(1); }
    atomic_store_explicit(&o->owner, 0, memory_order_relaxed);
    o->depth   = 0;
    o->size    = size;
    o->rc_kind = rc_kind;
    memset(o->payload, 0, (size_t)size);

    return o;
}
// Companion to _tin_atomic_alloc_init for the obj path: copies the
// initial payload and retains its rc-slot under the protocol that
// _tin_atomic_obj_store would, but skips the lock (no other thread can
// observe this slot before the constructor returns).
void *_tin_atomic_obj_new_init(int64_t size, int rc_kind, const void *initial) {
    _tin_atomic_obj *o = (_tin_atomic_obj *)_tin_atomic_obj_new(size, rc_kind);
    if (initial) {
        rc_retain_slot((void *)initial, o->rc_kind);
        memcpy(o->payload, initial, (size_t)size);
    }
    return o;
}

void _tin_atomic_obj_free(void *p) {
    if (!p) return;
    _tin_atomic_obj *o = (_tin_atomic_obj *)p;
    rc_release_slot(o->payload, o->rc_kind);
    free(o);
}

void _tin_atomic_obj_load(void *p, void *out) {
    _tin_atomic_obj *o = (_tin_atomic_obj *)p;
    _tin_atomic_obj_lock(o);
    memcpy(out, o->payload, (size_t)o->size);
    // Retain on extract: caller owns RC=1, balanced by their normal
    // scope-exit release. Same protocol as Channel.recv.
    rc_retain_slot(out, o->rc_kind);
    _tin_atomic_obj_spin_unlock(o);
}

void _tin_atomic_obj_store(void *p, void *src) {
    _tin_atomic_obj *o = (_tin_atomic_obj *)p;
    _tin_atomic_obj_lock(o);
    // Retain new before releasing old in case they alias the same payload.
    rc_retain_slot(src, o->rc_kind);
    rc_release_slot(o->payload, o->rc_kind);
    memcpy(o->payload, src, (size_t)o->size);
    _tin_atomic_obj_spin_unlock(o);
}

// Locking primitives for for_locked. The Tin side calls
// _tin_atomic_obj_lock_payload to acquire the spinlock and receive a
// pointer into the protected payload, runs its callback, then calls
// _tin_atomic_obj_unlock to release. Splits the lock/unlock so the
// user's callback runs INSIDE the critical section.
void *_tin_atomic_obj_lock_payload(void *p) {
    _tin_atomic_obj *o = (_tin_atomic_obj *)p;
    _tin_atomic_obj_lock(o);
    return o->payload;
}

// Public wrapper for the unlock -- same body as the static inline above
// but exposed as a regular extern symbol the Tin extern can bind to.
void _tin_atomic_obj_unlock(void *p) {
    _tin_atomic_obj_spin_unlock((_tin_atomic_obj *)p);
}
