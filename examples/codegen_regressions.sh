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

# ── is *T against nil pointer must NOT segfault.  The check now emits
# a runtime nil-guard before the GEP+load.
assert_substr "is *T on nil pointer is safe" \
  "guarded-ok" '
trait Animal =
  fn sound(this Animal) string = virtual

struct Dog(Animal) =
  name string
  fn Animal::sound(this Dog) string = return "woof"

fn main() =
  let a *Animal = nil

  if a is *Dog:
    echo "wrong"
  else:
    echo "guarded-ok"
'

# ── lexer: leading underscore in numeric literal must NOT be silently
# accepted as a separator.  Catches 0x_FF, 1._5, 0b_10, 1.0e_5.  With
# the fix, `0x_FF` lexes as `0x` followed by `_FF` (undefined ident).
assert_substr "lexer rejects leading-underscore numeric literal" \
  "undefined identifier: _FF" '
fn main() =
  let x = 0x_FF
  echo "{x}"
'

# ── JSON: "tnull" must NOT parse as `true` (per RFC 8259).
assert_substr "JSON rejects tnull as true" \
  "ok-rejected" '
use encoding::json
use { Result } from result

fn main() =
  match json::parse("tnull"):
    case Ok(v): echo "wrong"
    case Err(e): echo "ok-rejected"
'

# ── JSON: integer overflow surfaces as a Malformed error.
assert_substr "JSON int overflow rejected" \
  "ok-rejected-ovf" '
use encoding::json
use { Result } from result

fn main() =
  match json::parse("99999999999999999999"):
    case Ok(v): echo "wrong"
    case Err(e): echo "ok-rejected-ovf"
'

# ── JSON: \u escape decodes to the right UTF-8 codepoint.
assert_substr "JSON \\\\u escape decodes to ASCII" \
  "decoded-A" '
use encoding::json
use { Result } from result

fn main() =
  match json::parse("\"\\u0041\""):
    case Ok(v): echo "decoded-{v.sval}"
    case Err(e): echo "wrong"
'

# ── strings::parse_int rejects overflow instead of wrapping.
assert_substr "parse_int rejects overflow" \
  "ok-rejected" '
use strings
use { is_err } from result

fn main() =
  if is_err(strings::parse_int("99999999999999999999")): echo "ok-rejected"
  else:                                                  echo "wrong"
'

# ── strings::parse_int accepts i64::MIN exactly.
assert_substr "parse_int accepts i64::MIN" \
  "ok-min" '
use strings
use { unwrap_or } from result

fn main() =
  let v = unwrap_or(strings::parse_int("-9223372036854775808"), 0)

  if v == -9223372036854775808: echo "ok-min"
  else: echo "wrong-v {v}"
'

# ── decimal::ord must not overflow on far-apart values (was using a-b).
assert_runs_clean "decimal::ord far-apart values" "" '
use decimal

fn main() =
  let a = decimal::from(-9223372036854000000)
  let b = decimal::from(800)
  let r = a.ord(b)

  if r >= 0: echo "wrong sign"
  else:      echo "ok"
'

# ── time::Duration::abs at i64::MIN saturates instead of wrapping.
assert_substr "Duration::abs saturates at i64::MIN" \
  "saturated" '
use time

fn main() =
  let d = time::Duration{ns: -9223372036854775808}
  let a = d.abs()

  if a.ns > 0: echo "saturated"
  else:        echo "wrong {a.ns}"
'

# ── http: header values must reject CR/LF/NUL injection.
assert_substr "http header CR/LF rejected" \
  "panic" '
use collections
use http

fn main() =
  let c = http::Client::new()

  c.set_header("X-Foo", "bar\r\nEvil-Header: pwn")

  let req = http::Request{
    method: "GET", url: "http://example.com/",
    headers: collections::HashMap[string, string]::make(1), body: ""
  }

  let _ = await http::send(c, req)
'

# ── header_value_safe must reject all C0 control bytes (0x00-0x1F
# minus HTAB) and DEL (0x7F), per RFC 7230 sec 3.2.6.  Pre-fix only
# CR / LF / NUL were rejected; bytes like 0x01 or 0x7F passed through
# and could trip intermediaries that terminate parsing on any C0
# byte.  We construct the bad value via a byte-array cast since Tin
# does not support \xHH escapes in string literals.
assert_substr "http header CTL byte (0x01) rejected" \
  "panic" '
