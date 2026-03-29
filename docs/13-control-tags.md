# 12 - Control Tags

Control tags annotate functions, methods, lambdas, macros, and blocks with
compile-time constraints and properties. The compiler either enforces them or
uses them as documentation.

**Syntax:** `fn{#tag} name(...)`  -  one or more tags in braces after the keyword.

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

| Tag                | Applies to           | Enforced                         | Meaning                                      |
|--------------------|----------------------|----------------------------------|----------------------------------------------|
| `#pure`            | fn / method / lambda | Yes  -  transitive AST walk      | No side effects                              |
| `#sideffect`       | fn / method / lambda | No (declaration)                 | Has side effects; auto-applied to extern fns |
| `#no_recurse`      | fn / method / lambda | Yes  -  transitive AST walk      | Must not call itself (at any depth)          |
| `#no_thread`       | fn / method / lambda | No (advisory)                    | Unsafe for concurrent use                    |
| `#allow_sideffect` | block                | Yes  -  suppresses `#pure` check | Permits side effects in this block           |
| `#no_excl`         | macro                | Parser                           | Callable without `!` suffix                  |
| `#no_parens`       | macro                | Parser                           | Callable without parentheses                 |
| `#async`           | fn / method / lambda | No (enables fiber codegen)       | Runs as a cooperative green thread (fiber)   |
