// tin runtime - defer chain, panic, and recover

#include "runtime.h"
#include <stdio.h>
#include <stdlib.h>

// Global defer chain: newest entry first (LIFO)
static TinDeferEntry *_tin_defer_chain = NULL;

// Called at every `defer` statement; entry points to a TinDeferEntry on the caller's stack
void _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *), void *env) {
    entry->fn   = fn;
    entry->env  = env;
    entry->prev = _tin_defer_chain;
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
        if (e->fn) e->fn(e->env);
        e = next;
    }
    if (_tin_panic_msg == NULL) return;  // a deferred fn called recover()
    fflush(stdout);
    fprintf(stderr, "tin panic: %s\n", msg);
    exit(1);
}

void _tin_assert(int32_t cond, const char *msg) {
    if (!cond) _tin_panic(msg);
}
