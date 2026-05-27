// libs/sqlite/sqlite_worker - blocking SQLite bindings.
//
// All sqlite3 calls run directly on the caller's thread; there's no
// dedicated worker pthread or task queue.  A per-connection mutex
// guards the sqlite3* handle and the prepared-statement cache so
// multiple Tin fibers can share a Connection across the scheduler's
// worker threads.
//
// Two API tiers on top of this file (see libs/sqlite/sqlite.tin):
//   High level: db.exec_*(sql, ...) + db.query(sql, ...) + db.prepare(...)
//   Low level:  stmt.bind_*(...) + stmt.step() + stmt.exec_*(...)
// Both blocking; both share the same per-conn prepared-statement cache.

#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200112L
#endif

#include <pthread.h>
#include <sqlite3.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Forward decl: runtime helper that allocates a fresh ARC-managed
// Tin string from a (ptr, len) pair.  Used by query result text
// materialization (writes into a Tin `*string` out-param).
typedef struct { char *ptr; int64_t len; int64_t cap; } _TinString;
extern _TinString _tin_string_from_bytes(const char *ptr, int64_t len);

// ---------------------------------------------------------------------------
// Connection + per-conn prepared-statement cache
// ---------------------------------------------------------------------------

// Cap on cached prepared statements.  On insert past the cap we
// finalize and drop the oldest entry.  Effective for hot loops that
// recycle a handful of statements.
#define STMT_CACHE_MAX 32

typedef struct cached_stmt {
    char               *sql;       // strdup'd; survives caller's drop
    const char         *sql_ptr;   // last-seen caller ptr (for ptr-equality fast path)
    sqlite3_stmt       *stmt;
    struct cached_stmt *next;      // MRU at head
} cached_stmt_t;

typedef struct conn {
    sqlite3        *db;
    // db_mu serializes access to db + stmt_cache so multiple Tin
    // fibers sharing a Connection across worker threads don't race.
    pthread_mutex_t db_mu;
    cached_stmt_t  *stmt_cache_head;
    int             stmt_cache_count;
} conn_t;

// ---------------------------------------------------------------------------
// Cache lookup
// ---------------------------------------------------------------------------

// Generic cache lookup by content (strcmp).  Caller must hold db_mu.
static sqlite3_stmt *stmt_cache_get(conn_t *c, const char *sql, int *out_rc) {
    cached_stmt_t **link = &c->stmt_cache_head;
    while (*link) {
        cached_stmt_t *e = *link;
        if (strcmp(e->sql, sql) == 0) {
            // Update the borrowed-ptr cache so the stable fast-path
            // hits on subsequent calls with the same literal.
            e->sql_ptr = sql;
            // Detach + re-insert at head (MRU).
            *link = e->next;
            e->next = c->stmt_cache_head;
            c->stmt_cache_head = e;
            sqlite3_reset(e->stmt);
            sqlite3_clear_bindings(e->stmt);
            *out_rc = SQLITE_OK;
            return e->stmt;
        }
        link = &(*link)->next;
    }
    // Miss: prepare.
    sqlite3_stmt *stmt = NULL;
    int rc = sqlite3_prepare_v2(c->db, sql, -1, &stmt, NULL);
    *out_rc = rc;
    if (rc != SQLITE_OK) {
        if (stmt) sqlite3_finalize(stmt);
        return NULL;
    }
    cached_stmt_t *e = (cached_stmt_t *)calloc(1, sizeof(*e));
    e->sql     = strdup(sql);
    e->sql_ptr = sql;
    e->stmt    = stmt;
    e->next    = c->stmt_cache_head;
    c->stmt_cache_head = e;
    c->stmt_cache_count++;
    if (c->stmt_cache_count > STMT_CACHE_MAX) {
        cached_stmt_t **t = &c->stmt_cache_head;
        for (int i = 0; i < STMT_CACHE_MAX && *t; i++) t = &(*t)->next;
        if (*t) {
            cached_stmt_t *victim = *t;
            *t = NULL;
            sqlite3_finalize(victim->stmt);
            free(victim->sql);
            free(victim);
            c->stmt_cache_count = STMT_CACHE_MAX;
        }
    }
    return stmt;
}

