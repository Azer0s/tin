# 05 - Structs

## Basic structs

```rust
struct person =
  name string
  age  u8
```

Fields are listed indented below the `struct` keyword. Every field has a name
and a type.

### Struct literals

```rust
let pete = person{name: "Pete", age: 20}
```

Fields may be given in any order by name. Positional initialisation is also
supported:

```rust
let pete = person{"Pete", 20}   // positional: name first, age second
```

### Struct destructuring

Destructuring extracts multiple fields into separate variables in a single
`let` statement:

```rust
struct point =
  x i64
  y i64

let p = point{x: 3, y: 4}
let {x, y} point = p
// x == 3, y == 4
```

The type name after `}` is required to tell the compiler the struct's field
layout. Field order in the name list does not matter - each name is looked up
by name, not position.

---

## Methods

Methods are functions defined inside a `struct` block. The first parameter
is conventionally named `this` and has the struct's type:

```rust
struct person =
  name string
  age  u8

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
echo pete.show()   // Pete is 20 years old
```

Methods are called with dot notation: `value.method(args)`.

---

## fn init  -  initializer

If a struct defines `fn init(this S)`, it is called **automatically** every
time a struct literal is evaluated. It is used for setup side-effects (logging,
validation, etc.). `init` receives the newly created struct by value.

```rust
struct person =
  name string
  age  i64

  fn init(this person) =
    echo "new person: {this.name}"

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete  = person{name: "Pete",  age: 20}   // prints: new person: Pete
let alice = person{name: "Alice", age: 30}   // prints: new person: Alice
```

> `init` is **not** called when the struct is created via `malloc`.

---

## fn deinit  -  destructor

If a struct defines `fn deinit(this S)`, it is called **automatically** when a
named variable of that type goes out of scope. It runs before the struct's
ARC-managed fields are released, so fields are still accessible.

```rust
struct resource =
  id i64

  fn init(this resource) =
    echo "init {this.id}"

  fn deinit(this resource) =
    echo "deinit {this.id}"

fn use_it() =
  let r = resource{id: 42}
  echo "using {r.id}"
  // r goes out of scope: deinit 42 printed here

use_it()
// prints:
//   init 42
//   using 42
//   deinit 42
```

**Ownership semantics**: `deinit` is called for the *owner* of a value (a
named `let`/`var` binding), not for function parameters (which are by-value
copies that borrow the struct). This means `deinit` fires exactly once per
struct value.

**Nested structs**: if a struct contains a field of another named struct type,
and that type also defines `deinit`, the inner `deinit` is called automatically
after the outer `deinit` completes.

> `deinit` is **not** called when the struct is boxed to `any` or when it is
> stored as an element of a slice.  See `docs/internals/deinit.md` for details.

---

## Static methods

A static method has no `this` parameter and belongs to the struct as a
namespace. Call it as `StructName.methodName(args)`:

```rust
struct counter =
  value i64

  static fn new() counter =
    return counter{value: 0}

  fn inc(this counter) counter =
    return counter{value: this.value + 1}

  fn get(this counter) i64 =
    return this.value

let c = counter.new()
echo c.get()               // 0

let c2 = c.inc().inc().inc()
echo c2.get()              // 3
```

---

## Pointers to structs

```rust
let pete  = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo (*pPtr).show()        // dereference then call
echo pPtr->show()          // shorthand for (*pPtr).show()
```

`->` is syntactic sugar for dereferencing a pointer and calling a method.

---

## Generic structs

A struct can be parameterised over one or more type parameters, written in
square brackets:

```rust
struct tuple[t] =
  first  t
  second t

  fn show(this tuple) string =
    return "first: {this.first}, second: {this.second}"
```

Generic structs are **templates**; they are not compiled directly. They are
only compiled when instantiated through a `type` alias.

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

---

## Heap allocation

Prefixing a struct literal with `&` allocates it on the heap and returns a
pointer to it:

```rust
struct node =
  value i64
  next  *node

fn make_node(v i64) *node =
  return &node{value: v, next: null}

let n = make_node(42)   // n : *node
echo n.value            // 42
```

The allocated struct is ARC-tracked (the pointer is released when the variable
goes out of scope). This is the idiomatic pattern for factory functions that
return heap-allocated structs.

---

## type aliases

