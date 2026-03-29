# 08 - C Interop & Low-Level Features

## Linker directives (`//!`)

Files can declare linker flags as special comments starting with `//!`:

```rust
//!-lm
//!-lraylib
//!-lpthread
```

Each `//!` line is appended verbatim to the linker command line. Common uses:

| Directive      | Purpose                          |
|----------------|----------------------------------|
| `//!-lm`       | Link the C math library (`libm`) |
| `//!-lraylib`  | Link Raylib                      |
| `//!-lpthread` | Link pthreads                    |

Directives must appear before any non-comment code in the file.

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

```rust
fn ex_printf(fmt const *char, ...) i32 = extern(printf)
```

The Tin function `ex_printf` becomes a direct call to the C symbol `printf`.
Extern functions are automatically tagged `#sideffect`.

### Bulk extern import (`use extern`)

```rust
use extern (
  malloc as fn(size_t) *void,
  free   as fn(*void),
  strcpy as fn(*char, const *char) *char,
)
```

### Wrapping extern functions

It is idiomatic to wrap low-level externs with a clean Tin interface:

```rust
fn ex_printf(const *char, ...) i32 = extern(printf)

fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)
```

---

## C struct interop

When an extern function takes or returns a named Tin struct, the compiler
**automatically** converts between Tin's internal layout (which includes a
hidden type-ID field and optional vtable pointers) and the C-compatible layout
(user fields only, no metadata).

```c
// helpers.c
typedef struct { double x; double y; } point2d;

double  c_add_xy(point2d p)              { return p.x + p.y; }
point2d c_make_point(double x, double y) { return (point2d){x, y}; }
```

```tin
//!+helpers.c

struct point2d =
  x f64
  y f64

fn c_add_xy(p point2d) f64            = extern("c_add_xy")
fn c_make_point(x f64, y f64) point2d = extern("c_make_point")

let p = point2d{x: 3.0, y: 4.0}
echo c_add_xy(p)        // 7

let p2 = c_make_point(1.5, 2.5)
echo p2.x               // 1.5
```

The compiler generates a thin `__tinwrap_<name>` for each extern function
with struct parameters or returns. The wrapper extracts user fields into a
C-native struct (or reconstructs the full Tin struct on return). Nested
structs are handled recursively. Structs larger than 16 bytes are passed
`byval` per AMD64 calling conventions.

### Pointer-to-struct parameters

Extern functions taking `*S` where `S` is a named Tin struct receive a
pointer to the C-native layout:

```tin
fn c_init_point(dst *point2d, x f64, y f64) = extern("c_init_point")
```

---

## Pointers

### Pointer types

| Syntax     | Meaning                          |
|------------|----------------------------------|
| `*T`       | Mutable pointer to `T`           |
| `const *T` | Immutable pointer to `T`         |
| `*void`    | Untyped pointer (like C `void*`) |

### Address-of and dereference

```rust
let x i64 = 42
let p *i64 = &x        // address of x
let y = *p             // dereference
*p = 100               // write through pointer
```

### Multi-level pointers

```tin
let x i64  = 42
let p  *i64  = &x
let pp **i64 = &p

echo **pp        // 42
*p = 100
echo **pp        // 100
```

### Pointer arithmetic

```rust
let video_mem *char = addr(0xB8000).(*char)
*video_mem = 'H'
video_mem += 1
```

`addr(N)` interprets an integer as an address (bare-metal / embedded use).

### Casting pointers

```rust
let raw *void   = malloc(sizeof(person))
let p   *person = raw.(*person)
```

### Struct pointer shorthand

`ptr->method()` is shorthand for `(*ptr).method()`:

```rust
let pPtr *person = &pete
echo pPtr->show()
```

---

## sizeof

`sizeof(T)` returns the byte size of a type as `size_t`:

```rust
let buf *char = malloc(10 * sizeof(*char)).(*char)
```

---

## Function pointers and higher-order interop

When a named or extern function is passed as a value to a higher-order
function, the compiler synthesises a **shim**:

```rust
fn ex_abs(x i64) i64 = extern("labs")

fn apply(f fn(i64) i64, x i64) i64 = return f(x)

echo apply(ex_abs, -42)    // 42
```

The shim wraps `labs` to match Tin's fat-function-pointer calling convention
`{ fn(i8* env, args...) ret*, i8* env }`. ABI coercions (e.g. Tin fat-string
`{i8*,i64}` -> C `i8*`) are applied inside the shim; the return value is
wrapped back to Tin conventions. This ensures `typeof` and `getfield` work
correctly on values returned through function pointers.

---

For packages, imports, and the standard library overview, see
[09 - Packages](09-packages.md).