// Stable-pointer fast path.  Caller must hold db_mu.  Skips strcmp +
// cache walk by checking only head's last-seen caller pointer.  Safe
// for callers that pass Tin string literals (deduped by the linker
// into one constant); the EXEC_FAST tail always resets, so no
// explicit reset/clear_bindings needed on a stable-cache hit.
static sqlite3_stmt *stmt_cache_get_stable(conn_t *c, const char *sql, int *out_rc) {
    if (c->stmt_cache_head && c->stmt_cache_head->sql_ptr == sql) {
        *out_rc = SQLITE_OK;
        return c->stmt_cache_head->stmt;
    }
    return stmt_cache_get(c, sql, out_rc);
}

static void stmt_cache_clear(conn_t *c) {
    cached_stmt_t *e = c->stmt_cache_head;
    while (e) {
        cached_stmt_t *next = e->next;
        sqlite3_finalize(e->stmt);
        free(e->sql);
        free(e);
        e = next;
    }
    c->stmt_cache_head = NULL;
    c->stmt_cache_count = 0;
}

// ---------------------------------------------------------------------------
// Open / close
// ---------------------------------------------------------------------------

void *_tin_sqlite_open(const char *path) {
    conn_t *c = (conn_t *)calloc(1, sizeof(*c));
    if (!c) return NULL;
    if (sqlite3_open(path, &c->db) != SQLITE_OK) {
        if (c->db) sqlite3_close(c->db);
        free(c);
        return NULL;
    }
    pthread_mutex_init(&c->db_mu, NULL);
    return c;
}

void _tin_sqlite_close(void *conn) {
    if (!conn) return;
    conn_t *c = (conn_t *)conn;
    stmt_cache_clear(c);
    sqlite3_close(c->db);
    pthread_mutex_destroy(&c->db_mu);
    free(c);
}

// ---------------------------------------------------------------------------
// Binding helper
// ---------------------------------------------------------------------------

// Bind one param to the prepared stmt at the given 1-based index.
// Uses SQLITE_TRANSIENT so sqlite copies TEXT data into stmt storage.
static int bind_one(sqlite3_stmt *stmt, int idx, int64_t tag,
                    int64_t int_val, double real_val, const char *text_val) {
    switch (tag) {
    case 1: return sqlite3_bind_int64(stmt, idx, int_val);
    case 2: return sqlite3_bind_double(stmt, idx, real_val);
    case 3: return sqlite3_bind_text(stmt, idx, text_val ? text_val : "", -1, SQLITE_TRANSIENT);
    case 5: return sqlite3_bind_null(stmt, idx);
    default: return SQLITE_MISUSE;
    }
}

// ---------------------------------------------------------------------------
// Generic exec (array of params)
// ---------------------------------------------------------------------------

// Returns changes() on success, -(error_code) on failure.
int64_t _tin_sqlite_exec(void *conn, const char *sql,
                         const int64_t *tags, const int64_t *ints,
                         const double *reals,
                         const char *const *texts, int64_t n_params) {
    conn_t *c = (conn_t *)conn;
    pthread_mutex_lock(&c->db_mu);

    int rc;
    sqlite3_stmt *stmt = stmt_cache_get(c, sql, &rc);
    if (rc != SQLITE_OK || !stmt) {
        pthread_mutex_unlock(&c->db_mu);
        return -(int64_t)rc;
    }
    for (int64_t i = 0; i < n_params; i++) {
        rc = bind_one(stmt, (int)(i + 1),
                      tags[i], ints[i], reals ? reals[i] : 0.0,
                      texts ? texts[i] : NULL);
        if (rc != SQLITE_OK) {
            sqlite3_reset(stmt);
            pthread_mutex_unlock(&c->db_mu);
            return -(int64_t)rc;
        }
    }
    rc = sqlite3_step(stmt);
    if (rc != SQLITE_DONE && rc != SQLITE_ROW) {
        sqlite3_reset(stmt);
        pthread_mutex_unlock(&c->db_mu);
        return -(int64_t)rc;
    }
    int64_t changes = (int64_t)sqlite3_changes(c->db);
    sqlite3_reset(stmt);
    pthread_mutex_unlock(&c->db_mu);
    return changes;
}

