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

```rust
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

```rust
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

```rust
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

## Ownership handover (`#handover`)

When a C function allocates memory and returns a pointer, Tin normally has no
way to know that it should manage that memory. The `#handover` tag transfers
ownership of the returned pointer into Tin's ARC system.

```rust
fn{#handover} make_buffer() *i8  = extern("make_buffer")
fn{#handover} make_point()  *vec2 = extern("make_point")
```

### What happens at the call site

The compiler generates a thin wrapper around the C function. When the wrapper
receives the raw pointer back from C, it:

1. Uses `malloc_usable_size` to detect whether the pointer is a heap allocation.
2. Copies the pointed-to data into a fresh RC-managed (`_tin_rc_alloc`) block.
3. If the original was heap-allocated, frees it. If it was stack or static,
   only the copy is kept (no free).

The returned pointer is then treated exactly like any other RC-allocated Tin
pointer: retained on assignment, released at scope exit.

### Supported return types

| Return type     | Handover behaviour                                              |
|-----------------|-----------------------------------------------------------------|
| `*T` (primitive / struct pointer) | `_tin_ptr_handover`: copies element, frees original if malloc'd |
| `*i8` / `*char` returned as `string` | `_tin_string_handover`: RC-ifies the `char*`, builds fat-ptr |
| `*i8` / `*char` returned as `atom`   | Looks up/registers atom, then frees the `char*`              |
| `*StructName`   | Loads native layout, adds `type_id`, stores in RC block, frees original |

`#handover` has no effect on non-pointer return types.

### Example

```c
// helpers.c
#include <stdlib.h>
#include <stdint.h>

int64_t *make_i64_ptr(void) {
    int64_t *p = malloc(sizeof(int64_t));
    *p = 42;
    return p;
}

typedef struct { int64_t x; int64_t y; } c_vec2;

c_vec2 *make_vec2_ptr(void) {
    c_vec2 *v = malloc(sizeof(c_vec2));
    v->x = 10;
    v->y = 20;
    return v;
}
```

```rust
//!+helpers.c
use assert

struct vec2 =
  x i64
  y i64

fn{#handover} get_i64() *i64  = extern("make_i64_ptr")
fn{#handover} get_vec2() *vec2 = extern("make_vec2_ptr")

test "handover primitive" =
  let p = get_i64()        // p is RC-managed; C's malloc block is freed
  assert::equals(*p, 42)   // p released at end of scope - no leak

test "handover struct pointer" =
  let v = get_vec2()
  assert::equals((*v).x, 10)
  assert::equals((*v).y, 20)
```

No `mem::free` is needed - ARC releases the RC block when `p` and `v` go out
of scope.

### Combining with struct pointer fields

When the returned struct itself contains pointer fields pointing to other
heap-allocated data, those fields must be handled by the C side before
returning (or by a `deinit` method on the Tin struct). `#handover` only
takes ownership of the top-level allocation.

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
