#!/usr/bin/env bash
# Torture tests for the conversion machinery.  Specifically targets:
# - boundary cases for the redundant-cast warning (false positives + negatives)
# - corner cases of `is *T` / `as *T` on traits
# - interaction between coerce[bool] and other implicit-bool sites
# - stack-busting / aliasing pathologies that would surface as crashes
#
# Usage (from repo root):
#   go build -o tin . && bash examples/conversion_torture.sh

set -u

pass=0
fail=0

run_case() {
  local kind=$1   # "compiles" | "panics_no" | "want" | "wantnot"
  local name=$2
  local extra=""
  local src
  if [[ "$kind" == "panics_no" || "$kind" == "compiles" ]]; then
    src=$3
  else
    extra=$3
    src=$4
  fi
  local tmp
  tmp=$(mktemp /tmp/conv_t.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"

  case "$kind" in
    compiles)
      # Should compile and run without errors / panics.
      if [[ "$out" == *"error:"* ]] || [[ "$out" == *"panic:"* ]]; then
        printf '  FAIL  %s\n' "$name"
        printf '        unexpected error: %s\n' "${out:0:600}"
        fail=$((fail + 1))
      else
        printf '  ok    %s\n' "$name"
        pass=$((pass + 1))
      fi
      ;;
    panics_no)
      if [[ "$out" == *"panic:"* ]] || [[ "$out" == *"goroutine "*"running"* ]]; then
        printf '  FAIL  %s (compiler panic)\n' "$name"
        printf '        full: %s\n' "${out:0:600}"
        fail=$((fail + 1))
      else
        printf '  ok    %s\n' "$name"
        pass=$((pass + 1))
      fi
      ;;
    want)
      if [[ "$out" == *"$extra"* ]]; then
        if [[ "$out" == *"panic:"* ]]; then
          printf '  FAIL  %s (got expected substring BUT also crashed)\n' "$name"
          fail=$((fail + 1))
        else
          printf '  ok    %s\n' "$name"
          pass=$((pass + 1))
        fi
      else
        printf '  FAIL  %s\n' "$name"
        printf '        expected substring: %s\n' "$extra"
        printf '        got: %s\n' "${out:0:600}"
        fail=$((fail + 1))
      fi
      ;;
    wantnot)
      if [[ "$out" == *"$extra"* ]]; then
        printf '  FAIL  %s\n' "$name"
        printf '        unexpectedly contained: %s\n' "$extra"
        printf '        full: %s\n' "${out:0:600}"
        fail=$((fail + 1))
      elif [[ "$out" == *"error:"* ]]; then
        printf '  FAIL  %s (compile error)\n' "$name"
        printf '        full: %s\n' "${out:0:600}"
        fail=$((fail + 1))
      else
        printf '  ok    %s\n' "$name"
        pass=$((pass + 1))
      fi
      ;;
  esac
}

echo "conversion torture tests"

# ───────────────────────────────────────────────────────────────────── #
# A. Redundant-cast false-positive guards                                #
# ───────────────────────────────────────────────────────────────────── #

# A literal cast deeper in an expression must NOT be confused with a
# slot-pinned cast.
run_case wantnot "literal cast on rhs of arithmetic isn't redundant" \
  "redundant-type-cast" '
fn main() =
  let x i64 = 1 + 2 as i64
  echo "{x}"
'

# A function-call arg whose source has a different static type than
# the param should keep its `as` cast.
run_case wantnot "i64 arg cast to i32 param is not redundant" \
  "redundant-type-cast" '
fn take(n i32) i32 = return n + 1 as i32

fn main() =
  let big i64 = 100
  echo "{take(big as i32)}"
'

# A struct-field literal of a generic struct in a generic context.
run_case wantnot "Some(literal) is not redundant when slot is Option[T]" \
  "redundant-type-cast" '
use option

fn main() =
  let v Option[i64] = Some(42)   // no `as i64` here, should not warn
  match v:
    case Some(n): echo "{n}"
    case None:    echo "x"
'

# Cast inside an argument-passing context where the target IS i64
# but the source is a method call returning i32 -- real conversion.
run_case wantnot "method call cast is not redundant" \
  "redundant-type-cast" '
struct Source =
  v i32
  fn read(this Source) i32 = return this.v

fn want_i64(n i64) i64 = return n

fn main() =
  let s = Source{v: 10}
  let n = want_i64(s.read() as i64)
  echo "{n}"
'

# ───────────────────────────────────────────────────────────────────── #
# B. Trait dispatch corner cases                                         #
# ───────────────────────────────────────────────────────────────────── #

# `is *T` on a trait pointer that holds a value-form coerced struct
# (the pointer points at a fat-ptr borrow).
run_case panics_no "is *T on borrow-form trait pointer" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn main() =
  let d = Dog{name: "rex"}
  let a *Animal = &d
  if a is *Dog:
    echo "ok"
  else:
    echo "WRONG"
'

# Trait pointer that goes through a function and back -- ABI stable?
run_case panics_no "trait pointer round trip through fn does not corrupt" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn ident(a *Animal) *Animal = return a

fn main() =
  let d = &Dog{name: "rex"}
  let a *Animal = d
  let b = ident(a)
  if b is *Dog: echo "ok"
  else:         echo "WRONG"
'

# Two structs with same trait, downcast picks the right one.
run_case panics_no "is *T disambiguates between two impls" '
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
  if a is *Cat:
    echo "WRONG"
  else if a is *Dog:
    echo "ok"
'

# ───────────────────────────────────────────────────────────────────── #
# C. coerce[bool] interaction with implicit-bool sites                   #
# ───────────────────────────────────────────────────────────────────── #

# Auto coerce inside a for-init clause expression.
run_case panics_no "coerce[bool] on for-init i guard" '
struct Pred(coerce[bool]) =
  v i64
  static fn ::coerce(this Pred) bool = return this.v < 5