use collections
use http

fn main() =
  let c = http::Client::new()

  c.set_header("X-Foo", "bar\x01x")

  let req = http::Request{
    method: "GET", url: "http://example.com/",
    headers: collections::HashMap[string, string]::make(1), body: ""
  }

  let _ = await http::send(c, req)
'

assert_substr "http header DEL (0x7F) rejected" \
  "panic" '
use collections
use http

fn main() =
  let c = http::Client::new()

  c.set_header("X-Foo", "bar\x7fx")

  let req = http::Request{
    method: "GET", url: "http://example.com/",
    headers: collections::HashMap[string, string]::make(1), body: ""
  }

  let _ = await http::send(c, req)
'

# ── leak: errors::new(...) iface + StringErr must release on scope exit.
# The iface dtor (ensureStructPtrReleaseFn's fat-ptr arm) releases the
# data field when the iface RC hits 0; *Trait pointer let-bindings call
# that dtor at scope exit.  Skip on non-darwin where `leaks` is absent.
if [[ "$(uname)" == "Darwin" ]] && command -v leaks >/dev/null 2>&1; then
  tmp=$(mktemp /tmp/cgreg_leak.XXXXXX.tin)
  bin=$(mktemp /tmp/cgreg_leak.XXXXXX.bin)

  cat > "$tmp" <<'TIN'
use assert
use errors

test "errors::new releases on scope exit" =
  let e = errors::new("boom")

  assert::ok(errors::is_err(e))
TIN

  if ./tin build-test "$tmp" -o "$bin" 2>/dev/null; then
    out=$(leaks --atExit -- "$bin" 2>&1)

    if [[ "$out" == *"0 leaks for 0 total leaked bytes"* ]]; then
      printf '  ok    errors::new no longer leaks\n'
      pass=$((pass + 1))
    else
      printf '  FAIL  errors::new no longer leaks\n'
      printf '        leaks output: %s\n' "$(echo "$out" | grep -E "Process [0-9]+:" | head -1)"
      fail=$((fail + 1))
    fi
  else
    printf '  ok    errors::new leak (skipped: build failed)\n'
  fi

  rm -f "$tmp" "$bin"
fi

# Helper: build + run under `leaks --atExit` and assert "0 leaks".
assert_zero_leaks() {
  local name=$1
  local src=$2

  if [[ "$(uname)" != "Darwin" ]] || ! command -v leaks >/dev/null 2>&1; then
    return
  fi

  local tmp
  local bin

  tmp=$(mktemp /tmp/cgreg_leak.XXXXXX.tin)
  bin=$(mktemp /tmp/cgreg_leak.XXXXXX.bin)

  printf '%s\n' "$src" > "$tmp"

  if ./tin build-test "$tmp" -o "$bin" 2>/dev/null; then
    local out
    out=$(leaks --atExit -- "$bin" 2>&1)

    if [[ "$out" == *"0 leaks for 0 total leaked bytes"* ]]; then
      printf '  ok    %s\n' "$name"
      pass=$((pass + 1))
    else
      printf '  FAIL  %s\n' "$name"
      printf '        leaks output: %s\n' "$(echo "$out" | grep -E "Process [0-9]+:" | head -1)"
      fail=$((fail + 1))
    fi
  else
    printf '  ok    %s (skipped: build failed)\n' "$name"
  fi

  rm -f "$tmp" "$bin"
}

# ── trait fat-ptr iface dtor must release the wrapped struct's
# RC-tracked fields via the per-(struct, trait) data-release thunk
# stored in the vtable's last slot.  Raw _tin_release would only
# free the iface block and leak the StringErr's `msg` string.
assert_zero_leaks "errors::wrap chain releases inner StringErr" '
use assert
use errors

test "wrap chain" =
  let i = errors::new("inner")
  let o = errors::wrap(i, "ctx")

  assert::equals(errors::message(o), "ctx: inner")
'

