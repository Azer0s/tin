// tin runtime - stacktrace capture (frame-pointer walker)
//
// API contract:
//   int32_t tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags)
//     - writes up to `cap` interned atom codes into `out`
//     - `flags` is a bitfield of TIN_ST_HIDE_* constants (runtime.h);
//       matching frames are dropped before `cap` is applied
//     - returns the number actually written
//     - never panics; on total failure returns 0
//
// Atom format: each frame renders as "symbol@file:line:col" when
// libdwfl + dladdr both resolve. Without line info: just
// "symbol+0x<offset>". Without symbol or line info: "??+0x<addr>".
// Frozen spawn frames carry a "<spawn-of>:" prefix.
//
// Walking strategy: we follow the frame-pointer chain (rbp on x86_64,
// x29 on aarch64). Codegen emits `frame-pointer="all"` on every IR
// function (see codegen/codegen.go applyStacktracePostPass), so every
// Tin frame has a valid `[fp+0] = saved_fp, [fp+8] = return_ip`
// layout. This avoids libunwind entirely - libunwind 1.8.x ships with
// CONSERVATIVE_CHECKS=1 baked in, which probes memory readability via
// `syscall(SYS_write, pipe_fd, addr, 1)`; valgrind flags those as
// reads of unaddressable bytes (the writeable end of the probe pipe is
// the kernel, not us, so the bytes are conceptually fine but the
// instrumentation can't tell).
//
// Trade-off (FP walking vs libunwind .eh_frame walking): every frame
// in the chain MUST preserve the frame pointer. Tin frames do (codegen
// post-pass). The Tin runtime C does (main.go passes
// -fno-omit-frame-pointer when stacktraceLinkActive). User C code
// reachable via #interop callbacks does NOT by default - if a Tin
// stacktrace passes through user C code that was built with the usual
// `-O2` (which omits frame pointers on x86_64 Linux), the walk
// terminates at the first such frame and the chain truncates. Users
// who want stacktrace() to walk through their own C must build that C
// with `-fno-omit-frame-pointer`. libunwind's .eh_frame walking would
// have handled this transparently, but the valgrind issue above made
// it impractical.
//
// The whole libdwfl/dladdr path is gated on TIN_STACKTRACE so programs
// that never call stacktrace() do not pull in the libdw link
// dependency. Phase 6 of docs/plans/stacktrace-libunwind.md sets this
// define from main.go conditional on cg.StacktraceUsed(), which in
// turn is fed by the AST pre-pass detectStacktraceUsage. Programs
// that don't use stacktrace() get the stub branch below; programs
// that do get the real walk and link with -ldw/-rdynamic.

#include "runtime.h"

#ifdef TIN_STACKTRACE

// _GNU_SOURCE is defined in runtime.c (the umbrella) before any include,
// so dlfcn.h surfaces Dl_info / dladdr regardless of include order here.
#include <dlfcn.h>
#include <stdio.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>
#include <pthread.h>

// libdwfl (elfutils) is Linux/FreeBSD-only. macOS ships no equivalent;
// when targeting Darwin we drop the line-info pass and fall back to
// dladdr-only resolution ("symbol+0x<off>", no source coords).
#if defined(__linux__) || defined(__FreeBSD__)
#  define TIN_ST_HAVE_LIBDW 1
#  include <elfutils/libdwfl.h>
#endif

// Per-frame buffer for the formatted "symbol@file:line:col" /
// "<sym>+0x<off>" atom name. 512 bytes is overkill for typical paths
// and symbols (the longest path in a Tin source tree we've measured
// is ~200 chars); the extra slack absorbs deeply-nested workspaces
// without truncation. snprintf guarantees NUL termination, so silent
// truncation is the worst case if some pathological path blows past
// the limit.
#define TIN_ST_BUFSZ 512

// Cap clamping bounds for tin_capture_stacktrace's `cap` argument.
// _MIN ensures we always have at least one slot for the unwind to land
// in; _MAX bounds the per-call work and the IR-level stack alloc that
// codegen emits so a bogus user value can't request a megabyte of
// stack. The walk itself can step further (up to TIN_ST_WALK_LIMIT)
// when filtering is active so dropped frames don't eat the user's cap.
#define TIN_ST_MIN_CAP    1
#define TIN_ST_MAX_CAP    1024
#define TIN_ST_WALK_LIMIT 256  // hard upper bound on fp_walk iterations

