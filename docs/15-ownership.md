# Memory model: ARC and the borrow optimizer

Tin manages memory automatically via **reference counting (ARC)**. You
never call `free()`, you never write `unsafe`, and you never annotate
lifetimes. Every value is automatically retained when copied and released
when it goes out of scope.

On top of that, the compiler runs a **borrow analysis** at compile time
that elides redundant retain/release calls when it can prove they are not
needed. The borrow analysis is purely an optimization - it never changes
what your program does, only how many rc operations it takes to get
there.

> **This is not a borrow checker.** A Rust-style borrow checker replaces
> garbage collection with compile-time lifetime rules and rejects code
> that violates them. Tin's borrow analysis optimizes the rc traffic ARC
> would emit otherwise. Code that compiles in Tin is always memory-safe;
> the only question the analyzer answers is "how much rc bookkeeping is
> necessary?"

The fallback when the analyzer is unsure is the same retain/release pair
plain ARC would have emitted. No leak risk, no use-after-free, no
silently broken correctness - just slightly more rc work than the ideal.

## Conceptual model

Every binding in Tin has an **ownership state** decided by the compiler:

- **Owned**: holds a +1 reference to an rc-tracked block. Retains at
  creation; releases at scope exit.
- **Borrowed**: aliases a value whose lifetime is guaranteed by another
  binding. No retain, no release.
- **Moved**: ownership has been transferred. No release at scope exit.

You do not write these annotations. The compiler picks the cheapest
state that is safe. If unsure, it falls back to `Owned` (same as plain
ARC).

## For programmers from other languages

If you have used any of these, the mental model carries over with one
adjustment: in Tin, the compiler does extra work to make ARC cheaper.

| If you are used to...                  | In Tin...                                                                            |
| -------------------------------------- | ------------------------------------------------------------------------------------ |
| Garbage collection (Go, JS, Python)    | ARC instead - reference-counted at compile time, no GC pause, deterministic destructors |
| Manual `free()` (C)                    | None of that - ARC handles every allocation                                         |
| Swift's ARC                            | Same model. Tin's borrow analysis is what Swift's compiler does silently            |
| OCaml/Haskell tracing GC               | Different mechanism (RC vs tracing), same hands-off ergonomics                       |
| C++ `std::move`                        | Optional `move` keyword for the same idea, but never required                        |
| `&x` for references                    | `&x` produces `*T` (an rc-tracked pointer), same syntax                              |
| `let` / `const` for immutability       | `let` works the same; orthogonal to ownership                                        |

The closest analog is **Swift**. Tin's runtime is ARC, just like
Swift's; the borrow analysis is what Swift's compiler does invisibly to
elide retain/release pairs. If you have shipped Swift code, Tin's
memory model will feel familiar.

## For programmers from Rust

Tin trades Rust's compile-time guarantees for runtime ARC. Code that
will not compile in Rust often does compile and run correctly in Tin -
just paying a small ARC overhead for the cases where Rust's static
analysis would have proven the rc redundant. The borrow optimizer
reclaims much of that overhead automatically.

| Rust concept                             | Tin equivalent                                            | Difference                                                    |
| ---------------------------------------- | --------------------------------------------------------- | ------------------------------------------------------------- |
| Ownership rules (one owner, lifetimes)   | ARC (shared ownership via rc)                             | Tin has no "one owner" rule; shared ownership is the default  |
| Borrow checker compile errors            | None                                                      | Tin's borrow analysis only elides rc traffic; cannot error    |
| `&x` (immutable borrow with lifetime)    | `&x` produces `*T` (rc-tracked pointer)                   | Tin pointers are kept alive by ARC; no lifetime annotations   |
| `&mut x` (exclusive mutable borrow)      | No syntactic equivalent                                   | Tin has no exclusive-borrow checking                          |
| `move \|x\| ...` for closures            | Closures captured according to analyzer; no keyword needed | Optional `move` is a perf hint, not enforcement              |
| `let y = x` consumes x (move semantics)  | `let y = x` copies (analyzer may elide)                   | Use-after isn't a default error; only after explicit `move x` |
| `Box<T>`                                 | Heap by default for reference types                       | No `Box` type                                                  |
| `Rc<T>` / `Arc<T>`                       | Built-in for every reference type                         | Tin's rc is always atomic                                      |
| `unsafe { }` block                       | Does not exist                                            | No unsafe escape hatch - ARC always applies                   |
| Manual lifetime annotations (`'a`)       | None                                                      | Compiler infers everything                                     |

Rust prevents memory errors at compile time and emits zero runtime rc
work. Tin emits ARC rc work and uses an analyzer to prove which calls
are redundant. Same end goal (cheap correct memory management),
different cost model and different ergonomics.

## The `move` keyword

Tin has an optional `move` keyword that is a **perf hint to the
optimizer**, not an ownership-transfer primitive. You never need to
write it for correctness. You write it when you want to guarantee no
retain happens.

