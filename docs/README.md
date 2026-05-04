# Tin Language Documentation

Tin is a statically typed, compiled systems language with a clean,
expression-oriented syntax. It compiles to native code via LLVM.

## Table of Contents

| Document | Contents |
|----------|----------|
| [01 - Basics](01-basics.md) | Types, variables, `echo`, string interpolation, operators |
| [02 - Control Flow](02-control-flow.md) | `if/else`, `for`, `match` (struct + array patterns), `where`, `defer`, `panic` |
| [03 - Functions](03-functions.md) | Functions, closures, generics, pipe operator, overloading |
| [04 - Collections](04-collections.md) | Arrays, slices, ranges, destructuring |
| [05 - Structs](05-structs.md) | Structs, methods, `fn init`/`fn deinit`, generics, type aliases, tuples |
| [06 - Traits](06-traits.md) | Trait declaration, default methods, forward fields, vtable dispatch, generic traits |
| [07 - Enums & Unions](07-enums-unions.md) | Integer enums, atom enums, tagged unions, native C unions |
| [08 - C Interop](08-interop.md) | `extern`, pointers, C struct interop, linker directives (`//!`) |
| [09 - Packages](09-packages.md) | `use`/`export`, package resolution, standard library overview |
| [10 - Reflection](10-reflection.md) | Atoms, `any` type, `typeof`, `traitof`, `fieldnames`, `getfield`, `setfield`, `sourcepos`, `stacktrace` |
| [11 - Testing](11-testing.md) | `test` blocks, `assert` stdlib, `tin test` command |
| [12 - Macros](12-macros.md) | Simple macros (AST substitution), CTFE macros, backtick code-splice literals |
| [13 - Control Tags](13-control-tags.md) | `#pure`, `#sideffect`, `#no_recurse`, `#no_thread`, `#allow_sideffect` |
| [14 - Fibers & Channels](14-fibers.md) | `spawn`, `await`, `yield`, `await match`, `Channel[T]`, `Future[T]`, async I/O, M:N scheduler |

## Tooling

| Document | Contents |
|----------|----------|
| [Cross-compilation](cross-compile.md) | Build Linux/Darwin binaries from any host: `-target`, sysroot/SDK autodetect, lld, CTFE caching |

## Contributing

| Document | Contents |
|----------|----------|
| [Style Guide](style.md) | Code style for stdlib `.tin` files: spacing, comments, extern grouping, exports |

## Standard Library

| Document                           | Contents                                                                                       |
|------------------------------------|------------------------------------------------------------------------------------------------|
| [Collections](stdlib/collections.md) | Generic collections: `LinkedList[T]`, `HashMap[K,V]`, `List[T]` and `Map[K,V]` traits        |
| [Encoding](stdlib/encoding.md)     | Encoding/decoding: `base16`, `base64`, `url`, `json`, `yaml` sub-packages                     |
| [Errors](stdlib/errors.md)         | Error type: `Err` alias, `new`, `wrap`, `has`, `equals`                                        |
| [Floats](stdlib/floats.md)         | IEEE 754 special values: `NaN`, `Inf`, `NegInf`, `is_nan`, `is_finite`                         |
| [Hash](stdlib/hash.md)             | Hash functions: FNV-1a, MD5, SHA-1, xxHash3                                                    |
| [Log](stdlib/log.md)               | Leveled logging with text or JSON output, ANSI color, call-site source positions               |
| [Measure](stdlib/measure.md)       | Monotonic clock: `now_us`, `now_ms` for benchmarking                                           |
| [Source](stdlib/source.md)         | Parse `sourcepos` / `stacktrace` atoms into `SrcPos { symbol, file, line, col, lib, ... }`     |
| [Networking](stdlib/networking.md) | `io`, `ioutil`, `tcp`, `udp`, `unix` - async I/O and socket types                              |
| [Rc](stdlib/rc.md)                 | `rc::Cell[T]` refcounted handle for shared C resources -- the wrapper that lets `Atomic`, `Mutex`, etc. be copied safely |
| [Regex](stdlib/regex.md)           | PCRE regular expressions: `compile`, `exec`, `find_all`, `replace`, `split`                    |
| [SIMD](stdlib/simd.md)             | Portable SIMD: vector types, `splat`, `loadu`, `cmpeq`, `movemask`, arch directives            |
| [Strings](stdlib/strings.md)       | String operations: `replace`, `split`, `join`, `trim`, `contains`, `index_of`, case conversion |

## Quick taste

Tin compiles to native code via LLVM. Run a file with `tin run file.tin`,
build a binary with `tin build file.tin`, and run tests with `tin test file.tin`
(or `tin test dir/` for one directory, `tin test dir/...` to recurse).

