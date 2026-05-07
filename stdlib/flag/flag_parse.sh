#!/usr/bin/env bash
# End-to-end checks for flag::parse.  Driven by `tin run` rather than
# `tin test` because `tin test` packs the four constructor tests in
# flag_test.tin into a single binary and the shared module-level
# registry leaks state across runs in ways that confuse the runner.
# A separate one-shot tin run per scenario sidesteps that entirely.
#
# Usage (from repo root):
#   go build -o tin . && bash stdlib/flag/flag_parse.sh

set -u

pass=0
fail=0

run_case() {
  local name=$1
  local expected=$2
  local src=$3
  local tmp
  tmp=$(mktemp /tmp/flag_parse.XXXXXX.tin)
  printf '%s\n' "$src" > "$tmp"
  local out
  out=$(./tin run "$tmp" 2>&1 || true)
  rm -f "$tmp"
  if [[ "$out" == *"$expected"* ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected substring: %s\n' "$expected"
    printf '        got: %s\n' "${out:0:400}"
    fail=$((fail + 1))
  fi
}

echo "flag::parse end-to-end tests"

run_case "Ok happy path with mixed flags and positionals" "OK v=true p=9090 s=hello pos=[extra]" '
use flag
fn main(_args [string]) =
  let v = flag::bool("p1_v", false, "")
  let p = flag::int("p1_port", 0, "")
  let s = flag::str("p1_s", "", "")

  let argv [string] = ["prog", "--p1_v", "--p1_port=9090", "--p1_s", "hello", "extra"]
  match flag::parse(argv):
    case Ok(pos):
      let joined = ""
      let i i64 = 0
      for i < len(pos):
        if i > 0:
          joined = joined ++ ","
        joined = joined ++ pos[i]
        i = i + 1
      echo "OK v={v.value} p={p.value} s={s.value} pos=[{joined}]"
    case Err(e):
      echo "ERR {e.message()}"
'

run_case "Err on unknown flag" "ERR_KIND UnknownFlag definitely_unknown" '
use flag
fn main(_args [string]) =
  let argv [string] = ["prog", "--definitely_unknown"]

  match flag::parse(argv):
    case Ok(_): echo "OK unexpected"
    case Err(e):
      match e._kind:
        case UnknownFlag(name): echo "ERR_KIND UnknownFlag {name}"
        case MissingValue(_):   echo "ERR_KIND MissingValue"
'

run_case "Err on missing value for int flag" "ERR_KIND MissingValue needsval" '
use flag
fn main(_args [string]) =
  flag::int("needsval", 0, "")
  let argv [string] = ["prog", "--needsval"]

  match flag::parse(argv):
    case Ok(_): echo "OK unexpected"
    case Err(e):
      match e._kind:
        case MissingValue(name): echo "ERR_KIND MissingValue {name}"
        case UnknownFlag(_):     echo "ERR_KIND UnknownFlag"
'

run_case "Err on missing value for string flag" "ERR_KIND MissingValue username" '
use flag
fn main(_args [string]) =
  flag::str("username", "", "")
  let argv [string] = ["prog", "--username"]

  match flag::parse(argv):
    case Ok(_): echo "OK unexpected"
    case Err(e):
      match e._kind:
        case MissingValue(name): echo "ERR_KIND MissingValue {name}"
        case UnknownFlag(_):     echo "ERR_KIND UnknownFlag"
'

run_case "double-dash forces remaining args to positional" "OK v=false pos=[--unknown,extra]" '
use flag
fn main(_args [string]) =
  let v = flag::bool("dd_v", false, "")
  let argv [string] = ["prog", "--", "--unknown", "extra"]

  match flag::parse(argv):
    case Ok(pos):
      let joined = ""
      let i i64 = 0
      for i < len(pos):
        if i > 0: joined = joined ++ ","
        joined = joined ++ pos[i]
        i = i + 1
      echo "OK v={v.value} pos=[{joined}]"
    case Err(e):
      echo "ERR {e.message()}"
'

# FlagError widens to *errors::Err so callers can flow it through generic
# error-aware code paths and still call .message() polymorphically.
run_case "FlagError widens to *errors::Err for polymorphic dispatch" "GENERIC unknown flag: --bogus" '
use flag
use errors
fn main(_args [string]) =
  let argv [string] = ["prog", "--bogus"]

  match flag::parse(argv):
    case Ok(_): echo "OK unexpected"
    case Err(e):
      let widened *errors::Err = &e
      echo "GENERIC {errors::message(widened)}"
'

# *errors::Err can be downcast back to the concrete *FlagError so the
# caller can recover the typed kind for structured handling.
run_case "*errors::Err downcasts back to *FlagError for kind inspection" "DOWNCAST UnknownFlag bogus2" '
use flag
use errors
fn main(_args [string]) =
  let argv [string] = ["prog", "--bogus2"]

  match flag::parse(argv):
    case Ok(_): echo "OK unexpected"
    case Err(e):
      let widened *errors::Err = &e
      let recovered = widened as *flag::FlagError

      match (*recovered).kind():
        case UnknownFlag(name):  echo "DOWNCAST UnknownFlag {name}"
        case MissingValue(name): echo "DOWNCAST MissingValue {name}"
'

printf 'flag::parse: %d passed, %d failed\n' "$pass" "$fail"
exit "$fail"
