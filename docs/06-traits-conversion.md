# Conversion traits  -  `implicit[T]` and `coerce[T]`

Two op-traits cover *implicit* conversion in either direction. Listing one
of them in a struct's implements list registers a static method that the
compiler dispatches at the relevant call site automatically.

| Direction      | Trait         | Impl signature                 | Triggered at                  |
|----------------|---------------|--------------------------------|-------------------------------|
| Other -> struct | `implicit[T]` | `static fn ::implicit(t T) S`  | `let x S = t` and arg-passing |
| Struct -> other | `coerce[T]`   | `static fn ::coerce(this S) T` | `s as T`                      |

Both traits sit outside the regular vtable machinery: there is no `Trait`
fat-pointer view of `implicit` or `coerce`. The compiler stores the
registered static fn in a side-table keyed on the (struct, target type)
pair and inlines a direct call when the conversion fires.

### `implicit[T]` - "T flows into S"

`implicit[T]` makes a struct accept a foreign value as if it were already
the struct's type. The user writes `let x S = t` (where `t : T`) and the
compiler quietly inserts a call to the registered `static fn ::implicit(t
T) S`. The same coercion path drives function arguments and return values,
so any place an `S` is expected an `implicit[T]`-registered `T` is also
accepted.

```rust
struct Cents(implicit[i64], implicit[f64]) =
  amount i64

  static fn ::implicit(n i64) Cents = return Cents{amount: n}
  static fn ::implicit(x f64) Cents = return Cents{amount: (x * 100.0) as i64}

let c1 Cents = 250        // implicit[i64]
let c2 Cents = 1.99       // implicit[f64]
fn pay(c Cents) = ...
pay(42)                    // implicit[i64] at the call site
```

A struct may list multiple `implicit[T]` instantiations for distinct source
types - the compiler picks the impl whose param type matches the source.
Two impls with the *same* source type are indistinguishable and would be a
compile error at the `let` site.

The decimal stdlib uses this pattern to make `Value` accept ints, floats,
and strings transparently:

```rust
struct Value(implicit[i32], implicit[i64], implicit[f32], implicit[f64], implicit[string]) =
  coeff i64
  scale i32
  static fn ::implicit(n i64)    Value = ...
  static fn ::implicit(x f64)    Value = ...
  static fn ::implicit(s string) Value = ...
```

### `coerce[T]` - "S flows into T"

`coerce[T]` is the symmetric direction: writing `s as T` (where `s : S`)
calls the registered `static fn ::coerce(this S) T` and substitutes its
return value for the cast. This is the *only* place `as` dispatches to
user code; built-in numeric / pointer casts continue to short-circuit
ahead of trait lookup.

```rust
struct Money(coerce[i64], coerce[f64], coerce[string], coerce[bool]) =
  cents i64

  static fn ::coerce(this Money) i64    = return this.cents
  static fn ::coerce(this Money) f64    = return (this.cents as f64) / 100.0
  static fn ::coerce(this Money) string = return "${this.cents / 100}.{this.cents % 100}"
  static fn ::coerce(this Money) bool   = return this.cents != 0

let m = Money{cents: 1234}
let n = m as i64       // 1234
let d = m as f64       // 12.34
let s = m as string    // "$12.34"
let on = m as bool     // true
```

Because `coerce[T]` impls all share the receiver shape `(this S)`, plain
overload mangling on the parameter signature would collapse them. The
compiler special-cases `static fn ::coerce` and folds the return type
into the overload signature, so `coerce[i64]` and `coerce[string]` survive
side-by-side on the same struct.

`coerce` is value-form only: the receiver is `this S`, not `*S`. To convert
*through* a pointer, deref first - `(*p) as T` finds the registered impl
on `S`. A `static fn ::coerce(this *S) T` shape is reserved for a future
extension (no current call site dispatches to it).

#### Auto `coerce[bool]` in conditionals

When a struct implements `coerce[bool]`, the compiler calls the coerce
method automatically wherever a *condition* is expected:

```rust
struct Bag(coerce[bool]) =
  items [string]
  static fn ::coerce(this Bag) bool = return len(this.items) > 0

let b = Bag{items: ["x"]}

if b: ...           // auto: dispatches to ::coerce(b) bool
for b: ...          // auto
let v = b ? 1 : 0   // auto (ternary)
```

The conditional auto-coercion is intentionally narrow: it fires only
in `if` / `else if` / `for` / `while` / ternary / match-guard sites.
Boolean *operators* (`&&`, `||`, `!`) deliberately do *not* auto-coerce
- a struct may overload them via its own op-traits (e.g. `not[ret]`),
and silently coercing to bool would mask that overload. To use a
bool-coercible struct in a boolean expression, write the cast
explicitly:

```rust
if b as bool && other_cond(): ...   // explicit
if b && other_cond(): ...           // error: cannot use Bag as a boolean operand
```

The rejection error includes the actionable hint to add `as bool` if
the user really wants the coerce dispatch.

### Combining `implicit` and `coerce`

A struct may implement both directions. Pairing them is how a wrapper type
participates in arithmetic with primitives without the user ever writing
an explicit cast:

```rust
struct Percent(implicit[f64], coerce[f64], coerce[string]) =
  ratio f64

  static fn ::implicit(x f64) Percent     = return Percent{ratio: x}
  static fn ::coerce(this Percent) f64    = return this.ratio * 100.0
  static fn ::coerce(this Percent) string = return "{this.ratio * 100.0}%"

let p Percent = 0.25       // implicit[f64]:  0.25 -> Percent{0.25}
let n = p as f64           // coerce[f64]:    Percent{0.25} -> 25.0
let s = p as string        // coerce[string]: "25.000000%"
```

`implicit` and `coerce` are independent registries: `implicit[T]` does not
imply a `coerce[T]` going the other way (or vice versa). List each
direction explicitly.

### Cast safety

Two compile-time checks back the trait conversions:

1. **Impossible casts are errors.** `e as T` where neither a built-in
   conversion nor a `coerce[T]` impl applies fails at the cast site with
   `cannot cast <S> to <T>: no conversion path` rather than silently
   propagating a type mismatch to the next slot.
2. **Mixed pointer/value casts are errors.** `*Trait as Concrete` (the
   pointer indirection thrown away) and `Trait as *Concrete` (a fresh
   address minted for an iface that has no stable storage) both reject at
   the cast site. Use `(*p) as T` to deref first or `*Trait` from the
   start.

### Redundancy warning

When the surrounding slot already pins the destination type, `<lit> as T`
is redundant - the compiler would have auto-coerced the literal anyway.
The `[-Wredundant-type-cast]` warning fires on:

- `let x T = <lit> as T`
- `let x [T; N] = [<lit> as T, ...]` and `[T]` arrays
- `let x (T1, T2) = (<lit1> as T1, <lit2> as T2)`
- `let x Result[T, E] = Ok(<lit> as T)` (and any other ADT variant slot)
- `fn foo() T = return <lit> as T` (and tuple / ADT return shapes)
- `f(<lit> as T)` when `f`'s parameter is `T`
- `Foo{x: <lit> as T}` (named *and* positional struct field initializers)

The check is type-aware: `"hello" as i64` is *not* flagged because string
has no coercion path to `i64` - the cast is impossible (and now errors).
Only literals whose natural kind fits the slot type are flagged.

---

