# Internals: Function Coloring & Fat-Fn-Ptr ABI

## Overview

A Tin function's **color** is a property of the **call site**, not the
declaration.  When a fn is called from inside a fiber, the body needs
auto-yields inserted at loop backedges / heavy ops so the scheduler
keeps interleaving.  When the same fn is called from a non-fiber
thread, those instrumentation points are dead weight.  And when the
fn is `fn{#async}` and the call site is `spawn`, the body has to be
re-emitted as an LLVM coroutine (the `$coro` ramp).

The end-state design emits **three variants per fn**, regardless of
whether the declaration was `fn` or `fn{#async}`:

| Variant            | Body                                                                |
|--------------------|---------------------------------------------------------------------|
| non-colored sync   | vanilla body, no yield instrumentation                              |
| colored sync       | same body + auto-yield calls at loop backedges and heavy call sites |
| `$coro` ramp       | LLVM `presplitcoroutine` variant used by `spawn`                    |

A bare `f()` call site picks the sync variant based on `cg.inCoroFn`
(non-colored when the caller is plain sync, colored when the caller
is itself a coroutine body); `spawn f()` always uses the `$coro` ramp.

Sync fns get the `$coro` variant emitted too -- this is what lets the
compiler treat `fn` as a structural subtype of `fn{#async}` for the
implicit coercion.  The body is the same; only the wrapping differs.

**Current state:** the uniform 4-slot fat-fn-ptr layout has shipped
and every `fn{#async}` fn (plus every fn reachable from one) has its
`$coro` variant emitted.  The colored sync variant is **not yet
emitted** - slot 1 currently mirrors slot 0, so a fiber-bare call to
a sync helper still runs the non-yielding body and can wedge the
worker.  `-Wbare-parking-async-call` and `-Wsync-fn-coerced-to-async`
flag the worst cases until the colored emission lands.

Source:
- variant selection - `codegen/exprs_call.go` (call lowering)
- `$coro` emission - `codegen/coro.go`
- auto-yield placement - `codegen/autoyield.go`

---

## Fat-Fn-Ptr: First-Class Function Values

A fn value (`let f fn(i64) i64 = some_fn`) is stored as a **fat
pointer**: a struct that pairs the function pointer with a closure env
pointer so capturing and non-capturing lambdas share one ABI.

### Current shape (2026-05)

```c
//  fn(...)        T       ABI:  { fn(i8* env, params...) T*,  i8* env }
//  fn{#async}(...) T      ABI:  { fn(i8* env, params...) i8**, i8* env }
//                                 //   ^^^ coro handle (i8*) marks async
```

Two fields:

| Index | Type                     | Purpose                                            |
|------:|--------------------------|----------------------------------------------------|
| 0     | `fn(i8*, params...) ret` | Function pointer (or coro ramp for `fn{#async}`)   |
| 1     | `i8*`                    | Closure env, or `null` for non-capturing fns       |

Sync vs async is distinguishable at the LLVM-type level by the inner
fn's return type: `i8*` (coro handle) means async, anything else means
sync.  `isFatFnPtr` / `isAsyncFatFnPtr` at `codegen/types.go:984-1018`.

### Limitation

The fat-ptr carries **exactly one** variant: whichever the type
annotation picked at storage time.  Concretely:

- `let f fn() i64 = compute` stores the non-colored sync variant.
  Calling `f()` from inside a coroutine body skips auto-yield, even
  though the caller is colored.
- `let g fn{#async}() i64 = compute` stores the `$coro` ramp.  Calling
  `g()` bare in a fiber falls back to a tmp spawn-and-join (the
  current path) rather than dispatching to the colored sync body that
  would be a better fit.
- A `Channel[fn() i64]` pinned by its element type holds only one
  variant; awaiting `ch.recv()` and then calling the result drops the
  coloring information the call site needed.

The compiler papers over the worst cases with `-Wbare-parking-async-call`
and friends, but the structural fix is to carry the union of variants
the *type* promises and let the call site pick.

