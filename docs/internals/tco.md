# TCO - Tail Call Optimization

Tin implements TCO by transforming eligible self-recursive functions into
loops at the IR level. No LLVM pass is involved; the transformation is done
entirely during code generation.

## Design

### Eligibility

A function `fn f(p1 T1, ..., pN TN) R` is eligible when:

1. It is sync (not `{#async}`) and not `extern`.
2. It has no `defer` statements in its body.
3. It has at least one parameter (`len(params) > 0`).
4. No parameter's LLVM type is RC-tracked (`isRCTrackedType` returns false
   for all parameter allocas). RC-tracked types are: `string` (TinString fat
   struct), fat array pointers, `any` (fat any), function fat pointers.
5. The body contains at least one direct self tail call - a `CallExpr` with
   `Func = Identifier{Name: funcName}` appearing as:
   - the sole expression of a `where` clause body, or
   - the value of a `return` statement.

Eligibility is checked in `genFuncDeclAs` after parameter allocas are set up.
`hasSelfTailCall` does an AST walk to detect condition 5.

### IR shape

For a non-TCO function the entry block holds param allocas, stores, and the
entire body:

```
entry:
  %n.alloca = alloca i64
  store i64 %n, ptr %n.alloca
  ; ... body ...
  ret i64 %result
```

For a TCO-eligible function the entry block is split: only alloca/store lives
in `entry`; a new `tco_loop` block holds the match-subject load and the body:

```
entry:
  %n.alloca = alloca i64
  store i64 %n, ptr %n.alloca
  %acc.alloca = alloca i64
  store i64 %acc, ptr %acc.alloca
  br label %tco_loop

tco_loop:
  %n.0 = load i64, ptr %n.alloca      ; re-executes each iteration
  ; where n == 0: acc
  %cmp = icmp eq i64 %n.0, 0
  br i1 %cmp, label %where.then.1, label %where.else.1

where.then.1:
  %acc.0 = load i64, ptr %acc.alloca
  ret i64 %acc.0

where.else.1:
  ; tail call `fact(n - 1, n * acc)` rewritten as loop-back:
  %sub = sub i64 %n.0, 1
  %mul = mul i64 %n.0, %acc.0
  store i64 %sub, ptr %n.alloca
  store i64 %mul, ptr %acc.alloca
  br label %tco_loop
```

### State in CodeGen

Three fields on `CodeGen` track TCO context for the current function:

```go
tcoFuncName string    // Tin name of current TCO function; "" outside TCO fns
tcoLoopTop  *ir.Block // the tco_loop block to branch back to
tcoParams   []string  // parameter names in order (for alloca lookup)
```

All three are saved/restored in `genFuncDeclAs`'s context save/restore block
so nested function compilations (monomorphization, lambda bodies) do not
interfere.

### Interception points

Two code-generation paths emit tail self-calls and are intercepted:

**`genWhereBody` (expression bodies)**

When the body of a where clause is a bare expression, `genWhereBody` checks
whether it is a self-call before calling `genExpr`:

```go
if cg.tcoFuncName != "" {
    if ce, ok := body.(*ast.CallExpr); ok {
        if ident, ok2 := ce.Func.(*ast.Identifier); ok2 && ident.Name == cg.tcoFuncName {
            return cg.emitTCOLoopBack(block, ce)
        }
    }
}
```

**`genReturn` (explicit return statements)**

After the coro and defer-thunk guards:

```go
if cg.tcoFuncName != "" && s.Value != nil {
    if ce, ok := s.Value.(*ast.CallExpr); ok {
        if ident, ok2 := ce.Func.(*ast.Identifier); ok2 && ident.Name == cg.tcoFuncName {
            return cg.emitTCOLoopBack(block, ce)
        }
    }
}
```

### `emitTCOLoopBack`

This helper (in `codegen/funcs.go`) implements the loop-back:

1. Evaluate all new argument expressions in order before touching any alloca.
   This ensures expressions like `fact(n-1, n*acc)` read the CURRENT values
   of `n` and `acc` before either is overwritten.
2. Coerce each evaluated value to the alloca's element type.
3. Call `emitAllScopeReleases(block, "")` to free any RC-tracked locals that
   were created in the current scope during this iteration. Non-RC params are
   automatically skipped by the release logic (`elemNeedsRelease` returns
   false for them).
4. Store each new value into its corresponding parameter alloca.
5. Emit `br label %tco_loop`.

### Match subject re-evaluation

For where-list functions, `cg.matchSubject` is set to a `load` instruction
placed at the START of `tco_loop` (not `entry`). Because LLVM re-executes
all instructions in a block each time it is entered, the load re-runs on
every loop iteration and always yields the current value of the first
parameter alloca. All where-clause conditions reference this load, so they
compare against the UPDATED parameter value each iteration.

## Limitations

- Only direct self-calls are optimized. Mutual recursion (`f` calls `g` calls
  `f`) is not covered.
- RC-tracked parameter types (`string`, `any`, arrays, fn values) disable TCO
  for the whole function. Retain/release semantics on loop-back would require
  a retain of the new value and release of the old one; this is planned but
  not yet implemented.
- `defer` statements disable TCO because the defer mechanism reuses the normal
  return path which TCO bypasses.
- `{#async}` functions are excluded; coroutines have a fundamentally different
  call model.
- Only functions compiled by `genFuncDeclAs` (direct, non-generic, non-extern)
  can be TCO'd. Monomorphized generic instances follow the same path and CAN
  be TCO'd if the instantiated parameter types are all non-RC.
