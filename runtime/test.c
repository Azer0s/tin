// tin runtime - test runner

#include "runtime.h"
#include <stdio.h>
#include <stdlib.h>
#include <setjmp.h>
#include <unistd.h>

static jmp_buf  _tin_test_jmpbuf;
static int      _tin_test_active   = 0;
static int64_t  _tin_tests_failed  = 0;

static int _tin_color_stdout(void) {
    static int cached = -1;
    if (cached == -1) {
        const char *no_color = getenv("NO_COLOR");
        if (no_color && no_color[0] != '\0') {
            cached = 0;
        } else {
            const char *force = getenv("TIN_TEST_COLOR");
            if (force && force[0] != '\0') {
                cached = 1;
            } else {
                cached = isatty(fileno(stdout)) ? 1 : 0;
            }
        }
    }
    return cached;
}

#define C_GREEN(s) (_tin_color_stdout() ? "\x1b[32m" s "\x1b[0m" : s)
#define C_RED(s)   (_tin_color_stdout() ? "\x1b[31m" s "\x1b[0m" : s)

// Run a single test via setjmp/longjmp so assertion failures are caught
// and reported without killing the whole run.
void _tin_run_test(TinString desc, void *fn) {
    printf("test: %.*s ... ", (int)desc.len, desc.ptr);
    fflush(stdout);

    _tin_test_active = 1;
    if (setjmp(_tin_test_jmpbuf) == 0) {
        void (*f)(void) = (void (*)(void))fn;
        f();
        _tin_test_active = 0;
        printf("%s\n", C_GREEN("ok"));
    } else {
        _tin_test_active = 0;
        _tin_tests_failed++;
    }
}

int64_t _tin_test_finish(int64_t total) {
    int64_t passed = total - _tin_tests_failed;
    printf("\n");
    if (_tin_tests_failed == 0) {
        if (_tin_color_stdout()) {
            printf("\x1b[32m%lld test%s passed.\x1b[0m\n",
                   (long long)total, total == 1 ? "" : "s");
        } else {
            printf("%lld test%s passed.\n",
                   (long long)total, total == 1 ? "" : "s");
        }
    } else {
        if (_tin_color_stdout()) {
            printf("\x1b[31m%lld/%lld test%s failed.\x1b[0m\n",
                   (long long)_tin_tests_failed, (long long)total,
                   _tin_tests_failed == 1 ? "" : "s");
        } else {
            printf("%lld/%lld test%s failed.\n",
                   (long long)_tin_tests_failed, (long long)total,
                   _tin_tests_failed == 1 ? "" : "s");
        }
        (void)passed;
    }
    return _tin_tests_failed > 0 ? 1 : 0;
}

// Used by the assert stdlib: prints "FAILED", writes msg to stderr, then
// longjmps back to the test runner (or exits if not in a test).
void _tin_assert_abort(const char *msg) {
    printf("%s\n", C_RED("FAILED"));
    fprintf(stderr, "  %s\n", msg);
    if (_tin_test_active) {
        longjmp(_tin_test_jmpbuf, 1);
    } else {
        exit(1);
    }
}
