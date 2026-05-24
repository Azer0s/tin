# sourcepos, stacktrace, full example

## sourcepos and stacktrace builtins

Two builtins return source-position atoms with a shared format. They
compose with `reflect::SrcPos` (below) for typed access to symbol /
file / line / column.

### sourcepos

`sourcepos(expr?)` returns an atom describing the source position of
its argument, resolved at **compile time** with no runtime cost. The
atom shape is `'"<symbol>@<file>:<line>:<col>"` for identifiers and
`'"<file>:<line>:<col>"` for non-identifier expressions:

```rust
fn handler(req string) string = return req

echo sourcepos(handler)    // 'handler@src/server.tin:1:1
echo sourcepos(42 + 7)     // 'src/server.tin:5:14   - bare position
echo sourcepos()           // 'enclosing_fn@src/server.tin:6:6
```

`sourcepos()` with no argument resolves to the call-site, which is
how the [`log`](stdlib/log.md) macros capture the line that wrote each
entry.

Compile-time only: the arg is never evaluated. Shadowing a sourcepos
arg with a local binding still resolves to the binding's declaration
position. The compiler emits a `-Wbuiltin-shadow` warning when a
local rebinds the name `sourcepos` (default-off).

### stacktrace

`stacktrace(cap?, opts?)` returns `[atom]` - the live call chain at
the point of call, walked via the saved frame-pointer chain (rbp on
x86_64, x29 on aarch64) and resolved against a custom `__tin_pclntab`
section that codegen emits alongside the program text. No libdwfl /
elfutils dependency on any platform; the same path works on Linux,
FreeBSD, and macOS. Library frames (libc, libpthread, etc.) fall back
to `dladdr` for symbol-only resolution:

```rust
fn{#no_inline} probe() [atom] = return stacktrace()

let frames = probe()
for let f atom in frames:
  echo f
// 'probe@src/app.tin:1:24
// 'main@src/app.tin:5:14
// '"libc.so.6:__libc_start_main+0x8b"
// ...
```

| Argument | Type        | Default | Meaning                                     |
|----------|-------------|---------|---------------------------------------------|
| `cap`    | `i64`       | 64      | Max frames returned. Clamped to [1, 1024].  |
| `opts`   | `[atom]`    | `[]`    | Filter atoms applied during the walk.       |

Filter atoms (literal array required - codegen folds to a constant):

| Atom            | Effect                                                       |
|-----------------|--------------------------------------------------------------|
| `'hide_libc`    | Drop frames in libc, libpthread, libsystem.                  |
| `'hide_unknown` | Drop frames that resolved to `??+0x<addr>`.                  |
| `'hide_runtime` | Drop frames whose symbol begins with `_tin_`.                |
| `'hide_main`    | Drop the `main` / `_start` / `__libc_start_*` tail.          |

```rust
let user_frames = stacktrace(16, ['hide_libc, 'hide_main])
```

Cross-fiber capture: a frame from a spawned fiber renders as the live
top-of-stack frames followed by `<spawn-of>:` prefixed frames captured
at spawn time:

```
'inner@worker.tin:8:3
'"<spawn-of>:main@server.tin:42:5"
```

Atom format degrades gracefully when debug info is unavailable:

| Resolution available           | Atom shape                                       |
|--------------------------------|--------------------------------------------------|
| symbol + pclntab line          | `'"sym@file:line:col"`                           |
| line only (no symbol)          | `'"file:line:col"`                               |
| symbol only (lib frame)        | `'"libname.so:sym+0x<offset>"`                   |
| symbol only (main binary)      | `'"sym+0x<offset>"`                              |
| neither                        | `'"??+0x<addr>"`                                 |

Reachability gating: the compiler scans the AST for `stacktrace()` and
only emits the `__tin_pclntab` section and unwind tables
(`-funwind-tables`, `-rdynamic`, `-fno-omit-frame-pointer`) when at
least one call is reachable. Programs that never reference
`stacktrace()` pay zero binary-size or link-time cost.

> **No external library dependency.** `file:line:col` resolution
> comes from the in-binary `__tin_pclntab` section the compiler
> emits; no elfutils / libdw / libdwfl link is required on any
> platform. Library frames (libc, libpthread, etc.) still rely on
> `dladdr` for symbol-only resolution.

> **Frame-pointer requirement:** the walker follows the saved-fp
> chain, so any C compilation unit reachable from a Tin trace must be
> built with `-fno-omit-frame-pointer`. Tin codegen tags every IR
> function with `frame-pointer="all"` and the runtime sets the C flag
> on its own translation units; only third-party C reached via
> `#interop` callbacks is at risk. A frame whose caller omitted fp
> truncates the trace at that point.

---

## Parsing source positions

Atom parsing for `sourcepos` and `stacktrace` lives in
[`stdlib/source`](stdlib/source.md), not in `reflect`. `source::parse_sourcepos`
decodes any atom shape either builtin emits into a typed `SrcPos` struct:

```rust
use source

let p = source::parse_sourcepos(sourcepos(handler))
echo p.symbol    // "handler"
echo p.file      // "src/server.tin"
echo p.line      // 1
echo p.col       // 1
```

Fields populated based on what the atom contains:

| Field      | Type     | Description                                       |
|------------|----------|---------------------------------------------------|
| `symbol`   | string   | Function name; "" if anonymous                    |
| `file`     | string   | Source path; "" if symbol-only frame              |
| `line`     | i64      | 1-based line; 0 if absent                         |
| `col`      | i64      | 1-based column; 0 if absent                       |
| `lib`      | string   | Shared-lib basename (e.g. `libssl.so.3`); "" if not in a lib |
| `offset`   | i64      | Byte offset within symbol; 0 for file:line:col-only |
| `address`  | i64      | Raw IP for `??+0x<addr>` frames; 0 otherwise      |
| `spawn_of` | bool     | true for the `<spawn-of>:...` prefix on frozen spawn frames |

Use this to programmatically filter or group log lines and stack
frames by source location:

```rust
use source
use strings

fn from_user_code(f atom) bool =
  let p = source::parse_sourcepos(f)
  return p.file != "" && !strings::has_prefix(p.symbol, "_tin_")

for let f atom in stacktrace():
  if from_user_code(f):
    echo f
```

---

## Full example

```rust
use io

trait labeled =
  fn label(this labeled) string = virtual

struct shape(labeled) =
  name   string @"display_name"
  width  i64
  height i64

  fn label(this shape) string = return this.name

let s = shape{name: "box", width: 10, height: 5}

// Type and traits
io::printf("type   = %s\n", typeof(s))
io::printf("traits = %lld\n", traitof(s).len)

// Fields
let fns = fieldnames(s)
let fts = fieldtypes(s)
for let i i64 in 0..fns.len:
  io::printf("  %s : %s  tag=%s\n", fns[i], fts[i], fieldtag(s, fns[i]))

// Dynamic read / write
let w = getfield(s, "width")
echo w               // 10

setfield(s, "width", 20)
echo s.width         // 20

// Through any
let a any = s
io::printf("typeof(any) = %s\n", typeof(a))
echo getfield(a, "height")    // 5
```
