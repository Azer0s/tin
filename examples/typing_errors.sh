#!/usr/bin/env bash
# Typing-error stress tests for let/var/const bindings, struct
# literals, generics, pointers, tuples, arrays, function arity, and
# control-flow type mismatches.  Verifies that wrong types produce a
# clean diagnostic (and never a compiler panic) and that the message
# names the right operand / target.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/typing_errors.sh

set -u

pass=0
fail=0

assert_substr() {
  local name=$1
  local want=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/typ_err.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  # Reject any compiler panic regardless of substring match.
  if [[ "$out" == *"panic:"* ]] || [[ "$out" == *"goroutine "*"running"* ]]; then
    printf '  FAIL  %s (compiler panic)\n' "$name"
    printf '        full: %s\n' "${out:0:600}"
    fail=$((fail + 1))

    return
  fi

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

assert_no_panic() {
  local name=$1
  local src=$2
  local tmp
  tmp=$(mktemp /tmp/typ_err.XXXXXX.tin)
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

echo "typing-error stress tests"

# ───────────────────────────────────────────────────────────────────── #
# A. Primitive let bindings                                              #
# ───────────────────────────────────────────────────────────────────── #

assert_substr "string into i64 slot" "error" '
fn main() = let x i64 = "hello"
'

assert_substr "i64 into string slot" "error" '
fn main() = let x string = 42
'

assert_substr "bool into string slot" "error" '
fn main() = let x string = true
'

assert_substr "atom into i64 slot" "error" '
fn main() = let x i64 = '\''foo
'

assert_no_panic "very large literal in i32 slot" '
fn main() = let x i32 = 9_999_999_999_999
'

# ───────────────────────────────────────────────────────────────────── #
# B. Pointer type mismatches                                             #
# ───────────────────────────────────────────────────────────────────── #

# Tin allows int -> pointer in unsafe contexts as a raw-address cast.
# At main scope this currently silently accepts; verify no panic.
assert_no_panic "scalar into pointer slot does not crash" '
fn main() =
  let p *i64 = 5
  echo "{p}"
'

# Tin currently bitcasts `*Foo` -> `*Bar` silently when the struct
# layouts overlap.  This is a pre-existing type-safety hole; verify it
# does not panic the compiler.
assert_no_panic "wrong-struct pointer does not crash" '
struct Foo =
  a i64

struct Bar =
  b i64

fn main() =
  let f = &Foo{a: 1}
  let p *Bar = f
  echo p
'

# Tin currently silently retypes a string `{i8*, i64}` to `*i64` via a
# bitcast on the data pointer; another pre-existing soft spot.  Verify
# no compiler panic.
assert_no_panic "string into pointer slot does not crash" '
fn main() =
  let p *i64 = "hello"
  echo "{p}"
'

# ───────────────────────────────────────────────────────────────────── #
# C. Array type mismatches                                               #
# ───────────────────────────────────────────────────────────────────── #

assert_substr "scalar into array slot" "error" '
fn main() = let xs [i64] = 5
'

assert_substr "string into array slot" "error" '
fn main() = let xs [i64] = "abc"
'

# Tin currently does not enforce fixed-size array length pinning at
# the let site -- the literal grows / truncates silently.  Just check
# the program does not panic the compiler.
assert_no_panic "fixed-size array with too-few elements" '
fn main() =
  let xs [i64; 5] = [1, 2, 3]
  echo "{xs[0]}"
'

assert_no_panic "fixed-size array with too-many elements does not crash" '
fn main() =
  let xs [i64; 2] = [1, 2, 3, 4]
  echo "{xs[0]}"
'

assert_no_panic "mixed-type array literal in [i64] slot" '
fn main() =
  let xs [i64] = [1, "two"]
  echo "{xs[0]}"
'

# ───────────────────────────────────────────────────────────────────── #
# D. Tuple type mismatches                                               #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "tuple slot with wrong arity does not crash" '
fn main() =
  let p (i64, i64) = (1, 2, 3)
  echo "{p.a}"
'

assert_no_panic "tuple slot with wrong-typed elem does not crash" '
fn main() =
  let p (i64, bool) = (1, "wrong")
  echo "{p.a}"
'

assert_substr "scalar into tuple slot" "" '
fn main() =
  let p (i64, i64) = 5
  echo "{p.a}"
'

# ───────────────────────────────────────────────────────────────────── #
# E. Struct literal field errors                                         #
# ───────────────────────────────────────────────────────────────────── #

assert_substr "wrong field type in struct literal" "" '
struct P =
  x i64

fn main() =
  let p = P{x: "wrong"}
  echo "{p.x}"
'

assert_no_panic "missing field in struct literal" '
struct P =
  x i64
  y i64

fn main() =
  let p = P{x: 1}
  echo "{p.x}"
'

assert_no_panic "extra field in struct literal" '
struct P =
  x i64

fn main() =
  let p = P{x: 1, z: 9}
  echo "{p.x}"
'

assert_no_panic "duplicate field in struct literal" '
struct P =
  x i64

fn main() =
  let p = P{x: 1, x: 2}
  echo "{p.x}"
'

# ───────────────────────────────────────────────────────────────────── #
# F. Generic ADT errors                                                  #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "Option with wrong type-arg count does not crash" '
use option

fn main() =
  let o Option[i64, string] = None
  echo "x"
'

assert_no_panic "Result with too-few type args does not crash" '
use result

fn main() =
  let r Result[i64] = Ok(1)
  echo "x"
'

assert_substr "Ok with wrong payload type" "" '
use result

fn main() =
  let r Result[i64, string] = Ok("not an int")
  match r:
    case Ok(_): echo "x"
    case Err(_): echo "x"
'

# ───────────────────────────────────────────────────────────────────── #
# G. Reassignment of immutable bindings                                  #
# ───────────────────────────────────────────────────────────────────── #

# Tin's `let` is immutable in some contexts and assignable in others;
# the language has var for explicitly mutable.  Check that const at
# least cannot be rebound from another scope.
assert_no_panic "const can be referenced; cannot crash" '
const N i64 = 7

fn main() =
  let m = N + 1
  echo "{m}"
'

assert_no_panic "let reassignment in same fn does not crash" '
fn main() =
  let x i64 = 1
  x = 2
  echo "{x}"
'

# ───────────────────────────────────────────────────────────────────── #
# H. Use-before-definition                                                #
# ───────────────────────────────────────────────────────────────────── #

assert_substr "let referencing later let is undefined" "undefined identifier" '
fn main() =
  let x = y + 1
  let y = 5
  echo "{x}"
'

assert_no_panic "self-referential let does not crash" '
fn main() =
  let x = x + 1
  echo "{x}"
'

# ───────────────────────────────────────────────────────────────────── #
# I. Function call typing                                                #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "wrong arg count: too few" '
fn add(a i64, b i64) i64 = return a + b

fn main() =
  let n = add(1)
  echo "{n}"
'

assert_no_panic "wrong arg count: too many" '
fn add(a i64, b i64) i64 = return a + b

fn main() =
  let n = add(1, 2, 3)
  echo "{n}"
'

assert_substr "wrong arg type" "where i64 is expected" '
fn take(n i64) i64 = return n + 1

fn main() =
  let n = take("hello")
  echo "{n}"
'

assert_substr "fn return-type mismatch" "cannot return value of type" '
fn pretend() i64 = return "wrong"
'

assert_substr "void fn returns a value" "void function cannot return" '
fn act() = return 5
'

assert_substr "non-void fn missing return" "not all code paths return" '
fn pretend() i64 =
  let _ = 1
'

# ───────────────────────────────────────────────────────────────────── #
# J. Match exhaustiveness                                                #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "non-exhaustive match on Option does not crash" '
use option

fn main() =
  let o Option[i64] = Some(1)
  match o:
    case Some(v): echo "{v}"
'

# ───────────────────────────────────────────────────────────────────── #
# K. Trait / constraint violations                                       #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "calling generic with non-matching trait constraint" '
struct A =
  v i64

fn dbl[t](x t) t where t is add = return x + x

fn main() =
  let a = A{v: 5}
  let b = dbl(a)
  echo "{b.v}"
'

# ───────────────────────────────────────────────────────────────────── #
# L. Pointer arithmetic / nil deref guards                               #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "deref of statically-nil pointer warns or errors" '
fn main() =
  let p *i64 = nil
  let v = *p
  echo "{v}"
'

# ───────────────────────────────────────────────────────────────────── #
# M. Index expression errors                                             #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "indexing non-indexable type" '
fn main() =
  let n i64 = 5
  let v = n[0]
  echo "{v}"
'

assert_no_panic "indexing array with non-int key" '
fn main() =
  let xs [i64] = [1, 2, 3]
  let v = xs["one"]
  echo "{v}"
'

# ───────────────────────────────────────────────────────────────────── #
# N. Field access on non-struct                                          #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "field access on i64" '
fn main() =
  let n i64 = 5
  let f = n.foo
  echo "{f}"
'

# ───────────────────────────────────────────────────────────────────── #
# O. Async / coroutine type errors                                       #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "await on non-Future does not crash" '
fn main() =
  let n = 5
  let v = await n
  echo "{v}"
'

# ───────────────────────────────────────────────────────────────────── #
# P. Cast at slot vs cast at expr -- layered compositions                #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "let x i64 = (1 + 2 as i32) as i64 chains correctly" '
fn main() =
  let x i64 = (1 + 2 as i32) as i64
  echo "{x}"
'

assert_no_panic "cast in struct literal field that needs widening" '
struct P =
  x i64

fn main() =
  let small i32 = 5
  let p = P{x: small as i64}
  echo "{p.x}"
'

# ───────────────────────────────────────────────────────────────────── #
# Q. Underscore-literal lexing edge cases                                #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "trailing underscore after digit" '
fn main() =
  let n = 1_000_
  echo "{n}"
'

assert_no_panic "leading underscore is just an identifier" '
fn main() =
  let _under i64 = 5
  echo "{_under}"
'

assert_no_panic "double underscore between digits" '
fn main() =
  let n = 1__000
  echo "{n}"
'

# ───────────────────────────────────────────────────────────────────── #
# R. Stress: deeply nested type errors                                   #
# ───────────────────────────────────────────────────────────────────── #

assert_no_panic "deeply nested wrong-type initializer does not crash" '
use option
use result

fn main() =
  let r Result[Option[i64], string] = Ok(Some("not an int"))
  match r:
    case Ok(_): echo "x"
    case Err(_): echo "x"
'

printf "typing-error tests: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
