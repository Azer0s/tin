# log

`stdlib/log` provides leveled logging with text or JSON output, ANSI color
on TTY, and call-site source positions captured automatically via macros.

> **No extra linker flags are required.**

---

## Import

```rust
use log
```

---

## Quick start

```rust
use log

let l = log::text().level(log::LEVEL_INFO)

log::debug!(l, "wiring up workers")   // suppressed (below LEVEL_INFO)
log::info!(l,  "server started on :8080")
log::warn!(l,  "disk usage at 87%")
log::error!(l, "connect timeout")
// log::fatal!(l, "...")               // emits then calls exit(1)
```

Output (TTY):

```
2026-04-30T10:51:03+02:00 INFO  src/server.tin:42:3 server started on :8080
2026-04-30T10:51:03+02:00 WARN  src/server.tin:43:3 disk usage at 87%
2026-04-30T10:51:03+02:00 ERROR src/server.tin:44:3 connect timeout
```

Each level word is colored when stderr is a TTY (DEBUG cyan, INFO green,
WARN yellow, ERROR/FATAL red).

---

## Loggers

### `log::text() TextLogger`

Constructs a `TextLogger` with defaults: `LEVEL_INFO`, `COLOR_AUTO`,
position visible. Output: one human-readable line per entry, written to
**stderr** (fd 2).

### `log::json() JsonLogger`

Constructs a `JsonLogger` with defaults: `LEVEL_INFO`, position visible.
Output: one JSON object per line, written to stderr - suitable for log
shippers (ELK, Loki, fluentd):

```json
{"time":"2026-04-30T10:51:03+02:00","level":"info","src":"src/server.tin:42:3","msg":"server started on :8080"}
```

Both loggers share the same builder shape - chain methods to configure.
Each method returns a NEW logger; the original is untouched, so you can
fork configurations freely.

---

## Configuration

### `.level(lvl i32) Logger`

Sets the minimum level. Entries below this level are dropped at the
emit fast path (no string formatting cost):

```rust
let l = log::text().level(log::LEVEL_WARN)
log::info!(l, "ignored")    // suppressed
log::warn!(l, "shown")
```

| Constant            | Value | Method      |
|---------------------|------:|-------------|
| `log::LEVEL_DEBUG`  | 0     | `debug!`    |
| `log::LEVEL_INFO`   | 1     | `info!`     |
| `log::LEVEL_WARN`   | 2     | `warn!`     |
| `log::LEVEL_ERROR`  | 3     | `error!`    |
| `log::LEVEL_FATAL`  | 4     | `fatal!`    |

`fatal!` emits its line and then calls `exit(1)` - it does not return.

### `.color(c i32) TextLogger` (TextLogger only)

Controls ANSI color on the level word:

| Constant            | Behavior                              |
|---------------------|---------------------------------------|
| `log::COLOR_AUTO`   | Color when `isatty(stderr) == 1`      |
| `log::COLOR_ON`     | Always color                          |
| `log::COLOR_OFF`    | Never color                           |

Default: `COLOR_AUTO`. Use `COLOR_OFF` when piping to a file or shipper.

### `.hide_pos() Logger` / `.show_pos() Logger`

Toggles the source-position field. By default both loggers include the
file:line:col where the log macro was written. Hide it when the position
is uninteresting (e.g. inside a thin wrapper that always logs):

```rust
let l = log::text().hide_pos()
log::info!(l, "tick")
// 2026-04-30T10:51:03+02:00 INFO  tick
```

For JSON, `hide_pos` drops the `"src"` key entirely.

---

## Source positions

The `log::*!` macros capture the **macro call-site** position, not the
macro definition. This is automatic - you don't pass anything explicit:

```rust
// in src/server.tin, line 42
log::info!(l, "started")    // src/server.tin:42:3
```

Internally each macro expands to `l.<level>_at(sourcepos(), msg)`; the
compiler retags the macro body's `sourcepos()` to point at the caller's
site (see [12 - Macros](../12-macros.md)). The atom format matches what
[`source::parse_sourcepos`](source.md) decodes, so log lines can be
re-ingested for filtering by source location.

---

## Levels and methods

The `*!` macros are the recommended API. Direct method calls (`*_at`)
exist for code that needs to forward an explicit position - for example,
a logging wrapper that should report its caller's site rather than its
own:

```rust
fn log_request(l TextLogger, pos atom, route string) =
  l.info_at(pos, "request " ++ route)

// caller
log_request(l, sourcepos(), "/health")
```

Direct calls without a position go through the bare `info`/`warn`/...
shape, which simply omits the source-position field even when
`show_pos` is true (no position is available).

| Method (TextLogger / JsonLogger) | Macro form              |
|----------------------------------|-------------------------|
| `debug_at(pos atom, msg string)` | `log::debug!(l, msg)`   |
| `info_at (pos atom, msg string)` | `log::info!(l, msg)`    |
| `warn_at (pos atom, msg string)` | `log::warn!(l, msg)`    |
| `error_at(pos atom, msg string)` | `log::error!(l, msg)`   |
| `fatal_at(pos atom, msg string)` | `log::fatal!(l, msg)`   |

---

## Recipes

### Production JSON for log shippers

```rust
let l = log::json().level(log::LEVEL_INFO)
log::info!(l, "request handled")
// {"time":"...","level":"info","src":"...","msg":"request handled"}
```

### Quiet local debug log (no color, no position)

```rust
let l = log::text()
  .level(log::LEVEL_DEBUG)
  .color(log::COLOR_OFF)
  .hide_pos()
log::debug!(l, "loop iter 0")
// 2026-04-30T10:51:03+02:00 DEBUG loop iter 0
```

### Per-subsystem loggers

Forks are cheap - `level()` returns a new logger, sharing nothing
mutable with the original:

```rust
let base = log::text()
let net  = base.level(log::LEVEL_DEBUG)   // verbose for net
let app  = base.level(log::LEVEL_WARN)    // quiet for app
```

---

## Reference

| Symbol                                   | Kind     | Description                        |
|------------------------------------------|----------|------------------------------------|
| `TextLogger`                             | struct   | Human-readable line emitter        |
| `JsonLogger`                             | struct   | JSON-per-line emitter              |
| `text() TextLogger`                      | fn       | Construct a `TextLogger`           |
| `json() JsonLogger`                      | fn       | Construct a `JsonLogger`           |
| `LEVEL_DEBUG / INFO / WARN / ERROR / FATAL` | const | Level constants                    |
| `COLOR_AUTO / ON / OFF`                  | const    | TextLogger color modes             |
| `debug! / info! / warn! / error! / fatal!` | macro  | Emit at level, capture call-site   |

`fatal!` calls `exit(1)` after emitting; control does not return.
