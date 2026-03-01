# 10 – Testing

Tin has built-in support for unit tests via `test` blocks and the `assert`
standard library. Tests are discovered and run by the `tin test` command.

---

## test blocks

A `test` block declares a named test case. The syntax is:

```rust
test "description" =
  // assertions and code
```

Test blocks look like functions but have no parameters or return type. They
live at the top level of a `.tin` file alongside regular declarations.

```rust
use assert

fn add(a i64, b i64) i64 =
  return a + b

test "addition is correct" =
  assert::equals(add(1, 2), 3)
  assert::equals(add(0, 0), 0)
  assert::equals(add(-1, 1), 0)

test "string concatenation" =
  assert::equals_str("foo" ++ "bar", "foobar")

test "boolean assertions" =
  assert::ok(1 == 1)
  assert::not_ok(1 == 2)
```

Test blocks are only compiled and executed when the file is run with
`tin test`. They are ignored during a normal `tin run` or `tin build`.

---

## Running tests

### Single file

```
tin test examples/test_example.tin
```

Each test block is executed in declaration order. The runner reports pass/fail
for each test and prints a summary at the end.

### Entire directory

```
tin test examples/
```

All `.tin` files in the directory are compiled and tested. Files that contain
no `test` blocks are still compiled (to check for errors) but produce no test
output.

---

## The `assert` stdlib

Import the assert package with `use assert`. All assertion functions are in
the `assert::` namespace.

### assert::equals

Asserts that two `i64` values are equal:

```rust
assert::equals(expected i64, actual i64)
```

```rust
assert::equals(add(2, 3), 5)
assert::equals(len([1, 2, 3]), 3)
```

### assert::equals_str

Asserts that two strings are equal:

```rust
assert::equals_str(expected string, actual string)
```

```rust
assert::equals_str(greet("world"), "Hello, world")
assert::equals_str("foo" ++ "bar", "foobar")
```

### assert::equals_f64

Asserts that two `f64` values are equal:

```rust
assert::equals_f64(expected f64, actual f64)
```

```rust
assert::equals_f64(3.14, compute_pi())
```

### assert::ok

Asserts that a boolean condition is `true`:

```rust
assert::ok(cond bool)
```

```rust
assert::ok(x > 0)
assert::ok(name != "")
```

### assert::not_ok

Asserts that a boolean condition is `false`:

```rust
assert::not_ok(cond bool)
```

```rust
assert::not_ok(list.len == 0)
assert::not_ok(1 == 2)
```

### assert::not_equals

Asserts that two `i64` values differ:

```rust
assert::not_equals(a i64, b i64)
```

```rust
assert::not_equals(result, 0)
assert::not_equals(1, 2)
```

### assert::fails

Unconditionally fails the current test with a message:

```rust
assert::fails(msg string)
```

```rust
assert::fails("this code path should not be reached")
```

---

## Failure behaviour

When an assertion fails:

- Inside `tin test`: the failure message is printed and the test runner moves
  on to the next test (via `longjmp` back to the runner). All subsequent
  assertions in the same test block are skipped.
- Outside `tin test` (standalone run): `exit(1)` is called immediately.

Failure messages include the expected and actual values.

---

## Full example

```rust
// test_example.tin  -  demonstrates test blocks and the assert library

use assert

fn add(a i64, b i64) i64 =
  return a + b

fn greet(name string) string =
  return "Hello, " ++ name

test "addition is correct" =
  assert::equals(add(1, 2), 3)
  assert::equals(add(0, 0), 0)
  assert::equals(add(-1, 1), 0)

test "string concatenation" =
  assert::equals_str(greet("world"), "Hello, world")
  assert::equals_str("foo" ++ "bar", "foobar")

test "boolean assertions" =
  assert::ok(1 == 1)
  assert::ok(2 > 1)
  assert::not_ok(1 == 2)

test "inequality check" =
  assert::not_equals(1, 2)
  assert::not_equals(42, 0)
```

Run it with:

```
tin test examples/test_example.tin
```

---

## Mixing tests and top-level code

A file can contain both top-level executable code and `test` blocks. The
top-level code runs unconditionally (in both `tin run` and `tin test`); test
blocks only run under `tin test`.

This allows example files to both demonstrate behaviour with `echo` output and
verify correctness with assertions:

```rust
use assert

fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10)    // always runs

test "fib values" =
  assert::equals(fib(0) as i64, 0)
  assert::equals(fib(1) as i64, 1)
  assert::equals(fib(10) as i64, 55)
```
