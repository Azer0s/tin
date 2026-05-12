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

## Generic functions

Type parameters are declared in square brackets before the parameter list:

```rust
fn identity[t](x t) t =
  return x

echo identity(42)
echo identity("hello")
```

The compiler infers type arguments from the call-site arguments.

Multiple type parameters:

```rust
fn map[t, r](f fn(i t) r) fn([t]) [r] =
  return fn(list [t]) [r] =
    let res [r] = []
    for let i t in list:
      res ++= f(i)
    return res
```

### Type constraints

A type parameter can be constrained with a `where t is <constraint>` clause
after the parameter list. Only types satisfying the constraint may be used as
the type argument:

```rust
fn min[t](a t, b t) t where t is ord =
  if a < b: return a
  return b

fn contains[t](haystack [t], needle t) bool where t is comp =
  for let item t in haystack:
    if item == needle: return true
  return false
```

The two built-in constraints are:

| Constraint | Satisfied by | Operators available |
|------|-------|-----------|
| `ord` | All integer types (`i8`..`i128`, `u8`..`u128`), float types (`f32`, `f64`, `f128`), `byte`, `char` | `<`, `<=`, `>`, `>=` (and by inclusion all `comp` operators) |
| `comp` | Everything `ord` accepts, plus `string` and `bool` | `==`, `!=` |

`comp` is a superset of `ord`: every `ord` type also satisfies `comp`.

```rust
// sort needs only ordering
fn sort[t](list [t]) [t] where t is ord = ...

// equality check needs only comparability
fn index_of[t](list [t], val t) i64 where t is comp =
  for let i i64 in 0..len(list):
    if list[i] == val: return i
  return -1
```

A `where` clause can also name a user-defined trait:

```rust
fn print_all[t](items [t]) where t is display =
  for let item t in items:
    echo item.display()
```

### Boolean bound expressions

A bound is a boolean expression over trait checks, not just a single trait
name. The grammar per type parameter is:

```
bound := or
or    := and ('||' and)*
and   := unary ('&&' unary | '+' unary)*
unary := 'not' atom | atom
atom  := '(' bound ')' | <type-or-trait-name>
```

- `&&` / `||` combine constraints with the obvious semantics.
- `not <trait>` excludes types that satisfy the trait.
- `+` is legacy shorthand for `&&` (both `T is A+B` and `T is A && B` work).
- Each atom is checked against the enclosing type parameter - no need to
  repeat `T is` inside the expression.

```rust
fn max3[T](a T, b T, c T) T where T is ord && not bool = ...
fn num_op[T](x T) T  where T is i32 || i64        = ...
fn fancy[T](x T)     where T is ord && (not bool && not f64) = ...
```

Multiple type parameters can be bounded independently by separating with a
comma or a new `where`:

```rust
fn combine[A, B](a A, b B) where A is comp, B is comp = ...
fn combine[A, B](a A, b B) where A is comp where B is comp = ...
```

When a bound fails at monomorphization the compiler points at the clause
and names the failing sub-check:

```
.../foo.tin:7:23: fn sort_pair[bool]: type "bool" does not satisfy constraint
`where T is ord && not bool` (failing sub-check: `ord`)
```

### Bounds on type aliases

A generic `type` declaration can attach a `where` clause. It's checked
when the alias is used with a concrete type:

```rust
struct Pair[A, B] =
  a A
  b B

type StrPair[T] = Pair[string, T] where T is ord && not bool

let p = StrPair[i32]{a: "hi", b: 42}   // ok
let q = StrPair[bool]{a: "hi", b: true} // error: T=bool fails `not bool`
```

Generic aliases resolve to their underlying struct at instantiation -
`StrPair[i32]{...}` expands to `Pair[string, i32]{...}` at codegen after
the alias's bounds are satisfied. Both alias bounds and the underlying
struct's bounds are enforced (a bound failing on either side fires with
the same `failing sub-check` error shape).

### Constructor inference for generic structs

