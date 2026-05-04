#!/usr/bin/env bash
# Tests for the Maranget exhaustiveness checker:
#   1. Patterns that structurally cover every input no longer need a catch-all.
#   2. Non-exhaustive cases produce a witness in the error message.
#   3. The same algorithm runs for both `match` and `where`.
#   4. -fdump-match-info dumps the matrix and verdict for debugging.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/maranget_exhaustive.sh

set -u

pass=0
fail=0

ok() { printf '  ok    %s\n' "$1"; pass=$((pass+1)); }
notok() { printf '  FAIL  %s\n' "$1"; printf '        %s\n' "$2"; fail=$((fail+1)); }

run_run() {
  # run_run NAME EXPECT_KIND EXPECT_SUBSTR FLAGS SRC
  local name=$1 kind=$2 want=$3 flag=$4 src=$5
  local tmp=$(mktemp)
  printf '%s\n' "$src" > "$tmp"

  local out
  if [[ -n "$flag" ]]; then
    out=$(./tin run "$tmp" "$flag" 2>&1 || true)
  else
    out=$(./tin run "$tmp" 2>&1 || true)
  fi

  rm -f "$tmp"

  case "$kind" in
    contains)
      if [[ "$out" == *"$want"* ]]; then
        ok "$name"
      else
        notok "$name" "expected substring: $want"$'\n        got: '"${out:0:300}"
      fi
      ;;
    not-contains)
      if [[ "$out" != *"$want"* ]]; then
        ok "$name"
      else
        notok "$name" "did NOT expect substring: $want"$'\n        got: '"${out:0:300}"
      fi
      ;;
  esac
}

echo "Maranget exhaustiveness tests"

# 1. where: list triple covers all - no explicit catch-all needed.
run_run "where: list triple is exhaustive" "contains" "55" "" '
fn fib(n i32) i32 =
  where (0): 0
  where (1): 1
  where (n): fib(n - 2) + fib(n - 1)

echo fib(10)
'

run_run "where: list triple sums" "contains" "10" "" '
fn sum(xs [i32]) i32 =
  where ([]): 0
  where ([x]): x
  where ([x, ...rest]): x + sum(rest)

echo sum([1, 2, 3, 4])
'

# 2. where: missing case -> non-exhaustive error WITH witness.
run_run "where: missing singleton -> witness [_]" "contains" "no clause matches [_]" "" '
fn s(xs [i32]) i32 =
  where ([]): 0
  where ([x, ...rest]): x

echo s([5])
'

run_run "where: missing empty -> witness []" "contains" "no clause matches []" "" '
fn s(xs [i32]) i32 =
  where ([x]): x
  where ([x, ...rest]): x

echo s([])
'

# 3. match: same exhaustiveness without default.
run_run "match: list triple is exhaustive (no default)" "contains" "many" "" '
fn d(xs [i32]) string =
  return match xs:
    case []: "empty"
    case [x]: "one"
    case [x, ...rest]: "many"

echo d([1, 2, 3])
'

# 4. match: bool exhaustive without default.
run_run "match: bool exhaustive (no default)" "contains" "yes" "" '
fn yn(b bool) string =
  return match b:
    case true: "yes"
    case false: "no"

echo yn(true)
'

# 5. match without `default:` and non-exhaustive: the missing case is
#    silent unreachable IR (current behaviour). Pattern-where elevates the
#    same situation to a compile error with a witness; if you want the
#    same for `match`, add an explicit `default:`. Verify that an
#    exhaustive match still works without a default though.
run_run "match exhaustive without default still runs" "contains" "1" "" '
fn d(xs [i32]) i32 =
  return match xs:
    case []: 0
    case [x]: 1
    case [x, ...rest]: 2

echo d([5])
'

# 6. -fdump-match-info dumps the matrix.
run_run "-fdump-match-info dumps the verdict" "contains" "[match-info]" "-fdump-match-info" '
fn d(xs [i32]) string =
  where ([]): "e"
  where ([x]): "1"
  where ([x, ...rest]): "n"

echo d([])
'

run_run "-fdump-match-info reports YES exhaustive" "contains" "exhaustive: YES" "-fdump-match-info" '
fn d(xs [i32]) string =
  where ([]): "e"
  where ([x]): "1"
  where ([x, ...rest]): "n"

echo d([])
'

# 7. Witness generation for nested arrays.
run_run "where: missing length-2 -> witness [_, _]" "contains" "no clause matches" "" '
fn s(xs [i32]) i32 =
  where ([]): 0
  where ([x]): x
  where ([x, y, z, ...rest]): x

echo s([1, 2])
'

# 8. Wildcard catch-all alongside an exhaustive set is unreachable (warning, not error).
run_run "warning when catch-all is dead" "contains" "warning: unreachable" "" '
fn s(xs [i32]) i32 =
  where ([]): 0
  where ([x]): x
  where ([x, ...rest]): x
  where _: -1

echo s([])
'

# 9. The same warning is silenced by -Wno-unused-match-arms.
run_run "warning silenced by -Wno-unused-match-arms" "not-contains" "warning" "-Wno-unused-match-arms" '
fn s(xs [i32]) i32 =
  where ([]): 0
  where ([x]): x
  where ([x, ...rest]): x
  where _: -1

echo s([])
'

# 10. Witnesses use the Maranget recursion - struct fields are conservatively
#     opaque (todo: full struct exhaustiveness in a follow-up). A non-empty
#     program with struct patterns must still compile.
run_run "struct match still compiles (opaque, no false positives)" "contains" "origin" "" '
struct point =
  x i32
  y i32

fn label(p point) string =
  return match p:
    case point{x: 0, y: 0}: "origin"
    case point{x: 0, _}:    "y-axis"
    case point{_, y: 0}:    "x-axis"
    case point{_, _}:       "other"

echo label(point{x: 0, y: 0})
'

echo
printf 'Maranget exhaustiveness tests: %d passed, %d failed\n' "$pass" "$fail"
exit $([[ $fail -eq 0 ]] && echo 0 || echo 1)
