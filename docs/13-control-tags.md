# 12 - Control Tags

Control tags annotate functions, methods, lambdas, macros, blocks, and
structs with compile-time constraints and properties. The compiler either
enforces them or uses them as documentation.

**Syntax:** `fn{#tag} name(...)` or `struct{#tag} Name = ...`  -  one or
more tags in braces after the keyword. Struct tags additionally accept a
member-scope qualifier (`#tag@fn`, `#tag@field`, etc.) that propagates
the tag to matching struct members; see [Struct tags](#struct-tags).

**Naming convention:** all lowercase with underscores. Tags describe what a
thing *is* or *has*, not what it *cannot* do.

---

## Function / method / lambda tags

### `#pure`

The function has no observable side effects: no I/O, no calls to side-effectful
functions, and no direct extern calls.

**Enforcement:** the compiler walks the complete (transitive) call graph and
rejects any path that reaches an `echo`, a `#sideffect` function, or an extern
function.

```rust
fn{#pure} square(n i64) i64 = return n * n

fn{#pure} dist_sq(x i64, y i64) i64 =
  return square(x) + square(y)   // OK  -  square is also pure

fn{#pure} bad() i64 =
  echo "oops"   // ERROR: #pure violation  -  echo is a side effect
  return 1
```

Transitivity means the check follows calls through helper functions:

```rust
fn helper() =
  echo "I/O here"

fn{#pure} indirect() i64 =
  helper()    // ERROR: helper reaches echo, so this is a violation too
  return 0
```

Mutual recursion between `#pure` functions is allowed:

```rust
fn{#pure} is_even(n i64) bool =
  where n == 0: true
  where _: is_odd(n - 1)

fn{#pure} is_odd(n i64) bool =
  where n == 0: false
  where _: is_even(n - 1)
```

---

### `#sideffect`

Declares that the function has observable side effects (I/O, mutations of
external state, non-determinism). Calling a `#sideffect` function from a
`#pure` function is a compile error.

```rust
fn{#sideffect} log(msg string) =
  echo "[log] {msg}"

fn{#pure} compute(n i64) i64 =
  log("computing")   // ERROR: calls #sideffect function "log"
  return n * 2
```

**Extern functions are automatically tagged `#sideffect`.** You do not need to
add the tag manually:

```rust
fn c_printf(fmt *char, ...) i32 = extern("printf")  // #sideffect applied automatically

fn{#pure} safe() i64 =
  c_printf("hi")   // ERROR: extern functions are always side-effectful
  return 1
```

`#sideffect` functions may freely call other `#sideffect` functions:

```rust
fn{#sideffect} log_prefix(prefix string, msg string) =
  echo "[{prefix}] {msg}"

fn{#sideffect} info(msg string) =
  log_prefix("INFO", msg)   // OK
```

---

### `#no_recurse`

The function must not call itself, directly or indirectly through any chain
of helpers.

**Enforcement:** the compiler performs a transitive AST-level call-graph walk
before code generation and rejects any path that leads back to the tagged
function.

**Exception:** functions tagged with both `#pure` and `#no_recurse` are CTFE
macros. Their self-calls are resolved at compile time by the CTFE evaluator,
so the runtime recursion check is skipped for them.

```rust
fn{#no_recurse} fib(n i64) i64 =
  if n <= 1:
    return n
  return fib(n - 1) + fib(n - 2)   // ERROR: #no_recurse violation

// Correct: use an iterative implementation instead
fn{#no_recurse} fib_iter(n i64) i64 =
  let a i64 = 0
  let b i64 = 1
  for let i i64 = 0; i < n; i++:
    let t i64 = a + b
    a = b
    b = t
  return a
```

Indirect recursion through helper functions is also caught:

```rust
fn step_a(n i64) i64 = return target(n - 1)   // calls back into target

fn{#no_recurse} target(n i64) i64 =
  if n <= 0:
    return 0
  return step_a(n)   // ERROR: step_a -> target is indirect recursion
```

Helpers that recurse *among themselves* without calling back into the tagged
function do not trigger the check:

```rust
fn countdown_a(n i64) i64 = return countdown_b(n - 1)
fn countdown_b(n i64) i64 = return countdown_a(n - 1)  // mutual, but not back to use_it

fn{#no_recurse} use_it(n i64) i64 =
  return countdown_a(n)   // OK  -  no path from countdown_a/b back to use_it
```

---

### `#no_thread`

The function is not safe to call concurrently from multiple threads.
Advisory only  -  not enforced at compile time.

```rust
fn{#no_thread} init_globals() =
  // ... initialises non-thread-safe state
```

---

### `#interop` <a id="interop"></a>

Marks a function as C-callable. The compiler emits a wrapper under
the bare function name (the Tin entry point gets a hidden mangled
symbol) that lazy-initialises the runtime, marshals C ABI types to
Tin types, calls the entry point, and marshals the return value back
to C. See [08 - C Interop](08-interop.md#calling-tin-from-c-interop)
for the full type-mapping table, allocator hook, and end-to-end build
recipe.

```rust
fn{#interop} add(a i32, b i32) i32 = return a + b
fn{#interop} greet(name string) string = return "hello, " ++ name
fn{#interop} sum(xs [i32]) i32 =
  let t i32 = 0
  for let v i32 in xs:
    t += v
  return t
```

```c
int32_t      add(int32_t a, int32_t b);
const char  *greet(const char *name);
int32_t      sum(const int32_t *xs, int64_t xs_len);
```

**Restrictions** (rejected at compile time):

- Cannot also be `#async` - C cannot drive a coroutine.
- Return type must not contain `Future[T]`.
- No parameter type may contain `any`.
- Cannot be a generic function (no concrete C symbol exists).
- Cannot be a struct method (top-level functions only in v1).
- Cannot be `extern` (already C, no wrapper needed).
- Cannot be named `main` (would clobber the binary's entry point).
- Two `#interop` functions sharing a name are rejected at the
  declaration site rather than waiting for the linker to complain.

**Allowed parameter types**: primitives, pointers, `string`, fat array
`[T]`, plus packed cLayoutStructs through pointers.

**Allowed return types**: primitives, pointers, packed cLayoutStructs
through pointers, `string` (via the `tin_extern_alloc` callback), and
fat array `[T]` (reshaped to status return + out-params).

**Strings and arrays returned to C** are copied via the user's
`tin_set_extern_alloc` callback (default `malloc`). The C caller owns
the buffer and frees it with whatever pairs with that allocator.

**Spawning fibers** is allowed. The wrapper does not wait for them;
spawned fibers run on the runtime worker pool and may outlive the
wrapper. Use `await` if you need to block until completion.

**Header generation**: pass `--emit-header=foo.h` to `tin build`. The
generator emits include guards, an `extern "C"` block, the allocator
typedef + setter, and one prototype per `#interop` function with the
original Tin signature in a leading comment.

---

### `#heavy`

Forces the function to be classified as "auto-yield" by the compile-time
heuristic pass. Any caller compiled as a `$coro` (fiber) variant will emit a
`coro.suspend` before each call to this function.

Use this when the compiler's complexity score falls below the auto-heavy
threshold but the function is known to be expensive in practice:

```rust
fn{#heavy} custom_encoder(data string) string =
  // hand-rolled encoding; loop score below threshold but call is costly
  ...
```

Without the tag, the compiler auto-classifies functions based on their loop
count, allocation count, and whether they call other heavy/recursive functions.
See `docs/internals/codegen-auto-yield.md` for the full scoring formula.

Inspect the compiler's classification with the `-v-heuristics` flag:

```sh
tin run myfile.tin -v-heuristics     # prints per-function labels to stderr
```

---

### `#no_autoyield`

Disables **all** automatic yield point insertion inside this function's `$coro`
variant: both loop backedge yields and call-site yields before heavy/recursive
callees.

```rust
fn{#async #no_autoyield} tight_inner(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // no yield at loop backedge
  return sum + fib(n)   // no yield before fib even though fib is recursive
```

Use this for innermost compute loops where the yield overhead is measurable.
The sync variant of the function is never affected (it never yields regardless).

Multiple tags are space-separated: `fn{#async #no_autoyield}`.

---

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

## Semantics and known limitations

### `#pure` enforcement

The compiler follows the full call graph reachable from a `#pure` function and
rejects any call whose purity cannot be fully verified:

**Function pointer / indirect calls**  -  calling through a function-typed
variable is rejected, because the target cannot be statically verified:

```rust
fn{#pure} bad(f fn(i64) i64, n i64) i64 =
  return f(n)   // ERROR: indirect call through function pointer is not verifiable
```

**Package / module calls**  -  calls qualified with `::` (e.g. `math::sqrt`,
`assert::equals`) are rejected, because external package functions are not in
the compiler's function registry and cannot be transitively checked:

```rust
use math
fn{#pure} bad(n f64) f64 =
  return math::sqrt(n)   // ERROR: cannot verify purity of package call "math::sqrt"
```

**Known-pure built-ins**  -  a small set of built-in functions (`len`, `sizeof`,
`default`, `typeof`, `traitof`, `fieldnames`, `fieldtypes`, `fieldtag`,
`getfield`) are always allowed inside `#pure` functions.

**Method call resolution**  -  when a `#pure` function calls a method via
`obj.method()`, the compiler performs a suffix search across all registered
struct methods named `method`. If *any* matching struct method is `#sideffect`
or extern, the call is rejected. Unknown methods (from external types) are
rejected because their purity cannot be verified.

### `#no_recurse` catches all recursive paths

`#no_recurse` performs a transitive AST-level call-graph walk, similar to
`#pure`. Any path through any number of helper functions that leads back to the
tagged function is a violation. Indirect calls through function pointers cannot
be statically resolved and are conservatively allowed (not traced).

Functions tagged `#pure #no_recurse` are exempt: they are CTFE macros whose
self-calls are evaluated at compile time rather than emitted as runtime calls.

---

## Quick reference

| Tag                | Applies to              | Enforced                         | Meaning                                      |
|--------------------|-------------------------|----------------------------------|----------------------------------------------|
| `#pure`            | fn / method / lambda    | Yes  -  transitive AST walk      | No side effects                              |
| `#sideffect`       | fn / method / lambda    | No (declaration)                 | Has side effects; auto-applied to extern fns |
| `#no_recurse`      | fn / method / lambda    | Yes  -  transitive AST walk      | Must not call itself (at any depth)          |
| `#no_thread`       | fn / method / lambda    | No (advisory)                    | Unsafe for concurrent use                    |
| `#allow_sideffect` | block                   | Yes  -  suppresses `#pure` check | Permits side effects in this block           |
| `#no_excl`         | macro                   | Parser                           | Callable without `!` suffix                  |
| `#no_parens`       | macro                   | Parser                           | Callable without parentheses                 |
| `#async`           | fn / method / lambda    | No (enables fiber codegen)       | Runs as a cooperative green thread (fiber)   |
| `#no_autoyield`    | fn / method / lambda    | No (disables codegen)            | Suppresses auto-yield at loop backedges and call sites |
| `#heavy`           | fn / method             | No (changes heuristic)           | Forces "auto-yield" classification; callers in `$coro` yield before calling |
| `#handover`        | fn (extern only)        | No (changes codegen)             | Transfers ownership of returned C pointer into ARC |
| `#interop`         | top-level fn            | Yes  -  signature whitelist      | Emits a C-callable wrapper alongside the Tin entry point |
| `#packed`          | struct (unscoped)       | Yes (layout)                     | Fields laid out contiguously, no alignment padding |
| `#<tag>@fn`        | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every method (instance + static) |
| `#<tag>@method`    | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every instance method |
| `#<tag>@static_fn` | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every `static fn`     |
| `#const@field`     | struct (scoped)         | Yes (default-flip)               | Unmarked fields become `const` by default; `var` opts out |
