# 03 – Functions

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

See also [02 – Control Flow](02-control-flow.md#where--pattern-matching-on-function-arguments).

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

## Closures

Functions are first-class values. An anonymous function (closure) is written
with `fn`:

```rust
let double = fn(x i32) i32 = return x * 2
let square = fn(x i32) i32 = return x * x

echo double(5)   // 10
echo square(4)   // 16
```

Closures **capture outer variables** by value:

```rust
let offset i64 = 10
let add_offset = fn(x i64) i64 = return x + offset

echo add_offset(5)   // 15
```

### Closure type syntax

The type of a function value is written as `fn(ParamTypes) RetType`:

```rust
fn apply(f fn(x i32) i32, n i32) i32 =
  return f(n)
```

For a function that returns a function:

```rust
fn compose(f fn(x i32) i32, g fn(x i32) i32) fn(i32) i32 =
  return fn(x i32) i32 = return f(g(x))

let double_then_inc = compose(inc, double)
echo apply(double_then_inc, 3)   // 7
```

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

A struct method can be declared `static` — it does not receive a `this`
parameter and is called as `StructName.methodName(args)`:

```rust
struct counter =
  value i64

  static fn new() counter =
    return counter{value: 0}

let c = counter.new()
```

See [05 – Structs](05-structs.md) for the full struct method system.
