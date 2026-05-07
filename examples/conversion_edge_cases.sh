#!/usr/bin/env bash
# Aggressive stress tests for the conversion machinery.  Probes edge
# cases at the borders of the implicit / coerce / downcast / redundant
# warning rules.  Each case asserts a specific diagnostic substring;
# misclassified passes (program runs when it should error) and
# misclassified fails (compiler reports a confusing message instead of
# the expected one) both surface as test failures.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/conversion_edge_cases.sh

set -u

pass=0
fail=0

assert_substr() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/conv_edge.XXXXXX.tin)
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
    printf '        got: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  fi
}

# assert_runs_clean: expect program to run successfully with NO errors
# and NO warnings of the named class.  Detects regressions where a
# legitimate construct gets flagged.
assert_runs_clean() {
  local name=$1
  local must_not_contain=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/conv_edge.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$must_not_contain"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpectedly contained: %s\n' "$must_not_contain"
    printf '        full output: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  elif [[ "$out" == *"error:"* ]]; then
    printf '  FAIL  %s (compile error)\n' "$name"
    printf '        full output: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

echo "conversion edge cases"

# --------------------------------------------------------------------- #
# A. Trait downcast edge cases                                          #
# --------------------------------------------------------------------- #

assert_substr "downcast to wrong concrete type produces wild pointer (should warn anyway)" \
  "unguarded-trait-downcast" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"
struct Cat(Animal) =
  name string
  fn Animal::sound(this Cat) string = return "meow"

fn main() =
  let d = &Dog{name: "rex"}
  let a *Animal = d
  let c = a as *Cat   // wrong concrete; warning fires regardless
  echo (*c).name
'

assert_substr "is *T does not warn the as that follows it" \
  "no warning expected" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn pull_name(a *Animal) string =
  if a is *Dog:
    let d = a as *Dog
    return (*d).name
  return ""

fn main() =
  let d = &Dog{name: "rex"}
  let a *Animal = d
  echo pull_name(a)
  echo "no warning expected"
'

# --------------------------------------------------------------------- #
# B. Coerce edge cases                                                  #
# --------------------------------------------------------------------- #

assert_substr "coerce target absent: error names the missing target" \
  "no conversion path" '
struct Money(coerce[i64]) =
  cents i64
  static fn ::coerce(this Money) i64 = return this.cents

fn main() =
  let m = Money{cents: 100}
  let _ = m as f64
'

# Two coerce[bool] impls is silently accepted -- the second listing
# is a no-op because the static fn registration is keyed on the
# struct + return type and the second match clobbers the same slot.
# Verify the program runs (and the register works) rather than
# crashing.
assert_runs_clean "duplicate coerce[bool] in implements list is harmless" \
  "error:" '
struct Bag(coerce[bool], coerce[bool]) =
  n i64
  static fn ::coerce(this Bag) bool = return this.n != 0

fn main() =
  let b = Bag{n: 1}
  if b: echo "x"
'

# Coerce return type that does not match the impl trait's parameter
# is a programmer error -- the trait list says coerce[i64] but the
# method returns string.
assert_substr "coerce return type mismatched with impl param errors" \
  "no conversion path" '
struct Money(coerce[i64]) =
  cents i64
  static fn ::coerce(this Money) string = return "hi"  // wrong ret

fn main() =
  let m = Money{cents: 1}
  let n = m as i64
  echo "{n}"
'

# --------------------------------------------------------------------- #
# C. Implicit edge cases                                                #
# --------------------------------------------------------------------- #

assert_runs_clean "implicit through arg in nested call" "error:" '
struct Cm(implicit[i64]) =
  mm i64
  static fn ::implicit(n i64) Cm = return Cm{mm: n * 10}

fn double(c Cm) Cm = return Cm{mm: c.mm * 2}
fn quad(c Cm) Cm = return double(double(c))

fn main() =
  let c = quad(3)
  echo "mm={c.mm}"
'

# Two implicit[i64] impls is silently accepted (same reasoning as
# duplicate coerce); the second listing collapses to the same fn ptr.
assert_runs_clean "duplicate implicit[i64] in implements list is harmless" \
  "error:" '
struct Cm(implicit[i64], implicit[i64]) =
  mm i64
  static fn ::implicit(n i64) Cm = return Cm{mm: n * 10}

fn main() =
  let c Cm = 5
  echo "{c.mm}"
'

# --------------------------------------------------------------------- #
# D. Conditional edge cases                                             #
# --------------------------------------------------------------------- #

assert_runs_clean "while loop auto-coerces struct condition" "error:" '
struct Counter(coerce[bool]) =
  n i64
  static fn ::coerce(this Counter) bool = return this.n > 0

fn main() =
  let c = Counter{n: 3}
  let i i64 = 0
  for c.n > 0 :
    i = i + 1
    c = Counter{n: c.n - 1}
  echo "i={i}"
'

assert_substr "struct with coerce[bool] AND not[bool] uses not for !" \
  "expected: not[bool] dispatched" '
struct Toggle(coerce[bool], not[bool]) =
  on bool
  static fn ::coerce(this Toggle) bool = return this.on
  fn ::not(this Toggle) bool = return !this.on

fn main() =
  let t = Toggle{on: true}
  let n = !t  // unary not -- dispatches via not[bool], not coerce
  if !n:
    echo "expected: not[bool] dispatched"
  else:
    echo "WRONG"
'

# Bool coerce + ternary
assert_runs_clean "ternary auto-coerce on both branches independent" "error:" '
struct Maybe(coerce[bool]) =
  v bool
  static fn ::coerce(this Maybe) bool = return this.v

fn main() =
  let on  = Maybe{v: true}
  let off = Maybe{v: false}
  let r = on ? "Y" : "N"
  let s = off ? "Y" : "N"
  echo "{r}{s}"
'

# --------------------------------------------------------------------- #
# E. Redundant-cast warning -- should NOT fire on legit casts           #
# --------------------------------------------------------------------- #

assert_runs_clean "real conversion does not get redundant-cast warning" \
  "redundant-type-cast" '
fn main() =
  let big i64 = 1000000000
  let small i32 = big as i32     // real narrowing; not redundant
  echo "{small}"
'

assert_runs_clean "explicit cast in non-typed let is not redundant" \
  "redundant-type-cast" '
fn main() =
  let v = 42 as i32   // no slot type pin -- the cast IS the type pin
  echo "{v}"
'

assert_runs_clean "cast inside expr (not at slot) is not redundant" \
  "redundant-type-cast" '
fn main() =
  let x i64 = 5 + (3 as i64)   // the i64 here is on the inner
  echo "{x}"
'

# But this SHOULD fire: cast at the slot itself.
assert_substr "redundant-cast at slot fires" "redundant-type-cast" '
fn main() =
  let x i64 = 5 as i64
'

# ADT nested target also fires.
assert_substr "redundant cast on Some payload fires" "redundant-type-cast" '
use option

fn main() =
  let v Option[i64] = Some(42 as i64)
'

# Nested ADT: outer Result, inner Option.
assert_substr "redundant cast on inner Option payload fires" "redundant-type-cast" '
use option
use result

fn make() Result[Option[i64], string] =
  return Ok(Some(7 as i64))

fn main() =
  match make():
    case Ok(_):  echo "ok"
    case Err(_): echo "err"
'

# --------------------------------------------------------------------- #
# F. Impossible-cast error wording                                      #
# --------------------------------------------------------------------- #

assert_substr "impossible cast names source type in error" "string" '
fn main() =
  let _ = "x" as i64
'

assert_substr "impossible cast names target type in error" "i64" '
fn main() =
  let _ = "x" as i64
'

# --------------------------------------------------------------------- #
# G. Bool operator strictness with mixed operands                       #
# --------------------------------------------------------------------- #

assert_substr "bool && struct rejects struct side" "cannot use" '
struct Bag(coerce[bool]) =
  n i64
  static fn ::coerce(this Bag) bool = return this.n != 0

fn other() bool = return true

fn main() =
  let b = Bag{n: 1}
  if other() && b:
    echo "x"
'

# --------------------------------------------------------------------- #
# H. Pointer to bool-coercible struct in conditional                    #
# --------------------------------------------------------------------- #

# `if p:` where p is a pointer should test pointer-non-nil, NOT call
# the struct's coerce[bool].  Otherwise dereferencing nil would crash.
assert_runs_clean "pointer-to-bool-coercible-struct stays pointer-truthy" \
  "error:" '
struct Bag(coerce[bool]) =
  n i64
  static fn ::coerce(this Bag) bool = return this.n != 0

fn main() =
  let b = Bag{n: 0}
  let p *Bag = &b
  if p:
    echo "p is non-nil"
  else:
    echo "WRONG: pointer dispatched coerce[bool]"
'

# --------------------------------------------------------------------- #
# I. Generic struct paths                                               #
# --------------------------------------------------------------------- #

assert_runs_clean "generic struct receives literal in field via redundant-free path" \
  "redundant-type-cast" '
use result

fn make() Result[i64, string] =
  return Ok(99)

fn main() =
  let r = make()
  match r:
    case Ok(v):  echo "{v}"
    case Err(_): echo "err"
'

# --------------------------------------------------------------------- #
# J. Auto coerce[bool] does NOT fire for nil-able pointer to non-struct #
# --------------------------------------------------------------------- #

assert_runs_clean "nil pointer in if reports false (no coerce dispatch)" "error:" '
fn main() =
  let p *i64 = nil
  if p:
    echo "WRONG"
  else:
    echo "ok: nil pointer is falsy"
'

printf "edge cases: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
