# 04 – Collections

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
let r = 1..10   // values 1, 2, …, 9

for let i i64 in 1..6:
  echo "{i}"    // prints 1 through 5
```

Ranges are most commonly used with `for … in` loops.

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
