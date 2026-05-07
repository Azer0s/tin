# 06 - Traits

A **trait** is a named collection of method signatures and optional default
implementations. Structs declare the traits they implement; the compiler
generates the vtable machinery needed for dynamic dispatch.

---

## Declaring a trait

```rust
trait describable =
  fn describe(this describable) string = virtual
```

- `= virtual` marks a method that **must** be overridden by every
  implementing struct.
- A method with a body (instead of `= virtual`) is a **default method**
  that implementing structs inherit automatically.

---

## Default methods

```rust
trait greetable =
  fn greet(this greetable) string =
    return "Hello, I am {this.name()}"
```

Every struct implementing `greetable` gets this method for free. It can
still be overridden.

---

## Forward fields  -  injecting state into implementing structs

A **forward field** (`fieldName Type forward`) is automatically added to
every struct that implements the trait. Default methods on the trait can
read this field, giving a clean "mixin with state" pattern:

```rust
trait named =
  label string forward                    // injected into implementors
  fn name(this named) string =
    return this.label                     // reads the injected field
```

A struct implementing `named` must supply a value for `label` in its
struct literal:

```rust
struct animal(named) =
  species string

let cat = animal{label: "Whiskers", species: "cat"}
echo cat.name()     // Whiskers
```

This mirrors the spec's `trait size = s size_t forward; fn size() = return s`
pattern.

---

## Implementing traits

List traits in parentheses after the struct name:

```rust
trait named =
  label string forward
  fn name(this named) string = return this.label

trait describable =
  fn describe(this describable) string = virtual

trait greetable =
  fn greet(this greetable) string =
    return "Hello, I am {this.name()}"

struct animal(named, greetable, describable) =
  species string

  fn describable::describe(this animal) string =
    return "{this.label} is a {this.species}"
```

- The struct receives all forward fields (here: `label string`).
- Default methods (`name`, `greet`) are inherited.
- Virtual methods (`describe`) must be provided.

### Qualified impl form

Trait implementations are written in **qualified form** so the binding is
explicit:

| Trait shape                              | Impl method must be              |
|------------------------------------------|----------------------------------|
| Multi-method (`trait T = fn a(...) ...`) | `fn T::a(this Foo, ...) ret`     |
| `as fn` (`trait T as fn(...) ret`)       | `fn ::T(this Foo, ...) ret`      |
| `as static fn`                           | `static fn ::T(...) ret`         |

For a generic trait, type-args are part of the qualifier:

```rust
fn iter[i64]::get(this int_list, i i64) i64 = ...   // explicit args
fn iter::get(this int_list, i i64) i64       = ...  // covers all instantiations
```

Bare-named methods (e.g. `fn get(this int_list, i i64) i64`) are regular
struct methods and **do not** count as a trait impl. This catches typos at
compile time:

> `struct int_list declares iter[i64] but does not implement fn iter::get(this int_list, i i64) i64`

**Carve-out for default-bodied methods.** If a trait method has a default
body (e.g. `init` / `deinit` on `observable`), the bare-name form
`fn methodName(this Foo)` *also* counts as an override, and the trait's
default still runs (chained - see "Trait init/deinit chaining" below).
Virtual methods (no default body) are strict-qualified.

```rust
let cat = animal{label: "Whiskers", species: "cat"}
echo cat.name()        // Whiskers
echo cat.greet()       // Hello, I am Whiskers
echo cat.describe()    // Whiskers is a cat
```

---

## Overriding default methods

A struct can override any default method by simply defining a method with
the same name:

```rust
struct robot(named, greetable, describable) =
  model string

  fn describable::describe(this robot) string =
    return "{this.label} is robot model {this.model}"

  fn greetable::greet(this robot) string =
    return "Beep boop, I am {this.name()}"
```

```rust
let bot = robot{label: "R2D2", model: "astromech"}
echo bot.greet()       // Beep boop, I am R2D2
```

