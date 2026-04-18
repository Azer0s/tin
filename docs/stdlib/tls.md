# tls - Async TLS client connections

`stdlib/tls` wraps an existing TCP file descriptor with TLS using OpenSSL.
`TlsConn` implements `io::AsyncReader` and `io::AsyncWriter`, so all `ioutil`
functions work transparently over TLS.

```rust
use tls
```

Requires OpenSSL at link time (handled automatically by the package directives).

---

## Quick start

```rust
use { loop, async } from macros
use tls
use net::tcp
use ioutil
use errors

async handle_conn(host string, fd i32) =
  let conn = await tls::connect(host, fd)
  await ioutil::write_string(conn, "GET / HTTP/1.0\r\nHost: {host}\r\n\r\n")
  loop:
    let line = await ioutil::read_string(conn)
    if len(line) == 0: break
    echo line
  conn.close()

fn{#async} main() =
  let (tcp_conn, err) = tcp::dial("example.com", 443)
  if errors::has(err): panic(err.message())
  spawn handle_conn("example.com", tcp_conn.fd)
```

---

## API

### `tls::connect`

```rust
fn connect(host string, fd i32) Future[TlsConn]
```

Wraps `fd` with TLS and performs the async TLS handshake. `host` is used for
SNI (Server Name Indication) and certificate verification. The returned
`TlsConn` is immediately ready for reading and writing.

- `host` must match the certificate's CN or SAN
- `fd` must be a connected TCP socket (e.g. from `tcp::dial`)
- Certificate verification is on by default; use `set_ca_file` for custom CAs
- Handshake is fully async - the calling fiber parks and retries when I/O is ready

```rust
let (conn, err) = tcp::dial("api.example.com", 443)
if errors::has(err): panic(err.message())
let tls_conn = await tls::connect("api.example.com", conn.fd)
```

---

### `tls::set_ca_file`

```rust
fn set_ca_file(path string)
```

Loads a PEM certificate file into the global SSL trust store. Call this
before any `connect` call when the server uses a self-signed or custom CA
certificate.

```rust
tls::set_ca_file("certs/ca.pem")
let tls_conn = await tls::connect("internal-service", conn.fd)
```

---

### `tls::TlsConn`

```rust
struct TlsConn (io::AsyncReader, io::AsyncWriter) =
  fd i32   // underlying TCP file descriptor
```

`TlsConn` is a value type with reference semantics (copies share the same
OpenSSL `SSL*` state). Do not use a `TlsConn` after calling `close`.

| Method | Signature | Description |
|--------|-----------|-------------|
| `read`  | `(buf *byte, n i64) Future[i64]` | Read up to n bytes; parks fiber if not ready |
| `write` | `(buf *byte, n i64) Future[i64]` | Write up to n bytes; parks fiber if not ready |
| `close` | `()` | Send TLS close_notify and free OpenSSL state |

The `fd` field holds the underlying TCP file descriptor. Close it separately
after `close()`:

```rust
tls_conn.close()   // TLS layer
conn.close()       // TCP layer
```

`TlsConn` works with all `ioutil` functions:

```rust
let line = await ioutil::read_string(tls_conn)
await ioutil::write_string(tls_conn, "response\n")
let data = await ioutil::read_bytes(tls_conn, 256)
```

---

## Initialization

`tls` declares `fn init()`, which initializes the global `SSL_CTX` once at
program startup. No explicit setup is needed - just `use tls`.

The global context is shared across all connections and is configured with
`SSL_VERIFY_PEER` and the system default CA bundle. Override with
`set_ca_file` before the first `connect`.

---

## Testing with self-signed certificates

Generate a test certificate:

```bash
openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365 -nodes \
  -subj "/CN=localhost" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost"
```

Load it before connecting:

```rust
tls::set_ca_file("cert.pem")
let tls_conn = await tls::connect("localhost", conn.fd)
```

---

## Concurrent connections

Each `TlsConn` is independent. Multiple fibers can hold and use different
`TlsConn` values concurrently - the underlying OpenSSL `SSL*` objects are
per-connection, and the global `SSL_CTX` is read-only after initialization.

```rust
use { async } from macros

async worker(id i64, done sync::Channel[i64]) =
  let (conn, err) = tcp::dial("example.com", 443)
  if errors::has(err):
    await done.send(1)
    return
  let tls_conn = await tls::connect("example.com", conn.fd)
  await ioutil::write_string(tls_conn, "ping\n")
  let resp = await ioutil::read_string(tls_conn)
  tls_conn.close()
  conn.close()
  await done.send(0)

fn{#async} main() =
  let n    i64 = 20
  let done = sync::Channel[i64].make(n)
  let i    i64 = 0
  for i < n:
    spawn worker(i, done)
    i = i + 1
  let j i64 = 0
  for j < n:
    await done.recv()
    j = j + 1
  echo "all done"
```
