// examples/c_interop_matrix/tin_to_c/matrix.c
//
// C side of the Tin -> C interop matrix.  Each function pairs with a
// test in matrix.tin; the test ID is in the function comment so a
// failing test maps back to the doc row.

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// ===== T1-T4 =====
int32_t c_add_i32(int32_t a, int32_t b) { return a + b; }
double  c_sub_f64(double a, double b)   { return a - b; }
_Bool   c_negate_bool(_Bool b)          { return !b; }
void    c_write_42(int64_t *out)        { *out = 42; }

// ===== T5-T8 =====
int64_t c_strlen_wrap(const char *s)    { return (int64_t)strlen(s); }
const char *c_static_str(void)          { return "static"; }

// T7: #handover -- Tin adopts the malloc'd buffer.
char *c_malloc_str(void) {
    const char *msg = "from-malloc";
    size_t len = strlen(msg);
    char *buf = (char *)malloc(len + 1);
    memcpy(buf, msg, len);
    buf[len] = 0;
    return buf;
}

// T8: concat two strings into a fresh malloc; the call site copies
// out (non-handover default), so we leak the malloc'd buffer in this
// fixture for test simplicity.  A production caller would either
// tag #handover or use a static buffer.
const char *c_concat_pair(const char *a, const char *b) {
    static char buf[64];
    snprintf(buf, sizeof(buf), "%s%s", a, b);
    return buf;
}

// ===== T9-T10 =====
int64_t c_atom_len(const char *a)       { return (int64_t)strlen(a); }
const char *c_make_atom_name(void)      { return "from_c"; }

// ===== T11-T13 =====
int64_t c_sum_i32_arr(const int32_t *xs, int64_t n) {
    int64_t total = 0;
    for (int64_t i = 0; i < n; i++) total += xs[i];
    return total;
}

void c_double_f64_arr(double *xs, int64_t n) {
    for (int64_t i = 0; i < n; i++) xs[i] *= 2.0;
}

int64_t c_byte_first(const uint8_t *buf) { return (int64_t)buf[0]; }

// ===== T14-T15 =====
int64_t c_strarr_total(const char **xs, int64_t n) {
    int64_t total = 0;
    for (int64_t i = 0; i < n; i++) total += (int64_t)strlen(xs[i]);
    return total;
}

int64_t c_atomarr_total(const char **xs, int64_t n) {
    int64_t total = 0;
    for (int64_t i = 0; i < n; i++) total += (int64_t)strlen(xs[i]);
    return total;
}

// ===== T16-T17 =====
typedef struct { int32_t x; int32_t y; } pt_small;
int32_t c_pt_small_sum(pt_small p) { return p.x + p.y; }

typedef struct { int64_t a; int64_t b; int64_t c; } pt_big;
int64_t c_pt_big_sum(pt_big p)  { return p.a + p.b + p.c; }
pt_big c_pt_big_make(int64_t a, int64_t b, int64_t c) {
    pt_big r = { a, b, c };
    return r;
}

// ===== T18-T19 =====
typedef struct { float x; float y; float z; } vec3;
static vec3 g_vec = { 0.0f, 0.0f, 10.0f };
vec3 *c_vec3_get(void)              { return &g_vec; }
void  c_vec3_bump_z(vec3 *v, float dz) { v->z += dz; }

// ===== T20 =====
typedef struct { int64_t a; int64_t b; } inner_t;
typedef struct { inner_t lo; inner_t hi; } outer_t;
int64_t c_outer_sum(outer_t o) {
    return o.lo.a + o.lo.b + o.hi.a + o.hi.b;
}

// ===== T21 =====
typedef struct __attribute__((packed)) { int32_t x; int32_t y; } packed_pt;
int32_t c_packed_sum(packed_pt p) { return p.x + p.y; }

// ===== T23-T25 =====
int64_t c_invoke_cb_i64(int64_t (*cb)(int64_t), int64_t x) { return cb(x); }
int64_t c_invoke_cb_str(int64_t (*cb)(const char *), const char *s) { return cb(s); }

// ===== T30 =====
typedef struct { int64_t value; } handle_t;

void *c_handle_make(void) {
    handle_t *h = (handle_t *)malloc(sizeof(handle_t));
    h->value = 1234;
    return h;
}

int64_t c_handle_use(void *p) {
    handle_t *h = (handle_t *)p;
    return h->value;
}

void c_handle_free(void *p) { free(p); }

// ===== T32-T33 =====
typedef struct { int64_t a; int64_t b; } dyad_t;
int64_t c_dyad_read(const dyad_t *p) { return p->a + p->b; }
void    c_dyad_mutate(dyad_t *p)     { p->a = 11; p->b = 22; }

// ===== T34-T37: Mixed-parameter shapes =====

int64_t c_mix_sps(const dyad_t *p, int64_t n, const char *s) {
    return (p->a + p->b) * n + (int64_t)strlen(s);
}

int64_t c_mix_full(
    int64_t (*cb)(int64_t),
    const char *s,
    const dyad_t *p,
    const int64_t *xs, int64_t xs_len,
    int64_t k
) {
    int64_t sum = 0;
    for (int64_t i = 0; i < xs_len; i++) sum += xs[i];
    return cb(p->a + p->b) + (int64_t)strlen(s) + sum + k;
}

dyad_t c_mix_make(const char *s, int64_t n) {
    dyad_t r = { (int64_t)strlen(s), n };
    return r;
}

int64_t c_mix_arr_struct(const int64_t *xs, int64_t xs_len, pt_big p) {
    int64_t sum = 0;
    for (int64_t i = 0; i < xs_len; i++) sum += xs[i];
    return sum + p.a + p.b + p.c;
}
