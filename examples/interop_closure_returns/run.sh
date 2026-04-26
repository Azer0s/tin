#!/usr/bin/env bash
# End-to-end test for #interop functions that RETURN a Tin closure to C.
#
# Builds closures.tin as a static object with --emit-header, then
# compiles harness.c against the generated header, runs it, and
# asserts on the printed lines. Pass --valgrind to also run under
# memcheck and require zero definitely/indirectly-lost bytes.
#
# Usage (from repo root):
#   go build -o tin .
#   bash examples/interop_closure_returns/run.sh [--valgrind]

set -u

valgrind_mode=0
for a in "$@"; do
  if [[ "$a" == "--valgrind" ]]; then valgrind_mode=1; fi
done

pass=0
fail=0

check() {
  local name=$1
  local got=$2
  local want=$3

  if [[ "$got" == "$want" ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        want: %s\n' "$want"
    printf '        got:  %s\n' "$got"
    fail=$((fail + 1))
  fi
}

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
src_dir="$repo_root/examples/interop_closure_returns"
tin_bin="$repo_root/tin"

if [[ ! -x "$tin_bin" ]]; then
  printf 'tin binary not found at %s; run `go build -o tin .` first\n' "$tin_bin"
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cp "$src_dir/closures.tin" "$work/closures.tin"
cp "$src_dir/harness.c"    "$work/harness.c"

cd "$work"

if ! "$tin_bin" build -lib closures.tin -o closures.o --emit-header=closures.h >/tmp/build_log 2>&1; then
  printf 'tin build failed:\n'
  cat /tmp/build_log
  exit 1
fi

if ! clang -O1 -o harness harness.c closures.o "$repo_root/runtime/runtime.c" \
    -lpthread -ldl -lm 2>/tmp/clang_log; then
  printf 'clang link failed:\n'
  cat /tmp/clang_log
  exit 1
fi

out=$(./harness 2>&1)

check "single adder returns base+x"          "$(echo "$out" | grep '^add5(10)' | head -1)"  "add5(10)=15"
check "two adders independent"               "$(echo "$out" | grep '^add5(1)')"             "add5(1)=6 add42(1)=43 add5(7)=12 add42(8)=50"
check "stateful counter increments"          "$(echo "$out" | grep ^ctr=)"                  "ctr=1 2 3 4"
check "independent counters"                 "$(echo "$out" | grep ^ctr2=)"                 "ctr2=1 ctr=5 ctr2=2"
check "freed slot is reusable"               "$(echo "$out" | grep '^mul3(')"               "mul3(7)=21"
# 256*1000 + (255*256/2) = 288640
check "256 concurrent live trampolines"      "$(echo "$out" | grep ^stress_sum=)"           "stress_sum=288640"
check "string-returning closure marshaling"  "$(echo "$out" | grep ^greet=)"                "greet=hello ariel|goodbye ariel|hello world"
check "bool-returning closure marshaling"    "$(echo "$out" | grep ^pred=)"                 "pred=0 0 1"
check "nested trampoline calls"              "$(echo "$out" | grep ^nested=)"               "nested=1014"
# strlen("hello world") = 11; total = 10000 * 11 = 110000
check "10k string-param calls (no leak)"     "$(echo "$out" | grep ^string_stress=)"        "string_stress=110000"
check "double-free is a no-op"               "$(echo "$out" | grep ^double_free_)"          "double_free_distinct=1 slots=1 2"
check "bogus pointers safely ignored"        "$(echo "$out" | grep ^bogus_safe)"            "bogus_safe after=107"
check "leaked trampolines reported"          "$(echo "$out" | grep ^leaked=)"               "leaked=5"
check "harness completes cleanly"            "$(echo "$out" | grep ^done)"                  "done"

if [[ "$valgrind_mode" -eq 1 ]]; then
  if ! command -v valgrind >/dev/null 2>&1; then
    printf '  SKIP  valgrind run (valgrind not installed)\n'
  else
    vg_out=$(valgrind --leak-check=full --error-exitcode=99 ./harness 2>&1)
    vg_rc=$?

    definitely=$(echo "$vg_out" | sed -n 's/.*definitely lost: \([0-9,]*\) bytes.*/\1/p')
    indirectly=$(echo "$vg_out" | sed -n 's/.*indirectly lost: \([0-9,]*\) bytes.*/\1/p')
    check "valgrind: 0 definitely lost" "$definitely" "0"
    check "valgrind: 0 indirectly lost" "$indirectly" "0"
    if [[ "$vg_rc" -ne 0 && "$vg_rc" -ne 99 ]]; then
      printf '  FAIL  valgrind run produced unexpected exit code %d\n' "$vg_rc"
      fail=$((fail + 1))
    fi
  fi
fi

printf '#interop closure-return tests: %d passed, %d failed\n' "$pass" "$fail"
exit $fail