// Per-IP atom-code cache. Without this, a hot stacktrace path would hit
// _tin_learn_atom (process-wide mutex + linked-list scan) once per frame
// per call. The cache is `__thread`-local so two stacktraces on
// different threads can populate independently and never share a mutex
// after warmup.
//
// Key encoding: (ip << 1) | spawn_of_bit. The same IP can appear once
// as a live frame and once as a <spawn-of>: frame in different traces;
// the bit keeps those distinct. ip << 1 is safe because instruction
// addresses on every supported arch align to at least 4 bytes (top bit
// is always free).
//
// Note: the cache is keyed only on (ip, spawn_of) and ignores filter
// flags. Filtering happens BEFORE the cache lookup (we decide whether
// to keep a frame, then resolve its atom), so two calls with different
// flag sets share cached atom codes for the same IPs they both keep.
//
// Direct-mapped open-addressed table with 256 slots: 16-byte entries
// give a 4 KiB TLS region per thread that ever calls stacktrace.
// Threads that never call stacktrace never touch this region (lazy
// zero-page mapping in glibc). Hash collisions overwrite — a
// pathological access pattern thrashes but never corrupts.
//
// Hash: 64-bit golden ratio (Knuth's multiplicative). Top byte selected
// because the multiply spreads entropy uniformly across the upper bits.
#define TIN_ST_CACHE_SLOTS 256
static __thread struct {
    uintptr_t key;       // (ip << 1) | spawn_of, or 0 for empty
    int32_t   atom_code;
} _tin_st_cache[TIN_ST_CACHE_SLOTS];

// fp_get reads the current frame pointer (rbp on x86_64, x29 on
// aarch64). Marked always_inline so the value seen is the caller's
// frame, not fp_get's own; if the compiler ever decides not to inline
// despite the hint, the result still lands at fp_get itself which
// fp_walk's first iteration discards harmlessly.
#if defined(__x86_64__)
static __attribute__((always_inline)) inline uintptr_t fp_get(void) {
    uintptr_t fp;
    __asm__ volatile ("movq %%rbp, %0" : "=r"(fp));
    return fp;
}
#elif defined(__aarch64__)
static __attribute__((always_inline)) inline uintptr_t fp_get(void) {
    uintptr_t fp;
    __asm__ volatile ("mov %0, x29" : "=r"(fp));
    return fp;
}
#else
static inline uintptr_t fp_get(void) { return 0; }
#endif

// thread_stack_bounds yields the [lo, hi) address range of the current
// thread's stack. Used by fp_walk to detect a chain that wandered off
// into a foreign mapping (a return address overwritten by a buffer
// overrun, an alloca that smashed an old fp, etc) before we dereference
// garbage. Returns 0 on failure - fp_walk falls back to a "trust the
// chain until it hits 0 or fails an alignment check" mode.
//
// macOS exposes pthread_get_stackaddr_np / pthread_get_stacksize_np
// directly; glibc / musl require pthread_getattr_np + pthread_attr_getstack.
// pthread_get_stackaddr_np returns the highest address (top of the
// stack, which grows down), so we subtract size to get the low end.
static int thread_stack_bounds(uintptr_t *lo, uintptr_t *hi) {
#if defined(__APPLE__)
    void *top = pthread_get_stackaddr_np(pthread_self());
    size_t size = pthread_get_stacksize_np(pthread_self());
    if (top == NULL || size == 0) return 0;
    *hi = (uintptr_t)top;
    *lo = *hi - size;
    return 1;
#else
    pthread_attr_t attr;
    if (pthread_getattr_np(pthread_self(), &attr) != 0) return 0;
    void  *base = NULL;
    size_t size = 0;
    int    ok   = (pthread_attr_getstack(&attr, &base, &size) == 0);
    pthread_attr_destroy(&attr);
    if (!ok || base == NULL || size == 0) return 0;
    *lo = (uintptr_t)base;
    *hi = (uintptr_t)base + size;
    return 1;
#endif
}

