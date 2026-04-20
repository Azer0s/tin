// tin runtime - M:N fiber scheduler
//
// N OS worker threads execute M coroutine fibers from a shared run queue.
// Number of workers = TINMAXPROCS env var, defaulting to nproc (min 1).
//
// Fiber states:
//   FIBER_RUNNABLE  - queued, waiting to run
//   FIBER_RUNNING   - currently executing on a worker thread
//   FIBER_BLOCKED   - suspended, waiting for I/O, sleep, or join
//   FIBER_DONE      - completed; result is available
//
// Lock ordering (never hold inner while taking outer):
//   _table_mu  ->  f->done_mu
//   _run_queue.mu  (taken independently, never while holding _table_mu)
//
// API (called from generated Tin IR):
//   _tin_fiber_spawn(coro_hdl)   -> pid   push fiber onto run queue
//   _tin_fiber_join(pid, hdl)            park calling fiber until target done
//   _tin_fiber_yield_coro(hdl)           mark RUNNABLE so worker re-enqueues
//   _tin_fiber_complete(hdl, result)     mark done, wake waiters
//   _tin_fiber_run()                     drain run queue; join all workers
//   _tin_fiber_init()                    spawn worker threads

#include "runtime.h"
#include "fiber.h"
#include "timer.h"
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <sched.h>
#include <pthread.h>
#include <stdatomic.h>

// Defined in defer.c; declared here so the worker loop can save/restore the
// per-fiber defer chain around _coro_resume.
extern __thread TinDeferEntry *_tin_defer_chain;

// -------------------------------------------------------------------
// Fiber status
// -------------------------------------------------------------------

typedef enum {
    FIBER_RUNNABLE = 0,
    FIBER_RUNNING  = 1,
    FIBER_BLOCKED  = 2,
    FIBER_DONE     = 3
} FiberStatus;

// -------------------------------------------------------------------
// TinFiber - heap-allocated so embedded mutexes/condvars never move
// -------------------------------------------------------------------

#define FIBER_MAX_WAITERS 8

typedef struct {
    int64_t      pid;
    void        *hdl;          // LLVM coroutine handle
    FiberStatus  status;
    void        *result;       // heap-allocated result (set on FIBER_DONE)
    pthread_mutex_t done_mu;
    pthread_cond_t  done_cv;
    // Fibers waiting for this one to finish (protected by _table_mu).
    int64_t  waiters[FIBER_MAX_WAITERS];
    int      waiter_cnt;
    // pending_wakeup: set by _tin_fiber_unpark when the fiber is still RUNNING
    // (its coro.suspend hasn't fired yet).  The worker loop checks and clears
    // this flag after _coro_resume returns; if set, it re-enqueues instead of
    // blocking so no double-resume occurs.
    // Also set by _fire_done_waiters when a waiter has pending_join set.
    // Protected by _table_mu.
    int      pending_wakeup;
    // pending_join: set by _tin_fiber_join on the fiber's own OS thread before
    // the coroutine yields; read by the worker loop after _coro_resume returns.
    // Sequential execution on the same thread guarantees visibility without a
    // lock.  _fire_done_waiters reads it under _table_mu+state_lock, which is
    // also safe.  The worker loop now checks it inside state_lock (state_lock
    // is acquired first; _table_mu is NOT taken for this check).
    int      pending_join;
    // pending_park: set by _tin_fiber_park (called from async I/O / timer C
    // helpers) to signal the worker loop.  Same deferred-BLOCKED pattern as
    // pending_join: the fiber sets pending_park, continues to its next yield,
    // and the worker blocks it after _coro_resume returns.  Without this,
    // the wakeup can fire between _tin_fiber_park and the yield, see BLOCKED,
    // and enqueue a concurrent resume - causing a double-resume race.
    // Protected by _table_mu.
    int      pending_park;
    // If the fiber panicked and the panic was caught by the worker loop,
    // this field holds the message as an ARC-managed buffer (via _tin_rc_alloc)
    // so it can be safely wrapped in a TinString by the awaiting fiber.
    // NULL means the fiber completed normally.
    // Freed via _tin_release() in fiber cleanup.
    char    *panic_msg;
    // Set to 1 when _tin_fiber_get_panic_msg reads a non-NULL panic_msg.
    // Used at shutdown to detect fire-and-forget panics nobody awaited.
    int      panic_checked;
    // Set to 1 by _tin_fiber_set_direct_recv when a channel sender delivers data
    // directly to this fiber's recv `out` buffer (bypassing the ring buffer).
    // The worker loop propagates this to _direct_recv_flag TLS before resuming,
    // allowing recv_direct's fast path to return 0 without acquiring the mutex.
    // Visibility: written by sender before _tin_fiber_unpark_fib (_fib_unlock
    // provides release); read by worker after _fib_lock (acquire), safe.
    int      direct_recv_done;
    // Set by _tin_fiber_spawn when this fiber spawns a child while running on a
    // worker.  Cleared by the worker loop at the start of each resume and again
    // when the yield path reads it.  Causes the first post-spawn yield to go to
    // the global queue so the spawned child (at the LIFO bottom of the local deque)
    // is not starved by a spawner that loops without parking.
    int      spawned_child;
    // Set by _tin_fiber_mark_handoff_yield when a send did direct delivery to a
    // recv_direct receiver (returning 2).  The yield path checks this and puts
    // the sender into LQ instead of selfnext so the freshly-unparked receiver
    // (already in runnext) runs immediately without contention.
    int      handoff_yield;
    // Per-fiber recv hint: set by _tin_set_recv_hint (called from genDirectChanRecv
    // entry) to record which channel+out-buffer this fiber will next receive from.
    // _tin_prepark_next_recv reads these to pre-register the fiber in that channel's
    // recv_wq at handoff yield time so the worker goes directly to BLOCKED.
    // Per-fiber (not TLS) so context switches don't cross-contaminate hints.
    void    *recv_hint_ch;
    void    *recv_hint_out;
    // Advisory pre-registration: set by _tin_prepark_next_recv when the fiber is
    // registered in a channel's recv_wq without calling _tin_fiber_park.  The fiber
    // goes to LQ via handoff_yield and uses this to skip re-registration in
    // recv_direct if data wasn't delivered before the LQ pop.  Cleared on recv
    // fast-path, on re-park, and on fiber completion (to remove stale entry).
    void    *preregistered_ch;
    // Per-fiber spinlock: protects status, pending_park, and pending_wakeup
    // in the park/unpark hot path.  Replacing the global _table_mu for these
    // transitions eliminates cross-core cache-line bouncing between workers
    // when each worker is running a different fiber (e.g. TINMAXPROCS=2).
    // Lock ordering: _table_mu (outer) -> state_lock (inner) - never reverse.
    _Atomic(uint32_t) state_lock;
    // Set by _tin_fiber_join_any when a fiber is waiting for any of N targets.
    // _fire_done_waiters checks this to do a CAS-based single-winner wakeup.
    // Protected by _table_mu.
    struct TinAnyWaiter *any_waiter;
    // Per-fiber defer chain: saves and restores _tin_defer_chain around each
    // _coro_resume so that a fiber's deferred functions are only visible while
    // that fiber is executing.  Without this, a second fiber running on the same
    // worker thread after a yield would inherit the first fiber's defer chain,
    // causing the second fiber's panic to accidentally consume the first fiber's
    // recover() entry.
    TinDeferEntry *saved_defer_chain;
} TinFiber;

// TinAnyWaiter - shared state for _tin_fiber_join_any.
// Stack-allocated in _tin_fiber_join_any (safe: the frame lives until after
// the fiber is unparked and the any_waiter pointer is cleared on all targets).
typedef struct TinAnyWaiter {
    int64_t          waiter_pid;
    _Atomic(int32_t) fired;       // CAS 0->1 by the winning target fiber
    int64_t          result_idx;  // index that completed; set before fired=1
    int64_t         *pids;        // the watched pid array (for cleanup)
    int64_t          n;           // length of pids
} TinAnyWaiter;

#define FIBER_DEFAULT_MAX (1 << 20)  // 1M

static TinFiber  **_fibers    = NULL;
static int64_t     _fiber_cap = 0;
static int64_t     _fiber_cnt = 1;   // next pid; 0 reserved
static int64_t     _fiber_max = 0;
static pthread_mutex_t _table_mu = PTHREAD_MUTEX_INITIALIZER;

// Set to 1 (atomically) whenever a fiber stores a panic_msg that has not yet
// been read by _tin_fiber_get_panic_msg / _tin_fiber_check_panic.
// Gives _tin_fiber_check_panic an O(1) fast-path so the loop-edge check is
// a single atomic load in the common (no panic) case.
_Atomic int _has_unhandled_panics = 0;


// -------------------------------------------------------------------
// Run queue - mutex + condvar FIFO ring buffer
// -------------------------------------------------------------------

typedef struct { void *hdl; int64_t pid; } TinRunnable;

