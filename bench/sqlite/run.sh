#!/usr/bin/env bash
# bench/sqlite/run.sh - build the INSERT benchmarks across five
# languages, run them at N=10k / 100k / 1M, print ops/sec.

set -euo pipefail
cd "$(dirname "$0")/../.."

BIN=bench/bin
mkdir -p "$BIN"

echo "--- build ---"
clang -O2 bench/sqlite/c_inserts.c -lsqlite3 -o "$BIN/c_inserts"
./tin build bench/sqlite/tin_inserts.tin      -o "$BIN/tin_inserts"      2>&1 \
  | grep -E "error|warning" || true
./tin build bench/sqlite/tin_inserts_stmt.tin -o "$BIN/tin_inserts_stmt" 2>&1 \
  | grep -E "error|warning" || true
(cd bench/sqlite/go && go build -o ../../bin/go_inserts .)
(cd bench/sqlite/rs && cargo build --release --quiet && cp target/release/rs_inserts ../../bin/)
(cd bench/sqlite/cr && shards install --quiet && crystal build --release inserts.cr -o ../../bin/cr_inserts)

echo
echo "--- run ---"
for N in 10000 100000 1000000; do
  echo "N=$N"
  for prog in c_inserts rs_inserts cr_inserts go_inserts tin_inserts tin_inserts_stmt; do
    case "$prog" in
      tin_inserts_stmt) rm -f /tmp/tin_sqlite_bench_stmt.db ;;
      tin_inserts)      rm -f /tmp/tin_sqlite_bench.db ;;
      *)                rm -f /tmp/${prog%_inserts}_sqlite_bench.db ;;
    esac
    printf "  %-18s " "$prog"
    "$BIN/$prog" "$N"
  done
done
