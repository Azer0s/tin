#!/usr/bin/env bash
# Compile-time rejections for method-level where guards on generic
# struct methods. When a method's `where t is X` clause does not hold
# for the concrete instantiation, the method is dead-stripped and the
# call site emits a diagnostic naming the failing constraint.
#
# Three shapes covered:
#   1. Single impl, single leaf bound  -> "(t = "X")"
#   2. Single impl, AND bound          -> 'failed at `Z` (t = "X")'
#   3. Single impl, OR/union bound     -> '(t = "X" matches none)'
#   4. Multiple impls, none match      -> "N candidate impls, none matched"
#
# Usage (from repo root):
#   go build -o tin . && bash examples/method_where_errors.sh

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

echo "method-level where guards: dead-strip diagnostics"


# 1. Single leaf bound (`where t is i64`).
assert_err 'leaf bound failure: pretty struct name + bound' \
  "Box[bool].just_i64 doesn't match where t is i64" '
struct Box[t] =
  v t

  static fn make(v t) Box[t] = return Box[t]{v: v}

  fn just_i64(this Box[t]) i64 where t is i64 = return 1

fn main() i64 =
  let b = Box[bool].make(true)
  return b.just_i64()
'

# 2. AND bound — points at the failing conjunct via "(missing X)".
assert_err 'AND bound failure points at failing conjunct' \
  '(missing Sized)' '
trait Sized =
  fn size(this *Sized) i64 = virtual

struct Box[t] =
  v t

  static fn make(v t) Box[t] = return Box[t]{v: v}

  fn measure(this Box[t]) i64 where t is ord && Sized = return 1

fn main() i64 =
  let b = Box[i64].make(7)
  return b.measure()
'

# 3. OR / union-alias bound — bound name appears in the message.
assert_err 'OR bound failure shows the bound' \
  "doesn't match where t is intish" '
type intish = i32 | i64

struct Box[t] =
  v t

  static fn make(v t) Box[t] = return Box[t]{v: v}

  fn run(this Box[t]) i64 where t is intish = return 1

fn main() i64 =
  let b = Box[bool].make(true)
  return b.run()
'

# 4. Multiple impls, none satisfied — listed via "any of:".
assert_err 'multi-impl failure lists every candidate' \
  "doesn't match any of: where t is intish, where t is floatish" '
type intish = i32 | i64
type floatish = f32 | f64

struct Box[t] =
  v t

  static fn make(v t) Box[t] = return Box[t]{v: v}

  fn process(this Box[t]) i64 where t is intish = return 1
  fn process(this Box[t]) i64 where t is floatish = return 2

fn main() i64 =
  let b = Box[bool].make(true)
  return b.process()
'

# Sanity: the same method when the guard DOES hold compiles cleanly
# (proves the dead-strip is keyed on the bound, not on the method name).
# Rather than rely on exit code (which programs use for return values),
# `tin build` exits 0 on a clean compile and non-zero on diagnostic.
work=$(mktemp)
out_bin=$(mktemp -u)
cat > "$work" << 'EOF'
struct Box[t] =
  v t

  static fn make(v t) Box[t] = return Box[t]{v: v}

  fn just_i64(this Box[t]) i64 where t is i64 = return this.v

fn main() i64 =
  let b = Box[i64].make(7)
  return b.just_i64()
EOF

if ./tin build "$work" -o "$out_bin" >/dev/null 2>&1; then
  printf '  ok    method survives when guard holds\n'
  pass=$((pass + 1))
else
  printf '  FAIL  method survives when guard holds\n'
  fail=$((fail + 1))
fi

rm -f "$work" "$out_bin"

echo
echo "method_where_errors: $pass passed, $fail failed"

if [[ $fail -gt 0 ]]; then
  exit 1
fi
