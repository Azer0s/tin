# 03 - Functions

## Basic functions

```rust
fn add(a i64, b i64) i64 =
  return a + b

echo add(3, 4)   // 7
```

The return type follows the parameter list. `fn name(params) ReturnType =` is
the full form; the body is an indented block or a single expression.

A function with no return value omits the return type:

```rust
fn greet(name string) =
  echo "Hello, {name}!"
```

### Explicit returns required

Every non-void function must end with an explicit `return` on all reachable
paths. The compiler rejects functions where control can fall off the end
without returning:

```rust
fn sign(x i64) i64 =
  if x > 0:
    return 1
  // ERROR: fn sign: not all code paths return a value
```

The trailing expression of a block is **not** an implicit return. Use `return`:

```rust
fn sign(x i64) i64 =
  if x > 0:
    return 1
  return -1     // OK - all paths covered
```

If/else chains and `match` statements where every branch returns are
recognised as exhaustive - no extra `return` is needed after them:

```rust
fn sign(x i64) i64 =
  if x > 0:
    return 1
  else:
    return -1   // OK - both branches return; no fall-through possible
```

Void functions can use bare `return` to exit early:

```rust
fn greet(name string) =
  if len(name) == 0:
    return   // early exit; no value needed
  echo "Hello, {name}!"
```

--

## Single-expression functions

When the body is a single expression the block can be written inline:

```rust
fn square(n i64) i64 = return n * n
```

--

## Recursion

Tin supports direct recursion without any special annotation:

```rust
fn factorial(n i64) i64 =
  if n <= 1:
    return 1
  return n * factorial(n - 1)
```

--

## Tail call optimization (TCO)

When a function calls itself in tail position - the recursive call is the
very last operation before returning - Tin automatically eliminates the
recursive call and replaces it with a loop. No stack frame is allocated for
each recursive step, so tail-recursive functions run in O(1) stack space.

TCO applies automatically. There is no annotation required.

A call is in tail position when:

- it is the sole expression of a `where` clause: `where cond: fn(new_args)`
- it is the value of a `return` statement with no other work after it: `return fn(new_args)`

**Factorial with accumulator (tail-recursive):**

```rust
fn fact(n u64, acc u64) u64 =
  where n == 0: acc
  where _: fact(n - 1, n * acc)

fn factorial(n u64) u64 =
  return fact(n, 1)
```

**Greatest common divisor:**

```rust
fn gcd(a u64, b u64) u64 =
  where b == 0: a
  where _: gcd(b, a % b)
```

**Fibonacci with accumulator:**

```rust
fn fib_acc(n u64, a u64, b u64) u64 =
  where n == 0: a
  where _: fib_acc(n - 1, b, a + b)

fn fib_tco(n u64) u64 =
  return fib_acc(n, 0, 1)
```

### Eligibility

TCO is applied when all of the following hold:

- The function is sync (not `{#async}`).
- The function has no `defer` statements.
- No parameter is an RC-tracked type (`string`, `any`, arrays, function values).
- The body contains at least one direct self tail call.

If eligibility is not met (e.g. a parameter is a string, or `defer` is
present), the function is compiled normally with regular call frames.

### Non-tail recursion

A recursive call that is not in tail position is NOT optimized:

```rust
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)  // NOT tail - two calls, then addition
```

To get TCO for fibonacci, use the accumulator form (`fib_acc` above).

--

## where-clause style (pattern-matching functions)