---

## Dynamic dispatch  -  trait-typed parameters

A function that accepts a trait type receives a **fat pointer**
`{data*, vtable*}` and dispatches through it. Any concrete struct that
implements the trait can be passed:

```rust
fn print_name(x named) =
  echo x.name()

fn print_description(x describable) =
  echo x.describe()

let cat = animal{label: "Whiskers", species: "cat"}
let bot = robot{label: "R2D2", model: "astromech"}

print_name(cat)           // Whiskers
print_name(bot)           // R2D2

print_description(cat)    // Whiskers is a cat
print_description(bot)    // R2D2 is robot model astromech
```

The correct implementation is selected at runtime via the vtable.

---

## Generic traits

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

## Function-alias traits

`trait X as fn(params) RetType` declares a trait whose sole requirement is
that the struct supplies one method with the matching signature. The method
must be named after the trait:

```rust
trait display as fn() string
```

A struct implementing `display` must define `fn display(this S) string`:

```rust
struct point(display) =
  x i64
  y i64

  fn ::display(this point) string =
    return "({this.x}, {this.y})"
```

Usage:

```rust
fn show(d display) =
  echo d.display()

let p = point{x: 3, y: 7}
show(p)     // (3, 7)
```

This matches the spec's `trait print as fn() [char]` pattern.

---

## Operator overloading

Built-in operators dispatch through alias traits. To overload an operator
on a struct, list the trait in the struct's implements list and provide
the impl in qualified form. The operator on values of that struct then
lowers to a method call.

### Operator traits

| Operator                | Trait                          | Impl signature                         |
|-------------------------|--------------------------------|----------------------------------------|
| `+`                     | `add[rhs, ret]`                | `fn ::add(this T, other rhs) ret`      |
| `-` (binary)            | `sub[rhs, ret]`                | `fn ::sub(this T, other rhs) ret`      |
| `*`                     | `mul[rhs, ret]`                | `fn ::mul(this T, other rhs) ret`      |
| `/`                     | `div[rhs, ret]`                | `fn ::div(this T, other rhs) ret`      |
| `%`                     | `mod[rhs, ret]`                | `fn ::mod(this T, other rhs) ret`      |
| `++`                    | `concat[rhs, ret]`             | `fn ::concat(this T, other rhs) ret`   |
| `-` (unary)             | `neg[ret]`                     | `fn ::neg(this T) ret`                 |
| `+` (unary)             | `pos[ret]`                     | `fn ::pos(this T) ret`                 |
| `!`                     | `not[ret]`                     | `fn ::not(this T) ret`                 |
| `==` / `!=`             | `comp[rhs]`                    | `fn ::comp(this T, other rhs) bool`    |
| `<` `<=` `>` `>=`       | `ord[rhs]`                     | `fn ::ord(this T, other rhs) i64`      |
| `a[k]`     (rvalue)     | `index[key, ret]`              | `fn ::index(this T, k key) ret`        |
| `a[k] = v`              | `index_set[key, val]`          | `fn ::index_set(this T, k key, v val)` |

`comp` returns `bool`. `ord` returns `i64` and is interpreted strcmp-style:
negative means `lhs < rhs`, zero means equal, positive means greater. The
compiler synthesises `<`, `<=`, `>`, `>=` by comparing the result to `0`,
and `!=` by negating `comp`.

### Example - `Vec3`

```rust
struct Vec3(add[Vec3, Vec3], sub[Vec3, Vec3], neg[Vec3], comp[Vec3]) =
  x f64
  y f64
  z f64

  fn ::add(this Vec3, other Vec3) Vec3 =
    return Vec3{x: this.x + other.x, y: this.y + other.y, z: this.z + other.z}

  fn ::sub(this Vec3, other Vec3) Vec3 =
    return Vec3{x: this.x - other.x, y: this.y - other.y, z: this.z - other.z}

  fn ::neg(this Vec3) Vec3 =
    return Vec3{x: -this.x, y: -this.y, z: -this.z}

  fn ::comp(this Vec3, other Vec3) bool =
    return this.x == other.x && this.y == other.y && this.z == other.z

let a = Vec3{x: 1.0, y: 2.0, z: 3.0}
let b = Vec3{x: 4.0, y: 5.0, z: 6.0}
let c = a + b      // Vec3{5, 7, 9}
let d = -a         // Vec3{-1, -2, -3}
if a == a: ...
```

