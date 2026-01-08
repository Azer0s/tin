# 08 – Interop, Packages & Low-Level Features

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

## Macros

Macros are code-transforming functions that operate on syntax at compile time.
They are declared with `macro` instead of `fn` and return quoted code
(backtick syntax):

```rust
macro try!(action) =
  let i = "_" ++ guid::new().show().replace("-", "")
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
