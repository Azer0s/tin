#!/usr/bin/env bash
# Regression tests for codegen bugs caught in code review.  Each case
# pins one fix; if a future change re-introduces the bug, the matching
# entry here flips from `ok` to `FAIL`.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/codegen_regressions.sh

set -u

pass=0
fail=0

assert_substr() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/cgreg.XXXXXX.tin)
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

assert_runs_clean() {
  local name=$1
  local must_not=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/cgreg.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"error:"* ]] || [[ "$out" == *"panic:"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpected error/panic: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  elif [[ -n "$must_not" ]] && [[ "$out" == *"$must_not"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpectedly contained: %s\n' "$must_not"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

echo "codegen regressions"

# ── B1: const fold of trait-implementer struct must NOT produce a
# null vtable.  Use an int-only-field struct so the RC-field guard
# does NOT also fire -- this isolates the Implements check.  Without
# the fix the fold succeeds with zeroed vtable slots, and dispatching
# through the resulting `Greeter` value loads NULL and segfaults; the
# refusal turns it into a clean compile error pointing the user at
# `var`.
assert_substr "B1: trait-impl struct rejected for const fold" \
  "non-compile-time initializer" '
trait Bumper =
  fn bump(this Bumper) i64 = virtual

struct Counter(Bumper) =
  v i64
  fn Bumper::bump(this Counter) i64 = return this.v + 1

const C = Counter{v: 5}
fn main() = echo "{C.v}"
'

# ── B2a: const fold of struct with init/deinit must be refused so the
# init runs at module-init time.
assert_substr "B2a: struct with init rejected for const fold" \
  "non-compile-time initializer" '
struct Counter =
  n i64
  fn init(this Counter) =
    this.n = 100

const C = Counter{n: 0}
fn main() = echo "{C.n}"
'

# ── B2b: const fold of struct with RC-tracked field (string) must be
# refused so the field is properly retained at module init.
assert_substr "B2b: struct with string field rejected for const fold" \
  "non-compile-time initializer" '
struct Named =
  label string

const N = Named{label: "hi"}
fn main() = echo "x"
'

# ── const fold of a plain int-only struct still works.  Sanity-check
# the negative side so the refusal above does not over-fire.
assert_runs_clean "B1/B2: plain struct still folds" "" '
struct Point =
  x i64
  y i64

const P = Point{x: 3, y: 7}
fn main() = echo "{P.x}-{P.y}"
'

# ── B3: `is *T` against a struct that does not implement the trait
# must be a hard error (not a silent constant-false).
assert_substr "B3: is *NonImpl errors clearly" \
  "is *Rock\` is unsatisfiable" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

struct Rock =
  weight i64

fn check(a *Animal) =
  if a is *Rock:
    echo "rock"

fn main() =
  let d = &Dog{name: "rex"}
  check(d)
'

# ── B7: qualified-call args still get the redundant-cast warning.
assert_substr "B7: qualified call arg cast warns" \
  "redundant \`as i64\`" '
use time
fn main() =
  let d = time::from_ms(20 as i64)
  echo "{d.ns}"
'

# ── B11: tuple containing a value-form trait must round-trip through
# tuple-monomorphization without falling back to i64.
assert_runs_clean "B11: value-form trait in tuple" "error:" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn pair() (Animal, i64) =
  let d = Dog{name: "rex"}
  return (d, 7)

fn main() =
  let p = pair()
  echo "{p.b} {p.a.sound()}"
'

# ── auto-yield after _tin_sleep_ms: an awaited time::sleep(50ms)
# from non-async main must elapse at least ~30ms.  If externYieldsAfter
# regresses the elapsed time would drop to microseconds.
assert_substr "auto-yield: awaited sleep_ms actually waits" \
  "elapsed_ms_ok" '
use time

fn main() =
  let s = time::monotonic_ns()

  await time::sleep_ms(50)

  let elapsed = time::monotonic_ns() - s
  // 30ms slack covers timer-granularity jitter.

  if elapsed >= 30000000:
    echo "elapsed_ms_ok"
  else:
    echo "elapsed_too_short {elapsed}"
'

# ── #allow_drop: tagged async should NOT trigger unused-must-use.
assert_runs_clean "#allow_drop suppresses must-use warning" \
  "unused-must-use" '
use sync

fn{#async} producer(ch sync::Channel[i64]) =
  ch.send(42)

fn main() =
  let ch = sync::Channel[i64].make(1)

  spawn producer(ch)
  echo "{await ch.recv()}"
'

printf "codegen regressions: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