// fp_walk traverses the frame-pointer chain starting at `start_fp`
// and writes up to `cap` return addresses into `out`. Each frame's
// layout is `[fp+0] = saved_caller_fp, [fp+sizeof(void*)] = ret_ip` -
// this is the AAPCS64 convention on aarch64 and the
// frame-pointer-preserving convention on x86_64 (`-fno-omit-frame-pointer`
// or LLVM's "frame-pointer"="all" attribute, which codegen always
// emits when stacktrace is in use).
//
// Tail calls: with `frame-pointer="all"`, LLVM emits the rbp/x29
// pop (or `ldp x29,x30,[sp]`) BEFORE the tail `jmp`/`b`, so the
// tail-caller's frame is properly torn down. The chain therefore
// skips tail-call ancestors and reports the tail-callee's frame
// linked directly to its grand-caller. That's the expected
// behaviour of any FP-based unwinder; we just document it here so
// "where did the intermediate frame go?" has an answer.
//
// The walk stops on any of:
//   - reached cap (caller's responsibility, we just bound by it)
//   - fp == 0 (chain terminator on every supported ABI)
//   - fp misaligned (corruption / wrong ABI guess)
//   - fp outside [lo, hi) when bounds were available
//   - fp didn't grow monotonically up (corruption / fp re-use)
//
// Stack bounds are queried once per call. Multi-fiber programs
// switch threads under us during a single call only if a fiber
// yields mid-resolution, which we don't; so the bounds stay valid
// for the duration of fp_walk.
static int fp_walk(uintptr_t start_fp, uintptr_t *out, int cap) {
    uintptr_t lo = 0, hi = (uintptr_t)-1;
    int have_bounds = thread_stack_bounds(&lo, &hi);

    int       n       = 0;
    uintptr_t fp      = start_fp;
    uintptr_t prev_fp = 0;
    while (n < cap) {
        if (fp == 0) break;
        if (fp & (sizeof(void *) - 1)) break;
        if (have_bounds && (fp < lo || fp + 2 * sizeof(void *) > hi)) break;
        // Frame pointers walk toward higher addresses (stack grows
        // down, callers live above callees). A non-monotonic chain
        // means we've fallen out of valid frames and would loop or
        // wander into garbage.
        if (prev_fp != 0 && fp <= prev_fp) break;

        uintptr_t next_fp = ((uintptr_t *)fp)[0];
        uintptr_t ret_ip  = ((uintptr_t *)fp)[1];
        if (ret_ip == 0) break;
        out[n++] = ret_ip;
        prev_fp = fp;
        fp      = next_fp;
    }
    return n;
}

#ifdef TIN_ST_HAVE_LIBDW

// libdwfl session, lazily initialised on first stacktrace call.
// dwfl_begin / dwfl_linux_proc_report walk /proc/self/maps to discover
// every loaded module and their .debug_line tables. The session itself
// is process-wide and read-only after initialisation, so we share it
// across threads with a mutex around the (rare) init step.
//
// Concurrency: the fast path reads `_tin_dwfl_tried` with acquire
// semantics, paired with a release store on the slow path AFTER
// `_tin_dwfl` is published. Without the acquire/release pair, a
// reader on a weakly-ordered architecture (AArch64) could observe
// `_tin_dwfl_tried == 1` while still seeing a stale `_tin_dwfl ==
// NULL`, dropping every line-info lookup on that thread.
//
// stdatomic.h gives us the proper relaxed-acq-rel pair without
// needing a mutex on the hot path.
//
// Known limitation: the dwfl session is a one-shot snapshot of
// /proc/self/maps. Modules loaded later via `dlopen` don't get a
// `.debug_line` lookup; their frames render with symbol+offset only.
// Modules unloaded with `dlclose` AFTER first stacktrace call leave
// stale entries in the per-IP atom cache, which can produce wrong
// resolutions for IPs in addresses since reused. Tin's runtime
// largely doesn't dlclose, but the CTFE per-fn cache occasionally
// does. Acknowledged as out-of-scope for now.
#include <stdatomic.h>

static Dwfl              *_tin_dwfl       = NULL;
static pthread_mutex_t    _tin_dwfl_mu    = PTHREAD_MUTEX_INITIALIZER;
static atomic_int         _tin_dwfl_tried = 0;

