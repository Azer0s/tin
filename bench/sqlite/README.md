# sqlite INSERT throughput - Tin vs C, Rust, Go, Crystal

One workload across five languages plus two Tin variants:
`INSERT INTO bench (i, name) VALUES (?, ?)` run N times inside a
single transaction, against a fresh WAL+sync=NORMAL database, using
each language's idiomatic sqlite binding with a cached prepared
statement.

| binary                  | binding                                       |
|-------------------------|-----------------------------------------------|
| `c_inserts`             | C: `sqlite3.h` direct + `sqlite3_step`        |
| `rs_inserts`            | Rust: `rusqlite` (bundled libsqlite3)         |
| `cr_inserts`            | Crystal: `crystal-lang/crystal-sqlite3`       |
| `go_inserts`            | Go: `database/sql` + `mattn/go-sqlite3` (cgo) |
| `tin_inserts`           | Tin: `libs/sqlite` `await db.exec(...)` (async via worker thread) |
| `tin_inserts_blocking`  | Tin: `libs/sqlite` `db.exec_blocking(...)` (caller-thread, bypasses worker) |

All six drive the same SQLite engine and the same query plan; the
gap is binding overhead per call.

## Results (M1 Pro, macOS, sqlite 3.51, single connection)

`bench/sqlite/run.sh` builds every binary and runs each at
N=10k / 100k / 1M.  Numbers below are ops/sec (median of a few cold-db
runs).

| N         | C     | Rust  | Crystal | Go    | Tin (blocking) | Tin (async)  |
|-----------|-------|-------|---------|-------|----------------|--------------|
| 10 000    | 3.2 M | 4.8 M | 2.6 M   | 1.4 M | 1.6 M          | 89 k         |
| 100 000   | 3.3 M | 5.0 M | 2.7 M   | 1.4 M | 1.9 M          | 73 k         |
| 1 000 000 | 3.3 M | 4.4 M | 2.8 M   | 1.3 M | 1.9 M          | 72 k         |

Two Tin numbers reflect the two API shapes the library offers and the
very different per-call costs.

## Two Tin API shapes, two cost profiles

`libs/sqlite` runs every Connection on its own dedicated pthread so
the blocking sqlite3 C API doesn't block the fiber scheduler.  This
gives the async API:

```tin
let n = await db.exec("INSERT ...", [...])
```

which does, per call:

1. Tin fiber allocates a `completion_t` and pushes a task onto an
   MPSC queue (mutex lock + linked-list insert + condvar signal).
2. The worker thread wakes from `pthread_cond_wait`, dequeues, runs
   `sqlite3_reset` + `sqlite3_bind_*` + `sqlite3_step` against the
   cached prepared statement.
3. Worker stores the result, then `_tin_fiber_unpark_fib`s the caller.
4. Caller fiber, parked via `_tin_fiber_park`, gets re-scheduled and
   reads the result.

Steps 1, 3, 4 add ~10-15 µs of pure overhead per call - a worker-thread
round-trip plus a fiber context switch.  That's the cost of keeping
slow SQLite calls off the scheduler.  For tight cached-stmt INSERT
loops it dominates: 89k ops/sec vs 3.2 M for direct C.

The same library also exposes:

```tin
let n = db.exec_blocking("INSERT ...", [...])   // no await
```

which runs the sqlite call directly on the calling thread.  It still
goes through the same prepared-statement cache (under a per-connection
mutex shared with the worker), but skips the queue + park/unpark cycle
entirely.  That cuts per-op overhead from ~10 µs to roughly the same
allocation+marshalling cost C-via-FFI bindings carry; throughput jumps
from 72-89 k ops/sec to 1.6-1.9 M ops/sec (a 20-25× win).

**When to pick which:**
- Use `await db.exec(...)` when the call may be slow (long SELECT,
  contended write, big BLOB write).  The worker thread eats the
  blocking time so the calling fiber's worker stays free to schedule
  other work.
- Use `db.exec_blocking(...)` when you know the call is fast (cached
  stmt, simple INSERT/UPDATE).  No fiber park, no queue, but the
  calling thread is pinned inside sqlite for the duration.

The two paths share the same Connection, the same prepared-statement
cache, and serialize against each other through a per-conn mutex - so
mixing them on one Connection is safe.  In pure-async or pure-blocking
workloads the mutex is uncontended.

## Reproducing

```bash
bash bench/sqlite/run.sh
```

Each binary also runs standalone: `bench/bin/<lang>_inserts N`.

## Compiler / library versions

| Tool    | Version                                           |
|---------|---------------------------------------------------|
| clang   | Homebrew clang 22.1.4                             |
| Rust    | 1.88.0 (rusqlite 0.31 bundled)                    |
| Go      | 1.25.4 (mattn/go-sqlite3 1.14.22)                 |
| Crystal | 1.19.1 (crystal-sqlite3 0.20)                     |
| Tin     | this repo, clang LLVM backend                     |
| sqlite  | 3.51.0 (bundled with Rust; system on other langs) |

Rust uses `rusqlite`'s `bundled` feature so its sqlite is statically
linked and may differ slightly from the system version the C/Go/Crystal
binaries pick up; in practice this matters less than the per-call
overhead.
