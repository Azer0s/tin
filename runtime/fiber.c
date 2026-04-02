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
    // Per-fiber spinlock: protects status, pending_park, and pending_wakeup
    // in the park/unpark hot path.  Replacing the global _table_mu for these
    // transitions eliminates cross-core cache-line bouncing between workers
    // when each worker is running a different fiber (e.g. TINMAXPROCS=2).
    // Lock ordering: _table_mu (outer) -> state_lock (inner) — never reverse.
    _Atomic(uint32_t) state_lock;
} TinFiber;

static TinFiber  **_fibers    = NULL;
static int64_t     _fiber_cap = 0;
static int64_t     _fiber_cnt = 1;   // next pid; 0 reserved
static pthread_mutex_t _table_mu = PTHREAD_MUTEX_INITIALIZER;


// -------------------------------------------------------------------
// Run queue - mutex + condvar FIFO ring buffer
// -------------------------------------------------------------------

typedef struct { void *hdl; int64_t pid; } TinRunnable;

typedef struct {
    pthread_mutex_t mu;
    pthread_cond_t  not_empty;
    TinRunnable    *buf;
    int64_t         cap, head, tail, count;
    int             shutdown;
} TinRunQueue;

static TinRunQueue _run_queue;

static void _rq_init(void) {
    pthread_mutex_init(&_run_queue.mu, NULL);
    pthread_cond_init(&_run_queue.not_empty, NULL);
    _run_queue.cap  = 1024;
    _run_queue.buf  = (TinRunnable *)calloc((size_t)_run_queue.cap, sizeof(TinRunnable));
    _run_queue.head = _run_queue.tail = _run_queue.count = 0;
    _run_queue.shutdown = 0;
}

// Declared here so _rq_push can check it; defined after _rq_pop.
static atomic_int _spinning_workers;

