# 06 – Traits

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

## Forward fields — injecting state into implementing structs

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

  fn describe(this animal) string =
    return "{this.label} is a {this.species}"
```

- The struct receives all forward fields (here: `label string`).
- Default methods (`name`, `greet`) are inherited.
- Virtual methods (`describe`) must be provided.

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

  fn describe(this robot) string =
    return "{this.label} is robot model {this.model}"

  fn greet(this robot) string =
    return "Beep boop, I am {this.name()}"
```

```rust
let bot = robot{label: "R2D2", model: "astromech"}
echo bot.greet()       // Beep boop, I am R2D2
```

---

## Dynamic dispatch — trait-typed parameters

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

  fn len(this int_list) i64 = return this.count

  fn get(this int_list, i i64) i64 =
    if i == 0: return this.val
    return 0

struct pair(iter[i64]) =
  a i64
  b i64

  fn len(this pair) i64 = return 2

  fn get(this pair, i i64) i64 =
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

  fn display(this point) string =
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

## Combining multiple traits

A struct can implement any number of traits:

```rust
struct int_list(iter[i64], display, labeled) =
  val   i64
  count i64
  // 'tag' is injected by the 'labeled' trait's forward field

  fn len(this int_list) i64 = return this.count
  fn get(this int_list, i i64) i64 = /* ... */
  fn display(this int_list) string = return "int_list({this.val})"
```

Each trait contributes its own vtable. A value of type `int_list` can be
passed to any function expecting `iter[i64]`, `display`, or `labeled`.

---

## Vtable design

Internally, for each `(struct, trait_instantiation)` pair the compiler
generates:

1. **One wrapper function per method** — `structName__instKey__methodName(i8* self, ...)` — bitcasts `self` to the concrete pointer, loads the struct value, and calls the concrete method.
2. **One vtable global constant** — an immutable struct of function pointers, one per trait method.
3. **Fat pointer** — `{i8* data, vtable*}` — created when a concrete value is coerced to a trait type.

Alias traits and mixin/forward-field traits do not generate vtables. Only traits with virtual or virtual-overridable methods do.
