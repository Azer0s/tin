# Channel[T], awaitable[T], sync primitives, await match


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

## awaitable[T] trait

`Future[T]` implements the `awaitable[t]` trait. You can implement this trait
on your own types to make them awaitable with the `await` keyword. The trait
has two methods:

- `ready()`  - non-blocking poll, returns `true` when `result()` is ready.
- `result()` - produce the value.

`await x` desugars to a runtime-driven spin loop:

```
loop:
  if x.ready(): break
  yield
x.result()
```

The runtime owns the loop, so your `ready()` only has to answer the question
and your `result()` only has to produce the value. No manual `yield`.

```rust
struct MyResult(awaitable[i64]) =
  done bool
  data i64

  fn awaitable[i64]::ready(this MyResult)  bool = return this.done
  fn awaitable[i64]::result(this MyResult) i64  = return this.data

let r = MyResult{done: true, data: 42}
let v = await r   // 42
```

`Mutex.lock()`, `RWMutex.read_lock()`, and `RWMutex.lock()` return awaitable
handles - `await m.lock()` runs the try-lock loop inline in the calling fiber
without spawning a coroutine frame.

---

## sync primitives

`stdlib/sync` also provides:

| Type        | Description                                                                                                                                                                                       |
|-------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Mutex`     | Exclusive lock (pthread_mutex_t)                                                                                                                                                                  |
| `RWMutex`   | Reader-writer lock (pthread_rwlock_t)                                                                                                                                                             |
| `Cond`      | Condition variable (pthread_cond_t)                                                                                                                                                               |
| `Atomic[t]` | Generic atomic (C11 atomics for `i*/u*/f*/bool`; mutex-protected for any other `t`). `cas`/`add` only available when `t` is numeric - see [05 - Structs](05-structs.md#method-level-where-guards) |
| `Future[T]` | Typed future from `spawn`; implements `Awaitable[T]`                                                                                                                                              |
| `Unit`      | Placeholder return type for void async functions                                                                                                                                                  |

`Future[T]` is exported at the top level (no `sync::` prefix) for ergonomics
- write `Future[i64]`, not `sync::Future[i64]`.

---

## Bridging sync to async: `sync::wait` / `sync::wait_all`

A **sync** function cannot use `await` directly (the operand would have to
be driven by a scheduler that only exists inside `{#async}` frames). When
you have a `Future[T]` on the sync side, use `sync::wait` to drive the
scheduler from a non-async caller:

```rust
use sync

fn{#async} compute(n i64) i64 = return n * 2

fn main() =                      // plain sync main
  let f = spawn compute(21)      // f : Future[i64]
  let r = sync::wait(f)          // drives the scheduler, returns 42
  echo r
```

`sync::wait(f)`:

1. Asks the runtime to schedule the fiber and park *the calling OS thread*
   on a condvar until it completes (`_tin_fiber_sync_await`).
2. Transfers ownership of the result buffer to the caller
   (`_tin_fiber_get_result` returns `*void`).
3. Reads the typed value, frees the buffer (`_tin_free`), and returns it.

The transfer-then-free pattern is required - `_tin_future_await_raw` only
peeks at the buffer and leaks it on `recv`-style flows. Use `sync::wait` for
every sync->async bridge; never roll your own.

### `sync::wait_all` - drain a heterogeneous list

```rust
fn{#async} produce_i64() i64    = return 7
fn{#async} produce_str() string = return "ok"

fn main() =
  let fs [Future[any]] = [spawn produce_i64(), spawn produce_str()]
  let results = sync::wait_all(fs)   // [any], element types preserved
```

The element type `Future[any]` is intentional - it lets the same call
aggregate mixed-element-type futures. The compiler auto-coerces each
`Future[T]` literal via the `coerce[Future[any]]` trait on `Future[t]`, so
you do not need explicit `as Future[any]` casts.

`sync::wait_all` returns `[any]`; index into it and `as T` when you need
the concrete value.

> **Note.** `++=` is concat-assign (not append-one), so the wait_all
> body uses `out ++= [wait(fs[i])]` -- the bracket is a one-element
> slice, not an attempt to box. To append a single value to a slice,
> always wrap it.

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

  await match (a, b):
    case (x, _):   echo "a finished first: {x}"
    case (_, y):   echo "b finished first: {y}"
```

### Syntax

```
await match (expr, expr, ...):
  case (slot, slot, ...):          body
  case (slot, slot, ...) if guard: body
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
await match (a, b, c):
  case (x, _, _) if x > 0: echo "a done: {x}"
  case (_, y, _):           echo "b done: {y}"
  case (_, _, z):           echo "c done: {z}"
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

await match (a, b):
  case (x, _) if x > 5:  got = x
  case (x, _):            got = -1   // unguarded fallback for a
  case (_, y) if y > 5:  got = y
  case (_, y):            got = -2   // unguarded fallback for b
```

---

