// clayout_stress.c - C helpers for cLayoutStructs stress tests.
// Covers: mutation visibility, pointer identity, nested struct reads,
// struct literal round-trips, multiple Tin aliases to one C struct,
// and union members inside cLayoutStructs.

#include <stdint.h>
#include <stdbool.h>

// ---------------------------------------------------------------------------
// point2: basic 2-field mutable struct
// ---------------------------------------------------------------------------

typedef struct { int32_t x; int32_t y; } point2;

static point2 g_point = {10, 20};

point2 *get_point(void)                    { return &g_point; }
void    set_point_x(point2 *p, int32_t v)  { p->x = v; }
void    set_point_y(point2 *p, int32_t v)  { p->y = v; }
bool    same_point_ptr(point2 *a, point2 *b) { return a == b; }

// ---------------------------------------------------------------------------
// vec2 / rect: nested struct (non-handover)
// ---------------------------------------------------------------------------

typedef struct { int64_t x; int64_t y; } vec2;
typedef struct { vec2 origin; vec2 size; int64_t id; } rect;

static rect g_rect = {{0, 0}, {100, 200}, 42};

rect *get_rect(void)                               { return &g_rect; }
void  set_rect_origin(rect *r, int64_t x, int64_t y) { r->origin.x = x; r->origin.y = y; }
void  set_rect_size(rect *r, int64_t w, int64_t h)   { r->size.x = w; r->size.y = h; }
void  set_rect_id(rect *r, int64_t id)               { r->id = id; }

// ---------------------------------------------------------------------------
// color: u8 x4, used for struct literal round-trips
// ---------------------------------------------------------------------------

typedef struct { uint8_t r; uint8_t g; uint8_t b; uint8_t a; } color;

static color g_color = {10, 20, 30, 40};

color *get_color(void)                                              { return &g_color; }
void   set_color(color *c, uint8_t r, uint8_t g, uint8_t b, uint8_t a) {
    c->r = r; c->g = g; c->b = b; c->a = a;
}
bool   verify_color(color *c, uint8_t r, uint8_t g, uint8_t b, uint8_t a) {
    return c->r == r && c->g == g && c->b == b && c->a == a;
}

// ---------------------------------------------------------------------------
// tagged_val: struct with a union member - tests union fields in cLayoutStructs
// ---------------------------------------------------------------------------

typedef union { int32_t as_i; float as_f; } num_union;
typedef struct { num_union val; int32_t tag; } tagged_val;

static tagged_val g_tagged = {{.as_i = 42}, 7};

tagged_val *get_tagged(void)                       { return &g_tagged; }
void        set_tagged_i(tagged_val *t, int32_t v) { t->val.as_i = v; }