// ---------------------------------------------------------------------------
// Shape-specialized exec (fast paths for 0-2 primitive params).
//
// Skip the per-call array marshalling the generic exec needs.  Hot
// inner loops on these shapes hit the stmt_cache_get_stable ptr-fast
// path and land within ~10% of pure C.
// ---------------------------------------------------------------------------

#define EXEC_FAST(conn, sql, body) do { \
    conn_t *_c = (conn_t *)(conn); \
    pthread_mutex_lock(&_c->db_mu); \
    int _rc; \
    sqlite3_stmt *_stmt = stmt_cache_get_stable(_c, (sql), &_rc); \
    if (_rc != SQLITE_OK || !_stmt) { \
        pthread_mutex_unlock(&_c->db_mu); \
        return -(int64_t)_rc; \
    } \
    body \
    _rc = sqlite3_step(_stmt); \
    int64_t _out = (_rc == SQLITE_DONE) \
        ? (int64_t)sqlite3_changes(_c->db) \
        : -(int64_t)_rc; \
    sqlite3_reset(_stmt); \
    pthread_mutex_unlock(&_c->db_mu); \
    return _out; \
} while (0)

int64_t _tin_sqlite_exec_fast_0(void *conn, const char *sql) {
    EXEC_FAST(conn, sql, /* no binds */);
}
int64_t _tin_sqlite_exec_fast_i(void *conn, const char *sql, int64_t a) {
    EXEC_FAST(conn, sql, sqlite3_bind_int64(_stmt, 1, a););
}
int64_t _tin_sqlite_exec_fast_s(void *conn, const char *sql, const char *a, int64_t a_len) {
    EXEC_FAST(conn, sql, sqlite3_bind_text(_stmt, 1, a ? a : "", (int)a_len, SQLITE_TRANSIENT););
}
int64_t _tin_sqlite_exec_fast_ii(void *conn, const char *sql, int64_t a, int64_t b) {
    EXEC_FAST(conn, sql,
        sqlite3_bind_int64(_stmt, 1, a);
        sqlite3_bind_int64(_stmt, 2, b);
    );
}
int64_t _tin_sqlite_exec_fast_is(void *conn, const char *sql, int64_t a, const char *b, int64_t b_len) {
    EXEC_FAST(conn, sql,
        sqlite3_bind_int64(_stmt, 1, a);
        sqlite3_bind_text(_stmt, 2, b ? b : "", (int)b_len, SQLITE_TRANSIENT);
    );
}
int64_t _tin_sqlite_exec_fast_si(void *conn, const char *sql, const char *a, int64_t a_len, int64_t b) {
    EXEC_FAST(conn, sql,
        sqlite3_bind_text(_stmt, 1, a ? a : "", (int)a_len, SQLITE_TRANSIENT);
        sqlite3_bind_int64(_stmt, 2, b);
    );
}

#undef EXEC_FAST

// ---------------------------------------------------------------------------
// Query: materialize the whole result set into a packed buffer.
// ---------------------------------------------------------------------------
//
// Tin-side Rows handle owns the buffer via rc::Cell with rows_free as
// dtor.  Row accessors (get_i64/get_string/...) are pure C memcpy
// against this buffer -- no further sqlite calls.

typedef struct {
    int64_t tag;
    int64_t int_val;     // Int payload, or TEXT byte length
    double  real_val;
    char   *text_ptr;    // owned; freed with the result_set_t
} cell_t;

typedef struct {
    int64_t  n_rows;
    int64_t  n_cols;
    cell_t  *cells;      // n_rows * n_cols, row-major
} result_set_t;

