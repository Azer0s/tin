// tin runtime - umbrella
//
// This file pulls in all runtime sub-modules in dependency order.
// The sub-files are designed to be readable standalone (each includes runtime.h
// and its own system headers), but are compiled as a single translation unit
// via this umbrella so the build system only needs to reference one file.

// _GNU_SOURCE must be defined BEFORE any system header is pulled in by an
// included sub-module, otherwise glibc's feature-test macros leave Dl_info
// (used by stacktrace.c) and other GNU extensions undeclared. Runtime.h
// itself already includes <stdint.h>; defining the macro at the very top
// keeps both glibc and musl happy regardless of include order.
#define _GNU_SOURCE

#include "runtime.h"
#include <stdio.h>
#include <signal.h>

// Pull in every system header that declares the allocator family
// BEFORE the mimalloc macro shim activates below.  Otherwise our
// #define malloc mi_malloc-style substitutions would textually
// rewrite the system declarations themselves (e.g. <malloc/_malloc.h>'s
// `void *malloc(size_t)` becomes `void *mi_malloc(...)` which collides
// with the real prototype).  These includes are guarded internally so
// the order is purely about declaration visibility.
#include <stdlib.h>
#include <string.h>

// mimalloc is linked unconditionally (the build path errors loudly
// when libmimalloc cannot be located -- there is no silent libc
// fallback).  The header has to be available for arc.c's
// _tin_arena_alloc / mi_free calls and for heap_arena.c's arena
// reservation.
//
// We deliberately do NOT macro-substitute the generic malloc/free/...
// calls in this translation unit anymore.  Earlier sessions ran with
// a blanket #define malloc mi_malloc that routed every runtime
// allocation through Tin's arena -- including channel slots, fiber
// records, fastmutex slabs.  That broke the invariant
// _tin_is_managed depends on: with non-rc allocations sharing the
// arena range, the provenance check could not distinguish rc-blocks
// from anything else, and `*T` retain/release through
// _tin_retain_ptr would happily dereference the bytes above a pipe
// buffer as a fake TinRCHdr.  Today arc.c calls mi_heap_malloc /
// mi_free directly for rc-blocks (via _tin_arena_alloc /
// _tin_arena_free) and every other runtime allocation uses libc
// malloc/free as it appears in source.
#if TIN_USE_MIMALLOC
#  include <mimalloc.h>
#endif

// Make stdout line-buffered so echo output appears immediately even when
// stdout is connected to a pipe.  Line-buffering flushes on every '\n',
// which is how tin's echo statement terminates every value.
__attribute__((constructor)) static void _tin_stdout_init(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
}

// Ignore SIGPIPE process-wide. Without this, writing to a closed socket
// (a peer that disconnected mid-conversation) sends SIGPIPE to the
// process and the default disposition kills it. Tin async writes return
// -EPIPE to the caller via the normal error path; the runtime handles
// that, but only if SIGPIPE doesn't reach the default handler first.
// Echo-server stress tests under concurrent CI runners reliably hit
// this when fragmented sends race the peer's close.
__attribute__((constructor)) static void _tin_sigpipe_init(void) {
    struct sigaction sa = {0};
    sa.sa_handler = SIG_IGN;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGPIPE, &sa, NULL);
}
#include "heap_arena.c" // initializes Tin arena; exposes _tin_managed_heap
#include "arc.c"
#include "strings.c"   // uses _tin_rc_alloc (arc)
#include "slice.c"     // uses _tin_rc_alloc (arc)
#include "echo.c"
#include "mem.c"
#include "defer.c"     // uses _tin_str_from_cstr (strings)
#include "len.c"
#include "test.c"
#include "atom.c"
#include "any.c"
#include "fiber.c"     // M:N fiber scheduler (TINMAXPROCS worker threads)
#include "fastmutex.c" // TinFastMutex: atomic spinlock + coro wait queue
#include "async_io.c"  // epoll/kqueue async I/O (dedicated I/O thread)
#include "timer.c"     // fiber sleep / timer support
#include "net.c"       // TCP socket helpers
#include "time.c"      // _tin_now_ms / _tin_now_us monotonic clock
#include "interop.c"   // C-callable boundary helpers (#interop wrappers)
#include "interop_trampoline.c"  // mmap'd per-instance closure trampolines
#include "pclntab.c"   // PC -> file:line:col table (replaces libdw / DWARF)
#include "stacktrace.c" // stacktrace() builtin support (FP walker + pclntab)
#include "reflect_table.c" // link-time `tin_impl` walker (D1)
