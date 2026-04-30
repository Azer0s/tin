# Plan: runtime `stacktrace()` via LLVM libunwind

## Status
Deferred. `sourcepos()` ships first; this is the design we'd implement
when runtime stack walking is genuinely wanted.

## Goal
`stacktrace()` returns `[atom]` where each entry identifies one frame on
the live call stack. Top frame is the caller of `stacktrace()`. The
trace walks:

1. The frames of the currently-executing fiber (or OS thread, if not
   inside a fiber).
2. Across fiber boundaries: when the bottom of a fiber's stack is the
   trampoline that the runtime entered the fiber with, the trace
   continues with the frozen IP recorded at the spawn site that created
   this fiber, then the spawn site of the fiber that spawned that one,
   and so on up to main.

Closures, trait dispatch, generics, and inlined-but-not-vanished frames
all surface naturally because they compile to ordinary LLVM functions
the unwinder can see.

Three cost properties matter, in priority order:
1. **Zero per-call cost when stacktrace is never invoked.** Programs that
   don't call `stacktrace()` get zero per-spawn overhead, zero unwind
   tables in the binary, zero dynamic-symbol-table bloat. The spawn-site
   codegen for fiber-creation is itself conditional on `cg.stacktraceUsed`
   so the runtime fast path is unchanged for programs that don't opt in.
   The `spawn_caller_ip` / `spawn_parent` fields stay on the TinFiber
   struct (16 bytes RAM) but are never written to and never read.
2. **No binary-size tax for programs that don't use stacktrace.** Unwind
   tables (`.eh_frame` on ELF, `__unwind_info`/`__compact_unwind` on
   Mach-O) are only emitted when the compiler proves the program calls
   `stacktrace()` (or a fn that transitively does). Default builds get
   `-fno-unwind-tables -fno-asynchronous-unwind-tables` and stay lean.
3. **`stacktrace()` itself runs in microseconds.** Per-invocation cost is
   one libunwind walk plus one `dladdr` lookup per frame; budget ~5-50µs
   for a 64-frame walk. No shadow stack maintenance, no per-call
   instrumentation anywhere else.

Notably **NOT** included: full DWARF debug info. We accept the loss of
`file:line:col` per frame in exchange for not bloating every binary by
5-15% just because one helper somewhere happens to call stacktrace.

## Choice of unwinder

