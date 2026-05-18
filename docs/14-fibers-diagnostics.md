# Async-coloring diagnostics, memory safety, examples


Tin emits a family of warnings that help keep sync and async code from
silently mixing in surprising ways. They are listed here together because
they share a single conceptual goal - keeping the "color" of each call
explicit at the source-code level.

| Diagnostic                    | Default     | Triggers on                                                                              |
|-------------------------------|-------------|------------------------------------------------------------------------------------------|
| `bare-parking-async-call`     | **on**      | Bare `fn{#async}` call whose body transitively reaches a known-parking primitive         |
| `bare-async-call`             | pedantic    | Every bare `fn{#async}` call (superset; also flags pure-compute async fns)               |
| `sync-uses-await`             | pedantic    | Sync fn body contains an `await` expression                                              |
| `droppable-fiber`             | pedantic    | Statement-level `spawn fn(args)` whose `Future` is neither stored, returned, nor awaited |
| `non-tin-thread`              | pedantic    | `#interop` fn body reaches `await` or `spawn`                                            |
| `async-main`                  | on          | Plain `fn main()` uses `spawn` / `await` (slower bridge)                                 |
| `blocking`                    | on          | Blocking C extern called from a `{#async}` body                                          |
| `await-match-guards`          | on          | Every `await match` arm has a guard and there is no `default`                            |

Pedantic warnings (default-off) are enabled by `-Wpedantic` or
individually with `-W<name>`. Any warning can be suppressed with
`-Wno-<name>` and escalated to an error with `-Werror=<name>`.

### `-Wbare-parking-async-call` (default on)

Calling an `fn{#async}` directly invokes the **sync variant** - which
ignores the scheduler. If the callee's body transitively reaches a
known-parking primitive (channel `send`/`recv`, `sleep_ms`, async I/O,
etc.), the calling OS thread parks too, defeating the whole point of
running async.

```rust
fn{#async} chunk_read(fd i32) i64 =
  return await spawn io::async_read(fd, buf, 4096)

fn main() =
  let _ = chunk_read(fd)
  // warning: calling async fn 'chunk_read' from sync context may park the
  // calling thread; consider `await spawn chunk_read(fd)` or
  // `sync::wait(spawn chunk_read(fd))`.
```

The check is *transitive*: any callee whose call graph reaches a parking
extern is classified parking. The seed set lives in
`codegen.knownParkingExterns`.

### `-Wbare-async-call` (pedantic)

Pedantic superset: fires on **every** bare `fn{#async}` call, even ones
the compiler proves cannot park. Useful when the codebase wants to make
the sync->async boundary explicit at every call site.

### `-Wsync-uses-await` (pedantic)

`await` inside a sync fn body works (Tin will emit a temporary fiber to
drive it), but it is rarely what the author intended:

```rust
fn uses_await() i64 =
  return await spawn compute(5)
  // -Wsync-uses-await: `await` inside sync fn "uses_await" drives the
  // scheduler at runtime; prefer `sync::wait(future)` to make the
  // sync->async bridge explicit, or promote this fn to `fn{#async}`.
```

Either promote the caller to `fn{#async}` or replace the `await` with
`sync::wait(spawn ...)`.

### `-Wdroppable-fiber` (pedantic)

A statement-level `spawn` whose `Future` is discarded is fire-and-forget:

```rust
fn main() =
  spawn log_event("ignored-result")
  // -Wdroppable-fiber: the `Future[T]` returned by this `spawn` is
  // discarded; bind to `let _ =` if intentional, or `await` it.
```

Suppress by binding to `_`:

```rust
let _ = spawn log_event("ignored-result")
```

### `-Wnon-tin-thread` (pedantic)

`#interop` fns are callable from arbitrary OS threads. Calling `await` or
`spawn` from such a thread executes against scheduler state the calling
thread does not own:

```rust
fn{#interop} c_entry(arg *void) =
  let _ = await spawn bg_work()
  // -Wnon-tin-thread: `#interop` fn "c_entry" awaits or spawns; calls
  // from non-Tin threads will execute against scheduler state they do
  // not own.
```

Move the await/spawn behind a normal Tin entrypoint and call *into* it
from the interop layer rather than the other way around.

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
