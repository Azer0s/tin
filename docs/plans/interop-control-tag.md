# Plan: `#interop` control tag for C-callable Tin functions

Status: draft for review — not yet implemented.

Motivation:

- Tin binaries cannot be called from C code except through hand-written
  shims that know about Tin's ABI, fat pointers, ARC, and runtime init.
- A single `#interop` tag on a function should automate all of that:
  emit a C-ABI wrapper, lazy-init the runtime, marshal argument and
  return types, drain the fiber scheduler before returning, and emit
  a C header declaration on demand.
- Target users: anyone embedding Tin as a library in a larger C/C++
  program, or writing Tin code meant to be called through FFI from
  another language (Python via ctypes, Go via cgo, etc.).

---

## 1. Syntax

```
fn{#interop} name(params...) ret = body
```

The wrapper is emitted under the **same name** the user wrote. The
Tin-internal entry point is remangled to an internal name
(`__tin_<pkg>__<name>`) so the external symbol is free.

Multiple tags compose: `fn{#interop #no_thread}` is legal if the user
wants to advise that their function is not thread-safe. `#interop`
itself carries no thread-safety promise.

---

## 2. Compile-time restrictions

A `#interop` function is rejected at declaration time when any of the
following hold:

| Rejected case                      | Reason                                    |
|------------------------------------|-------------------------------------------|
| `#interop #async`                  | An async fn is a coroutine, C can't drive it |
| Return type contains `Future[T]`   | C has no way to await                     |
| Any parameter type contains `any`  | No stable C representation for boxed discriminated values |
| Generic function (`fn[T] ...`)     | No wrapper name for an un-instantiated template |
| Struct method                      | v1: top-level functions only              |
| Reserved name (`main`, etc.)       | Would clobber the binary's entry point    |
| Duplicate `#interop` name across functions | Linker error avoided by raising at parse/codegen site |

---

## 3. Allowed types at the boundary

Two whitelists. Everything outside is a compile error with a specific
message pointing at the offending param/return.

### 3.1 Parameter types (C → Tin)

| Tin type                    | C ABI shape                              | Marshal                                  |
|-----------------------------|------------------------------------------|------------------------------------------|
| `i8`..`i64`, `u8`..`u64`    | matching `int8_t`..`uint64_t`            | pass-through                             |
| `f32`, `f64`                | `float`, `double`                        | pass-through                             |
| `bool`                      | `uint8_t` (document: non-zero = true)    | pass-through                             |
| `*T`, `*void`               | `T*`, `void*`                            | pass-through                             |
| `string`                    | `const char*`                            | copy bytes into a fresh ARC Tin string   |
| `[T]` fat array             | expands to two C params `T*` + `int64_t` | copy data into a fresh ARC fat array     |
| packed cLayoutStruct (`struct{#packed} S`) | `struct S*` or by value | pass-through (layouts already match)     |

Notes:
- A NULL `const char*` on the C side is mapped to the empty Tin string
  (length 0, data = the empty-string sentinel). Don't segfault.
- For `[T]` params, the arity of the C signature is larger than the Tin
  one; the header generator makes this explicit.

### 3.2 Return types (Tin → C)

