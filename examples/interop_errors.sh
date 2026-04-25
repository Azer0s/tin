#!/usr/bin/env bash
# Compile-time rejections for the #interop control tag (Phase A: validation).
# Each case writes a tiny .tin source and checks for a specific error
# substring.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/interop_errors.sh

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

echo "#interop validation tests"

# ── #interop and #async are mutually exclusive ──
assert_err "interop+async rejected" "#interop and #async cannot be combined" '
fn{#interop #async} bad() i32 = return 0

fn main() i64 = return 0
'

# ── return type cannot contain Future[T] ──
assert_err "Future return rejected" "must not contain Future[T]" '
use sync

fn{#interop} bad(n i32) Future[i32] =
  return Future[i32]{pid: 0}

fn main() i64 = return 0
'

# ── any parameter rejected ──
assert_err "any param rejected" "contains \`any\`" '
fn{#interop} bad(x any) i32 = return 0

fn main() i64 = return 0
'

# ── generic functions rejected ──
assert_err "generic rejected" "cannot be applied to a generic function" '
fn{#interop} bad[t](x t) t = return x

fn main() i64 = return 0
'

# ── extern declarations rejected (already C) ──
assert_err "extern rejected" "extern declaration is meaningless" '
fn{#interop} bad(x i32) i32 = extern("c_bad")

fn main() i64 = return 0
'

# ── name `main` is reserved ──
assert_err "main name rejected" "reserved name" '
fn{#interop} main() i32 = return 0
'

# ── struct methods rejected (top-level only in v1) ──
assert_err "method rejected" "is not allowed on methods" '
struct point =
  x i32

  fn{#interop} show(this point) i32 = return this.x

fn main() i64 = return 0
'

# ── duplicate #interop names rejected at validate time ──
assert_err "duplicate name rejected" "duplicate #interop function name" '
fn{#interop} dup(a i32) i32 = return a
fn{#interop} dup(b i32) i32 = return b

fn main() i64 = return 0
'

echo
printf "#interop validation tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
