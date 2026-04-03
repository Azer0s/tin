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

## ARC heap allocation

Any pointer obtained via Tin's address-of operator or return-escape promotion
is ARC-managed. `mem::free` is never needed in normal Tin code - it is only
for explicitly `mem::malloc`'d blocks used in C interop.

**`&struct{}` literal** - inline RC-alloc, `isHeapOwned` on the holder:

```tin
let p = &point{x: 10, y: 20}   // _tin_rc_alloc; freed when p leaves scope
```

**`return &localVar`** - late promotion at the return site, `heapPromotingFns`
marks the callee so the caller sets `isHeapOwned` automatically:

```tin
fn make_counter() *i64 =
  let x i64 = 0
  return &x          // x promoted to _tin_rc_alloc block at return

let c = make_counter()   // isHeapOwned; freed when c leaves scope
```

Both paths use `_tin_rc_alloc` and clean up via `emitHeapChainRelease` at
scope exit. Calling `mem::free` on either kind of pointer is undefined behavior
(the user-data pointer passed to C `free` is 8 bytes past the actual header).

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

---

## Heap promotion

### The problem

A local variable declared with `let` lives on the stack (`alloca` in LLVM IR).
If a pointer to that variable escapes the function frame - typically via
`return &x` - the caller receives a dangling pointer. The stack frame is gone as
soon as the callee returns.

```tin
fn bad() *i64 {
    let x = 42
    return &x  // would be a dangling pointer if x lives on the stack
}
```

### What the compiler does

Before generating the body of any function, the codegen runs a lightweight
**escape analysis** (`findEscapingAddressTakenVars` in `codegen/stmts.go`).
It walks the AST and collects every local variable whose address is returned:

- `return &x` - direct address escape
- `let p = &x; return p` - escape via alias variable
- `return (&x, y)` - escape inside a tuple literal

When a variable is found to escape, the promotion happens at the return site
(`emitChainedHeapPromotion`). The variable lives on the stack during the
function body; just before the return, it is copied into a fresh
`_tin_rc_alloc` block and the heap pointer is returned:

```
; x lives on the stack during the function body
%x = alloca i64
...
; at the return site - copy into ARC block
%sz   = <llvmSizeOf i64>
%heap = call i8* @_tin_rc_alloc(i64 %sz)   ; rc = 1
%xptr = bitcast i8* %heap to i64*
%val  = load i64, i64* %x
store i64 %val, i64* %xptr
ret i64* %xptr
```

The returned pointer is ARC-managed. The compiler tracks which functions use
late heap promotion in `heapPromotingFns`. At the call site, if the callee is
in `heapPromotingFns`, the result variable is marked `isHeapOwned = true` and
`emitHeapChainRelease` frees it automatically at scope exit - no `mem::free`
needed.

### `noRelease` flag

Heap-promoted variables in the **callee** are marked `noRelease: true` in the
scope entry so scope-exit cleanup skips them - the callee must not release
memory it is handing off to the caller. The **caller** gets `isHeapOwned = true`
on the receiving variable and releases it at scope exit instead.

### Which patterns trigger promotion

| Pattern                            | ARC?  | Notes                                                          |
|------------------------------------|-------|----------------------------------------------------------------|
| `return &localVar`                 | Yes   | Late-promoted to `_tin_rc_alloc`; caller auto-releases         |
| `let p = &localVar; return p`      | Yes   | Alias chain - same promotion                                   |
| `return (&x, y)`                   | Yes   | Tuple element - x promoted                                     |
| `let p = &x; let q = &p; return q` | Yes   | Transitive chain - both promoted                               |
| `let p = &MyStruct{}`              | Yes   | Inline RC-alloc; freed when `p` leaves scope                   |
| `return &MyStruct{}`               | Yes   | Inline RC-alloc; caller takes ownership                        |
| `return localVar`                  | No    | Value return - no pointer                                      |
| `return MyStruct{}`                | No    | Struct returned by value                                       |
| `let p = &x; foo(p)` (no return)   | No    | Pointer stays local - no promotion needed                      |
| `let p = &x; return y`             | No    | Alias exists but only `y` is returned                          |

### Limitations

- **Parameter addresses**: `return &param` is not tracked - parameters use plain
  `alloca` regardless. Returning the address of a parameter is undefined behavior.
- **Name shadowing**: if two variables in different scopes share the same name and
  one escapes, both get heap-promoted. Wasteful but safe.
- **Non-return escapes**: passing `&x` to a function that stores it externally
  (e.g., a channel send) is not tracked. The analysis is limited to return-path
  escapes only.

### Implementation files

| File               | Role                                                                                                                                         |
|--------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| `codegen/stmts.go` | `findEscapingAddressTakenVars`, `walkForAliases`, `walkForEscapes`, `markEscapeVal`, `markEscapeChain`; heap-allocation path in `genVarDecl` |
| `codegen/funcs.go` | Sets `cg.curFnEscapingVars` before compiling each function body                                                                              |
| `codegen/coro.go`  | `llvmSizeOf` - GEP null-pointer trick to compute `sizeof(T)` as an LLVM value                                                                |
| `codegen/scope.go` | `noRelease` field on `scopeEntry` prevents scope-exit free                                                                                   |
