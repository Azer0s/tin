# Function-alias traits


`trait X as fn(params) RetType` declares a trait whose sole requirement is
that the struct supplies one method with the matching signature. The method
must be named after the trait:

```rust
trait display as fn() string
```

A struct implementing `display` must define `fn display(this S) string`:

```rust
struct point(display) =
  x i64
  y i64

  fn ::display(this point) string =
    return "({this.x}, {this.y})"
```

Usage:

```rust
fn show(d display) =
  echo d.display()

let p = point{x: 3, y: 7}
show(p)     // (3, 7)
```

This matches the spec's `trait print as fn() [char]` pattern.

---

