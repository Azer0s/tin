# Values - Strings, Slices, Echo, Len, Any

## `TinString` - fat-pointer strings

```c
typedef struct { const char *ptr; int64_t len; } TinString;
```

Tin strings are **fat pointers**: a data pointer and a byte length. They are
_not_ null-terminated (though the allocator usually writes a trailing `\0` as a
convenience). All string operations use the `len` field for bounds.

`ptr` may point into:
- A global constant (immortal, `rc = -1` in the 8 bytes before `ptr`).
- An ARC-managed heap block (allocated by `_tin_rc_alloc`).
- A plain `malloc` block (e.g. `_tin_str_concat` output, released by the caller).

---

## String operations (`strings.c`)

### `_tin_str_concat`

```c
TinString _tin_str_concat(TinString a, TinString b);
```

Allocates `a.len + b.len + 1` bytes via `_tin_rc_alloc` (ARC-managed, `rc = 1`),
copies both strings in, and returns a new `TinString`. The `+1` adds a
convenience null terminator; the returned `len` does not include it.

The caller owns one reference. Call `_tin_release(result.ptr)` when done.

Note: the compiler does not call `_tin_str_concat` directly - it inlines the
concatenation logic using `_tin_rc_alloc` and LLVM `memcpy` intrinsics. The C
function exists for use from extern C code that calls into the runtime.

### `_tin_str_from_cstr`

```c
TinString _tin_str_from_cstr(const char *s);
```

Wraps a null-terminated C string in a `TinString` with `len = strlen(s)`. No
copy is made; the pointer is used directly. Used internally by the runtime for
panic messages and other C-side string literals.

### `_tin_str_eq`

```c
int32_t _tin_str_eq(TinString a, TinString b);
```

Returns `1` if `a.len == b.len && memcmp(a.ptr, b.ptr, len) == 0`, else `0`.

---

## `TinSlice` - fat-pointer dynamic arrays

```c
typedef struct { void *ptr; int64_t len; } TinSlice;
```

Identical layout to `TinString`. The `ptr` field points to the element data;
`len` is the number of elements (not bytes). All slice operations take an
explicit `elem_size` argument for type-erased handling of arbitrary element
types.

`ptr` always points to an ARC-managed block (allocated via `_tin_rc_alloc`),
or is `NULL` for an empty slice.

---

## Slice operations (`slice.c`)

### `_tin_slice_append`

```c
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size);
```

Creates a new ARC block of `(s.len + 1) * elem_size` bytes, copies the
existing elements, appends one new element, and returns a new `TinSlice`.

The original slice is **not freed** - ARC handles it. The caller's old slice
variable will be released by the compiler at its next scope exit or
overwrite.

### `_tin_slice_concat`

```c
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size);
```

Allocates a new ARC block large enough for `a.len + b.len` elements, copies
both inputs, and returns the combined slice. Neither input is freed.

### `_tin_slice_idx`

```c
void *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size);
```

Bounds-checked element access. If `i` is outside `[0, s.len)`, prints a
diagnostic to stderr and calls `exit(1)`. Returns a pointer to the `i`-th
element within the slice's data block.

### `_tin_slice_subslice`

```c
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size);
```

Returns a copy of `s[start:]` as a new ARC-managed allocation (`rc = 1`).
Uses `_tin_rc_alloc` consistently with all other slice operations.

If `start >= s.len`, returns `{NULL, 0}` (empty slice).

---

## Echo and print (`echo.c`)

The `echo` statement in tin is lowered by the compiler to a call to one of
these type-specific functions, selected based on the static type of the
expression:

| Function              | Format                               |
|-----------------------|--------------------------------------|
| `_tin_echo_i64(v)`    | `"%lld\n"`                           |
| `_tin_echo_u64(v)`    | `"%llu\n"`                           |
| `_tin_echo_f64(v)`    | `"%g\n"` (suppresses trailing zeros) |
| `_tin_echo_bool(v)`   | `"true\n"` or `"false\n"`            |
| `_tin_echo_char(v)`   | `"%c\n"`                             |
| `_tin_echo_string(s)` | `"%.*s\n"` with bounded length       |

The `_tin_print_*` variants are identical but emit no trailing newline. They
are used for the individual pieces of string interpolation, followed by a
single `_tin_print_newline()` call at the end.

Using `%.*s` for strings means the output is correct even if the string data
contains embedded null bytes (rare in practice but possible when interfacing
with binary data).

---

## Length builtins (`len.c`)

```c
int64_t _tin_len_string(TinString s);
int64_t _tin_len_slice(TinSlice s);
```

Both simply return the `len` field of the fat pointer. The compiler selects
the appropriate variant based on the static type of the `len()` argument.

---

## `any` equality (`any.c`)

The `any` type is a tagged union:

```c
typedef struct { int32_t tag; void *ptr; } _TinAny;
```

`tag` is a small integer that encodes the dynamic type. `ptr` points to the
boxed value (on the stack or heap). The tag codes are:

| Tag  | Type                  | Comparison                                                          |
|------|-----------------------|---------------------------------------------------------------------|
| `0`  | `i64`                 | Dereference as `int64_t*`, compare values                           |
| `1`  | `f64`                 | Dereference as `double*`, compare values                            |
| `2`  | `string` / `atom`     | Dereference as `char**` (pointer to `TinString.ptr`), then `strcmp` |
| `3`  | `bool`                | Dereference as `uint8_t*`, compare values                           |
| else | pointer / struct / fn | Compare `ptr` pointers directly (identity)                          |

```c
int64_t _tin_any_eq(_TinAny a, _TinAny b);
```

Returns `1` if equal, `0` otherwise. If tags differ, always returns `0`
regardless of the pointed-to values.

The string/atom case (tag 2) uses `strcmp` on the underlying C string pointer
rather than a length-bounded comparison because `TinString.ptr` is always
null-terminated in practice and atoms are interned strings whose comparison
relies on identity.
