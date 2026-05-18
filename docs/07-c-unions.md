# Native C unions


`union` maps directly to a C union - all variants share the same memory with
no tag. Use this for FFI or when you need to reinterpret bytes.

**Memory layout:** a single `[maxSize x i8]` byte array.

### Unnamed union

```rust
union raw = i32 | i64
```

Access via type cast:

```rust
let r raw = 42
let v i32 = r.(i32)   // read storage as i32
let w i64 = r.(i64)   // read storage as i64
```

### Named union

```rust
union color = as_i32 i32 | as_r u8
```

Access via field name or type cast:

```rust
let c color = 255
let v i32 = c.as_i32   // read as i32
let b u8  = c.as_r     // read same bytes as u8
```

> Native unions have no tag - reading from the "wrong" field reinterprets
> bytes silently. Use tagged unions (`type x = T | U`) for safe sum types.

---

