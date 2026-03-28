// tin runtime - len() builtin helpers

#include "runtime.h"

int64_t _tin_len_string(TinString s) { return s.len; }
int64_t _tin_len_slice(TinSlice s)   { return s.len; }
