# tin

A statically typed, expression-oriented systems language that compiles to
native code through LLVM. Tin sits between Go and Rust on the
ergonomics-vs-control axis: it has fibers and channels like Go, ARC and
deterministic destruction like Rust, and a syntax that reads like Python
or Crystal.

## Quick taste

```rust
echo "Hello, world!"

// Pattern-matched fibonacci
fn fib(n u32) u32 =
  where n <= 1: n
  where _:      fib(n - 1) + fib(n - 2)

echo fib(10)

// Structs with methods
struct person =
  name string
  age  u8

  fn show(this person) string = return "{this.name} is {this.age}"

let pete = person{name: "Pete", age: 20}
echo pete.show()

// Fibers and channels
use sync

fn{#async} worker(id i64, ch sync::Channel[string]) =
  ch.send("result from {id}")

let ch = sync::Channel[string].make(2)
spawn worker(1, ch)
spawn worker(2, ch)
echo await ch.recv()
echo await ch.recv()
```

The full [language reference](docs/README.md) covers syntax, traits, the
type system, fibers, macros, control tags, and C interop in depth.

## Getting started

Prerequisites: clang + LLVM 17+, Go 1.25+, libffi. See
[Dependencies](#dependencies) for the full list.

```sh
git clone <this-repo>
cd tin
go build -o tin .

./tin run examples/hello.tin
```

That's it. There are no install targets; treat the `tin` binary as
relocatable and put it on your `PATH` if you want.

### Common commands

```sh
tin run    file.tin           # compile and execute
tin build  file.tin -o out    # produce a native binary
tin build  --lib file.tin     # produce a relocatable .o
tin test   path/...           # run all `test` blocks under path/, recursive
tin repl                      # interactive REPL (preload with `tin repl file.tin`)
tin clean                     # wipe the .build/ cache
```

`tin run` and `tin test` content-hash every input file (entry source,
imported packages, `//!+` C sources) and skip lex/parse/codegen entirely
when the cache directory's `sbom.txt` matches. Cache lives at
`.build/<run|test>/<file>_<md5>/`.

For tighter loops on the test suite: `tin test --fast path/...` drops to
`-O0` so LLVM's optimizer doesn't dominate compile time.

## Dependencies

**Build-time:** clang + LLVM 17+, Go 1.25+, libffi. On Linux you also
need libdl (implicit on macOS).

**Runtime base:** libc, libpthread (already inside libc on glibc >= 2.34
and macOS libsystem; older glibc needs `-lpthread` explicit).

**Stdlib modules - only if imported:**

| Module | Adds |
|---|---|
| `math`, `floats` | `-lm` |
| `regex`          | `-lpcre2-8` |
| `tls`            | `-lssl -lcrypto` (OpenSSL >= 3) |
| `simd`           | `-msse4.2` on x86_64, NEON on aarch64 |

**Builtins - only if used:**

| Builtin | Adds |
|---|---|
| `stacktrace()` | `-ldw` (elfutils) on Linux/FreeBSD for `file:line:col` resolution; nothing extra on macOS |

`stacktrace()` walks the saved frame-pointer chain (rbp on x86_64,
x29 on aarch64), so any C code that should appear in a Tin trace
must be built with `-fno-omit-frame-pointer` - the runtime adds it
to its own translation units, and Tin's codegen tags every IR
function with `frame-pointer="all"`, but third-party `#interop`
callers compiled with the usual `-O2` will be invisible to the walk.

elfutils (`libdw`) is Linux/FreeBSD only - macOS has no equivalent
shipped. On Darwin the runtime falls back to dladdr-only resolution
(`<symbol>+0x<offset>` per frame, no source coords). Pipe through
`atos` or `llvm-symbolizer` post-hoc when you need source positions.

### Distro install hints

```sh
# Debian / Ubuntu
sudo apt install clang llvm libffi-dev libpcre2-dev libssl-dev libdw-dev

# Arch
sudo pacman -S clang llvm libffi pcre2 openssl elfutils

# macOS (Homebrew)
brew install llvm libffi pcre2 openssl@3
# elfutils is not packaged on macOS; stacktrace falls back to symbol+offset.
```

## License

See `LICENSE`.
