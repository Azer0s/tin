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

```c
TinRunQueue {
    TinRunnable *buf;           // heap-allocated ring buffer (grows on demand)
    int64_t      cap, head, tail, count;
    pthread_mutex_t mu;
    pthread_cond_t  not_empty;
    int          shutdown;      // set to 1 on exit
}
```

The buffer starts at 1024 slots and doubles on each overflow up to `_rq_max`
(default 1M, configurable via `TINMAXRUNNABLES`). Exceeding the cap calls
`_tin_panic` with a message naming the env var to raise.

- **Push**: append to ring, increment `count`, signal `not_empty` (skipped when a spinner is polling).
- **Pop**: adaptive spin (`RQ_SPIN_ITERS=500` trylock attempts, one spinner at a time) then
  block on `not_empty`; returns empty sentinel on shutdown.
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
     unpark() │  (timer fires / epoll/kqueue fires)
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


## See also

- [Park/unpark, I/O wake, timer wake, await spawn](scheduler-runtime.md)
- [Shutdown, channel waiters, queue sizing, lock ordering](scheduler-coordination.md)
