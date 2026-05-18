# Variadics, static methods, overloading, defer override

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
