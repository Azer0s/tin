#!/usr/bin/env bash
# Compile-time IR assertions for #pure folding.
#
# These tests confirm that the AST-level CTFE evaluator collapses #pure calls
# into LLVM constants, instead of letting the call survive to runtime. They
# back the correctness assertions in examples/pure_soundness.tin with proof
# that folding actually happens.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/pure_fold_ir_checks.sh

set -u

pass=0
fail=0

assert_ir() {
  local name=$1
  local pattern=$2
  local antipattern=$3
  local src=$4
  local tmp ir
  tmp=$(mktemp)
  printf '%s\n' "$src" > "$tmp"
  ir=$(./tin ir "$tmp" 2>&1 || true)
  rm -f "$tmp"
  local ok=1
  if [[ -n "$pattern" ]] && ! grep -qE "$pattern" <<< "$ir"; then
    ok=0
  fi
  if [[ -n "$antipattern" ]] && grep -qE "$antipattern" <<< "$ir"; then
    ok=0
  fi
  if (( ok )); then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    [[ -n "$pattern" ]]      && printf '        expected match:    %s\n' "$pattern"
    [[ -n "$antipattern" ]]  && printf '        forbidden match:   %s\n' "$antipattern"
    fail=$((fail + 1))
  fi
}

# Extract just the body of @_tin_user_main (program entry) so the assertions
# do not also match against the #pure function definitions themselves.
extract_user_main() {
  awk '/^define .* @_tin_user_main/,/^}/' <<< "$1"
}

assert_main_ir() {
  local name=$1
  local pattern=$2
  local antipattern=$3
  local src=$4
  local tmp ir main
  tmp=$(mktemp)
  printf '%s\n' "$src" > "$tmp"
  ir=$(./tin ir "$tmp" 2>&1 || true)
  rm -f "$tmp"
  main=$(extract_user_main "$ir")
  local ok=1
  if [[ -n "$pattern" ]] && ! grep -qE "$pattern" <<< "$main"; then
    ok=0
  fi
  if [[ -n "$antipattern" ]] && grep -qE "$antipattern" <<< "$main"; then
    ok=0
  fi
  if (( ok )); then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    [[ -n "$pattern" ]]      && printf '        expected match:    %s\n' "$pattern"
    [[ -n "$antipattern" ]]  && printf '        forbidden match:   %s\n' "$antipattern"
    printf '        @_tin_user_main body:\n%s\n' "$main"
    fail=$((fail + 1))
  fi
}

echo "#pure IR-fold checks"

# Recursive #pure fact(10) folds to 3628800 in main - validates depth-limit-only
# strategy from phase B (no #no_recurse needed).
assert_main_ir "fact(10) folds to 3628800" 'ret i64 3628800' 'call i64 @fact' '
fn{#pure} fact(n i64) i64 =
  if n <= 1:
    return 1
  return n * fact(n - 1)

fn main() i64 = return fact(10)
'

# Memoization: each cube(3) call site folds to the same constant 27 in main.
assert_main_ir "memoised cube(3) call sites all fold" 'i64 27' 'call i64 @cube' '
fn{#pure} cube(n i64) i64 = return n * n * n

fn main() i64 =
  let a = cube(3) + cube(3) + cube(3)
  return a
'

# {#allow_sideffect} blocks must NOT fold - the side effect has to run.
assert_main_ir "allow_sideffect prevents fold" 'call i64 @traced' '' '
fn{#pure} traced(n i64) i64 =
  let r i64 = n * n
  { #allow_sideffect } {
    echo "n={n}"
  }
  return r

fn main() i64 = return traced(4)
'

# alwaysinline + readnone + nounwind attributes are emitted for strict #pure.
# (linkage keyword `internal` is unrelated to the optimizer hints.)
assert_ir "strict #pure gets alwaysinline+readnone+nounwind" 'define [a-z ]*i64 @sq.*alwaysinline readnone nounwind' '' '
fn{#pure} sq(n i64) i64 = return n * n

fn main() i64 = return sq(7)
'

# #pure with #allow_sideffect gets alwaysinline only (no readnone).
assert_ir "#pure + allow_sideffect: alwaysinline without readnone" 'define [a-z ]*i64 @noisy.* alwaysinline \{' 'define [a-z ]*i64 @noisy.*readnone' '
fn{#pure} noisy(n i64) i64 =
  { #allow_sideffect } { echo "n={n}" }
  return n + 1

fn main() i64 = return noisy(3)
'

echo
printf "#pure IR-fold checks: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
