#pragma once
// tin runtime - fiber sleep / timer support

#include <stdint.h>

// Start the timer thread. Called automatically by _tin_fiber_init.
void _tin_timer_init(void);

// Stop the timer thread. Called automatically by _tin_fiber_run.
void _tin_timer_shutdown(void);

// Park the current fiber for `ms` milliseconds.
// coro.suspend must fire after this call (via the `yield` in the Tin wrapper).
void _tin_sleep_ms(int64_t ms);
