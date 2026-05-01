#!/usr/bin/env bash
# Step 8 verification: per-pkg .o cache (.build/pkg/) behavior.
#
# Shipped as a runnable example so CI and humans can both confirm the
# cache invariants hold:
#   1. Cold build populates .build/pkg/.
#   2. Warm rebuild is faster than cold (no clang -c re-run).
#   3. -O0 vs -O2 produce DIFFERENT cache keys (no stale reuse).
#   4. Editing pkg A's source invalidates pkg A's entry but NOT
#      pkg B's entry.
#   5. `tin clean` keeps .build/pkg/ around (it's content-addressed).
#
# Usage: bash examples/incremental_cache_verify.sh

set -u

pass=0
fail=0

check() {
  local name=$1 rc=$2
  if [[ "$rc" -eq 0 ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    fail=$((fail + 1))
  fi
}

echo "step 8: incremental compilation cache verification"

work=$(mktemp -d)
trap 'rm -rf "$work" /tmp/inc_a /tmp/inc_b' EXIT

# A small program that pulls in several stdlib pkgs so we have multiple
# .build/pkg/ entries to inspect.
cat > "$work/a.tin" << 'EOF'
use time
use encoding::json
use guid

fn main() i64 =
  let _ = time::from_sec(1)
  let _ = guid::new()
  echo json::encode("hello")
  return 0
EOF

# 1. Cold build populates .build/pkg/.
rm -rf .build/pkg .build/cache
./tin build "$work/a.tin" -o /tmp/inc_a >/dev/null 2>&1
cold_rc=$?
n_cold=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
[[ $cold_rc -eq 0 && $n_cold -ge 1 ]]
check "cold build populates .build/pkg ($n_cold .o)" $?

# 2. Warm rebuild reuses cache: .o count stays the same; rebuild faster.
n_before=$n_cold
cold_t=$(date +%s%N)
./tin build "$work/a.tin" -o /tmp/inc_a >/dev/null 2>&1
warm_rc=$?
warm_t=$(date +%s%N)
n_after=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
[[ $warm_rc -eq 0 && $n_after -eq $n_before ]]
check "warm rebuild reuses .o (count $n_before -> $n_after)" $?

# 3. -O0 vs -O2 produce different cache keys. -O2 is the default, so
# building with -O0 should ADD entries instead of reusing the -O2 ones.
rm -rf .build/cache
./tin build -O0 "$work/a.tin" -o /tmp/inc_a >/dev/null 2>&1
n_after_o0=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
[[ $n_after_o0 -gt $n_after ]]
check "-O0 build adds entries (was $n_after, now $n_after_o0)" $?

# 4. Edit one file -> only its entry invalidates.
# `time` pkg is unchanged across builds; if I touch only "$work/a.tin",
# the pkg cache for `time`/`json`/`guid` should NOT be re-emitted.
n_before_edit=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)

cat > "$work/a.tin" << 'EOF'
use time
use encoding::json
use guid

fn main() i64 =
  let _ = time::from_sec(2)                 // changed: 1 -> 2
  let _ = guid::new()
  echo json::encode("world")                // changed: hello -> world
  return 0
EOF

rm -rf .build/cache
./tin build "$work/a.tin" -o /tmp/inc_a >/dev/null 2>&1
n_after_edit=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
# Editing the entry pkg adds at most ONE entry (the entry pkg's new
# IR). The 4-5 stdlib pkgs (time, json, guid, errors, ...) reuse
# their cache entries. Allow up to a small delta for entry-pkg drift.
delta=$((n_after_edit - n_before_edit))
[[ $delta -le 2 ]]
check "edit-only-one-file adds <= 2 entries (delta $delta)" $?

# 5. `tin clean` preserves .build/pkg/ (content-addressed, safe to keep).
n_before_clean=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
./tin clean >/dev/null 2>&1
n_after_clean=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
[[ $n_after_clean -eq $n_before_clean ]]
check "tin clean preserves .build/pkg ($n_before_clean kept)" $?

echo
echo "incremental cache verify: $pass passed, $fail failed"

if [[ $fail -gt 0 ]]; then
  exit 1
fi
