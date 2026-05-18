# 01 - Basics

## Hello, world

```rust
echo "Hello, world!"
```

`echo` is a built-in statement that prints its argument followed by a newline.
It works with any type  -  strings, integers, floats, booleans, and structs.

---

## Primitive types

| Type     | Description                                    | Size        |
|----------|------------------------------------------------|-------------|
| `bool`   | Boolean                                        | 1 byte      |
| `i8`     | Signed 8-bit integer                           | 1 byte      |
| `i16`    | Signed 16-bit integer                          | 2 bytes     |
| `i32`    | Signed 32-bit integer                          | 4 bytes     |
| `i64`    | Signed 64-bit integer                          | 8 bytes     |
| `u8`     | Unsigned 8-bit integer                         | 1 byte      |
| `u16`    | Unsigned 16-bit integer                        | 2 bytes     |
| `u32`    | Unsigned 32-bit integer                        | 4 bytes     |
| `u64`    | Unsigned 64-bit integer                        | 8 bytes     |
| `f32`    | 32-bit float                                   | 4 bytes     |
| `f64`    | 64-bit float                                   | 8 bytes     |
| `string` | UTF-8 string                                   | fat pointer |
| `char`   | Single byte character (`u8`)                   | 1 byte      |
| `any`    | Dynamically-typed box (type-id + heap pointer) | 16 bytes    |

Integer literals default to `i64`; float literals default to `f64`.
The compiler coerces integer literals to the required width automatically.

The `any` type can hold a value of any other type at runtime. See
[10 - Reflection](10-reflection.md) for full details.

### Integer overflow

Tin integer arithmetic **wraps on overflow**. `i64::max + 1` produces
`i64::min` rather than trapping; multiplying two large `i64` values
silently truncates to 64 bits. This matches C and is intentional: the
compiler emits the same `add` / `mul` / `sub` instructions that the
CPU natively wraps on without any checks at the call site.

When the result is a compile-time constant the folder catches some
cases (`100 / 0` is a compile error), but values that flow through
function arguments, channel receives, or struct fields are
unchecked at the operation. If a particular code path must trap on
overflow, write it explicitly:

```tin
fn add_or_panic(a i64, b i64) i64 =
  let r = a + b
  if (b > 0 and r < a) or (b < 0 and r > a) :
    panic("i64 overflow in add")
  return r
```

Tin does **not** insert overflow checks for you. Same applies to
unsigned types (`u32::max + 1 == 0`), and to floats (`f64` follows
IEEE 754 - overflow goes to `\pm inf`, divide-by-zero goes to `inf` or
`NaN` without a trap).

---

## Integer literal formats

Integer literals can be written in decimal, hexadecimal, octal, or binary:

| Prefix      | Base               | Example      | Decimal value |
|-------------|--------------------|--------------|---------------|
| (none)      | 10  -  decimal     | `255`        | 255           |
| `0x` / `0X` | 16  -  hexadecimal | `0xFF`       | 255           |
| `0o` / `0O` | 8  -  octal        | `0o377`      | 255           |
| `0b` / `0B` | 2  -  binary       | `0b11111111` | 255           |

```rust
let mask  i64 = 0xFF00FF        // hex
let perms i64 = 0o755           // octal  (rwxr-xr-x)
let flags i64 = 0b10100011      // binary (underscores not yet supported)
```

All bases produce an `i64` literal; the `as` operator or an explicit type
annotation coerces to a narrower type.

### Character code literals

`@'x'` produces the ASCII/byte value of a single character as an `i8`:

```rust
let newline i8 = @'\n'   // 10
let tab     i8 = @'\t'   // 9
let A       i8 = @'A'    // 65
let zero    i8 = @'0'    // 48
```

The following escape sequences are recognised inside `@'...'`:

| Escape | Value                |
|--------|----------------------|
| `\n`   | newline (10)         |
| `\t`   | tab (9)              |
| `\r`   | carriage return (13) |
| `\\`   | backslash (92)       |
| `\'`   | single quote (39)    |
| `\0`   | null byte (0)        |

### 8-bit type formatting

The four 8-bit scalar types render differently by default in `echo` and string
interpolation `{}`. The behaviour mirrors how byte arrays are printed:

| Type   | Default `echo` / `{}` | Example (`@'A'` = 65) |
|--------|-----------------------|-----------------------|
| `char` | character (`%c`)      | `A`                   |
| `byte` | hex lowercase (`%x`)  | `41`                  |
| `u8`   | decimal (`%d`)        | `65`                  |
| `i8`   | signed decimal (`%d`) | `65`                  |

```rust
let c char = @'A'
let b byte = 0x41
let u u8   = 65

echo c          // A
echo b          // 41
echo u          // 65

echo "{c}"      // A
echo "{b}"      // 41
echo "{u}"      // 65
```

Format specifiers override the default rendering:

