# Tin stdlib code style

Formatting and style conventions for the Tin standard library. New stdlib code should follow these rules; existing code is being updated to match.

---

## File structure

A stdlib file is laid out in this order:

```
1. Build directives  (//!+file.c, //!-lfoo)     -- if any
2. File header comment                           -- required
3. Imports (use ...)                             -- if any
4. Extern declarations                           -- if any
5. Internal helpers (private fns, structs)
6. Public API (exported fns, structs)
7. Export statement
```

One blank line between each section. No blank lines within the directives block or within the import block.

```tin
//!+helper.c
//!-lfoo

// stdlib/mypkg - one-line summary.
//
// Longer description if needed.

use strings
use errors
use { Err } from errors

fn c_helper_one() i32 = extern("_tin_helper_one")
fn c_helper_two() i32 = extern("_tin_helper_two")

// internal helper
fn do_thing(x i32) i32 = ...

// Public function
fn public_fn(x i32) i32 = ...

export { public_fn } as mypkg
```

Only import packages you actually use.

---

## Comments

### File header

Every file starts with a header comment naming the package and giving a brief description. Usage examples belong here if the API is non-obvious.

```tin
// stdlib/http - HTTP/1.x async client.
//
// Usage:
//   let resp = await http::get("https://example.com")
//   echo resp.status
```

### Section comments

A plain comment line before a group of related declarations is enough. No dashes, no box drawing.

```tin
// PCRE C API
fn pcre_compile(...) ...
fn pcre_exec(...) ...
```

### Inline comments

Short inline comments may appear on the same line as a declaration:

```tin
_groups  [string]   // captured groups: index 0 = whole match, 1+ = sub-groups
```

### Function doc comments

The doc comment sits immediately above the function with no blank line between them. A blank line precedes the comment:

```tin
// encode returns the lowercase hex encoding of s.
fn encode(s string) string = ...

// encode_upper returns the uppercase hex encoding of s.
fn encode_upper(s string) string = ...
```

Comment what isn't obvious from the signature. Don't restate the name.

---

## Extern declarations

Group related externs together. Separate logical groups with a blank line:

```tin
fn tin_fmutex2_new() *void                    = extern("_tin_fmutex2_new")
fn tin_fmutex2_free(m *void)                  = extern("_tin_fmutex2_free")

fn tin_fmutex2_try_lock(m *void, pid i64) i32 = extern("_tin_fmutex2_try_lock")
fn tin_fmutex2_unlock(m *void)                = extern("_tin_fmutex2_unlock")
```

When all externs form a single cohesive group they may appear together without internal blank lines.

---

## Top-level functions

One blank line between every pair of top-level function definitions:

```tin
fn encode(s string) string =
  ...

fn decode(s string) string =
  ...
```

---

## Structs

### Fields

Fields are listed compactly with no blank lines between them. Align types and inline comments at a consistent column when field names are similar in length:

```tin
struct Match =
  _matched bool
  _groups  [string]   // index 0 = whole match, 1+ = sub-groups
  _begs    [i64]
  _ends    [i64]
```

### Methods

One blank line between every pair of methods (including `static fn`, `deinit`, and `_fiber_retain`). One blank line between the last field and the first method.

```tin
struct Mutex =
  _ptr *void

  static fn make() Mutex =
    return Mutex{_ptr: tin_fmutex2_new()}

  fn{#async #no_autoyield} lock(this Mutex) =
    let pid = tin_current_pid_mx()
    for true:
      if tin_fmutex2_try_lock(this._ptr, pid) != 0:
        return
      yield

  fn unlock(this Mutex) =
    tin_fmutex2_unlock(this._ptr)

  fn deinit(this Mutex) =
    tin_fmutex2_free(this._ptr)
```

---

## Function bodies

### Short functions (1-3 lines)

No internal blank lines needed:

```tin
fn len(this LinkedList[T]) i64 =
  return this._len

fn close(fd i32) = extern("_tin_fd_close")
```

### Longer functions

Add a blank line between distinct logical phases. Common boundaries:

- After the initial variable declarations, before the first loop or major conditional
- Between separate processing phases (e.g. integer part, fractional part, exponent in a float parser)
- Before a final cleanup/return block that is logically separate from the loop above it

```tin
fn exec_match(code *void, subject string, start i64) Match =
  let slen   = len(subject)
  let ovsize i32 = 30
  let ov     = mem::malloc(ovsize as i64 * 4) as *i32

  let rc i32 = pcre_exec(code, 0 as *void, subject, slen as i32, start as i32, 0, ov, ovsize)
  if rc < 0:
    mem::free(ov as *void)
    return Match{_matched: false, _groups: [], _begs: [], _ends: []}

  let groups [string] = []
  let begs   [i64]    = []
  let ends   [i64]    = []
  let i i32 = 0
  for i < rc:
    let beg = ov[i * 2] as i64
    let end = ov[i * 2 + 1] as i64
    groups ++= [substr(subject, beg, end)]
    begs   ++= [beg]
    ends   ++= [end]
    i = i + 1

  mem::free(ov as *void)
  return Match{_matched: true, _groups: groups, _begs: begs, _ends: ends}
```

### Guard clauses

Short, structurally identical early-return guards may be grouped without blank lines:

```tin
fn parse_int(s string) (i64, Err) =
  let n = len(s)
  if n == 0:
    return (0, errors::new("parse_int: empty string"))
  let i i64 = 0
  if s[0] == @'-' || s[0] == @'+':
    i = 1
  if i >= n:
    return (0, errors::new("parse_int: no digits"))
  ...
```

---

## Export statement

One blank line before the export statement. A single line is preferred when everything fits:

```tin
export { encode, encode_upper, decode } as base16
```

For longer exports, break across lines with the closing brace on its own line:

```tin
export {
  strlen, strcmp, strncmp, strcasecmp,
  strchr, strrchr, strstr,
  atoi, atol, atof,
} as str
```