static void result_set_free(result_set_t *rs) {
    if (!rs) return;
    if (rs->cells) {
        int64_t total = rs->n_rows * rs->n_cols;
        for (int64_t i = 0; i < total; i++) {
            if (rs->cells[i].text_ptr) free(rs->cells[i].text_ptr);
        }
        free(rs->cells);
    }
    free(rs);
}

// Returns the result_set_t* on success; on error returns NULL and
// writes -(error_code) into *out_err (0 on success).
void *_tin_sqlite_query(void *conn, const char *sql,
                        const int64_t *tags, const int64_t *ints,
                        const double *reals,
                        const char *const *texts, int64_t n_params,
                        int64_t *out_err) {
    conn_t *c = (conn_t *)conn;
    pthread_mutex_lock(&c->db_mu);

    int rc;
    sqlite3_stmt *stmt = stmt_cache_get(c, sql, &rc);
    if (rc != SQLITE_OK || !stmt) {
        *out_err = -(int64_t)rc;
        pthread_mutex_unlock(&c->db_mu);
        return NULL;
    }
    for (int64_t i = 0; i < n_params; i++) {
        rc = bind_one(stmt, (int)(i + 1),
                      tags[i], ints[i], reals ? reals[i] : 0.0,
                      texts ? texts[i] : NULL);
        if (rc != SQLITE_OK) {
            sqlite3_reset(stmt);
            *out_err = -(int64_t)rc;
            pthread_mutex_unlock(&c->db_mu);
            return NULL;
        }
    }

    int64_t n_cols   = (int64_t)sqlite3_column_count(stmt);
    int64_t cap_rows = 8;
    int64_t n_rows   = 0;
    cell_t *cells    = (cell_t *)calloc((size_t)(cap_rows * n_cols), sizeof(cell_t));

    for (;;) {
        rc = sqlite3_step(stmt);
        if (rc == SQLITE_DONE) break;
        if (rc != SQLITE_ROW) {
            for (int64_t i = 0; i < n_rows * n_cols; i++) {
                if (cells[i].text_ptr) free(cells[i].text_ptr);
            }
            free(cells);
            sqlite3_reset(stmt);
            *out_err = -(int64_t)rc;
            pthread_mutex_unlock(&c->db_mu);
            return NULL;
        }
        if (n_rows == cap_rows) {
            cap_rows *= 2;
            cells = (cell_t *)realloc(cells, (size_t)(cap_rows * n_cols) * sizeof(cell_t));
            memset(cells + (n_rows * n_cols), 0,
                   (size_t)((cap_rows - n_rows) * n_cols) * sizeof(cell_t));
        }
        cell_t *row = cells + (n_rows * n_cols);
        for (int64_t col = 0; col < n_cols; col++) {
            int col_type = sqlite3_column_type(stmt, (int)col);
            switch (col_type) {
            case SQLITE_INTEGER:
                row[col].tag = 1;
                row[col].int_val = sqlite3_column_int64(stmt, (int)col);
                break;
            case SQLITE_FLOAT:
                row[col].tag = 2;
                row[col].real_val = sqlite3_column_double(stmt, (int)col);
                break;
            case SQLITE_TEXT: {
                row[col].tag = 3;
                const unsigned char *src = sqlite3_column_text(stmt, (int)col);
                int len = sqlite3_column_bytes(stmt, (int)col);
                row[col].int_val = len;
                row[col].text_ptr = (char *)malloc((size_t)len + 1);
                if (src && len > 0) memcpy(row[col].text_ptr, src, (size_t)len);
                row[col].text_ptr[len] = '\0';
                break;
            }
            case SQLITE_NULL:
            default:
                row[col].tag = 5;
                break;
            }
        }
        n_rows++;
    }

    sqlite3_reset(stmt);
    pthread_mutex_unlock(&c->db_mu);

    result_set_t *rs = (result_set_t *)calloc(1, sizeof(*rs));
    rs->n_rows = n_rows;
    rs->n_cols = n_cols;
    rs->cells  = cells;
    *out_err = 0;
    return rs;
}

// ---------------------------------------------------------------------------
// Rows accessors
// ---------------------------------------------------------------------------

