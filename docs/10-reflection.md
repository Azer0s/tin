# 10 - Reflection & Runtime Type Information

Tin includes built-in reflection operators that let programs inspect types,
traits, field names, and field values at runtime.

---

## Atoms

An **atom** is a compile-time symbolic constant. Atoms compare by identity
(interned at compile time) and are the return type of `typeof`, `traitof`,
`fieldnames`, and `fieldtypes`.

### Simple atoms

Simple atoms are written with a leading `'` followed by a name that contains
only letters, digits, and underscores:

```rust
'ok
'err
'sunny
'my_type_1
```

These are the atoms used in enum declarations, `where` pattern matching, and
returned by `typeof`:

```rust
enum atom status =
  'ok
  'err

fn check(s atom) =
  where 'ok:  echo "all good"
  where 'err: echo "error"
  where _:    echo "unknown"

let t = typeof(42)     // 'i64
let t2 = typeof(true)  // 'bool
```

### Complex (quoted) atoms

When a type specification includes characters that are not allowed in a simple
atom name (parentheses, brackets, `*`, `,`) a **quoted atom** is used. The
syntax is `'"..."` with the type string inside double quotes:

```rust
'"fn(i64)bool"
'"fn(i64,f64)bool"
'"*bool"
'"[string]"
'"fn(fn(i64)bool,i64)string"
```

Quoted atoms are how the reflect API represents pointer types, array types, and
function types. They are also produced by `typeof` for such values:

```rust
let arr = [1, 2, 3]
echo typeof(arr)         // '[i64]

let p = 42
let ptr = &p
echo typeof(ptr)         // '*i64
```

When working with the `reflect` package, pass quoted atoms directly to
interrogate type structure:

```rust
use reflect

echo reflect::is_fn('"fn(i64)bool")      // 1
echo reflect::fn_ret('"fn(i64,f64)bool") // 'bool
echo reflect::elem('"*bool")             // 'bool
echo reflect::elem('"[string]")          // 'string
```

Both simple and quoted atoms have type `atom` and work identically with
`==`, `where` guards, and reflection functions.

---

## The `any` type

`any` is a **dynamically-typed container** that holds a value of any tin type
together with its runtime type identity. Assigning a concrete value to `any`
is called **boxing**:

```rust
let x i64  = 42
let a any  = x        // box: stores (type='i64', data->42)

let p = point{x: 3, y: 4}
let ap any = p        // box: stores (type='point', data->copy of p)
```

The stored type identity is exact - boxing a `rect` stores type `'rect`, not
any base trait or generic name.

### any and extern functions

Values returned from extern C functions are boxed with the correct Tin type:

```rust
fn c_abs(x i64) i64 = extern("labs")
fn c_sqrt(x f64) f64 = extern("sqrt")

let r = c_abs(-42)
let a any = r
echo typeof(a)    // 'i64

let s any = c_sqrt(144.0)
echo typeof(s)    // 'f64
```

### any and function pointers

If a value goes through a function pointer or higher-order function, the type
is still preserved:

```rust
fn apply(f fn(i64) i64, x i64) i64 = return f(x)

let r = apply(c_abs, -77)
let a any = r
echo typeof(a)    // 'i64
```

---

## typeof

`typeof(expr)` returns the runtime type of a value as an atom:

```rust
let p = point{x: 1, y: 2}

echo typeof(p)      // 'point
echo typeof(42)     // 'i64
echo typeof(3.14)   // 'f64
echo typeof(true)   // 'bool
echo typeof("hi")   // 'string

let ptr = &p
echo typeof(ptr)    // '*point

let arr = [p, p]
echo typeof(arr)    // '[point]
```

When called on an `any` value, `typeof` inspects the stored type at runtime:

```rust
let a any = point{x: 0, y: 0}
echo typeof(a)      // 'point  - not 'any!

let b any = rect{w: 5, h: 3}
echo typeof(b)      // 'rect
```

The return value is an atom (type `atom`), so it can be matched with `where`
or compared with `==`.

---

## traitof

`traitof(expr)` returns the list of traits implemented by the value's runtime
type, as a `[atom]` array:

