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

A union type is declared with `|`:

```rust
type u = i8 | string
```

A variable of union type can hold a value of any of the listed types. The
runtime representation carries a tag to remember which variant is active.

### Assigning union values

```rust
let a u = 10          // holds an i8
let b u = "hello"     // holds a string
```

### Testing and narrowing with `is`

`is` tests which variant is active and binds the inner value:

```rust
if a is i i8:
  echo i * i          // i is bound as i8
```

### Matching on union type

```rust
match a.(type):
  case i i8:
    echo i
  case s string:
    echo s
```

### Casting with `as`

```rust
echo a as string
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
pattern-matching semantics.

---

## Native C unions (`union`)

`union` maps directly to a C union — all variants share the same memory:

```rust
union u = i8 | string
union u_named = as_i8 i8 | as_string string

let a u = 10
echo a as string
```

Named union variants can be accessed with dot notation:

```rust
let b u_named = 10
echo b.as_string    // same as b.(string)
```

> Native unions provide no runtime type tag. Use `data` for safe tagged
> unions.

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
