# Scheduler: shutdown, channel waiters, sizing, locking

## Shutdown Sequence

1. `main()` returns.
2. `_tin_fiber_shutdown()` sets `_run_queue.shutdown = 1` and broadcasts `cond`.
3. All worker threads wake and return.
4. Worker threads are joined.
5. Timer thread and I/O thread are stopped (`_tin_timer_shutdown()`, `_tin_async_io_shutdown()`).
6. Process exits.

After workers are joined, `_tin_fiber_run` iterates any entries remaining in
the run queue and calls `_tin_coro_free` on each handle before freeing the
queue buffer. This ensures coroutine frames for fibers that were enqueued but
never resumed are returned to the per-thread coro pool (and ultimately freed
when the pool is flushed) rather than leaking.

---

## Channel Waiter Queues

`stdlib/sync/channel_arc.c` parks fibers that call `send` on a full channel or
`recv` on an empty one. Each `TinChannel` has two dynamic waiter queues:

```c
TinWaiterQueue {
    int64_t *pids;   // fiber PIDs
    void   **hdls;   // coroutine handles (for _tin_fiber_unpark_hdl)
    void   **fibs;   // TinFiber* pointers (for _tin_fiber_unpark_fib)
    void   **outs;   // recv-direct out buffers (recv queue only; NULL = recv_blocking)
    int      cnt;    // current count
    int      cap;    // current capacity
}
```

Each queue starts at 8 slots and doubles up to `_chan_waiter_max` (default 64K,
configurable via `TINMAXCHANWAITERS`). Growth is triggered while the channel's
`TinFastMutex` is held; on OOM or cap exceeded the mutex is unlocked before
`_tin_panic` is called to avoid deadlock.

`_tin_channel_close` collects all parked fibers into heap-allocated snapshot
arrays (sized to the actual waiter count) before releasing the lock, so it
handles arbitrarily many waiters correctly.

---

## Queue Sizing Reference

All queues grow dynamically (doubling) up to a configurable cap, then panic.

| Queue              | Initial | Default cap | Env var             | `=0` behaviour | Slot reuse |
|--------------------|---------|-------------|---------------------|----------------|------------|
| Fiber table        | 256     | 1M          | `TINMAXFIBERS`      | panic          | yes (`_free_slots`) |
| Fiber struct pool  | 0       | 4096        | -                   | -              | yes (pool) |
| Run queue          | 1024    | 1M          | `TINMAXRUNNABLES`   | **unlimited**  | N/A |
| Timer table        | 1024    | 1M          | `TINMAXTIMERS`      | panic          | no |
| IO watch table     | 256     | 64K         | `TINMAXIOWATCHES`   | panic          | no |
| Channel waiter q.  | 8       | 64K         | `TINMAXCHANWAITERS` | **unlimited**  | N/A |

`=0` means "no cap - grow forever without panicking" (restores pre-cap behaviour
for run queue and channel waiters). Not supported for the other queues because
their pre-cap behaviour was a silent hang rather than a safe degradation.

The fiber table reuses slots via `_free_slots`: both fire-and-forget reclaim and
`_tin_fiber_get_result` push freed pids back onto this list so future spawns can
reuse them. This bounds peak slot usage to the number of concurrent live fibers
rather than the total spawned over the program's lifetime.

---

## Lock Ordering

To avoid deadlocks, always acquire locks in this order:

1. `_table_mu` (fiber table + status)
2. `_run_queue.mu` (run queue push/pop)

Never hold `_run_queue.mu` while acquiring `_table_mu`. The worker releases
`_table_mu` before touching the run queue.
