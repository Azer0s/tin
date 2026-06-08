// bench/sqlite/go/inserts - INSERT N rows sequentially against one
// sqlite db via database/sql + mattn/go-sqlite3.  Reference for the
// Tin / C / Rust / Crystal counterparts.
//
// Build: go build -o ../../bin/go_inserts .
// Usage: ./go_inserts N

package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const dbPath = "/tmp/go_sqlite_bench.db"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go_inserts N")
		os.Exit(1)
	}
	total, err := strconv.ParseInt(os.Args[1], 10, 64)
	if err != nil {
		log.Fatal(err)
	}

	os.Remove(dbPath)
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_sync=NORMAL")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// Single connection so the bench mirrors the other implementations.
	db.SetMaxOpenConns(1)

	mustExec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			log.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("DROP TABLE IF EXISTS bench")
	mustExec("CREATE TABLE bench (i INTEGER, name TEXT)")
	mustExec("BEGIN")

	stmt, err := db.Prepare("INSERT INTO bench (i, name) VALUES (?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()

	t0 := time.Now()
	for i := int64(0); i < total; i++ {
		if _, err := stmt.Exec(i, "row"); err != nil {
			log.Fatal(err)
		}
	}
	elapsedUs := time.Since(t0).Microseconds()

	mustExec("COMMIT")

	opsPerSec := total * 1000000 / elapsedUs
	fmt.Printf("n=%d elapsed_us=%d ops_per_sec=%d\n", total, elapsedUs, opsPerSec)
}
