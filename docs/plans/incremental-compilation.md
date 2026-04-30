# Incremental compilation plan

Status: planning. One coherent design; the steps below are the order
work lands in, not separately-shipped versions.

## Goal

Cache compilation work at the package level so the typical "edit one
file, rebuild" loop reuses everything else. Whole-program rebuild
remains the fallback (first run, toolchain change, `tin clean`).

Hard requirements - all of these must work in the same shipped design,
not deferred:

- Top-level globals (`var x = expr` at module scope) keep working,
  including init AND deinit order. Init runs deps-before-dependents
  topologically; deinit runs in the exact reverse so a var that
  depended on another at construction is torn down before the thing
  it depended on.
- Generic monomorphization is correct and de-duplicated across packages
  - one `parse[Point]` definition in the linked binary, regardless of
  how many packages instantiate it.
- Runtime globals (atom table, fiber scheduler, panic state) keep
  working; they live in `runtime/runtime.c` and stay content-cached
  via `.build/csrc/<hash>/`.
- `stacktrace()` keeps resolving to `symbol@file:line:col`.
- Debug symbols (`-g`, `-gline-tables-only`) keep working.
- `traitof(v)` returns the complete trait list for `v`'s type, even
  after a fresh `impl` is added in a downstream package - without
  invalidating every consumer that ever called `traitof`.
- A single edit to one stdlib package does not invalidate every other
  package that imported it; only consumers of the edit's *interface*
  recompile.

## Where the compiler is today

- One LLVM IR module (`cg.mod`) for the entire program. There's a
  sibling `cg.shimMod` for CTFE wrappers (Phase C7 of the pure-fn
  cache work) - so per-module routing exists in shape, but the
  user-facing pipeline still emits one `.ll` and one TU.
- Existing caches: `.build/run/<file_md5>/` and
  `.build/test/<file_md5>/` for final binaries (sbom-validated);
  `.build/csrc/<hash>/` for content-addressed C-source compile;
  `.build/pure-fn/<hash>/bin.so` for CTFE per-fn shared libraries.
- `runParallelClang` is already wired - it's used for the batched
  C-source compile - so parallel per-package compile is reusing
  existing infrastructure, not net-new.

## Decisions

These are settled. No "v0 with X, later with Y" - one design, one
implementation.

### D1. Trait vtables and `traitof()`: link-time reflection table

`traitof(v)` returns every trait `v`'s runtime type implements. That's
whole-program by definition: pkg D adding `impl ExistingTrait for
ExistingStruct` must be visible to every existing `traitof` caller.
With per-package `.o` files, this only works if the trait-implements
table is materialized at **link time**, not at codegen time.

Each package emits its `impl` declarations as discrete symbols in a
custom section. At program start, a runtime helper walks every entry in
that section and builds the canonical `(type_id -> [trait_id])` table
consumed by `traitof`. Adding an `impl` in a new pkg becomes a re-link,
not a recompile of every consumer.

The section name and the walk mechanism differ between ELF (Linux) and
Mach-O (macOS); see the **Cross-platform considerations** section below
for the concrete syntax and the runtime abstraction
(`_tin_iter_impl_section`) that hides the difference from codegen.

Vtable globals follow the same pattern: each `impl` produces a vtable
global in the *implementing* package's `.o`, named
`_tin_vtable_<S>__<T>`, weak-linked so duplicate impls fail at link
time rather than silently picking one.

### D2. Monomorphization: content-addressed shared cache

`parse[Point]` template lives in pkg `json`; instantiated in any number
of consumer packages. The instantiation goes into a separate
content-addressed `.o`, not into either pkg's `.o`:

```
mono_hash = sha256(template_iface_hash || sorted_type_args_hash)
.build/mono/<mono_hash>/bin.o
```

First consumer to encounter `parse[Point]` populates the cache; all
subsequent consumers (in this or any later build) link the same `.o`.
No `linkonce_odr` duplication, no per-consumer disk bloat, and the
mono cache survives across pkg recompiles since its key depends only
on the template's *interface* and the type arguments - not on where
it was first triggered.