| Tin type                    | C ABI shape                              | Marshal                                  |
|-----------------------------|------------------------------------------|------------------------------------------|
| `i8`..`i64`, `u8`..`u64`    | matching integer type                    | pass-through                             |
| `f32`, `f64`                | `float`, `double`                        | pass-through                             |
| `bool`                      | `uint8_t`                                | pass-through                             |
| `*T`, `*void`               | `T*`, `void*`                            | pass-through (pointer lifetime is user's problem) |
| packed cLayoutStruct        | `struct S` by value or via out-ptr       | ABI matches                              |
| `string`                    | `const char*` (user frees)               | copy via `tin_extern_alloc(len+1)` + NUL |
| `[T]` fat array             | out-params `T** out_data, int64_t* out_len`, return status | copy via `tin_extern_alloc(len*sizeof(T))` |

Notes:
- For `string` returns, the Tin string is NUL-terminated in the copy.
  Embedded NUL bytes survive but C-string conventions will truncate
  reads — document that binary strings should use `[u8]`.
- For `[T]` returns the wrapper's C signature sprouts two out-params.
  The function's logical return value is mapped to an `int` status
  (0 = OK, nonzero = failure, e.g. OOM). This is the only shape where
  the wrapper's arity is wider than the Tin signature's + 1.
- OOM (`tin_extern_alloc` returns NULL): the wrapper sets the out-ptr
  to NULL, zeros the out-length (if applicable), and returns NULL or a
  non-zero status as appropriate for the return shape. No panic.

---

## 4. Runtime initialization

Lazy + atomic. Every wrapper begins with:

```c
if (!atomic_load_explicit(&_tin_rt_initialized, memory_order_acquire)) {
    _tin_runtime_init_once();
}
```

`_tin_runtime_init_once` uses a compare-and-swap on `_tin_rt_initialized`
to ensure exactly-one initialization across concurrent first callers.
Double-init on success is harmless; failures during init (e.g. OOM
during atom-table setup) abort the process — that's not a
user-recoverable condition.

A Tin binary with a `main()` also calls `_tin_runtime_init_once` from
its existing entry prologue so that a subsequent `#interop` call
flipped into the same binary (rare) sees the flag already set.

Init work (v1 list):
- Atom table initialization.
- ARC slab allocator init (if any).
- Scheduler queue init (empty).
- No background threads are spawned.

---

## 5. The `tin_set_extern_alloc` callback

Exported from the runtime:

```c
typedef void* (*tin_alloc_fn)(size_t);

void tin_set_extern_alloc(tin_alloc_fn fn);   /* NULL resets to malloc */
void* tin_extern_alloc(size_t n);             /* internal helper       */
```

- `tin_extern_alloc` is what `#interop` wrappers call when returning
  strings or fat arrays.
- Default allocator is `malloc`. The matching free is whatever pairs
  with the user's allocator (default `free`). Tin does not export a
  `tin_set_extern_free` — Tin never frees buffers it has handed to C.
- `tin_set_extern_alloc` is a process-wide global. Thread-safety is
  the user's problem; document "call before any interop traffic."
- If `tin_extern_alloc` returns NULL, the wrapper propagates NULL to
  the C caller (no panic, no retry).

---

## 6. Scheduler semantics (E2: drain-before-return)

When a `#interop` function's body spawns fibers, those fibers run on
the same thread as the wrapper, via explicit scheduler ticks, before
the wrapper returns to C.

Flow inside the wrapper:
1. Init runtime if needed.
2. Marshal inputs.
3. Call the Tin-internal entry point; capture its return value.
4. **Drain**: while the scheduler has runnable fibers, tick once.
5. Marshal the return value through `tin_extern_alloc` where needed.
6. Return to C.

Consequences:
- From the C caller's perspective, the interop call is synchronous.
  Any side effects from fibers spawned during the body are observable
  when control returns.
- A `#interop` function that spawns a long-running or non-terminating
  fiber will never return to C. This is documented, not enforced.
- No background scheduler thread. No multi-threading is introduced.

Upgrading to a background scheduler thread (E1 in the design
discussion) is a future-work item and a semantic widening — code that
worked under E2 continues to work.

---

## 7. Panic behavior

Panics inside a `#interop` function use Tin's normal semantics:
deferred chain runs, then `exit(1)`. Users who want to survive a panic
at the FFI boundary write `defer { recover() }` at the top of the
body, same as they would anywhere else. No special magic at the
wrapper.

---

## 8. `--emit-header=<path>` CLI flag

```
tin build --emit-header=foo.h file.tin
tin build file.tin --lib --emit-header=foo.h    /* combined with --lib */
```

Emits a `.h` file with:
- Include guards.
- `extern "C"` block for C++ consumers.
- Typedef + prototype for `tin_set_extern_alloc`.
- One prototype per `#interop` function, with the mapped C signature
  from §3.
- A trailing comment listing the Tin signature each prototype was
  generated from, for user reference.

The header does not include any Tin types directly; it depends only on
`<stdint.h>` and `<stddef.h>`.

---

## 9. Implementation phases

### Phase A — parser + validation (small)

1. `#interop` is already recognizable via the existing tag parser; no
   syntax change.
2. After parse, walk all `FuncDecl` nodes and reject:
   - `#interop #async`
   - generic (`len(TypeParams) > 0`)
   - method (`IsMethod` / inside a struct body)
   - reserved name `main`
   - duplicate `#interop` names

### Phase B — type whitelist enforcement (small)

3. For each `#interop` function, walk its param and return types and
   reject anything outside the tables in §3. The diagnostic names the
   offending type and suggests the nearest allowed type where
   applicable.

### Phase C — runtime C helpers (small)

4. Add `runtime/interop.c` implementing:
   - `_tin_rt_initialized` (atomic int)
   - `_tin_runtime_init_once()`
   - `tin_set_extern_alloc(fn)`
   - `tin_extern_alloc(size)` (reads the global or defaults to malloc)
5. Declare the atomic flag and setter in `runtime/runtime.h`.

### Phase D — wrapper emission (medium)

6. In codegen, for each `#interop` `FuncDecl`:
   - Emit the Tin-internal function under its mangled name (existing
     pipeline).
   - Emit a second LLVM function under the user's verbatim name with
     C ABI. Its body:
     a. Load `_tin_rt_initialized`; call init if zero.
     b. For each param, emit the marshal sequence from §3.1.
     c. Call the internal entry.
     d. Emit scheduler-drain loop.
     e. Marshal the return value (§3.2), including `tin_extern_alloc`
        for string / array returns.
     f. Release any ARC handles still owned (the Tin return value, the
        fresh ARC copies created for string / array params).
     g. Return to C.
7. Tin-internal call sites must keep using the mangled name (already
   the case — only external entry points see the wrapper).

### Phase E — scheduler drain primitive (small)

8. Expose an internal `_tin_scheduler_drain()` that ticks the queue
   until empty. Runs synchronously on the calling thread. Called from
   the wrapper before return.

### Phase F — `--emit-header` flag (small)

9. New CLI flag `--emit-header=<path>`. When set, after codegen, walk
   funcDecls filtered by `hasTag(fd.Tags, "interop")` and write the C
   header per §8.

### Phase G — tests

10. Positive:
    - `fn{#interop} add(a i32, b i32) i32` — primitive round-trip.
    - `fn{#interop} greet(name string) string` — string in + out via
      `tin_extern_alloc`.
    - `fn{#interop} sum(xs [i32]) i32` — fat array param split.
    - `fn{#interop} range(lo i32, hi i32, out_data *out *i32, out_len *out *i64) i32`
      — fat array return via out-params.
    - `tin_set_extern_alloc(arena_alloc)` — custom allocator path.
    - OOM: allocator returns NULL → wrapper returns NULL / zero.
11. Negative (shell script similar to
    `examples/scoped_struct_tags_errors.sh`):
    - `#interop #async` rejected.
    - `#interop` returning `Future[T]` rejected.
    - `#interop` with `any` param rejected.
    - `#interop` on a generic rejected.
    - `#interop` on a method rejected.
    - `#interop` named `main` rejected.
    - Duplicate `#interop` names rejected.

### Phase H — documentation

12. `SPEC.md`: new "Interop" subsection under Control tags referencing
    this plan.
13. `docs/13-control-tags.md`: add `#interop` entry with the
    restriction + allowed-types tables.
14. `docs/08-interop.md`: new page or section describing the C-side
    usage (`tin_set_extern_alloc`, header generation, ownership
    conventions).
15. Example: `examples/interop_minimal.tin` + companion
    `examples/interop_minimal_driver.c` demonstrating the flow.

### Phase I — shared-library build (prerequisite check)

`tin build --lib file.tin` already emits an object file. For libraries,
users will want `.so` / `.dylib` output. Check whether this already
works via `tin build --lib` + linker wrapper (e.g., `gcc -shared`). If
not, a small CLI extension (`-shared` flag) lands here. May be a
no-op if the existing flow already handles it.

---

## 10. Non-goals (v1)

- Background scheduler thread (E1). Revisit when a real user needs it.
- `#interop` on struct methods. v1 is top-level only.
- `#interop` on generics. User writes per-type wrappers.
- Auto-`defer recover` at the wrapper boundary. Panics kill the
  process; users who care write the recover themselves.
- Explicit wrapper-name override (`#interop("c_name")`). Tin name is
  used verbatim. Can be added later without breaking anything.
- Return types of `struct` with ARC-tracked fields. No clean ownership
  story. Deferred.
- `any`, trait objects, ADTs, closures at the boundary. No stable C
  representation.
- C++ name mangling for C++ consumers. `extern "C"` block in the
  header is enough.
- Generating a `.def` / `.rc` / Windows-specific export file.

---

## 11. Open questions / deferred decisions

1. **Param-copy for `string` is O(len) on every call.** For hot paths,
   consider a future zero-copy variant (`fn{#interop #borrow_string}`)
   where the Tin side promises not to retain the pointer. Not in v1.
2. **`tin_extern_alloc` error channel.** Right now OOM is signaled by
   returning NULL. If the user wants a richer error (what went wrong),
   we'd need a TLS `tin_last_error` string. Left for a follow-up.
3. **Reentrance**: C → interop → extern-C → interop → Tin. The lazy
   init check is idempotent; the scheduler drain is re-entrant only if
   we don't hold a lock across the inner call. The v1 design uses no
   locks, so should be fine, but needs a test.
4. **Windows support.** `__attribute__((visibility("default")))`
   vs `__declspec(dllexport)`. v1 targets Linux / macOS; Windows is a
   later concern.

---

## 12. Test plan (summary)

- `examples/interop_minimal.tin` + `.c` driver: primitives round-trip.
- `examples/interop_string.tin` + `.c` driver: string in + out,
  custom allocator.
- `examples/interop_array.tin` + `.c` driver: fat array in + out.
- `examples/interop_errors.sh`: 7 compile-error cases.
- Negative: panic in `#interop` calls `exit(1)` (verify via driver
  exit code).
- Verify `tin_set_extern_alloc(NULL)` resets to malloc.
- Verify the emitted header compiles standalone (`cc -c foo.h`).
