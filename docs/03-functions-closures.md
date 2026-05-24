# Closures and lambdas

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