---

## 4-Slot Fat-Fn-Ptr Shape (shipped)

The fat-fn-ptr is a **uniform 4-slot struct**, identical bytes whether
the fn was declared with `{#async}` or not:

```c
//  ABI:  { non_colored_sync*, colored_sync*, coro_ramp*, i8* env }
```

| Index | Variant            | Used when                                                       |
|------:|--------------------|-----------------------------------------------------------------|
| 0     | non-colored sync   | bare `f()` call from non-fiber thread / sync caller             |
| 1     | colored sync       | bare `f()` call from coroutine body (`cg.inCoroFn`)             |
| 2     | `$coro` ramp       | `spawn f(args)`                                                 |
| 3     | env                | passed as first arg to every variant                            |

A uniform layout means call-lowering doesn't inspect the inner fn's
return type to find the right slot - the index is fixed by call
context.  Round-trip storage (`fn{#async}` -> `fn` slot -> `fn{#async}`
slot) is a bitwise copy: nothing is stripped on downcast and nothing
needs re-synthesis on upcast.

Slot 0 is the canonical user-facing signature (returns `T`), so any
codegen path that consults the inner fn type for inference still sees
the sync signature, not the coro ramp's `i8*` return.

### Slot 2 (coro ramp) construction

- `fn{#async}` declarations get the `$coro` ramp from natural
  compilation (the body is emitted with LLVM coroutine intrinsics).
- Plain `fn` declarations don't have one.  When the fn is referenced
  as a value (boxed into a fat-fn-ptr) or called from a `$coro` body,
  a **synthesized coro wrapper** is emitted on demand via
  `ensureCoroWrapperFor`: a tiny `presplitcoroutine` shim that takes
  the same params, allocates a coro frame, calls the source body, and
  packages the result through `_tin_fiber_complete`.

### Slot 1 (colored sync) construction - planned, not shipped

Today slot 1 mirrors slot 0 (same fn pointer).  The end-state design
emits a separate `<name>$colored` symbol per fn that needs it.  See
the **Colored variants** section below.

---

## Colored variants

A `$colored` variant is the **sync body with auto-yield decisions
applied, but no coro ramp of its own**.  Same signature as the plain
sync variant (returns `T`, takes the same params), same body shape,
but at every loop back-edge and heavy/recursive call site the
existing coloring analysis (`codegen/autoyield.go`) inserts a yield.
The yield lowers to a runtime call

```
_tin_fiber_yield_coro(_tin_current_coro_hdl())
```

which suspends the **caller's** fiber via the TLS-tracked coro hdl.
The colored body has no frame of its own; it borrows the caller's.

### Why colored matters

**Cooperation across the sync/async boundary.**  Today an
`{#async}` fn's `$coro` body that calls a plain sync helper blocks
the worker for as long as the helper runs.  The async caller's yields
are *outside* the helper, so any hot loop or heavy work inside the
helper monopolises the worker thread; other fibers on that worker
don't progress, channel sends to peer fibers stall, async I/O
completions don't get drained.  The workaround is to tag every
potentially-hot helper `{#async}` and rewrite call sites as
`await spawn helper(...)` - signature pollution and a spawn per call.

With colored emission, the helper keeps its sync `(...) T` signature
for non-fiber callers, but the `$coro` body of the async caller
routes to the `helper$colored` variant, which yields at the same
coloring points and cooperates with the scheduler without any change
to the helper's source or signature.

**Perf.**  Beyond correctness, the colored variant is cheaper than the
existing "make this fn `{#async}` and `await spawn` it" workaround:

- **No extra fiber allocation.**  Colored runs in the caller's coro
  frame.  The workaround costs a `_tin_fiber_spawn` + a `_tin_fiber_join`
  per call.
