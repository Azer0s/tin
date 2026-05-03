# 02 - Control Flow

## if / else

```rust
let x = 10

if x > 5:
  echo "big"

if x % 2 == 0:
  echo "even"
else:
  echo "odd"

if x < 0:
  echo "negative"
else if x == 0:
  echo "zero"
else:
  echo "positive"
```

Conditions do **not** use parentheses. Bodies are indented (Python-style).

---

## for loops

### C-style loop

```rust
for let i i32 = 0; i < 5; i++:
  echo "i = {i}"
```

### Range loop (exclusive upper bound)

```rust
for let j i32 in 1..6:     // 1, 2, 3, 4, 5
  echo "j = {j}"
```

The `..` operator creates an exclusive range. Both bounds must be integers.

### Collection loop

```rust
let items [string] = ["alpha", "beta", "gamma"]
for let s string in items:
  echo s
```

Any array can be iterated with `in`. The loop variable holds a *copy*
of each element -- assignments to it do not propagate back to the
source array.

### Mutating-loop with `for ref`

Use `for ref` when you want each iteration's variable to ALIAS the
source slot rather than hold a copy. Assignments to the variable
mutate the underlying array in place:

```rust
let xs = [1, 2, 3]
for ref x in xs:
  x += 10
echo xs   // [11, 12, 13]
```

Replacing an RC-tracked element (string, fat array, `any`, `*Struct`)
through `ref` releases the previous value and retains the new one,
so no leaks and no double-free regardless of how the array later
drops:

```rust
let labels = ["a", "b", "c"]
for ref s in labels:
  s = s ++ "!"
echo labels   // ["a!", "b!", "c!"]
```

`for ref` works the same way when the array lives behind any number
of indirections, e.g. as a struct field, returned from a function,
or both:

```rust
struct Holder = xs [i64]
fn make() Holder = return Holder{xs: [10, 20, 30]}

let h = make()
for ref x in h.xs:
  x += 1
echo h.xs   // [11, 21, 31]
```

`for ref` is rejected on inputs that don't have writable slots:

- Ranges (`for ref x in 0..5:`) -- a range produces values, not
  storage.
- `const` arrays (`const xs = [1,2,3]; for ref x in xs:`) -- the
  binding is immutable; drop the `const` to allow mutation.

### Const arrays cannot be element-assigned

`const xs[i] = ...` and `const xs[i] += ...` are also rejected at
compile time, mirroring the read-only-storage guarantee top-level
`const` already provides.

### String iteration

`for ... in string` iterates byte-by-byte, yielding each byte as an `i8`:

```rust
let s = "hello"
for let b i8 in s:
  echo "{b}"   // prints ASCII codes: 104, 101, 108, 108, 111
```

This is useful for byte-level string processing. For character code comparisons,
use `@'x'` literals:

```rust
let count i64 = 0
for let b i8 in "hello world":
  if b == @' ':
    count = count + 1
echo count   // 1  (one space)
```

---

## match

`match` dispatches on a value against a list of cases:

```rust
enum i32 direction =
  north: 0,
  south: 1,
  east:  2,
  west:  3,

fn direction_name(d direction) string =
  match d:
    case direction.north: return "north"
    case direction.south: return "south"
    case direction.east:  return "east"
    case direction.west:  return "west"
    default:              return "unknown"
```

- Each arm starts with `case <value>:`.
- `default:` is the fallthrough/catch-all arm.
- Arms can contain multiple statements by indenting them:

```rust
match d:
  case direction.north:
    echo "heading north"
    return "north"
  default:
    return "other"
```

### Struct destructuring patterns

A `case` arm can destructure a struct value by naming fields inside `{}`.
Free names are bound as local variables; a `field: literal` pair constrains
the field to that exact value; `_` discards a field without binding it.

```rust
struct point =
  x i64
  y i64

fn classify(p point) string =
  match p:
    case point{x: 0, y: 0}: return "origin"
    case point{x: 0, y}:    return "y-axis at {y}"
    case point{x, y: 0}:    return "x-axis at {x}"
    case point{x, y}:       return "({x}, {y})"
```

