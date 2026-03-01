# Tin Language Documentation

Tin is a statically typed, compiled systems language with a clean,
expression-oriented syntax. It compiles to native code via LLVM.

## Table of Contents

| Document | Contents |
|---|---|
| [01 – Basics](01-basics.md) | Types, variables, `echo`, string interpolation |
| [02 – Control Flow](02-control-flow.md) | `if/else`, `for`, `match`, `where` pattern matching, `defer`, `panic` |
| [03 – Functions](03-functions.md) | Functions, recursion, closures, generics, the `\|>` pipe operator |
| [04 – Collections](04-collections.md) | Arrays, ranges, iteration |
| [05 – Structs](05-structs.md) | Structs, methods, `fn init`, generic structs, `type` aliases |
| [06 – Traits](06-traits.md) | Trait declaration, default methods, forward fields, vtable dispatch, generic traits, alias traits |
| [07 – Enums & Unions](07-enums-unions.md) | Enums, `match`, union types, `data` wrapper types |
| [08 – Interop & Packages](08-interop.md) | `extern`, pointers, `use`/`export`, `defer`, linker directives (`//!`) |
| [09 – Reflection](09-reflection.md) | `any` type, `typeof`, `traitof`, `fieldnames`, `fieldtypes`, `fieldtag`, `getfield`, `setfield`, atoms |
| [10 – Testing](10-testing.md) | `test` blocks, `assert` stdlib, `tin test` command |
| [11 – Macros](11-macros.md) | Simple macros (AST substitution), CTFE macros (compile-time execution), backtick code-splice literals |
| [12 – Control Tags](12-control-tags.md) | `#pure`, `#sideffect`, `#no_recurse`, `#no_thread`, `#allow_sideffect`, macro tags |

## Quick taste

Tin compiles to native code via LLVM. Run a file with `tin run file.tin`,
build a binary with `tin build file.tin`, and run tests with `tin test file.tin`
(or `tin test dir/` to test an entire directory).



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
```