LLVM **libunwind** (the one shipped from llvm-project / compiler-rt, NOT
GNU libunwind from nongnu.org and NOT libgcc's `_Unwind_Backtrace`). The
public API matches HP libunwind so the code is portable, but linking
against the LLVM build keeps the project's tooling story coherent
(everything else already comes through clang/LLVM).

Rationale vs alternatives:
- **Glibc `backtrace()`**: glibc-only, needs frame pointers, weaker on
  optimized code.
- **GNU libunwind**: extra dep many distros split into two packages
  (`libunwind8` + `libunwind-dev`); LLVM's is shipping with toolchains we
  already require.
- **libgcc `_Unwind_Backtrace`**: C++-EH oriented, awkward for plain
  introspection, frame-skipping APIs are private.

## Surface

```tin
// returns one atom per frame, top-of-stack first (i.e. immediate caller
// of stacktrace() is element 0). Empty array if unwinding fails entirely.
fn stacktrace() [atom]

// optional explicit frame cap. Useful for deep recursion or if the
// caller wants a tighter trace. Clamped to [1, 1024] in the runtime
// to bound the stack allocation; values outside that range silently
// saturate.
fn stacktrace(cap i64) [atom]

// optional filter atoms - drops noisy frames before the cap is applied
// so the returned slice contains up to `cap` *kept* frames. Known
// filters (must be a literal `[atom]` so codegen can fold to a constant
// bitfield):
//
//   'hide_libc      drop frames in libc / libpthread / libsystem
//   'hide_unknown   drop frames that resolved to "??+0x<addr>"
//   'hide_runtime   drop frames whose symbol starts with "_tin_"
//   'hide_main      drop main() / _start / __libc_start_*
//
// Combine freely; an empty array is the same as omitting the arg.
fn stacktrace(cap i64, opts [atom]) [atom]
```

The default cap is 64 frames. All forms route through the same runtime
helper. To use opts you must specify cap (the array always comes after
the amount) — this avoids overload-resolution ambiguity between the
`[atom]` and `i64` shapes.

To skip frames at the top — useful when wrapping `stacktrace()` in a
logging helper — slice the result: `stacktrace()[1:]` drops the
immediate caller. We deliberately don't expose a `skip:` named-arg form;
slicing keeps the API one builtin instead of bolting on syntax for a
single corner case.

## Atom format

Each frame's atom matches the compile-time `sourcepos()` shape so a
captured stacktrace and a sourcepos atom for the same source line
compare equal as strings. Symbol prefix comes from libunwind's
`unw_get_proc_name` (live frames) or `dladdr` (spawn frames); file:line:col
comes from libdwfl. The four resolution outcomes:

```
"<symbol>@<file>:<line>:<col>"              // both resolved (typical user-fn frame)
"<file>:<line>:<col>"                       // line known, no symbol (rare)
"<symbol>+0x<offset>"                       // symbol known, no line info
"<lib_basename>:<symbol>+0x<offset>"        // symbol-only frame in a shared library
"??+0x<absolute_address>"                   // nothing resolved
"<spawn-of>:..."                            // frozen frame at a fiber boundary, any of the above shapes
```

Examples:

```
"render@src/server.tin:142:7"           // user fn with full debug info
"_worker_thread@runtime/fiber.c:541:5"  // Tin runtime helper with debug info
"compute_value+0x1c"                    // user fn without source position
"libssl.so.3:SSL_read+0x142"            // frame is in a linked C library
"<spawn-of>:run_workers@src/main.tin:42:5"
"<spawn-of>:libssl.so.3:SSL_handshake+0x91"
"??+0x7f3a4b8c1234"                     // dladdr / libdwfl both gave up (rare)
```

Two prefix conventions, layered:
- `<spawn-of>:` — frame frozen at fiber-spawn time, not on the live stack.
- `<lib_basename>:` — frame is in a linked shared library, not the main
  binary. Frames in the main binary omit this prefix to keep the common
  case clean. The basename is the trailing path component of
  `dladdr`'s `dli_fname` (e.g. `libssl.so.3`, not `/usr/lib/libssl.so.3`).

Consumers can filter by either prefix. They're independent: a frozen
spawn frame in a shared lib is `<spawn-of>:<lib>:<sym>+<off>`.

The `file:line:col` shape matches what the compile-time `sourcepos()`
builtin produces. A user can compare `stacktrace()[0]` against
`sourcepos(my_fn)` directly, or filter the trace by source-file
substring without parsing the offset variants. The runtime achieves
this by linking libdw (elfutils) and resolving each IP via
`dwfl_module_getsrc`; the compiler emits `.debug_line` (via
`-gline-tables-only`) only when stacktrace is reachable, so default
builds stay lean. When line tables are missing (asm fragments, JIT
code, third-party libs without debug packages) the resolver falls back
through `unw_get_proc_name` and `dladdr` to the symbol+offset form.

## Conditional unwind-table emission

The compiler tracks whether `stacktrace()` is reachable in the program.
Detection:

1. During codegen, set `cg.stacktraceUsed = true` when the `stacktrace`
   builtin name is recognised (similar to how `sourcepos` is intercepted
   in genCallExpr today). Imported packages contribute through the same
   pass.
2. In main.go (post-codegen, before composing clang flags), branch on
   `cg.StacktraceUsed()`:

   **Used (Linux / FreeBSD):**
   ```
   -funwind-tables
   -fasynchronous-unwind-tables
   -rdynamic                    # ELF only; expose private-linkage Tin
                                # symbols to the dynsym so dladdr can
                                # resolve them
   ```

   **Used (macOS):**
   ```
   -funwind-tables
   -fasynchronous-unwind-tables
                                # NO -rdynamic on Darwin — clang either
                                # warns or no-ops it. Mach-O keeps local
                                # symbols in the symbol table by default
                                # (until `strip` removes them), and dladdr
                                # reads from there, so private-linkage
                                # Tin fns resolve without extra flags.
   ```

   **Not used (all platforms):**
   ```
   -fno-unwind-tables
   -fno-asynchronous-unwind-tables
   ```

3. Per-TU compilation: the flag set is uniform across TUs in a single
   build. The csrc cache key already includes the canonical clang argv,
   so changing the flag automatically invalidates cached objects.

Platform notes for the conditional-strip path:
- **Linux ELF:** stripping `.eh_frame` and `.eh_frame_hdr` saves a real
  5-10% of binary size on most programs.
- **macOS Mach-O:** the linker still emits some `__compact_unwind` for
  system unwinder support regardless of `-fno-unwind-tables`, so the
  savings shrink to ~1-3%. The conditional-emission story is still
  correct — just the win is smaller on Darwin.
- **FreeBSD ELF:** behaves like Linux.

## Symbol resolution

Two steps per frame:

1. **Symbol name + offset within fn:** `unw_get_proc_name` and
   `unw_get_proc_info` from libunwind. These read the same OS facilities
   `dladdr` would use, but go through the libunwind cursor we already
   have, which means libunwind can sometimes find symbols dladdr can't
   (e.g. when the unwind-table FDE entries carry richer info than the
   dynsym).
2. **Containing library path:** `dladdr` for the `dli_fname` field.
   libunwind doesn't surface this directly, and we want it for the
   `<lib>:<sym>` prefix on frames that landed in a shared library.

Both calls are O(1) symbol-table lookups; the cost is dominated by the
mutex inside the OS dynamic linker, not the lookup itself.

### IP-minus-1 (load-bearing)

`unw_get_reg(UNW_REG_IP)` returns the **return address**, which is the
instruction *after* the call that produced this frame. If a call sits
right at the end of a function's machine code, the return address falls
into the *next* function — and the symbol resolver returns the wrong
name.

The standard fix is to subtract 1 from the IP before resolution,
landing inside the call instruction itself:

```c
uintptr_t ip;
unw_word_t raw;
unw_get_reg(&cur, UNW_REG_IP, &raw);
ip = (uintptr_t)raw - 1;   // see below for the top-frame exception
```

**Exception: the top frame.** The first call to `unw_init_local` lands
on the IP of the actual currently-executing instruction inside
`tin_capture_stacktrace`, not a return address. We skip the top two
frames anyway (the helper itself and its wrapper), so by the time we
start emitting frames they're all from `unw_step` and are return
addresses. Apply -1 uniformly to every emitted frame.

### Per-IP atom-code cache

A naive implementation calls `_tin_learn_atom` for every captured
frame. `_tin_learn_atom` acquires a process-wide mutex and walks a
linked list. A 64-frame trace = 64 mutex acquisitions; a program that
captures stacktraces in a hot path pays this on every call.

The IPs themselves are stable for a given run (modulo dlopen, which we
don't do beyond the CTFE cache that's separate). So we memoise on the
IP:

```c
// Thread-local cache: IP -> already-interned atom code.
// The cache key is the post-minus-one IP plus a one-bit "kind" for
// spawn-of frames (since the same IP can appear once as a live frame
// and once as a spawn-of frame in different traces).
static __thread struct {
    uintptr_t key;       // (ip << 1) | spawn_of
    int32_t   atom_code;
} _tin_st_cache[256];

static int32_t resolve_and_intern(uintptr_t ip, int spawn_of) {
    uintptr_t key = (ip << 1) | (spawn_of & 1);
    uint32_t  slot = (uint32_t)((key * 0x9E3779B1u) >> 24) & 255;
    if (_tin_st_cache[slot].key == key) {
        return _tin_st_cache[slot].atom_code;
    }
    char buf[512];
    format_frame(ip, spawn_of, buf, sizeof buf);
    int32_t code = _tin_learn_atom(buf);
    _tin_st_cache[slot].key = key;
    _tin_st_cache[slot].atom_code = code;
    return code;
}
```

256 slots × 16 bytes = 4 KiB of TLS per OS thread that ever calls
stacktrace. Open-addressed direct-mapped cache (no chaining) — hash
collisions overwrite, so a pathological access pattern thrashes but
never corrupts. Hot stacktrace paths converge to a fully-warm cache
within a few calls and skip the global atom mutex entirely.

A 4 KiB TLS allocation is paid only by threads that actually call
stacktrace; threads that never do never touch the cache.

### Why dynsym still matters (and why linkage promotion is required)

Empirically verified during Phase 2 implementation: `-rdynamic` on its
own does NOT make Tin user fns visible to dladdr. Tin's funcs.go marks
non-#interop user fns with LLVM `internal` linkage, which lowers to ELF
STB_LOCAL. Local-binding symbols never reach the dynamic symbol table
regardless of `--export-dynamic` / `-rdynamic`; the linker flag only
exports STB_GLOBAL symbols. Without intervention every Tin frame would
render as `??+0xADDR`.

The compiler resolves this by promoting Tin user fns to external linkage
when an AST pre-pass (codegen.detectStacktraceUsage) finds any
`stacktrace(...)` call site reachable from `prog.Stmts`. The promotion
is gated on cg.stacktraceUsed in funcs.go:

```go
if !hasTag(n.Tags, "interop") && !cg.stacktraceUsed {
    f.Linkage = enum.LinkageInternal
}
```

This costs some per-fn DCE / IPO opportunity inside clang for programs
that use stacktrace, but `-ffunction-sections` + `--gc-sections` still
strip unreachable user fns at link time, so binary-size impact is
modest. Programs that don't reference stacktrace are unaffected; they
keep internal linkage and the existing optimizer behaviour.

On macOS dyld already exposes the binary's own symbols through the
symbol table (until `strip` removes them), so no `-rdynamic` is needed.
The same internal/external linkage mechanic still applies on Mach-O —
STB_LOCAL symbols also stay out of dyld's symbol exports — so the
linkage promotion is platform-agnostic.

Demangling is unnecessary for Tin's own symbols (we don't mangle in the
C++ sense). For `extern fn` calls into C++, the user can pipe through
`c++filt` if they want demangled names.

