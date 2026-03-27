# 09 - Reflection & Runtime Type Information

Tin includes a set of built-in reflection operators that let programs inspect
types, traits, field names, and field values at runtime without any virtual
method overhead.

---

## Atoms

An **atom** is a compile-time symbolic constant. Atoms compare by identity
(interned at compile time) and are the return type of `typeof`, `traitof`,
`fieldnames`, and `fieldtypes`.

### Simple atoms

Simple atoms are written with a leading `'` followed by a name that contains
only letters, digits, and underscores:

```rust
'ok
'err
'sunny
'my_type_1
```

These are the atoms used in enum declarations, `where` pattern matching, and
returned by `typeof`:

```rust
enum atom status =
  'ok,
  'err,

fn check(s atom) =
  where 'ok:  echo "all good"
  where 'err: echo "error"
  where _:    echo "unknown"

let t = typeof(42)     // 'i64
let t2 = typeof(true)  // 'bool
```

### Complex (quoted) atoms

When a type specification includes characters that are not allowed in a simple
atom name (parentheses, brackets, `*`, `,`) a **quoted atom** is used. The
syntax is `'"..."` with the type string inside double quotes:

```rust
'"fn(i64)bool"
'"fn(i64,f64)bool"
'"*bool"
'"[string]"
'"fn(fn(i64)bool,i64)string"
```

Quoted atoms are how the reflect API represents pointer types, array types, and
function types. They are also produced by `typeof` for such values:

```rust
let arr = [1, 2, 3]
echo typeof(arr)         // '[i64]   (simple form when unambiguous)

let p = 42
let ptr = &p
echo typeof(ptr)         // '*i64
```

When working with the `reflect` package, pass quoted atoms directly to
interrogate type structure:

```rust
use reflect

echo reflect::is_fn('"fn(i64)bool")      // 1
echo reflect::fn_ret('"fn(i64,f64)bool") // 'bool
echo reflect::elem('"*bool")             // 'bool
echo reflect::elem('"[string]")          // 'string
```

Both simple and quoted atoms have type `atom` and work identically with
`==`, `where` guards, and reflection functions.

---

## The `any` type

`any` is a **dynamically-typed container** that holds a value of any tin type
together with its runtime type identity. Assigning a concrete value to `any`
is called **boxing**:

```rust
let x i64  = 42
let a any  = x        // box: stores (type='i64', data->42)

let p = point{x: 3, y: 4}
let ap any = p        // box: stores (type='point', data->copy of p)
```

The stored type identity is exact  -  boxing a `rect` stores type `'rect`, not
any base trait or generic name.

### any and extern functions

Values returned from extern C functions are boxed with the correct Tin type:

```rust
fn c_abs(x i64) i64 = extern("labs")
fn c_sqrt(x f64) f64 = extern("sqrt")

let r = c_abs(-42)
let a any = r
echo typeof(a)    // 'i64

let s any = c_sqrt(144.0)
echo typeof(s)    // 'f64
```

### any and function pointers

If a value goes through a function pointer or higher-order function, the type
is still preserved:

```rust
fn apply(f fn(i64) i64, x i64) i64 = return f(x)

let r = apply(c_abs, -77)
let a any = r
echo typeof(a)    // 'i64

fn make_rect(w i64, h i64) rect = return rect{w: w, h: h}

fn box_rect(f fn(i64, i64) rect, w i64, h i64) any =
  return f(w, h)

let ar = box_rect(make_rect, 5, 10)
echo typeof(ar)   // 'rect
```

---

## typeof

`typeof(expr)` returns the runtime type of a value as an atom:

```rust
let p = point{x: 1, y: 2}

echo typeof(p)      // 'point
echo typeof(42)     // 'i64
echo typeof(3.14)   // 'f64
echo typeof(true)   // 'bool
echo typeof("hi")   // 'string

let ptr = &p
echo typeof(ptr)    // '*point

let arr = [p, p]
echo typeof(arr)    // '[point]
```

When called on an `any` value, `typeof` inspects the stored type at runtime:

```rust
let a any = point{x: 0, y: 0}
echo typeof(a)      // 'point  -  not 'any!

let b any = rect{w: 5, h: 3}
echo typeof(b)      // 'rect
```

The return value is an atom (type `atom`), so it can be matched with `where`
or compared with `==`.

---

## traitof

`traitof(expr)` returns the list of traits implemented by the value's runtime
type, as a `[atom]` array:

