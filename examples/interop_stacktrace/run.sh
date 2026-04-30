#!/usr/bin/env bash
# End-to-end test for stacktrace() across the C / Tin #interop
# boundary. See interop_st.tin and harness.c for the two scenarios.
#
# The harness MUST be compiled with -fno-omit-frame-pointer: Tin's
# stacktrace() is an FP-walking unwinder (see runtime/stacktrace.c),
# so any C frame on the stack that omits the frame pointer will
# truncate the chain at that frame.
#
# Usage (from repo root):
#   go build -o tin .
#   bash examples/interop_stacktrace/run.sh [--valgrind]

set -u

valgrind_mode=0
for a in "$@"; do
  if [[ "$a" == "--valgrind" ]]; then valgrind_mode=1; fi
done

pass=0
fail=0

check() {
  local name=$1
  local cond=$2
  local hint=${3-}

  if eval "$cond"; then
    printf '  ok    %s\n' "$name"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s\n' "$name"
    if [[ -n "$hint" ]]; then printf '        hint: %s\n' "$hint"; fi
    fail=$((fail + 1))
  fi
}

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
src_dir="$repo_root/examples/interop_stacktrace"
tin_bin="$repo_root/tin"

if [[ ! -x "$tin_bin" ]]; then
  printf 'tin binary not found at %s; run `go build -o tin .` first\n' "$tin_bin"
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cp "$src_dir/interop_st.tin" "$work/interop_st.tin"
cp "$src_dir/harness.c"      "$work/harness.c"

cd "$work"

if ! "$tin_bin" build --lib interop_st.tin -o interop_st.o --emit-header=interop_st.h \
    >/tmp/build_log 2>&1; then
  printf 'tin build failed:\n'
  cat /tmp/build_log
  exit 1
fi

# -fno-omit-frame-pointer is REQUIRED: Tin's FP walker bails at any
# frame that doesn't preserve fp.
# -rdynamic exposes c_call_back / main in .dynsym so dladdr (used by
# stacktrace.c's resolver) can name them.
# -DTIN_STACKTRACE=1 swaps the runtime stacktrace.c stub for the real
# FP walker - without it tin_capture_stacktrace returns 0 frames.
# -gline-tables-only embeds .debug_line so libdwfl can map IPs to
# "file:line:col"; without it, frames render as bare "symbol+0x<off>".
if ! clang -O1 -fno-omit-frame-pointer -rdynamic \
      -DTIN_STACKTRACE=1 -gline-tables-only \
      -o harness harness.c interop_st.o "$repo_root/runtime/runtime.c" \
      -lpthread -ldl -lm -ldw 2>/tmp/clang_log; then
  printf 'clang link failed:\n'
  cat /tmp/clang_log
  exit 1
fi

out=$(./harness 2>&1)

# Tin #interop functions appear in the trace as their generated
# wrapper symbol `__tin_interop_<name>`, not the bare Tin name.
check "scenario 1: prints direct frames count" \
  "echo \"\$out\" | grep -q '^DIRECT_FRAMES='"
check "scenario 1: trace contains print_st_direct wrapper" \
  "echo \"\$out\" | grep -q 'DIRECT:.*__tin_interop_print_st_direct'"
check "scenario 1: trace crosses into C (main visible)" \
  "echo \"\$out\" | grep -q 'DIRECT:.*main@'" \
  "if this fails, the harness probably wasn't built with -fno-omit-frame-pointer"

check "scenario 2: prints nested frames count" \
  "echo \"\$out\" | grep -q '^NESTED_FRAMES='"
check "scenario 2: trace contains print_st_nested wrapper" \
  "echo \"\$out\" | grep -q 'NESTED:.*__tin_interop_print_st_nested'"
check "scenario 2: trace contains c_call_back (Tin -> C -> Tin)" \
  "echo \"\$out\" | grep -q 'NESTED:.*c_call_back'" \
  "the C frame must show because c_call_back sits between two Tin frames"
check "scenario 2: trace contains run_nested (outer Tin frame)" \
  "echo \"\$out\" | grep -q 'NESTED:.*run_nested'" \
  "if this fails, LLVM tail-called c_call_back and tore down run_nested's frame; the trailing printf in run_nested should prevent it"

# Direct trace should be SHORTER than nested (nested = direct + 2 extra
# frames: c_call_back + run_nested wrapper, minus print_st_direct).
direct_n=$(echo "$out" | sed -n 's/^DIRECT_FRAMES=\([0-9]*\)$/\1/p')
nested_n=$(echo "$out" | sed -n 's/^NESTED_FRAMES=\([0-9]*\)$/\1/p')
check "nested trace is at least one frame longer than direct" \
  "[[ -n \"$direct_n\" && -n \"$nested_n\" && \"$nested_n\" -gt \"$direct_n\" ]]" \
  "direct=$direct_n nested=$nested_n"

if [[ "$valgrind_mode" -eq 1 ]]; then
  if ! command -v valgrind >/dev/null 2>&1; then
    printf '  SKIP  valgrind run (valgrind not installed)\n'
  else
    # Assert no leaks. Tin's lazy runtime init (fiber pool, io pool,
    # timer wheel) is torn down by the atexit handler tin_runtime_init
    # registers; without that hook this harness used to leak ~40 KiB
    # of still-reachable runtime state on every interop call.
    vg_out=$(valgrind --leak-check=full --errors-for-leak-kinds=all \
                       --error-exitcode=99 ./harness 2>&1)
    vg_rc=$?

    # Parse leak counts. Valgrind prints "All heap blocks were freed --
    # no leaks are possible" instead of a LEAK SUMMARY when the program
    # is fully clean, so each grep falls back to 0 when its line is
    # absent.
    leak_n() {
      local kind=$1
      local n
      n=$(echo "$vg_out" | sed -n "s/.*$kind lost: \([0-9,]*\) bytes.*/\1/p")
      if [[ -z "$n" ]]; then n=0; fi
      echo "$n"
    }
    definitely=$(leak_n definitely)
    indirectly=$(leak_n indirectly)
    still=$(echo "$vg_out" | sed -n 's/.*still reachable: \([0-9,]*\) bytes.*/\1/p')
    if [[ -z "$still" ]]; then still=0; fi
    check "valgrind: 0 definitely lost" "[[ \"$definitely\" == \"0\" ]]"
    check "valgrind: 0 indirectly lost" "[[ \"$indirectly\" == \"0\" ]]"
    check "valgrind: 0 still reachable" "[[ \"$still\" == \"0\" ]]" \
      "tin_runtime_init's atexit hook should drain the fiber/IO/timer pools"
    if [[ "$vg_rc" -ne 0 && "$vg_rc" -ne 99 ]]; then
      printf '  FAIL  valgrind run produced unexpected exit code %d\n' "$vg_rc"
      fail=$((fail + 1))
    fi
  fi
fi

printf '#interop stacktrace tests: %d passed, %d failed\n' "$pass" "$fail"
exit $fail
