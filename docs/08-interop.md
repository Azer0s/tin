# 08 - C Interop & Low-Level Features

(Linker directives lifted out to [08-interop-linker.md](08-interop-linker.md) -- see there first if you're
looking for `//!`)

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



## See also

- [Linker directives](08-interop-linker.md)
- [Pointers, sizeof, handover](08-interop-pointers.md)
- [Function pointers + Tin-from-C](08-interop-callbacks.md)
