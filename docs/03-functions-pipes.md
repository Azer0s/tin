# Higher-order functions and the pipe operator

## Higher-order functions (curried style)

A curried function returns a closure, which is common for `map`, `filter`,
and `reduce`:

```rust
fn filter[t](f fn(i t) bool) fn([t]) [t] =
  return fn(list [t]) [t] =
    let res [t] = []
    for let i t in list:
      if f(i):
        res ++= [i]
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

