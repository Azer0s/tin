// stdlib/tls - OpenSSL TLS connection helpers
//
// Provides async-friendly TLS over an existing non-blocking file descriptor.
// The fd must already be connected (TCP or Unix) before calling _tin_tls_new.
//
// Async pattern: SSL_read/SSL_write/SSL_do_handshake return SSL_ERROR_WANT_READ
// or SSL_ERROR_WANT_WRITE on a non-blocking fd when I/O is not yet available.
// We map those to TIN_IO_BLOCKED and call _tin_async_park_read/write so the
// Tin fiber scheduler can wake us when the fd is ready.

#include "runtime.h"
#include "async_io.h"
#include <openssl/ssl.h>
#include <openssl/err.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>

#define TIN_IO_BLOCKED INT64_MIN

typedef struct {
    SSL *ssl;
    int  fd;
} TinTLS;

static SSL_CTX *_g_ssl_ctx = NULL;

// _tin_tls_init initializes the global SSL_CTX.  Called from fn init().
void _tin_tls_init(void) {
    if (_g_ssl_ctx) return;
    SSL_library_init();
    SSL_load_error_strings();
    _g_ssl_ctx = SSL_CTX_new(TLS_client_method());
    if (!_g_ssl_ctx) return;
    SSL_CTX_set_default_verify_paths(_g_ssl_ctx);
    SSL_CTX_set_verify(_g_ssl_ctx, SSL_VERIFY_PEER, NULL);
}

// _tin_tls_set_ca_file loads a PEM CA certificate into the global SSL_CTX.
// Use this to trust a custom or self-signed certificate before connecting.
// Returns 1 on success, 0 on failure.
int32_t _tin_tls_set_ca_file(const char *path) {
    if (!_g_ssl_ctx) return 0;
    return SSL_CTX_load_verify_locations(_g_ssl_ctx, path, NULL) == 1 ? 1 : 0;
}

// _tin_tls_new wraps fd with TLS client state and sets SNI to host.
// host must be a null-terminated string (Tin strings are null-terminated).
// Returns an opaque pointer; the caller must call _tin_tls_do_handshake next.
void *_tin_tls_new(const char *host, int32_t fd) {
    if (!_g_ssl_ctx) return NULL;

    TinTLS *t = (TinTLS *)malloc(sizeof(TinTLS));
    if (!t) return NULL;

    t->fd = (int)fd;
    _tin_set_nonblocking(t->fd);

    t->ssl = SSL_new(_g_ssl_ctx);
    if (!t->ssl) { free(t); return NULL; }

    SSL_set_fd(t->ssl, t->fd);
    SSL_set_connect_state(t->ssl);
    SSL_set_tlsext_host_name(t->ssl, host);

    return (void *)t;
}

// _tin_tls_do_handshake advances the TLS handshake one step.
// Returns 0 when complete, TIN_IO_BLOCKED if more I/O is needed, -1 on error.
int64_t _tin_tls_do_handshake(void *handle) {
    TinTLS *t = (TinTLS *)handle;
    int r = SSL_do_handshake(t->ssl);
    if (r == 1) return 0;

    int err = SSL_get_error(t->ssl, r);
    if (err == SSL_ERROR_WANT_READ)  return _tin_async_park_read(t->fd);
    if (err == SSL_ERROR_WANT_WRITE) return _tin_async_park_write(t->fd);
    return -1;
}

// _tin_tls_read reads up to n bytes into buf.
// Returns bytes read (>0), 0 on clean close, TIN_IO_BLOCKED, or -1 on error.
int64_t _tin_tls_read(void *handle, char *buf, int64_t n) {
    TinTLS *t = (TinTLS *)handle;
    int r = SSL_read(t->ssl, buf, (int)n);
    if (r > 0) return (int64_t)r;
    if (r == 0) return 0;

    int err = SSL_get_error(t->ssl, r);
    if (err == SSL_ERROR_WANT_READ)  return _tin_async_park_read(t->fd);
    if (err == SSL_ERROR_WANT_WRITE) return _tin_async_park_write(t->fd);
    return -1;
}

// _tin_tls_write writes up to n bytes from buf.
// Returns bytes written (>0), TIN_IO_BLOCKED, or -1 on error.
int64_t _tin_tls_write(void *handle, const char *buf, int64_t n) {
    TinTLS *t = (TinTLS *)handle;
    int r = SSL_write(t->ssl, buf, (int)n);
    if (r > 0) return (int64_t)r;

    int err = SSL_get_error(t->ssl, r);
    if (err == SSL_ERROR_WANT_READ)  return _tin_async_park_read(t->fd);
    if (err == SSL_ERROR_WANT_WRITE) return _tin_async_park_write(t->fd);
    return -1;
}

// _tin_tls_close sends close_notify and frees the TLS state.
// Does NOT close the underlying fd - the caller manages that.
void _tin_tls_close(void *handle) {
    TinTLS *t = (TinTLS *)handle;
    if (!t) return;
    if (t->ssl) {
        SSL_shutdown(t->ssl);
        SSL_free(t->ssl);
    }
    free(t);
}
