#include <stdint.h>
#include <stdbool.h>

typedef struct {
    int8_t  a;
    int16_t b;
    int32_t c;
    int64_t d;
} small_struct;

static small_struct s = {1, 2, 3, 4};

small_struct* get_small_struct() {
    return &s;
}

bool compare_small_struct_ptr(small_struct* a) {
    return (a == &s);
}