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


---

## See also

Subpages branched off as this doc grew. The numbered ordering
still works as a reading order:

- [Generic traits](06-traits-generics.md) - type-parameterised
  trait declarations, call-site generics, wildcard slots.
- [Function-alias traits](06-traits-aliases.md) - traits
  declared as `as fn(...) T` shorthand.
- [Operator overloading](06-traits-operators.md) - `add`,
  `mul`, comparison, indexing, compound assign.
- [Conversion traits](06-traits-conversion.md) -
  `implicit[T]` and `coerce[T]`.
- [Trait coercion: value vs pointer](06-traits-coercion.md) -
  snapshot vs alias semantics, downcasting, mutation
  write-back.

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
