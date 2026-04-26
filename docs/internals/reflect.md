# Reflect - Type Atom Introspection

## Source location

`stdlib/reflect/reflect.c` - compiled alongside `reflect.tin` via:

```rust
//!+reflect.c -- -I ../../runtime
```

This means `reflect.c` is compiled only when a program `use reflect`. It links
against the runtime's `_tin_rc_alloc` (available because `runtime.c` is always
compiled into the same final binary).

---

## Atom string format

Every type in tin has a corresponding atom string. The compiler produces these
via `typeof()`, `fieldtypes()`, etc. The strings follow a recursive grammar:

```
atom    ::= primitive | pointer | array | fn_type | struct_name
primitive ::= "i8" | "i16" | "i32" | "i64" | "i128"
            | "u8" | "u16" | "u32" | "u64" | "u128"
            | "f32" | "f64" | "f128"
            | "bool" | "string" | "char" | "void"
            | "atom" | "byte"
pointer   ::= "*" atom
array     ::= "[" atom "]"
fn_type   ::= "fn(" params ")" atom
params    ::= ε | atom ("," atom)*
struct_name ::= identifier
```

**Simple atoms** (identifiers only, no special characters) are stored bare:
`i64`, `bool`, `point`.

**Complex atoms** (containing `(`, `)`, `[`, `]`, `,`, `*`) are stored
wrapped in double-quotes: `"fn(i64,f64)bool"`, `"[i64]"`, `"*point"`.

The `atom_spec` helper strips the quotes before parsing:

```c
static const char *atom_spec(const char *atom, char *buf) {
    if (!atom || atom[0] != '"') return atom; // bare atom
    const char *s = atom + 1;                 // skip opening "
    size_t len = strlen(s);
    if (len > 0 && s[len-1] == '"') len--;    // strip trailing "
    if (len >= 256) len = 255;
    memcpy(buf, s, len);
    buf[len] = '\0';
    return buf;
}
```

The `buf` parameter is a caller-provided 256-byte stack buffer. The returned
pointer is either `atom` (no copy) or `buf` (stripped copy).

---

## `_tin_reflect_kind`

```c
const char *_tin_reflect_kind(const char *atom);
```

Returns one of six immortal string constants by examining the first character
of the type spec:

| First char / prefix | Return value  |
|---------------------|---------------|
| `*`                 | `"ptr"`       |
| `[`                 | `"array"`     |
| `fn(`               | `"fn"`        |
| primitive name      | `"primitive"` |
| anything else       | `"struct"`    |

The return values point into static `{ int64_t rc; char data[12]; }` structs
with `rc = TIN_IMMORTAL_RC`, so they are never freed.

---

## `_tin_reflect_elem`

```c
const char *_tin_reflect_elem(const char *atom);
```

Extracts the element type of pointer and array atoms:

- `"*T"` -> allocates and returns `"T"`.
- `"[T]"` -> allocates and returns `"T"` (strips leading `[` and trailing `]`).
- Anything else -> returns the immortal empty string `""`.

Returned strings (for the first two cases) are ARC-allocated via
`_tin_rc_alloc` with `rc = 1`. The caller (or compiler-emitted release) is
responsible for releasing them.

---

## Parsing function types

Function atoms like `"fn(i64,fn(bool)string)f64"` require careful parsing
because parameter types can themselves be function types (containing nested
parentheses). The helper `_reflect_find_params_end` handles this:

```c
static const char *_reflect_find_params_end(const char *open_paren) {
    const char *p = open_paren;
    int depth = 0;
    while (*p) {
        if      (*p == '(') depth++;
        else if (*p == ')') { if (--depth == 0) return p + 1; }
        p++;
    }
    return NULL;
}
```

Given a pointer to the `(` that opens the parameter list, it returns a pointer
to the character immediately after the matching `)`. This is used by
`_tin_reflect_fn_ret` to locate the return type.

All other fn-parsing functions (`fn_arity`, `fn_param`, `fn_params`) track
parenthesis depth manually while scanning for top-level commas:

```c
while (*p && !(*p == ')' && depth == 0)) {
    if      (*p == '(') depth++;
    else if (*p == ')') depth--;
    else if (*p == ',' && depth == 0) { /* found a param boundary */ }
    p++;
}
```

---

## `_tin_reflect_fn_params` - memory layout

`fn_params` is the most complex function in the reflect module. It returns a
`TinStringArray` whose backing storage is a **single ARC block** containing:

1. An array of `arity` `TinString` structs.
2. Followed by `arity` **immortal records**, each containing:
   - An `int64_t` ARC header (`rc = -1`, immortal).
   - The parameter name string data (`plen` bytes + `\0`).
   - Padding to the next 8-byte boundary.

```
ARC block (rc=1):
  ┌────────────────────────────┐
  │  TinString[0]              │  <- ptr points into record[0].data
  │  TinString[1]              │     len = strlen(param[1])
  │  ...                       │
  │  TinString[arity-1]        │
  ├────────────────────────────┤
  │  record[0]:                │
  │    int64_t rc = -1         │  <- immortal sentinel
  │    char data[plen+1]       │  <- TinString[0].ptr points here
  │    padding to 8-byte align │
  ├────────────────────────────┤
  │  record[1]: ...            │
  │  ...                       │
  └────────────────────────────┘
```

Each `TinString.ptr` points into its corresponding immortal record's `data`
field. Because the records are immortal (`rc = -1`), `_tin_retain` and
`_tin_release` on the individual string pointers are no-ops. Releasing the
outer ARC block (the `TinStringArray.ptr`) frees everything in one `free`
call.

This layout gives callers simple ownership semantics: hold one reference to
the array, read the strings freely, release the array when done.

The maximum number of parameters is capped at `MAX_PARAMS = 64`.

---

## Primitive name table

```c
static const char *_tin_primitives[] = {
    "i8","i16","i32","i64","i128",
    "u8","u16","u32","u64","u128",
    "f32","f64","f128",
    "bool","string","char","void","atom","byte",NULL
};
```

`_tin_is_primitive_name` performs a linear scan of this table. It is `static`
and used only within `reflect.c`.

---

## Reflect functions summary

| Function                    | Input       | Output                                                    | Allocates?                 |
|-----------------------------|-------------|-----------------------------------------------------------|----------------------------|
| `_tin_reflect_kind`         | atom        | `"ptr"` / `"array"` / `"fn"` / `"primitive"` / `"struct"` | No (immortal constant)     |
| `_tin_reflect_is_ptr`       | atom        | `0` or `1`                                                | No                         |
| `_tin_reflect_is_array`     | atom        | `0` or `1`                                                | No                         |
| `_tin_reflect_is_fn`        | atom        | `0` or `1`                                                | No                         |
| `_tin_reflect_is_primitive` | atom        | `0` or `1`                                                | No                         |
| `_tin_reflect_elem`         | atom        | inner type string                                         | Yes (ARC, caller releases) |
| `_tin_reflect_fn_ret`       | atom        | return type string                                        | Yes (ARC, caller releases) |
| `_tin_reflect_fn_arity`     | atom        | `int64_t` count                                           | No                         |
| `_tin_reflect_fn_param`     | atom, index | parameter type string                                     | Yes (ARC, caller releases) |
| `_tin_reflect_fn_params`    | atom        | `TinStringArray`                                          | Yes (one ARC block)        |
