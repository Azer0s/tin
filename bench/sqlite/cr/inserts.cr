# bench/sqlite/cr/inserts - INSERT N rows sequentially against one
# crystal-sqlite3 DB.  Counterpart of c_inserts.c, tin_inserts.tin,
# go/inserts.go, rs/src/main.rs.
#
# Build (from this dir):
#   shards install
#   crystal build --release inserts.cr -o ../../bin/cr_inserts
# Usage: ./cr_inserts N

require "db"
require "sqlite3"

DB_PATH = "/tmp/cr_sqlite_bench.db"

if ARGV.size < 1
  STDERR.puts "usage: cr_inserts N"
  exit 1
end

total = ARGV[0].to_i64
File.delete?(DB_PATH)

DB.open("sqlite3://#{DB_PATH}?journal_mode=WAL&synchronous=NORMAL") do |db|
  db.exec "DROP TABLE IF EXISTS bench"
  db.exec "CREATE TABLE bench (i INTEGER, name TEXT)"
  db.exec "BEGIN"

  sql = "INSERT INTO bench (i, name) VALUES (?, ?)"

  t0 = Time.monotonic
  total.times do |i|
    db.exec sql, i, "row"
  end
  elapsed_us = (Time.monotonic - t0).total_microseconds.to_i64

  db.exec "COMMIT"

  ops_per_sec = total * 1_000_000 // elapsed_us
  puts "n=#{total} elapsed_us=#{elapsed_us} ops_per_sec=#{ops_per_sec}"
end
