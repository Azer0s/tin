// bench/sqlite/rs - INSERT N rows sequentially against one rusqlite
// Connection.  Counterpart of c_inserts.c, tin_inserts.tin,
// go/inserts.go, cr_inserts.cr.
//
// Build: cargo build --release --manifest-path=Cargo.toml
// Usage: ./rs_inserts N

use rusqlite::{params, Connection};
use std::env;
use std::time::Instant;

const DB_PATH: &str = "/tmp/rs_sqlite_bench.db";

fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 {
        eprintln!("usage: rs_inserts N");
        std::process::exit(1);
    }
    let total: i64 = args[1].parse().expect("N must be an integer");

    let _ = std::fs::remove_file(DB_PATH);
    let conn = Connection::open(DB_PATH).expect("open");
    conn.execute_batch(
        "PRAGMA journal_mode=WAL;\
         PRAGMA synchronous=NORMAL;\
         DROP TABLE IF EXISTS bench;\
         CREATE TABLE bench (i INTEGER, name TEXT);\
         BEGIN;",
    )
    .expect("setup");

    let mut stmt = conn
        .prepare("INSERT INTO bench (i, name) VALUES (?, ?)")
        .expect("prepare");

    let t0 = Instant::now();
    for i in 0..total {
        stmt.execute(params![i, "row"]).expect("step");
    }
    let elapsed_us = t0.elapsed().as_micros() as i64;

    drop(stmt);
    conn.execute_batch("COMMIT;").expect("commit");

    let ops_per_sec = total * 1_000_000 / elapsed_us;
    println!("n={} elapsed_us={} ops_per_sec={}", total, elapsed_us, ops_per_sec);
}