## Cross-fiber spawn-chain capture

This is part of the always-on baseline, not a follow-up. Without it
`stacktrace()` lies by omission whenever it runs inside a fiber.

### Fiber struct change

```c
typedef struct TinFiber {
    /* ... existing fields ... */
    uintptr_t      spawn_caller_ip;   // IP in the parent at the spawn site
    struct TinFiber *spawn_parent;    // non-NULL for fibers spawned by another fiber
} TinFiber;
```

Two pointer-sized fields per fiber. Initialised at fiber creation,
read-only thereafter, no synchronisation needed.

### Spawn-site codegen (conditional)

Spawn-site codegen is gated on `cg.stacktraceUsed`. Programs that don't
call `stacktrace()` emit the unchanged spawn fast path; programs that do
emit the chain-capture form.

`tin_spawn_fiber`'s C signature accepts both extra args unconditionally;
the unused-stacktrace branch passes 0 / NULL and the runtime skips the
parent-retain + ip-store work via a single nullness check. This keeps
the runtime ABI stable across stacktrace-on / stacktrace-off builds.

**Stacktrace not used (default):**

```llvm
; spawn worker(args)
%fib = call %TinFiber* @tin_spawn_fiber(
    i8* bitcast (void(...)* @worker to i8*),
    i8* %args,
    i8* null,                  ; caller_ip = 0
    %TinFiber* null            ; parent_fiber = NULL
)
```

