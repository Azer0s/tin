# Tin masterplan: language features + stdlib rework

This is the single source of truth for what we're building. It unifies
two previously-separate documents (the runtime/builtin language plan
and the stdlib rework plan) into one execution narrative. Update this
file as work lands; do not let the two predecessor plans drift back.

## Overview

There are two interlocking workstreams plus a refactor pass:

**Workstream A - language features.** Partial generic bounds, ADT trait
impls, unified static-table vtables, the `tryable` trait and `try`
keyword, method-call qualification, the `runtime/builtin/` directory,
and the `Awaitable` → `awaitable` rename. These are foundational
type-system and codegen changes.

**Workstream B - stdlib rework.** Typed errors (`errors::Err` trait
plus per-domain ADTs), `(T, Err)` → `Result[T, errors::Err]` migration,
`os::File` and process management, `io::copy`/`BufWriter`, DNS, and
JWT. Most of this can ship with today's language. Phase 2 is partially
in flight - recent commits land `OsError`, `StringsError`, `TimeError`,
`DecimalError`, JSON/YAML errors using the wrapper-struct pattern
(ErrorKind ADT + wrapper struct implementing `errors::Err`).

**Workstream C - cross-cutting refactor (after A).** Once ADT trait
impls + widening land, collapse the per-domain pattern: the wrapper
struct goes away and the `XxxError` ADT directly implements
`errors::Err`. Stdlib code adopts the `try` keyword in place of
explicit Ok/Err pattern matching. Result return signatures stop
needing `*errors::Err` indirection; the ADT widens to the trait
fat-pointer at the return site.

The two workstreams are largely independent - A is compiler/runtime
changes, B is library code - so they can run in parallel or
interleaved. C is sequenced strictly after A.

## Status snapshot (2026-05-09)

- **Workstream A**: not started.
- **Workstream B**:
  - Phase 0: not started.
  - Phase 1 (errors::Err trait + StringErr): exists; verify against
    locked-in pattern.
  - Phase 2: in progress. Done so far: `os::read_file` family, `os_dirs`
    + `os_stat`, `strings::parse_int` family, `time::parse{,_rfc3339}`,
    `decimal::parse`, JSON/YAML errors. Remaining: net (tcp/udp/unix),
    `strings::index_of`/`last_index_of` → Option, regex, csv, flag.
  - Phases 3–6: not started.
- **Workstream C**: blocked on Workstream A.

## Locked-in design decisions

Merged from both predecessor plans. Each decision is permanent unless
explicitly revisited.

### Language (Workstream A)

A1. **Partial generic bounds.** Type parameters in trait-bound
   positions may be partially-applied type constructors with `_` slots,
   e.g. `tryable[V, Result[_, E]]`. A `_` denotes an
   existentially-bound slot the impl picks. Any number of `_`s may
   appear in a single bound. Anonymous `_` is the default; naming via
   `_: T` is only used when the trait body wants to refer to the type
   (e.g., compile-time `match typeof(T)` dispatch). `_` is never a
   value-level type - it appears only inside trait bounds.

A2. **Unified static-table vtables for all values.** No value of any
   kind carries embedded vtable pointers. Struct layout becomes
   `{type_id, user fields}`; ADT layout stays
   `{type_id, tag, payload}`. Widening a value to a trait-object type
   produces a fat pointer `(data_ptr, vtable_ptr)` where `vtable_ptr`
   comes from a single static table keyed by `(type_id, trait_id)`.
   ADTs and structs become symmetric at the dispatch layer.

A3. **Source-level compatibility for existing struct trait-call code.**
   The static-table migration is internal: struct values shrink, but
   user-facing semantics (which method dispatches, ARC behavior,
   reflection results) are preserved.

A4. **`runtime/builtin/` for language-defined traits only.**
   Compiler-known traits live in `runtime/builtin/`, separate from
   `stdlib/`. Auto-loaded into every Tin program. Concrete types
   (`Result`, `Option`) stay in stdlib.

A5. **Naming.** Compiler-known traits are lowercase
   (`tryable`, `awaitable`, `add`, `sub`, `comp`, `ord`, `print`,
   `iter`, …). Library traits remain PascalCase (`Reader`, `Writer`,
   `Closeable`, `Hasher`, `Err`, `List`, `Map`). `Awaitable` is renamed
   to `awaitable`. No backward-compat alias - Tin is pre-alpha.

A6. **`tryable` is a three-method trait** parameterized over success
   type `V` and container `C`. Methods are `is_err`, `ok_value`,
   `err_value`. `is_err` is the propagation predicate, `ok_value`
   returns `V`, `err_value` returns `C` (the container to propagate).
   Methods are independent operations, not contractually partial.

A7. **V and C are independent in `tryable[V, C]`.** The trait does not
   enforce that V equals C's existential slot.

A8. **`try` is an expression-position keyword** with no hardcoded
   knowledge of specific containers. Desugar dispatches through trait
   methods, so any `tryable` impl works with `try`.

A9. **Cross-container `try`** is allowed when the enclosing fn returns
   the same `C` as `expr.err_value()`. Otherwise the user converts
   explicitly with `.map_err(...)`.