#define RQ_DEFAULT_MAX (1 << 20)  // 1M

typedef struct {
    pthread_mutex_t mu;
    pthread_cond_t  not_empty;
    TinRunnable    *buf;
    int64_t         cap, head, tail, count;
    int             shutdown;
} TinRunQueue;

static TinRunQueue _run_queue;
static int64_t     _rq_max = 0;

static void _rq_init(void) {
    pthread_mutex_init(&_run_queue.mu, NULL);
    pthread_cond_init(&_run_queue.not_empty, NULL);
    _run_queue.cap  = 1024;
    _run_queue.buf  = (TinRunnable *)calloc((size_t)_run_queue.cap, sizeof(TinRunnable));
    _run_queue.head = _run_queue.tail = _run_queue.count = 0;
    _run_queue.shutdown = 0;

    const char *env = getenv("TINMAXRUNNABLES");
    // 0 = unlimited (old behaviour: grow forever without panicking).
    _rq_max = (env && *env) ? (int64_t)atoi(env) : RQ_DEFAULT_MAX;
    if (_rq_max < 0) _rq_max = RQ_DEFAULT_MAX;
}

// Declared here so _rq_push can check it; defined after _rq_pop.
static atomic_int _spinning_workers;

static void _rq_push(TinRunnable r) {
    pthread_mutex_lock(&_run_queue.mu);
    if (_run_queue.count == _run_queue.cap) {
        if (_rq_max > 0 && _run_queue.cap >= _rq_max) {
            pthread_mutex_unlock(&_run_queue.mu);
            _tin_panic("run queue overflow: too many runnable fibers - raise TINMAXRUNNABLES");
            return;
        }
        int64_t newcap = _run_queue.cap * 2;
        if (_rq_max > 0 && newcap > _rq_max) newcap = _rq_max;
        TinRunnable *nb = (TinRunnable *)malloc((size_t)newcap * sizeof(TinRunnable));
        if (!nb) {
            pthread_mutex_unlock(&_run_queue.mu);
            _tin_panic("run queue OOM");
            return;
        }
        // Copy linearised.
        for (int64_t i = 0; i < _run_queue.count; i++)
            nb[i] = _run_queue.buf[(_run_queue.head + i) % _run_queue.cap];
        free(_run_queue.buf);
        _run_queue.buf  = nb;
        _run_queue.head = 0;
        _run_queue.tail = _run_queue.count;
        _run_queue.cap  = newcap;
    }
    _run_queue.buf[_run_queue.tail] = r;
    _run_queue.tail = (_run_queue.tail + 1) % _run_queue.cap;
    _run_queue.count++;
    // Skip the condvar signal when a spinner is already polling the queue.
    // The spinner will find this item in its trylock loop without an OS wake-up.
    // Signal only when all workers are sleeping.
    if (atomic_load_explicit(&_spinning_workers, memory_order_relaxed) == 0)
        pthread_cond_signal(&_run_queue.not_empty);
    pthread_mutex_unlock(&_run_queue.mu);
}

// Adaptive spin: at most one worker spins at a time (Go-style single spinner).
// More than one spinner causes trylock contention that is worse than sleeping.
#define RQ_SPIN_ITERS 500

// CPU-level spin hint (avoids cache-line thrashing on the mutex).
static inline void _cpu_relax(void) {
#if defined(__x86_64__) || defined(__i386__)
    __asm__ volatile("pause" ::: "memory");
#elif defined(__aarch64__) || defined(__arm__)
    __asm__ volatile("yield" ::: "memory");
#else
    sched_yield();
#endif
}

// Pop one runnable from the queue.
// One worker at a time is allowed to spin briefly before sleeping so that
// short idle gaps (back-to-back spawns) skip the OS wake-up latency (~3 us).
// All other idle workers block on the condvar immediately.
// Returns a sentinel with pid=-1 on shutdown.
static TinRunnable _rq_pop(void) {
    // Compete to become the single spinning worker.
    int expected = 0;
    int became_spinner = atomic_compare_exchange_strong_explicit(
        &_spinning_workers, &expected, 1,
        memory_order_acquire, memory_order_relaxed);

    if (became_spinner) {
        for (int spin = 0; spin < RQ_SPIN_ITERS; spin++) {
            if (pthread_mutex_trylock(&_run_queue.mu) == 0) {
                if (_run_queue.shutdown) {
                    atomic_fetch_sub_explicit(&_spinning_workers, 1, memory_order_release);
                    pthread_mutex_unlock(&_run_queue.mu);
                    return (TinRunnable){ NULL, -1 };
                }
                if (_run_queue.count > 0) {
                    TinRunnable r = _run_queue.buf[_run_queue.head];
                    _run_queue.head = (_run_queue.head + 1) % _run_queue.cap;
                    _run_queue.count--;
                    atomic_fetch_sub_explicit(&_spinning_workers, 1, memory_order_release);
                    pthread_mutex_unlock(&_run_queue.mu);
                    return r;
                }
                pthread_mutex_unlock(&_run_queue.mu);
            }
            _cpu_relax();
        }
        atomic_fetch_sub_explicit(&_spinning_workers, 1, memory_order_release);
    }

    // No work found during spin (or we weren't the spinner): block on condvar.
    pthread_mutex_lock(&_run_queue.mu);
    while (_run_queue.count == 0 && !_run_queue.shutdown)
        pthread_cond_wait(&_run_queue.not_empty, &_run_queue.mu);
    if (_run_queue.shutdown) {
        pthread_mutex_unlock(&_run_queue.mu);
        return (TinRunnable){ NULL, -1 };
    }
    TinRunnable r = _run_queue.buf[_run_queue.head];
    _run_queue.head = (_run_queue.head + 1) % _run_queue.cap;
    _run_queue.count--;
    pthread_mutex_unlock(&_run_queue.mu);
    return r;
}

// -------------------------------------------------------------------
// Per-worker local work-stealing deque (bounded Chase-Lev deque).
//
// Each worker owns one deque.  Unpark calls on a worker push directly
// to that deque (no mutex).  When idle, a worker steals from peers
// before blocking on the global queue.  This eliminates global-queue
// mutex contention in high-concurrency workloads (e.g. MPMC, jitter).
//
// Owner   pushes/pops from bottom (LIFO for cache locality).
// Thieves steal            from top  (FIFO to avoid contention).
// -------------------------------------------------------------------

#define WORKER_LQ_SIZE  64                    // must be power of 2
#define WORKER_LQ_MASK  (WORKER_LQ_SIZE - 1)
#define WORKER_MAX      256                   // max TINMAXPROCS supported

typedef struct {
    _Atomic(int64_t) top;       // steal pointer (thieves read/CAS)
    char _pad0[56];             // keep top on its own cache line
    _Atomic(int64_t) bottom;    // owner pointer (owner reads/writes)
    char _pad1[56];             // keep bottom on its own cache line
    TinRunnable      buf[WORKER_LQ_SIZE];
    TinFiber        *fibs[WORKER_LQ_SIZE];
} WorkerLocalQueue;

static WorkerLocalQueue _worker_lqs[WORKER_MAX];
static _Atomic(int)     _worker_idx_counter = 0;

// Forward-declared here so _lq_push/_lq_pop/_worker_push can use them;
// definitions are in the per-worker TLS section below.
static __thread int      _is_worker          = 0;
static __thread int      _worker_lq_idx      = -1;
// Single-slot priority queue checked before the Chase-Lev deque.
// No atomic ops needed - only the owning worker reads/writes these.
static __thread int64_t  _worker_runnext_pid = -1;
static __thread void    *_worker_runnext_hdl = NULL;
static __thread TinFiber *_worker_runnext_fib = NULL;
// _worker_selfnext: TLS slot for the currently-running fiber to re-enqueue
// itself on an explicit yield.  Separate from runnext so that an unparked
// fiber in runnext and the yielding fiber in selfnext can coexist without
// either being displaced to the LQ (and its seq_cst fence).
static __thread int64_t   _worker_selfnext_pid = -1;
static __thread void     *_worker_selfnext_hdl = NULL;
static __thread TinFiber *_worker_selfnext_fib = NULL;

// Push to calling worker's local deque; spills to global queue when full.
static void _lq_push(TinRunnable r, TinFiber *f) {
    int idx = _worker_lq_idx;
    if (idx < 0 || idx >= WORKER_MAX) { _rq_push(r); return; }
    WorkerLocalQueue *q = &_worker_lqs[idx];
    int64_t b = atomic_load_explicit(&q->bottom, memory_order_relaxed);
    int64_t t = atomic_load_explicit(&q->top,    memory_order_acquire);
    if (b - t >= WORKER_LQ_SIZE) { _rq_push(r); return; }
    q->buf [b & WORKER_LQ_MASK] = r;
    q->fibs[b & WORKER_LQ_MASK] = f;
    atomic_thread_fence(memory_order_release);
    atomic_store_explicit(&q->bottom, b + 1, memory_order_relaxed);
}

