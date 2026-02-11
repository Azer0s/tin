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

| Prefix | Base | Example | Value |
|--------|------|---------|-------|
| (none) | 10 | `255` | 255 |
| `0x` / `0X` | 16 | `0xFF` | 255 |
| `0o` / `0O` | 8 | `0o377` | 255 |
| `0b` / `0B` | 2 | `0b11111111` | 255 |

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

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo pete.show()
echo (*pPtr).show()
echo pPtr->show()
```

```rust
let pete *person = malloc(sizeof(person)).(*person)
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
  // extern functions are always marked as #sideEffectful
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
    { #ignoreSideEffectful } {
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
      { #ignoreSideEffectful } { f(this.v[i]) }

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

### Wrapper types

```rust
data maybe[t] = t | None

let m maybe[string] = None

if m is s string:
  echo s

if m is None:
  echo "m is unset"
```

### Native union types

`union` creates a C-style union — overlapping memory, no tag.
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
  sunny: 0,
  rainy: 1,
  foggy: 2,
  clear: 3,

let w weather = weather.sunny

match w:
  case weather.sunny:
    echo "it is sunny outside"
  case weather.rainy:
    echo "it rains"
  default:
    echo "there is weather"

enum slider_type = // takes the smallest integer type possible (u8 here)
  horizontal,
  vertical,
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

```rust
fn{#pure #recurse} fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10) // calculated at compile time
```

```rust
fn{#noRecurse} foo() =
  foo()

// this will fail to compile
```

```rust
macro{#noExcl #noParens} proc() =
  return `fn{#pure #recurse #noThread}`

proc fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

fn ex_printf(const *char, ...) i32 = extern("printf") // automatically has #sideEffectful tag
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)

proc print() =
  printf("Hello world") // this will fail to compile
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

let noop = fn() = pass

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

| Function | Description |
|---|---|
| `assert::equals(expected i64, actual i64)` | Assert two `i64` values are equal |
| `assert::equals_str(expected string, actual string)` | Assert two strings are equal |
| `assert::equals_f64(expected f64, actual f64)` | Assert two `f64` values are equal |
| `assert::ok(cond bool)` | Assert condition is true |
| `assert::not_ok(cond bool)` | Assert condition is false |
| `assert::not_equals(a i64, b i64)` | Assert two `i64` values differ |
| `assert::fails(msg string)` | Unconditionally fail with message |

When an assertion fails inside `tin test`, the runner prints the failure and
moves on to the next test (via `longjmp`). In a standalone run, `exit(1)` is
called.

---

### Atoms

Atoms are compile-time symbolic constants. They have type `atom` and compare
by identity (interned at compile time).

**Simple atoms** — a leading `'` followed by letters, digits, and underscores
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

**Complex (quoted) atoms** — when the type string contains characters not
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

**String representation** — `echo` prints atoms with their leading apostrophe:

```rust
echo 'ok    // 'ok
echo 'err   // 'err
```

When an atom is coerced to `string` (for comparisons or passed to an `extern`
function declared as returning `atom`), the apostrophe is **not** included —
the bare name is used:

```rust
assert::ok('ok == "ok")      // true — bare name comparison
assert::equals_str('ok, "ok") // passes
```

**Runtime atom learning** — `__tin_string_to_atom` searches the compile-time
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

enum atom status =
  'ok,
  'err

struct result[t] =
  val t
  status

  static fn ok(val t) result[t] =
    return result{val: val, status: status.ok}

  static fn err() result[t] =
    return result{val: default(t), status: status.err}

macro try!(action) =
  let i = "_" ++ guid::new().show().replace("-", "")
  return `
    (let {i} = {action}; {i}.status == status.ok) ? {i}.val : return result.err()
  `

fn do_stuff() result[u32] =
  return result[u32]::ok(42)

let val = try!(do_stuff())
```

```rust
fn it_is(weather atom) =
  where 'sunny: echo "It is sunny!"
  where 'rainy: echo "It is rainy!"
  where _: echo "Sorry, I don't know this condition :("
```

## Trait System Design Notes

### Trait kinds

| Kind | Syntax | Description |
|------|--------|-------------|
| Regular | `trait T = fn m(...) = virtual` | virtual + default methods, forward fields |
| Alias | `trait T as fn(...) R` | single function type; impl with `fn ::T` |
| Generic alias | `trait[k] T[t] as static fn(val t) k` | static conversion; `k` inferred as struct type |

### Implementing traits in a struct

```rust
// Regular virtual method — name matches the trait's virtual method:
fn speak(this dog) string = "Woof"

// Alias trait — prefix with :: to mark it as the alias implementation:
fn ::print() [char] = return this.v

// Disambiguation when the same generic trait is implemented twice:
fn iter[char]::get(this str, i size_t) char = return this.v[i]
fn iter[str]::get(this str, i size_t) str = ...
```

### Method Name Conflict Resolution
When two traits declare methods with the same name, resolution is by **type context**:
- If the receiver has a concrete struct type, the struct's own method is called directly (static dispatch).
- If the receiver has a trait-typed value (fat pointer), the vtable for that specific trait is used.
- Use `traitName[args]::method` inside a struct body to disambiguate when the same generic trait is instantiated more than once on the same struct.

### Implicit Trait Type Parameter Inference
For generic implicit conversion traits like `trait[k] implicit[t] as static fn(val t) k`:
- When a struct declares `implicit[[char]]`, the compiler infers `k = StructType` and `t = [char]`.
- Explicit type arguments are not required — parameters are inferred from context.

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
