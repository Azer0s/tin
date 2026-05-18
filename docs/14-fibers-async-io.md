# Async I/O, sleep, spawn-do, TCP helpers

## `async` macro shorthand

The `async` macro from `stdlib/macros` is a convenient shorthand for
`fn{#async}`:

```rust
use { loop, async } from macros

async handle(fd i32) =   // same as fn{#async} handle(fd i32) =
  loop:
    // ...
```

`async` and `loop` are commonly imported together for fiber-based I/O code.

---

## sleep

Suspend the current fiber for at least `ms` milliseconds without blocking the
worker thread. `io::sleep` returns `Future[Unit]` - use `await io::sleep(ms)`:

```rust
use io

fn{#async} delayed_hello() =
  await io::sleep(500)   // sleep 500ms; other fibers run during this time
  echo "hello after 500ms"
```

---

## spawn do: blocks

`spawn do:` starts an anonymous fiber without a named function.
Captured local variables are safely copied into an env struct before
the fiber starts (ARC-retained if they are reference-counted types):

```rust
use sync

fn main() =
  let results = sync::Channel[string].make(3)
  let i i64 = 0
  for i < 3:
    let idx = i          // capture by value - each fiber gets its own copy
    let tag = "item-{i}"
    spawn do:
      results.send("{tag}-done-{idx}")
    i = i + 1
  let j i64 = 0
  for j < 3:
    echo await results.recv()
    j = j + 1
```

All captured local variables (including strings and other ARC-managed values)
are safe to use inside the `spawn do:` body - the runtime retains them before
the parent scope exits and releases them after the fiber finishes.

---

## Async I/O

Non-blocking I/O integrates with the fiber scheduler via `stdlib/io`. When a
read or write would block (EAGAIN), the fiber is parked and the underlying
file descriptor is registered with **epoll** (Linux) or **kqueue**
(macOS/FreeBSD). The fiber is automatically woken when the fd becomes ready.

```rust
use io

fn{#async} handle_conn(fd i32) =
  let buf [byte; 4096]
  loop:
    let n = await spawn io::async_read(fd, &buf[0], 4096)
    if n <= 0:
      break
    await io::write_all(fd, &buf[0], n)
  io::close(fd)
```

`io::async_read` is a low-level `{#async}` primitive used with `await spawn`.
`io::write_all` returns `Future[Unit]` and is awaited directly.

### Async I/O functions

| Function      | Signature                                   | How to call   | Description                                      |
|---------------|---------------------------------------------|---------------|--------------------------------------------------|
| `async_read`  | `fn{#async}(fd i32, buf *byte, n i64) i64`  | `await spawn` | Non-blocking read; suspends if not ready         |
| `async_write` | `fn{#async}(fd i32, buf *byte, n i64) i64`  | `await spawn` | Non-blocking write; suspends if not ready        |
| `write_all`   | `fn(fd i32, buf *byte, n i64) Future[Unit]` | `await`       | Write all bytes, retrying partial writes         |
| `read_exact`  | `fn(fd i32, buf *byte, n i64) Future[i64]`  | `await`       | Read exactly n bytes, retrying until full or EOF |
| `sleep`       | `fn(ms i64) Future[Unit]`                   | `await`       | Suspend fiber for ms milliseconds                |

All are in `stdlib/io` and accessed via `io::name`.

### `{#blocking}` warning

If you call a blocking C extern inside an `{#async}` function, the compiler
emits a warning:

```rust
fn raw_read(fd i32, buf *byte, n i64) i64 = extern("read") {#blocking}

fn{#async} example(fd i32) =
  let buf [byte; 64]
  raw_read(fd, &buf[0], 64)
  // warning: calling blocking extern "raw_read" inside an {#async} function
```

Use `io::async_read` / `io::async_write` instead to avoid blocking the worker
thread.

---

## TCP helpers

`stdlib/io` includes thin wrappers around POSIX socket helpers:

```rust
use io

let listen_fd = io::tcp_listen(8080)       // create TCP listen socket; returns fd or -errno
let conn_fd   = io::tcp_accept(listen_fd)  // blocking accept; returns conn fd or -errno
io::close(fd)                              // close any file descriptor
```

| Function                        | Description                                         |
|---------------------------------|-----------------------------------------------------|
| `tcp_listen(port i32) i32`      | Create + bind + listen; returns listen fd or -errno |
| `tcp_accept(listen_fd i32) i32` | Accept next connection; returns conn fd or -errno   |
| `close(fd i32)`                 | Close a file descriptor                             |

### Echo server example (low-level fd)

```rust
use { loop, async } from macros
use io

async handle_conn(fd i32) =
  let buf [byte; 4096]
  loop:
    let n = await spawn io::async_read(fd, &buf[0], 4096)
    if n <= 0:
      break
    await io::write_all(fd, &buf[0], n)
  io::close(fd)

fn main() =
  let listen_fd = io::tcp_listen(8080)
  if listen_fd < 0:
    echo "listen failed"
    return
  echo "listening on :8080"
  loop:
    let conn_fd = io::tcp_accept(listen_fd)
    if conn_fd < 0:
      break
    spawn handle_conn(conn_fd)
```

### Echo server example (tcp + ioutil)

The `tcp` and `ioutil` packages provide a higher-level API:

- `tcp::Conn.read` and `tcp::Conn.write` return `Future[i64]` - await directly.
- `ioutil::read_string` and `ioutil::write_string` return `Future[T]` - await directly.
- `tcp::Server.accept` returns `(Conn, bool)` - destructure with `let (c, ok) = srv.accept()`.

See `examples/echo_server/echo_server.tin`:

```rust
use { loop, async } from macros
use ioutil
use tcp

async handle_conn(conn tcp::Conn) =
  defer conn.close()
  loop:
    let line = await ioutil::read_string(conn)
    if len(line) == 0:
      return
    await ioutil::write_string(conn, "ECHO: {line}\n")

fn main() =
  let srv = tcp::listen(8080)
  echo "listening on :8080"
  loop:
    let (c, ok) = srv.accept()
    if !ok:
      break
    spawn handle_conn(c)
```

---

