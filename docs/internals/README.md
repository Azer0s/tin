# Tin Runtime Internals

This directory documents the internals of the tin runtime (`runtime/`). The
runtime is a small C library compiled alongside every tin binary. It provides
the primitives that the LLVM IR emitted by the compiler calls at runtime.

## Source layout

```
runtime/
  runtime.h    shared types and forward declarations
  runtime.c    umbrella: #includes all sub-files below
  arc.c        Automatic Reference Counting
  strings.c    TinString operations
  slice.c      TinSlice (dynamic array) operations
  echo.c       typed echo / print helpers
  mem.c        raw malloc/free wrappers
  defer.c      defer chain, panic, recover
  len.c        len() builtins
  test.c       test runner
  atom.c       runtime atom table
  any.c        any-type equality
  fiber.c      M:N fiber scheduler, run queue, park/unpark, fiber join
  async_io.c   Non-blocking I/O integration (epoll/kqueue), async_read/write
  timer.c      Timer thread (sleep / millisecond timeouts)
  net.c        TCP socket helpers (tcp_listen, tcp_accept, close)

stdlib/sync/
  channel_arc.c   Channel[T] ring buffer with ARC retain/release, recv buffer
  sync_helpers.c  Mutex, RWMutex, Cond, AtomicI64, Future[T]

stdlib/reflect/
  reflect.c    type-atom reflection (compiled in via //!+reflect.c)
```

`runtime.c` is the only file the build system references directly. It
`#include`s the sub-files in dependency order so the whole runtime compiles as
a single translation unit. External C files that need to call into the runtime
(such as `reflect.c`) include `runtime.h` and are compiled via the `//!+file.c`
directive with `-I /path/to/runtime` on the include path.

## Documents

| File                                           | Topics                                                                                      |
|------------------------------------------------|---------------------------------------------------------------------------------------------|
| [memory.md](memory.md)                         | ARC, immortal sentinel, `TinRCHdr`, `_tin_rc_alloc/retain/release`, raw mem, heap promotion |
| [values.md](values.md)                         | `TinString`, `TinSlice`, echo/print, `len`, `any` equality                                  |
| [control.md](control.md)                       | Defer chain, `_tin_panic`, `_tin_recover`, assert                                           |
| [atoms.md](atoms.md)                           | Compile-time atom table, runtime atom learning, CRC32 collision resolution                  |
| [reflect.md](reflect.md)                       | Atom string format, `reflect.c` parsing, `fn_params` memory layout                          |
| [testing.md](testing.md)                       | Test runner, `setjmp`/`longjmp` isolation, `_tin_assert_abort`                              |
| [scheduler.md](scheduler.md)                   | M:N fiber scheduler, run queue, park/unpark, `await spawn` fiber join                       |
| [deinit.md](deinit.md)                         | Struct `deinit` lifecycle, call sites, nested structs, value semantics                      |
| [arc-threads.md](arc-threads.md)               | ARC thread safety, spawn/channel/Future ownership transfer                                  |
| [codegen-auto-yield.md](codegen-auto-yield.md) | Auto-yield at loop backedges, `{#no_autoyield}` opt-out                                     |
| [fiber-panic.md](fiber-panic.md)               | Fiber panic lifecycle, `await` re-raise, `defer+recover` in async fns, codegen invariants   |
| [clayout-structs.md](clayout-structs.md)       | cLayoutStructs wrapper+native layout, c_data_ptr, non-handover/handover/literal paths       |
| [tco.md](tco.md)                               | Tail call optimization: loop transformation, eligibility, IR shape, interception points      |