A10. **`return try expr` triggers a warning.** Yields the success
    value, usually wants `return Ok(try expr)`. Warning + underlying
    type error if applicable.

A11. **Multi-`try` evaluates left-to-right.** Each desugared block
    runs to completion before the next.

A12. **Option does not auto-convert to Result.** Option's `tryable`
    impl uses `Option[_]` as its container.

A13. **Method calls on values resolve unqualified when unique;
    ambiguity demands trait-qualified calls** of the form
    `Trait[Args]::method(receiver, args)` or
    `receiver.Trait[Args]::method(args)`. Compiler-emitted desugars
    (`try`, `await`, operator desugars) always use the qualified form.

### Stdlib (Workstream B)

B1. **Typed errors via a trait + per-domain ADT.** Until Workstream A
    lands, the wrapper-struct pattern is used:
    - `data XxxErrorKind = Variant(payload) | ...` - variant set
    - `struct XxxError(errors::Err) = _kind XxxErrorKind` - trait impl
    - free `error_message(k XxxErrorKind) string` - formatter
    - `xxx::as_xxx(e errors::Err) Option[*XxxError]` - safe downcast

    Functions return `Result[T, errors::Err]`. Caller can use
    `e.message()` generically or downcast for variant recovery.

    **Future state (Workstream C):** when ADT trait impls + widening
    land, the wrapper struct disappears and `XxxError` is itself an
    ADT implementing `errors::Err` directly. Function signatures stay
    `Result[T, errors::Err]`; the ADT widens to the trait at the
    return site.

B2. **`HashMap.get` and `[]` both return `(V, bool)`.** `[]` does
    comma-ok auto-unwrap-or-panic in `genIndexExpr`. `m.get(k)` is two
    overloads selected by return-type context: `(V, bool)` or `V`
    (panics on miss). Bare `let v = m.get(k)` is ambiguous, errors
    out. Auto-unwrap stays scoped to `[]`.

B3. **`Stream[t]` async-iter trait is dropped.** Channels cover the
    pull-driven async case.

B4. **Deferred indefinitely:** HTTP server, TLS server, iterator
    combinators, TOML/XML/MessagePack, per-module log levels, log
    file rotation, HashSet/Deque/Heap.

B5. **JWT is the sole user-facing feature in Workstream B's scope.**
    Validates the typed-error pattern and the `Hasher`/HMAC machinery.

---

## Workstream A: language features

The four commits below land in order. Detail for each piece follows.

### A.commit 1 - Partial generic bounds

#### Where `_` may appear

`_` (anonymous) and `_: T` (named) appear only in trait-bound
positions:

- Inside an impl-side trait bound (header tuples on `data`/`struct`).
- Inside a `where` clause on a generic function:
  `where T is tryable[V, Result[_, E]]`.
- Inside a concretely-instantiated trait-object type at a struct
  widening site (the `_` resolves to a concrete type at widening).

`_` is never a value-level type. Variable types, function parameter
types, return types, and let-binding annotations do not accept `_` -
that is a parse error with the message *"wildcards are only valid in
trait bounds; use a named generic parameter."*

#### Anonymous vs. named

Anonymous `_` is the default. Multi-slot bounds like `Map[_, _]` are
legal without naming - the impl-matcher handles each slot positionally
and binds each independently.

`_: T` introduces a name only when the trait body or impl methods need
to refer to the existential type by name (most common: compile-time
`match typeof(T)` dispatch). Names are scoped to the trait body and
impl methods; they are not visible at use sites.

#### Semantics

- At the impl site, each `_` is bound to the impl's concrete witness
  type. `Result[T, E]` impls `tryable[T, Result[_, E]]` with `_ := T`.
- At the use site, the impl-matcher unifies each `_` against the
  operand's concrete type and projects it back into method return
  types.
- Method-return projection: when a method signature references a
  named `_: T`, instantiation at the call site stamps in the witness
  from the impl. Equivalent to associated-types-by-position.

#### Type-checker work

- Parser: `_` and `_: T` accepted only in trait-bound positions.
- Impl-matcher: extends unification to handle `_` slots positionally.
- Compile-time type inspection: `_: T` names work with
  `match typeof(T)` to emit the selected branch at compile time.

#### Test cases (synthetic, no `try` yet)

- Single anonymous `_` in a trait bound; method returns the slot's
  resolved type.
- Multi-`_` anonymous (`Pair[_, _]`); two methods return each slot
  independently.
- Named `_: T` for typeof dispatch.
- Mixed universal/existential: `tryable[V, Result[_, E]]`.
- Negative: `_` in a function param type, in a let-binding type, in a
  value-level return type - all parse errors.

### A.commit 2 - ADT trait impls + unified static-table vtables

This single commit lands two coupled changes:

- **ADTs may implement traits** (header-tuple syntax, methods, widening).
- **Struct values lose their embedded vtable pointers.** All
  trait-object widening goes through one static vtable table keyed by
  `(type_id, trait_id)`.

These ship together because they share the static-table machinery.

#### Syntax (ADT impls)

