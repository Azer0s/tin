# Trait coercion: value vs pointer

Tin gives you two ways to hold a struct as a trait. The choice at the
`let` (or return) site is your alias signal.

### `Trait = b` -- snapshot

```rust
let f Fooable = b
```

The struct `b` is heap-copied into a fresh allocation owned by `f`.
`f` and `b` are independent storage from this point on. Mutations
through pointer-receiver impl methods land on the snapshot, not on
`b`. The copy is released when `f` goes out of scope.

This matches Go's interface assignment from a value.

### `Trait = &b` -- alias

```rust
let f Fooable = &b
f.foo(2)
echo b.v        // mutated -- f and b share the same storage
```

The trait fat-pointer's `data` slot aliases `&b` directly. Mutations
through pointer-receiver impl methods propagate to `b`. The
compiler traces the source pointer's provenance to decide
ownership:

- RC source (`&T{...}` or any `_tin_rc_alloc`-rooted pointer):
  retain to give the iface its own slice of the lifetime; the
  scope-exit release balances.
- Stack borrow (`&local_var`) or external pointer (`mem::malloc +
  cast`, FFI returns): swap in the trait's no-op borrow vtable so
  the iface doesn't try to release storage it doesn't own. Same
  outlives gotcha as any other `*T` borrow -- `b` must outlive
  `f`.

### `*Trait` -- discouraged

```rust
let a *Fooable = &b
(*a).foo(2)
```

`*Trait` is a pointer to a trait fat-pointer. Since the fat-pointer
already carries a heap pointer in its `data` slot, the outer `*` is
a second indirection on top of an indirection that's already there.
The compiler emits `-Wptr-trait` whenever `*Trait` shows up in a
function signature or struct field. Prefer the value-form `Trait =
&b` -- it has the same alias semantics with one fewer level of
indirection.

### Receiver shape

Each trait method may declare its `this` receiver as `this T`
(value) or `this *T` (pointer). The two forms differ only in what
the vtable adapter does at dispatch:

- `this *Self` -- adapter passes the trait fat-pointer's `data` slot
  directly. Mutations to `this.field` land on whatever the source
  binding was (the snapshot or the aliased original).
- `this Self` -- adapter loads from `data` into a stack-local copy
  and passes that. Mutations to `this.field` are local and discarded
  when the method returns. Useful for the "this method won't change
  observable state" signal at the API level.

Mixed receivers within a single trait are fine (Go's standard
pattern -- read methods on value, mutating methods on pointer):

```rust
trait Map[K, V] =
  fn len(this Map[K, V]) i64
  fn get(this Map[K, V], k K) V
  fn set(this *Map[K, V], k K, v V)
  fn delete(this *Map[K, V], k K)
```

Each impl method must match the trait def's declared receiver per
method:

```rust
struct HashMap[K, V](Map[K, V]) =
  ...
  fn Map::len(this HashMap[K, V]) i64 = ...           // matches def
  fn Map::set(this *HashMap[K, V], k K, v V) = ...    // matches def

  // fn Map::len(this *HashMap[K, V]) i64    -- ERROR: def says value
```

If the trait method omits `this` entirely
(`fn next() Option[t]`), each impl is free to choose value or
pointer per its own needs -- the trait def isn't pinning a contract
on that method.

### Default methods that mutate forward fields

A default method that assigns to a forward field must have a
pointer receiver. The trait def has to say `this *T` up front; a
value-receiver default that tries to mutate a forward field is
rejected at the trait declaration:

```rust
trait counter =
  count i64 forward
  fn inc(this counter) =            // value receiver
    this.count = this.count + 1     // ERROR: would mutate a copy
```

The fix is `this *counter`. The auto-injected default on every
implementing struct then carries the same pointer receiver and the
mutation lands on the caller's storage.

### Warning: value-source coerce to a trait that mutates

When the source is a value (`let f Trait = StructLit{...}`) and any
impl method on the source struct takes `*Self`, the compile is
legal but unusual -- the trait fat-ptr owns a heap snapshot and
mutations through pointer-receiver methods won't propagate. The
compiler emits `-Wtrait-snapshot-mutation` pointing at the line and
suggests the `&StructLit{...}` alias form.

