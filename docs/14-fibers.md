# Fibers and Channels

Tin has built-in support for lightweight concurrent green threads called
**fibers**, scheduled by an M:N scheduler: **M fibers** run across **N worker
OS threads**. Fibers are compiled to LLVM coroutines; context switching is
cheap (a few dozen instructions, no OS context switch).

### Environment variables

| Variable            | Default | `=0` meaning  | Description                                                 |
|---------------------|---------|---------------|-------------------------------------------------------------|
| `TINMAXPROCS`       | nproc   | n/a           | Number of worker OS threads                                 |
| `TINMAXFIBERS`      | 1M      | panic         | Maximum concurrent fibers before panic                      |
| `TINMAXRUNNABLES`   | 1M      | **unlimited** | Maximum entries in the run queue before panic               |
| `TINMAXTIMERS`      | 1M      | panic         | Maximum simultaneous `sleep` timers before panic            |
| `TINMAXIOWATCHES`   | 64K     | panic         | Maximum simultaneous async I/O watches before panic         |
| `TINMAXCHANWAITERS` | 64K     | **unlimited** | Maximum fibers parked per channel waiter queue before panic |

All queues grow dynamically (doubling on demand) up to their cap, then
`panic()` with a message naming the variable to raise.

`TINMAXRUNNABLES=0` and `TINMAXCHANWAITERS=0` disable the cap entirely -
the queue grows without bound and never panics (pre-cap behaviour).

```sh
TINMAXPROCS=4 ./my_program               # use 4 worker threads
TINMAXFIBERS=4000000 ./my_program        # allow up to 4M concurrent fibers
TINMAXCHANWAITERS=262144 ./my_program    # 256K waiters per channel
TINMAXRUNNABLES=0 ./my_program           # unlimited run queue (no panic)
TINMAXCHANWAITERS=0 ./my_program         # unlimited channel waiter queues
```

### Exit semantics

When `main()` returns, the process exits immediately. Spawned fibers that
have not been awaited are abandoned (same as Go). If you need to wait for
background work, use `await` or a `Channel`.

---

## Async main

For programs that use `spawn` or `await` at the top level, mark `main` as
`{#async}`:

```rust
use sync

fn{#async} main() =
  let ch = sync::Channel[i64].make(1)
  let f = spawn some_worker(ch)
  await ch.send(42)
  let result = await f
  echo result
```

An `{#async} main` is spawned as a fiber before the scheduler starts. The
process exits after `main`'s fiber completes.

If you use `spawn` or `await` inside a non-async `main()`, the compiler
emits a warning:

```
tin: warning: main() uses 'spawn' or 'await' but is not marked async.
    Each await in a non-async main() creates a temporary fiber, which is slower
    and bypasses inline channel optimizations.
    Fix: change 'fn main()' to 'fn{#async} main()'
```

Suppress this warning with `-Wno-async-main`:

```sh
tin build myfile.tin -Wno-async-main
```

---

## Async functions

### `fn{#async}` tag

Mark a function with the `{#async}` tag to make it spawnable as a fiber:

```rust
fn{#async} compute(n i64) i64 =
  return n * n
```

An async function:
- Has the same parameter and return types as a normal function.
- Can call `yield` to voluntarily give up the CPU.
- Automatically yields at every loop backedge (see **Auto-yield** below).
- Is compiled to a standard (sync) variant **and** a `$coro` ramp that the
  fiber scheduler uses.

---

## Async function types

`fn{#async}(params...) ret` is a first-class type that represents an async
function value. It is distinct from `fn(params...) ret` (a sync function value):
the compiler tracks the difference at the type level and generates correct spawn
behavior for each.

### Parameters

Declare a parameter as `fn{#async}(params...) ret` to accept an async function:

```rust
fn run_async(cb fn{#async}(i64) i64, n i64) i64 =
  let f = spawn cb(n)   // spawns cb as a fiber
  let r = await f
  return r

fn{#async} double(n i64) i64 =
  await io::sleep(1)
  return n * 2

test "pass async fn as parameter" =
  let result = run_async(double, 21)
  assert::equals(result, 42)
```

The compiler picks the correct overload automatically: if multiple functions
share a name (sync and async variants), the one matching the declared parameter
type is selected.

### Arrays

Use `[fn{#async}(params...) ret]` as the element type:

```rust
fn{#async} double(n i64) i64 =
  await io::sleep(1)
  return n * 2

test "store async fn in array" =
  let fns [fn{#async}(i64) i64] = [double]
  let f = spawn fns[0](5)
  let r = await f
  assert::equals(r, 10)

test "multiple async fns in array" =
  let fns [fn{#async}(i64) i64] = [double, double]
  let f0 = spawn fns[0](3)
  let f1 = spawn fns[1](7)
  assert::equals(await f0, 6)
  assert::equals(await f1, 14)
```

