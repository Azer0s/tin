# Errors

`stdlib/errors` provides a lightweight error type for propagating failures
without panicking. A nil `Err` means success.

## Import

```rust
use errors
use { Err } from errors   // bring the Err type alias into scope
```

---

## The `Err` type

`Err` is a type alias for `*Error`. Functions that can fail return it as the
last value of a tuple:

```rust
fn open(path string) (*void, Err) =
  let fd = io::fopen(path, "r")
  if fd == nil:
    return (nil, errors::new("could not open: " ++ path))
  return (fd, nil)

let (fd, err) = open("config.txt")
if errors::has(err):
  echo "error: {err.message()}"
```

`nil` always means no error. The `Error` struct has a single method:

| Method | Description |
|--------|-------------|
| `message() string` | Returns the error description |

---

## Functions

### `errors::new`

Creates a new `Err` with the given message:

```rust
return errors::new("connection refused")
```

### `errors::wrap`

Prepends context to an inner error's message. If `inner` is nil, acts as
`new(msg)`:

```rust
let inner = errors::new("disk full")
let e = errors::wrap(inner, "write failed")
echo e.message()   // "write failed: disk full"

let e2 = errors::wrap(nil, "standalone")
echo e2.message()  // "standalone"
```

### `errors::has`

Returns `true` when `err` is non-nil. The idiomatic guard pattern:

```rust
if errors::has(err):
  panic(err.message())
```

> **Note:** `is` is a reserved keyword in Tin (used for trait bounds), so the
> predicate is named `has`.

### `errors::equals`

Returns `true` if both errors are nil, or both are non-nil with the same
message. Returns `false` if only one is nil:

```rust
let a = errors::new("timeout")
let b = errors::new("timeout")
errors::equals(a, b)    // true

errors::equals(a, nil)  // false
errors::equals(nil, nil) // true
```

---

## Reference

| Symbol | Kind | Description |
|--------|------|-------------|
| `Error` | struct | Error value; use `*Error` / `Err` as the return type |
| `Err` | type alias | Alias for `*Error`; nil means no error |
| `new(msg string) Err` | fn | Create a new error |
| `wrap(inner Err, msg string) Err` | fn | Prepend context to an error |
| `has(err Err) bool` | fn | True when err is non-nil |
| `equals(a, b Err) bool` | fn | True when both nil or same message |