The runtime sees `caller_ip == 0` and skips the spawn-record write +
parent-retain. Cost: one extra register move per spawn (~0.5ns on
x86_64). Practically free.

**Stacktrace used:**

```llvm
; spawn worker(args)
%retaddr = call i8* @llvm.returnaddress(i32 0)
%fib = call %TinFiber* @tin_spawn_fiber(
    i8* bitcast (void(...)* @worker to i8*),
    i8* %args,
    i8* %retaddr,
    %TinFiber* %current_fiber
)
```

`@llvm.returnaddress(0)` gives the IP in the user's fn just after the
spawn call. dladdr later resolves it to `<user_fn>+<offset>`. The
current fiber pointer comes from the per-thread fiber TLS the scheduler
already maintains. Per-spawn cost: ~10ns (one returnaddress + one
parent-retain + two stores).

### Walk algorithm

```c
int32_t tin_capture_stacktrace(int32_t *out, int32_t cap) {
    int32_t n = walk_current_stack(out, cap);

    // Walk the spawn chain. Each parent contributes exactly one frame
    // (the IP frozen at its spawn site); we don't try to reconstruct
    // the parent's full stack-as-of-spawn because the parent has moved
    // on since.
    TinFiber *fib = tin_current_fiber();
    while (fib && fib->spawn_parent && n < cap) {
        char buf[256];
        Dl_info info;
        uintptr_t ip = fib->spawn_caller_ip;
        if (dladdr((void *)ip, &info) && info.dli_sname) {
            unsigned long off = (unsigned long)ip - (unsigned long)info.dli_saddr;
            snprintf(buf, sizeof buf,
                "\"<spawn-of>:%s+0x%lx\"", info.dli_sname, off);
        } else {
            snprintf(buf, sizeof buf, "\"<spawn-of>:??+0x%lx\"", ip);
        }
        out[n++] = _tin_learn_atom(buf);
        fib = fib->spawn_parent;
    }

    return n;
}
```

