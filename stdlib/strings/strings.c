// _tin_str_replace(s, old, newstr): returns a new heap-allocated char* with all
// occurrences of `old` replaced by `newstr`. Called from stdlib/strings/strings.tin
// via extern("_tin_str_replace").  Note: takes char* (fat-ptr data field, not TinString).
#include <stddef.h>
#include <stdlib.h>
#include <string.h>

char *_tin_str_replace(const char *s, const char *old, const char *newstr) {
    if (!s || !old || !newstr) return (char *)s;
    size_t oldlen = strlen(old);
    size_t newlen = strlen(newstr);
    if (oldlen == 0) {
        char *dup = strdup(s);
        return dup ? dup : (char *)s;
    }
    // Count occurrences
    size_t count = 0;
    const char *p = s;
    while ((p = strstr(p, old)) != NULL) {
        count++;
        p += oldlen;
    }
    if (count == 0) return strdup(s);
    // Allocate result
    size_t slen = strlen(s);
    size_t rlen = slen + count * (newlen - oldlen) + 1;
    // But if newlen < oldlen, rlen might underflow - cap at slen+1 min
    if (newlen < oldlen && count * (oldlen - newlen) > slen) rlen = slen + 1;
    char *result = (char *)malloc(rlen);
    if (!result) return strdup(s);
    char *out = result;
    p = s;
    while (*p) {
        if (strncmp(p, old, oldlen) == 0) {
            memcpy(out, newstr, newlen);
            out += newlen;
            p += oldlen;
        } else {
            *out++ = *p++;
        }
    }
    *out = '\0';
    return result;
}