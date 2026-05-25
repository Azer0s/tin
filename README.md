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

// Result + `try`: parse, propagate, done.
use errors
use { Result } from result
use strings

fn parse_pair(s string) Result[(i64, i64), errors::Err] =
  let parts = strings::split(s, ",")

  if len(parts) != 2:
    return Err(errors::new("need two comma-separated ints"))

  let a = try strings::parse_int(parts[0])
  let b = try strings::parse_int(parts[1])

  return Ok((a, b))

fn main() Result[Unit, errors::Err] =
  let (x, y) = try parse_pair("12,30")

  echo "{x} + {y} = {x + y}"

  return Ok(())

// Structs with methods
struct Person =
  name string
  age  u8

  fn show(this Person) string = return "{this.name} is {this.age}"

let pete = Person{name: "Pete", age: 20}
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

Prerequisites: clang + LLVM tools (`opt`, `llc`, `ld.lld`) 17+, Go 1.25+,
libffi. See [Dependencies](#dependencies) for the full list.

```sh
git clone <this-repo>
cd tin
go build -o tin ./cmd/tin

./tin run examples/hello.tin
```

That's the whole install: no install target, no package manager, no
project file. The `tin` binary is relocatable; drop it on your `PATH` if
you want.

### Common commands

```sh
tin run    file.tin           # compile and execute
tin build  file.tin -o out    # produce a native binary
tin build  --lib file.tin     # produce a relocatable .o
tin test   path/...           # run every `test` block under path/, recursive
tin repl                      # interactive REPL (preload with `tin repl file.tin`)
tin clean                     # wipe the .build/ cache
```

`tin run` and `tin test` content-hash every input (entry source,
imported packages, `//!+` C sources) and skip the entire lex / parse /
codegen pipeline when the cache directory's `sbom.txt` matches. The
cache lives at `.build/<run|test>/<file>_<md5>/` -- delete it manually
with `tin clean` or just `rm -rf .build/`.

For tight inner loops on the test suite: `tin test --fast path/...`
drops to `-O0` so LLVM's optimizer stops dominating the wall-clock
compile time.

## Dependencies

**Build-time:** clang (for C source) + LLVM tools (`opt`, `ld.lld` --
the `lld` package on most distros) 17+, Go 1.25+, libffi. On Linux you
also need libdl (implicit on macOS).

The IR pipeline runs through `opt` and `ld.lld` directly; clang is only
invoked for C-source compilation (runtime, `simd_x86.c`, user `//!+`
sources) and for tooling probes (target-triple detection, link-args
discovery). Both tool sets must be the same LLVM major version so the
bitcode they exchange stays compatible.

**Runtime base:** libc, libpthread (already inside libc on glibc >= 2.34
and macOS libsystem; older glibc needs `-lpthread` explicit).

**Stdlib modules - only if imported:**

| Module | Adds |
|---|---|
| `math`, `floats` | `-lm` |
| `regex`          | `-lpcre2-8` |
| `tls`            | `-lssl -lcrypto` (OpenSSL >= 3) |
| `simd`           | `-msse4.2` on x86_64, NEON on aarch64 |

### Distro install hints

```sh
# Debian / Ubuntu
sudo apt install clang lld llvm libffi-dev libpcre2-dev libssl-dev

# Arch
sudo pacman -S clang lld llvm libffi pcre2 openssl

# macOS (Homebrew). Homebrew's `llvm` formula bundles clang, opt and
# ld64.lld together.
brew install llvm libffi pcre2 openssl@3
```

## License

See `LICENSE`.
