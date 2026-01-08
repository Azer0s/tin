# Tin Compiler Implementation Plan

This plan covers every confirmed bug and missing feature found through spec
analysis and testing. Each item lists the exact file(s) to touch, the
function(s) involved, and what the expected outcome is.

Phases are ordered by impact and coupling: bugs that break basic usability
come first; additive language features come last.

---

## Phase 1 — Critical Bugs

### 1.1 Fix `++` binary concat always returning 0

- **File:** `codegen/codegen.go`
- **Function:** `genBinExpr` (~line 3117)
- **Problem:** The `switch e.Op` has no `case "++"` branch — falls through to
  `return constant.NewInt(irtypes.I64, 0)`.

- [x] Add `case "++"` in `genBinExpr`. Both operands are string/array fat-ptrs
  `{i8*, i64}`:
  1. Extract `left_ptr`, `left_len` (fields 0, 1).
  2. Extract `right_ptr`, `right_len`.
  3. `total = left_len + right_len`.
  4. `malloc(total + 1)`.
  5. `memcpy(buf, left_ptr, left_len)`.
  6. `memcpy(buf + left_len, right_ptr, right_len)`.
  7. Store `'\0'` at `buf[total]`.
  8. Return `{buf, total}` fat-ptr.

**Expected:** `"hello" ++ " world"` → `"hello world"`, `[1,2] ++ [3]` → `[1,2,3]`.

---

### 1.2 Fix `data T = V | None` — store panic on value assignment

- **File:** `codegen/codegen.go`
- **Problem:** `DataDecl` is skipped at line ~268. No LLVM type is registered,
  so `tinTypeToLLVM` returns `i64`. Storing a struct value into an `i64` alloca panics.

- [x] Implement `genDataDecl(n *ast.DataDecl) error`. Model as a tagged union:
  `{ i8 tag, [payload_bytes x i8] payload }` where `payload_bytes` = size of
  widest non-None variant. Register in `cg.structTypes[n.Name]`.
- [x] Assign tag constants: `None = 0`, variants `= 1, 2, …` in `cg.enumValues`.
- [x] In `preregister`, add `case *ast.DataDecl` to register a placeholder type
  for forward references.
- [x] Replace the `case *ast.DataDecl: // skip` in the main generate loop with
  a call to `genDataDecl`.
- [x] In `genVarDecl`, when assigning a value to a `data` type, wrap it:
  store `tag=1` at field 0, bitcast-store the payload at field 1.

**Expected:** `let m maybe[string] = "hello"` stores `{tag=1, data="hello"}` without panic.

---

### 1.3 Fix `data T` `is v T:` binding — "undefined identifier: v"

- **File:** `codegen/codegen.go`
- **Function:** `genIsExpr` (~line 3828)
- **Problem:** Only handles `e.IsNone`. For typed `is v T`, returns `i1 1`
  without registering `v` in scope.

- [x] Extend `genIsExpr` for the typed case:
  1. `genExpr` the subject to get the tagged-union value.
  2. Load `tag` field (index 0).
  3. Compare against expected variant tag constant.
  4. If `e.VarName != ""`, alloca a variable of the payload type, GEP into
     field 1, bitcast to payload pointer, register in `cg.curScope`.
  5. Return the `i1` comparison.

**Expected:** `if m is s string:` binds `s` as the unwrapped string value.

---

### 1.4 Fix `echo` printing integer for `char`/`u8`

- **File:** `codegen/codegen.go`
- **Function:** `genEcho` (~line 2355)
- **Problem:** `case irtypes.IsInt(t)` uses `%lld` for all integer widths.
  `char` is `i8` and should use `%c`.

- [x] Inside the `IsInt` branch, check `it.BitSize == 8` first:
  use `%c` format, zero-extend to `i32`, call printf.

**Expected:** `echo 'h'` prints `h`, not `104`.

---

### 1.5 Fix `echo` for `atom` printing `0`

