# REPL: LLVM ORC JIT Design (Option B)

## Overview

A full-featured Tin REPL built on LLVM's ORC JIT. Each cell is compiled to
LLVM IR by the existing Tin codegen and loaded into a persistent JIT host
process that runs the fiber scheduler. Background fibers persist across cells.
`await` works naturally. No subprocess-per-cell, no state serialization.

---

## Architecture

```
User terminal
     |
     | readline / raw stdin
     v
tin repl  (Go, main.go "repl" subcommand)
     |
     | protocol over stdin/stdout pipe pair
     v
tin-jit-host  (C, ~400 lines, ships alongside tin binary)
     |
     | LLVM ORC JIT C API
     v
  JIT session  (grows per cell, never unloads modules)
     |
     +-- fiber scheduler (_tin_fiber_run, epoll I/O thread)
     +-- cell fibers  (each cell is a spawned {#async} fn)
     +-- user background fibers  (persist across cells)
```

The Go driver handles:
- Readline / multi-line input detection
- Invoking Tin codegen to produce IR for each cell
- Sending IR to the JIT host over a pipe
- Forwarding JIT host stdout/stderr to the terminal
- Tracking accumulated session state (symbol names and types)

The JIT host handles:
- LLVM ORC JIT session lifecycle
- Coroutine lowering pass pipeline
- Running the fiber scheduler
- Receiving IR modules, loading them, executing cell entry functions
- Printing results of bare expressions

---

## Protocol (Go driver <-> JIT host)

Simple length-prefixed binary protocol over a pipe pair (stdin/stdout of the
JIT host process). The Go driver writes; the JIT host reads on fd 0. The JIT
host writes output on fd 1; the Go driver forwards it to the terminal.

### Go -> JIT host messages

```
CELL_IR   [4-byte len] [IR text bytes]
QUIT
```

`CELL_IR` delivers a complete LLVM IR module for one cell. The JIT host loads
it, runs initialization, and calls the cell entry function as a new fiber.

### JIT host -> Go messages

All regular output (echo, etc.) from fibers goes to stdout and is forwarded
verbatim by the Go driver to the terminal.

A special framing byte sequence marks cell completion so the Go driver knows
when to show the next prompt:

```
CELL_DONE [1 byte status: 0=ok, 1=panic]
```

This is written to a separate fd (e.g., fd 3) so it doesn't mix with program
output. The Go driver `select`s on the JIT host's stdout and fd 3.

---

## IR structure per cell

For each cell the Go driver produces an LLVM IR module containing:

### Extern declarations for all previously defined globals

Every top-level binding introduced by a prior cell is declared external:

```llvm
@x = external global %TinString
@resp = external global %HttpResponse
```

The JIT's symbol table resolves these to the actual globals from the modules
that defined them.

### New global definitions for this cell's top-level bindings

```llvm
@count = global i64 0
```

### The cell's init function (runs synchronously at load time)

Initializes new globals with non-trivial expressions (pure, non-async):

```llvm
define void @__tin_cell_N_init() {
  ; store computed initial values into new globals
}
```

### The cell entry coroutine

Every cell's statements run inside an `{#async}` function so `await` works:

```llvm
define ptr @__tin_cell_N_entry(ptr %coro.id, ...) {
  ; cell body as a coroutine
}
```

This is a standard Tin `{#async}` function - the coroutine lowering pass
transforms it into a state machine exactly as it does for any async function.

### Bare expression result (optional)

If the cell input is a bare expression (not a statement), the codegen wraps it:

```tin
echo (<expr>)
```

The Go driver detects this case by inspecting the parsed AST before codegen.

---

## JIT host: coroutine pass pipeline

This is the most critical piece. Tin uses LLVM coroutines, which require
specific lowering passes run in order. The JIT host must replicate what clang
does via the `IRTransformLayer`.

Required pass sequence (per module, via ORC `IRTransformLayer`):

```c
// Pass 1: O1 with coro passes (matches Tin's two-pass approach)
PassBuilder pb1;
pb1.registerCGSCCAnalyses(cgam);
...
FunctionPassManager fpm1 = pb1.buildFunctionSimplificationPipeline(
    OptimizationLevel::O1, ThinOrFullLTOPhase::None);
// Add coro early lowering:
//   CoroEarlyPass, CoroSplitPass, CoroElidePass, CoroCleanupPass
// Run on module

// Pass 2: O2 (after coro frames are materialized)
// Standard O2 pipeline - coro state machines are now plain functions
```

