# Heap allocation, weak and own fields


Prefixing a struct literal with `&` allocates it on the heap and returns a
pointer to it:

```rust
struct node =
  value i64
  next  *node

fn make_node(v i64) *node =
  return &node{value: v, next: nil}

let n = make_node(42)   // n : *node
echo n.value            // 42
```

The allocated struct is ARC-tracked - the pointer is released automatically
when the variable goes out of scope. No `mem::free` is needed. The same applies
when returning the address of a named local (`return &x`): the compiler promotes
the variable to an ARC block at the return site and the caller's variable
releases it on scope exit.

--

## Weak and own fields

Tin uses ARC (automatic reference counting) for memory management. Two structs
that each hold a strong pointer to the other form a reference cycle: neither
can ever reach RC == 0, so both leak. The compiler detects cycles in the struct
field graph at compile time using Tarjan's SCC algorithm and enforces that
every cycle is annotated with the programmer's intent.

There are two field ownership modifiers:

| Modifier | RC behaviour | Cycle role |
|-----|-------|------|
| *(none)* - plain strong | retain on assign, release on free | Default owning reference |
| `weak` | no retain / no release | Non-owning back-reference; breaks ARC cycles |
| `own` | retain on assign, release on free (same as strong) | Owning tree-edge; declares the referenced data is acyclic at runtime |

--

### `weak` - non-owning back-references

Use `weak` when a field is a back-reference that must not keep the target
alive. The classic example is a doubly-linked list:

```rust
struct Node[T] =
  val  T
  next *Node[T]        // strong: owns the forward chain
  prev weak *Node[T]   // weak:   back-reference, does not own
```

`weak` goes directly before the field type. A weak field:

- Is **not** retained when assigned.
- Is **not** released when the struct is freed.
- The programmer is responsible for not letting the weak reference outlive
  the owning side.

--

### `own` - tree-ownership declaration

Use `own` when a struct contains children of the same type and the runtime
data is guaranteed to form a tree (no cycles). A common example is a parsed
AST or a JSON/YAML value tree:

```rust
struct Expr =
  kind  ExprKind
  left  own *Expr    // owns left child - promise: no cycles at runtime
  right own *Expr    // owns right child
```

`own` is semantically identical to a plain strong field at runtime: the field
is retained on assign and released on free. The only difference is at compile
time: the cycle checker accepts a cycle that contains at least one `own` edge
without requiring a corresponding `weak` edge.

**The programmer declares a contract:** fields marked `own` will never form a
runtime cycle. The compiler does not verify this - doing so would require
either a full ownership type system or an O(depth) walk on every assignment.
Violating the contract (e.g. `node.left own= node`) produces a memory leak,
exactly as manually constructing a strong-reference cycle would in any ARC
language.

> `own` is the programmer saying "I am not a morron."
>
> Future work: a debug-mode build option will add a runtime acyclicity check
> on `own` field assignments to catch contract violations during development.

--

### Compiler cycle detection rules

Every strongly-connected component that contains a cycle must satisfy:

| Cycle composition | Result |
|--|--|
| All plain strong, no `weak`, no `own` | **Error** - annotate intent |
| All `weak`, no strong, no `own` | **Error** - no owner; objects would be freed immediately |
| At least one `weak` (any number of strong) | **OK** - classic ARC cycle-breaking |
| At least one `own` (no `weak` needed) | **OK** - programmer declares acyclic |

```rust
// ERROR: mutual plain strong references - nobody breaks the cycle
struct Parent =
  child *Child

struct Child =
  parent *Parent

// OK: one strong owner, one weak back-reference
struct Parent =
  child *Child

struct Child =
  parent weak *Parent

// OK: self-referential tree type - own declares acyclic data
struct JsonValue =
  items own [*JsonValue]   // owns child values; data is always a tree
```

--

