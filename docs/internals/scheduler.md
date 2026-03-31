# Internals: M:N Fiber Scheduler

## Overview

Tin's fiber scheduler is an **M:N scheduler**: M fiber coroutines are
multiplexed onto N worker OS threads. Each fiber is an LLVM coroutine
(`$coro` variant) whose handle is passed through a shared run queue. Context
switching is a coroutine resume/suspend - no OS context switch, no signal
delivery.

Source: `runtime/fiber.c`

---

## Run Queue

```
TinRunQueue {
    TinFiber *ring[TIN_MAX_FIBERS];  // circular buffer
    int       head, tail, count;     // FIFO indices
    pthread_mutex_t mu;
    pthread_cond_t  cond;
    int       shutdown;              // set to 1 on exit
}
```

- **Push**: `ring[tail++ % TIN_MAX_FIBERS] = f`, increment `count`, signal `cond`.
- **Pop**: blocks on `cond` until `count > 0` or `shutdown == 1`; returns `NULL` on shutdown.
- Lock ordering: only one lock at a time. Never hold `_table_mu` while taking `_run_queue.mu`.

---

## FiberStatus State Machine

```
                   spawn()
                    │
                    ▼
              ┌──RUNNABLE──┐
              │            │
         worker picks up   │
              │            │
              ▼            │
           RUNNING ────────┘ (yield / auto-yield: re-enqueue)
              │
       park() │  (sleep / blocked I/O)
              ▼
           BLOCKED
              │
      unpark()│  (timer fires / epoll/kqueue fires)
              ▼
           RUNNABLE -> ... -> DONE
```

- **FIBER_RUNNABLE**: in the run queue, waiting for a worker.
- **FIBER_RUNNING**: currently executing on a worker thread.
- **FIBER_BLOCKED**: parked (sleeping or waiting for I/O). NOT in the run queue.
- **FIBER_DONE**: coroutine completed; result available; waiting for `await`.

Transitions:
- `spawn()` -> RUNNABLE + push to queue
- worker picks up -> RUNNING (`_current_pid = pid`)
- `yield` / auto-yield backedge -> if status == RUNNING -> RUNNABLE + re-enqueue
- `_tin_fiber_park(pid)` -> BLOCKED (caller must NOT re-enqueue)
- `_tin_fiber_unpark(pid)` -> RUNNABLE + push to queue
- coroutine `llvm.coro.done` -> DONE; wake any `_tin_fiber_join` waiters

---

## Worker Loop

```c
// Simplified
void *_worker_fn(void *arg) {
    while (1) {
        TinFiber *f = _run_queue_pop();      // blocks if empty
        if (!f) break;                        // shutdown
        _current_pid = f->pid;
        f->status = FIBER_RUNNING;
        llvm_coro_resume(f->handle);          // run fiber until next suspend
        _current_pid = -1;
        if (f->status == FIBER_RUNNING)       // not parked or done
            f->status = FIBER_RUNNABLE;
        if (f->status == FIBER_RUNNABLE)
            _run_queue_push(f);               // re-enqueue for next turn
    }
}
```

The worker only re-enqueues if status is still RUNNING after resume. If
`_tin_fiber_park` set it to BLOCKED before the resume returns, the worker
does NOT re-enqueue (the fiber stays parked until explicitly unparked).

---

## Park / Unpark Protocol

**Parking** (fiber blocks itself - e.g., I/O or sleep):

```c
void _tin_fiber_park(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    if (_fibers[pid]) _fibers[pid]->status = FIBER_BLOCKED;
    pthread_mutex_unlock(&_table_mu);
    // Fiber remains BLOCKED; worker will NOT re-enqueue on next check
}
```

**Unparking** (I/O thread or timer fires):

```c
void _tin_fiber_unpark(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    TinFiber *f = _fibers[pid];
    if (f && f->status == FIBER_BLOCKED) {
        f->status = FIBER_RUNNABLE;
        _run_queue_push(f);               // wake the fiber
    }
    pthread_mutex_unlock(&_table_mu);
}
```

---

## I/O Thread Wakeup

`runtime/async_io.c` runs a dedicated I/O thread that calls `epoll_wait`
(Linux) or `kevent` (macOS/FreeBSD) with a 5ms timeout.

When `_tin_async_read` or `_tin_async_write` gets EAGAIN:
1. Sets fd to O_NONBLOCK.
2. Calls `_io_park(fd, outerPid, readFlag)` which:
   - Calls `_tin_fiber_park(outerPid)` -> fiber status = BLOCKED.
   - Registers `fd` with epoll/kqueue for the appropriate event.
