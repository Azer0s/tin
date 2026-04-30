# Plan: Struct control tags + per-field mutability

Status: **implemented** (Phases A-D).
- Phase A: `dd0d3ad` - per-field `const` / `var` modifier
- Refinement: `2f45a6e` - const is compile-time only; `setfield` + `&` allowed
- Phase B+C: `beb5703` - scoped struct tags + `#const@field` default-flip
- Phase D: documentation in `docs/05-structs.md` and `docs/13-control-tags.md`

Motivation:
- SPEC.md line 238 shows `struct{ #pure@fn #const@field } str(...)` as a desired
  syntax; the `@fn` / `@field` qualifier on struct-level tags is parsed today
  (`parser/parser.go:328-332`) but the qualifier is discarded and no propagation
  happens.
- The existing `#packed` struct tag is implemented and tested
  (`codegen/decls.go:169`, `examples/packed_struct.tin`) but missing from
  `docs/13-control-tags.md`.
- Fields currently have no way to express immutability. Tin has `let` vs `var`
  at the variable level; fields should get the same treatment.

Scope: this plan covers (1) scoped struct-level tags that propagate to methods
and fields, (2) per-field `const` / `var` mutability prefix, and (3) documenting
`#packed`. It does NOT cover new tags like `#opaque`, `#align(N)`, `#repr(c)`.

---

## 1. Per-field `const` / `var`

### Syntax

A field declaration may begin with an optional `const` or `var` keyword, placed
before the field name. Unmarked fields default to mutable (current behaviour).

```
field_decl := [const|var] <name> [weak|own] <type> [forward] [@"metadata"]
```

### Examples

```rust
struct point =
  const x i64
  const y i64
        z i64          // unmarked - mutable (current default)

struct message =
  const from   string
  const body   string
  var   status i64     // explicit mutable

struct tree =
  const value i64
  const left  own *tree    // const + ownership modifier coexist
  var   cache weak *tree   // var + weak
```

### Semantics

`const` is a **compile-time-only, syntactic** tag. It rejects writes through
the field's named identifier; it does not instrument runtime-dispatched or
pointer-mediated paths. Concretely:

- `s.f = expr` — compile error when `f` is `const`.
- `s.f op= expr` (augmented assign), `s.f++` — compile error.
- `setfield(s, "f", v)` — **allowed**. `setfield` is reflective, by design
  dynamic, and treated as an explicit escape hatch.
- `&s.f` when `f` is `const` — **allowed**. Taking an address is not an
  assignment. Writes through the returned pointer are not tracked; const
  is not a safety guarantee against pointer-based mutation.

`const` does NOT affect:

- ARC retain/release — a const pointer field still retains on assign (at
  construction) and releases on free.
- Field reads — always allowed.
- Replacing the whole struct `s = S{...}` — this is a variable assignment,
  not a field write. The binding (`let` vs `var`) controls whether that's
  allowed; `const` field modifier does not.
- Construction paths — struct literal, positional init, destructuring `let`,
  match-arm bindings, trait-forwarded initial values are all fine (they set
  the initial value; they do not mutate an already-constructed struct).

### `init` / `deinit`

`init(this T)` receives the struct by value after every field is set. Field
writes inside `init` affect only the local copy, so `const` does not constrain
`init` in a user-visible way. Implementation may still statically reject
`this.const_field = ...` inside `init` for consistency, since the write is
dead anyway.

`deinit(this T)` runs before field release; it is effectively read-only.

### Interaction with traits

A trait's `forward` field declaration may use `const` / `var`:

```rust
trait size =
  const s size_t forward
  fn size(this size) size_t = return this.s
```

Implementers inherit the constness. The default `size()` body reads the field,
which is fine; any method that tried to assign `this.s = ...` would be
rejected.

### Grammar integration

`const` and `var` are already reserved keywords in Tin. The parser's
field-loop in `parseStructDecl` must accept an optional leading
`KW_CONST` or `KW_VAR` token before the identifier. Disambiguation: the
token at position 0 of a struct-body line is either `const`/`var`/`static`/`fn`
or an identifier. This extension introduces no new ambiguity.

---

## 2. Scoped struct-level tags (`#tag@scope`)

### Syntax

```
struct{#tag1 #tag2@scope} Name(...) = ...
```

Each tag is optionally suffixed with `@<scope>`. Unqualified tags apply to
the struct itself (e.g. `#packed`). Qualified tags propagate to members.

### Scopes

| Scope         | Members covered                                                    |
|---------------|--------------------------------------------------------------------|
| `@fn`         | every `fn` declared in the body — both instance and static methods |
| `@method`     | instance methods only (excludes `static fn`)                       |
| `@static_fn`  | `static fn` only                                                   |
| `@field`      | every declared field                                               |

### Tag-scope compatibility matrix