// find_debuginfo_no_debuginfod replaces dwfl_standard_find_debuginfo so
// that libdwfl never falls through to debuginfod for modules without
// local .debug data (typically stripped libc / libpthread). The
// standard callback's debuginfod fallback dlopens libdebuginfod.so.1,
// whose `debuginfod_begin` unconditionally calls `curl_global_init`,
// which in turn pulls in libcurl -> ngtcp2 -> libssl/libcrypto plus a
// pile of OpenSSL provider state. Those allocations are intentionally
// leaked by their respective libraries (no public destructor) and
// surface under valgrind as ~40 KiB of still-reachable blocks (23
// distinct loss records).
//
// We don't need network-backed debug info: Tin's own frames have
// .debug_line embedded directly via -gline-tables-only, and libc
// frames degrade gracefully to "symbol+0x<off>" without it. Returning
// -1 immediately tells libdwfl "no separate .debug file available";
// the embedded debug data is still returned from .debug_line lookups
// in the main module.
static int find_debuginfo_no_debuginfod(Dwfl_Module *mod, void **userdata,
        const char *modname, Dwarf_Addr base, const char *file_name,
        const char *debuglink_file, GElf_Word debuglink_crc,
        char **debuginfo_file_name) {
    (void)mod; (void)userdata; (void)modname; (void)base;
    (void)file_name; (void)debuglink_file; (void)debuglink_crc;
    (void)debuginfo_file_name;
    return -1;
}

static const Dwfl_Callbacks _tin_dwfl_cb = {
    .find_elf       = dwfl_linux_proc_find_elf,
    .find_debuginfo = find_debuginfo_no_debuginfod,
    .debuginfo_path = NULL,
};

// dwfl_session_atexit calls dwfl_end on the process-wide libdwfl session at
// program shutdown. libdw caches DIE units in a tsearch tree (per-CU,
// allocated lazily by __libdw_intern_next_unit) that lives on the heap with
// no public destructor of its own; dwfl_end is the only documented way to
// flush them. Without this hook valgrind reports 100s-1000s of bytes
// "possibly lost" inside libdw_findcu/dwarf_get_units for any program that
// resolved a single stacktrace frame.
static void dwfl_session_atexit(void) {
    pthread_mutex_lock(&_tin_dwfl_mu);
    if (_tin_dwfl != NULL) {
        dwfl_end(_tin_dwfl);
        _tin_dwfl = NULL;
    }
    pthread_mutex_unlock(&_tin_dwfl_mu);
}

static Dwfl *dwfl_session(void) {
    if (atomic_load_explicit(&_tin_dwfl_tried, memory_order_acquire)) {
        return _tin_dwfl;
    }

    pthread_mutex_lock(&_tin_dwfl_mu);
    if (!atomic_load_explicit(&_tin_dwfl_tried, memory_order_relaxed)) {
        Dwfl *d = dwfl_begin(&_tin_dwfl_cb);
        if (d != NULL) {
            if (dwfl_linux_proc_report(d, getpid()) != 0
                || dwfl_report_end(d, NULL, NULL) != 0) {
                dwfl_end(d);
                d = NULL;
            }
        }
        if (d != NULL) {
            // Register the teardown hook on first successful init only; atexit
            // is process-wide so a second registration would call dwfl_end
            // twice. Failure to register is non-fatal: we'd lose the libdw
            // cache flush at shutdown but resolution still works.
            (void)atexit(dwfl_session_atexit);
        }
        // Publish _tin_dwfl FIRST, then release-store the flag so any
        // reader that observes _tried == 1 also observes the d store.
        _tin_dwfl = d;
        atomic_store_explicit(&_tin_dwfl_tried, 1, memory_order_release);
    }
    pthread_mutex_unlock(&_tin_dwfl_mu);
    return _tin_dwfl;
}

#else  // TIN_ST_HAVE_LIBDW

// Stub for platforms without elfutils (macOS). Stacktrace falls back
// to dladdr-only resolution; resolve_frame's libdwfl branch never
// produces a hit so the format degrades to "symbol+0x<off>".
static void *dwfl_session(void) { return NULL; }

#endif  // TIN_ST_HAVE_LIBDW

static int32_t resolve_and_intern_cached(uintptr_t ip, int spawn_of);

// is_shared_lib_path classifies dli_fname as belonging to a shared lib
// (.so on Linux/BSD, .dylib on macOS, versioned ".so." anywhere) vs the
// main binary. Used by the dladdr fallback path when libdwfl can't
// resolve an IP; libdw / dwfl don't surface "is this the executable"
// directly, so we detect by extension.
static int is_shared_lib_path(const char *path) {
    if (!path) return 0;
    size_t len = strlen(path);
    if (len >= 3 && memcmp(path + len - 3, ".so", 3) == 0) return 1;
    if (len >= 6 && memcmp(path + len - 6, ".dylib", 6) == 0) return 1;
    return strstr(path, ".so.") != NULL;
}

