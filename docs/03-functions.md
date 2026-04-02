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

---

## Single-expression functions

When the body is a single expression the block can be written inline:

```rust
fn square(n i64) i64 = return n * n
```

---

## Recursion

Tin supports direct recursion without any special annotation:

```rust
fn factorial(n i64) i64 =
  if n <= 1:
    return 1
  return n * factorial(n - 1)
```

---

## where-clause style (pattern-matching functions)

See also [02 - Control Flow](02-control-flow.md#where---pattern-matching-on-function-arguments).

```rust
fn gcd(a i64, b i64) i64 =
  where b == 0: a
  where _: gcd(b, a % b)
```

---

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

---

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

---

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

---

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

---

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

---

## Variadic functions

The last parameter can be variadic with `...`:

```rust
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)
```

Variadic parameters are typically used with `extern` C functions.

---

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

---

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
|-------------------------------------|---------------------------|
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

---

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

---

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

---

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
