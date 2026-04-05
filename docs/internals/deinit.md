# Struct `fn deinit` - destructor lifecycle

## Overview

Tin structs support an optional **destructor** method named `deinit`.  When
defined, the compiler calls it automatically every time a struct value is
released through ARC - at scope exit, when a variable is overwritten, or when
a function returns and the callee's parameters go out of scope.

`deinit` mirrors `fn init` syntactically: it is declared inside the struct
body and receives the struct by value.

```rust
struct resource =
  id i64

  fn init(this resource) =
    echo "init {this.id}"

  fn deinit(this resource) =
    echo "deinit {this.id}"
```

---

## When deinit is called

`fn deinit` is invoked from `emitRelease` (`codegen/runtime.go`) whenever
the compiler emits an ARC release for a named struct type.  The call-sites
that trigger this:

| Trigger                                    | Code path                                         |
|--------------------------------------------|---------------------------------------------------|
| Local variable goes out of scope           | `emitScopeRelease` -> `emitRelease`               |
| All scopes unwound on `return`             | `emitAllScopeReleases` -> `emitRelease`            |
| Old value overwritten by assignment        | `genAssign` -> `emitRelease` (on old value)       |
| Global variable overwritten by assignment  | `genAssign` -> `emitRelease` (on old value)       |
| Top-level `var` at program exit            | `emitTopLevelVarDeinits` -> `emitRelease`         |
| Coro parameter with `_fiber_retain` at exit| `emitAllScopeReleases` -> `emitRelease`           |

### Assignment overwrites (`genAssign`)

When a variable of a struct type with `fn deinit` is overwritten, the old
value is released before the new value is stored.  This applies to both local
variables and top-level `var` globals:

```tin
var g_mu sync::Mutex

fn{#async} setup() =
  g_mu = sync::Mutex.make()   // first call: deinit(zero_mutex) -> free(null), safe
  g_mu = sync::Mutex.make()   // second call: deinit(old_mutex) -> free(old._ptr)
```

The check is: if `typeNameOf(target.ElemType)` yields a non-empty name and
`cg.curScope.lookup(name + "_deinit")` succeeds, load the old value and call
`emitRelease` before the store.  Only struct types with an explicit `fn deinit`
are affected; primitives and structs without `deinit` are unchanged.

### Top-level globals at program exit

`emitTopLevelVarDeinits` (`codegen/globals.go`) iterates all `var` declarations
in reverse order and calls `emitRelease` on each loaded value.  This runs
immediately after `_tin_fiber_run()` returns (all fibers have finished) and
before the process exits.

In test-mode binaries this is emitted inside `genTestRunner` after
`emitFiberMainEnd`, ensuring every test's globals are cleaned up when the
runner exits.

### Coroutine parameters with `_fiber_retain`

Some stdlib types (e.g. `sync::Channel[T]`) define `fn _fiber_retain` to
manage a C-level reference count that is outside the ARC system.  When a coro
function receives such a type as a parameter:

1. The ramp block calls `_fiber_retain` (C-level RC++).
2. The parameter is registered in scope with `noDeinit: false` (not the usual
   `noDeinit: true`), because the coro co-owns the value.
3. At scope exit, `emitRelease` calls `deinit` which decrements the C-level RC.

For all other coro parameters, `noDeinit: true` is still set so the callee
does not call `deinit` on values it does not own.

### Order within a struct

For a struct with a `deinit` and RC-tracked fields:

1. `deinit(this)` is called first - the struct is still fully intact.
2. ARC-tracked fields (strings, arrays, `any`) are released.
3. Named struct fields are recursed into (each gets its own deinit + field
   releases, depth-first).

This ordering guarantees that `deinit` can safely read all fields.

---

## Nested structs

`walkRCStructFields` (`codegen/runtime.go`) now recurses into fields whose
LLVM type is a named struct, in addition to the previously handled RC-tracked
types (strings, fat arrays, `any`).  This means:

```rust
struct inner =
  name string

  fn deinit(this inner) =
    echo "deinit inner"

struct outer =
  item inner

  fn deinit(this outer) =
    echo "deinit outer"

// When an `outer` value is released:
//   1. outer.deinit called
//   2. inner.deinit called  (via field walk)
//   3. inner.name string released
```