Main is the bottom: the runtime allocates a TinFiber for the OS thread's
initial stack with `spawn_parent = NULL`. The loop terminates there.

Frames are emitted top-of-stack-first: live frames of the current fiber,
then `<spawn-of>:` parent, then `<spawn-of>:` grandparent, and so on.

## Runtime: `runtime/stacktrace.c`

```c
#include <libunwind.h>
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>
#include "runtime.h"
#include "fiber.h"

#define TIN_ST_DEFAULT_CAP   64
#define TIN_ST_MAX_CAP       1024
#define TIN_ST_CACHE_SLOTS   256
#define TIN_ST_BUFSZ         512   // symbol+lib+offset, generously sized

static __thread struct {
    uintptr_t key;        // (ip << 1) | spawn_of
    int32_t   atom_code;
} _tin_st_cache[TIN_ST_CACHE_SLOTS];

static int32_t resolve_and_intern(uintptr_t ip, int spawn_of);

// Captures up to `cap` frames into `out`. `cap` is clamped to
// [1, TIN_ST_MAX_CAP] before allocation so a malicious or sloppy
// caller can't request a megabyte of stack. Returns the number written.
//
// Concurrent-safe: the libunwind cursor lives entirely on the calling
// thread's stack, dladdr is reentrant, and the IP-cache is __thread.
// Two fibers calling stacktrace simultaneously each see their own
// thread's view; nothing is shared except the global atom intern table
// (already mutex-protected, and bypassed for cached IPs).
int32_t tin_capture_stacktrace(int32_t *out, int32_t cap) {
    if (cap < 1) cap = 1;
    if (cap > TIN_ST_MAX_CAP) cap = TIN_ST_MAX_CAP;

    unw_context_t ctx;
    unw_cursor_t  cur;
    if (unw_getcontext(&ctx) < 0) return 0;
    if (unw_init_local(&cur, &ctx) < 0) return 0;

    int32_t n = 0;

    // Skip frames internal to the dispatch path:
    //   frame 0 = tin_capture_stacktrace itself
    //   frame 1 = the wrapper emitted for the builtin
    // examples/stacktrace_smoke.tin pins these so a codegen change that
    // alters wrapping is caught.
    for (int skip = 0; skip < 2; skip++) {
        if (unw_step(&cur) <= 0) return 0;
    }

    // Walk the current fiber's frames. We subtract 1 from each IP before
    // resolution (see "IP-minus-1" in the symbol-resolution section) so
    // calls at the very end of a function resolve to the calling fn,
    // not the next fn over.
    while (n < cap && unw_step(&cur) > 0) {
        unw_word_t raw;
        if (unw_get_reg(&cur, UNW_REG_IP, &raw) < 0) break;
        uintptr_t ip = (uintptr_t)raw - 1;
        out[n++] = resolve_and_intern(ip, /*spawn_of=*/0);
    }

    // Append the spawn chain. Each parent contributes exactly one frame
    // (the IP frozen at its spawn site); we don't try to reconstruct
    // the parent's full stack-as-of-spawn because the parent has moved
    // on since.
    for (TinFiber *fib = tin_current_fiber();
         fib && fib->spawn_parent && n < cap;
         fib = fib->spawn_parent) {
        // spawn_caller_ip is already the return address from
        // llvm.returnaddress(0); apply the same -1 correction so the
        // resolver lands inside the call instruction.
        uintptr_t ip = fib->spawn_caller_ip - 1;
        out[n++] = resolve_and_intern(ip, /*spawn_of=*/1);
    }

    return n;
}

// Resolve the ip to a frame string and intern it as an atom code,
// memoising the result thread-locally so the next stacktrace that
// includes the same IP skips the global atom mutex.
static int32_t resolve_and_intern(uintptr_t ip, int spawn_of) {
    uintptr_t key = (ip << 1) | (uintptr_t)(spawn_of & 1);
    uint32_t  slot = (uint32_t)((key * 0x9E3779B1u) >> 24) & (TIN_ST_CACHE_SLOTS - 1);
    if (_tin_st_cache[slot].key == key && _tin_st_cache[slot].atom_code != 0) {
        return _tin_st_cache[slot].atom_code;
    }

    // libunwind's symbol API needs a fresh cursor anchored at this IP.
    // We can't reuse the walk's cursor; cheaper to just dladdr first
    // and only fall back to libunwind's resolver when dladdr fails to
    // produce a name. In practice dladdr handles every Tin / linked-C
    // case once -rdynamic is on. (We considered cursor-based resolution
    // via unw_get_proc_name; left as a follow-up if dladdr proves
    // insufficient on some target.)
    char buf[TIN_ST_BUFSZ];
    const char *spawn_pfx = spawn_of ? "<spawn-of>:" : "";

    Dl_info info;
    if (dladdr((void *)ip, &info) && info.dli_sname) {
        unsigned long off = (unsigned long)ip - (unsigned long)info.dli_saddr;

        // Only prefix the lib name when the frame is NOT in the main
        // binary. dli_fname for the main binary is argv[0] (or
        // /proc/self/exe); for shared libs it's the .so path.
        const char *lib = "";
        if (info.dli_fname && _tin_is_shared_lib_path(info.dli_fname)) {
            const char *base = strrchr(info.dli_fname, '/');
            lib = base ? base + 1 : info.dli_fname;
        }

        if (lib[0] != '\0') {
            if (off == 0) {
                snprintf(buf, sizeof buf, "\"%s%s:%s\"",
                    spawn_pfx, lib, info.dli_sname);
            } else {
                snprintf(buf, sizeof buf, "\"%s%s:%s+0x%lx\"",
                    spawn_pfx, lib, info.dli_sname, off);
            }
        } else if (off == 0) {
            snprintf(buf, sizeof buf, "\"%s%s\"", spawn_pfx, info.dli_sname);
        } else {
            snprintf(buf, sizeof buf, "\"%s%s+0x%lx\"",
                spawn_pfx, info.dli_sname, off);
        }
    } else {
        snprintf(buf, sizeof buf, "\"%s??+0x%lx\"",
            spawn_pfx, (unsigned long)ip);
    }

    int32_t code = _tin_learn_atom(buf);
    _tin_st_cache[slot].key = key;
    _tin_st_cache[slot].atom_code = code;
    return code;
}

// _tin_is_shared_lib_path returns true when path looks like a shared
// library (.so on Linux/BSD, .dylib on macOS) and not the main binary.
// The main binary's dli_fname matches argv[0] / /proc/self/exe; we
// distinguish by extension since libunwind doesn't surface the
// "is main" bit directly.
static int _tin_is_shared_lib_path(const char *path) {
    size_t len = strlen(path);
    if (len > 3 && memcmp(path + len - 3, ".so", 3) == 0) return 1;
    if (len > 6 && memcmp(path + len - 6, ".dylib", 6) == 0) return 1;
    // Versioned .so: libfoo.so.3 — match ".so." anywhere
    return strstr(path, ".so.") != NULL;
}
```