This makes D3 mandatory: the mono key must produce byte-identical IR
for the same `(template, types)` pair across runs.

### D3. Deterministic IR emission

Required for D2 (the mono cache is content-addressed, so re-emission
that produces byte-different IR misses the cache forever). Also a
correctness benefit on its own (reproducible builds, stable diffs).

Audit and fix in this order:

1. Replace map iteration in IR emission paths with sorted-key
   iteration. Affects emit paths that walk `cg.funcDecls`,
   `cg.structTypes`, `cg.macros`, `cg.overloads`, anywhere else.
2. Atom code allocation: `_tin_learn_atom` resolves CRC32 collisions
   by `code++`. The collision resolution order must depend on the
   *atom table state*, not on the order interns happened. Either
   pre-collect every atom string, sort, then assign codes; or make
   the resolution itself sorted-deterministic.
3. Anon/temp symbol numbering: keys must be a stable property of the
   source (e.g. source position) rather than a counter that varies
   with traversal order.
4. Trait-impl method emission order: walk impls in sorted name order.
5. Test harness: a CI step that compiles the same source twice and
   asserts byte-identical IR for every pkg module (and the mono
   `.o`s).

This work lands as part of the same incremental-compile patch - it's
prerequisite, not optional.

### D4. Init and deinit order: topological per-pkg, reverse-topological deinit

Each package with non-trivial top-level initializers gets a
`_tin_init_<pkg>(void)` function in its own module. Each package
with top-level vars whose types or fields require `deinit` gets a
matching `_tin_deinit_<pkg>(void)` that tears those down in
**reverse declaration order within the pkg**.

The entry pkg's `_tin_user_main` wrapper calls `_tin_init_<pkg>`
for every imported pkg in **topological order** (deps before
dependents) before running user code. On normal exit (return from
user `main`, fall-through past last statement, explicit
`std::os::exit`) it then calls `_tin_deinit_<pkg>` for every
imported pkg in the **exact reverse** of the init order: dependents
first, deps last.

Implementation hooks:

- `_tin_user_main` registers a single `atexit(_tin_deinit_all)`
  before user code runs. `_tin_deinit_all` walks the topo order
  in reverse and calls each pkg's deinit. This catches both
  fall-through-from-main and `exit(N)` paths (libc atexit handlers
  run on `exit`).
- `panic` paths that abort the process (no clean unwind) skip
  deinit, matching today's behavior - this is documented, not
  fixed here.
- The order is computed once from the import graph collected during
  package loading; cycles remain forbidden. The forward order is
  recorded as a static array in the entry pkg's module so reverse
  iteration is `for i := len-1; i >= 0; i--` over that same array.

Runtime globals (atom table, scheduler, panic state) stay in
`runtime/runtime.c`, are still content-cached via `.build/csrc/`,
and their init/deinit are governed by the runtime itself - they
sit OUTSIDE the per-pkg scheme: runtime init runs before any
`_tin_init_<pkg>`, runtime cleanup runs after any
`_tin_deinit_<pkg>`. Same convention as today.

For incremental specifically: the init/deinit dispatcher is
regenerated whenever the entry pkg's import graph changes. It is
NOT cached at the per-pkg level - it's a small whole-program
artifact that lives in the link step.

### D5. Compilation unit: one Tin package = one `.o`

A package is `pkg/pkg.tin` plus any `use "./..."` file-mode imports
it pulls in (e.g. `stdlib/encoding/encoding.tin` plus its
`./base16/base16.tin` etc.). Sub-package files merge into the parent
package's `.o`. The package boundary already exists in
`loadPackageFromSource`; we extend it to drive code emission.

Monomorphization `.o`s and the link-time reflection table are
*not* per-package - they live in their own caches.

### D6. Interface vs implementation split

A package's `iface.json` records:

- Every exported type (struct/data/enum) layout.
- Every exported function signature.
- Full bodies of exported macros (they expand at the call site).
- Full bodies of exported generics (they monomorphize at the call site).
- Full bodies of exported `#pure` functions (they fold into callers
  via CTFE).
