#!/usr/bin/env bash
# Verifies that bare `t[k]` on a missing key panics with the
# "index miss at L:C" message, instead of silently returning a zero
# value. Companion to examples/index_overload.tin (which exercises the
# comma-ok destructure form via assert).
set -u

repo=$(cd "$(dirname "$0")/.." && pwd)
tin="$repo/tin"

if [[ ! -x "$tin" ]]; then
  printf 'tin binary not found at %s; run `go build -o tin .` first\n' "$tin"
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cat > "$work/miss.tin" <<'EOF'
struct Bag (index[string, i64]) =
  keys [string]
  vals [i64]
  fn ::index(this Bag, k string) (i64, bool) =
    let i i64 = 0
    for i < len(this.keys):
      if this.keys[i] == k:
        return (this.vals[i], true)
      i = i + 1
    return (0, false)

fn main() =
  let b = Bag{keys: ["x"], vals: [42]}
  let v = b["never_set"]
  echo v
EOF

out=$("$tin" run "$work/miss.tin" 2>&1)
rc=$?

# Tin's panic exits non-zero; the message should be on stderr and contain
# the line / column of the bare `b["never_set"]` access.
if [[ "$out" == *"index miss"* && "$out" == *"never_set"* || "$out" == *"index miss at "* ]]; then
  printf '  ok    bare miss panics with location-tagged message\n'
  exit 0
fi

printf '  FAIL  expected "index miss at ..." panic\n'
printf '        got: %s\n' "$out"
printf '        rc:  %d\n' "$rc"
exit 1