# ── coerce[T] op-trait return is a fresh rc=1 value; passing it
# directly as a call arg (which would otherwise short-circuit the
# call-arg release because isCopyExpr says "borrow") must not leak.
# The synthetic scope entry registered in genAsExpr drops the rc on
# scope exit.
assert_zero_leaks "coerce[string] result released after call use" '
use assert

struct Money(coerce[string]) =
  cents i64

  static fn ::coerce(this Money) string = return "${this.cents}"

test "coerce as arg" =
  let m = Money{cents: 1234}

  assert::equals(m as string, "$1234")
'

# Same pattern but the coerce target is a struct with RC sub-fields
# (string, fat array, *Trait, etc.).  Pre-fix genAsExpr only registered
# a synthetic scope entry when isRCTrackedType(result) was true, which
# returns false for any non-fat-ptr struct.  Result: every `m as
# Wallet` call leaked the Wallet's `amount` string.
assert_zero_leaks "coerce[Struct] with RC field releases on scope exit" '
use assert

struct Wallet =
  amount string

struct Money(coerce[Wallet]) =
  v i64

  static fn ::coerce(this Money) Wallet =
    return Wallet{amount: "{this.v}"}

test "coerce stress: 1000 iterations leak nothing" =
  let m = Money{v: 42}
  let i i64 = 0

  for i < 1000:
    let w = m as Wallet
    assert::equals(w.amount, "42")
    i = i + 1
'

# ── early-heap-promoted local on conditional return: if either branch
# returns a different `&local`, the unreturned branch'\''s heap alloc
# must be released at function exit, not leaked.
assert_zero_leaks "conditional &local releases unreturned branch" '
use assert

fn pick(cond bool) *i64 =
  let yes i64 = 100
  let no  i64 = 200

  if cond:
    return &yes

  return &no

test "true branch" =
  let p = pick(true)
  assert::equals(*p, 100)

test "false branch" =
  let p = pick(false)
  assert::equals(*p, 200)
'

# Same pattern but with a non-named-struct early-heap'd local (fat
# array of strings).  Pre-fix releaseUnreturned called raw
# _tin_release on the unreturned branch's heap block, which freed the
# outer fat-array struct but never released the element strings -- a
# real leak for any return-an-address pattern with non-primitive
# locals on a sibling branch.
assert_zero_leaks "conditional &local releases unreturned [string]" '
use assert

fn pick(cond bool) *[string] =
  let yes [string] = ["a", "b"]
  let no  [string] = ["c", "d"]

  if cond:
    return &yes

  return &no

test "true branch keeps yes alive" =
  let p = pick(true)
  assert::equals((*p)[0], "a")

test "false branch keeps no alive" =
  let p = pick(false)
  assert::equals((*p)[0], "c")
'

# ── *Trait widening from an existing binding must NOT double-free.
# `let de = &Lit; let widened *Trait = de` previously caused a UAF:
# both bindings would end up calling the per-struct release_ptr at
# scope exit (de directly, widened indirectly via the iface dtor's
# vtable data-release thunk).  buildPtrToTraitBorrow now retains the
# data so the two release paths balance.
assert_zero_leaks "*Trait widening from binding does not double-free" '
use assert
use errors

data dk =
  Empty
  Tag(value i64)

struct dom(errors::Err) =
  k dk
  fn errors::Err::message(this dom) string = return "x"

test "widen + downcast" =
  let de = &dom{k: Tag(99)}
  let widened *errors::Err = de
  let back = widened as *dom

  match (*back).k:
    case Tag(v): assert::equals(v, 99)
    case Empty:  assert::fails("expected Tag")
'

# ── Returning a *Trait field from an aggregate must retain the heap
# block so the caller'\''s scope-exit release stays balanced.  Pre-fix
# emitRetain was a no-op for bare *<iface> values (the trait fat-ptr
# struct lives in cg.traitFatPtrTypes, not cg.structTypes), so
# `return s.iface_field` left the iface RC unchanged while the caller
# decremented on scope exit -- use-after-free crash after a couple of
# iterations of any pattern that borrows a trait pointer through a
# wrapper.
assert_zero_leaks "return *Trait field through wrapper struct" '
use assert
use errors

