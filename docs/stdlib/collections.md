# Collections

`stdlib/collections` provides generic collection types built on ARC-managed
storage. All types are memory-safe and require no manual `free` calls.

```rust
use collections
```

## Traits

### `collections::List[T]`

Interface for ordered, indexed sequences.

```rust
trait List[T] =
  fn len(this List[T]) i64
  fn get(this List[T], i i64) T
  fn push_back(this *List[T], v T)
  fn push_front(this *List[T], v T)
  fn pop_front(this *List[T]) T
  fn pop_back(this *List[T]) T
```

### `collections::Map[K, V]`

Interface for key-value stores.

```rust
trait Map[K, V] =
  fn len(this Map[K, V]) i64
  fn get(this Map[K, V], k K) V
  fn get_or(this Map[K, V], k K, default_val V) V
  fn has(this Map[K, V], k K) bool
  fn set(this *Map[K, V], k K, v V)
  fn delete(this *Map[K, V], k K)
  fn keys(this Map[K, V]) [K]
  fn values(this Map[K, V]) [V]
```

---

## `collections::LinkedList[T]`

A doubly-linked list implementing `List[T]`. Nodes are ARC-managed heap
objects. The forward chain (`next`) holds strong references; back-pointers
(`prev`) are weak to avoid reference cycles.

**Complexity:** `push_back`, `push_front`, `pop_front` are O(1). `pop_back`
and `get` are O(n).

### Construction

```rust
let l = collections::LinkedList[i64].new()
```

### Methods

| Method | Signature | Description |
|--------|-----------|-------------|
| `new` | `() LinkedList[T]` | Create an empty list |
| `len` | `(this LinkedList[T]) i64` | Number of elements |
| `get` | `(this LinkedList[T], i i64) T` | Element at index `i` (no bounds check) |
| `push_back` | `(this *LinkedList[T], v T)` | Append to the end |
| `push_front` | `(this *LinkedList[T], v T)` | Prepend to the front |
| `pop_front` | `(this *LinkedList[T]) T` | Remove and return the first element |
| `pop_back` | `(this *LinkedList[T]) T` | Remove and return the last element |

### Example

```rust
use collections

let l = collections::LinkedList[string].new()
l.push_back("a")
l.push_back("b")
l.push_front("z")
echo l.get(0)       // z
echo l.pop_front()  // z
echo l.len()        // 2
```

---

## `collections::HashMap[K, V]`

An open-addressing hash map implementing `Map[K, V]`. Keys are compared by
their string representation (`"{k}"`), supporting primitives, strings, and
structs. Uses xxHash3 internally.

**Load factor:** resizes at 75% capacity. **Minimum capacity:** 8 (rounded up
to the next power of 2).

### Construction

```rust
let m = collections::HashMap[string, i64].new(initial_cap)
```

`initial_cap` is rounded up to the nearest power of 2, minimum 8.

### Methods

| Method | Signature | Description |
|--------|-----------|-------------|
| `new` | `(initial_cap i64) HashMap[K, V]` | Create with initial capacity |
| `len` | `(this HashMap[K, V]) i64` | Number of entries |
| `get` | `(this HashMap[K, V], k K) V` | Value for key, or zero value if absent |
| `get_or` | `(this HashMap[K, V], k K, default_val V) V` | Value for key, or `default_val` |
| `has` | `(this HashMap[K, V], k K) bool` | Whether key exists |
| `set` | `(this *HashMap[K, V], k K, v V)` | Insert or overwrite entry |
| `delete` | `(this *HashMap[K, V], k K)` | Remove entry (no-op if absent) |
| `keys` | `(this HashMap[K, V]) [K]` | All keys (unordered) |
| `values` | `(this HashMap[K, V]) [V]` | All values (unordered) |

### Example

```rust
use collections

let m = collections::HashMap[string, i64].new(8)
m.set("apples", 3)
m.set("bananas", 5)
echo m.get("apples")           // 3
echo m.get_or("grapes", 0)     // 0
echo m.has("bananas")          // true
m.delete("bananas")
echo m.len()                   // 1

let ks = m.keys()
echo len(ks)                   // 1
```

### Integer keys

```rust
let m = collections::HashMap[i64, string].new(8)
m.set(1, "one")
m.set(42, "forty-two")
echo m.get(42)    // forty-two
echo m.get(99)    // (empty string - zero value)
```