// is_libc_path is a more-specific match used by TIN_ST_HIDE_LIBC.
// libc on glibc and musl all match libc.so.* / libpthread.so.* /
// libsystem.* (macOS); on Apple platforms libsystem_*. The check is
// conservative: false positives only mean we hide something the user
// might have wanted to see. Mismatches in either direction can be
// addressed by extending this list later.
static int is_libc_module(const char *path) {
    if (!path) return 0;
    const char *base = strrchr(path, '/');
    base = base ? base + 1 : path;
    if (strncmp(base, "libc.so",      7) == 0) return 1;  // glibc
    if (strncmp(base, "libc.musl",    9) == 0) return 1;  // musl shared
    if (strncmp(base, "libpthread.so", 13) == 0) return 1;
    if (strncmp(base, "libdl.so",     8) == 0) return 1;
    if (strncmp(base, "libsystem_",  10) == 0) return 1;  // macOS
    if (strncmp(base, "ld-linux",     8) == 0) return 1;  // glibc loader
    if (strncmp(base, "ld-musl",      7) == 0) return 1;  // musl loader
    if (strncmp(base, "ld.so",        5) == 0) return 1;
    return 0;
}

// is_main_entry checks whether sym is a program-entry frame that the
// HIDE_MAIN flag should drop. Covers main() itself plus the libc
// startup machinery and the kernel-side _start trampoline. The list is
// intentionally narrow: we only drop frames a user almost never wants
// to see at the bottom of every trace.
static int is_main_entry(const char *sym) {
    if (!sym) return 0;
    if (strcmp(sym, "main")    == 0) return 1;
    if (strcmp(sym, "_start")  == 0) return 1;
    if (strncmp(sym, "__libc_start", 12) == 0) return 1;
    return 0;
}

// frame_decision classifies one resolved frame against the active flag
// mask. Returns 1 if the frame should be kept, 0 if filtered out.
//
// `lib_path` is dli_fname (or NULL). `sym` is the resolved symbol name
// (or NULL when only address is known). `unresolved` is set when both
// libdwfl and dladdr came up empty — i.e. the frame would render as
// "??+0x<addr>".
static int frame_decision(int32_t flags, const char *lib_path,
                          const char *sym, int unresolved) {
    if ((flags & TIN_ST_HIDE_UNKNOWN) && unresolved) return 0;
    if ((flags & TIN_ST_HIDE_LIBC) && is_libc_module(lib_path)) return 0;
    if ((flags & TIN_ST_HIDE_RUNTIME) && sym
        && strncmp(sym, "_tin_", 5) == 0
        // Tin codegen renames the user's `fn main` to `_tin_user_main`
        // (avoids collision with the C-side entry point). It's USER
        // code, not runtime — never hide it under HIDE_RUNTIME or the
        // user loses the root frame of every filtered trace.
        && strcmp(sym, "_tin_user_main") != 0) return 0;
    if ((flags & TIN_ST_HIDE_MAIN) && is_main_entry(sym)) return 0;
    return 1;
}

