# Generic traits


A trait can have a type parameter. Each instantiation gets its own vtable:

```rust
trait iter[t] =
  fn len(this iter[t]) i64 = virtual
  fn get(this iter[t], i i64) t = virtual
```

Structs declare which instantiation they implement:

```rust
struct int_list(iter[i64]) =
  val   i64
  count i64

  fn iter::len(this int_list) i64 = return this.count

  fn iter::get(this int_list, i i64) i64 =
    if i == 0: return this.val
    return 0

struct pair(iter[i64]) =
  a i64
  b i64

  fn iter::len(this pair) i64 = return 2

  fn iter::get(this pair, i i64) i64 =
    if i == 0: return this.a
    return this.b
```

Functions can then accept any `iter[i64]` without knowing the concrete type:

```rust
fn first(it iter[i64]) i64 = return it.get(0)
fn last(it iter[i64])  i64 = return it.get(it.len() - 1)

fn sum_ends(it iter[i64]) i64 =
  return it.get(0) + it.get(it.len() - 1)

let item = int_list{val: 42, count: 1}
let p    = pair{a: 10, b: 20}

echo first(item)      // 42
echo sum_ends(p)      // 30
```

Different type argument instantiations are independent. `iter[i64]` and
`iter[f32]` produce separate vtable types.

---

## Call-site generics  -  wildcard slots in trait bounds

A trait bound or method return type may contain `_` (wildcard). The
slot is filled per call site by context, and the compiler emits one
monomorphization per unique fill - in addition to the data type's
own generic monomorphizations:

```rust
data Result[T, E](tryable[T, Result[_, E]]) =
  Ok(v T)
  Err(msg E)

  fn err_value(this Result[T, E]) Result[_, E] =
    match this:
      case Ok(_):  panic("err_value on Ok")
      case Err(_): return this
```

The `_` in `Result[_, E]` says "this slot is impl-determined; let
each call pick." When `err_value` is called from four functions that
fix the wildcard differently:

```rust
fn a() Result[i64,    MyErr] = return try r
fn b() Result[string, MyErr] = return try r
fn c() Result[any,    MyErr] = return try r
fn d() Result[bool,   MyErr] = return try r
```

the compiler emits four monomorphizations - one per distinct fill -
and each call dispatches directly to the correct one. There is no
runtime tag-walk; resolution happens at compile time via the same
mechanism Tin uses for regular generic functions.

Without the wildcard, the impl commits to its concrete container
shape and cross-T calls fail with a regular type-mismatch error:

```rust
data Result[T, E](tryable[T, Result[T, E]]) =
  fn err_value(this Result[T, E]) Result[T, E] = this  // no `_`

let r Result[i64, MyErr]    = ...
let s Result[string, MyErr] = r.err_value()  // compile error
```

The wildcard is the explicit opt-in saying "this slot is
fillable per call."

The compiler picks the wildcard's substitution from whichever of
these is unambiguous at the call site:

- An explicit type annotation:
  `let r Result[string, E] = adt.err_value()`.
- The enclosing function's declared return type, when the call's
  result is being returned: `return adt.err_value()`.
- The expected argument type at a call: passing the result into a
  function whose parameter type fixes the slot.

If multiple contexts apply they must agree; if none fixes the slot
the call is a compile error. Tin does not guess across wildcards.

Wildcards may appear only in trait-bound positions (impl headers,
generic where clauses, the return type of a method whose enclosing
trait bound declares the slot). They are not value-level types:
`let x _ = ...` and `fn f(x _) ...` are syntax errors.

The internals doc at `docs/internals/call-site-generics.md`
covers how the monomorphization is keyed and when the
variant-aware reconstruction codegen runs.

---

