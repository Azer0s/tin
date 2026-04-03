// tin runtime - generic fd helper
//
// TCP/UDP/Unix socket creation helpers have been moved to their respective
// stdlib packages (stdlib/tcp/tcp.c, stdlib/udp/udp.c, stdlib/unix/unix.c)
// and are compiled on-demand via //!+ directives.

#include "runtime.h"
#include <unistd.h>

// Close a file descriptor.
void _tin_fd_close(int32_t fd) {
    close((int)fd);
}
