// handover_ptrs_extra.c - extended helpers for #handover edge-case tests.

#include <stdlib.h>
#include <stdint.h>
#include <string.h>

// --- NULL pointer ---
// Exercises the null-guard path: _tin_ptr_handover(NULL, ...) must return NULL
// and the bound variable must survive scope exit without crashing.
int64_t *get_null_ptr(void) { return NULL; }

// --- Multi-element array ---
// C allocates a contiguous block of N int64_t values.  malloc_usable_size
// reports the full block, so _tin_ptr_handover copies all N elements and frees
// the original.  Tin sees *i64; elements are accessed via pointer arithmetic.
int64_t *get_i64_array(int64_t n) {
    int64_t *p = malloc((size_t)n * sizeof(int64_t));
    for (int64_t i = 0; i < n; i++) p[i] = (i + 1) * 10; // 10, 20, 30, ...
    return p;
}

// --- Nested struct ---
// Returns a pointer to a C struct containing a nested sub-struct.
typedef struct { int64_t x; int64_t y; } inner_t;
typedef struct { inner_t a; inner_t b; int64_t tag; } outer_t;

outer_t *get_outer_ptr(void) {
    outer_t *p = malloc(sizeof(outer_t));
    p->a.x = 1; p->a.y = 2;
    p->b.x = 3; p->b.y = 4;
    p->tag = 42;
    return p;
}