The exact pass sequence must match what Tin's `compileIR` function does in
`main.go`. If they diverge, async functions will malfunction silently or crash.

The LLVM ORC C API exposes `LLVMOrcCreateNewThreadSafeModule` and
`LLVMOrcIRTransformLayerSetTransform` for this. The transform callback
receives the module, runs the passes, and returns it.

---

## JIT host: fiber integration

The JIT host's `main`:

```c
int main(int argc, char **argv) {
    // 1. Initialize LLVM ORC JIT session
    LLVMOrcLLJIT *jit = create_jit_with_coro_passes();

    // 2. Load Tin runtime symbols into the JIT's process symbol table
    //    (malloc, _tin_rc_alloc, _tin_fiber_run, etc. are already in the
    //    process since tin-jit-host links against the Tin runtime)
    register_process_symbols(jit);

    // 3. Start the fiber scheduler in a background OS thread
    //    The scheduler runs independently; cell fibers are submitted to it.
    start_fiber_scheduler();

    // 4. Enter the command loop: read CELL_IR messages, load modules,
    //    call cell init + spawn cell fiber, wait for fiber completion,
    //    write CELL_DONE.
    repl_loop(jit);
}
```

The fiber scheduler (`_tin_fiber_run`) runs on its own OS thread. Cell fibers
are created via `_tin_fiber_spawn` and submitted to the scheduler. The command
loop thread waits for cell completion via a semaphore or condition variable that
the cell fiber signals when it exits.

Background fibers (spawned by user code, not awaited) are never waited on -
they just keep running in the scheduler across cells.

---

## Symbol redefinition

The hardest problem. Two cases:

### Redefining a function

The old module is removed via ORC `ResourceTracker` before the new module is
added. The new `fn foo()` replaces the old one. Any fiber currently executing
inside the old `foo` continues to run its old copy (the function body is in
memory until the coro frame is freed). New calls to `foo` after the module swap
use the new version.

Implementation:
- Each cell's module is associated with a `ResourceTracker`
- When a cell redefines a name that was defined by a previous cell, remove the
  previous cell's `ResourceTracker` (which also removes all other symbols from
  that module - see complication below)

Complication: removing a module removes ALL its symbols, including globals from
that cell that weren't redefined. If cell 2 defines `x` and `y`, and cell 5
redefines only `x`, removing cell 2's module also removes `y`. Solution:
on redefinition, re-emit `y`'s current value into the new module (requires
reading the old global's value from the JIT before removing the module).

This is complex. A simpler first cut: **disallow function redefinition**. Print
an error: "foo is already defined; restart the REPL to redefine it." This
covers the MVP and can be relaxed later.

### Redefining a struct

Must be rejected if any live global has that struct type. Detecting "live
globals of type X" requires tracking which globals have which types - the REPL
driver already has this information (it built the extern declarations). If there
are no live globals of that type, struct redefinition is safe: remove the old
module, add the new module with the new layout.

Default for MVP: reject struct redefinition entirely.

---

## Type feedback from codegen

The Go driver needs to know, after codegen for a cell, what new top-level
bindings were introduced and what their types are. This is needed to:
- Emit extern declarations in the next cell's IR
- Know whether a redefinition is occurring

Mechanism: add a method to `CodeGen`:

```go
// NewTopLevelBindings returns the name and LLVM type string for each
// top-level let/var binding introduced during the last Generate() call.
func (cg *CodeGen) NewTopLevelBindings() []ReplBinding
```

The REPL codegen path (a new function or flag on `Generate`) promotes top-level
`let` statements to `var` globals and records them. The existing global
declaration machinery handles the rest.

---

## Multi-line input

The Go driver needs to distinguish a complete input from a continuation.
Strategy: attempt to parse after each newline. If parsing succeeds, the input
is complete. If parsing returns an "unexpected EOF" error, show a continuation
prompt (`...`) and read another line.

Tin's parser already produces meaningful errors at EOF vs. syntax errors, so
this is straightforward. The only ambiguity: a bare expression on one line is
always complete. A function definition `fn foo() =` followed by a newline
is incomplete (expects indented body).