### Multiple impls per operator

A struct may implement an operator trait at several `[rhs, ret]` pairs.
The compiler picks the variant whose `rhs` matches the actual operand type
exactly:

```rust
struct Vec3(add[Vec3, Vec3], add[f64, Vec3]) =
  x f64
  y f64
  z f64

  fn add[Vec3, Vec3]::add(this Vec3, other Vec3) Vec3 = ...
  fn add[f64,  Vec3]::add(this Vec3, k     f64 ) Vec3 = ...

let v + v    // calls Vec3+Vec3 impl
let v + 2.0  // calls Vec3+f64 impl
```

When several `[rhs, ret]` pairs are implemented, the type-args **must** be
written on the impl (`fn add[Vec3, Vec3]::add ...`); the bare `fn ::add`
form covers a single instantiation.

### Commutative swap (primitive on the left)

For commutative operators (`+`, `*`, `==`, `!=`), a primitive-on-the-left
expression `prim OP struct` is rewritten to `struct.OP(prim)` when the
struct implements the corresponding trait at `[primType, retType]`:

```rust
let r = 2.0 + v   // dispatches to v.add(2.0) via add[f64, Vec3]
```

Non-commutative operators (`-`, `/`, `%`, `<`, `>`, `<=`, `>=`) require an
explicit struct-on-the-left form.

### Compound assignment

`+=`, `-=`, `*=`, `/=`, `%=`, `++=` desugar through their corresponding
operator trait. `a += b` is equivalent to `a = a.add(b)`. The previous value
of `a` is released before the new value is stored, so RC-tracked fields
are not leaked.

### Where-clause shorthand

Single-type-param trait constraints accept a bare reference, expanded to
`Trait[t]`:

```rust
fn min[t](a t, b t) t where t is ord =     // sugar for: where t is ord[t]
  if a < b: return a
  return b

fn eq[t](a t, b t) bool where t is comp =  // sugar for: where t is comp[t]
  return a == b
```

This expansion only fires for traits with exactly one type parameter.
Multi-param traits (`add[rhs, ret]`, `index[k, v]`) require explicit args.

`ord` and `comp` are also satisfied by primitive types via a built-in
shortcut: any integer or floating-point type satisfies `ord`; integers,
floats, `string`, `bool`, and atoms satisfy `comp`. So `min(3, 5)` and
`min(myVec, otherVec)` both type-check.

### Missing impls are a compile error

Applying an operator to a struct that does not implement the corresponding
trait at the relevant `[rhs, ret]` is a compile error - never silently
zero-valued IR or a runtime crash:

```text
binary operator "+" is not defined for operands of type Vec3 and i64
```

This catches both typos in trait names (impl is bare instead of qualified)
and missing variants in multi-impl structs.

### REPL highlighting

In the REPL, an operator token is colored only when the corresponding trait
has been overloaded by some struct in the current session. Plain primitive
arithmetic stays uncolored. The colored set persists across cells (and
across `:reset`, since `:reset` does not wipe declarations from memory).

---

## Conversion traits  -  `implicit[T]` and `coerce[T]`

Two op-traits cover *implicit* conversion in either direction. Listing one
of them in a struct's implements list registers a static method that the
compiler dispatches at the relevant call site automatically.

