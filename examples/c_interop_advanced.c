// c_interop_advanced.c - C helpers for testing advanced struct/callback interop.
#include <stdint.h>
#include <stddef.h>

// -- 1. Deep nesting: 3-level by-value structs --------------------------
// Uses distinct types from the pointer tests to avoid cLayout conflicts.

typedef struct { int32_t x; int32_t y; } bvec2;
typedef struct { bvec2 min; bvec2 max; } bounds;
typedef struct { bounds clip; int64_t layer; int64_t flags; } viewport;

int64_t c_viewport_area(viewport v) {
    int64_t w = (int64_t)(v.clip.max.x - v.clip.min.x);
    int64_t h = (int64_t)(v.clip.max.y - v.clip.min.y);
    return w * h;
}

int32_t c_bounds_width(bounds b) {
    return b.max.x - b.min.x;
}

viewport c_make_viewport(int32_t x1, int32_t y1, int32_t x2, int32_t y2,
                          int64_t layer, int64_t flags) {
    viewport v;
    v.clip.min.x = x1; v.clip.min.y = y1;
    v.clip.max.x = x2; v.clip.max.y = y2;
    v.layer = layer;
    v.flags = flags;
    return v;
}

bvec2 c_viewport_min(viewport v) { return v.clip.min; }

// -- 2. Pointer structs -------------------------------------------------

typedef struct { int32_t x; int32_t y; } pvec2;

static pvec2 g_vec = {42, 99};

void c_get_vec_ptr(pvec2 **out) {
    *out = &g_vec;
}

int32_t c_read_vec_x(pvec2 *p) { return p->x; }
int32_t c_read_vec_y(pvec2 *p) { return p->y; }

// -- 3. Pointer-to-struct with C mutation --------------------------------

typedef struct { int64_t a; int64_t b; int64_t c; } triple;

static triple g_triple = {10, 20, 30};

triple *c_get_triple(void) { return &g_triple; }

void c_set_triple(triple *t, int64_t a, int64_t b, int64_t c) {
    t->a = a; t->b = b; t->c = c;
}

// -- 4. Tree structure via pointers --------------------------------------

typedef struct tree_node {
    int64_t value;
    struct tree_node *left;
    struct tree_node *right;
} tree_node;

static tree_node g_nodes[7];

tree_node *c_build_tree(void) {
    g_nodes[0] = (tree_node){2,  NULL, NULL};
    g_nodes[1] = (tree_node){7,  NULL, NULL};
    g_nodes[2] = (tree_node){12, NULL, NULL};
    g_nodes[3] = (tree_node){20, NULL, NULL};
    g_nodes[4] = (tree_node){5,  &g_nodes[0], &g_nodes[1]};
    g_nodes[5] = (tree_node){15, &g_nodes[2], &g_nodes[3]};
    g_nodes[6] = (tree_node){10, &g_nodes[4], &g_nodes[5]};
    return &g_nodes[6];
}

int64_t c_tree_sum(tree_node *n) {
    if (!n) return 0;
    return n->value + c_tree_sum(n->left) + c_tree_sum(n->right);
}

int64_t c_tree_depth(tree_node *n) {
    if (!n) return 0;
    int64_t ld = c_tree_depth(n->left);
    int64_t rd = c_tree_depth(n->right);
    return 1 + (ld > rd ? ld : rd);
}

int64_t c_tree_value(tree_node *n) {
    return n ? n->value : -1;
}

tree_node *c_tree_left(tree_node *n) { return n ? n->left : NULL; }
tree_node *c_tree_right(tree_node *n) { return n ? n->right : NULL; }

// -- 5. Callbacks (Tin fn passed to C) ------------------------------------
// Tin lowers `fn(...) T` extern params to raw C function pointers
// (codegen/extern.go).  Closure environments, when any, are baked
// into a runtime-synthesized trampoline by `tin_make_trampoline`;
// the C side just calls the function pointer through its declared
// signature -- it never sees an env arg.

typedef int64_t (*callback_i64)(int64_t);
typedef int64_t (*callback_i64_2)(int64_t, int64_t);
typedef void    (*callback_void)(int64_t);

int64_t c_apply_cb(callback_i64 cb, int64_t n) {
    return cb(n);
}

int64_t c_apply2_cb(callback_i64_2 cb, int64_t a, int64_t b) {
    return cb(a, b);
}

void c_for_each(callback_void cb, int64_t n) {
    for (int64_t i = 0; i < n; i++) {
        cb(i);
    }
}

// -- 6. 4-level nested struct -------------------------------------------

typedef struct { int32_t v; } leaf;
typedef struct { leaf a; leaf b; } pair;
typedef struct { pair p; int32_t tag; } tagged_pair;
typedef struct { tagged_pair inner; int64_t id; } deep4;

int64_t c_deep4_sum(deep4 d) {
    return (int64_t)d.inner.p.a.v + (int64_t)d.inner.p.b.v
         + (int64_t)d.inner.tag + d.id;
}

deep4 c_make_deep4(int32_t a, int32_t b, int32_t tag, int64_t id) {
    deep4 d;
    d.inner.p.a.v = a;
    d.inner.p.b.v = b;
    d.inner.tag = tag;
    d.id = id;
    return d;
}

// -- 7. Array of struct pointers ----------------------------------------

static pvec2 g_points[4] = {{1,2},{3,4},{5,6},{7,8}};

pvec2 *c_get_point_at(int32_t idx) {
    return &g_points[idx];
}

// -- 8. Double pointer: c_get_vec_ptr already in section 2. ----------
// (static pvec2 g_vec = {42, 99} and c_get_vec_ptr defined there.)

// -- 9. Chained traversal helpers (use c_build_tree + c_tree_left/right) -------
// Already defined in section 4. Needed for multi-hop tests.

// -- 10. Triple pointer out-param -----------------------------------------

// Static pvec2 used for triple-pointer test.
static pvec2 g_triple_point = {77, 88};
static pvec2 *g_triple_ptr = NULL;

// c_get_pvec2_triple_ptr(out ***pvec2): C writes **pvec2.native into *out.
void c_get_pvec2_triple_ptr(pvec2 ***out) {
    g_triple_ptr = &g_triple_point;
    *out = &g_triple_ptr;
}

// -- 11. Callbacks with varying argument patterns -----------------------

// Callback that receives two i32s and returns i32.
typedef int32_t (*callback_i32_2)(int32_t, int32_t);

int32_t c_apply_i32_2(callback_i32_2 cb, int32_t a, int32_t b) {
    return cb(a, b);
}

// Sum x-fields of all g_points via callback, passing i32 each time.
typedef void (*callback_xy)(int32_t x, int32_t y);

void c_for_each_point_xy(callback_xy cb) {
    for (int32_t i = 0; i < 4; i++) {
        cb(g_points[i].x, g_points[i].y);
    }
}
