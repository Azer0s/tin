# Tin Channel Benchmark Suite

Compares fiber/goroutine channel performance across Go, Crystal, Rust (Tokio
`current_thread`), and Tin.

## Benchmarks

| Name       | Description                                                      |
|------------|------------------------------------------------------------------|
| `bench`    | Pingpong - 2 fibers, 1M round trips (SPSC latency baseline)    |
| `pipeline` | 4 relay fibers in series, 1M passes (multi-hop latency)         |
| `mpmc`     | 4 producers + 4 consumers, 1M msgs (MPMC throughput)            |
| `jitter`   | 8 workers, variable 0-3 yields/task, 1M tasks (scheduler stress) |

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
| **OS**  | Arch Linux, kernel 6.19.6-arch1-1 |

## Compiler versions

| Language | Version |
|----------|---------|
| Go       | 1.25.8  |
| Crystal  | 1.18.2 (LLVM 18.1.8) |
| Rust     | 1.91.1  |
| Tin      | clang 22.1.2 (LLVM backend) |

## Results

Best self-reported result observed across 20 runs. Latency and throughput are
measured internally by each benchmark (excludes process startup time).

### Pingpong - 1M round trips (lower is better)

| Language | Latency / round trip |
|----------|---------------------|
| Crystal  | ~71 ns  |
| **Tin**  | **~109 ns** |
| Go       | ~270 ns |
| Rust     | ~299 ns |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Latency / pass |
|----------|---------------|
| Crystal  | ~151 ns |
| **Tin**  | **~268 ns** |
| Go       | ~692 ns |
| Rust     | ~698 ns |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Throughput |
|----------|-----------|
| Crystal  | ~99.5M msgs/s |
| Go       | ~19M msgs/s |
| **Tin**  | **~3.5M msgs/s** |
| Rust     | ~3.3M msgs/s  |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Throughput |
|----------|-----------|
| Rust     | ~3.5M tasks/s |
| Go       | ~2.5M tasks/s |
| **Tin**  | **~1.8M tasks/s** |
| Crystal  | ~1.1M tasks/s |

## Notes

- Tin uses M:N scheduling: M fiber coroutines multiplexed onto N OS worker
  threads via a single shared run queue. Go uses M:N with per-P work-stealing
  queues; Crystal uses M:N green threads. Rust Tokio `current_thread` is
  single-threaded. Tin's shared-queue design avoids per-thread queue overhead
  at the cost of higher lock contention under MPMC/jitter workloads.
- Crystal's pingpong lead comes from lower green-thread context-switch cost vs
  OS threads. Tin's ~109 ns is 2.5x faster than Go/Rust for this workload.
- Jitter throughput reflects scheduler resilience under irregular yield patterns;
  Rust Tokio `current_thread` wins because it avoids cross-thread wakeups entirely.
- Tin MPMC variance is high due to OS thread scheduling across 8 threads competing
  on the shared run queue and channel; results between ~2.6M and ~4.8M msgs/s are
  normal across runs.
