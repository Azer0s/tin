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

Best self-reported result observed across 5 runs. Latency and throughput are
measured internally by each benchmark (excludes process startup time).

### Pingpong - 1M round trips (lower is better)

| Language | Latency / round trip |
|----------|---------------------|
| Crystal  | ~72 ns  |
| **Tin**  | **~99 ns** |
| Go       | ~268 ns |
| Rust     | ~299 ns |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Latency / pass |
|----------|---------------|
| Crystal  | ~152 ns |
| **Tin**  | **~252 ns** |
| Go       | ~698 ns |
| Rust     | ~695 ns |

### Pipeline10 - 500K passes, 10 stages (lower is better)

| Language | Latency / pass |
|----------|---------------|
| Crystal  | ~178 ns |
| **Tin**  | **~549 ns** |
| Go       | ~791 ns |
| Rust     | ~754 ns |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Throughput |
|----------|-----------|
| Crystal  | ~90M msgs/s |
| **Tin**  | **~47M msgs/s** |
| Go       | ~22M msgs/s |
| Rust     | ~3.3M msgs/s |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Throughput |
|----------|-----------|
| **Tin**  | **~16.5M tasks/s** |
| Go       | ~2.9M tasks/s |
| Rust     | ~3.5M tasks/s |
| Crystal  | ~1.2M tasks/s |

### Fanout - 1M items, 8 workers (higher is better)

| Language | Throughput |
|----------|-----------|
| Crystal  | ~14M items/s |
| **Tin**  | **~6.2M items/s** |
| Go       | ~3.6M items/s |
| Rust     | ~2.3M items/s |

## Notes

- Tin uses M:N scheduling: M fiber coroutines multiplexed onto N OS worker
  threads via a single shared run queue. Go uses M:N with per-P work-stealing
  queues; Crystal uses M:N green threads. Rust Tokio `current_thread` is
  single-threaded.
- The fiber struct pool (introduced in this version) reuses `TinFiber` heap
  allocations including live `pthread_mutex_t`/`pthread_cond_t` across
  spawn/reclaim cycles.  This eliminates the per-spawn `calloc` +
  `pthread_mutex_init` + `pthread_mutex_destroy` + `free` overhead that
  dominated short-lived fiber workloads like MPMC. MPMC throughput improved
  from ~3.5M msgs/s to ~47M msgs/s as a result.
- Crystal's pingpong and pipeline leads come from lower green-thread
  context-switch cost vs OS threads. Tin's ~99 ns pingpong is 2.7x faster
  than Go/Rust for this workload.
- Jitter throughput reflects scheduler resilience under irregular yield
  patterns. Tin leads here due to the Chase-Lev per-worker work-stealing
  deque (selfnext/runnext/handoff fast paths) combined with the fiber struct
  pool.
- Tin MPMC variance across runs is ~5% with the pool active. Peak pool
  reaches 8 structs (4P+4C) and is reused for all 1M messages after warmup.