```rust
echo "{c:d}"    // 65    (decimal for a char)
echo "{b:d}"    // 65    (decimal for a byte; zero-extends)
echo "{u:x}"    // 41    (hex for u8)
```

### Appending a byte/char to a string

`++` accepts a `byte`, `char`, or `u8` on either side of a string. The byte
value is appended as a single raw byte:

```rust
let c char = @'!'
let s = "hello" ++ c      // "hello!"
let s2 = c ++ " world"    // "! world"
```

---

## nil

`nil` is the zero pointer literal, assignable to any pointer type:

```rust
let p *i64  = nil
let s *char = nil
```

Dereferencing a nil pointer is undefined behaviour (no runtime nil check).

---

## The ternary operator

Tin supports the C ternary expression `cond ? a : b`:

```rust
let abs_x = x < 0 ? 0 - x : x
```

---

## Variables

`let` declares a local variable. It is always scoped to the enclosing function
- there are no module-level `let` declarations. Statements written at the top
level of a file are collected into an implicit `main` function, so even
top-level `let` lines are local to that `main`. Use `var` for mutable
package-level state (see below).

Variables are declared with `let`. Types are written after the name:

```rust
let x i64 = 42
let pi f64 = 3.14159
let greeting string = "hello"
let flag bool = true
```

The type annotation can be omitted when it can be inferred:

```rust
let x = 42           // inferred i64
let pi = 3.14        // inferred f64
let msg = "hi"       // inferred string
```

Variables are mutable by default:

```rust
let count i64 = 0
count = count + 1
echo count           // 1
```

### Constants

`const` declares a compile-time constant. The right-hand side must be a
literal, an arithmetic / cast / shift / bitwise expression, an
identifier reference to another `const`, or a call to a `#pure`
function. The type may be omitted when the initializer is a literal:

```rust
const MAX i32 = 100
const PI  f64 = 3.14159265
const FORTY_TWO = 42        // inferred i64

const A = 6
const B = A + 1             // chained references fold

fn{#pure} double(x i64) i64 = return x * 2
const Y = double(21)        // pure-call result, inferred i64 = 42
```

Top-level constants live in read-only storage (`@X = constant ...` in
LLVM IR, `.rodata` section). Writes through an aliased pointer
(`let p = &MAX; *p = 0`) are undefined behavior; the compiler emits a
`-Wwrite-to-const` warning at the write site, and the binary segfaults
at `-O0` if the alias is reached.

Block-level `const` is statically immutable too: `const xs = [1,2,3]`
followed by `xs[0] = 99` is rejected at compile time.

### Top-level (global) variables

`var` declares a mutable package-level variable. It lives outside any
function and is visible throughout the entire file:

```rust
var counter i64          // zero-initialized (0)
var name    string       // zero-initialized ("")
var flag    bool = true  // optional initializer, runs once at startup
var buf     [byte; 4096] // fixed array, zero-initialized
```

Key properties:

- **Zero-initialized** by default (`0`, `""`, `false`, all-zero array, etc.)
- An optional `= expr` initializer runs once at the start of `main()`
- **Hoisted**: visible throughout the file regardless of declaration order
- **Exported** when the name starts with an uppercase letter

```rust
var TotalRequests i64   // exported - readable from other packages

fn handle() =
  TotalRequests = TotalRequests + 1
```

For variables shared across concurrent fibers, use `sync::Atomic[i64]`
(or any other primitive: `Atomic[bool]`, `Atomic[f64]`, etc.) or a
`sync::Mutex` - plain `var` declarations are not thread-safe.

---

## Simple type aliases

`type` creates an alias for an existing type:

```rust
type char   = u8
type size_t = u64
```

After this, `char` and `u8` are interchangeable. Type aliases do not introduce
new types; they simply give an existing type a more readable name.

---

## String interpolation

Curly braces inside a string literal are replaced at runtime with the
formatted value of the expression:

```rust
let name = "Alice"
let age  = 30

echo "Name: {name}"                    // Name: Alice
echo "{name} is {age} years old"       // Alice is 30 years old
echo "2 + 2 = {2 + 2}"                // 2 + 2 = 4
```

Struct fields and method calls can appear inside `{}` too:

```rust
struct point =
  x i64
  y i64

let p = point{x: 3, y: 4}
echo "point: ({p.x}, {p.y})"          // point: (3, 4)
```

### Format specifiers

A `:` inside `{}` introduces a printf-style format specifier:

```rust
let n u32 = 255
echo "{n:08x}"     // 000000ff  (8-wide zero-padded hex)
echo "{n:d}"       // 255       (signed decimal)
echo "{n:u}"       // 255       (unsigned decimal)

let f f64 = 3.14159
echo "{f:.2f}"     // 3.14      (2 decimal places)
echo "{f:e}"       // 3.141590e+00
```

