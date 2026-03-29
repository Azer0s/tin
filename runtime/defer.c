// tin runtime - defer chain, panic, and recover

#include "runtime.h"
#include <stdio.h>
#include <stdlib.h>

// Per-thread defer chain: newest entry first (LIFO).
// Thread-local so each fiber (OS thread) has its own independent defer stack.
__thread TinDeferEntry *_tin_defer_chain = NULL;

// ---------------------------------------------------------------------------
// Fiber panic interception
//
// The worker loop sets _fiber_panic_catching = true before calling _coro_resume.
// If _tin_panic() reaches the exit(1) path while catching is active, it stores
// the message in _fiber_panic_caught and returns instead of exiting.
// The worker loop reads _fiber_panic_caught after _coro_resume returns.
// ---------------------------------------------------------------------------

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
static const char *_tin_panic_msg = NULL;

// An immortal (rc=-1) empty string returned by _tin_recover() when not panicking.
static const struct { int64_t rc; char c; } _tin_empty_str_sentinel = { TIN_IMMORTAL_RC, '\0' };

// Called from a deferred function to retrieve and clear the current panic message.
// Returns an empty TinString if not currently panicking.
TinString _tin_recover(void) {
    if (_tin_panic_msg == NULL)
        return (TinString){ (const char *)&_tin_empty_str_sentinel.c, 0 };
    TinString s = _tin_str_from_cstr(_tin_panic_msg);
    _tin_panic_msg = NULL;  // marks as recovered
    return s;
}

// Run all pending deferred calls in LIFO order, print the panic message, then exit.
// If a deferred function calls recover(), _tin_panic_msg is cleared and we return.
void _tin_panic(const char *msg) {
    _tin_panic_msg = msg;
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
    fprintf(stderr, "tin panic: %s\n", msg);
    exit(1);
}

void _tin_assert(int32_t cond, const char *msg) {
    if (!cond) _tin_panic(msg);
}
