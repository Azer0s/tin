# Memory - ARC, biased RC, and the heap arena

## Overview

Tin manages heap memory with **automatic reference counting (ARC)**. Every
ARC block carries a 16-byte header (`TinRCHdr`) just before its data, and
the compiler emits `_tin_retain` and `_tin_release` calls at every point
where a reference is copied or leaves scope.

Two pieces sit underneath the basic ARC story:

- **Biased RC** lets blocks that have only ever been touched by one fiber
  use a non-atomic retain/release fast path. A single SHARED flag picks
  between the two paths.
- The **heap arena** is a contiguous virtual region (default 16 GiB, set
  via `TINMAXHEAP`) registered with mimalloc. Every Tin-managed
  allocation lands inside that region, so the runtime can ask
  `_tin_is_managed(ptr)` in O(1) when it needs to know whether a pointer
  came from Tin or from foreign C code.

Static string literals and similar compile-time data carry the
`TIN_RC_IMMORTAL` flag bit and skip all bookkeeping.

## `TinRCHdr` - the ARC header

```c
typedef struct {
    uint32_t rc;     // reference count
    uint32_t flags;  // TIN_RC_* bitmask
    uint64_t _pad;   // keeps header at 16 bytes; user data inherits 16-byte alignment
} TinRCHdr;
```

`runtime.h` pins `sizeof(TinRCHdr) == 16` with a `_Static_assert`. The
codegen for cLayoutStruct stack composites and the immortal-RC sentinel
hardcodes that size, so growing the header would silently drift those
offsets.

The **public pointer** (stored in `TinString.ptr`, `TinSlice.ptr`, struct
fields, etc.) points to the first byte of user data, not to the header.
The header is recovered by subtracting `sizeof(TinRCHdr)`:

```c
static inline TinRCHdr *_rc_hdr(void *ptr) {
    return (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
}
```

### Flag bits

| Flag                 | Status   | Meaning                                                                                                   |
|----------------------|----------|-----------------------------------------------------------------------------------------------------------|
| `TIN_RC_IMMORTAL`    | live     | Retain/release short-circuit. Set on static string literals, the empty-string sentinel, and atom blocks. |
| `TIN_RC_SHARED`      | live     | Picks atomic retain/release. Set by default in `_tin_rc_alloc`; cleared in `_tin_rc_alloc_local`.        |
| `TIN_RC_UNIQUE`      | reserved | Tracked by `_tin_rc_alloc_local` + `_tin_retain` + `_tin_release` for a future CoW-elision optimization. No consumer reads it yet. |
| `TIN_RC_ARENA`       | reserved | Defined and defensively checked in `_tin_release` / `_tin_release_struct` so an external-arena owner could opt out of `free()`. Never set today. |
| `TIN_RC_STATIC_DTOR` | reserved | Bit reserved for a future "no inner ARC fields" hint that lets the per-type dtor walk be skipped. Neither set nor read today. |

The `_pad` field is stamped with `TIN_RC_HDR_MAGIC` (a 64-bit nonce)
on every heap-allocated rc-block. `_tin_is_managed` reads `_pad`
through the would-be header offset and rejects pointers whose `_pad`
slot does not carry the magic -- that's how the runtime tells a
genuine block-start pointer from an interior pointer that happens to
land inside one of our blocks. See "The heap arena" below.

### The immortal sentinel

A static string literal is emitted as a `{ i64, i64, [N x i8] }` global
where the first eight bytes are stored as `-1` (all-ones). That covers
both `rc` (now `0xFFFFFFFF`) and `flags` (also `0xFFFFFFFF`, which
includes the IMMORTAL bit). `_tin_retain` and `_tin_release` see the
IMMORTAL bit and return immediately, so static data never gets touched.

## Biased RC

Most heap blocks live their whole life inside a single fiber and never
get retained or released from a different worker thread. Forcing atomic
ops on every retain/release for that common case costs roughly 3-5x on
ARM and 10-30x on x86 versus a plain increment. Biased RC pays the
atomic cost only after a block has provably escaped its origin fiber.

### Two allocators

```c
void *_tin_rc_alloc(int64_t size);        // starts with rc=1, SHARED=1 (atomic path)
void *_tin_rc_alloc_local(int64_t size);  // starts with rc=1, SHARED=0 (non-atomic), UNIQUE=1
```

The compiler picks `_tin_rc_alloc_local` for allocations inside
functions that the call-graph analyzer proved cannot publish their
result across a fiber boundary. Allocations outside that proof
(everything in shared packages, every generic monomorphization, every
function transitively reachable from a `spawn`) keep using
`_tin_rc_alloc`.

The picker lives in `codegen/runtime_ensure.go::ensureRCAlloc` and the
underlying analysis in `codegen/coro_callgraph.go::computeSpawnerReachable`.
`codegen/funcs.go::nameLooksCrossContext` is the conservative filter
that keeps the optimization off for any symbol containing `__` (package
fns, generic monos, trait-qualified methods) - those names live across
compilation contexts the per-package call graph doesn't see.

### `_tin_make_shared` - escape transition (defined, not yet emitted)

```c
void _tin_make_shared(void *ptr);
```

