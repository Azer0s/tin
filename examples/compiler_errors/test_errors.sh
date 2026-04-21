#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TIN="${SCRIPT_DIR}/../../tin"

pass=0
fail=0

for f in "$SCRIPT_DIR"/*.tin; do
  name="$(basename "$f")"
  output="$("$TIN" build "$f" 2>&1 || true)"
  if [ -z "$output" ]; then
    echo "FAIL $name: expected a compiler error but got none"
    fail=$((fail + 1))
  else
    echo "ok   $name: $output"
    pass=$((pass + 1))
  fi
done

echo ""
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