// Pop from calling worker's local deque (LIFO). Returns 1 on success.
static int _lq_pop(TinRunnable *r, TinFiber **f) {
    int idx = _worker_lq_idx;
    if (idx < 0 || idx >= WORKER_MAX) return 0;
    WorkerLocalQueue *q = &_worker_lqs[idx];
    int64_t b = atomic_load_explicit(&q->bottom, memory_order_relaxed) - 1;
    atomic_store_explicit(&q->bottom, b, memory_order_relaxed);
    atomic_thread_fence(memory_order_seq_cst);
    int64_t t = atomic_load_explicit(&q->top, memory_order_relaxed);
    if (t > b) {
        atomic_store_explicit(&q->bottom, b + 1, memory_order_relaxed);
        return 0;
    }
    *r = q->buf [b & WORKER_LQ_MASK];
    *f = q->fibs[b & WORKER_LQ_MASK];
    if (t == b) {
        if (!atomic_compare_exchange_strong_explicit(&q->top, &t, t + 1,
                memory_order_seq_cst, memory_order_relaxed)) {
            atomic_store_explicit(&q->bottom, b + 1, memory_order_relaxed);
            return 0;
        }
        atomic_store_explicit(&q->bottom, b + 1, memory_order_relaxed);
    }
    return 1;
}

// Steal from victim worker's deque (FIFO). Returns 1 on success.
static int _lq_steal(int victim, TinRunnable *r, TinFiber **f) {
    if (victim < 0 || victim >= WORKER_MAX) return 0;
    WorkerLocalQueue *q = &_worker_lqs[victim];
    int64_t t = atomic_load_explicit(&q->top,    memory_order_acquire);
    atomic_thread_fence(memory_order_seq_cst);
    int64_t b = atomic_load_explicit(&q->bottom, memory_order_acquire);
    if (t >= b) return 0;
    *r = q->buf [t & WORKER_LQ_MASK];
    *f = q->fibs[t & WORKER_LQ_MASK];
    if (!atomic_compare_exchange_strong_explicit(&q->top, &t, t + 1,
            memory_order_seq_cst, memory_order_relaxed)) {
        return 0;
    }
    return 1;
}

// Push (r, f) to runnext when on a worker; fall through to LQ if occupied.
// Runnext is a priority slot for unparked fibers.  When it is already occupied
// the new fiber goes to LQ - no eviction.  Yielding fibers use _worker_selfnext
// (a separate TLS slot) so they can self-loop without touching the LQ at all.
static inline void _worker_push(TinRunnable r, TinFiber *f) {
    if (!_is_worker || _worker_lq_idx < 0) { _rq_push(r); return; }
    if (_worker_runnext_pid < 0) {
        _worker_runnext_pid = r.pid;
        _worker_runnext_hdl = r.hdl;
        _worker_runnext_fib = f;
    } else {
        _lq_push(r, f);
    }
}

// Per-worker FIFO handoff queue: holds fibers that yielded via handoff_yield
// (direct-delivery sends returning 2).  Checked between runnext and the
// Chase-Lev LQ so that upstream senders run before downstream relay fibers.
// FIFO ordering means the fiber that did handoff_yield first (the upstream
// sender, e.g. main) is consumed before later relay fibers, so data is
// present in the ring buffer when a relay calls recv_direct - eliminating
// the "LQ pop -> empty recv -> park" waste cycle in pipeline workloads.
// No atomic ops needed: only the owning worker reads/writes this queue.
#define HANDOFF_Q_SIZE  64
#define HANDOFF_Q_MASK  (HANDOFF_Q_SIZE - 1)
typedef struct {
    TinRunnable  r[HANDOFF_Q_SIZE];
    TinFiber    *f[HANDOFF_Q_SIZE];
    int          head, tail;
} HandoffQueue;
static __thread HandoffQueue _handoff_q;

static inline void _handoff_push(TinRunnable r, TinFiber *f) {
    int t    = _handoff_q.tail;
    int next = (t + 1) & HANDOFF_Q_MASK;
    if (__builtin_expect(next == _handoff_q.head, 0)) {
        _lq_push(r, f);  // overflow: fall back to LIFO LQ
        return;
    }
    _handoff_q.r[t] = r;
    _handoff_q.f[t] = f;
    _handoff_q.tail = next;
}

static inline int _handoff_pop(TinRunnable *r, TinFiber **f) {
    int h = _handoff_q.head;
    if (h == _handoff_q.tail) return 0;
    *r = _handoff_q.r[h];
    *f = _handoff_q.f[h];
    _handoff_q.head = (h + 1) & HANDOFF_Q_MASK;
    return 1;
}

// Cold slow path: push a yielding fiber to the global queue so that a
// freshly spawned fire-and-forget child (sitting at the LIFO bottom of
// the local deque) can be picked up on the next pop.  Marked noinline+cold
// so the compiler keeps _rq_push (a mutex operation) out of the hot
// FIBER_RUNNING yield path, preserving register allocation and inlining
// for _lq_push there.
__attribute__((noinline, cold))
static void _yield_to_global(TinRunnable r) {
    _rq_push(r);
}

// -------------------------------------------------------------------
// LLVM coroutine resume/destroy
// -------------------------------------------------------------------

static inline void _coro_resume(void *hdl) {
    typedef void (*CoroFn)(void *);
    ((CoroFn *)hdl)[0](hdl);
}

static inline void _coro_destroy(void *hdl) {
    typedef void (*CoroFn)(void *);
    ((CoroFn *)hdl)[1](hdl);
}

// -------------------------------------------------------------------
// Per-worker thread-locals
// -------------------------------------------------------------------

__thread int64_t  _current_pid    = -1;
__thread void     *_current_hdl   = NULL;  // coro handle of running fiber
__thread TinFiber *_current_fib   = NULL;  // TinFiber* of running fiber (no table lock needed)
// Set to 1 by the worker loop when the fiber it's about to resume had a
// direct recv delivery (sender wrote to the receiver's out buffer directly).
// Checked by _tin_channel_recv_direct's fast path to skip mutex+dequeue.
__thread int       _direct_recv_flag = 0;
static __thread int       _coro_done     = 0;
static __thread void     *_coro_result   = NULL;
// Inline-drive result mode: set to 1 by genInlineAsyncDrive's mode_begin call
// (inside an outer coroutine body) so the inner $coro's emitCoroComplete skips
// the malloc and stores the result in the TLS buffer instead.  Reset to 0 by
// the worker loop before each fiber resume so that TLS mode cannot leak across
// a fiber context switch to another fiber's resume on the same OS thread.
static __thread int       _inline_result_mode = 0;

// Called by the I/O layer to access current fiber pid.
int64_t _tin_current_pid(void) { return _current_pid; }

// Called by channel_arc.c (and other C helpers) to get the coro handle of the
// currently-running fiber without acquiring _table_mu.  Returns NULL when called
// from a non-worker context (I/O thread, timer thread, main thread).
// (channel_arc.c inlines this directly via extern __thread _current_hdl.)
void *_tin_current_coro_hdl(void) { return _current_hdl; }

// Returns the TinFiber* of the currently-running fiber as an opaque void*.
// Safe to store in channel waiter lists because the fiber is alive for the
// duration of the park; it cannot be freed until after _tin_fiber_unpark_fib
// transitions it out of FIBER_BLOCKED.
void *_tin_current_fib(void) { return (void *)_current_fib; }

// Mark that a channel sender delivered data directly to this fiber's recv
// out buffer.  Called by send_blocking before _tin_fiber_unpark_fib so that
// the release in _fib_unlock (inside unpark) makes the write visible to the
// worker's subsequent _fib_lock (acquire) when the fiber is resumed.
void _tin_fiber_set_direct_recv(void *fib) {
    if (fib) ((TinFiber *)fib)->direct_recv_done = 1;
}

void _tin_fiber_mark_handoff_yield(void) {
    if (_current_fib) _current_fib->handoff_yield = 1;
}

void _tin_set_recv_hint(void *ch, void *out) {
    if (_current_fib) { _current_fib->recv_hint_ch = ch; _current_fib->recv_hint_out = out; }
}
void *_tin_get_recv_hint_ch(void)  { return _current_fib ? _current_fib->recv_hint_ch  : NULL; }
void *_tin_get_recv_hint_out(void) { return _current_fib ? _current_fib->recv_hint_out : NULL; }
void  _tin_clear_recv_hint(void) {
    if (_current_fib) { _current_fib->recv_hint_ch = NULL; _current_fib->recv_hint_out = NULL; }
}

void  _tin_set_preregistered_ch(void *ch) {
    if (_current_fib) _current_fib->preregistered_ch = ch;
}
void *_tin_get_preregistered_ch(void) {
    return _current_fib ? _current_fib->preregistered_ch : NULL;
}
void  _tin_clear_preregistered_ch(void) {
    if (_current_fib) _current_fib->preregistered_ch = NULL;
}