Explicit type arguments are optional on a struct literal when they can be
inferred from the field values. The compiler unifies each provided field
value's type against the declared field type to bind the template's type
parameters:

```rust
struct Box[T] =
  value T

let a = Box{value: 42 as i32}     // Box[i32] inferred
let b = Box{value: "hello"}       // Box[string] inferred
let c = Box[f64]{value: 3.14}     // explicit also works
```

Multi-parameter structs work the same way, with each type parameter bound
by the first field in the literal that mentions it:

```rust
struct Pair[A, B] =
  a A
  b B

let p = Pair{a: 1 as i32, b: "hi"}    // Pair[i32, string]
```

Inference requires every type parameter to be reachable from the provided
fields. If a parameter is ambiguous (never mentioned by a literal field)
you get the existing "unknown struct type" error and should add an
explicit `[T]` annotation. Bound checks fire against the *inferred* type:

```rust
struct Num[T] where T is i32 || i64 =
  value T

let n = Num{value: "x"}    // error: type "string" does not satisfy
                           // constraint `where T is i32 || i64`
```

--

## Closures / Lambdas

Functions are first-class values. An anonymous function (lambda/closure) is
written with `fn`:

```rust
let double = fn(x i64) i64 = return x * 2
let square = fn(x i64) i64 = return x * x

echo double(5)   // 10
echo square(4)   // 16
```

Closures **capture outer variables** by value:

```rust
let offset i64 = 10
let add_offset = fn(x i64) i64 = return x + offset

echo add_offset(5)   // 15
```

### Void lambdas (no return type)

A lambda with no return type discards its result. It is useful as a callback:

```rust
fn run(f fn()) = f()

let noop = fn() = pass
run(noop)

fn apply_void(f fn(i64), x i64) = f(x)

let printer = fn(x i64) =
  echo "value: {x}"
apply_void(printer, 42)
```

### Multiline lambda bodies

A lambda body can contain multiple statements in an indented block:

```rust
let clamp = fn(x i64) i64 =
  if x < 0 :
    return 0
  if x > 100 :
    return 100
  return x

echo clamp(-5)    // 0
echo clamp(50)    // 50
echo clamp(200)   // 100
```

Multiline lambdas can be passed directly as function arguments:

```rust
fn apply(f fn(i64) i64, x i64) i64 = return f(x)

let result = apply(fn(x i64) i64 =
  let doubled = x * 2
  let plus_one = doubled + 1
  return plus_one
, 5)
echo result   // 11
```

### Where-clause lambda bodies

A lambda body can use the `where` pattern-matching syntax:

```rust
let sign = fn(x i64) i64 =
  where x < 0: -1
  where x > 0: 1
  where _: 0

echo sign(-10)   // -1
echo sign(0)     // 0
echo sign(42)    // 1
```

The first parameter is used as the implicit match subject, mirroring how
`where` works in named functions.

### Immediate invocation

A lambda can be invoked immediately after it is defined by appending `(args)`:

```rust
let result = (fn() i64 = return 42)()
echo result   // 42

let doubled = (fn(x i64) i64 = return x * 2)(5)
echo doubled  // 10
```

This is sometimes useful for immediately-invoked computations or with `defer`:

```rust
defer (fn() void = cleanup())()
```

### Closure type syntax

The type of a function value is written as `fn(ParamTypes) RetType`:

```rust
fn apply(f fn(i64) i64, n i64) i64 =
  return f(n)
```

For a function that returns a function:

```rust
fn compose(f fn(i64) i64, g fn(i64) i64) fn(i64) i64 =
  return fn(x i64) i64 = return f(g(x))
```

--

## The `pass` keyword

`pass` is an explicit no-op statement. It is used to fill a block that would
otherwise be empty  -  in function bodies, lambda bodies, `if`/`else` branches,
and loop bodies:

