// handover_ptrs_helper.c - C helpers for the #handover pointer test.
// These functions malloc data and return pointers; the Tin test adopts
// them via fn{#handover} extern declarations.

#include <stdlib.h>
#include <stdint.h>

// --- Primitive pointer ---
int64_t *make_i64_ptr(void) {
    int64_t *p = malloc(sizeof(int64_t));
    *p = 42;
    return p;
}

// --- Void pointer (byte buffer) ---
// Returns a malloc'd single byte set to 99.
void *make_void_ptr(void) {
    char *p = malloc(1);
    *p = 99;
    return p;
}

// --- Struct pointer ---
// Layout matches Tin's vec2.native = { i64 x, i64 y } (no type_id).
typedef struct { int64_t x; int64_t y; } c_vec2;

c_vec2 *make_vec2_ptr(void) {
    c_vec2 *v = malloc(sizeof(c_vec2));
    v->x = 10;
    v->y = 20;
    return v;
}