- **File:** `codegen/codegen.go`
- **Function:** `genExpr` case `*ast.AtomLit` (~line 2994)
- **Problem:** `AtomLit` emits `constant.NewInt(irtypes.I64, 0)`. Name is lost.

- [x] Change `AtomLit` codegen to emit the atom name as a string fat-ptr
  (e.g. `"'ok"`). Use `cg.buildStringFatPtr(block, "'"+e.Name)`.
- [x] The existing `isStringType` branch in `genEcho` will then handle it correctly.

**Expected:** `echo 'ok` prints `'ok`.

---

### 1.6 Fix `addr(N)` — "not an lvalue: \*ast.IntLit"

- **File:** `codegen/codegen.go`
- **Function:** `genAddrExpr` (~line 3740)
- **Problem:** `genAddrExpr` calls `genLValue` on an integer literal, which
  is not an lvalue.

- [x] In `genAddrExpr`, detect `e.Val` is `*ast.IntLit`:
  1. `genExpr` to get `i64` constant.
  2. `block.NewIntToPtr(val, irtypes.I8Ptr)` and return.
- [x] Continue using `genLValue` for identifier/index/field lvalue cases.

**Expected:** `addr(0xB8000).(*char)` compiles to an `inttoptr` cast.

---

### 1.7 Fix `where 'atom: echo "msg"` — parse error on statement body

- **File:** `parser/parser.go`
- **Function:** `parseWhereClause` (inline body branch)
- **Problem:** Inline where body calls `p.parseExpr()`. `echo` is a statement,
  not an expression → parse error "unexpected token echo".

- [x] Change the inline branch to call `p.parseStatement()` instead of
  `p.parseExpr()`.
- [x] In `genWhereList` (codegen), dispatch: if `clause.Body` is a statement
  node call `cg.genStmt`; if it is an expression node call `cg.genExpr`.

**Expected:** `where 'ok: echo "all good"` parses and executes.

---

## Phase 2 — Missing Builtins

### 2.1 Add `len(arr)` / `len(str)` builtin

- **File:** `codegen/codegen.go`
- **Function:** `genCallExpr` identifier branch (~line 3312)
- **Problem:** `len` is not in scope → "undefined function: len".

- [x] In the `*ast.Identifier` case of `genCallExpr`, before the scope lookup,
  intercept `fn.Name == "len"` with one argument:
  - String fat-ptr `{i8*, i64}`: extract field 1.
  - Array fat-ptr `{T*, i64}`: extract field 1.
  - Static array `[N x T]`: return `constant N`.
- [x] Return `i64`.

**Expected:** `len("hello")` = `5`, `len([1,2,3])` = `3`.

---

### 2.2 Add `default(t)` builtin

- **File:** `codegen/codegen.go`
- **Function:** `genCallExpr`, `genExpr` for `*ast.DefaultExpr`
- **Problem:** `default(T)` as a call expression reaches `genCallExpr` via
  the identifier path and fails.

- [x] Intercept `fn.Name == "default"` in `genCallExpr`. Resolve the type
  from the first type argument (`e.TypeArgs[0]`) and return `cg.zeroValue(lt)`.
- [x] Verify `ast.DefaultExpr` in `genExpr` already handles the keyword form.

**Expected:** `default(i64)` = `0`, `default(string)` = `{"", 0}` fat-ptr.

---

## Phase 3 — Missing Language Features

### 3.1 Add multi-line comments `/* ... */`

- **File:** `lexer/lexer.go`
- **Function:** `nextToken` (line ~291), `handleLineStart`
- **Problem:** Lexer only handles `//`. The `/` of `/*` is emitted as SLASH
  → parse error.

- [x] In `nextToken`, after the `//` check, add a `/*` handler:
  consume through matching `*/` (handle nested `/*` if desired, or flat).
  Recurse to `nextToken()` after the closing `*/`.
- [x] Apply the same skip in `handleLineStart` so block comments at
  line-start don't interfere with indentation tracking.

**Expected:** `/* comment */` is silently ignored everywhere.

---