### Struct fields

Use `fn{#async}(params...) ret` as a struct field type:

```rust
struct Handler =
  handle fn{#async}(i64) i64

fn{#async} double(n i64) i64 =
  await io::sleep(1)
  return n * 2

test "async fn in struct field" =
  let h = Handler{handle: double}
  let f = spawn h.handle(10)
  let r = await f
  assert::equals(r, 20)
```

### Overload resolution

When a function has both sync and async overloads (or any two overloads), the
compiler selects the correct one based on the declared parameter type:

```rust
fn apply(cb fn(i64) i64, n i64) i64 = return cb(n)

fn add_one(n i64) i64 = return n + 1

test "passing sync overload to higher-order function" =
  let result = apply(add_one, 5)
  assert::equals(result, 6)
```

If a function is named in a context where the target type is
`fn{#async}(params...) ret`, the compiler wraps it in the async variant
automatically. If the target is `fn(params...) ret`, the sync variant is used.

---

## spawn

`spawn expr` starts a fiber and returns `Future[T]` where `T` is the return
type of the async function (`Future[Unit]` for void functions):

```rust
let f = spawn compute(7)    // f : Future[i64]
```

Only calls to `{#async}` functions can be spawned.

---

## await

`await expr` is **purely type-based**: it is valid if and only if `expr`
evaluates to a value implementing `Awaitable[T]`. The form of `expr` does not
matter - any of the following work:

```rust
// 1. Variable that holds a Future[T]:
let f = spawn compute(7)   // f : Future[i64]
let result = await f       // f is Awaitable[i64] -> result : i64

// 2. spawn expression directly:
let n = await spawn io::async_read(fd, &buf[0], 4096)

// 3. Any function call whose return type is Future[T] (or Awaitable[T]):
let body = await fetch(url)                     // fetch() returns Future[string]
await io::sleep(500)                            // io::sleep returns Future[Unit]
let line = await ioutil::read_string(conn)      // returns Future[string]
```

### `await spawn` vs `await`

Many stdlib functions return `Future[T]` directly and can be awaited without
`spawn`. Low-level primitives (`io::async_read`, `io::async_write`) are
`{#async}` functions that require `spawn`:

```rust
// Low-level: async_read is {#async} - must use spawn
let n = await spawn io::async_read(fd, &buf[0], 4096)

// High-level: these functions return Future[T] - await directly
await io::sleep(100)
await io::write_all(fd, &buf[0], n)
let line = await ioutil::read_string(conn)
await ioutil::write_string(conn, "hello\n")
```

The rule: **if a function is tagged `{#async}`, use `await spawn`; if a
function returns `Future[T]` in its signature, use `await` directly.**

For void fibers (`Future[Unit]`), `await` is used as a statement:

```rust
fn{#async} worker(n i64) =
  echo "fiber started: {n}"
  yield
  echo "fiber done: {n}"

fn main() =
  let p = spawn worker(1)
  await p   // blocks until worker finishes
```

Calling an `{#async}` function directly (without `spawn`) invokes its **sync
variant**, which runs inline and returns a plain value - not a `Future[T]`.
This is a compile error with `await`:

```rust
// ERROR: async_read() returns i64, not Awaitable[T]
let n = await io::async_read(fd, &buf[0], 4096)

// CORRECT: spawn creates a fiber and returns Future[T]; await blocks until done
let n = await spawn io::async_read(fd, &buf[0], 4096)
```

---

## Future[T]

`Future[T]` (from `stdlib/sync`) wraps the PID of a spawned fiber and
implements `Awaitable[T]`. It is returned automatically by `spawn`.

You do not need to `use sync` to use `Future[T]` - the sync module is
auto-imported when fibers are used.

```rust
fn{#async} compute(n i64) i64 =
  return n * n

let f = spawn compute(7)   // f is Future[i64]
let r = await f            // r is i64 = 49
```

---

## yield

`yield` suspends the current fiber and re-enqueues it so other fibers can run:

```rust
fn{#async} heavy(n i64) =
  let i i64 = 0
  for i < n:
    i = i + 1
    if i % 1000 == 0:
      yield   // give CPU to other fibers every 1000 iterations
```

---

## Auto-yield

By default, every `{#async}` function **automatically emits a yield point at
every loop backedge** in its `$coro` variant. This means loops cooperate with
other fibers without any manual `yield` calls.

```rust
fn{#async} count_up(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // yields to scheduler after each iteration automatically
  return sum
```

The sync variant (non-spawned call) is never affected - auto-yield only applies
inside the fiber scheduler.

### Disabling auto-yield

Use `{#no_autoyield}` to disable auto-yield for a specific async function:

```rust
fn{#async #no_autoyield} tight_loop(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // no auto-yield; runs without interruption
  return sum
```

Multiple tags are separated by spaces (no commas):
`fn{#async #no_autoyield} ...`

