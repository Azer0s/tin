# Tin Language Documentation

Tin is a statically typed, compiled systems language with a clean,
expression-oriented syntax. It compiles to native code via LLVM.

## Table of Contents

| Document | Contents |
|----------|----------|
| [01 - Basics](01-basics.md) | Types, variables, `echo`, string interpolation, operators |
| [02 - Control Flow](02-control-flow.md) | `if/else`, `for`, `match`, `where` pattern matching, `defer`, `panic` |
| [03 - Functions](03-functions.md) | Functions, closures, generics, pipe operator, overloading |
| [04 - Collections](04-collections.md) | Arrays, slices, ranges, destructuring |
| [05 - Structs](05-structs.md) | Structs, methods, `fn init`/`fn deinit`, generics, type aliases, tuples |
| [06 - Traits](06-traits.md) | Trait declaration, default methods, forward fields, vtable dispatch, generic traits |
| [07 - Enums & Unions](07-enums-unions.md) | Integer enums, atom enums, tagged unions, native C unions |
| [08 - C Interop](08-interop.md) | `extern`, pointers, C struct interop, linker directives (`//!`) |
| [09 - Packages](09-packages.md) | `use`/`export`, package resolution, standard library overview |
| [10 - Reflection](10-reflection.md) | Atoms, `any` type, `typeof`, `traitof`, `fieldnames`, `getfield`, `setfield` |
| [11 - Testing](11-testing.md) | `test` blocks, `assert` stdlib, `tin test` command |
| [12 - Macros](12-macros.md) | Simple macros (AST substitution), CTFE macros, backtick code-splice literals |
| [13 - Control Tags](13-control-tags.md) | `#pure`, `#sideffect`, `#no_recurse`, `#no_thread`, `#allow_sideffect` |
| [14 - Fibers & Channels](14-fibers.md) | `spawn`, `await`, `yield`, `Channel[T]`, `Future[T]`, async I/O, M:N scheduler |

## Standard Library

| Document | Contents |
|----------|----------|
| [JSON](stdlib/json.md) | JSON encoding/decoding: `encode`, `parse`, `parse[T]`, field tags |
| [Measure](stdlib/measure.md) | Monotonic clock: `now_us`, `now_ms` for benchmarking |
| [Regex](stdlib/regex.md) | PCRE regular expressions: `compile`, `exec`, `find_all`, `replace`, `split` |

## Quick taste

Tin compiles to native code via LLVM. Run a file with `tin run file.tin`,
build a binary with `tin build file.tin`, and run tests with `tin test file.tin`
(or `tin test dir/` for one directory, `tin test dir/...` to recurse).

```rust
// Hello world
echo "Hello, world!"

// Fibonacci with pattern matching
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10)

// Structs with methods
struct person =
  name string
  age  u8

  fn init(this person) =
    echo "new person: {this.name}"

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
echo pete.show()

// Traits
trait named =
  label string forward
  fn name(this named) string = return this.label

struct cat(named) =
  breed string

let c = cat{label: "Whiskers", breed: "tabby"}
echo c.name()

// Fibers and channels
use sync

fn{#async} worker(id i64, ch sync::Channel[string]) =
  ch.send("result from fiber {id}")

fn main() =
  let ch = sync::Channel[string].make(4)
  spawn worker(1, ch)
  spawn worker(2, ch)
  echo await ch.recv()   // "result from fiber 1" or "2", whichever finishes first
  echo await ch.recv()
```
