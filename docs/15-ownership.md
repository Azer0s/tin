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
  creation (when needed); releases at scope exit.
- **Borrowed**: aliases a value whose lifetime is guaranteed by another
  binding. No retain, no release.
- **Moved**: ownership has been transferred. No release at scope exit;
  the binding is invalidated and any subsequent read is a compile error.

You do not write these annotations. The compiler picks the cheapest
state that is safe. If unsure, it falls back to `Owned` (same as plain
ARC).

**Function bodies are rc-neutral on their parameters** in the common
case. When the body never reassigns a parameter, the body emits no
entry retain and no scope-exit release on that parameter. The caller's
binding keeps the rc share alive throughout the call, so the body can
freely read, sink, or pass through without bookkeeping. When the body
*does* reassign a parameter (`s = some_other_value` against the param
name), the compiler reverts to the classical owned model for that
parameter: entry retain on `s`, scope-exit release on `s`, and normal
release-old/retain-new on every subsequent assign.

A function that returns a non-reassigned parameter directly emits one
retain at the return site so the caller's receiving binding gets a
share of its own (separate from the caller's source binding).

## The call site is where rc work happens

The analyzer infers a per-parameter **convention** for every function
based on what the body does:

- **transparent**: read-only - the body never sinks and never reassigns.
- **consumes**: the body has a sink that retains the value (a field
  store, channel send, captured-in-escaping-closure, etc.) but does
  not reassign the parameter.
- **retains**: the body reassigns the parameter (or escapes the rc in
  a way the analyzer cannot track precisely).

The convention shows up in `--explain-ownership` and gates the `ref`
keyword (only `transparent` permits `ref`). It does **not** drive
extra rc work at the call site - the body is responsible for its own
sink/reassign accounting, and the caller's side stays simple:

| call form          | what the caller emits                             |
|--------------------|----------------------------------------------------|
| `f(a)` default     | borrow if `a` is still live after; move if it's `a`'s last use (Phase F implicit move) |
| `f(ref a)`         | borrow (compile error if convention != transparent) |
| `f(move a)`        | post-call release; skip caller scope-exit         |
| `f(copy(a))`       | deep-copy `a` into a fresh temp; pass the temp default |

The default form auto-selects between borrow-style (no boundary rc)
and move-style (post-call release) based on the liveness pre-pass.
The callee's own classification (transparent / consumes / retains)
gates the `ref` keyword but does **not** affect the caller's emission:
the same call site emits the same boundary rc regardless of what the
callee does internally.

**Value semantics for every value type.** A parameter passed by
value (no leading `*`) is fully isolated from the caller's binding
when the callee mutates it - regardless of whether the value is a
struct, array, string, or ADT. The compiler emits an automatic
deep-copy at the call boundary whenever the callee's body mutates
the parameter and the binding is still live after the call. The
rule applies uniformly:

| arg shape       | callee mutates? | what happens                  |
|-----------------|-----------------|-------------------------------|
| `f(value)`      | no              | shared (zero rc work)         |
| `f(value)`      | yes, last use   | move (transfer rc)            |
| `f(value)`      | yes, still live | deep-copy at call boundary    |
| `f(&value)`     | (anything)      | explicit sharing via pointer  |

The same rule covers method calls: `value.method()` deep-copies
the receiver when the method body mutates `this`.

**To opt into Go-style buffer sharing**, declare the parameter as a
pointer (`fn f(arr *[T])`) and pass `f(&arr)` at the call site.
Pointers say "I mean this to share"; bare value types say "this
is mine."

Deep-copy machinery handles all the shapes the compiler knows
how to walk: scalars are bit-copied, string and `[T]` buffers get
fresh allocations via `_tin_rc_alloc`+memcpy, `[Struct]` elements
recurse per-element, nested struct fields recurse through the
per-struct generator, and ADT (data) values dispatch on the
variant tag and deep-copy the active variant's payload fields.
Pointer fields and `any` fields stay shared by reference (those
are explicit sharing shapes).

You never have to write `move` or `copy` to get the optimization -
the keywords are for when you want use-after-move enforcement or to
make the transfer visible in source.

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
| Borrow checker compile errors            | None for inferred conventions; explicit `ref`/`move` are checked | Inference can never error; the keywords are opt-in assertions |
| `&x` (immutable borrow with lifetime)    | `&x` produces `*T` (rc-tracked pointer)                   | Tin pointers are kept alive by ARC; no lifetime annotations   |
| `&mut x` (exclusive mutable borrow)      | No syntactic equivalent                                   | Tin has no exclusive-borrow checking                          |
| `move \|x\| ...` for closures            | Closures captured according to analyzer; no keyword needed | Optional `move` is a perf hint, not enforcement              |
| `let y = x` consumes x (move semantics)  | `let y = x` copies (analyzer may elide); ref/move are call-site keywords | Use-after isn't a default error; only after explicit `move x` |
| `Box<T>`                                 | Heap by default for reference types                       | No `Box` type                                                  |
| `Rc<T>` / `Arc<T>`                       | Built-in for every reference type                         | Tin's rc is always atomic                                      |
| `unsafe { }` block                       | Does not exist                                            | No unsafe escape hatch - ARC always applies                   |
| Manual lifetime annotations (`'a`)       | None                                                      | Compiler infers everything                                     |

Rust prevents memory errors at compile time and emits zero runtime rc
work. Tin emits ARC rc work and uses an analyzer to prove which calls
are redundant. Same end goal (cheap correct memory management),
different cost model and different ergonomics.

## The call-site keywords

Tin has three optional keywords that you can write at a call site to
constrain how the call lowers. None of them are required for
correctness - the analyzer picks the right semantics automatically.
You write them when you want to make your intent explicit or to lock
in an optimization regardless of inference.

```
f(a)         // default: analyzer-inferred per the convention table
f(ref a)     // assert: zero rc traffic at this call
f(move a)    // transfer a's rc into the callee
f(copy(a))   // force a retain before the call, regardless of inference
```

All three are **call-site-only**. They live on the caller, not in the
callee's signature - the same function can be called four different
ways depending on what each call site wants.

### `ref a` -- assert borrow

`ref a` is the explicit "this call must not bump the rc" assertion.
The compiler accepts it only when the callee's convention is
`transparent`; otherwise it's a compile error:

```rust
fn store_box(p *Place, b Box) =
  p.box = b      // sink: convention(b) = consumes

let a = make_box()
store_box(p, ref a)
//          ^^^^^ error: cannot pass `ref a` to `store_box`:
//                parameter `b` consumes its rc
```

For a read-only callee, `ref` is a no-op assertion - the analyzer
already would have produced zero rc traffic. The keyword is for
locking that in so a future edit that adds a field-store to the
callee body becomes a localized error instead of a silent retain at
the call site.

### `move a` -- transfer ownership

`move a` transfers the caller's rc into the callee. After the call,
`a` is invalidated and any subsequent read is a compile error.

```rust
let s = build_big_thing()
hand_off(move s)
echo s   // error: use of moved value `s`
```

Validity of `move`:
- The source must be an Owned local binding. Parameters, iterator
  bindings, globals, and `ref` bindings cannot be moved.
- Partial moves (`move x.field`) are not supported - extract first.

The lowering is uniform across conventions: one post-call release at
the call site, caller scope-exit on `a` skipped. The body's per-class
behavior (no rc work on non-reassigned params, full owned model on
reassigned params) handles its own accounting independently. Math
balances either way: every rc the body or its sinks added is matched
by the caller's post-release plus the body's own scope-exit on whatever
local the slot holds at the time.

You get the use-after-move compile check regardless of convention.

The optimizer also picks move automatically when it can prove a
binding's last use is a transfer - you don't have to write `move` to
get the optimization. Write it when:

1. You want a compile-time guarantee that the binding is dead after
   this call (use-after-move enforcement).
2. You want the consumption to be visible in source for readers.
3. `-Wmove-suggested` flagged a call site where the optimizer would
   have applied move and a manual hint would make it explicit.

### `copy(a)` -- deep copy + default call

`copy(a)` lowers to a deep copy of `a` into a fresh allocation
(`rc=1`) and then passes that fresh temp through the default call
protocol. The original `a` is untouched - it keeps its rc and
remains usable after the call.

```rust
let a = [1, 2, 3]
mutate(copy(a))   // callee receives an independent buffer
echo a            // still [1, 2, 3]
```

Use it when the caller wants the callee's modifications to be
isolated from the original binding, or when you want to defeat the
implicit-move-on-last-use heuristic at a specific call site.

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

### Fresh allocation into consumer (move)

```rust
fn one_shot() =
  let s = build_big_thing()
  hand_off(move s)
```

`build_big_thing` returns rc=1; `move s` transfers that rc straight
into `hand_off` without an extra retain at the call site. If
`hand_off` consumes (stores in a field), the body's field-store
takes the rc directly. If it just reads, the caller drops the rc
right after the call returns.

### Iterator into sink (extraction + move)

```rust
for item in xs:
  let owned = item     // retain (the only mandatory rc op)
  sink(move owned)     // callee's sink takes the rc; no extra traffic
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

The analyzer infers each function's per-parameter convention
automatically: whether the body treats the param as transparent,
consumes its rc into a sink, or retains an additional reference. The
convention drives the `ref` keyword's compile-time check and the
body-side rc model (borrowed body for transparent/consumes,
classical owned body for retains). The caller's call-site code
chooses between borrow (no boundary rc) and move (post-call release)
based purely on the binding's liveness.

```rust
fn print_path(p string) =
  echo p                     // p only read; convention(p) = transparent

fn main() =
  let cfg = load_config()
  print_path(cfg.path)       // zero rc work at the call site
```

The convention is inferred, never written. You see it through
`--explain-ownership`:

```
fn print_path:
  param p    transparent  (read-only in body)
fn main:
  let cfg    owned         (returned from load_config)
```

For recursive functions, the analyzer iterates until the inferred
conventions stabilize. For mutually-recursive groups, the whole group
is analyzed together. You do not need to think about either case.

## When the analyzer is unsure

The fallback is **owned** + `retains` convention - same as plain ARC.
This guarantees:

- **No memory leak** beyond what ARC produces.
- **No use-after-free** at any optimization level.
- **No silent loss of correctness**; the worst case is performance equal
  to (or slightly worse than) plain ARC.

Compile-time warnings nudge you toward patterns the analyzer can
optimize. Runtime checks (opt-in via `--check=`) validate the
optimizer's decisions on every elided pair.

## See also

- [15-ownership-borrowing.md](./15-ownership-borrowing.md) - diagnostic
  flags, warning ladder, the call-site keyword error catalog.
- [internals/memory.md](./internals/memory.md) - the ARC runtime model.
- [internals/memory-arc-codegen.md](./internals/memory-arc-codegen.md) -
  where the compiler emits retains and releases.
