// tin runtime - test runner

#include "runtime.h"
#include <stdio.h>
#include <stdlib.h>
#include <setjmp.h>

static jmp_buf  _tin_test_jmpbuf;
static int      _tin_test_active   = 0;
static int64_t  _tin_tests_failed  = 0;

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
        printf("ok\n");
    } else {
        _tin_test_active = 0;
        _tin_tests_failed++;
    }
}

int64_t _tin_test_finish(int64_t total) {
    int64_t passed = total - _tin_tests_failed;
    printf("\n");
    if (_tin_tests_failed == 0) {
        printf("%lld test%s passed.\n", (long long)total,
               total == 1 ? "" : "s");
    } else {
        printf("%lld/%lld test%s failed.\n",
               (long long)_tin_tests_failed, (long long)total,
               _tin_tests_failed == 1 ? "" : "s");
        (void)passed;
    }
    return _tin_tests_failed > 0 ? 1 : 0;
}

// Used by the assert stdlib: prints "FAILED", writes msg to stderr, then
// longjmps back to the test runner (or exits if not in a test).
void _tin_assert_abort(const char *msg) {
    printf("FAILED\n");
    fprintf(stderr, "  %s\n", msg);
    if (_tin_test_active) {
        longjmp(_tin_test_jmpbuf, 1);
    } else {
        exit(1);
    }
}