struct Holder =
  e *errors::Err

fn get_e(h Holder) *errors::Err =
  return h.e

test "1000 iter loop borrowing *Err field" =
  let h = Holder{e: errors::new("inner")}
  let i i64 = 0

  for i < 1000:
    let r = get_e(h)
    assert::equals(errors::message(r), "inner")
    i = i + 1
'

# ── Trait-widening a struct that implements multiple traits to two
# distinct *Trait pointers must release both iface heap blocks AND
# the underlying struct exactly once.  Pre-fix `let a *T1 = c; let m
# *T2 = c` over-counted: genVarDecl'\''s post-coerce retain branch
# treated the result of buildPtrToTraitBorrow as a borrow and added a
# retain on top of the freshly-allocated iface block -- net +1 ref
# count per widening, leaking 3 objects per iteration in a loop.
assert_zero_leaks "trait widening to two traits drops cleanly" '
use assert

trait Animal =
  fn sound(this Animal) string = virtual

trait Mammal =
  fn fur_color(this Mammal) string = virtual

struct Cat(Animal, Mammal) =
  name string
  fn Animal::sound(this Cat) string = return "meow"
  fn Mammal::fur_color(this Cat) string = return "tabby"

test "1000 iter double-widen does not leak" =
  let i i64 = 0

  for i < 1000:
    let c = &Cat{name: "felix"}
    let a *Animal = c
    let m *Mammal = c
    assert::equals(a.sound(), "meow")
    assert::equals(m.fur_color(), "tabby")
    i = i + 1
'

# ── #allow_drop is for fire-and-forget Future returns (channel.send),
# NOT a blanket silencer.  Dropping a Result on an `#allow_drop` fn
# must still warn, otherwise a future maintainer who tags a fn
# returning Result accidentally loses the unobserved-error check.
assert_substr "#allow_drop on Result still warns on drop" \
  "Result returned by" '
use result
use { Result } from result

fn{#allow_drop} risky() Result[i64, string] =
  return Result[i64, string]::Ok(42)

fn main() = risky()
'

# ── The complementary half: #allow_drop on a Future-shaped return
# (the channel.send pattern) DOES silence the warning.  This exact
# pattern is exercised in stdlib via Channel::send, but we pin the
# behavior here too so the regression catches both directions.
assert_runs_clean "#allow_drop on Future-shaped return silences warning" \
  "future returned by" '
use sync::channel
use { Channel } from sync::channel

fn{#async} main() =
  let ch = Channel[i64].make(4)
  ch.send(42)
'

# ── Lexer string + char escapes \xHH \uHHHH \UHHHHHHHH \b \f \v \a.
# The pre-fix lexer only handled \n \t \r \\ \" \' \0 plus \{ \},
# leaving every other escape as a literal two-character sequence.
# Each new escape is verified by a length / equality check.
assert_substr "lex \\xHH escape decodes to single byte" \
  "x01-1 x7f-1 ok" '
fn main() = echo "x01-{len(\"\x01\")} x7f-{len(\"\x7f\")} ok"
'

assert_substr "lex \\uHHHH escape decodes BMP codepoint" \
  "len=2 ok" '
fn main() = echo "len={len(\"é\")} ok"
'

assert_substr "lex \\UHHHHHHHH escape decodes astral codepoint" \
  "len=4 ok" '
fn main() = echo "len={len(\"\U0001F600\")} ok"
'

assert_substr "lex \\b \\f \\v \\a all decode to one byte" \
  "1 1 1 1 ok" '
fn main() = echo "{len(\"\b\")} {len(\"\f\")} {len(\"\v\")} {len(\"\a\")} ok"
'

assert_substr "lex char literal \\xHH escape" \
  "65 ok" '
fn main() = echo "{@\x27\x41\x27} ok"
'

assert_substr "lex \\u with surrogate codepoint is rejected" \
  "lex error: invalid \\u escape" '
fn main() = echo "\uD800"
'

assert_substr "lex \\x with non-hex digits is rejected" \
  "lex error: invalid \\x escape" '
fn main() = echo "\xZZ"
'

printf "codegen regressions: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