- Every `impl Trait for Struct` declared in this package
  (whether or not the trait or struct is local).

Anything else is implementation. A change confined to a non-exported
function body, a private struct, a private macro, etc., bumps
`input_hash` but not `iface_hash`, so downstream packages stay cached.

The advisor pointed out that for stdlib-heavy code most of what's in
a `.tin` file is in the iface (because so much is generic / macro /
`#pure`). True. We still get a meaningful win from non-exported
internals, and the `iface.json` is necessary for correct invalidation
regardless of size of win - it's not an optimization that can be
skipped.

## Cache layout

```
.build/
  pkg/<pkg_path_hash>/<input_hash>/
    pkg.o            # this package's compiled object
    iface.json       # exported items (D6)
    iface_hash       # SHA-256 of iface.json
    sbom.txt         # source files + their MD5s
                     # + import iface_hashes seen at compile time
  mono/<mono_hash>/
    bin.o            # one monomorphization (D2)
                     # mono_hash = hash(template_iface_hash || type_args)
  csrc/<hash>/...    # already exists (runtime + stdlib C sources)
  pure-fn/<hash>/... # already exists (CTFE shims, host-arch always)
  link/<entry_hash>/
    bin              # final linked binary (replaces .build/run, .build/test)
    sbom.txt         # records every contributing pkg + mono cache key
```

### Cache key composition

Every cache hash above (`<input_hash>`, `<mono_hash>`, `<entry_hash>`)
mixes in everything that can change emitted IR or object format. Without
this an arm64 entry silently gets reused on an amd64 build, or a `-O0`
entry on a `-O2` build. The fixed composition for `<input_hash>` and
`<mono_hash>` is:

```
hash = sha256(
    source_content      ||  // .tin sources for pkg, or template+typeargs for mono
    target_triple       ||  // e.g. "aarch64-apple-darwin"
    data_layout         ||  // LLVM data layout string for the triple
    opt_level           ||  // "O0" / "O2" / "O3"
    debug_flags         ||  // "" / "g" / "gline-tables-only"
    stacktrace_active   ||  // "0" / "1" - changes -funwind-tables propagation
    clang_version       ||  // `clang --version` first line
    tin_compiler_rev    ||  // git hash of the tin binary, fallback to mtime
)
```

The `<entry_hash>` for the final link adds the deduped, ordered list of
input pkg cache keys + mono cache keys + `csrc` keys + linker-flag set
+ `-rdynamic` / `-Wl,-export_dynamic` mode.

Cache hit for a package requires:

1. Every source file in the package hashes to its recorded value.
2. Every imported package's *current* `iface_hash` matches the value
   recorded in this pkg's `sbom.txt` at last compile.
3. The composition above produces the same `<input_hash>` it did before
   (i.e. nothing in the toolchain or build mode changed).

### Three caches, three arch invariants

The cache directories hold objects with three different host/target
relationships, and the design must keep them strictly separated:

| Cache              | Architecture | Why                                              |
|--------------------|--------------|--------------------------------------------------|
| `.build/csrc/`     | target arch  | Linked into the final binary                     |
| `.build/pkg/`      | target arch  | Linked into the final binary                     |
| `.build/mono/`     | target arch  | Linked into the final binary                     |
| `.build/pure-fn/`  | **host arch**| `dlopen`'d by the running `tin` process at CTFE  |
| `.build/link/`     | target arch  | The final binary                                 |

During a Linux/amd64 -> Darwin/arm64 cross-compile a single `tin build`
populates entries in both kinds of caches in the same run: the per-fn
CTFE shims are x86_64 ELF, everything else is arm64 Mach-O. Because the
key composition includes the triple, target-arch and host-arch entries
land at different keys and never collide - even if a package's content
is identical, the `pkg/` cache key differs from the `pure-fn/` cache key
because their inputs include different triples.

## Architecture

### Per-pkg LLVM modules

