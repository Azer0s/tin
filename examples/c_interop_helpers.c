// c_interop_helpers.c - C helper functions for testing C struct interop from Tin.
#include <stdint.h>

typedef struct { double x; double y; }          point2d;
typedef struct { point2d origin; double width; double height; } rect;

double   c_add_xy(point2d p)              { return p.x + p.y; }
point2d  c_scale(point2d p, double f)     { return (point2d){p.x * f, p.y * f}; }
double   c_rect_area(rect r)              { return r.width * r.height; }
point2d  c_make_point(double x, double y) { return (point2d){x, y}; }
point2d  c_origin(rect r)                 { return r.origin; }
