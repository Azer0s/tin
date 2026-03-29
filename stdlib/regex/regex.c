// stdlib/regex/regex.c - thin C shims for the PCRE API
//
// pcre_free is a global variable holding a function pointer, not a
// directly callable function.  Tin's extern() mechanism only handles
// direct C functions, so we wrap it here.
//
// pcre_fullinfo has a complex signature; we expose only the capture-count
// query as a simple function.

#include <pcre.h>
#include <stdint.h>

// pcre_free_fn_wrapper calls pcre_free(ptr) through the global function pointer.
void pcre_free_fn_wrapper(void *ptr) {
    pcre_free(ptr);
}

// pcre_fullinfo_capturecount queries the number of capture groups in code.
// Stores the result in *out and returns the pcre_fullinfo return code.
int32_t pcre_fullinfo_capturecount(const pcre *code, int32_t *out) {
    int count = 0;
    int rc = pcre_fullinfo(code, NULL, PCRE_INFO_CAPTURECOUNT, &count);
    if (out) *out = (int32_t)count;
    return (int32_t)rc;
}
