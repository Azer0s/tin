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
type direction = enum:
  north, south, east, west

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
