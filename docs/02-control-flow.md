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

Any array can be iterated with `in`.

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
