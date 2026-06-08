// bench/sqlite/c_inserts - INSERT N rows sequentially through one
// sqlite3 connection with a prepared statement.  Reference for the
// Tin / Go / Rust / Crystal counterparts.
//
// Compile (macOS / Linux):
//   clang -O2 c_inserts.c -lsqlite3 -o c_inserts
// Usage: ./c_inserts N

#include <sqlite3.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static const char *DB_PATH = "/tmp/c_sqlite_bench.db";

static int64_t now_us(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: c_inserts N\n");
        return 1;
    }
    int64_t total = atoll(argv[1]);

    sqlite3 *db = NULL;
    if (sqlite3_open(DB_PATH, &db) != SQLITE_OK) {
        fprintf(stderr, "open failed: %s\n", sqlite3_errmsg(db));
        return 1;
    }
    char *err = NULL;
    sqlite3_exec(db, "PRAGMA journal_mode=WAL;",      NULL, NULL, &err);
    sqlite3_exec(db, "PRAGMA synchronous=NORMAL;",    NULL, NULL, &err);
    sqlite3_exec(db, "DROP TABLE IF EXISTS bench;",   NULL, NULL, &err);
    sqlite3_exec(db, "CREATE TABLE bench (i INTEGER, name TEXT);",
                 NULL, NULL, &err);
    sqlite3_exec(db, "BEGIN;", NULL, NULL, &err);

    sqlite3_stmt *stmt = NULL;
    if (sqlite3_prepare_v2(db,
            "INSERT INTO bench (i, name) VALUES (?, ?)",
            -1, &stmt, NULL) != SQLITE_OK) {
        fprintf(stderr, "prepare failed: %s\n", sqlite3_errmsg(db));
        return 1;
    }

    int64_t t0 = now_us();

    for (int64_t i = 0; i < total; i++) {
        sqlite3_reset(stmt);
        sqlite3_clear_bindings(stmt);
        sqlite3_bind_int64(stmt, 1, i);
        sqlite3_bind_text(stmt, 2, "row", -1, SQLITE_STATIC);
        sqlite3_step(stmt);
    }

    int64_t t1 = now_us();

    sqlite3_finalize(stmt);
    sqlite3_exec(db, "COMMIT;", NULL, NULL, &err);
    sqlite3_close(db);

    int64_t elapsed_us = t1 - t0;
    int64_t ops_per_sec = total * 1000000 / elapsed_us;
    printf("n=%lld elapsed_us=%lld ops_per_sec=%lld\n",
           (long long)total, (long long)elapsed_us, (long long)ops_per_sec);
    return 0;
}
