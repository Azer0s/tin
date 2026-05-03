# rc - refcounted handles for shared C resources

`rc::Cell[T]` wraps a value -- almost always a raw C pointer or fd --
behind a Tin-managed refcount. Hold the cell as `*Cell[T]`: copying
the pointer retains, scope-exit releases, and a user-supplied destructor
runs exactly once on the last drop.

This is what lets stdlib types like `Atomic`, `Mutex`, `RWMutex`,
`Cond`, and `Regex` be copied freely. Without `rc::Cell` (or another
sharing primitive), `let b = a` for any of these would shallow-copy a
pointer and the second `deinit` would double-free.

## Usage

```rust
use rc

fn close_handle(p i64) = echo "destructor fired"

fn main() i64 =
  let cell *rc::Cell[i64] = rc::Cell[i64].alloc(42, close_handle)

  // copies retain the cell, drops release. close_handle runs once.
  let a = cell
  let b = a
  echo a.payload()       // 42
  echo b.payload()       // 42
  return 0               // "destructor fired" prints once
```

## API

```rust
struct {#no_copy #closed} Cell[T] =
  static fn alloc(value T, dtor fn(T)) *Cell[T]
  fn payload(this *Cell[T]) T
  fn retain(this *Cell[T])
```

- `Cell[T]` is `#no_copy` and `#closed`. The only way to obtain one is
  through `alloc`, and the only way to hold one is via `*Cell[T]`. See
  [#no_copy / #closed in control-tags](../13-control-tags.md#no_copy-struct-level).
- `alloc(value, dtor)` heap-allocates a cell with refcount 1, stores
  `value`, and registers `dtor` as the destructor. dtor runs exactly
  once when the last `*Cell[T]` reference drops.
- `payload()` returns the stored value (no RC change). Reads only.
- `retain()` bumps the refcount. The compiler invokes this implicitly
  through the standard pointer-ARC machinery on `let b = a`, struct-
  field assignment, etc. -- you only need to call it directly when
  something outside Tin's automatic field-ARC takes a fresh handle on
  the cell (e.g. raw fiber-frame copy via `_fiber_retain`).

## When to use Cell

Use `*rc::Cell[T]` when a Tin struct holds a C-managed resource that:

- can be heap-shared between multiple Tin owners, AND
- has a single C cleanup function that should run exactly once.

The typical `T` is `*void` (raw C handle). Integers wrapping fds work
too; the `dtor` then takes an `i64` and calls the C `close`.

## When NOT to use Cell

- For values Tin already RC-tracks (string, fat array, `any`, `fn`
  closure, `*S` to a Tin struct), the existing struct-field ARC handles
  ownership. Wrapping in Cell is redundant and adds an extra heap
  allocation per value.
- For "borrow" pointers that should never own anything, declare the
  field with `weak`. The compiler skips ARC entirely on weak fields.
- For scopes where the resource is provably stack-local and never
  shared, plain `*void` is fine. The `-Wunwrapped-c-resource` warning
  fires only when the value crosses an extern boundary AND the
  containing struct is copyable -- not every raw pointer.

## Performance

Each `Cell[T]` is one extra `_tin_rc_alloc` (24 bytes overhead +
sizeof(T) + sizeof(fn-ptr) for the destructor), plus an extra load
through `cell.payload()` on every access. Stdlib uses Cell everywhere
EXCEPT `Channel`, where the inline send/recv fast path lives in the
~100ns range and the extra load is measurable.

## Why `#no_copy` and `#closed`

If `Cell[T]` were copyable as a value, two `let b = a` copies would
share the same heap block but Tin would not know to retain on the
copy. `#no_copy` forces every reference to go through the pointer
form, where Tin's field-ARC machinery already knows how to retain.

`#closed` forbids `Cell[T]{...}` literals outside the struct's own
static methods. Without it, user code could bypass `alloc` and
construct a Cell with a bogus destructor or skip the heap allocation
entirely.

## See also

- [#no_copy / #closed control-tags](../13-control-tags.md#no_copy-struct-level)
- [-Wunwrapped-c-resource diagnostic](#wunwrapped-c-resource) below
- stdlib types using Cell internally: `sync::Atomic`, `sync::Mutex`,
  `sync::RWMutex`, `sync::Cond`, `regex::Regex`

## -Wunwrapped-c-resource

Compiler warning that fires when a struct field has the shape of a
C-managed resource (raw `*void`, `i64` named `fd`, or `i64` returned
by a known POSIX fd-returning function), the field's value transitively
crosses an extern boundary, and the field isn't wrapped in `*Cell[T]`
or another `#no_copy` wrapper.

Fix: wrap with `*rc::Cell[*void]` (or `*rc::Cell[i64]` for fds), or
mark the field `weak` if it is genuinely a borrow.

To suppress for a specific field that can't be wrapped (for example,
`Channel` which deliberately bypasses Cell for the inline send/recv
fast path), put `//!-Wno-unwrapped-c-resource` on the line above the
field declaration. Document why next to it.

```rust
struct Channel[T] =
  // Channel deliberately does NOT wrap _ptr in *rc::Cell[*void] --
  // the inline send/recv path hardcodes a single GEP+load and
  // adding Cell would force an extra load on every send/recv.
  //!-Wno-unwrapped-c-resource
  _ptr *void
```
