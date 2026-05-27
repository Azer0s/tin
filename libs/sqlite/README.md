# libs/sqlite - blocking SQLite bindings for Tin

A thin, blocking wrapper around `libsqlite3`.  All `sqlite3_*` calls
run synchronously on the caller's thread; a per-connection mutex
guards the handle + prepared-statement cache so a `Connection` is
safe to share across Tin fibers.

Two API tiers, both blocking, both share the same per-conn stmt
cache.  Plus a stdlib/db driver impl so the same `Connection` works
as a generic ORM backend.

## Install / build

`libs/sqlite/sqlite.tin` has `//!+sqlite.c -- -lsqlite3` at the
top, so `tin build any_file.tin` that imports `sqlite` will compile
`sqlite.c` next to it and link against the system sqlite.  Make
sure `libsqlite3` headers + library are installed:

| OS              | Install                                    |
|-----------------|--------------------------------------------|
| macOS           | preinstalled                               |
| Ubuntu / Debian | `apt install libsqlite3-dev`               |
| Alpine          | `apk add sqlite-dev`                       |
| Fedora / RHEL   | `dnf install sqlite-devel`                 |

## High-level API

```tin
use sqlite

let db = sqlite::open("test.db").unwrap()

db.exec("CREATE TABLE u (id INTEGER, name TEXT)", [])
db.exec_is("INSERT INTO u VALUES (?, ?)", 1, "alice")
db.exec("INSERT INTO u VALUES (?, ?)", [2, "bob"])

let rows = db.query("SELECT id, name FROM u", []).unwrap()
for let i i64 = 0; i < rows.count(); i = i + 1:
  echo(rows.get_i64(i, 0), rows.get_string(i, 1))

db.close()
```

`Connection` methods:

| method                                 | purpose                                          |
|----------------------------------------|--------------------------------------------------|
| `open(path) Result[Connection, Err]`   | open or create the database                      |
| `exec(sql, params [SqlValue]) i64`     | generic exec; returns `changes()` or `-errcode`  |
| `exec_i / exec_s / exec_ii / exec_is / exec_si` | shape-specialised exec; raw primitives in, zero `SqlValue` boxing |
| `query(sql, params) Result[Rows, Err]` | run SELECT, materialise all rows                 |
| `prepare(sql) Result[Stmt, Err]`       | compile to a reusable `Stmt`                     |
| `run(SqlQuery) i64`                    | `_sql_query`-macro friendly twin of `exec`       |
| `fetch(SqlQuery) Result[Rows, Err]`    | `_sql_query`-macro friendly twin of `query`      |
| `close()`                              | release the handle (Closeable)                   |

Bind value types (`data SqlValue`):

- `Int(i64)`, `Real(f64)`, `Text(string)`, `Null`
- `i64`, `f64`, `string` auto-coerce via `static fn ::implicit`,
  so `[1, "row", 3.14]` works directly.

## Low-level API

When a hot loop shows the per-call cache lookup on the high-level
path, drop to the prepared statement directly:

```tin
let stmt = db.prepare("INSERT INTO u VALUES (?, ?)").unwrap()
for let i i64 = 0; i < n; i = i + 1:
  stmt.exec_is(i, "row")
stmt.close()
```

`Stmt` methods:

- `bind_int / bind_real / bind_text / bind_null` + `step` + `reset`:
  one direct C call each, full control.
- `exec_i / exec_s / exec_ii / exec_is / exec_si / exec_iis`: bind
  every column + step + reset under one mutex acquire and one FFI
  call boundary.
- `close()` (Closeable) finalises the statement.

## The `_sql_query` suffix macro

`"... {ident} ..."_sql_query` is a CTFE-expanded suffix macro that
rewrites every `{name}` placeholder into a `?` and builds the
`SqlQuery{sql, params}` value at compile time:

```tin
use { sql_query } from sqlite

let id   i64    = 11
let name string = "carol"

let n i64 = db.run("INSERT INTO u VALUES ({id}, {name})"_sql_query)
let rows = db.fetch("SELECT * FROM u WHERE id >= {id}"_sql_query).unwrap()
```

Brace escapes work the standard way: `\{` and `\}` stay literal.
The identifier flows through `SqlValue`'s implicit coercions, so
any `i64 / f64 / string` works as a placeholder argument.

## stdlib/db driver impl

`sqlite::Connection` implements `db::Conn`; `sqlite::Rows` exposes
a `to_rowset()` adapter that hands stdlib/db a concrete `RowSet`
with the right function pointers.  Together they let `stdlib/db`'s
tag-driven CRUD work against sqlite with no extra code:

```tin
use sqlite
use db

struct user =
  id    i64    @"db:id"
  name  string @"db:name"
  score f64    @"db:score"
  pwd   string @"db:-"      // skipped column

let c = sqlite::open("app.db").unwrap()
c.exec("CREATE TABLE user (id INTEGER, name TEXT, score REAL)", [])

db::insert[user](c, user{id: 1, name: "alice", score: 4.5, pwd: ""})
let u = db::find_by_id[user](c, 1).unwrap()
let all = db::all[user](c).unwrap()
db::update[user](c, user{id: 1, name: "renamed", score: 9.0, pwd: ""})
db::delete[user](c, 1)
```

The default table name is the struct name (`user` here).  Use the
`*_in` variants (`insert_into`, `find_by_id_in`, `all_in`,
`update_in`, `delete_in`) to override.

## Performance

`bench/sqlite/run.sh` runs the same single-transaction INSERT
workload across C / Rust / Crystal / Go / Tin at
N=10k / 100k / 1M; see `bench/sqlite/README.md` for the table.
The high-level `db.exec_is` path lands at ~3.1 M ops/sec on 1 M
inserts (M1 Pro, macOS, sqlite 3.51) - ~10% faster than Crystal
and ~90% of the C baseline.

## Tests

```bash
./tin test libs/sqlite/...
./tin test --valgrind libs/sqlite/...   # Linux only
```

`sqlite_test.tin` covers the high- and low-level APIs end-to-end;
`db_orm_test.tin` exercises stdlib/db CRUD against this driver.
