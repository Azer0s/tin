// tin runtime - async I/O
//
// A dedicated I/O thread runs epoll_wait (Linux) or kevent (macOS/BSD).
// When a fiber calls _tin_async_read/_tin_async_write and the fd is not
// ready, the fiber is parked (FIBER_BLOCKED) and its pid is registered in
// the watch table.  When the I/O event fires the I/O thread calls
// _tin_fiber_unpark(pid) to re-enqueue the fiber.
//
// After resuming, the Tin-level async_read/async_write wrapper retries the
// syscall, so the C function itself only needs to return INT64_MIN as an
// "io-blocked" sentinel - no retry loop in C.

#include "runtime.h"
#include "fiber.h"
#include "async_io.h"
#include <stdlib.h>
#include <stdio.h>
#include <errno.h>
#include <fcntl.h>
#include <unistd.h>
#include <string.h>
#include <pthread.h>

#define TIN_IO_WATCHES_INIT        256
#define TIN_IO_WATCHES_DEFAULT_MAX (1 << 16)  // 64K

// Sentinel: caller must retry the I/O after being woken.
#define TIN_IO_BLOCKED INT64_MIN

typedef struct {
    int     fd;
    int64_t pid;    // fiber to wake when fd is ready
    int     events; // EPOLLIN / EPOLLOUT equivalent
} TinIOWatch;

static TinIOWatch      *_io_watches    = NULL;
static int              _io_watch_len  = 0;
static int              _io_watch_cap  = 0;
static int              _io_watch_max  = 0;
static pthread_mutex_t  _io_watch_mu   = PTHREAD_MUTEX_INITIALIZER;

static pthread_t _io_thread_id;
static volatile int _io_shutdown = 0;

#if defined(__linux__)
#include <sys/epoll.h>
static int _epoll_fd = -1;
#elif defined(__APPLE__) || defined(__FreeBSD__)
#include <sys/event.h>
static int _kqueue_fd = -1;
#endif

static void _set_nonblocking(int fd) {
    int flags = fcntl(fd, F_GETFL, 0);
    if (flags >= 0) fcntl(fd, F_SETFL, flags | O_NONBLOCK);
}

// Watch-table semantics: each entry is keyed by (fd, events) so a fiber
// parking for read on fd 8 and another fiber parking for write on the
// same fd 8 coexist as TWO entries. The original keying-by-fd-only
// collapsed concurrent read+write parkers into one and lost the older
// one's wakeup, manifesting as UDP server hangs when handle_conn's
// async_write parked on the same fd as main's recv_from_impl read.
//
// events bits: 1 = read (EPOLLIN), 2 = write (EPOLLOUT). The two values
// are kept disjoint so per-direction lookup in the I/O thread is a
// trivial mask check.
static int _io_watch_add_locked(int fd, int64_t pid, int events) {
    for (int i = 0; i < _io_watch_len; i++) {
        if (_io_watches[i].fd == fd && _io_watches[i].events == events) {
            // Same direction on same fd: replace pid (latest parker
            // wins; in practice only one fiber should be parked on a
            // given (fd, direction) at a time).
            _io_watches[i].pid = pid;
            return 1;
        }
    }

    if (_io_watch_len >= _io_watch_cap) {
        if (_io_watch_cap >= _io_watch_max) return 0;

        int new_cap = _io_watch_cap * 2;
        if (new_cap > _io_watch_max) new_cap = _io_watch_max;

        TinIOWatch *nw = (TinIOWatch *)realloc(_io_watches, (size_t)new_cap * sizeof(TinIOWatch));
        if (!nw) return 0;

        _io_watches    = nw;
        _io_watch_cap  = new_cap;
    }

    _io_watches[_io_watch_len++] = (TinIOWatch){ fd, pid, events };
    return 1;
}