Codegen routes IR object creation through `cg.pkgMod(name)
*ir.Module`, which lazily creates a module per package with the same
target triple and data layout as the umbrella module. The current
`cg.mod` becomes the umbrella for genuinely whole-program things:
the link-time reflection table's section header, the entry pkg's
`_tin_user_main`, the runtime-init dispatch.

`cg.currentPkg` already tracks the active package during
`loadPackageFromSource`; extend to the entry package and keep set
during the codegen passes that emit user code.

Pkg modules emit:

- Pkg-prefixed functions (`pkg__fn`, `pkg__Struct_method`).
- Pkg-prefixed globals (top-level `var`s, vtable globals for the
  pkg's own `impl`s, the pkg's `_tin_init_<pkg>`).
- Type defs for any struct/data the pkg defines or instantiates
  (LLVM type identity is per-module - defining pkg "owns" the
  canonical layout; consumers get an echo type def).
- A `.tin_impl` section listing every `impl Trait for Struct`
  declared in the pkg, contributing to the link-time reflection
  table.

### Cross-package symbol resolution

Calls from pkg B to pkg A's exported function are external symbol
references - identical to today's mechanism, just with the symbol's
definition living in a separate `.o` instead of the same module.
LLVM `external` linkage and the linker do the rest.

`@llvm.used` pinning (which keeps CTFE wrappers and similar from
being stripped by `--gc-sections`) lives in the *defining* package's
module, not the consumer's. Audit during the per-pkg split.

### Monomorphization flow

1. Codegen sees `parse[Point](x)` in pkg B.
2. Compute `mono_hash = sha256(json::parse_iface_hash || Point_layout_hash)`.
3. If `.build/mono/<mono_hash>/bin.o` exists: record dependency in
   pkg B's sbom and continue.
4. Else: produce a self-contained IR slice (template + concrete
   type defs) using the existing slicer (the one that already
   powers the CTFE per-fn cache - see `ctfe_emit.go`); compile it;
   write `.build/mono/<mono_hash>/bin.o`.
5. Add `bin.o` to the linker's input list.

Pkg B's `.o` references `parse__Point` as an external symbol.
The mono cache `.o` defines it. Linker resolves.

The mono cache key is independent of pkg B - so pkg C asking for
the same `parse[Point]` finds the same cached `.o`.

### Link-time reflection table

Each pkg's `.tin_impl` section contains entries of the form
`(struct_type_id, trait_type_id, vtable_symbol_address)`. At link
time:

- Linker concatenates all `.tin_impl` sections into one contiguous
  region per the linker script.
- A small generated function `_tin_build_reflection_table()` runs
  before user code (called from the pkg-init dispatcher), iterates
  the section, and populates the runtime's
  `(type_id -> [trait_id])` table that `traitof` queries.
- Adding an `impl` in a new pkg means that pkg's `.o` adds an entry;
  re-link picks it up; no recompile of any other pkg.

Vtable globals (`_tin_vtable_<S>__<T>`) live in the impl's home pkg
with weak linkage - duplicate impls of the same `(S, T)` pair
across pkgs are link errors, not silent ambiguity.

### Init and deinit order

Each pkg with non-trivial top-level initializers emits
`void _tin_init_<pkg>(void)` into its own module. Each pkg with
top-level vars that require teardown emits a matching
`void _tin_deinit_<pkg>(void)` that tears them down in reverse of
the order they were initialized within the pkg.

The entry pkg's codegen emits two dispatchers:

```
// Topological order over imported pkgs, deps first.
static const init_fn pkg_inits[] = {
    _tin_init_<dep_1>,
    _tin_init_<dep_2>,
    ...
    _tin_init_<entry_pkg>,
};

void _tin_init_all(void) {
    for (size_t i = 0; i < N; i++) pkg_inits[i]();
    _tin_build_reflection_table();
}

void _tin_deinit_all(void) {
    // Reverse order: dependents tear down before their deps.
    for (ssize_t i = N - 1; i >= 0; i--) pkg_deinits[i]();
}
```

