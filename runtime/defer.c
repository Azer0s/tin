// tin runtime - defer chain, panic, and recover

#include "runtime.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Per-thread defer chain: newest entry first (LIFO).
// Thread-local so each fiber (OS thread) has its own independent defer stack.
__thread TinDeferEntry *_tin_defer_chain = NULL;

// Fiber panic interception
//
// The worker loop sets _fiber_panic_catching = true before calling _coro_resume.
// If _tin_panic() reaches the exit(1) path while catching is active, it stores
// the message in _fiber_panic_caught and returns instead of exiting.
// The worker loop reads _fiber_panic_caught after _coro_resume returns.

static __thread bool        _fiber_panic_catching = false;
static __thread const char *_fiber_panic_caught   = NULL;

void _tin_panic_catch_begin(void) {
    _fiber_panic_catching = true;
    _fiber_panic_caught   = NULL;
}

const char *_tin_panic_catch_end(void) {
    _fiber_panic_catching = false;
    const char *msg = _fiber_panic_caught;
    _fiber_panic_caught   = NULL;
    return msg;
}

// Called at every `defer` statement; entry points to a TinDeferEntry on the caller's stack
void _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *, void *), void *env, void *ret_slot) {
    entry->fn       = fn;
    entry->env      = env;
    entry->ret_slot = ret_slot;
    entry->prev     = _tin_defer_chain;
    _tin_defer_chain = entry;
}

// Called before a normal return to deregister the n most-recent defers (already run inline)
void _tin_defer_pop(int64_t n) {
    for (int64_t i = 0; i < n && _tin_defer_chain != NULL; i++)
        _tin_defer_chain = _tin_defer_chain->prev;
}

// recover() support: stores the current panic message so a deferred function
// can retrieve and clear it via _tin_recover().
// Must be thread-local: each worker thread executes one fiber at a time, and
// two fibers panicking concurrently on different workers must not corrupt each
// other's panic state.  A plain static here is a data race.
static __thread const char *_tin_panic_msg = NULL;

// Captured at panic time, before the defer chain unwinds.  Returned along
// with the message in the tuple form of `recover()` so a deferred function
// can pair the recovered text with the original call site.  Walking the
// FP chain inside the defer itself would resolve the defer's frame, not
// the panic's; the snapshot has to happen the moment `_tin_panic` is
// entered, before any cleanup runs.  Sized to match the format buffer
// `_tin_panic` already uses for its own dump.
#define TIN_PANIC_TRACE_CAP 64
static __thread int32_t _tin_panic_trace[TIN_PANIC_TRACE_CAP];
static __thread int32_t _tin_panic_trace_n = 0;

// An immortal empty string returned by _tin_recover() when not panicking.
// Layout matches TinRCHdr (16 bytes: { u32 rc; u32 flags; u64 _pad; })
// so that _tin_release(ptr) sees TIN_RC_IMMORTAL in flags and skips.
static const struct {
    uint32_t rc;
    uint32_t flags;
    uint64_t _pad;
    char     c;
} _tin_empty_str_sentinel = { 0, TIN_RC_IMMORTAL, 0, '\0' };

// Called from a deferred function to retrieve and clear the current panic message.
// Writes the recovered panic message into *out.  Out-param shape
// instead of a return value: the SRet shim path in codegen/extern.go
// has an ABI mismatch with clang 18's lowering on Linux x86_64 for
// >16-byte struct returns -- caller and callee disagree on where
// the result lands and the user sees a zeroed / garbage TinString
// out of recover().  Out-param routing dodges the entire issue.
//
// Writes an empty TinString (len=0, cap=-1) if not currently panicking.
void _tin_recover(TinString *out) {
    if (_tin_panic_msg == NULL) {
        out->ptr = (const char *)&_tin_empty_str_sentinel.c;
        out->len = 0;
        out->cap = -1;
        return;
    }
    *out = _tin_str_from_cstr(_tin_panic_msg);
    _tin_panic_msg = NULL;  // marks as recovered
}

// Companion to `_tin_recover()` used by the `recover('trace)` opt-in
// form.  Returns the panic-site stack trace captured when `_tin_panic`
// fired as an ARC-managed `[atom]` whose elements line up with what
// `stacktrace()` would have produced at the panic call site.  Empty
// `[atom]` when not panicking or when the build stripped the resolver
// (TIN_STACKTRACE off / no panic reachable in this binary).
//
// Caller order matters: invoke this BEFORE `_tin_recover()` -- recover
// clears `_tin_panic_msg`, after which we treat the panic as handled
// and the trace as stale.  The codegen builtin for `recover('trace)`
// emits both calls in the right order and packs the results into a
// `Tuple[string, [atom]]`.
// Out-param sibling to _tin_recover.  Same SRet-shim ABI workaround.
void _tin_recover_trace_atoms(TinAtomArray *out) {
    if (_tin_panic_msg == NULL || _tin_panic_trace_n == 0) {
        out->ptr = NULL;
        out->len = 0;
        out->cap = 0;
        return;
    }

    int32_t n = _tin_panic_trace_n;
    TinAtom *buf = (TinAtom *)_tin_rc_alloc((int64_t)n * (int64_t)sizeof(TinAtom));
    if (!buf) {
        out->ptr = NULL;
        out->len = 0;
        out->cap = 0;
        return;
    }

    for (int32_t i = 0; i < n; i++) {
        buf[i].code = _tin_panic_trace[i];
    }

    out->ptr = buf;
    out->len = (int64_t)n;
    out->cap = (int64_t)n;
}

// Run all pending deferred calls in LIFO order, print the panic message, then exit.
// If a deferred function calls recover(), _tin_panic_msg is cleared and we return.
//
// NOTE: panic-time stacktrace capture was removed -- the FP walk
// crashed on Linux x86_64 when called from setjmp-protected test
// runner frames.  As a result `recover('trace)` returns an empty
// atom array and the unrecovered-exit path prints just the panic
// message (no trace dump).  Re-introduce both behind a codegen-set
// runtime flag once we have a robust capture path.
void _tin_panic(const char *msg) {
    _tin_panic_msg = msg;
    _tin_panic_trace_n = 0;
    TinDeferEntry *e = _tin_defer_chain;
    _tin_defer_chain = NULL; // prevent re-entrant panic from re-running
    while (e != NULL) {
        TinDeferEntry *next = e->prev;
        if (e->fn) e->fn(e->env, e->ret_slot);
        e = next;
    }
    if (_tin_panic_msg == NULL) return;  // a deferred fn called recover()
    // If the worker loop has installed a fiber-level catch, store the message
    // and return instead of exiting.  The worker will re-raise or propagate.
    if (_fiber_panic_catching) {
        _fiber_panic_caught = _tin_panic_msg;
        _tin_panic_msg = NULL;
        return;
    }
    fflush(stdout);
    fprintf(stderr, "panic: %s\n", msg);
    exit(1);
}

void _tin_assert(int32_t cond, const char *msg) {
    if (!cond) _tin_panic(msg);
}

// _tin_main_err_exit prints "error: <msg>\n" to stderr and exits 1.
// Used by the C-main wrapper when fn main() Result[T, errors::Err]
// returns Err: the iface's message string is fetched on the Tin
// side, then forwarded to this helper which formats and exits.
void _tin_main_err_exit(const char *msg, int64_t len) {
    fflush(stdout);
    if (msg && len > 0) {
        fprintf(stderr, "error: %.*s\n", (int)len, msg);
    } else {
        fputs("error: <empty message>\n", stderr);
    }
    exit(1);
}
