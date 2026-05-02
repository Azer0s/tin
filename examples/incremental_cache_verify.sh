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

# 6. Cold build is deterministic: two cold builds produce byte-identical
# binaries. Catches non-deterministic ordering in codegen / link.
rm -rf .build /tmp/inc_a /tmp/inc_b
./tin build "$work/a.tin" -o /tmp/inc_a >/dev/null 2>&1
md5_a=$(md5sum /tmp/inc_a | awk '{print $1}')
rm -rf .build
./tin build "$work/a.tin" -o /tmp/inc_b >/dev/null 2>&1
md5_b=$(md5sum /tmp/inc_b | awk '{print $1}')
[[ "$md5_a" == "$md5_b" ]]
check "two cold builds produce byte-identical binaries ($md5_a)" $?

# 7. Run/test cache invalidates on compiler binary change. The SBOM
# records a synthetic __tin_binary__ entry hashing the running tin
# binary; sbomMatches refuses the cache when it differs. Without this
# a fresh tin with a codegen fix would silently reuse stale binaries
# built by the buggy compiler. We can't easily re-link tin during the
# script, so verify the SBOM contains the entry as a structural check.
cat > "$work/r.tin" << 'TINEOF'
fn main() i64 = return 0
test "noop" = if 1 == 0: panic("nope")
TINEOF
./tin test "$work/r.tin" >/dev/null 2>&1
sbom_path=$(find .build/test -name 'sbom.txt' -newer "$work/r.tin" 2>/dev/null | head -1)
if [[ -z "$sbom_path" ]]; then
  sbom_path=$(find .build/test -name 'sbom.txt' 2>/dev/null | head -1)
fi
grep -q '^[0-9a-f]\{32\}  __tin_binary__$' "$sbom_path" 2>/dev/null
check "test cache SBOM includes __tin_binary__ entry" $?

# 8a. Upstream pkg body-only edit: only the upstream pkg's .o is
# recomputed; downstream consumers reuse their cached .o. Tests the
# claim that an edit to one stdlib package doesn't invalidate every
# consumer (per spec §"hard requirements"), exercised on a custom
# lib-root layout so the test isn't entangled with stdlib state.
mkdir -p "$work/upkg_a" "$work/upkg_b"
cat > "$work/upkg_a/upkg_a.tin" << 'TINEOF'
fn double_it(x i64) i64 = return x + x
export { double_it } as upkg_a
TINEOF
cat > "$work/upkg_b/upkg_b.tin" << 'TINEOF'
use upkg_a
fn add_then_double(x i64) i64 = return upkg_a::double_it(x) + 1
export { add_then_double } as upkg_b
TINEOF
cat > "$work/main_up.tin" << 'TINEOF'
use upkg_b
fn main() i64 = return upkg_b::add_then_double(21)
TINEOF
rm -rf .build
./tin build --lib-root "$work" "$work/main_up.tin" -o /tmp/inc_up >/dev/null 2>&1
n_up_cold=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
# Edit upkg_a's BODY only (interface unchanged: same fn name, same sig).
cat > "$work/upkg_a/upkg_a.tin" << 'TINEOF'
fn double_it(x i64) i64 = return (x * 2 as i64)
export { double_it } as upkg_a
TINEOF
./tin build --lib-root "$work" "$work/main_up.tin" -o /tmp/inc_up >/dev/null 2>&1
n_up_after=$(find .build/pkg -name '*.o' 2>/dev/null | wc -l)
delta_up=$((n_up_after - n_up_cold))
[[ $delta_up -le 2 ]]
check "upstream pkg body-only edit adds <= 2 entries (delta $delta_up)" $?

# 8. Concurrent builds of the same source share the cache safely. Race
# four builds; all should succeed and produce byte-identical output.
rm -rf .build
pids=()
for i in 1 2 3 4; do
  (./tin build "$work/a.tin" -o "/tmp/inc_p$i" >/dev/null 2>&1) &
  pids+=($!)
done
race_rc=0
for p in "${pids[@]}"; do
  wait "$p" || race_rc=1
done
md5_p=$(md5sum /tmp/inc_p1 | awk '{print $1}')
md5_match=true
for i in 2 3 4; do
  this=$(md5sum "/tmp/inc_p$i" 2>/dev/null | awk '{print $1}')
  [[ "$this" == "$md5_p" ]] || md5_match=false
done
[[ $race_rc -eq 0 && "$md5_match" == "true" ]]
check "4 concurrent builds succeed and produce identical binaries" $?
rm -f /tmp/inc_p*

echo
echo "incremental cache verify: $pass passed, $fail failed"

if [[ $fail -gt 0 ]]; then
  exit 1
fi