Notes:
- Buffer is 512 bytes, generous enough for typical Tin and C symbols
  even with monomorphised generics. Truncation is silent (snprintf
  guarantees NUL termination); we accept it rather than dynamically
  growing.
- The cache is direct-mapped open-addressed (256 × 16 B = 4 KiB TLS per
  thread that calls stacktrace). On a hot stacktrace path this gives
  near-zero amortised cost beyond a few register operations and a
  hashtable hit.
- The leading/trailing `"` characters in `snprintf` produce atoms that
  match the in-source complex-atom literal form (same convention as
  sourcepos), so users can compare a captured frame against a
  hand-written atom literal directly.

## Codegen wiring

In `codegen/exprs_call.go` alongside `sourcepos`:
- Recognise the `stacktrace` builtin name with zero or one i64 arg.
  The runtime clamps the cap to `[1, 1024]` so a constant-folded
  out-of-range value still produces a valid call.
- Set `cg.stacktraceUsed = true`.
- Allocate a stack buffer sized for the requested cap (or 64 by
  default) of i32 entries.
- Emit a call to `tin_capture_stacktrace(buf, cap)`.
- Wrap the populated buffer into an `[atom]` fat-pointer the same way an
  `[i32]` slice return is built today.

Frame skipping ("don't include the immediate caller of stacktrace")
is left to the user: `stacktrace()[1:]` slices the result. Adding a
`skip:` named-arg form would expand the syntax surface for one corner
case.

