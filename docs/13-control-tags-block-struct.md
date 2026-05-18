# Block, struct, and macro tags

## Block tags

Block tags appear on a `{ #tag } { body }` construct. The body is a
brace-delimited block that inherits the tag's semantics.

### `#allow_sideffect`

Inside this block, calls to `#sideffect` functions (including `echo`) are
permitted even if the enclosing function is tagged `#pure`. Use this to
isolate a small side-effectful region inside an otherwise pure function.

```rust
fn{#pure} mostly_pure(n i64) i64 =
  { #allow_sideffect } {
    echo "debug: n = {n}"
  }
  return n * n

// The function is still considered pure by callers; the echo is an
// explicitly acknowledged exception.
```

Multiple statements can appear inside the block, and multiple blocks can
appear in the same function:

```rust
fn{#pure} logging_compute(n i64) i64 =
  { #allow_sideffect } {
    echo "start"
    echo "n = {n}"
  }
  let result i64 = n * n * n
  { #allow_sideffect } {
    echo "result = {result}"
  }
  return result
```

Calling a `#sideffect` function is also permitted inside the block:

```rust
fn{#sideffect} trace(msg string) = echo "[trace] {msg}"

fn{#pure} guarded(n i64) i64 =
  { #allow_sideffect } {
    trace("entering guarded")   // OK  -  inside allow_sideffect
  }
  return n * n
```

### `#unsafe`

Raw-pointer operations are rejected at compile time outside an `{#unsafe}`
block. The block opts the inner code in to two things:

- **Pointer arithmetic** - `ptr + n` / `ptr - n` (any `*T` plus an integer).
- **`addr(int_literal)`** - interpreting a constant integer as a raw address.

```rust
fn write_from(buf *byte, n i64) =
  { #unsafe } {
    let head = buf + 0
    let tail = buf + n
    // ... use head/tail ...
  }
```

The check is lexical: nested calls do not inherit the unsafe context. Each
function must declare its own `{#unsafe}` block where it does pointer work.

---

## Struct tags

Struct declarations accept control tags in `{#tag}` braces directly after
the `struct` keyword. Two shapes:

1. **Unscoped tags** - apply to the struct itself (layout, ABI).
2. **Scoped tags** with an `@scope` qualifier - propagate to matching
   members before any tag-consuming pass runs.

```rust
struct{#packed}                 raw_header = ...  // struct-level tag
struct{#pure@fn #const@field}   vec2       = ...  // scoped to methods and fields
```

### `#packed` (struct-level)

Fields are laid out with no padding. `sizeof(struct)` equals the sum of
field sizes. Use this for binary protocols, wire formats, or C ABI
compatibility with `__attribute__((packed))`.

```rust
struct{#packed} record =
  tag   u8
  value u32
// sizeof(record) = 5 (without packing: 8 due to u32 alignment)
```

The compiler emits `align 1` annotations on field loads/stores so
unaligned access stays correct on all targets.

### `#no_copy` (struct-level)

Reject value-form bindings of this struct anywhere code could later
copy them. The compiler errors at:

- `let b = a` where `a` is a value of the `#no_copy` struct.
- by-value function parameters and return types of the `#no_copy` type.
- struct fields whose type is the `#no_copy` value (would let a copy
  of the containing struct alias the cell).

The receiver `this` of a method on the same struct is exempt -- a
deinit on `this S` is not a copy, it is the unique owner about to be
torn down.

Pointer form `*S` of a `#no_copy` type is fine: pointer copies are
just retains, and the cell is freed on the last release.

Used by `rc::Cell[T]` so that the only way to hold the cell is via
`*Cell[T]`. See [stdlib/rc](stdlib/rc.md) for the wrapper this enables.

```rust
struct {#no_copy} Cell[T] = ...
let c = Cell[i64].alloc(7, dtor)   // *Cell[i64], OK
let v Cell[i64] = ...              // error: Cell is #no_copy
let d = c                          // *Cell[i64] copy: OK (retain)
```

### `#closed` (struct-level)

Reject struct-literal use (`S{...}`) outside the struct's own static
methods. External code is forced through a constructor; this lets the
type maintain invariants the struct-literal syntax would otherwise let
the user bypass.

```rust
struct {#closed} Cell[T] =
  ...
  static fn alloc(...) *Cell[T] = return &Cell[T]{...}  // OK, inside Cell

fn main() i64 =
  let c = Cell[i64]{...}   // error: Cell is #closed
  let c = Cell[i64].alloc(...)   // OK
  return 0
```

