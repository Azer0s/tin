# BLAS bindings - speed vs C

Three implementations of the same `C := A*B` loop, compared via
hyperfine on macOS / Apple Accelerate:

| binary           | how the data is held                                        |
|------------------|-------------------------------------------------------------|
| `c_dgemm`        | C: `double *` from `malloc` + `cblas_dgemm`                 |
| `tin_raw_dgemm`  | Tin: `*f64` from `mem::malloc` + `dgemm_raw`                |
| `tin_high_dgemm` | Tin: `[[f64]]` + `blas::dgemm` (flatten/unflatten per call) |

Each program takes `N` and `ITERS`, builds two `N*N` matrices, calls
`dgemm` in a loop, and prints a checksum (so the optimizer can't drop
the work and we can verify identical numeric output).

## Results (M1 Pro, macOS)

`hyperfine -N --warmup 2 'bench/bin/c_dgemm N ITERS' 'bench/bin/tin_raw_dgemm N ITERS'`:

| size             | C             | Tin raw       | ratio |
|------------------|---------------|---------------|-------|
| N=256, 200 iters | 23.4 $\pm$ 0.6 ms | 27.7 $\pm$ 2.7 ms | 1.19$\times$ |
| N=512, 50 iters  | 46.3 $\pm$ 1.3 ms | 50.3 $\pm$ 1.9 ms | 1.09$\times$ |

**Tin's raw bindings are within 10-20% of C** - and most of the
remaining gap is process startup, not per-call FFI cost. At N=512
the actual BLAS work dominates and the two are essentially
indistinguishable.

## High-level wrapper

`blas::dgemm` takes `[[f64]]` (row-of-rows) and flattens A, B, and
zero-allocates a flat C buffer on every call. At small sizes the
wrapper cost dominates BLAS work; at larger sizes it grows
superlinearly because Tin's `xs ++= [v]` is currently O(n) per
append (so building the c_flat zero buffer is $O(n^4)$ for an $N \times N$
matrix). Internal timings:

| size              | gemm loop (Tin high) | per call |
|-------------------|----------------------|----------|
| N=32, 1000 iters  | 6.0 s                | 6 ms     |
| N=64, 100 iters   | 0.34 s               | 3.4 ms   |
| N=128, 100 iters  | 23.6 s               | 236 ms   |
| N=256, 50 iters   | 69.8 s               | 1.4 s    |

This is **not** a fundamental BLAS-binding limit - it's the cost of
Tin's current `++=` implementation hitting the wrapper's c_flat
zero-init loop. If `++=` ever lands an amortized growth strategy,
the wrapper drops to the same ~10-20% overhead as the raw path
plus one $O(n^2)$ flatten/unflatten copy.

## Reproducing

```bash
# Build (M1 with Accelerate):
clang -O2 -framework Accelerate bench/blas/c_dgemm.c -o bench/bin/c_dgemm
./tin build bench/blas/tin_raw_dgemm.tin  -o bench/bin/tin_raw_dgemm
./tin build bench/blas/tin_high_dgemm.tin -o bench/bin/tin_high_dgemm

# Run:
hyperfine -N --warmup 2 \
  'bench/bin/c_dgemm 512 50' \
  'bench/bin/tin_raw_dgemm 512 50'
```

Each binary also prints `init_us=... gemm_us=... sum_us=...` so you can
read the per-phase breakdown without re-running under hyperfine.