Atomically ORs `TIN_RC_SHARED` into `flags` with release ordering.
Designed to run at every site where a `_tin_rc_alloc_local` block
might cross a fiber boundary (spawn capture, channel send, global
store) so the receiving thread sees `SHARED=1` on its first retain.

The compiler exposes an `ensureMakeShared` declaration but currently
**emits no call to it**. Soundness today rests on the conservative
allocator picker: anything that might escape uses `_tin_rc_alloc`
(SHARED=1 from the start), so escape-transition machinery is not yet
required. The runtime function is kept so a future tightening of the
analyzer can switch more allocations to the non-atomic fast path
without round-tripping through the runtime.

### Retain / release dispatch

```c
void _tin_retain(void *ptr);
void _tin_release(void *ptr);
```

Both functions read `flags` with acquire ordering, short-circuit on
IMMORTAL, then branch on SHARED:

- `SHARED=0`: plain `hdr->rc++` / `hdr->rc--`. Release sets UNIQUE when
  `rc` drops back to 1 so the next CoW site can mutate in place.
- `SHARED=1`: `__atomic_fetch_add` / `__atomic_fetch_sub` with
  `ACQ_REL`. UNIQUE is never set on this path (it would race).

`_tin_release` calls `free(hdr)` when the post-decrement `rc` reaches
zero, unless `TIN_RC_ARENA` is set (the storage belongs to an external
pool).

## The heap arena

```
TINMAXHEAP=64G  -> reserve 64 GiB virtual
TINMAXHEAP=2G   -> reserve 2 GiB
TINMAXHEAP=0    -> disable; _tin_is_managed returns 1 for any non-null
                   pointer (degraded mode, sanitizer builds)
```

`runtime/heap_arena.c` calls `mi_reserve_os_memory_ex` at constructor
priority 101 (before any other Tin constructor touches the allocator)
to register a contiguous virtual range with mimalloc. macOS aarch64
caps large reservations even with unlimited `ulimit -v`, so the helper
halves the requested size on failure down to 256 MiB before giving up.

The reservation is virtual only (`commit=false`); pages commit lazily
as mimalloc grows into them, and `madvise(MADV_DONTNEED)` returns RSS
to the OS as regions go cold.

### `_tin_is_managed`

```c
int _tin_is_managed(void *ptr);
```

Returns 1 when `ptr` is the public pointer of one of Tin's rc-blocks.
Two-stage check:

1. **Range.** `ptr` must fall inside `[arena_base, arena_base + arena_size)`.
   The arena is `exclusive=true`, so only heaps explicitly bound to it
   (the per-thread rc-heaps in this file) draw from it -- libc malloc,
   stack frames, rodata, extern returns, foreign mmap'd regions all
   land outside.
2. **Magic.** The would-be header at `ptr - sizeof(TinRCHdr)` must
   carry `TIN_RC_HDR_MAGIC` in its `_pad` slot. `_tin_rc_alloc` and
   friends stamp it at allocation. Interior pointers (e.g. `&arr[5]`
   into a fat array's data buffer) pass the range check but land
   inside block data; `_pad` there is random user bytes and the magic
   fails to match.

Codegen uses `_tin_is_managed` indirectly through `_tin_retain_ptr` /
`_tin_release_ptr` for every primitive `*T` retain/release site --
foreign pointers and interior pointers both fall out as safe no-ops,
and only legitimate block-start pointers actually touch the rc field.

### `_tin_retain_ptr` / `_tin_release_ptr`

```c
void _tin_retain_ptr(void *ptr);
void _tin_release_ptr(void *ptr);
```

Provenance-aware retain/release for bare pointer values whose source
is unknown at the call site (`*void`, `*i64`, raw extern returns).
Both short-circuit via `_tin_is_managed` before falling through to the
biased `_tin_retain` / `_tin_release` path. A pointer that came from
`mem::malloc`, an extern C call, or `&local_var` is outside the arena
and returns immediately - no header math, no free.

This is what lets Tin treat every pointer value as a uniformly
rc-tracked binding without breaking C interop: foreign pointers cost
one range check; Tin-arena pointers participate in ARC normally. The
data-pointer fast path (struct field walks, fat-string releases) stays
on the non-provenance entry points because callers there know the
pointer is Tin-managed by construction.

### mimalloc

The runtime is built with `-DTIN_USE_MIMALLOC=1` by default and links
against `libmimalloc`. `runtime/runtime.c` macro-substitutes every
allocator call in the Tin translation unit (`malloc`, `free`, `realloc`,
`calloc`, `strdup`, `strndup`) to `mi_*` after the system headers
declare them. The arena registration in `heap_arena.c` is what makes
those calls land in the segregated address range.

`--no-mimalloc` opts out of the link and the runtime falls through to
libc allocators. The arena is then a no-op (`TINMAXHEAP=0` semantics).

## See also

- [memory-arc-codegen.md](memory-arc-codegen.md) - ARC heap allocation
  and retain/release codegen rules.
- [memory-special-cases.md](memory-special-cases.md) - heap promotion,
  special-case releases.
- [arc-threads.md](arc-threads.md) - how ARC interacts with the fiber
  scheduler and the worker thread pool.
- [clayout-structs.md](clayout-structs.md) - cLayoutStruct wrapper
  layout at the FFI boundary.
