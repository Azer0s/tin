# 11 - Macros

Tin macros are compile-time templates. Every macro call is fully evaluated
before the program runs  -  there is no macro call overhead at runtime.

There are two kinds of macros:

| Kind | Body | Evaluation |
|------|------|-----------|
| **Simple** | single expression | AST substitution |
| **CTFE** | indented block | compiled and executed at compile time |

---

## Simple macros

A simple macro has an expression body. When called, the arguments are
substituted into the expression and the result is used in place of the call.

```rust
macro square!(x)  = x * x
macro double!(x)  = x * 2
macro add!(a, b)  = a + b
macro greet!(name) = "Hello, " ++ name ++ "!"

echo square!(5)          // 25
echo add!(3, 4)          // 7
echo greet!("Alice")     // Hello, Alice!
```

Macros can nest freely:

```rust
echo square!(double!(3))          // 36
echo add!(square!(4), square!(3)) // 25
```

Arguments can be any expression, including variables:

```rust
let x i64 = 7
echo square!(x)   // 49
echo add!(x, 3)   // 10
```

---

## CTFE macros (compile-time function evaluation)

When the macro body is an indented block the macro is a **CTFE macro**.
The block is wrapped in a typed helper function, compiled to a standalone
program, and executed. The result (printed via `echo`) is parsed back as a
tin expression and substituted at the call site.

```rust
// Iterative factorial  -  computed entirely at compile time
macro factorial!(n) =
  let result i64 = 1
  for let i i64 = 1; i <= n; i++:
    result = result * i
  return result

echo factorial!(10)   // 3628800  (no work done at runtime)
```

Use `return` to produce the macro's value, just like a function.

### Recursion

Recursive CTFE macros are allowed. A 5-second timeout prevents runaway
recursion from hanging the compiler.

```rust
macro fib!(n) =
  if (n <= 1):
    return n
  return (fib!((n - 1)) + fib!((n - 2)))

echo fib!(10)   // 55
```

Recursive self-calls inside the CTFE body are converted to ordinary function
recursion, so they do not spawn additional compiler processes.

### Inter-macro calls

A CTFE macro can call simple (expression-body) macros defined in the same
file. The compiler automatically includes those macro declarations in the
generated CTFE program.

```rust
macro sq!(x)   = x * x
macro hyp_sq!(a, b) =
  return (sq!(a) + sq!(b))

echo hyp_sq!(3, 4)   // 25
```

### Return types

The compiler infers the return type of a CTFE macro by inspecting `return`
statements in the block body. If no clear type can be found it falls back to
the type of the first argument.

| Returned expression | Inferred type |
|---------------------|---------------|
| integer literal     | `i64`         |
| float literal       | `f64`         |
| bool literal        | `bool`        |
| string literal      | `string`      |
| parameter name      | same as that argument |

### String-returning CTFE macros

A CTFE macro can return a `string`. The compiler captures the raw output and
substitutes a `StringLit` node  -  no quoting tricks required.

```rust
macro greet_ctfe!(name) =
  return "Hello, " ++ name ++ "!"

echo greet_ctfe!("World")   // Hello, World!
```

---

## Backtick code-splice literals

A backtick literal `` `expr` `` is a **code splice**  -  its content is raw
tin source that is parsed and inserted at the call site as an AST node,
rather than being treated as a string value.

### In simple macros

```rust
macro pi_ref!()      = `pi`         // splices the identifier pi
macro add_xy!()      = `(x + y)`    // splices an arithmetic expression

let pi f64 = 3.14159
echo pi_ref!()       // 3.14159

let x i64 = 10
let y i64 = 5
echo add_xy!()       // 15
```

The backtick content is parsed as a tin expression. Variables, operators,
and any other valid expression syntax are all valid inside backticks.

### In CTFE macros

A CTFE macro can `return` a backtick literal to conditionally splice
different code at the call site:

```rust
macro pick_var!(n) =
  if (n == 0):
    return `alpha`
  return `beta`

let alpha i64 = 100
let beta  i64 = 200
echo pick_var!(0)    // 100  (spliced identifier `alpha`)
echo pick_var!(1)    // 200  (spliced identifier `beta`)
```

At runtime of the CTFE program the backtick string is echoed with its
delimiters (`` `alpha` ``); the compiler detects the delimiters, strips
them, and parses the inner text as a tin expression.

---

## Purity constraint

Macro bodies must be **pure**  -  they may not contain `echo` or other I/O
statements. The compiler rejects any macro that violates this rule:

```rust
// Error: macro sq!: macros must be pure (no echo or I/O statements)
macro sq!(x) =
  echo x       // not allowed
  return x * x
```

This applies to both simple and CTFE macros.

---

## Naming convention

By convention macro names end with `!` to make call sites visually distinct:

```rust
macro assert_pos!(x) = (x > 0)

assert_pos!(5)   // clear: this is a compile-time expansion
```

The `!` suffix is part of the name but is optional during lookup  -  the
compiler recognises both `name!` and `name` when resolving macro calls.

---

## `tin preprocess` - expand macros and print source

The `tin preprocess` subcommand runs macro expansion on a file and prints
the resulting source with all macro calls replaced by their expansions:

```
tin preprocess myfile.tin
```

Simple (expression-body) macros are substituted inline. CTFE (block-body)
macros are evaluated and their splice result is inserted at the call site.
Macro declarations themselves are omitted from the output.

Example:

```rust
macro double!(x) = x * 2

let a = double!(5)
let b = double!(a + 1)
```

Running `tin preprocess` on this file outputs:

```rust
let a = 5 * 2
let b = (a + 1) * 2
```

This is useful for understanding what a CTFE macro expands to before compiling.
