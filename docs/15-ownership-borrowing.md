# Ownership: diagnostics and runtime checks

Companion to [15-ownership.md](./15-ownership.md). This page covers the
flags, warnings, and runtime checks that let you see what the borrow
optimizer is doing and validate that it is doing the right thing.

## Warnings

The optimizer emits warnings when it had to back off from an
optimization it wanted to apply, or when an explicit `move` is invalid.
None of these are errors by default; the generated code is always
correct.

| Flag                          | Default level | When it fires                                                                              |
| ----------------------------- | ------------- | ------------------------------------------------------------------------------------------ |
| `-Wconservative-promotion`    | `-W2`         | A binding was promoted to owned because the analyzer could not prove a safer state.        |
| `-Wcopy-could-be-borrow`      | `-W2`         | A `let t = s` followed by read-only uses of `t` with no mutations; `t` could be a borrow.  |
| `-Wescaping-borrow`           | `-W2`         | A binding was borrow-eligible but the analyzer could not prove the source outlives it.     |
| `-Wclayout-pointer-extern`    | `-W2`         | A pointer-extern cLayoutStruct wrapper crossed a boundary that may outlive the C-side memory. |
| `-Wcycle-risk`                | `-W2`         | A struct definition admits an RC cycle in the static type graph (`A` -> `*B` -> `*A`).    |
| `-Wmove-suggested`            | `-W3`         | A fresh-allocation flows into a single consumer; explicit `move` would be clearer.         |

The following are hard errors, not warnings (no flag to disable):

| Error                         | When it fires                                                                              |
| ----------------------------- | ------------------------------------------------------------------------------------------ |
| `use-after-move`              | A binding is used after `move`.                                                            |
| `self-move`                   | `x = move x`.                                                                              |
| `move-in-loop`                | A move on a binding declared outside the loop; iteration 2 would be use-after-move.        |
| `move-after-use`              | `move x` when `x` was aliased earlier in the same scope (would invalidate the alias).      |
| `move-non-owning-binding`     | A `move` on a function parameter or an iterator binding - the scope does not own this binding. |

Every diagnostic carries a source span and offers all available
remediation paths. Example for `move-non-owning-binding`:

```
foo.tin:42:7: error: cannot move `s` (parameter, not owned by this scope)
   42  |   move s
       |   ^^^^^^
   note: parameters are owned by the caller; the callee borrows them
   help: drop the `move` keyword -- `s` works as-is; the compiler will
         pick the cheapest semantics automatically
   help: or, if you need explicit transfer, copy first:
              let owned = s
              move owned
```

For iterator bindings:

```
foo.tin:18:5: error: cannot move `item` (iterator binding, view into `xs`)
   18  |     move item
       |     ^^^^^^^^^
   note: iterator bindings are views into the container, not owners
   help: drop the `move` keyword -- `item` works as-is
   help: or, if you need owned semantics, copy first:
              let owned = item
              move owned
```

To promote warnings to errors for CI hygiene:

```
$ tin build -Werror=conservative-promotion src/main.tin
$ tin build -Werror src/main.tin   # all warnings at current -W level
```

## `--explain-ownership`

Prints the analyzer's decision for every binding in the program.
Useful when you want to confirm an optimization fired (or did not):

```
$ tin build --explain-ownership src/main.tin
fn handle_request:
  let req       owned   (returned to caller)
  let path      borrow  (read-only, dropped at scope exit)
  let method    borrow  (read-only, passed to log() which takes a borrow)
  let response  owned   (captured by fiber spawned at line 51)
  let cleanup   move    (last use at line 58, transferred to defer chain)
```

Scope to a single function:

```
$ tin build --explain-ownership=foo.tin:handle_request src/main.tin
```

## `--dump-rc`

Emits LLVM IR with ownership decisions attached as metadata comments.
For compiler and runtime debugging:

```
; binding `s` owned, alloc rc=1
%s = call i8* @_tin_alloc(...)
; binding `t` borrow of `s` -- no entry retain
%t = bitcast i8* %s to ...
; scope exit (line 50)
call void @_tin_release(i8* %s)
; --- no release for %t (borrow)
```

## Runtime checks (opt-in)

By default Tin builds emit **no runtime safety checks** - the binary is
production-shaped out of the box. Checks are opt-in via `--check=NAME`,
repeatable and comma-separated:

```
$ tin build src/main.tin                                       # no checks
$ tin build src/main.tin --check=rc-balance                    # one check
$ tin build src/main.tin --check=rc-balance,ownership-canary   # comma list
$ tin build src/main.tin --check=rc-balance --check=cycle-detect  # repeated
$ tin build src/main.tin --check=all                           # everything
$ tin build src/main.tin --check=all --no-check=heap-sample    # all except one
```

`tin --check=help` (or `tin help check`) prints the full list of
available checks.

### Available checks

| Name                | What it does                                                                          | Cost                                    |
| ------------------- | ------------------------------------------------------------------------------------- | --------------------------------------- |
| `rc-balance`        | At process exit, walk every allocation and report any with `rc > 0`.                  | One walk at exit; negligible during run |
| `ownership-canary`  | Validate every elided retain/release pair: assert the source was alive at elision.    | One check per elided pair; runtime hot  |
| `append-cap`        | Check `++=` on a borrowed-view slice (cap < 0).                                       | One check per `++=`                     |
| `cycle-detect`      | Walk the heap at exit and report unreleased RC cycles.                                | One walk at exit; same as `rc-balance`  |
| `heap-sample`       | Periodic heap-state snapshot (interval via `TINHEAPSAMPLE=300`).                      | Per-tick cost (cheap)                   |
| `bounds`            | Array bounds checks on indexing operations (where the compiler cannot prove safety).  | One check per indexed access            |

### `rc-balance` example

```
$ tin run --check=rc-balance src/main.tin
... program output ...
rc-balance report at exit:
  3 unreleased allocations:
  - foo.tin:42 string "config.path"   rc=1  (binding: owned, expected drop at scope exit)
  - bar.tin:17 *Cache                  rc=2  (binding: owned, expected drop at scope exit)
  - bar.tin:17 *Cache                  rc=2  (binding: owned, expected drop at scope exit)
```

## What is never affected by `--check`

- **ARC itself.** Every retain/release the optimizer could not elide is
  always emitted. Memory safety is unchanged at every level.
- **`move` use-after-move compile errors.** Compile-time only; not
  gated by any runtime flag.
- **`-W*` warnings.** These are compile-time, not runtime.
- **`defer` blocks, panic handlers, fiber scheduler.** Program
  semantics, not safety checks.

## Recommended workflow

| Phase              | Flags                                                                | What you get                                              |
| ------------------ | -------------------------------------------------------------------- | --------------------------------------------------------- |
| Local development  | `--check=rc-balance` (when debugging memory issues)                  | Lean default; opt in when you need to investigate         |
| CI                 | `--check=all -Werror`                                                | Every safety net on; warnings fail the build              |
| Staging / canary   | `--check=rc-balance,cycle-detect,heap-sample` (or `--check=all`)    | Production-shaped binary plus the observability you want  |
| Production         | (no checks)                                                          | Smallest, fastest binary; compile-time analysis applies   |

The compile-time analysis runs in every mode. Runtime checks are
purely a question of which validation you want compiled into the
binary - none, some, or all.