---

## Panic in fibers

### Panic propagation via `await`

When a spawned fiber panics, the panic is not immediately fatal. The runtime
stores the panic message on the fiber and marks it done. When you `await` that
fiber, the panic is re-raised in the calling fiber's context - making it
catchable with `defer + recover()` just like any other panic:

```rust
fn{#async} risky(n i64) i64 =
  if n < 0:
    panic("negative input")
  return n * 2

fn main() =
  let caught string = ""
  defer do:
    let msg = recover()
    if len(msg) > 0:
      caught = msg
  let result = await spawn risky(-1)   // panic re-raised here
  echo "unreachable"
```

The panic message is preserved exactly:

```rust
fn{#async} panics_with_value(msg string) =
  panic(msg)

test "await panic message is preserved" =
  let caught string = ""
  defer do:
    let msg = recover()
    if len(msg) > 0:
      caught = msg
  await spawn panics_with_value("my error")
  assert::equals_str(caught, "my error")
```

### `defer + recover()` inside an `{#async}` function

An `{#async}` function can also catch panics from fibers it spawns internally.
When a panic is recovered and the defer does not explicitly override the return
value, the outer awaiter receives the **zero value** of the return type:

```rust
fn{#async} always_panics() i64 =
  panic("boom")
  return 0

fn{#async} safe_wrapper() i64 =
  defer do:
    let _ = recover()   // swallow the panic; no return override
  let _ = await spawn always_panics()
  return 0

test "recovered panic in async fn yields zero value" =
  let result = await spawn safe_wrapper()
  assert::equals(result, 0)   // 0 = zero value of i64
```

The same applies to a `panic()` call directly inside the async function:

```rust
fn{#async} catches_direct_panic() i64 =
  defer do:
    let _ = recover()
  panic("direct")
  return 0

test "recovered direct panic yields zero value" =
  let result = await spawn catches_direct_panic()
  assert::equals(result, 0)
```

Zero values by type: `0` for integers, `false` for `bool`, `""` for `string`,
`0.0` for floats, `nil` for pointers.

### Fire-and-forget panics

If a fiber panics and is **never awaited**, the panic is detected at process
shutdown and re-raised on the main thread - killing the process. This matches
Go's goroutine panic semantics: an unhandled panic in any concurrent task is
fatal.

```rust
fn{#async} background_task() =
  panic("something went wrong")

fn main() =
  spawn background_task()   // fire-and-forget; never awaited
  // process exits cleanly here, BUT:
  // at shutdown the runtime detects the unhandled panic and calls exit(1)
```

To handle errors from fire-and-forget fibers, use a `Channel` to report them
back to a supervising fiber, or catch them inside the async function itself
with `defer + recover()`.

---

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

## Channel[T]

`Channel[T]` from `stdlib/sync` is a bounded FIFO channel backed by a ring
buffer protected by a mutex and condition variables.

```rust
use sync

let ch = sync::Channel[i64].make(8)  // capacity 8

ch.send(1)          // blocks if full
let v = await ch.recv()   // blocks if empty
ch.close()          // wakes all blocked receivers
```

Channels handle ARC automatically: RC-tracked values (strings, slices) are
retained on `send` and ownership is transferred on `recv`. The `isrc(T)`
builtin determines at compile time whether element type `T` requires
retain/release - primitive types (`i64`, `bool`, etc.) incur no overhead.

### Channel example: producer/consumer

```rust
use sync

fn{#async} producer(ch sync::Channel[string], n i64) =
  let i i64 = 0
  for i < n:
    ch.send("item-{i}")
    i = i + 1
  ch.close()

fn{#async} consumer(ch sync::Channel[string]) =
  for !ch.is_closed_empty():
    let v = await ch.recv()
    echo v

fn main() =
  let ch = sync::Channel[string].make(16)
  spawn producer(ch, 5)
  let p = spawn consumer(ch)
  await p
```

---

## Awaitable[T] trait

`Future[T]` implements the `Awaitable[t]` trait. You can implement this trait
on your own types to make them awaitable with the `await` keyword:

```rust
use sync

struct MyResult(sync::Awaitable[i64]) =
  data i64

  fn await_result(this MyResult) i64 =
    return this.data

let r = MyResult{data: 42}
let v = await r   // calls r.await_result() -> 42
```

---

## sync primitives

`stdlib/sync` also provides:

| Type        | Description                                          |
|-------------|------------------------------------------------------|
| `Mutex`     | Exclusive lock (pthread_mutex_t)                     |
| `RWMutex`   | Reader-writer lock (pthread_rwlock_t)                |
| `Cond`      | Condition variable (pthread_cond_t)                  |
| `AtomicI64` | Lock-free 64-bit integer (C11 atomics)               |
| `Future[T]` | Typed future from `spawn`; implements `Awaitable[T]` |
| `Unit`      | Placeholder return type for void async functions     |

