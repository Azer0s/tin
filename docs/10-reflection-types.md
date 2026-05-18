# Atoms, the any type, typeof, traitof


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
  'ok
  'err

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
echo typeof(arr)         // '[i64]

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

The stored type identity is exact - boxing a `rect` stores type `'rect`, not
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
echo typeof(a)      // 'point  - not 'any!

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
trait drawable  = fn draw(this drawable) string = virtual
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

