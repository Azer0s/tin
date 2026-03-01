# 01 – Basics

## Hello, world

```rust
echo "Hello, world!"
```

`echo` is a built-in statement that prints its argument followed by a newline.
It works with any type  -  strings, integers, floats, booleans, and structs.

---

## Primitive types

| Type | Description | Size |
|---|---|---|
| `bool` | Boolean | 1 byte |
| `i8` | Signed 8-bit integer | 1 byte |
| `i16` | Signed 16-bit integer | 2 bytes |
| `i32` | Signed 32-bit integer | 4 bytes |
| `i64` | Signed 64-bit integer | 8 bytes |
| `u8` | Unsigned 8-bit integer | 1 byte |
| `u16` | Unsigned 16-bit integer | 2 bytes |
| `u32` | Unsigned 32-bit integer | 4 bytes |
| `u64` | Unsigned 64-bit integer | 8 bytes |
| `f32` | 32-bit float | 4 bytes |
| `f64` | 64-bit float | 8 bytes |
| `string` | UTF-8 string | fat pointer |
| `char` | Single byte character (`u8`) | 1 byte |
| `any` | Dynamically-typed box (type-id + heap pointer) | 16 bytes |

Integer literals default to `i64`; float literals default to `f64`.
The compiler coerces integer literals to the required width automatically.

The `any` type can hold a value of any other type at runtime. See
[09 – Reflection](09-reflection.md) for full details.

---

## Integer literal formats

Integer literals can be written in decimal, hexadecimal, octal, or binary:

| Prefix | Base | Example | Decimal value |
|--------|------|---------|---------------|
| (none) | 10  -  decimal | `255` | 255 |
| `0x` / `0X` | 16  -  hexadecimal | `0xFF` | 255 |
| `0o` / `0O` | 8  -  octal | `0o377` | 255 |
| `0b` / `0B` | 2  -  binary | `0b11111111` | 255 |

```rust
let mask  i64 = 0xFF00FF        // hex
let perms i64 = 0o755           // octal  (rwxr-xr-x)
let flags i64 = 0b1010_0011     // binary (underscores not yet supported)
```

All bases produce an `i64` literal; the `as` operator or an explicit type
annotation coerces to a narrower type.

---

## null

`null` is the zero pointer literal, assignable to any pointer type:

```rust
let p *i64  = null
let s *char = null
```

Dereferencing a null pointer is undefined behaviour (no runtime null check).

---

## The ternary operator

Tin supports the C ternary expression `cond ? a : b`:

```rust
let abs_x = x < 0 ? 0 - x : x
```

---

## Variables

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

`const` declares a compile-time constant. Its type must be specified:

```rust
const MAX i32 = 100
const PI  f64 = 3.14159265
```

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

| Category | Operators |
|---|---|
| Arithmetic | `+` `-` `*` `/` `%` |
| Comparison | `==` `!=` `<` `<=` `>` `>=` |
| Logical | `&&` `\|\|` `!` |
| Bitwise | `&` `\|` `^` `<<` `>>` |
| Unary | `-` (negation) `!` (boolean not) |
| Increment / decrement | `i++` `i--` (statement form) |
| Array append | `++=` |
| Concatenation | `++` |
| Pipe | `\|>` (see [03 – Functions](03-functions.md)) |

Tin uses **C-style operator precedence**.
