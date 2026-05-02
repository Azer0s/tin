#!/usr/bin/env bash
# bench/run.sh - build and benchmark channel patterns across Go, Crystal, Rust, Tin.
#
# Benchmarks:
#   pingpong  - 2 fibers, 1M round trips (SPSC latency baseline)
#   pipeline  - 4 relay fibers in series, 1M passes (multi-hop latency)
#   mpmc      - 4 producers + 4 consumers, 1M msgs (MPMC throughput)
#   jitter    - 8 workers, variable 0-3 yields/task, 1M tasks (scheduler stress)
#
# Requires: go, crystal, cargo, tin (built at ../tin)
# Optional: hyperfine (cargo install hyperfine)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"
mkdir -p "$BIN_DIR"

echo "=== Building ==="

echo "  go..."
(cd "$SCRIPT_DIR/go" && go build -o "$BIN_DIR/bench_go" .)

echo "  crystal..."
for bench in bench pipeline mpmc jitter pipeline10 fanout; do
    src="$SCRIPT_DIR/crystal/${bench}.cr"
    [ -f "$src" ] || src="$SCRIPT_DIR/crystal/bench.cr"
    crystal build --release "$SCRIPT_DIR/crystal/${bench}.cr" -o "$BIN_DIR/bench_crystal_${bench}" 2>/dev/null \
        || crystal build --release "$SCRIPT_DIR/crystal/bench.cr" -o "$BIN_DIR/bench_crystal_${bench}"
done
# canonical name for pingpong
cp "$BIN_DIR/bench_crystal_bench" "$BIN_DIR/bench_crystal"

echo "  rust..."
(cd "$SCRIPT_DIR/rust" && cargo build --release -q)
for bin in bench_rust bench_rust_pipeline bench_rust_mpmc bench_rust_jitter bench_rust_pipeline10 bench_rust_fanout; do
    cp "$SCRIPT_DIR/rust/target/release/$bin" "$BIN_DIR/$bin"
    chmod +x "$BIN_DIR/$bin"
done
cp "$BIN_DIR/bench_rust" "$BIN_DIR/bench_rust_bench"
chmod +x "$BIN_DIR/bench_rust_bench"

echo "  tin..."
for bench in bench pipeline mpmc jitter pipeline10 fanout; do
    "$ROOT_DIR/tin" build "$SCRIPT_DIR/tin/${bench}.tin" -o "$BIN_DIR/bench_tin_${bench}"
done
cp "$BIN_DIR/bench_tin_bench" "$BIN_DIR/bench_tin"
chmod +x "$BIN_DIR/bench_tin"

echo ""

run_bench() {
    local name="$1" label="$2" metric="$3"
    echo "--- $label ---"
    for lang in go crystal rust tin; do
        printf "  %-10s " "$lang"
        case "$lang" in
            go)      "$BIN_DIR/bench_go" "$name" ;;
            crystal) "$BIN_DIR/bench_crystal_${name}" ;;
            rust)    "$BIN_DIR/bench_rust_${name}" ;;
            tin)     "$BIN_DIR/bench_tin_${name}" ;;
        esac | grep "$metric"
    done
    echo ""
}

# Locate hyperfine: prefer PATH, fall back to asdf rust installs.
HYPERFINE=$(command -v hyperfine 2>/dev/null || \
    ls "$HOME"/.asdf/installs/rust/*/bin/hyperfine 2>/dev/null | tail -1 || true)

if [ -n "$HYPERFINE" ] && [ -x "$HYPERFINE" ]; then
    for bench in bench pipeline mpmc jitter pipeline10 fanout; do
        case "$bench" in
            bench)      label="Pingpong (1M round trips)" ;;
            pipeline)   label="Pipeline (1M passes, 4 stages)" ;;
            mpmc)       label="MPMC (1M msgs, 4P+4C)" ;;
            jitter)     label="Jitter (1M tasks, 8 workers, 0-3 yields)" ;;
            pipeline10) label="Pipeline10 (500K passes, 10 stages)" ;;
            fanout)     label="Fanout (1M items, 8 workers)" ;;
        esac
        echo "=== $label ==="
        "$HYPERFINE" \
            --warmup 2 \
            --export-markdown "$SCRIPT_DIR/results_${bench}.md" \
            --command-name go      "$BIN_DIR/bench_go ${bench}" \
            --command-name crystal "$BIN_DIR/bench_crystal_${bench}" \
            --command-name rust    "$BIN_DIR/bench_rust_${bench}" \
            --command-name tin     "$BIN_DIR/bench_tin_${bench}"
        echo ""
    done
    echo "Results written to bench/results_*.md"
else
    echo "hyperfine not found - install with: yay -S hyperfine  (or: cargo install hyperfine)"
    echo "Running each benchmark once:"
    echo ""
    run_bench bench      "pingpong (1M round trips)"                    "latency"
    run_bench pipeline   "pipeline (1M passes, 4 stages)"               "latency"
    run_bench mpmc       "MPMC (1M msgs, 4P+4C)"                        "throughput"
    run_bench jitter     "jitter (1M tasks, 8 workers, 0-3 yields)"     "throughput"
    run_bench pipeline10 "pipeline10 (500K passes, 10 stages)"          "latency"
    run_bench fanout     "fanout (1M items, 8 workers)"                  "throughput"
fi
