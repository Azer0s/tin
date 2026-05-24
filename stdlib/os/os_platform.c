// stdlib/os - compile-time host OS detection.
//
// Returns a fixed integer per OS, chosen at C-preprocessor time via
// the well-known compiler-defined macros (`__APPLE__`, `__linux__`, ...).
// The Tin side maps the number to an atom (`'darwin`, `'linux`, ...) so
// user code can `match os::platform()` without leaking the numeric ABI.
//
// Picking the platform at preprocessor time -- rather than calling
// `uname(2)` or reading `/etc/os-release` -- keeps the call to a single
// `mov` + `ret` instruction, lets the compiler fold platform-specific
// branches statically, and makes the result robust against environment
// variable / mount-namespace tampering.
//
// Codes are tin-internal ABI -- callers should consume them through
// `os::platform()` (which converts to atoms), not directly.

#include <stdint.h>

int32_t _tin_os_platform(void) {
#if defined(__APPLE__)
    return 0;
#elif defined(__linux__)
    return 1;
#elif defined(_WIN32) || defined(_WIN64)
    return 2;
#elif defined(__FreeBSD__)
    return 3;
#elif defined(__OpenBSD__)
    return 4;
#elif defined(__NetBSD__)
    return 5;
#elif defined(__DragonFly__)
    return 6;
#elif defined(__sun) && defined(__SVR4)
    return 7;
#elif defined(__unix__)
    return 8;
#else
    return -1;
#endif
}
