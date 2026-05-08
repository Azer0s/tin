// tin runtime - fiber sleep / timer support
//
// A dedicated timer thread polls every 1ms and fires expired timers by
// calling _tin_fiber_unpark(pid) to re-enqueue the sleeping fiber.
//
// API:
//   _tin_sleep_ms(ms)  - park the current fiber for `ms` milliseconds
//   _tin_timer_init()  - start the timer thread (called from _tin_fiber_init)
//   _tin_timer_shutdown() - stop the timer thread (called from _tin_fiber_run)

#include "runtime.h"
#include "fiber.h"
#include "timer.h"
#include <errno.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <pthread.h>
#include <time.h>

#define TIN_TIMERS_INIT        1024
#define TIN_TIMERS_DEFAULT_MAX (1 << 20)  // 1M

typedef struct {
    int64_t deadline_ms;  // absolute deadline in milliseconds since epoch
    int64_t pid;          // fiber to wake
} TinTimer;

static TinTimer       *_timers    = NULL;
static int             _timer_cnt = 0;
static int             _timer_cap = 0;
static int             _timer_max = 0;
static pthread_mutex_t _timer_mu  = PTHREAD_MUTEX_INITIALIZER;

static pthread_t     _timer_thread_id;
static volatile int  _timer_shutdown = 0;
static int           _timer_initialized = 0;

static int64_t _now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000LL + (int64_t)ts.tv_nsec / 1000000LL;
}

static void *_timer_thread_fn(void *_) {
    (void)_;
    while (!_timer_shutdown) {
        struct timespec req = { 0, 1000000L }; // 1ms
        nanosleep(&req, NULL);

        int64_t now = _now_ms();

        pthread_mutex_lock(&_timer_mu);
        int i = 0;
        while (i < _timer_cnt) {
            if (_timers[i].deadline_ms <= now) {
                int64_t pid = _timers[i].pid;
                // Remove by swapping with last.
                _timers[i] = _timers[--_timer_cnt];
                pthread_mutex_unlock(&_timer_mu);
                _tin_fiber_unpark(pid);
                pthread_mutex_lock(&_timer_mu);
                // Don't increment i: the swapped entry needs checking too.
            } else {
                i++;
            }
        }
        pthread_mutex_unlock(&_timer_mu);
    }
    return NULL;
}

void _tin_timer_init(void) {
    if (_timer_initialized) return;
    _timer_initialized = 1;

    const char *env = getenv("TINMAXTIMERS");
    _timer_max = (env && *env) ? atoi(env) : TIN_TIMERS_DEFAULT_MAX;
    if (_timer_max <= 0) _timer_max = TIN_TIMERS_DEFAULT_MAX;

    _timer_cap = TIN_TIMERS_INIT;
    _timers = (TinTimer *)malloc((size_t)_timer_cap * sizeof(TinTimer));
    if (!_timers) { fputs("tin: timer table OOM\n", stderr); exit(1); }

    _timer_shutdown = 0;
    pthread_create(&_timer_thread_id, NULL, _timer_thread_fn, NULL);
}

void _tin_timer_shutdown(void) {
    if (!_timer_initialized) return;
    _timer_shutdown = 1;
    pthread_join(_timer_thread_id, NULL);
    _timer_initialized = 0;
    free(_timers);
    _timers    = NULL;
    _timer_cap = 0;
    _timer_cnt = 0;
}

// Park the current fiber for `ms` milliseconds.  Outside a fiber (e.g.
// from a top-level main or the test runner) falls back to a plain
// nanosleep so callers see a real wait either way -- the fiber-park
// branch matters only when other fibers should keep running while
// this one is parked.  The "no-op when not awaited" guarantee for
// time::sleep is enforced at the Tin level (the lazy SleepFuture is
// only consulted via await), so this function is unconditionally
// allowed to actually sleep.
void _tin_sleep_ms(int64_t ms) {
    if (ms <= 0) return;

    int64_t pid = _tin_current_pid();

    if (pid < 0) {
        struct timespec req, rem;
        req.tv_sec  = (time_t)(ms / 1000);
        req.tv_nsec = (long)((ms % 1000) * 1000000L);
        // Loop on EINTR so a signal (e.g. SIGCHLD on test harnesses)
        // does not cut the wait short.  rem is updated with the
        // remaining time; on success nanosleep returns 0 and the
        // loop exits.
        while (nanosleep(&req, &rem) == -1 && errno == EINTR) {
            req = rem;
        }
        return;
    }

    int64_t deadline = _now_ms() + ms;

    // Park the fiber before adding to the timer list.  _tin_fiber_park now sets
    // pending_park (not FIBER_BLOCKED directly) so even if the timer fires before
    // coro.suspend, _tin_fiber_unpark sees FIBER_RUNNING and sets pending_wakeup;
    // the worker loop re-enqueues after coro.suspend returns, avoiding a
    // double-resume race.
    _tin_fiber_park(pid);

    pthread_mutex_lock(&_timer_mu);
    if (_timer_cnt >= _timer_cap) {
        if (_timer_cap >= _timer_max) {
            pthread_mutex_unlock(&_timer_mu);
            _tin_panic("sleep: timer queue full - raise TINMAXTIMERS");
            return;
        }
        int new_cap = _timer_cap * 2;
        if (new_cap > _timer_max) new_cap = _timer_max;
        TinTimer *nt = (TinTimer *)realloc(_timers, (size_t)new_cap * sizeof(TinTimer));
        if (!nt) {
            pthread_mutex_unlock(&_timer_mu);
            _tin_panic("sleep: timer queue OOM");
            return;
        }
        _timers    = nt;
        _timer_cap = new_cap;
    }
    _timers[_timer_cnt++] = (TinTimer){ deadline, pid };
    pthread_mutex_unlock(&_timer_mu);
}