`_tin_user_main` (the wrapper around user `main`) calls
`_tin_init_all`, then `atexit(_tin_deinit_all)`, then invokes user
code. On normal exit (return from main, `exit(N)`) the libc atexit
machinery runs the deinits. On `panic`-driven abort, deinits are
skipped (matches today; documented, not fixed here).

Both dispatchers and the `pkg_inits` / `pkg_deinits` arrays live in
the entry pkg's module - they're whole-program artifacts re-emitted
at link time whenever the import graph changes, and are NOT cached
per-pkg. Topological order is computed from the package import
graph maintained during loading. Cycles remain forbidden.

### Stacktrace + debug symbols

Each per-pkg compile gets `-funwind-tables -gline-tables-only` when
`cg.StacktraceUsed()` is set, mirroring today's logic. The linker
(lld or system ld) stitches DWARF line tables across `.o` files
automatically; libdwfl reads the final binary's combined DWARF
unchanged. `-g` propagates the same way for full debug info.

Verified post-implementation by running the existing
`examples/stacktrace_*.tin` suite and a `-g` build under gdb.

### REPL

`repl/session.go` builds a `.so` per cell. Once package compilation
is incremental, cells reuse the same package cache instead of
rebuilding the runtime + stdlib for every cell. The hot path is
"compile this cell against the existing pkg cache, link, dlopen" -
which is essentially what's there today, plus reuse of pkg `.o`s.

## Cross-platform considerations

The design supports the matrix Tin already ships: Linux x86_64, Linux
arm64, macOS x86_64, macOS arm64 (Apple Silicon), and the cross-compile
combinations between them via `lld`. Below is every place that varies
by platform plus the agreed solution.

### The link-time reflection table section

D1 emits each `impl Trait for Struct` as one entry in a custom section
that the runtime walks at startup. The section name and walk mechanism
differ between ELF and Mach-O.

**ELF (Linux x86_64, Linux arm64).** The compiler emits each entry with:

```c
__attribute__((used, section(".tin_impl"), aligned(8)))
static const tin_impl_entry _tin_impl_<S>__<T> = { ... };
```

The linker auto-synthesizes `__start___tin_impl` / `__stop___tin_impl`
symbols when a section name is a valid C identifier (so we use
`__tin_impl` for the "C identifier" form alongside `.tin_impl` - in
practice we pick `__tin_impl` for ELF). The runtime walks
`for (e = &__start___tin_impl; e < &__stop___tin_impl; e++)`.

**Mach-O (macOS x86_64, macOS arm64).** The compiler emits each entry with:

```c
__attribute__((used, section("__DATA,__tin_impl"), aligned(8)))
static const tin_impl_entry _tin_impl_<S>__<T> = { ... };
```

ld64 / lld-Mach-O do *not* synthesize start/stop symbols. The runtime
locates the section via `getsectiondata()` from `<mach-o/getsect.h>`:

```c
size_t sz;
const tin_impl_entry *base = (const tin_impl_entry *)
    getsectiondata(&_mh_execute_header, "__DATA", "__tin_impl", &sz);
```

**Solution.** Codegen emits the same section-attribute string per target
triple. The runtime exposes one platform-agnostic API:

```c
// runtime/reflect_table.c
void _tin_iter_impl_section(void (*cb)(const tin_impl_entry *, void *), void *ctx);
```

with `#ifdef __APPLE__` selecting `getsectiondata`-based walk, otherwise
the `__start___tin_impl` / `__stop___tin_impl` linker symbols. Codegen
never sees the platform difference; only `runtime/reflect_table.c`
does.

The section attribute string IS platform-dependent, however, so codegen
must pick the right one based on the target triple at compile time:

```go
func implSectionAttr(triple string) string {
    if strings.Contains(triple, "apple") || strings.Contains(triple, "darwin") {
        return `section("__DATA,__tin_impl")`
    }
    return `section("__tin_impl")`
}
```

### Linker selection

