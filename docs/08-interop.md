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

### Attaching C source files (`//!+file.c`)

The `//!+file.c` directive compiles a C source file alongside the Tin program and
links the resulting object:

```rust
//!+mylib.c
```

The path is resolved relative to the `.tin` file. Compiler flags for that
translation unit follow `--`:

```rust
//!+impl.c -- -I/usr/local/include -DFOO=1
```

### Platform qualifiers

Any `//!` directive can be restricted to a specific platform by appending
`[linux]`, `[darwin]`, or `[windows]` before the `--` flags (or at end of line):

```rust
//!-lssl
//!-lcrypto
//!-L/opt/local/lib [darwin]
//!+posix_impl.c [linux]
//!+win_impl.c   [windows]
```

### Environment variable expansion

`$VAR` tokens in directive strings are expanded using the process environment.
Two variables are always available:

| Variable        | Value                                          |
|-----------------|------------------------------------------------|
| `$TIN_RUNTIME`  | Path to the Tin runtime headers directory      |
| `$TIN_STDLIB`   | Path to the Tin standard library root          |

```rust
//!+impl.c -- -I $TIN_RUNTIME
```

### Shell expression expansion (`$(cmd)`)

`$(cmd args...)` tokens are evaluated by running the command in a shell and
substituting the trimmed stdout. This is useful for locating libraries whose
prefix is reported by a tool rather than a fixed environment variable:

```rust
//!+tls_impl.c [darwin] -- -I $TIN_RUNTIME -I $(brew --prefix openssl@3)/include
//!-L$(brew --prefix openssl@3)/lib [darwin]
```

Shell expansion happens before field-splitting, so paths with spaces are handled
correctly. If the command fails the token expands to an empty string; the
compiler then produces a clear error (e.g. missing header) rather than a
cryptic path error.

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

## Calling Tin from C (`#interop`)

A Tin function tagged `#interop` is exported under its bare name as a
C-callable symbol. The compiler emits a wrapper that:

1. Lazily initialises the Tin runtime on first call (atomic, idempotent).
2. Marshals each parameter from its C ABI shape to the Tin
   representation.
3. Calls the Tin-internal entry point.
4. Marshals the return value back to a C-friendly shape.
5. Releases any temporary ARC allocations created at the boundary.

```rust
fn{#interop} add(a i32, b i32) i32 = return a + b

fn{#interop} greet(name string) string =
  return "hello, " ++ name ++ "!"

fn{#interop} sum(xs [i32]) i32 =
  let total i32 = 0
  for let v i32 in xs:
    total += v
  return total
```

```c
// driver.c
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

int32_t      add(int32_t a, int32_t b);
const char  *greet(const char *name);
int32_t      sum(const int32_t *xs, int64_t xs_len);

int main(void) {
    printf("%d\n", add(2, 3));
    const char *g = greet("World");
    printf("%s\n", g);
    free((void *)g);
    int32_t arr[] = {1, 2, 3, 4};
    printf("%d\n", sum(arr, 4));
    return 0;
}
```

### Building a Tin library

```bash
tin build -lib mylib.tin -o mylib.o --emit-header=mylib.h
cc -c runtime/runtime.c -o runtime.o
cc driver.c mylib.o runtime.o -o driver -lpthread
```

`-lib` produces an object file with no `main`, no automatic linking.
`--emit-header=PATH` writes a `.h` file declaring every `#interop`
function.

### Type mapping at the boundary

