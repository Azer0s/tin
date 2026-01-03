# tin

## Hello world

```rust
echo "Hello, world!"
```

## Fibonacci sequence

```rust
fn fib(n u32) u32 =
  if n <= 1:
    return n
  else:
    return fib(n - 1) + fib(n - 2)

echo fib(10)
```

## Functional Fibonacci sequence

```rust
fn fib(n u32) u32 =
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)
```

## Language samples

```rust
type char = u8
type string = [char]

let a string = "hello"
const b i8 = 22

fn main(args [string], argc u16) i32 =
  echo "Hello world"

  for let i i32; i < 10; i++:
    echo "{i}"
```

### External functions

````rust
fn ex_printf(const *char, ...) i32 = extern(printf)
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)

let hello = "Hello!"
printf(hello)
````

```rust
use extern (
  malloc as fn(size_t) *void,
  strcpy as fn(*char, const *char) *char,
)

let s *char = mallloc(10 * sizeof(*char)).(*char)
strcpy(s, "abcdefghij")

let a = "Hello" //string is inferred
a = a ++ ", world!"
a = a ++ [',', ' ', 'w', 'o', 'r', 'l', 'd', '!']
```

### Structs

```rust
struct person =
  name string
  age u8

  fn init(this person) =
    echo "this is called when a person struct is initialized (except for malloc)"

  fn show(this person) string =
    return "{this.name} is {this.age} years old"

let pete = person{name: "Pete", age: 20}
let pPtr *person = &pete

echo pete.show()
echo (*pPtr).show()
echo pPtr->show()
```

```rust
let pete *person = malloc(sizeof(person)).(*person)
(*pete).name = "Pete"
(*pete).age = 20
```

```rust
struct tuple[t] =
  first t
  second t

  fn show(this tuple) string =
    return "first: {this.first}, second: {this.second}"

type point = tuple[f32] override =	
  fn show(this point) string =
    return "({point.first}, {point.second})"

let p1 point = point{1.2, 1.4}

let t tuple = p1
```


### Map | Filter | Reduce

```rust
let nums [i32] = [1, 2, 3, 4, 5, 6, 7]

fn filter[t](f fn(i t) bool) fn([t]) [t] =
  return fn(list [t]) [t] = 
    let res [t] = []

    for let i t in list:
      if f(i):
        res ++= i

    return res

fn map[t, r](f fn(i t) r) fn([t]) [r] =
  return fn(list [t]) [r] =
    let res [r] = []

    for let i t in list:
      res ++= f(i)

    return res

nums
|> filter(fn(i i32) bool = return i % 2 == 0)
|> map(fn(i i32) i32 = return i * i)
```

```rust
fn subsquences[t](l [t]) [t] =
  let res [[t]] = []
  
  for let i i64 = 0..(len(l) ^ 2):
    let sequence [t] = []
    
    for let j i64 in 0..len(2):
      let pick = (i >> j) & 1
      if pick == 0:
        sequence ++= l[j]
	
    res ++= sequence
```

### Close to metal programming

```rust
fn write_string(color i32, s string) =
  let video_mem *char = addr(0xB8000).(*char)
  for let c char in s:
    *video_mem = c
    video_mem += 1
    *video_mem = color
    video_mem += 1
```

### Iterators & traits

```rust
type size_t = uint32

use extern (
  // extern functions are always marked as #sideEffectful
  strlen as fn(const *char) size_t
)

trait iter[t] = 
  fn size() size_t = virtual
  fn idx(i size_t) t = virtual

trait size = 
	s size_t forward
	fn size() size_t = return s

trait print as fn() [char]

trait[k] implicit[t] as static fn(val t) k

struct{ #pure@fn #const@field } str(size, print,
    implicit[[char]], implicit[char], 
    iter[char], iter[str]) = 

  v [char]

  static fn ::implicit[[char]] (val [char]) str =
    let len size_t = 0
    { #ignoreSideEffectful } {
      len = strlen(val)
    }
    return str{v: val, s: len}

  static fn ::implicit[char] (val char) str = 
    return str{v: [val], s: 1}

  fn idx(i size_t) char = return v[i]

  fn iter[char]::idx(i size_t) char = idx
  fn iter[str]::idx(i size_t) str = idx

  fn ::print() [char] = return v

  fn for_each(this str, f fn(c char)) = 
    for let i size_t = 0; i < this.s; i++:
      { #ignoreSideEffectful } { f(v[i]) }

let h str = "Hello world"
echo h

h.for_each(fn(c char) = io::print("{}", c))
```

