# measure

`measure` provides a monotonic clock for benchmarking and timing. It wraps
`CLOCK_MONOTONIC` from the C standard library - the clock never goes backwards
and is unaffected by wall-clock adjustments.

> **No extra linker flags are required.**

---

## Import

```rust
use measure
```

---

## Functions

### `measure::now_us() i64`

Returns the current monotonic time in **microseconds**.

Use this for sub-millisecond precision timing, such as benchmarking
tight loops, measuring channel latency, or profiling individual operations.

### `measure::now_ms() i64`

Returns the current monotonic time in **milliseconds**.

Sufficient for coarse timing - timeouts, I/O round trips, or any measurement
where microsecond resolution is not needed.

---

## Usage

### Basic elapsed time

```rust
use measure

let start = measure::now_us()
// ... work ...
let elapsed = measure::now_us() - start
echo "elapsed: ~{elapsed / 1000}ms"
```

### Per-operation latency (nanoseconds)

`now_us` returns microseconds. Multiply by 1000 to convert to nanoseconds
before dividing by the operation count, to avoid losing precision to integer
division:

```rust
use measure

let n i64 = 1000000
let start = measure::now_us()
// ... n operations ...
let elapsed_us = measure::now_us() - start
let ns_per_op  = elapsed_us * 1000 / n
echo "{n} ops in ~{elapsed_us / 1000}ms"
echo "latency: ~{ns_per_op}ns / op"
```

### Throughput

```rust
use measure

let n i64 = 1000000
let start = measure::now_us()
// ... process n items ...
let elapsed_us  = measure::now_us() - start
let throughput  = n * 1000000 / elapsed_us   // items per second
echo "throughput: ~{throughput} items/sec"
```

---

## Implementation notes

Both functions call `clock_gettime(CLOCK_MONOTONIC, ...)` from `time.h`.
The underlying C functions (`_tin_now_us`, `_tin_now_ms`) live in
`runtime/time.c` and are always compiled into every Tin binary.
