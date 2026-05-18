# Generic functions


Type parameters are declared in square brackets before the parameter list:

```rust
fn identity[t](x t) t =
  return x

echo identity(42)
echo identity("hello")
```

The compiler infers type arguments from the call-site arguments.

Multiple type parameters:

```rust
fn map[t, r](f fn(i t) r) fn([t]) [r] =
  return fn(list [t]) [r] =
    let res [r] = []
    for let i t in list:
      res ++= [f(i)]
    return res
```

### Type constraints

A type parameter can be constrained with a `where t is <constraint>` clause
after the parameter list. Only types satisfying the constraint may be used as
the type argument:

```rust
fn min[t](a t, b t) t where t is ord =
  if a < b: return a
  return b

fn contains[t](haystack [t], needle t) bool where t is comp =
  for let item t in haystack:
    if item == needle: return true
  return false
```

The two built-in constraints are:

| Constraint | Satisfied by | Operators available |
|------|-------|-----------|
| `ord` | All integer types (`i8`..`i128`, `u8`..`u128`), float types (`f32`, `f64`, `f128`), `byte`, `char` | `<`, `<=`, `>`, `>=` (and by inclusion all `comp` operators) |
| `comp` | Everything `ord` accepts, plus `string` and `bool` | `==`, `!=` |

`comp` is a superset of `ord`: every `ord` type also satisfies `comp`.

```rust
// sort needs only ordering
fn sort[t](list [t]) [t] where t is ord = ...

// equality check needs only comparability
fn index_of[t](list [t], val t) i64 where t is comp =
  for let i i64 in 0..len(list):
    if list[i] == val: return i
  return -1
```

A `where` clause can also name a user-defined trait:

```rust
fn print_all[t](items [t]) where t is display =
  for let item t in items:
    echo item.display()
```

### Boolean bound expressions

A bound is a boolean expression over trait checks, not just a single trait
name. The grammar per type parameter is:

```
bound := or
or    := and ('||' and)*
and   := unary ('&&' unary | '+' unary)*
unary := 'not' atom | atom
atom  := '(' bound ')' | <type-or-trait-name>
```

- `&&` / `||` combine constraints with the obvious semantics.
- `not <trait>` excludes types that satisfy the trait.
- `+` is legacy shorthand for `&&` (both `T is A+B` and `T is A && B` work).
- Each atom is checked against the enclosing type parameter - no need to
  repeat `T is` inside the expression.

```rust
fn max3[T](a T, b T, c T) T where T is ord && not bool = ...
fn num_op[T](x T) T  where T is i32 || i64        = ...
fn fancy[T](x T)     where T is ord && (not bool && not f64) = ...
```

Multiple type parameters can be bounded independently by separating with a
comma or a new `where`:

```rust
fn combine[A, B](a A, b B) where A is comp, B is comp = ...
fn combine[A, B](a A, b B) where A is comp where B is comp = ...
```

When a bound fails at monomorphization the compiler points at the clause
and names the failing sub-check:

```
.../foo.tin:7:23: fn sort_pair[bool]: type "bool" does not satisfy constraint
`where T is ord && not bool` (failing sub-check: `ord`)
```

### Bounds on type aliases

A generic `type` declaration can attach a `where` clause. It's checked
when the alias is used with a concrete type:

```rust
struct Pair[A, B] =
  a A
  b B

type StrPair[T] = Pair[string, T] where T is ord && not bool

let p = StrPair[i32]{a: "hi", b: 42}   // ok
let q = StrPair[bool]{a: "hi", b: true} // error: T=bool fails `not bool`
```

Generic aliases resolve to their underlying struct at instantiation -
`StrPair[i32]{...}` expands to `Pair[string, i32]{...}` at codegen after
the alias's bounds are satisfied. Both alias bounds and the underlying
struct's bounds are enforced (a bound failing on either side fires with
the same `failing sub-check` error shape).

### Constructor inference for generic structs

Explicit type arguments are optional on a struct literal when they can be
inferred from the field values. The compiler unifies each provided field
value's type against the declared field type to bind the template's type
parameters:

```rust
struct Box[T] =
  value T

let a = Box{value: 42 as i32}     // Box[i32] inferred
let b = Box{value: "hello"}       // Box[string] inferred
let c = Box[f64]{value: 3.14}     // explicit also works
```

Multi-parameter structs work the same way, with each type parameter bound
by the first field in the literal that mentions it:

```rust
struct Pair[A, B] =
  a A
  b B

let p = Pair{a: 1 as i32, b: "hi"}    // Pair[i32, string]
```

Inference requires every type parameter to be reachable from the provided
fields. If a parameter is ambiguous (never mentioned by a literal field)
you get the existing "unknown struct type" error and should add an
explicit `[T]` annotation. Bound checks fire against the *inferred* type:

```rust
struct Num[T] where T is i32 || i64 =
  value T

let n = Num{value: "x"}    // error: type "string" does not satisfy
                           // constraint `where T is i32 || i64`
```

--

