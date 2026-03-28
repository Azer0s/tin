#include <stdio.h>

int add(int a, int b) { return a + b; }

#ifdef DEBUG
void greet() { printf("debug mode\n"); }
#else
void greet() { printf("release mode\n"); }
#endif
