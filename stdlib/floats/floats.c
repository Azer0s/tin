// floats.c - IEEE 754 special-value helpers for stdlib/floats.
#include <math.h>

double _tin_float_nan(void)       { return NAN; }
double _tin_float_inf(void)       { return INFINITY; }
double _tin_float_neg_inf(void)   { return -INFINITY; }
// is_nan / is_inf via libc so we get the correct unordered comparison semantics.
int    _tin_float_is_nan(double x) { return isnan(x) ? 1 : 0; }
int    _tin_float_is_inf(double x) { return isinf(x) ? 1 : 0; }
