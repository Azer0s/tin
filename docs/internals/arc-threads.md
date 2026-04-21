# Internals: ARC Thread Safety

## Overview

Tin uses **Automatic Reference Counting (ARC)** to manage the lifetime of
heap-allocated values: strings and dynamic arrays (slices). When a value is
shared across fibers, ARC operations must be atomic to prevent races.

---

## Reference Count Operations

The RC field in every managed allocation is a plain `int32_t`. Retain and
release use C11 GCC built-ins:

```c
// Retain (increment ref count)
__atomic_fetch_add(&obj->rc, 1, __ATOMIC_ACQ_REL);

// Release (decrement and free if zero)
if (__atomic_fetch_sub(&obj->rc, 1, __ATOMIC_ACQ_REL) == 1) {
    _tin_dealloc(obj);
}
```

`__ATOMIC_ACQ_REL` provides acquire-release semantics: a release store by one
thread is visible as an acquire load by another, preventing use-after-free
when ownership is transferred between fibers.

---

## Spawn Ownership Transfer

When a fiber is spawned with arguments that include RC-tracked values (strings,
slices), the compiler emits a **retain** on each such argument before the spawn
and a corresponding **release** inside the spawned coroutine after it has
finished using the value.

```rust
fn{#async} worker(s string) =
  echo s      // s is retained by the spawn; released when worker exits

fn main() =
  let msg = "hello from main"
  spawn worker(msg)   // retain(msg); spawned fiber owns a reference
  // msg still valid here; main's reference released on scope exit
```

The retain happens in the **spawning** fiber; the release happens in the
**spawned** fiber. Because both are protected by `__ATOMIC_ACQ_REL`, this is
safe even on different worker threads.

---

## Channel Ownership Protocol

`Channel.send` **retains** the value; `Channel.recv` **transfers** ownership
(no retain on receive, release on channel close or element overwrite).

```
send:   retain(value)  -> store in ring slot
recv:   load from ring slot -> zero the slot -> caller owns the reference
close:  for each remaining slot -> release(value)
```

This means:
- The sender retains a copy (its own reference) and the channel holds another.
- The receiver inherits the channel's reference.
- If the channel is closed with unread values, they are released by `close`.

For primitive types (`i64`, `bool`, etc.) the retain/release calls are
compiled away (only pointer-typed values are RC-tracked).

---

## Future Result Ownership Transfer

When a spawned fiber completes (`FIBER_DONE`), the worker loop stores the
result in `f->result` on the `TinFiber` struct (moved from the thread-local
`_coro_result` via `_tin_coro_take_result`).

The awaiting caller retrieves the result via `_tin_fiber_get_result(pid)`:

```c
void *_tin_fiber_get_result(int64_t pid) {
    // under _table_mu
    TinFiber *f = _fibers[pid];
    void *r = f->result;     // heap-allocated result box
    f->result = NULL;
    _free_slot_push(pid);    // reclaim slot for reuse
    // free fiber outside lock
    return r;                // caller owns this allocation
}
```

`Future_await_result` calls `_tin_fiber_join(pid)` (parks / blocks until
`FIBER_DONE`), then checks for a panic via `_tin_fiber_get_panic_msg`, then
calls `_tin_fiber_get_result` to take ownership of the result box. The result
box is freed by the caller after unboxing the value.

`_tin_coro_take_result()` is used only by the **inline drive** path (when a
coroutine drives an inner `$coro` directly without spawning a fiber). In that
path the result is stored in a TLS buffer to avoid a malloc, and the outer
coroutine reads it before calling `coro.destroy`.

---

## AtomicI64

`sync::AtomicI64` wraps a C11 `_Atomic int64_t`:

```c
// add: returns old value
int64_t _tin_atomic_add(TinAtomic64 *a, int64_t delta) {
    return __atomic_fetch_add(&a->val, delta, __ATOMIC_SEQ_CST);
}

// load: sequentially consistent read
int64_t _tin_atomic_load(TinAtomic64 *a) {
    return __atomic_load_n(&a->val, __ATOMIC_SEQ_CST);
}
```

All operations use `__ATOMIC_SEQ_CST` for simplicity. If you need weaker
ordering for performance-critical code, use the raw C11 `_Atomic` type via
extern.

---

## What is NOT thread-safe

- **Plain struct fields**: reading/writing a struct field from multiple fibers
  without a `Mutex` is a data race. The compiler does not add implicit locking.
- **Top-level `var` variables**: use `sync::AtomicI64` or `sync::Mutex` if
  multiple fibers may access the same `var`.
- **Slice append**: `append` is not atomic. Protect with a `Mutex` if multiple
  fibers append to the same slice concurrently.
