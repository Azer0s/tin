#pragma once
// tin runtime - M:N fiber scheduler
//
// N worker OS threads (TINMAXPROCS) execute M coroutine fibers from a shared
// run queue.  Blocking operations park the fiber (FIBER_BLOCKED) and are
// re-enqueued when the blocking condition resolves.
//
// FiberStatus: RUNNABLE -> RUNNING -> (BLOCKED ->)* DONE

#include <stdint.h>
#include <stdbool.h>

// Initialize the fiber runtime. Called from the generated main() wrapper.
void _tin_fiber_init(void);

// Spawn a new fiber: creates an OS thread that drives coro_hdl to completion.
// Returns a unique fiber PID.
int64_t _tin_fiber_spawn(void *hdl);

// Called by the coroutine body when it has finished.
// result: pointer to the return value; NULL for void-returning fibers.
// The hdl parameter was removed — LLVM's coro-elide pass can now see that
// the inner coroutine handle does not escape to any external function, enabling
// stack-allocation of inner coroutine frames for inline-drive call sites.
void _tin_fiber_complete(void *result);

// Inline-drive result helpers — used by emitCoroComplete / genInlineAsyncDrive.
// When an inner $coro is driven inline (no fiber spawn), the result is stored
// in a per-thread TLS buffer instead of a heap allocation.  The outer coroutine
// reads the result via _tin_coro_take_result() before calling coro.destroy.
//
//  _tin_inline_result_mode_begin()   — enable TLS result storage
//  _tin_inline_result_alloc(sz)      — returns TLS ptr if inline mode + fits,
//                                      else falls back to malloc(sz)
//  _tin_inline_result_mode_end()     — disable TLS result storage
//  _tin_inline_result_free(ptr)      — free() only if ptr was heap-allocated
void  _tin_inline_result_mode_begin(void);
void *_tin_inline_result_alloc(int64_t sz);
void  _tin_inline_result_mode_end(void);
void  _tin_inline_result_free(void *ptr);

// Block the calling thread until the fiber with the given PID is done.
// In the thread model, my_hdl is ignored (kept for API compatibility).
void _tin_fiber_join(int64_t pid, void *my_hdl);

// Retrieve the result stored by _tin_fiber_complete for fiber `pid`.
// Returns NULL if the fiber returned void or hasn't completed yet.
void *_tin_fiber_get_result(int64_t pid);

// Cooperative yield hint: calls sched_yield().
// The coro.suspend that follows in the IR is a no-op in thread model
// (drive loop immediately resumes).
void _tin_fiber_yield_coro(void *hdl);

// Join all outstanding fibers then free the fiber table.
// Called at the end of the generated main() wrapper.
void _tin_fiber_run(void);

// Blocking await for non-coroutine context (e.g., main without spawn).
void _tin_fiber_sync_await(int64_t pid);

// Block until fiber `pid` completes and return its heap-allocated result.
// Returns a non-null pointer to a static sentinel for void-returning fibers.
// Used by Future[t].await_result() in stdlib/sync/future.tin.
void *_tin_future_await_raw(int64_t pid);

// _tin_current_pid and _tin_current_coro_hdl are static inline in runtime.h
// (via extern __thread _current_pid / _current_hdl).

// Mark fiber `pid` as RUNNABLE and push it onto the global run queue.
// Used by the I/O thread and timer thread to wake blocked fibers.
void _tin_fiber_unpark(int64_t pid);

// Mark fiber `pid` as BLOCKED so the worker will not re-enqueue it after yield.
// Caller must register a waker that calls _tin_fiber_unpark before yielding.
void _tin_fiber_park(int64_t pid);

// Like _tin_fiber_unpark but the caller already holds the coro handle (hdl),
// skipping the fiber-table lookup for it.  Used by TinFastMutex and the channel
// waiter lists to shorten the unpark hot path.
void _tin_fiber_unpark_hdl(int64_t pid, void *hdl);

// Returns the coro handle of the fiber currently running on this worker thread,
// or NULL if called from outside a fiber context (I/O thread, timer, main).
// Used by channel_arc.c to pass hdl to TinFastMutex without _table_mu.
// (Defined as static inline in runtime.h via extern __thread _current_hdl.)
void *_tin_current_coro_hdl(void);

// Returns the internal TinFiber* of the fiber currently running on this worker
// thread as an opaque void*, or NULL outside a fiber context.  Store this in
// channel waiter lists to allow _tin_fiber_unpark_fib to skip _table_mu.
void *_tin_current_fib(void);

// Like _tin_fiber_unpark_hdl but uses a pre-captured TinFiber* (from
// _tin_current_fib) to bypass the global _table_mu lock entirely.
// Only the per-fiber spinlock is acquired on the unpark hot path.
void  _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl);

// Called by channel_arc.c send_blocking to mark that data was delivered
// directly to the receiver's out buffer (bypassing the ring buffer).
// Must be called BEFORE _tin_fiber_unpark_fib so the write is visible via
// the release/acquire pair on the per-fiber state_lock.
void  _tin_fiber_set_direct_recv(void *fib);
