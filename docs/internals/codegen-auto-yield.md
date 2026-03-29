# Codegen: Auto-yield at Loop Backedges

## Overview

Every `{#async}` function compiled as a `$coro` (fiber) variant
automatically yields to the scheduler at each loop backedge. This
allows concurrent fibers to cooperate without explicit `yield` calls.

The sync (non-fiber) variant of the same function is **never** affected.

## Flag: `curFnAutoYield`

```go
// In CodeGen struct (codegen/codegen.go):
curFnAutoYield bool  // true in $coro variant without #no_autoyield
```

Set in `genCoroFuncBody` (coro.go) at the start of each `$coro` body:

```go
cg.curFnAutoYield = !hasTag(n.Tags, "no_autoyield")
```

Cleared to `false` in `genFuncDeclAs` (funcs.go) for the sync variant:

```go
cg.curFnAutoYield = false // sync variant never auto-yields
```

Both are restored from their saved `prevAutoYield` value on exit.

## Helper: `genYieldAutoAt`

```go
// In coro.go:
func (cg *CodeGen) genYieldAutoAt(from *ir.Block, header *ir.Block) {
    resume := cg.emitSuspendPoint(from, cg.curCoroFrame)
    resume.NewBr(header)
}
```

`emitSuspendPoint` emits a `coro.suspend` intrinsic, creating a new
"resume" basic block. `resume.NewBr(header)` routes the resume edge back
to the loop header (condition or post block).

## Backedge Injection Sites

Five sites in the codegen inject auto-yield when `cg.curFnAutoYield` is true:

| Function          | File       | Backedge target                              |
|-------------------|------------|----------------------------------------------|
| `genForWhile`     | `stmts.go` | `endBody` -> `condBlock`                     |
| `genForCStyle`    | `stmts.go` | `postBlock` -> `condBlock`                   |
| `genForIn`        | `stmts.go` | `bodyBlock` (after increment) -> `condBlock` |
| `genForRange`     | `stmts.go` | `bodyBlock` (after increment) -> `condBlock` |
| `genForIterTrait` | `decls.go` | `bodyBlock` (after increment) -> `condBlock` |

Pattern (same at every site):

```go
if cg.curFnAutoYield {
    cg.genYieldAutoAt(backEdgeBlock, targetBlock)
} else {
    backEdgeBlock.NewBr(targetBlock)
}
```

## Opting Out

Add the `#no_autoyield` tag to disable auto-yield for a specific function:

```tin
fn{#async #no_autoyield} tight_loop(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // no yield inserted here
  return sum
```

Multiple tags have no separators (no commas): `fn{#async #no_autoyield}`.

## Performance Notes

- The sync variant has zero overhead: `curFnAutoYield` is always false.
- The `$coro` variant (only used when the function is spawned as a fiber)
  inserts one `coro.suspend` per loop iteration by default.
- For tight numeric loops, `{#no_autoyield}` eliminates the suspend overhead
  while still producing correct fiber results.
- LLVM's coroutine split pass lowers `coro.suspend` to a pair of
  `setjmp`/`longjmp`-style intrinsics; the actual cost is a register save
  and a scheduler callback.
