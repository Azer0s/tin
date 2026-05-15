# Tin compiler TODO

Open design items captured during recent code reviews and stress
sessions.  Each entry includes the motivation, the proposed shape,
the implementation outline, and any open questions / risks so a
future contributor (or future-self) can pick it up cold.

## `*void as *[T; N]` typed-array cast for `mem::malloc`

### Motivation

`mem::malloc` returns `*void`.  Currently a user who wants to view
that allocation as an array of T cannot do so cleanly:

- `*void as [T]` is rejected because `[T]` is a fat-pointer
  (`{*T, i64}`) and a raw `*void` is missing the length field; even
  if we accepted the cast it would produce an ARC-managed slice that
  the rest of the system would try to release.
- `*void as [T; N]` (fixed-size value array) is rejected too; even
  if accepted, the cast value semantics would stack-copy the heap
  contents into the alloca, disconnecting from the original
  allocation that still needs `mem::free`.

`stdlib/mem/mem.tin`'s `alloc[t](n) [*t]` arity-2 form was removed
in this session because it cannot be implemented without one of the
above casts.  Users that need an array of bytes today drop down to
`mem::malloc(bytes)` and walk the buffer with pointer arithmetic
inside an `{#unsafe}` block.

### Proposed shape

Allow exactly one cast direction: `*void as *[T; N]`.  Pointer
identity is preserved; no value copy; ARC stays out of the way
because there's no fat-pointer struct.

```tin
let p   = mem::malloc(sizeof([i64; 16]))
let arr = p as *[i64; 16]   // pointer to fixed-size array
(*arr)[0] = 42
echo (*arr)[0]
mem::free(p)
```

The cast is purely a pointer reinterpret at the LLVM layer
(`bitcast i8* to [16 x i64]*`).

### Static check (best-effort)

Where the malloc size argument is a constant expression, the
checker MUST verify it covers the destination type's bytecount.

Categories:

**Always-correct (silent)** — `mem::malloc(sizeof([T; N]))` or
`mem::malloc(K)` where the literal K equals `sizeof(T) * N`.
Accept without warning.

**Statically-wrong (hard error)** — same shape, but K is less than
`sizeof(T) * N`.

```
error: cast *void to *[i64; 16] requires sizeof(i64) * 16 = 128
       bytes, but the allocation gave 64 bytes
  --> file.tin:L:C
  |
L |   let arr = p as *[i64; 16]
  |             ^
```

**Indeterminate (silent default, pedantic warning)** — K is a
runtime value (parameter, computation involving non-constants).
Under `-Wpedantic` emit a `unchecked-alloc-cast` warning suggesting
the user write the size via `sizeof([T; N])` so the verifier can
prove the match.

### Implementation outline

1. **Type checker** — extend the `as` resolver to accept
   `*void -> *[T; N]` (and equivalently `*T -> *[T; N]` where T
   matches the element type).  Reject all other shapes (no
   `*void as [T; N]`, no `*void as [T]`, no `*void as *<incomplete>`).

2. **Element-type restriction** — at the cast site, refuse T that
   contains ARC-tracked types (`string`, `[T]`, `any`, `*Struct`
   pointing into ARC space, fat-fn-ptr).  Fixed-size arrays of
   primitives and `#no_arc`-tagged structs are accepted.  Without
   this gate, code could obtain a `*[*Struct; N]` cast and walk
   into ARC release at scope-exit on uninitialised memory.

3. **Size dataflow trace** — at the cast site, walk the receiver:
   - `*ast.Identifier` → look up the binding's init expression in
     a per-fn map populated during VarDecl walks (similar shape to
     `cg.manualAllocSites`).
   - If init is `mem::malloc(<const>)` / `mem::calloc(<const>, <const>)`
     / `mem::realloc(_, <const>)` / `mem::alloc[U]()` — record the
     allocation byte count.
   - Compare against `sizeof([T; N])`.  Emit error on mismatch.

   For indirect bindings (init came from a fn call returning *void,
   or the size argument is a non-constant), fall through to the
   pedantic warning path.

4. **`sizeof` of fixed-size arrays** — verify codegen lowers
   `sizeof([T; N])` to a compile-time constant (it should; the
   array type is monomorphic).

5. **Stdlib re-add** — once (1)-(4) are in, restore `alloc[t](n)`
   in `stdlib/mem/mem.tin` as:

   ```tin
   fn alloc[t](n i64) *[t; <const_n>] = ...
   ```

   ... but only with a literal `n`.  A runtime `n` cannot pin
   `*[t; ?]` because the type is non-uniform.  This means
   `mem::alloc[t](n)` as written today (n a parameter) still
   cannot be implemented; we'd be giving users a fixed-`N` form
   like `mem::alloc[t, N]()` instead.

### Open questions

- **Naming for the warning**: `-Wunchecked-alloc-cast`?
  `-Walloc-size-mismatch`?  Pick one when implementing.

- **Larger-than-needed allocs**: `mem::malloc(256)` cast to
  `*[i64; 16]` (128 bytes).  Strictly fine (no out-of-bounds);
  silent or pedantic-warn?  Recommendation: silent, since
  over-allocating is a legitimate pattern (size to next page
  boundary, etc.).

- **`mem::realloc` interactions**: `let p = mem::malloc(64);
  p = mem::realloc(p, 128); let arr = p as *[i64; 16]` — the
  size is the realloc's new_size, not malloc's.  The dataflow
  trace has to follow the latest assignment.  Already handled by
  the `manualAlloc` lattice's reassign-clears-state logic; just
  needs the size map to reset on AssignStmt as well.

- **Cast through `as *T`**: should `let q = p as *i64; let arr = q
  as *[i64; 16]` work?  Probably yes — the underlying allocation
  doesn't change.  The trace would need to follow non-`*void`
  pointer aliases as well.  Defer until the simple case is in.

### Related cleanup

- Once landed, remove the TODO comment block in
  `stdlib/mem/mem.tin` that explains the arity-2 form's absence.

- Add a section to `docs/08-interop.md` showing the canonical
  fixed-size-array alloc pattern.

- The existing manual-alloc dataflow (`-Wmanual-alloc-leak`,
  `-Wmanual-double-free`, `-Wmanual-use-after-free`) already
  tracks bindings through reassign; verify it continues to work
  for the cast result (`arr` aliases `p`; freeing `p` should
  conceptually free `arr` too -- today the check tracks them
  independently and would miss the aliasing).  Decide whether to
  thread alias info or accept the limitation.