| Host          | Target          | Linker            | Notes                                  |
|---------------|-----------------|-------------------|----------------------------------------|
| Linux         | Linux           | system `ld` / lld | Default; `lld` if `-fuse-ld=lld`       |
| Linux         | macOS           | `lld` (Mach-O)    | Already wired by cross-compile work    |
| macOS         | macOS           | `ld64` (Apple)    | Default; sole platform we don't already exercise the section trick on - verify in step 4 |
| macOS         | Linux           | `lld` (ELF)       | Already wired by cross-compile work    |

`lld` handles both ELF and Mach-O which makes every cross-compile
combination uniform. Native `ld64` on macOS is the one that needs
explicit verification once D1 lands - the design doesn't change, but
"does ld64 keep our section through `--gc-sections`-equivalent" is a
test we have to actually run.

To keep the section from being stripped: per-entry static globals are
already `__attribute__((used))`, which marks them as
`@llvm.compiler.used` in IR and prevents `--gc-sections` (lld / ELF)
and `-dead_strip` (ld64 / Mach-O) from removing them.

### `-rdynamic` portability

Symbol export from the main binary so `dladdr` / libunwind can resolve
runtime helper names in stacktraces:

| Platform | Flag                       | Where it goes |
|----------|----------------------------|---------------|
| Linux    | `-rdynamic`                | Final link    |
| macOS    | `-Wl,-export_dynamic`      | Final link    |

Already handled today in `compileIR` for whole-program builds. The
per-pkg compile/link flow must keep the same gating
(`stacktraceLinkActive` flag) and pick the right flag form per target
triple. No change to the gating logic, just keep it correct as we move
to per-pkg.

### Apple Silicon specifics

- **TF mode (`__float128`).** Apple clang on `arm64-apple-darwin`
  doesn't support `__float128` at any level; `runtime/runtime.h`
  already gates this for all `__APPLE__`. No incremental work.
- **16K page size.** Affects `mmap` alignment in
  `runtime/interop_trampoline.c`; not a codegen concern. Already
  handled by the existing runtime, which is content-cached as
  `.build/csrc/` and unchanged.
- **`presplitcoroutine` attribute.** Requires LLVM 15+; macOS 13+
  ships clang 14+, but with `presplitcoroutine` available in 15. The
  existing CI guard ("clang version" check) covers this; per-pkg
  compile inherits.
- **Rosetta x86_64 Tin on arm64 Mac.** Triple is
  `x86_64-apple-darwin`; CTFE per-fn shims under `.build/pure-fn/` are
  x86_64 Mach-O; pkg cache is whatever the user targets. The cache key
  composition (which includes the triple) keeps these separated even
  in a single working tree shared between Rosetta and native tin
  invocations.
- **Mach-O atexit ordering.** macOS runs `atexit` in LIFO order across
  the whole binary, same as Linux. D4's reverse-topological deinit
  works identically on both. Verified by D4's deinit ordering test.

### Linux ARM specifics

- **`aarch64-pc-linux-gnu` vs `aarch64-apple-darwin` triple difference.**
  Both are 64-bit ARM, but data layout strings differ slightly (f128
  alignment, vendor field, ABI). Cache keying by full triple + data
  layout (see "Cache key composition" above) keeps them apart even
  when the source is identical.
- **NEON intrinsics.** SIMD stdlib has arch-specific paths; the
  emitted IR differs from x86 SSE paths, but the same per-arch IR is
  byte-stable across runs. Determinism guard (D3) still applies; cache
  key includes triple so amd64 and arm64 entries can't collide.
- **Atomic memory model.** ARM is weaker than x86; clang lowers
  C11/Tin atomics to LDXR/STXR on `armv8`, LDADD/SWP/CAS on `armv8.1+`.
  Not our concern - clang picks the right instructions for the target;
  cache key includes triple so the resulting IR/`.o` is
  triple-specific.
- **32-bit ARM (`armv7-unknown-linux-gnueabihf`)** is *not* in the
  supported matrix. If added later, sysroot must include the right
  libgcc / libatomic; out of scope for this design.