The same walk applies to `emitRetain`, so copying an `outer` value
correctly retains the nested `inner`'s string fields.

---

## Implementation

### `codegen/runtime.go` - `walkRCStructFields`

```go
for i, ft := range fieldTypes {
    _, isNestedStruct := ft.(*irtypes.StructType)
    if !isRCTrackedType(ft) && !isNestedStruct {
        continue
    }
    // ... GEP + load + visit(fieldVal)
}
```

Previously only `isRCTrackedType` fields were visited; the addition of
`isNestedStruct` makes the walk cover nested named structs as well.

### `codegen/runtime.go` - `emitRelease`

```go
// Named struct: call deinit (if defined) before releasing RC fields.
structName := cg.typeNameOf(val.Type())
if structName != "" && cg.curScope != nil {
    deinitName := structName + "_deinit"
    if entry, ok := cg.curScope.lookup(deinitName); ok {
        if fn, ok2 := entry.val.(*ir.Func); ok2 {
            args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
            block.NewCall(fn, args...)
        }
    }
}
```

The lookup uses the same `cg.curScope` chain as the rest of the codegen,
so it finds `StructName_deinit` regardless of package nesting depth.

No parser changes were needed - `fn deinit` is already parsed as a regular
`*ast.FuncDecl` inside `StructDecl.Methods`, and the existing method
compilation machinery (predeclare -> compile -> register in scope) handles it
identically to any other method.

---

## Edge cases and known limitations

### Value semantics: ownership model

Tin structs are value types.  The deinit model distinguishes **owners** from
**borrowers**:

- A named variable (`let`/`var` binding) is the **owner**.  `deinit` is called
  when that variable goes out of scope.
- A **function parameter** is a by-value copy loaned to the callee.  The
  callee is a borrower; `deinit` is NOT called when the parameter goes out
  of scope at function exit (only the RC-tracked field counts are balanced via
  retain-on-entry / release-on-exit).
- A **coro parameter whose struct type defines `fn _fiber_retain`** takes
  ownership: the ramp block calls `_fiber_retain` before the first suspend,
  incrementing the type's C-level RC.  Because the coro is now a co-owner,
  `deinit` IS called at coro exit to decrement that RC.

This means `deinit` fires exactly once per struct value from the owner's
perspective, regardless of how many times the struct is passed to functions.

```rust
struct file =
  handle ptr

  fn deinit(this file) =
    close_file(this.handle)

fn read_file(f file) string = ...   // borrows f; deinit NOT called on exit

let f = file{handle: open("data.txt")}
let s = read_file(f)   // f is copied; caller's f still valid
// f goes out of scope here: deinit called once - correct
```

### Return transfers ownership

When a function returns a struct, the compiler skips releasing the returned
value inside the callee (via `skipName` in `emitAllScopeReleases`).  The
caller's variable inherits ownership and `deinit` fires when that variable
goes out of scope.

### Boxing to `any`

When a struct is boxed into an `any` value, the `_TinAny.ptr` field points
to the boxed data.  `_tin_release` on that pointer frees the heap block but
does **not** call `deinit`, because the runtime's `_tin_release` function
has no knowledge of the stored type.

**Known limitation**: structs with non-trivial `deinit` semantics should not
be boxed to `any`.

### Slices of structs

Slice elements are stored as raw `memcpy`'d bytes.  When a `[S]` slice is
released the ARC block containing the elements is freed, but individual
element `deinit` methods are **not** called.

**Known limitation**: structs with non-trivial `deinit` semantics should not
be stored in slices.

### Extern C calls

Structs passed by value to extern C functions are copies from the Tin side.
The C callee has no knowledge of ARC or `deinit`.  The Tin copy is released
(and `deinit` called) when it goes out of scope in the calling Tin function -
this is correct behaviour.

### `defer`

`defer` executes deferred statements at function exit, after all local
variables have been released.  If a struct variable has a `deinit`, it fires
at its normal scope-exit point (before the deferred calls run if the variable
is in an inner scope, or when the function returns if it is in the outermost
scope).
