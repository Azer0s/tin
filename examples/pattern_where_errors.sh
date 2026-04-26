#!/usr/bin/env bash
# Exercises every compile-error path of the pattern-where feature.
# Each case writes a tiny .tin source, invokes `./tin run`, and checks that
# compilation fails with a specific diagnostic substring. Positive ("should
# compile and run") tests live in examples/pattern_where.tin; this file is
# about error quality.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/pattern_where_errors.sh

set -u

pass=0
fail=0

assert_err() {
  local name=$1
  local want=$2
  local src=$3
  local tmp=$(mktemp --suffix=.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$want"* ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected substring: %s\n' "$want"
    printf '        got: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  fi
}

echo "pattern-where compile-error tests"

# 1. Mixing bool clauses and pattern clauses is rejected.
assert_err "mix: bool then pattern" "cannot mix bool clauses and pattern clauses" '
fn f(n i32) i32 =
  where n < 0: -1
  where (0): 0
  where _: 1
'

assert_err "mix: pattern then bool" "cannot mix bool clauses and pattern clauses" '
fn f(n i32) i32 =
  where (0): 0
  where n > 0: 1
  where _: -1
'

# 2. Non-exhaustive where-lists are rejected.
assert_err "no catch-all (pattern)" "non-exhaustive where" '
fn g(n i32) i32 =
  where (0): 0
  where (1): 1
'

assert_err "no catch-all (bool)" "non-exhaustive where" '
fn g(n i32) i32 =
  where n < 0: -1
  where n > 0: 1
'

assert_err "only guarded patterns (no true catch-all)" "non-exhaustive where" '
fn g(n i32) i32 =
  where (n) if n < 0: -1
  where (n) if n > 0: 1
'

# 3. Arity mismatch between function and clause pattern.
assert_err "single-arg fn, tuple clause" "function takes 1 argument but clause has a 2-element tuple pattern" '
fn h(n i32) i32 =
  where (0, 0): 0
  where _: 1
'

assert_err "multi-arg fn, bare clause" "function takes 2 arguments but clause has a single-pattern" '
fn h(a i32, b i32) i32 =
  where (0): 0
  where _: 1
'

assert_err "multi-arg fn, wrong tuple width" "clause pattern has 3 components" '
fn h(a i32, b i32) i32 =
  where (0, 0, 0): 0
  where _: 1
'

# 4. Type mismatch between pattern literal and arg type.
assert_err "int pattern on string arg" "integer literal pattern used against non-integer argument" '
fn t(s string) string =
  where (0): "z"
  where _: "o"
'

assert_err "bool pattern on int arg" "bool literal pattern used against non-bool argument" '
fn t(n i32) string =
  where (true): "y"
  where _: "o"
'

assert_err "string pattern on int arg" "string literal pattern used against non-string argument" '
fn t(n i32) string =
  where ("x"): "y"
  where _: "o"
'

assert_err "atom pattern on int arg" "atom literal pattern used against non-atom argument" '
fn t(n i32) string =
  where ('\''red): "y"
  where _: "o"
'

assert_err "array pattern on int arg" "array pattern used against non-array argument" '
fn t(n i32) string =
  where ([]): "y"
  where _: "o"
'

# 5. Empty tuple pattern ().
assert_err "empty where pattern" "empty where pattern" '
fn g() i32 =
  where (): 0
  where _: 1
'

# 6. Nested tuple patterns - not supported.
assert_err "nested tuple rejected" "nested tuple patterns are not supported" '
fn g(a i32, b i32) i32 =
  where ((0, 0), _): 0
  where _: 1
'

# 7. Struct patterns in where - slice 2 placeholder.
assert_err "struct pattern placeholder" "struct patterns in where-clauses are not yet supported" '
struct Point =
  x i32
  y i32

fn g(p Point) i32 =
  where (Point{x: 0, y: 0}): 0
  where _: 1
'

echo
printf 'pattern-where error tests: %d passed, %d failed\n' "$pass" "$fail"
exit $([[ $fail -eq 0 ]] && echo 0 || echo 1)