In codegen for `spawn`:
- Branch on `cg.stacktraceUsed`. When false, emit the existing spawn
  fast path with NULL extra args (the runtime nullness check skips the
  bookkeeping). When true, compute `llvm.returnaddress(0)` and pass it
  along with the current fiber pointer to `tin_spawn_fiber`, which
  retains the parent and stores both fields on the new fiber struct.
- `tin_spawn_fiber`'s C signature is the same in both modes so the
  runtime ABI doesn't change between builds.

The shadow-warning machinery (`compileTimeBuiltins`) already includes
`stacktrace`, so a local binding shadowing it produces the existing
`-Wbuiltin-shadow` diag.

## Concurrent and re-entrant calls

`stacktrace()` is safe to call from any fiber, from any OS thread,
re-entrantly, and concurrently with itself:

- The libunwind cursor and context are stack-local to
  `tin_capture_stacktrace`, so two simultaneous walks don't share
  state.
- `dladdr` is reentrant on every supported platform.
- The per-IP atom-code cache is `__thread`-local, so two threads have
  independent cache views. A thread that never calls stacktrace never
  allocates the 4 KiB TLS region.
- The global atom intern table (`_tin_learn_atom`) is mutex-protected.
  After cache warmup almost all stacktrace frames bypass this mutex
  entirely; only a cold IP requires the global lock briefly.
- The fiber-spawn-chain walk reads `spawn_parent` and `spawn_caller_ip`
  fields that are written exactly once at fiber creation and never
  mutated thereafter. Reads need no synchronisation.

The runtime's M:N scheduler uses cooperative yield points (`yield`,
`await`); libunwind walks complete entirely within a single fiber
quantum, so a fiber can't be preempted mid-walk and observe a
half-formed cursor. Document this assumption explicitly so a future
preemptive scheduler doesn't break stacktrace silently.

## Test plan

- `examples/stacktrace_smoke.tin` — main → A → B → stacktrace(). Assert
  the result is exactly `[B+offset, A+offset, _tin_user_main+offset, ...]`
  with no `<spawn-of>:` entries (no fibers involved).
- `examples/stacktrace_closure.tin` — closure invoked from a higher-order
  helper, calls stacktrace inside. Assert the trace shows the closure's
  generated symbol and the helper's symbol.
- `examples/stacktrace_fiber.tin` — main spawns A, A spawns B, B calls
  stacktrace. Assert the trace ends with two `<spawn-of>:` frames and
  terminates at main (not infinite-looping on a self-parent or similar).
