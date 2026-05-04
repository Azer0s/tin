# Stack traces

Tin's `stacktrace()` returns the current call chain as a list of
atoms shaped `"<symbol>+<offset>@<file>:<line>:<col>"`. The
implementation has two halves: a frame-pointer walker (resolves
return addresses) and a `pclntab` lookup (resolves addresses to
symbols + source positions).

## Frame-pointer walker

The runtime walks the saved frame-pointer chain rather than reading
DWARF: `rbp` on x86_64, `x29` on aarch64. Each frame stores the
caller's saved fp and return address at well-known offsets, so the
walk is a tight 4-instruction loop in `runtime/stacktrace.c`.

Consequences:

- Tin codegen tags every emitted IR function with
  `frame-pointer="all"` so user code never elides the frame pointer
  even at `-O2`.
- The runtime's own translation units (`runtime/*.c`) are compiled
  with `-fno-omit-frame-pointer -mno-omit-leaf-frame-pointer` when
  `stacktrace()` is reachable from the program (gated on linking to
  `_tin_register_stacktrace`).
- Third-party C linked via `#interop` is invisible to the walker
  unless the user opts in by adding `-fno-omit-frame-pointer` to
  their `//!+file.c -- ...` flag list. Without it, those frames just
  don't appear in the trace -- the walker terminates cleanly when
  it loses the chain.

DWARF is still emitted under `-g` for debuggers, but `stacktrace()`
never reads it.

## pclntab section

`file:line:col` resolution comes from a custom ELF/Mach-O section
called `__tin_pclntab` (Go's term, since the layout is similar
in spirit). The codegen post-pass at `codegen/pclntab.go` walks the
emitted IR and writes one record per emitted function:

```
<func_pc_lo> <func_pc_hi> <name_offset> <file_offset> <line> <col>
```

The runtime helper `runtime/pclntab.c` performs an in-image binary
search keyed on instruction pointer. It needs no dynamic-symbol
lookup, no `dladdr`, and no DWARF. The section adds ~5-10% to the
binary's `.rodata` size; programs that don't reach `stacktrace()`
linkage-wise let the linker dead-strip it via `--gc-sections` /
`-dead_strip`.

## Resolving non-Tin frames

When the IP falls outside any pclntab range (typically: libc, libpthread,
third-party `#interop`), the runtime falls back to `dladdr(3)`. On
Linux, that requires the symbol to be in `.dynsym`; tin's link adds
`-rdynamic` (or `--export-dynamic` with ld.lld) when stacktrace is
reachable so user fns are promoted into dynsym. macOS keeps local
symbols visible to `dladdr` until `strip` removes them, so the
`-rdynamic` cost is Linux-only.

Frames that resolve neither way render as `??+0x<addr>`.

## Cross-platform notes

| Platform     | Frame walker | pclntab | dladdr fallback |
|--------------|--------------|---------|-----------------|
| linux/amd64  | rbp          | yes     | needs `-rdynamic` |
| linux/arm64  | x29          | yes     | needs `-rdynamic` |
| darwin/amd64 | rbp          | yes     | implicit (Mach-O) |
| darwin/arm64 | x29          | yes     | implicit (Mach-O) |

No `libdw` / `libdwarf` dependency in any build.