- **No scheduler handoff per call.**  Plain function call vs. spawn-and-await.
- **Tiny IR footprint.**  Colored is the sync body + a few
  `call @_tin_fiber_yield_coro` instructions.  A full `$coro` variant
  carries `llvm.coro.id` / `coro.alloc` / `coro.begin` / per-yield
  suspend/resume blocks / `coro.end` plus a separate frame layout -
  much heavier.
- **Slot 2 of a boxed sync fn (synth coro wrapper) internally calls
  slot 1 (colored)**, so a `spawn`ed sync fn cooperates at the same
  points without the wrapper having to instrument the body itself.

The runtime yield call is slightly more expensive than the
`llvm.coro.suspend` intrinsic ($coro) - one TLS load + one function
call vs. an inline state-machine edge - but the comparison that
matters is "colored vs. uncolored sync today", and there the colored
variant adds *checkpoints where there were none*; it's strictly better
for any caller running inside a fiber.

### Who needs a colored variant

A fn `f` gets a `$colored` emission when it satisfies any of:

1. **Called from a `$coro` body.**  The callee runs inside a coro
   frame at runtime; it must cooperate.
2. **Boxed into a fat-fn-ptr** (referenced as a value, not the
   `.Func` of a `CallExpr`).  Slot 1 needs to point somewhere; if
   the value is invoked from cooperative context, that "somewhere"
   must yield.
3. **Reached transitively** from any fn already in the colored set.
   A colored body calls colored variants of its sync callees (call
   routing rule below), so the BFS propagates through `cg.callGraph`
   the same way `coroCallable` does.

`{#no_autoyield}` suppresses colored emission for the tagged fn (the
user has opted out of cooperative checkpoints - typically a tight
arithmetic kernel where yields would dominate).

### Call routing

| Caller body | Sync callee has `$colored` | Sync callee plain only |
|-------------|----------------------------|------------------------|
| plain sync  | call plain sync            | call plain sync        |
| `$colored`  | call `$colored`            | call plain sync        |
| `$coro`     | call `$colored`            | call plain sync + pre-call yield (existing `genCallSiteYieldFor`) |

Plain-sync bodies always call plain-sync callees - no behavior change
for non-fiber programs.

### Implementation status

Tracked by task #98 (umbrella) and sub-tasks #100 - #108: root-set
collection, BFS propagation, yield-lowering switch, predeclaration,
body emission, call routing, slot-1 wiring, `-Wsync-fn-coerced-to-async`
revisit, regression tests.

---

## C interop and the fat-fn-ptr

The Tin fat-fn-ptr is **never** passed directly across the C boundary
because:

- C ABI doesn't carry the env arg; a closure can't be a raw C fn ptr.
- C sees one function pointer (8 bytes), not a multi-slot struct.

Tin's bridge is built around `tin_make_trampoline`
(`runtime/interop_trampoline.c`): a runtime allocator that takes a
`{fn, env}` pair, allocates a slot in an mmap'd executable page, and
writes a per-arch thunk that restores `{fn, env}` via a scratch
register (`x16` on ARM64, `%r10` on x86_64).  The C caller sees a
plain function pointer; the dispatcher reads the scratch register
and forwards to `fn(env, args...)`.

The four translation paths, all wired up through the same mechanism:

| Direction                                          | Mechanism                                                                                                                                                                                |
|----------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Tin fn called from C (`#interop` decl)             | `#interop` emits `__tin_interop_<name>` (internal) + a bare-name C-callable wrapper that marshals string/slice args (`emitInteropWrapperFor`)                                            |
| `#interop` Tin fn returns a fn value to C          | The wrapper extracts `{fn, env}` from the fat-ptr and calls `tin_make_trampoline(fn, env, dispatch)`; the returned `i8*` is the C function pointer                                       |
| Tin fn passed as arg to a C extern (callback)      | `tinTypeToExternLLVM` lowers `fn(...)` extern params to `i8*`; the call site retains env, builds the trampoline, passes the result (`wrapFatFnPtrAsCCallback` in `codegen/exprs_call.go`) |
| C fn ptr returned to Tin                           | `wrapFromExtern` wraps the raw `i8*` in a fat-fn-ptr where env carries the C ptr and slot 0 is `__tin_c_fn_shim_<sig>` -- an env-dropping stub that bitcasts env back to the C fn type    |