---

## await match

`await match` selects among multiple futures: it blocks until one of them
completes, then dispatches to the matching arm. Each arm binds exactly one
future's result; wildcards (`_`) fill the remaining slots.

```rust
use { async } from macros
use io

async slow(n i64) i64 =
  await io::sleep(10)
  return n

fn{#async} main() =
  let a = spawn slow(1)
  let b = spawn slow(2)

  await match [a, b]:
    case [x, _]:   echo "a finished first: {x}"
    case [_, y]:   echo "b finished first: {y}"
```

### Syntax

```
await match [expr, expr, ...]:
  case [slot, slot, ...]:          body
  case [slot, slot, ...] if guard: body
  default:                         body
```

- The bracketed list must be an **inline literal** - no array variables or
  computed expressions.
- Each `case` must have **exactly one** non-wildcard slot. The bound variable
  receives the result of the future at that position (its type is inferred from
  the future's type parameter).
- Multiple `case` arms can match the same slot position (e.g., for guards).
- Futures may be **heterogeneous** (`Future[i64]` and `Future[string]` in the
  same list are fine).

### Blocking semantics (no `default`)

Without `default`, `await match` blocks until a future completes **and** a
guard passes:

1. Poll all futures once; if one is already done and its guard passes,
   dispatch immediately.
2. Otherwise block until any future completes, check its arms (in order);
   if a guard fails, mark that future as skipped and keep blocking.
3. If every future has completed and no arm's guard passed, the program
   panics at runtime.

The compiler emits a warning when **every** case arm has a guard and there is
no `default` arm (because the exhaustion panic becomes reachable):

```
warning: every await match arm has a guard and there is no default arm ...
```

Suppress this warning with `-Wno-await-match-guards`.

### Non-blocking semantics (`default`)

With `default`, `await match` performs a **single non-blocking check**: it
polls all futures once and dispatches to the first arm whose future is done
and guard passes. If nothing is actionable, `default` runs immediately.

```rust
await match [a, b, c]:
  case [x, _, _] if x > 0: echo "a done: {x}"
  case [_, y, _]:           echo "b done: {y}"
  case [_, _, z]:           echo "c done: {z}"
  default:                  echo "nothing ready yet"
```

### Guards

Guards filter the arm without affecting which future is considered done. A
guard failure causes the runtime to skip that arm and continue waiting (in
blocking mode) or fall through to `default` (in non-blocking mode).

```rust
let a = spawn slow(10)
let b = spawn slow(20)
let got i64 = 0

await match [a, b]:
  case [x, _] if x > 5:  got = x
  case [x, _]:            got = -1   // unguarded fallback for a
  case [_, y] if y > 5:  got = y
  case [_, y]:            got = -2   // unguarded fallback for b
```

---

## Memory safety

- Arguments passed to spawned fibers are retained automatically (ARC).
- Variables captured by `spawn do:` blocks are retained before the parent
  scope exits and released inside the fiber after they are unpacked.
- `Channel.send` retains RC-tracked values; `Channel.recv` transfers ownership.
- `await` on `Future[T]` returns the owned result; the fiber's memory is freed.
- The ARC implementation uses C11 atomics for correctness when values are
  shared across fibers.

---

## Examples

See `examples/fibers/` for runnable programs:

- `basic_spawn.tin` - spawn, yield, await, and typed `Future[T]` results
- `auto_yield.tin` - auto-yield default and `{#no_autoyield}` opt-out
- `yield_cpu.tin` - cooperative CPU sharing
- `channel_pingpong.tin` - two fibers communicating via a channel
- `stress_100.tin`, `stress_10k.tin` - 100 and 10,000 concurrent fibers

See `examples/echo_server/echo_server.tin` for a concurrent TCP echo server
demonstrating `spawn`, async I/O, and M:N scheduling.

See `examples/arc_stress/` for ARC correctness tests under concurrent load:

- `fiber_arc.tin` - 1,000 fibers building strings and passing them through a channel
- `fiber_channel_stress.tin` - 10 producers * 10 consumers, 1,000 total messages
- `fiber_nested.tin` - nested fiber spawning (parent fibers spawn child fibers)
- `fiber_spawn_storm.tin` - 5,000 fibers spawned in rapid succession
- `fiber_defer_arc.tin` - `defer` + ARC in async fibers (deferred channel sends)
- `fiber_string_storm.tin` - 500 fibers building and discarding long strings concurrently
- `fiber_spawn_do_capture.tin` - 500 `spawn do:` blocks each capturing local `i64` + `string`

See `examples/bench/` for performance benchmarks:

- `spawn_throughput.tin` - fibers/second spawn rate
- `channel_latency.tin` - channel round-trip latency
- `yield_overhead.tin` - auto-yield vs manual-yield overhead
