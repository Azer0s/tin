# Debug Symbols Implementation Plan

## Goal

Full DWARF debug info for `tin build -g`: line numbers, variable types and
names, struct layouts, and correct variable locations across all Tin runtime
patterns including heap-promoted values and async (coro-split) functions.

## Flag design

```
tin build -g file.tin    # full debug info, O0 final compile
tin run   -g file.tin    # same, for interactive debugging
tin test  -g dir/...     # same for test runner
```

`-g` drives two things together: debug metadata emission AND the pass pipeline.
Running `-g` with O2 is explicitly unsupported - optimizations lie about variable
locations.

---

## Pass pipeline for debug builds

The current pipeline for async IR (in `compileIR`):

```
1. clang -O1 -S -emit-llvm  input.ll  -> split.ll   (coro split)
2. clang -O2                split.ll  -> binary      (final compile)
```

For `-g` builds, only step 2 changes. Step 1 stays at `-O1` because:
- It runs CoroEarlyPass + CoroSplitPass + CoroCleanupPass (required for coro correctness).
- The probe (see below) confirmed that LLVM 22's CoroSplitPass correctly transforms
  `dbg.declare` on frame-spilled allocas to `#dbg_value` with `DW_OP_plus_uconst +
  DW_OP_deref` expressions pointing into the frame. Running it at O1 is correct.

Step 2 for debug builds:
```
clang -O0  split.ll  -> binary      (debug build final compile)
```

No `opt` binary is needed (and none is available on this platform). Everything goes
through `clang`.

For non-async IR (no `llvm.coro.` in the output), there is no step 1 today, and no
change is needed - step 2 becomes `clang -O0` instead of `clang -O2`.

---

## Probe results

All three probe questions are answered. No prerequisites are missing.

### 1. AST position tracking - READY

`ast.Pos{Line, Col}` is embedded in every AST node via the `base` struct.
`cg.currentPos ast.Pos` is already updated in `genExpr` on every node.
`cg.filename string` is already available in the codegen context.
No AST changes needed.

### 2. Metadata library - llir/llvm pure Go

Tin uses `github.com/llir/llvm v0.3.6`, a pure-Go LLVM IR library - not
`tinygo-org/go-llvm` (the CGo C API wrapper). There is no `DIBuilder` object.

All debug metadata is created by direct struct instantiation from the
`github.com/llir/llvm/ir/metadata` package:

```go
import (
    "github.com/llir/llvm/ir"
    "github.com/llir/llvm/ir/enum"
    "github.com/llir/llvm/ir/metadata"
)

// Module-level: compile unit and file
diFile := &metadata.DIFile{Filename: "main.tin", Directory: "/path/to"}
diCU := &metadata.DICompileUnit{
    Distinct:     true,
    Language:     enum.DwarfLangC99,
    File:         diFile,
    Producer:     "tin",
    IsOptimized:  false,
    EmissionKind: enum.EmissionKindFullDebug,
}
// Register in module named metadata
mod.NamedMetadataDefs["llvm.dbg.cu"] = &metadata.NamedDef{
    Name:  "llvm.dbg.cu",
    Nodes: []metadata.Node{diCU},
}
mod.NamedMetadataDefs["llvm.module.flags"] = &metadata.NamedDef{
    Name: "llvm.module.flags",
    Nodes: []metadata.Node{
        &metadata.Tuple{Fields: []metadata.Field{
            metadata.UintLit(2), metadata.MDString("Debug Info Version"), metadata.UintLit(3),
        }},
    },
}

// Per-function: attach DISubprogram
diSubprog := &metadata.DISubprogram{
    Distinct:     true,
    Name:         "myFunc",
    File:         diFile,
    Line:         42,
    Type:         diSubroutineType,
    IsDefinition: true,
    Unit:         diCU,
    SPFlags:      enum.DISPFlagDefinition,
}
fn.Metadata = append(fn.Metadata, &metadata.Attachment{Name: "dbg", Node: diSubprog})

// Per-statement: attach DILocation
diLoc := &metadata.DILocation{Line: 10, Column: 5, Scope: diSubprog}
inst.Metadata = append(inst.Metadata, &metadata.Attachment{Name: "dbg", Node: diLoc})

// Per variable: dbg.declare intrinsic call + DILocalVariable
dbgDeclare := cg.mod.NewFunc("llvm.dbg.declare", irtypes.Void,
    ir.NewParam("", metadata.NewValue(nil)),  // i64* alloca as metadata
    ir.NewParam("", metadata.NewValue(nil)),  // DILocalVariable
    ir.NewParam("", metadata.NewValue(nil)),  // DIExpression
)
diVar := &metadata.DILocalVariable{Name: "x", Scope: diSubprog, File: diFile, Line: 10, Type: diI64}
diExpr := &metadata.DIExpression{}  // empty = direct address

block.NewCall(dbgDeclare,
    &metadata.Value{Value: alloca},
    &metadata.Value{Value: diVar},
    &metadata.Value{Value: diExpr},
)

// DW_OP_deref for heap-allocated values:
diExprDeref := &metadata.DIExpression{Fields: []metadata.DIExpressionField{
    enum.DwarfOpDeref,
}}
```