| Tin type             | C ABI shape (param)                     | C ABI shape (return)                              |
|----------------------|-----------------------------------------|---------------------------------------------------|
| `i8`..`i64`, `u8`..`u64` | matching `intN_t` / `uintN_t`       | matching `intN_t` / `uintN_t`                     |
| `f32`, `f64`         | `float`, `double`                       | `float`, `double`                                 |
| `bool`               | `uint8_t` (non-zero = true)             | `uint8_t`                                         |
| `*<primitive>`, `*void` | `T*`, `void*` (passthrough)          | `T*`, `void*` (passthrough)                       |
| `string`             | `const char*` (NUL-terminated; `strlen` on entry) | `const char*` (caller frees via extern_alloc's matching free) |
| `[T]` fat array, T = primitive or `*X` | splits into `const T* xs, int64_t xs_len` | reshapes to status return + out-params `T** out_data, int64_t* out_len` |

The validation pass rejects any other parameter or return type with a
specific message at the function declaration site. Methods, generics,
extern declarations, `#async`, `Future[T]` returns, `any` parameters,
and the reserved name `main` are all rejected at compile time. See
[13 - Control tags](13-control-tags.md#interop) for the full
restriction list.

### Pointer-to-struct: use `*void`, not `*MyStruct`

Tin user structs carry a hidden `i32 type_id` prefix (and possibly
vtable pointers). C cannot construct one with the right layout, so a
C-allocated `MyStruct *` passed to a `#interop` function reading
`p->x` would silently read **the wrong field**. To prevent this trap,
the validator rejects pointer-to-named-struct in `#interop` signatures
and asks for `*void` instead:

```rust
fn{#interop} make_point(x i32, y i32) *void =
  return (&point{x: x, y: y}) as *void

fn{#interop} get_x(p *void) i32 = return (p as *point).x
```

The C side sees these as opaque `void *` handles. As long as the
pointer was originally produced by Tin (e.g. returned from another
`#interop` call), reads work correctly. C must NOT allocate the
struct itself.

**Lifetime caveat for `*void` returns from `#interop`.** A pointer
returned from `&Foo{...}` is ARC-allocated with refcount 1. The
wrapper does not release it on the way out, so the C caller becomes
the sole owner of that reference. Use the runtime helper
`tin_release` (declared in the emitted header) to drop it:

```c
void *p = make_point(1, 2);
/* ... use p ... */
tin_release(p);   // free the Tin-allocated block, NULL-safe
```

Pass-by-value `#packed` structs **are supported**, with size
limitations dictated by the SysV x86_64 ABI:

- Naturally-aligned packed structs of **≤ 8 bytes** travel in a single
  integer register (e.g. `struct{#packed} pt = x i32; y i32` → `i64`).
- Packed structs whose layout would have padding under natural
  alignment travel via byval pointer / sret hidden return slot (e.g.
  `struct{#packed} small = a u8; b u16; c u8` → byval).
- Naturally-aligned packed structs **larger than 8 bytes** are
  rejected today (the multi-eightbyte coercion clang emits is
  content-dependent and not implemented in v1). Pass them via
  `*void` instead.

The emitted header declares each used `#packed` Tin struct as a C
`typedef struct __attribute__((packed)) { ... } Name;` so callers can
construct values directly:

```rust
struct {#packed} pt =
  x i32
  y i32

fn{#interop} make_pt(x i32, y i32) pt = return pt{x: x, y: y}
fn{#interop} sum_pt(p pt) i32 = return p.x + p.y
```

```c
pt p = make_pt(7, 11);
printf("%d\n", sum_pt(p));  // 18
```

Non-`#packed` user structs remain rejected — they carry trait vtable
pointers and field padding that have no stable C representation.

NULL passed to a `#interop` function expecting a non-NULL pointer
will segfault inside the Tin body. Treat all pointer params as
non-nullable unless your Tin code explicitly checks.

### Strings and embedded NUL bytes

`string` parameters use C convention: the wrapper computes the length
with `strlen` on entry, so any embedded `\0` truncates the value seen
by the Tin side. For binary data or strings with deliberate NUL bytes,
use `[u8]` instead - the C signature becomes `(const uint8_t* xs,
int64_t xs_len)` and the byte buffer crosses the boundary intact.

```rust
fn{#interop} hash(buf [u8]) i64 = ...   // binary-safe; len is explicit
fn{#interop} greet(name string) string = ...   // NUL-terminated, strlen-bounded
```

### Returned strings and arrays - allocator hook

Strings and fat arrays returned from `#interop` are copied into a
caller-owned buffer via the user-configurable allocator. The default
is `malloc(3)`; replace it with `tin_set_extern_alloc`:

```c
typedef void *(*tin_alloc_fn)(size_t);
void tin_set_extern_alloc(tin_alloc_fn fn);   // NULL resets to malloc
```

The matching free is whatever pairs with the user's allocator (default
`malloc` / `free`; for arena allocators, the arena's own lifecycle).

If the allocator returns NULL the wrapper signals OOM:
- String returns yield NULL.
- Slice returns set `*out_data = NULL`, `*out_len = 0`, and return a
  non-zero status.

### Function-pointer callbacks

A `#interop` parameter typed `fn(args...) ret` accepts a raw C
function pointer. The wrapper boxes the C callback into Tin's fat
fn-ptr representation through an internally-emitted thunk, so Tin can
call it back like any other closure.

```rust
fn{#interop} apply(cb fn(i32) i32, x i32) i32 = return cb(x)
fn{#interop} fold(cb fn(i32, i32) i32, init i32, n i32) i32 =
  let acc i32 = init
  for let i i32 = 1; i <= n; i++:
    acc = cb(acc, i)
  return acc
```

The emitted header declares the callback in C function-pointer syntax:

```c
int32_t apply(int32_t (*cb)(int32_t), int32_t x);
int32_t fold(int32_t (*cb)(int32_t, int32_t), int32_t init, int32_t n);
```

Restrictions:
- Callback parameter and return types must be primitives or pointers
  to primitives. Strings, slices, structs, and other aggregates
  inside a callback signature are rejected: the thunk would have to
  marshal each call across the boundary, which v1 does not implement.
- `bool` is allowed; the thunk converts between Tin's i1 and C's
  uint8_t (matching `_Bool`/C23 `bool`, both 1 byte) on each call.
- Callbacks are accepted only as parameters, not as return types.

### Spawning fibers

`#interop` functions may spawn fibers. The runtime starts a worker
pool on first call (`tin_runtime_init`); spawned fibers run on that
pool and may outlive the wrapper invocation. The wrapper itself
returns as soon as the Tin body returns - it does not wait for
spawned-and-not-awaited fibers. Use `await` inside the body if you
need to block until a fiber completes.

C code may call `tin_runtime_init()` directly to control init timing
(e.g., to set up a custom allocator before any Tin code runs):

```c
void tin_runtime_init(void);   // idempotent, thread-safe
```

`#interop` cannot itself be `#async`, since C has no way to drive a
coroutine.

### Header generation

`tin build --emit-header=PATH` produces a single `.h` file with:

- Include guards derived from the path.
- An `extern "C"` block for C++ consumers.
- Forward declarations for `tin_set_extern_alloc` plus its function
  pointer typedef.
- One prototype per `#interop` function, with the original Tin
  signature reproduced in a leading comment for traceability.

Re-run the flag whenever signatures change; the file is overwritten.

---

For packages, imports, and the standard library overview, see
[09 - Packages](09-packages.md).
