// bench/blas/c_dgemm.c - reference CBLAS dgemm benchmark.
//
// Compile (macOS): clang -O2 -framework Accelerate c_dgemm.c -o c_dgemm
// Usage:           ./c_dgemm N ITERS

#include <Accelerate/Accelerate.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

static long now_us(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (long)ts.tv_sec * 1000000L + ts.tv_nsec / 1000L;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: c_dgemm N ITERS\n");
        return 1;
    }

    int n = atoi(argv[1]);
    long iters = atol(argv[2]);

    long t0 = now_us();

    double *a = malloc((size_t)n * n * sizeof(double));
    double *b = malloc((size_t)n * n * sizeof(double));
    double *c = calloc((size_t)n * n, sizeof(double));

    for (long i = 0; i < (long)n * n; i++) {
        a[i] = (double)(i % 7 + 1);
        b[i] = (double)(i % 5 + 1);
    }

    long t1 = now_us();

    for (long it = 0; it < iters; it++) {
        cblas_dgemm(CblasRowMajor, CblasNoTrans, CblasNoTrans,
                    n, n, n,
                    1.0,
                    a, n,
                    b, n,
                    0.0,
                    c, n);
    }

    long t2 = now_us();

    // Checksum: prints something the optimizer can't drop and lets us
    // verify identical numeric output across the three implementations.
    double sum = 0.0;
    for (long i = 0; i < (long)n * n; i++) sum += c[i];

    long t3 = now_us();
    printf("sum: %.0f\n", sum);
    printf("init_us=%ld gemm_us=%ld sum_us=%ld\n",
           t1 - t0, t2 - t1, t3 - t2);

    free(a);
    free(b);
    free(c);
    return 0;
}
