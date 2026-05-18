# Generic structs


A struct can be parameterised over one or more type parameters, written in
square brackets:

```rust
struct tuple[t] =
  first  t
  second t

  fn show(this tuple) string =
    return "first: {this.first}, second: {this.second}"
```

Generic structs are **templates**; they are not compiled directly. They
are compiled on demand at each instantiation site - either via a `type`
alias or directly at a struct-literal site.

### Constructor inference

Type arguments on a generic struct literal are optional when they can be
inferred from the provided field values. The compiler unifies each field
value's type against the template's declared field type to bind the
template's type parameters:

```rust
struct Box[t] =
  value t

let a = Box{value: 42 as i32}     // Box[i32] inferred
let b = Box{value: "hello"}       // Box[string] inferred
let c = Box[f64]{value: 3.14}     // explicit still works
```

Multi-parameter structs work the same way, with each parameter bound by
the first literal field that mentions it:

```rust
struct pair[a, b] =
  a a
  b b

let p = pair{a: 1 as i32, b: "hi"}    // pair[i32, string]
```

If a type parameter can't be reached from any provided field the compiler
emits the existing "unknown struct type" error and you should add an
explicit `[T]` annotation. See
[03-functions.md](03-functions.md#type-constraints) for boolean bound
expressions (`&&`, `||`, `not`, parens) that apply equally to structs.

### Method-level where guards

Methods on a generic struct can carry their own `where` clause. The
clause is evaluated at the **instantiation** site against the
concrete type substitution: methods whose guard fails are
**dead-stripped** from the concrete struct, so calling them produces
a clean compile error pointing at the failing constraint instead of
unreachable IR.

This lets one struct hold multiple implementations selected at
compile time:

```rust
type numeric = i64 | u64 | f64 | i32 | u32 | f32 | i16 | u16 | i8 | u8
type primitive = numeric | bool

struct Atomic[t] =
  _ptr *void

  // load/store/make work for any primitive (i*, u*, f*, bool)
  static fn make(v t) Atomic[t] where t is primitive = ...
  fn load(this Atomic[t]) t where t is primitive = ...
  fn store(this Atomic[t], v t) where t is primitive = ...

  // add/cas only on numeric types - bool can be loaded/stored but
  // arithmetic and compare-and-swap don't apply
  fn add(this Atomic[t], d t) t where t is numeric = ...
  fn cas(this Atomic[t], old t, nw t) t where t is numeric = ...
```

Trying to call a stripped method:

```rust
let b = sync::Atomic[bool].new(false)
let _ = b.cas(false, true)
//      ^^^^^
// error: Atomic[bool].cas doesn't match where t is numeric
```

Multiple impls of the same name with different guards form a
**type-dispatched overload set**: the surviving impl after
dead-strip is what gets called. This pattern lets a single struct
have a fast path for primitives and a fallback path for everything
else:

```rust
struct Atomic[t] =
  _ptr *void

  // primitive: lock-free C11 atomics
  static fn make(v t) Atomic[t] where t is primitive = ...

  // anything else: heap-copy under a mutex
  static fn make(v t) Atomic[t] where t is not primitive = ...
```

If neither guard matches, the call fails at compile time with every
candidate's failing constraint listed:

```
error: Box[bool].process doesn't match any of:
       where t is intish, where t is floatish
```

The bound expression is the same `&&` / `||` / `not` algebra used by
function-level constraints (see
[03-functions.md](03-functions.md#type-constraints)). Type-equality
bounds (`t is i64`) match only the literal type - a user struct that
implements `ord` can't accidentally satisfy `where t is i64`.

---

### Generic struct arity overloading

Multiple generic structs can share the same name as long as they have different
numbers of type parameters (different arities):

```rust
struct result[ok] =
  val ok
  ok  bool

struct result[ok, err] =
  val   ok
  err   err
  is_ok bool

type int_result  = result[i64]
type str_or_err  = result[string, string]
```

The compiler selects the correct template based on the number of type arguments
at the instantiation site. This enables "same concept, different shapes" without
name collisions.

--

