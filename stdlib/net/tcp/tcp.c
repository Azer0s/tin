// stdlib/tcp - TCP socket creation helpers
//
// Moved from runtime/net.c so that TCP socket support is an opt-in stdlib
// package rather than always linked.  Compiled via:
//   //!+tcp.c -- -I $TIN_RUNTIME

#include "runtime.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <netdb.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>

// Create a TCP listen socket on the given port.
// Returns the fd on success, or -errno on error.
int32_t _tin_tcp_listen(int32_t port) {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
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
    if (listen(fd, 128) < 0) {
        int e = errno; close(fd); return -(int32_t)e;
    }
    return (int32_t)fd;
}

// Accept the next incoming connection on listen_fd.
// Returns the new connection fd, or -errno on error.
int32_t _tin_tcp_accept(int32_t listen_fd) {
    int fd = accept((int)listen_fd, NULL, NULL);
    if (fd < 0) return -(int32_t)errno;
    return (int32_t)fd;
}

// Connect to host:port via TCP.
// Performs DNS resolution synchronously, then connect().
// Returns the connected fd on success, or -errno on error.
int32_t _tin_tcp_dial(const char *host, int32_t port) {
    char port_str[8];
    snprintf(port_str, sizeof(port_str), "%d", port);

    struct addrinfo hints, *res;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family   = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    int r = getaddrinfo(host, port_str, &hints, &res);
    if (r != 0) return -EHOSTUNREACH;

    int fd = -1;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (fd < 0) continue;
        if (connect(fd, ai->ai_addr, ai->ai_addrlen) == 0) break;
        int e = errno;
        close(fd);
        fd = -e;
    }
    freeaddrinfo(res);
    return (int32_t)fd;
}
