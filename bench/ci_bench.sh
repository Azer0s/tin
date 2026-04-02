#!/usr/bin/env bash
# bench/ci_bench.sh — build tin+go benchmarks, run 3x, compare Tin/Go ratios.
#
# Thresholds are expressed as ratios against Go measured in the same run, so
# CI runner speed variation cancels out.  Both binaries see the same load and
# the same scheduler pressure at the same moment, making the ratio stable.
#
# Current dev-machine ratios (approximate):
#   pingpong   Tin ~110ns  Go ~290ns  → Tin is ~2.6x faster  (ratio 0.38)
#   pipeline   Tin ~275ns  Go ~720ns  → Tin is ~2.6x faster  (ratio 0.38)
#   mpmc       Tin ~3.3M   Go ~17M    → Tin is ~0.19x Go     (ratio 0.19)
#   jitter     Tin ~1.4M   Go ~2.7M   → Tin is ~0.52x Go     (ratio 0.52)
#
# Regression thresholds (generous: catch >5x degradation, not noise):
#   latency:   Tin must be <= Go * 4   (fail if Tin becomes >4x slower than Go)
#   mpmc:      Tin must be >= Go * 0.02 (fail if Tin drops below 2% of Go)
#   jitter:    Tin must be >= Go * 0.05 (fail if Tin drops below 5% of Go)
#
# Exit code: 0 = all ratios within bounds, 1 = regression detected.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"
TIN="${ROOT_DIR}/tin"

mkdir -p "$BIN_DIR"

echo "=== Building tin benchmarks ==="
for bench in bench pipeline mpmc jitter; do
    "$TIN" build "$SCRIPT_DIR/tin/${bench}.tin" -o "$BIN_DIR/bench_tin_${bench}"
done

echo "=== Building go benchmarks ==="
(cd "$SCRIPT_DIR/go" && go build -o "$BIN_DIR/bench_go" .)
echo ""

# Run a command 3 times, extract the first integer matching $pattern, return median.
median3() {
    local cmd="$1" pattern="$2"
    local vals=()
    for _ in 1 2 3; do
        v=$(eval "$cmd" | grep -oP "$pattern" | head -1)
        vals+=("$v")
    done
    printf '%s\n' "${vals[@]}" | sort -n | sed -n '2p'
}

FAIL=0

# check_ratio label tin_val go_val direction threshold
# direction "lo": tin_val <= go_val * threshold  (latency: lower is better)
# direction "hi": tin_val >= go_val * threshold  (throughput: higher is better)
check_ratio() {
    local label="$1" tv="$2" gv="$3" dir="$4" thr_num="$5" thr_den="$6"
    local ratio_pct=$(( tv * 100 / gv ))
    local threshold_pct=$(( thr_num * 100 / thr_den ))
    local result="ok"
    if [ "$dir" = "lo" ] && [ "$tv" -gt $(( gv * thr_num / thr_den )) ]; then
        result="REGRESSION (tin=${tv}, go=${gv}, ratio=${ratio_pct}% > threshold ${threshold_pct}%)"
        FAIL=1
    elif [ "$dir" = "hi" ] && [ "$tv" -lt $(( gv * thr_num / thr_den )) ]; then
        result="REGRESSION (tin=${tv}, go=${gv}, ratio=${ratio_pct}% < threshold ${threshold_pct}%)"
        FAIL=1
    fi
    printf "  %-40s tin=%-10s go=%-10s ratio=%3d%%  %s\n" \
        "$label" "$tv" "$gv" "$ratio_pct" "$result"
}

echo "=== Benchmark results (3-run medians) ==="
echo ""

# Pingpong latency (ns/round-trip). Tin must be <= Go * 4.
tin_pp=$(median3 "$BIN_DIR/bench_tin_bench"    '(?<=latency: ~)\d+')
go_pp=$(median3  "$BIN_DIR/bench_go bench"     '(?<=latency: ~)\d+')
echo "pingpong (1M round trips, ns/round-trip, lower=better):"
check_ratio "channel round-trip latency" "$tin_pp" "$go_pp" lo 4 1

echo ""

# Pipeline latency (ns/pass). Tin must be <= Go * 4.
tin_pl=$(median3 "$BIN_DIR/bench_tin_pipeline"  '(?<=latency: ~)\d+')
go_pl=$(median3  "$BIN_DIR/bench_go pipeline"   '(?<=latency: ~)\d+')
echo "pipeline (1M passes, 4 stages, ns/pass, lower=better):"
check_ratio "pipeline pass latency"      "$tin_pl" "$go_pl" lo 4 1

echo ""

# MPMC throughput (msgs/sec). Tin must be >= Go * 0.02.
tin_mp=$(median3 "$BIN_DIR/bench_tin_mpmc"  '(?<=throughput: ~)\d+')
go_mp=$(median3  "$BIN_DIR/bench_go mpmc"   '(?<=throughput: ~)\d+')
echo "MPMC (1M msgs, 4P+4C, msgs/sec, higher=better):"
check_ratio "throughput"                 "$tin_mp" "$go_mp" hi 1 50

echo ""

# Jitter throughput (tasks/sec). Tin must be >= Go * 0.05.
tin_jt=$(median3 "$BIN_DIR/bench_tin_jitter"  '(?<=throughput: ~)\d+')
go_jt=$(median3  "$BIN_DIR/bench_go jitter"   '(?<=throughput: ~)\d+')
echo "jitter (1M tasks, 8 workers, 0-3 yields, tasks/sec, higher=better):"
check_ratio "throughput"                 "$tin_jt" "$go_jt" hi 1 20

echo ""

if [ "$FAIL" -eq 0 ]; then
    echo "All benchmarks within bounds."
else
    echo "REGRESSION: one or more benchmarks fell outside acceptable range vs Go."
fi
exit "$FAIL"
