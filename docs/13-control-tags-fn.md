# Function / method / lambda tags


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

Inspect the compiler's classification with the `-fdump-heuristics` flag:

```sh
tin run myfile.tin -fdump-heuristics     # prints per-function labels to stderr
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

