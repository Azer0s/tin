# Tin Channel Benchmark Suite

Compares fiber/goroutine channel performance across Go, Crystal, Rust (Tokio
`current_thread`), and Tin.

## Benchmarks

| Name         | Description                                                       |
|--------------|-------------------------------------------------------------------|
| `bench`      | Pingpong - 2 fibers, 1M round trips (SPSC latency baseline)     |
| `pipeline`   | 4 relay fibers in series, 1M passes (multi-hop latency)          |
| `pipeline10` | 10 relay fibers in series, 500K passes (deep pipeline latency)   |
| `mpmc`       | 4 producers + 4 consumers, 1M msgs (MPMC throughput)             |
| `jitter`     | 8 workers, variable 0-3 yields/task, 1M tasks (scheduler stress) |
| `fanout`     | 1 producer, 8 workers, 1M items (fanout throughput)              |

## Running

```bash
bash bench/run.sh
```

Requires `go`, `crystal`, `cargo`, and `tin` (built at `../tin`).
Uses `hyperfine` (2 warmup runs + statistical multi-run) if available.
Install via `yay -S hyperfine` or `cargo install hyperfine`.

## Hardware

| | |
|-|-|
| **CPU** | Intel Core i7-9700K @ 3.60GHz (8 cores, no HT) |
| **RAM** | 32 GB DDR4 |
| **OS**  | Arch Linux, kernel 6.19.11-arch1-1 |

## Compiler versions

| Language | Version |
|----------|---------|
| Go       | 1.25.8  |
| Crystal  | 1.18.2 (LLVM 18.1.8) |
| Rust     | 1.91.1  |
| Tin      | clang 22.1.2 (LLVM backend) |

## Results

Mean wall-clock time from hyperfine (2 warmup runs). Latency is computed as
mean_ms / message_count; throughput as message_count / mean_ms.

### Pingpong - 1M round trips (lower is better)

| Language | Mean time  | Latency / round trip |
|----------|------------|---------------------|
| Crystal  | 74.7 ms    | ~75 ns  |
| **Tin**  | **106.1 ms** | **~106 ns** |
| Go       | 275.3 ms   | ~275 ns |
| Rust     | 303.2 ms   | ~303 ns |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Mean time  | Latency / pass |
|----------|------------|---------------|
| Crystal  | 162.1 ms   | ~162 ns |
| **Tin**  | **259.6 ms** | **~260 ns** |
| Rust     | 703.3 ms   | ~703 ns |
| Go       | 708.9 ms   | ~709 ns |

### Pipeline10 - 500K passes, 10 stages (lower is better)

| Language | Mean time  | Latency / pass |
|----------|------------|---------------|
| Crystal  | 184.1 ms   | ~368 ns |
| **Tin**  | **282.9 ms** | **~566 ns** |
| Rust     | 765.5 ms   | ~1531 ns |
| Go       | 806.2 ms   | ~1612 ns |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Mean time  | Throughput |
|----------|------------|-----------|
| Crystal  | 11.4 ms    | ~88M msgs/s |
| **Tin**  | **26.3 ms**  | **~38M msgs/s** |
| Go       | 44.7 ms    | ~22M msgs/s |
| Rust     | 302.8 ms   | ~3.3M msgs/s |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Mean time  | Throughput |
|----------|------------|-----------|
| **Tin**  | **67.0 ms**  | **~14.9M tasks/s** |
| Rust     | 288.5 ms   | ~3.5M tasks/s |
| Go       | 345.4 ms   | ~2.9M tasks/s |
| Crystal  | 859.2 ms   | ~1.2M tasks/s |

### Fanout - 1M items, 8 workers (higher is better)

| Language | Mean time  | Throughput |
|----------|------------|-----------|
| Crystal  | 73.2 ms    | ~13.7M items/s |
| **Tin**  | **169.2 ms** | **~5.9M items/s** |
| Go       | 277.7 ms   | ~3.6M items/s |
| Rust     | 435.9 ms   | ~2.3M items/s |

## Notes

- Tin uses M:N scheduling: M fiber coroutines multiplexed onto N OS worker
  threads via a single shared run queue. Go uses M:N with per-P work-stealing
  queues; Crystal uses M:N green threads. Rust Tokio `current_thread` is
  single-threaded.
- The fiber struct pool reuses `TinFiber` heap allocations including live
  `pthread_mutex_t`/`pthread_cond_t` across spawn/reclaim cycles, eliminating
  the per-spawn `calloc` + `pthread_mutex_init` + `pthread_mutex_destroy` +
  `free` overhead that dominated short-lived fiber workloads. MPMC throughput
  improved from ~3.3M msgs/s (pre-pool) to ~38M msgs/s as a result.
- Crystal's pingpong and pipeline leads come from lower green-thread
  context-switch cost vs OS threads. Tin's ~106 ns pingpong is ~2.7x faster
  than Go/Rust for this workload.
- Jitter throughput reflects scheduler resilience under irregular yield
  patterns. Tin leads here due to the Chase-Lev per-worker work-stealing
  deque (selfnext/runnext/handoff fast paths) combined with the fiber struct
  pool eliminating allocation overhead per task.
- Tin MPMC variance is ~6% (σ 1.7 ms on 26.3 ms mean); the pool reaches 8
  structs (4P+4C) on the first burst and reuses them for all 1M messages.
- Pipeline10 shows per-stage hop cost of ~57 ns (566 ns / 10 stages), slightly
  better than the 4-stage pipeline's ~65 ns/stage, consistent with the
  selfnext/runnext fast paths keeping hot fibers on the same worker.
- Fanout (1 producer, 8 consumers) is bandwidth-limited by the single shared
  run queue; Crystal's lead reflects lower green-thread wake-up cost. Go and
  Rust both trail Tin despite being multi-threaded, reflecting the overhead of
  their respective work distribution mechanisms at this fan ratio.
