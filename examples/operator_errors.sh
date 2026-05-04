#!/usr/bin/env bash
# Compile-time rejections for binary operators and index expressions on
# types with no defined operator behavior.
#
# Phase 0 of docs/plans/operator-overloading.md: locks down the silent
# `genBinExpr -> i64 0` and `genIndexExpr -> nil` fall-throughs that
# previously caused nonsense codegen for `Foo + Foo` etc. Once operator
# overloading lands these errors get suppressed when the relevant
# operator trait is implemented; until then, every bad operator usage
# must be a clean compile error.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/operator_errors.sh

set -u

pass=0
fail=0

assert_err() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp)
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

echo "operator/index lockdown tests"

# ── struct + struct rejected ──
assert_err "struct + struct rejected" "binary operator" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let b = point{x: 2}
  let _ = a + b
  return 0
'

# ── struct - struct rejected ──
assert_err "struct - struct rejected" "binary operator" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let b = point{x: 2}
  let _ = a - b
  return 0
'

# ── struct * struct rejected ──
assert_err "struct * struct rejected" "binary operator" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let b = point{x: 2}
  let _ = a * b
  return 0
'

# ── struct / struct rejected ──
assert_err "struct / struct rejected" "binary operator" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let b = point{x: 2}
  let _ = a / b
  return 0
'

# ── mixed struct + primitive rejected ──
assert_err "struct + i64 rejected" "binary operator" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let _ = a + 5
  return 0
'

# ── struct[index] rejected for struct without index trait ──
assert_err "struct[i64] rejected" "does not support index expressions" '
struct point =
  x i32

fn main() i64 =
  let a = point{x: 1}
  let _ = a[0]
  return 0
'

# ── verify primitives still work (negative regression check) ──
# This MUST succeed; if it fails, the lockdown is over-eager.
tmp=$(mktemp)
printf 'fn main() i64 =\n  let a = 1 + 2\n  let b = a * 3\n  let arr [i64] = [10, 20]\n  return b + arr[0]\n' > "$tmp"
out=$(./tin run "$tmp" 2>&1 || true)
rm -f "$tmp"
if [[ "$out" == *"error"* || "$out" == *"FAIL"* ]]; then
  printf '  FAIL  primitives still work\n'
  printf '        got: %s\n' "${out:0:400}"
  fail=$((fail + 1))
else
  printf '  ok    primitives still work (i64 arithmetic + array indexing)\n'
  pass=$((pass + 1))
fi

printf 'operator/index lockdown tests: %d passed, %d failed\n' "$pass" "$fail"
exit $fail
