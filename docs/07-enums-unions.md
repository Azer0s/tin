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

Access members directly - they are plain atom values. Use `atom` as the
binding type (the enum name is only the declaration grouping):

```rust
let s atom = 'ok
```

### Pattern matching with `where`

The natural way to dispatch on atom enums is `where`. Functions take the
parameter as type `atom`, and the compiler currently requires a `where _`
wildcard fallback even when every named atom is covered:

```rust
fn describe(s atom) string =
  where 'ok:  return "all good"
  where 'err: return "error occurred"
  where _:    return "unknown"

fn weather_msg(w atom) string =
  where 'sunny: return "bring sunglasses"
  where 'rainy: return "bring an umbrella"
  where _:      return "check the forecast"
```

> Atom values are stored as runtime-interned identities (parameter type
> `atom`), not as the enum's underlying integer. Using the enum name
> (`status`, `weather`) as a variable or parameter type binds an i32
> slot and will not accept atom literals.

`match` over atoms is not supported - use `where` clauses for atom dispatch.

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



## See also

- [Tagged unions](07-tagged-unions.md)
- [Native C unions](07-c-unions.md)
- [Algebraic data types](07-data-types.md)
