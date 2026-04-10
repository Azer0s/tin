// c_interop_exotic.c - C helpers for exotic interop: callbacks with pointer
// args, callback pointers, stashed callbacks, chained callbacks, and structs
// with multi-level pointer fields.
#include <stdint.h>
#include <stddef.h>

// -- Struct types --

typedef struct { int32_t x; int32_t y; } exvec;

// Structs with scalar/integer pointer fields (no struct-pointer fields to avoid
// the cLayout wrapper mismatch when copying C return values into Tin structs).
typedef struct { int64_t *a; int64_t *b; }    i64_ptr_pair;
typedef struct { i64_ptr_pair ptrs; int32_t tag; } tagged_i64_pair;
typedef struct { int64_t **pp; int32_t n; }   i64_dbl;
typedef struct { int64_t ***ppp; int32_t n; } i64_tri;
typedef struct { i64_dbl inner; int64_t id; } nested_i64_dbl;

// -- Callback struct types (Tin fat-fn ABI: { fn*, env* }) --

typedef struct { void    (*fn)(void *env, exvec *p);              void *env; } cb_evptr;
typedef struct { void    (*fn)(void *env, exvec *a, exvec *b);    void *env; } cb_evpair;
typedef struct { int64_t (*fn)(void *env, exvec *p, int32_t n);   void *env; } cb_evptr_int;
typedef struct { int64_t (*fn)(void *env, int64_t);               void *env; } cb_i64;

// -- Static data for exvec (pointer tests) --

static exvec   g_evs[4]  = {{1,2},{3,4},{5,6},{7,8}};
static int     g_ev_inited = 0;

static void ensure_ev_init(void) {
    (void)g_ev_inited;  // suppress unused warning; g_evs is static-initialised
}

exvec *c_ev_at(int32_t idx) { return &g_evs[idx]; }

// Read helpers so Tin callbacks can inspect fields without direct dereference.
int32_t c_ev_x(exvec *p) { return p->x; }
int32_t c_ev_y(exvec *p) { return p->y; }

// -- Static data for i64 pointer tests --

static int64_t   g_ints[4]   = {10, 20, 30, 40};
static int64_t  *g_ipts[4];
static int64_t **g_iptrptr;
static int64_t ***g_ippp;
static int       g_int_inited = 0;

static void ensure_int_init(void) {
    if (g_int_inited) return;
    for (int i = 0; i < 4; i++) g_ipts[i] = &g_ints[i];
    g_iptrptr = &g_ipts[2];   // *g_iptrptr = g_ipts[2] = &g_ints[2] = {30}
    g_ippp    = &g_iptrptr;
    g_int_inited = 1;
}

// -- Section 1: callbacks that receive struct pointers from C --

void c_foreach_ev_ptr(cb_evptr cb) {
    for (int i = 0; i < 4; i++) cb.fn(cb.env, &g_evs[i]);
}

void c_foreach_ev_pair(cb_evpair cb) {
    // calls cb with consecutive pairs: (evs[0],evs[1]), (evs[1],evs[2]), (evs[2],evs[3])
    for (int i = 0; i < 3; i++) cb.fn(cb.env, &g_evs[i], &g_evs[i+1]);
}

int64_t c_apply_ev_int(cb_evptr_int cb, exvec *p, int32_t n) {
    return cb.fn(cb.env, p, n);
}

// -- Section 2: callback pointer (C takes cb_i64*) --

int64_t c_apply_cb_ptr(cb_i64 *cbp, int64_t n) {
    return cbp->fn(cbp->env, n);
}

// -- Section 3: C stashes callback, calls it later --

static cb_i64 g_stashed;

void    c_stash_cb(cb_i64 cb)     { g_stashed = cb; }
int64_t c_call_stashed(int64_t n) { return g_stashed.fn(g_stashed.env, n); }

// -- Section 4: chained callbacks --

int64_t c_chain_cbs(cb_i64 f, cb_i64 g, int64_t n) {
    return g.fn(g.env, f.fn(f.env, n));
}

// -- Section 5: structs with i64 pointer fields --

i64_ptr_pair c_get_i64_ptr_pair(int32_t i, int32_t j) {
    ensure_int_init();
    i64_ptr_pair p;
    p.a = &g_ints[i];
    p.b = &g_ints[j];
    return p;
}

tagged_i64_pair c_get_tagged_i64_pair(int32_t i, int32_t j, int32_t tag) {
    ensure_int_init();
    tagged_i64_pair t;
    t.ptrs.a = &g_ints[i];
    t.ptrs.b = &g_ints[j];
    t.tag    = tag;
    return t;
}

// -- Section 6: double and triple pointer fields in structs --

i64_dbl c_get_i64_dbl(int32_t idx) {
    ensure_int_init();
    // *d.pp = g_ipts[idx] = &g_ints[idx]  =>  **d.pp = g_ints[idx]
    i64_dbl d;
    d.pp = &g_ipts[idx];
    d.n  = idx;
    return d;
}

i64_tri c_get_i64_tri(void) {
    ensure_int_init();
    // ***t.ppp = **g_iptrptr = *g_ipts[2] = g_ints[2] = 30
    i64_tri t;
    t.ppp = g_ippp;
    t.n   = 3;
    return t;
}

nested_i64_dbl c_get_nested_i64_dbl(int32_t idx, int64_t id) {
    ensure_int_init();
    nested_i64_dbl d;
    d.inner.pp = &g_ipts[idx];   // **d.inner.pp = g_ints[idx]
    d.inner.n  = idx;
    d.id       = id;
    return d;
}

// -- Section 7: struct with inline fn field --

typedef struct { cb_i64 f; int64_t base; } fn_val_s;

int64_t c_apply_fn_val(fn_val_s s, int64_t n) {
    return s.f.fn(s.f.env, s.base + n);
}

// -- Section 8: struct with *fn pointer field --

typedef struct { cb_i64 *f; int64_t base; } fn_ptr_s;

int64_t c_apply_fn_ptr(fn_ptr_s s, int64_t n) {
    return s.f->fn(s.f->env, s.base + n);
}