### 3.2 Add `atom` as a first-class type

- **File:** `codegen/codegen.go`
- **Function:** `resolveSimpleType` (~line 474)
- **Problem:** `"atom"` is not handled → falls through to `i64` accidentally,
  but standalone atom variables and function params don't work properly.

- [x] Add `case "atom": return irtypes.I64, nil` in `resolveSimpleType`.
  Atoms are interned as integers.
- [x] Add `atomIDs map[string]int64` to `CodeGen`. Assign stable sequential
  IDs on first encounter of each atom name.
- [x] Change `AtomLit` codegen (already addressed in 1.5 for echo) to emit
  the atom's integer ID from the intern table when used in arithmetic/comparison
  contexts; emit the name string fat-ptr only when the result feeds directly
  into a string context (echo, interpolation).

**Expected:** `let a atom = 'ok` stores the integer ID of `'ok`. Two atom
values with the same name compare equal.

---

### 3.3 Fix `enum atom` matching with `where 'name:`

- **File:** `codegen/codegen.go`
- **Function:** `genWhereList` (~line 2133)
- **Problem:** The where condition evaluates the `AtomLit` as a standalone
  value and calls `toBool` on it, which is always truthy/falsy rather than
  an equality check against the dispatch subject.

- [x] Add a `matchSubject value.Value` field to `CodeGen` (set before entering
  `genWhereList`, cleared after).
- [x] In `genWhereList`, when `clause.Cond` is `*ast.AtomLit`, emit:
  `icmp eq matchSubject, atomID` instead of `toBool(atomID)`.
- [x] When `clause.Cond` is `*ast.WildcardExpr`, emit `i1 true` (already done).

**Expected:** `where 'ok:` correctly dispatches when the subject equals `'ok`.

---

### 3.4 Pointer arithmetic: `ptr += 1` should advance by element size

