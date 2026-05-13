// stdlib/net/dns - getaddrinfo / getnameinfo wrappers.
//
// Compiled via:
//   //!+dns.c -- -I $TIN_RUNTIME
//
// Convention: Tin passes its `string` type to externs as a NUL-
// terminated `const char *` (the data half of the fat-ptr), and any
// TinString we produce flows back through an out-parameter set via
// _tin_string_from_bytes.  This mirrors stdlib/net/udp.

#include "runtime.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <netdb.h>
#include <arpa/inet.h>
#include <stdio.h>
#include <string.h>
#include <stdint.h>

// _tin_dns_lookup_host resolves `host` to one-or-more textual IP
// addresses.  On success writes 0 to *err_code and stores a newline-
// joined TinString in *result.  On failure writes a non-zero EAI_*
// code and stores an empty TinString.  Both v4 and v6 records are
// emitted.
void _tin_dns_lookup_host(const char *host,
                          int32_t *err_code,
                          TinString *result) {
    // Leave *result alone on error so we don't allocate then leak an
    // RC-managed empty string the Tin caller never reads.  Tin
    // already initialised the slot to a "" literal; on err_code != 0
    // the caller short-circuits before touching *result.
    *err_code = 0;

    if (host == NULL) {
        *err_code = (int32_t)EAI_NONAME;
        return;
    }

    struct addrinfo hints;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family   = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    struct addrinfo *res = NULL;
    int rc = getaddrinfo(host, NULL, &hints, &res);
    if (rc != 0) {
        *err_code = (int32_t)(rc == 0 ? EAI_FAIL : (rc < 0 ? -rc : rc));
        return;
    }

    // Two-pass: measure total length, then materialise once.
    size_t total = 0;
    int count = 0;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        char ipbuf[INET6_ADDRSTRLEN];
        const char *ok = NULL;
        if (ai->ai_family == AF_INET) {
            struct sockaddr_in *sa = (struct sockaddr_in *)ai->ai_addr;
            ok = inet_ntop(AF_INET, &sa->sin_addr, ipbuf, sizeof(ipbuf));
        } else if (ai->ai_family == AF_INET6) {
            struct sockaddr_in6 *sa = (struct sockaddr_in6 *)ai->ai_addr;
            ok = inet_ntop(AF_INET6, &sa->sin6_addr, ipbuf, sizeof(ipbuf));
        }
        if (ok == NULL) continue;
        size_t l = strlen(ipbuf);
        total += l + (count > 0 ? 1 : 0);
        count++;
    }

    if (count == 0) {
        freeaddrinfo(res);
        *err_code = (int32_t)EAI_NONAME;
        return;
    }

    // INET6_ADDRSTRLEN is 46 on POSIX; even 32 mixed RRs fit in 1.5
    // KiB.  Cap at a comfortable upper bound so the bookkeeping stays
    // on the C stack and no heap alloc shows up under leak tracking.
    if (total > 8192) {
        freeaddrinfo(res);
        *err_code = (int32_t)EAI_MEMORY;
        return;
    }

    char scratch[8193];

    char *p = scratch;
    int seen = 0;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        char ipbuf[INET6_ADDRSTRLEN];
        const char *ok = NULL;
        if (ai->ai_family == AF_INET) {
            struct sockaddr_in *sa = (struct sockaddr_in *)ai->ai_addr;
            ok = inet_ntop(AF_INET, &sa->sin_addr, ipbuf, sizeof(ipbuf));
        } else if (ai->ai_family == AF_INET6) {
            struct sockaddr_in6 *sa = (struct sockaddr_in6 *)ai->ai_addr;
            ok = inet_ntop(AF_INET6, &sa->sin6_addr, ipbuf, sizeof(ipbuf));
        }
        if (ok == NULL) continue;
        size_t l = strlen(ipbuf);
        if (seen > 0) *p++ = '\n';
        memcpy(p, ipbuf, l);
        p += l;
        seen++;
    }
    *p = '\0';
    freeaddrinfo(res);

    *result = _tin_string_from_bytes(scratch, (int64_t)total);
}

// _tin_dns_lookup_addr does the reverse: take a textual IP and
// return the hostname its PTR record resolves to.  Same convention
// as lookup_host (empty *result + non-zero *err_code on failure).
void _tin_dns_lookup_addr(const char *ip,
                          int32_t *err_code,
                          TinString *result) {
    *err_code = 0;

    if (ip == NULL) {
        *err_code = (int32_t)EAI_NONAME;
        return;
    }

    struct sockaddr_in  sa4;
    struct sockaddr_in6 sa6;
    struct sockaddr    *sa = NULL;
    socklen_t           slen = 0;

    memset(&sa4, 0, sizeof(sa4));
    memset(&sa6, 0, sizeof(sa6));

    if (inet_pton(AF_INET, ip, &sa4.sin_addr) == 1) {
        sa4.sin_family = AF_INET;
        sa  = (struct sockaddr *)&sa4;
        slen = (socklen_t)sizeof(sa4);
    } else if (inet_pton(AF_INET6, ip, &sa6.sin6_addr) == 1) {
        sa6.sin6_family = AF_INET6;
        sa  = (struct sockaddr *)&sa6;
        slen = (socklen_t)sizeof(sa6);
    } else {
        *err_code = (int32_t)EAI_NONAME;
        return;
    }

    char hostbuf[NI_MAXHOST];
    int rc = getnameinfo(sa, slen, hostbuf, sizeof(hostbuf), NULL, 0, NI_NAMEREQD);
    if (rc != 0) {
        *err_code = (int32_t)(rc < 0 ? -rc : rc);
        return;
    }

    *result = _tin_string_from_bytes(hostbuf, (int64_t)strlen(hostbuf));
}

// _tin_dns_gai_strerror writes the textual error matching the
// EAI_* code from lookup_host / lookup_addr into *out.
void _tin_dns_gai_strerror(int32_t code, TinString *out) {
    int actual = (int)code;
    if (actual < 0) actual = -actual;
    const char *msg = gai_strerror(actual);
    if (msg == NULL) msg = "dns: unknown error";
    *out = _tin_string_from_bytes(msg, (int64_t)strlen(msg));
}
