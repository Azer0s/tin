# Yield, auto-yield, panic in fibers


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

Every `{#async}` function automatically emits yield points in its `$coro`
variant at two kinds of site:

1. **Loop backedges** - after every `for` loop iteration.
2. **Call sites of heavy or recursive functions** - before every call to a
   function the compiler classifies as "auto-yield" (score >= 30, or a member
   of a recursive call-graph cycle).

Both happen without any manual `yield` calls. The sync (non-spawned) variant
is never affected.

```rust
fn fib(n i64) i64 =          // recursive -> classified auto-yield
  if n <= 1: return n
  return fib(n - 1) + fib(n - 2)

fn{#async} work(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i             // auto-yield at each loop backedge
  sum = sum + fib(n)          // auto-yield before fib (recursive)
  return sum
```

### Forcing classification with `{#heavy}`

Mark a function explicitly as heavy (regardless of its complexity score) so
that any async caller yields before invoking it:

```rust
fn{#heavy} expensive_hash(data string) u64 =
  // manually tuned; compiler score below threshold but call is costly
  ...
```

### Disabling auto-yield

Use `{#no_autoyield}` to disable **all** auto-yield (loop backedges and
call-site yields) for a specific async function:

```rust
fn{#async #no_autoyield} tight_loop(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // no auto-yield; runs without interruption
  return sum
```

Multiple tags are space-separated: `fn{#async #no_autoyield}`.

### Inspecting heuristics: `-fdump-heuristics`

Pass `-fdump-heuristics` after the source file to print the compiler's
classification for every function:

```sh
tin run  file.tin -fdump-heuristics
tin test file.tin -fdump-heuristics
tin ir   file.tin -fdump-heuristics
```

Output (to stderr):

```
[autoyield] fn fib         loops=0  allocs=0  calls=1  heavyCalls=0  score=2   [recursive]
[autoyield] fn tight_loop  loops=1  allocs=0  calls=0  heavyCalls=0  score=10  [normal]
[autoyield] fn work        loops=1  allocs=0  calls=0  heavyCalls=1  score=30  [auto-heavy]
```

Labels: `heavy` (explicit `{#heavy}` tag), `recursive` (call-graph cycle),
`auto-heavy` (computed score >= 30), `normal` (no auto-yield inserted).

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