Often paired with `#no_copy` for refcounted handles where the only
correct constructor needs to do C-side allocation and register a
destructor.

### Scoped tag syntax

`#tag@scope` on a struct declaration applies `#tag` to every member
matching `scope`:

| Scope         | Members covered                                                    |
|---------------|--------------------------------------------------------------------|
| `@fn`         | every `fn` declared in the body - both instance and static methods |
| `@method`     | instance methods only (excludes `static fn`)                       |
| `@static_fn`  | `static fn` only                                                   |
| `@field`      | every declared field                                               |

### Tag-scope compatibility

| Tag               | `@fn` / `@method` / `@static_fn` | `@field` | struct-level (unscoped) |
|-------------------|----------------------------------|----------|-------------------------|
| `#pure`           | yes                              | error    | error                   |
| `#sideffect`      | yes                              | error    | error                   |
| `#no_recurse`     | yes                              | error    | error                   |
| `#no_thread`      | yes                              | error    | error                   |
| `#no_autoyield`   | yes                              | error    | error                   |
| `#heavy`          | `@fn`, `@method`                 | error    | error                   |
| `#async`          | `@fn`, `@method`                 | error    | error                   |
| `#handover`       | never (extern-only)              | error    | error                   |
| `#const`          | error                            | yes      | error                   |
| `#packed`         | error                            | error    | yes                     |

Combinations outside this matrix are rejected at the struct declaration
site. Examples:

```rust
struct{#pure@field} bad = ...    // ERROR: #pure does not apply to fields
struct{#packed@fn}  bad = ...    // ERROR: #packed is struct-level, not scoped
struct{#const@fn}   bad = ...    // ERROR: #const is a field tag
struct{#pure@blah}  bad = ...    // ERROR: unknown scope @blah
```

### `#pure@fn` - all methods must be pure

```rust
struct{#pure@fn} vec2 =
  const x f64
  const y f64

  fn magnitude(this vec2) f64 =           // implicitly #pure
    return sqrt(this.x * this.x + this.y * this.y)

  fn dot(this vec2, o vec2) f64 =         // implicitly #pure
    return this.x * o.x + this.y * o.y
```

The compiler enforces `#pure` on every propagated method using the same
transitive AST walk applied to hand-tagged `fn{#pure}`. Adding an echo
inside any method is a compile error.

### Member-level override (silent cascade)

An explicit tag on a member takes precedence over the scoped propagation
for that member only. The propagation is silently skipped. This keeps
the 95% case ceremony-free while letting exceptions opt out.

```rust
struct{#pure@fn} mixed =
  const label string

  fn id(this mixed) string =
    return this.label                     // inherits #pure

  fn{#sideffect} announce(this mixed) =
    echo this.label                       // #sideffect wins; #pure skipped
```

Conflict pairs that trigger the silent cascade:
- `(#pure, #sideffect)`
- `(#heavy, #no_autoyield)`

Extern methods carry an auto-`#sideffect`, so `#pure@fn` over an extern
method hits the same cascade and is silently skipped.

### `#const@field` - immutable-by-default fields <a id="const_field"></a>

`#const@field` flips the unmarked-field default from `var` to `const`.
Unmarked fields become immutable; `const` and `var` prefixes still work
and override the default per-field.

```rust
struct{#const@field} message =
        from   string      // const (inherited default)
        body   string      // const (inherited default)
  var   status i64         // explicit var - mutable
```

See [05 - Structs](05-structs.md#field-mutability---const--var) for the
detailed `const` field semantics (what counts as a write, interaction
with `setfield` and `&field`, etc.). `#const` is compile-time-only.

### Empty-match scopes

A scoped tag that matches zero members is not an error - it simply has
no effect today and will pick up future members if they are added.

```rust
struct{#pure@fn} empty =
  x i64
// no methods yet. #pure@fn is validated but propagates to nothing.
// Adding a method tomorrow inherits #pure automatically.
```

---

## Macro tags

Macro tags change how a macro is called at the call site.

### `#no_excl`

The macro can be called without the `!` suffix.

```rust
macro{#no_excl} double(x) = x * 2

echo double(5)    // OK  -  no ! required
echo double!(5)   // also OK
```

### `#no_parens`

The macro can be called without parentheses.

```rust
macro{#no_excl #no_parens} proc() =
  return `fn{#pure #no_recurse}`

proc add(a i64, b i64) i64 = return a + b
```

---

