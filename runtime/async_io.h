#pragma once
// tin runtime - async I/O (dedicated epoll/kqueue I/O thread)

#include <stdint.h>

// Initialize the async I/O event loop. Called automatically by _tin_fiber_init.
void _tin_io_init(void);

// Shut down the I/O thread. Called automatically by _tin_fiber_run.
void _tin_io_shutdown(void);

// Non-blocking read.  Returns bytes read (>=0), -errno on hard error,
// or INT64_MIN if the fd is not ready (caller must yield and retry).
int64_t _tin_async_read(int fd, void *buf, int64_t n);

// Non-blocking write.  Same sentinel convention as _tin_async_read.
int64_t _tin_async_write(int fd, const void *buf, int64_t n);

// Returns the INT64_MIN sentinel so Tin wrappers can compare without embedding the literal.
int64_t _tin_io_blocked_val(void);

// Legacy stub -- no-op; kept for source compatibility.
void _tin_io_poll(int timeout_ms);
