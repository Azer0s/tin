# Atoms - Compile-Time and Runtime

## What is an atom?

An atom is an interned symbol - a value that represents a name or type tag.
In tin, atoms are written with a leading apostrophe: `'ok`, `'error`,
`'i64`. Under the hood, an atom is a 32-bit integer code derived from the
atom's string name. Comparing two atoms is an integer comparison, not a string
comparison.

Atoms are used for:
- Enum-style discriminants (`'ok`, `'err`).
- Type names returned by `typeof()` and used by the `reflect` package.
- Keys in the `any` type's tag field (via the tag integer).

---

## Compile-time atom table

The compiler (`codegen/atoms.go`) assigns a code to every atom that appears
in the source at compile time. At the end of code generation it emits two
globals into the LLVM IR:

```llvm
@__tin_atom_table = private constant [N x { i32, i8* }] [
    { i32 <code>, i8* @.str_<name> },
    ...
]
@__tin_atom_count = private constant i64 N
```

Each entry is a `{ code, pointer-to-name-string }` pair.

Two generated functions provide the two-way mapping:

### `__tin_atom_to_string(code) -> TinString`

Linear search of `@__tin_atom_table` for a matching `code`. On a hit, returns
a `TinString` wrapping the corresponding name string. On a miss (runtime-learned
atom), falls through to `_tin_rt_atom_to_str` (see below).

### `__tin_string_to_atom(ptr) -> atom`

Linear search of `@__tin_atom_table` for an entry whose name string matches
`ptr` (compared with `strcmp`). On a hit, returns the code as an atom value.
On a miss, calls `_tin_learn_atom` to register the string at runtime and
returns the resulting code.

---

## Code assignment and collision resolution

The compiler uses CRC32 to assign codes. The same algorithm is used in both
the compiler (Go) and the runtime C code so that codes are consistent:

```
code = crc32(atom_name_string)
while any existing atom has the same code but a different name:
    code++
```

The increment-until-free approach is a simple open-addressing collision
resolution. In practice, atom names are short and collisions are rare, so the
loop almost never runs more than once.

The compiler records the mapping in `atomCodes` and `atomCodeToName` maps to
detect and resolve collisions at build time.

---

## Runtime atom table (`atom.c`)

Atoms created at runtime - for example, from a value constructed dynamically
via reflection - need to be registered so they can be converted back to
strings. The runtime maintains a thread-safe linked list for this purpose.

```c
typedef struct TinRtAtomNode {
    int32_t  code;
    char    *str;    // strdup'd copy
    struct TinRtAtomNode *next;
} TinRtAtomNode;

static TinRtAtomNode *_tin_rt_atom_head = NULL;
static pthread_mutex_t _tin_rt_atom_mu = PTHREAD_MUTEX_INITIALIZER;
```

### `_tin_learn_atom(str) -> int32_t code`

Registers a new atom string and returns its code:

1. Acquires `_tin_rt_atom_mu`.
2. Walks the list: if `str` is already registered, returns its code.
3. Computes `code = crc32(str)`.
4. Increments `code` until no existing entry has that code with a different
   name (same collision resolution as compile time).
5. Allocates a new `TinRtAtomNode` with `strdup(str)` and prepends it to the
   list (O(1) insertion).
6. Releases the mutex and returns `code`.

Prepending keeps insertion O(1) at the cost of O(n) lookup - acceptable
because the runtime atom table is expected to be small.

### `_tin_rt_atom_to_str(code) -> const char*`

Linear search of the linked list for a matching code. Returns the `str`
pointer (owned by the node), or `NULL` if not found. Thread-safe via mutex.

---

## CRC32 implementation

The same CRC32 algorithm is used by both the compiler (Go) and `atom.c` (C).
The C implementation processes one byte at a time with bit-reverse polynomial
`0xEDB88320` (Castagnoli variant, same as Go's `crc32.IEEE`):

```c
static uint32_t _tin_crc32_str(const char *str) {
    uint32_t crc = 0xFFFFFFFFu;
    while (*str) {
        crc ^= (unsigned char)*str++;
        for (int k = 0; k < 8; k++)
            crc = (crc >> 1) ^ (0xEDB88320u & (uint32_t)(-(int32_t)(crc & 1u)));
    }
    return ~crc;
}
```

This function is `static` and used only within `atom.c`. It is not declared in
`runtime.h`.

---

## Atom string encoding for type atoms

Type atoms produced by `typeof()` follow a structured string format (see
[reflect.md](reflect.md) for parsing details):

| Example atom string | Meaning                                                          |
|---------------------|------------------------------------------------------------------|
| `i64`               | primitive type `i64`                                             |
| `*point`            | pointer to `point`                                               |
| `[i64]`             | array of `i64`                                                   |
| `"fn(i64,f64)bool"` | function `(i64, f64) -> bool` (quoted because it contains parens) |

Simple atoms (identifiers only) are stored bare. Complex atoms whose names
contain characters that are not valid in identifiers are stored with surrounding
double-quotes preserved. The `atom_spec` helper in `reflect.c` strips these
quotes before parsing.