- `examples/stacktrace_fiber_grandparent.tin` — three-deep spawn chain;
  verify the order of the `<spawn-of>:` entries matches the spawn order
  child → parent → grandparent.
- `examples/stacktrace_fiber_concurrent.tin` — N fibers in flight, each
  calling stacktrace. Assert each fiber's chain reflects ITS spawner,
  not whichever fiber's chain happened to be most recently captured (no
  cross-fiber leakage of TLS).
- `examples/stacktrace_unused_size.sh` — compile a program that does
  NOT call stacktrace, then a near-identical one that does, both at
  `-O2`. Assert the second binary is larger by a measurable margin
  (proves conditional unwind-table emission works). On Mach-O the
  margin will be smaller; the test asserts a per-platform threshold.
- `examples/stacktrace_unused_perf.sh` — run the existing pingpong
  benchmark from `bench/` against a tin built without stacktrace and
  with stacktrace. Without-stacktrace must match the established
  baseline within noise (~108ns/round-trip). This guards the "zero
  per-spawn cost when stacktrace is unused" property end-to-end.
- `examples/stacktrace_strip.sh` — strip the dynsym from a built binary
  and verify `stacktrace()` falls back to `??+0xADDR` cleanly.
- `examples/stacktrace_cap.tin` — `stacktrace(8)` returns at most 8
  entries; `stacktrace(0)` and `stacktrace(99999)` saturate to the
  clamped range without crashing.
- `examples/stacktrace_shared_lib.tin` — call into a `//!+helper.c`
  source compiled to a shared library, capture stacktrace inside the C
  fn. Assert the resulting atom has the `<libname>:` prefix while
  Tin-internal frames don't.
- `examples/stacktrace_ip_minus_one.tin` — construct a worst-case where
  the spawn call sits at the very last instruction of its enclosing fn
  (use `#no_inline` + tail layout). Without the IP-1 fix the resolver
  picks the next fn; with it, the enclosing fn name is reported. The
  test diffs the resolved name against the expected fn name.
- `examples/stacktrace_cache_perf.sh` — call `stacktrace()` 10000 times
  in a loop, time the second iteration onward. With the per-IP cache
  warm, total wall time should be well under N times the cold cost
  (target ratio: < 5x cold). Functional regression sentinel for the
  cache hit path.

## Risks

- **LLVM libunwind not always installed.** Distros split it
  inconsistently: Arch ships it as `llvm-libs`, Debian as `libunwind-dev`
  (which is GNU's, NOT what we want), macOS bundles it. Document the
  install requirement clearly. Possibly vendor the headers; the lib
  itself can come from the system clang's runtime dir.
- **Inlined frames invisible.** Same caveat as every native unwinder.
  Tag fns the user really wants visible with `#no_inline` (already
  exists).
- **`-rdynamic` increases dynamic symbol table size.** Only matters when
  stacktrace is enabled; default builds aren't affected. ~1-3% binary
  size increase on programs that DO use stacktrace, on top of the unwind
  tables.
- **Reachability detection is conservative.** A program that puts
  `stacktrace()` inside an `if false:` branch still triggers unwind-
  table emission. Acceptable: false positives only cost binary size,
  never correctness.
- **Spawn-record allocation lifetime.** A fiber's `spawn_parent` pointer
  must outlive the fiber. We pin parent fibers via the existing fiber
  RC: `tin_spawn_fiber` retains the parent on construction and the
  child's release path drops the reference — but ONLY when `caller_ip`
  is non-zero, i.e. when the spawn-site codegen actually populated it.
  Programs without `stacktrace()` pass NULL/0 and the retain branch is
  skipped entirely. ~10ns + one retain per spawn for stacktrace-using
  programs; literally zero for everyone else.

## Out of scope

- Walking another fiber's stack from a different fiber (introspection
  feature, distinct from `stacktrace()`'s "show MY current path").
- Windows / MSVC support.
- Symbolicating a stacktrace post-hoc against a separate `.dSYM` / `.dwp`
  in the runtime itself (user can pipe through `llvm-symbolizer`).
