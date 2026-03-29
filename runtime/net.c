// tin runtime - TCP socket helpers
//
// Minimal POSIX TCP helpers used by the echo server example and any Tin
// code that needs raw socket access.  The non-blocking async variants
// (async_accept, async_connect) are left for future work - for now
// _tin_tcp_accept is blocking and should be called from the main fiber
// or from a dedicated accept-loop fiber that does not need low latency.

#include "runtime.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>

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

// Close a file descriptor.
void _tin_fd_close(int32_t fd) {
    close((int)fd);
}
