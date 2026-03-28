# Control - Defer, Panic, and Recover

## Overview

Tin's `defer`, `panic`, and `recover` are implemented in `runtime/defer.c` as
a global defer chain plus a panic-message slot. The mechanism is purely
stack-based - no heap allocation for the defer entries themselves - and
executes defers in strict LIFO order before either a normal return or a panic.

---

## The defer chain

```c
typedef struct TinDeferEntry {
    struct TinDeferEntry *prev;   // next-older entry in the chain
    void (*fn)(void *env);        // thunk that executes the deferred call
    void *env;                    // heap-allocated closure environment
} TinDeferEntry;

static TinDeferEntry *_tin_defer_chain = NULL;
```

Each entry is **allocated on the stack of the registering function** (as an
`alloca` in the LLVM IR). This means no heap allocation per `defer` statement.
The chain is singly-linked, newest first, so executing defers in LIFO order is
a simple forward walk.

### `_tin_defer_push`

```c
void _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *), void *env);
```

Called at every `defer` statement. `entry` is the caller's stack-allocated
node. The function fills in `fn`, `env`, and `prev`, then prepends `entry` to
`_tin_defer_chain`.

The `env` pointer is typically a heap-allocated struct that captures any local
variables referenced by the deferred call (a closure environment). It may be
`NULL` if the deferred call has no captures.

### `_tin_defer_pop`

```c
void _tin_defer_pop(int64_t n);
```

Pops the `n` most-recent entries from the chain **without executing them**.
Called immediately before every normal `return`, after the inline deferred
calls have already run.

Why? A normal return executes its own defers inline (in the generated IR),
then calls `_tin_defer_pop` to remove them so a subsequent `_tin_panic` call
(from a caller's `defer`) does not re-run them.

---

## Panic and recover

```c
static const char *_tin_panic_msg = NULL;
```

This slot is `NULL` when not panicking. During a `_tin_panic` call it holds the
panic message string; `_tin_recover` can clear it to suppress the panic.

### `_tin_panic`

```c
void _tin_panic(const char *msg);
```

1. Stores `msg` in `_tin_panic_msg`.
2. Snapshots and **clears** `_tin_defer_chain` (prevents re-entrant panics from
   re-running the same defers).
3. Walks the snapshotted chain in LIFO order, calling `entry->fn(entry->env)`
   for each entry.
4. After all defers run, checks `_tin_panic_msg`:
   - If `NULL`, a deferred function called `_tin_recover()` and cleared it.
     `_tin_panic` returns normally - the panic is **suppressed**.
   - If still set, flushes stdout, writes `"tin panic: {msg}\n"` to stderr,
     and calls `exit(1)`.

### `_tin_recover`

```c
TinString _tin_recover(void);
```

May only be usefully called from within a deferred function (while
`_tin_panic` is walking the chain).

- If `_tin_panic_msg != NULL`: copies the message into a `TinString` using
  `_tin_str_from_cstr`, clears `_tin_panic_msg` to suppress the panic, and
  returns the message string.
- If `_tin_panic_msg == NULL` (not panicking): returns an empty `TinString`
  whose backing data is an immortal sentinel so `_tin_release` is a no-op.

```c
// immortal empty string sentinel
static const struct { int64_t rc; char c; } _tin_empty_str_sentinel
    = { TIN_IMMORTAL_RC, '\0' };
```

The `TinString.ptr` points to `_tin_empty_str_sentinel.c` so the usual 8 bytes
before `ptr` contain `rc = -1`, making `_tin_retain`/`_tin_release` skip it
correctly.

### `_tin_assert`

```c
void _tin_assert(int32_t cond, const char *msg);
```

Calls `_tin_panic(msg)` if `cond` is false. Used by the compiler for built-in
assertions.

---

## Interaction diagram

```
Normal return path:
  [deferred calls execute inline in generated IR]
  _tin_defer_pop(n)          <- remove n entries from chain
  return

Panic path:
  _tin_panic(msg)
    _tin_panic_msg = msg
    snapshot & clear chain
    for each entry (LIFO):
      entry->fn(entry->env)  <- may call _tin_recover()
    if _tin_panic_msg == NULL:
      return                 <- panic suppressed by recover()
    else:
      fflush(stdout)
      fprintf(stderr, "tin panic: %s\n", msg)
      exit(1)
```

---

## Stack discipline for defer entries

Because each `TinDeferEntry` lives on the registering function's stack frame,
the chain invariant is:

> An entry is always valid as long as the frame that registered it has not
> returned.

Normal returns deregister their own entries with `_tin_defer_pop` before
returning, so the stack frames are valid when `_tin_panic` walks the chain.
Entries registered by outer (still-live) frames are also safe because those
frames have not yet returned.

This means the defer mechanism is **entirely allocation-free** for the entries
themselves. Only the closure environment (`env`) may require a heap allocation
if the deferred call captures local variables.
