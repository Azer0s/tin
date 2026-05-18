# Function pointers, Calling Tin from C

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
tin build --lib mylib.tin -o mylib.o --emit-header=mylib.h
cc -c runtime/runtime.c -o runtime.o
cc driver.c mylib.o runtime.o -o driver -lpthread
```

`--lib` produces an object file with no `main`, no automatic linking.
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

### Pointer-to-struct semantics

Tin user structs carry a hidden `i32 type_id` prefix (and possibly
vtable pointers). C cannot construct one with the right layout, so
the wrapper renders any `*MyStruct` parameter / return as `void*` in
the emitted header. The pointer round-trips correctly **as long as
it was originally produced by Tin**:

```rust
fn{#interop} make_point(x i32, y i32) *point =
  return &point{x: x, y: y}

fn{#interop} get_x(p *point) i32 = return (*p).x
```

```c
void *make_point(int32_t x, int32_t y);   // header renders *point as void*
int32_t get_x(void *p);

void *p = make_point(7, 11);
printf("%d\n", get_x(p));  // 7
tin_release(p);
```

`*void` and `*MyStruct` produce the same C-side signature; pick
whichever reads better in your Tin code. **C must NOT allocate the
struct itself** - the type_id prefix would be wrong and field reads
would silently return garbage.

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

- Naturally-aligned packed structs of **<= 8 bytes** travel in a single
  integer register (e.g. `struct{#packed} pt = x i32; y i32` -> `i64`).
- Packed structs whose layout would have padding under natural
  alignment travel via byval pointer / sret hidden return slot (e.g.
  `struct{#packed} small = a u8; b u16; c u8` -> byval).
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

Non-`#packed` user structs remain rejected - they carry trait vtable
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

### Function-pointer returns (closures back to C)

A `#interop` function may also **return** a `fn(args...) ret` value.
The wrapper hands C a stable function pointer backed by a per-instance
trampoline (a small mmap'd stub) that captures the Tin closure's
environment and tail-jumps to a per-signature dispatcher. C calls the
returned pointer like any other function pointer; each Tin closure
gets its own pointer, so multiple instances coexist without sharing
state.

```rust
fn{#interop} make_adder(base i64) fn(i64) i64 =
  return fn(x i64) i64 = return base + x
```

The emitted header pre-declares one typedef per unique callback-return
signature, then uses it as the wrapper's return type. Names follow
`tin_cb_<ret>_from_<args>_t` using Tin's own type spellings (e.g.
`i64`, `string`, `bool`, `slice_i64`):

```c
typedef int64_t (*tin_cb_i64_from_i64_t)(int64_t);
tin_cb_i64_from_i64_t make_adder(int64_t base);
```

Other examples:
- `fn() i64`        -> `tin_cb_i64_from_void_t`
- `fn(string) string` -> `tin_cb_string_from_string_t`
- `fn(i64) bool`    -> `tin_cb_bool_from_i64_t`
- `fn([i64]) i64`   -> `tin_cb_i64_from_slice_i64_t`
- `fn(*i64) void`   -> `tin_cb_void_from_i64ptr_t`

C usage:

```c
tin_cb_i64_from_i64_t add5  = make_adder(5);
tin_cb_i64_from_i64_t add42 = make_adder(42);
printf("%lld %lld\n", (long long)add5(10), (long long)add42(10));  // 15 52
tin_interop_closure_free(add5);
tin_interop_closure_free(add42);
```

`tin_interop_closure_free` releases the trampoline slot and drops the
captured environment's ARC reference (so any captured strings, slices,
or structs hit their normal Tin destructors). It is NULL-safe and
ignores pointers that are not Tin trampolines, so a single C-side
release routine can be wired through it. Trampolines that survive
process exit are munmap'd by an atexit handler so leak detectors stay
quiet.

Inner-signature restrictions match callback parameters: primitives,
pointers to primitives, `string`, `bool`. Closure-return signatures
that include `string` or `bool` get the same C-side widening / NUL-
terminated marshaling as callback-parameter thunks, applied in the
opposite direction inside the dispatcher.

Concurrency: invoking the same trampoline from multiple threads is
safe (it just reads the captured pointers and forwards). Calling
`tin_interop_closure_free` while another thread is invoking the same
trampoline is a use-after-free.

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
