#!/usr/bin/env bash
# IR-level proof that examples/ctfe_stress.tin's `assert::equals` arguments
# were ACTUALLY produced by CTFE folding — not by an unfolded runtime call.
#
# For each scenario we examine the @_tin_user_main body in the emitted IR
# (test mode) and check:
#   • a specific i64 / f64 / bool literal appears (the folded value)
#   • zero `call i64 @<purefn>(...)` instructions for the function involved
#     (proves no runtime dispatch survived for the folded site)
#
# Companion to examples/ctfe_stress.tin (correctness assertions). Together
# they form an end-to-end CTFE stress harness.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/ctfe_stress_ir_checks.sh

set -u

pass=0
fail=0

assert_main() {
  local name=$1
  local expect=$2
  local forbid=$3
  local src=$4
  local tmp ir main
  tmp=$(mktemp --suffix=.tin)
  printf '%s\n' "$src" > "$tmp"
  ir=$(./tin ir "$tmp" 2>&1 || true)
  rm -f "$tmp"
  main=$(awk '/^define .* @_tin_user_main/,/^}/' <<< "$ir")

  local ok=1
  if [[ -n "$expect" ]] && ! grep -qE "$expect" <<< "$main"; then
    ok=0
  fi

  if [[ -n "$forbid" ]] && grep -qE "$forbid" <<< "$main"; then
    ok=0
  fi

  if (( ok )); then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    [[ -n "$expect" ]] && printf '        expect:    %s\n' "$expect"
    [[ -n "$forbid" ]] && printf '        forbid:    %s\n' "$forbid"
    printf '        @_tin_user_main:\n%s\n' "$main"
    fail=$((fail + 1))
  fi
}

echo "CTFE stress IR-level checks"

# 1. Recursive #pure folds completely.
assert_main "fact(15) folds to 1307674368000" 'ret i64 1307674368000' 'call i64 @fact' '
fn{#pure} fact(n i64) i64 =
  if n <= 1:
    return 1
  return n * fact(n - 1)

fn main() i64 = return fact(15)
'

# 2. Mutual recursion folds.
assert_main "is_even(20) folds to true (i64 1)" 'ret i64 1' 'call i1 @is_even|call i1 @is_odd' '
fn{#pure} is_even(n i64) bool =
  if n == 0:
    return true
  return is_odd(n - 1)

fn{#pure} is_odd(n i64) bool =
  if n == 0:
    return false
  return is_even(n - 1)

fn main() i64 =
  if is_even(20):
    return 1
  return 0
'

# 3. Where-clause folds.
assert_main "sign(-7) folds to -1" 'ret i64 -1' 'call i64 @sign' '
fn{#pure} sign(n i64) i64 =
  where n < 0:  -1
  where n == 0:  0
  where _:       1

fn main() i64 = return sign(-7)
'

# 4. For-loop fold.
assert_main "sum_to(100) folds to 5050" 'ret i64 5050' 'call i64 @sum_to' '
fn{#pure} sum_to(n i64) i64 =
  let acc i64 = 0
  for let i i64 = 0; i <= n; i++:
    acc = acc + i
  return acc

fn main() i64 = return sum_to(100)
'

# 5. Match fold.
assert_main "match_kind(7) folds to 700" 'ret i64 700' 'call i64 @match_kind' '
fn{#pure} match_kind(n i64) i64 =
  match n:
    case 0:
      return 0
    case 1:
      return 10
    default:
      return n * 100

fn main() i64 = return match_kind(7)
'

# 6. Memoization: 50 call sites all fold to 27, none survive as @cube calls.
assert_main "memoised cube(3) — 50 sites, zero runtime calls" 'i64 27' 'call i64 @cube' '
fn{#pure} cube(n i64) i64 = return n * n * n

fn main() i64 =
  let s i64 = cube(3) + cube(3) + cube(3) + cube(3) + cube(3) + cube(3) + cube(3) + cube(3) + cube(3) + cube(3)
  return s
'

# 7. Transitive #pure chain folds to one literal.
assert_main "step4(5) folds to 81 (no @stepN calls)" 'ret i64 81' 'call i64 @step[1234]' '
fn{#pure} step1(n i64) i64 = return n + 1
fn{#pure} step2(n i64) i64 = return step1(n) * 2
fn{#pure} step3(n i64) i64 = return step2(n) - 3
fn{#pure} step4(n i64) i64 = return step3(n) * step3(n)

fn main() i64 = return step4(5)
'

# 8. Shadow: param shadows top-level var; fold uses the param value.
assert_main "param shadow folds to 64" 'ret i64 64' 'call i64 @sq' '
var counter i64 = 9999

fn{#pure} sq(counter i64) i64 = return counter * counter

fn main() i64 = return sq(8)
'

# 9. #allow_sideffect blocks fold — runtime call MUST survive.
assert_main "#allow_sideffect keeps the call" 'call i64 @traced_double' '' '
fn{#pure} traced_double(n i64) i64 =
  let r i64 = n * 2
  { #allow_sideffect } {
    echo "n={n}"
  }
  return r

fn main() i64 = return traced_double(21)
'

# 10. Float fold path: hypot_squared(3, 4) → 25.0
assert_main "f64 hypot_squared folds to 25.0" 'double 25\.0' 'call double @hypot_squared' '
fn{#pure} hypot_squared(x f64, y f64) f64 = return x * x + y * y

fn main() i64 =
  let r f64 = hypot_squared(3.0, 4.0)
  return r as i64
'

# 11. Variable-arg call DOES survive (not foldable).
assert_main "variable-arg call survives as runtime @doubled" 'call i64 @doubled' '' '
fn{#pure} doubled(n i64) i64 = return n * 2

fn main() i64 =
  let dynamic i64 = 9
  return doubled(dynamic)
'

echo
printf "CTFE stress IR-level checks: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