---

## Downcasting `*Trait` back to a concrete pointer

Pointer-form trait values (`*Trait`) can be narrowed to a specific
concrete pointer with `as *Concrete`. The cast loads the trait iface's
`data` field and bitcasts it to the destination pointer type:

```rust
let e *errors::Err = some_typed_err()

if e is *FlagError:
  let fe = e as *FlagError       // legal downcast: matches dynamic type
  match (*fe).kind():
    case UnknownFlag(name): ...
    case MissingValue(name): ...
```

The downcast is *unchecked*: the compiler emits a load+bitcast with no
runtime type check. If the dynamic type does not match, the resulting
pointer aliases foreign memory and dereferencing it crashes. Two
guard mechanisms enforce safe usage:

1. **`expr is *Concrete`** - returns `bool` after comparing the iface's
   vtable pointer with the (Concrete, Trait) vtable global. Use this in
   the `if` condition that gates the cast. Same dispatch as Go's
   `iface.(T)` ok-check, no allocation.
2. **`[-Wunguarded-trait-downcast]`** - default-on warning that fires
   when the compiler can't see a same-target `is` check guarding the
   cast in the enclosing control-flow path. The walk is conservative:
   only `is` checks at the root of an `if` condition (or under an
   `&&` chain rooted at the condition) are recognised as guards. To
   silence the warning intentionally, use the matching `is` form or
   add `//!-Wno-unguarded-trait-downcast`.

The compiler also rejects the two **shape mismatches** outright:

| Cast                        | Result                                                                          |
|-----------------------------|----------------------------------------------------------------------------------|
| `*Trait as *Concrete`       | downcast (legal; warns if unguarded)                                            |
| `*Trait as Concrete`        | compile error - pointer-to-value would load the iface as if it were the value   |
| `Trait as *Concrete`        | compile error - value iface has no stable address to hand out as a pointer      |
| `Trait as Concrete`         | falls through to built-in / `coerce[T]` dispatch                                |

The error messages point at concrete fixes - either deref first
(`(*p) as T`) or take an address (`(&t) as *T`).

### Domain error pattern

The pattern of a `*Trait` flowing through generic code paths and
downcasting back to inspect a kind ADT is canonical for typed errors:

```rust
data FlagErrorKind =
  UnknownFlag(name string)
  MissingValue(name string)

struct FlagError(errors::Err) =
  _kind FlagErrorKind

  fn kind(this FlagError) FlagErrorKind = return this._kind

  fn errors::Err::message(this FlagError) string =
    return error_message(this._kind)

fn parse(args [string]) Result[[string], FlagError] = ...

// Caller flow:
match parse(argv):
  case Ok(positional): ...
  case Err(e):
    let widened *errors::Err = &e
    log_error(widened)              // generic logger consumes *errors::Err

    if widened is *FlagError:
      let fe = widened as *FlagError
      match (*fe).kind():
        case UnknownFlag(name):  ...
        case MissingValue(name): ...
```

`Result[T, FlagError]` keeps the typed error available in the `Err`
variant; widening to `*errors::Err` is a one-line cast and gives access
to the polymorphic `message()` / `print()` interface.

---


## Trait method mutation write-back

When a trait method returns the same trait type (e.g. `fn inc(this counter) counter`),
the vtable wrapper automatically **writes the returned struct value back** to the
caller's storage. This ensures mutations are visible after calling through a fat pointer:

```rust
trait counter =
  fn inc(this counter) counter = virtual
  fn get(this counter) i64 = virtual

struct tally(counter) =
  count i64
  fn counter::inc(this tally) tally = return tally{count: this.count + 1}
  fn counter::get(this tally) i64   = return this.count

let t = tally{count: 0}
let ct counter = t
let ct2 = ct.inc()   // returns incremented counter fat ptr
echo ct2.get()       // 1
```

The vtable wrapper:
1. Loads the concrete struct value from the fat pointer's data slot.
2. Calls the concrete method (e.g. `tally_inc`).
3. **Stores the result back** to the data slot so subsequent calls see the update.
4. Constructs a fat pointer from the updated data slot and the static vtable.

---

