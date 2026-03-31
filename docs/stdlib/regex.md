# 14 - Regular Expressions

Tin provides PCRE-based regular expressions through `stdlib/regex`.

> **Dependency:** The regex stdlib links against `libpcre`. Install it with
> `apt-get install libpcre3-dev` (Debian/Ubuntu) or `brew install pcre` (macOS).
> The `//!-lpcre` directive in the stdlib handles the linker flag automatically.

---

## Import

```rust
use regex
```

---

## Compiling a pattern

```rust
let re = regex::compile("^hello (\\w+)$")
if re.is_err():
  echo "compile error: {re.err_msg()}"
```

`compile` returns a `Regex` value. Always check `is_err()` before using it.
Patterns use standard PCRE syntax; remember to double-escape backslashes in
Tin string literals (`\\w`, `\\d`, `\\s`, etc.).

---

## Executing a match

```rust
let re = regex::compile("(\\w+)@(\\w+\\.\\w+)")
let m  = re.exec("send mail to user@example.com please")

if m.matched():
  echo m.group(0)   // "user@example.com"  - whole match
  echo m.group(1)   // "user"              - first capture group
  echo m.group(2)   // "example.com"       - second capture group
```

`exec` returns a `Match`. Methods on `Match`:

| Method         | Returns  | Description                                     |
|----------------|----------|-------------------------------------------------|
| `matched()`    | `bool`   | `true` if the pattern matched                   |
| `count()`      | `i64`    | number of capture groups (including group 0)    |
| `group(i i64)` | `string` | text of capture group `i`; `""` if out of range |

---

## Convenience functions

These one-shot functions compile and execute in a single call:

### `regex::matches`

Returns `true` if the pattern matches anywhere in `s`:

```rust
if regex::matches("^\\d+$", "12345"):
  echo "all digits"
```

### `regex::find`

Returns the first match, or `""` if there is no match:

```rust
let first = regex::find("\\d+", "abc 123 def 456")
echo first   // "123"
```

### `regex::find_all`

Returns all non-overlapping matches as an array of strings:

```rust
let nums = regex::find_all("\\d+", "a1 b22 c333")
// nums: ["1", "22", "333"]
for let n string in nums:
  echo n
```

### `regex::replace`

Replaces the **first** occurrence of the pattern with `repl`:

```rust
let result = regex::replace("\\s+", "hello   world", " ")
echo result   // "hello world"
```

### `regex::replace_all`

Replaces **all** non-overlapping occurrences:

```rust
let result = regex::replace_all("\\d", "a1b2c3", "*")
echo result   // "a*b*c*"
```

### `regex::split`

Splits `s` on every match of the pattern:

```rust
let parts = regex::split(",\\s*", "one, two,three,  four")
// parts: ["one", "two", "three", "four"]
```

---

## Compiled `Regex` methods

When the same pattern is used many times, compile it once and reuse the
`Regex` value:

```rust
let re = regex::compile("\\d+")
if re.is_err():
  return

let m1 = re.exec("price: 42")
let m2 = re.exec("count: 100")
let all = re.find_all("a1 b22 c333")
```

| Method | Signature | Description |
|--------|-----------|-------------|
| `is_err()` | `() bool` | `true` if compilation failed |
| `err_msg()` | `() string` | error message from PCRE |
| `exec(s)` | `(string) Match` | match against `s` starting at offset 0 |
| `match_at(s, start)` | `(string, i64) Match` | match starting at byte offset `start` |
| `find_all(s)` | `(string) [string]` | all non-overlapping matches |

---

## Reference table

| Function / Method | Description |
|-------------------|-------------|
| `regex::compile(pat)` | Compile pattern; returns `Regex` |
| `regex::matches(pat, s)` | Quick boolean test |
| `regex::find(pat, s)` | First match string |
| `regex::find_all(pat, s)` | All match strings |
| `regex::replace(pat, s, repl)` | Replace first match |
| `regex::replace_all(pat, s, repl)` | Replace all matches |
| `regex::split(pat, s)` | Split on matches |
| `re.exec(s)` | Execute compiled regex |
| `re.match_at(s, start)` | Execute at byte offset |
| `re.find_all(s)` | All matches with compiled regex |
| `m.matched()` | Check if match succeeded |
| `m.group(i)` | Get capture group text |
| `m.count()` | Number of captured groups |