### Simple alias

```rust
type char   = u8
type size_t = u64
```

`char` and `u8` become interchangeable names for the same type.

### Monomorphization alias

`type` can instantiate a generic struct, producing a new concrete type:

```rust
struct tuple[t] =
  first  t
  second t

  fn show(this tuple) string =
    return "first: {this.first}, second: {this.second}"

// Create a concrete "point" type with t = f32
type point = tuple[f32]

let p = point{1.5, 2.5}
echo p.show()              // first: 1.5, second: 2.5
```

`point` is a fully independent concrete struct with fields
`{first f32, second f32}`.  It inherits all methods from the template
with type parameter `t` replaced by `f32`.

Multiple monomorphizations of the same template are independent types:

```rust
type int_pair = tuple[i64]

let nums = int_pair{10, 20}
echo nums.show()           // first: 10, second: 20
```

### override  -  replace inherited methods

`override` lets you replace specific methods from the template:

```rust
type mypoint = tuple[f32] override =
  fn show(this mypoint) string =
    return "({this.first}, {this.second})"

let mp = mypoint{3.0, 4.0}
echo mp.show()             // (3, 4)
```

Only the listed methods are overridden; all others are inherited unchanged.
You can override multiple methods:

```rust
type mypoint = tuple[f32] override =
  fn show(this mypoint) string =
    return "({this.first}, {this.second})"
  fn magnitude(this mypoint) f64 =
    return /* ... */
```

---

## Field tags

Any field in a struct can carry a compile-time metadata tag. Tags are written
as `@"tag_string"` immediately after the field type:

```rust
struct user =
  id    i64    @"primary_key"
  email string @"unique"
  score f64    @"indexed"
  notes string            // no tag
```

Tags do not affect the struct's memory layout or runtime performance.
They are retrieved at runtime with `fieldtag(value, "fieldName")`:

```rust
echo fieldtag(user{}, "id")     // 'primary_key
echo fieldtag(user{}, "email")  // 'unique
echo fieldtag(user{}, "notes")  // '' (empty atom)
```

See [10 - Reflection](10-reflection.md) for the full reflection API.

---

## Traits on structs

Structs can implement traits by listing them in parentheses after the struct
name. See [06 - Traits](06-traits.md) for full details.

```rust
trait named =
  label string forward
  fn name(this named) string = return this.label

struct cat(named) =
  breed string

let c = cat{label: "Whiskers", breed: "tabby"}
echo c.name()    // Whiskers
```

---

## Tuples

A **tuple** is an anonymous struct whose fields are named alphabetically
(`a`, `b`, `c`, ...). Tin provides built-in `Tuple` templates for arities
2 through 10.

### Type syntax

`(T1, T2)` is shorthand for `Tuple[T1, T2]`:

```tin
type int_pair = (i64, i64)
type result   = (i64, bool)
```

Use tuple types anywhere a type annotation is accepted: variable
declarations, function parameters, and return types.

### Literal syntax

Write `(e1, e2, ...)` to create a tuple value:

```tin
let t = (10, true)       // infers Tuple[i64, bool]
let p (i64, i64) = (3, 7)
```

### Function return

A function can declare a tuple return type and return a tuple literal
directly without naming a struct:

```tin
fn swap(x i64, y i64) (i64, i64) =
  return (y, x)

fn min_max(arr [i64]) (i64, i64) =
  // ...
  return (min, max)
```

### Field access

Tuple fields are accessed by name (`a`, `b`, `c`, ...):

```tin
let t = (42, true, 3)
echo t.a    // 42
echo t.b    // true
echo t.c    // 3
```

### Destructuring

`let (name1, name2, ...) = expr` unpacks a tuple into individual variables:

```tin
let (lo, hi) = min_max([5, 2, 9, 1])
echo lo   // 1
echo hi   // 9

let (x, y) = swap(3, 7)
echo x    // 7
echo y    // 3
```

### Tuples vs named structs

Use a tuple when the shape is transient (e.g., returning two values from a
function). Use a named struct when the type is reused, has meaningful field
names, or carries methods.

### Type aliases for tuples

A named type alias creates a distinct type with the same layout:

```tin
type point2d = (i64, i64)
let p point2d = (10, 20)
echo p.a   // 10
echo p.b   // 20
```
