# Encoding

`stdlib/encoding` groups all encoding and decoding sub-packages under a single
import. Import the umbrella package to get all of them, or import a sub-package
directly to keep the namespace clean.

```rust
use encoding           // encoding::base16, encoding::base64, encoding::url, encoding::json, encoding::yaml
use encoding::base16   // base16::encode, base16::decode
use encoding::base64   // base64::encode, base64::decode
use encoding::url      // url::encode, url::decode, url::encode_query, url::parse_query
use encoding::json     // json::encode, json::parse
use encoding::yaml     // yaml::encode, yaml::parse
```

---

## `encoding::base16`

Hex (base-16) encoding and decoding. Output uses lowercase hex digits by
default; `encode_upper` produces uppercase.

| Function | Signature | Description |
|----------|-----------|-------------|
| `encode` | `(s string) string` | Encode bytes to lowercase hex (`"ab0f"`) |
| `encode_upper` | `(s string) string` | Encode bytes to uppercase hex (`"AB0F"`) |
| `decode` | `(s string) string` | Decode hex string back to raw bytes; ignores invalid characters |

```rust
use encoding::base16

echo base16::encode("Hi!")         // "486921"
echo base16::encode_upper("Hi!")   // "486921" -> "486921" (uppercase: "486921")
echo base16::decode("486921")      // "Hi!"
```

---

## `encoding::base64`

Base-64 encoding and decoding. Standard alphabet uses `+` and `/`; URL-safe
alphabet uses `-` and `_`. Both variants pad output with `=` to a multiple of
4 characters.

| Function | Signature | Description |
|----------|-----------|-------------|
| `encode` | `(s string) string` | Standard base-64 encode (`+/` alphabet, padded) |
| `encode_url` | `(s string) string` | URL-safe base-64 encode (`-_` alphabet, padded) |
| `decode` | `(s string) string` | Decode standard or URL-safe base-64; skips unknown characters |

```rust
use encoding::base64

echo base64::encode("Hello, Tin!")      // "SGVsbG8sIFRpbiE="
echo base64::encode_url("Hello, Tin!")  // "SGVsbG8sIFRpbiE="
echo base64::decode("SGVsbG8sIFRpbiE=") // "Hello, Tin!"
```

---

## `encoding::url`

URL percent-encoding and query string handling (RFC 3986).

| Function | Signature | Description |
|----------|-----------|-------------|
| `encode` | `(s string) string` | Percent-encode a string; only RFC 3986 unreserved chars (`A-Za-z0-9-_.~`) pass through |
| `encode_component` | `(s string) string` | Alias for `encode` |
| `decode` | `(s string) string` | Decode `%XX` sequences; `+` is treated as a space |
| `encode_query` | `(params HashMap[string, string]) string` | Encode a map as a sorted query string (`key=value&...`) |
| `parse_query` | `(s string) HashMap[string, string]` | Parse a query string into a map; keys and values are percent-decoded |

```rust
use encoding::url
use collections

echo url::encode("hello world")          // "hello%20world"
echo url::encode("foo=bar&baz=1")        // "foo%3Dbar%26baz%3D1"
echo url::decode("hello%20world")        // "hello world"
echo url::decode("foo+bar")             // "foo bar"

let params = collections::HashMap[string, string].make(4)
params.set("q", "hello world")
params.set("lang", "tin")
echo url::encode_query(params)           // "lang=tin&q=hello%20world"

let m = url::parse_query("q=hello+world&page=2")
echo m.get("q")                          // "hello world"
echo m.get("page")                       // "2"
```

---

## `encoding::json`

See [JSON](json.md) for the full reference. Quick summary:

| Function | Description |
|----------|-------------|
| `encode[T](v T) string` | Serialize a typed value to JSON |
| `parse(s string) json::Value` | Parse JSON into an untyped value tree |
| `parse[T](s string) T` | Parse JSON directly into a struct |

---

## `encoding::yaml`

See [YAML](yaml.md) for the full reference. Quick summary:

| Function | Description |
|----------|-------------|
| `encode[T](v T) string` | Serialize a typed value to YAML |
| `parse(s string) yaml::Value` | Parse YAML into an untyped value tree |
| `parse[T](s string) T` | Parse YAML directly into a struct |
