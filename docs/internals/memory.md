# Memory - ARC and Raw Allocation

## Overview

Tin uses **Automatic Reference Counting (ARC)** for heap-managed objects. Every
ARC block carries a small header immediately before its data. The compiler emits
`_tin_retain` and `_tin_release` calls at every point where a reference is
copied or goes out of scope.

Static string literals and other immortal objects opt out of ARC via a sentinel
value, eliminating all reference-counting overhead for compile-time data.

---

## `TinRCHdr` - the ARC header

```c
typedef struct { int64_t rc; } TinRCHdr;
#define TIN_IMMORTAL_RC ((int64_t)-1)
```

Every ARC-managed heap block is laid out as:

```
low address
  ┌─────────────────────────────┐
  │  TinRCHdr  { int64_t rc }   │  <- 8 bytes
  ├─────────────────────────────┤
  │  user data ...              │  <- public pointer starts here
  └─────────────────────────────┘
high address
```

The **public pointer** (stored in `TinString.ptr`, `TinSlice.ptr`, struct
fields, etc.) points to the first byte of user data, not to the header.
The header is recovered by subtracting `sizeof(TinRCHdr)`:

```c
static inline TinRCHdr *_rc_hdr(void *ptr) {
    return (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
}
```

### Reference count semantics

| `rc` value               | Meaning                                                                                                              |
|--------------------------|----------------------------------------------------------------------------------------------------------------------|
| `> 0`                    | Number of live references. `_tin_release` decrements; frees when it hits 0.                                          |
| `0`                      | No live references - the block is freed (no pointer should hold this state).                                         |
| `-1` (`TIN_IMMORTAL_RC`) | Immortal: never retained, never freed. Used for compile-time string literals and static data embedded in the binary. |

---

## `_tin_rc_alloc` - allocate an ARC block

```c
void *_tin_rc_alloc(int64_t size);
```

Allocates `sizeof(TinRCHdr) + size` bytes, initializes `rc = 1`, and returns
the public pointer (past the header). The caller owns one reference.

`_tin_rc_alloc` aborts with `"tin: out of memory"` on allocation failure rather
than returning NULL, so callers never need to check the return value.

---

## `_tin_retain` - increment reference count

```c
void _tin_retain(void *ptr);
```

Increments the reference count of the ARC block that `ptr` points into.

Skips:
- `NULL` - safe no-op.
- Blocks with `rc == TIN_IMMORTAL_RC` - immortal objects are never retained.

Called by the compiler whenever a reference is **copied** (assigned to a new
variable, passed to a function, stored in a struct field, etc.).

---

## `_tin_release` - decrement reference count and maybe free

```c
void _tin_release(void *ptr);
```

Decrements the reference count. When `rc` reaches zero, the entire block
(header + data) is freed with a single `free(hdr)`.

Skips:
- `NULL` - safe no-op.
- Immortal blocks - never freed.

Called by the compiler at every point a reference leaves scope or is
overwritten.

---

## Immortal sentinel

String literals in tin source (e.g. `"hello"`) are compiled into the binary as
global constants. Their memory layout mimics an ARC block:

```c
// Example for the string "hello"
static const struct {
    int64_t rc;          // TIN_IMMORTAL_RC = -1
    char    data[6];     // "hello\0"
} __str_hello = { -1, "hello" };
```

The `TinString.ptr` for this literal points to `__str_hello.data`, four bytes
past the `rc` field. `_tin_retain` and `_tin_release` see `rc == -1` and skip
the block entirely - no bookkeeping for static data.

---

## Raw memory helpers (`mem.c`)

For cases where ARC overhead is unwanted (e.g. temporary internal buffers),
two simple wrappers are available:

```c
void *_tin_malloc(int64_t size);
void  _tin_free(void *p);
```

`_tin_malloc` aborts on failure; `_tin_free` is a direct `free`. These are
used sparingly inside the runtime itself (e.g. `_tin_str_concat` uses plain
`malloc` for the concatenated buffer). In user code, ARC functions are
preferred.

---

## Which types are ARC-tracked?

`isRCTrackedType` (`codegen/runtime.go`) determines whether the compiler
inserts retain/release calls for a value of a given type:

| LLVM type                  | Tin type            | ARC-tracked? | Notes                               |
|----------------------------|---------------------|--------------|-------------------------------------|
| `{i8*, i64}`               | `string` / `atom`   | Yes          | ptr may be immortal or rc-alloc'd   |
| `{T*, i64}`                | `[T]` (typed array) | Yes          | ptr always rc-alloc'd               |
| `{i32, i8*}`               | `any`               | Yes          | ptr always rc-alloc'd (boxed value) |
| Named struct               | user-defined struct | Indirect     | Fields checked recursively          |
| `i64`, `f64`, `bool`, etc. | primitives          | No           | value types, no heap                |

For **named structs**, `walkRCStructFields` recursively retains/releases any
fields whose types are ARC-tracked. A struct is never itself ARC-managed -
its fields are.

`extractRCDataPtr` extracts the `i8*` that `_tin_retain`/`_tin_release` will
receive:

- **string** `{i8*, i64}` -> field 0 directly (already `i8*`)
- **fat array** `{T*, i64}` -> field 0 bitcast to `i8*`
- **any** `{i32, i8*}` -> field 1 directly

---

## When does the compiler emit retain/release?

The codegen (`codegen/runtime.go`) inserts ARC calls at these points:

| Event                                                      | ARC call                                        |
|------------------------------------------------------------|-------------------------------------------------|
| Variable assigned from an existing reference (identifier)  | `_tin_retain(new_ref)`                          |
| Variable goes out of scope                                 | `_tin_release(var)`                             |
| Variable overwritten                                       | `_tin_release(old)`, then retain the new value  |
| Return value                                               | retain before returning; caller takes ownership |
| Temporary result from `++` or function call used in `echo` | `_tin_release(temp)` after use                  |
| Function argument passing                                  | no retain (callee borrows, not owns)            |

This follows a **caller-retain, callee-borrows** model. Ownership transfers
happen explicitly at assignment and scope exit.

Fresh allocations (`++` concat, slice append, function return values) start
with `rc = 1`. If they are stored in a named variable, the scope-exit release
brings `rc` back to zero. If they are used as temporaries (e.g., passed
directly to `echo`), an explicit release is emitted at the use site.