// resolve_frame formats one IP into an atom name and returns its
// interned code via _tin_learn_atom. Returns 0 (no atom) when the
// frame is filtered out by `flags`, signalling to the caller that it
// should not write this frame.
//
// Resolution priority for the atom name:
//   1. libdwfl line + dladdr symbol -> "symbol@file:line:col"
//   2. line only                    -> "file:line:col"
//   3. symbol only                  -> "<lib>:symbol+0x<off>" or "symbol+0x<off>"
//   4. neither                      -> "??+0x<addr>"
//
// Frozen spawn frames get a leading "<spawn-of>:" prefix so consumers
// can filter by either prefix.
//
// Symbol resolution is dladdr-only since dropping libunwind. main.go
// links the binary with `-rdynamic`, which copies all defined symbols
// into .dynsym, so dladdr finds user functions and tin_test_*
// trampolines just as well as libunwind's _Uelf64_get_proc_name did.
static int32_t resolve_frame(uintptr_t ip, int spawn_of, int32_t flags) {
    char buf[TIN_ST_BUFSZ];
    const char *spawn_pfx = spawn_of ? "<spawn-of>:" : "";

    Dl_info info;
    int have_dli = dladdr((void *)ip, &info);

    const char   *sym_name = NULL;
    unsigned long off      = 0;
    if (have_dli && info.dli_sname) {
        sym_name = info.dli_sname;
        off = (unsigned long)ip - (unsigned long)info.dli_saddr;
    }

    // libdwfl line lookup (may produce file:line:col). Only available
    // on platforms with elfutils (see TIN_ST_HAVE_LIBDW above); on
    // macOS this branch is compiled out and `src` stays NULL so the
    // formatter falls through to the symbol+offset path.
    const char *src    = NULL;
    int         lineno = 0;
    int         colno  = 0;
#ifdef TIN_ST_HAVE_LIBDW
    Dwfl *d = dwfl_session();
    if (d != NULL) {
        Dwfl_Module *mod  = dwfl_addrmodule(d, (Dwarf_Addr)ip);
        Dwfl_Line   *line = mod ? dwfl_module_getsrc(mod, (Dwarf_Addr)ip) : NULL;
        if (line) {
            src = dwfl_lineinfo(line, NULL, &lineno, &colno, NULL, NULL);
        }
    }
#endif

    // Decide whether to keep this frame given the active filters.
    int unresolved = (sym_name == NULL && src == NULL);
    if (flags != 0 && !frame_decision(flags,
            have_dli ? info.dli_fname : NULL, sym_name, unresolved)) {
        return 0;
    }

    // Format the atom according to resolution coverage. Atoms whose
    // name contains a colon or @ route through the lexer's "complex
    // atom" branch, so they need surrounding quotes for round-trip
    // equality with hand-written '"name"' literals.
    int written;
    if (sym_name && src) {
        written = snprintf(buf, sizeof buf, "\"%s%s@%s:%d:%d\"",
            spawn_pfx, sym_name, src, lineno, colno);
    } else if (src) {
        written = snprintf(buf, sizeof buf, "\"%s%s:%d:%d\"",
            spawn_pfx, src, lineno, colno);
    } else if (sym_name) {
        const int in_lib = have_dli && is_shared_lib_path(info.dli_fname);
        if (in_lib) {
            const char *base = strrchr(info.dli_fname, '/');
            const char *lib  = base ? base + 1 : info.dli_fname;
            if (off == 0) {
                written = snprintf(buf, sizeof buf, "\"%s%s:%s\"",
                    spawn_pfx, lib, sym_name);
            } else {
                written = snprintf(buf, sizeof buf, "\"%s%s:%s+0x%lx\"",
                    spawn_pfx, lib, sym_name, off);
            }
        } else if (off == 0 && !spawn_of) {
            written = snprintf(buf, sizeof buf, "%s", sym_name);
        } else {
            written = snprintf(buf, sizeof buf, "\"%s%s+0x%lx\"",
                spawn_pfx, sym_name, off);
        }
    } else {
        written = snprintf(buf, sizeof buf, "\"%s??+0x%lx\"",
            spawn_pfx, (unsigned long)ip);
    }

    // Truncation defence: deeply mangled C++ symbols can blow past
    // BUFSZ, and two distinct frames whose first BUFSZ bytes match
    // would otherwise dedupe to a single atom (different IPs, same
    // truncated name, same atom code). Append the raw IP as a unique
    // disambiguator. Reserve ~24 bytes at the tail for the suffix.
    if (written >= (int)sizeof buf) {
        const int tail = 28; // "@@0x<16 hex>\"" + '\0' headroom
        int prefix = (int)sizeof buf - tail;
        if (prefix < 0) prefix = 0;
        snprintf(buf + prefix, tail, "@@0x%lx\"", (unsigned long)ip);
    }

    return _tin_learn_atom(buf);
}

