#!/usr/bin/env bash
# Phase 0 fall-through: when a struct lacks an op-trait impl for the rhs type,
# we should still get the "binary operator not defined" compile error - not a
# silent compile or incorrect dispatch.
set -u

cd "$(dirname "$0")/.."
TMP=$(mktemp /tmp/op_phase0_XXXXXX.tin)
trap 'rm -f "$TMP"' EXIT

cat > "$TMP" <<'EOF'
struct Foo(add[Foo, Foo]) =
  v i64
  fn ::add(this Foo, other Foo) Foo = return Foo{v: this.v + other.v}
fn main() =
  let a = Foo{v: 1}
  let b = a + 2
  return
EOF

OUT=$(./tin run "$TMP" 2>&1)
ST=$?

echo "$OUT" | grep -qi "binary operator" || {
  echo "FAIL: expected 'binary operator' error, got:"
  echo "$OUT"
  exit 1
}

if [ "$ST" -eq 0 ]; then
  echo "FAIL: expected non-zero exit, got 0"
  exit 1
fi

echo "ok: phase 0 fall-through still errors"
