# Tin Channel Benchmark Suite

Compares fiber/goroutine channel performance across Go, Crystal, Rust (Tokio
`current_thread`), and Tin.

## Benchmarks

| Name         | Description                                                      |
|--------------|------------------------------------------------------------------|
| `bench`      | Pingpong - 2 fibers, 1M round trips (SPSC latency baseline)      |
| `pipeline`   | 4 relay fibers in series, 1M passes (multi-hop latency)          |
| `mpmc`       | 4 producers + 4 consumers, 1M msgs (MPMC throughput)             |
| `jitter`     | 8 workers, variable 0-3 yields/task, 1M tasks (scheduler stress) |
| `pipeline10` | 10 relay fibers in series, 500K passes (deep pipeline latency)   |
| `fanout`     | 1 producer fans out to 8 worker fibers, 1M items (dispatch)      |

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
| Tin      | clang 22.1.3 (LLVM backend) |

## Results

Hyperfine wall-clock means (2 warmup + statistical multi-run). Latency and
throughput are derived from the wall-clock time / message count internal to
each benchmark.

### Pingpong - 1M round trips (lower is better)

| Language | Wall time | Latency / round trip |
|----------|----------:|---------------------:|
| Crystal  |  72.3 ms  | ~72 ns  |
| **Tin**  | **103.6 ms** | **~104 ns** |
| Rust     | 297.3 ms  | ~297 ns |
| Go       | 537.1 ms  | ~537 ns |

### Pipeline - 1M passes, 4 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| Crystal  | 157.6 ms  | ~158 ns |
| **Tin**  | **257.1 ms** | **~257 ns** |
| Rust     | 694.0 ms  | ~694 ns |
| Go       | 1122 ms   | ~1122 ns |

### MPMC - 1M messages, 4 producers + 4 consumers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| Crystal  |  11.2 ms  | ~89.3M msgs/s |
| **Tin**  | **34.7 ms** | **~28.8M msgs/s** |
| Go       |  56.5 ms  | ~17.7M msgs/s |
| Rust     | 303.0 ms  | ~3.30M msgs/s |

### Jitter - 1M tasks, 8 workers, 0-3 yields (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| **Tin**  | **67.3 ms** | **~14.86M tasks/s** |
| Rust     | 284.3 ms  | ~3.52M tasks/s |
| Go       | 407.5 ms  | ~2.45M tasks/s |
| Crystal  | 850.5 ms  | ~1.18M tasks/s |

### Pipeline10 - 500K passes, 10 stages (lower is better)

| Language | Wall time | Latency / pass |
|----------|----------:|---------------:|
| Crystal  | 180.0 ms  | ~360 ns |
| **Tin**  | **282.2 ms** | **~564 ns** |
| Rust     | 752.8 ms  | ~1506 ns |
| Go       | 757.6 ms  | ~1515 ns |

### Fanout - 1M items, 1 producer + 8 workers (higher is better)

| Language | Wall time | Throughput |
|----------|----------:|-----------:|
| Crystal  |  71.1 ms  | ~14.06M items/s |
| **Tin**  | **164.0 ms** | **~6.10M items/s** |
| Rust     | 432.7 ms  | ~2.31M items/s |
| Go       | 651.2 ms  | ~1.54M items/s |

## Summary

Across the 6 benchmarks Crystal wins 5 (latency-bound channel patterns) and
Tin wins 1 (jitter / irregular yield patterns). Tin places second in the
remaining 5 and beats Go and Rust on every single benchmark.

| Benchmark   | Tin vs leader | Tin vs runner-up |
|-------------|--------------:|-----------------:|
| Pingpong    | 1.43x slower than Crystal | 2.87x faster than Rust |
| Pipeline-4  | 1.63x slower than Crystal | 2.70x faster than Rust |
| MPMC        | 3.10x slower than Crystal | 1.63x faster than Go |
| Jitter      | **leader**                | 4.22x faster than Rust |
| Pipeline-10 | 1.57x slower than Crystal | 2.67x faster than Rust |
| Fanout      | 2.31x slower than Crystal | 2.64x faster than Rust |

## Notes

- **Scheduler model.** Tin uses M:N scheduling: M fiber coroutines multiplexed
  onto N OS worker threads via a single shared run queue. Go uses M:N with
  per-P work-stealing queues; Crystal uses M:N green threads. Rust Tokio
  `current_thread` is single-threaded. Tin's shared-queue design avoids
  per-thread queue overhead at the cost of higher lock contention under
  MPMC/fanout workloads.
- **Pingpong / pipeline.** Crystal's lead comes from lower green-thread
  context-switch cost vs OS threads. Tin's ~104 ns pingpong is ~3x faster than
  Go/Rust; the gap to Crystal is the cost Tin pays for real OS threads under
  the channel.
- **Jitter.** Tin now leads this benchmark by a wide margin (~12.6x ahead of
  Crystal, ~4.2x ahead of Rust). The irregular yield pattern stresses
  scheduler-wake throughput; Tin's autoyield + worker-stealing keeps every
  worker hot, while Crystal's single-runqueue serializes wakeups.
- **MPMC.** Tin's throughput now sits between Go and Crystal. Variance under
  contention is moderate; multiple runs cluster within ~25-40 ms.
- **Fanout / pipeline10.** Both are dispatch-density benchmarks; Crystal's
  green-thread cheapness dominates. Tin holds a clean 2nd place.
- **Rust Tokio current_thread** is single-threaded by construction, so the
  multi-worker patterns penalize it heavily.
