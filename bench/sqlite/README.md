# sqlite INSERT throughput - Tin vs C, Rust, Go, Crystal

One workload across five languages: `INSERT INTO bench (i, name) VALUES (?, ?)`
run N times inside a single transaction, against a fresh WAL+sync=NORMAL
database, using each language's idiomatic sqlite binding with a cached
prepared statement.

| binary          | binding                                       |
|-----------------|-----------------------------------------------|
| `c_inserts`     | C: `sqlite3.h` direct + `sqlite3_step`        |
| `rs_inserts`    | Rust: `rusqlite` (bundled libsqlite3)         |
| `cr_inserts`    | Crystal: `crystal-lang/crystal-sqlite3`       |
| `go_inserts`    | Go: `database/sql` + `mattn/go-sqlite3` (cgo) |
| `tin_inserts`   | Tin: `libs/sqlite` (this repo)                |

All five drive the same SQLite engine and the same query plan; the
gap is entirely binding overhead per call.

## Results (M1 Pro, macOS, sqlite 3.51, single connection)

`bench/sqlite/run.sh` builds every binary and runs each at N=10k / 100k / 1M.
Numbers below are ops/sec; the column is the median of 3 cold-db runs.

| N         |  C    | Rust  | Crystal | Go    | Tin    |
|-----------|-------|-------|---------|-------|--------|
| 10 000    | 3.3 M | 4.8 M | 2.6 M   | 1.4 M | 89 k   |
| 100 000   | 3.3 M | 5.0 M | 2.7 M   | 1.4 M | 82 k   |
| 1 000 000 | 3.3 M | 4.4 M | 2.8 M   | 1.3 M | 75 k   |

Tin is **~40x slower than C** on this workload. The gap is the cost of
the async wrapper described below, not the SQLite call itself.

## Why Tin is much slower here

`libs/sqlite` runs every Connection on its own dedicated pthread so the
blocking sqlite3 C API doesn't block the fiber scheduler.  A single
`db.exec(...)` does, per call:

1. Tin fiber allocates a `completion_t` and pushes a task onto an
   MPSC queue (mutex lock + linked-list insert + condvar signal).
2. The worker thread wakes from `pthread_cond_wait`, dequeues, runs
   `sqlite3_reset` + `sqlite3_bind_*` + `sqlite3_step` against the
   cached prepared statement.
3. Worker stores the result, then `_tin_fiber_unpark_fib`s the caller.
4. Caller fiber, parked via `_tin_fiber_park`, gets re-scheduled.
5. Caller reads the result code and returns from `await`.

Steps 1, 3, 4 add ~10-15 µs of pure overhead per call on this hardware -
a worker-thread round-trip plus a fiber context switch.  The other four
implementations do step 2 directly on the calling thread, so for a tight
INSERT loop their per-op cost is dominated by sqlite itself (~0.3 µs
with a cached stmt + WAL).

This design choice is deliberate: it keeps slow SQLite calls (long
SELECTs, contended writes) off the scheduler.  For workloads with
expensive per-op cost (anything bigger than a single cached-stmt write)
the wrapper overhead is a small fraction of total time.  For tight
INSERT loops like this benchmark, the overhead dominates.

A `db.exec_blocking(...)` shortcut that bypasses the worker would
recover most of the gap for callers that know their sqlite call is
fast, but does not exist today.

## Reproducing

```bash
bash bench/sqlite/run.sh
```

Each binary also runs standalone: `bench/bin/<lang>_inserts N`.

## Compiler / library versions

| Tool    | Version                              |
|---------|--------------------------------------|
| clang   | Homebrew clang 22.1.4                |
| Rust    | 1.88.0 (rusqlite 0.31 bundled)       |
| Go      | 1.25.4 (mattn/go-sqlite3 1.14.22)    |
| Crystal | 1.19.1 (crystal-sqlite3 0.20)        |
| Tin     | this repo, clang LLVM backend        |
| sqlite  | 3.51.0 (bundled with Rust; system on other langs) |

Rust uses `rusqlite`'s `bundled` feature so its sqlite is statically
linked and may differ slightly from the system version the C/Go/Crystal
binaries pick up; in practice this matters less than the per-call
overhead.
