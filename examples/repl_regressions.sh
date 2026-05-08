#!/usr/bin/env bash
# REPL regression tests.  Each case drives `tin repl` non-interactively
# via stdin and asserts on the captured output.
#
# Usage (from repo root):
#   go build -o tin . && bash examples/repl_regressions.sh

set -u

# Resolve the compiler binary once with an absolute path so the script
# works regardless of cwd.  `tin repl` writes ANSI color sequences when
# stdout is a TTY but not when piped; we still strip them defensively
# in case TIN_FORCE_COLOR or similar is set in the environment.
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/.." && pwd)
tin_bin="$repo_root/tin"

if [[ ! -x "$tin_bin" ]]; then
  printf 'FATAL: tin binary not found at %s -- run `go build -o tin .` first\n' "$tin_bin" >&2
  exit 2
fi

stderr_log=$(mktemp /tmp/repl_regressions_stderr.XXXXXX)
trap 'rm -f "$stderr_log"' EXIT

pass=0
fail=0
__repl_stdout=""
__repl_stderr=""

# strip_ansi removes CSI escape sequences (color, cursor moves, etc)
# so substring assertions are stable regardless of REPL color output.
strip_ansi() {
  perl -pe 's/\x1B\[[0-9;]*[a-zA-Z]//g'
}

# run_repl pipes a script + ":q" to `tin repl` and captures stdout
# and stderr SEPARATELY so a panic / linker error on stderr cannot
# satisfy a positive substring assertion that searches stdout.
run_repl() {
  local script=$1
  __repl_stdout=$(printf '%s\n:q\n' "$script" | "$tin_bin" repl 2>"$stderr_log" | strip_ansi)
  __repl_stderr=$(cat "$stderr_log")
}

# assert_repl_line asserts that the given line appears verbatim in
# stdout (anchored, after ANSI strip).  Avoids the "substring matches
# the banner / version string" false positive of plain `*$want*`.
assert_repl_line() {
  local name=$1
  local want_line=$2
  local script=$3
  run_repl "$script"
  if printf '%s\n' "$__repl_stdout" | grep -Fxq -- "$want_line"; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    printf '        expected line (exact): %s\n' "$want_line"
    printf '        stdout: %s\n' "${__repl_stdout:0:400}"
    printf '        stderr: %s\n' "${__repl_stderr:0:400}"
    fail=$((fail + 1))
  fi
}

# assert_no_link_error checks both streams for the dlopen / linker
# failure modes that the merge fix was written to prevent.  A silent
# regression that printed the right output AND a "symbol not found"
# error would otherwise pass the positive assertion above.
assert_no_link_error() {
  local name=$1
  if printf '%s\n%s\n' "$__repl_stdout" "$__repl_stderr" | \
       grep -qE 'symbol not found|undefined symbol|undefined reference|Undefined symbols'; then
    printf '  FAIL  %s (link / dlopen error in output)\n' "$name"
    printf '        stdout: %s\n' "${__repl_stdout:0:400}"
    printf '        stderr: %s\n' "${__repl_stderr:0:400}"
    fail=$((fail + 1))
  fi
}

echo "repl regressions"

# REPL must resolve symbols that live in pkg/mono modules.  Each cell
# .so previously contained only cg.mod, so calling any function from
# `errors`, `strings`, `math`, etc. failed at dlopen with
# `symbol not found in flat namespace '_<pkg>__<fn>'`.  Generate now
# folds pkgMods and monoMods into cg.mod for replMode.

assert_repl_line "REPL resolves errors:: stdlib symbol" \
  "ctx: inner" '
use errors
let a = errors::new("inner")
let b = errors::wrap(a, "ctx")
echo errors::message(b)
'
assert_no_link_error "REPL resolves errors:: stdlib symbol"

assert_repl_line "REPL resolves strings:: stdlib symbol" \
  "HELLO" '
use strings
echo strings::to_upper("hello")
'
assert_no_link_error "REPL resolves strings:: stdlib symbol"

assert_repl_line "REPL resolves math:: stdlib symbol" \
  "4" '
use math
echo math::sqrt(16.0)
'
assert_no_link_error "REPL resolves math:: stdlib symbol"

printf "repl regressions: %d passed, %d failed\n" "$pass" "$fail"
exit "$fail"
