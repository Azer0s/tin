# JSON

Tin provides JSON encoding and decoding through `stdlib/json`.

> **No extra linker flags are required.** The json stdlib uses only libc and
> the Tin runtime.

---

## Import

```tin
use json
```

---

## Encoding

`json::encode[T](v T) string` serialises any Tin value to a JSON string.
`T` may be a primitive type (`bool`, `i64`, `f64`), `string`, struct, or tuple.

```tin
json::encode(42 as i64)          // "42"
json::encode(true)               // "true"
json::encode(false)              // "false"
json::encode("hello")            // "\"hello\""
json::encode("say \"hi\"")       // "\"say \\\"hi\\\"\""
```

### Struct encoding

Struct fields are encoded using compile-time reflection. By default the field
name is used as the JSON key:

```tin
struct point =
  x i64
  y i64

json::encode(point{x: 3, y: 4})  // "{\"x\":3,\"y\":4}"
```

### Nested struct encoding

Top-level struct fields of primitive types are encoded correctly. Fields whose
type is another struct are encoded as `null` - this is a limitation of the
reflection system, which resolves field names at compile time but cannot
enumerate fields of runtime `any` values:

```tin
struct rect =
  origin point
  size   point

json::encode(rect{origin: point{x:0, y:0}, size: point{x:100, y:50}})
// {"origin":null,"size":null}
```

For nested structures, use untyped encoding per-level or build the JSON
string manually.

### Tuple encoding

Tuples are encoded as JSON objects with alphabetical field names (`a`, `b`,
`c`, ...):

```tin
json::encode((10, "hello"))           // "{\"a\":10,\"b\":\"hello\"}"
json::encode((1, true, 3.14))         // "{\"a\":1,\"b\":true,\"c\":3.14}"
```

### Field tags

Add a `@"json:..."` tag to control how a field appears in JSON:

```tin
struct user =
  id    i64    @"json:user_id"          // rename key
  email string @"json:email_address"   // rename key
  pwd   string @"json:-"               // omit field entirely

json::encode(user{id: 1, email: "a@b.com", pwd: "secret"})
// {"user_id":1,"email_address":"a@b.com"}
```

| Tag            | Effect                           |
|----------------|----------------------------------|
| `@"json:name"` | Use `name` as the JSON key       |
| `@"json:-"`    | Omit this field from the output  |
| _(no tag)_     | Use the Tin field name unchanged |

---

## Parsing - untyped

`json::parse(s string) Value` parses a JSON string into a dynamic `Value` tree.
Use this when the JSON structure is not known at compile time.

```tin
let v = json::parse("{\"name\":\"alice\",\"age\":30}")
v.get("name").as_string()   // "alice"
v.get("age").as_int()       // 30
```

### `Value` kind checks

```tin
v.is_null()    // true for JSON null
v.is_bool()
v.is_int()
v.is_float()
v.is_string()
v.is_array()
v.is_object()
```

### `Value` accessors

```tin
v.as_bool()    // bool
v.as_int()     // i64
v.as_float()   // f64
v.as_string()  // string
```

### Object access

```tin
let obj = json::parse("{\"x\":1,\"y\":2}")

// Get value by key (returns null Value if missing):
obj.get("x").as_int()        // 1
obj.get("missing").is_null() // true

// Iterate all keys:
let keys = obj.keys()        // [string]
for let k string in keys:
  echo "{k}: {obj.get(k).as_int()}"
```

### Array access

```tin
let arr = json::parse("[10,20,30]")

arr.array_len()           // 3
arr.index(0).as_int()     // 10
arr.index(1).as_int()     // 20
arr.index(99).is_null()   // true - out of bounds returns null
```

---

## Parsing - typed (generic)

`json::parse[T](s string) T` decodes JSON directly into a struct `T` using
compile-time reflection. This is more convenient than untyped parsing when the
JSON schema is fixed at compile time.

```tin
struct point =
  x i64
  y i64

let p = json::parse[point]("{\"x\":3,\"y\":4}")
echo p.x    // 3
echo p.y    // 4
```

Field tags work the same way as for encoding:

```tin
struct api_user =
  id   i64    @"json:user_id"
  name string @"json:display_name"

let u = json::parse[api_user]("{\"user_id\":7,\"display_name\":\"bob\"}")
echo u.id    // 7
echo u.name  // "bob"
```

Unknown JSON keys are silently ignored. Missing JSON keys leave the
corresponding struct field at its zero value.

> **Note:** Typed parsing (`parse[T]`) currently supports flat structs with
> primitive fields (`bool`, `i64`, `f64`, `string`). Nested struct fields are
> not yet populated via typed parsing; use untyped `parse` + `get` for nested
> structures.

---

## Supported field types

Typed parsing maps JSON values to Tin types as follows:

| JSON type          | Tin field types                                         |
|--------------------|---------------------------------------------------------|
| `number` (integer) | `i64`, `i32`, `i16`, `i8` (truncated), `f64` (promoted) |
| `number` (float)   | `f64`; `i64` (truncated)                                |
| `bool`             | `bool`                                                  |
| `string`           | `string`                                                |
| `null`             | _(field left at zero value)_                            |

---

## Reference

| Function / Method                  | Description                                                              |
|------------------------------------|--------------------------------------------------------------------------|
| `json::encode(v)`                  | Encode any Tin value to a JSON string (primitives, structs, tuples)      |
| `json::parse(s)`                   | Parse JSON into an untyped `Value` tree                                  |
| `json::parse[T](s)`                | Parse JSON directly into struct `T` (flat structs with primitive fields) |
| `v.is_null()` / `v.is_int()` / ... | Kind checks on `Value`                                                   |
| `v.as_bool()` / `v.as_int()` / ... | Type accessors on `Value`                                                |
| `v.get(key)`                       | Look up key in a JSON object `Value`                                     |
| `v.keys()`                         | All keys in a JSON object `Value`                                        |
| `v.index(i)`                       | Element `i` of a JSON array `Value`                                      |
| `v.array_len()`                    | Number of elements in a JSON array `Value`                               |
