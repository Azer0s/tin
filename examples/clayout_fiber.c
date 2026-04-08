// clayout_fiber.c - C helpers for testing cLayoutStruct interop with fibers.
//
// Provides two families of structs:
//   packet_t  - backed by a static global (non-handover: C owns memory)
//   counter_t - malloc'd on request (#handover: Tin takes ownership)
//   circle_t  - malloc'd nested struct (#handover, nested cLayoutStruct)

#include <stdint.h>
#include <stdlib.h>

// ---------------------------------------------------------------------------
// packet_t: static global, non-handover (C owns memory, Tin borrows pointer)
// ---------------------------------------------------------------------------

typedef struct { int64_t seq; int64_t payload; } packet_t;

static packet_t g_packet = {1, 100};

packet_t *get_packet(void) { return &g_packet; }

void set_packet(packet_t *p, int64_t seq, int64_t payload) {
    p->seq     = seq;
    p->payload = payload;
}

// ---------------------------------------------------------------------------
// counter_t: heap-allocated, #handover (Tin takes ownership via ARC)
// ---------------------------------------------------------------------------

typedef struct { int64_t value; int64_t id; } counter_t;

counter_t *make_counter(int64_t value, int64_t id) {
    counter_t *c = malloc(sizeof(counter_t));
    c->value = value;
    c->id    = id;
    return c;
}

// ---------------------------------------------------------------------------
// circle_t: heap-allocated nested struct, #handover
// ---------------------------------------------------------------------------

typedef struct { int64_t x; int64_t y; } point_t;
typedef struct { point_t origin; int64_t radius; } circle_t;

circle_t *make_circle(int64_t x, int64_t y, int64_t radius) {
    circle_t *c = malloc(sizeof(circle_t));
    c->origin.x = x;
    c->origin.y = y;
    c->radius   = radius;
    return c;
}
