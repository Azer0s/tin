# C interop matrix

A reference of every shape Tin <-> C interop must support, organized by
direction. Each row corresponds to one test fixture under
`examples/c_interop_matrix/`. The matrix exists so a regression in
one corner of the boundary doesn't go unnoticed - every supported
pattern has a runnable proof.

Direction conventions:
- **Tin -> C**: Tin code calls a C library function via `extern("...")`.
  The Tin program owns the lifecycle (`tin run`, `tin test`, `tin build`).
- **C -> Tin** (`#interop`): C code calls a Tin function exported via
  `fn{#interop}`. Tin is built with `tin build --lib` and the C program
  drives.

## ABI / layout shapes that determine behavior

| Shape | Where it lives | What changes |
|-------|---------------|--------------|
| Scalar <=8B | Single register (RDI/RSI/... or x0/x1/...) | None |
| Scalar > 8B, <=16B | Two registers, by SysV class | None |
| Struct <=16B by value | Two registers, all-integer / packed | Trivial coerce |
| Struct > 16B by value | Memory: x86_64 SysV uses `byval`, AAPCS64 uses indirect pointer | Codegen needs explicit byval/sret matching clang |
| Struct return > 16B | Hidden first param: `sret` on both ABIs | Same |
| Fat pointer (`string`, `[T]`) | `{ptr, len, cap}` 24B internally | Lowered to `*T` on extern boundary |
| HFA (homogeneous float aggregate, 1-4 floats) | VFP registers on AAPCS64 | No byval; passed in d0-d3 |

---

## Tin -> C: every supported pattern

### Scalars

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T1  | `fn f(x i32) i32`                 | `int32_t f(int32_t)`                 | Baseline |
| T2  | `fn f(x f64, y f64) f64`          | `double f(double, double)`           | SSE registers |
| T3  | `fn f(b bool) bool`               | `_Bool f(_Bool)`                     | i1 <-> i8 boundary trunc/zext |
| T4  | `fn f(out *i64)`                  | `void f(int64_t *)`                  | Out-param via pointer |