```rust
// empty function body
fn do_nothing() =
  pass

// empty lambda
let noop = fn() = pass

// explicit empty else branch
if condition :
  do_something()
else :
  pass

// no-op loop
for let i i64 in 0..n :
  pass
```

`pass` has no runtime effect; the compiler simply ignores it.

--

## Higher-order functions (curried style)

A curried function returns a closure, which is common for `map`, `filter`,
and `reduce`:

```rust
fn filter[t](f fn(i t) bool) fn([t]) [t] =
  return fn(list [t]) [t] =
    let res [t] = []
    for let i t in list:
      if f(i):
        res ++= i
    return res

fn reduce[t, r](f fn(acc r, i t) r, init r) fn([t]) r =
  return fn(list [t]) r =
    let acc r = init
    for let i t in list:
      acc = f(acc, i)
    return acc
```

Usage:

```rust
let nums = [1, 2, 3, 4, 5]
let evens = nums |> filter(fn(i i64) bool = return i % 2 == 0)
let sum   = nums |> reduce(fn(acc i64, i i64) i64 = return acc + i, 0)
```

--

## The pipe operator `|>`

`x |> f` passes `x` as the argument to `f`. It is used to chain
function calls left-to-right without deeply nested calls:

```rust
let nums = [1, 2, 3, 4, 5, 6, 7]

let result = nums
  |> filter(fn(i i64) bool = return i % 2 == 0)
  |> map(fn(i i64) i64 = return i * i)
```

This is equivalent to `map(square)(filter(even)(nums))`, but reads in the
natural order of operations.

The pipe operator works with any single-argument function or curried function
that expects one more argument:

```rust
let sum = nums |> reduce(fn(acc i64, i i64) i64 = return acc + i, 0)
echo sum   // 15
```

### Pipe to a struct / ADT method

`x |> Type::method` resolves `method` as an instance method on `Type` and
calls it with `x` as the receiver -- the same thing `x.method()` does, but
in pipe form so it composes with other pipe stages.  Generic type
arguments are recovered from the LHS's compile-time type, so the bare
name works without spelling out `[T, U]` again:

```rust
use { Result } from result

fn parse(s string) Result[i64, errors::Err] = ...

let v = parse("42")
  |> Result::unwrap          // 42  -- bare form
  |> double                  // 84

// Explicit type-args are also accepted:
let w = parse("7") |> Result[i64, errors::Err]::unwrap   // 7
```

The same form works for trait methods on an ADT or struct.  When a method
is package-qualified (`pkg::Type::method`), the path goes through the
package alias just like any other scope reference -- so if you only did
`use { Type } from pkg` (selective import without the package alias), the
qualified form errors and you must write the bare `Type::method`; if you
imported both, the qualified form fires `-Wredundant-import-prefix`.

The receiver value is matched against the first parameter of the chosen
method, so a method taking `this *Foo` accepts a `*Foo` LHS, and a method
taking `this Foo` accepts a `Foo` LHS.

--

## Variadic functions

The last parameter can be variadic with `...`:

```rust
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)
```

Variadic parameters are typically used with `extern` C functions.

--

## Static methods

A struct method can be declared `static`  -  it does not receive a `this`
parameter and is called as `StructName.methodName(args)`:

```rust
struct counter =
  value i64

  static fn new() counter =
    return counter{value: 0}

let c = counter.new()
```

See [05 - Structs](05-structs.md) for the full struct method system.

--

## Function overloading

Multiple functions may share the same name if their parameter signatures differ
(by arity or by type). The compiler resolves the correct variant at each call
site by comparing the evaluated argument types.

### Overloading by type

```rust
fn describe(n i64) string =
  return "integer: {n}"

fn describe(s string) string =
  return "string: " ++ s

fn describe(b bool) string =
  return "bool: {b}"

echo describe(42)       // integer: 42
echo describe("hello")  // string: hello
echo describe(true)     // bool: true
```

### Overloading by arity

