# Algebraic data types


A `data` declaration defines a sum type whose variants are named
constructors, each carrying zero or more positional or named fields. This is
the right tool when two outcomes share the same underlying type but
different meanings (e.g. `Ok(i32)` vs `Err(i32)`) - something a tagged union
cannot express.

```rust
data Option[t] =
  None
  Some(v t)

data Result[t, e] =
  Ok(v t)
  Err(msg e)

data Shape =
  Dot
  Circle(radius f64)
  Rect(width f64, height f64)
```

### Requirements

- At least one variant must carry a payload. Pure-nullary sums belong in
  `enum`.
- `data` is a *contextual* keyword: it only introduces a declaration when it
  appears where a top-level decl is expected. Anywhere else (field name,
  variable name) it parses as a plain identifier.

### Construction

```rust
let a Option[i64] = Option[i64]::Some(42)   // fully qualified
let b Option[i64] = Some(42)                // inferred from let annotation
let r Result[i32, string] = Ok(42)          // inferred from let annotation

fn unwrap(r Result[i32, string], d i32) i32 = ...

unwrap(Ok(42), 0)                           // inferred from argument position
```

Inference rule for bare `Ctor(...)`:

1. If an expected type is known at the call site (let-binding annotation,
   function argument, function return) AND the expected type is a known
   ADT whose variants include `Ctor`, use it.
2. Otherwise, if exactly one ADT in scope declares `Ctor`, use it.
3. Otherwise, the compiler asks you to qualify (`Adt[...]::Ctor(...)`).

### Pattern matching

```rust
match r:
  case Ok(v):   echo "ok: {v}"
  case Err(m):  echo "error: {m}"

// where-patterns reuse the same constructor syntax;
// where-clauses always require a `where _` fallback even when every
// variant is handled
fn unwrap_or(r Result[i32, string], d i32) i32 =
  where r is Ok(v):  return v
  where r is Err(_): return d
  where _:           return d
```

Match arms exhaustively covering every variant are recognised without a
`default`. Match-arm bindings of RC-tracked fields (string, array, closure,
any) are retained on entry and released on exit, so `return v` transfers
ownership cleanly.

`where x is Ctor(b, ...)` bindings are *borrows*: they avoid the retain so
they cannot be returned as-is. Prefer `match` when you need to hand an
owning field back to the caller.

### Recursive ADTs

Recursive variants use `own *Self` (or `weak *Self` for back-edges):

```rust
data Tree[t] =
  Leaf
  Node(v t, left own *Tree[t], right own *Tree[t])

data DList[t] =
  DNil
  DCons(v t, next own *DList[t], prev weak *DList[t])
```

ARC rules are identical to struct fields: owning pointers are released when
the ADT value is freed, dispatched by the tag; weak references don't
contribute to the retain count.

### Methods, traits, and operators

An ADT body can carry methods directly, just like a struct.  Use
`match this:` to dispatch on the variant:

```rust
data Color =
  Red
  Green
  Blue
  Rgb(r i64, g i64, b i64)

  fn luminance(this Color) i64 =
    match this:
      case Red:        return 76
      case Green:      return 150
      case Blue:       return 29
      case Rgb(r,g,b): return (r * 76 + g * 150 + b * 29) / 256

let c = Color::Rgb(255, 128, 64)
echo c.luminance()
```

The implements list works exactly the same way it does on structs.
Trait methods qualify with `<Trait>::` so the compiler can wire them into
the right vtable slot:

```rust
trait Show =
  fn show(this Show) string = virtual

data Shape(Show) =
  Dot
  Circle(radius f64)
  Rect(width f64, height f64)

  fn Show::show(this Shape) string =
    match this:
      case Dot:        return "."
      case Circle(r):  return "() r={r}"
      case Rect(w, h): return "[] {w}*{h}"

let s Show = Shape::Circle(3.0)   // value-form coerce, heap-copy
echo s.show()
let p *Show = &Shape::Rect(2.0, 4.0)  // borrow-form, no copy
echo (*p).show()
```

