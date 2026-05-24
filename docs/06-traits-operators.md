# Operator overloading

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