- **File:** `codegen/codegen.go`
- **Function:** `genAugAssign` (~line 2439)
- **Problem:** `+=` always calls `block.NewAdd`. On pointer types this is
  invalid (pointers aren't integers in LLVM IR).

- [x] In the `+=` / `-=` cases, check if `elemType` is `*irtypes.PointerType`:
  ```go
  if pt, ok := elemType.(*irtypes.PointerType); ok {
      offset := rhs  // or Neg(rhs) for -=
      result = block.NewGetElementPtr(pt.ElemType, current, offset)
  }
  ```

**Expected:** `video_mem += 1` advances the `*char` pointer by 1 byte.

---

### 3.5 Dynamic dispatch via trait-typed parameters (`fn foo(x named)`)

- **File:** `codegen/codegen.go`
- **Functions:** `genFuncDeclAs` (~line 1899), `predeclareFuncAs`, `genCallExpr`
- **Problem:** Trait-typed params are deliberately rejected with an error
  message. The spec and `docs/06-traits.md` show this syntax as valid, and
  the fat-pointer infrastructure already exists.

- [x] Remove the trait-as-param rejection block in `genFuncDeclAs`.
  When a param type is a known trait, `tinTypeToLLVM` already returns the fat-ptr
  struct type — allow it through.
- [x] Mirror the same change in `predeclareFuncAs`.
- [x] In the argument-adaptation loop in `genCallExpr`, auto-coerce concrete
  struct values to trait fat-ptrs when the expected param type is a fat-ptr:
  call `cg.coerceToTrait(block, argVal, instKey)`.

**Expected:** `fn print_name(x named) = echo x.name()` compiles and dispatches
through the vtable at runtime.

---

## Phase 4 — Control Tag Enforcement

### 4.1 Enforce `#noRecurse`

- **File:** `codegen/codegen.go`
- **Function:** `genFuncDeclAs` or a post-IR walk
- **Problem:** `#noRecurse` tag is parsed into `FuncDecl.Tags` but never checked.

- [x] After generating a function body, walk its basic blocks. If any `call`
  instruction's callee is the function itself and the function has tag
  `"noRecurse"`, return a compile error.

**Expected:** A `#noRecurse` function that calls itself fails to compile.

---

### 4.2 `#pure #recurse` compile-time evaluation

- **File:** `codegen/codegen.go`
- **Problem:** No constant folding for pure recursive functions.

- [x] Add a `evalConstFunc(fn *ast.FuncDecl, args []value.Value) (value.Value, bool)`
  method. If all args are LLVM constants, run a simple Go-level interpreter
  over the function AST (arithmetic, comparisons, recursion only).
- [x] Call it from `genCallExpr` when the callee has both `"pure"` and
  `"recurse"` tags and all arguments are constants.

**Expected:** `fib(10)` tagged `#pure #recurse` emits a constant `55` in IR.

---

### 4.3 Struct-level control tags `struct{#pure@fn #const@field}`

- **File:** `codegen/codegen.go`
- **Function:** `genStructDecl`
- **Problem:** `StructDecl.Tags` and `StructField.Tags` are parsed but ignored.

- [x] In `genStructDecl`, iterate `n.Tags`. For `"pure@fn"` enforce `#pure`
  on all methods. For `"const@field"` reject writes to that field in `genLValue`.

**Expected:** Struct-level tags produce diagnostics when violated.

---

## Phase 5 — Macro System

### 5.1 Macro expansion

- **Files:** `codegen/codegen.go`, `parser/parser.go`
- **Problem:** `MacroDecl` nodes are collected but calls are never expanded.

- [x] Add `macros map[string]*ast.MacroDecl` to `CodeGen`.
- [x] In `preregister`, `case *ast.MacroDecl: cg.macros[n.Name] = n`.
- [x] In `genCallExpr`, before the scope lookup, check `cg.macros[fn.Name]`.
  If found, bind each macro parameter to the corresponding argument AST node
  and recursively `genExpr` / `genStmt` the substituted body.
- [x] Handle `macro!` call syntax — ensure the `!` suffix is part of the name
  in the call AST.

**Expected:** `macro double!(x) = x + x` expands inline at the call site.

---

## Phase 6 — Docs Update

### 6.1 Update `docs/06-traits.md` dynamic dispatch examples

- **File:** `docs/06-traits.md`
- **Problem:** "Dynamic dispatch — trait-typed parameters" section shows
  `fn print_name(x named)` as valid syntax; currently rejected by compiler.

- [x] After completing task 3.5: verify the examples compile and produce
  correct output.
- [x] If dynamic dispatch via trait params is kept disabled by design,
  update every `fn foo(x traitName)` example to the
  `fn foo[t](x t) where t is traitName` form.

---

## Dependency Order

```
1.2 → 1.3   (data type must be registered before is-binding)
1.5 → 3.2   (atom literal codegen before atom type)
3.2 → 3.3   (atom type before atom where-matching)
3.5 → 6.1   (dynamic dispatch before doc update)
```

All other tasks are independent.

---

## Key Locations Quick Reference

| Task | File | ~Line |
|------|------|-------|
| `++` missing in genBinExpr | codegen/codegen.go | 3162–3227 |
| DataDecl skipped | codegen/codegen.go | 268 |
| genIsExpr stub | codegen/codegen.go | 3828 |
| genEcho int branch | codegen/codegen.go | 2378 |
| AtomLit returns 0 | codegen/codegen.go | 2994 |
| genAddrExpr / genLValue | codegen/codegen.go | 3740, 4340 |
| parseWhereClause inline body | parser/parser.go | 375 |
| genWhereList body dispatch | codegen/codegen.go | 2133, 2151 |
| genCallExpr identifier lookup | codegen/codegen.go | 3312–3353 |
| resolveSimpleType | codegen/codegen.go | 474 |
| genAugAssign += case | codegen/codegen.go | 2456 |
| genFuncDeclAs trait rejection | codegen/codegen.go | 1900 |
| `/* */` comment missing | lexer/lexer.go | 291 |
| docs dyn dispatch examples | docs/06-traits.md | 124 |