See also [02 - Control Flow](02-control-flow.md#where--pattern-matching-on-function-arguments).

A `where`-bodied function dispatches on its arguments. Each clause chooses a
branch; the first matching clause runs.

### Bool-guard clauses

A bool expression selects the branch when truthy. `where _:` is the universal
catch-all. A where-list that relies on bool clauses must include `where _:`.

```rust
fn gcd(a i64, b i64) i64 =
  where b == 0: a
  where _: gcd(b, a % b)
```

### Pattern clauses

`where (pattern):` matches the function's argument(s) against a pattern. The
parentheses are required and distinguish pattern mode from bool mode.
Patterns may be literals, identifiers (binders), `_` (wildcard),
array-destructuring `[x, ...xs]`, and (for multi-arg functions) a tuple of
the above. Bindings introduced by a pattern are in scope for the clause body.

```rust
fn fib(n i32) i32 =
  where (0): 0
  where (1): 1
  where (n): fib(n - 2) + fib(n - 1)
```

Multi-argument functions use a tuple pattern; each slot corresponds to one
argument in order.

```rust
fn foo(a i32, b string) bool =
  where (0, _):       true
  where (1, "hello"): true
  where (_, _):       false
```

Array destructuring works exactly as in `match`. The rest slot in
`[x, ...rest]` always binds at least one element, so `[x, ...rest]` matches
lists of length >= 2. Use `[x]` for the singleton case explicitly:

```rust
fn sum(xs [i32]) i32 =
  where ([]):           0
  where ([x]):          x                       // singleton
  where ([x, ...rest]): x + sum(rest)           // length >= 2
  where _:              0
```

### Guards on patterns

A pattern may carry a postfix `if` guard (same syntax as `match case`). The
pattern's bindings are in scope for the guard.

```rust
fn sign(n i32) string =
  where (0):          "zero"
  where (n) if n < 0: "neg"
  where (n) if n > 0: "pos"
  where _:            "unreachable"
```

### Rules

- **No mixing.** A single where-list is either all bool clauses or all pattern
  clauses (bare `where _:` works in both). Mixing is a compile error pointing
  at the conflicting clauses and suggesting `where (pat) if cond:` as the fix.
- **Exhaustiveness.** Bool mode requires `where _:` because boolean
  predicates aren't structurally analysable. Pattern mode is checked by
  the Maranget exhaustiveness algorithm (Maranget, 2007 - see
  `codegen/maranget.go` for the citation): the canonical triple
  `where ([]): ... where ([x]): ... where ([x, ...xs]):` is recognised as
  covering every list, `where (true): ... where (false):` covers every
  bool, etc. - no explicit catch-all needed. When the patterns leave gaps,
  the compiler reports the missing case with a concrete witness:
  ```
  non-exhaustive where: no clause matches [_]; add the missing case or a
  catch-all `where _:`
  ```
  Guards make a pattern refutable, so a guarded clause never counts as a
  structural cover. Adding a `where _:` to an already-exhaustive list is
  not an error but produces an "unreachable where clause" warning
  (suppress with `-Wno-unused-match-arms`).
- **Arity match.** A single-arg function's clause patterns must each be a
  single pattern (no tuple). A multi-arg function's clause patterns must each
  be a tuple with exactly as many slots as the function has arguments.
- **Pattern binding shadows the arg name** for the clause scope. In the
  pattern `(n)` on a function whose argument is also `n`, the pattern rebinds
  it (same value); on a function whose argument is `a`, writing `where (n):`
  binds `n` and hides `a` in that clause.
- **Guard scope.** In `where (pat) if guard:`, the names bound by `pat` are
  in scope for both `guard` and the body. The same scoping rule as `match
  case <pat> if guard:`.
- **Struct patterns in `where` are not yet implemented** (planned). The
  compiler emits a clear "struct patterns in where-clauses are not yet
  supported (planned for slice 2); use `match` for now" diagnostic. Until
  then, dispatch on a struct-typed argument inside a regular `match`
  expression and use `where` for the bool/array/tuple/literal patterns it
  already supports.
- **`where ():` is a parse error.** Tin has no zero-ary unit value, so an
  empty pattern tuple is rejected at parse time with a hint to use
  `where _:` if a catch-all is what you want.

### Exhaustiveness algorithm and citation

Pattern-where exhaustiveness is decided by the algorithm from Luc Maranget,
"Warnings for pattern matching", *Journal of Functional Programming*,
vol. 17 no. 3, 2007, pp. 387-421, doi:10.1017/S0956796807006223
(http://moscova.inria.fr/~maranget/papers/warn/warn.pdf). The same
algorithm runs for `match` reachability warnings.

For debugging the analysis itself, build with `-fdump-match-info` and the
compiler will print, on stderr, the pattern matrix it built for every
`match` and `where`, the per-arm reachability marker (`ok` / `guarded` /
`UNREACHABLE`), and the final exhaustiveness verdict (`YES` or `NO ...
missing witness: <value>`). Pair with `-Wno-unused-match-arms` to silence
the user-facing warning while keeping the trace.

### Slice status (current)

Pattern-where ships with literal patterns (integer, float, string,
bool, atom), identifier binders, `_` wildcards, `ArrayPattern` (`[]`,
`[x]`, `[x, ...rest]`, `[_, ...]`, ...), and top-level `TuplePattern`
for multi-arg dispatch. Struct patterns (`Type{field: pat, ...}`) inside
`where` are recognised by the parser but rejected by codegen with the
diagnostic noted above; full struct support is a planned follow-up.

--


---

## See also

- [Generic functions](03-functions-generics.md)
- [Closures and lambdas](03-functions-closures.md)
- [Higher-order + pipe](03-functions-pipes.md)
- [Variadics, static methods, overloading, defer override](03-functions-overloading.md)
