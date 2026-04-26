#!/usr/bin/env bash
# Compiler-warning tests for unreachable match cases / where clauses.
# Each case writes a tiny .tin source, compiles+runs it, and verifies the
# expected warning appears (or does NOT appear) on stderr.
# -Wno-unused-match-arms must silence every warning case.
#
# Usage (from the repo root):
#   go build -o tin . && bash examples/unreachable_arm_warnings.sh

set -u

pass=0
fail=0

run_case() {
  local name=$1
  local expect=$2  # "warn" or "silent"
  local match_substr=$3
  local extra_flag=$4
  local src=$5

  local tmp=$(mktemp --suffix=.tin)
  printf '%s\n' "$src" > "$tmp"

  local stderr_out
  if [[ -n "$extra_flag" ]]; then
    stderr_out=$(./tin run "$tmp" "$extra_flag" 2>&1 >/dev/null || true)
  else
    stderr_out=$(./tin run "$tmp" 2>&1 >/dev/null || true)
  fi

  rm -f "$tmp"

  local got_warning="no"
  if [[ "$stderr_out" == *"$match_substr"* ]]; then
    got_warning="yes"
  fi

  case "$expect" in
    warn)
      if [[ "$got_warning" == "yes" ]]; then
        printf '  ok    %s\n' "$name"; pass=$((pass+1))
      else
        printf '  FAIL  %s -- expected warning %q\n' "$name" "$match_substr"
        printf '        stderr: %s\n' "${stderr_out:0:300}"
        fail=$((fail+1))
      fi
      ;;
    silent)
      if [[ "$got_warning" == "no" ]]; then
        printf '  ok    %s\n' "$name"; pass=$((pass+1))
      else
        printf '  FAIL  %s -- expected NO warning, got %q\n' "$name" "$stderr_out"
        fail=$((fail+1))
      fi
      ;;
  esac
}

echo "unreachable-arm warning tests"

# 1. where with array patterns covering all + bare _ wildcard.
run_case "where: arr triple covers all, _ unreachable" "warn" "unreachable where" "" '
fn d(xs [i32]) string =
  where ([]):           "empty"
  where ([x]):          "one"
  where ([x, ...rest]): "many"
  where _:              "unreachable"

echo d([1, 2])
'

# 2. Same case suppressed with -Wno-unused-match-arms.
run_case "where: suppressed by -Wno-unused-match-arms" "silent" "warning" "-Wno-unused-match-arms" '
fn d(xs [i32]) string =
  where ([]):           "empty"
  where ([x]):          "one"
  where ([x, ...rest]): "many"
  where _:              "unreachable"

echo d([1, 2])
'

# 3. where with bool literal patterns covering both + _ unreachable.
run_case "where: bool true/false + _ unreachable" "warn" "unreachable where" "" '
fn yn(b bool) string =
  where (true):  "yes"
  where (false): "no"
  where _:       "??"

echo yn(true)
'

# 4. where with overlapping integer literal patterns.
run_case "where: duplicate int literal arm" "warn" "unreachable where" "" '
fn pick(n i32) string =
  where (0): "zero-1"
  where (0): "zero-2"
  where _:   "other"

echo pick(0)
'

# 5. where: catch-all binder followed by anything = anything is unreachable.
run_case "where: bind-all then more clauses" "warn" "unreachable where" "" '
fn echo_n(n i32) i32 =
  where (n): n
  where (0): 0

echo echo_n(5)
'

# 6. match: case 0 then case 0.
run_case "match: duplicate int literal case" "warn" "unreachable match case" "" '
fn pick(n i32) string =
  return match n:
    case 0: "first-zero"
    case 0: "second-zero"
    default: "other"

echo pick(0)
'

# 7. match: array patterns covering all + default.
run_case "match: array triple covers all, default unreachable" "warn" "unreachable match default" "" '
fn d(xs [i32]) string =
  return match xs:
    case []:           "empty"
    case [x]:          "one"
    case [x, ...rest]: "many"
    default:           "unreachable"

echo d([1, 2])
'

# 8. match: same suppressed with -Wno-unused-match-arms.
run_case "match: suppressed by -Wno-unused-match-arms" "silent" "warning" "-Wno-unused-match-arms" '
fn d(xs [i32]) string =
  return match xs:
    case []:           "empty"
    case [x]:          "one"
    case [x, ...rest]: "many"
    default:           "unreachable"

echo d([1, 2])
'

# 9. match: NO warning when default is genuinely needed (atom literals).
run_case "match: default needed (atom open domain)" "silent" "warning" "" '
fn color(a atom) string =
  return match a:
    case '\''red:   "R"
    case '\''green: "G"
    default:        "?"

echo color('\''red)
'

# 10. where: NO warning when bool catch-all is the only safety net.
run_case "where: bool guards need explicit _" "silent" "warning" "" '
fn classify(n i32) string =
  where n < 0: "neg"
  where n > 0: "pos"
  where _:     "zero"

echo classify(0)
'

# 11. where: guarded patterns never count as exhausting -- no false warning.
run_case "where: guarded clause does not cover" "silent" "warning" "" '
fn s(n i32) string =
  where (n) if n < 0: "neg"
  where (n) if n > 0: "pos"
  where _:            "zero"

echo s(5)
'

# 12. match: bool exhaustive cases -- the explicit `default:` is unreachable.
run_case "match: bool exhaustive default IS unreachable" "warn" "unreachable match default" "" '
fn yn(b bool) string =
  return match b:
    case true:  "y"
    case false: "n"
    default:    "?"

echo yn(true)
'

echo
printf 'unreachable-arm warning tests: %d passed, %d failed\n' "$pass" "$fail"
exit $([[ $fail -eq 0 ]] && echo 0 || echo 1)