fn main() =
  let p = Pred{v: 0}
  let i i64 = 0
  for p :
    i = i + 1
    p = Pred{v: p.v + 1}
  echo "{i}"
'

# coerce[bool] AND a different coerce[T] -- in if context, must pick
# coerce[bool] specifically.
run_case panics_no "if uses coerce[bool] and not coerce[i64]" '
struct Mix(coerce[bool], coerce[i64]) =
  v i64
  static fn ::coerce(this Mix) bool = return this.v != 0
  static fn ::coerce(this Mix) i64  = return this.v * 10

fn main() =
  let m = Mix{v: 7}
  if m:
    let n = m as i64
    echo "{n}"      // 70 -- not the bool path
  else:
    echo "WRONG"
'

# ───────────────────────────────────────────────────────────────────── #
# D. Operator overload + coerce coexistence                              #
# ───────────────────────────────────────────────────────────────────── #

# Struct with both add and coerce -- arithmetic dispatches to add,
# explicit `as` dispatches to coerce, neither bleeds into the other.
run_case panics_no "add overload and coerce do not interfere" '
struct Points(add[Points, Points], coerce[i64]) =
  v i64
  fn ::add(this Points, other Points) Points = return Points{v: this.v + other.v}
  static fn ::coerce(this Points) i64 = return this.v * 100

fn main() =
  let a = Points{v: 5}
  let b = Points{v: 7}
  let c = a + b              // add, not coerce
  echo "{c.v}"               // 12
  echo "{c as i64}"          // 1200
'

# ───────────────────────────────────────────────────────────────────── #
# E. Aliasing & ARC pathologies                                          #
# ───────────────────────────────────────────────────────────────────── #

# Struct that holds a heap slice; coerce[i64] consumes len.  Multiple
# casts must not affect the original.
run_case panics_no "coerce reads heap-backed field repeatedly" '
struct ListLen(coerce[i64]) =
  xs [string]
  static fn ::coerce(this ListLen) i64 = return len(this.xs)

fn main() =
  let l = ListLen{xs: ["a", "b", "c"]}
  let a = l as i64
  let b = l as i64
  let c = l as i64

  echo "{a} {b} {c} -- xs len {len(l.xs)}"
'

# ───────────────────────────────────────────────────────────────────── #
# F. Method call on coerced result                                       #
# ───────────────────────────────────────────────────────────────────── #

run_case panics_no "method call chained off `as` result" '
struct Wrap(coerce[string]) =
  v i64
  static fn ::coerce(this Wrap) string = return "v={this.v}"

fn main() =
  let w = Wrap{v: 42}
  let s = w as string
  let n = len(s)
  echo "{s} len={n}"
'

# ───────────────────────────────────────────────────────────────────── #
# G. Truthiness of native shapes (regression guards)                     #
# ───────────────────────────────────────────────────────────────────── #

run_case panics_no "if on string non-empty" '
fn main() =
  let s = "hello"
  if s: echo "ok"
  else: echo "WRONG"
'

run_case panics_no "if on empty string" '
fn main() =
  let s = ""
  if s: echo "WRONG"
  else: echo "ok"
'

run_case panics_no "if on i64 non-zero" '
fn main() =
  let n = 5
  if n: echo "ok"
  else: echo "WRONG"
'

run_case panics_no "if on i64 zero" '
fn main() =
  let n = 0
  if n: echo "WRONG"
  else: echo "ok"
'

run_case panics_no "if on empty fat-array" '
fn main() =
  let xs [i64] = []
  if xs: echo "WRONG: empty array shouldnt be truthy unless len-based"
  else:  echo "ok: empty array is falsy via pointer-non-nil check"
'

# ───────────────────────────────────────────────────────────────────── #
# H. Trait pointer chained through Result                                #
# ───────────────────────────────────────────────────────────────────── #

run_case panics_no "Result-Err carrying *errors::Err round-trip" '
use errors
use result

fn maybe_fail(do bool) Result[i64, *errors::Err] =
  if do:
    return Err(errors::new("something"))
  return Ok(7)

fn main() =
  match maybe_fail(true):
    case Ok(_): echo "WRONG"
    case Err(e): echo errors::message(e)
  match maybe_fail(false):
    case Ok(n): echo "got {n}"
    case Err(_): echo "WRONG"
'

# ───────────────────────────────────────────────────────────────────── #
# I. Compile-error coverage                                              #
# ───────────────────────────────────────────────────────────────────── #

run_case want "downcast warning shows variable name" "downcast \`a as" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  fn Animal::sound(this Dog) string = return "woof"

fn unsafe_get(a *Animal) =
  let _ = a as *Dog

fn main() =
  let d = &Dog{}
  unsafe_get(d)
'

run_case want "redundant cast points at slot" "redundant \`as" '
fn main() =
  let x u32 = 5 as u32
  echo "{x}"
'

# ───────────────────────────────────────────────────────────────────── #
# J. Negative tests for the redundant warning's structural rules         #
# ───────────────────────────────────────────────────────────────────── #

# Redundant cast inside the value of a let with a non-matching slot
# should NOT fire on the inner.
run_case wantnot "inner cast that doesnt match outer slot stays silent" \
  "redundant-type-cast" '
fn main() =
  let x f64 = (5 as i64) as f64
  echo "{x}"
'

# Cast on a generic type-arg that is itself a generic.
run_case wantnot "Result[Option[T], E] outer Ok arg without redundant" \
  "redundant-type-cast" '
use option
use result

fn main() =
  let r Result[Option[i64], string] = Ok(Some(7))
  match r:
    case Ok(_): echo "ok"
    case Err(_): echo "err"
'

printf "torture tests: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