### Canonical case: fresh allocation into consumer

```rust
fn one_shot() =
  let s = build_big_thing()    // rc=1 (fresh)
  hand_off(move s)             // zero ops; callee owns the original rc=1
```

Without `move`: the call site emits a retain (to give the callee its own
reference) and the caller's scope exit emits a release. With `move`: the
rc=1 flows straight from `build_big_thing` into the callee and gets
dropped there. Net savings: a retain plus the caller's redundant
release.

After `move x`, `x` is invalidated:

```rust
let s = build_big_thing()
hand_off(move s)
echo s   // ERROR: use of moved value `s`
```

The optimizer also picks move automatically when it can prove a binding's
last use is a transfer. The keyword is for cases where (a) you want to
guarantee the optimization regardless of inference, (b) you want the
consumption to be visible in source for readers, or (c) a
`-Wmove-suggested` warning told you a manual hint would help.

### Where `move` does not apply

- **Iterator bindings**: `for item in xs:` exposes a view into xs, not
  ownership. `move item` is a compile error. Extract first:
  ```rust
  for item in xs:
    let owned = item     // copy (retain)
    move owned           // now allowed
  ```
- **Function parameters**: callee does not own them; the caller does.
- **Captured-by-reference closure variables**: same reason.

The rule: **`move` requires a binding the current scope owns.** Partial
moves (`move x.field`) are not supported - extract the field to its own
binding first, or take a pointer (`let p = &x.field`).

## Common patterns

### Pass-through reads (zero rc traffic)

```rust
fn render(state State) string =
  let header = state.header
  let body   = state.body
  return header ++ "\n" ++ body
```

`header` and `body` are borrowed (read-only, never escape). No retain
anywhere. The `++` consumes its operands and returns a fresh owned
string.

### Fresh allocation into consumer (one `move`)

See the canonical case above. This is where `move` shines.

### Iterator into sink (extraction + move)

```rust
for item in xs:
  let owned = item     // retain (the only mandatory rc op)
  sink(move owned)     // callee skips entry retain
```

Saves one retain/release pair vs naive ARC. The array still holds its
own reference to each element, so the extraction retain is unavoidable -
but the consumer-side pair is.

### Closure captures (field-narrowed)

When a closure or fiber body only accesses specific fields of a captured
variable, the analyzer captures **just those fields**, not the whole
variable:

```rust
fn handle(req Request) =
  spawn fn() = log(req.method)   // captures only req.method (a string)
  echo req.url                    // req stays borrowable; no extra rc traffic
```

The fiber's env holds the one string `req.method`, not the whole
`Request` struct. The retain happens on that string only. `req` itself
remains borrowed in `handle`.

### When the optimizer cannot narrow the capture

```rust
fn handle(req Request) =
  spawn fn{#async}() = 
    validate(req)     // closure passes the whole `req`
  // -> analyzer captures req in full, promoted to owned
```

When the closure passes the whole variable somewhere (a function call
taking the whole struct, an `&req`, or generic dispatch), the capture
cannot be narrowed and `req` is promoted to owned. No leak, no UAF;
just a retain you did not get to skip. The analyzer stays silent here -
whole-capture is the conservative default, not a problem to surface.

## Across function boundaries

The analyzer infers each function's ownership contract automatically:
which parameters it treats as borrows vs owned, whether the return is
fresh-owned or a borrow of a parameter. Callers consult that contract
when classifying their own bindings. So:

```rust
fn print_path(p string) =
  echo p                     // p only read; analyzer infers `p: borrow`

fn main() =
  let cfg = load_config()
  print_path(cfg.path)       // caller knows print_path borrows; no rc bump
```

The contract is inferred, never written. You see it through
`--explain-ownership`:

```
fn print_path:
  param p   borrow  (read-only in body)
  return    -       (no value)
```

For recursive functions, the analyzer iterates until the inferred
contracts stabilize. For mutually-recursive groups, the whole group is
analyzed together. You do not need to think about either case.

## When the analyzer is unsure

The fallback is **owned** - same as plain ARC. This guarantees:

- **No memory leak** beyond what ARC produces.
- **No use-after-free** at any optimization level.
- **No silent loss of correctness**; the worst case is performance equal
  to (or slightly worse than) plain ARC.

Compile-time warnings nudge you toward patterns the analyzer can
optimize. Runtime checks (default on, stripped by `--release`) validate
the optimizer's decisions on every elided pair.

## See also

- [15-ownership-borrowing.md](./15-ownership-borrowing.md) - diagnostic
  flags, warning ladder, `--release` mode.
- [internals/memory.md](./internals/memory.md) - the ARC runtime model.
- [internals/clayout-structs.md](./internals/clayout-structs.md) -
  cLayoutStruct wrapper layout (special case at the FFI boundary).