| Tag               | `@fn` / `@method` / `@static_fn` | `@field` | Struct-level (no scope) |
|-------------------|----------------------------------|----------|-------------------------|
| `#pure`           | yes                              | error    | error                   |
| `#sideffect`      | yes                              | error    | error                   |
| `#no_recurse`     | yes                              | error    | error                   |
| `#no_thread`      | yes                              | error    | error                   |
| `#heavy`          | `@fn`, `@method`                 | error    | error                   |
| `#no_autoyield`   | yes                              | error    | error                   |
| `#async`          | `@fn`, `@method`                 | error    | error                   |
| `#handover`       | error (extern-only)              | error    | error                   |
| `#const`          | error                            | yes      | error                   |
| `#packed`         | error                            | error    | yes                     |

Anything outside this matrix is a compile error raised at the struct
declaration site.

### `#const@field` as default-flipper

`#const@field` does NOT directly rewrite every field to `const`. Instead it
flips the default: in a struct tagged `#const@field`, an unmarked field means
`const`; a `var`-marked field stays mutable. This lets users declare
"everything is const except X" concisely:

```rust
struct{#const@field} message =
        from   string     // const (inherited default)
        body   string     // const
  var   status i64        // explicit var overrides the default
```

Without the tag, the unmarked default remains mutable (backwards compatible).

### Propagation semantics for function tags

For each scoped tag `#T@scope` on the struct and each matching member M:

1. If M already has `#T` (explicitly or by other propagation) — no-op.
2. If M has a tag that **conflicts** with `#T` — member wins, propagation
   skipped silently. Conflict pairs:
   - `(#pure, #sideffect)`
   - `(#heavy, #no_autoyield)`
3. Otherwise — `#T` is added to M's tag set.

Rationale for silent override: CSS-style cascading. Forcing users to resolve
every conflict with explicit syntax adds ceremony for the 95% case where there
is no conflict. Users can still inspect the effective tag set with tooling.

Extern methods carry an auto-`#sideffect`. A `#pure@fn` struct with an extern
method therefore hits rule (2) — extern wins, propagation silently skipped.

### Empty-match scopes

A scoped tag that matches zero members in the current body is not an error.
This keeps future method additions honest — add a method tomorrow and it picks
up the propagation automatically.

### Example

```rust
struct{#pure@fn #const@field} vec2 =
        x f64                              // const by default
        y f64                              // const by default
  var   recompute_count i64                // mutable override

  fn magnitude(this vec2) f64 =            // implicitly #pure
    return sqrt(this.x * this.x + this.y * this.y)

  fn{#sideffect} bump_counter(this vec2) = // #sideffect wins; #pure skipped
    this.recompute_count = this.recompute_count + 1
```

---

## 3. Document `#packed`

`#packed` is already implemented; the spec needs a short section in
`docs/13-control-tags.md` describing it. Proposed addition:

> ### `#packed` (struct-level)
>
> Applied to a struct declaration. Fields are laid out contiguously with no
> padding between them. Size of the struct equals the sum of field sizes.
>
> ```rust
> struct{#packed} record =
>   tag   u8
>   value u32
> // sizeof(record) = 5 (without packing: 8 due to u32 alignment)
> ```
>
> Use this for binary protocol buffers, wire formats, or C ABI compatibility
> with `__attribute__((packed))`. Unaligned field access may be slower on
> architectures that do not support it natively; the compiler emits the
> appropriate LLVM `align 1` annotations.

Plus a new row in the Quick Reference table:

| Tag                 | Applies to | Enforced        | Meaning                                |
|---------------------|------------|-----------------|----------------------------------------|
| `#packed`           | struct     | Yes (layout)    | Fields contiguous, no alignment padding |

---

## 4. Implementation plan

### Phase A — per-field `const` / `var` (independent, small)

1. Lexer — already has `KW_CONST` and `KW_VAR`; no change.
2. AST — add `IsConst bool` to `ast.StructField` (a new `IsVar` would be
   redundant since `!IsConst && !inheritedConstDefault` == var). A `Mutability`
   enum (`inherit` / `const` / `var`) is cleaner but more verbose; pick one
   per review.
3. Parser — extend the field loop in `parseStructDecl` to accept an optional
   leading `KW_CONST` or `KW_VAR` before the identifier. Set the new field.
4. Codegen — at every AST-level field-write site (`s.f = x`, `s.f op= x`,
   `s.f++`), check the field's resolved constness and emit a clear error when
   violated. `setfield` and `&s.f` are intentionally not checked: const is a
   compile-time syntactic tag, not a runtime guarantee.
5. Tests — unit examples in `examples/` covering each rejection path and
   confirming reads continue to work.

### Phase B — scoped struct tags (depends on A for `#const@field`)

