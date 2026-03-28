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

```tin
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
