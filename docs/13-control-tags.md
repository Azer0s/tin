# 12 - Control Tags

Control tags annotate functions, methods, lambdas, macros, blocks, and
structs with compile-time constraints and properties. The compiler either
enforces them or uses them as documentation.

**Syntax:** `fn{#tag} name(...)` or `struct{#tag} Name = ...`  -  one or
more tags in braces after the keyword. Struct tags additionally accept a
member-scope qualifier (`#tag@fn`, `#tag@field`, etc.) that propagates
the tag to matching struct members; see [Struct tags](#struct-tags).

**Naming convention:** all lowercase with underscores. Tags describe what a
thing *is* or *has*, not what it *cannot* do.

---


## See also

- [Function / method / lambda tags](13-control-tags-fn.md)
- [Block, struct, and macro tags](13-control-tags-block-struct.md)

---

## Semantics and known limitations

### `#pure` enforcement

The compiler follows the full call graph reachable from a `#pure` function and
rejects any call whose purity cannot be fully verified:

**Function pointer / indirect calls**  -  calling through a function-typed
variable is rejected, because the target cannot be statically verified:

```rust
fn{#pure} bad(f fn(i64) i64, n i64) i64 =
  return f(n)   // ERROR: indirect call through function pointer is not verifiable
```

**Package / module calls**  -  calls qualified with `::` (e.g. `math::sqrt`,
`assert::equals`) are rejected, because external package functions are not in
the compiler's function registry and cannot be transitively checked:

```rust
use math
fn{#pure} bad(n f64) f64 =
  return math::sqrt(n)   // ERROR: cannot verify purity of package call "math::sqrt"
```

**Known-pure built-ins**  -  a small set of built-in functions (`len`, `sizeof`,
`default`, `typeof`, `traitof`, `fieldnames`, `fieldtypes`, `fieldtag`,
`getfield`) are always allowed inside `#pure` functions.

**Method call resolution**  -  when a `#pure` function calls a method via
`obj.method()`, the compiler performs a suffix search across all registered
struct methods named `method`. If *any* matching struct method is `#sideffect`
or extern, the call is rejected. Unknown methods (from external types) are
rejected because their purity cannot be verified.

### `#no_recurse` catches all recursive paths

`#no_recurse` performs a transitive AST-level call-graph walk, similar to
`#pure`. Any path through any number of helper functions that leads back to the
tagged function is a violation. Indirect calls through function pointers cannot
be statically resolved and are conservatively allowed (not traced).

Functions tagged `#pure #no_recurse` are exempt: they are CTFE macros whose
self-calls are evaluated at compile time rather than emitted as runtime calls.

---

## Quick reference

| Tag                | Applies to              | Enforced                         | Meaning                                      |
|--------------------|-------------------------|----------------------------------|----------------------------------------------|
| `#pure`            | fn / method / lambda    | Yes  -  transitive AST walk      | No side effects                              |
| `#sideffect`       | fn / method / lambda    | No (declaration)                 | Has side effects; auto-applied to extern fns |
| `#no_recurse`      | fn / method / lambda    | Yes  -  transitive AST walk      | Must not call itself (at any depth)          |
| `#no_thread`       | fn / method / lambda    | No (advisory)                    | Unsafe for concurrent use                    |
| `#allow_sideffect` | block                   | Yes  -  suppresses `#pure` check | Permits side effects in this block           |
| `#unsafe`          | block                   | Yes  -  required for raw pointer ops | Permits pointer arithmetic and `addr(int)` |
| `#no_excl`         | macro                   | Parser                           | Callable without `!` suffix                  |
| `#no_parens`       | macro                   | Parser                           | Callable without parentheses                 |
| `#async`           | fn / method / lambda    | No (enables fiber codegen)       | Runs as a cooperative green thread (fiber)   |
| `#no_autoyield`    | fn / method / lambda    | No (disables codegen)            | Suppresses auto-yield at loop backedges and call sites |
| `#heavy`           | fn / method             | No (changes heuristic)           | Forces "auto-yield" classification; callers in `$coro` yield before calling |
| `#handover`        | fn (extern only)        | No (changes codegen)             | Transfers ownership of returned C pointer into ARC |
| `#interop`         | top-level fn            | Yes  -  signature whitelist      | Emits a C-callable wrapper alongside the Tin entry point |
| `#packed`          | struct (unscoped)       | Yes (layout)                     | Fields laid out contiguously, no alignment padding |
| `#<tag>@fn`        | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every method (instance + static) |
| `#<tag>@method`    | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every instance method |
| `#<tag>@static_fn` | struct (scoped)         | Propagation + tag's own check    | Propagates `#<tag>` to every `static fn`     |
| `#const@field`     | struct (scoped)         | Yes (default-flip)               | Unmarked fields become `const` by default; `var` opts out |
