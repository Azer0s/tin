// stdlib/unix - Unix domain socket helpers
//
// Compiled via: //!+unix.c -- -I $TIN_RUNTIME

#include "runtime.h"
#include <sys/socket.h>
#include <sys/un.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>

// Create a Unix domain socket server at path.
// Removes any stale socket file first.
// Returns the listen fd on success, or -errno on error.
// path is a null-terminated C string (Tin passes the char* from its fat string).
int32_t _tin_unix_listen(const char *path) {
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return -(int32_t)errno;

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
    addr.sun_path[sizeof(addr.sun_path) - 1] = '\0';

    unlink(addr.sun_path);

    if (bind(fd, (struct sockaddr *)&addr, (socklen_t)sizeof(addr)) < 0) {
        int e = errno; close(fd); return -(int32_t)e;
    }
    if (listen(fd, 128) < 0) {
        int e = errno; close(fd); return -(int32_t)e;
    }
    return (int32_t)fd;
}

// Accept the next connection on a Unix domain listen socket.
// Returns the new fd on success, or -errno on error.
int32_t _tin_unix_accept(int32_t listen_fd) {
    int fd = accept((int)listen_fd, NULL, NULL);
    if (fd < 0) return -(int32_t)errno;
    return (int32_t)fd;
}

// Connect to a Unix domain socket at path.
// Returns the connected fd on success, or -errno on error.
// path is a null-terminated C string (Tin passes the char* from its fat string).
int32_t _tin_unix_connect(const char *path) {
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return -(int32_t)errno;

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
    addr.sun_path[sizeof(addr.sun_path) - 1] = '\0';

    if (connect(fd, (struct sockaddr *)&addr, (socklen_t)sizeof(addr)) < 0) {
        int e = errno; close(fd); return -(int32_t)e;
    }
    return (int32_t)fd;
}
