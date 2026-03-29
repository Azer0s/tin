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
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <pthread.h>
#include <time.h>

#define TIN_MAX_TIMERS 1024

typedef struct {
    int64_t deadline_ms;  // absolute deadline in milliseconds since epoch
    int64_t pid;          // fiber to wake
} TinTimer;

static TinTimer        _timers[TIN_MAX_TIMERS];
static int             _timer_cnt = 0;
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
    _timer_shutdown = 0;
    pthread_create(&_timer_thread_id, NULL, _timer_thread_fn, NULL);
}

void _tin_timer_shutdown(void) {
    if (!_timer_initialized) return;
    _timer_shutdown = 1;
    pthread_join(_timer_thread_id, NULL);
    _timer_initialized = 0;
}

// Park the current fiber for `ms` milliseconds.
// Must be called from within a fiber (coro.suspend fires after this returns).
void _tin_sleep_ms(int64_t ms) {
    int64_t pid = _tin_current_pid();
    if (pid < 0 || ms <= 0) return;  // not in a fiber or no-op

    int64_t deadline = _now_ms() + ms;

    // Park the fiber BEFORE adding to the timer list. This way, even if the
    // timer fires immediately in another thread, _tin_fiber_unpark will see
    // BLOCKED and enqueue exactly once. The worker then sees RUNNABLE (not
    // RUNNING) and skips its own re-enqueue.
    _tin_fiber_park(pid);

    pthread_mutex_lock(&_timer_mu);
    if (_timer_cnt < TIN_MAX_TIMERS) {
        _timers[_timer_cnt++] = (TinTimer){ deadline, pid };
    }
    pthread_mutex_unlock(&_timer_mu);
}