1. AST — change `StructDecl.Tags []string` to `Tags []TagWithScope` where
   `TagWithScope = { Name string; Scope string /* "" | "fn" | "method" | "static_fn" | "field" */ }`.
   Migrate call-sites (`hasTag` in codegen should still work via a helper
   that considers only `Scope == ""` tags for struct-level checks).
2. Parser — stop discarding `@qualifier`. Attach the scope to each tag.
   Validate the scope name at parse time against the allowed set.
3. Codegen — new pass `propagateStructScopedTags(decl)` that iterates
   `decl.Tags`, routes `@fn`/`@method`/`@static_fn` tags onto the matching
   method AST nodes, and applies `@field` tags. Runs before the existing
   `#pure` transitive check, the auto-yield classifier, and the const-field
   check so all downstream code sees the expanded tag set.
4. Codegen — compatibility-matrix validation: reject `#pure@field`,
   `#packed@fn`, etc. with a specific error message pointing at the struct.
5. Conflict resolution — when propagation would add a tag that conflicts with
   an existing member tag, skip silently. Emit a `-v-tags` trace line when
   that flag is set (mirrors `-fdump-demorgan` / `-fdump-heuristics`) so users can
   verify what propagated where.

### Phase C — `#const@field` default-flip

1. Requires Phase A (the `IsConst` field) and Phase B (the `@field` scope).
2. When a struct has `#const@field`, change the field loop's default: fields
   without explicit `const`/`var` are treated as `const`.
3. Tests — example that mixes unmarked + `var` in a `#const@field` struct.

### Phase D — documentation

1. Amend `docs/05-structs.md` with a "Field mutability" section describing
   `const` / `var` prefixes.
2. Amend `docs/13-control-tags.md` with:
   - New "Struct-level tags" section (covers `#packed`).
   - New "Scoped tags" section (covers `#tag@scope` syntax + matrix).
   - Updated Quick Reference table.
3. Add cross-references from `SPEC.md` line 238 example to the new doc
   sections.

### Phase order rationale

A is self-contained and provides immediate value (per-field immutability). It
can ship on its own. B + C unlock the struct-level shorthand. D can run
alongside each phase. Committing after each phase keeps the review surface
small.

---

## 5. Open questions / deferred decisions

1. **`&s.f` when `f` is const — resolved.**
   `const` is a compile-time-only tag. Taking the address of a const field is
   allowed; writes through the resulting pointer are not tracked by design.
   Users who want stronger guarantees should not hand out mutable pointers to
   const-field data in the first place.

2. **`Mutability` enum vs `IsConst bool` in `ast.StructField`.**
   Enum with three states (`inherit`, `const`, `var`) is more explicit but
   adds complexity. `IsConst bool` + a struct-level flag for "const default"
   is terser. Decide during Phase A.

3. **Does `#const@field` also imply a stronger guarantee** (e.g., ARC can
   skip retain/release on const pointer fields)?
   No — constness is about mutation; lifetime is unchanged. Retain on assign
   (at construction time), release on free. Keep the simple rule.

4. **Scope aliases.** Should `@method` be spellable as `@instance_fn`?
   Plan says no — one canonical name per scope. Bikeshed later if a user
   complains.

5. **Spread/update expressions for const-heavy structs** (like Rust's
   `Point { x: 5, ..old }`).
   Out of scope for this plan. Tracked separately.

6. **Per-field control tags beyond `const`** (`var x i64 #volatile`?).
   Not proposed. If future need arises, they become a separate line item.

---

## 6. Test plan (high level)

- Rejection cases (compile-time):
  - `const` field written via `s.f = x`, `s.f += x`, `s.f++`.
  - Scope/tag mismatch: `struct{#pure@field}`, `struct{#const@fn}`,
    `struct{#packed@fn}`.
  - Unknown scope name: `struct{#pure@blub}`.
- Non-rejection cases (const is compile-time-only):
  - `setfield(s, "f", v)` on a const field — allowed.
  - `&s.f` on a const field — allowed; pointer-based writes are not tracked.
- Propagation cases:
  - `struct{#pure@fn}` with two methods — both end up pure.
  - Explicit `fn{#sideffect}` override — that method is sideffect,
    others stay pure.
  - Extern method under `#pure@fn` — extern wins silently.
- Default-flip cases:
  - `struct{#const@field}` with mix of unmarked, `const`, `var` fields.
- Backwards compatibility:
  - Every existing struct in `examples/` and `stdlib/` compiles unchanged.
  - `#packed` continues to work on every existing test.
- Reflection:
  - `fieldnames` / `fieldtypes` output unchanged (constness is not
    reflected through atoms at runtime — it is a compile-time property).

---

## 7. Non-goals

- New struct-level tags (`#align(N)`, `#repr(c)`, `#opaque`).
- Macros on struct declarations.
- `own` / `weak` default-flip at struct level.
- Per-field control tags beyond `const` / `var`.
- Spread/update syntax for reconstruction.
