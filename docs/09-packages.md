# 09 - Packages & Standard Library

## Defining a package

A file becomes a package by declaring an `export` block at the end:

```rust
fn greet(name string) =
  echo "Hello, {name}"

export { greet } as greeter
```

---

## Importing a package

```rust
use greeter

greeter::greet("world")
```

### Package resolution

Package names map to directories under `stdlib/`. The short name `sync`
resolves to `stdlib/sync/sync.tin`. You can also use the path form with `::`:

```rust
use sync          // stdlib/sync/sync.tin - preferred
use std::sync     // same package via a nested path
```

For the standard library the short form (`use sync`) is always preferred.

### Selective imports

Import only specific names to avoid pulling the whole namespace into scope:

```rust
use { Channel, AtomicI64 } from sync

let ch = Channel[i64].make(8)
let c  = AtomicI64.make(0)
```

### File imports

Import from a relative file path:

```rust
use "./helpers.tin"

helpers::greet("world")
```

Selective imports also work with file paths:

```rust
use { greet } from "./helpers.tin"

greet("world")
```

### Re-exporting

A package can re-export other packages:

```rust
use io
use math

export { io, math } as std
```

```rust
use std

echo std::math::PI
```

---

## Package initialization - `fn init()`

A package can declare an `fn init()` to run one-time setup at program startup:

```rust
// stdlib/tls/tls.tin
fn init() =
  _tin_tls_init_c()   // initialize the global SSL_CTX

export { TlsConn, connect } as tls
```

`fn init()` is:
- Zero arguments, returns nothing
- Called automatically before any user code runs, after top-level variable initializations
- Called in dependency order: if package A uses package B, B's `init` runs first
- Idiomatic for one-time C library setup that must not be called from user code

It does not need to be exported and should not be called explicitly.

---

## Standard library

| Package   | Description                                                                |
|-----------|----------------------------------------------------------------------------|
| `io`      | Console I/O, file I/O, async reads/writes, TCP sockets                     |
| `ioutil`  | High-level line-oriented `read_string` / `write_string`                    |
| `net::tcp` | `tcp::Conn`, `tcp::Server`, `tcp::listen`, `tcp::dial` - high-level TCP   |
| `tls`     | Async TLS client connections over OpenSSL - see [`docs/stdlib/tls.md`](stdlib/tls.md) |
| `sync`    | `Channel[T]`, `Mutex`, `RWMutex`, `Cond`, `AtomicI64`, `Future[T]`, `Unit` |
| `math`    | `sqrt`, `pow`, `floor`, `ceil`, `sin`, `cos`, `PI`, `E` - links `-lm`      |
| `measure` | `now_us`, `now_ms` - monotonic clock for benchmarking                      |
| `strings` | `strings::replace(s, old, new) string`                                     |
| `str`     | Wrappers around libc string functions: `atoi`, `strcmp`, `strlen`, ...     |
| `regex`   | PCRE regular expressions - see [`docs/stdlib/regex.md`](stdlib/regex.md)   |
| `json`    | JSON encoding/decoding - see [`docs/stdlib/json.md`](stdlib/json.md)       |
| `os`      | `exit`, `getenv`, `setenv`, `getpid`, ...                                  |
| `assert`  | Test assertion helpers: `ok`, `equals`, `equals_str`, `fails`, `panics`    |
| `mem`     | C-level `malloc`, `calloc`, `realloc`, `free`, `memcpy` - for C interop only |
| `std`     | Convenience re-export: `io`, `math`, `os`, `assert`                        |

---

## `io` - input/output

```rust
use io

io::puts("hello")                                              // stdout
io::fprintf(io::fopen("out.txt", "w"), "%s", "data")          // file I/O
```

Async I/O integrates with the fiber scheduler. Low-level functions
(`async_read`, `async_write`) are `{#async}` and require `await spawn`.
Higher-level functions (`sleep`, `write_all`, `read_exact`) return `Future[T]`
and are awaited directly:

```rust
fn{#async} handler(fd i32) =
  let buf [byte; 4096]
  let n = await spawn io::async_read(fd, &buf[0], 4096)   // {#async} - needs spawn
  await io::write_all(fd, &buf[0], n)                     // Future[Unit] - direct await
  await io::sleep(100)                                     // Future[Unit] - direct await
```

Exported I/O traits: `Reader`, `Writer` (sync), `AsyncReader`, `AsyncWriter`
(return `Future[i64]`).

---

## `ioutil` - line-oriented I/O

```rust
use ioutil
use tcp

fn{#async} handler(conn tcp::Conn) =
  let line = await ioutil::read_string(conn)          // reads until \n
  await ioutil::write_string(conn, "echo: {line}\n")
```

All `ioutil` functions return `Future[T]` and are awaited directly. They
accept both raw file descriptors (`i32`) and `AsyncReader`/`AsyncWriter`
trait values.

---

## `tcp` - TCP connections

```rust
use tcp

let srv = tcp::listen(8080)    // Server
let (conn, ok) = srv.accept()  // (Conn, bool)
await conn.write("hello\n")    // Future[i64]
conn.close()
```

`tcp::Conn` implements both `AsyncReader` and `AsyncWriter`, so it works
directly with `ioutil`.

---

## `sync` - concurrency primitives

```rust
use sync

let ch  = sync::Channel[i64].make(8)   // bounded channel, cap 8
let mu  = sync::Mutex.make()
let cnt = sync::AtomicI64.make(0)
```

See [14 - Fibers & Channels](14-fibers.md) for fiber and channel usage.

---

## `math` - mathematics

```rust
//!-lm
use math

echo math::sqrt(2.0)   // 1.4142...
echo math::PI          // 3.14159...
```

Requires `//!-lm` at the top of the file.

---

## `measure` - monotonic clock

```rust
use measure

let start = measure::now_us()
// ... work ...
let elapsed_us = measure::now_us() - start
echo "elapsed: ~{elapsed_us / 1000}ms"
echo "per-op: ~{elapsed_us * 1000 / n}ns"
```

| Function         | Returns | Description                    |
|------------------|---------|--------------------------------|
| `measure::now_us()` | `i64`   | Monotonic time in microseconds |
| `measure::now_ms()` | `i64`   | Monotonic time in milliseconds |

Both functions use `CLOCK_MONOTONIC` and never go backwards. Use `now_us` for
sub-millisecond precision; `now_ms` when millisecond granularity is enough.

---

## `mem` - C-level allocator

`mem` exposes the raw C heap functions directly. It is intended for **C interop
only** - use it when interfacing with a C library that expects you to manage
memory manually (`malloc`/`free` contracts) or when constructing objects that
live outside Tin's ARC system.

```rust
use mem

let p = mem::malloc(64) as *byte    // raw C malloc - caller must free
// ... use p ...
mem::free(p as *void)               // explicit release required
```

Normal Tin code does not need `mem`. All pointers obtained via `&` (struct
literals, named locals returned from functions) are ARC-managed and freed
automatically when the owning variable leaves scope.

---

## `assert` - test assertions

```rust
use assert

assert::equals(result, 42)
assert::equals_str(msg, "ok")
assert::ok(flag)
assert::panics(fn() = risky())
```

See [11 - Testing](11-testing.md) for full test block syntax.

---

## `std` - convenience meta-package

`use std` imports `io`, `math`, `os`, and `assert` in one step:

```rust
use std

io::puts("hello")
echo math::PI
```