No `Finalize()` call needed - metadata is serialized as part of `mod.String()`.

### 3. Coro split debug info survival - Option A confirmed

Test: minimal async `.ll` with `dbg.declare` on alloca `%x` (runtime i64 value from
parameter), value used after `llvm.coro.suspend`. Ran through `clang -O1 -S -emit-llvm`.

Result: LLVM 22 CoroSplitPass correctly handles the debug info:

1. `dbg.declare` on the alloca is converted to `#dbg_value` with a frame-relative
   expression: `#dbg_value(ptr %hdl, !var, !DIExpression(DW_OP_plus_uconst, 16, DW_OP_deref), !loc)`
   where `16` is the byte offset of the spilled value in the coroutine frame.

2. The `DILocalVariable` is automatically re-scoped from the original `foo$coro`
   subprogram to the generated `foo$coro.resume` subprogram.

3. New `DISubprogram` nodes for `.resume` and `.destroy` are created automatically.

4. The original `foo$coro` function has no variable debug info (it only allocates the
   frame and returns the handle).

**Option B is not needed.** No custom IR walking required.

---

## Implementation phases

### Phase 1: Infrastructure

1. Add `-g` flag to CLI (`main.go`).
2. In `compileIR`: when `-g` is set, use `clang -O0` for the final step instead of `-O2`.
   The coro split step (`clang -O1 -S -emit-llvm`) is unchanged.
3. Add to codegen context (`codegen/codegen.go`):
   - `debugMode bool`
   - `diFile *metadata.DIFile`
   - `diCU *metadata.DICompileUnit`
   - `currentScope metadata.Field` (DISubprogram or DILexicalBlock)
   - `diTypeCache map[string]metadata.Field`
   - `dbgDeclareFn *ir.Func`
4. When `-g`: create `DICompileUnit` + `DIFile`, register named metadata
   `!llvm.dbg.cu` and `!llvm.module.flags` on the module.

### Phase 2: Line numbers

For each statement's first emitted instruction, attach `!dbg`:
```go
if cg.debugMode {
    diLoc := &metadata.DILocation{Line: int64(cg.currentPos.Line), Column: int64(cg.currentPos.Col), Scope: cg.currentScope}
    inst.Metadata = append(inst.Metadata, &metadata.Attachment{Name: "dbg", Node: diLoc})
}
```

ARC operations (retain/release/deinit calls) must use `Line: 0` so the debugger
does not stop on invisible compiler-generated operations. Track this with
`emittingARC bool` on the codegen context.

### Phase 3: Function debug info

