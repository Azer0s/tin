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

--

## Field mutability - `const` / `var`

A field declaration may be prefixed with `const` or `var` to control whether
it can be reassigned after construction:

```rust
struct point =
  const x i64          // immutable after init
  const y i64          // immutable after init
  var   scratch i64    // explicit mutable
  z i64                // unmarked - mutable (default)
```

Rules:

- Fields default to mutable. Writing `var` is always allowed but redundant
  unless the struct is tagged `#const@field` (see
  [13 - Control tags](13-control-tags.md#const_field)).
- Writing `const` rejects direct field writes at compile time. The canonical
  forms caught are:

  ```rust
  p.x = 5            // compile error
  p.x += 1           // compile error
  p.x++              // compile error
  pp->x = 5          // compile error (p pointer-dereference shorthand)
  this.x = 5         // compile error inside a method body
  ```

- Construction paths are unaffected. Struct literals, positional init,
  destructuring `let`, and match-arm bindings all set const fields normally.
- Replacing the whole struct (`p = point{...}`) is a variable assignment,
  not a field write, so it works regardless of per-field constness. The
  binding's own mutability (`let` vs `var`) controls whether the replace
  itself is allowed.

`const` is a **compile-time tag**, not a runtime guarantee:

- `setfield(p, "x", 99)` still compiles and writes the field at runtime.
  This is the explicit reflective escape hatch.
- `&p.x` returns a usable pointer; writes through the pointer bypass
  static tracking by design.

Combine `const` / `var` with the existing `weak` / `own` ownership
modifiers. Ordering is `[const|var] <name> [weak|own] <type> [forward]
[@"metadata"]`:

```rust
struct tree =
  const value i64
  const left  own *tree    // const + own
  var   cache weak *tree   // var + weak
```

--

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

--

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

--

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

--

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

--

## Pointers to structs

```rust
let pete  = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo (*pPtr).show()        // dereference then call
echo pPtr->show()          // shorthand for (*pPtr).show()
```

`->` is syntactic sugar for dereferencing a pointer and calling a method.

--

## Operator overloading

Built-in operators (`+ - * / % == < > [] ++ ! +=` etc.) on a struct dispatch
through alias traits (`add`, `sub`, `mul`, `comp`, `ord`, `index`, ...). To
overload an operator, list the trait in the implements list and provide the
impl in qualified form:

```rust
struct Vec3(add[Vec3, Vec3]) =
  x f64
  y f64
  z f64

  fn ::add(this Vec3, other Vec3) Vec3 =
    return Vec3{x: this.x + other.x, y: this.y + other.y, z: this.z + other.z}
```

See `docs/06-traits.md` (Operator overloading) for the full trait table,
multi-impl resolution, commutative swap, where-clause shorthand, and
compound-assignment semantics.

--


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

--

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

--

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

--


---

## See also

- [Generic structs](05-structs-generics.md)
- [Heap allocation, weak / own fields](05-structs-heap.md)
- [Tuples](05-structs-tuples.md)
