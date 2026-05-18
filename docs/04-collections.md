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

### Array concat-assign (`++=`)

`++=` extends an array variable in place with every element of another
slice of the same element type.  The right-hand side must be a `[T]`
matching the left -- to append a single value, wrap it as a one-element
slice:

```rust
let res [i64] = []
res ++= [1]            // append one element
res ++= [2, 3]         // append two elements
// res is now [1, 2, 3]

let more = [4, 5]
res ++= more           // concat with another slice  -> [1, 2, 3, 4, 5]
```

Passing a bare element (`res ++= 1`) is a compile error.  The diagnostic
points to the wrap fix:

```
error: `++=` expects a `[i64]` on the right-hand side; got i64.
       `++=` is concat-assign (not append-one) -- to append a single
       value, wrap it: `xs ++= [v]`
```

The strict shape catches a class of silent bugs (e.g. `xs ++= [v]` on
`xs : [any]` used to box the whole `[v]` slice as one `any` element,
leaking memory and giving surprising semantics).  Concat-assign with
matching slice types has no such ambiguity.

### Array concatenation (`++`)

`++` creates a new array by joining two arrays of matching element
type.  Both sides must be slices of the same element type -- to
prepend or append a single value, wrap it as a one-element slice
(symmetric with `++=`):

```rust
let a = [1, 2, 3]
let b = [4, 5]
let c = a ++ b          // [1, 2, 3, 4, 5]
let d = a ++ [99]       // [1, 2, 3, 99]
let e = [0] ++ a        // [0, 1, 2, 3]

let s = "Hello" ++ ", world!"   // string concatenation works the same way
```

Mismatched shapes produce a clear error:

```
error: `++` is slice concat: both sides must be the same slice
       type, got [i64] ++ i64. To prepend or append a single value,
       wrap it as a one-element slice: `[v] ++ xs` or `xs ++ [v]`
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

### Array shape (`nlen`)

For nested arrays, `nlen` returns the **size of each dimension** as an
`[i64]`. It descends through `arr[0]` at every level, so it captures
the canonical shape under the assumption that the array is
rectangular:

```rust
nlen([1, 2, 3])                        // [3]
nlen([[1, 2], [3, 4]])                 // [2, 2]
nlen([[[1, 2], [3, 4]], [[5, 6], [7, 8]]])  // [2, 2, 2]
nlen([])                               // [0]
```

`len(nlen(arr))` is the **rank** (number of dimensions). `nlen` is not
"how many dimensions" on its own; it's the per-dimension sizes.

When descent hits an empty layer the remaining inner dims are reported
as `0` (rather than dereferencing the null data pointer of the empty
array):

```rust
let empty [[i64]] = []
nlen(empty)                            // [0, 0]

let outer [[i64]] = [[]]
nlen(outer)                            // [1, 0]
```

`nlen` only samples `arr[0]` at each level, so jagged arrays report
the first row's shape:

```rust
nlen([[1, 2, 3], [4]])                 // [2, 3]  (not [2, ?])
```

If shape integrity matters, pair `nlen` with `nrect` (below).

### Rectangularity (`nrect`)

`nrect(arr) bool` walks every element at every interior depth and
returns `true` iff every sub-array shares its sibling's length.
Use it whenever you need the shape from `nlen` to be trustable —
BLAS-style matrix code, contiguous-buffer flattening, anything that
treats a `[[T]]` as a real rectangle:

```rust
nrect([[1, 2], [3, 4]])                // true
nrect([[1, 2, 3], [4]])                // false  (rows differ)
nrect([1, 2, 3])                       // true   (depth 1 is vacuous)
nrect([])                              // true   (no rows to compare)
nrect("hello")                         // true   (strings are flat)
```

`nrect` and `nlen` are complementary, not redundant:

| Builtin | Cost          | Reads             | Returns                  |
|---------|---------------|-------------------|--------------------------|
| `len`   | O(1)          | outer length      | `i64`                    |
| `nlen`  | O(depth)      | `arr[0]` chain    | `[i64]` per-dim sizes    |
| `nrect` | O(N elements) | every sub-array   | `bool`                   |

The usual pattern is:

```rust
if !nrect(matrix):
  panic("expected a rectangular matrix")
