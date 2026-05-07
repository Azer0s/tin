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

## Heap allocation

Prefixing a struct literal with `&` allocates it on the heap and returns a
pointer to it:

```rust
struct node =
  value i64
  next  *node

fn make_node(v i64) *node =
  return &node{value: v, next: nil}

let n = make_node(42)   // n : *node
echo n.value            // 42
```

The allocated struct is ARC-tracked - the pointer is released automatically
when the variable goes out of scope. No `mem::free` is needed. The same applies
when returning the address of a named local (`return &x`): the compiler promotes
the variable to an ARC block at the return site and the caller's variable
releases it on scope exit.

--

## Weak and own fields

Tin uses ARC (automatic reference counting) for memory management. Two structs
that each hold a strong pointer to the other form a reference cycle: neither
can ever reach RC == 0, so both leak. The compiler detects cycles in the struct
field graph at compile time using Tarjan's SCC algorithm and enforces that
every cycle is annotated with the programmer's intent.

There are two field ownership modifiers:

| Modifier | RC behaviour | Cycle role |
|-----|-------|------|
| *(none)* - plain strong | retain on assign, release on free | Default owning reference |
| `weak` | no retain / no release | Non-owning back-reference; breaks ARC cycles |
| `own` | retain on assign, release on free (same as strong) | Owning tree-edge; declares the referenced data is acyclic at runtime |

--

### `weak` - non-owning back-references

Use `weak` when a field is a back-reference that must not keep the target
alive. The classic example is a doubly-linked list:

```rust
struct Node[T] =
  val  T
  next *Node[T]        // strong: owns the forward chain
  prev weak *Node[T]   // weak:   back-reference, does not own
```

`weak` goes directly before the field type. A weak field:

- Is **not** retained when assigned.
- Is **not** released when the struct is freed.
- The programmer is responsible for not letting the weak reference outlive
  the owning side.

--

### `own` - tree-ownership declaration

Use `own` when a struct contains children of the same type and the runtime
data is guaranteed to form a tree (no cycles). A common example is a parsed
AST or a JSON/YAML value tree:

```rust
struct Expr =
  kind  ExprKind
  left  own *Expr    // owns left child - promise: no cycles at runtime
  right own *Expr    // owns right child
```

`own` is semantically identical to a plain strong field at runtime: the field
is retained on assign and released on free. The only difference is at compile
time: the cycle checker accepts a cycle that contains at least one `own` edge
without requiring a corresponding `weak` edge.

**The programmer declares a contract:** fields marked `own` will never form a
runtime cycle. The compiler does not verify this - doing so would require
either a full ownership type system or an O(depth) walk on every assignment.
Violating the contract (e.g. `node.left own= node`) produces a memory leak,
exactly as manually constructing a strong-reference cycle would in any ARC
language.

> `own` is the programmer saying "I am not a morron."
>
> Future work: a debug-mode build option will add a runtime acyclicity check
> on `own` field assignments to catch contract violations during development.

--

### Compiler cycle detection rules

Every strongly-connected component that contains a cycle must satisfy:

| Cycle composition | Result |
|--|--|
| All plain strong, no `weak`, no `own` | **Error** - annotate intent |
| All `weak`, no strong, no `own` | **Error** - no owner; objects would be freed immediately |
| At least one `weak` (any number of strong) | **OK** - classic ARC cycle-breaking |
| At least one `own` (no `weak` needed) | **OK** - programmer declares acyclic |

```rust
// ERROR: mutual plain strong references - nobody breaks the cycle
struct Parent =
  child *Child

struct Child =
  parent *Parent

// OK: one strong owner, one weak back-reference
struct Parent =
  child *Child

struct Child =
  parent weak *Parent

// OK: self-referential tree type - own declares acyclic data
struct JsonValue =
  items own [*JsonValue]   // owns child values; data is always a tree
```

--

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

## Tuples

A **tuple** is an anonymous struct whose fields are named alphabetically
(`a`, `b`, `c`, ...). Tin provides built-in `Tuple` templates for arities
2 through 10.

### Type syntax

`(T1, T2)` is shorthand for `Tuple[T1, T2]`:

```rust
type int_pair = (i64, i64)
type result   = (i64, bool)
```

Use tuple types anywhere a type annotation is accepted: variable
declarations, function parameters, and return types.

### Literal syntax

Write `(e1, e2, ...)` to create a tuple value:

```rust
let t = (10, true)       // infers Tuple[i64, bool]
let p (i64, i64) = (3, 7)
```

### Function return

A function can declare a tuple return type and return a tuple literal
directly without naming a struct:

```rust
fn swap(x i64, y i64) (i64, i64) =
  return (y, x)

fn min_max(arr [i64]) (i64, i64) =
  // ...
  return (min, max)
```

### Field access

Tuple fields are accessed by name (`a`, `b`, `c`, ...):

```rust
let t = (42, true, 3)
echo t.a    // 42
echo t.b    // true
echo t.c    // 3
```

### Destructuring

`let (name1, name2, ...) = expr` unpacks a tuple into individual variables:

```rust
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

```rust
type point2d = (i64, i64)
let p point2d = (10, 20)
echo p.a   // 10
echo p.b   // 20
```
