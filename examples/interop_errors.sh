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

# ─── Phase B: type whitelist enforcement ──────────────────────────────

# ── struct param rejected (use pointer instead) ──
assert_err "struct param rejected" "named user types at the interop boundary" '
struct point =
  x i32

fn{#interop} bad(p point) i32 = return p.x

fn main() i64 = return 0
'

# ── struct return rejected (use pointer instead) ──
assert_err "struct return rejected" "named user types at the interop boundary" '
struct point =
  x i32

fn{#interop} bad() point = return point{x: 1}

fn main() i64 = return 0
'

# ── fixed-size array rejected ──
assert_err "fixed-size array rejected" "fixed-size arrays are not representable" '
fn{#interop} bad(xs [i32; 4]) i32 = return xs[0]

fn main() i64 = return 0
'

# ── atom rejected ──
assert_err "atom param rejected" "atom" '
fn{#interop} bad(a atom) i32 = return 0

fn main() i64 = return 0
'

# ── fn-typed return rejected (callback returns are not supported) ──
assert_err "fn-typed return rejected" "fn-typed return values are not supported" '
fn{#interop} bad() fn(i32) i32 = return fn(x i32) i32 = return x

fn main() i64 = return 0
'

# ── callback with non-primitive inner type rejected ──
assert_err "callback with string inner rejected" "callback inner type" '
fn{#interop} bad(cb fn(string) i32) i32 = return cb("x")

fn main() i64 = return 0
'

# ── union return rejected ──
assert_err "union return rejected" "union types are not representable" '
fn{#interop} bad() i32 | f64 = return 0

fn main() i64 = return 0
'

# ── [bool] arrays rejected (i1 vs i8 ABI ambiguity) ──
assert_err "[bool] param rejected" "[bool] forces a per-element" '
fn{#interop} bad(xs [bool]) i32 = return xs.len as i32

fn main() i64 = return 0
'

# ── [string] rejected (ARC-managed elements would dangle) ──
assert_err "[string] param rejected" "Tin strings are ARC-managed" '
fn{#interop} bad(xs [string]) i32 = return xs.len as i32

fn main() i64 = return 0
'

# ── [[T]] rejected (nested fat arrays would dangle) ──
assert_err "[[i32]] param rejected" "nested fat arrays are ARC-managed" '
fn{#interop} bad(xs [[i32]]) i32 = return xs.len as i32

fn main() i64 = return 0
'

# ── tin_runtime_init reserved (would shadow the runtime helper) ──
assert_err "tin_runtime_init name reserved" "would clash with a runtime symbol" '
fn{#interop} tin_runtime_init(x i32) i32 = return x

fn main() i64 = return 0
'

# ── __tin_interop_ prefix reserved (internal-symbol prefix) ──
assert_err "__tin_interop_ prefix reserved" "reserved internal-symbol prefix" '
fn{#interop} __tin_interop_foo(x i32) i32 = return x

fn main() i64 = return 0
'

# ─── Phase B positive: pointer to user struct is fine ─────────────────

assert_ok() {
  local name=$1
  local src=$2
  local tmp
  tmp=$(mktemp --suffix=.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"#interop"* ]] && [[ "$out" == *"error"* || "$out" == *"violation"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpected interop error: %s\n' "${out:0:300}"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

assert_ok "*void opaque handle accepted" '
struct point =
  x i32

fn{#interop} good(p *void) i32 =
  return (p as *point).x

fn main() i64 = return 0
'

assert_err "*Struct rejected (use *void)" "unsafe at the interop boundary" '
struct point =
  x i32

fn{#interop} bad(p *point) i32 = return (*p).x

fn main() i64 = return 0
'

assert_ok "string param accepted" '
fn{#interop} good(s string) i32 = return len(s) as i32

fn main() i64 = return 0
'

assert_ok "fat array param accepted" '
fn{#interop} good(xs [i32]) i32 = return xs.len as i32

fn main() i64 = return 0
'

echo
printf "#interop validation tests: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
