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

# Coverage: top-level var read inside an array literal element.
assert_err "pure reads global inside array literal" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad() i64 =
  let xs [i64; 3] = [g, 1, 2]
  return xs[0]

fn main() i64 = return bad()
'

# Coverage: top-level var read inside a struct literal field.
assert_err "pure reads global inside struct literal" 'reads mutable top-level var "g"' '
var g i64 = 0

struct foo =
  a i64

fn{#pure} bad() foo = return foo{a: g}

fn main() i64 = return 0
'

# Coverage: top-level var read inside an interpolated string.
assert_err "pure reads global inside interpolated string" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad() i64 =
  let s string = "x={g}"
  return 0

fn main() i64 = return bad()
'

# Coverage: top-level var read inside a match case body.
assert_err "pure reads global inside match case" 'reads mutable top-level var "g"' '
var g i64 = 0

fn{#pure} bad(n i64) i64 =
  match n:
    case 0:
      return g
    default:
      return 0

fn main() i64 = return bad(0)
'

# Coverage: spawn / await are unconditionally rejected (side effects).
assert_err "pure spawn rejected" '#pure violation - spawn' '
fn worker() i64 = return 1

fn{#pure} bad() i64 =
  let f = spawn worker()
  return await f

fn main() i64 = return bad()
'

echo
printf "#pure soundness compile-error tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
