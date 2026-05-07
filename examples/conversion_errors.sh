#!/usr/bin/env bash
# Compile-time error paths for the implicit / coerce conversion machinery.
#
# Each case feeds a short Tin program through `tin run` and asserts that
# the compiler reports a specific diagnostic.  Driven by `tin run` because
# the error must appear before the program executes.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/conversion_errors.sh

set -u

pass=0
fail=0

assert_err() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/conv_err.XXXXXX.tin)
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

echo "conversion-trait error paths"

# --------------------------------------------------------------------- #
# 1. Impossible casts                                                   #
# --------------------------------------------------------------------- #

assert_err "string -> i64 has no conversion path" "no conversion path" '
fn main() = let x i64 = "hello" as i64
'

assert_err "bool -> string has no conversion path" "no conversion path" '
fn main() = let x string = true as string
'

assert_err "i64 -> string has no conversion path" "no conversion path" '
fn main() = let x string = 42 as string
'

# --------------------------------------------------------------------- #
# 2. Mixed pointer / value casts                                        #
# --------------------------------------------------------------------- #

assert_err "*Trait as Concrete (pointer to value) errors" "cannot cast a trait pointer to non-pointer type" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn main() =
  let d = &Dog{name: "rex"}
  let a *Animal = d
  let _ = a as Dog
'

assert_err "Trait as *Concrete (value to pointer) errors" "cannot cast a value-form trait to pointer type" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn taker(a Animal) =
  let _ = a as *Dog

fn main() =
  let d = Dog{name: "rex"}
  taker(d)
'

# --------------------------------------------------------------------- #
# 3. Unguarded trait downcast warning                                   #
# --------------------------------------------------------------------- #

assert_err "downcast without is-guard warns" "unguarded-trait-downcast" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn unsafe_get(a *Animal) string =
  let d = a as *Dog
  return (*d).name

fn main() =
  let d = &Dog{name: "rex"}
  let a *Animal = d
  echo unsafe_get(a)
'

# --------------------------------------------------------------------- #
# 4. Bool-operator strictness                                            #
# --------------------------------------------------------------------- #

assert_err "&& on coerce[bool] struct rejects" "cannot use Bag as a boolean operand" '
struct Bag(coerce[bool]) =
  items [string]
  static fn ::coerce(this Bag) bool = return len(this.items) > 0

fn other() bool = return true

fn main() =
  let b = Bag{items: ["x"]}
  if b && other(): echo "x"
'

assert_err "|| on coerce[bool] struct rejects" "cannot use Bag as a boolean operand" '
struct Bag(coerce[bool]) =
  items [string]
  static fn ::coerce(this Bag) bool = return len(this.items) > 0

fn other() bool = return false

fn main() =
  let b = Bag{items: ["x"]}
  if b || other(): echo "x"
'

# `!` already errors earlier via the unary-op trait lookup.
assert_err "! on bool-coercible struct rejects (no not[ret] impl)" "unary operator" '
struct Bag(coerce[bool]) =
  items [string]
  static fn ::coerce(this Bag) bool = return len(this.items) > 0

fn main() =
  let b = Bag{items: ["x"]}
  if !b: echo "x"
'

# --------------------------------------------------------------------- #
# 5. Redundant-type-cast warnings                                       #
# --------------------------------------------------------------------- #

assert_err "redundant single-value cast warns" "redundant-type-cast" '
fn main() =
  let x i64 = 42 as i64
'

assert_err "redundant array-element cast warns" "redundant-type-cast" '
fn main() =
  let xs [u32; 3] = [0x1 as u32, 0x2 as u32, 0x3 as u32]
'

assert_err "redundant tuple-element cast warns" "redundant-type-cast" '
fn main() =
  let p (i64, bool) = (5 as i64, true)
'

assert_err "redundant fn-arg cast warns" "redundant-type-cast" '
fn add(a i64, b i64) i64 = return a + b

fn main() =
  let _ = add(1 as i64, 2 as i64)
'

assert_err "redundant struct-field cast warns" "redundant-type-cast" '
struct P =
  x i64

fn main() =
  let p = P{x: 1 as i64}
'

# --------------------------------------------------------------------- #
# 6. Coerce trait dispatch errors                                       #
# --------------------------------------------------------------------- #

assert_err "coerce target not registered errors at cast site" "no conversion path" '
struct Money(coerce[i64]) =
  cents i64
  static fn ::coerce(this Money) i64 = return this.cents

fn main() =
  let m = Money{cents: 100}
  let s = m as string  // coerce[string] not impl
  echo s
'

printf "conversion errors: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