Struct fields and `*fn(...)` extern params get the same treatment:

- A struct passed by value to C has its native layout's fn-typed
  fields lowered to `i8*` (`tinStructNativeLLVM`).  At the call
  site, `wrapStructToExtern` builds a trampoline per fn-typed field
  during the Tin->native struct conversion.
- `*fn(...) T` extern params lower to `i8**`.  The call site loads
  the fat-fn-ptr from the source pointer, builds the trampoline,
  stores it in a stack slot, and passes the slot's address.

In every direction, env is retained before being handed to the
trampoline (the trampoline transfers one ARC ref; the source still
owns its own).  The `atexit_release_all_pages` handler in
`interop_trampoline.c` walks every live slot and releases its env at
process exit -- without the retain, that walk would double-free.

### Why callbacks matter

Real-world C callbacks (audio buffer callbacks, OS signal handlers, UI
event loops, libcurl progress callbacks, GTK signal handlers, ...) take
a function pointer plus an opaque `void *userdata`.  Tin maps any
fat-fn-ptr (including capturing closures) onto that calling convention
so users can pass any Tin fn value as the callback without manually
emitting an `#interop` wrapper for every variant.

### Dispatch rules

Given a fat-fn-ptr value `f`, the call-lowering site picks the variant
based on the *calling* context, not the value's declared type:

```
f(args) outside   -> f[0]   // non-colored sync (vanilla body)
f(args) in coro   -> f[1]   // colored sync (auto-yield instrumented)
spawn f(args)     -> f[2]   // coro ramp
                  -> f[3]   // env, passed as first arg to every variant
```

A sync fn can be coerced to `fn{#async}` at zero cost because every
slot is already populated: slot 0 (sync body), slot 1 (`$colored`
once #98 lands), slot 2 (synth coro wrapper).  This is the
"structural sync->async coercion" that
`-Wsync-fn-coerced-to-async` warns about today and that #107 will
reconsider after #98 ships.

### Implementation status

| Component                          | Source                                 | Status                   |
|------------------------------------|----------------------------------------|--------------------------|
| 4-slot layout + ABI                | `codegen/types.go` (`isFatFnPtr`)      | shipped                  |
| Sync slot 0                        | `codegen/funcs.go`                     | shipped                  |
| `$coro` slot 2 (async fns)         | `codegen/coro.go`                      | shipped                  |
| Synth coro wrapper for sync fns    | `codegen/coro.go` (`ensureCoroWrapperFor`) | shipped              |
| Boxed-fn detection                 | (planned, task #100)                   | not started              |
| `$colored` emission                | (planned, task #104)                   | not started              |
| Slot 1 wiring                      | (planned, task #106)                   | slot 1 = slot 0 today    |
| Runtime ARC for fat-fn elements    | `codegen/runtime.go`, `runtime/arc.c`  | shipped                  |

---

## Why coloring is a call-site property

The same fn body has the *same* externally-visible behaviour whether
its loop emits a `_tin_yield()` every N iterations or not - both
return the same result.  What changes is **scheduler responsiveness**:
- Inside a fiber, a non-yielding loop monopolises a worker thread and
  starves every other fiber on that worker.
- Outside a fiber, the yield would no-op (or fault) because there's no
  scheduler state to give time slices back to.

Thus the *instrumentation* depends on the caller's context.  The
declaration only fixes:
- whether a `$coro` ramp is even emitted (yes for `fn{#async}`, no
  otherwise),
- whether the body itself can `await` / `spawn` / `yield` (yes for
  `fn{#async}`),
- the auto-yield score (some bodies are tagged `{#heavy}` explicitly).

Everything else - including the *colored vs non-colored* split - is
the caller's choice.  The fat-fn-ptr ABI has to carry both so the
choice can be made at call time.
