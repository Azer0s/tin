# Operator overloading via built-in traits

Compile-time operator overloading. Operators (`+ - * / % == != < > <= >= ! [ ]
++` and friends) on user-defined struct types lower to method calls on
generic built-in traits the user (or compiler, for primitives) implements.
Multi-impl across type combinations is supported by reusing the existing
`traitImplKey()` machinery (`codegen/decls.go:998-1012`) — the same
mechanism that lets one struct implement `iter[i64]` and `iter[char]`
simultaneously.

ADTs are intentionally out of scope; tagged sums aren't realistic operator
targets and adding `Implements` to `DataDecl` is its own decision.

## Operator traits

All instance-form, all generic, declared with the `as fn` shorthand
(parser already supports this — `examples/iter.tin:15`, `parser/decls.go:240`):

```
trait add[rhs, ret]       as fn(this Self, other rhs) ret
trait sub[rhs, ret]       as fn(this Self, other rhs) ret
trait mul[rhs, ret]       as fn(this Self, other rhs) ret
trait div[rhs, ret]       as fn(this Self, other rhs) ret
trait mod[rhs, ret]       as fn(this Self, other rhs) ret

trait neg[ret]            as fn(this Self) ret
trait pos[ret]            as fn(this Self) ret
trait not[ret]            as fn(this Self) ret

trait comp[rhs]           as fn(this Self, other rhs) bool   // ==, !=
trait ord[rhs]            as fn(this Self, other rhs) i64    // <0/0/>0 -> <,<=,>,>=

trait index[key, ret]     as fn(this Self, k key) ret
trait index_set[key, val] as fn(this Self, k key, v val)

trait concat[rhs, ret]    as fn(this Self, other rhs) ret    // ++
```

Operator → trait-name table the parser uses for the lowering:

| Operator           | Trait      | Method lowering          |
|--------------------|------------|--------------------------|
| `+`                | `add`      | `a.add(b)`               |
| `-` (binary)       | `sub`      | `a.sub(b)`               |
| `*`                | `mul`      | `a.mul(b)`               |
| `/`                | `div`      | `a.div(b)`               |
| `%`                | `mod`      | `a.mod(b)`               |
| `-` (unary)        | `neg`      | `a.neg()`                |
| `+` (unary)        | `pos`      | `a.pos()`                |
| `!`                | `not`      | `a.not()`                |
| `==` / `!=`        | `comp`     | `a.comp(b)` (negated for `!=`) |
| `<` `<=` `>` `>=`  | `ord`      | `a.ord(b)` (compared to 0) |
| `a[k]` (rvalue)    | `index`    | `a.get(k)`               |
| `a[k] = v`         | `index_set`| `a.set(k, v)`            |
| `++`               | `concat`   | `a.concat(b)`            |

`ord` returns i64 (negative/zero/positive — matches existing `str::strcmp`
and `examples/stress_tests/trait_generics_combo.tin:31` `comparable` patterns).
i64 keeps the common `return this.key - other.key` impl style ergonomic.

### Where-clause shorthand

Single-param trait constraints get sugared:

```
where t is ord            // sugar for: where t is ord[t]
where t is comp           // sugar for: where t is comp[t]
where t is neg            // sugar for: where t is neg[t]
```

This preserves all existing `where t is ord` / `where t is comp` usages
in the codebase. Resolution: when the where-clause typechecker
(`codegen/funcs.go:1029` area) sees a bare trait name, if the trait has
exactly one type parameter, default it to the constrained type variable.
Multi-param traits (`add[rhs, ret]`, `index[k, v]`) require explicit args.

## Phases

### Phase 0 — Lockdown: silent fall-through becomes a compile error

**Why first**: today `codegen/exprs.go:1221` falls off the end of
`genBinExpr` for unhandled type pairs and silently returns `i64 0`.
`codegen/exprs_call.go:1701` returns nil from `genIndexExpr` similarly.
Both must die before adding overloading on top — otherwise some operator
expressions that *should* still be errors after Phase 2 (e.g. `Vec3 + i64`
when only `add[Vec3, Vec3]` is implemented) would silently codegen to 0
or crash at runtime.

**Change**:
- `genBinExpr`: replace silent `return constant.NewInt(irtypes.I64, 0)` with
  `return nil, cg.nodeErr(e, "binary operator %q is not defined for %s and %s", op, lhsTypeName, rhsTypeName)`.
- `genIndexExpr`: replace silent `return nil` with
  `return nil, cg.nodeErr(e, "type %s does not support index expressions", lhsTypeName)`.

**Tests**: `examples/operator_errors.sh` covering:
- `Foo + Foo` for `struct Foo` with no `add` impl
- `Foo + i64` similarly
- `Foo[k]` for `struct Foo` with no `index` impl
- `arr[k]` where arr is a non-indexable type (existing fat-array path stays valid)

Phase 0 ships standalone — it's a real soundness fix today.

### Phase 1 — Strict trait-impl naming

**Rule**: a function is recognized as a trait impl only when written in
qualified form. Anything else is a regular struct method, ignored by trait
binding.

| Trait shape | Impl method must be |
|---|---|
| Multi-method (`trait T = fn a(...) ...`) | `fn T::a(this Foo, ...) ret` |
| `as fn` (`trait T as fn(...) ret`) | `fn ::T(this Foo, ...) ret` |
| `as static fn` (`trait T as static fn(...) ret`) | `static fn ::T(...) ret` |

**Why**: today `fn len(this int_list) i64` inside `struct int_list(iter[i64])`
is auto-bound to `iter::len` by name. Typo in the bare name → struct
silently doesn't implement the trait, with a confusing downstream error.
Qualified form makes intent explicit and lets Phase 1b run a clean
completeness check.

