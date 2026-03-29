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
// result: heap-allocated result value; NULL for void-returning fibers.
void _tin_fiber_complete(void *hdl, void *result);

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

// Returns the PID of the fiber currently executing on this worker thread,
// or -1 if called from outside a fiber context (e.g. I/O thread, main).
// Used by async_io.c and timer.c to park/unpark fibers.
int64_t _tin_current_pid(void);

// Mark fiber `pid` as RUNNABLE and push it onto the global run queue.
// Used by the I/O thread and timer thread to wake blocked fibers.
void _tin_fiber_unpark(int64_t pid);

// Mark fiber `pid` as BLOCKED so the worker will not re-enqueue it after yield.
// Caller must register a waker that calls _tin_fiber_unpark before yielding.
void _tin_fiber_park(int64_t pid);
