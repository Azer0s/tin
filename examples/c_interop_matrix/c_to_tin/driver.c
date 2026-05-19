// examples/c_interop_matrix/c_to_tin/driver.c
//
// C harness for the C -> Tin matrix.  Each call prints one tagged
// line; the runner test greps for the prefix and asserts the value
// matches.  Failures localize to a single row (C-A through C-I).
//
// Build via the runner test in examples/c_interop_matrix_c_to_tin_test.tin.

#include "lib.h"
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

static int32_t my_callback(int32_t x) { return x + 1; }

int main(void) {
    /* C-A: scalar add */
    printf("CA=%d\n", c_a_add(40, 2));

    /* C-B: string in / string out */
    const char *g = c_b_greet("world");
    printf("CB=%s\n", g);
    /* greet's return was alloc'd via tin_extern_alloc; the default
       allocator is libc malloc, so free is the right release here. */
    free((void *)g);

    /* C-C: array in */
    int32_t xs[] = { 1, 2, 3, 4, 5 };
    printf("CC=%d\n", c_c_sum(xs, 5));

    /* C-D: callback in */
    printf("CD=%d\n", c_d_apply(my_callback, 20));

    /* C-E: packed struct by value (IN) */
    c_e_pt p = { 17, 25 };
    printf("CE=%d\n", c_e_sum(p));

    /* C-F: packed struct by value (OUT) */
    c_e_pt q = c_f_make(3, 4);
    printf("CF=%d,%d\n", q.x, q.y);

    /* C-G / C-H / C-I: struct pointer round trip.
       Tin allocates via c_i_make; C reads via c_g_sum; C mutates
       via c_h_fill; C reads again to confirm; C releases. */
    void *h = c_i_make(10, 20);
    printf("CI_sum=%lld\n", (long long)c_g_sum(h));
    c_h_fill(h, 5);
    printf("CH_sum=%lld\n", (long long)c_g_sum(h));
    tin_release(h);

    /* C-J: mixed (string, scalar, *struct, [i32]) -> i64
       expected: len("hi")=2 + n=7 + a=10 + b=20 + sum([1,2,3,4])=10 = 49 */
    void *h2 = c_i_make(10, 20);
    int32_t arr[] = { 1, 2, 3, 4 };
    printf("CJ=%lld\n", (long long)c_j_combine("hi", 7, h2, arr, 4));
    tin_release(h2);

    /* C-K: mixed (cb, packed_struct, string) -> i32
       expected: cb(x+y)=cb(3+4)=8 + len("ab")=2 = 10 */
    c_e_pt pk = { 3, 4 };
    printf("CK=%d\n", c_k_invoke(my_callback, pk, "ab"));

    /* C-L: mixed inputs returning *struct
       expected dyad: a = len("hey")=3, b = cb(9)=10 */
    void *h3 = c_l_build("hey", 9, my_callback);
    printf("CL_sum=%lld\n", (long long)c_g_sum(h3));
    tin_release(h3);

    printf("done\n");
    return 0;
}
