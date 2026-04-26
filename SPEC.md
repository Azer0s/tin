# tin

## Hello world

```rust
echo "Hello, world!"
```

## Fibonacci sequence

```rust
fn fib(n u32) u32 =
  if n <= 1:
    return n
  else:
    return fib(n - 1) + fib(n - 2)

echo fib(10)
```

## Functional Fibonacci sequence

```rust
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)
```

## Integer literal formats

Integer literals can be written in decimal, hexadecimal, octal, or binary:

| Prefix      | Base | Example      | Value |
|-------------|------|--------------|-------|
| (none)      | 10   | `255`        | 255   |
| `0x` / `0X` | 16   | `0xFF`       | 255   |
| `0o` / `0O` | 8    | `0o377`      | 255   |
| `0b` / `0B` | 2    | `0b11111111` | 255   |

```rust
let mask  i64 = 0xFF00FF     // hex
let perms i64 = 0o755        // octal
let flags i64 = 0b10100011   // binary
```

All integer literals are `i64` by default and coerce automatically to narrower types.

---

## Language samples

```rust
type char = u8
type string = [char]

let a string = "hello"
const b i8 = 22

fn main(args [string], argc u16) i32 =
  echo "Hello world"

  for let i i32; i < 10; i++:
    echo "{i}"
```

### Control flow

```rust
if n < 0:
  echo "negative"
else if n == 0:
  echo "zero"
else:
  echo "positive"
```

