# 07 - Enums & Unions

## Integer enums

An enum declares a set of named integer constants.

### With explicit base type

Specify the base integer type before the enum name:

```rust
enum i32 direction =
  north: 0
  south: 1
  east:  2
  west:  3
```

### Auto-typed enums

Omit the type and let the compiler pick the smallest integer type:

```rust
enum Kind =
  null_v
  bool_v
  int_v
  float_v
  string_v
  array_v
  object_v
```

Members without explicit values are auto-numbered from 0. You can mix
explicit and automatic values:

```rust
enum day =
  monday: 0
  tuesday
  wednesday
  thursday
  friday
  saturday
  sunday
```

### Accessing values

Use `EnumName.MemberName` dot syntax:

```rust
let d direction = direction.north
let today day = day.saturday
```

### Matching on enums

```rust
fn direction_name(d direction) string =
  match d:
    case direction.north: return "north"
    case direction.south: return "south"
    case direction.east:  return "east"
    case direction.west:  return "west"
    default:              return "unknown"

fn is_weekend(d day) bool =
  match d:
    case day.saturday: return true
    case day.sunday:   return true
    default:           return false
```

### Exhaustive enum matches

When every member of an enum is covered by an explicit `case`, the `default`
clause is optional. The compiler detects this and does not require a trailing
`return` after the match:

```rust
enum direction =
  north
  south
  east
  west

fn direction_name(d direction) string =
  match d:
    case direction.north: return "north"
    case direction.south: return "south"
    case direction.east:  return "east"
    case direction.west:  return "west"
    // no default needed - all 4 members are covered
```

This only applies when **all** declared members are explicitly listed. A
partial match (some members omitted, no `default`) is a compile-time error:

```rust
// ERROR: fn direction_name: not all code paths return a value
fn direction_name(d direction) string =
  match d:
    case direction.north: return "north"
    // south, east, west not handled
```

---

## Atom enums

Atom enums use symbolic `'name` constants instead of integers. They are
interned at compile time and compare by identity (like atoms in Erlang or
symbols in Ruby).

```rust
enum atom status =
  'ok
  'err

enum atom weather =
  'sunny
  'rainy
  'cloudy
```

Access members directly - they are plain atom values:

```rust
let s status = 'ok
```

### Pattern matching with `where`

The natural way to dispatch on atom enums is `where`:

```rust
fn describe(s status) string =
  where 'ok:  return "all good"
  where 'err: return "error occurred"

fn weather_msg(w weather) string =
  where 'sunny: return "bring sunglasses"
  where 'rainy: return "bring an umbrella"
  where _:      return "check the forecast"
```

`match` works too:

```rust
match s:
  case 'ok:  echo "ok"
  case 'err: echo "error"
```

### Standalone atoms

Atoms can be used as ad-hoc symbolic constants without declaring an enum:

```rust
let tag = 'pending
if tag == 'pending:
  echo "waiting"
```

Atoms are also the return type of `typeof`, `fieldnames`, and `fieldtypes`.
See [10 - Reflection](10-reflection.md) for the full atom and reflection API.

---

## Tagged unions

A tagged union is a `type` alias over two or more types joined with `|`:

```rust
type num    = i64 | f64
type strnum = string | i64
```

**Memory layout:** `{ i8 tag, [maxSize x i8] payload }` where tag 0 = first
variant, 1 = second. The payload is sized to the largest variant.

### Assigning values

Assignment automatically selects the correct tag by matching the value's type:

```rust
let a num = 42        // tag=0 (i64)
let b num = 3.14      // tag=1 (f64)
```

### Testing with `is`

```rust
if a is i64:          // type check only
  echo "integer"

if a is n i64:        // binds n as i64 if the check passes
  echo n
```

### Matching on a tagged union

`match x.(type)` dispatches on the active tag and binds the payload:

```rust
match a.(type):
  case n i64:
    echo n
  case x f64:
    echo x
  default:
    echo "other"
```

### Tagged unions as generic type arguments

`type X = A | B` plays two roles:

1. A **tagged-union type** when you have a value of it. The value carries a
   tag and is laid out as `{ i8 tag, payload }`.
2. A **type-set alias** when used in a where-guard. `where t is X` is
   satisfied by any structural variant of X.

Both meanings are preserved when X appears as a generic type argument. A
generic instantiated with X stores the **literal tagged union** (with the
tag), not one of the underlying variants:

```rust
use sync

type num = i64 | f64

let a num = 42
let atom = sync::Atomic[num].new(a)   // Atomic stores num, not i64
let v = atom.load()                   // v is num, tag preserved

match v.(type):
  case n i64: echo "i64 {n}"
  case f f64: echo "f64 {f}"
```

### Where-guards over tagged unions

`where t is X` matches **either** the literal X or any of its structural
variants, so a single overload can cover both forms:

```rust
type num = i64 | f64

struct Box[t] =
  v t
  fn show(this Box[t]) string where t is num =
    match this.v.(type):
      case n i64: return "i64 {n}"
      case f f64: return "f64 {f}"

let b1 = Box[num]{v: 42 as num}    // t = num   -> matches `where t is num`
let b2 = Box[i64]{v: 7}            // t = i64   -> also matches (i64 is a variant of num)
let b3 = Box[f64]{v: 3.14}         // t = f64   -> also matches
```

Inside a `where t is X` body, `t` is only known to be one of X's variants -
it has no single concrete type. Use `match v.(type)` to recover the
concrete variant. The compiler will not let you call type-specific operations
on a `t`-typed value without one.

### Compile-time match resolution

When the generic is instantiated with a **single concrete variant** (e.g.
`Box[i64]` for `where t is num`), `match v.(type)` is statically
resolvable. The compiler keeps only the arm whose type matches the
substituted `t` and dead-strips the rest -- no tag load, no switch, no
branch. The arms for unreachable variants don't even reach the IR.

```rust
let bi = Box[i64]{v: 42}
echo bi.show()   // emits the i64 arm body only; the f64 arm is gone

let bn = Box[num]{v: 42 as num}
echo bn.show()   // emits the full tag-dispatched switch (real tagged union)
```

If no arm matches the concrete type:

- with a `default:` arm, the default body becomes the only emitted code.
- without a `default:` arm, the compiler errors out -- the user wrote a
  match that proves the instantiation is wrong (e.g. `Box[bool]` for a
  `where t is num` body matching only i64/f64).

This means `match v.(type)` inside a where-guarded function is free at
runtime when the type-arg is concrete and pays only for the variants you
actually write when the type-arg is the literal tagged union.

---

## Native C unions

`union` maps directly to a C union - all variants share the same memory with
no tag. Use this for FFI or when you need to reinterpret bytes.

**Memory layout:** a single `[maxSize x i8]` byte array.

### Unnamed union

```rust
union raw = i32 | i64
```

Access via type cast:

```rust
let r raw = 42
let v i32 = r.(i32)   // read storage as i32
let w i64 = r.(i64)   // read storage as i64
```

### Named union

```rust
union color = as_i32 i32 | as_r u8
```

Access via field name or type cast:

```rust
let c color = 255
let v i32 = c.as_i32   // read as i32
let b u8  = c.as_r     // read same bytes as u8
```

> Native unions have no tag - reading from the "wrong" field reinterprets
> bytes silently. Use tagged unions (`type x = T | U`) for safe sum types.

---

## Algebraic data types (`data`)

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

// where-patterns reuse the same constructor syntax
fn unwrap_or(r Result[i32, string], d i32) i32 =
  where r is Ok(v):  return v
  where r is Err(_): return d
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