```rust
// Hello world
echo "Hello, world!"

// Fibonacci with pattern matching
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10)

// Structs with methods
struct person =
  name string
  age  u8

  fn init(this person) =
    echo "new person: {this.name}"

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
echo pete.show()

// Traits
trait named =
  label string forward
  fn name(this named) string = return this.label

struct cat(named) =
  breed string

let c = cat{label: "Whiskers", breed: "tabby"}
echo c.name()

// Fibers and channels
use sync

fn{#async} worker(id i64, ch sync::Channel[string]) =
  ch.send("result from fiber {id}")

fn main() =
  let ch = sync::Channel[string].make(4)
  spawn worker(1, ch)
  spawn worker(2, ch)
  echo await ch.recv()   // "result from fiber 1" or "2", whichever finishes first
  echo await ch.recv()
```

## Get started

Prerequisites: `clang`, `opt`, and `ld.lld` (the `lld` package on most
distros), all from LLVM 21 or newer, on `PATH`. The IR pipeline runs
through `opt` and `ld.lld` directly; `clang` is only invoked for C
source compilation and a few one-shot tooling probes.

```sh
go build .
./tin
```

That's it. `./tin run file.tin` to compile and execute, `./tin test dir/` to run
test blocks, or `./tin repl` for the interactive REPL (optionally `./tin repl
file.tin` to preload a file's declarations).

## Incremental compilation

Tin's build pipeline is content-addressed end to end. There are three
cache layers, each serving a different recompile pattern:

### 1. Final-binary cache (`tin run`, `tin test`, `tin build`)

All three subcommands cache the compiled binary under
`.build/<mode>/<file>_<src_md5>_<flags_hash>/`, alongside an
`sbom.txt` listing every file the build pulled in (entry source,
imported package sources, `//!+` C sources, the **compiler binary
itself**) with its MD5.

Subsequent invocations skip lex/parse/codegen entirely if the entry
source MD5 still names a cache dir AND every file recorded in that
dir's `sbom.txt` still hashes the same. The `flags_hash` covers
`-O0/-O2`, `-g`, `-target os/arch`, and `--cflag` forwards, so an
`-O0` rebuild won't silently reuse an `-O2` binary.

`tin run`/`tin test` exec the cached binary directly. `tin build`
copies it to the user's `-o` path. A typical hot rebuild of a
medium program is ~30-50 ms (read + copy), down from several seconds
of thinLTO link work.

### 2. Per-package object cache (`.build/pkg/`)

Each imported package is compiled to its own `.o` file keyed by
SHA-256 of the package's IR text + canonical compile-flag set + host
arch + clang version. The slot also stores `.iface.json` +
`.iface_hash` (the interface manifest — exported function signatures,
struct shapes, trait impls).

Effect: editing one package only invalidates that package's `.o`.
Downstream consumers' IR is unchanged (their imports' interfaces
didn't change), so their `.o` slots still hit. The link step still
runs, but per-pkg compile is parallelized via the worker pool
(`-j N`, default GOMAXPROCS) using `opt` for IR-to-bitcode and ld.lld
for the link.

### 3. C-source cache (`.build/csrc/`)

`runtime.c` and every `//!+file.c` source compile to a `.o` keyed
by file content + flags + host arch + clang version. Globally shared
across every Tin compile on the machine; saves ~400 ms per invocation
on warm cache.

### Cleanup

`tin clean` wipes per-program artifacts (`.build/run/`, `.build/test/`,
`.build/build/`). The content-addressed caches (`.build/pkg/`,
`.build/csrc/`, `.build/pure-fn/`) are preserved on purpose — they
can never serve a stale entry because the key includes everything
that affects the output. Manually `rm -rf .build/` to nuke everything.

### Verification

`bash examples/incremental_cache_verify.sh` runs 10 invariant checks
(cold/warm equivalence, `-O0` vs `-O2` cache key separation,
edit-only-one-file isolation, byte-identical determinism, concurrent
build safety, upstream-pkg-body-edit isolation, etc.). CI runs this
on every push.

### Build observability

`-v` prints per-stage progress to stderr. During the parallel compile
phase, each compile job emits start/done events with elapsed time so
you can see what's actually running:

```
[5/6] hello.tin                  compile (44 TUs)
[par 1/44] pkg:assert start
[par 2/44] pkg:json start
...
[par 1/44] pkg:assert done 30ms
...
[par] 44 jobs done in 296ms
[6/6] hello                      link  3651ms
done in 4215ms
```
