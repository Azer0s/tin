# Hash

`stdlib/hash` provides non-cryptographic and cryptographic hash functions,
all implemented in pure Tin with no C dependencies.

## Modules

| Module | Import | Algorithm | Output |
|--------|--------|-----------|--------|
| `hash::fnv` | `use hash::fnv` | FNV-1a | 32-bit or 64-bit |
| `hash::md5` | `use hash::md5` | MD5 | 128-bit digest / hex string |
| `hash::sha1` | `use hash::sha1` | SHA-1 | 160-bit digest / hex string |
| `hash::xxhash3` | `use hash::xxhash3` | xxHash3 | 64-bit |

---

## `hash::fnv` - FNV-1a

FNV-1a is a fast non-cryptographic hash suitable as a default hash for small
keys. Each function is overloaded to accept either raw bytes (`*u8` + length)
or a `string` directly.

```rust
use hash::fnv

// 32-bit
let h32 u32 = fnv::fnv1a_32("hello")
let h32b u32 = fnv::fnv1a_32(ptr as *u8, byte_count)

// 64-bit
let h64 u64 = fnv::fnv1a_64("hello")
let h64b u64 = fnv::fnv1a_64(ptr as *u8, byte_count)
```

### Reference

| Function | Signature | Description |
|----------|-----------|-------------|
| `fnv1a_32` | `(data *u8, len i64) u32` | FNV-1a 32-bit over raw bytes |
| `fnv1a_32` | `(s string) u32` | FNV-1a 32-bit over a string |
| `fnv1a_64` | `(data *u8, len i64) u64` | FNV-1a 64-bit over raw bytes |
| `fnv1a_64` | `(s string) u64` | FNV-1a 64-bit over a string |

---

## `hash::md5` - MD5

MD5 produces a 128-bit digest. **Not cryptographically safe** - use only for
checksums and legacy compatibility.

```rust
use hash::md5

// Hex digest string from a string:
let hex = md5::md5("hello")            // "5d41402abc4b2a76b9719d911017c592"

// Hex digest from raw bytes:
let hex2 = md5::md5_hex(ptr as *u8, byte_count)

// Raw [u8; 16] digest:
let digest = md5::md5_hex_from_digest(raw_digest)
let raw [u8; 16] = md5::md5(ptr as *u8, byte_count)
```

Overloads: `md5(s string) string` is the most convenient form and returns a
40-character lowercase hex string.

### Reference

| Function | Signature | Description |
|----------|-----------|-------------|
| `md5` | `(data *u8, len i64) [u8; 16]` | Raw 128-bit digest |
| `md5` | `(s string) string` | Hex digest of a string |
| `md5_hex` | `(data *u8, len i64) string` | Hex digest of raw bytes |
| `md5_hex_from_digest` | `(digest [u8; 16]) string` | Convert raw digest to hex |

---

## `hash::sha1` - SHA-1

SHA-1 produces a 160-bit (20-byte) digest. **Not cryptographically safe** -
use for checksums and legacy compatibility only (e.g. Git object IDs).

```rust
use hash::sha1

// Hex digest string from a string:
let hex = sha1::sha1("hello")    // "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"

// Hex digest from raw bytes:
let hex2 = sha1::sha1_hex(ptr as *u8, byte_count)

// Raw [u8; 20] digest:
let raw [u8; 20] = sha1::sha1(ptr as *u8, byte_count)

// Hex from a pre-computed digest:
let hex3 = sha1::sha1_hex_from_digest(raw)
```

### Reference

| Function | Signature | Description |
|----------|-----------|-------------|
| `sha1` | `(data *u8, len i64) [u8; 20]` | Raw 160-bit digest |
| `sha1` | `(s string) string` | Hex digest of a string |
| `sha1_hex` | `(data *u8, len i64) string` | Hex digest of raw bytes |
| `sha1_hex_from_digest` | `(digest [u8; 20]) string` | Convert raw digest to hex |

---

## `hash::xxhash3` - xxHash3

xxHash3 is a high-speed non-cryptographic hash designed for hash tables and
checksums. Produces a 64-bit result. Suitable as a general-purpose hashmap hash.

```rust
use hash::xxhash3

// Hash a string (seed = 0):
let h u64 = xxhash3::xxhash3("hello world")

// Hash raw bytes (seed = 0):
let h2 u64 = xxhash3::xxhash3(ptr as *u8, byte_count)

// Seeded hash (string overload):
let h3 u64 = xxhash3::xxhash3_seeded("hello", 0xdeadbeef)

// Seeded hash (raw bytes):
let h4 u64 = xxhash3::xxhash3_seeded(ptr as *u8, byte_count, seed)
```

The seeded variant accepts either `(data *u8, len i64, seed u64)` or
`(s string, seed u64)`.

### Reference

| Function | Signature | Description |
|----------|-----------|-------------|
| `xxhash3` | `(data *u8, len i64) u64` | Hash raw bytes, seed = 0 |
| `xxhash3` | `(s string) u64` | Hash a string, seed = 0 |
| `xxhash3_seeded` | `(data *u8, len i64, seed u64) u64` | Hash raw bytes with seed |
| `xxhash3_seeded` | `(s string, seed u64) u64` | Hash a string with seed |

### Implementation notes

- Pure Tin scalar path - no SIMD dependency.
- Implements the xxHash3 v0.8.x algorithm (192-byte secret, multiple
  length-specific dispatch paths).
- Passes all reference test vectors from `xxhsum -H3`.
- Not suitable for security-sensitive use cases.