An optional `if <guard>` after the pattern further filters the arm.
Guards can reference any fields bound in that arm:

```rust
match p:
  case point{x, y} if x == y: return "diagonal"
  case point{x, y}:           return "other"
```

A match is considered exhaustive when at least one arm has no literal
constraints and no guard (a "total" arm that matches every value), or when
a `default` arm is present. Guards do not count toward exhaustiveness.

### Field rename bindings

By default a free name in a pattern binds the field under its own name.
Use `field: newName` to bind the field under a different variable name:

```rust
struct point =
  x i64
  y i64

match p:
  case point{x: col, y: row}:  // bound as col and row, not x and y
    echo "col={col} row={row}"
```

A rename is recognized when the right side of `:` is a bare identifier not
followed by operators or `(`, `[`, `.`, `::`. Literal constraints still work
as before: `x: 0` requires the field to equal `0`; `name: "alice"` requires
the string field to equal `"alice"`.

### Nested struct patterns

Field values that are themselves structs can be matched by nesting a pattern
after the `:` separator. Any depth of nesting is supported. Free names (and
renames) at any nesting level are bound as local variables:

```rust
struct vec2 =
  x i64
  y i64

struct rect =
  origin vec2
  size   vec2

fn classify(r rect) string =
  match r:
    case rect{origin: vec2{x: 0, y: 0}, size}:        return "at-origin"
    case rect{origin: vec2{x: ox, y: oy}, size} if ox == oy: return "diagonal"
    case rect{origin: vec2{x: ox, y: oy}, size}:      return "({ox}, {oy})"
```

Fields bound inside a nested pattern are available in the guard and body of
the same arm, alongside fields from outer levels.

String fields work in literal constraints inside nested patterns too:

```rust
match emp:
  case employee{person: person{name: "alice", age, _}, _, salary}:
    echo "alice is {age}, earns {salary}"
```

### match as an expression

`match` can produce a value when used in an expression context. Each arm body
must be **exactly one expression** (no `return`, no multi-statement blocks):

```rust
let label = match p:
  case point{x: 0, y: 0}: "origin"
  case point{x, y} if x == y: "diagonal"
  case point{x, y}: "other"
```

All arms must produce the same (non-void) type. If arms need multiple
statements, use `return match ...` instead of `let x = match ...`.

### Nested match

`match` can be nested freely. A match arm can contain another match statement
as its body, or use a match expression as its value:

```rust
struct point =
  x i64
  y i64

// match statement inside a match arm
fn describe(p point) void =
  match p:
    case point{x, y}:
      match x:
        case 0: echo "on y-axis"
        default: echo "x={x}"

// nested match expression - arm value is itself a match
let label = match p:
  case point{x, y}: match x:
    case 0: "zero"
    case 1: "one"
    default: "many"
```

Struct fields bound in an outer arm are visible inside any nested match in
that arm's body.

---

### Array pattern matching

`match` can destructure array/slice values. Each arm lists expected elements
between `[` and `]`; free names bind to elements at that position, `_`
discards an element, and `...name` binds the remainder of the array as a
slice:

```rust
fn describe(xs [i64]) string =
  match xs:
    case []:             return "empty"
    case [x]:            return "one: {x}"
    case [x, y]:         return "two: {x}, {y}"
    case [x, ...rest]:   return "head={x}, tail has {len(rest)} elements"
    default:             return "unreachable if rest pattern above is present"
```

- An arm matches only if the array length equals the pattern length (for
  exact patterns) or is **strictly greater** than the prefix length (for
  rest patterns). The rest slot binds at least one element, so
  `[x, ...rest]` matches lists of length >= 2; use `[x]` for the singleton
  case.
- `_` discards an element without binding it.
- `...name` binds the tail slice starting at that position. If you do not
  need the tail, write `..._` to discard it.
- Guards (`if <expr>`) work the same as in struct patterns.

```rust
fn head_positive(xs [i64]) bool =
  match xs:
    case [x] if x > 0:       return true
    case [x, ..._] if x > 0: return true
    default:                 return false
```

