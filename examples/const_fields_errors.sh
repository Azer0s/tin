#!/usr/bin/env bash
# Compile-time rejections for per-field const. Each case writes a tiny
# .tin source, invokes `./tin run`, and checks for a specific error
# substring. Positive ("should compile and run") tests live in
# examples/const_fields.tin.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/const_fields_errors.sh

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

assert_ok() {
  local name=$1
  local src=$2
  local tmp
  tmp=$(mktemp)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  local rc=$?
  rm -f "$tmp"
  if [[ "$out" == *"cannot assign to const"* ]] || [[ "$out" == *"setfield: cannot assign"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpected const error: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

echo "const-field compile-error tests"

# ── direct assign to a const field is a compile error ──
assert_err "direct assign" "cannot assign to const field point.x" '
struct point =
  const x i64
  const y i64

fn main() i64 =
  let p = point{x: 1, y: 2}
  p.x = 99
  return 0
'

# ── augmented assign (+=, -=, ...) to a const field is a compile error ──
assert_err "aug assign +=" "cannot assign to const field point.x" '
struct point =
  const x i64
  const y i64

fn main() i64 =
  let p = point{x: 1, y: 2}
  p.x += 5
  return 0
'

# ── postfix (++/--) on a const field is a compile error ──
assert_err "postfix ++" "cannot assign to const field point.x" '
struct point =
  const x i64
  const y i64

fn main() i64 =
  let p = point{x: 1, y: 2}
  p.x++
  return 0
'

# ── method body writing a const field is a compile error ──
assert_err "method write" "cannot assign to const field point.x" '
struct point =
  const x i64
  const y i64

  fn bump(this point, dx i64) =
    this.x = this.x + dx

fn main() i64 =
  let p = point{x: 1, y: 2}
  p.bump(5)
  return 0
'

# ── pointer-through (->) write is a compile error ──
assert_err "pointer -> write" "cannot assign to const field point.x" '
struct point =
  const x i64
  const y i64

fn main() i64 =
  let p = point{x: 1, y: 2}
  let pp = &p
  pp->x = 99
  return 0
'

# ── generic struct monomorphization preserves const-ness ──
assert_err "generic monomorph" "cannot assign to const field Box[i64].value" '
struct Box[t] =
  const value t

type IntBox = Box[i64]

fn main() i64 =
  let b IntBox = IntBox{value: 5}
  b.value = 99
  return 0
'

# ── const is COMPILE-TIME ONLY ─────────────────────────────────────────
# setfield is reflective and remains allowed even for const fields.
assert_ok "setfield on const allowed" '
use assert

struct point =
  const x i64
  const y i64

test "setfield compiles on const field" =
  let p = point{x: 1, y: 2}
  setfield(p, "x", 42)
  assert::equals(p.x, 42)
'

# Taking the address of a const field is allowed (const is not a safety guarantee).
assert_ok "address-of const field allowed" '
use assert

struct point =
  const x i64
  const y i64

test "&const field compiles" =
  let p = point{x: 5, y: 6}
  let px = &p.x
  assert::equals(*px, 5)
'

echo
printf "const-field compile-error tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
