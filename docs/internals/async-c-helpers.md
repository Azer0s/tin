# Internals: Async C Helpers

How to write C helper functions that cooperate with Tin's fiber scheduler
so callers can `await` them without blocking a worker thread.

---

## The pattern in one sentence

The C helper sets the fd non-blocking, returns `INT64_MIN` ("io-blocked")
on EAGAIN, and calls `_io_park(fd, pid)` to register for an epoll/kqueue
wakeup. The Tin wrapper loops with `yield` between retries.

---

## Full example: a custom async recv

### C side (`my_proto.c`)

```c
#include "runtime.h"    // _tin_current_pid, _tin_async_park_read/write
#include <unistd.h>
#include <errno.h>

// Returns bytes read (>= 0), INT64_MIN if fd not ready, or -errno on error.
int64_t _my_proto_recv(int32_t fd, void *buf, int64_t n) {
    _tin_set_nonblocking(fd);          // set O_NONBLOCK (idempotent)

    ssize_t r;
    do { r = read(fd, buf, (size_t)n); } while (r < 0 && errno == EINTR);

    if (r >= 0) return (int64_t)r;     // success
    if (errno == EAGAIN || errno == EWOULDBLOCK)
        return _tin_async_park_read(fd); // park + return INT64_MIN
    return -(int64_t)errno;            // hard error
}
```

For write-side helpers, replace `read` with `write` and use
`_tin_async_park_write(fd)`.

### Tin side (`my_proto.tin`)

```rust
//!+my_proto.c -- -I ../../runtime   // compile the C file with this package

fn _my_proto_recv_c(fd i32, buf *byte, n i64) i64 = extern("_my_proto_recv")
fn _tin_io_blocked_val_c() i64 = extern("_tin_io_blocked_val")

// recv reads up to n bytes from fd. Use: `let r = await spawn recv(fd, buf, n)`
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
            2. epoll_ctl(fd, EPOLLIN | EPOLLONESHOT)  [or kevent on BSD]
            3. _tin_fiber_park(pid)  ->  pending_park = 1
            return INT64_MIN
       |
       v
Tin wrapper sees sentinel, executes `yield`
       |
       v
Worker: after coro.suspend
  pending_park && !pending_wakeup  ->  status = BLOCKED, fiber off run queue
       |
       v
       ...other fibers run...
       |
       v
I/O thread: epoll_wait fires
  _tin_fiber_unpark(pid)  ->  status = RUNNABLE, push to run queue
       |
       v
Worker resumes fiber, Tin loop retries C helper
  fd is ready -> returns bytes
```

The `pending_park`/`pending_wakeup` flags prevent a race where the I/O
event fires between `_tin_async_park_read` and the `yield`: in that case
`pending_wakeup` is set, the worker sees it and re-enqueues the fiber
immediately instead of blocking.

---

## Runtime helpers available to C files

Declared in `runtime/async_io.h` and `runtime/fiber.h`. Link by adding
`-- -I ../../runtime` (or the appropriate relative path) to the `//!+file.c`
directive.

| Function | Purpose |
|---|---|
| `_tin_set_nonblocking(fd)` | Set `O_NONBLOCK` on an fd (idempotent) |
| `_tin_async_park_read(fd)` | Park current fiber for read readiness; return `INT64_MIN` |
| `_tin_async_park_write(fd)` | Park current fiber for write readiness; return `INT64_MIN` |
| `_tin_io_blocked_val()` | Return the `INT64_MIN` sentinel (for Tin-side comparison) |
| `_tin_current_pid()` | Return the current fiber's pid, or -1 if not in a fiber |

`_tin_async_park_read` and `_tin_async_park_write` are the composites: they
call `_tin_current_pid`, `_io_park`, and return `INT64_MIN` in one shot. Use
them instead of assembling the pieces manually.

---

## Non-async C helpers (socket setup, etc.)

For operations that don't need to yield (socket creation, bind, listen,
accept on a pre-connected fd), just return `i32` or `i64` directly:

- Success: positive value or 0
- Error: `-errno` (negative; Tin checks `fd < 0`)

No parking, no sentinel. See `stdlib/tcp/tcp.c` for examples.

---

## Wrapping in a higher-level struct

Once you have the `{#async}` primitive, expose it through a struct method
that returns `Future[T]` so callers can use `await` without `spawn`:

```rust
struct MyConn =
  fd i32

  // read returns Future[i64] so callers write: let n = await conn.read(buf, n)
  fn read(this MyConn, buf *byte, n i64) Future[i64] =
    return spawn recv(this.fd, buf, n)

  fn close(this MyConn) =
    io::close(this.fd)
```

The `tcp::Conn` struct in `stdlib/tcp/tcp.tin` follows this pattern
exactly, delegating to `io::async_read` and `io::async_write`.

---

## Retry is in Tin, not C

The C helper returns the sentinel and returns immediately. The retry loop is
entirely in the `{#async}` Tin wrapper. This is intentional: the retry must
cross a `yield` point so the fiber actually suspends and frees its worker
thread. A retry loop in C would spin-block the thread.

---

## Key files

| File | Role |
|---|---|
| `runtime/async_io.c` | I/O thread, epoll/kqueue, `_io_park`, sentinel value |
| `runtime/async_io.h` | Public exports for stdlib C files |
| `runtime/fiber.c` | `_tin_fiber_park`, `_tin_fiber_unpark`, worker loop |
| `stdlib/io/io.tin` | `async_read`, `async_write`, `sleep` - canonical retry loops |
| `stdlib/tcp/tcp.c` | Non-async socket creation (listen, accept) |
| `stdlib/tcp/tcp.tin` | `Conn` struct wrapping `io::async_read/write` |