For each function:
- Create `DISubroutineType` from param and return types.
- Create `DISubprogram(name, linkageName, file, line, type, isDefinition=true, unit=diCU)`.
- Attach to LLVM function: `fn.Metadata = append(fn.Metadata, &metadata.Attachment{Name: "dbg", Node: diSubprog})`.
- Set `cg.currentScope = diSubprog` at function entry.
- For generic monomorphizations: each instantiation gets its own `DISubprogram` at
  the same source line (same as Rust's behavior).

### Phase 4: Type system

Map every Tin type to a `metadata.Field` (a DI type node). Cache by type string
in `cg.diTypeCache` to avoid duplicate metadata nodes.

| Tin type | DWARF |
|----------|-------|
| `i8/i16/i32/i64` | `DIBasicType(encoding=DW_ATE_signed, size=N)` |
| `u8/u16/u32/u64` | `DIBasicType(encoding=DW_ATE_unsigned, size=N)` |
| `f32/f64` | `DIBasicType(encoding=DW_ATE_float, size=N)` |
| `bool` | `DIBasicType(encoding=DW_ATE_boolean, size=8)` |
| `byte` | `DIBasicType(encoding=DW_ATE_unsigned, size=8)` |
| `*T` | `DIDerivedType(tag=DW_TAG_pointer_type, base=DIType(T), size=64)` |
| `string` | `DICompositeType` matching fat ptr layout: `{ptr *u8, len i64}` |
| `[T]` | `DICompositeType` matching fat array layout: `{ptr *T, len i64, cap i64}` |
| `any` | `DICompositeType`: `{type_tag i64, data *void}` |
| struct S | `DICompositeType` with one `DIDerivedType(DW_TAG_member)` per field, correct byte offsets |
| enum E | `DICompositeType` (integer underlying type) |
| `(A, B)` tuple | `DICompositeType` with fields `_0`, `_1` |
| fn type | `DISubroutineType` |

Generics: one `DIType` per monomorphized instantiation (`List[i64]` and `List[string]`
are distinct composite types).

### Phase 5: Local variable declarations

For each `let x T = expr` at any scope depth:

1. Create `DILocalVariable(name=x, type=diType(T), scope=currentScope, line=N)`.
2. After emitting the alloca for `x`:
   - Stack alloca: `dbg.declare(alloca, var, DIExpression())`.
   - Heap-allocated (`&Struct{}`): `dbg.declare(ptr_alloca, var, DIExpression(DW_OP_deref))`.
3. Function parameters: same pattern at function entry with `Arg` field set to
   the 1-based parameter index.

Scope tracking: create a `DILexicalBlock` for each new block scope (if body,
for body, match arm). Push/pop on the codegen scope stack.

### Phase 6: Coro frame variables

Option A path only - no Option B needed.

Emit `dbg.declare` on allocas as usual in Phase 5. The `clang -O1 -S -emit-llvm`
coro split step automatically:
- Converts `dbg.declare` on frame-spilled allocas to `#dbg_value` with
  `DW_OP_plus_uconst <offset>, DW_OP_deref` pointing into the heap-allocated frame.
- Re-scopes `DILocalVariable` nodes to the generated `.resume` subprogram.
- Creates new `DISubprogram` nodes for `.resume`, `.destroy`, and other generated
  coroutine functions.

No post-split IR walking needed.

---

## Known limitations (acceptable)

- `print s` for a string shows the fat pointer struct unless a gdb/lldb pretty-printer
  is provided (separate future work).
- No support for optimized builds with partial debug info (`-Og`). Out of scope.

---

## Files to modify

| File | Change |
|------|--------|
| `main.go` | Add `-g` flag; pass `debugMode` to codegen; use `clang -O0` for final step |
| `codegen/codegen.go` | Add `debugMode`, `diFile`, `diCU`, `currentScope`, `diTypeCache`, `dbgDeclareFn`, `emittingARC` |
| `codegen/decls.go` | Emit `DISubprogram` per function; set `currentScope` |
| `codegen/exprs.go` | Emit `DILocation` per statement; `DILocalVariable` + `dbg.declare` per `let` |
| `codegen/exprs_async.go` | No changes needed (Option A handles coro variables automatically) |
| `codegen/runtime.go` | Set `emittingARC = true` around retain/release/deinit calls |