// _io_combined_events_locked returns the OR of every event bit currently
// registered for fd. Used so a single epoll registration can carry both
// EPOLLIN and EPOLLOUT when read and write watches coexist on one fd.
// Caller must hold _io_watch_mu.
static int _io_combined_events_locked(int fd) {
    int combined = 0;

    for (int i = 0; i < _io_watch_len; i++) {
        if (_io_watches[i].fd == fd) combined |= _io_watches[i].events;
    }

    return combined;
}

// Register fd for the given events and park the current fiber.
static void _io_park(int fd, int64_t pid, int read_not_write) {
    int events = read_not_write ? 1 : 2;
    int combined = 0;

    pthread_mutex_lock(&_io_watch_mu);

    int ok = _io_watch_add_locked(fd, pid, events);
    if (ok) combined = _io_combined_events_locked(fd);

    pthread_mutex_unlock(&_io_watch_mu);

    if (!ok) {
        _tin_panic("async I/O: watch table full - raise TINMAXIOWATCHES");
        return;
    }

#if defined(__linux__)
    if (_epoll_fd >= 0) {
        struct epoll_event ev;
        // Combined mask covers every direction currently parked on fd,
        // so EPOLL_CTL_MOD never accidentally removes the OTHER
        // direction. Without this, parking write on a fd that already
        // had a read watch would overwrite the read registration and
        // the read parker would never wake.
        uint32_t mask = (uint32_t)(EPOLLET | EPOLLONESHOT);
        if (combined & 1) mask |= EPOLLIN;
        if (combined & 2) mask |= EPOLLOUT;
        ev.events  = mask;
        ev.data.fd = fd;

        if (epoll_ctl(_epoll_fd, EPOLL_CTL_MOD, fd, &ev) < 0)
            epoll_ctl(_epoll_fd, EPOLL_CTL_ADD, fd, &ev);
    }
#elif defined(__APPLE__) || defined(__FreeBSD__)
    if (_kqueue_fd >= 0) {
        // kqueue tracks read and write filters as separate events, so
        // the per-direction registration is naturally independent and
        // doesn't need the combined-mask treatment epoll requires.
        struct kevent kev;
        int filter = read_not_write ? EVFILT_READ : EVFILT_WRITE;
        EV_SET(&kev, (uintptr_t)fd, filter, EV_ADD | EV_ONESHOT, 0, 0, (void *)(intptr_t)pid);
        kevent(_kqueue_fd, &kev, 1, NULL, 0, NULL);
    }
#endif
    // Park the fiber: mark BLOCKED before returning so the worker won't
    // re-enqueue it after coro.suspend fires. The I/O thread calls
    // _tin_fiber_unpark(pid) when the fd is ready.
    if (pid >= 0)
        _tin_fiber_park(pid);
}

