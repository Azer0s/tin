# Cross-compilation

Tin can produce binaries for an OS/architecture other than the one it
runs on. Both directions are supported on amd64 and arm64:

| Host         | Target                                          |
|--------------|-------------------------------------------------|
| Linux amd64  | linux/{amd64, arm64, 386}, darwin/{amd64, arm64}|
| Linux arm64  | linux/{amd64, arm64}, darwin/{amd64, arm64}     |
| macOS amd64  | darwin/{amd64, arm64}, linux/{amd64, arm64}     |
| macOS arm64  | darwin/{amd64, arm64}, linux/{amd64, arm64}     |

The IR pipeline runs through host `opt` and host `ld.lld` (which
handles both ELF and Mach-O via `ld64.lld` for darwin targets), with
clang only invoked for C source compilation -- the host clang's
`-target <triple>` flag, plus the appropriate sysroot, sets up the
cross-compile cleanly. Per-fn CTFE caches always build for the host
triple -- they're `dlopen`'d by the running compiler during constant
folding, so they must match the host ABI even when the user-facing
binary targets a different platform.

---

## Quick start

```sh
# Linux host -> Darwin arm64 binary
./tin build server.tin -target darwin/arm64 -o server.darwin

# macOS host -> Linux amd64 binary
./tin build server.tin -target linux/amd64 -o server.linux
```

Run the result with the matching toolchain - `darling shell ./server.darwin`
on Linux, or directly on macOS / Linux.

---

## Targeting Darwin (any host)

Cross-compiling to Darwin needs the **macOS SDK** so clang can find
Darwin headers (`malloc/malloc.h`, system frameworks) and the `lld`
Mach-O linker. The compiler resolves the SDK in this order:

1. `--macos-sdk PATH` flag
2. `$TIN_MACOS_SDK` environment variable
3. `xcrun --sdk macosx --show-sdk-path` (only on macOS hosts)
4. `~/.darling/MacOSX.sdk` (Darling install on Linux)
5. `/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk` (Xcode CLT, macOS)

If none resolves, the build falls through to bare `-target` and clang
produces a clear "header not found" error.

```sh
# explicit SDK
./tin build app.tin -target darwin/arm64 --macos-sdk /opt/MacOSX14.sdk

# environment override (handy for CI)
TIN_MACOS_SDK=/opt/MacOSX14.sdk ./tin build app.tin -target darwin/arm64
```

On macOS hosts the SDK is auto-detected via `xcrun` - no flag needed.

---

## Targeting Linux from a non-Linux host

Cross-compiling to Linux from macOS needs a **Linux sysroot** with
glibc/musl headers, `ld-linux*`, and the libraries Tin links against
(libpcre2, openssl, libffi, etc.). Resolution order:

1. `--linux-sysroot PATH` flag
2. `$TIN_LINUX_SYSROOT` environment variable
3. `$(brew --prefix)/x86_64-linux-gnu/sysroot` (Homebrew cross-toolchain)
4. `/opt/cross/x86_64-linux-gnu/sysroot` (manual install)

```sh
# explicit
./tin build app.tin -target linux/amd64 --linux-sysroot /opt/x86_64-linux-gnu

# environment
TIN_LINUX_SYSROOT=/opt/cross-rootfs ./tin build app.tin -target linux/arm64
```

On Linux hosts targeting Linux (different arch only), the host's
`/usr/include` and `/usr/lib` are already correct and no sysroot is
needed.

---

## How the toolchain is selected

The compiler picks tools and flags based on host vs target:

| Step              | Same arch                | Cross-compile                          |
|-------------------|--------------------------|----------------------------------------|
| IR -> bitcode     | host `opt`               | host `opt` (target-agnostic at this stage) |
| C source          | host clang               | host clang + `-target <triple>`        |
| Link              | `ld.lld` / `ld64.lld`    | same; lld port chosen by target OS     |
| Sysroot           | implicit                 | `-isysroot` (Darwin) / `--sysroot` (Linux) |
| CTFE per-fn `.so` | host triple always       | host triple always (ignores `-target`) |

`lld` is required because GNU `ld` doesn't speak Mach-O and Apple's
`ld64` doesn't speak ELF. lld provides both `ld.lld` (ELF) and
`ld64.lld` (Mach-O) under one project; the build picks the right one
based on `-target`.

> **Note:** the Mach-O path (`ld64.lld`) hasn't been end-to-end
> validated on a real macOS host since the move to direct LLVM tools.
> The probe + split-argv machinery is host-OS-agnostic, but Mach-O
> uses an entirely different linker syntax (`-arch`, `-platform_version`,
> `-l<name>` only -- no `--build-id`, no `--gc-sections`), so the
> first macOS build under this pipeline may surface issues. File
> a bug if you hit one.

---

## CTFE and cross-compile

The `#pure` constant-folding cache compiles per-function shared
objects that the running `tin` process `dlopen`s during compilation.
Those `.so` files MUST match the host architecture, even when the
user-facing build targets a different platform - otherwise `dlopen`
fails with "wrong ELF class" or similar.

This is handled automatically: the per-fn cache pipeline calls
`hostClangTargetFlag()` (which deliberately returns nil) instead of
`clangTargetFlag()`. So you can cross-compile a `#pure`-heavy program
without thinking about the cache; both the cache `.so`s and the final
binary land on the right side.

---

## Verifying a cross-build

```sh
$ ./tin build app.tin -target darwin/arm64 -o app.darwin
$ file app.darwin
app.darwin: Mach-O 64-bit arm64 executable, dynamically linked

$ ./tin build app.tin -target linux/amd64 --linux-sysroot /opt/sysroot
$ file app
app: ELF 64-bit LSB pie executable, x86-64, dynamically linked
```

---

## Troubleshooting

**"can't find macOS SDK"**: install the Xcode Command Line Tools
(`xcode-select --install`) on macOS, or [Darling](https://www.darlinghq.org)
on Linux. As a fallback, download a standalone SDK tarball and pass it
with `--macos-sdk`.

**"can't find linker lld"**: install LLVM's `lld`. `apt install lld`
on Debian/Ubuntu, `pacman -S lld` on Arch, `brew install lld` on macOS.
The package installs `ld.lld` (ELF) and `ld64.lld` (Mach-O) together;
tin picks the right port based on `-target`.

**"header not found" at clang stage**: the sysroot is incomplete or
points at the wrong path. Confirm `<sysroot>/usr/include/stdio.h`
exists and that the arch under `<sysroot>/usr/lib/<arch>` matches
your target.

**dlopen fails during CTFE**: this should not happen - the cache
always builds for the host triple. If it does, file an issue with the
output of `./tin --verbose build ...`; the cache directory is
`.build/pure-fn/<hash>/` and `file bin.so` should report the host
arch.
