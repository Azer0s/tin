#!/usr/bin/env bash
# Compile-time + runtime semantics for trait coercion.
#
# Two semantics:
#   1. `Trait` (value form) is always a heap-copy. Go-like.
#   2. `*Trait` (pointer form) is a borrow. Mutations propagate.
#
# Hard error: if a trait has any *Self method, value-form coercion
# is rejected (silent heap-copy mutation is too easy a footgun).
#
# Usage (from repo root):
#   go build -o tin . && bash examples/trait_coercion_errors.sh

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

assert_runs() {
  local name=$1
  local want_output=$2
  local src=$3
  local tmp
  tmp=$(mktemp)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$want_output"* ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected output substring: %s\n' "$want_output"
    printf '        got: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  fi
}

echo "trait coercion: hard-error + borrow semantics"

# 1. Hard-error: value coercion rejected when trait has *Self method.
assert_err 'value form into pointer-receiver trait is rejected' \
  'value-form coercion silently mutates a heap copy' '
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual

struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n

fn main() i64 =
  let b = Box{v: 0}
  let f Fooable = b
  return 0
'

# 1b. The same rule fires for `let f Trait = &b` - pointer source still
# heap-copies through coerceToTrait when the target is value-form Trait.
assert_err 'pointer source into value-form Trait also rejected' \
  'value-form coercion silently mutates a heap copy' '
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual
struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n
fn main() i64 = let b = Box{v: 0}; let f Fooable = &b; return 0
'

# 2. Hard-error message includes the failing method name(s).
assert_err 'error mentions the failing method name' \
  'pointer-receiver methods (foo)' '
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual
struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n
fn main() i64 = let b = Box{v: 0}; let f Fooable = b; return 0
'

# 3. Hard-error message tells the user how to fix it.
assert_err 'error suggests *Trait fix' \
  '`let a *Fooable = &b`' '
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual
struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n
fn main() i64 = let b = Box{v: 0}; let f Fooable = b; return 0
'

# 4. Pointer form works: mutation through *Trait propagates to source.
assert_runs 'pointer form lets mutation propagate' \
  'b.v = 2' '
trait Fooable =
  fn foo(this *Fooable, n i64) = virtual
struct Box (Fooable) =
  v i64
  fn Fooable::foo(this *Box, n i64) = this.v = n

fn main() i64 =
  let b = Box{v: 0}
  let a *Fooable = &b
  (*a).foo(2)
  echo "b.v = {b.v}"
  return 0
'

# 5. Value form works when the trait has only Self-receiver methods.
# Heap-copy is the right semantic: caller wants a snapshot.
assert_runs 'value form works for read-only traits' \
  'r.read() = 7' '
trait Readable =
  fn read(this Readable) i64 = virtual

struct Source (Readable) =
  v i64
  fn Readable::read(this Source) i64 = return this.v

fn main() i64 =
  let s = Source{v: 7}
  let r Readable = s
  echo "r.read() = {r.read()}"
  return 0
'


# 6. Atomic[t].for_locked accepts a multi-line anon lambda whose last
# statement carries the call's `)` on the same line. The parser
# defers the lambda body's DEDENT so it doesn't pop the surrounding
# scope (regression for the original "for_locked won't parse" bug).
assert_runs 'Atomic.for_locked accepts indented multi-stmt anon lambda' \
  'count=2 total=300' '
use sync

struct Stats =
  count i64
  total i64

fn main() i64 =
  let s = sync::Atomic[Stats].new(Stats{count: 0, total: 0})
  s.for_locked(fn(p *Stats) =
    p.count = p.count + 1
    p.total = p.total + 100)
  s.for_locked(fn(p *Stats) =
    p.count = p.count + 1
    p.total = p.total + 200)
  let snap = s.load()
  echo "count={snap.count} total={snap.total}"
  return 0
'

echo
echo "trait coercion errors: $pass passed, $fail failed"

if [[ $fail -gt 0 ]]; then
  exit 1
fi
