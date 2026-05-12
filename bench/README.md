# Tin Channel Benchmark Suite

Compares fiber/goroutine channel performance across Go, Crystal, Rust (Tokio
`current_thread`), and Tin.

## Running

```bash
bash bench/run.sh
```

Requires `go`, `crystal`, `cargo`, and `tin` (built at `../tin`).
Uses `hyperfine` (2 warmup runs + statistical multi-run) if available.
Install via `yay -S hyperfine` or `cargo install hyperfine`.

## Compiler versions

| Language | Version |
|----------|---------|
| Go       | 1.25.8  |
| Crystal  | 1.18.2 (LLVM 18.1.8) |
| Rust     | 1.91.1  |
| Tin      | clang 22.1.3 (LLVM backend) |

## Results

Hyperfine wall-clock means (2 warmup + statistical multi-run). Latency and
throughput are derived from the wall-clock time / message count internal to
each benchmark. The **bold** row is the fastest runtime on that host.

<details open>
<summary><b>Benchmarks</b> (what each pattern measures)</summary>

| Name         | Description                                                      |
|--------------|------------------------------------------------------------------|
| `bench`      | Pingpong - 2 fibers, 1M round trips (SPSC latency baseline)      |
| `pipeline`   | 4 relay fibers in series, 1M passes (multi-hop latency)          |
| `mpmc`       | 4 producers + 4 consumers, 1M msgs (MPMC throughput)             |
| `jitter`     | 8 workers, variable 0-3 yields/task, 1M tasks (scheduler stress) |
| `pipeline10` | 10 relay fibers in series, 500K passes (deep pipeline latency)   |
| `fanout`     | 1 producer fans out to 8 worker fibers, 1M items (dispatch)      |

**Pingpong** is the cheapest possible channel exercise: two fibers, one
unbuffered channel, ping-pong forever. Drives SPSC latency / context-switch
cost more than anything else.

**Pipeline** chains 4 (or 10) fibers in series so each message hops through
the whole chain before the next one enters. Stresses many cheap wakeups
per message; the deep variant amplifies any per-hop overhead.

**MPMC** is the contention test: four producers all racing into one shared
buffered channel while four consumers race to drain. Single-runqueue
schedulers shine here because they can dispatch from one ready queue
without a per-thread lock.

**Jitter** is the irregular-yield stress: each task yields 0-3 times
before it finishes, so wakeups are bursty. Schedulers that can keep every
worker hot win.

**Fanout** is the dispatch test: one producer hands work to 8 worker fibers
through a single channel. Throughput is gated by how fast the runtime can
hand off ready tasks to idle workers.

The two host tabs below are independent runs of the same suite; absolute
numbers vary by CPU + OS, so the ordering within a host is the relevant
signal.

| Host | Machine | CPU | OS |
|------|---------|-----|----|
| **M4 Pro** | Apple MacBook Pro 16 (2024) | Apple M4 Pro (10P + 4E) | macOS 26.4.1 (arm64) |
| **i7-9700K** | Custom desktop | Intel i7-9700K @ 4.9 GHz (8C / 8T) | Arch Linux, kernel 7.0.5-arch1-1 |

</details>

<details>
<summary><b>M4 Pro</b> (MacBook Pro 16, macOS 26.4.1)</summary>

### Pingpong - 1M round trips (lower is better)

| Language | Wall time | Latency / round trip |
|----------|----------:|---------------------:|
| **Tin**  | **38.6 ms** | **~39 ns** |
| Crystal  |  69.2 ms  | ~69 ns  |
| Rust     | 120.3 ms  | ~120 ns |
| Go       | 216.4 ms  | ~216 ns |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| **Tin**  | **97.9 ms** | **~98 ns** |
| Crystal  | 171.0 ms  | ~171 ns |
| Rust     | 249.6 ms  | ~250 ns |
| Go       | 538.1 ms  | ~538 ns |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| Crystal  |  9.2 ms   | ~108.7M msgs/s |
| Go       |  44.7 ms  | ~22.4M msgs/s |
| **Tin**  | **46.6 ms** | **~21.5M msgs/s** |
| Rust     | 102.7 ms  | ~9.7M msgs/s |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| **Tin**  | **32.5 ms** | **~30.8M tasks/s** |
| Rust     | 111.8 ms  | ~8.94M tasks/s |
| Crystal  | 253.1 ms  | ~3.95M tasks/s |
| Go       | 393.8 ms  | ~2.54M tasks/s |

### Pipeline10 - 500K passes, 10 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| **Tin**  | **95.8 ms** | **~192 ns** |
| Crystal  | 257.8 ms  | ~516 ns |
| Rust     | 262.6 ms  | ~525 ns |
| Go       | 586.2 ms  | ~1172 ns |

### Fanout - 1M items, 1 producer + 8 workers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| **Tin**  | **49.1 ms** | **~20.4M items/s** |
| Crystal  |  71.6 ms  | ~14.0M items/s |
| Rust     | 164.9 ms  | ~6.06M items/s |
| Go       | 211.1 ms  | ~4.74M items/s |