let shape = nlen(matrix)
let rows = shape[0]
let cols = shape[1]
```

For a rare opt-out — when you've just built the matrix yourself and
know it's rectangular — skip `nrect` and call `nlen` directly.

### Element type must match

An array of one element type cannot be silently passed where a different
element type is expected. The compiler rejects it with a message suggesting
the explicit cast:

```rust
fn consume(xs [i32]) void = ...

let wide [i64] = [100, 200, 300]
consume(wide)          // error: cannot pass [i64] where [i32] is expected;
                       //        use `arg as [i32]` to convert
consume(wide as [i32]) // ok: element-wise narrowing (truncates on overflow)
```

Array literals are context-sensitive: `consume([1, 2, 3])` works without any
cast because the literal is generated at the target element type directly.
The cast only applies when the source is a typed variable or expression.

`[T1] as [T2]` allocates a fresh buffer and converts each element (truncate
on narrowing, sign-extend on widening for signed sources). Same-width
conversions like `[i32] as [u32]` are a zero-cost pointer reinterpretation.

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
let arr [i64] = [10, 20]
let [a, b] [i64] = arr
// a == 10, b == 20
```

A fixed-length destructuring pattern requires the array length to match
exactly. `let [a, b] = arr` panics at runtime if `arr.len != 2`. To bind
the first N elements of a longer array, use a rest slot.

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

The rest slot **must bind at least one element**: `let [x, ...xs] = arr`
panics at runtime if `arr.len < 2`. The same rule applies to rest patterns
in `match` and `where` (see [02-control-flow](02-control-flow.md)). To
match an empty array use `[]`; to bind a singleton use `[x]`. The exhaustive
partition for a list of any length is `[]` + `[x]` + `[x, ...xs]`.

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
struct vec2 =
  x f64
  y f64

struct rect =
  origin vec2
  size   vec2

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

### Array slicing

A slice is a sub-view of an array created with `[start:end]` syntax.  It
returns a new fat-array `{T*, i64}` pointing into the original data:

```rust
let arr = [10, 20, 30, 40, 50]

let s1 = arr[1:]     // from index 1 to end   -> [20, 30, 40, 50]
let s2 = arr[:3]     // first 3 elements       -> [10, 20, 30]
let s3 = arr[1:4]    // indices 1..3 inclusive -> [20, 30, 40]
```

The `.len` field reflects the slice length:

```rust
echo s1.len   // 4
echo s3[0]    // 20
```

Both `start` and `end` are optional.  Omitting `start` defaults to `0`;
omitting `end` defaults to `arr.len`.

The slice shares the backing memory of the original array.  Mutations
through the slice pointer are visible in the original.  Appending (`++=`)
to a slice variable reallocates and does not affect the original.

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

`string` is internally a fat pointer over UTF-8 bytes (`{i8*, i64}`).
You can use array indexing and iteration:

```rust
let hello = "hello"
let first_char = hello[0]   // 104 -- ASCII 'h' as i8

for let b i8 in hello:
  echo "{b}"   // 104 101 108 108 111 (one per line)
```

For decoded Unicode codepoints (multi-byte UTF-8 sequences as a single
`rune`), use `for let r rune in s:` -- see "String iteration" in
[02-control-flow](02-control-flow.md). To render bytes as characters
in `echo`, cast the whole string to `[char]` first; that selects the
char-format dispatch for the array, see "Byte-array echo formats"
below.

String literals can be concatenated with `++`:

```rust
let a = "Hello"
let b = a ++ ", world!"
```

### Byte-array echo formats

`[byte]`, `[u8]`, and `[char]` are all the same underlying type (`{i8*, i64}`),
but `echo` formats each one differently based on the declared element type:

| Element type | `echo` output          | Per-element format |
|--------------|------------------------|--------------------|
| `[byte]`     | `[48 65 6c 6c 6f]`     | two-digit hex      |
| `[u8]`       | `[72 101 108 108 111]` | unsigned decimal   |
| `[char]`     | `[H e l l o]`          | character          |
| `string`     | `Hello`                | UTF-8 string       |

```rust
let s = "Hello"

let as_bytes = s as [byte]
echo as_bytes   // [48 65 6c 6c 6f]

let as_u8s = s as [u8]
echo as_u8s     // [72 101 108 108 111]

let as_chars = s as [char]
echo as_chars   // [H e l l o]
```

The format is chosen at compile time from the static type of the variable or
cast expression -- there is no runtime overhead.
