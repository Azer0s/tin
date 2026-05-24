# fieldnames, fieldtypes, fieldtag, getfield, setfield


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

Internal implementation fields (the hidden type-ID and vtable pointers)
are not included - only the fields visible in source code.

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
  next own *node

let types = fieldtypes(node{val: 0, next: nil})
echo types[0]       // 'i64
echo types[1]       // '*node
```

---

## fieldtag

`fieldtag(expr, "fieldName")` returns the tag annotation attached to a
field declaration, as an `atom`. Fields are tagged with `@"tag"` in the
struct body (see [05 - Structs](05-structs.md) for tag declaration syntax):

```rust
struct user =
  id   i64    @"primary_key"
  name string @"required"
  bio  string

let u = user{id: 1, name: "Alice", bio: ""}

echo fieldtag(u, "id")    // 'primary_key
echo fieldtag(u, "name")  // 'required
echo fieldtag(u, "bio")   // '   (empty atom - no tag)
```

Field tags are stored in a compile-time global map with no runtime overhead
beyond the atom lookup.

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
```

On a concrete struct, `getfield` is lowered to a direct GEP + load at compile
time. On an `any` value, runtime dispatch selects the correct struct layout:

```rust
let a any = p

let gx = getfield(a, "x")    // runtime dispatch via type_id
echo gx                        // 10
```

### Unboxing `any` with `as`

`getfield` returns `any`. To use the result as a concrete type, unbox it
with `as`:

```rust
let vx = getfield(p, "x") as i64
let vy = getfield(p, "y") as i64

echo vx    // 10
echo vy    // 20
```

This works for all scalar types: `i64`, `f64`, `bool`, etc. The `as` cast
also works on values returned by `any`-typed extern or higher-order functions:

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
let p = point{x: 10, y: 20}

setfield(p, "x", 99)
echo p.x    // 99
```

`setfield` is useful when the field name is only known at runtime:

```rust
let fields = ["x", "y"]
let vals   = [1, 2]

for let i i64 in 0..fields.len:
  setfield(p, fields[i], vals[i])

echo p.x    // 1
echo p.y    // 2
```

---