Supported specifiers: `d` / `i` (signed int), `u` / `x` / `X` / `o` (unsigned/hex/octal), `f` / `e` / `g` and their uppercase variants (float), `s` (string). A width or precision prefix (e.g. `08`, `.2`) may precede the letter.

For 8-bit types (`char`, `byte`, `u8`, `i8`) the default rendering depends on
the type (see the 8-bit type formatting table above). Format specifiers always
override the default: `{c:d}` gives the decimal value of a `char`; `{b:x}`
gives hex for a `byte`; `{u:d}` gives decimal for a `u8` even though that is
also the default.

### Escape sequences in strings

String literals support the following escape sequences:

| Escape | Meaning                                  |
|--------|------------------------------------------|
| `\n`   | newline                                  |
| `\t`   | tab                                      |
| `\r`   | carriage return                          |
| `\\`   | literal backslash                        |
| `\"`   | literal double quote                     |
| `\0`   | null byte                                |
| `\{`   | literal `{` (not an interpolation start) |
| `\}`   | literal `}` (not an interpolation end)   |

```rust
echo "line1\nline2"        // two lines
echo "path: C:\\Users"     // path: C:\Users
echo "price: \{42}"        // price: {42}  (braces not interpolated)
```

---

## Statement separators

Statements can be separated by newlines (the default) or by semicolons on the same line:

```rust
let x = 1; let y = 2; echo x + y   // 3
```

---

## Comments

```rust
// single-line comment

/*
  multi-line
  comment
*/
```

---

## Operators

| Category              | Operators                                     |
|-----------------------|-----------------------------------------------|
| Arithmetic            | `+` `-` `*` `/` `%`                           |
| Comparison            | `==` `!=` `<` `<=` `>` `>=`                   |
| Logical               | `&&` `\|\|` `!`                               |
| Bitwise               | `&` `\|` `^` `<<` `>>`                        |
| Unary                 | `-` (negation) `!` (boolean not)              |
| Increment / decrement | `i++` `i--` (statement form)                  |
| Slice concat-assign   | `++=` (RHS must be `[T]`; wrap a value: `xs ++= [v]`) |
| Concatenation         | `++`                                          |
| Pipe                  | `\|>` (see [03 - Functions](03-functions.md)) |

### Operator precedence

Higher rows bind tighter (evaluated first):

| Priority    | Operators                               | Associativity |
|-------------|-----------------------------------------|---------------|
| 1 (highest) | `()` `[]` `.` `->` (call, index, field) | left          |
| 2           | unary `-` `!` `*` `&`                   | right         |
| 3           | `*` `/` `%`                             | left          |
| 4           | `+` `-` `++`                            | left          |
| 5           | `<<` `>>`                               | left          |
| 6           | `<` `<=` `>` `>=`                       | left          |
| 7           | `==` `!=`                               | left          |
| 8           | `&` (bitwise and)                       | left          |
| 9           | `^` (bitwise xor)                       | left          |
| 10          | `\|` (bitwise or)                       | left          |
| 11          | `&&`                                    | left          |
| 12          | `\|\|`                                  | left          |
| 13          | `? :` (ternary)                         | right         |
| 14 (lowest) | `\|>` (pipe)                            | left          |

When in doubt, use parentheses.

---

## Floating-point types

`f32` is a 32-bit single-precision float; `f64` is a 64-bit double-precision
float. Float literals (e.g. `3.14`, `1.0`, `2.5e-3`) default to `f64`.

```rust
let x f64 = 3.14
let y f32 = 2.5

// integer -> float
let n i64 = 42
let f = n as f64        // 42

// float -> integer (truncates toward zero)
let i = 3.9 as i64      // 3
```

Arithmetic between `f64` values uses the standard operators:

```rust
let a f64 = 1.5
let b f64 = 2.5
echo a + b              // 4
echo a * b              // 3.75
echo b / a              // 1.66667
```

The `math` standard library provides transcendental functions (`sqrt`, `sin`,
`cos`, `pow`, ...) and constants (`math::PI`, `math::E`). It requires linking
with `-lm`:

```rust
//!-lm
use math

echo math::sqrt(2.0)    // 1.41421
echo math::PI           // 3.14159
```

See [`examples/floats.tin`](../examples/floats.tin) for a complete float example.

---

## The `default` builtin

`default(T)` returns the zero value for type `T`:

| Type     | `default` value     |
|----------|---------------------|
| integers | `0`                 |
| floats   | `0.0`               |
| `bool`   | `false`             |
| pointers | `nil`               |
| strings  | `""` (empty string) |
| arrays   | `[]` (empty array)  |
| structs  | all fields zeroed   |

```rust
let n  = default(i64)     // 0
let f  = default(f64)     // 0.0
let ok = default(bool)    // false
```

`default` is most useful in generic code where the concrete type is not known
at the call site:

```rust
fn zero[t](x t) t =
  return default(t)
```
