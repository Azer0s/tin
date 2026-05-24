# Pointers, sizeof, ownership handover


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

Pointer arithmetic and `addr(int_literal)` are unsafe operations and must be
wrapped in an `{#unsafe} { ... }` block. Outside such a block the compiler
rejects them with an error.

```rust
{ #unsafe } {
  let video_mem *char = addr(0xB8000).(*char)
  *video_mem = 'H'
  video_mem += 1
}
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