`else if` chains are unbounded; each branch uses Python-style indented
bodies. `match` is covered under [Pattern matching](#pattern-matching)
below.

### External functions

```rust
// Symbol name is a quoted string.
fn ex_printf(const *char, ...) i32 = extern("printf")
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)

let hello = "Hello!"
printf(hello)
```

```rust
// use extern imports C symbols.
// Local name == C name when no rename is given.
// Use localName("cName") to rename.
use extern (
  malloc as fn(size_t) *void,
  strcpy as fn(*char, const *char) *char,
  myFree("free") as fn(*void),
)

let s *char = malloc(10 * sizeof(*char)).(*char)
strcpy(s, "abcdefghij")
defer myFree(s)

let a = "Hello" // string is inferred
a = a ++ ", world!"
a = a ++ [',', ' ', 'w', 'o', 'r', 'l', 'd', '!']
```

### Structs

```rust
struct person =
  name string
  age u8

  fn init(this person) =
    echo "called when a person struct is initialized (except for malloc)"

  fn deinit(this person) =
    echo "called when a named person variable goes out of scope"

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo pete.show()
echo (*pPtr).show()
echo pPtr->show()
```

```rust
use mem

struct person =
  name string
  age u8

let pete *person = mem::calloc(1, sizeof(person)).(*person)
(*pete).name = "Pete"
(*pete).age = 20
```

```rust
struct tuple[t] =
  first t
  second t

  fn show(this tuple) string =
    return "first: {this.first}, second: {this.second}"

type point = tuple[f32] override =
  fn show(this point) string =
    return "({this.first}, {this.second})"

let p1 point = point{1.2, 1.4}

let t tuple = p1
```


### Map | Filter | Reduce

```rust
let nums [i32] = [1, 2, 3, 4, 5, 6, 7]

fn filter[t](f fn(i t) bool) fn([t]) [t] =
  return fn(list [t]) [t] =
    let res [t] = []

    for let i t in list:
      if f(i):
        res ++= i

    return res

fn map[t, r](f fn(i t) r) fn([t]) [r] =
  return fn(list [t]) [r] =
    let res [r] = []

    for let i t in list:
      res ++= f(i)

    return res

nums
|> filter(fn(i i32) bool = return i % 2 == 0)
|> map(fn(i i32) i32 = return i * i)
```

```rust
fn subsequences[t](l [t]) [[t]] =
  let res [[t]] = []

  for let i i64 = 0..(len(l) ^ 2):
    let sequence [t] = []

    for let j i64 in 0..len(l):
      let pick = (i >> j) & 1
      if pick == 0:
        sequence ++= l[j]

    res ++= sequence
  return res
```

### Close to metal programming

```rust
fn write_string(color i32, s string) =
  let video_mem *char = addr(0xB8000).(*char)
  for let c char in s:
    *video_mem = c
    video_mem += 1
    *video_mem = color
    video_mem += 1
```

### Iterators & traits

```rust
type size_t = u32

use extern (
  // extern functions are always marked as #sideffect
  strlen as fn(const *char) size_t,
)

// Regular trait with virtual methods.
trait iter[t] =
  fn len(this iter[t]) size_t = virtual
  fn get(this iter[t], i size_t) t = virtual

// Forward-field trait: injects a field + default method.
trait size =
  s size_t forward
  fn size(this size) size_t =
    return this.s

// Alias trait: maps to a single function signature.
// Implementing structs use fn ::print() to provide the impl.
trait print as fn() [char]

// Generic alias trait: static conversion function.
// k is the output type (inferred as the implementing struct).
// t is the input type (specified in the trait bound, e.g. implicit[[char]]).
trait[k] implicit[t] as static fn(val t) k

struct{ #pure@fn #const@field } str(size, print,
    implicit[[char]], implicit[char],
    iter[char], iter[str]) =

  v [char]

  // Alias trait implementations use fn ::traitName.
  static fn ::implicit[[char]](val [char]) str =
    let len size_t = 0
    { #allow_sideffect } {
      len = strlen(val)
    }
    return str{v: val, s: len}

  static fn ::implicit[char](val char) str =
    return str{v: [val], s: 1}

  fn ::print() [char] =
    return this.v

  // When a struct implements the same generic trait twice (e.g. iter[char]
  // and iter[str]), qualify each impl with the trait instantiation.
  fn iter[char]::get(this str, i size_t) char =
    return this.v[i]

  fn iter[str]::get(this str, i size_t) str =
    return str{v: [this.v[i]], s: 1}

  fn len(this str) size_t =
    return this.s

  fn for_each(this str, f fn(c char)) =
    for let i size_t = 0; i < this.s; i++:
      { #allow_sideffect } { f(this.v[i]) }

let h str = "Hello world"
echo h

h.for_each(fn(c char) = io::print("{}", c))
```

### Ranges

```rust
let r = 1..10
for let i i8 in 1..10:
  echo "{i}"
```

### Union types (tagged unions)

`type` aliases over `|`-separated types create a tagged union.
Layout: `{ i8 tag, [maxSize x i8] payload }`.

```rust
type num = i64 | f64
type strnum = string | i64

let a num = 42        // tag=0 (i64)
let b num = 3.14      // tag=1 (f64)

// is-check (no binding)
if a is i64:
  echo "integer"

// is-check with binding
if a is n i64:
  echo n              // n bound as i64

// type dispatch
match a.(type):
  case n i64:
    echo n
  case x f64:
    echo x
```

### Native union types

`union` creates a C-style union  -  overlapping memory, no tag.
Layout: `{ [maxSize x i8] storage }`.

```rust
// Unnamed: access via .(Type) cast
union raw = i32 | i64

let r raw = 42
let v i32 = r.(i32)
let w i64 = r.(i64)

// Named: access via field name or .(Type)
union color = as_i32 i32 | as_r u8

let c color = 255
let v i32 = c.as_i32
let b u8  = c.as_r    // same memory, read as u8
```

### Enums

```rust
enum i32 weather =
  sunny: 0
  rainy: 1
  foggy: 2
  clear: 3

let w weather = weather.sunny

match w:
  case weather.sunny:
    echo "it is sunny outside"
  case weather.rainy:
    echo "it rains"
  default:
    echo "there is weather"

enum slider_type = // takes the smallest integer type possible (u8 here)
  horizontal
  vertical
```

### Packages & exports

```rust
fn print(t string) =
  echo t

export { print } as io
```

```rust
use io

io::print("hello")
```

```rust
use io
use math

export { io, math } as std
```

```rust
use std
let a = std::math::floor(std::math::PI)
```

```rust
use extern (
  malloc as fn(size_t) *void,
  memset as fn(*void, i32, size_t),
)

fn malloc_zeroed(s size_t) *void =
  let chunk = malloc(s)
  memset(chunk, 0, s)
  return chunk
```

### Control tags

Tags are written in lowercase with underscores (`#tag_name`) and describe the
**capability or property** of the annotated construct. Syntax: `fn{#tag} name(...)`.

#### `#pure`  -  no observable side effects (enforced)

The compiler transitively verifies that the function contains no `echo` and
calls no `#sideffect` or extern functions, unless wrapped in a `#allow_sideffect` block.

```rust
fn{#pure} square(n i64) i64 = return n * n
fn{#pure} dist_sq(x i64, y i64) i64 = return square(x) + square(y)

fn{#pure} bad() i64 =
  echo "oops"   // compile error: #pure violation  -  echo is a side effect
  return 1
```

#### `#sideffect`  -  declares observable side effects

Advisory tag. Extern C functions are automatically tagged `#sideffect`.
Calling a `#sideffect` function from a `#pure` function is a compile error.

```rust
fn{#sideffect} log(msg string) = extern("c_log")  // auto-tagged #sideffect

fn{#pure} read_only() i64 =
  log("hi")   // compile error: #pure violation  -  calls #sideffect function "log"
  return 42
```

#### `#allow_sideffect`  -  escape hatch inside a `#pure` function

A tagged block `{ #allow_sideffect } { ... }` suppresses purity checks for its body,
allowing side effects in an otherwise pure function.

```rust
fn{#pure} mostly_pure(n i64) i64 =
  { #allow_sideffect } {
    echo "debug"   // allowed
  }
  return n * n
```

#### `#no_recurse`  -  must not call itself (enforced)

The compiler walks the generated IR to ensure no self-call exists.

```rust
fn{#no_recurse} foo() =
  foo()   // compile error: #no_recurse violation  -  function calls itself
```

#### `#no_thread`  -  unsafe for concurrent calls (advisory)

Not enforced at compile time; serves as documentation.

```rust
fn{#no_thread} init_globals() = pass
```

#### Other function / method tags

| Tag              | Applies to           | Meaning                                              |
|------------------|----------------------|------------------------------------------------------|
| `#async`         | fn / method / lambda | Runs as a cooperative green thread (fiber)           |
| `#heavy`         | fn / method          | Forces "auto-yield" classification for schedulers    |
| `#no_autoyield`  | fn / method / lambda | Suppresses auto-yield at loop backedges and calls    |
| `#handover`      | extern fn only       | Transfers ownership of a returned C pointer into ARC |
| `#interop`       | top-level fn         | Emits a C-callable wrapper; see [08 - C Interop](docs/08-interop.md#calling-tin-from-c-interop) |

#### Struct tags

Structs accept both unscoped tags (apply to the struct itself) and
**scoped** tags with an `@scope` qualifier that propagates the tag to
matching members:

```rust
struct{#packed} record =                 // unscoped: struct-level
  tag   u8
  value u32                              // sizeof(record) = 5 (no padding)

struct{#pure@fn #const@field} vec2 =     // scoped: propagates to members
  x f64
  y f64
  fn magnitude(this vec2) f64 =          // inherits #pure
    return this.x * this.x + this.y * this.y
```

Scopes: `@fn` (all methods), `@method` (instance methods only),
`@static_fn` (static methods only), `@field` (all fields).

`#const@field` flips the unmarked-field default to `const`; `var`
opts out. Per-field `const` / `var` also work standalone - see
[Per-field mutability](#per-field-mutability---const--var).

See [docs/13-control-tags.md](docs/13-control-tags.md) for the full
tag-scope compatibility matrix and cascade semantics.

#### Macro tags

| Tag          | Meaning                            |
|--------------|------------------------------------|
| `#no_excl`   | Macro callable without `!` suffix  |
| `#no_parens` | Macro callable without parentheses |

```rust
macro{#no_excl #no_parens} proc() =
  return `fn{#pure #no_recurse #no_thread}`

proc fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)
```

Extern functions are automatically tagged `#sideffect`:

```rust
fn ex_printf(const *char, ...) i32 = extern("printf")  // auto-tagged #sideffect
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)

fn{#pure} pure_fn() =
  printf("Hello world")  // compile error: #pure violation  -  calls #sideffect function "printf"
```

### defer

```rust
use extern (
  malloc as fn(size_t) *void,
  free   as fn(*void),
)

let s *void = malloc(10 * sizeof(char))
defer free(s)
// free(s) is called at end of scope, even on early return
```

### pass statement

`pass` is a no-op statement that explicitly marks an empty block:

```rust
fn do_nothing() =
  pass

fn do_thing() = pass

let noop = fn() = pass
let x i64 = 5
let n i64 = 3

if x > 10 :
  do_thing()
else :
  pass

for let i i64 in 0..n :
  pass
```

`pass` has no runtime effect. It may appear anywhere a statement is expected.

---

### Linker directives

Files can embed linker flags with `//!` comment lines at the top of the file
(before any non-comment code):

```rust
//!-lm
//!-lraylib
```

The text after `//!` is appended verbatim to the linker command line. Common
uses: `//!-lm` (C math library), `//!-lraylib` (Raylib), `//!-lpthread`.

---

### Test blocks

`test` blocks declare named test cases. They are compiled and run only when
the file is executed with `tin test`; they are skipped during `tin run` and
`tin build`.

```rust
use assert

test "description" =
  assert::equals(1 + 1, 2)
  assert::ok(true)
```

Run tests:

```
tin test examples/test_example.tin   # single file
tin test examples/                   # entire directory
```

The `assert` stdlib (`use assert`) provides:

| Function                                       | Description                             |
|------------------------------------------------|-----------------------------------------|
| `assert::equals[t](expected t, actual t)`      | Generic equality; `t` must implement `comp` |
| `assert::not_equals[t](a t, b t)`              | Generic inequality; `t` must implement `comp` |
| `assert::ok(cond bool)`                        | Assert condition is true                |
| `assert::not_ok(cond bool)`                    | Assert condition is false               |
| `assert::fails(msg string)`                    | Unconditionally fail with message       |

`assert::equals` is fully generic: it works on any type implementing the
`comp` trait (all primitives, strings, atoms, and user types with a
`==` overload). There are no type-specific `equals_str` / `equals_f64`
variants - one generic function covers all cases.

When an assertion fails inside `tin test`, the runner prints the failure and
moves on to the next test (via `longjmp`). In a standalone run, `exit(1)` is
called.

---

### Atoms

Atoms are compile-time symbolic constants. They have type `atom` and compare
by identity (interned at compile time).

**Simple atoms**  -  a leading `'` followed by letters, digits, and underscores
only:

```rust
'ok
'err
'sunny
'my_type_1
```

These are used in enum declarations, `where` pattern matching, and are returned
by `typeof` for primitive types:

```rust
let t = typeof(42)    // 'i64
let t2 = typeof(true) // 'bool
```

**Complex (quoted) atoms**  -  when the type string contains characters not
allowed in a simple atom name (`(`, `)`, `[`, `]`, `*`, `,`), use the quoted
form `'"..."`:

```rust
'"fn(i64)bool"
'"fn(i64,f64)bool"
'"*bool"
'"[string]"
'"fn(fn(i64)bool,i64)string"
```

Quoted atoms are produced by `typeof` for pointer, array, and function types,
and are the form expected by `reflect` API functions:

```rust
use reflect

echo reflect::is_fn('"fn(i64)bool")      // 1
echo reflect::fn_ret('"fn(i64,f64)bool") // bool
echo reflect::elem('"*bool")             // bool
echo reflect::elem('"[string]")          // string
```

Both simple and quoted atoms have type `atom` and work identically with `==`,
`where` guards, and reflection functions.

**String representation**  -  `echo` prints atoms with their leading apostrophe:

```rust
echo 'ok    // 'ok
echo 'err   // 'err
```

When an atom is coerced to `string` (for comparisons or passed to an `extern`
function declared as returning `atom`), the apostrophe is **not** included  - 
the bare name is used:

```rust
use assert
assert::ok('ok == "ok")      // true  -  bare name comparison
assert::equals_str('ok, "ok") // passes
```

**Runtime atom learning**  -  `__tin_string_to_atom` searches the compile-time
atom table first. If the bare string is not found (e.g., it came from an
external C function at runtime), the CRC32 is computed on the fly, the atom is
stored in a mutex-protected linked-list table, and subsequent lookups return the
same code (idempotent):

```rust
fn dynamic_name() atom = extern("some_c_fn_returning_char_ptr")

let a = dynamic_name()
let b = dynamic_name()
assert::ok(a == b)            // same code, regardless of static table
```

---

### Atoms & Macros

```rust
use io
use guid
use strings

enum atom status =
  'ok
  'err

struct result[t] =
  val t
  status status

  static fn ok(val t) result[t] =
    return result{val: val, status: status.ok}

  static fn err() result[t] =
    return result{val: default(t), status: status.err}

macro try!(action) =
  let i = "_" ++ strings::replace(guid::new(), "-", "")
  return `(let {i} = {action}; {i}.status == status.ok ? {i}.val : default(typeof({i}.val)))`

fn do_stuff() result[u32] =
  return result[u32]::ok(42)

fn main() void =
  let val = try!(do_stuff())
  echo val
```

```rust
fn it_is(weather atom) =
  where 'sunny: echo "It is sunny!"
  where 'rainy: echo "It is rainy!"
  where _: echo "Sorry, I don't know this condition :("
```

## Trait System Design Notes

### Trait kinds

| Kind          | Syntax                                | Description                                    |
|---------------|---------------------------------------|------------------------------------------------|
| Regular       | `trait T = fn m(...) = virtual`       | virtual + default methods, forward fields      |
| Alias         | `trait T as fn(...) R`                | single function type; impl with `fn ::T`       |
| Generic alias | `trait[k] T[t] as static fn(val t) k` | static conversion; `k` inferred as struct type |

### Implementing traits in a struct

```rust
// Regular virtual method  -  name matches the trait's virtual method:
trait speaker =
  fn speak(this speaker) string = virtual

struct dog(speaker) =
  fn speak(this dog) string = "Woof"

let d = dog{}
echo d.speak()

// Alias trait  -  implementing struct uses fn ::traitName(this T) to provide the impl:
trait show as fn() string

struct point(show) =
  x i64
  y i64
  fn ::show(this point) string = return "point"

let p = point{x: 1, y: 2}
echo p.show()
```

### Method Name Conflict Resolution
When two traits declare methods with the same name, resolution is by **type context**:
- If the receiver has a concrete struct type, the struct's own method is called directly (static dispatch).
- If the receiver has a trait-typed value (fat pointer), the vtable for that specific trait is used.
- Use `traitName[args]::method` inside a struct body to disambiguate when the same generic trait is instantiated more than once on the same struct.

### Implicit Trait Type Parameter Inference
For generic implicit conversion traits like `trait[k] implicit[t] as static fn(val t) k`:
- When a struct declares `implicit[[char]]`, the compiler infers `k = StructType` and `t = [char]`.
- Explicit type arguments are not required  -  parameters are inferred from context.

### Vtable Layout
- One vtable per `(struct, trait_instantiation)` pair.
- Fat pointer: `{data_ptr: i8*, vtable_ptr: VTableType*}` for trait-typed values.
- Mixin/forward/alias traits do not generate vtables.

### Default Method Bodies
- Methods declared in a trait with a body (not `virtual`) are mixin defaults.
- All implementing structs inherit the default unless they override it.

### defer
- `defer expr` registers a call to be executed before the function returns.
- Multiple defers fire in LIFO order (last deferred runs first).
- The deferred call fires on every exit path including early `return`.
- `defer fn() void = body` defers an inline lambda. The lambda captures outer
  variables by reference, so mutations inside propagate back to the caller.
- `defer (fn() void = body)()` - immediately-invoked anonymous function also
  works with defer.

### recover
- `recover()` is a built-in function that can only be called inside a `defer`'d
  function.
- If the enclosing function panicked, `recover()` returns the panic message as
  a `string` and suppresses the panic (the process does not exit).
- If no panic is in progress, `recover()` returns `""`.

```rust
fn safe(f fn() void) string =
  let msg string = ""
  defer fn() void =
    msg = recover()
  f()
  return msg
```

### Array and struct destructuring

```rust
// Uniform array destructuring
let arr [i64] = [1, 2, 3]
let [a, b] [i64] = arr      // a=1, b=2

// Per-slot from [any] with runtime bounds check
let mixed [any] = [42 as any, true as any]
let [n, flag] [i32, bool] = mixed   // n=42, flag=true

// Rest split
let [head, ...tail] [i64] = arr     // head=1, tail=[2,3]

// Named type alias for per-slot pattern
type res = @[i32, bool]
let [code, ok] res = mixed          // code=42, ok=true

// Struct destructuring
struct point =
  x i64
  y i64
let p = point{x: 3, y: 4}
let {x, y} point = p               // x=3, y=4
```

---

## Per-field mutability - const / var

Struct fields may be prefixed with `const` or `var` to control whether
they can be reassigned after construction. Unmarked fields default to
mutable, matching variable semantics.

```rust
struct point =
  const x i64           // immutable after init
  const y i64           // immutable after init
  var   scratch i64     // explicit mutable
  z i64                 // unmarked - mutable (default)

let p = point{x: 1, y: 2, scratch: 0, z: 5}
p.scratch = 42          // OK
p.x = 99                // compile error: cannot assign to const field point.x
```

`const` is a compile-time-only tag: direct writes through the field
name are rejected (`s.f = v`, `s.f += v`, `s.f++`, method-body writes,
pointer-dereference writes `pp->f = v`). Reflective writes
(`setfield(s, "f", v)`) and address-taking (`&s.f`) remain allowed.

The struct-level tag `#const@field` flips the default for unmarked
fields to `const`; see [Struct tags](#struct-tags) above.

See [docs/05-structs.md](docs/05-structs.md#field-mutability---const--var)
for detailed semantics and interaction with `weak` / `own` modifiers.

---

## Pattern matching

`match` dispatches on a value against structural patterns:

```rust
fn classify(p point) string =
  match p:
    case point{x: 0, y: 0}:             return "origin"
    case point{x: 0, y}:                return "y-axis at {y}"
    case point{x, y: 0}:                return "x-axis at {x}"
    case point{x, y} if x == y:         return "diagonal"
    case point{x, y}:                   return "({x}, {y})"
```

Pattern kinds: struct `Type{field: lit, bound}`, array `[x, y]` /
`[x, ...tail]`, tuple `(a, b)`, ADT constructor `Ok(v)`, literal,
identifier (bind), `_` (wildcard). Guards (`if expr`) further filter
any arm. Exhaustiveness uses Maranget's algorithm; a `default` arm or
a catch-all pattern is required only when the compiler cannot prove
completeness.

`match` also works as an expression (single-expr arms only):

```rust
let label = match p:
  case point{x: 0, y: 0}:   "origin"
  case point{x, y} if x==y: "diagonal"
  case point{x, y}:         "other"
```

### `where` pattern clauses

Inside a function body `where` can replace a match by pattern-dispatching
on the function's arguments:

```rust
fn sign(n i64) i64 =
  where (0):  0
  where (n) if n > 0:  1
  where _:  -1
```

See [docs/02-control-flow.md](docs/02-control-flow.md) for the full
pattern grammar and [docs/04-collections.md](docs/04-collections.md)
for array patterns.

---

## Algebraic data types

`data` declares a tagged sum type. Variants may be nullary, positional,
or named:

```rust
data Shape =
  Dot
  Circle(radius f64)
  Rect(width f64, height f64)
  Rgb(r i64, g i64, b i64)

fn area(s Shape) f64 =
  match s:
    case Dot:           return 0.0
    case Circle(r):     return r * r * 3.14
    case Rect(w, h):    return w * h
    case Rgb(_, _, _):  return 0.0
```

Generic ADTs parameterise over types:

```rust
data Result[t, e] =
  Ok(val t)
  Err(msg e)

fn lookup(k string) Result[i64, string] =
  if k == "answer": return Ok(42)
  return Err("not found")
```

ADT fields may use `own` to declare acyclic recursion:

```rust
data Tree[t] =
  Leaf
  Node(val t, left own *Tree[t], right own *Tree[t])
```

Maranget exhaustiveness applies: the compiler verifies every variant
is covered (or rejects the match with a specific missing-variant).

Cross-module ADTs work through `use { Result, Ok, Err } from result`
(the canonical `stdlib/result` package ships Result and Option).

---

## Fibers, async, and channels

Tin has stackless coroutines with cooperative scheduling. A function
tagged `#async` runs as a fiber; callers use `spawn` to launch it and
`await` to collect the result.

```rust
fn{#async} worker(n i64) i64 =
  yield                          // voluntary reschedule
  return n * 2

fn main() =
  let f = spawn worker(21)
  echo await f                   // 42
```

The compiler auto-inserts yield points at loop backedges and before
calls to heavy or recursive callees; `#no_autoyield` disables this
per-function, and `#heavy` forces a callee to be classified as
yield-before.

`await match` selects among multiple in-flight futures:

```rust
await match [fa, fb, fc]:
  case [x, _, _]:  echo "fa fired: {x}"
  case [_, y, _]:  echo "fb fired: {y}"
  case [_, _, z] if z > 0:  echo "fc positive: {z}"
  default:         echo "nothing ready"
```

The `sync` stdlib provides channels (`Channel[T]`, bounded + unbounded),
mutexes (`Mutex`), and atomics (`Atomic[T]`). See
[docs/14-fibers.md](docs/14-fibers.md) for the full model, scheduler
contract, and performance notes.

---

## Reflection builtins

Tin ships five zero-ceremony reflection builtins that operate on any
struct value (including values boxed as `any`):

| Builtin                        | Returns              | Meaning                              |
|--------------------------------|----------------------|--------------------------------------|
| `typeof(v)`                    | `atom`               | Runtime type as an atom              |
| `traitof(v)`                   | `[atom]`             | List of implemented trait names      |
| `fieldnames(v)`                | `[atom]`             | User-visible field names, in order   |
| `fieldtypes(v)`                | `[atom]`             | Field types (matches `fieldnames`)   |
| `fieldtag(v, "fieldName")`     | `atom`               | The `@"..."` metadata tag for a field |
| `getfield(v, "fieldName")`     | `any`                | Read a field dynamically             |
| `setfield(v, "fieldName", x)`  | -                    | Write a field dynamically            |

```rust
struct user =
  id    i64    @"primary_key"
  email string @"unique"

let u = user{id: 1, email: "a@b.c"}
echo typeof(u)              // 'user
echo fieldtag(u, "id")      // 'primary_key
echo fieldnames(u)          // ['id, 'email]
echo getfield(u, "email")   // a@b.c
setfield(u, "id", 42)
```

`getfield` / `setfield` on a concrete struct lower to a direct GEP
+ load/store; on an `any` value, a runtime dispatch chain selects the
correct struct layout. See [docs/10-reflection.md](docs/10-reflection.md)
for the full API.