// -------------------------------------------------------------------
// Worker threads
// -------------------------------------------------------------------

static pthread_t *_workers    = NULL;
static int        _worker_cnt = 0;

// Returns 1 if the calling thread is one of the M:N worker threads.
// Used to decide whether to access _current_pid (a __thread variable) inside
// _tin_fiber_join: accessing a __thread variable for the first time on a thread
// causes macOS dyld to lazily allocate TLS storage for that thread.  Worker
// threads clean up their TLS when they exit (pthread_join); the main thread
// does not, producing a spurious LSAN report.  Avoiding TLS access on
// non-worker threads eliminates this leak without changing the fast path.
static int _is_worker_thread(void) {
    pthread_t me = pthread_self();
    for (int i = 0; i < _worker_cnt; i++) {
        if (pthread_equal(_workers[i], me)) return 1;
    }
    return 0;
}

// Per-fiber spinlock helpers for the park/unpark hot path.
// Only used for status, pending_park, and pending_wakeup transitions.
static inline void _fib_lock(TinFiber *f) {
    uint32_t z = 0;
    while (!atomic_compare_exchange_weak_explicit(&f->state_lock, &z, 1u,
               memory_order_acquire, memory_order_relaxed)) {
        z = 0;
        _cpu_relax();
    }
}
static inline void _fib_unlock(TinFiber *f) {
    atomic_store_explicit(&f->state_lock, 0u, memory_order_release);
}

// Like _tin_fiber_unpark_hdl but uses a pre-captured TinFiber* to skip the
// Clear advisory pre-registration state after delivery (fast path).
// Clears preregistered_ch and pending_wakeup under _fib_lock so the stale
// advisory pending_wakeup (set by _tin_fiber_unpark_fib for RUNNABLE fibers)
// doesn't cause a spurious re-queue on the next park.
void _tin_clear_advisory_state(void) {
    TinFiber *f = _current_fib;
    if (!f || !f->preregistered_ch) return;
    f->preregistered_ch = NULL;
    _fib_lock(f);
    f->pending_wakeup = 0;
    _fib_unlock(f);
}

// _table_mu global lock entirely.  Only the per-fiber state_lock is needed.
void _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl) {
    TinFiber *f = (TinFiber *)fib;
    if (!f) return;
    _fib_lock(f);
    if (f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        _fib_unlock(f);
        _worker_push((TinRunnable){ hdl, pid }, f);
        return;
    }
    if (f->status == FIBER_RUNNING) {
        f->pending_wakeup = 1;
        _fib_unlock(f);
        return;
    }
    // Advisory pre-registration: fiber is RUNNABLE (in LQ) but still in recv_wq.
    // Set pending_wakeup so that if the fiber misses direct_recv_done and parks in
    // recv_direct, the worker re-queues it (pending_park + pending_wakeup) instead
    // of blocking, giving it a second chance to see the delivery.
    if (f->status == FIBER_RUNNABLE && f->preregistered_ch != NULL) {
        f->pending_wakeup = 1;
        _fib_unlock(f);
        return;
    }
    _fib_unlock(f);
}

// _enqueue_waiter wakes wpid (already locked under _table_mu).
// Caller must hold w's state_lock; this function releases it.
static void _enqueue_waiter(int64_t wpid, TinFiber *w) {
    if (w->status == FIBER_BLOCKED) {
        w->status = FIBER_RUNNABLE;
        void *whdl = w->hdl;
        _fib_unlock(w);
        _worker_push((TinRunnable){ whdl, wpid }, w);
    } else if (w->pending_join) {
        w->pending_wakeup = 1;
        _fib_unlock(w);
    } else {
        _fib_unlock(w);
    }
}

static void _fire_done_waiters(TinFiber *f) {
    // Must be called with _table_mu held.
    for (int i = 0; i < f->waiter_cnt; i++) {
        int64_t wpid = f->waiters[i];
        if (wpid <= 0 || wpid >= _fiber_cnt || !_fibers[wpid]) continue;
        TinFiber *w = _fibers[wpid];
        _fib_lock(w);

        // If this waiter is part of a join_any group, attempt CAS to win.
        TinAnyWaiter *aw = w->any_waiter;
        if (aw) {
            // Find which index f->pid corresponds to in aw->pids.
            int64_t idx = -1;
            for (int64_t j = 0; j < aw->n; j++) {
                if (aw->pids[j] == f->pid) { idx = j; break; }
            }
            int32_t expected = 0;
            if (idx >= 0 && atomic_compare_exchange_strong(&aw->fired, &expected, 1)) {
                // We won: record the result index and wake the waiter.
                aw->result_idx = idx;
                // Clear any_waiter on all other targets to avoid dangling pointer.
                for (int64_t j = 0; j < aw->n; j++) {
                    if (aw->pids[j] != f->pid && aw->pids[j] > 0 &&
                        aw->pids[j] < _fiber_cnt && _fibers[aw->pids[j]]) {
                        _fibers[aw->pids[j]]->any_waiter = NULL;
                    }
                }
                w->any_waiter = NULL;
                _enqueue_waiter(wpid, w); // releases w's state_lock
            } else {
                // Another target already won; just clear any_waiter on this target.
                f->any_waiter = NULL;
                _fib_unlock(w);
            }
            continue;
        }

        _enqueue_waiter(wpid, w); // releases w's state_lock
    }
    f->waiter_cnt = 0;
}

// Defined in stdlib/sync/channel_arc.c.  Weak so programs without channels link.
__attribute__((weak)) void _tin_chan_remove_recv_waiter(void *ch_ptr, int64_t pid) {
    (void)ch_ptr; (void)pid;
}

// Forward declarations for per-thread coro frame pool (defined after _worker_thread).
#define CORO_POOL_MAX 16
static _Thread_local void *_coro_pool[CORO_POOL_MAX];
static _Thread_local int   _coro_pool_cnt = 0;