```rust
trait drawable = fn draw(this drawable) string = virtual
trait resizable = fn resize(this resizable, factor i64) i64 = virtual

struct rect(drawable, resizable) =
  w i64
  h i64
  fn draw(this rect) string = return "rect"
  fn resize(this rect, factor i64) i64 = return this.w * factor

let r = rect{w: 10, h: 20}

let traits = traitof(r)
echo traits.len     // 2
echo traits[0]      // 'drawable
echo traits[1]      // 'resizable
```

Works on `any` values too:

```rust
let a any = rect{w: 10, h: 20}
let ts = traitof(a)
echo ts.len         // 2
```

Structs that implement no traits return an empty array.

---

## fieldnames

`fieldnames(expr)` returns the names of a struct's user-visible fields as
a `[atom]` array:

```rust
struct point =
  x i64
  y i64

let p = point{x: 3, y: 4}
let names = fieldnames(p)

echo names.len      // 2
echo names[0]       // 'x
echo names[1]       // 'y
```

Works on `any`:

```rust
let a any = p
let an = fieldnames(a)

echo an.len         // 2
echo an[0]          // 'x
```

Internal implementation fields (the leading `i32` type-ID and vtable
pointers) are not included  -  only the fields visible in the source code.

---

## fieldtypes

`fieldtypes(expr)` returns the type names of each user field as a `[atom]`
array, in the same order as `fieldnames`:

```rust
struct person =
  name string
  age  i64

let p = person{name: "Alice", age: 30}
let types = fieldtypes(p)

echo types[0]       // 'string
echo types[1]       // 'i64
```

Pointer and array field types include nested type information:

```rust
struct node =
  val  i64
  next *node

let types = fieldtypes(node{val: 0, next: null})
echo types[0]       // 'i64
echo types[1]       // '*node
```

---

## fieldtag

`fieldtag(expr, "fieldName")` returns the tag annotation attached to a
field declaration, as an `atom`. Fields are tagged with `@"tag"` in the
struct body:

```rust
struct user =
  id   i64  @"primary_key"
  name string @"required"
  bio  string

let u = user{id: 1, name: "Alice", bio: ""}

echo fieldtag(u, "id")    // 'primary_key
echo fieldtag(u, "name")  // 'required
echo fieldtag(u, "bio")   // '' (empty atom  -  no tag)
```

Field tags are stored in a compile-time global map and require no runtime
overhead beyond the atom lookup.

---

## getfield

`getfield(expr, "fieldName")` reads the value of a named field, returning
it as `any`:

```rust
struct point =
  x i64
  y i64

let p = point{x: 10, y: 20}

let vx = getfield(p, "x")    // vx is any
echo vx                        // 10

let vy = getfield(p, "y")
echo vy                        // 20
```

On a concrete struct, `getfield` is lowered to a direct GEP + load at
compile time (no dispatch table). On an `any` value, runtime dispatch
selects the correct struct layout:

```rust
let a any = p

let gx = getfield(a, "x")    // runtime dispatch via type_id
echo gx                        // 10
```

This makes it possible to write generic inspect/serialise functions that
work on any struct type.

### Unboxing `any` with `as`

`getfield` returns `any`. To use the result as a concrete scalar type, unbox
it with `as`:

```rust
let p = point{x: 10, y: 20}

let vx = getfield(p, "x") as i64    // unbox to i64
let vy = getfield(p, "y") as i64

echo vx    // 10
echo vy    // 20
```

This works for all primitive scalar types: `i64`, `f64`, `bool`, etc. It is
particularly useful when passing the result directly to a function that
expects a concrete type:

```rust
assert::equals(getfield(r, "w") as i64, 8)
assert::equals(getfield(r, "h") as i64, 16)
```

The `as` cast also works on values returned by `any`-typed extern or
higher-order functions:

```rust
fn c_sqrt(x f64) f64 = extern("sqrt")

let result any = c_sqrt(144.0)
echo result as f64    // 12.0
```

---

## setfield

`setfield(expr, "fieldName", value)` writes `value` into the named field.
The expression must be an lvalue (a variable, not a temporary):

```rust
struct point =
  x i64
  y i64

let p = point{x: 10, y: 20}

setfield(p, "x", 99)
echo p.x    // 99
```

`setfield` is particularly useful when the field name is only known at
runtime (for example, when reading from a config map):

```rust
let fields = ["x", "y"]
let vals   = [1, 2]

for let i i64 in 0..fields.len:
  setfield(p, fields[i], vals[i])

echo p.x    // 1
echo p.y    // 2
```

