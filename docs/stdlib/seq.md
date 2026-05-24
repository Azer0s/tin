# Seq

`stdlib/seq` is a curried, pipe-first toolbox for working with sequences.
The same operator names work in two shapes:

- **eager** -- `[t]` in, `[u]` out.  Materializes at every step.
- **lazy**  -- `Seq[t]` in, `Seq[u]` out.  Builds up an iterator chain;
  nothing actually runs until a terminal pulls.

Overload resolution at each pipe site picks the right form from the
shape of what's piped in, so the call sites read the same regardless
of which strategy you're on.

```rust
use seq
```

## Eager arrays

Every transform is curried -- it takes its non-data argument first
(closure, count, predicate, ...) and returns a closure that consumes
the array.  That shape is what makes the `|>` chains read top-down:

```rust
let r = [1, 2, 3, 4, 5, 6]
          |> seq::filter(fn(x i64) bool = return x % 2 == 0)
          |> seq::map(fn(x i64) i64 = return x * x)
          |> seq::sum
// r == 56  (2*2 + 4*4 + 6*6)
```

Transforms:

| Name | Shape | Notes |
|------|-------|-------|
| `map[t,u](f)` | `[t] -> [u]` | applies `f` element-wise |
| `filter[t](pred)` | `[t] -> [t]` | keeps elements where `pred` is true |
| `flat_map[t,u](f)` | `[t] -> [u]` | concatenates per-element arrays |
| `take[t](n)` | `[t] -> [t]` | first `n` elements |
| `skip[t](n)` | `[t] -> [t]` | drops the first `n` |
| `take_while[t](pred)` | `[t] -> [t]` | prefix where `pred` holds |
| `skip_while[t](pred)` | `[t] -> [t]` | drops the leading run, keeps the rest |
| `step_by[t](n)` | `[t] -> [t]` | every nth element |
| `chain[t](ys)` | `[t] -> [t]` | appends `ys` |
| `zip[t,u](ys)` | `[t] -> [(t,u)]` | pairs with `ys`, truncates to shorter |
| `enumerate[t](xs)` | `[t] -> [(i64,t)]` | NOT curried -- direct call |
| `chunks[t](n)` | `[t] -> [[t]]` | fixed-size groups, last may be smaller |
| `windows[t](n)` | `[t] -> [[t]]` | overlapping fixed-size windows |
| `dedup_by[t](eq)` | `[t] -> [t]` | collapses runs of equal-by-`eq` neighbours |
| `reverse[t](xs)` | `[t] -> [t]` | direct |
| `sort_by[t](cmp)` | `[t] -> [t]` | insertion sort on a copy |

Terminals:

| Name | Shape |
|------|-------|
| `count[t](xs)` | `[t] -> i64` |
| `sum(xs)` | `[i64] -> i64` |
| `sumf(xs)` | `[f64] -> f64` |
| `min_by[t](cmp)` | `[t] -> Option[t]` |
| `max_by[t](cmp)` | `[t] -> Option[t]` |
| `fold[t,u](init, f)` | `[t] -> u` |
| `reduce[t](f)` | `[t] -> Option[t]` |
| `find[t](pred)` | `[t] -> Option[t]` |
| `exists[t](pred)` | `[t] -> bool` |
| `all[t](pred)` | `[t] -> bool` |
| `first[t](xs)` | `[t] -> Option[t]` |
| `last[t](xs)` | `[t] -> Option[t]` |
| `nth[t](i)` | `[t] -> Option[t]` |
| `position[t](pred)` | `[t] -> Option[i64]` |
| `for_each[t](f)` | `[t] -> ()` |

Type arguments are inferred from the pipe LHS, so `seq::take[i64](2)`
and similar explicit annotations are normally redundant -- `xs |>
seq::take(2)` is enough.

## Lazy `Seq[t]`

`seq::Seq[t]` is the trait every iterator implements:

```rust
trait Seq[t] =
  fn next(this Seq[t]) Option[t]
```

A `Seq[t]` value is a trait fat-pointer `{data, vtable}`; the `data`
field stores the heap-allocated iterator state directly, so the same
chain composes without per-step boxing.

Lazy entry points share their names with the eager ones -- the overload
picker disambiguates by the LHS:

```rust
let r = seq::range(1, 1_000_000)
          |> seq::filter(fn(x i64) bool = return x % 2 == 0)
          |> seq::map(fn(x i64) i64 = return x * x)
          |> seq::take(3)
          |> seq::sum
// r == 56  (same answer as the eager chain above)
```

Nothing in that pipeline runs `range`-end-to-end -- `take(3)` only ever
pulls three values through.

Lazy constructors:

| Name | Shape |
|------|-------|
| `range(s, e)` | `Seq[i64]` -- yields `s..e-1` |
| `iter[t](xs)` | `Seq[t]` -- adapt an array into a lazy seq |
| `to_array[t](s)` | `Seq[t] -> [t]` -- materialise back to an array |

Lazy transforms (all curried, returning `Seq[t] -> Seq[u]`):

`map`, `filter`, `take`, `skip`, `take_while`, `skip_while`, `chain`,
`step_by`.

Lazy terminals:

`count`, `sum`, `sumf`, `fold`, `reduce`, `find`, `first`, `nth`,
`for_each`, `to_array`.

The iterator structs themselves are also exported (`Range`, `Iter[t]`,
`MapSeq[t,u]`, `FilterSeq[t]`, `TakeSeq[t]`, `SkipSeq[t]`,
`TakeWhileSeq[t]`, `SkipWhileSeq[t]`, `ChainSeq[t]`, `StepBySeq[t]`) in
case you want to construct them by hand or write a `match` over a known
adapter shape.

## Custom iterators

Implement `Seq[t]` on any struct.  The trait declares pointer
receivers (`next` mutates iterator state), so impls must declare
`this *YourIter` to match.  Construct iterators with the explicit
`&YourIter{...}` form -- the trait fat-pointer stores the heap
pointer in its `data` slot and `*Self` impl methods see your live
storage.

```rust
use { Option } from option
use seq
use { Seq } from seq

struct Counter (Seq[i64]) =
  cur  i64
  step i64
  max  i64

  fn Seq[i64]::next(this *Counter) Option[i64] =
    if this.cur >= this.max:
      return None
    let v = this.cur
    this.cur = this.cur + this.step
    return Some(v)

fn counter(start i64, step i64, max i64) Seq[i64] =
  return &Counter{cur: start, step: step, max: max}

fn main() i64 =
  let evens = counter(0, 2, 20)
                |> seq::filter(fn(x i64) bool = return x % 4 == 0)
                |> seq::to_array
  // evens == [0, 4, 8, 12, 16]
  return evens[2]
```

If you write `return Counter{...}` (value form) instead, the compiler
rejects it -- the receiver mismatch would silently heap-copy and the
mutations would land on a hidden copy.  Use `&Counter{...}` and the
trait fat-ptr's `data` field stores the heap pointer directly.

## Choosing eager vs lazy

- Reach for eager when the source array already fits in memory and the
  pipeline is small enough that allocating intermediates is fine.  The
  generated code is simpler and the chain runs in one straight pass.
- Reach for lazy when the source is large, when you only need a prefix
  of the output (`take(n)`, `find(...)`, `first`), or when you want to
  compose iterators returned from different functions without forcing
  materialization between them.

Both forms ship under the same `seq::*` names; converting from one to
the other is `seq::iter(arr)` / `arr |> seq::iter` (array -> lazy) and
`s |> seq::to_array` (lazy -> array).