static void *_worker_thread(void *_) {
    (void)_;
    // Assign this worker's local deque index before setting _is_worker so
    // _worker_push is valid as soon as the worker starts handling fibers.
    int lq = atomic_fetch_add_explicit(&_worker_idx_counter, 1, memory_order_relaxed);
    if (lq < WORKER_MAX) {
        _worker_lq_idx = lq;
        atomic_store_explicit(&_worker_lqs[lq].top,    0, memory_order_relaxed);
        atomic_store_explicit(&_worker_lqs[lq].bottom, 0, memory_order_relaxed);
    }
    _is_worker       = 1;
    _tin_defer_chain = NULL;
    while (1) {
        // 0. Selfnext (TLS): the currently-running fiber re-enqueued itself via
        //    an explicit yield.  Highest priority so looping fibers cycle back
        //    without any atomic ops (no LQ seq_cst fence, no global mutex).
        // 1. Runnext (TLS): a fiber just unparked by channel/join/spawn.
        // 2. Handoff queue (TLS FIFO): fibers that yielded via handoff_yield.
        //    FIFO ordering ensures upstream senders run before downstream relay
        //    fibers, so relays find data in the ring buffer on their next recv.
        // 3. Local deque (lock-free Chase-Lev, LIFO for cache locality).
        // 4. Work-steal from a peer's deque (lock-free).
        // 5. Global run queue (mutex + condvar, blocks when truly idle).
        TinRunnable r;
        TinFiber   *f = NULL;
        if (_worker_selfnext_pid >= 0) {
            r.pid = _worker_selfnext_pid;
            r.hdl = _worker_selfnext_hdl;
            f     = _worker_selfnext_fib;
            _worker_selfnext_pid = -1;
            _worker_selfnext_hdl = NULL;
            _worker_selfnext_fib = NULL;
        } else if (_worker_runnext_pid >= 0) {
            r.pid = _worker_runnext_pid;
            r.hdl = _worker_runnext_hdl;
            f     = _worker_runnext_fib;
            _worker_runnext_pid = -1;
            _worker_runnext_hdl = NULL;
            _worker_runnext_fib = NULL;
        } else if (_handoff_pop(&r, &f)) {
            // handoff queue: f is already set, nothing else to do here
        } else if (!_lq_pop(&r, &f)) {
            int stolen = 0;
            int my = _worker_lq_idx;
            if (my >= 0) {
                for (int s = 1; s < _worker_cnt && !stolen; s++)
                    stolen = _lq_steal((my + s) % _worker_cnt, &r, &f);
            }
            if (!stolen) {
                r = _rq_pop();
                if (r.pid < 0) break;  // shutdown sentinel
                f = NULL;
            }
        }

        if (!f) {
            // Global-queue path: no pre-captured TinFiber*, look it up.
            pthread_mutex_lock(&_table_mu);
            if (r.pid >= _fiber_cnt || !_fibers[r.pid]) {
                pthread_mutex_unlock(&_table_mu);
                continue;
            }
            f = _fibers[r.pid];
            pthread_mutex_unlock(&_table_mu);
        }
        _fib_lock(f);
        f->status = FIBER_RUNNING;
        _fib_unlock(f);

        _current_pid         = r.pid;
        _current_hdl         = r.hdl;
        _current_fib         = f;
        // Propagate direct-recv delivery flag from the fiber struct to TLS.
        // The _fib_lock above provides the acquire barrier that makes the
        // sender's _tin_fiber_set_direct_recv write (done before _fib_unlock
        // in _tin_fiber_unpark_fib) visible here.
        _direct_recv_flag    = f->direct_recv_done;
        if (f->direct_recv_done) f->direct_recv_done = 0;
        _coro_done           = 0;
        _coro_result         = NULL;
        _inline_result_mode  = 0;  // reset so inline-drive TLS cannot leak across fibers
        f->spawned_child     = 0;  // reset: spawn-displacement only applies within one resume
        f->handoff_yield     = 0;  // reset: handoff only applies to the current yield

        // Restore this fiber's defer chain so its deferred functions are only
        // visible while the fiber is executing.  Without save/restore, a second
        // fiber running on the same worker would inherit the first fiber's chain,
        // letting the second fiber's panic accidentally consume the first fiber's
        // recover() entry.
        _tin_defer_chain = f->saved_defer_chain;

        _tin_panic_catch_begin();
        _coro_resume(r.hdl);
        const char *panicked = _tin_panic_catch_end();

        // Save the (possibly updated) defer chain back into the fiber so that
        // the next resume on any worker thread sees the correct chain head.
        f->saved_defer_chain = _tin_defer_chain;
        _tin_defer_chain     = NULL;  // don't let this fiber's chain bleed into the next

        if (panicked) {
            // Fiber panicked.  Destroy the coro frame.
            // The cleanup path (emitCoroEpilogue) calls llvm.coro.free which
            // frees the heap-allocated frame inside _coro_destroy.  Spawned
            // fiber frames cannot be stack-allocated by coro-elide (the hdl
            // escapes to _tin_fiber_spawn in the caller), so _coro_destroy
            // always runs the full cleanup here.
            // Clear direct-recv flag: if the fiber panicked before consuming
            // it, the next fiber on this worker must not inherit a stale flag.
            _direct_recv_flag = 0;
            // Free any intermediate result that may have been stored by
            // emitCoroComplete before the panic path took over (e.g., genBuiltinPanic
            // calls emitCoroComplete then the runtime re-raises the panic).
            if (_coro_result) {
                _tin_inline_result_free(_coro_result);
                _coro_result = NULL;
            }
            // LLVM's coro-split pass generates empty destroy functions for
            // trivially-destructible C/Tin coroutines (no C++ dtors), so
            // _coro_destroy is a no-op and the frame is never freed via the
            // cleanup path.  Free it explicitly here.
            _tin_coro_free(r.hdl);
            pthread_mutex_lock(&_table_mu);
            // Allocate with _tin_rc_alloc so the message has a proper ARC header.
            // This lets the awaiting fiber wrap it in a TinString and release it
            // normally via _tin_release(). See _tin_fiber_get_panic_msg.
            size_t plen = strlen(panicked);
            char *pmsg_buf = (char *)_tin_rc_alloc((int64_t)(plen + 1));
            memcpy(pmsg_buf, panicked, plen + 1);
            f->panic_msg = pmsg_buf;
            atomic_store(&_has_unhandled_panics, 1);
            f->status    = FIBER_DONE;
            _fire_done_waiters(f);           // wake any fiber waiters
            pthread_mutex_lock(&f->done_mu);
            pthread_cond_broadcast(&f->done_cv);  // wake any OS-thread waiters
            pthread_mutex_unlock(&f->done_mu);
            pthread_mutex_unlock(&_table_mu);
            _current_pid = -1;
            // Unchecked panics from fire-and-forget fibers are detected at
            // shutdown by _tin_fiber_run (which calls _tin_panic if any fiber
            // has a panic_msg that was never read via _tin_fiber_get_panic_msg).
            continue;
        }

        if (_coro_done) {
            // Fiber completed: store result, wake waiters, signal done_cv.
            // LLVM's coro-split pass generates empty destroy functions for
            // trivially-destructible C/Tin coroutines, so _coro_destroy is a
            // no-op.  Free the frame explicitly instead of relying on the
            // cleanup path.
            _tin_coro_free(r.hdl);
            // Remove stale advisory recv_wq entry if the fiber completed without
            // consuming its pre-registration (e.g. last loop iteration exited
            // before reaching recv_direct).
            if (f->preregistered_ch) {
                _tin_chan_remove_recv_waiter(f->preregistered_ch, f->pid);
                f->preregistered_ch = NULL;
            }
            pthread_mutex_lock(&_table_mu);
            f->result = _coro_result;
            f->status = FIBER_DONE;
            _fire_done_waiters(f);
            pthread_mutex_lock(&f->done_mu);
            pthread_cond_broadcast(&f->done_cv);
            pthread_mutex_unlock(&f->done_mu);
            pthread_mutex_unlock(&_table_mu);
        } else {
            // Fiber yielded, parked, or joined.
            //
            // Hot path: use only the per-fiber state_lock.  All three flags
            // (pending_join, pending_park, pending_wakeup) are written while
            // holding state_lock (or on the same OS thread as the fiber body,
            // which provides sequential visibility before the yield).  _table_mu
            // is only needed for the slow join completion path, which acquires it
            // AFTER the flag check inside state_lock so the lock order
            // (state_lock first, _table_mu second) never reverses.
            _fib_lock(f);
            if (__builtin_expect(f->pending_join, 0)) {
                // Fiber called _tin_fiber_join.  Check pending_wakeup to see if
                // the target already completed before coro.suspend fired.
                // pending_wakeup is written by _fire_done_waiters under
                // _table_mu+state_lock; reading it here under state_lock is safe.
                f->pending_join = 0;
                int wake = f->pending_wakeup;
                if (wake) f->pending_wakeup = 0;
                if (wake) {
                    // Target completed before we suspended; re-queue now.
                    f->status = FIBER_RUNNABLE;
                    _fib_unlock(f);
                    _lq_push(r, f);
                } else {
                    // Target not yet done; block until _fire_done_waiters wakes us.
                    f->status = FIBER_BLOCKED;
                    _fib_unlock(f);
                }
            } else if (f->pending_park) {
                // Fiber called _tin_fiber_park (async I/O / timer / channel):
                // block unless a wakeup already arrived (pending_wakeup).
                f->pending_park = 0;
                if (f->pending_wakeup) {
                    // Wakeup arrived before coro.suspend; re-queue now.
                    f->pending_wakeup = 0;
                    f->status = FIBER_RUNNABLE;
                    _fib_unlock(f);
                    _lq_push(r, f);
                } else {
                    // No wakeup yet; block until _tin_fiber_unpark fires.
                    f->status = FIBER_BLOCKED;
                    _fib_unlock(f);
                }
            } else if (f->status == FIBER_RUNNING) {
                // Normal yield (_tin_fiber_yield_coro): re-enqueue.
                //
                // Fast path: put self in _worker_selfnext for zero-overhead
                // resume.  This is a separate TLS slot from runnext so that
                // unparked fibers (in runnext) and the yielding fiber (in
                // selfnext) can coexist without eviction or LQ touching.
                // The worker loop gives selfnext higher priority so looping
                // fibers cycle back immediately without any atomic ops.
                int had_spawn   = f->spawned_child;
                int had_handoff = f->handoff_yield;
                f->spawned_child = 0;
                f->handoff_yield = 0;
                f->status = FIBER_RUNNABLE;
                _fib_unlock(f);
                if (__builtin_expect(had_spawn, 0)) {
                    _yield_to_global(r);
                } else if (had_handoff) {
                    // Direct-delivery handoff: receiver is in runnext.
                    // Push sender to the FIFO handoff queue so upstream senders
                    // run before downstream relay fibers (FIFO ordering ensures
                    // the sender that pushed first is consumed first, making data
                    // available in the ring buffer before the relay tries recv).
                    _handoff_push(r, f);
                } else if (_worker_selfnext_pid < 0) {
                    _worker_selfnext_pid = r.pid;
                    _worker_selfnext_hdl = r.hdl;
                    _worker_selfnext_fib = f;
                } else {
                    _lq_push(r, f);
                }
            } else {
                // FIBER_RUNNABLE: waker already enqueued - don't push again.
                // FIBER_BLOCKED:  park already processed; waker will enqueue.
                _fib_unlock(f);
            }
        }
        _current_pid = -1;
        _current_hdl = NULL;
    }
    // Flush per-thread coro frame pool to avoid leaking frames at worker exit.
    for (int i = 0; i < _coro_pool_cnt; i++)
        free((char *)_coro_pool[i] - 8);
    _coro_pool_cnt = 0;
    return NULL;
}