---

## Field tags  -  `@"tag"` syntax

Any field in a struct can be annotated with a string tag immediately after
its type:

```rust
struct db_row =
  id    i64    @"pk"
  email string @"unique"
  score f64    @"indexed"
  notes string            // untagged
```

Tags are purely metadata: they are stored in a compile-time table and have
no runtime representation in the struct layout. Retrieve them with
`fieldtag(expr, "name")`.

---

## Full reflection example

```rust
use io

trait labeled =
  fn label(this labeled) string = virtual

struct shape(labeled) =
  name   string @"display_name"
  width  i64
  height i64

  fn label(this shape) string = return this.name

let s = shape{name: "box", width: 10, height: 5}

// Type and traits
io::printf("type   = %s\n", typeof(s))
io::printf("traits = %lld\n", traitof(s).len)

// Fields
let fns = fieldnames(s)
let fts = fieldtypes(s)
for let i i64 in 0..fns.len:
  io::printf("  %s : %s  tag=%s\n", fns[i], fts[i], fieldtag(s, fns[i]))

// Dynamic read / write
let w = getfield(s, "width")
echo w               // 10

setfield(s, "width", 20)
echo s.width         // 20

// Through any
let a any = s
io::printf("typeof(any) = %s\n", typeof(a))
echo getfield(a, "height")    // 5
```

---

## Implementation notes

### any representation

`any` is a two-field struct `{ i32 type_id, i8* data }`. The `type_id`
identifies the contained type and the `data` pointer points to a
heap-allocated copy of the value.

Type IDs are assigned at compile time:

| ID | Type |
|---|---|
| 0 | `i64` (all integer types are widened to i64 when boxed) |
| 1 | `f64` (all float types are widened to f64 when boxed) |
| 2 | `string` |
| 3 | `bool` |
| 4 | `*T` pointer |
| 5+ | User-defined structs and `data` types, in declaration order |

### Heap allocation for escape safety

When a value is boxed into `any`, a copy is placed on the heap via
`malloc`. This ensures the `any` value can safely escape the scope where
it was created (e.g., be returned from a function or stored in a
collection) without the data pointer becoming a dangling reference.

### Struct memory layout

Every struct has a hidden leading `i32` field at LLVM layout index 0 that
stores the struct's compile-time type ID. Vtable pointers (one per
implemented trait) occupy the next positions, followed by user-visible
fields. Given:

```rust
struct rect(drawable, resizable) =
  w i64
  h i64
```

The LLVM layout is:
```
%rect = type { i32, %drawable_vtable*, %resizable_vtable*, i64, i64 }
       field:   0          1                  2             3    4
```

`fieldnames(rect)` returns `['w, 'h]`  -  only indices 3 and 4.
`vtableOffset("rect")` = 2, so user field index `k` lives at LLVM index
`1 + 2 + k`.

### Struct size and ABI alignment

When boxing a struct to `any`, the compiler computes the allocation size
using `llvmTypeSizeAlign`, which accounts for ABI padding between fields
(e.g., the 4-byte pad between an `i32` and the next `i64`). This is
important: under `-O2`, LLVM uses the malloc size for pointer-provenance
alias analysis, so an undersized allocation corrupts field reads.

### Runtime dispatch for `any`

`typeof`, `traitof`, `fieldnames`, `fieldtypes`, and `getfield` on an `any`
value use a compile-time-generated linear `select` chain keyed on
`type_id`. For each struct registered in the module, the compiler emits:

```
result = select(type_id == struct_id && field_match, boxed_field, result)
```

All comparisons are emitted in a single basic block  -  no branching, no
function calls (apart from `strcmp` for field-name matching in `getfield`).

### Function pointer coercion

When a named or extern function is passed to a higher-order function that
expects a fat function pointer `{ fn(i8* env, params...)*, i8* }`, the
compiler generates a **shim** wrapper:

- The shim has the fat-pointer calling convention (first arg is `i8* env`,
  then the Tin-typed parameters).
- Inside the shim, arguments are coerced to the original function's C-level
  types (e.g., fat-string `{i8*,i64}` -> raw `i8*`).
- The original function is called, and the return value is wrapped back to
  Tin conventions via `wrapFromExtern`.
- The fat pointer is `{ shim_ptr, null }` (no environment needed for
  non-capturing references to named functions).

This is why `typeof` and `getfield` work correctly even on values returned
by extern functions passed through higher-order functions.