```rust
trait drawable  = fn draw(this drawable) string = virtual
trait resizable = fn resize(this resizable, factor i64) i64 = virtual

struct rect(drawable, resizable) =
  w i64
  h i64
  fn draw(this rect) string = return "rect"
  fn resize(this rect, factor i64) i64 = return this.w * factor

let r = rect{w: 10, h: 20}

let traits = traitof(r)
echo traits.len     // 2
echo traits[0]      // 'drawable
echo traits[1]      // 'resizable
```

Works on `any` values too:

```rust
let a any = rect{w: 10, h: 20}
let ts = traitof(a)
echo ts.len         // 2
```

Structs that implement no traits return an empty array.

---

## fieldnames

`fieldnames(expr)` returns the names of a struct's user-visible fields as
a `[atom]` array:

```rust
struct point =
  x i64
  y i64

let p = point{x: 3, y: 4}
let names = fieldnames(p)

echo names.len      // 2
echo names[0]       // 'x
echo names[1]       // 'y
```

Works on `any`:

```rust
let a any = p
let an = fieldnames(a)

echo an.len         // 2
echo an[0]          // 'x
```

Internal implementation fields (the hidden type-ID and vtable pointers)
are not included - only the fields visible in source code.

---

## fieldtypes

`fieldtypes(expr)` returns the type names of each user field as a `[atom]`
array, in the same order as `fieldnames`:

```rust
struct person =
  name string
  age  i64

let p = person{name: "Alice", age: 30}
let types = fieldtypes(p)

echo types[0]       // 'string
echo types[1]       // 'i64
```

Pointer and array field types include nested type information:

```rust
struct node =
  val  i64
  next own *node

let types = fieldtypes(node{val: 0, next: nil})
echo types[0]       // 'i64
echo types[1]       // '*node
```

---

## fieldtag

`fieldtag(expr, "fieldName")` returns the tag annotation attached to a
field declaration, as an `atom`. Fields are tagged with `@"tag"` in the
struct body (see [05 - Structs](05-structs.md) for tag declaration syntax):

```rust
struct user =
  id   i64    @"primary_key"
  name string @"required"
  bio  string

let u = user{id: 1, name: "Alice", bio: ""}

echo fieldtag(u, "id")    // 'primary_key
echo fieldtag(u, "name")  // 'required
echo fieldtag(u, "bio")   // '   (empty atom - no tag)
```

Field tags are stored in a compile-time global map with no runtime overhead
beyond the atom lookup.

---

## getfield

`getfield(expr, "fieldName")` reads the value of a named field, returning
it as `any`:

```rust
struct point =
  x i64
  y i64

let p = point{x: 10, y: 20}

let vx = getfield(p, "x")    // vx is any
echo vx                        // 10
```

On a concrete struct, `getfield` is lowered to a direct GEP + load at compile
time. On an `any` value, runtime dispatch selects the correct struct layout:

```rust
let a any = p

let gx = getfield(a, "x")    // runtime dispatch via type_id
echo gx                        // 10
```

### Unboxing `any` with `as`

`getfield` returns `any`. To use the result as a concrete type, unbox it
with `as`:

```rust
let vx = getfield(p, "x") as i64
let vy = getfield(p, "y") as i64

echo vx    // 10
echo vy    // 20
```

This works for all scalar types: `i64`, `f64`, `bool`, etc. The `as` cast
also works on values returned by `any`-typed extern or higher-order functions:

```rust
fn c_sqrt(x f64) f64 = extern("sqrt")

let result any = c_sqrt(144.0)
echo result as f64    // 12.0
```

---

## setfield

`setfield(expr, "fieldName", value)` writes `value` into the named field.
The expression must be an lvalue (a variable, not a temporary):

```rust
let p = point{x: 10, y: 20}

setfield(p, "x", 99)
echo p.x    // 99
```

`setfield` is useful when the field name is only known at runtime:

```rust
let fields = ["x", "y"]
let vals   = [1, 2]

for let i i64 in 0..fields.len:
  setfield(p, fields[i], vals[i])

echo p.x    // 1
echo p.y    // 2
```

---

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
