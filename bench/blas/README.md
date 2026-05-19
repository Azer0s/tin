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

`hyperfine -N --warmup 2 'bench/bin/c_dgemm N ITERS' 'bench/bin/tin_raw_dgemm N ITERS' 'bench/bin/tin_high_dgemm N ITERS'`:

| size            | C              | Tin raw          | Tin high          | raw vs C | high vs C |
|-----------------|----------------|------------------|-------------------|----------|-----------|
| N=256, 50 iters | 8.4 $\pm$ 0.8 ms  | 16.0 $\pm$ 2.5 ms   | 27.0 $\pm$ 2.3 ms   | 1.9$\times$    | 3.1$\times$     |
| N=512, 50 iters | 47.6 $\pm$ 2.3 ms | 52.3 $\pm$ 2.2 ms   | 131 $\pm$ 5 ms     | 1.1$\times$    | 2.7$\times$     |

**Tin's raw bindings are within 10-20% of C at the work-dominated
sizes**, matching the FFI baseline.  The high-level wrapper carries
extra O($n^2$) flatten/unflatten cost per call; with the amortized
`++=` and row-at-a-time concat (May 2026) it sits at 2.7-3$\times$ C,
down from 18-24$\times$ before those landed.

## High-level wrapper

`blas::dgemm` takes `[[f64]]` (row-of-rows) and flattens A, B, and
allocates a flat C buffer on every call.  Two iterative
optimizations dropped wrapper cost dramatically:

1. **Amortized `++=`**: each append is O(1) amortized, so the
   flatten loops dropped from $O(n^4)$ (per-element appends $\times$ per-call
   reallocation) to $O(n^2)$.
2. **Row-wise concat + skip c_flat zero-init**: `flatten_f64`
   now does `out ++= m[i]` once per row (one bulk memcpy) instead
   of `out ++= [m[i][j]]` per element, and c_flat is initialised
   via `flatten_f64(c)` instead of an N$^2$ zero-fill loop (dgemm
   has `beta=0` so the contents are discarded anyway).

Internal timings after both landed:

| size              | gemm loop (Tin high) | per call |
|-------------------|----------------------|----------|
| N=64, 1000 iters  | 17.3 ms              | 17 us    |
| N=128, 100 iters  | 6.1 ms               | 61 us    |
| N=256, 50 iters   | 15.7 ms              | 315 us   |
| N=512, 50 iters   | 99 ms                | 2.0 ms   |

Compared to the pre-amortization numbers (N=128 / 100 iters was
23.6 s, N=256 / 50 iters was 69.8 s) the wrapper is now hundreds
to thousands of times faster at these sizes.  Remaining per-call
cost is dominated by the two row-wise memcpy fans (A, B) plus the
unflatten store back into the caller's `[[f64]]`.

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
