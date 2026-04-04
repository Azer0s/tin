# Atoms

Atoms are lightweight, globally unique symbolic values  -  similar to atoms in Erlang/Elixir or
symbols in Ruby/Lisp. They are written with a leading apostrophe:

```rust
let status = 'ok
let kind   = 'error
let tag    = '"fn(i64)bool"   // quoted form for names with special characters
```

## Syntax

| Form                      | Example          | Atom name     |
|---------------------------|------------------|---------------|
| Simple                    | `'ok`            | `ok`          |
| Quoted (plain identifier) | `'"Point"`       | `Point`       |
| Quoted (complex)          | `'"fn(i64)bool"` | `fn(i64)bool` |

The quoted form is used when the atom name contains characters that are not letters, digits, or
underscores. If the quoted content is a plain identifier, the surrounding double-quotes are
stripped and the atom is equivalent to its unquoted form: `'"hello"` == `'hello`.

## Internal representation

Every atom is compiled to a named LLVM struct type:

```llvm
%__atom = type { i32 }
```

The single `i32` field holds the **CRC32** of the atom's name, computed at compile time using
`crc32.ChecksumIEEE`. This makes atom comparisons a single integer comparison rather than a
string comparison.

### Collision resolution

If two distinct atom names produce the same CRC32, the compiler increments the code by one
until a free slot is found:

```
code = CRC32(name)
while code already used by a different name:
    code++
```

This is detected and resolved entirely at compile time.

## Atom table

After all source atoms are registered, the compiler emits two globals:

```llvm
@__tin_atom_table  = constant [N x { i32, i8* }]  ; array of (code, string) pairs
@__tin_atom_count  = constant i64 N
```

Each entry stores the CRC32 code and a pointer to the atom's bare name string (without
the leading apostrophe). For example, the atom `'ok` is stored as the C string `"ok"`.

## Atom-to-string conversion

The compiler generates `__tin_atom_to_string` which searches first the static compile-time
table, then the runtime learned-atom table:

```llvm
define { i8*, i64 } @__tin_atom_to_string(i32 %code) {
  ; 1. loop over @__tin_atom_table until table[i].code == %code
  ; 2. if not found, call _tin_rt_atom_to_str(%code)  -  runtime learned table
  ; returns {ptr, strlen(ptr)} string fat-pointer, or {null, 0} if unknown
}
```

The returned string is the bare atom name. For example, `'ok` converts to `"ok"`.

When `echo` prints an atom, it prints the bare name: `echo 'ok` outputs `ok`.

## String-to-atom conversion

The compiler also generates `__tin_string_to_atom` for the reverse direction:

```llvm
define %__atom @__tin_string_to_atom(i8* %ptr) {
  ; 1. loop over @__tin_atom_table comparing via strcmp
  ; 2. if not found, call _tin_learn_atom(%ptr)  -  computes CRC32 at runtime,
  ;    stores in the runtime linked-list table, returns the new code
}
```

The input string is a bare atom name (no apostrophe). For example, calling
`__tin_string_to_atom("ok")` returns the same atom as the compile-time `'ok`.

This means extern functions that return strings can be used directly as atoms without
needing to prepend `'`.

## Runtime learned-atom table

The runtime maintains an **unbounded linked list** of atoms learned at runtime
(atoms whose names were not known at compile time). The list is protected by a mutex
for thread safety.

C API (in `runtime/runtime.c`):

| Function                            | Description                                                 |
|-------------------------------------|-------------------------------------------------------------|
| `_tin_learn_atom(const char*)`      | Search list; if absent compute CRC32, add node, return code |
| `_tin_rt_atom_to_str(int32_t code)` | Return the string for a runtime-learned code, or NULL       |

Each node allocates its string as an immortal ARC block (`_tin_rc_alloc` with
`rc = TIN_IMMORTAL_RC`) so that `_tin_retain`/`_tin_release` on the string are
safe no-ops. The node stores both `hdr` (the head of the `malloc` block) and
`str` (`hdr + 1`, the usable string data). Keeping the head pointer in the
node ensures valgrind can see a pointer to the start of each allocation and
classifies the atoms as "still reachable" rather than "possibly lost" at
program exit.

## Extern conversion

When an atom value is passed to an `extern` (C) function, the compiler automatically calls
`__tin_atom_to_string` and passes the resulting `i8*` (the bare atom name) to the C function:

```rust
fn print_atom(a atom) = extern("my_c_func")
// Tin calls: my_c_func(__tin_atom_to_string(a.code).ptr)
```

When a C function returns a `char*` and the Tin declaration says it returns `atom`, the
compiler calls `__tin_string_to_atom` to convert back (no apostrophe required in the C string).

## `= extern()` syntax

Inline extern declarations use a string literal to specify the C symbol name:

```rust
fn abs(x i64) i64    = extern("labs")
fn sqrt(x f64) f64   = extern("sqrt")
fn kind(t atom) string = extern("_tin_reflect_kind")
```

The `use extern {}` block form still uses identifiers (optionally with an override string):

```rust
use extern {
    malloc("my_malloc") as fn(i64) *void
    free as fn(*void)
}
```

## Equality rules

| Left     | Right    | Operation                               |
|----------|----------|-----------------------------------------|
| `atom`   | `atom`   | `icmp eq` on the two i32 codes  -  O(1) |
| `atom`   | `string` | convert atom to string, then `strcmp`   |
| `string` | `atom`   | convert atom to string, then `strcmp`   |

## ARC (automatic reference counting)

`%__atom` is **not** ARC-tracked. Atoms are plain `i32` wrappers with no heap allocation,
so they never need retain/release calls.

## Reflection

The `typeof`, `traitof`, `fieldnames`, and `fieldtypes` builtins return atoms. For example:

```rust
let x i64 = 42
echo typeof(x)         // i64
let r = rect { w: 10, h: 20 }
echo typeof(r)         // rect
echo fieldnames(r)[0]  // w
```