// -------------------------------------------------------------------
// Public API
// -------------------------------------------------------------------

void _tin_fiber_init(void) {
    pthread_mutex_lock(&_table_mu);
    if (!_fibers) {
        const char *env = getenv("TINMAXFIBERS");
        _fiber_max = (env && *env) ? (int64_t)atoi(env) : FIBER_DEFAULT_MAX;
        if (_fiber_max <= 0) _fiber_max = FIBER_DEFAULT_MAX;

        _fiber_cap = 256;
        _fibers    = (TinFiber **)calloc((size_t)_fiber_cap, sizeof(TinFiber *));
        if (!_fibers) { fputs("tin: fiber table OOM\n", stderr); exit(1); }
        _fiber_cnt = 1;
    }
    pthread_mutex_unlock(&_table_mu);

    _rq_init();

    // Determine worker count from TINMAXPROCS env var.
    int nworkers = 0;
    const char *env = getenv("TINMAXPROCS");
    if (env && *env) nworkers = atoi(env);
    if (nworkers <= 0) {
#if defined(_SC_NPROCESSORS_ONLN)
        nworkers = (int)sysconf(_SC_NPROCESSORS_ONLN);
#endif
        if (nworkers <= 0) nworkers = 1;
    }

    _worker_cnt = nworkers;
    _workers    = (pthread_t *)malloc((size_t)nworkers * sizeof(pthread_t));
    if (!_workers) { fputs("tin: worker alloc OOM\n", stderr); exit(1); }

    for (int i = 0; i < nworkers; i++) {
        int rc = pthread_create(&_workers[i], NULL, _worker_thread, NULL);
        if (rc != 0) {
            fprintf(stderr, "tin: pthread_create worker %d: %d\n", i, rc);
            exit(1);
        }
    }

    // Start the async I/O thread.
    _tin_io_init();
    // Start the timer thread.
    _tin_timer_init();
}

int64_t _tin_fiber_spawn(void *hdl) {
    pthread_mutex_lock(&_table_mu);

    if (!_fibers) {
        // _tin_fiber_init was not called; bootstrap lazily (single-threaded context).
        const char *env = getenv("TINMAXFIBERS");
        _fiber_max = (env && *env) ? (int64_t)atoi(env) : FIBER_DEFAULT_MAX;
        if (_fiber_max <= 0) _fiber_max = FIBER_DEFAULT_MAX;
        _fiber_cap = 256;
        _fibers    = (TinFiber **)calloc((size_t)_fiber_cap, sizeof(TinFiber *));
        if (!_fibers) { fputs("tin: fiber table OOM\n", stderr); exit(1); }
        _fiber_cnt = 1;
    }
    if (_fiber_cnt >= _fiber_cap) {
        int64_t max = (_fiber_max > 0) ? _fiber_max : FIBER_DEFAULT_MAX;
        if (_fiber_cap >= max) {
            pthread_mutex_unlock(&_table_mu);
            _tin_panic("fiber limit reached: too many concurrent fibers - raise TINMAXFIBERS");
            return -1;
        }
        int64_t new_cap = _fiber_cap * 2;
        if (new_cap > max) new_cap = max;
        TinFiber **nf = (TinFiber **)realloc(_fibers, sizeof(TinFiber *) * (size_t)new_cap);
        if (!nf) {
            pthread_mutex_unlock(&_table_mu);
            _tin_panic("fiber table OOM");
            return -1;
        }
        memset(nf + _fiber_cap, 0, sizeof(TinFiber *) * (size_t)(new_cap - _fiber_cap));
        _fibers    = nf;
        _fiber_cap = new_cap;
    }

    TinFiber *f = (TinFiber *)calloc(1, sizeof(TinFiber));
    if (!f) { fputs("tin: fiber alloc OOM\n", stderr); exit(1); }

    int64_t pid = _fiber_cnt++;
    f->pid       = pid;
    f->hdl       = hdl;
    f->status    = FIBER_RUNNABLE;
    f->result    = NULL;
    f->waiter_cnt = 0;
    pthread_mutex_init(&f->done_mu, NULL);
    pthread_cond_init(&f->done_cv,  NULL);
    _fibers[pid] = f;
    pthread_mutex_unlock(&_table_mu);

    // When spawning from a worker, push to the local deque for cache locality.
    // Set _spawned_unresumed so the spawner's very next yield goes to the global
    // queue instead of the local deque; this ensures the spawned fiber (at the
    // LIFO bottom) gets a chance to run rather than being starved by the spawner.
    // Spawned fibers go directly to the LQ (bypassing runnext) so unpark-based
    // runnext hand-offs are not disturbed by rapid spawning.
    _lq_push((TinRunnable){ hdl, pid }, f);
    if (_is_worker && _current_fib) _current_fib->spawned_child = 1;
    return pid;
}

void _tin_fiber_complete(void *result) {
    // Free any previous result before overwriting.  This handles the case where
    // genBuiltinPanic emits emitCoroComplete (storing an intermediate result),
    // and the subsequent 'return' statement emits emitCoroComplete again.  Without
    // this guard the first result buffer leaks.
    if (_coro_result && _coro_result != result)
        _tin_inline_result_free(_coro_result);
    _coro_done   = 1;
    _coro_result = result;
}

// _tin_coro_take_result is used by the coroutine-chaining drive loop.
// After an inner $coro completes (coro.done returns true), its result
// was stored in _coro_result by _tin_fiber_complete.  This function
// reads and clears both thread-locals so the outer fiber's own completion
// can be detected correctly by the worker loop.
void *_tin_coro_take_result(void) {
    void *r = _coro_result;
    _coro_result = NULL;
    _coro_done   = 0;
    return r;
}

// ---------------------------------------------------------------------------
// Inline-drive result buffer - zero-malloc result storage for await.
//
// When genInlineAsyncDrive drives an inner $coro directly (no fiber spawn),
// the inner$coro's emitCoroComplete should NOT heap-allocate the result box,
// because:
//  1. The outer coroutine reads the result immediately (before coro.destroy).
//  2. The inner$coro runs on the same OS thread as the outer → TLS is safe.
//  3. Avoiding the malloc lets LLVM's coro-elide promote inner frames to the
//     outer coroutine frame (stack-allocated when coro-elide works).
//
// Usage (genInlineAsyncDrive, outer coroutine):
//   _tin_inline_result_mode_begin();
//   innerHdl = call innerCoro(args...);
//   [drive loop...]
//   resultRaw = _tin_coro_take_result();   // TLS ptr or heap ptr
//   _tin_inline_result_mode_end();
//   load T from resultRaw;
//   _tin_inline_result_free(resultRaw);    // no-op for TLS, free() for heap
//
// Usage (emitCoroComplete, inner $coro body):
//   slot = _tin_inline_result_alloc(sizeof(T));
//   store T to slot;
//   _tin_fiber_complete(slot);
//
// Maximum result size that fits in the TLS buffer.  Types larger than this
// fall back to malloc (rare - most return types are scalars or small structs).
// ---------------------------------------------------------------------------
#define INLINE_RESULT_BUF_SIZE 256

static _Thread_local char _inline_result_buf[INLINE_RESULT_BUF_SIZE];

void _tin_inline_result_mode_begin(void) {
    _inline_result_mode = 1;
}

void _tin_inline_result_mode_end(void) {
    _inline_result_mode = 0;
}

// Returns a pointer to the TLS buffer (no malloc) when inline mode is active
// and sz <= INLINE_RESULT_BUF_SIZE; otherwise falls back to malloc(sz).
void *_tin_inline_result_alloc(int64_t sz) {
    if (_inline_result_mode && sz <= INLINE_RESULT_BUF_SIZE) {
        return _inline_result_buf;
    }
    return malloc((size_t)sz);
}

// Free the result pointer returned by _tin_coro_take_result().
// If it points to the TLS buffer it is a no-op; otherwise it is a heap pointer
// that must be freed to avoid a memory leak.
void _tin_inline_result_free(void *ptr) {
    if (ptr != (void *)_inline_result_buf) {
        free(ptr);
    }
}

