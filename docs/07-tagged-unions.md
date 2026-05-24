# Tagged unions


A tagged union is a `type` alias over two or more types joined with `|`:

```rust
type num    = i64 | f64
type strnum = string | i64
```

**Memory layout:** `{ i8 tag, [maxSize x i8] payload }` where tag 0 = first
variant, 1 = second. The payload is sized to the largest variant.

### Assigning values

Assignment automatically selects the correct tag by matching the value's type:

```rust
let a num = 42        // tag=0 (i64)
let b num = 3.14      // tag=1 (f64)
```

### Testing with `is`

```rust
if a is i64:          // type check only
  echo "integer"

if a is n i64:        // binds n as i64 if the check passes
  echo n
```

### Matching on a tagged union

`match x.(type)` dispatches on the active tag and binds the payload:

```rust
match a.(type):
  case n i64:
    echo n
  case x f64:
    echo x
  default:
    echo "other"
```

### Tagged unions as generic type arguments

`type X = A | B` plays two roles:

1. A **tagged-union type** when you have a value of it. The value carries a
   tag and is laid out as `{ i8 tag, payload }`.
2. A **type-set alias** when used in a where-guard. `where t is X` is
   satisfied by any structural variant of X.

Both meanings are preserved when X appears as a generic type argument. A
generic instantiated with X stores the **literal tagged union** (with the
tag), not one of the underlying variants:

```rust
use sync

type num = i64 | f64

let a num = 42
let atom = sync::Atomic[num].new(a)   // Atomic stores num, not i64
let v = atom.load()                   // v is num, tag preserved

match v.(type):
  case n i64: echo "i64 {n}"
  case f f64: echo "f64 {f}"
```

### Where-guards over tagged unions

`where t is X` matches **either** the literal X or any of its structural
variants, so a single overload can cover both forms:

```rust
type num = i64 | f64

struct Box[t] =
  v t
  fn show(this Box[t]) string where t is num =
    match this.v.(type):
      case n i64: return "i64 {n}"
      case f f64: return "f64 {f}"

let b1 = Box[num]{v: 42 as num}    // t = num   -> matches `where t is num`
let b2 = Box[i64]{v: 7}            // t = i64   -> also matches (i64 is a variant of num)
let b3 = Box[f64]{v: 3.14}         // t = f64   -> also matches
```

Inside a `where t is X` body, `t` is only known to be one of X's variants -
it has no single concrete type. Use `match v.(type)` to recover the
concrete variant. The compiler will not let you call type-specific operations
on a `t`-typed value without one.

### Compile-time match resolution

When the generic is instantiated with a **single concrete variant** (e.g.
`Box[i64]` for `where t is num`), `match v.(type)` is statically
resolvable. The compiler keeps only the arm whose type matches the
substituted `t` and dead-strips the rest -- no tag load, no switch, no
branch. The arms for unreachable variants don't even reach the IR.

```rust
let bi = Box[i64]{v: 42}
echo bi.show()   // emits the i64 arm body only; the f64 arm is gone

let bn = Box[num]{v: 42 as num}
echo bn.show()   // emits the full tag-dispatched switch (real tagged union)
```

If no arm matches the concrete type:

- with a `default:` arm, the default body becomes the only emitted code.
- without a `default:` arm, the compiler errors out -- the user wrote a
  match that proves the instantiation is wrong (e.g. `Box[bool]` for a
  `where t is num` body matching only i64/f64).

This means `match v.(type)` inside a where-guarded function is free at
runtime when the type-arg is concrete and pays only for the variants you
actually write when the type-arg is the literal tagged union.

---