// I/O thread: polls for events and wakes blocked fibers.
//
// Per-direction dispatch: an epoll event for fd 8 may carry EPOLLIN,
// EPOLLOUT, or both. For each direction that fired we look up the
// matching (fd, events) watch entry and unpark exactly that fiber.
// After consuming, if any direction remains parked on the fd, we
// re-arm epoll with the remaining mask (EPOLLONESHOT only disarms
// what just fired; the leftover direction needs a fresh registration).
static void *_io_thread_fn(void *_) {
    (void)_;

    while (!_io_shutdown) {
#if defined(__linux__)
        if (_epoll_fd < 0) { usleep(5000); continue; }

        struct epoll_event evs[64];
        int n = epoll_wait(_epoll_fd, evs, 64, 5);

        for (int i = 0; i < n; i++) {
            int fd = evs[i].data.fd;
            uint32_t fired = evs[i].events;

            int64_t read_pid = -1;
            int64_t write_pid = -1;
            int remaining = 0;

            pthread_mutex_lock(&_io_watch_mu);

            // Pop the read watch if read fired.
            if (fired & (EPOLLIN | EPOLLERR | EPOLLHUP)) {
                for (int j = 0; j < _io_watch_len; j++) {
                    if (_io_watches[j].fd == fd && _io_watches[j].events == 1) {
                        read_pid = _io_watches[j].pid;
                        _io_watches[j] = _io_watches[--_io_watch_len];

                        break;
                    }
                }
            }

            // Pop the write watch if write fired.
            if (fired & (EPOLLOUT | EPOLLERR | EPOLLHUP)) {
                for (int j = 0; j < _io_watch_len; j++) {
                    if (_io_watches[j].fd == fd && _io_watches[j].events == 2) {
                        write_pid = _io_watches[j].pid;
                        _io_watches[j] = _io_watches[--_io_watch_len];

                        break;
                    }
                }
            }

            // Compute leftover direction(s) still parked on fd and
            // re-arm epoll INSIDE the lock so a concurrent _io_park
            // for the same fd can't race the registration. Without
            // this, the racing _io_park's MOD could happen first,
            // then this re-arm clobbers it back to a stale mask.
            remaining = _io_combined_events_locked(fd);

            if (remaining != 0) {
                struct epoll_event re;
                uint32_t mask = (uint32_t)(EPOLLET | EPOLLONESHOT);

                if (remaining & 1) mask |= EPOLLIN;
                if (remaining & 2) mask |= EPOLLOUT;

                re.events  = mask;
                re.data.fd = fd;
                epoll_ctl(_epoll_fd, EPOLL_CTL_MOD, fd, &re);
            }

            pthread_mutex_unlock(&_io_watch_mu);

            if (read_pid >= 0)  _tin_fiber_unpark(read_pid);
            if (write_pid >= 0) _tin_fiber_unpark(write_pid);
        }
#elif defined(__APPLE__) || defined(__FreeBSD__)
        if (_kqueue_fd < 0) { usleep(5000); continue; }

        struct timespec ts = { 0, 5000000L }; // 5ms
        struct kevent evs[64];
        int n = kevent(_kqueue_fd, NULL, 0, evs, 64, &ts);

        for (int i = 0; i < n; i++) {
            int64_t pid = (int64_t)(intptr_t)evs[i].udata;
            int fd = (int)evs[i].ident;
            int wantedEvents = (evs[i].filter == EVFILT_READ) ? 1 : 2;

            pthread_mutex_lock(&_io_watch_mu);

            // kqueue events are already per-direction (separate filters
            // for EVFILT_READ vs EVFILT_WRITE), so match (fd, events)
            // exactly to remove the right entry.
            for (int j = 0; j < _io_watch_len; j++) {
                if (_io_watches[j].fd == fd && _io_watches[j].events == wantedEvents) {
                    _io_watches[j] = _io_watches[--_io_watch_len];

                    break;
                }
            }

            pthread_mutex_unlock(&_io_watch_mu);

            if (pid >= 0) _tin_fiber_unpark(pid);
        }
#else
        usleep(5000);
#endif
    }

    return NULL;
}

static int _io_initialized = 0;

void _tin_io_init(void) {
    if (_io_initialized) return;
    _io_initialized = 1;

    const char *env = getenv("TINMAXIOWATCHES");
    _io_watch_max = (env && *env) ? atoi(env) : TIN_IO_WATCHES_DEFAULT_MAX;
    if (_io_watch_max <= 0) _io_watch_max = TIN_IO_WATCHES_DEFAULT_MAX;

    _io_watch_cap = TIN_IO_WATCHES_INIT;
    _io_watches = (TinIOWatch *)malloc((size_t)_io_watch_cap * sizeof(TinIOWatch));
    if (!_io_watches) { fputs("tin: IO watch table OOM\n", stderr); exit(1); }

#if defined(__linux__)
    _epoll_fd = epoll_create1(0);
    if (_epoll_fd < 0) perror("tin: epoll_create1");
#elif defined(__APPLE__) || defined(__FreeBSD__)
    _kqueue_fd = kqueue();
    if (_kqueue_fd < 0) perror("tin: kqueue");
#endif
    _io_shutdown = 0;
    pthread_create(&_io_thread_id, NULL, _io_thread_fn, NULL);
}