Operator overloads use the same `add[rhs, ret]` / `sub[rhs, ret]` / ...
trait aliases as on structs (see
[traits / operator overloading](06-traits.md#operator-overloading)).
List the alias in the implements list and provide the impl in qualified
form:

```rust
data Counter(add[bool, Counter], add[i64, Counter]) =
  Zero
  Cnt(n i64)

  fn add[bool, Counter]::add(this Counter, b bool) Counter =
    let inc = 0 as i64
    if b: inc = 1 as i64
    match this:
      case Zero:   return Cnt(inc)
      case Cnt(c): return Cnt(c + inc)

  fn add[i64, Counter]::add(this Counter, n i64) Counter =
    match this:
      case Zero:   return Cnt(n)
      case Cnt(c): return Cnt(c + n)

let a Counter = Some(true)  // bare ctor inferred from let-type
let b = a + true            // -> Cnt(2)
let c = b + 40              // -> Cnt(42)
```

The full list of operator traits (`mul`, `div`, `concat`, `neg`, `comp`,
`ord`, `index`, ...) lives in the [traits document](06-traits.md#operator-traits).

### Runtime layout

ADT values share the tagged-union layout:

```
{ i32 type_id, i8 tag, [max_payload_size x i8] payload }
```

- `tag` is the ordinal index of the active variant (0-based, declaration
  order).
- `payload` holds the active variant's packed field struct; unused variants
  pad out to the largest payload size.
- `type_id` is a compile-time constant shared with structs/unions, used by
  `typeof` and `any` boxing.

### `data` vs `type = A | B | C`

- **Tagged union** (`type num = i64 | f64`): discriminated by the *type*
  of the stored value. Test with `is T` or `match x.(type)`.
- **ADT** (`data Kind = ...`): discriminated by *constructor name*. Test
  with `is Ctor(...)` or `match x: case Ctor(...)`.

They compile to the same runtime shape, so interop through `any` boxing is
uniform.

### The `try` keyword

Writing `match` arms for every Result is fine when each call really does
different work on Err -- but it gets noisy fast. Most code wants the same
thing: "give me the value or bubble the error up". `try` is the shorthand:

```rust
fn parse_pair(s string) Result[(i64, i64), errors::Err] =
  let parts = strings::split(s, ",")

  if len(parts) != 2:
    return Err(errors::new("need two comma-separated ints"))

  let a = try strings::parse_int(parts[0])
  let b = try strings::parse_int(parts[1])

  return Ok((a, b))
```

Each `try e` desugars to:

```rust
match e:
  case Ok(v):  v             // bound into the surrounding expression
  case Err(x): return Err(x) // propagate
```

`try` is sugar over the `tryable` trait (see [traits](06-traits.md)).
Anything that implements `tryable[T, E]` can be used with `try`, not just
`Result[T, E]` -- `Option[T]` works too (`None` propagates as the empty
error of the surrounding `Result`). User types that want the same flow
implement `tryable::is_err`, `tryable::ok_value`, `tryable::err_value`.

The enclosing function must return a compatible `Result` (or `Option`)
shape; otherwise the compiler refuses the `try` and tells you what type
it expected.

### `fn main() Result[...]`

`main` may return a `Result` instead of nothing:

```rust
use errors
use { Result } from result

fn main() Result[i64, errors::Err] =
  let cfg = try load_config()
  let port = try cfg.port()

  return Ok(port)
```

The C-main wrapper unpacks the return value:

- `Ok(n)` (where `n: i64`) -> `exit(n)`. Use this when you want the
  process exit code to come from your program's logic.
- `Ok(())` for `Result[Unit, ...]` -> `exit(0)`.
- `Err(e)` -> prints `error: {e.message()}` to stderr and `exit(1)`.

You can also use `try` at the top level without writing a `main` at all
-- the compiler synthesizes a `Result`-returning wrapper around the
top-level statements:

```rust
use errors

let cfg = try load_config()  // bubbles up to the synthesized main
echo cfg.port
```
