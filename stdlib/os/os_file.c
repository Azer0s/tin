// stdlib/os/os_file.c - POSIX wrappers backing stdlib/os/os_file.tin.
//
// open(2) on POSIX takes (path, flags, mode).  flags is a bitfield of
// O_RDONLY / O_WRONLY / O_RDWR / O_CREAT / O_TRUNC / O_APPEND.  Tin
// surfaces those at the source level as an `OpenMode` atom enum and
// translates here.

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

// Flag bits expressed in a portable way so the .tin caller can OR
// them without depending on libc header constants.  These are
// returned by the per-flag accessors below.
int32_t _tin_os_o_rdonly(void) { return O_RDONLY; }
int32_t _tin_os_o_wronly(void) { return O_WRONLY; }
int32_t _tin_os_o_rdwr(void)   { return O_RDWR;   }
int32_t _tin_os_o_creat(void)  { return O_CREAT;  }
int32_t _tin_os_o_trunc(void)  { return O_TRUNC;  }
int32_t _tin_os_o_append(void) { return O_APPEND; }

// _tin_os_open opens path with the given POSIX flags and 0644
// permissions when creating.  Returns the fd on success, -1 on
// failure (errno preserved).
int32_t _tin_os_open(const char *path, int32_t flags) {
    return open(path, (int)flags, 0644);
}

// _tin_os_lseek wraps lseek(2). whence: 0 = SEEK_SET, 1 = SEEK_CUR,
// 2 = SEEK_END.  Returns the new offset on success, -1 on error.
int64_t _tin_os_lseek(int32_t fd, int64_t offset, int32_t whence) {
    return (int64_t)lseek((int)fd, (off_t)offset, (int)whence);
}

// _tin_os_fsync flushes any buffered writes for fd to the underlying
// device.  Returns 0 on success, -1 on failure.
int32_t _tin_os_fsync(int32_t fd) {
    return fsync((int)fd);
}

// _tin_os_pipe creates a pipe pair: out[0] is the read end, out[1]
// the write end.  Returns 0 on success, -1 on failure.
int32_t _tin_os_pipe(int32_t *out) {
    int fds[2];
    int rc = pipe(fds);

    if (rc != 0) {
        return -1;
    }

    out[0] = (int32_t)fds[0];
    out[1] = (int32_t)fds[1];

    return 0;
}

// _tin_os_chmod wraps chmod(2).
int32_t _tin_os_chmod(const char *path, int32_t mode) {
    return chmod(path, (mode_t)mode);
}

// _tin_os_symlink creates a symlink at linkpath pointing to target.
int32_t _tin_os_symlink(const char *target, const char *linkpath) {
    return symlink(target, linkpath);
}

// _tin_os_readlink reads the value of a symlink at path into out (up
// to out_len bytes).  Returns the number of bytes read (excluding
// any nul terminator readlink doesn't write), or -1 on failure.
int64_t _tin_os_readlink(const char *path, char *out, int64_t out_len) {
    ssize_t n = readlink(path, out, (size_t)out_len);

    return (int64_t)n;
}

// _tin_os_mkstemp creates a unique file from a template ending in
// "XXXXXX" (mkstemp(3)).  Modifies template in place with the actual
// path.  Returns the fd, or -1 on failure.
int32_t _tin_os_mkstemp(char *template_buf) {
    return mkstemp(template_buf);
}

// _tin_os_mkdtemp creates a unique directory from a template ending
// in "XXXXXX" (mkdtemp(3)).  Returns 0 on success (template_buf
// holds the dir path), -1 on failure.
int32_t _tin_os_mkdtemp(char *template_buf) {
    return mkdtemp(template_buf) == NULL ? -1 : 0;
}

// _tin_os_tmpdir returns the platform's tempfile root.  Honours
// $TMPDIR when set, otherwise /tmp.  Caller copies into a Tin string;
// the returned buffer is owned by libc/this function.
const char *_tin_os_tmpdir(void) {
    const char *t = getenv("TMPDIR");

    if (t && *t) {
        return t;
    }

    return "/tmp";
}
