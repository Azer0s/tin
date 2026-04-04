# YAML

Tin provides YAML encoding and decoding through `stdlib/yaml`.

> **No extra linker flags are required.** The yaml stdlib is written entirely in
> Tin with no external C dependencies.

---

## Import

```rust
use yaml
```

---

## Encoding

`yaml::encode[T](v T) string` serialises any Tin value to a YAML block-style
string. `T` may be a primitive type (`bool`, `i64`, `f64`), `string`, or struct.

```rust
yaml::encode(true)             // "true\n"
yaml::encode(false)            // "false\n"
yaml::encode(42 as i64)        // "42\n"
yaml::encode("hello")          // "hello\n"
yaml::encode("key: val")       // "\"key: val\"\n"
```

String values that contain YAML-special characters (`:`, `#`, `{`, `}`, `[`,
`]`, newlines, etc.) or that match YAML reserved words (`null`, `true`,
`false`, `yes`, `no`, `on`, `off`, `~`) are automatically double-quoted with
escape sequences.

### Struct encoding

Struct fields are encoded using compile-time reflection. By default the field
name is used as the YAML key:

```rust
struct point =
  x i64
  y i64

yaml::encode(point{x: 3, y: 4})
// x: 3
// y: 4
```

### Nested struct encoding

Nested structs are encoded as indented block mappings:

```rust
struct rect =
  origin point
  size   point

yaml::encode(rect{origin: point{x: 0, y: 0}, size: point{x: 100, y: 50}})
// origin:
//   x: 0
//   y: 0
// size:
//   x: 100
//   y: 50
```

### Field tags

Add a `@"yaml:..."` tag to control how a field appears in the output:

```rust
struct user =
  id    i64    @"yaml:user_id"        // rename key
  email string @"yaml:email_address"  // rename key
  pwd   string @"yaml:-"              // omit field entirely

yaml::encode(user{id: 1, email: "a@b.com", pwd: "secret"})
// user_id: 1
// email_address: a@b.com
```

| Tag             | Effect                            |
|-----------------|-----------------------------------|
| `@"yaml:name"`  | Use `name` as the YAML key        |
| `@"yaml:-"`     | Omit this field from the output   |
| _(no tag)_      | Use the Tin field name unchanged  |

---

## Parsing - untyped

`yaml::parse(s string) Value` parses a YAML document into a dynamic `Value`
tree. Use this when the YAML structure is not known at compile time.

```rust
let v = yaml::parse("name: alice\nage: 30\n")
v.get("name").as_string()   // "alice"
v.get("age").as_int()       // 30
```

### `Value` kind checks

```rust
v.is_null()    // true for YAML null, ~, empty input
v.is_bool()
v.is_int()
v.is_float()
v.is_string()
v.is_array()
v.is_object()
```

### `Value` accessors

```rust
v.as_bool()    // bool
v.as_int()     // i64
v.as_float()   // f64
v.as_string()  // string
```

### Object access

```rust
let obj = yaml::parse("x: 1\ny: 2\n")

// Get value by key (returns null Value if missing):
obj.get("x").as_int()          // 1
obj.get("missing").is_null()   // true

// Iterate all keys:
let keys = obj.keys()          // [string]
for let k string in keys:
  echo "{k}: {obj.get(k).as_int()}"
```

### Array access

```rust
let arr = yaml::parse("- 10\n- 20\n- 30\n")

arr.array_len()          // 3
arr.index(0).as_int()    // 10
arr.index(1).as_int()    // 20
arr.index(99).is_null()  // true - out of bounds returns null
```

### Supported input formats

**Block mappings:**
```yaml
name: alice
age: 30
```

**Block sequences:**
```yaml
- apple
- banana
- cherry
```

**Flow collections:**
```yaml
{x: 1, y: 2}
[1, 2, 3]
```

**Nested block mappings:**
```yaml
origin:
  x: 3
  y: 4
size:
  x: 100
  y: 50
```

**Scalars:** booleans (`true`/`false`/`yes`/`no`/`on`/`off`), integers,
floats, plain strings, double-quoted strings with escape sequences.

**Document markers:** `---` document start, `#` comments, and blank lines are
all skipped automatically.

---

## Parsing - typed (generic)

`yaml::parse[T](s string) T` decodes YAML directly into a struct `T` using
compile-time reflection. This is more convenient than untyped parsing when the
YAML schema is fixed at compile time.

```rust
struct point =
  x i64
  y i64

let p = yaml::parse[point]("x: 3\ny: 4\n")
echo p.x    // 3
echo p.y    // 4
```

Field tags work the same way as for encoding:

```rust
struct api_user =
  id   i64    @"yaml:user_id"
  name string @"yaml:display_name"

let u = yaml::parse[api_user]("user_id: 7\ndisplay_name: bob\n")
echo u.id    // 7
echo u.name  // "bob"
```

Unknown YAML keys are silently ignored. Missing YAML keys leave the
corresponding struct field at its zero value.

### Nested struct parsing

Nested structs are decoded recursively:

```rust
struct rect =
  origin point
  size   point

let r = yaml::parse[rect]("origin:\n  x: 3\n  y: 4\nsize:\n  x: 100\n  y: 50\n")
echo r.origin.x   // 3
echo r.size.y     // 50
```

---

## Supported field types

Typed parsing maps YAML scalars to Tin types as follows:

| YAML type             | Tin field types                                          |
|-----------------------|----------------------------------------------------------|
| integer               | `i64`, `i32`, `i16`, `i8` (truncated), `f64` (promoted) |
| float                 | `f64`; `i64` (truncated)                                 |
| bool                  | `bool`                                                   |
| string                | `string`                                                 |
| null / ~              | _(field left at zero value)_                             |
| nested mapping        | nested struct (decoded recursively)                      |

---

## Reference

| Function / Method                  | Description                                                        |
|------------------------------------|--------------------------------------------------------------------|
| `yaml::encode(v)`                  | Encode any Tin value to a YAML block-style string                  |
| `yaml::parse(s)`                   | Parse YAML into an untyped `Value` tree                            |
| `yaml::parse[T](s)`                | Parse YAML directly into struct `T`                                |
| `v.is_null()` / `v.is_int()` / ... | Kind checks on `Value`                                             |
| `v.as_bool()` / `v.as_int()` / ... | Type accessors on `Value`                                          |
| `v.get(key)`                       | Look up key in a YAML object `Value`                               |
| `v.keys()`                         | All keys in a YAML object `Value`                                  |
| `v.index(i)`                       | Element `i` of a YAML array `Value`                                |
| `v.array_len()`                    | Number of elements in a YAML array `Value`                         |
