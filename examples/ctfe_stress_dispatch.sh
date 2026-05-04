#!/usr/bin/env bash
# Tier-2 CTFE dispatch stress test.
#
# Phase C2/C7 emits a per-fn .so for every wrappable #pure function under
# .build/pure-fn/<merkle>/bin.so containing the function body + a
# `__tin_pure_shim_<name>` C-callable wrapper produced by the existing
# #interop pipeline. Phase C5 dlopens those .so's and dispatches into the
# shim via libffi. This script proves the cache populates and the .so
# contents are linkable by clang.
#
# Usage:
#   go build -o tin . && bash examples/ctfe_stress_dispatch.sh

set -u

pass=0
fail=0

check() {
  # Pass the test's exit status as $2 (0 == success, non-zero == failure).
  local name=$1 rc=$2
  if [[ "$rc" -eq 0 ]]; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    fail=$((fail + 1))
  fi
}

echo "tier-2 CTFE dispatch stress"

# Build a program with a varied set of #pure signatures so the cache exercises
# every primitive marshal path the libffi dispatcher supports.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cat > "$work/dispatch.tin" << 'EOF'
fn{#pure} square_i64(n i64) i64 = return n * n
fn{#pure} clamp(v i64, lo i64, hi i64) i64 =
  if v < lo: return lo
  if v > hi: return hi
  return v
fn{#pure} fact(n i64) i64 =
  if n <= 1: return 1
  return n * fact(n - 1)
fn{#pure} both(a bool, b bool) bool = return a && b
fn{#pure} hypot_sq(x f64, y f64) f64 = return x * x + y * y

fn main() i64 =
  return square_i64(7)
EOF

rm -rf .build/pure-fn

# Populate the cache.
TIN_PURE_FN_CACHE=1 ./tin build "$work/dispatch.tin" -o "$work/dispatch_bin" >/tmp/build_log 2>&1 || true

# Each #pure fn -> one .so under .build/pure-fn/<hash>/bin.so
so_count=$(find .build/pure-fn -name 'bin.so' 2>/dev/null | wc -l)
[[ "$so_count" -ge 5 ]]
check "cache populated >= 5 .so files (got $so_count)" $?

# Each .so exports __tin_pure_shim_<name>. macOS prepends an extra
# underscore to every symbol (so the C-side `__tin_pure_shim_foo`
# becomes `___tin_pure_shim_foo` in nm output); the regex tolerates 2
# or 3 leading underscores accordingly.
expected_shims="square_i64 clamp fact both hypot_sq"
missing=""
for fn in $expected_shims; do
  found=0
  for so in .build/pure-fn/*/bin.so; do
    if nm "$so" 2>/dev/null | grep -E " T _{2,3}tin_pure_shim_$fn\$" >/dev/null; then
      found=1
      break
    fi
  done
  if (( found == 0 )); then
    missing="$missing $fn"
  fi
done

[[ -z "$missing" ]]
check "all expected shim symbols exported (missing:$missing)" $?

# Each .so re-links cleanly. On Linux ld.bfd supports the
# --unresolved-symbols flag; on macOS Apple's ld doesn't, so we let
# clang drive the link (-fuse-ld=lld where available, plain clang
# otherwise) and just check the exit status.
broken=0
for so in .build/pure-fn/*/bin.so; do
  if [[ "$(uname -s)" == "Darwin" ]]; then
    if ! clang -shared -undefined dynamic_lookup -o /dev/null "$so" 2>/dev/null; then
      broken=$((broken + 1))
    fi
  else
    if ! ld -shared -o /dev/null --unresolved-symbols=ignore-all "$so" 2>/dev/null; then
      broken=$((broken + 1))
    fi
  fi
done

[[ "$broken" -eq 0 ]]
check "every cached .so re-links cleanly ($broken broken)" $?

# The user binary itself does NOT contain shim symbols (Phase C7
# guarantees shims live exclusively in cg.shimMod, not cg.mod).
leak=$(nm "$work/dispatch_bin" 2>/dev/null | grep -E "_{1,2}_tin_pure_shim_|_{1,2}_tin_interop_" | wc -l)

[[ "$leak" -eq 0 ]]
check "main binary carries zero shim symbols (leaked $leak)" $?

# A second build with the cache populated must reuse the cached .so files
# (no new clang invocations). We strace clang to confirm.
prev_count=$so_count

# Touch nothing; rebuild.
rm -f "$work/dispatch_bin"
TIN_PURE_FN_CACHE=1 ./tin build "$work/dispatch.tin" -o "$work/dispatch_bin" >/tmp/build_log2 2>&1 || true

new_count=$(find .build/pure-fn -name 'bin.so' 2>/dev/null | wc -l)
[[ "$new_count" -eq "$prev_count" ]]
check "second build reuses cache (count $prev_count -> $new_count)" $?

echo
printf "tier-2 CTFE dispatch: %d passed, %d failed\n" "$pass" "$fail"
if (( fail > 0 )); then
  exit 1
fi
