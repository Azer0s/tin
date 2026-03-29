#pragma once
// tin runtime - Future[T] support
//
// The Tin-visible Future[T] struct maps to { pid i64 } at the IR level.
// The actual result storage is in the fiber scheduler (fiber.c).
// These helpers allow Tin programs to check/await futures from C code.

#include <stdint.h>
#include <stdbool.h>

// Tin-level Future struct layout (matches Future[T] in runtime/future.tin).
typedef struct {
    int64_t pid;  // fiber PID
} TinFuture;