```
data Result[T, E](tryable[T, Result[_, E]]) =
  Ok(v T)
  Err(msg E)

  fn tryable[T, Result[_, E]]::is_err(this Result[T, E]) bool = ...
  fn tryable[T, Result[_, E]]::ok_value(this Result[T, E]) T = ...
  fn tryable[T, Result[_, E]]::err_value(this Result[T, E]) Result[T, E] = ...
```

Parser must accept (a) a trait list after the data's generic params,
(b) `fn ::method` / `fn trait[...]::method` declarations inside a
`data` body. Today only structs accept this - verify and extend as
needed.

#### Layout (after migration)

```
struct Foo:                  { i32 type_id, user fields... }
data Result[T, E]:           { i32 type_id, i8 tag, payload... }
```

Both kinds carry only `type_id` at offset 0; no embedded vtable
pointers. Struct values shrink by 8 bytes per implemented trait per
value. ADT values are unchanged from today's layout.

#### Dispatch

- **Static dispatch** when the concrete type is known at the call
  site (no vtable lookup).
- **Dynamic dispatch** when calling through a widened trait-object
  pointer (one vtable load + one indirect call).

Uniform across ADTs and structs.

#### Widening via the static vtable table

Mechanism (uniform):

- Compiler emits one vtable per (concrete monomorphization,
  implemented trait) into static memory at link time. Keyed by
  `(type_id, trait_id)`.
- At a widening site, codegen reads `type_id` from offset 0 of the
  value (or, for compile-time-known types, references the static
  vtable symbol directly) and pairs it with `data_ptr` to form
  `(data_ptr, vtable_ptr)`.
- Trait pointers are non-owning. The caller keeps the underlying
  value alive. Owning trait pointers (`Box[T]`-style) are a separate
  feature, out of scope for this plan.

Partial-bound widening rule applies to all value types: a
trait-object type with `_` left existential is not a valid
value-level type. Concrete instantiations work.

#### `any` → trait widening: runtime-checked

When the source of a widening is a value of static type `any`
(or any other type-erased shape), the compiler does not know the
underlying concrete type at codegen time. The widening must defer
to runtime:

- Read the boxed value's `type_id` (still at offset 0, the same as
  the typed paths).
- Look up the static vtable table for `(type_id, trait_id)`.
- If an entry exists, build the fat pointer.
- If no entry exists, **panic**. Message names the source type and
  the requested trait so the user can see what was missing.

This is the only widening path that can fail at runtime; the static
paths are compile-checked. The runtime helper is shared with the
static path's lookup function (no second table; same data
structure).

Test: a value boxed into `any`, then widened to a trait the
underlying type does not implement, panics with a clear diagnostic.

#### Migration of existing struct code

Source-level: no changes. Codegen-level: struct declarations stop
emitting embedded vtable slots; struct widening sites change from
"load vtable from value header" to "lookup vtable in static table."

Audit checklist (see also "Items to confirm" at the bottom):

- FFI / C-interop sites that may have hardcoded struct field offsets
  - adjust offsets or hide behind accessor macros.
- ARC traversal code (`runtime/arc.c`) for assumptions about
  vtable-pointer slots between `type_id` and user fields.
- Any reflection or codegen-internal code that reads a vtable pointer
  from a struct value's header rather than via a static table.
- Tests asserting on `sizeof(SomeStruct)` or specific byte offsets.

#### Reflection

The existing `(type_id -> [trait_atom])` table
(`runtime/runtime.h:195`) stays. The static vtable table is added
alongside, keyed by `(type_id, trait_id)`. `traitof` and friends
keep working with no source-level change.

#### Motivating use case: errors::Err directly on ADTs

