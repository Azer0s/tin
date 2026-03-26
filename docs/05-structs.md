# 05 – Structs

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
layout. Field order in the name list does not matter — each name is looked up
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

See [09 – Reflection](09-reflection.md) for the full reflection API.

---

## Traits on structs

Structs can implement traits by listing them in parentheses after the struct
name. See [06 – Traits](06-traits.md) for full details.

```rust
trait named =
  label string forward
  fn name(this named) string = return this.label

struct cat(named) =
  breed string

let c = cat{label: "Whiskers", breed: "tabby"}
echo c.name()    // Whiskers
```