// ---------------------------------------------------------------------------
// Coroutine frame pool - eliminates per-operation malloc/free on the hot path.
//
// _tin_coro_malloc(size) is called from emitCoroPrologue instead of malloc().
// It prefixes the allocation with an 8-byte size field and returns ptr+8 as
// the LLVM frame pointer.  On a pool hit, the frame is reused without any
// system allocator call.
//
// _tin_coro_free(ptr) is called from emitCoroEpilogue instead of free().
// It receives the LLVM frame pointer (ptr+8 from the original allocation) and
// returns it to the per-thread pool.  Null (coro-elided / stack frame) is a no-op.
//
// Pool layout (per-thread):
//   _coro_pool[0..cnt-1]  – LLVM frame pointers (raw_alloc + 8)
//   Lookup: linear scan checking *(int64_t*)(ptr - 8) == requested size.
// ---------------------------------------------------------------------------
void *_tin_coro_malloc(int64_t size) {
    // Fast path: search pool for a frame of the exact size.
    for (int i = _coro_pool_cnt - 1; i >= 0; i--) {
        int64_t cached_sz;
        memcpy(&cached_sz, (char *)_coro_pool[i] - 8, sizeof(int64_t));
        if (cached_sz == size) {
            void *p = _coro_pool[i];
            // Remove by swapping with last entry.
            _coro_pool_cnt--;
            if (i < _coro_pool_cnt)
                _coro_pool[i] = _coro_pool[_coro_pool_cnt];
            return p;
        }
    }
    // Miss: allocate raw + 8-byte size prefix; return the LLVM frame pointer.
    char *raw = (char *)malloc((size_t)size + 8);
    if (!raw) { fputs("tin: coro frame alloc failed\n", stderr); exit(1); }
    memcpy(raw, &size, sizeof(int64_t));
    return raw + 8;
}

void _tin_coro_free(void *ptr) {
    if (!ptr) return;  // coro-elided (stack-allocated) frame - nothing to do
    if (_coro_pool_cnt < CORO_POOL_MAX) {
        _coro_pool[_coro_pool_cnt++] = ptr;
        return;
    }
    // Pool full: evict the oldest entry (index 0) and free it to system.
    free((char *)_coro_pool[0] - 8);
    // Shift pool left by one and insert at end.
    memmove(_coro_pool, _coro_pool + 1, (size_t)(_coro_pool_cnt - 1) * sizeof(void *));
    _coro_pool[_coro_pool_cnt - 1] = ptr;
}

// Static sentinel for void-returning fibers so Future[Unit].await_result
// never returns NULL.
static char _tin_unit_sentinel = 0;

void *_tin_fiber_get_result(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    void *r = (pid > 0 && pid < _fiber_cnt && _fibers[pid])
              ? _fibers[pid]->result : NULL;
    pthread_mutex_unlock(&_table_mu);
    return r;
}

// Returns the panic message of a completed fiber, or NULL if it completed normally.
// Called inline by generated await expressions to check for a panic before reading
// the result.  The codegen emits the _tin_panic call itself (after this check) so
// that the panic runs in the calling Tin function's defer context - making the panic
// catchable via defer + recover().
const char *_tin_fiber_get_panic_msg(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    TinFiber *f = (pid > 0 && pid < _fiber_cnt) ? _fibers[pid] : NULL;
    const char *msg = f ? f->panic_msg : NULL;
    if (msg && f) {
        f->panic_checked = 1;
        // Retain so the caller holds a reference.  The caller (codegen-emitted
        // await path) passes this to _tin_panic, whose defer chain will run
        // recover() which wraps the message in a TinString and releases it via
        // ARC.  The fiber's own reference (_tin_release at cleanup) brings the
        // count back to zero once all callers are done.
        _tin_retain((void *)msg);
    }
    pthread_mutex_unlock(&_table_mu);
    return msg;
}

// Check for any non-awaited fiber panic and return (retained) message, or NULL.
// Called at every loop back edge of async functions so unhandled panics surface
// as early as possible rather than only at scheduler shutdown.
//
// Fast path: if _has_unhandled_panics == 0, returns NULL without acquiring any
// lock.  When a panic is found, marks it as checked and clears the flag if no
// further unchecked panics remain.  The returned message is retained once; the
// caller is expected to pass it to _tin_panic which will release it via ARC.
const char *_tin_fiber_check_panic(void) {
    if (!atomic_load(&_has_unhandled_panics)) {
        return NULL;
    }

    pthread_mutex_lock(&_table_mu);

    const char *msg = NULL;

    for (int64_t i = 1; i < _fiber_cnt; i++) {
        if (_fibers[i] && _fibers[i]->panic_msg && !_fibers[i]->panic_checked) {
            msg = _fibers[i]->panic_msg;
            _fibers[i]->panic_checked = 1;
            _tin_retain((void *)msg);
            break;
        }
    }

    // Recheck whether any unchecked panics remain so we can clear the flag.
    int still_pending = 0;

    for (int64_t i = 1; i < _fiber_cnt; i++) {
        if (_fibers[i] && _fibers[i]->panic_msg && !_fibers[i]->panic_checked) {
            still_pending = 1;
            break;
        }
    }

    if (!still_pending) {
        atomic_store(&_has_unhandled_panics, 0);
    }

    pthread_mutex_unlock(&_table_mu);

    return msg;
}

void _tin_fiber_join(int64_t pid, void *my_hdl) {
    (void)my_hdl;
    pthread_mutex_lock(&_table_mu);
    if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) {
        pthread_mutex_unlock(&_table_mu);
        return;
    }
    TinFiber *target = _fibers[pid];
    if (target->status == FIBER_DONE) {
        pthread_mutex_unlock(&_table_mu);
        return;
    }

    // Only access the __thread _current_pid on actual worker threads.
    // Accessing __thread variables on the main thread causes macOS dyld to
    // lazily allocate TLS storage that is never freed (main thread TLS has
    // process lifetime with no explicit cleanup), producing LSAN reports.
    // _is_worker_thread() uses pthread_self() which needs no TLS allocation.
    int64_t my_pid = _is_worker_thread() ? _current_pid : -1;
    if (my_pid > 0 && my_pid < _fiber_cnt && _fibers[my_pid]) {
        // Called from inside a fiber: register as a waiter and defer blocking.
        // We must NOT set FIBER_BLOCKED here because coro.suspend hasn't fired
        // yet - _fire_done_waiters could see FIBER_BLOCKED, push us onto the
        // run queue, and a second worker would call _coro_resume concurrently
        // (double-resume, corrupting the coroutine frame).
        // Instead, set pending_join so the worker loop sets BLOCKED (or
        // re-queues if pending_wakeup is already set) after _coro_resume returns.
        TinFiber *me = _fibers[my_pid];
        if (target->waiter_cnt < FIBER_MAX_WAITERS) {
            target->waiters[target->waiter_cnt++] = my_pid;
            me->pending_join = 1;
            pthread_mutex_unlock(&_table_mu);
            // coro.suspend fires after this call returns in the generated IR.
            return;
        }
        // Waiter list full: fall through to OS-blocking wait below.
    }

    // Non-fiber context (e.g. main thread) or waiter list full: block OS thread.
    pthread_mutex_lock(&target->done_mu);
    pthread_mutex_unlock(&_table_mu);
    while (target->status != FIBER_DONE)
        pthread_cond_wait(&target->done_cv, &target->done_mu);
    pthread_mutex_unlock(&target->done_mu);
}

// ---------------------------------------------------------------------------
// await match runtime support
// ---------------------------------------------------------------------------

// _tin_fiber_poll_any: non-blocking check.
// Returns the index of the first FIBER_DONE pid in pids[0..n-1], or -1.
int64_t _tin_fiber_poll_any(int64_t *pids, int64_t n) {
    pthread_mutex_lock(&_table_mu);
    int64_t result = -1;
    for (int64_t i = 0; i < n; i++) {
        int64_t pid = pids[i];
        if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) continue;
        if (_fibers[pid]->status == FIBER_DONE) { result = i; break; }
    }
    pthread_mutex_unlock(&_table_mu);
    return result;
}

// _tin_fiber_poll_any_skip: like poll_any but skips indices where skip[i] != 0.
// Used by the re-block loop when guards fail.
int64_t _tin_fiber_poll_any_skip(int64_t *pids, int64_t n, int8_t *skip) {
    pthread_mutex_lock(&_table_mu);
    int64_t result = -1;
    for (int64_t i = 0; i < n; i++) {
        if (skip[i]) continue;
        int64_t pid = pids[i];
        if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) continue;
        if (_fibers[pid]->status == FIBER_DONE) { result = i; break; }
    }
    pthread_mutex_unlock(&_table_mu);
    return result;
}

