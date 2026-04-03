# Networking - io, ioutil, tcp, udp, unix

Tin's networking stack is built on top of a non-blocking I/O system backed by
epoll (Linux) or kqueue (macOS/FreeBSD). Blocking operations park the calling
fiber so other fibers can run; no OS thread is tied up waiting.

All networking stdlib requires linking with `-lm` when using `tin build` or
`tin test`.

---

## io

`stdlib/io` is the foundation: raw async reads/writes, sleep, file I/O, and
the `AsyncReader`/`AsyncWriter` traits that higher-level packages build on.

```rust
use io
```

### Async I/O primitives

| Function                                     | How to call   | Description                                   |
|----------------------------------------------|---------------|-----------------------------------------------|
| `async_read(fd i32, buf *byte, n i64) i64`   | `await spawn` | Non-blocking read; parks fiber if not ready   |
| `async_write(fd i32, buf *byte, n i64) i64`  | `await spawn` | Non-blocking write; parks fiber if not ready  |
| `write_all(fd i32, buf *byte, n i64) Future[Unit]` | `await` | Write all bytes, retrying partial writes      |
| `read_exact(fd i32, buf *byte, n i64) Future[i64]` | `await` | Read exactly n bytes or until EOF             |
| `sleep(ms i64) Future[Unit]`                 | `await`       | Suspend fiber for at least ms milliseconds    |
| `close(fd i32)`                              | direct call   | Close a file descriptor                       |

```rust
use io
use mem

fn{#async} echo_fd(fd i32) =
  let buf = mem::malloc(4096) as *byte
  loop:
    let n = await spawn io::async_read(fd, buf, 4096)
    if n <= 0: break
    await io::write_all(fd, buf, n)
  mem::free(buf as *void)
  io::close(fd)
```

### AsyncReader / AsyncWriter traits

```rust
trait AsyncReader =
  fn read(this *AsyncReader, buf *byte, n i64) Future[i64] = virtual

trait AsyncWriter =
  fn write(this *AsyncWriter, buf *byte, n i64) Future[i64] = virtual
```

Any struct that implements `read` / `write` with those signatures can be used
where `io::AsyncReader` / `io::AsyncWriter` is expected - including with
`ioutil` functions.

### Monotonic clock

```rust
let t0 = io::now_ms()   // milliseconds
let t1 = io::now_us()   // microseconds
```

---

## ioutil

`stdlib/ioutil` provides line-oriented and byte-oriented I/O on top of any
`io::AsyncReader` / `io::AsyncWriter` (or a raw fd `i32`). All functions are
overloaded so the same name works for both.

```rust
use ioutil
```

### String I/O

| Function | Signature | Description |
|----------|-----------|-------------|
| `read_string` | `(fd i32) Future[string]` | Read until newline or EOF |
| `read_string` | `(r AsyncReader) Future[string]` | Same, from any reader |
| `read_string_until` | `(fd i32, delim byte) Future[string]` | Read until delimiter or EOF; pass `0` for EOF-only |
| `read_string_until` | `(r AsyncReader, delim byte) Future[string]` | Same, from any reader |
| `write_string` | `(fd i32, s string) Future[Unit]` | Write all bytes of s |
| `write_string` | `(w AsyncWriter, s string) Future[Unit]` | Same, to any writer |

The delimiter is stripped from the result. `\r` preceding the delimiter is
also stripped (for CRLF line endings).

```rust
use ioutil
use tcp

fn{#async} handle(conn tcp::Conn) =
  defer conn.close()
  loop:
    let line = await ioutil::read_string(conn)
    if len(line) == 0: return
    await ioutil::write_string(conn, "ECHO: {line}\n")
```

### Byte I/O

| Function | Signature | Description |
|----------|-----------|-------------|
| `read_bytes` | `(fd i32, n i64) Future[[byte]]` | Read exactly n bytes |
| `read_bytes` | `(r AsyncReader, n i64) Future[[byte]]` | Same, from any reader |
| `read_bytes_until` | `(fd i32, delim byte) Future[[byte]]` | Read bytes until delimiter |
| `read_bytes_until` | `(r AsyncReader, delim byte) Future[[byte]]` | Same, from any reader |
| `write_bytes` | `(fd i32, data [byte]) Future[Unit]` | Write all bytes of slice |
| `write_bytes` | `(w AsyncWriter, data [byte]) Future[Unit]` | Same, to any writer |

```rust
let data = await ioutil::read_bytes(conn, 256)   // read exactly 256 bytes
await ioutil::write_bytes(conn, data)             // write them back
```

---

## tcp

`stdlib/tcp` wraps POSIX TCP sockets into `Server` and `Conn` types that
integrate with the fiber scheduler and `ioutil`.

```rust
use tcp
```

### Server

```rust
let srv = tcp::listen(8080)   // panics if the port cannot be bound
```

`Server.accept()` blocks the calling fiber until the next client connects,
then returns `(Conn, bool)`. The boolean is `false` only on hard error.

