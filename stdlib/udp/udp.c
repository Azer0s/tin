// stdlib/udp - UDP socket helpers
//
// Compiled via: //!+udp.c -- -I ../../runtime

#include "runtime.h"
#include "async_io.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <string.h>
#include <stdio.h>
#include <unistd.h>
#include <errno.h>

// Create a bound UDP socket on port (for receiving datagrams).
// Returns the fd on success, or -errno on error.
int32_t _tin_udp_listen(int32_t port) {
    int fd = socket(AF_INET, SOCK_DGRAM, 0);
    if (fd < 0) return -(int32_t)errno;

    int opt = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family      = AF_INET;
    addr.sin_port        = htons((uint16_t)port);
    addr.sin_addr.s_addr = INADDR_ANY;

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        int e = errno; close(fd); return -(int32_t)e;
    }
    return (int32_t)fd;
}

// Create a connected UDP socket to host:port.
// Returns the fd on success, or -errno on error.
// host is a null-terminated C string (Tin passes the char* from its fat string).
int32_t _tin_udp_connect(const char *host, int32_t port) {
    char portbuf[16];
    snprintf(portbuf, sizeof(portbuf), "%d", (int)port);

    struct addrinfo hints, *res;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family   = AF_INET;
    hints.ai_socktype = SOCK_DGRAM;
    if (getaddrinfo(host, portbuf, &hints, &res) != 0)
        return -(int32_t)errno;

    int fd = socket(res->ai_family, res->ai_socktype, res->ai_protocol);
    if (fd < 0) { freeaddrinfo(res); return -(int32_t)errno; }

    if (connect(fd, res->ai_addr, (socklen_t)res->ai_addrlen) < 0) {
        int e = errno; close(fd); freeaddrinfo(res); return -(int32_t)e;
    }
    freeaddrinfo(res);
    return (int32_t)fd;
}

// Non-blocking recvfrom.
// Returns bytes received (>= 0), -errno on hard error, or INT64_MIN if blocked.
// On success, constructs a TinString in *host_out with the dotted-decimal source IP
// and sets *port_out to the source port in host byte order.
int64_t _tin_udp_recvfrom(int32_t fd, void *buf, int64_t n,
                           TinString *host_out, int32_t *port_out) {
    _tin_set_nonblocking(fd);

    struct sockaddr_in src;
    socklen_t src_len = sizeof(src);
    ssize_t r;
    do {
        r = recvfrom((int)fd, buf, (size_t)n, 0,
                     (struct sockaddr *)&src, &src_len);
    } while (r < 0 && errno == EINTR);

    if (r >= 0) {
        char host_buf[48];
        inet_ntop(AF_INET, &src.sin_addr, host_buf, sizeof(host_buf));
        *host_out = _tin_string_from_bytes(host_buf, (int64_t)strlen(host_buf));
        *port_out = (int32_t)ntohs(src.sin_port);
        return (int64_t)r;
    }
    if (errno == EAGAIN || errno == EWOULDBLOCK)
        return _tin_async_park_read(fd);

    *host_out = _tin_string_from_bytes("", 0);
    *port_out = 0;
    return -(int64_t)errno;
}

// Non-blocking sendto.
// Returns bytes sent (>= 0), -errno on hard error, or INT64_MIN if blocked.
// host is a null-terminated C string (Tin passes the char* from its fat string).
int64_t _tin_udp_sendto(int32_t fd, const void *buf, int64_t n,
                         const char *host, int32_t port) {
    struct sockaddr_in dst;
    memset(&dst, 0, sizeof(dst));
    dst.sin_family = AF_INET;
    dst.sin_port   = htons((uint16_t)port);

    if (inet_pton(AF_INET, host, &dst.sin_addr) <= 0) {
        struct addrinfo hints, *res;
        memset(&hints, 0, sizeof(hints));
        hints.ai_family   = AF_INET;
        hints.ai_socktype = SOCK_DGRAM;
        if (getaddrinfo(host, NULL, &hints, &res) != 0)
            return -(int64_t)EINVAL;
        dst = *(struct sockaddr_in *)res->ai_addr;
        dst.sin_port = htons((uint16_t)port);
        freeaddrinfo(res);
    }

    _tin_set_nonblocking(fd);
    ssize_t r;
    do {
        r = sendto((int)fd, buf, (size_t)n, 0,
                   (const struct sockaddr *)&dst, sizeof(dst));
    } while (r < 0 && errno == EINTR);

    if (r >= 0) return (int64_t)r;
    if (errno == EAGAIN || errno == EWOULDBLOCK)
        return _tin_async_park_write(fd);

    return -(int64_t)errno;
}