static void _rq_push(TinRunnable r) {
    pthread_mutex_lock(&_run_queue.mu);
    if (_run_queue.count == _run_queue.cap) {
        int64_t newcap = _run_queue.cap * 2;
        TinRunnable *nb = (TinRunnable *)malloc((size_t)newcap * sizeof(TinRunnable));
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

// Set to 1 at the start of each worker thread; stays 0 on all other threads
// (main thread, I/O thread, timer thread).  Used by _tin_fiber_unpark to
// decide whether a direct runnext hand-off is safe.
static __thread int        _is_worker      = 0;

// Per-worker hot slot: populated by _tin_fiber_unpark when called from within
// a worker thread.  The worker loop drains this before calling _rq_pop, giving
// a zero-overhead direct hand-off to the newly-runnable fiber without going
// through the global run queue (no mutex, no condvar).
static __thread int64_t    _worker_runnext_pid = -1;
static __thread void      *_worker_runnext_hdl = NULL;
// Companion to runnext: store the TinFiber* so the worker loop can skip the
// _table_mu lookup that otherwise happens on every dequeue.  Only valid when
// _worker_runnext_pid >= 0.
static __thread TinFiber  *_worker_runnext_fib = NULL;

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
// _table_mu global lock entirely.  Only the per-fiber state_lock is needed.
// Also stores the TinFiber* in _worker_runnext_fib so the worker loop can skip
// the subsequent _table_mu lookup for the runnext case.
void _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl) {
    TinFiber *f = (TinFiber *)fib;
    if (!f) return;
    _fib_lock(f);
    if (f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        _fib_unlock(f);
        if (_is_worker) {
            if (_worker_runnext_pid < 0) {
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
            } else {
                TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
                _rq_push(old);
            }
        } else {
            _rq_push((TinRunnable){ hdl, pid });
        }
        return;
    }
    if (f->status == FIBER_RUNNING) {
        f->pending_wakeup = 1;
        _fib_unlock(f);
        return;
    }
    _fib_unlock(f);
}

static void _fire_done_waiters(TinFiber *f) {
    // Must be called with _table_mu held.
    for (int i = 0; i < f->waiter_cnt; i++) {
        int64_t wpid = f->waiters[i];
        if (wpid <= 0 || wpid >= _fiber_cnt || !_fibers[wpid]) continue;
        TinFiber *w = _fibers[wpid];
        _fib_lock(w);  // lock ordering: _table_mu (held) -> state_lock (acquired here)
        if (w->status == FIBER_BLOCKED) {
            // Waiter already suspended; wake it immediately.
            w->status = FIBER_RUNNABLE;
            void *whdl = w->hdl;
            _fib_unlock(w);
            // Use runnext when called from a worker to avoid cross-core
            // condvar wakeup — keeps the woken fiber on the same worker.
            if (_is_worker) {
                if (_worker_runnext_pid < 0) {
                    _worker_runnext_pid = wpid;
                    _worker_runnext_hdl = whdl;
                    _worker_runnext_fib = w;
                } else {
                    TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                    _worker_runnext_pid = wpid;
                    _worker_runnext_hdl = whdl;
                    _worker_runnext_fib = w;
                    _rq_push(old);
                }
            } else {
                _rq_push((TinRunnable){ whdl, wpid });
            }
        } else if (w->pending_join) {
            // Waiter called _tin_fiber_join but its coro.suspend hasn't fired
            // yet (status is still FIBER_RUNNING).  Setting FIBER_RUNNABLE and
            // pushing NOW would cause a second worker to call _coro_resume
            // while the first worker's _coro_resume is still on the stack -
            // a double-resume that corrupts the coroutine frame.
            // Instead, set pending_wakeup: the worker loop will re-queue the
            // fiber after _coro_resume returns and coro.suspend has completed.
            w->pending_wakeup = 1;
            _fib_unlock(w);
        } else {
            _fib_unlock(w);
        }
    }
    f->waiter_cnt = 0;
}

static void *_worker_thread(void *_) {
    (void)_;
    _is_worker       = 1;
    _tin_defer_chain = NULL;
    while (1) {
        // Drain runnext first: direct hand-off from the previous fiber's unpark
        // call, bypassing the global run queue entirely (no mutex, no condvar).
        TinRunnable r;
        TinFiber   *f = NULL;
        if (_worker_runnext_pid >= 0) {
            r.pid = _worker_runnext_pid;
            r.hdl = _worker_runnext_hdl;
            f     = _worker_runnext_fib;   // pre-captured TinFiber*, no _table_mu needed
            _worker_runnext_pid = -1;
            _worker_runnext_hdl = NULL;
            _worker_runnext_fib = NULL;
        } else {
            r = _rq_pop();
            if (r.pid < 0) break;  // shutdown sentinel
        }

        if (!f) {
            // runnext didn't carry a pre-captured TinFiber* (run-queue path or
            // legacy unpark via _tin_fiber_unpark_hdl): fall back to table lookup.
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

        _tin_panic_catch_begin();
        _coro_resume(r.hdl);
        const char *panicked = _tin_panic_catch_end();

        if (panicked) {
            // Fiber panicked.  Destroy the coro frame.
            // The cleanup path (emitCoroEpilogue) calls llvm.coro.free which
            // frees the heap-allocated frame inside _coro_destroy.  Spawned
            // fiber frames cannot be stack-allocated by coro-elide (the hdl
            // escapes to _tin_fiber_spawn in the caller), so _coro_destroy
            // always runs the full cleanup here.
            _coro_destroy(r.hdl);
            pthread_mutex_lock(&_table_mu);
            // Allocate with _tin_rc_alloc so the message has a proper ARC header.
            // This lets the awaiting fiber wrap it in a TinString and release it
            // normally via _tin_release(). See _tin_fiber_get_panic_msg.
            size_t plen = strlen(panicked);
            char *pmsg_buf = (char *)_tin_rc_alloc((int64_t)(plen + 1));
            memcpy(pmsg_buf, panicked, plen + 1);
            f->panic_msg = pmsg_buf;
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
            // The cleanup path (emitCoroEpilogue) calls llvm.coro.free which
            // frees the heap-allocated frame inside _coro_destroy.
            _coro_destroy(r.hdl);
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
                    if (_worker_runnext_pid < 0) {
                        _worker_runnext_pid = r.pid;
                        _worker_runnext_hdl = r.hdl;
                        _worker_runnext_fib = f;
                    } else {
                        TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                        _worker_runnext_pid = r.pid;
                        _worker_runnext_hdl = r.hdl;
                        _worker_runnext_fib = f;
                        _rq_push(old);
                    }
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
                    if (_worker_runnext_pid < 0) {
                        _worker_runnext_pid = r.pid;
                        _worker_runnext_hdl = r.hdl;
                        _worker_runnext_fib = f;
                    } else {
                        TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                        _worker_runnext_pid = r.pid;
                        _worker_runnext_hdl = r.hdl;
                        _worker_runnext_fib = f;
                        _rq_push(old);
                    }
                } else {
                    // No wakeup yet; block until _tin_fiber_unpark fires.
                    f->status = FIBER_BLOCKED;
                    _fib_unlock(f);
                }
            } else if (f->status == FIBER_RUNNING) {
                // Normal yield (_tin_fiber_yield_coro): re-enqueue.
                f->status = FIBER_RUNNABLE;
                _fib_unlock(f);
                if (_worker_runnext_pid < 0) {
                    _worker_runnext_pid = r.pid;
                    _worker_runnext_hdl = r.hdl;
                    _worker_runnext_fib = f;
                } else {
                    _rq_push(r);
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
    return NULL;
}

// -------------------------------------------------------------------
// Public API
// -------------------------------------------------------------------

void _tin_fiber_init(void) {
    pthread_mutex_lock(&_table_mu);
    if (!_fibers) {
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
        _fiber_cap = 256;
        _fibers    = (TinFiber **)calloc((size_t)_fiber_cap, sizeof(TinFiber *));
        if (!_fibers) { fputs("tin: fiber table OOM\n", stderr); exit(1); }
        _fiber_cnt = 1;
    }
    if (_fiber_cnt >= _fiber_cap) {
        int64_t new_cap = _fiber_cap * 2;
        TinFiber **nf = (TinFiber **)realloc(_fibers, sizeof(TinFiber *) * (size_t)new_cap);
        if (!nf) { fputs("tin: fiber table OOM\n", stderr); exit(1); }
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

    // When spawning from a worker thread, use runnext so the new fiber runs on
    // the same worker as the spawner.  This avoids waking idle workers and keeps
    // cooperating fibers (e.g. ping-pong channels) on the same core, eliminating
    // cross-core cache-line bouncing on the per-fiber state_lock.
    // If runnext is occupied, flush the old entry to the global queue (waking
    // a sleeping worker) and replace it with the new fiber.
    if (_is_worker) {
        if (_worker_runnext_pid < 0) {
            _worker_runnext_pid = pid;
            _worker_runnext_hdl = hdl;
            _worker_runnext_fib = f;
        } else {
            TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
            _worker_runnext_pid = pid;
            _worker_runnext_hdl = hdl;
            _worker_runnext_fib = f;
            _rq_push(old);
        }
    } else {
        _rq_push((TinRunnable){ hdl, pid });
    }
    return pid;
}

void _tin_fiber_complete(void *result) {
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
// Inline-drive result buffer — zero-malloc result storage for await.
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
// fall back to malloc (rare — most return types are scalars or small structs).
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
// Coroutine frame pool — eliminates per-operation malloc/free on the hot path.
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
#define CORO_POOL_MAX 16

static _Thread_local void    *_coro_pool[CORO_POOL_MAX];
static _Thread_local int      _coro_pool_cnt = 0;

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
    if (!ptr) return;  // coro-elided (stack-allocated) frame — nothing to do
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

// Wake a blocked fiber by marking it RUNNABLE and re-enqueueing it.
// Called from the I/O thread, timer thread, and channel helpers (while a
// worker fiber is running).
//
// Fast path (called from within a worker thread):
//   Store the newly-runnable fiber in _worker_runnext so the calling worker
//   picks it up on the very next loop iteration — no global run queue mutex,
//   no condvar signal.  If runnext is already occupied, flush the old entry to
//   the global queue and replace it with the new one.
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
        if (_is_worker) {
            if (_worker_runnext_pid < 0) {
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
            } else {
                TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
                _rq_push(old);
            }
        } else {
            _rq_push((TinRunnable){ hdl, pid });
        }
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
        if (_is_worker) {
            if (_worker_runnext_pid < 0) {
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
            } else {
                TinRunnable old = { _worker_runnext_hdl, _worker_runnext_pid };
                _worker_runnext_pid = pid;
                _worker_runnext_hdl = hdl;
                _worker_runnext_fib = f;
                _rq_push(old);
            }
        } else {
            _rq_push((TinRunnable){ hdl, pid });
        }
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
    // pid is always _tin_current_pid() — the currently running fiber.
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

    // Free run queue.
    free(_run_queue.buf);
    _run_queue.buf = NULL;
    pthread_mutex_destroy(&_run_queue.mu);
    pthread_cond_destroy(&_run_queue.not_empty);
}

// (moved above _tin_fiber_await_result)

void *_tin_future_await_raw(int64_t pid) {
    _tin_fiber_join(pid, NULL);
    void *r = _tin_fiber_get_result(pid);
    return r ? r : (void *)&_tin_unit_sentinel;
}
