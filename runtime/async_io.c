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

#define TIN_IO_MAX_WATCHES 4096

// Sentinel: caller must retry the I/O after being woken.
#define TIN_IO_BLOCKED INT64_MIN

typedef struct {
    int     fd;
    int64_t pid;    // fiber to wake when fd is ready
    int     events; // EPOLLIN / EPOLLOUT equivalent
} TinIOWatch;

static TinIOWatch       _io_watches[TIN_IO_MAX_WATCHES];
static int              _io_watch_len = 0;
static pthread_mutex_t  _io_watch_mu  = PTHREAD_MUTEX_INITIALIZER;

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

// Add or update a watch entry.  Must be called with _io_watch_mu held.
static void _io_watch_add_locked(int fd, int64_t pid, int events) {
    for (int i = 0; i < _io_watch_len; i++) {
        if (_io_watches[i].fd == fd) {
            _io_watches[i].pid    = pid;
            _io_watches[i].events = events;
            return;
        }
    }
    if (_io_watch_len < TIN_IO_MAX_WATCHES)
        _io_watches[_io_watch_len++] = (TinIOWatch){ fd, pid, events };
}

// Register fd for the given events and park the current fiber.
static void _io_park(int fd, int64_t pid, int read_not_write) {
    int events = read_not_write ? 1 : 2;

    pthread_mutex_lock(&_io_watch_mu);
    _io_watch_add_locked(fd, pid, events);
    pthread_mutex_unlock(&_io_watch_mu);

#if defined(__linux__)
    if (_epoll_fd >= 0) {
        struct epoll_event ev;
        ev.events  = read_not_write
                     ? (uint32_t)(EPOLLIN  | EPOLLET | EPOLLONESHOT)
                     : (uint32_t)(EPOLLOUT | EPOLLET | EPOLLONESHOT);
        ev.data.fd = fd;
        // Use MOD if already registered; ADD otherwise.  Ignore errors.
        if (epoll_ctl(_epoll_fd, EPOLL_CTL_MOD, fd, &ev) < 0)
            epoll_ctl(_epoll_fd, EPOLL_CTL_ADD, fd, &ev);
    }
#elif defined(__APPLE__) || defined(__FreeBSD__)
    if (_kqueue_fd >= 0) {
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
static void *_io_thread_fn(void *_) {
    (void)_;
    while (!_io_shutdown) {
#if defined(__linux__)
        if (_epoll_fd < 0) { usleep(5000); continue; }
        struct epoll_event evs[64];
        int n = epoll_wait(_epoll_fd, evs, 64, 5);
        for (int i = 0; i < n; i++) {
            int fd = evs[i].data.fd;
            int64_t pid = -1;
            pthread_mutex_lock(&_io_watch_mu);
            for (int j = 0; j < _io_watch_len; j++) {
                if (_io_watches[j].fd == fd) {
                    pid = _io_watches[j].pid;
                    // Remove watch (EPOLLONESHOT already disarmed kernel side).
                    _io_watches[j] = _io_watches[--_io_watch_len];
                    break;
                }
            }
            pthread_mutex_unlock(&_io_watch_mu);
            if (pid >= 0) _tin_fiber_unpark(pid);
        }
#elif defined(__APPLE__) || defined(__FreeBSD__)
        if (_kqueue_fd < 0) { usleep(5000); continue; }
        struct timespec ts = { 0, 5000000L }; // 5ms
        struct kevent evs[64];
        int n = kevent(_kqueue_fd, NULL, 0, evs, 64, &ts);
        for (int i = 0; i < n; i++) {
            int64_t pid = (int64_t)(intptr_t)evs[i].udata;
            int fd = (int)evs[i].ident;
            pthread_mutex_lock(&_io_watch_mu);
            for (int j = 0; j < _io_watch_len; j++) {
                if (_io_watches[j].fd == fd) {
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
