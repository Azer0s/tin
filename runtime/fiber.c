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
    // Pending wakeup flag: set by _tin_fiber_unpark when the fiber is still
    // RUNNING (park hasn't happened yet). _tin_fiber_park checks and clears
    // this flag; if set, it skips setting BLOCKED so no double-resume occurs.
    // Protected by _table_mu.
    int      pending_wakeup;
    // If the fiber panicked and the panic was caught by the worker loop,
    // this field holds the message as an ARC-managed buffer (via _tin_rc_alloc)
    // so it can be safely wrapped in a TinString by the awaiting fiber.
    // NULL means the fiber completed normally.
    // Freed via _tin_release() in fiber cleanup.
    char    *panic_msg;
    // Set to 1 when _tin_fiber_get_panic_msg reads a non-NULL panic_msg.
    // Used at shutdown to detect fire-and-forget panics nobody awaited.
    int      panic_checked;
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
    pthread_cond_signal(&_run_queue.not_empty);
    pthread_mutex_unlock(&_run_queue.mu);
}

// Blocks until a runnable is available or shutdown is signalled.
// Returns a sentinel with pid=-1 on shutdown (even if items remain in queue).
// This ensures that when _tin_fiber_run signals shutdown, workers stop
// immediately without draining unawaited fibers.
static TinRunnable _rq_pop(void) {
    pthread_mutex_lock(&_run_queue.mu);
    while (_run_queue.count == 0 && !_run_queue.shutdown)
        pthread_cond_wait(&_run_queue.not_empty, &_run_queue.mu);
    if (_run_queue.shutdown) {
        // Shut down immediately regardless of remaining items.
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

static __thread int64_t  _current_pid    = -1;
static __thread int       _coro_done     = 0;
static __thread void     *_coro_result   = NULL;

// Called by the I/O layer to access current fiber pid.
int64_t _tin_current_pid(void) { return _current_pid; }

// -------------------------------------------------------------------
// Worker threads
// -------------------------------------------------------------------

static pthread_t *_workers    = NULL;
static int        _worker_cnt = 0;

static void _fire_done_waiters(TinFiber *f) {
    // Must be called with _table_mu held.
    for (int i = 0; i < f->waiter_cnt; i++) {
        int64_t wpid = f->waiters[i];
        if (wpid <= 0 || wpid >= _fiber_cnt || !_fibers[wpid]) continue;
        TinFiber *w = _fibers[wpid];
        if (w->status == FIBER_BLOCKED) {
            w->status = FIBER_RUNNABLE;
            _rq_push((TinRunnable){ w->hdl, wpid });
        }
    }
    f->waiter_cnt = 0;
}

static void *_worker_thread(void *_) {
    (void)_;
    _tin_defer_chain = NULL;
    while (1) {
        TinRunnable r = _rq_pop();
        if (r.pid < 0) break;  // shutdown sentinel

        pthread_mutex_lock(&_table_mu);
        if (r.pid >= _fiber_cnt || !_fibers[r.pid]) {
            pthread_mutex_unlock(&_table_mu);
            continue;
        }
        TinFiber *f = _fibers[r.pid];
        f->status = FIBER_RUNNING;
        pthread_mutex_unlock(&_table_mu);

        _current_pid  = r.pid;
        _coro_done    = 0;
        _coro_result  = NULL;

        _tin_panic_catch_begin();
        _coro_resume(r.hdl);
        const char *panicked = _tin_panic_catch_end();

        if (panicked) {
            // Fiber panicked.  Destroy the coro frame (it returned after panic).
            _coro_destroy(r.hdl);
            free(r.hdl);
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
            _coro_destroy(r.hdl);
            free(r.hdl);
            pthread_mutex_lock(&_table_mu);
            f->result = _coro_result;
            f->status = FIBER_DONE;
            _fire_done_waiters(f);
            pthread_mutex_lock(&f->done_mu);
            pthread_cond_broadcast(&f->done_cv);
            pthread_mutex_unlock(&f->done_mu);
            pthread_mutex_unlock(&_table_mu);
        } else {
            // Fiber yielded or parked.
            // Only the worker re-enqueues on regular yield (RUNNING status).
            // If a waker (timer/IO) already transitioned to RUNNABLE and enqueued,
            // we see RUNNABLE here and must NOT push again (avoids double-enqueue).
            // If the fiber parked (sleep/IO set BLOCKED), the waker will enqueue.
            pthread_mutex_lock(&_table_mu);
            FiberStatus st = f->status;
            if (st == FIBER_RUNNING) {
                f->status = FIBER_RUNNABLE;
                pthread_mutex_unlock(&_table_mu);
                _rq_push(r);
            } else {
                // FIBER_RUNNABLE: waker already enqueued - don't push again.
                // FIBER_BLOCKED:  waker will enqueue when condition resolves.
                pthread_mutex_unlock(&_table_mu);
            }
        }
        _current_pid = -1;
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

    _rq_push((TinRunnable){ hdl, pid });
    return pid;
}

void _tin_fiber_complete(void *hdl, void *result) {
    _coro_done   = 1;
    _coro_result = result;
    (void)hdl;
}

// _tin_coro_take_result is used by the coroutine-chaining drive loop.
// After an inner $coro completes (coro.done returns true), its heap-boxed
// result was stored in _coro_result by _tin_fiber_complete.  This function
// reads and clears both thread-locals so the outer fiber's own completion
// can be detected correctly by the worker loop.
void *_tin_coro_take_result(void) {
    void *r = _coro_result;
    _coro_result = NULL;
    _coro_done   = 0;
    return r;
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

    int64_t my_pid = _current_pid;
    if (my_pid > 0 && my_pid < _fiber_cnt && _fibers[my_pid]) {
        // Called from inside a fiber: park this fiber as a waiter.
        TinFiber *me = _fibers[my_pid];
        if (target->waiter_cnt < FIBER_MAX_WAITERS) {
            target->waiters[target->waiter_cnt++] = my_pid;
            me->status = FIBER_BLOCKED;
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
// Called from the I/O thread and timer thread.
void _tin_fiber_unpark(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    if (pid <= 0 || pid >= _fiber_cnt || !_fibers[pid]) {
        pthread_mutex_unlock(&_table_mu);
        return;
    }
    TinFiber *f = _fibers[pid];
    if (f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        pthread_mutex_unlock(&_table_mu);
        _rq_push((TinRunnable){ f->hdl, pid });
        return;
    }
    if (f->status == FIBER_RUNNING) {
        // _tin_fiber_park hasn't been called yet (fiber is still running).
        // Set pending_wakeup so that when _tin_fiber_park is called it will
        // skip blocking the fiber; the worker re-enqueues it after yield.
        f->pending_wakeup = 1;
        pthread_mutex_unlock(&_table_mu);
        return;
    }
    pthread_mutex_unlock(&_table_mu);
}

void _tin_fiber_yield_coro(void *hdl) {
    (void)hdl;
    // Status management is done by the worker loop (RUNNING -> RUNNABLE + push)
    // or by park callers (_tin_fiber_park: RUNNING -> BLOCKED).
    // This function is intentionally a no-op: the worker checks status after
    // _coro_resume returns and acts accordingly.
}

// Park the current fiber: mark it BLOCKED so the worker does not re-enqueue it.
// The caller is responsible for registering a waker that calls _tin_fiber_unpark.
// Must be called before the coro.suspend (yield) that follows in the IR.
//
// If a waker called _tin_fiber_unpark while the fiber was still RUNNING (before
// this function), pending_wakeup is set. In that case we skip marking BLOCKED so
// the worker re-enqueues the fiber normally after the subsequent yield - avoiding
// a double-resume race where two workers both try to _coro_resume the same fiber.
void _tin_fiber_park(int64_t pid) {
    if (pid <= 0) return;
    pthread_mutex_lock(&_table_mu);
    if (pid < _fiber_cnt && _fibers[pid]) {
        TinFiber *f = _fibers[pid];
        if (f->pending_wakeup) {
            // Wakeup arrived before the park; clear the flag and stay RUNNABLE.
            // The fiber will be re-enqueued by the worker after the next yield.
            f->pending_wakeup = 0;
        } else {
            f->status = FIBER_BLOCKED;
        }
    }
    pthread_mutex_unlock(&_table_mu);
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
