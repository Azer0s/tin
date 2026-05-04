#!/usr/bin/env bash
# Compile-time rejections for struct-level scoped tags #tag@scope.
# Each case writes a tiny .tin source and checks for a specific error
# substring. Positive tests live in examples/scoped_struct_tags.tin.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/scoped_struct_tags_errors.sh

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

echo "struct scoped-tag compile-error tests"

# ── #pure@fn propagation must reject echo in any method ──
assert_err "pure@fn catches echo in propagated method" "pure violation" '
struct {#pure@fn} bad =
  x i64

  fn write(this bad) =
    echo this.x

fn main() i64 =
  return 0
'

# ── Unknown scope @blah at parse time ──
assert_err "unknown scope @blah" "unknown struct tag scope @blah" '
struct {#pure@blah} bad =
  x i64

fn main() i64 =
  return 0
'

# ── #pure cannot be scoped @field ──
assert_err "#pure@field rejected" "tag #pure cannot be scoped @field" '
struct {#pure@field} bad =
  x i64

fn main() i64 =
  return 0
'

# ── #const cannot be scoped @fn ──
assert_err "#const@fn rejected" "tag #const is a field tag" '
struct {#const@fn} bad =
  x i64

  fn show(this bad) i64 =
    return this.x

fn main() i64 =
  return 0
'

# ── #packed cannot carry any scope qualifier ──
assert_err "#packed@fn rejected" "tag #packed is struct-level and cannot be scoped" '
struct {#packed@fn} bad =
  x i64

fn main() i64 =
  return 0
'

# ── #handover may not appear on struct declarations ──
assert_err "#handover@fn rejected" "tag #handover is extern-only" '
struct {#handover@fn} bad =
  x i64

fn main() i64 =
  return 0
'

# ── const@field default-flip: unmarked becomes const, assign rejected ──
assert_err "const@field default-flips unmarked to const" "cannot assign to const field bad.x" '
struct {#const@field} bad =
  x i64
  var scratch i64

fn main() i64 =
  let b = bad{x: 1, scratch: 0}
  b.x = 99
  return 0
'

echo
printf "scoped-tag compile-error tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
