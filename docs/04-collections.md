# 04 - Collections

## Arrays

An array is a dynamically sized sequence of elements. The type is written
`[ElementType]`:

```rust
let nums   [i64]    = [1, 2, 3, 4, 5]
let names  [string] = ["Alice", "Bob", "Carol"]
let empty  [i64]    = []
```

### Element access

```rust
let x = nums[0]    // 1
let y = nums[4]    // 5
```

### Array append (`++=`)

`++=` appends a single element to an array variable in place:

```rust
let res [i64] = []
res ++= 1
res ++= 2
res ++= 3
// res is now [1, 2, 3]
```

### Array concatenation (`++`)

`++` creates a new array by joining two arrays or an array and an element:

```rust
let a = [1, 2, 3]
let b = [4, 5]
let c = a ++ b     // [1, 2, 3, 4, 5]

let s = "Hello" ++ ", world!"   // string concatenation works the same way
```

### Iterating arrays

```rust
let items [string] = ["alpha", "beta", "gamma"]
for let s string in items:
  echo s
```

### Array length

Use the `len` builtin or the `.len` field shorthand:

```rust
let n = len(nums)     // builtin call
let m = nums.len      // equivalent field access
```

Both return `i64`. The `.len` form works on strings too:

```rust
let s = "hello"
echo s.len    // 5
```

### Implementation note

Arrays are fat pointers `{ i8* data, i64 len, i64 cap }`. The `len` builtin
simply reads the second word; it compiles to a single load with no function
call overhead.

### Array destructuring

Array destructuring binds multiple variables from an array in a single `let`
statement.

#### Uniform typed destructuring

All slots have the same element type:

```rust
let arr [i64] = [10, 20, 30]
let [a, b] [i64] = arr
// a == 10, b == 20
```

#### Per-slot typed destructuring from `[any]`

When different slots hold different types (stored in a `[any]` array), list
the types inside the brackets. A **runtime bounds check** is performed:

```rust
let arr [any] = [42 as any, true as any]
let [n, flag] [i32, bool] = arr
// n == 42 (i32), flag == true (bool)
```

If the array has fewer elements than needed, the program panics at runtime.

#### Rest split

Capture the first element and the remaining slice:

```rust
let arr [i64] = [1, 2, 3, 4]
let [x, ...xs] [i64] = arr
// x == 1, xs == [2, 3, 4]
```

Only the two-name form `[first, ...rest]` is supported. The rest variable
holds a sub-slice (a fresh copy) of the remaining elements.

#### Named type alias for per-slot destructuring

Use `@[T1, T2, ...]` to define a named type alias for a per-slot destructuring
pattern, then reference it by name:

```rust
type coord = @[i32, bool]

let arr [any] = [10 as any, false as any]
let [n, flag] coord = arr
// n == 10 (i32), flag == false (bool)
```

### Destructuring in loops

Array destructuring can be combined with loop iteration:

```rust
let pairs [i64] = [1, 2, 3, 4, 6, 8]

for let i i64 = 0; i < len(pairs); i += 2:
  let a i64 = pairs[i]
  let b i64 = pairs[i + 1]
  echo "{a} + {b} = {a + b}"
```

### Nested struct field access

Struct fields that are themselves structs can be accessed with chained `.`:

```rust
struct vec2 = x f64; y f64
struct rect = origin vec2; size vec2

let r = rect{origin: vec2{x: 1.0, y: 2.0}, size: vec2{x: 5.0, y: 3.0}}
echo r.origin.x    // 1
echo r.size.y      // 3
```

Struct destructuring extracts the direct fields of the outer struct. To work
with inner fields you access them through the extracted variable:

```rust
let {origin, size} rect = r
echo origin.x      // 1
echo size.y        // 3
```

See [`examples/nested_destructuring.tin`](../examples/nested_destructuring.tin)
for a complete runnable example.

### Generic collections

Arrays are fully generic. Any type parameter `t` can be used as the element
type:

```rust
fn sum[t](list [t]) t =
  let acc t = 0
  for let x t in list:
    acc = acc + x
  return acc
```

---

## Ranges

The `..` operator creates a half-open integer range `[start, end)`:

```rust
let r = 1..10   // values 1, 2, ..., 9

for let i i64 in 1..6:
  echo "{i}"    // prints 1 through 5
```

Ranges are most commonly used with `for ... in` loops.

---

## Fixed-size arrays

A fixed-size array is written `[Type; N]`:

```rust
let buf = malloc(10 * sizeof(*char)).([char; 10])
```

Fixed-size arrays are mostly used in low-level / `extern` code.

---

## Strings as arrays

`string` is internally `[char]` (an array of `u8`). You can use array
indexing and iteration on strings:

```rust
let hello = "hello"
let first_char = hello[0]   // 'h' as u8

for let c char in hello:
  echo "{c}"
```

String literals can be concatenated with `++`:

```rust
let a = "Hello"
let b = a ++ ", world!"
```
