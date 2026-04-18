# Internals: Async C Helpers

How to write C helper functions that cooperate with Tin's fiber scheduler
so callers can `await` them without blocking a worker thread.

---

## The pattern in one sentence

The C helper sets the fd non-blocking, returns `INT64_MIN` ("io-blocked")
on EAGAIN, and calls `_tin_async_park_read(fd)` or `_tin_async_park_write(fd)`
to register with epoll/kqueue. The Tin wrapper loops with `yield` between
retries.

---

## Full example: a custom async recv

### C side (`my_proto.c`)

```c
#include "runtime.h"
#include "async_io.h"
#include <unistd.h>
#include <errno.h>

#define TIN_IO_BLOCKED INT64_MIN

// Returns bytes read (>= 0), TIN_IO_BLOCKED if fd not ready, or -errno on error.
int64_t _my_proto_recv(int32_t fd, void *buf, int64_t n) {
    _tin_set_nonblocking(fd);

    ssize_t r;
    do { r = read(fd, buf, (size_t)n); } while (r < 0 && errno == EINTR);

    if (r >= 0) return (int64_t)r;
    if (errno == EAGAIN || errno == EWOULDBLOCK)
        return _tin_async_park_read(fd);   // park fiber + return INT64_MIN
    return -(int64_t)errno;
}
```

For write-side helpers, replace `read` with `write` and use
`_tin_async_park_write(fd)`.

### Tin side (`my_proto.tin`)

```rust
//!+my_proto.c -- -I $TIN_RUNTIME

use { loop } from macros
use io

fn _my_proto_recv_c(fd i32, buf *byte, n i64) i64 = extern("_my_proto_recv")
fn _tin_io_blocked_val_c() i64 = extern("_tin_io_blocked_val")

// recv reads up to n bytes from fd.
// Low-level: call with `let r = await spawn recv(fd, buf, n)`.
fn{#async} recv(fd i32, buf *byte, n i64) i64 =
  let blocked = _tin_io_blocked_val_c()
  loop:
    let r = _my_proto_recv_c(fd, buf, n)
    if r != blocked:
      return r
    yield
```

---

## How it works

```
Fiber calls C helper
       |
       v
 EAGAIN?
  yes --> _tin_async_park_read(fd)
            1. add (fd, pid) to watch table
            2. epoll_ctl(EPOLLIN | EPOLLONESHOT)  [or kevent on BSD]
            3. _tin_fiber_park(pid)  ->  pending_park = 1
            return INT64_MIN
       |
       v
Tin wrapper sees sentinel, executes `yield`
       |
       v
Worker: after coro.suspend
  pending_park && !pending_wakeup  ->  status = BLOCKED, off run queue
       |
       v
       ...other fibers run...
       |
       v
I/O thread: epoll_wait fires
  _tin_fiber_unpark(pid)  ->  status = RUNNABLE, push to run queue
       |
       v
Worker resumes fiber, Tin loop retries C helper -> fd ready, return bytes
```

`pending_park`/`pending_wakeup` prevent a race where the I/O event fires
between `_tin_async_park_read` and the `yield`: `pending_wakeup` is set,
the worker sees it and re-enqueues immediately instead of blocking.

---

## Runtime helpers available to C files

Include both `"runtime.h"` and `"async_io.h"`. Link with `-- -I $TIN_RUNTIME`
in the `//!+file.c` directive.

| Function | Purpose |
|---|---|
| `_tin_set_nonblocking(fd)` | Set `O_NONBLOCK` on an fd (idempotent) |
| `_tin_async_park_read(fd)` | Park current fiber for read, return `INT64_MIN` |
| `_tin_async_park_write(fd)` | Park current fiber for write, return `INT64_MIN` |
| `_tin_io_blocked_val()` | Return `INT64_MIN` (for Tin-side comparison) |
| `_tin_current_pid()` | Current fiber pid, or -1 if not in a fiber |

`_tin_async_park_read/write` are the composites: they call `_tin_current_pid`,
register with epoll/kqueue, and return `INT64_MIN` in one shot.

---

## Error conventions

**Raw syscall helpers** (socket read/write): return `-errno` on hard error.
The caller checks `r < 0 && r != blocked`.

**Library-specific helpers** (e.g. TLS via OpenSSL `SSL_read`): return `-1`
on error, since OpenSSL error codes don't map to errno. The `WANT_READ` and
`WANT_WRITE` states both map to `_tin_async_park_read/write` as appropriate:

```c
int r = SSL_read(ssl, buf, (int)n);
if (r > 0) return (int64_t)r;
int err = SSL_get_error(ssl, r);
if (err == SSL_ERROR_WANT_READ)  return _tin_async_park_read(fd);
if (err == SSL_ERROR_WANT_WRITE) return _tin_async_park_write(fd);
return -1;
```

See `stdlib/tls/tls_impl.c` for the full example.

---

## Non-async C helpers (socket setup, etc.)

For operations that don't yield (socket creation, bind, listen, accept):
return `i32` with success as a positive fd and error as `-errno`. No parking,
no sentinel. See `stdlib/tcp/tcp.c`.

---

## Wrapping in a higher-level struct

Expose the `{#async}` primitive through a struct method that returns
`Future[T]` so callers use plain `await`:

```rust
struct MyConn (io::AsyncReader, io::AsyncWriter) =
  fd i32

  // read returns Future[i64] - use `await conn.read(buf, n)`
  fn read(this MyConn, buf *byte, n i64) Future[i64] =
    return spawn recv(this.fd, buf, n)

  fn close(this MyConn) =
    io::close(this.fd)
```

`tcp::Conn` and `tls::TlsConn` follow this exact pattern.

---

## Retry is in Tin, not C

The C helper returns `INT64_MIN` and returns immediately - no retry loop in
C. The retry crosses a `yield` in Tin, which is what actually suspends the
fiber and frees the worker thread. A C-side spin loop would block the thread.

---

## Key files

| File | Role |
|---|---|
| `runtime/async_io.c` | I/O thread, epoll/kqueue, `_io_park`, sentinel value |
| `runtime/async_io.h` | Exports for stdlib C files |
| `runtime/fiber.c` | `_tin_fiber_park`, `_tin_fiber_unpark`, worker loop |
| `stdlib/io/io.tin` | `async_read`, `async_write` - canonical retry loops |
| `stdlib/tcp/tcp.c` | Non-async socket setup (listen, accept) |
| `stdlib/tcp/tcp.tin` | `Conn` struct wrapping `io::async_read/write` |
| `stdlib/tls/tls_impl.c` | OpenSSL example with `WANT_READ`/`WANT_WRITE` |
