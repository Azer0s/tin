# 08 – Interop, Packages & Low-Level Features

## Linker directives (`//!`)

Files can declare linker flags that are passed to the linker at compile time.
These directives appear at the top of the file as special comments starting
with `//!`:

```rust
//!-lm
//!-lraylib
```

Each `//!` line is a linker flag. The text after `//!` is appended verbatim to
the linker command line. Common uses:

| Directive | Purpose |
|---|---|
| `//!-lm` | Link the C math library (`libm`) |
| `//!-lraylib` | Link Raylib |
| `//!-lpthread` | Link pthreads |

Linker directives must appear before any non-comment code in the file. They
are processed before the rest of the compilation unit.

Example  -  using `sqrt` and `pow` from `libm`:

```rust
//!-lm

fn c_sqrt(x f64) f64 = extern("sqrt")
fn c_pow(x f64, y f64) f64 = extern("pow")

echo c_sqrt(144.0)    // 12.0
echo c_pow(2.0, 10.0) // 1024.0
```

---

## Calling C functions (`extern`)

### Inline extern declaration

A single C function can be imported with `= extern(cName)`:

```rust
fn ex_printf(fmt const *char, ...) i32 = extern(printf)
```

The Tin function `ex_printf` becomes a direct call to the C symbol `printf`.
Extern functions are automatically tagged `#sideEffectful`.

### Bulk extern import (`use extern`)

Multiple C functions can be imported in one block:

```rust
use extern (
  malloc as fn(size_t) *void,
  free   as fn(*void),
  strcpy as fn(*char, const *char) *char,
)
```

Each entry binds a C symbol to a Tin-typed function.

### Wrapping extern functions

It is idiomatic to wrap low-level externs with a clean Tin interface:

```rust
fn ex_printf(const *char, ...) i32 = extern(printf)

fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)
```

---

## Pointers

### Pointer types

| Syntax | Meaning |
|---|---|
| `*T` | Mutable pointer to `T` |
| `const *T` | Immutable pointer to `T` |
| `*void` | Untyped pointer (like C `void*`) |

### Address-of and dereference

```rust
let x i64 = 42
let p *i64 = &x        // address of x
let y = *p             // dereference
*p = 100               // write through pointer
```

### Pointer arithmetic

```rust
let video_mem *char = addr(0xB8000).(*char)
*video_mem = 'H'
video_mem += 1
```

`addr(N)` interprets an integer literal as an address. This is for
bare-metal / embedded use.

### Casting pointers

Use the `.(Type)` syntax to cast a pointer to another pointer type:

```rust
let raw *void  = malloc(sizeof(person))
let p   *person = raw.(*person)
```

### Struct pointer shorthand

`ptr->method()` is shorthand for `(*ptr).method()`:

```rust
let pete  = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo pPtr->show()        // same as (*pPtr).show()
```

---

## sizeof

`sizeof(T)` returns the byte size of a type as `size_t`:

```rust
let buf *char = malloc(10 * sizeof(*char)).(*char)
```

---

## defer

`defer stmt` schedules `stmt` to run when the current scope exits. It is
useful for pairing allocations with frees:

```rust
fn process() =
  let s = malloc(10 * sizeof(*char)).([char; 10])
  defer free(s)
  // ... s is used here
  // free(s) is called automatically on exit
```

---

## Packages and exports

### Defining a package

```rust
fn print(t string) =
  // ...

export { print } as io
```

### Importing a package

```rust
use io

io::print("hello")
```

### Re-exporting

```rust
use io
use math

export { io, math } as std
```

```rust
use std

let a = std::math::floor(std::math::PI)
```

---

## Standard library packages

### `io` - input/output

```rust
use io

io::print("hello\n")        // print without trailing newline
io::println("hello")        // print with trailing newline
io::read_line()             // read a line from stdin, returns string
```

### `math` - mathematics

```rust
use math

echo math::sqrt(2.0)        // 1.4142...
echo math::floor(3.7)       // 3
echo math::ceil(3.2)        // 4
echo math::PI               // 3.14159...
```

### `strings` - string manipulation

```rust
use strings

let s = strings::replace("hello-world", "-", " ")   // "hello world"
```

| Function | Description |
|---|---|
| `strings::replace(s, old, new) string` | Replace all occurrences of `old` with `new` in `s` |

### `guid` - globally unique identifiers

```rust
use guid

let id = guid::new()   // e.g. "6b8b4567-327b-4b23-8663-6b8b4567327b"
```

`guid::new()` returns a UUID v4 formatted string. Each call produces a different value.

### `std` - convenience meta-package

`use std` is equivalent to `use io` + `use math` in one import:

```rust
use std

echo std::math::PI
std::io::println("done")
```

---

## Control tags

Control tags annotate functions with compiler hints. They are written in
curly braces after `fn`:

| Tag | Effect |
|---|---|
| `#pure` | Function has no side effects; may be evaluated at compile time |
| `#recurse` | Function is allowed to call itself |
| `#noRecurse` | Compiler error if the function recurses |
| `#sideEffectful` | Function has side effects (all extern fns get this automatically) |

```rust
fn{#pure #recurse} fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10)    // computed at compile time
```

```rust
fn{#noRecurse} foo() =
  foo()    // compile error: recursion in a #noRecurse function
```

Tags can also appear on struct fields:

```rust
struct{ #pure@fn #const@field } str(...) = ...
```

`#pure@fn` means all methods are `#pure`; `#const@field` means all fields
are immutable.

---

## Function pointers and higher-order interop

When a named function or extern function is passed as a value to a
higher-order function, the compiler synthesises a **shim**:

```rust
fn ex_abs(x i64) i64 = extern("labs")

fn apply(f fn(i64) i64, x i64) i64 = return f(x)

echo apply(ex_abs, -42)    // 42
```

The shim wraps `labs` to match Tin's fat-function-pointer calling convention
`{ fn(i8* env, args...) ret*, i8* env }`. For non-capturing references, the
environment pointer is `null`.

Parameters that require ABI coercion (e.g., Tin fat-string `{i8*,i64}` →
C `i8*`) are converted inside the shim; the return value is wrapped back to
Tin conventions via `wrapFromExtern`. This means extern functions retain
correct type information when returned as `any` or inspected with `typeof`.
See [09 – Reflection](09-reflection.md) for details.

---

## Macros

Macros are code-transforming functions that operate on syntax at compile time.
They are declared with `macro` instead of `fn` and return quoted code
(backtick syntax):

```rust
macro try!(action) =
  let i = "_" ++ strings::replace(guid::new(), "-", "")
  return `
    (let {i} = {action}; {i}.status == status.ok) ? {i}.val : return result.err()
  `
```

Macros with `!` in the name (`#noExcl` tag removes this requirement) must be
called with `!`:

```rust
let val = try!(do_stuff())
```

The `#noParens` tag lets a macro be called without parentheses:

```rust
macro{#noExcl #noParens} proc() =
  return `fn{#pure #recurse #noThread}`

proc fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)
```

Here `proc` expands to the tag set and `fib` receives those tags automatically.
