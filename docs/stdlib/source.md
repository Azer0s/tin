# source

`stdlib/source` parses the atom shapes that the `sourcepos()` and
`stacktrace()` builtins emit into a typed `SrcPos` struct. Useful for
filtering, grouping, or rendering frames programmatically.

> **No extra linker flags are required.**

---

## Import

```rust
use source
```

---

## SrcPos

```rust
struct SrcPos =
  symbol   string
  file     string
  line     i64
  col      i64
  lib      string
  offset   i64
  address  i64
  spawn_of bool
```

Every field is optional - absent ones stay zero/empty depending on
which atom shape produced the value:

| Field      | When set                                                       |
|------------|----------------------------------------------------------------|
| `symbol`   | Function name resolved from dladdr / DWARF                     |
| `file`     | Source path resolved by libdwfl (Linux) / unset on macOS       |
| `line`     | 1-based line number; 0 if libdwfl had no entry                 |
| `col`      | 1-based column; 0 if absent                                    |
| `lib`      | Shared-library basename (e.g. `libssl.so.3`, `libsystem_c.dylib`) |
| `offset`   | Byte offset within `symbol`; 0 for `file:line:col`-only frames |
| `address`  | Raw IP for fully unresolved `??+0x<addr>` frames               |
| `spawn_of` | true when the atom carried the `<spawn-of>:` prefix that stacktrace emits on frames frozen at fiber spawn |

---

## Functions

### `parse_sourcepos(a atom) SrcPos`

Decodes any atom shape `sourcepos()` or `stacktrace()` can emit:

```
symbol@file:line:col          -> sourcepos / stacktrace user-fn frame
file:line:col                 -> sourcepos on a non-identifier expr
<lib>:symbol+0x<offset>       -> stacktrace shared-lib frame
<lib>:symbol                  -> shared-lib fn entry (offset 0)
symbol+0x<offset>             -> stacktrace symbol-only frame
symbol                        -> sourcepos / stacktrace fn entry (offset 0)
??+0x<addr>                   -> stacktrace unresolved frame
<spawn-of>:<any of above>     -> spawn-chain frame, sets spawn_of = true
```

```rust
use source

let p = source::parse_sourcepos(sourcepos(my_handler))
echo p.symbol    // "my_handler"
echo p.file      // "src/server.tin"
echo p.line      // 42
```

### `is_resolved(p SrcPos) bool`

True when the parser found *any* identifying information (symbol or
file). False only for raw `??+0x<addr>` atoms after the `spawn-of`
prefix is peeled.

### `is_in_lib(p SrcPos) bool`

True when the frame belongs to a shared library (`p.lib != ""`). Useful
for filtering out libc / vendor frames before rendering.

### `is_unknown(p SrcPos) bool`

True when the parser failed to find symbol or file - a raw
`??+0x<addr>` atom.

---

## Recipes

### Drop libc and runtime frames before printing a trace

The `stacktrace` builtin already takes a filter-atom array
(`'hide_libc`, `'hide_unknown`, `'hide_runtime`, `'hide_main`) that
applies inside the runtime walk - prefer that for the common cases
because it avoids materializing dropped frames in the first place:

```rust
let frames = stacktrace(64, ['hide_libc, 'hide_runtime, 'hide_unknown])
```

For predicates the builtin doesn't cover, parse + pipe. Given a generic
curried `filter`:

```rust
fn filter[t](pred fn(i t) bool) fn([t]) [t] =
  return fn(xs [t]) [t] =
    let out [t] = []
    for let x t in xs:
      if pred(x):
        out ++= x
    return out
```

filter and print frames in your own code, dropping the libc / libsystem
tail. The predicate keeps frames that resolved to *something* (symbol or
file) and aren't inside a shared library; this works identically on
Linux (where libdwfl gives full `file:line:col`) and macOS (where there
is no libdwfl and frames are `symbol+0x<offset>` with `file == ""`):

```rust
use source

fn in_user_code(f atom) bool =
  let p = source::parse_sourcepos(f)
  if source::is_unknown(p): return false
  if source::is_in_lib(p):  return false
  return source::is_resolved(p)

stacktrace()
  |> filter(in_user_code)
  |> fn(fs [atom]) =
       for let f atom in fs:
         echo f
```

### Group log lines by source file

```rust
use source
use collections

let count = collections::HashMap[string, i64].make()
for let line string in log_lines:
  let p = source::parse_sourcepos(line as atom)
  if p.file != "":
    count.set(p.file, count.get_or(p.file, 0) + 1)
```

---

## Reference

| Symbol                                | Kind   | Description                              |
|---------------------------------------|--------|------------------------------------------|
| `SrcPos`                              | struct | Parsed shape of a sourcepos/stacktrace atom |
| `parse_sourcepos(a atom) SrcPos`      | fn     | Decode any emitted atom shape            |
| `is_resolved(p SrcPos) bool`          | fn     | Symbol or file present                   |
| `is_in_lib(p SrcPos) bool`            | fn     | Frame is in a shared library             |
| `is_unknown(p SrcPos) bool`           | fn     | Atom was the `??+0x<addr>` sentinel      |

The complementary builtins that *produce* the atoms this package parses
are documented in [10 - Reflection](../10-reflection.md#sourcepos-and-stacktrace-builtins).
