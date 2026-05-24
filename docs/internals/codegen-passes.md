# Codegen passes - order and invariants

Tin's compiler is a multi-pass codegen built on llir/llvm. Each top-level
`Generate(prog)` call and each `use pkg` recursion walks the AST several
times, with each pass producing a different shape of partial IR. The pass
order matters: getting it wrong produces "store operands not compatible"
panics, "undefined identifier" failures during initializer folding, or
ADT payloads sized against placeholder structs.

This doc names every pass, says what it produces, and (most importantly)
lists the **invariants** that determine where a new pass has to slot in.

If you're touching the package loader (`loadPackageFromSource`,
`loadPackageFromFilePath`) or the top-level `Generate()`, read the
**Invariants** section first.

---

## Three driver functions, same shape

There are three pass schedulers in the compiler. They each walk a list of
`ast.Node` statements multiple times, but the per-driver pass set is
slightly different because the entry program has different needs than an
imported package.

| Driver                         | Where                              | Used for                                     |
|--------------------------------|------------------------------------|----------------------------------------------|
| `Generate`                     | `codegen/codegen_generate.go`      | the entry program (the file passed to `tin`) |
| `loadPackageFromSource`        | `codegen/packages.go`              | `use pkg` (named package import)             |
| `loadPackageFromFilePath`      | `codegen/packages_file.go`         | `use "./file"` (file-path import)            |

The three drivers all converge on the same set of pass primitives:
`preregister`, `genStructLayout`, `genDataDecl`, `predeclareFuncAs`,
`predeclareCoroVariant`, `genStructMethods`, `genFuncDecl`. The ordering
between those primitives is what differs.

---

## Pass primitives, what they need, what they produce

Each entry below names a primitive, what state it **reads** from `cg.*`,
and what it **writes** back. New passes have to slot in at a point where
every "reads" entry is already satisfied.

### `preregister(node)` -- `codegen/funcs_prereg.go`

- Reads: nothing.
- Writes: `cg.structDeclsByName`, `cg.enumDecls`, `cg.typeAliases`,
  `cg.traitDecls`, `cg.unionDecls`, `cg.dataDecls`, `cg.macros`.
- Effect: makes the *names* of every user-defined type, trait, macro
  visible. Forward refs across the file work after this pass.
- Does NOT emit LLVM struct types, just records the AST.

### `genStructLayout(sd)` -- `codegen/decls_struct.go`

- Reads: `cg.structDeclsByName` (from `preregister`), `cg.typeAliases`
  for any referenced types.
- Writes: `cg.structTypes[canonicalKey]` -- the `*irtypes.StructType`
  with all fields populated.
- Effect: a struct becomes "real" -- its size, field offsets, and field
  LLVM types are now fixed. Any code that previously sees the struct
  by name gets a placeholder opaque type until this runs.
- Trap: ADT payload sizes (`Result[Foo, Err]`) are computed in
  `genDataDecl` by reading struct sizes. Running `genDataDecl` before
  `genStructLayout` for `Foo` undersizes the payload.

### `genDataDecl(dd)` -- `codegen/data_codegen.go`

- Reads: `cg.structTypes` for every struct mentioned in any variant
  payload. ADT types instantiated through generics also pull in
  monomorphization.
- Writes: `cg.dataConcreteLayouts[name]`, `cg.dataVariantInfos[name]`,
  registers the ADT's LLVM struct type.
- Effect: ADT payload buffer is sized to the largest variant.
- Must run AFTER `genStructLayout` for every struct that appears in a
  variant field.

### `predeclareFuncAs(fd, irName)` -- `codegen/funcs_predecl.go`

- Reads: `cg.structTypes` and `cg.dataConcreteLayouts` for any type in
  the function signature.
- Writes: an `ir.Func` with empty body in `cg.activeMod`; registers
  the func in `cg.curScope`.
- Effect: callers can take the function's address; mutual recursion
  resolves.

### `predeclareCoroVariant(fd, irName, hasEnv)` -- `codegen/coro_variants.go`