### Cross-compile interaction with the three caches

A single `tin build -target darwin/arm64` run on a Linux/amd64 host
populates entries in both kinds of caches:

```
.build/pure-fn/<H>/bin.so          -> x86_64-pc-linux-gnu  (host)
.build/csrc/<H>/runtime.o          -> aarch64-apple-darwin (target)
.build/pkg/<H>/<I>/pkg.o           -> aarch64-apple-darwin (target)
.build/mono/<M>/bin.o              -> aarch64-apple-darwin (target)
.build/link/<E>/bin                -> aarch64-apple-darwin Mach-O
```

The cache key composition (target triple is part of the hash for
target-arch caches; host triple is part of the hash for `pure-fn/`)
guarantees these never collide. Two runs with different `-target`
flags share the host CTFE cache and have entirely separate target-arch
cache trees.

### File mode bits

Cached object files (`.o`, `.so`) are written `0644`. The runtime
helper that `dlopen`s `.build/pure-fn/<H>/bin.so` doesn't need execute
bit (Linux `dlopen` doesn't require it for non-executable .so under
normal mounts). macOS code-signing is not engaged for `.o` /
non-installed `.so`. No platform-specific work here.

### `tin clean` cleans everything

Add `.build/pkg/`, `.build/mono/`, and `.build/link/` to `tin clean`'s
removal list. Cross-platform: works the same on Linux and macOS since
it's just `os.RemoveAll`. Already part of step 7.

## Implementation order

These are internal milestones in landing the single coherent design,
not separately shipped versions. Each one is a checkpoint where the
tree builds and tests pass, but nothing about the user-facing
behavior or cache design changes between them.

1. **Determinism hardening (D3).** Audit map iteration, atom
   resolution order, anon symbol numbering, trait-impl emission
   order. Add the "compile twice, byte-identical IR" CI check.
   Lands first because it's prerequisite for D2 and benefits the
   current monolithic codegen too.

2. **Per-pkg LLVM modules (Phase 1 of D5).** Split codegen output
   so each package's IR lands in its own `*ir.Module`. Compile
   each in parallel via existing `runParallelClang`. Link all
   `.o` files. The binary is bit-identical to today; only build
   topology changes.

3. **Init + deinit order rework (D4).** Replace the single-`main()`
   initializer dump with per-pkg `_tin_init_<pkg>` and the topo
   dispatcher; emit matching per-pkg `_tin_deinit_<pkg>` and the
   reverse-topo dispatcher; register `_tin_deinit_all` via
   `atexit`. Verify init behavior is unchanged, and that top-level
   vars whose types implement `deinit` get torn down in reverse
   order on normal program exit.

4. **Link-time reflection table (D1).** Move trait-implements
   from compile-time tables to a `.tin_impl` section per pkg
   plus the link-time builder. Verify `traitof` and trait
   dispatch still work across the whole test suite.

5. **Mono cache (D2).** Carve monomorphizations out of pkg
   modules into content-addressed `.o`s under
   `.build/mono/<hash>/`. Reuse the existing slicer infrastructure.

6. **Iface vs impl split (D6).** Generate `iface.json` per pkg;
   compute `iface_hash`; record import iface_hashes in pkg sbom.

7. **Pkg cache + cache-hit fast path.** Add
   `.build/pkg/<pkg_path_hash>/<input_hash>/` lookup before
   running codegen for a pkg. Hit means skip codegen + clang.
   Final-binary cache (`.build/link/<entry_hash>/`) replaces the
   current `.build/run` and `.build/test` final-binary caches.

8. **Cross-cutting verification.** Stacktrace tests under cold
   and warm cache. `-g` build under gdb. `-target darwin/arm64`
   cross-compile producing arm64 Mach-O `.o`s while CTFE caches
   stay host arch. Touch-one-file rebuild measurements.

## Risks and gotchas

- **`@llvm.used` pinning home.** Today these live in `cg.mod` with
  whole-program scope. Per-pkg requires moving each pinning to the
  defining pkg's module. Audit during step 2.
