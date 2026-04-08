# Strings

`stdlib/strings` provides higher-level string manipulation functions. All
functions are implemented in pure Tin.

Single-byte search operations (`index_of`, `count`, `contains`) use a
16-byte-at-a-time SIMD fast path on x86-64 (SSE4.2) and AArch64 (NEON), with
an automatic scalar fallback for any remaining tail bytes.

## Import

```rust
use strings
```

---

## Searching

### `strings::contains`

Reports whether `sub` appears anywhere in `s`:

```rust
strings::contains("hello world", "world")   // true
strings::contains("hello world", "xyz")     // false
```

### `strings::index_of`

Returns the byte index of the first occurrence of `sub` in `s`, or `-1`:

```rust
strings::index_of("abcabc", "bc")    // 1
strings::index_of("hello", "xyz")    // -1
strings::index_of("hello", "")       // 0
```

### `strings::last_index_of`

Returns the byte index of the last occurrence of `sub` in `s`, or `-1`.
Returns `len(s)` when `sub` is empty:

```rust
strings::last_index_of("abcabc", "bc")   // 4
strings::last_index_of("hello", "xyz")   // -1
```

### `strings::has_prefix`

Reports whether `s` begins with `prefix`:

```rust
strings::has_prefix("hello world", "hello")   // true
strings::has_prefix("hello world", "world")   // false
```

### `strings::has_suffix`

Reports whether `s` ends with `suffix`:

```rust
strings::has_suffix("hello world", "world")   // true
strings::has_suffix("hello world", "hello")   // false
```

### `strings::count`

Returns the number of non-overlapping occurrences of `sub` in `s`.
When `sub` is empty, returns `len(s) + 1`:

```rust
strings::count("aaa", "a")     // 3
strings::count("aaaa", "aa")   // 2  (non-overlapping)
strings::count("abc", "")      // 4
```

---

## Slicing

### `strings::substr`

Returns the bytes of `s` in the half-open range `[from, to)`:

```rust
strings::substr("hello", 1, 4)   // "ell"
strings::substr("hello", 0, 5)   // "hello"
strings::substr("hello", 2, 2)   // ""
```

---

## Transformation

### `strings::replace`

Returns a copy of `s` with every non-overlapping occurrence of `old` replaced
by `new_str`. If `old` is empty, `s` is returned unchanged:

```rust
strings::replace("foo bar foo", "foo", "baz")   // "baz bar baz"
strings::replace("aXbXc", "X", "")              // "abc"
```

### `strings::to_lower` / `strings::to_upper`

Convert ASCII letters. Non-ASCII bytes pass through unchanged:

```rust
strings::to_lower("Hello World")   // "hello world"
strings::to_upper("Hello World")   // "HELLO WORLD"
```

### `strings::repeat`

Returns a new string consisting of `n` copies of `s`:

```rust
strings::repeat("ab", 3)   // "ababab"
strings::repeat("x", 0)    // ""
```

---

## Trimming

All three functions remove ASCII whitespace (`' '`, `'\t'`, `'\n'`, `'\r'`):

```rust
strings::trim("  hello  ")        // "hello"
strings::trim_left("  hello  ")   // "hello  "
strings::trim_right("  hello  ")  // "  hello"
```

---

## Splitting and joining

### `strings::split`

Slices `s` into all substrings separated by `sep` and returns them as
`[string]`. If `sep` is empty, each byte becomes its own element:

```rust
strings::split("a,b,c", ",")     // ["a", "b", "c"]
strings::split("abc", "")        // ["a", "b", "c"]
strings::split("hello", ",")     // ["hello"]
strings::split(",a,", ",")       // ["", "a", ""]
```

### `strings::join`

Concatenates the elements of `parts`, placing `sep` between each pair:

```rust
let parts [string] = ["a", "b", "c"]
strings::join(parts, ", ")   // "a, b, c"
strings::join(parts, "")     // "abc"

let empty [string] = []
strings::join(empty, ",")    // ""
```

---

## Reference

| Function | Signature | Description |
|----------|-----------|-------------|
| `contains` | `(s, sub string) bool` | `sub` appears in `s` |
| `count` | `(s, sub string) i64` | non-overlapping occurrences |
| `has_prefix` | `(s, prefix string) bool` | `s` starts with `prefix` |
| `has_suffix` | `(s, suffix string) bool` | `s` ends with `suffix` |
| `index_of` | `(s, sub string) i64` | first occurrence, or -1 |
| `last_index_of` | `(s, sub string) i64` | last occurrence, or -1 |
| `substr` | `(s string, from, to i64) string` | bytes `[from, to)` |
| `replace` | `(s, old, new_str string) string` | replace all occurrences |
| `repeat` | `(s string, n i64) string` | `n` copies of `s` |
| `split` | `(s, sep string) [string]` | split on separator |
| `join` | `(parts [string], sep string) string` | join with separator |
| `to_lower` | `(s string) string` | ASCII lowercase |
| `to_upper` | `(s string) string` | ASCII uppercase |
| `trim` | `(s string) string` | strip leading+trailing whitespace |
| `trim_left` | `(s string) string` | strip leading whitespace |
| `trim_right` | `(s string) string` | strip trailing whitespace |