</details>

<details>
<summary><b>i7-9700K</b> (Arch Linux, kernel 7.0.5)</summary>

### Pingpong - 1M round trips (lower is better)

| Language | Wall time | Latency / round trip |
|----------|----------:|---------------------:|
| Crystal  |  72.8 ms  | ~73 ns  |
| **Tin**  | **103.7 ms** | **~104 ns** |
| Rust     | 297.8 ms  | ~298 ns |
| Go       | 749.6 ms ± 305 ms | host-noisy |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| Crystal  | 155.6 ms  | ~156 ns |
| **Tin**  | **259.1 ms** | **~259 ns** |
| Rust     | 698.7 ms  | ~699 ns |
| Go       | 1317 ms ± 511 ms | host-noisy |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| Crystal  |  11.3 ms  | ~88.5M msgs/s |
| **Tin**  | **36.1 ms** | **~27.7M msgs/s** |
| Go       |  56.2 ms  | ~17.8M msgs/s |
| Rust     | 304.4 ms  | ~3.29M msgs/s |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| **Tin**  | **67.6 ms** | **~14.8M tasks/s** |
| Rust     | 289.0 ms  | ~3.46M tasks/s |
| Go       | 409.4 ms  | ~2.44M tasks/s |
| Crystal  | 489.6 ms  | ~2.04M tasks/s |

### Pipeline10 - 500K passes, 10 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| Crystal  | 180.8 ms  | ~362 ns |
| **Tin**  | **287.2 ms** | **~574 ns** |
| Rust     | 748.8 ms  | ~1498 ns |
| Go       | 761.7 ms  | ~1523 ns |

### Fanout - 1M items, 1 producer + 8 workers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| Crystal  |  70.6 ms  | ~14.2M items/s |
| **Tin**  | **164.9 ms** | **~6.06M items/s** |
| Rust     | 428.8 ms  | ~2.33M items/s |
| Go       | 744.1 ms ± 343 ms | host-noisy |

</details>

## Summary

Tin leads 5 of the 6 benchmarks on M4 Pro and the jitter benchmark on
9700K; Crystal still wins MPMC and the dispatch-density patterns on the
older 8-core Linux box. On M4 Pro Tin beats every other runtime on every
benchmark except MPMC.

| Benchmark   | Tin on M4 Pro             | Tin on i7-9700K           |
|-------------|---------------------------|---------------------------|
| Pingpong    | **leader** (1.80x ahead of Crystal) | 1.42x slower than Crystal |
| Pipeline-4  | **leader** (1.75x ahead of Crystal) | 1.67x slower than Crystal |
| MPMC        | 5.05x slower than Crystal           | 3.19x slower than Crystal |
| Jitter      | **leader** (3.44x ahead of Rust)    | **leader** (4.28x ahead of Rust) |
| Pipeline-10 | **leader** (2.69x ahead of Crystal) | 1.59x slower than Crystal |
| Fanout      | **leader** (1.46x ahead of Crystal) | 2.34x slower than Crystal |

## Notes

- **Scheduler model.** Tin uses M:N scheduling: M fiber coroutines multiplexed
  onto N OS worker threads via a single shared run queue. Go uses M:N with
  per-P work-stealing queues; Crystal uses M:N green threads. Rust Tokio
  `current_thread` is single-threaded. Tin's shared-queue design pays for
  itself on dispatch-heavy patterns and stresses under MPMC contention.
- **Pingpong / pipeline.** Tin wins both outright on M4 Pro; on the older
  8-core 9700K Crystal's green-thread cheapness still leads by ~1.4-1.7x.
  Both numbers represent ~30-260 ns per channel hop -- Tin and Crystal sit
  in the same order of magnitude, well ahead of Go/Rust.
- **MPMC.** Crystal's single-runqueue green threads dominate this one on
  both hosts (~89-108M msgs/s). Tin sits in a near-tie with Go around ~21-28M
  msgs/s. Multi-worker channel contention is the regime where Tin's shared
  queue hurts most -- a known trade-off, not a regression.
- **Jitter.** Tin leads on both hosts (3.4-7.8x ahead of next runtime).
  The irregular yield pattern stresses scheduler-wake throughput; Tin's
  autoyield + multi-worker pool keeps every worker hot, while Crystal's
  single runqueue serializes wakeups.
- **Pipeline10 / fanout.** Dispatch-density patterns. M4 Pro's higher core
  count + faster IPC tips them to Tin; on the 8-core 9700K Crystal's
  green-thread cheapness still wins these two.
- **Go variance on the 9700K box.** Several Go runs (pingpong, pipeline,
  fanout) show standard deviations in the hundreds of ms (mean ±300 ms).
  The host had background load during the sweep; Go's worker-startup is
  most sensitive to that. Treat those numbers as upper bounds.
- **Rust Tokio current_thread** is single-threaded by construction, so the
  multi-worker patterns penalize it heavily.
