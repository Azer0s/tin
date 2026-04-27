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
default still runs (chained — see "Trait init/deinit chaining" below).
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

## Trait fat pointers (`*TraitName`)

A trait value can be held as a pointer using the `*TraitName` type. Dereferencing
or calling methods through a `*TraitName` pointer works via the `->` operator,
which loads the fat pointer and dispatches through the vtable:

```rust
trait greeter =
  fn greet(this greeter) string = virtual

struct person(greeter) =
  name string
  fn greet(this person) string = return "Hello, I am {this.name}"

let p = person{name: "Alice"}
let gp *greeter = &p          // pointer to fat pointer
echo gp->greet()              // "Hello, I am Alice"
```

The `->` operator automatically dereferences `*TraitName` to get the fat pointer,
then dispatches via the vtable.

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
