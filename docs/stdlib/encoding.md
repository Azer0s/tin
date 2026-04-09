# Encoding

`stdlib/encoding` groups all encoding and decoding sub-packages under a single
import. Import the umbrella package to get all of them, or import a sub-package
directly to keep the namespace clean.

```rust
use encoding           // encoding::base16, encoding::base64, encoding::json, encoding::yaml
use encoding::base16   // base16::encode, base16::decode
use encoding::base64   // base64::encode, base64::decode
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
