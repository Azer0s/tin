# SIMD

`stdlib/simd` provides portable vectorized operations backed by a C shim. The
correct implementation is selected at compile time via arch-specific `//!+`
directives: SSE4.2 on x86-64, NEON on AArch64.

> **No extra flags are needed** when importing `simd` from Tin code - the
> arch-specific C file is included automatically.

---

## Import

```rust
use simd
```

---

## SIMD types

The language supports the following first-class vector types:

| Type | Element | Lanes | Width |
|------|---------|-------|-------|
| `u8x16` / `i8x16` | `u8` / `i8` | 16 | 128-bit |
| `u16x8` / `i16x8` | `u16` / `i16` | 8 | 128-bit |
| `u32x4` / `i32x4` | `u32` / `i32` | 4 | 128-bit |
| `u64x2` / `i64x2` | `u64` / `i64` | 2 | 128-bit |
| `f32x4` | `f32` | 4 | 128-bit |
| `f64x2` | `f64` | 2 | 128-bit |
| `u8x32` / `i8x32` | `u8` / `i8` | 32 | 256-bit |
| `u32x8` / `i32x8` | `u32` / `i32` | 8 | 256-bit |
| `f32x8` | `f32` | 8 | 256-bit |
| `f64x4` | `f64` | 4 | 256-bit |

### Literals

```rust
let v = f32x4{ 1.0, 2.0, 3.0, 4.0 }
let w = u32x4{ 1, 2, 3, 4 }
let b = u8x16{ 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15 }
```

### Lane access

```rust
let x f32 = v[0]    // extract lane 0
```

### Arithmetic operators

The standard arithmetic and bitwise operators work element-wise on SIMD types
with no wrappers needed:

```rust
let a = f32x4{ 1.0, 2.0, 3.0, 4.0 }
let b = f32x4{ 0.5, 0.5, 0.5, 0.5 }

let sum  = a + b     // f32x4{ 1.5, 2.5, 3.5, 4.5 }
let diff = a - b     // f32x4{ 0.5, 1.5, 2.5, 3.5 }
let prod = a * b     // f32x4{ 0.5, 1.0, 1.5, 2.0 }

let x = u32x4{ 0xFF, 0x0F, 0xF0, 0x00 }
let y = u32x4{ 0x0F, 0xFF, 0x00, 0xF0 }
let anded = x & y    // u32x4{ 0x0F, 0x0F, 0x00, 0x00 }
let ored  = x | y    // u32x4{ 0xFF, 0xFF, 0xF0, 0xF0 }
let xored = x ^ y    // u32x4{ 0xF0, 0xF0, 0xF0, 0xF0 }
```

---

## Functions

### `simd::splat` - broadcast scalar to all lanes

Each overload accepts a scalar of the matching element type and returns a
vector with all lanes set to that value:

```rust
let v = simd::splat(1.0 as f32)     // f32x4{ 1.0, 1.0, 1.0, 1.0 }
let b = simd::splat(0x42 as u8)     // u8x16{ 0x42, ... }
let w = simd::splat(0 as u32)       // u32x4{ 0, 0, 0, 0 }
```

| Overload | Returns |
|----------|---------|
| `splat(v u8)` | `u8x16` |
| `splat(v i8)` | `i8x16` |
| `splat(v u32)` | `u32x4` |
| `splat(v i32)` | `i32x4` |
| `splat(v u64)` | `u64x2` |
| `splat(v f32)` | `f32x4` |
| `splat(v f64)` | `f64x2` |

---

### `simd::loadu` - unaligned load from pointer

Loads a vector from a (possibly unaligned) memory address:

```rust
let data = s as *u8
let chunk u8x16 = simd::loadu(addr(data[i]))
```

| Overload | Returns |
|----------|---------|
| `loadu(ptr *u8)` | `u8x16` |
| `loadu(ptr *u32)` | `u32x4` |
| `loadu(ptr *f32)` | `f32x4` |

---

### `simd::storeu` - unaligned store to pointer

```rust
simd::storeu(addr(data[i]), v)
```

| Overload | Stores |
|----------|--------|
| `storeu(ptr *u8, v u8x16)` | 16 bytes |
| `storeu(ptr *u32, v u32x4)` | 16 bytes |
| `storeu(ptr *f32, v f32x4)` | 16 bytes |

---

### `simd::cmpeq` - element-wise equality

Returns a mask vector where matching lanes are all-ones (`0xFF` / `0xFFFFFFFF`)
and non-matching lanes are zero:

```rust
let needle = simd::splat(@'x')
let chunk  = simd::loadu(addr(data[i]))
let eq     = simd::cmpeq(chunk, needle)   // u8x16 mask
```

| Overload | Returns |
|----------|---------|
| `cmpeq(a u8x16, b u8x16)` | `u8x16` |
| `cmpeq(a u32x4, b u32x4)` | `u32x4` |
| `cmpeq(a f32x4, b f32x4)` | `f32x4` |

---

### `simd::movemask` - collapse MSBs to integer bitmask

Extracts the most significant bit of each byte lane and packs them into a `u32`.
For a `u8x16` input, bits 0-15 of the result correspond to lanes 0-15:

```rust
let mask u32 = simd::movemask(eq)
if mask != 0:
  // at least one lane matched
```

| Overload | Returns |
|----------|---------|
| `movemask(a u8x16)` | `u32` (bits 0-15 used) |

---

### `simd::hadd` - horizontal add

Sums all lanes into a single scalar:

```rust
let sum u32 = simd::hadd(simd::splat(1 as u32))   // 4
let fsum f32 = simd::hadd(f32x4{ 1.0, 2.0, 3.0, 4.0 })   // 10.0
```

| Overload | Returns |
|----------|---------|
| `hadd(a u32x4)` | `u32` |
| `hadd(a f32x4)` | `f32` |

---

### `simd::dot` - dot product

```rust
let d f32 = simd::dot(a, b)
```

| Overload | Returns |
|----------|---------|
| `dot(a f32x4, b f32x4)` | `f32` |

---

### `simd::rotate_left` - rotate 32-bit lanes left

```rust
let r u32x4 = simd::rotate_left(v, 13)
```

| Overload | Returns |
|----------|---------|
| `rotate_left(a u32x4, n i32)` | `u32x4` |

---

## Arch-specific build directives

To include platform-specific C files in your own module use the `[arch]`
qualifier on `//!+` directives:

```
//!+my_x86.c  [x86_64] -- -msse4.2
//!+my_neon.c [aarch64]
//!-lfoo      [linux]
```

Supported arch tokens:

| Token | Condition |
|-------|-----------|
| `x86_64` | `GOARCH == "amd64"` |
| `aarch64` | `GOARCH == "arm64"` |
| `darwin` | `GOOS == "darwin"` |
| `linux` | `GOOS == "linux"` |

Multiple tokens separated by commas are ANDed: `[aarch64,darwin]` matches
Apple Silicon only.

---

## Typical usage pattern

```rust
use simd

fn search_byte(s string, needle byte) i64 =
  let n = len(s)
  let data = s as *u8
  let v_needle = simd::splat(needle)
  let i i64 = 0
  for i + 16 <= n:
    let chunk = simd::loadu(addr(data[i]))
    let eq    = simd::cmpeq(chunk, v_needle)
    let mask  = simd::movemask(eq)
    if mask != 0:
      // find the first set bit (position of first match)
      let j i64 = 0
      for j < 16:
        if (mask >> (j as u32)) & (1 as u32) != 0:
          return i + j
        j = j + 1
    i = i + 16
  // scalar tail
  for i < n:
    if data[i] == needle:
      return i
    i = i + 1
  return -1
```