3. Returns `TIN_IO_BLOCKED` sentinel (`INT64_MIN`).

When epoll/kqueue fires:
1. I/O thread calls `_tin_fiber_unpark(outerPid)`.
2. Fiber re-enters the run queue as RUNNABLE.
3. Worker resumes the fiber's drive loop.
4. Drive loop resumes the inner `async_read$coro`.
5. Inner coro retries the read; it succeeds (or gets EAGAIN again).

---

## Timer Thread Wakeup

`runtime/timer.c` runs a timer thread that polls every 1ms.

When `io::sleep(ms)` is called:
1. `_tin_sleep_ms(ms)` in C parks the fiber: `_tin_fiber_park(pid)`.
2. Appends `TinTimer{deadline, pid}` to the timer table.
3. Timer thread wakes -> finds expired entries -> calls `_tin_fiber_unpark(pid)`.

---

## await spawn - Fiber Join

`await spawn fn_b(args)` is the correct pattern for running an async function
as a fiber and blocking the calling fiber until it completes. This is a
**two-step** operation:

1. `spawn fn_b(args)` starts a new fiber (pid2) and returns `Future[T]`.
2. `await future` evaluates `future` (which implements `Awaitable[T]`) and
   calls `Future_await_result(future)` -> `_tin_fiber_join(pid2)`.

```
await spawn fn_b(args)
  │
  ├─ spawn fn_b(args)   -> allocates fiber pid2, pushes to run queue
  │                        returns Future[T]{pid: pid2}
  │
  └─ await future       -> calls Future_await_result(future)
                        -> calls _tin_fiber_join(pid2)
                        -> parks calling fiber (BLOCKED)
                        -> waits until pid2 reaches FIBER_DONE
                        -> returns owned result pointer
```

### _tin_fiber_join

```c
// Simplified
void *_tin_fiber_join(int64_t pid) {
    _join_register(_current_pid, pid);   // wake calling fiber when pid2 is DONE
    _tin_fiber_park(_current_pid);       // park calling fiber (BLOCKED)
    // ... calling fiber resumes here after pid2 completes ...
    return _tin_coro_take_result();      // ownership transferred to caller
}
```

When pid2 reaches `FIBER_DONE`:
1. `_tin_fiber_complete` stores the result in the result buffer.
2. Looks up any waiting pids from the join table.
3. Calls `_tin_fiber_unpark` for each -> back to RUNNABLE.
4. Calling fiber resumes; `_tin_fiber_join` returns the result pointer.

There is **no inline drive loop** - the inner fiber runs independently on any
worker thread. The calling fiber simply parks until the target is done.

### `await` requires `Awaitable[T]`

The `await` guard checks only the **type** of the expression - any
`Awaitable[T]` value can be awaited, regardless of how it was produced:

```rust
// All valid - expression evaluates to Awaitable[T]:
await spawn fn_b(args)       // spawn returns Future[T]
let f = spawn fn_b(args)
await f                      // f is Future[T]
await fetch()                // fetch() returns Future[T] directly (wraps spawn internally)
await io::sleep(500)         // io::sleep returns Future[Unit] directly
await ioutil::read_string(conn) // returns Future[string] directly

// INVALID - fn_b is {#async}: fn_b(args) calls the sync variant, returns T not Awaitable[T]:
await fn_b(args)             // compile error if fn_b is {#async}
```

Many stdlib functions (`io::sleep`, `io::write_all`, `ioutil::read_string`,
`tcp::Conn.read`, etc.) return `Future[T]` directly and can be awaited without
`spawn`. Low-level primitives (`io::async_read`, `io::async_write`) are
`{#async}` functions that must be called with `await spawn`.

---

## Shutdown Sequence

1. `main()` returns.
2. `_tin_fiber_shutdown()` sets `_run_queue.shutdown = 1` and broadcasts `cond`.
3. All worker threads wake and return.
4. Worker threads are joined.
5. Timer thread and I/O thread are stopped (`_tin_timer_shutdown()`, `_tin_async_io_shutdown()`).
6. Process exits.

Unawaited fibers still in the run queue or parked are abandoned - their
coroutine handles are not destroyed (memory is reclaimed by the OS on process exit).

---

## Lock Ordering

To avoid deadlocks, always acquire locks in this order:

1. `_table_mu` (fiber table + status)
2. `_run_queue.mu` (run queue push/pop)

Never hold `_run_queue.mu` while acquiring `_table_mu`. The worker releases
`_table_mu` before touching the run queue.