```rust
fn sum(a i64) i64 =
  return a

fn sum(a i64, b i64) i64 =
  return a + b

fn sum(a i64, b i64, c i64) i64 =
  return a + b + c

echo sum(7)       // 7
echo sum(1, 2)    // 3
echo sum(1, 2, 3) // 6
```

### Method overloading

Struct methods follow the same rules. The receiver (`this`) is not part of the
overload signature - only the explicit call-site arguments are compared.

```rust
struct Box =
  value i64

  fn set(this *Box, n i64) =
    this.value = n

  fn set(this *Box, s string) =
    this.value = len(s)   // store length of string

  fn scale(this Box) i64 =
    return this.value

  fn scale(this Box, factor i64) i64 =
    return this.value * factor

let b = Box{value: 0}
b.set(42)          // calls set(i64)
b.set("hello")     // calls set(string) - b.value becomes 5
echo b.scale()     // 5
echo b.scale(10)   // 50
```

### Resolution rules

1. **Exact match** - arity matches and every argument type equals the corresponding
   parameter type.  This is tried first.
2. **Arity match** - arity matches but types differ.  Used as a fallback when
   no exact match is found (e.g. when passing a numeric literal that could widen).

An error is reported at compile time if no variant matches the given arity.

### Name mangling

The compiler internally mangles overloaded names using the parameter-type
signature:

| Declaration                         | Internal IR name          |
|-------------------|--------------|
| `fn foo(n i64)`                     | `foo__i64`                |
| `fn foo(s string)`                  | `foo__string`             |
| `fn foo(n i64, s string)`           | `foo__i64__string`        |
| `fn foo()` (zero params)            | `foo__`                   |
| `fn (MyStruct) bar(n i64)`          | `MyStruct_bar__i64`       |
| `fn (MyStruct) bar(s string)`       | `MyStruct_bar__string`    |

Non-overloaded functions keep their plain names and are unaffected.

### Limitations

- Overloading is resolved within a single source file (and its directly imported
  packages).  Cross-package overloads are not supported.
- `extern` functions and constrained generic functions cannot be overloaded.
- The return type alone does **not** distinguish overloads - parameter types and
  arity are the only discriminators.

--

## Defer return value override

A `return` statement inside a `defer do:` block overrides the enclosing
function's return value. The deferred block runs as usual on exit, and if it
executes a `return`, the value replaces whatever the function was going to
return:

```rust
fn guarded() i64 =
  defer do:
    return 42   // always returns 42, regardless of what the function does
  return 1      // this is overridden

echo guarded()  // 42
```

If multiple defers override the return, LIFO order applies - the **last-registered**
defer that executes a `return` wins (because it runs last in LIFO order):

```rust
fn multi() i64 =
  defer do: return 1   // registered first -> runs last -> wins
  defer do: return 2   // registered second -> runs first
  return 0

echo multi()  // 1
```

--

### `defer (fn() *T = ...)()` - pointer-based override

An anonymous function deferred via `defer (fn() *T = ...)()` can conditionally
override the return value by returning a non-nil pointer:

- The lambda's return type must be `*T` where `T` is the enclosing function's
  return type.
- If the lambda returns a **non-nil** `*T`, the dereferenced value replaces the
  function's return value.
- If the lambda returns nil, the function's return value is left unchanged.

```rust
fn maybe_override(flag bool) i64 =
  let result i64 = 99
  defer (fn() *i64 =
    if flag:
      return &result   // override with result's current value
    return None        // no override
  )()
  return 1

echo maybe_override(true)   // 99  (overridden)
echo maybe_override(false)  // 1   (not overridden)
```

--

### Defer return with `recover()`

`return` in a `defer do:` that also calls `recover()` works correctly - the
override takes effect only when a panic was actually caught:

```rust
fn safe_call() i64 =
  defer do:
    let msg = recover()
    if len(msg) > 0:
      return -1    // override: signal error
  do_something_risky()
  return 0

echo safe_call()  // -1 if do_something_risky() panics, 0 otherwise
```
