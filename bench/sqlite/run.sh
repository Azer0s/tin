#!/usr/bin/env bash
# bench/sqlite/run.sh - build all five INSERT benchmarks, run them at
# N=10k / 100k / 1M, print ops/sec for each language side by side.
#
# Requires: clang, ./tin (built at repo root), cargo, go, crystal,
# shards.  hyperfine optional (not used here; we re-run via internal
# us measurements with cold dbs each time).

set -euo pipefail
cd "$(dirname "$0")/../.."

BIN=bench/bin
mkdir -p "$BIN"

echo "--- build ---"
clang -O2 bench/sqlite/c_inserts.c -lsqlite3 -o "$BIN/c_inserts"
./tin build bench/sqlite/tin_inserts.tin -o "$BIN/tin_inserts" 2>&1 \
  | grep -E "error|warning" || true
./tin build bench/sqlite/tin_inserts_blocking.tin -o "$BIN/tin_inserts_blocking" 2>&1 \
  | grep -E "error|warning" || true
./tin build bench/sqlite/tin_inserts_prepared.tin -o "$BIN/tin_inserts_prepared" 2>&1 \
  | grep -E "error|warning" || true
(cd bench/sqlite/go && go build -o ../../bin/go_inserts .)
(cd bench/sqlite/rs && cargo build --release --quiet && cp target/release/rs_inserts ../../bin/)
(cd bench/sqlite/cr && shards install --quiet && crystal build --release inserts.cr -o ../../bin/cr_inserts)

echo
echo "--- run ---"
for N in 10000 100000 1000000; do
  echo "N=$N"
  for prog in c_inserts rs_inserts cr_inserts go_inserts tin_inserts tin_inserts_blocking tin_inserts_prepared; do
    # Each binary uses its own bench db so we wipe the matching path.
    case "$prog" in
      tin_inserts_blocking) rm -f /tmp/tin_sqlite_bench_blocking.db ;;
      tin_inserts_prepared) rm -f /tmp/tin_sqlite_bench_prepared.db ;;
      tin_inserts)          rm -f /tmp/tin_sqlite_bench.db ;;
      *)                    rm -f /tmp/${prog%_inserts}_sqlite_bench.db ;;
    esac
    printf "  %-21s " "$prog"
    "$BIN/$prog" "$N"
  done
done
