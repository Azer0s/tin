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

### Strict await rule

`await` operates **purely on values implementing `awaitable[T]`** - the
compiler does not insert an implicit `spawn` under the operand. Older Tin
versions silently desugared `await fn(args)` (where `fn` was `{#async}`) into
`await spawn fn(args)`; that sugar has been removed. The two valid shapes are:

```rust
// 1. await a value that already implements awaitable[T]
let f = spawn compute(7)
let r = await f

// 2. await a SpawnExpr or a call that returns Future[T] / awaitable[T]
let r = await spawn compute(7)
let body = await fetch(url)         // fetch returns Future[string]
```

The compile error for the removed sugar reads:

```
await expects an awaitable value (Future[T] / awaitable[T]), but the
operand is a raw i64 returned from an async fn called in sync mode.
Did you mean `await spawn compute(7)`?
```

Bare `fn{#async}` calls (without `spawn` or `await`) are still legal -
they run the sync variant inline - but trigger `-Wbare-parking-async-call`
when the callee may park (see [diagnostics](#async-coloring-diagnostics)).

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

### Future[T] auto-coercion to Future[any]

`Future[T]` implements `coerce[Future[any]]`, the op-trait that the compiler
consults at every implicit coercion site. Any context that expects a
`Future[any]` therefore accepts a `Future[T]` of any element type without an
explicit cast - including `[Future[any]]` literals passed to
`sync::wait_all`:

```rust
use sync

fn{#async} produce_i64() i64 = return 7
fn{#async} produce_str() string = return "ok"

fn main() =
  let fs [Future[any]] = [spawn produce_i64(), spawn produce_str()]
  let _ = sync::wait_all(fs)
```

The conversion is a typed-field copy of the underlying PID, so it is O(1)
and never allocates. Use `as any` if you need to widen the element type
inside an already-typed `Future[T]`.

---


---

## See also

Subpages branched off as this doc grew. The numbered ordering
still works as a reading order:

- [Yield, auto-yield, panic](14-fibers-yield-panic.md)
- [Async I/O, sleep, spawn-do, TCP](14-fibers-async-io.md)
- [Channels, sync primitives, await match](14-fibers-channels.md)
- [Async-coloring diagnostics + memory + examples](14-fibers-diagnostics.md)
