# sqlite INSERT throughput - Tin vs C, Rust, Go, Crystal

One workload across five languages plus a low-level Tin variant:
`INSERT INTO bench (i, name) VALUES (?, ?)` run N times inside a
single transaction, against a fresh WAL+sync=NORMAL database, using
each language's idiomatic sqlite binding with a cached prepared
statement.

| binary             | binding                                                     |
|--------------------|-------------------------------------------------------------|
| `c_inserts`        | C: `sqlite3.h` direct + `sqlite3_step`                      |
| `rs_inserts`       | Rust: `rusqlite` (bundled libsqlite3)                       |
| `cr_inserts`       | Crystal: `crystal-lang/crystal-sqlite3`                     |
| `go_inserts`       | Go: `database/sql` + `mattn/go-sqlite3` (cgo)               |
| `tin_inserts`      | Tin high level: `db.exec_is(sql, i, s)` (shape-specialized) |
| `tin_inserts_stmt` | Tin low level:  `db.prepare(...)` + `stmt.exec_is(i, s)`    |

## Results (M1 Pro, macOS, sqlite 3.51, single connection)

`bench/sqlite/run.sh` builds every binary and runs each at
N=10k / 100k / 1M.  Numbers below are ops/sec (median of a few
cold-db runs).

| N         | C     | Rust  | Crystal | Go    | Tin (high level) | Tin (stmt) |
|-----------|-------|-------|---------|-------|------------------|------------|
| 10 000    | 3.3 M | 4.8 M | 2.6 M   | 1.4 M | 2.9 M            | 2.9 M      |
| 100 000   | 3.3 M | 5.0 M | 2.8 M   | 1.4 M | 3.0 M            | 3.0 M      |
| 1 000 000 | 3.4 M | 4.3 M | 2.8 M   | 1.3 M | **3.1 M**        | 3.1 M      |

Tin's high-level `db.exec_is` lands at **3.1 M ops/sec on 1 M inserts -
~10% faster than Crystal**, ~90% of C.  The low-level `Stmt`-based
variant is identical at steady state; the per-call cache lookup in
the high-level path hits a pointer-equality fast-path when the SQL
is a stable string literal, so it's effectively free.

## The two API tiers

Both blocking - all sqlite3 calls run directly on the caller's
thread.  A per-connection mutex guards the sqlite3 handle and stmt
cache so a Connection can be shared safely across Tin fibers.

### High level

```tin
let db = sqlite::open("test.db").unwrap()
db.exec("CREATE TABLE u (id INTEGER, name TEXT)", [])
db.exec_is("INSERT INTO u VALUES (?, ?)", 1, "alice")
let rows = db.query("SELECT id, name FROM u", []).unwrap()
for let i i64 = 0; i < rows.count(); i = i + 1:
  echo(rows.get_i64(i, 0), rows.get_string(i, 1))
db.close()
```

Connection methods:
- `exec(sql, params [SqlValue])` - generic any-arity exec
- `exec_i / exec_s / exec_ii / exec_is / exec_si` - shape-specialized,
  raw primitives in, no SqlValue boxing
- `query(sql, params)` - returns `Result[Rows, ...]`, materialized
- `run(q SqlQuery)` / `fetch(q)` - `_sql`-macro friendly variants
- `prepare(sql)` - returns `Stmt` (opens the low-level door)
- `close()`

### Low level

```tin
let stmt = db.prepare("INSERT INTO u VALUES (?, ?)").unwrap()
for let i i64 = 0; i < n; i = i + 1:
  stmt.exec_is(i, "row")
stmt.close()
```

Stmt methods:
- `bind_int / bind_real / bind_text / bind_null` + `step` + `reset` -
  one direct C call each, full control
- `exec_i / exec_s / exec_ii / exec_is / exec_si / exec_iis` -
  bind every column + step + reset in one FFI call

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
binding overhead.
