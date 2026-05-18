# Linker directives (`//!`)


Files can declare linker flags as special comments starting with `//!`:

```rust
//!-lm
//!-lraylib
//!-lpthread
```

Each `//!` line is split on whitespace and the resulting tokens are forwarded to
the linker as separate argv entries. Common uses:

| Directive      | Purpose                          |
|----------------|----------------------------------|
| `//!-lm`       | Link the C math library (`libm`) |
| `//!-lraylib`  | Link Raylib                      |
| `//!-lpthread` | Link pthreads                    |

Directives must appear before any non-comment code in the file.

### Multi-token linker flags

Flags that the linker expects as several argv entries -- the classic example
being macOS `-framework Foo`, where `ld` looks up `-framework` and `Foo`
independently -- are written with the tokens space-separated on one line:

```rust
//!-framework Cocoa            [darwin]
//!-framework CoreFoundation   [darwin]
//!-Xlinker -rpath -Xlinker /opt/lib
```

Each space-separated chunk becomes its own argv element. Comma-separated forms
(`//!-Wl,-rpath,/opt/lib`) stay as a single token because they contain no
whitespace -- this matches how `clang` itself unpacks `-Wl,` arguments.

Use `$(brew --prefix ...)` shell substitution when a token must contain a
literal path that may have spaces; the command's stdout is treated as one piece
before the whitespace split runs.

```rust
//!-lm

fn c_sqrt(x f64) f64 = extern("sqrt")
fn c_pow(x f64, y f64) f64 = extern("pow")

echo c_sqrt(144.0)    // 12.0
echo c_pow(2.0, 10.0) // 1024.0
```

### Attaching C source files (`//!+file.c`)

The `//!+file.c` directive compiles a C source file alongside the Tin program and
links the resulting object:

```rust
//!+mylib.c
```

The path is resolved relative to the `.tin` file. Compiler flags for that
translation unit follow `--`:

```rust
//!+impl.c -- -I/usr/local/include -DFOO=1
```

### Platform qualifiers

Any `//!` directive can be restricted to a specific platform by appending
`[linux]`, `[darwin]`, or `[windows]` before the `--` flags (or at end of line):

```rust
//!-lssl
//!-lcrypto
//!-L/opt/local/lib [darwin]
//!+posix_impl.c [linux]
//!+win_impl.c   [windows]
```

### Environment variable expansion

`$VAR` tokens in directive strings are expanded using the process environment.
Two variables are always available:

| Variable        | Value                                          |
|-----------------|------------------------------------------------|
| `$TIN_RUNTIME`  | Path to the Tin runtime headers directory      |
| `$TIN_STDLIB`   | Path to the Tin standard library root          |

```rust
//!+impl.c -- -I $TIN_RUNTIME
```

### Shell expression expansion (`$(cmd)`)

`$(cmd args...)` tokens are evaluated by running the command in a shell and
substituting the trimmed stdout. This is useful for locating libraries whose
prefix is reported by a tool rather than a fixed environment variable:

```rust
//!+tls_impl.c [darwin] -- -I $TIN_RUNTIME -I $(brew --prefix openssl@3)/include
//!-L$(brew --prefix openssl@3)/lib [darwin]
```

Shell expansion happens before field-splitting, so paths with spaces are handled
correctly. If the command fails the token expands to an empty string; the
compiler then produces a clear error (e.g. missing header) rather than a
cryptic path error.

### Local diagnostic suppression (`//!-Wno-...`)

Place `//!-Wno-<name>` on a comment line directly above any
declaration to silence the named warning for *that one declaration*.
Blank lines and regular `//` comments between the directive and the
declaration are allowed.

```rust
struct Channel[T] =
  // Channel deliberately bypasses rc::Cell because the inline
  // send/recv path needs a single GEP+load. Sharing across copies
  // is provided by the C-level ref_count.
  //!-Wno-unwrapped-c-resource
  _ptr *void
```

Multiple comma-separated names are supported:
`//!-Wno-foo,bar`. The directive only affects the next declaration --
it does not propagate further.

This is per-source-file and per-line; for project-wide suppression
use the `-Wno-<name>` command-line flag instead.

### Valgrind suppressions (`//!-suppressions=`)

When a test runs under `tin test --valgrind`, files can opt into a valgrind
suppressions file with:

```rust
//!-suppressions=path/to/file.supp [linux]
```

The path is resolved relative to the `.tin` file. `$TIN_RUNTIME`, `$TIN_STDLIB`,
and `$ENV` variables expand the same way they do in `//!+file.c` flag lists.
The platform qualifier honours the standard `[linux]` / `[darwin]` /
`[windows]` gates so a glibc-only suppression stays scoped to Linux runs.

The directive is silently ignored on `tin test --leaks` (macOS) and on regular
`tin run` / `tin test` invocations -- it only feeds `--suppressions=PATH` to
valgrind. The silence is therefore opt-in per file: tests that don't carry
the directive run under the unmodified valgrind error budget.

`stdlib/net/dns/dns_test.tin` ships such a suppression for glibc's lazy
`__libc_dlopen_mode` -> `_dl_open` calls during the first `getaddrinfo()`;
those leave 21 rtld-bookkeeping blocks reachable for the process lifetime
that valgrind otherwise flags. The suppression rules anchor at `_dl_open` so
real leaks elsewhere are unaffected.

---

