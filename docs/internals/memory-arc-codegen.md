# ARC heap allocation + retain/release codegen rules


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

| LLVM type                  | Tin type             | ARC-tracked? | Notes                                       |
|----------------------------|----------------------|--------------|---------------------------------------------|
| `{i8*, i64}`               | `string` / `atom`    | Yes          | ptr may be immortal or rc-alloc'd           |
| `{T*, i64}`                | `[T]` (typed array)  | Yes          | ptr always rc-alloc'd                       |
| `{i32, i8*}`               | `any`                | Yes          | ptr always rc-alloc'd (boxed value)         |
| `T*`                       | `*T` in `[*T]` array | In-array     | Element pointer must be heap-allocated; see below |
| Named struct               | user-defined struct  | Indirect     | Fields checked recursively                  |
| `i64`, `f64`, `bool`, etc. | primitives           | No           | value types, no heap                        |

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

Tin uses a **caller-keeps-the-share** model. The caller's binding holds
the rc throughout the call; the body never touches the rc of its
parameters *unless* the body reassigns one of them (in which case that
parameter reverts to the classical owned model for the duration of the
function).

The codegen (`codegen/runtime.go` and `codegen/exprs_call.go`) inserts
ARC calls at these points:

| Event                                                      | ARC call                                                       |
|------------------------------------------------------------|----------------------------------------------------------------|
| Variable assigned from an existing reference (identifier)  | `_tin_retain(new_ref)`                                         |
| Variable goes out of scope                                 | `_tin_release(var)`                                            |
| Variable overwritten                                       | `_tin_release(old)`, then retain the new value                 |
| `return ident` where `ident` is a borrowed RC binding (most params) | `_tin_retain` at the return site so the caller's receiving binding gets its own share |
| Temporary result from `++` or function call used in `echo` | `_tin_release(temp)` after use                                 |
| Function argument default `f(a)`, `a` still live after     | no rc work at the call site                                    |
| Function argument default `f(a)`, `a`'s last use           | post-call `_tin_release` + caller scope-exit on `a` skipped (implicit move) |
| Function argument with explicit `move`                     | post-call `_tin_release`; caller's scope-exit release skipped  |
| Function argument with explicit `ref` (asserts transparent) | no rc work at the call site                                    |
| Function argument with explicit `copy(a)`                  | deep-copy `a` into a fresh temp; pass temp via default protocol |
| Method called on temporary struct receiver (`tmp.method()`) | `emitRelease(tmp)` after the outer call                       |
| `*ptr` when `ptr` is a temporary producer                  | `_tin_release(ptr)` on the outer allocation                    |

**Function body emission for parameters:**

- *Non-reassigning param* (analyzer proves the body never executes
  `p = ...`): no entry retain, no scope-exit release. The param's
  scope entry is classified `ownershipBorrowed`. Return-of-this-param
  emits one retain at the return site.
- *Reassigning param* (analyzer sees any reassign of the param name):
  entry retain + scope-exit release + normal release-old/retain-new
  on every store (classical owned model). The entry retain gives the
  body its own +1 to manage independently of the caller's binding.

In both cases, the body's *sinks* (`p.box = b`, `chan.send(b)`, etc.)
emit their own retains on the value being stored - those are sink-side
rc work, unrelated to the parameter classification.

The per-parameter **convention** computed by the borrow analyzer:

- `transparent` - body neither sinks nor reassigns. Eligible for `ref`.
- `consumes`    - body sinks but does not reassign.
- `retains`     - body reassigns the param (or escapes the rc in an
  opaque way the analyzer cannot decompose).

The convention drives the `ref` keyword's compile-time check
(`transparent` is the only one that admits `ref`) and shows up in
`--explain-ownership`. It does **not** by itself decide the body-side
rc emission - that decision is the binary "does the body reassign this
param?" (`transparent` and `consumes` share the borrowed body codegen;
only `retains` uses the owned model).

Fresh allocations (`++` concat, slice append, function return values)
start with `rc = 1`. If they are stored in a named variable, the
scope-exit release brings `rc` back to zero. If they are used as
temporaries (e.g., passed directly to `echo`), an explicit release is
emitted at the use site.

### Call-site keywords

| Keyword     | Caller behavior                                                                 |
|-------------|---------------------------------------------------------------------------------|
| `f(a)`      | Default: nothing if `a` is still live after, post-call release + skip scope-exit if it's `a`'s last use (implicit move). |
| `f(ref a)`  | Asserts callee is `transparent`. Compile error otherwise. Emits no rc work.     |
| `f(move a)` | Post-call `_tin_release`; caller's scope-exit release skipped. Marks `a` ownership-moved. Allowed for any callee convention. |
| `f(copy(a))`| Lowers to `let __t = deep_copy(a); f(__t)`. `__t` is fresh (`rc=1`) and goes through the default protocol. `a` is untouched. |

All four lower to the same LLVM function body for `f`. The only thing
that varies is the rc operations the caller emits around the call
instruction itself.

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
| `return &param`                    | Yes   | Parameter value copied into `_tin_rc_alloc`; caller releases   |
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

---