The parser's error type should be checked: if it's an "unexpected end of input"
error, continue reading. Any other error is a real syntax error - display it
and reset to a fresh cell.

---

## LLVM version coupling

The JIT host must be compiled against the same LLVM version that produced the
IR Tin is emitting. Mismatch causes crashes (IR bitcode format changes,
intrinsic signatures change).

Mitigation:
- `tin-jit-host` is compiled as part of the Tin build process using the system's
  `llvm-config` to find the right include paths and libraries.
- At startup, the JIT host prints its LLVM version. The Go driver checks it
  matches the version used to build the Tin compiler and aborts with a clear
  error if not.
- Build system: `Makefile` target `tin-jit-host` that runs
  `clang $(llvm-config --cflags --ldflags --libs orcjit core passes) tin-jit-host.c -o tin-jit-host`

The version check at startup catches mismatches early rather than producing
mysterious JIT crashes.

---

## Challenges summary

| Challenge                            | Severity | Mitigation                                                                                                              |
|--------------------------------------|----------|-------------------------------------------------------------------------------------------------------------------------|
| Coroutine pass pipeline              | High     | Replicate Tin's two-pass O1->O2 exactly in `IRTransformLayer`; test with async functions before anything else           |
| Symbol redefinition                  | High     | MVP: reject all redefinition with a clear error; relax for functions later via `ResourceTracker`                        |
| Type feedback from codegen           | Medium   | Add `NewTopLevelBindings()` to `CodeGen`; requires small codegen change to track promoted globals                       |
| Multi-line input                     | Medium   | Parser EOF-error detection; straightforward given Tin's error types                                                     |
| LLVM version coupling                | Medium   | Compile `tin-jit-host` from source at build time; version check at startup                                              |
| Struct redefinition with live values | High     | Reject in MVP; requires type-tagged global tracking to relax                                                            |
| Module memory growth                 | Low      | Acceptable for REPL sessions; no mitigation needed                                                                      |
| Output interleaving                  | Low      | Expected and correct behavior; document it                                                                              |
| Partial async binding persistence    | Medium   | Top-level `let x = await ...` is promoted to a global written at end of cell init; requires async init function pattern |

---

## Implementation order

### Phase 1: JIT host skeleton

- `tin-jit-host.c`: initializes ORC JIT, registers process symbols, runs
  coroutine passes (verify with a hand-written async IR fixture), starts fiber
  scheduler, reads CELL_IR from stdin, loads module, calls init function,
  writes CELL_DONE to fd 3.
- No Go integration yet - test by piping hand-written IR directly.
- Key validation: an async function that `await`s a timer runs correctly.

### Phase 2: Go driver + codegen integration

- `NewTopLevelBindings()` on `CodeGen` in REPL mode.
- Promote top-level `let` to `var` global in REPL codegen path.
- Emit extern declarations for prior-cell globals.
- Go driver spawns JIT host, pipes IR, reads CELL_DONE.
- Basic synchronous cells work: `echo 1 + 1`.

### Phase 3: Async cells

- Cell entry is `{#async}` unconditionally.
- `await` in cell body works: `let resp = await http::get(...)`.
- Top-level `let` from async binding is written to global before cell fiber
  exits (via a synthetic `var __cell_N_x HttpResponse; ... __cell_N_x = resp`).

### Phase 4: Multi-line input + expression printing

- Readline integration (libedit or raw mode).
- Parser EOF detection for continuation prompts.
- Bare expression detection in AST; wrap in `echo`.

### Phase 5: Background fiber persistence (already works at Phase 1)

No extra work - background fibers are just fibers the user spawned and didn't
await. They live in the scheduler across cell boundaries automatically.

---

## Files to create

```
repl/
  tin-jit-host.c     ~400 lines C: ORC JIT, coro passes, fiber integration, protocol
  Makefile.jit       build rules for tin-jit-host (uses llvm-config)
codegen/
  repl.go            REPL codegen: top-level binding promotion, extern decls,
                     NewTopLevelBindings(), cell IR structure
main.go              new "repl" subcommand: spawn JIT host, readline loop,
                     multi-line detection, pipe management
```

No changes needed to the parser, runtime, or existing codegen paths.