void _tin_sqlite_rows_free(void *p) { result_set_free((result_set_t *)p); }

int64_t _tin_sqlite_rows_count(void *p) {
    result_set_t *rs = (result_set_t *)p;
    return rs ? rs->n_rows : 0;
}

int64_t _tin_sqlite_rows_cols(void *p) {
    result_set_t *rs = (result_set_t *)p;
    return rs ? rs->n_cols : 0;
}

int64_t _tin_sqlite_rows_get_i64(void *p, int64_t row, int64_t col) {
    result_set_t *rs = (result_set_t *)p;
    if (!rs || row >= rs->n_rows || col >= rs->n_cols) return 0;
    return rs->cells[row * rs->n_cols + col].int_val;
}

double _tin_sqlite_rows_get_f64(void *p, int64_t row, int64_t col) {
    result_set_t *rs = (result_set_t *)p;
    if (!rs || row >= rs->n_rows || col >= rs->n_cols) return 0.0;
    return rs->cells[row * rs->n_cols + col].real_val;
}

void _tin_sqlite_rows_get_text(void *p, int64_t row, int64_t col, _TinString *out_str) {
    result_set_t *rs = (result_set_t *)p;
    if (!rs || row >= rs->n_rows || col >= rs->n_cols) {
        *out_str = _tin_string_from_bytes("", 0);
        return;
    }
    cell_t *cell = &rs->cells[row * rs->n_cols + col];
    if (cell->text_ptr == NULL || cell->int_val <= 0) {
        *out_str = _tin_string_from_bytes("", 0);
        return;
    }
    *out_str = _tin_string_from_bytes(cell->text_ptr, cell->int_val);
}

int _tin_sqlite_rows_is_null(void *p, int64_t row, int64_t col) {
    result_set_t *rs = (result_set_t *)p;
    if (!rs || row >= rs->n_rows || col >= rs->n_cols) return 1;
    return rs->cells[row * rs->n_cols + col].tag == 5 ? 1 : 0;
}

// ---------------------------------------------------------------------------
// Low-level Stmt API: user-owned sqlite3_stmt.
// ---------------------------------------------------------------------------
//
// Each bind/step/reset is one direct C call.  Same db_mu as the
// cache-using paths, so safe to interleave with db.exec/query.

typedef struct stmt_handle {
    conn_t       *conn;
    sqlite3_stmt *stmt;
} stmt_handle_t;

void *_tin_sqlite_stmt_prepare(void *conn, const char *sql, int *out_rc) {
    conn_t *c = (conn_t *)conn;
    sqlite3_stmt *stmt = NULL;
    pthread_mutex_lock(&c->db_mu);
    int rc = sqlite3_prepare_v2(c->db, sql, -1, &stmt, NULL);
    pthread_mutex_unlock(&c->db_mu);
    *out_rc = rc;
    if (rc != SQLITE_OK || !stmt) {
        if (stmt) sqlite3_finalize(stmt);
        return NULL;
    }
    stmt_handle_t *h = (stmt_handle_t *)calloc(1, sizeof(*h));
    h->conn = c;
    h->stmt = stmt;
    return h;
}

void _tin_sqlite_stmt_finalize(void *p) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    if (!h) return;
    if (h->stmt) {
        pthread_mutex_lock(&h->conn->db_mu);
        sqlite3_finalize(h->stmt);
        pthread_mutex_unlock(&h->conn->db_mu);
    }
    free(h);
}

int _tin_sqlite_stmt_bind_int(void *p, int idx, int64_t v) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    int rc = sqlite3_bind_int64(h->stmt, idx, v);
    pthread_mutex_unlock(&h->conn->db_mu);
    return rc;
}

int _tin_sqlite_stmt_bind_real(void *p, int idx, double v) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    int rc = sqlite3_bind_double(h->stmt, idx, v);
    pthread_mutex_unlock(&h->conn->db_mu);
    return rc;
}

