/* C harness exercising stacktrace() across the C / Tin boundary.
 *
 * Two scenarios:
 *
 *   1. C -> Tin (direct): main() calls print_st_direct(), which is a
 *      Tin #interop entry that captures and prints its own
 *      stacktrace. We expect to see the print_st_direct frame plus
 *      surrounding C frames (main, _start) when this file is compiled
 *      with -fno-omit-frame-pointer (see run.sh).
 *
 *   2. Tin -> C -> Tin: main() calls run_nested() (Tin #interop),
 *      which calls c_call_back() (defined here in C), which in turn
 *      calls print_st_nested() (Tin #interop) to capture the trace.
 *      We expect to see print_st_nested at the top, c_call_back next,
 *      then run_nested's wrapper, then main.
 *
 * The Tin side prints lines tagged DIRECT_FRAMES= / NESTED_FRAMES= and
 * DIRECT: / NESTED:; run.sh greps for those prefixes and asserts that
 * each scenario produces a stacktrace AND that the chain crosses the
 * C boundary (i.e. contains an entry referencing this harness).
 */

#include "interop_st.h"
#include <stdio.h>

void c_call_back(void) {
    fprintf(stderr, "C: c_call_back entered\n");
    print_st_nested();
    fprintf(stderr, "C: c_call_back returning\n");
}

int main(void) {
    fprintf(stderr, "C: main start\n");

    fprintf(stderr, "C: -- scenario 1: C -> Tin --\n");
    print_st_direct();

    fprintf(stderr, "C: -- scenario 2: Tin -> C -> Tin --\n");
    run_nested();

    fprintf(stderr, "C: main done\n");
    return 0;
}
