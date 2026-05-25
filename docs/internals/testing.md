# Test Runner

## Overview

`runtime/test.c` implements the test harness. It runs each `test` block in
isolation using `setjmp`/`longjmp` so that a single failing assertion does not
abort the entire test run. The `assert` stdlib (`stdlib/assert/assert.tin`)
builds on top of `_tin_assert_abort` to provide the user-facing assertion
functions.

---

## State

```c
static jmp_buf  _tin_test_jmpbuf;       // longjmp target for failing assertions
static int      _tin_test_active  = 0;  // 1 while a test function is executing
static int64_t  _tin_tests_failed = 0;  // cumulative count of failed tests
```

All three are `static` globals, meaning they are private to the runtime
translation unit and not visible to user code.

---

## `_tin_run_test`

```c
void _tin_run_test(TinString desc, void *fn);
```

Runs one test function and records the result.

1. Prints `"test: {desc} ... "` (no newline) and flushes stdout.
2. Sets `_tin_test_active = 1`.
3. Calls `setjmp(_tin_test_jmpbuf)`:
   - **First call** (returns `0`): casts `fn` to `void(*)(void)` and calls it.
     If the function returns normally, prints `"ok\n"` and clears
     `_tin_test_active`.
   - **Longjmp return** (returns nonzero): the test function called
     `_tin_assert_abort`, which already printed the failure message. The
     handler clears `_tin_test_active` and increments `_tin_tests_failed`.

The test function pointer is passed as `void*` to match the type-erased IR
representation; the cast is safe because all test functions have the same
signature (`void(void)`).

---

## `_tin_test_finish`

```c
int64_t _tin_test_finish(int64_t total);
```

Prints a summary after all tests have run:

```
(blank line)
N tests passed.
```
or:
```
(blank line)
F/N tests failed.
```

Returns `0` if all tests passed, `1` if any failed. The generated test-runner
`main` passes this return value to `exit()`.

---

## `_tin_assert_abort`

```c
void _tin_assert_abort(const char *msg);
```

Called by the `assert` stdlib when any assertion fails. It is the single point
where assertion failures escape into the test runner.

1. Prints `"FAILED\n"` to stdout (completing the `"test: ... FAILED"` line).
2. Prints `"  {msg}\n"` to stderr for the detailed failure message.
3. If `_tin_test_active == 1`: calls `longjmp(_tin_test_jmpbuf, 1)` to jump
   back to the `setjmp` in `_tin_run_test`.
4. If `_tin_test_active == 0`: calls `exit(1)` - an assertion outside a test
   block is a fatal error.

---

## Integration with the `assert` stdlib

The `assert` stdlib (`stdlib/assert/assert.tin`) implements the comparison and
message-formatting logic entirely in tin, then calls `_tin_assert_abort` with
the formatted message. For example:

```rust
fn equals(actual i64, expected i64) =
  if actual != expected
    _tin_assert_abort("expected " + str(expected) + ", got " + str(actual))
```

This separation keeps the runtime C code minimal: it only needs `setjmp`,
`longjmp`, and the two print calls. All assertion logic lives in tin.

---

## Compiler-generated test runner

When `tin test` compiles a file, the codegen (`cg.SetTestMode(true)`) switches
to test mode:

- Each `test "name" = body` block is compiled into a private function.
- Instead of a `main` function, the compiler generates a test-runner `main`
  that calls `_tin_run_test` once per test and finishes with `_tin_test_finish`.

A simplified view of the generated IR:

```llvm
define i32 @main() {
  ; call each test in declaration order
  call void @_tin_run_test({ i8*, i64 } { "kind", 4 }, i8* @test__kind)
  call void @_tin_run_test({ i8*, i64 } { "elem", 4 }, i8* @test__elem)
  ; ...
  %rc = call i64 @_tin_test_finish(i64 <total>)
  %rc32 = trunc i64 %rc to i32
  ret i32 %rc32
}
```

The exit code is `0` (all passed) or `1` (at least one failure), suitable for
use in CI pipelines.

---

## Directory mode

`tin test <dir>` calls `runDirTests(dir, extraFlags)` in `cmd/tin/main.go`. It reads
the directory entries once, skips non-`.tin` files, and skips files whose
parsed AST has no test blocks (`!cg.HasTests()`).

## Recursive directory mode (`dir/...`)

`tin test <dir>/...` strips the trailing `...` component (detected via
`filepath.Base(file) == "..."`), then calls `runDirTestsRecursive`. The
function walks the tree depth-first and calls `runDirTests` on every
subdirectory that contains at least one `.tin` file.

Directories named `wip` are skipped. This convention marks test files that
exercise compiler features which are not yet fully implemented -- they remain
in-tree as specification and regression targets but are excluded from the
normal CI gate. When the underlying compiler bug is fixed the file should be
moved out of `wip/` and into the parent directory.

## CI pipeline

The CI workflow (`.github/workflows/ci.yml`) runs:

1. `./tin test examples/... -lm` -- covers all test-block files in `examples/`
   and every subdirectory except `wip/`.
2. Per-binary build loops for `examples/fibers/` and `examples/arc_stress/`
   (these are build-and-run programs, not test-block files).
3. Selected valgrind runs for fiber and ARC stress binaries.
4. `examples/echo_server/stress_test.py` -- builds `echo_server_bad` and runs
   the Python stress harness against it.
5. `examples/io_stress/` valgrind loop -- compiles each test file with
   `tin build-test`, then runs the resulting binary under
   `valgrind --error-exitcode=1 --leak-check=no`.

## examples/stress_tests layout

```
examples/stress_tests/
  async_channel_patterns.tin   -- channel fan-out, fan-in, pipeline
  async_nested_await.tin       -- await spawn patterns, phi nodes across awaits
  deep_generics.tin            -- skipped: multi-line method chaining not parsed
  dominance_edge_cases.tin     -- IR dominance regression tests
  mixed_async_sync.tin         -- sync helpers called from async functions
  ...
  wip/
    init_deinit_stress.tin     -- compiler bug: deinit fires in FIFO not LIFO order
    return_closures.tin        -- compiler bug: closure-over-array hangs

```

## examples/io_stress layout

I/O-focused tests that run under valgrind:

```
examples/io_stress/
  channel_fanout.tin    -- N consumers, shared buffered channel, spawn do: capture
  concurrent_writes.tin -- N fibers concurrently echoing formatted lines
  string_pipeline.tin   -- 3-stage generator/transformer/sink pipeline
```

Each file uses test blocks so it is picked up by both `tin test examples/...`
and the dedicated valgrind step. The valgrind step uses `tin build-test` to
produce the same binary the test runner would execute.

Any value -- including structs -- can be passed as a named parameter to a spawned 
`#async` function. For structs whose resources are managed outside the ARC system 
(e.g. a C-level heap pointer), define `fn _fiber_retain(this S)` on the struct. 
The compiler calls this method in the ramp block for each parameter of that struct type,
before the initial suspend. `fn deinit` serves as the matching release: it is called by both the
caller's scope-exit and the fiber's scope-exit, so the underlying resource must
be reference-counted.

`sync::Channel[T]` implements this convention: it adds an atomic reference
count to the C control block (`TinChannel.ref_count`), `fn _fiber_retain`
increments it via `_tin_channel_retain`, and `fn deinit` calls
`_tin_channel_free` which decrements and frees only when the count reaches 0.