- Reads: signature types (same as `predeclareFuncAs`).
- Writes: a `<name>$coro` predeclared func; registers it in
  `cg.coroCallable`.
- Effect: async callers can spawn / await this function before its
  body is emitted.
- Must run BEFORE any code that calls the `$coro` variant (i.e. before
  most struct-method bodies and `await`-using free-fn bodies).

### `preregisterTopLevelVar(tv)` / `preregisterPkgTopLevelVar(...)` -- `codegen/globals.go`

- Reads: `cg.structTypes`, `cg.dataConcreteLayouts`, `cg.funcDecls`
  (for `#pure` callable initializers), all type aliases.
- Writes: an LLVM `*ir.Global` for the var, with an initializer that
  was folded via `tryConstantFold` if possible.
- Effect: globals are addressable as `@name` from any later-emitted
  function body.
- **Critical invariant**: every type that the initializer expression
  may evaluate to must already exist as a complete `*irtypes.StructType`
  / ADT layout. A top-level `const C = Color{r: 1}` requires `Color`
  to have run through `genStructLayout`. Otherwise the const folds to a
  void global and later stores into it panic with "store operands not
  compatible: src=%Color; dst=void*".

### `genStructMethods(sd)` -- `codegen/decls_struct.go`

- Reads: all `cg.structTypes`, all top-level globals (so method
  bodies can reference module-scope vars), all `predeclareFuncAs`
  results (so methods can call free functions).
- Writes: filled-in `ir.Func` bodies for each method; trait-impl
  registrations; trait vtables.

### `genFuncDecl(fd)` -- `codegen/funcs_genfunc.go`

- Reads: everything from earlier passes.
- Writes: filled-in `ir.Func` body. The final pass for non-generic
  functions.

### `emitTopLevelVarInits(block)` -- `codegen/globals.go`

- Reads: every global's preregistered storage; the implicit main's
  entry block.