`implicit[t]` (already declared `as static fn` per `examples/special_traits.tin:21`)
is unaffected — its impl form `static fn ::implicit(val f64) celsius`
already matches the rule.

**Carve-out for default-bodied methods.** Trait methods that have a default
body (e.g. `init`/`deinit` on the `observable` pattern) still accept the
bare-name override `fn methodName(this Foo)`. That's the documented chain
form: writing the bare name keeps the trait's default body running too,
while writing `fn Trait::methodName(this Foo)` replaces it entirely. Virtual
methods (no default body) are strict-qualified; bare-name impls error.

### Phase 1b — Completeness check

When a struct declares `(T1, T2, ...)`:
1. Walk each listed trait.
2. For each method whose body is `virtual` (no default), verify the struct
   provides a matching qualified impl with the right static-ness and signature.
3. Default-bodied methods (like `labeled.label()`) stay optional.

Errors should pinpoint the missing method:
> `struct foo declares iter[i64] but does not implement fn iter::get(this foo, i i64) i64`

When future trait inheritance lands (`trait B(A) = ...`), the same walker
recurses through inherited methods transparently.

### Phase 1c — Migration

Mechanical rewrite of stdlib + examples that bind trait methods by bare name.
Affected files (~12 in stdlib, more in examples):
- `stdlib/io/io.tin`, `stdlib/sync/awaitable.tin`, `stdlib/sync/future.tin`
- `stdlib/encoding/{json,yaml}/`
- `stdlib/collections/{list,map,linked_list,hashmap}.tin`
- `stdlib/ioutil/`, `stdlib/net/{tcp,udp,unix}/`, `stdlib/time/`, `stdlib/guid/`
- `examples/iter.tin`, `examples/special_traits.tin`, `examples/reflection.tin`,
  `examples/json_yaml_traits.tin`, `examples/test_{json,yaml}.tin`

Single-pass `s/^(  fn )(<methodName>)\(/\1<traitName>::\2(/` per impl.

### Phase 2 — Built-in operator traits + where-shorthand

Add `registerBuiltinOpTraits()` mirroring `registerBuiltinTraits()` at
`codegen/codegen.go:987-1014`. Construct each trait as an alias-form
`TraitDecl` (`IsAlias: true`, `IsStaticAlias: false`, `AliasType: <FuncType>`).

Synthesize built-in impls of every operator trait for every primitive type
so `where t is ord[t]` resolves for `t = i64`, `f64`, etc. without a
user-written impl. Concretely: when codegen sees a primitive type used
in a constraint or operator position, it emits the corresponding vtable
entry on demand (no user-visible source).

Add the where-shorthand resolver: in `codegen/funcs.go` near the current
`ord`/`comp` switch (line 1029), if the trait name resolves to a generic
trait with exactly one type parameter, default the parameter to the
constrained type variable.

### Phase 3 — Operator dispatch

In `genBinExpr` (`codegen/exprs.go:875`), before the existing
primitive-arithmetic switch:

1. Compute `lhsTy`, `rhsTy`.
2. If both are user types and an operator trait impl exists (look up via
   `traitImplKey(lhsTy, "add", rhsTy)`), lower to `lhs.add(rhs)` — produce
   the same IR as a hand-written method call. The existing trait/vtable
   machinery handles dispatch; LLVM constant-folds the vtable load when
   the receiver type is statically known (no special operator code path).
3. **Asymmetric primitives**: if lhs is primitive and rhs is a struct that
   implements `add[<lhsTy>, ?]`, swap and rewrite to `rhs.add(lhs)`.
   Only applies to commutative operators (`+`, `*`, `==`). Non-commutative
   (`-`, `/`, `%`, `<`, `>`) require the user to provide the explicit form.
4. Else → Phase-0 error fires. Single error path.

Same pattern for unary operators in `genUnaryExpr`.

### Phase 4 — Index / IndexSet

In `genIndexExpr` (`codegen/exprs_call.go:1617`):
- Existing primitive cases (fat array, fixed array, raw pointer, SIMD) unchanged.
- New: if receiver is a user struct/data with `index[K, V]` registered,
  lower `a[k]` to `a.get(k)`.

For index-assignment (find via `IndexAssign` in stmt-codegen):
- New: if receiver implements `index_set[K, V]`, lower `a[k] = v` to `a.set(k, v)`.

### Phase 5 — REPL highlight

In `repl/highlight.go`:
- Add `ansiBoldBlue = "\033[1;34m"`.
- Extend `colorFor()` with a case for the operator tokens that resolve to
  operator traits:
  ```go
  case t == lexer.PLUS, t == lexer.MINUS, t == lexer.STAR, t == lexer.SLASH,
       t == lexer.PERCENT, t == lexer.EQEQ, t == lexer.NEQ,
       t == lexer.LT, t == lexer.LTEQ, t == lexer.GT, t == lexer.GTEQ,
       t == lexer.LBRACKET, t == lexer.RBRACKET, t == lexer.NOT, t == lexer.INC,
       t == lexer.PLUSEQ, t == lexer.MINUSEQ, t == lexer.STAREQ,
       t == lexer.SLASHEQ, t == lexer.PERCENTEQ:
      return ansiBoldBlue
  ```
- `LBRACKET` / `RBRACKET` will color blue everywhere (also in type params and
  array literals); acceptable consistency cost.

Compound assignments (`+=`, `-=`, ...) are operator-trait-driven too —
`a += b` desugars through `add`.

## Sequencing

Phase 0, 1, 1b, 1c are independently shippable, each its own commit. Phase 2
+ Phase 3 ship together (a trait without dispatch is dead code; dispatch
without a trait has nothing to call). Phase 4 follows. Phase 5 anytime.