### Ranges

```rust
let r = 1..10
for let i i8 in 1..10:
  echo "{i}"
```

### Union types

```rust
type u = i8 | string

let a u = 10

if a is i i8:
  printf("%d", i * i)
else:
  echo i as string

match a.(type):
  case i i8:
    printf("%d", i)
  case s string:
    echo s
```

### Wrapper types

```rust
data maybe[t] = t | None

let m maybe[string] = None

if m is s string:
  echo s

if m is None:
  echo "m is unset"
```

### Native union types

```rust
//this is a C union
union u = i8 | string
union u_named = as_i8 i8 | as_string string

let a u = 10

echo a as string
printf("%d", a.(int))

let b u_named = 10
echo b.as_string //this is the same as b.(string)
printf("%d", b.as_i8)
```

### Enums

```rust
enum i32 weather = 
  sunny: 0,
  rainy: 1,
  foggy: 2,
  clear: 3,

let w weather = weather.sunny

match w:
  case weather.sunny:
    echo "it is sunny outside"
  case weather.rainy:
    echo "it rains"
  default:
    echo "there is weather"

enum slider_type = //this will take the smallest integer type possible (u8 in this case)
  horizontal,
  vertical,
```

### Packages & exports

```rust
fn print(t string) = 
	break

export { print } as io
```

```rust
use io

io::print("hello")
```

```rust
use io
use math

export { io, math } as std
```

```rust
use std
let a = std::math::floor(std::math::PI)
```

```rust
use extern (
  malloc as fn(size_t) *void,
  memset as fn(*void, value_t, size_t)
)

fn malloc_zeroed(s size_t) *void =
  let chunk = malloc(s)
  memset(chunk, 0, s)
  return chunk
```

### Control tags

```rust
fn{#pure #recurse} fib(n u32) u32 = 
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

echo fib(10) //this will be calculated at compile time
```

```rust
fn{#noRecurse} foo() = 
  foo()

//this will fail to compile
```

```rust
macro{#noExcl #noParens} proc() =
  return `fn{#pure #recurse #noThread}`

proc fib(n u32) u32 = 
  where n <= 1: n
  where _: fib(n - 1) + fib(n - 2)

fn ex_printf(const *char, ...) i32 = extern(printf) //this will automatically have a #sideEffectful tag
fn printf(format string, args ...) i32 =
  return ex_printf(&format[0], args)

proc print() =
  printf("Hello world") //this will fail to compile
```

### defer

```rust
let s = mallloc(10 * sizeof(*char)).([char; 10])
defer free(s)
```

### Atoms & Macros

```rust
use io
use guid

enum atom status =
  'ok,
  'err

struct result[t] =
  val t
  status

  static fn ok(val t) =
    return result{val, status: status.ok}

  static fn err() = 
    return result{val: default(t), status: status.err}

macro try!(action) = 
  let i = "_" ++ guid::new().show().replace("-", "")
  return `
    (let {i} = {action}; {i}.status == status.ok) ? {i}.val : return result.err()
	`

fn do_stuff() result[u32] =
	return ok(42)

let val = try!(do_stuff())	
```

```rust
fn it_is(weather atom) = 
	where 'sunny: echo "It is sunny!"
	where 'rainy: echo "It is rainy!"
	where _: echo "Sorry, I don't know this condition :("
```
