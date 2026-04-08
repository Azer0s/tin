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

### By-value struct parameters and returns

When an extern function takes or returns a named Tin struct **by value**, the
compiler automatically converts between Tin's internal layout (type-ID + vtable
pointers + user fields) and the C-compatible layout (user fields only, no
metadata).

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
with struct parameters or returns. The wrapper strips the Tin metadata when
passing to C and reconstructs it on return. Nested structs are handled
recursively. Structs larger than 16 bytes are passed `byval` per AMD64
calling conventions.

### Pointer-to-struct parameters and returns

When an extern function's signature contains `*S` for a named struct `S`,
the compiler uses a **wrapper + native layout** for `S`:

| LLVM type   | Contents                                              |
|-------------|-------------------------------------------------------|
| `%S`        | `{ i32 type_id, vtable_ptrs..., i8* c_data_ptr }`     |
| `%S.native` | `{ field_0_type, field_1_type, ... }` - C layout only |

`c_data_ptr` holds a raw pointer to the live C memory. All field reads and
writes go through `c_data_ptr`, so **C mutations to the original struct are
immediately visible** through the Tin wrapper - no snapshot is taken.

```c
// scene.c
typedef struct { float x; float y; float z; } vec3;
static vec3 g_camera = {0.0f, 5.0f, 10.0f};

vec3 *get_camera(void)              { return &g_camera; }
void  move_camera(vec3 *c, float dz) { c->z += dz; }
```

```rust
//!+scene.c

struct vec3 =
  x f32
  y f32
  z f32

fn get_camera() *vec3 = extern("get_camera")
fn move_camera(c *vec3, dz f32) void = extern("move_camera")

let cam = get_camera()     // cam.c_data_ptr -> &g_camera
echo (*cam).z              // 10.0
move_camera(cam, 5.0)      // C mutates g_camera.z
echo (*cam).z              // 15.0  (live view - no snapshot)
```

When passing `*S` back to C (e.g., as a parameter), the compiler automatically
extracts `c_data_ptr` and passes the raw C pointer.

#### Struct literals with pointer-to-struct types

When you create a struct literal for a type that appears as `*S` in extern
signatures, the compiler allocates a native data region alongside the wrapper
and wires `c_data_ptr` to it:

```rust
let color = color{r: 255, g: 0, b: 128, a: 255}
draw_rect(x, y, w, h, &color)   // passes raw C pointer to native fields
```

#### Nested pointer-to-struct types

If a struct that appears as `*S` has fields of another struct type `T`, `T`
is also treated as a pointer-to-struct type transitively. Field access on an
embedded `T` value reads directly from its position in the C memory layout
(no additional indirection).

```rust
struct inner_t =
  x i64
  y i64

struct outer_t =
  a inner_t
  b inner_t
  tag i64

fn get_data() *outer_t = extern("get_data")

let d = get_data()
echo (*d).a.x   // direct read from C memory
echo (*d).b.y   // nested field, still live view
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

By default, when an extern function returns `*S`, Tin stores the raw C pointer
in `c_data_ptr` and does **not** take ownership - C still manages that memory.
The `#handover` tag tells the compiler that C is transferring ownership of the
returned pointer to Tin's ARC system.

```rust
fn{#handover} make_buffer() *i8  = extern("make_buffer")
fn{#handover} make_point()  *vec2 = extern("make_point")
```

### Non-handover vs handover behaviour

| Mode                   | What Tin does with the raw C pointer                                               |
|------------------------|------------------------------------------------------------------------------------|
| Non-handover (default) | Stores raw C pointer in `c_data_ptr`; C owns memory; mutations immediately visible |
| `#handover`            | Copies C data into a fresh RC-managed block; C ptr freed; Tin owns the copy        |

Use non-handover for long-lived C objects (e.g., a scene graph node returned
by a C engine). Use `#handover` when C allocates a temporary and expects the
caller to own and free it.

### What happens at the call site with `#handover`

The compiler generates a wrapper around the C function. When it receives the
raw pointer from C it:

1. For `*StructName`: copies the native C struct data into an RC block that
   also holds the Tin wrapper (`type_id`, `c_data_ptr` pointing to the
   copied data). Frees the original C pointer if it was heap-allocated.
2. For `*i8`/`string`: copies the C string into an RC block, builds a fat-ptr.
3. For `*T` (primitive): copies the value, frees original if heap-allocated.

The returned pointer is then treated exactly like any other RC-allocated Tin
pointer: retained on assignment, released at scope exit.

### Supported return types

| Return type                          | Handover behaviour                                        |
|--------------------------------------|-----------------------------------------------------------|
| `*T` (primitive)                     | `_tin_ptr_handover`: copies value, frees original         |
| `*StructName`                        | Copies native layout into RC block; c_data_ptr -> copy    |
| `*i8` / `*char` returned as `string` | Copies string into RC block, builds fat-ptr               |
| `*i8` / `*char` returned as `atom`   | Looks up/registers atom, then frees the `char*`           |

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