int _tin_sqlite_stmt_bind_text(void *p, int idx, const char *s, int64_t len) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    int rc = sqlite3_bind_text(h->stmt, idx, s ? s : "", (int)len, SQLITE_TRANSIENT);
    pthread_mutex_unlock(&h->conn->db_mu);
    return rc;
}

int _tin_sqlite_stmt_bind_null(void *p, int idx) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    int rc = sqlite3_bind_null(h->stmt, idx);
    pthread_mutex_unlock(&h->conn->db_mu);
    return rc;
}

// step returns changes() on SQLITE_DONE, -1 on SQLITE_ROW (caller
// iterates via the query path instead), or -(error_code).
int64_t _tin_sqlite_stmt_step(void *p) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    int rc = sqlite3_step(h->stmt);
    int64_t out;
    if (rc == SQLITE_DONE)      out = (int64_t)sqlite3_changes(h->conn->db);
    else if (rc == SQLITE_ROW)  out = -1;
    else                        out = -(int64_t)rc;
    pthread_mutex_unlock(&h->conn->db_mu);
    return out;
}

void _tin_sqlite_stmt_reset(void *p) {
    stmt_handle_t *h = (stmt_handle_t *)p;
    pthread_mutex_lock(&h->conn->db_mu);
    sqlite3_reset(h->stmt);
    sqlite3_clear_bindings(h->stmt);
    pthread_mutex_unlock(&h->conn->db_mu);
}

// exec_<shape>: bind every column + step + reset under one mutex
// acquire and one FFI call boundary.  Returns changes() on SQLITE_DONE
// or -(error_code) on failure.

#define STMT_EXEC(p, body) do { \
    stmt_handle_t *_h = (stmt_handle_t *)(p); \
    pthread_mutex_lock(&_h->conn->db_mu); \
    body \
    int _rc = sqlite3_step(_h->stmt); \
    int64_t _out = (_rc == SQLITE_DONE) \
        ? (int64_t)sqlite3_changes(_h->conn->db) \
        : -(int64_t)_rc; \
    sqlite3_reset(_h->stmt); \
    sqlite3_clear_bindings(_h->stmt); \
    pthread_mutex_unlock(&_h->conn->db_mu); \
    return _out; \
} while (0)

int64_t _tin_sqlite_stmt_exec_i(void *p, int64_t a) {
    STMT_EXEC(p, sqlite3_bind_int64(_h->stmt, 1, a););
}
int64_t _tin_sqlite_stmt_exec_s(void *p, const char *a, int64_t a_len) {
    STMT_EXEC(p, sqlite3_bind_text(_h->stmt, 1, a ? a : "", (int)a_len, SQLITE_TRANSIENT););
}
int64_t _tin_sqlite_stmt_exec_ii(void *p, int64_t a, int64_t b) {
    STMT_EXEC(p,
        sqlite3_bind_int64(_h->stmt, 1, a);
        sqlite3_bind_int64(_h->stmt, 2, b);
    );
}
int64_t _tin_sqlite_stmt_exec_is(void *p, int64_t a, const char *b, int64_t b_len) {
    STMT_EXEC(p,
        sqlite3_bind_int64(_h->stmt, 1, a);
        sqlite3_bind_text(_h->stmt, 2, b ? b : "", (int)b_len, SQLITE_TRANSIENT);
    );
}
int64_t _tin_sqlite_stmt_exec_si(void *p, const char *a, int64_t a_len, int64_t b) {
    STMT_EXEC(p,
        sqlite3_bind_text(_h->stmt, 1, a ? a : "", (int)a_len, SQLITE_TRANSIENT);
        sqlite3_bind_int64(_h->stmt, 2, b);
    );
}
int64_t _tin_sqlite_stmt_exec_iis(void *p, int64_t a, int64_t b, const char *c, int64_t c_len) {
    STMT_EXEC(p,
        sqlite3_bind_int64(_h->stmt, 1, a);
        sqlite3_bind_int64(_h->stmt, 2, b);
        sqlite3_bind_text(_h->stmt, 3, c ? c : "", (int)c_len, SQLITE_TRANSIENT);
    );
}

#undef STMT_EXEC