**Exhaustiveness.** A match on arrays is exhaustive (no `default` needed)
when the set of patterns covers all possible lengths. The compiler's
exhaustiveness analysis follows Maranget, "Warnings for pattern matching",
*Journal of Functional Programming* 17(3), 2007, pp. 387-421
(doi:10.1017/S0956796807006223). The canonical exhaustive partition is the
triple `[]` + `[x]` + `[x, ...rest]`:

```rust
fn sum(xs [i64]) i64 =
  match xs:
    case []:           return 0
    case [x]:          return x
    case [x, ...rest]: return x + sum(rest)
```

`[]` covers length 0, `[x]` covers length 1, `[x, ...rest]` covers length
>= 2 (rest binds at least one element), so together they are exhaustive.

---

## where - pattern matching on function arguments

`where` is a declarative alternative to `if/else` or `match` inside a
function body. Each arm specifies a guard expression; the first arm whose
guard is true is used:

```rust
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)
```

- `where <guard>: <expr>`  -  if guard is true, return expr.
- `where _: <expr>`  -  wildcard, always matches (like `default`).

`where` can replace a multi-branch function body entirely:

```rust
fn abs(n i64) i64 =
  where n < 0: 0 - n
  where _: n

fn sign(n i64) i64 =
  where n < 0: -1
  where n == 0: 0
  where _: 1
```

`where` arms are evaluated top-to-bottom; the first matching arm wins.

---

## defer

`defer` schedules a statement to run when the enclosing scope exits:

```rust
fn read_file() =
  let buf = malloc(1024 * sizeof(char)).(*char)
  defer free(buf)
  // ... use buf ...
  // free(buf) is called here automatically
```

`defer` is useful for resource cleanup without requiring `try/finally`.
Multiple defers in one function run in **last-in, first-out** order.

---

## panic

`panic(msg)` terminates the program after running **all** deferred calls in
the entire call stack (not just the current function). The deferred calls run
from the innermost live frame to the outermost, in the same LIFO order as for
normal returns.

```rust
fn setup() =
  let buf = malloc(64)
  defer free(buf)
  panic("setup failed")
  // free(buf) is called automatically before exit

fn main() =
  defer echo "main cleanup"
  setup()            // panic here runs setup's defer, then main's defer
```

Output:
```
tin panic: setup failed
main cleanup
```

`panic` is a built-in statement, not a function; it does not return.

### How it works

Each `defer` statement pushes a lightweight thunk onto a process-global
linked list (the **defer chain**). On a normal function return, the current
frame's entries are popped and run inline. On `panic`, the runtime function
`_tin_panic` walks the entire remaining chain  -  including entries from all
live stack frames above the panic site  -  and calls each cleanup thunk before
calling `exit(1)`.

Because `_tin_panic` runs as a normal C function call (without C-level stack
unwinding via `longjmp` or exceptions), all stack frames are still live when
their cleanup thunks execute, so deferred calls can safely read local
variables.

---

## recover

`recover()` can be called inside a **deferred function** to catch a panic,
preventing the process from exiting. It returns the panic message as a
`string`, or an empty string if no panic is in progress.

```rust
fn risky() void =
  let got string = ""
  defer fn() void =
    let msg = recover()
    if len(msg) > 0 :
      got = msg
  panic("something went wrong")
  // got == "something went wrong" after the deferred fn runs
```

`recover()` only has an effect when called from inside a `defer`'d function
during a panic. Calling it outside a defer (or when no panic occurred) returns
`""` and has no side effect.

```rust
fn safe_call(f fn() void) bool =
  let panicked bool = false
  defer fn() void =
    let msg = recover()
    panicked = len(msg) > 0
  f()
  return panicked
```

> **Note:** When a panic is recovered, the panicking function returns its zero
> value rather than exiting the process. Subsequent code in the calling
> function continues executing normally.

---

## is  -  type narrowing for union types

`is` tests a union-typed value and binds the inner value if the test
succeeds:

```rust
type u = i8 | string

let a u = 10

if a is i i8:
  echo i * i        // i is bound as i8
else:
  echo a as string
```

See [07 - Enums & Unions](07-enums-unions.md) for details.
