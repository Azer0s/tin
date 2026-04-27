# Codegen: Auto-yield Pass

## Overview

Every `{#async}` function compiled as a `$coro` (fiber) variant automatically
yields to the cooperative scheduler at two kinds of site:

1. **Loop backedges** - at the end of every `for` loop iteration (existing).
2. **Call sites of heavy or recursive functions** - before each call to a
   function classified as "auto-yield" by the static heuristic pass (new).

The sync (non-fiber) variant of the same function is **never** affected.

---

## Phase 1: Loop backedge yields

### Flag: `curFnAutoYield`

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

### Helper: `genYieldAutoAt`

```go
// In coro.go:
func (cg *CodeGen) genYieldAutoAt(from *ir.Block, header *ir.Block)
```

`emitSuspendPoint` emits a `coro.suspend` intrinsic on `from`, creating a new
resume block that branches unconditionally to `header` (the loop condition or
post block).

### Backedge injection sites

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

---

## Phase 2: Call-site yields (heuristic pass)

### Source files

- `codegen/autoyield.go` - heuristic analysis pass
- `codegen/coro.go` - `genCallSiteYield`, `genCallSiteYieldFor`
- `codegen/exprs_call.go` - injection points

### Heuristic classification

Each user-defined Tin function receives a `ComplexScore`:

```
score = loopCount      * 10   (points per ForStmt)
      + allocCount     *  5   (points per AddressOfExpr)
      + callCount      *  2   (points per call to non-heavy callee)
      + heavyCallCount * 20   (points per call to heavy/recursive callee)
```

A function is **auto-heavy** when `score >= 30`. It is **recursive** when DFS
on the static call graph finds it belongs to a cycle (direct or mutual).

`AutoYield = IsHeavy || IsRecursive`.

The analysis runs after `colorCallGraph()`, in three passes over `funcDecls`:

| Pass | Purpose |
|------|---------|
| 1 | Count raw loops/allocs/calls; apply explicit `{#heavy}` tag |
| 2 | Re-classify calls as heavy vs normal using pass-1 results; compute final `ComplexScore` |
| 3 | DFS on `callGraph` to detect recursive cycles |

### `FuncHeuristicInfo`

```go
type FuncHeuristicInfo struct {
    Name           string
    LoopCount      int
    AllocCount     int
    CallCount      int     // calls to non-heavy callees
    HeavyCallCount int     // calls to heavy/recursive callees
    ComplexScore   int
    IsHeavy        bool    // {#heavy} tag or score >= threshold
    IsRecursive    bool    // member of a call-graph cycle
    AutoYield      bool    // IsHeavy || IsRecursive
}
```

Populated in `computeAutoYieldHeuristics` (autoyield.go), stored in
`cg.funcHeuristics` (map[string]*FuncHeuristicInfo).

### Injection helpers

```go
// genCallSiteYield emits a coro.suspend + fiber panic check before a call.
// Returns the resume block ("callsite.yield.after.N") where the call should
// be emitted.  Sets cg.curBlock = afterBlk so statement-level code generators
// that check cg.curBlock pick up the block advance.
func (cg *CodeGen) genCallSiteYield(from *ir.Block) *ir.Block

// genCallSiteYieldFor guards on curCoroFrame, curFnAutoYield, and
// funcHeuristics[calleeName].AutoYield before calling genCallSiteYield.
// Returns the (possibly updated) block to use for the actual call instruction.
func (cg *CodeGen) genCallSiteYieldFor(block *ir.Block, calleeName string) *ir.Block
```

### Injection sites

Two sites in `exprs_call.go`:

1. **Method calls** (FieldAccess path, ~line 687):
   ```go
   block = cg.genCallSiteYieldFor(block, methodName)
   result := block.NewCall(callee, llArgs...)
   ```

2. **Regular Tin function calls** (common fallthrough, ~line 1100):
   ```go
   if f, ok := callee.(*ir.Func); ok {
       block = cg.genCallSiteYieldFor(block, f.Name())
   }
   result := block.NewCall(callee, llArgs...)
   ```

### Block propagation

`genCallSiteYield` sets `cg.curBlock = afterBlk` before returning. Every
statement-level code generator that can call `genExpr` checks:

```go
if cg.curBlock != nil && cg.curBlock != block {
    block = cg.curBlock
}
```

after the call, so subsequent instructions land on the correct resume block.

This matches the identical pattern used by `genAwaitExpr` for `await` expressions.

### Resume block structure

```
[call block]
    call token @llvm.coro.suspend(token none, i1 false)
    switch i8 %sp, label %coro.suspended [i8 0, %coro.resume; i8 1, %coro.cleanup.br]

coro.resume:
    %msg = call i8* @_tin_fiber_check_panic()
    %not_null = icmp ne i8* %msg, null
    br i1 %not_null, label %callsite.yield.panic, label %callsite.yield.after

callsite.yield.after:          <- resume here; actual call goes here
    %result = call T @callee(...)

callsite.yield.panic:
    call void @_tin_panic(i8* %msg)
    call void @_tin_fiber_complete(i8* ...)
    br label %coro.final
```

---

## Opting out

Add the `{#no_autoyield}` tag to disable **both** backedge and call-site
auto-yields for a specific function:

```rust
fn{#async #no_autoyield} tight_inner(n i64) i64 =
  let sum i64 = 0
  for let i i64 = 0; i < n; i = i + 1:
    sum = sum + i   // no yield inserted (backedge or call-site)
  return sum
```

Multiple tags are space-separated: `fn{#async #no_autoyield}`.

## Forcing classification

Add `{#heavy}` to manually mark a function as heavy regardless of its score:

```rust
fn{#heavy} custom_hash(data string) u64 =
  // complex hand-rolled hash; score might fall below threshold
  ...
```

Any caller in a `$coro` context will get a yield inserted before calling it.

---

## Verbose output: `-fdump-heuristics`

Pass `-fdump-heuristics` after the source file to print one line per function to
stderr:

```
tin ir file.tin -fdump-heuristics
tin run file.tin -fdump-heuristics
tin test file.tin -fdump-heuristics
```

Output format:

```
[autoyield] fn <name>   loops=N allocs=N calls=N heavyCalls=N  score=N  [label]
```

Labels: `heavy` (explicit tag), `recursive` (call-graph cycle),
`auto-heavy` (score >= threshold), `normal` (no auto-yield).

Example:

```
[autoyield] fn fib            loops=0  allocs=0  calls=1  heavyCalls=0  score=2   [recursive]
[autoyield] fn sum_range      loops=1  allocs=0  calls=0  heavyCalls=0  score=10  [normal]
[autoyield] fn busy_loop      loops=1  allocs=0  calls=0  heavyCalls=0  score=10  [normal]
[autoyield] fn double_sum     loops=0  allocs=0  calls=0  heavyCalls=2  score=40  [auto-heavy]
```

---

## Performance notes

- The sync variant has zero overhead: `curFnAutoYield` is always false.
- Each call-site yield adds one `coro.suspend` + fiber-panic check per call
  to a heavy/recursive function.
- LLVM's coroutine split pass lowers `coro.suspend` to a register save and a
  scheduler callback (equivalent cost to one yield statement).
- Use `{#no_autoyield}` for innermost tight loops where the yield overhead is
  measurable.
