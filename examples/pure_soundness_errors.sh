#!/usr/bin/env bash
# Compile-time rejections for the #pure soundness checker. Each case writes a
# tiny .tin source and checks for a specific error substring. Positive tests
# live in examples/pure_soundness.tin.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/pure_soundness_errors.sh

set -u

pass=0
fail=0

assert_err() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp --suffix=.tin)
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

echo "#pure soundness compile-error tests"

# Reading a top-level mutable var from a #pure body is rejected.
assert_err "pure reads top-level var" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad() i64 = return g + 1

fn main() i64 = return bad()
'

# Writing a top-level mutable var from a #pure body is rejected (the target
# walk catches the read of g, which is sufficient since both need the same name).
assert_err "pure writes top-level var" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad() i64 =
  g = 42
  return g

fn main() i64 = return bad()
'

# Augmented assign target also catches it.
assert_err "pure aug-assigns top-level var" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad() i64 =
  g += 1
  return 0

fn main() i64 = return bad()
'

# Transitive: helper that reads a top-level var taints the #pure caller.
assert_err "pure transitively reads via helper" 'reads mutable top-level var "g"' '
var g i64 = 0

fn helper() i64 = return g

fn{#pure} bad() i64 = return helper()

fn main() i64 = return bad()
'

echo
printf "#pure soundness compile-error tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
