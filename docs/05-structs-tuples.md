# Tuples


A **tuple** is an anonymous struct whose fields are named alphabetically
(`a`, `b`, `c`, ...). Tin provides built-in `Tuple` templates for arities
2 through 10.

### Type syntax

`(T1, T2)` is shorthand for `Tuple[T1, T2]`:

```rust
type int_pair = (i64, i64)
type result   = (i64, bool)
```

Use tuple types anywhere a type annotation is accepted: variable
declarations, function parameters, and return types.

### Literal syntax

Write `(e1, e2, ...)` to create a tuple value:

```rust
let t = (10, true)       // infers Tuple[i64, bool]
let p (i64, i64) = (3, 7)
```

### Function return

A function can declare a tuple return type and return a tuple literal
directly without naming a struct:

```rust
fn swap(x i64, y i64) (i64, i64) =
  return (y, x)

fn min_max(arr [i64]) (i64, i64) =
  // ...
  return (min, max)
```

### Field access

Tuple fields are accessed by name (`a`, `b`, `c`, ...):

```rust
let t = (42, true, 3)
echo t.a    // 42
echo t.b    // true
echo t.c    // 3
```

### Destructuring

`let (name1, name2, ...) = expr` unpacks a tuple into individual variables:

```rust
let (lo, hi) = min_max([5, 2, 9, 1])
echo lo   // 1
echo hi   // 9

let (x, y) = swap(3, 7)
echo x    // 7
echo y    // 3
```

### Tuples vs named structs

Use a tuple when the shape is transient (e.g., returning two values from a
function). Use a named struct when the type is reused, has meaningful field
names, or carries methods.

### Type aliases for tuples

A named type alias creates a distinct type with the same layout:

```rust
type point2d = (i64, i64)
let p point2d = (10, 20)
echo p.a   // 10
echo p.b   // 20
```
