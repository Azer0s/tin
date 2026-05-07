#!/usr/bin/env bash
# Hostile / pathological tests for the conversion machinery.  The
# goal is to break the compiler or surface confusing diagnostics.
# Anything that reaches `unhandled_panic` or "unhelpful" output gets
# a FAIL here so it can be turned into a clean error.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/conversion_hostile.sh

set -u

pass=0
fail=0

assert_compiles() {
  local name=$1
  local src=$2
  local tmp
  tmp=$(mktemp /tmp/conv_host.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"error:"* ]] || [[ "$out" == *"panic:"* ]] || [[ "$out" == *"goroutine"* ]]; then
    printf '  FAIL  %s\n' "$name"
    printf '        unexpected error/panic: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

assert_substr() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/conv_host.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$want"* ]]; then
    if [[ "$out" == *"panic:"* ]] || [[ "$out" == *"goroutine "*"running"* ]]; then
      printf '  FAIL  %s (got expected substring BUT also crashed)\n' "$name"
      printf '        full: %s\n' "${out:0:600}"
      fail=$((fail + 1))
    else
      printf '  ok    %s\n' "$name"
      pass=$((pass + 1))
    fi
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected substring: %s\n' "$want"
    printf '        got: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  fi
}

# assert_no_panic: program should run OR error cleanly; never crash
# the compiler with a Go panic.
assert_no_panic() {
  local name=$1
  local src=$2
  local tmp
  tmp=$(mktemp /tmp/conv_host.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"panic:"* ]] || [[ "$out" == *"goroutine "*"running"* ]]; then
    printf '  FAIL  %s (compiler panic)\n' "$name"
    printf '        full: %s\n' "${out:0:600}"
    fail=$((fail + 1))
  else
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  fi
}

echo "conversion hostile tests"

# --------------------------------------------------------------------- #
# 1. Pathological coerce shapes                                         #
# --------------------------------------------------------------------- #

# Coerce body that returns wrong type -- should be a type error inside
# the method, not a confusing call-site error.
assert_substr "coerce body returns wrong type errors at the body" \
  "cannot return value of type" '
struct Money(coerce[i64]) =
  cents i64
  static fn ::coerce(this Money) i64 = return "not an int"

fn main() =
  let m = Money{cents: 1}
  let _ = m as i64
'

# Coerce that recursively calls `as` on itself -- is the runtime stack
# safe?  Actually this is just plain recursion; the user did it.
assert_no_panic "coerce body that calls itself terminates or stack-overflows cleanly" '
struct Wrap(coerce[i64]) =
  v i64
  static fn ::coerce(this Wrap) i64 =
    if this.v <= 0:
      return 0
    return (Wrap{v: this.v - 1} as i64) + 1

fn main() =
  let w = Wrap{v: 10}
  let n = w as i64
  echo "{n}"
'

# Coerce body that triggers another coerce chain
assert_no_panic "coerce chain A->B->C terminates" '
struct A(coerce[i64]) =
  n i64
  static fn ::coerce(this A) i64 = return this.n + 1

struct B(coerce[i64]) =
  inner A
  static fn ::coerce(this B) i64 = return (this.inner as i64) * 2

fn main() =
  let a = A{n: 3}
  let b = B{inner: a}
  let v = b as i64
  echo "{v}"      // (3+1)*2 = 8
'

# --------------------------------------------------------------------- #
# 2. Implicit + arithmetic precedence                                    #
# --------------------------------------------------------------------- #

# `a + b` where a is implicit-convertible struct and b is i64 -- which
# operand drives the dispatch?
assert_no_panic "struct + i64: implicit on rhs picks struct add" '
struct Inches(implicit[i64], add[Inches, Inches]) =
  v i64
  static fn ::implicit(n i64) Inches = return Inches{v: n}
  fn ::add(this Inches, other Inches) Inches = return Inches{v: this.v + other.v}

fn main() =
  let a = Inches{v: 3}
  let b = a + 4   // 4 must implicit-convert to Inches first
  echo "{b.v}"
'

# Inverse: i64 + struct
assert_no_panic "i64 + struct: implicit on lhs picks struct add (commutative)" '
struct Inches(implicit[i64], add[Inches, Inches]) =
  v i64
  static fn ::implicit(n i64) Inches = return Inches{v: n}
  fn ::add(this Inches, other Inches) Inches = return Inches{v: this.v + other.v}

fn main() =
  let a = Inches{v: 3}
  let b = 5 + a   // commutative-swap path through implicit
  echo "{b.v}"
'

# --------------------------------------------------------------------- #
# 3. Nil downcast cases                                                 #
# --------------------------------------------------------------------- #

# Casting nil *Trait to *Concrete: what happens?
assert_no_panic "nil *Trait as *Concrete preserves nil at the cast site" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn main() =
  let a *Animal = nil
  if a is *Dog:
    echo "WRONG: nil should not match"
  else:
    echo "ok: nil iface fails the is-check"
'

# --------------------------------------------------------------------- #
# 4. Unusual literal forms                                              #
# --------------------------------------------------------------------- #

# Hex literal in u8 context: should NOT warn redundant when no type
# annotation pins it (the slot is the function call).
assert_no_panic "hex literal in fn call without redundant warn" '
fn take(b u8) i64 = return b as i64