### Strings

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T5  | `fn f(s string) i64`              | `int64_t f(const char *)`            | string lowered to data ptr |
| T6  | `fn f() string`                   | `const char *f(void)` (static)       | `tin_interop_str_in` copies on return |
| T7  | `fn{#handover} f() string`        | `char *f(void)` (malloc'd)           | Tin takes ownership; frees source |
| T8  | `fn f(s string) string`           | `const char *f(const char *)`        | round-trip; Tin copies the return |

### Atoms

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T9  | `fn f(a atom) i64`                | `int64_t f(const char *)`            | atom lowered to interned name |
| T10 | `fn f() atom`                     | `const char *f(void)`                | C name interned to atom code |

### Arrays of primitives

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T11 | `fn f(xs [i32], n i64) i64`       | `int64_t f(const int32_t *, int64_t)`| Tin passes data ptr; user provides length |
| T12 | `fn f(xs [f64], n i64)`           | `void f(double *, int64_t)`          | C may mutate; shared buffer visible |
| T13 | `fn f(buf [byte; 8]) i64`         | `int64_t f(const uint8_t *)`         | Fixed-size array decays to pointer |

### Arrays of strings / atoms

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T14 | `fn f(xs [string], n i64) i64`    | `int64_t f(const char **, int64_t)`  | Inline marshal: stack alloca of `char*[n]` |
| T15 | `fn f(xs [atom], n i64) i64`      | `int64_t f(const char **, int64_t)`  | Same; atom->name lookup per element |

### Structs

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T16 | small struct <=16B by value        | `T f(T)`                             | Register coerce |
| T17 | large struct >16B by value        | `T f(T)`                             | byval / sret ABI (see ABI shim) |
| T18 | `fn f(p *T)` then read fields     | `void f(T *)`                        | Live view via c_data_ptr |
| T19 | `fn make() *T`                    | `T *make(void)`                      | Non-handover: borrow; mutations visible |
| T19h| `fn{#handover} make() *T`         | `T *make(void)`                      | Tin RC-ifies; frees C-side |
| T20 | nested struct param               | `T f(Outer{Inner ...})`              | Recursive native lowering |
| T21 | `struct{#packed}` <=8B by value    | `__attribute__((packed)) T`          | Coerces to integer register |
| T22 | `struct{#packed}` 9-16B by value  | same                                 | Two-eightbyte split |

### Callbacks

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T23 | `fn f(cb fn(i64) i64) i64`        | `int64_t f(int64_t (*)(int64_t))`    | Tin fat-fn-ptr -> trampoline |
| T24 | `fn f(cb fn(string) i64) i64`     | `int64_t f(int64_t (*)(const char*))`| String marshal in dispatcher |
| T25 | callback with captured Tin local  | same                                 | env block trampoline |
| T26 | C stores cb, invokes later        | `void store_cb(...); int call_later(...)` | Trampoline outlives caller fn |
| T27 | NULL callback                     | C param accepts NULL                 | Tin passes nil; C must guard |

### Variadic

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T28 | `printf("%d", x)`                 | `int printf(const char *, ...)`      | i32 in vararg slot |
| T29 | `printf("%s", s)`                 | same                                 | string -> data ptr in vararg |

### Pointers / handles

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T30 | `*void` handle round-trip         | `void *make(); use(void *); free(void *);` | Opaque, no codegen interpretation |
| T31 | `**T` double-indirect out-param   | `void f(T **out)`                    | Nested deref |

### Address-of-local struct -> C

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T32 | `fn f(p *T)` with `&local_struct` | `void f(const T *)`                  | Tin allocation has type_id+vtable header; `adaptTinPtrToNativePtr` GEPs to user-fields region |
| T33 | same; C mutates through pointer   | `void f(T *)`                        | Mutations land in caller's Tin storage; out-param semantics work for any ABI-compat struct |

### Mixed-parameter shapes

| ID  | Tin signature                     | C signature                          | Notes |
|-----|-----------------------------------|--------------------------------------|-------|
| T34 | `f(p *T, n i64, s string)`        | `int64_t f(const T *, int64_t, const char *)` | Three different lowerings in one call: pointer adapter + integer reg + string |
| T35 | `f(cb fn, s string, p *T, xs [i64], n i64, k i64)` | `int64_t f(int64_t (*)(int64_t), const char *, const T *, const int64_t *, int64_t, int64_t)` | Five lowerings in one call (callback trampoline, string, struct ptr, fat-array, scalars) |
| T36 | `f(s string, n i64) T`            | `T f(const char *, int64_t)`         | Mixed inputs, struct-by-value return (sret on >16B path) |
| T37 | `f(xs [i64], n i64, p T)`         | `int64_t f(const int64_t *, int64_t, T)` | Array + large struct by value (byval) in the same signature |

---

## C -> Tin (`#interop`): every supported pattern

### Scalars

| ID  | Tin signature                          | C signature                          | Notes |
|-----|----------------------------------------|--------------------------------------|-------|
| C1  | `fn{#interop} add(a i32, b i32) i32`   | `int32_t add(int32_t, int32_t)`      | Baseline |

### Strings

| ID  | Tin signature                          | C signature                                          | Notes |
|-----|----------------------------------------|------------------------------------------------------|-------|
| C2  | `fn{#interop} greet(name string) string` | `const char *greet(const char *)`                  | Return allocated via `tin_extern_alloc`; C frees |

### Arrays

| ID  | Tin signature                          | C signature                          | Notes |
|-----|----------------------------------------|--------------------------------------|-------|
| C3  | `fn{#interop} sum(xs [i32]) i32`       | `int32_t sum(const int32_t *xs, int64_t xs_len)` | `[T]` splits into `(T*, i64)` |
| C4  | `fn{#interop} take() [i32]`            | `int take(int32_t **out, int64_t *out_len)` | Status + out-params |

### Callbacks

| ID  | Tin signature                                | C signature                                       | Notes |
|-----|----------------------------------------------|---------------------------------------------------|-------|
| C5  | `fn{#interop} apply(cb fn(i32) i32, n i32) i32` | `int32_t apply(int32_t (*cb)(int32_t), int32_t)` | C-fn -> Tin closure (thunk) |
| C6  | `fn{#interop} make_adder(b i64) fn(i64) i64` | `tin_cb_i64_from_i64_t make_adder(int64_t)`      | Tin returns closure to C |

### Pointers / opaque handles

| ID  | Tin signature                             | C signature                          | Notes |
|-----|-------------------------------------------|--------------------------------------|-------|
| C7  | `fn{#interop} make_pt(...) *point`        | `void *make_pt(...)`                 | Header renders `*MyStruct` as `void*`; C uses `tin_release` |
| C8  | `fn{#interop} sum(p packed_pt) i32`       | `int32_t sum(packed_pt)`             | `#packed` struct typedef'd in header |

### Struct in / out for #interop

End-to-end fixtures live under `examples/c_interop_matrix/c_to_tin/`.
Row IDs `C-A` ... `C-L` map to the corresponding `c_<letter>_*`
function in `lib.tin`; the C driver in `driver.c` prints one tagged
line per row and the runner test asserts each.

| ID  | Tin signature                                | C signature                          | Notes |
|-----|----------------------------------------------|--------------------------------------|-------|
| C-E | `fn{#interop} f(p packed_pt) i32`            | `int32_t f(packed_pt)`               | `#packed` struct by value IN -- single integer-register coerce |
| C-F | `fn{#interop} f(x i32, y i32) packed_pt`     | `packed_pt f(int32_t, int32_t)`      | `#packed` struct by value OUT -- same coerce on return |
| C-G | `fn{#interop} f(p *T) i64`                   | `int64_t f(void *)`                  | Struct pointer IN -- header renders `*T` as `void*`; wrapper converts to Tin layout via `emitStructPtrBorrow` |
| C-H | `fn{#interop} f(p *T, x i64)`                | `void f(void *, int64_t)`            | Same; Tin writes through pointer; C reads changes back |
| C-I | `fn{#interop} f(a i64, b i64) *T`            | `void *f(int64_t, int64_t)`          | Struct pointer OUT -- C gets opaque handle; must `tin_release` |
| C-J | `fn{#interop} f(s string, n i32, p *T, xs [i32]) i64` | `int64_t f(const char *, int32_t, void *, const int32_t *, int64_t)` | Mixed: string + scalar + struct ptr + array |
| C-K | `fn{#interop} f(cb fn, p packed_pt, s string) i32` | `int32_t f(int32_t (*)(int32_t), packed_pt, const char *)` | Mixed: callback + packed value + string |
| C-L | `fn{#interop} f(s string, k i32, cb fn) *T`  | `void *f(const char *, int32_t, int32_t (*)(int32_t))` | Mixed inputs returning struct pointer |

### Lifecycle

| ID  | Pattern                                  | Notes |
|-----|------------------------------------------|-------|
| C9  | `tin_runtime_init()` / `tin_runtime_shutdown()` | Idempotent; atexit-registered teardown |
| C10 | `#interop` call from non-Tin pthread     | Each thread inits scheduler state lazily |

---

## Cross-cutting / weird combos

| ID  | Pattern                                                            |
|-----|--------------------------------------------------------------------|
| X1  | C makes thing -> passes to Tin -> Tin stores callback -> C invokes later |
| X2  | Tin defines closure, passes to C, C-driver main returns, atexit    |
| X4  | C calls Tin which calls C which calls Tin (re-entry)               |
| X5  | Multiple Tin closures returned to C, each captures different env   |
| X6  | `#handover` on a malloc'd struct pointer                           |

---

## NOT SUPPORTED (or rejected at compile time)

The interop-validation pass produces specific errors for these; the
matrix lists them to make the boundary explicit.

| ID  | Pattern                                                 | Why |
|-----|---------------------------------------------------------|-----|
| X3  | `fn{#interop} f(xs [string])`                          | ARC-managed elements; the inbound wrapper would have to RC-track each element and run their destructors at boundary exit. v1 doesn't implement element-aware marshaling. Workaround: pass `[u8]` for raw bytes, or have the C side build the array post-call. |
| -   | `fn{#interop} f(x any)`                                | `any` is a tagged union with a dynamic dispatch table; C has no way to construct one. |
| -   | `fn{#interop, #async} f(...)`                          | C has no driver for a coroutine. |
| -   | `fn{#interop} f() Future[T]`                           | Same. |
| -   | Generic `fn{#interop} f[T](...)`                       | No monomorphization at the C boundary - C is non-generic. |
| -   | `fn{#interop} f(xs [[i32]])`                           | Nested fat array; inner element type still needs the element marshal X3 was rejected for. |
| -   | Callback param/return with non-primitive aggregate     | The thunk would have to marshal per call across the boundary; v1 only handles primitives + string + bool inside callback signatures. |
| -   | Naturally-aligned `#packed` struct > 8 bytes via the multi-eightbyte clang coercion | v1 doesn't implement clang's content-dependent register classification for this case. Workaround: pass via `*void`. |
| -   | Non-`#packed` user struct param/return                 | Carries trait vtable pointers and padding with no stable C representation. |
| -   | `main` reserved                                        | The wrapper name `main` would collide with C's entry point. |
| -   | `tin_runtime_init` / `__tin_interop_` prefix reserved | Internal namespace. |

---

## Test layout

Both directions live under `examples/c_interop_matrix/`:

- `tin_to_c/matrix.tin` + `matrix.c` -- one Tin test per row (`T1` ...
  `T37`). `./tin test examples/c_interop_matrix/tin_to_c/matrix.tin`
  builds, links, and runs every row.
- `c_to_tin/lib.tin` + `driver.c` -- Tin `#interop` library plus a C
  driver that calls each `c_<letter>_*` function. The runner test
  `examples/c_interop_matrix_c_to_tin_test.tin` builds the lib with
  `tin build --lib`, links with `driver.c` and the runtime, runs the
  driver, and asserts on its tagged output lines.

Both directions get a `--leaks` / `--valgrind` pass in CI to catch
regressions in lifetime contracts. Test code that allocates on the
C side must release; the matrix's per-row fixture is a worked
example of correct lifetime handling.