| Direction      | Trait         | Impl signature                 | Triggered at                  |
|----------------|---------------|--------------------------------|-------------------------------|
| Other -> struct | `implicit[T]` | `static fn ::implicit(t T) S`  | `let x S = t` and arg-passing |
| Struct -> other | `coerce[T]`   | `static fn ::coerce(this S) T` | `s as T`                      |

Both traits sit outside the regular vtable machinery: there is no `Trait`
fat-pointer view of `implicit` or `coerce`. The compiler stores the
registered static fn in a side-table keyed on the (struct, target type)
pair and inlines a direct call when the conversion fires.

### `implicit[T]` - "T flows into S"

`implicit[T]` makes a struct accept a foreign value as if it were already
the struct's type. The user writes `let x S = t` (where `t : T`) and the
compiler quietly inserts a call to the registered `static fn ::implicit(t
T) S`. The same coercion path drives function arguments and return values,
so any place an `S` is expected an `implicit[T]`-registered `T` is also
accepted.

```rust
struct Cents(implicit[i64], implicit[f64]) =
  amount i64

  static fn ::implicit(n i64) Cents = return Cents{amount: n}
  static fn ::implicit(x f64) Cents = return Cents{amount: (x * 100.0) as i64}

let c1 Cents = 250        // implicit[i64]
let c2 Cents = 1.99       // implicit[f64]
fn pay(c Cents) = ...
pay(42)                    // implicit[i64] at the call site
```

A struct may list multiple `implicit[T]` instantiations for distinct source
types - the compiler picks the impl whose param type matches the source.
Two impls with the *same* source type are indistinguishable and would be a
compile error at the `let` site.

The decimal stdlib uses this pattern to make `Value` accept ints, floats,
and strings transparently:

```rust
struct Value(implicit[i32], implicit[i64], implicit[f32], implicit[f64], implicit[string]) =
  coeff i64
  scale i32
  static fn ::implicit(n i64)    Value = ...
  static fn ::implicit(x f64)    Value = ...
  static fn ::implicit(s string) Value = ...
```

### `coerce[T]` - "S flows into T"

`coerce[T]` is the symmetric direction: writing `s as T` (where `s : S`)
calls the registered `static fn ::coerce(this S) T` and substitutes its
return value for the cast. This is the *only* place `as` dispatches to
user code; built-in numeric / pointer casts continue to short-circuit
ahead of trait lookup.

```rust
struct Money(coerce[i64], coerce[f64], coerce[string], coerce[bool]) =
  cents i64

  static fn ::coerce(this Money) i64    = return this.cents
  static fn ::coerce(this Money) f64    = return (this.cents as f64) / 100.0
  static fn ::coerce(this Money) string = return "${this.cents / 100}.{this.cents % 100}"
  static fn ::coerce(this Money) bool   = return this.cents != 0

let m = Money{cents: 1234}
let n = m as i64       // 1234
let d = m as f64       // 12.34
let s = m as string    // "$12.34"
let on = m as bool     // true
```

Because `coerce[T]` impls all share the receiver shape `(this S)`, plain
overload mangling on the parameter signature would collapse them. The
compiler special-cases `static fn ::coerce` and folds the return type
into the overload signature, so `coerce[i64]` and `coerce[string]` survive
side-by-side on the same struct.

`coerce` is value-form only: the receiver is `this S`, not `*S`. To convert
*through* a pointer, deref first - `(*p) as T` finds the registered impl
on `S`. A `static fn ::coerce(this *S) T` shape is reserved for a future
extension (no current call site dispatches to it).

#### Auto `coerce[bool]` in conditionals

When a struct implements `coerce[bool]`, the compiler calls the coerce
method automatically wherever a *condition* is expected:

```rust
struct Bag(coerce[bool]) =
  items [string]
  static fn ::coerce(this Bag) bool = return len(this.items) > 0

let b = Bag{items: ["x"]}

if b: ...           // auto: dispatches to ::coerce(b) bool
for b: ...          // auto
let v = b ? 1 : 0   // auto (ternary)
```