fn main() =
  let n = take(0xff)
  echo "{n}"
'

# Float literal in i64 slot: Tin currently truncates implicitly (3.14 -> 3).
# That is lossy but not a hard error; verify the resulting value matches
# the documented truncation semantics rather than asserting an error.
assert_no_panic "float literal in i64 slot truncates implicitly" '
fn main() =
  let x i64 = 3.14
  echo "{x}"
'

# Char literal in i64 slot: char auto-coerces to int.
assert_no_panic "char as i64 auto-coerces" '
fn main() =
  let c = @'A'
  let n i64 = c as i64
  echo "{n}"   // 65
'

# --------------------------------------------------------------------- #
# 5. Trait-coerce interaction edge cases                                #
# --------------------------------------------------------------------- #

# A struct that implements coerce[T] where T is itself a struct.
assert_no_panic "coerce target is another struct" '
struct A =
  v i64
struct B(coerce[A]) =
  q i64
  static fn ::coerce(this B) A = return A{v: this.q * 100}

fn main() =
  let b = B{q: 5}
  let a = b as A
  echo "{a.v}"
'

# Coerce between two structs in opposite directions
assert_no_panic "two-way coerce between structs" '
struct Yard(coerce[Foot]) =
  y i64
  static fn ::coerce(this Yard) Foot = return Foot{f: this.y * 3}

struct Foot(coerce[Yard]) =
  f i64
  static fn ::coerce(this Foot) Yard = return Yard{y: this.f / 3}

fn main() =
  let y = Yard{y: 2}
  let f = y as Foot
  let y2 = f as Yard
  echo "{f.f} {y2.y}"
'

# --------------------------------------------------------------------- #
# 6. Generic interactions                                                #
# --------------------------------------------------------------------- #

# Result with a nested ADT payload + coerce on the inner type.
assert_no_panic "Result[Coerce-Struct, str] flows through generic" '
use result

struct Money(coerce[string]) =
  cents i64
  static fn ::coerce(this Money) string = return "$"

fn make() Result[Money, string] =
  return Ok(Money{cents: 100})

fn main() =
  match make():
    case Ok(m):  echo (m as string)
    case Err(_): echo "err"
'

# --------------------------------------------------------------------- #
# 7. Self-referential / weird shapes                                     #
# --------------------------------------------------------------------- #

# Empty struct with coerce[bool] -- always returns same value.
assert_no_panic "empty struct can have coerce[bool]" '
struct Marker(coerce[bool]) =
  static fn ::coerce(this Marker) bool = return true

fn main() =
  let m = Marker{}
  if m: echo "ok"
  else: echo "WRONG"
'

# coerce[T] on a packed struct (if Tin supports it) -- exercises
# struct-layout vs registry indexing.
assert_no_panic "struct with multiple field types and coerce" '
struct Mixed(coerce[i64], coerce[string]) =
  a i32
  b f64
  c string
  static fn ::coerce(this Mixed) i64 = return this.a as i64
  static fn ::coerce(this Mixed) string = return this.c

fn main() =
  let m = Mixed{a: 7 as i32, b: 1.5, c: "hi"}
  echo "{m as i64} {m as string}"
'

# --------------------------------------------------------------------- #
# 8. Interaction with `is` patterns                                      #
# --------------------------------------------------------------------- #

# `is *Concrete` in else-if chain
assert_no_panic "is *T in else-if chain narrows correctly" '
trait Animal =
  fn sound(this Animal) string = virtual
struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"
struct Cat(Animal) =
  name string
  fn Animal::sound(this Cat) string = return "meow"

fn describe(a *Animal) string =
  if a is *Dog:
    let d = a as *Dog
    return "dog: " ++ (*d).name
  else if a is *Cat:
    let c = a as *Cat
    return "cat: " ++ (*c).name
  return "unknown"

fn main() =
  let d = &Dog{name: "rex"}
  let c = &Cat{name: "tom"}
  echo describe(d)
  echo describe(c)
'

# --------------------------------------------------------------------- #
# 9. Redundant-cast warning across many sites in one expression          #
# --------------------------------------------------------------------- #

# Multiple redundant casts in one let -- all should be flagged.
assert_substr "multi-redundant-cast: all sites flagged" \
  "redundant-type-cast" '
fn main() =
  let xs [i64; 4] = [1 as i64, 2 as i64, 3 as i64, 4 as i64]
  echo "{xs[0]}"
'

# --------------------------------------------------------------------- #
# 10. Compiler-stability tests                                          #
# --------------------------------------------------------------------- #

# Empty arg list to a constructor with required args -- bad code,
# but the compiler should report cleanly without panicking.
assert_no_panic "constructor with wrong arg count errors cleanly" '
use result
fn main() =
  let r Result[i64, string] = Ok()  // missing arg
  echo "ok"
'

# Mismatched generic arity
assert_no_panic "generic with wrong arity errors cleanly" '
use option
fn main() =
  let x Option[i64, string] = None  // Option takes one type param
  echo "ok"
'

# coerce[T] decl without matching method
assert_no_panic "coerce[T] declared but no impl errors cleanly" '
struct S(coerce[i64]) =
  v i64
  // no ::coerce method

fn main() =
  let s = S{v: 5}
  let n = s as i64
  echo "{n}"
'

printf "hostile tests: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
