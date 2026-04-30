// tin runtime - stacktrace capture (LLVM libunwind backed)
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
// libdwfl + libunwind/dladdr both resolve. Without line info: just
// "symbol+0x<offset>". Without symbol or line info: "??+0x<addr>".
// Frozen spawn frames carry a "<spawn-of>:" prefix.
//
// The whole libunwind/libdwfl/dladdr path is gated on TIN_STACKTRACE
// so programs that never call stacktrace() do not pull in the
// libunwind / libdw link dependencies. Phase 6 of
// docs/plans/stacktrace-libunwind.md sets this define from main.go
// conditional on cg.StacktraceUsed(), which in turn is fed by the
// AST pre-pass detectStacktraceUsage. Programs that don't use
// stacktrace() get the stub branch below; programs that do get the
// real walk and link with -lunwind/-ldw/-rdynamic.

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

#define UNW_LOCAL_ONLY
#include <libunwind.h>

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
#define TIN_ST_WALK_LIMIT 256  // hard upper bound on libunwind iterations

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
static char              *_tin_dwfl_dbgpath = NULL;
static pthread_mutex_t    _tin_dwfl_mu    = PTHREAD_MUTEX_INITIALIZER;
static atomic_int         _tin_dwfl_tried = 0;

static const Dwfl_Callbacks _tin_dwfl_cb = {
    .find_elf       = dwfl_linux_proc_find_elf,
    .find_debuginfo = dwfl_standard_find_debuginfo,
    .debuginfo_path = &_tin_dwfl_dbgpath,
};

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

static int32_t resolve_and_intern_cached(uintptr_t ip, int spawn_of,
                                          unw_cursor_t *cur);

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
//   1. libdwfl line + libunwind/dladdr symbol -> "symbol@file:line:col"
//   2. line only                              -> "file:line:col"
//   3. symbol only                            -> "<lib>:symbol+0x<off>" or "symbol+0x<off>"
//   4. neither                                -> "??+0x<addr>"
//
// Frozen spawn frames get a leading "<spawn-of>:" prefix so consumers
// can filter by either prefix.
//
// `cur` is non-NULL for live-stack frames — there libunwind can give
// us a symbol name as a fallback; spawn-of frames have no live cursor
// and rely on dladdr alone.
static int32_t resolve_frame(uintptr_t ip, int spawn_of, int32_t flags,
                              unw_cursor_t *cur) {
    char buf[TIN_ST_BUFSZ];
    const char *spawn_pfx = spawn_of ? "<spawn-of>:" : "";

    // Always pull lib path + symbol from dladdr; both feed the
    // filter decision and the atom string.
    Dl_info info;
    int have_dli = dladdr((void *)ip, &info);

    char         sym_buf[TIN_ST_BUFSZ];
    const char  *sym_name = NULL;
    unsigned long off = 0;

    if (cur != NULL) {
        unw_word_t uw_off = 0;
        if (unw_get_proc_name(cur, sym_buf, sizeof sym_buf, &uw_off) == 0
            && sym_buf[0] != '\0') {
            sym_name = sym_buf;
            off = (unsigned long)uw_off;
        }
    }
    if (sym_name == NULL && have_dli && info.dli_sname) {
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
static int32_t resolve_and_intern_cached(uintptr_t ip, int spawn_of,
                                          unw_cursor_t *cur) {
    uintptr_t key  = (ip << 1) | (uintptr_t)(spawn_of & 1);
    uint32_t  slot = (uint32_t)((key * 0x9E3779B97F4A7C15ULL) >> 56)
                     & (TIN_ST_CACHE_SLOTS - 1);
    // Empty slots have key=0 (BSS-initialised TLS). A real cache entry
    // can have atom_code=0 if _tin_learn_atom happens to compute a
    // CRC32 of 0 for some string — testing `atom_code != 0` would
    // re-resolve those forever. Test on key instead: a real cache key
    // is `(ip << 1) | spawn_of`, which is 0 only if ip == 0 (impossible
    // — libunwind would have failed earlier).
    if (_tin_st_cache[slot].key == key && key != 0) {
        return _tin_st_cache[slot].atom_code;
    }
    // No filtering at the cache layer: the cache always stores the
    // unfiltered atom. The caller pre-filters when flags are non-zero.
    int32_t code = resolve_frame(ip, spawn_of, /*flags=*/0, cur);
    _tin_st_cache[slot].key       = key;
    _tin_st_cache[slot].atom_code = code;
    return code;
}

int32_t tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags) {
    if (out == NULL) return 0;
    if (cap < TIN_ST_MIN_CAP) cap = TIN_ST_MIN_CAP;
    if (cap > TIN_ST_MAX_CAP) cap = TIN_ST_MAX_CAP;

    unw_context_t ctx;
    unw_cursor_t  cur;
    if (unw_getcontext(&ctx) < 0) return 0;
    if (unw_init_local(&cur, &ctx) < 0) return 0;

    // unw_init_local lands on tin_capture_stacktrace itself; one step
    // moves us to the caller (the user fn that invoked stacktrace()),
    // which is the first frame we want to emit.
    if (unw_step(&cur) <= 0) return 0;

    int32_t n = 0;
    int     walked = 0;
    do {
        if (n >= cap || walked >= TIN_ST_WALK_LIMIT) break;
        walked++;
        unw_word_t raw;
        if (unw_get_reg(&cur, UNW_REG_IP, &raw) < 0) break;

        // IP-minus-1 lands inside the call instruction itself, not
        // the return address that follows it.
        uintptr_t ip = (uintptr_t)raw - 1;

        // When filtering is active we re-resolve so frame_decision
        // can inspect the symbol/lib metadata; the cache only stores
        // the unfiltered atom, so a kept frame still benefits.
        int32_t code;
        if (flags != 0) {
            code = resolve_frame(ip, /*spawn_of=*/0, flags, &cur);
            if (code == 0) continue;  // filtered out
        } else {
            code = resolve_and_intern_cached(ip, /*spawn_of=*/0, &cur);
        }
        out[n++] = code;
    } while (unw_step(&cur) > 0);

    // Append the spawn chain. Each parent contributes exactly one
    // frame. Filtering applies the same way.
    uintptr_t ip; int64_t parent_pid, parent_gen;
    if (_tin_fiber_spawn_info(0, 0, &ip, &parent_pid, &parent_gen)) {
        while (ip != 0 && n < cap && walked < TIN_ST_WALK_LIMIT) {
            walked++;
            int32_t code;
            if (flags != 0) {
                code = resolve_frame(ip - 1, /*spawn_of=*/1, flags, NULL);
                if (code != 0) out[n++] = code;
            } else {
                out[n++] = resolve_and_intern_cached(ip - 1, /*spawn_of=*/1, NULL);
            }

            if (parent_pid == 0) break;
            if (!_tin_fiber_spawn_info(parent_pid, parent_gen,
                    &ip, &parent_pid, &parent_gen)) {
                break;
            }
        }
    }

    return n;
}

#else  // TIN_STACKTRACE

// Stub variant: linked into binaries that don't reference stacktrace().
int32_t tin_capture_stacktrace(int32_t *out, int32_t cap, int32_t flags) {
    (void)out; (void)cap; (void)flags;
    return 0;
}

#endif  // TIN_STACKTRACE