- Writes: stores into globals that need runtime init (initializers
  that didn't fold at compile time).
- Runs **last** -- inside the synthetic main body.

---

## Invariants (the rules that order the passes)

Every pass insertion has to preserve all of these. They're listed in
priority order: the earlier ones trip the loudest panics.

1. **`genStructLayout(sd)` before any code that asks for `sizeof(sd)`.**
   ADT layouts (`genDataDecl`), function signatures with struct-typed
   params, struct-typed globals -- everything that materializes an
   LLVM struct shape has to wait.

2. **`genDataDecl(dd)` before any signature that references `dd`.**
   Otherwise the payload is sized against the placeholder
   `[1 x i8]` and any variant that holds a real struct overflows.

3. **`predeclareFuncAs(fd)` before any caller body.** Mutual recursion
   needs every name to be declared first. Bodies emit in a later pass.

4. **`preregisterTopLevelVar(tv)` AFTER `genStructLayout` of every
   struct that could appear in the initializer.** This is the "const
   uses imported struct" case. The fix when you add a new var-emitting
   pass is to schedule it *after* layouts, not to special-case the
   initializer fold.

5. **`predeclareCoroVariant(fd)` before any body that `await`s `fd` or
   spawns through `fd`.** The vtable builder also reads `$coro` names
   when laying out trait method slots, so async traits force this
   even earlier.

6. **`genStructMethods(sd)` AFTER `preregisterTopLevelVar` for every
   global that any method body reads.** Methods refer to module vars
   by bare name; the var has to exist as an addressable global.

7. **`emitTopLevelVarInits` LAST.** Initializers that need runtime
   code (impure pure-fn calls, struct literals with computed fields)
   emit as instructions inside the implicit main. By the time main
   runs, every function body, every method, every vtable is final.

The driver functions encode these invariants by the order they run
their passes. When you add a new pass, write down which invariants
constrain its position before you pick a slot.

---

## Canonical pass order in each driver

The pass numbers below come from the source comments
(`// Pass 0.5: ...`). They are **not contiguous** -- the gaps reflect
historical inserts and the priority order above.

### `Generate` (entry program, `codegen_generate.go`)

1. `preregister` for every type / trait / macro decl
2. struct cycle check
3. macro side-effect check
4. extern symbol name collection (pre-pass 1.5)
5. `genUseDecl` for every `use` (pre-pass 1.9, recurses into the
   package drivers below)
6. overload-name scan (pre-pass 1.8)
7. `genStructLayout` for entry-program structs (pre-pass 1.85)
8. implicit-main synthesis for top-level `try` (pre-pass 1.9)
9. predeclare entry-program function signatures (pass 2)
10. Phase A: struct layouts + enum / type / union decls
11. Phase B: ADT layouts (`genDataDecl`)
12. **Pass 2.5: `preregisterTopLevelVar` for every entry-program var**
13. Phase C: struct method bodies + trait vtables
14. Pass 3: free-fn bodies, generic monomorphization deferred
15. `emitTopLevelVarInits` inside the implicit main

### `loadPackageFromSource` (`use pkg`, `codegen/packages.go`)

1. dedup by absolute source path
2. lex + parse + #no_parens macro scan
3. collect exported names
4. recurse into nested `use` (loadPackage / loadPackageFromFilePath /
   loadPackageSelective)
5. register package-qualified type aliases for exported structs
6. push child scope
7. Pass 0.5: `preregister`
8. Pre-pass 0.8: overload-name scan
9. Pass 1: extern func bodies
10. Pass 1.5a: `genStructLayout` + register package type aliases
11. Pass 1.6: `genDataDecl`
12. **Pass 1.65: `preregisterPkgTopLevelVar`** (after layouts -- see invariant #4)
13. Pass 1.2: async struct-method coro predeclare
14. Pass 1.4: predeclare module-level functions
15. Pass 1.45: predeclare async free-fn coro variants
16. Pass 1.5b: `genStructMethods`
17. Pass 2: predeclare remaining functions (for mutual recursion)
18. Pass 2.5: mark async fns coro-callable
19. Pass 2.8: register const declarations in scope
20. Pass 3: non-extern function bodies
21. Pass 4: register exported constants under qualified keys
22. Pass 5: register exported macros
23. Pass 6: propagate re-exported child-package macros

### `loadPackageFromFilePath` (`use "./file"`, `codegen/packages_file.go`)

1. lex + parse
2. recurse into nested `use`
3. Pass 0.5: `preregister`
4. Pass 1: extern funcs
5. Pass 1.2: async struct-method coro predeclare
6. Pass 1.5a: `genStructLayout` + bare-name type aliases
7. Pass 1.6: `genDataDecl`
8. Pass 0.8: overload-name scan
9. Pass 1.45: predeclare async overloaded free fns
10. Pass 1.5c: predeclare ALL module-level async coro variants
11. Pass 2: predeclare non-extern funcs
12. Pass 1.5b: `genStructMethods`
13. Pass 2.8: register top-level consts in scope
14. Pass 3: non-extern function bodies
15. symbol propagation into the parent scope

---

---

## Adding a new pass

1. Read the invariants section.
2. List every `cg.*` field your pass reads from. For each, find the pass
   that writes that field. Your pass must run after the latest of those.
3. List every `cg.*` field your pass writes. For each, find the earliest
   pass that reads it. Your pass must run before the earliest of those.
4. Insert in `Generate`, then mirror in `loadPackageFromSource` and
   `loadPackageFromFilePath` so the entry program and `use`d packages
   behave identically.
5. If your pass writes any global LLVM symbol (`predeclare*`,
   `genStructLayout`, `genDataDecl`), make sure both package drivers run
   it before the symbol can be reached from a method or free-fn body.
6. Update this doc.

---

## See also

- [memory.md](memory.md) - ARC header + heap-block layout the codegen targets
- [values.md](values.md) - `TinString` / `TinSlice` shapes
- [fn-coloring.md](fn-coloring.md) - how `$coro` variants are picked
- [call-site-generics.md](call-site-generics.md) - on-demand monomorphization