// resolve_and_intern_cached wraps resolve_frame with a per-IP TLS
// memoisation cache so repeated stacktraces in a hot path don't keep
// hitting the global atom mutex. Cache misses fall through to the
// uncached resolver and store the result for next time.
//
// `flags` is consulted: a cached entry is reused only when the filter
// decision would also keep this frame. We store atom_code=0 to mean
// "filtered" so a flag-driven drop persists across calls with the
// same flags. Calls with different flag sets re-resolve naturally
// because they hit a different runtime path before consulting the
// cache (the filter decision is keyed off the resolved frame, not the
// cached atom).
static int32_t resolve_and_intern_cached(uintptr_t ip, int spawn_of) {
    uintptr_t key  = (ip << 1) | (uintptr_t)(spawn_of & 1);
    uint32_t  slot = (uint32_t)((key * 0x9E3779B97F4A7C15ULL) >> 56)
                     & (TIN_ST_CACHE_SLOTS - 1);
    // Empty slots have key=0 (BSS-initialised TLS). A real cache entry
    // can have atom_code=0 if _tin_learn_atom happens to compute a
    // CRC32 of 0 for some string — testing `atom_code != 0` would
    // re-resolve those forever. Test on key instead: a real cache key
    // is `(ip << 1) | spawn_of`, which is 0 only if ip == 0 (impossible
    // - fp_walk filters those out before calling).
    if (_tin_st_cache[slot].key == key && key != 0) {
        return _tin_st_cache[slot].atom_code;
    }
    // No filtering at the cache layer: the cache always stores the
    // unfiltered atom. The caller pre-filters when flags are non-zero.
    int32_t code = resolve_frame(ip, spawn_of, /*flags=*/0);
    _tin_st_cache[slot].key       = key;
    _tin_st_cache[slot].atom_code = code;
    return code;
}

int32_t tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags) {
    if (out == NULL) return 0;
    if (cap < TIN_ST_MIN_CAP) cap = TIN_ST_MIN_CAP;
    if (cap > TIN_ST_MAX_CAP) cap = TIN_ST_MAX_CAP;

#if !defined(__x86_64__) && !defined(__aarch64__)
    // No FP-walker on this arch (32-bit ARM, RISC-V, etc).
    // Stacktrace returns empty; the rest of the runtime keeps working.
    (void)out; (void)flags;
    return 0;
#else
    // Buffer the raw IPs first, then resolve. We over-walk past `cap`
    // when filtering is active so dropped frames don't eat the user's
    // requested count, but never past TIN_ST_WALK_LIMIT.
    uintptr_t ips[TIN_ST_WALK_LIMIT];
    int       walk_cap = cap > TIN_ST_WALK_LIMIT ? TIN_ST_WALK_LIMIT : cap;
    if (flags != 0) walk_cap = TIN_ST_WALK_LIMIT;  // keep walking past filtered frames

    // fp_walk reads `[my_fp+sizeof(void*)]` which is the return
    // address THIS function will return to - i.e. an instruction
    // inside our caller (the user code that wrote stacktrace()).
    // So ips[0] is already the first user-visible frame; no skip
    // needed. The same holds with the always_inline wrapper around
    // stacktrace(): codegen lowers it to a direct call to
    // tin_capture_stacktrace, so the caller's frame is the user fn.
    int raw_n = fp_walk(fp_get(), ips, walk_cap);

    int32_t n = 0;
    for (int i = 0; i < raw_n && n < cap; i++) {
        // IP-minus-1 lands inside the call instruction itself, not
        // the return address that follows it. dladdr+libdwfl both
        // tolerate either, but the inside-call form gives the
        // correct line number for the call site.
        uintptr_t ip = ips[i] - 1;
        int32_t   code;
        if (flags != 0) {
            code = resolve_frame(ip, /*spawn_of=*/0, flags);
            if (code == 0) continue;
        } else {
            code = resolve_and_intern_cached(ip, /*spawn_of=*/0);
        }
        out[n++] = code;
    }

    // Append the spawn chain. Each parent contributes exactly one
    // frame. Filtering applies the same way.
    uintptr_t ip; int64_t parent_pid, parent_gen;
    if (_tin_fiber_spawn_info(0, 0, &ip, &parent_pid, &parent_gen)) {
        int walked = 0;
        while (ip != 0 && n < cap && walked < TIN_ST_WALK_LIMIT) {
            walked++;
            int32_t code;
            if (flags != 0) {
                code = resolve_frame(ip - 1, /*spawn_of=*/1, flags);
                if (code != 0) out[n++] = code;
            } else {
                out[n++] = resolve_and_intern_cached(ip - 1, /*spawn_of=*/1);
            }

            if (parent_pid == 0) break;
            if (!_tin_fiber_spawn_info(parent_pid, parent_gen,
                    &ip, &parent_pid, &parent_gen)) {
                break;
            }
        }
    }

    return n;
#endif
}

#else  // TIN_STACKTRACE

// Stub variant: linked into binaries that don't reference stacktrace().
int32_t tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags) {
    (void)out; (void)cap; (void)flags;
    return 0;
}

#endif  // TIN_STACKTRACE
