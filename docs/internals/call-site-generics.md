# Call-site generics

A function or method may declare some slots of its return type with `_`
(wildcard). At each call site those slots are filled in by context, and
the compiler emits a separate monomorphization for each unique fill.
This is in addition to — not instead of — the monomorphizations
produced by the function's own generic parameters and (for impl methods)
by the data type's parameters.

The key idea: **a wildcard is just another type-parameter axis, with
its substitution coming from call-site context rather than from explicit
syntax or argument inference**. Mechanically it is normal compile-time
monomorphization.

## Shape

```
data Result[T, E](tryable[T, Result[_, E]]) =
  Ok(v T)
  Err(msg E)

  fn err_value(this Result[T, E]) Result[_, E] = ...
```

`Result[_, E]` is the wildcard-bearing return type. `_` is the
existential slot the impl says "I don't commit to here; let each call
pick."

## Per-call-site monomorphization

If `err_value` is called from four functions whose return types each
fix the wildcard differently:

```
fn a() Result[i64,    MyErr] = return try r
fn b() Result[string, MyErr] = return try r
fn c() Result[any,    MyErr] = return try r
fn d() Result[bool,   MyErr] = return try r
```

the compiler emits four monomorphizations of `err_value`, one per
distinct wildcard fill:

```
Result__T__MyErr_err_value__W_i64
Result__T__MyErr_err_value__W_string
Result__T__MyErr_err_value__W_any
Result__T__MyErr_err_value__W_bool
```

These coexist with the per-data-instance monomorphizations
(`Result__i64__MyErr_*`, `Result__bool__MyErr_*`, ...). The
wildcard fill is a separate axis: a method can be both
data-monomorphized and call-site-monomorphized, with the cartesian
product determining the final symbol set.

Each call site emits a direct call to the appropriate
monomorphization. There is no runtime dispatch, no tag-walk, no
extra cost beyond a regular function call.

## What gets substituted

Three substitution sources combine into one map at codegen:

- The data type's generic parameters (T, E for `Result[T, E]`).
- The function's own generic parameters, if any (`fn foo[U, V](...)`).
- The wildcard slots in the return type, resolved from call-site
  context.

A method with a wildcard return is keyed by (data-args,
function-typeargs, wildcard-args). Identical keys share a
monomorphization; distinct keys produce distinct symbols.

## Body type-checking under substitution

Source:

```
fn err_value(this Result[T, E]) Result[_, E] =
  match this:
    case Ok(_):  panic("err_value on Ok")
    case Err(_): return this
```

After substitution at one call site (`_` ↦ string):

```
fn err_value__W_string(this Result[T, E]) Result[string, E] =
  match this:
    case Ok(_):  panic("err_value on Ok")
    case Err(_): return this
       // ^ this : Result[T, E]; declared return is Result[string, E]
```

The body returns `this`, which after substitution does not have the
declared type. The compiler inserts a variant-aware reconstruction at
the return point: walk the source ADT's variants and rebuild each one
in the target ADT type. For a variant whose payload does not depend
on the wildcard slot (`Err(msg E)`, `None`), the reconstruction is a
direct copy. For a variant whose payload references the slot (`Ok(v
T)`, `Some(v T)`), the reconstruction is impossible at differing
fills and the body must guarantee at runtime that such a variant is
not produced (the impl typically does this via match arms that
panic).

The reconstruction is generic across any ADT pair with same-named
variants whose non-wildcard fields have matching types. There is no
hardcoded knowledge of `Result`, `Option`, `Err`, `None`, etc.

## Without the wildcard

If the impl declares the return type as `Result[T, E]` instead of
`Result[_, E]`, no extra monomorphizations are emitted and a call
site that expects a different success type fails with a regular
type-mismatch error. The wildcard is the explicit opt-in saying
"this slot is fillable per call."

## Resolution rules

The wildcard's substitution at a call site comes from whichever of
these is unambiguous:

- An explicit type annotation on the receiving binding:
  `let r Result[string, E] = adt.err_value()`.
- The enclosing function's declared return type, when the call's
  result is being returned: `return adt.err_value()` inside a
  function declared `Result[string, E]`.
- The argument's expected type at a call: passing the call's result
  into a function whose parameter type is `Result[string, E]`.

If multiple contexts apply they must agree. If no context fixes the
wildcard, the call is a compile error — type inference does not guess
across the wildcard, because the choice is supposed to be intentional
on the caller's part.

## Where wildcards may appear

- Inside trait-bound positions: impl headers
  (`data Foo[T](Bar[Foo[_, T]])`), generic where clauses
  (`where X is Bar[Foo[_, T]]`).
- Inside the return-type expression of a method whose enclosing trait
  bound also declares the wildcard (so the slot is named consistently
  between bound and signature).

Wildcards are not value-level types: the parser rejects `_` in
let-binding type annotations, function-parameter types, and other
non-bound positions, with the error message *"wildcards are only
valid in trait bounds; use a named generic parameter."*

## State of the implementation

In place:

- Parser accepts `_` and `_: T` wildcards in trait bounds.
- AST tracks `RetTypeHasWildcard` per FuncDecl, preserved through
  generic substitution.
- Per-call-site monomorphization (`ensureWildcardMono` in
  `codegen/exprs.go`): each unique target type produces a distinct
  LLVM symbol named `<Type>_<method>__W_<targetType>`. Cached by
  mangled name in `cg.wildcardMonos`.
- Dispatch at both `try`-keyword call sites (in `genTryExpr`) and
  general method calls (in `genCallExpr`). For the latter, the
  target type comes from `cg.returnTypeHint` (set by let-bindings
  with type annotations and similar context-bearing positions).
- The variant-aware reconstruction codegen (`rewrapTryable`) is
  generic across ADT pairs with payload-compatible same-named
  variants. No hardcoded knowledge of Result, Option, Err, None.
- Tin-level errors at the call site:
  - "wildcard slot needs context to fill" when a wildcard-return
    method is called without a context-bearing position.
  - "cannot propagate X through Y; impl did not declare a wildcard
    slot" when the impl is missing the wildcard. Neither leaks
    LLVM IR diagnostics.

Migration steps still pending:

- The mono body currently wraps the impl method (calls it, then
  reconstructs the result), rather than re-emitting the impl's body
  under the wildcard substitution. The wrapper-around-impl form is
  semantically equivalent — each call site dispatches to a distinct
  per-W symbol, which is the call-site generics feature in full —
  but a future refactor could re-emit the body directly so the
  reconstruction falls out of normal return-type coercion. The
  migration is structural and does not change observable behavior.
- Function arguments at the call site are not yet a context source.
  Currently only let-bindings with annotations and the enclosing
  return type provide context. Adding the arg path is straight-
  forward: thread the formal parameter type as the hint while
  evaluating the argument expression.
