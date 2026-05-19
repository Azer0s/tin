# 04 - Collections: capacity, aliasing, copy

Companion to [04-collections.md](04-collections.md) covering the
mutation model that backs `++=`, `copy(...)`, and the cap-check
panic. The base file documents the syntax; this one covers the
*semantics* writers care about when slices outlive a single scope.

## Capacity (`cap`)

`cap(arr) i64` returns the allocated headroom of a string or dynamic
array. For owned/growable arrays, `cap >= len`; the slack is the
extra space `++=` can fill in place before the next reallocation.

```rust
let xs [i64] = []
cap(xs)         // 0  (empty growable)

xs ++= [1]
cap(xs)         // 1
xs ++= [2]
cap(xs)         // 3  (geometric growth: doubles + 1 on each grow)
xs ++= [3]
cap(xs)         // 3  (in-place; cap stays the same)
xs ++= [4]
cap(xs)         // 7
```

Three special sentinels carry intent on top of the integer value:

| `cap`  | meaning                  | indexed write | `++=`     |
|--------|--------------------------|---------------|-----------|
| `>= 0` | owned, growable          | OK            | OK        |
| `-1`   | borrowed view / immortal | OK            | **panic** |

`fieldnames(x)` / string literals / atom literals all live in
immortal read-only storage and carry `cap == -1`. `++=` on them
panics at runtime so a careless append doesn't silently break the
alias the caller still holds (see [Aliasing](#aliasing-and-copy)
below).

## Array fill (`[v] * n`)

`[v] * n` allocates a fresh `[T]` with `n` copies of `v`. The count
may be any `i64` expression - compile-time literals collapse to an
inline store sequence, runtime values lower to one `_tin_rc_alloc`
plus a fill loop.

```rust
let zeros [f64] = [0.0] * 1024
let buf   [i64] = [42] * (rows * cols)
let pad   [string] = [" "] * indent
```

Equivalent to the literal form `[v; N]` when `N` is a compile-time
integer; the `[v] * n` shape is the only form that accepts a runtime
count.

## Aliasing and `copy`

Tin's arrays and strings are fat pointers; passing or assigning
**shares the buffer**. Writes via the second name reach through to
the first:

```rust
let a [i64] = [1, 2, 3]
let b = a
b[0] = 99
a[0]    // 99 -- both names refer to the same buffer
```

`copy(arr) [T]` (or `copy(s) string`) allocates a fresh owned
duplicate. For RC-tracked element types every element gets a retain
so the copy and the source release independently.

```rust
let a [i64] = [1, 2, 3]
let b = copy(a)
b[0] = 99
a[0]    // 1   -- alias is broken
b[0]    // 99
```

Pair `copy` with `cap` to upgrade an immortal/borrowed view to an
owned, growable buffer:

```rust
let s = "hello"     // cap == -1 (immortal)
let m = copy(s)     // cap == 5 (owned)
```

The compiler warns at the alias site under `-Wpedantic` /
`-Walias-mutation`:

```
warning: writing to "b" via indexed assignment also mutates "a"
         (shared buffer); use `let b = copy(a)` to break the alias
         [-Walias-mutation]
```

## Slicing (`arr[lo..hi]`)

Slicing `[T]` **always copies**. The result is a fresh, owned `[T]`
that doesn't alias the source - `++=` and indexed writes are safe
without `copy(...)`:

```rust
let a [i64] = [10, 20, 30, 40]
let s = a[1..3]   // fresh buffer: [20, 30]
s[0] = 99
a[1]              // 20 (unchanged - independent buffer)
cap(s)            // 2  (cap == len; first ++= triggers a grow)
```

Both bounds are required. For "from index to end" write
`arr[lo..len(arr)]`; for "from start to index" write `arr[0..hi]`.
The same `..` is the range operator used by `for i in 0..n`, so the
slice and loop forms read consistently. The older `[lo:hi]` form is
rejected by the compiler with a hint to use `..`.

For RC-tracked element types every copied element is retained so the
slice's release path doesn't double-free.

The `*byte`/raw-pointer form `ptr[lo..hi]` produces an owned `[byte]`
via `_tin_bytes_from_buf`; the source pointer is not retained.

## Empty arrays and capacity

The literal `[]` allocates nothing - it produces a fat-pointer with
`len == 0` and `cap == 0`. The first `++=` allocates the backing
buffer:

```rust
let xs [i64] = []
cap(xs)         // 0
xs ++= [1]      // first append: allocates
cap(xs)         // 1
```

`[]` is *not* an immortal literal; `++=` succeeds because `cap == 0`
is still `>= 0`. Only `cap == -1` triggers the borrowed-view panic.

## Runtime cap-checks

`++=` checks `cap >= 0` at runtime before allocating. A panic fires
when the target buffer is borrowed or immortal (`cap < 0`) - the
program would otherwise silently allocate a fresh buffer the caller
never sees through the original binding. The check costs one icmp +
branch per `++=` site and can be stripped with `--no-runtime-checks`
for tight loops that have been audited to only `++=` owned slices.

```
panic: `++=` requires an owned slice (cap >= 0); the target is a
       borrowed view or immortal literal -- create an owned copy
       first with copy(...)
```

## Implementation note

`++=` amortises to O(1): the in-place fast path memcpys the appended
bytes into the existing buffer when `cap >= len + needed` and the
ARC reference count on the data pointer is 1; otherwise the grow
path allocates with geometric headroom (`cap = max(needLen,
2 * oldCap + 1)`). The shared-buffer guard (`rc == 1`) is what makes
the optimization safe with Tin's pass-shared semantics: appends to
the aliased binding take the grow path and the original observer
sees no change.
