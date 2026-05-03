#!/usr/bin/env bash
# Heap-promotion invariants: every `&local` whose value reaches an escape
# sink must outlive the enclosing function. The existing pass already
# handles direct `return &x`; this file pins the gaps that the per-pointer
# reachability extension should close.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/escape_promotion.sh

set -u

pass=0
fail=0

assert_runs() {
  local name=$1
  local want_output=$2
  local src=$3
  local tmp
  tmp=$(mktemp --suffix=.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$want_output"* ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected substring: %s\n' "$want_output"
    printf '        got: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  fi
}

echo "escape promotion: stack locals reachable through escape sinks"

# --- baseline (already works, regression guard) ----------------------------

assert_runs 'return &local heap-promotes the local' '999' '
fn make() *i64 =
  let x i64 = 999
  return &x

fn main() i64 =
  let p = make()
  echo *p
  return 0
'

# --- struct field with &local --------------------------------------------

assert_runs '&local in field of returned struct heap-promotes the local' '100' '
struct Box =
  p *i64

fn make_box() *Box =
  let x i64 = 100
  return &Box{p: &x}

fn main() i64 =
  let b = make_box()
  echo *b.p
  return 0
'

assert_runs '&local in field of returned by-value struct heap-promotes the local' '200' '
struct Box =
  p *i64

fn make_box() Box =
  let x i64 = 200
  return Box{p: &x}

fn main() i64 =
  let b = make_box()
  echo *b.p
  return 0
'

# --- *Trait = &local that escapes ----------------------------------------

assert_runs 'return *Trait of &local heap-promotes the local' '5' '
trait Showable =
  fn show(this *Showable) i64 = virtual

struct Pt (Showable) =
  v i64
  fn Showable::show(this *Pt) i64 = return this.v

fn make() *Showable =
  let pt = Pt{v: 5}
  let p *Showable = &pt
  return p

fn main() i64 =
  let s = make()
  echo (*s).show()
  return 0
'

echo
echo "escape promotion: $pass passed, $fail failed"

if [[ $fail -gt 0 ]]; then
  exit 1
fi