Today (Workstream B's wrapper pattern):

```
data IoErrorKind = NotFound(string) | Permission(string) | ...
struct IoError(errors::Err) =
  _kind IoErrorKind
  fn ::message(...) string = ...
  fn ::kind(...) atom = ...
```

Producers return `Result[T, errors::Err]`; callers receive a struct
trait pointer wrapping an ADT. Two values, one indirection.

After A.commit 2 (which is the unblock for Workstream C):

```
data IoError(errors::Err) =
  NotFound(string)
  Permission(string)
  ...

  fn errors::Err::message(this IoError) string = ...
  fn errors::Err::kind(this IoError) atom = ...
```

Producers still return `Result[T, errors::Err]`; the ADT widens to
the trait pointer at the return site. One value, one widening, no
wrapper.

#### Test cases

ADT-side:

- ADT impls a single trait, methods callable on values (static
  dispatch).
- ADT impls multiple traits, all callable.
- ADT impls operator traits (`add[Money, Money]` for a
  `data Money = USD(i64) | EUR(i64)`).
- ADT widening to a fully-concrete trait-object type, method dispatch
  through the fat pointer.
- Widening with `_` left existential at the widening site → compile
  error.
- Heterogeneous list `[errors::Err]` containing widened values from
  different ADT types.
- ADT in a generic-bound function (monomorphized).
- Partial-bound trait-header impl resolves correctly.
- The `errors::Err`-on-ADT use case round-trips: ADT widens, caller
  calls `.message()` and `.kind()` through the trait pointer.

Struct-migration regression:

- All existing struct trait-impl tests pass without source change.
- Struct values are smaller post-migration; size assertions update.
- Struct widening dispatches via the static table; behavior identical.
- ARC retain/release/traverse on struct values continues to work.
- `traitof(struct_value)` returns the same trait list as
  pre-migration.
- Heterogeneous list `[errors::Err]` mixing ADT and struct elements
  dispatches correctly.

### A.commit 3 - `runtime/builtin/` + awaitable rename + traits + qualification

Single commit covering:

- Create `runtime/builtin/` directory.
- Trait declarations in `runtime/builtin/`: `tryable`, `awaitable`,
  operator traits (`add`/`sub`/`mul`/`div`/`neg`), comparison traits
  (`comp`/`ord`), `implicit`, `print`, `iter` stub.
- `Awaitable` renamed to `awaitable`; file moves from
  `stdlib/sync/awaitable.tin` to `runtime/builtin/awaitable.tin`.
- Auto-load wiring (mirrors existing `sync` auto-load,
  `codegen/coro.go:144`).
- Operator-resolution migration from name-matching to trait-driven.
  Existing struct operator impls (e.g. `decimal::Value`) satisfy the
  new trait declarations bit-identically - decimal tests are the
  regression gate.
- Method-call qualification syntax + ambiguity detection (see below).

`Result` and `Option` do **not** move - they stay in stdlib.

#### Trait declarations

```
// runtime/builtin/tryable.tin
trait tryable[V, C] =
  fn is_err(this tryable[V, C])    bool = virtual
  fn ok_value(this tryable[V, C])  V    = virtual
  fn err_value(this tryable[V, C]) C    = virtual

// runtime/builtin/awaitable.tin
trait awaitable[T] =
  fn await_result(this awaitable[T]) T = virtual

// runtime/builtin/arith.tin
trait add[Other, Result] =
  fn ::add(this add[Other, Result], other Other) Result = virtual
trait sub[Other, Result] =
  fn ::sub(this sub[Other, Result], other Other) Result = virtual
trait mul[Other, Result] = ...
trait div[Other, Result] = ...
trait neg[Result] = ...

// runtime/builtin/cmp.tin: comp, ord
// runtime/builtin/conv.tin: implicit
// runtime/builtin/print.tin: print
// runtime/builtin/iter.tin: iter (stub for now; for-in keyword later)
```

#### Method-call qualification

Fully-qualified call syntax mirrors the impl side:

```
// method-style:
r.tryable[V, C]::is_err()
r.tryable::is_err()              // type params inferred when unambiguous

// function-style (UFCS):
tryable[V, C]::is_err(r)
tryable::is_err(r)
```

Default unqualified resolution: `r.method(args)` resolves when
exactly one trait `r`'s type implements has a matching method, OR
when only one such trait is currently in scope via `use`.

Ambiguity → compile error listing all candidates with a suggestion
to qualify:

```
error: ambiguous method call `r.is_err()` - receiver impls multiple
       traits with this method
  candidate 1: tryable[i64, Result[_, MyErr]]::is_err
  candidate 2: error_check::is_err
hint: qualify the call: `r.tryable::is_err()` or `r.error_check::is_err()`
```

Symmetric rule on the impl side: `fn ::method` shorthand legal only
when at most one implemented trait has a method with that name.

Compiler-emitted desugars (`try`, `await`, operator desugars) always
use the qualified form internally so user-imported traits with
overlapping method names cannot break them.

### A.commit 4 - `try` keyword + Result/Option `tryable` impls

#### Parse

`try` is a unary expression-position prefix with low precedence:

- `try await foo()` parses as `try (await foo())`.
- `let x = try foo() + try bar()` parses as
  `let x = (try foo()) + (try bar())`.
- `try foo.map_err(g)` parses as `try (foo.map_err(g))`.

#### Desugar

Uses fully-qualified method calls:

```
{
  let __t = expr
  if __t.tryable::is_err():
    return __t.tryable::err_value()
  __t.tryable::ok_value()
}
```

#### Evaluation order

Multiple `try`s in a single expression evaluate left-to-right.

#### Validity

`try` is valid only inside a function whose declared return type
unifies with the `C` of `expr`'s `tryable` impl. Outside that,
compile error pointing at the enclosing fn signature.

#### `return try expr` warning

Emits a warning: "`try` yields the success value V; did you mean
`return Ok(try expr)`?" Underlying type error still fires.

#### No automatic error conversion

Cross-error-type `try` requires explicit `.map_err(...)`. Within the
same container family, existing trait-pointer subtyping handles
`ConcreteErr → errors::Err` widening automatically.

#### Result and Option impls (added in this commit, in stdlib)

```
// stdlib/result/result.tin
data Result[T, E](tryable[T, Result[_, E]]) =
  Ok(v T)
  Err(msg E)

  fn tryable[T, Result[_, E]]::is_err(this Result[T, E]) bool =
    match this:
      case Ok(_):  return false
      case Err(_): return true

  fn tryable[T, Result[_, E]]::ok_value(this Result[T, E]) T =
    match this:
      case Ok(v):  return v
      case Err(_): panic("tryable::ok_value on Err")

  fn tryable[T, Result[_, E]]::err_value(this Result[T, E]) Result[T, E] = this

// stdlib/option/option.tin
data Option[T](tryable[T, Option[_]]) =
  Some(v T)
  None

  fn tryable[T, Option[_]]::is_err(this Option[T]) bool = ...
  fn tryable[T, Option[_]]::ok_value(this Option[T]) T = ...
  fn tryable[T, Option[_]]::err_value(this Option[T]) Option[T] = this
```

#### Test cases

- `try result_value` inside a Result-returning fn: success path
  yields, failure returns Err.
- `try option_value` inside an Option-returning fn: Some yields,
  None returns None.
- `try expr` outside a returning fn: compile error.
- `try expr` where `C` does not match enclosing return: compile error.
- `try await spawn fn(...)`: composition with `await`.
- `let x = try a() + try b()`: left-to-right; `b()` does not run if
  `a()` propagates.
- `return try expr`: emits warning.
- User-defined struct impl of `tryable`: `try parser_value` works.
- Struct-impl widening + dynamic-dispatch `try`.

---

## Workstream B: stdlib rework

### Phase 0 - small foundations

#### 0.1 Cleanup (mechanical)

- [ ] Delete `stdlib/encoding/base16.tin` and
      `stdlib/encoding/base64.tin` (verified-identical duplicates of
      the subdir versions).
- [ ] Resolve `encoding/url.tin` vs `url/url.tin`: keep the
      more-complete top-level, move tests up, delete the subdir.
- [ ] Rename `option::or_default` → `option::unwrap_or` (parallel with
      `result::unwrap_or`).
- [ ] Rename `errors::has` → `errors::is_err` (parallel with
      Result/Option).
- [ ] `LinkedList.pop_back` returns `Option[T]` instead of nil-deref
      on empty list.
- [ ] `flag::parse` returns `Result[[string], FlagError]` instead of
      `exit_proc(2)`. Defines `FlagErrorKind` ADT.

#### 0.2 `stdlib/crypto/subtle::ct_eq`

```tin
fn ct_eq(a [byte], b [byte]) bool   // #pure #no_thread
```

Constant-time bytes equality. ~20 LoC, no deps. Required for
HMAC/JWT signature verify.

#### 0.3 `stdlib/hash` rework - Hasher + SHA-2 + HMAC

```tin
trait Hasher =
  fn write(this *Hasher, data [byte]) = virtual
  fn sum(this *Hasher) [byte]         = virtual
  fn reset(this *Hasher)              = virtual
  fn size(this *Hasher) i64           = virtual

struct Sha256 (Hasher) = ...
struct Sha384 (Hasher) = ...
struct Sha512 (Hasher) = ...

fn sha256(data [byte]) [byte; 32]
fn sha384(data [byte]) [byte; 48]
fn sha512(data [byte]) [byte; 64]

fn hmac[H](key [byte], msg [byte]) [byte] where H is Hasher
```

Implementation: OpenSSL via the same C-binding pattern `tls` uses.

#### 0.4 `time::sleep` + `Duration` + monotonic clock

```tin
struct Duration = _ns i64
fn millis(n i64) Duration
fn seconds(n i64) Duration
fn minutes(n i64) Duration
fn hours(n i64) Duration

fn sleep(d Duration) Future[Unit]
fn now_mono() Instant
fn since(start Instant) Duration
```

#### 0.5 `stdlib/encoding/base64` URL-safe-no-pad variants

```tin
fn encode_url_no_pad(data [byte]) string         // RFC 4648 §5
fn decode_url_no_pad(s string) Result[[byte], errors::Err]
```

JWT requires this exact form.

### Phase 1 - error infrastructure

#### 1.1 The trait + ad-hoc string error

```tin
// stdlib/errors

trait Err =
  fn message(this *Err) string = virtual
  fn kind(this *Err) atom      = virtual

struct StringErr (Err) =
  _msg string

  fn Err::message(this *StringErr) string = return this._msg
  fn Err::kind(this *StringErr) atom      = return 'msg

fn new(msg string) errors::Err = return StringErr{_msg: msg}

fn wrap(prefix string, inner errors::Err) errors::Err =
  return StringErr{_msg: prefix ++ ": " ++ inner.message()}
```

Trait passed by value (Tin's trait fat-ptr representation is the
right shape across function boundaries).

#### 1.2 The per-domain pattern (transitional, refactored in Workstream C)

```tin
// stdlib/os example

data OsErrorKind =
  NotFound(path string)
  PermissionDenied(path string)
  AlreadyExists(path string)
  IsDirectory(path string)
  NotDirectory(path string)
  InvalidPath(path string, reason string)
  Io(path string, errno i32)
  Other(syscall string, errno i32)

struct OsError (errors::Err) =
  _kind OsErrorKind

  fn errors::Err::message(this *OsError) string = return error_message(this._kind)
  fn errors::Err::kind(this *OsError) atom      = return 'os

fn error_message(k OsErrorKind) string =
  match k:
    case NotFound(p):         return "file not found: {p}"
    ...

fn err(k OsErrorKind) errors::Err = return OsError{_kind: k}

fn as_os(e errors::Err) Option[*OsError] =
  if e.kind() != 'os: return None
  return Some(e as *OsError)
```

This is the **transitional** form. Workstream C collapses
`OsErrorKind` + `OsError` into a single `data OsError(errors::Err)`
ADT with the methods on the ADT directly.

#### 1.3 Caller ergonomics

```tin
fn read_file(path string) Result[string, errors::Err] = ...

match read_file("foo"):
  case Ok(s): echo s
  case Err(e):
    echo "load failed: {e.message()}"

    match os::as_os(e):
      case Some(oe):
        match oe._kind:
          case NotFound(_): create_default()
          case _:           panic(e.message())
      case None:
        panic(e.message())
```

After A.commit 4, the explicit pattern match becomes:

```
fn outer() Result[Unit, errors::Err] =
  let s = try read_file("foo")
  echo s
  return Ok(())
```

#### 1.4 Domains in this phase

Just the trait, `StringErr`, and the helpers in `stdlib/errors`
itself. Per-domain `XxxError` structs land alongside their function
migrations in Phase 2.

### Phase 2 - `(T, Err)` → `Result[T, errors::Err]` migration

For each row, the migration steps are:

1. Define `data XxxErrorKind` + `struct XxxError(errors::Err)` +
   `error_message` + `err()` constructor in the module.
2. Rewrite the function's return signature.
3. Rewrite the body to construct `XxxError` and return `Err(...)` /
   `Ok(...)`.
4. Update callers.

| File | Function | Old | Domain ADT | Status |
|---|---|---|---|---|
| `os/os_files.tin` | `read_file`, `read_file_bytes`, `write_file*`, `append_file` | `(string, Err)` / `Err` | `OsError` | ✓ done |
| `os/os_dirs.tin` | `mkdir`, `remove`, `rmdir`, `rename` | `Err` | `OsError` | ✓ done |
| `os/os_stat.tin` | `stat` | `(FileInfo, Err)` | `OsError` | ✓ done |
| `net/tcp/tcp.tin` | `dial`, `listen` | `(Conn, Err)` / `(Server, Err)` | `NetError` | pending |
| `net/udp/udp.tin` | `dial`, `listen` | same | `NetError` | pending |
| `net/unix/unix.tin` | `dial`, `listen`, `accept` | same | `NetError` | pending |
| `strings/strings.tin` | `parse_int`, `parse_int32`, `parse_float` | `(T, Err)` | `StringsError` | ✓ done |
| `strings/strings.tin` | `index_of`, `last_index_of` | `-1` sentinel | `Option[i64]` | pending |
| `decimal/decimal.tin` | `parse` | `(Value, Err)` | `DecimalError` | ✓ done |
| `time/time.tin` | `parse_rfc3339` | `(Instant, Err)` | `TimeError` | ✓ done |
| `regex/regex.tin` | `compile` | struct with `is_err()` flag | `RegexError` | pending |
| `encoding/csv/csv.tin` | `parse` | `(row, ok)` | `CsvError` | pending |
| `encoding/json/json.tin` | (refactor) | already has `JsonError` | refactor to `(errors::Err)` | ✓ done |
| `encoding/yaml/yaml.tin` | (refactor) | already has `YamlError` | refactor to `(errors::Err)` | ✓ done |
| `flag/flag.tin` | `parse` | exits process | `FlagError` (already in 0.1) | pending |

### Phase 3 - `os::File` + os primitives

#### 3.1 `os::File`

```tin
struct File (io::AsyncReader, io::AsyncWriter, io::Closeable) =
  _cell *rc::Cell[*void]
  _path string

  fn AsyncReader::read(this File, buf *byte, n i64) Future[i64]
  fn AsyncWriter::write(this File, buf *byte, n i64) Future[i64]
  fn Closeable::close(this File)

  fn seek(this File, offset i64, whence i32) Result[i64, errors::Err]
  fn flush(this File) Result[Unit, errors::Err]
  fn fd(this File) i32

enum atom OpenMode =
  'read 'write 'append 'read_write 'create 'truncate

fn open(path string, mode OpenMode) Result[File, errors::Err]
fn create(path string) Result[File, errors::Err]
```

#### 3.2 Process management

```tin
struct Process (io::Closeable) =
  _cell *rc::Cell[*void]
  _pid i64
  stdin  Option[File]
  stdout Option[File]
  stderr Option[File]

  fn pid(this Process) i64
  fn wait(this *Process) Result[i32, errors::Err]
  fn kill(this *Process) Result[Unit, errors::Err]
  fn Closeable::close(this Process) = await this.wait()

fn spawn(argv [string]) Result[Process, errors::Err]
fn spawn_with(argv [string], opts SpawnOptions) Result[Process, errors::Err]
fn exec(argv [string])

struct SpawnOptions =
  env       Option[[string]]
  cwd       Option[string]
  stdin     StdioMode
  stdout    StdioMode
  stderr    StdioMode

enum atom StdioMode = 'inherit 'pipe 'null
```

#### 3.3 Pipes & temp files

```tin
fn pipe() Result[(File, File), errors::Err]

fn tempfile() Result[File, errors::Err]
fn tempdir(prefix string) Result[string, errors::Err]
```

#### 3.4 Filesystem extras

```tin
fn chmod(path string, mode i32) Result[Unit, errors::Err]
fn symlink(target string, linkpath string) Result[Unit, errors::Err]
fn readlink(path string) Result[string, errors::Err]
```

### Phase 4 - `io::copy` + BufWriter

```tin
fn copy(dst io::AsyncWriter, src io::AsyncReader)
    Future[Result[i64, errors::Err]]

struct BufWriter (io::AsyncWriter, io::Closeable) =
  _inner *rc::Cell[*void]
  _buf [byte]
  _pos i64

  fn AsyncWriter::write(this *BufWriter, buf *byte, n i64) Future[i64]
  fn flush(this *BufWriter) Future[Result[Unit, errors::Err]]
  fn Closeable::close(this BufWriter)
```

### Phase 5 - DNS resolution

```tin
fn lookup_host(name string) Result[[string], errors::Err]
fn lookup_addr(ip string)   Result[string, errors::Err]
```

Implementation: getaddrinfo / getnameinfo via libc, wrapped in
`stdlib/net/dns/dns.c`. C side uses the udp-style out-parameter
pattern so RC-allocated strings flow back without leaking a
pre-allocated empty-string placeholder. Lives at
`stdlib/net/dns/`, exported through `net::dns`.

`parse_host_port`/`resolve` (host:port spec parsing) is deliberately
NOT part of `net::dns` -- it does no DNS work, and the masterplan's
earlier draft signature `resolve(spec, default_port)` confused
string-splitting with name resolution.  If we want it later it
belongs in `net` directly, not under `dns`.

### Phase 6 - JWT

Drops in once Phase 0 (hash + ct_eq + base64url + time::sleep) and
Phase 1 (errors::Err) exist.

```tin
enum atom Algorithm =
  'HS256 'HS384 'HS512 'RS256 'RS384 'RS512 'ES256 'ES384 'EdDSA

data JwtErrorKind =
  Malformed(reason string)
  InvalidBase64(segment string)
  InvalidJson(segment string, cause errors::Err)
  UnsupportedAlgorithm(alg string)
  AlgorithmMismatch(want Algorithm, got Algorithm)
  AlgNoneRejected
  InvalidSignature
  Expired(at time::Instant)
  NotYetValid(nbf time::Instant)
  IssuerMismatch(want string, got string)
  AudienceMismatch(want string, got string)
  MissingClaim(name string)
  InvalidKey(reason string)

struct JwtError (errors::Err) = ...

struct Header = ...
struct Claims = ...
struct Token  = ...

struct HmacKey =
  _bytes [byte]
  static fn from_bytes(b [byte]) HmacKey
  static fn from_string(s string) HmacKey

struct RsaPublicKey  (io::Closeable) = _cell *rc::Cell[*void]
struct RsaPrivateKey (io::Closeable) = _cell *rc::Cell[*void]
// ec / ed25519 keys analogous

fn parse_rsa_public_pem(pem string)  Result[RsaPublicKey, errors::Err]
fn parse_rsa_private_pem(pem string) Result[RsaPrivateKey, errors::Err]
// other PEM parsers analogous

struct VerifyOptions = ...

fn decode(token string) Result[Token, errors::Err]

fn sign(claims Claims, alg Algorithm, key HmacKey)        Result[string, errors::Err]
fn sign(claims Claims, alg Algorithm, key RsaPrivateKey)  Result[string, errors::Err]
fn sign(claims Claims, alg Algorithm, key EcPrivateKey)   Result[string, errors::Err]
fn sign(claims Claims, key Ed25519PrivateKey)             Result[string, errors::Err]

fn verify(token string, key HmacKey, opts VerifyOptions)        Result[Claims, errors::Err]
fn verify(token string, key RsaPublicKey, opts VerifyOptions)   Result[Claims, errors::Err]
fn verify(token string, key EcPublicKey, opts VerifyOptions)    Result[Claims, errors::Err]
fn verify(token string, key Ed25519PublicKey, opts VerifyOptions) Result[Claims, errors::Err]
```

Locked-in security defaults:

- `alg: none` rejected at parse - `JwtErrorKind::AlgNoneRejected`.
- Type-enforced alg/key match.
- Signature compared via `crypto::subtle::ct_eq`.
- `exp`/`nbf` checked by default with optional `leeway`.
- `aud` normalized to `[string]`.

---

## Workstream C: refactors after Workstream A lands

These commits depend on A.commit 2 (ADT trait impls + widening) and
A.commit 4 (`try` keyword).

### C.commit 1 - Collapse error wrappers

For every domain that has the `XxxErrorKind` ADT + `XxxError` wrapper
struct pattern, collapse into a single `data XxxError(errors::Err)`
ADT with the methods on the ADT directly.

Changes per module:

- Drop `struct XxxError(errors::Err)` and its methods.
- Rename `data XxxErrorKind` → `data XxxError`. Add the
  `(errors::Err)` header and the `errors::Err::message` /
  `errors::Err::kind` methods directly on the ADT.
- Update `as_xxx` downcast helper: `e as *XxxError` becomes
  `e as XxxError` (downcast to ADT, no pointer indirection).
- Update producers: `err(k)` becomes
  `XxxError::Variant(...)` directly; the ADT widens to
  `errors::Err` at the return site.

Per-domain audit list mirrors Phase 2's table; every domain that
landed in B touches here.

Tests: each module's existing error-path tests pass with the
collapsed shape. Behavior-preserving.

### C.commit 2 - Adopt `try` keyword in stdlib

Replace explicit `match` on `Result` / `Option` with `try` wherever
it fits:

- Sweep stdlib for patterns like
  `match foo(): case Ok(v): ... case Err(e): return Err(e)` and
  rewrite to `let v = try foo()`.
- Same for Option: `match foo(): case Some(v): ... case None: return None`
  → `let v = try foo()`.
- Don't touch sites where the error path does anything other than
  re-return - those are the cases the keyword's narrow desugar
  doesn't fit.

Tests: existing tests pass without modification.

### C.commit 3 (optional) - Re-evaluate trait-by-pointer patterns

Phase 1's trait says `fn message(this *Err) string`. With ADTs as
trait pointers, the `*Err` form may be unnecessary or may want
revisiting. Audit all `*Err`/`*OsError`/etc. sites in stdlib; consider
changing to value-type usage where it doesn't change semantics. Not
required for correctness, just cleanup.

---

## Execution order across workstreams

1. **Workstream B Phase 0** (cleanup + crypto + hash + time +
   base64url) - ~1 day. Independent foundation pieces.
2. **Workstream B Phase 1** (errors::Err trait + StringErr + wrap) -
   ~half day. Verify already exists.
3. **Workstream B Phase 2 finish** (remaining domains: net, regex,
   csv, flag, strings::index_of) - ~1 day. With current wrapper
   pattern.
4. **Workstream A.commit 1** (partial generic bounds) - multi-day.
   Foundational; nothing else in A unblocks without it.
5. **Workstream A.commit 2** (ADT trait impls + unified static-table
   vtables) - multi-day. Biggest piece. Migration audit critical.
6. **Workstream A.commit 3** (runtime/builtin/ + awaitable rename +
   trait declarations + qualification + operator-resolution) -
   multi-day. Decimal tests as the regression gate.
7. **Workstream A.commit 4** (`try` keyword + Result/Option tryable
   impls) - ~1 day.
8. **Workstream B Phases 3–6** (os::File, io::copy, DNS, JWT) - ~5
   days. Land in any order within respect to dependencies (JWT
   depends on Phase 0). May overlap with C.
9. **Workstream C.commit 1** (collapse error wrappers) - ~1 day.
   Mechanical sweep across all phase-2 domains.
10. **Workstream C.commit 2** (adopt `try` in stdlib) - ~half day.
    Mechanical sweep.
11. **Workstream C.commit 3** (trait-by-pointer audit, optional) -
    half day if pursued.

Total: ~3 weeks of focused work, parallelizable in places.

Ordering rationale: Phase 0 + 1 + 2-finish first because they're
unblocked and Phase 2 is partially in flight. Then Workstream A as a
focused block (commits 1–4 run sequentially). Then everything that
depends on A: Workstream B's later phases use the new patterns where
applicable, and Workstream C cleans up the transitional patterns
that landed in Phase 2.

---

## Items to confirm before starting

Carried over from both predecessor plans, deduplicated.

### Workstream A

- **`err_value` return type semantics.** Plan has `err_value` return
  `C` (the container) so the desugar can `return expr.err_value()`
  directly. Alternative: return `E`. The C interpretation preserves
  the container-agnostic keyword desugar; the E interpretation would
  require keyword-side container construction. Confirmed: C.
- **ADT method declarations in current Tin.** Plan assumes
  `fn ::method` declarations inside a `data` body work. Today only
  `struct` accepts this. Audit the parser; if it does not accept ADT
  methods, A.commit 2 grows by that scope.
- **Operator-resolution migration scope.** Decimal tests must pass
  bit-identically post-migration. Scope-check before A.commit 3 -
  if too large, split out as a prerequisite commit.
- **Error message wording.** The partial-bound widening error
  (applies to both ADTs and structs, fires when `_` is left
  existential at a widening site), the `try` return-type-mismatch
  error, and the `return try` warning - finalize phrasing before
  implementation so tests can assert on it.
- **Struct-layout migration audit.** Before A.commit 2 lands, sweep:
  (a) FFI / C-interop sites for hardcoded struct field offsets;
  (b) ARC traversal code (`runtime/arc.c`) for assumptions about
  vtable-pointer slots between `type_id` and user fields;
  (c) any reflection or codegen-internal code that reads a vtable
  pointer from a struct value's header rather than via a static
  table; (d) tests that assert on `sizeof(SomeStruct)` or specific
  byte offsets.
- **Widening-perf check.** Existing struct widening goes from one
  load to two. Profile post-migration; not a blocker for landing.
- **`iter` trait shape and the `for x in expr` keyword.** Declared
  as a stub in A.commit 3; the keyword and full semantics are
  deferred.

### Workstream B

- Existing Phase 1 status: verify `stdlib/errors` matches the
  locked-in pattern in section "Phase 1" before continuing Phase 2.
- Phase 2 remaining-domain count and scope.
- Phase 3's `os::File` ARC story: `*rc::Cell[*void]` pattern matches
  `net::Conn` - confirm before implementing.
- Phase 6 PEM-parser scope: OpenSSL binding pattern follows `tls`
  module - confirm linkage stays compatible.