The conditional auto-coercion is intentionally narrow: it fires only
in `if` / `else if` / `for` / `while` / ternary / match-guard sites.
Boolean *operators* (`&&`, `||`, `!`) deliberately do *not* auto-coerce
- a struct may overload them via its own op-traits (e.g. `not[ret]`),
and silently coercing to bool would mask that overload. To use a
bool-coercible struct in a boolean expression, write the cast
explicitly:

```rust
if b as bool && other_cond(): ...   // explicit
if b && other_cond(): ...           // error: cannot use Bag as a boolean operand
```

The rejection error includes the actionable hint to add `as bool` if
the user really wants the coerce dispatch.

### Combining `implicit` and `coerce`

A struct may implement both directions. Pairing them is how a wrapper type
participates in arithmetic with primitives without the user ever writing
an explicit cast:

```rust
struct Percent(implicit[f64], coerce[f64], coerce[string]) =
  ratio f64

  static fn ::implicit(x f64) Percent     = return Percent{ratio: x}
  static fn ::coerce(this Percent) f64    = return this.ratio * 100.0
  static fn ::coerce(this Percent) string = return "{this.ratio * 100.0}%"

let p Percent = 0.25       // implicit[f64]:  0.25 -> Percent{0.25}
let n = p as f64           // coerce[f64]:    Percent{0.25} -> 25.0
let s = p as string        // coerce[string]: "25.000000%"
```

`implicit` and `coerce` are independent registries: `implicit[T]` does not
imply a `coerce[T]` going the other way (or vice versa). List each
direction explicitly.

### Cast safety

Two compile-time checks back the trait conversions:

1. **Impossible casts are errors.** `e as T` where neither a built-in
   conversion nor a `coerce[T]` impl applies fails at the cast site with
   `cannot cast <S> to <T>: no conversion path` rather than silently
   propagating a type mismatch to the next slot.
2. **Mixed pointer/value casts are errors.** `*Trait as Concrete` (the
   pointer indirection thrown away) and `Trait as *Concrete` (a fresh
   address minted for an iface that has no stable storage) both reject at
   the cast site. Use `(*p) as T` to deref first or `*Trait` from the
   start.

### Redundancy warning

When the surrounding slot already pins the destination type, `<lit> as T`
is redundant - the compiler would have auto-coerced the literal anyway.
The `[-Wredundant-type-cast]` warning fires on:

- `let x T = <lit> as T`
- `let x [T; N] = [<lit> as T, ...]` and `[T]` arrays
- `let x (T1, T2) = (<lit1> as T1, <lit2> as T2)`
- `let x Result[T, E] = Ok(<lit> as T)` (and any other ADT variant slot)
- `fn foo() T = return <lit> as T` (and tuple / ADT return shapes)
- `f(<lit> as T)` when `f`'s parameter is `T`
- `Foo{x: <lit> as T}` (named *and* positional struct field initializers)

The check is type-aware: `"hello" as i64` is *not* flagged because string
has no coercion path to `i64` - the cast is impossible (and now errors).
Only literals whose natural kind fits the slot type are flagged.

---

## Combining multiple traits

A struct can implement any number of traits:

```rust
struct int_list(iter[i64], display, labeled) =
  val   i64
  count i64
  // 'tag' is injected by the 'labeled' trait's forward field

  fn iter::len(this int_list) i64           = return this.count
  fn iter::get(this int_list, i i64) i64    = /* ... */
  fn ::display(this int_list) string        = return "int_list({this.val})"
```

Each trait contributes its own vtable. A value of type `int_list` can be
passed to any function expecting `iter[i64]`, `display`, or `labeled`.

---

## Vtable design

Internally, for each `(struct, trait_instantiation)` pair the compiler
generates:

1. **One wrapper function per method**  -  `structName__instKey__methodName(i8* self, ...)`  -  bitcasts `self` to the concrete pointer, loads the struct value, and calls the concrete method.
2. **One vtable global constant**  -  an immutable struct of function pointers, one per trait method.
3. **Fat pointer**  -  `{i8* data, vtable*}`  -  created when a concrete value is coerced to a trait type.

Alias traits and mixin/forward-field traits do not generate vtables. Only traits with virtual or virtual-overridable methods do.

### Method dispatch sequence

When `print_name(cat)` is called, the compiler coerces the concrete `cat`
value to a fat pointer `{data*, vtable*}`. The callee dispatches through the
vtable pointer - one pointer load + one indirect call, the same as C++ virtual
dispatch. See `docs/internals/values.md` for the full LLVM layout details.

---

## Trait coercion: value vs pointer

Tin gives you two ways to hold a struct as a trait, and they have
different semantics. Pick the one you want at the `let` site.

### `Trait` (value form) - copy

```rust
let f Fooable = b
```

The struct `b` is **heap-copied** into a fresh allocation owned by `f`.
`f` and `b` are independent storage from this point on. Mutations
through `f` do not affect `b`. The copy is released when `f` goes out
of scope.

This matches Go's interface assignment. Predictable, safe across coroutine
suspends, no lifetime concerns.

### `*Trait` (pointer form) - borrow

```rust
let a *Fooable = &b
(*a).foo(2)
echo b.v        // mutated
```

`a` is a pointer to a fat pointer whose data field aliases `&b`
directly. Methods called through `*a` operate on `b`'s storage -
mutations propagate. `b` must outlive `a` (same gotcha as any other
`*T` borrow).

### `Trait = &b` - value-form coercion of a borrow

```rust
let b = Box{v: 5}
let f Fooable = &b   // borrow form coerced into a value-form trait
echo f.label()       // dispatches through &b's storage
```

When the right-hand side is `&b` (or any `*T`), the value-form trait
slot stores the borrow's data pointer in its data field directly --
no heap copy, no extra allocation. Read-only methods see the live
state of `b`. Mutations through `f` would still hit `b`'s storage,
but the same value/pointer-receiver rule from the next section
applies: a trait with any `*Self` method rejects this form, push
you to the explicit `*Fooable` borrow.

This form is convenient when an API takes `Fooable` by value but
you want to avoid the heap copy.

### Why both forms? Why does the choice matter?

Because `Trait` always copies, calling a `*Self` method via a value-form
trait would silently mutate the heap copy and the caller would never
see the change. To prevent that footgun, the compiler **rejects** value
coercion when the trait has any pointer-receiver method:

```rust
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual    // pointer receiver

struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n

let b = Box{v: 0}
let f Fooable = b
//  ^^^^^^^^^^^^^
// error: trait Fooable has pointer-receiver methods (foo);
// value coercion would silently mutate a heap copy.
// Use `let a *Fooable = &b` to mutate the original, or rewrite
// the trait's receivers to Fooable if a copy is intended
```

Read-only traits (all methods take `Self`) accept both forms - the
copy semantics are fine because there's nothing to mutate.

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

## Trait `init` / `deinit` chaining

When a struct **and** a trait both define `fn init` (or `fn deinit`), both are
called - the struct's version first, then the trait's version:

```rust
trait observable =
  fn init(this observable)   = global_init_count = global_init_count + 1
  fn deinit(this observable) = global_deinit_count = global_deinit_count + 1

struct widget(observable) =
  id i64
  fn init(this widget)   = setup(this.id)    // called first
  fn deinit(this widget) = teardown(this.id) // called first
```

Creating `widget{id: 1}` calls `widget_init` (struct's init) **and** the
trait's `observable_init` (trait's init). Similarly, when the widget goes out of
scope or is explicitly released, `widget_deinit` is called followed by the
trait's `observable_deinit`.

Chaining is automatic - no extra syntax is required. It mirrors base-class
constructor/destructor chaining in object-oriented languages.