// _tin_fiber_join_any: park the calling fiber until any of pids[0..n-1] completes.
// skip[i] != 0 means ignore that slot (already processed with a failing guard).
// On return the fiber has been unparked; call _tin_fiber_poll_any_skip to get idx.
void _tin_fiber_join_any(int64_t *pids, int64_t n, int8_t *skip, void *my_hdl,
                         TinAnyWaiter *aw) {
    (void)my_hdl;
    pthread_mutex_lock(&_table_mu);

    int64_t my_pid = _is_worker_thread() ? _current_pid : -1;

    // Check if any non-skipped target is already done.
    for (int64_t i = 0; i < n; i++) {
        if (skip && skip[i]) continue;
        int64_t pid = pids[i];
        if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) continue;
        if (_fibers[pid]->status == FIBER_DONE) {
            pthread_mutex_unlock(&_table_mu);
            return; // no need to park
        }
    }

    if (my_pid <= 0 || my_pid >= _fiber_cnt || !_fibers[my_pid]) {
        // Non-fiber context: fall through to sync wait below.
        pthread_mutex_unlock(&_table_mu);
        for (;;) {
            int64_t idx = _tin_fiber_poll_any_skip(pids, n, skip);
            if (idx >= 0) return;
            sched_yield();
        }
    }

    // Initialize the any_waiter.
    atomic_store(&aw->fired, 0);
    aw->result_idx = -1;
    aw->pids       = pids;
    aw->n          = n;
    aw->waiter_pid = my_pid;

    TinFiber *me = _fibers[my_pid];
    me->any_waiter = aw;

    // Register my_pid in each non-skipped, not-done target's waiter list.
    int registered = 0;
    for (int64_t i = 0; i < n; i++) {
        if (skip && skip[i]) continue;
        int64_t pid = pids[i];
        if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) continue;
        TinFiber *t = _fibers[pid];
        if (t->status == FIBER_DONE) {
            // Found a done target while registering: win immediately.
            atomic_store(&aw->fired, 1);
            aw->result_idx = i;
            me->any_waiter = NULL;
            pthread_mutex_unlock(&_table_mu);
            return;
        }
        if (t->waiter_cnt < FIBER_MAX_WAITERS) {
            t->waiters[t->waiter_cnt++] = my_pid;
            t->any_waiter = aw;
            registered++;
        }
    }

    if (registered == 0) {
        // Nothing to wait on (all skipped or list full).
        me->any_waiter = NULL;
        pthread_mutex_unlock(&_table_mu);
        return;
    }

    me->pending_join = 1;
    pthread_mutex_unlock(&_table_mu);
    // coro.suspend fires after this call returns in the generated IR.
}

// _tin_fiber_sync_await_any: synchronous (main-thread) wait.
// Spin-polls until any non-skipped pid completes. Returns its index.
int64_t _tin_fiber_sync_await_any(int64_t *pids, int64_t n, int8_t *skip) {
    for (;;) {
        int64_t idx = _tin_fiber_poll_any_skip(pids, n, skip);
        if (idx >= 0) return idx;
        sched_yield();
    }
}

// Wake a blocked fiber by marking it RUNNABLE and re-enqueueing it.
// Called from the I/O thread, timer thread, and channel helpers (while a
// worker fiber is running).
//
// Fast path (called from within a worker thread):
//   Push to the calling worker's local deque (lock-free).  The worker drains
//   this before falling through to the global run queue.
//
// Slow path (called from I/O thread / timer thread / main thread):
//   Push to the global run queue as before.
void _tin_fiber_unpark(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) {
        pthread_mutex_unlock(&_table_mu);
        return;
    }
    TinFiber *f = _fibers[pid];
    void *hdl = f->hdl;
    pthread_mutex_unlock(&_table_mu);  // release early; status via per-fiber lock

    _fib_lock(f);
    if (f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        _fib_unlock(f);
        _worker_push((TinRunnable){ hdl, pid }, f);
        return;
    }
    if (f->status == FIBER_RUNNING) {
        f->pending_wakeup = 1;
        _fib_unlock(f);
        return;
    }
    _fib_unlock(f);
}

// Like _tin_fiber_unpark but the caller already has the coro handle so we skip
// the _table_mu-protected f->hdl read.  The status check still needs _table_mu.
// Used by TinFastMutex (fastmutex.c) and the channel waiter lists so that the
// unpark path never has to dereference the fiber table to find the handle.
void _tin_fiber_unpark_hdl(int64_t pid, void *hdl) {
    pthread_mutex_lock(&_table_mu);
    if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) {
        pthread_mutex_unlock(&_table_mu);
        return;
    }
    TinFiber *f = _fibers[pid];
    pthread_mutex_unlock(&_table_mu);  // release early; status via per-fiber lock

    _fib_lock(f);
    if (f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        _fib_unlock(f);
        _worker_push((TinRunnable){ hdl, pid }, f);
        return;
    }
    if (f->status == FIBER_RUNNING) {
        f->pending_wakeup = 1;
        _fib_unlock(f);
        return;
    }
    _fib_unlock(f);
}

void _tin_fiber_yield_coro(void *hdl) {
    (void)hdl;
    // Status management is done by the worker loop (RUNNING -> RUNNABLE + push)
    // or by park callers (_tin_fiber_park: RUNNING -> BLOCKED).
    // This function is intentionally a no-op: the worker checks status after
    // _coro_resume returns and acts accordingly.
}

// Park the current fiber: signal the worker loop to block it after coro.suspend.
// The caller is responsible for registering a waker that calls _tin_fiber_unpark.
// Must be called before the coro.suspend (yield) that follows in the IR.
//
// We set pending_park (not FIBER_BLOCKED directly) to avoid a double-resume race:
// if the waker fires between _tin_fiber_park and the subsequent yield, it sees
// FIBER_RUNNING and sets pending_wakeup.  The worker loop then re-enqueues instead
// of blocking, so _coro_resume is never called on a coroutine that is still
// executing its body on another worker thread.
void _tin_fiber_park(int64_t pid) {
    if (pid <= 0) return;
    // pid is always _tin_current_pid() - the currently running fiber.
    // Running fibers are never freed, so _fibers[pid] is valid without _table_mu.
    TinFiber *f = _fibers[pid];
    if (!f) return;
    _fib_lock(f);
    f->pending_park = 1;  // Worker will set BLOCKED after coro.suspend fires.
    _fib_unlock(f);
}

void _tin_fiber_sync_await(int64_t pid) {
    _tin_fiber_join(pid, NULL);
}

void _tin_fiber_run(void) {
    if (!_workers) return;  // no workers spawned

    // Signal workers to stop immediately: they exit at their next _rq_pop,
    // even if items remain in the queue.  Unawaited fibers are abandoned
    // (Go semantics: main exit == process exit).
    pthread_mutex_lock(&_run_queue.mu);
    _run_queue.shutdown = 1;
    pthread_cond_broadcast(&_run_queue.not_empty);
    pthread_mutex_unlock(&_run_queue.mu);

    // Join workers BEFORE shutting down timer/IO so that timer wakeups
    // after workers exit are simply ignored (no running worker to receive them).
    for (int i = 0; i < _worker_cnt; i++)
        pthread_join(_workers[i], NULL);

    // Now it is safe to stop timer and I/O threads.
    _tin_timer_shutdown();
    _tin_io_shutdown();

    free(_workers);
    _workers    = NULL;
    _worker_cnt = 0;

    // Check for fire-and-forget panics: fibers that panicked but were never
    // awaited.  Re-raise the first such panic on the main thread (fatal).
    // This matches Go semantics: an unrecovered goroutine panic kills the process.
    if (_fibers) {
        for (int64_t i = 1; i < _fiber_cnt; i++) {
            if (_fibers[i] && _fibers[i]->panic_msg && !_fibers[i]->panic_checked) {
                // Re-panic: will call exit(1) since we're on the main thread
                // with no fiber catch mode active.
                const char *msg = _fibers[i]->panic_msg;
                _fibers[i]->panic_msg = NULL;  // prevent double-release below
                _tin_panic(msg);
                // unreachable (exit(1) above), but needed for C:
                return;
            }
        }
    }

    // Free fiber table.
    if (_fibers) {
        for (int64_t i = 1; i < _fiber_cnt; i++) {
            if (_fibers[i]) {
                free(_fibers[i]->result);
                if (_fibers[i]->panic_msg) _tin_release(_fibers[i]->panic_msg);
                pthread_mutex_destroy(&_fibers[i]->done_mu);
                pthread_cond_destroy(&_fibers[i]->done_cv);
                free(_fibers[i]);
                _fibers[i] = NULL;
            }
        }
        free(_fibers);
        _fibers    = NULL;
        _fiber_cnt = 1;
        _fiber_cap = 0;
    }

    // Free run queue - release any coro frames still pending in the queue
    // (fibers abandoned at shutdown that never got a chance to run).
    for (int64_t i = 0; i < _run_queue.count; i++) {
        TinRunnable r = _run_queue.buf[(_run_queue.head + i) % _run_queue.cap];
        if (r.hdl) _tin_coro_free(r.hdl);
    }
    free(_run_queue.buf);
    _run_queue.buf = NULL;
    pthread_mutex_destroy(&_run_queue.mu);
    pthread_cond_destroy(&_run_queue.not_empty);

    // Flush the main-thread coro frame pool.  Worker thread pools are flushed
    // in _worker_thread before return.  The main thread drives test bodies and
    // inline coroutines, accumulating frames here that are never otherwise freed.
    for (int i = 0; i < _coro_pool_cnt; i++)
        free((char *)_coro_pool[i] - 8);
    _coro_pool_cnt = 0;
}

// (moved above _tin_fiber_await_result)

void *_tin_future_await_raw(int64_t pid) {
    _tin_fiber_join(pid, NULL);
    void *r = _tin_fiber_get_result(pid);
    return r ? r : (void *)&_tin_unit_sentinel;
}
