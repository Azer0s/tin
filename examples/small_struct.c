#include <stdio.h>

typedef struct {
    unsigned char a;
    unsigned char b;
    unsigned char c;
    unsigned char d;
} small_struct;

void print_small_struct(small_struct s) {
    printf("small_struct: a=%u, b=%u, c=%u, d=%u\n", s.a, s.b, s.c, s.d);
}

typedef struct {
    double a;
    double b;
} point;

void print_point(point p) {
    printf("point: a=%f, b=%f\n", p.a, p.b);
}

typedef struct {
    unsigned char a;
    double b;
} mixed_struct;

void print_mixed_struct(mixed_struct s) {
    printf("mixed_struct: a=%u, b=%f\n", s.a, s.b);
}

typedef struct {
    int a;
    int b;
} int32_struct;

void print_int32_struct(int32_struct s) {
    printf("int32_struct: a=%d, b=%d\n", s.a, s.b);
}