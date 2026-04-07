# Floats

`stdlib/floats` provides the three IEEE 754 special values and pure-Tin
predicates for testing them.

> **Linker flag:** `floats` links against `-lm` via `//!-lm`. If your program
> already passes `-lm` you don't need to add it again.

## Import

```rust
use floats
```

---

## Special values

These are functions rather than constants because IEEE 754 special values
cannot be expressed as `f64` literals:

| Function | Value | Description |
|----------|-------|-------------|
| `floats::NaN()` | `NaN` | Not-a-Number; result of `0/0`, `sqrt(-1)`, etc. |
| `floats::Inf()` | `+∞` | Positive infinity; result of `1/0`, overflow |
| `floats::NegInf()` | `-∞` | Negative infinity; result of `-1/0` |

```rust
let n = floats::NaN()
let p = floats::Inf()
let m = floats::NegInf()
```

> **NaN comparison:** `NaN != NaN` is always `true` in IEEE 754, but Tin's
> `!=` operator on `f64` uses an ordered comparison that returns `false` when
> either operand is NaN. Always use `floats::is_nan()` instead of `x != x`.

---

## Predicates

All predicates return `bool`:

```rust
floats::is_nan(x)      // true if x is NaN
floats::is_inf(x)      // true if x is +∞ or -∞
floats::is_pos_inf(x)  // true if x is +∞
floats::is_neg_inf(x)  // true if x is -∞
floats::is_finite(x)   // true if x is neither NaN nor infinite
```

Example:

```rust
fn safe_div(a f64, b f64) f64 =
  let r = a / b
  if !floats::is_finite(r):
    return 0.0
  return r
```

---

## Reference

| Function | Returns | Description |
|----------|---------|-------------|
| `NaN() f64` | NaN | Not-a-Number |
| `Inf() f64` | +∞ | Positive infinity |
| `NegInf() f64` | -∞ | Negative infinity |
| `is_nan(x f64) bool` | - | True if `x` is NaN |
| `is_inf(x f64) bool` | - | True if `x` is ±∞ |
| `is_pos_inf(x f64) bool` | - | True if `x` is +∞ |
| `is_neg_inf(x f64) bool` | - | True if `x` is -∞ |
| `is_finite(x f64) bool` | - | True if `x` is a normal finite number |
