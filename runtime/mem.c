// tin runtime - memory helpers

#include "runtime.h"
#include <stdlib.h>
#include <stdio.h>

void *_tin_malloc(int64_t size) {
    void *p = malloc((size_t)size);
    if (!p) { fputs("tin: out of memory\n", stderr); exit(1); }
    return p;
}

void _tin_free(void *p) { free(p); }