```rust
fn{#async} main() =
  let srv = tcp::listen(8080)
  loop:
    let (c, ok) = srv.accept()
    if !ok: break
    spawn handle_conn(c)
```

### Conn

`tcp::Conn` implements `io::AsyncReader` and `io::AsyncWriter`.

| Method | Returns | Description |
|--------|---------|-------------|
| `read(buf *byte, n i64)` | `Future[i64]` | Read up to n bytes |
| `write(buf *byte, n i64)` | `Future[i64]` | Write up to n bytes |
| `close()` | - | Close the connection |
| `valid()` | `bool` | Returns false if fd < 0 |

Use `ioutil` for line-oriented access:

```rust
use { loop, async } from macros
use ioutil
use tcp

async handle_conn(conn tcp::Conn) =
  defer conn.close()
  loop:
    let line = await ioutil::read_string(conn)
    if len(line) == 0: return
    await ioutil::write_string(conn, "ECHO: {line}\n")

fn{#async} main() =
  let srv = tcp::listen(8080)
  echo "listening on :8080"
  loop:
    let (c, ok) = srv.accept()
    if !ok: break
    spawn handle_conn(c)
```

---

## udp

`stdlib/udp` wraps UDP sockets into `Server` and `Conn` types mirroring the
TCP API.

```rust
use udp
```

### Server

```rust
let srv = udp::listen(8082)
```

`Server.accept()` parks the calling fiber until a datagram arrives, then
returns `(Conn, bool)`. The runtime always allocates a full UDP-max receive
buffer (65536 bytes) and returns only the bytes that were actually received.

```rust
fn{#async} main() =
  let srv = udp::listen(8082)
  loop:
    let (c, ok) = await srv.accept()
    if ok:
      spawn handle_conn(c)
```

### Conn

A `udp::Conn` returned by `accept` carries the received datagram in `c.data`
and the sender's address in `c.host` / `c.port`.

| Field/Method | Type / Returns | Description |
|--------------|----------------|-------------|
| `data` | `[byte]` | Datagram received during `accept` (empty for dial-based Conn) |
| `host` | `string` | Peer IP address |
| `port` | `i32` | Peer port |
| `write(data [byte])` | `Future[i64]` | Send bytes to the peer |
| `write(buf *byte, n i64)` | `Future[i64]` | Same (raw pointer form; AsyncWriter) |
| `read(buf *byte, n i64)` | `Future[i64]` | Receive from peer's fd (AsyncReader) |
| `close()` | - | Closes the socket (no-op for accept-based Conn) |
| `valid()` | `bool` | Returns false if fd < 0 |

`udp::Conn` implements `io::AsyncReader` and `io::AsyncWriter`, so it works
with `ioutil` for dial-based connections.

```rust
use { loop, async } from macros
use udp

async handle_conn(c udp::Conn) =
  await c.write(c.data)   // echo datagram back to sender

fn{#async} main() =
  let srv = udp::listen(8082)
  echo "udp echo listening on :8082"
  loop:
    let (c, ok) = await srv.accept()
    if ok:
      spawn handle_conn(c)
```

### dial

`udp::dial` creates a connected UDP socket bound to a fixed peer:

```rust
let c = udp::dial("127.0.0.1", 8082)
await c.write(buf, n)
let got = await ioutil::read_bytes(c, 4096)
c.close()
```

---

## unix

`stdlib/unix` wraps Unix domain socket connections into `Server` and `Conn`
types that mirror the `tcp` package. Use it for low-latency IPC on the same
host.

```rust
use unix
```

### Server

```rust
let srv = unix::listen("/tmp/myapp.sock")   // removes stale socket; panics on error
```

`Server.accept()` blocks the calling fiber until the next client connects.

```rust
fn{#async} main() =
  let srv = unix::listen("/tmp/myapp.sock")
  loop:
    let (c, ok) = srv.accept()
    if !ok: break
    spawn handle_conn(c)
```

### Conn

`unix::Conn` implements `io::AsyncReader` and `io::AsyncWriter`. It is
identical in interface to `tcp::Conn`.

| Method | Returns | Description |
|--------|---------|-------------|
| `read(buf *byte, n i64)` | `Future[i64]` | Read up to n bytes |
| `write(buf *byte, n i64)` | `Future[i64]` | Write up to n bytes |
| `close()` | - | Close the connection |
| `valid()` | `bool` | Returns false if fd < 0 |

```rust
use { loop, async } from macros
use ioutil
use unix

async handle_conn(conn unix::Conn) =
  defer conn.close()
  loop:
    let line = await ioutil::read_string(conn)
    if len(line) == 0 || line == "quit": return
    await ioutil::write_string(conn, "ECHO: {line}\n")

fn{#async} main() =
  let srv = unix::listen("/tmp/tin_echo.sock")
  echo "listening on /tmp/tin_echo.sock"
  loop:
    let (c, ok) = srv.accept()
    if !ok: break
    spawn handle_conn(c)
```

### dial

```rust
let c = unix::dial("/tmp/myapp.sock")
await ioutil::write_string(c, "hello\n")
let reply = await ioutil::read_string(c)
c.close()
```
