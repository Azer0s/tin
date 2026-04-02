// tin runtime - umbrella
//
// This file pulls in all runtime sub-modules in dependency order.
// The sub-files are designed to be readable standalone (each includes runtime.h
// and its own system headers), but are compiled as a single translation unit
// via this umbrella so the build system only needs to reference one file.

#include "runtime.h"
#include <stdio.h>

// Make stdout line-buffered so echo output appears immediately even when
// stdout is connected to a pipe.  Line-buffering flushes on every '\n',
// which is how tin's echo statement terminates every value.
__attribute__((constructor)) static void _tin_stdout_init(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
}
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
