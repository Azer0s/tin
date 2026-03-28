// tin runtime - umbrella
//
// This file pulls in all runtime sub-modules in dependency order.
// The sub-files are designed to be readable standalone (each includes runtime.h
// and its own system headers), but are compiled as a single translation unit
// via this umbrella so the build system only needs to reference one file.

#include "runtime.h"
#include "arc.c"
#include "strings.c"   // uses _tin_rc_alloc (arc)
#include "slice.c"     // uses _tin_rc_alloc (arc)
#include "echo.c"
#include "mem.c"
#include "defer.c"     // uses _tin_str_from_cstr (strings)
#include "len.c"
#include "test.c"
#include "atom.c"
#include "any.c"
