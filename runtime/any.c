// tin runtime - any-type equality
//
// Dispatches on the type tag: 0=i64, 1=f64, 2=string/atom, 3=bool, else ptr equality.

#include "runtime.h"
#include <string.h>

int64_t _tin_any_eq(_TinAny a, _TinAny b) {
    if (a.tag != b.tag) return 0;
    switch (a.tag) {
    case 0: return *(int64_t *)a.ptr == *(int64_t *)b.ptr ? 1 : 0;   // i64
    case 1: return *(double  *)a.ptr == *(double  *)b.ptr ? 1 : 0;   // f64
    case 2: {  // string / atom: ptr -> TinString = {char*, i64 len, i64 cap}.
        // Compare length first, then memcmp -- Tin strings aren't NUL
        // terminated, so strcmp would read past the allocated buffer
        // and trip valgrind on "Invalid read 0 bytes after block".
        const TinString *sa = (const TinString *)a.ptr;
        const TinString *sb = (const TinString *)b.ptr;
        if (sa->len != sb->len) return 0;
        return memcmp(sa->ptr, sb->ptr, (size_t)sa->len) == 0 ? 1 : 0;
    }
    case 3: return *(uint8_t *)a.ptr == *(uint8_t *)b.ptr ? 1 : 0;   // bool
    default: return a.ptr == b.ptr ? 1 : 0;                           // ptr/struct/fn
    }
}