void _tin_io_shutdown(void) {
    if (!_io_initialized) return;
    _io_shutdown = 1;
    pthread_join(_io_thread_id, NULL);
    _io_initialized = 0;
    free(_io_watches);
    _io_watches   = NULL;
    _io_watch_cap = 0;
    _io_watch_len = 0;
#if defined(__linux__)
    if (_epoll_fd >= 0) { close(_epoll_fd); _epoll_fd = -1; }
#elif defined(__APPLE__) || defined(__FreeBSD__)
    if (_kqueue_fd >= 0) { close(_kqueue_fd); _kqueue_fd = -1; }
#endif
}

// _tin_async_read: non-blocking read from fd.
//
// Sets the fd to non-blocking mode, attempts read(2).  On EAGAIN/EWOULDBLOCK
// the current fiber is parked (FIBER_BLOCKED) and the fd is registered with
// the epoll/kqueue I/O thread so it will be unparked when data arrives.
// Returns TIN_IO_BLOCKED (INT64_MIN) as a sentinel so the Tin async_read
// wrapper (in stdlib/io/io.tin) retries via yield + loop.
//
// This works correctly with coroutine chaining: the outer $coro's drive loop
// sees the inner coro yield (because inner hit yield after getting the sentinel),
// suspends the outer coro, and the worker parks the outer fiber.  When epoll
// fires, _tin_fiber_unpark re-enqueues the outer fiber.
//
// Returns bytes read (>=0), TIN_IO_BLOCKED (fd not ready), or -errno on error.
int64_t _tin_async_read(int fd, void *buf, int64_t n) {
    _set_nonblocking(fd);
    ssize_t r;
    do { r = read(fd, buf, (size_t)n); } while (r < 0 && errno == EINTR);
    if (r >= 0) return (int64_t)r;
    if (errno == EAGAIN || errno == EWOULDBLOCK) {
        int64_t pid = _tin_current_pid();
        if (pid >= 0) _io_park(fd, pid, 1 /* read */);
        return TIN_IO_BLOCKED;
    }
    return -(int64_t)errno;
}

// _tin_async_write: non-blocking write to fd.
// See _tin_async_read for the rationale.
int64_t _tin_async_write(int fd, const void *buf, int64_t n) {
    _set_nonblocking(fd);
    ssize_t r;
    do { r = write(fd, buf, (size_t)n); } while (r < 0 && errno == EINTR);
    if (r >= 0) return (int64_t)r;
    if (errno == EAGAIN || errno == EWOULDBLOCK) {
        int64_t pid = _tin_current_pid();
        if (pid >= 0) _io_park(fd, pid, 0 /* write */);
        return TIN_IO_BLOCKED;
    }
    return -(int64_t)errno;
}

// Legacy poll entry point (kept for compatibility; no longer drives the loop).
void _tin_io_poll(int timeout_ms) {
    (void)timeout_ms;
}

// Returns the INT64_MIN sentinel used by _tin_async_read/_tin_async_write to
// indicate "io-blocked, retry after wakeup". Exposed so Tin wrappers can compare
// without embedding a magic literal.
int64_t _tin_io_blocked_val(void) {
    return TIN_IO_BLOCKED;
}

// Exported wrappers for use from stdlib C files compiled outside the runtime
// umbrella (e.g. stdlib/tcp/tcp.c, stdlib/udp/udp.c, stdlib/unix/unix.c).

void _tin_set_nonblocking(int fd) {
    _set_nonblocking(fd);
}

int64_t _tin_async_park_read(int fd) {
    int64_t pid = _tin_current_pid();
    if (pid >= 0) _io_park(fd, pid, 1 /* read */);
    return TIN_IO_BLOCKED;
}

int64_t _tin_async_park_write(int fd) {
    int64_t pid = _tin_current_pid();
    if (pid >= 0) _io_park(fd, pid, 0 /* write */);
    return TIN_IO_BLOCKED;
}
