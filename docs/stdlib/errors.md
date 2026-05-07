# Errors

`stdlib/errors` defines the `Err` trait that every error type in the Tin
stdlib implements, plus a string-backed default impl (`StringErr`) for
ad-hoc errors that do not warrant a typed kind ADT.

Errors flow through programs as `*Err` (pointer-to-trait) so the value
carries its concrete dispatch (the `message()` method comes from the
implementer) while `nil` cleanly denotes "no error" at function
boundaries.

## Import

```rust
use errors
```

---

## The `Err` trait

`Err` is the trait every error type in the stdlib implements. It has a
single virtual method:

```rust
trait Err =
  fn message(this Err) string = virtual
```

Functions that may fail return one of:

| Shape                | When to use it                                                  |
|----------------------|------------------------------------------------------------------|
| `(T, *Err)`          | Classic two-value form; `nil` = success                         |
| `Result[T, *Err]`    | Newer ADT form; `Ok(v)` / `Err(e)` discriminate explicitly      |
| `Result[T, FooError]`| Generic form bound to a concrete struct; downcast to inspect    |

The pointer indirection is what makes `nil` work - bare `Trait` values
in Tin are never nil. Always reach the trait through `*Err`.

```rust
fn open(path string) (*void, *errors::Err) =
  let fd = io::fopen(path, "r")
  if fd == nil:
    return (nil, errors::new("could not open: " ++ path))
  return (fd, nil)

let (fd, err) = open("config.txt")
if errors::is_err(err):
  echo "error: {(*err).message()}"
```

---

## `StringErr` - the default impl

`StringErr` is a no-frills `Err` whose body is a single string. Use it
for ad-hoc errors that do not need a typed kind ADT:

```rust
struct StringErr(Err) =
  msg string

  fn Err::message(this StringErr) string = return this.msg

fn parse_age(s string) (i64, *errors::Err) =
  let n = str::atol(s)
  if n < 0:
    return (0, errors::new("age must be non-negative"))
  return (n, nil)
```

`errors::new(msg)` heap-allocates a `StringErr` and returns it widened
to `*Err`.

---

## Domain errors - the typed pattern

For real domain errors, define a `data` ADT for the kinds and a struct
that wraps it and implements `Err`:

```rust
data FlagErrorKind =
  UnknownFlag(name string)
  MissingValue(name string)

struct FlagError(errors::Err) =
  _kind FlagErrorKind

  fn kind(this FlagError) FlagErrorKind = return this._kind

  fn errors::Err::message(this FlagError) string =
    return error_message(this._kind)

fn error_message(k FlagErrorKind) string =
  match k:
    case UnknownFlag(name):  return "unknown flag: --" ++ name
    case MissingValue(name): return "flag --" ++ name ++ " requires a value"
```

The pair gives you both ends of the spectrum:

- `Result[T, FlagError]` retains the concrete struct, so callers can
  pattern-match on `.kind()` directly inside the `Err` arm.
- The struct also widens to `*errors::Err` for any code that takes
  the trait pointer (logging helpers, generic fallbacks, etc.).
- `*errors::Err as *FlagError` recovers the concrete struct for
  inspection, with the `[-Wunguarded-trait-downcast]` warning making
  sure callers reach for the cast only inside an `if e is *FlagError:`
  guard.

See `docs/06-traits.md` for the complete trait-coercion / downcast
machinery.

---

## Functions

### `errors::new`

Heap-allocates a `StringErr{msg}` and returns it as `*Err`. The pointer
is non-nil; nil flows from explicit `return nil` paths.

```rust
return errors::new("connection refused")
```

### `errors::wrap`

Returns a fresh `*Err` whose message is `ctx: <inner>`. When `inner` is
nil, behaves as `new(ctx)`.

```rust
let inner = errors::new("disk full")
let e = errors::wrap(inner, "saving config")
echo (*e).message()      // "saving config: disk full"

let e2 = errors::wrap(nil, "standalone")
echo (*e2).message()     // "standalone"
```

### `errors::is_err`

Returns `true` when `err` is non-nil. The idiomatic guard pattern in
two-value-return code:

```rust
if errors::is_err(err):
  panic((*err).message())
```

### `errors::equals`

Returns `true` if both errors are nil, or both non-nil with identical
rendered messages. Returns `false` if only one is nil.

The comparison uses the trait's `message()` method, so two domain
errors of different concrete types compare equal when their formatted
messages match - useful for assertion-style checks. For identity-style
comparisons, hold onto the typed pointer instead.

```rust
let a = errors::new("timeout")
let b = errors::new("timeout")
errors::equals(a, b)         // true (same message)
errors::equals(a, nil)       // false
errors::equals(nil, nil)     // true
```

### `errors::message`

Convenience accessor: dereferences a `*Err` and returns its formatted
message, or `""` when the pointer is nil.

```rust
echo "fatal: {errors::message(err)}"
```

---

## Reference

| Symbol                              | Kind   | Description                                     |
|-------------------------------------|--------|-------------------------------------------------|
| `Err`                               | trait  | Error trait; `fn message(this Err) string`      |
| `StringErr`                         | struct | Default impl: just a `msg string`               |
| `new(msg string) *Err`              | fn     | Heap-allocate a `StringErr` and widen to `*Err` |
| `wrap(inner *Err, ctx string) *Err` | fn     | Prepend context to an error                     |
| `is_err(err *Err) bool`             | fn     | True when err is non-nil                        |
| `equals(a, b *Err) bool`            | fn     | True when both nil or same rendered message     |
| `message(err *Err) string`          | fn     | Render `(*err).message()` or `""` for nil       |
