// stdlib/os/os_process.c -- fork / execvp / waitpid / kill wrappers
// backing stdlib/os/os_process.tin.
//
// spawn does the fork-then-exec dance in one call so the caller's
// Tin-side surface is `let p = os::spawn(argv)` and the file
// descriptors of any redirected stdio remain in the parent's hand
// for reading / writing.

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

// argv_buf is a contiguous buffer of (char* | NULL)*; argc is the
// number of non-NULL entries.  in_fd / out_fd / err_fd are POSIX
// file descriptors that will be dup2'd onto fds 0/1/2 in the child
// before exec, or -1 to inherit the parent's.  cwd, if non-NULL,
// is chdir'd into before exec.  envp, if non-NULL, replaces the
// child's environment.
//
// Returns the child pid on success, -1 on failure.
int64_t _tin_os_spawn(char *const argv[], int32_t argc,
                      int32_t in_fd, int32_t out_fd, int32_t err_fd,
                      const char *cwd, char *const envp[]) {
    (void)argc;
    // Fast-fail on an absolute path that doesn't exist: skip the fork so
    // failure surfaces without an unreachable child.  Required under
    // macOS `leaks --atExit`, which traces forked children and freezes
    // any child that calls _exit before exec -- waitpid in the parent
    // hangs forever.  Relative names (no leading '/') still need execvp's
    // PATH resolution, so leave them to the normal fork path.
    if (argv != NULL && argv[0] != NULL && argv[0][0] == '/') {
        if (access(argv[0], F_OK) != 0) {
            return -1;
        }
    }

    pid_t pid = fork();
    if (pid < 0) {
        return -1;
    }

    if (pid == 0) {
        // Child.
        if (in_fd  >= 0) { dup2(in_fd,  STDIN_FILENO);  }
        if (out_fd >= 0) { dup2(out_fd, STDOUT_FILENO); }
        if (err_fd >= 0) { dup2(err_fd, STDERR_FILENO); }

        if (cwd != NULL && *cwd != '\0') {
            if (chdir(cwd) != 0) {
                _exit(127);
            }
        }

        if (envp != NULL) {
            // execvpe isn't portable (Linux glibc has it, macOS
            // doesn't). Mutate the child's environ pointer instead.
            // The cast through char** is safe - we're in the child,
            // about to exec, and POSIX guarantees no other thread
            // touches environ here.
            extern char **environ;
            environ = (char **)(uintptr_t)envp;
        }
        execvp(argv[0], argv);
        // execvp returned -> failure.
        _exit(127);
    }

    return (int64_t)pid;
}

// _tin_os_exec replaces the current process image with argv.  Only
// returns on failure.
int32_t _tin_os_exec(char *const argv[], int32_t argc) {
    (void)argc;
    execvp(argv[0], argv);

    return -1;
}

// _tin_os_waitpid blocks until pid exits.  out_status receives the
// raw status word from waitpid.  Returns the pid on success (== input),
// or -1 on failure.  Use the W*() helpers below to interpret status.
int64_t _tin_os_waitpid(int64_t pid, int32_t *out_status) {
    int status = 0;
    pid_t r = waitpid((pid_t)pid, &status, 0);

    if (r < 0) {
        return -1;
    }

    if (out_status) {
        *out_status = (int32_t)status;
    }

    return (int64_t)r;
}

// _tin_os_status_exit_code returns the exit code of a process whose
// status came from _tin_os_waitpid.  If the child died on a signal,
// returns -signo (negative).  If neither, returns -1.
int32_t _tin_os_status_exit_code(int32_t status) {
    if (WIFEXITED(status)) {
        return WEXITSTATUS(status);
    }
    if (WIFSIGNALED(status)) {
        return -WTERMSIG(status);
    }

    return -1;
}

// _tin_os_kill sends signal sig to pid.  Returns 0 on success, -1 on
// failure (errno preserved).
int32_t _tin_os_kill(int64_t pid, int32_t sig) {
    return kill((pid_t)pid, (int)sig);
}

// Common signal numbers (portably accessible from Tin without
// depending on signal.h constants leaking through).
int32_t _tin_os_sigterm(void) { return SIGTERM; }
int32_t _tin_os_sigkill(void) { return SIGKILL; }
int32_t _tin_os_sigint(void)  { return SIGINT;  }
int32_t _tin_os_sighup(void)  { return SIGHUP;  }