- **CTFE pure-fn cache compatibility.** The existing slicer
  (`ctfe_emit.go`) operates on whole-program IR. With per-pkg
  modules the slicer needs to find the target fn in its home pkg's
  module. Refactor during step 5 (mono cache reuses the same slicer
  pattern).
- **Cross-pkg type identity.** LLVM type defs are per-module. The
  defining pkg owns the canonical struct layout; consumer modules
  emit echo type defs. Verify these don't conflict (matching layouts
  must be byte-identical).
- **`tin clean`** must clear `.build/pkg/`, `.build/mono/`, and
  `.build/link/` in addition to the existing dirs. Update.
- **Cross-compile.** Each per-pkg compile must respect `-target`.
  CTFE per-fn caches still go to host arch (already handled by
  `hostClangTargetFlag()`); user-binary `.o`s and mono cache `.o`s
  use the cross triple.
- **Cache size.** Mono cache and pkg cache grow without bound under
  this design. `tin clean` clears everything. Consider a future LRU
  eviction; not required for correctness.

## Test plan

- "Compile twice, byte-identical IR" CI gate (lands with step 1).
- Init/deinit ordering test: top-level vars in pkgs `a`, `b`, `c`
  where `b` imports `a` and `c` imports `b`. Each var's init prints
  its name; each deinit prints its name. Run program, verify init
  output is `a, b, c` and exit output is `c, b, a`.
- Cold build, snapshot binary; rebuild with warm cache; binary
  bit-identical.
- Edit one user file, rebuild, only that pkg's `.o` is fresh.
- Edit one stdlib pkg's *non-exported* internals, rebuild, that
  pkg's `.o` is fresh, downstream consumers stay cached.
- Edit one stdlib pkg's *exported* signature, rebuild, the pkg
  + every direct importer's `.o` is fresh.
- Add a new `impl Trait for ExistingStruct` in a new pkg; rebuild;
  no consumer recompiles, only the new pkg compiles + final
  re-link.
- `examples/stacktrace_*.tin` pass under cold and warm cache.
- `-g` build runs under gdb with file:line resolution intact.
- `tin clean` followed by full rebuild matches cold-cache timing.
- `-target darwin/arm64` build produces arm64 Mach-O `.o`s; CTFE
  caches under `.build/pure-fn` are still host ELF.
- Cache-key isolation under cross-compile: build the same source
  three times in succession with `-target linux/amd64`,
  `-target linux/arm64`, `-target darwin/arm64`; verify all three
  binaries are produced and `.build/pkg/` contains three separate
  cache trees with no overlap. Re-run all three; verify all three
  hit cache and produce bit-identical binaries.
- `-O0` -> `-O2` cache invalidation: build with `-O0`, then `-O2`;
  verify the second build does NOT reuse `-O0` `.o` files
  (different cache key, different `pkg/<input_hash>/`).
- macOS native build (when a macOS runner is available): verify
  the link-time reflection table works under `ld64`, including
  `traitof` returning correct trait lists and the section surviving
  any dead-strip equivalent.
- Linux arm64 native build (CI matrix already has
  `ubuntu-24.04-arm`): full suite, including stacktrace + debug
  info, must pass with the per-pkg cache.
- Full `examples/...` and `stdlib/...` test suites under
  incremental.

## Out of scope

- Distributed / multi-machine cache (no `sccache`-style sharing).
  Local filesystem only.
- Replacing `tin run` / `tin test` semantics. The cache layout
  changes; the user-facing commands don't.
- Changing import syntax or package semantics.

## Open questions

- `iface.json` serialization: stable JSON for debuggability ("why
  did the iface hash change?"), or opaque bytes for compactness?
  Lean toward JSON.
- Where does the pkg dependency graph live? Reuse
  `cg.importedPkgs` (currently `map[string]bool`) or build a
  fresh `map[string][]string` so we can topo-sort? The latter -
  we need order, not just membership.
- Mono cache eviction policy. None for now; revisit if the dir
  grows unboundedly in practice.
