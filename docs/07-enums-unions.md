# 07 – Enums & Unions

## Enums

An enum declares a set of named integer constants. The base integer type is
specified before the enum name:

```rust
enum i32 direction =
  north: 0,
  south: 1,
  east:  2,
  west:  3,
```

If you omit explicit values, they are assigned automatically starting from 0:

```rust
enum slider_type =   // compiler picks the smallest type (u8 here)
  horizontal,
  vertical,
```

### Using enum values

```rust
let d direction = direction.north
```

Enum values are accessed with the dot syntax `EnumName.MemberName`.

### Matching on enums

```rust
fn direction_name(d direction) string =
  match d:
    case direction.north: return "north"
    case direction.south: return "south"
    case direction.east:  return "east"
    case direction.west:  return "west"
    default:              return "unknown"
```

```rust
fn is_weekend(d day) bool =
  match d:
    case day.saturday: return true
    case day.sunday:   return true
    default:           return false
```

### Using if with enums

```rust
enum i32 day =
  monday: 0, tuesday: 1, wednesday: 2, thursday: 3,
  friday: 4, saturday: 5, sunday: 6,

let today day = day.saturday
if is_weekend(today):
  echo "it is the weekend"
```

---

## Union types (tagged unions)

A tagged union is a `type` alias over two or more types separated by `|`:

```rust
type num = i64 | f64
type strnum = string | i64
```

**Memory layout:** `{ i8 tag, [maxSize x i8] payload }` where tag 0 = first
variant, 1 = second, and so on. The payload is sized to the largest variant.

### Assigning union values

Assignment automatically selects the correct tag by matching the value's type:

```rust
let a num = 42        // tag=0, payload holds i64
let b num = 3.14      // tag=1, payload holds f64
```

### Testing with `is`

`is` checks the active variant and optionally binds the inner value:

```rust
if a is i64:          // true/false check only
  echo "integer"

if a is n i64:        // n is bound as i64 in this branch
  echo n
```

### Matching on union type

`match a.(type)` dispatches on the tag, binding the payload in each case:

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

## Wrapper types (`data`)

`data` declares a discriminated union with a `None` variant, similar to
Rust's `Option` or Haskell's `Maybe`:

```rust
data maybe[t] = t | None

let m maybe[string] = None

if m is s string:
  echo s

if m is None:
  echo "m is unset"
```

`data` introduces a named wrapper type (not a raw C union) with safe
pattern-matching semantics. The layout includes a `i32` type ID plus variant
tag for runtime type checks.

---

## Native C unions (`union`)

`union` maps directly to a C union — all variants share the same memory with
no tag. Use this for FFI or when you need to manually reinterpret bytes.

**Memory layout:** `{ [maxSize x i8] storage }` — a single byte array.

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
let v i32 = c.as_i32    // read storage as i32
let b u8  = c.as_r      // read same storage as u8
```

> Native unions have no tag — reading from the "wrong" field returns
> reinterpreted bytes. Use `data` or tagged unions for safe alternatives.

---

## Atoms and atom enums

**Atoms** are symbolic constants written with a leading single-quote. They
are interned at compile time and compare by identity:

```rust
enum atom status =
  'ok,
  'err
```

Function bodies can use `where` to match atoms:

```rust
fn it_is(w atom) =
  where 'sunny: echo "It is sunny!"
  where 'rainy: echo "It is rainy!"
  where _:      echo "Unknown weather"

it_is('sunny)    // It is sunny!
```
