// libs/sqlite/sqlite_worker - dedicated worker thread per Connection.
//
// SQLite's C API is fully blocking; running calls on the scheduler's
// worker threads would risk blocking those threads.  Each Connection
// owns one pthread that exclusively holds the sqlite3* handle, plus
// an MPSC task queue (mutex + condvar) fed by Tin fibers.
//
// Result delivery uses a per-call Completion struct and the runtime's
// _tin_fiber_park / _tin_fiber_unpark_fib primitives.  The caller
// fiber parks on its own pid; the worker, after finishing the task,
// unparks via the pre-captured fiber pointer.  No Channel allocation
// per call, no scheduler ring traffic.

#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200112L
#endif

#include <pthread.h>
#include <sqlite3.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

extern void   _tin_fiber_park(int64_t pid);
extern void   _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl);
extern int64_t _tin_current_pid(void);
extern void  *_tin_current_fib(void);

// ---------------------------------------------------------------------------
// Completion: per-call oneshot between Tin fiber and worker thread.
// ---------------------------------------------------------------------------

typedef enum {
    OP_EXEC  = 1,
    OP_QUERY = 2,
    OP_SHUTDOWN = 99,
} op_kind_t;

// ResultSet packs all rows + columns from a SELECT into a single
// owned buffer so Tin-side get_* accessors are pure memcpy with no
// extra await per cell.  Layout per cell:
//   tag       i64   (1=Int, 2=Real, 3=Text, 5=Null)
//   int_val   i64   (Int payload, or TEXT length)
//   real_val  f64   (Real payload)
//   text_ptr  char* (TEXT data, owned, freed with the ResultSet)
typedef struct {
    int64_t tag;
    int64_t int_val;
    double  real_val;
    char   *text_ptr;
} cell_t;

typedef struct {
    int64_t  n_rows;
    int64_t  n_cols;
    cell_t  *cells;  // n_rows * n_cols, row-major
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

// Per-param wire format: three parallel slots.  tag picks how to
// interpret the row.  See encode_params in sqlite.tin.
//   tag=1 (Int):  int_val carries the i64
//   tag=3 (Text): text_val carries the C string (borrowed; deep-
//                 copied at submit time before the caller may drop it)
//   tag=5 (Null): no payload

typedef struct completion {
    pthread_mutex_t mu;
    int     done;         // set by worker after writing result
    int64_t waiter_pid;   // -1 until the fiber parks
    void   *waiter_fib;

    // Result fields (interpreted per op).
    int     result_code;  // SQLITE_OK or sqlite error code
    int64_t result_i64;   // exec: changes(); query: 0
    void   *result_ptr;   // query: owned result_set_t*; exec: NULL
    char   *result_err;   // strdup'd error string, or NULL
} completion_t;

static completion_t *completion_new(void) {
    completion_t *c = (completion_t *)calloc(1, sizeof(*c));
    if (!c) return NULL;
    pthread_mutex_init(&c->mu, NULL);
    c->waiter_pid = -1;
    return c;
}

static void completion_free(completion_t *c) {
    if (!c) return;
    pthread_mutex_destroy(&c->mu);
    if (c->result_err) free(c->result_err);
    free(c);
}

// Called from worker thread after executing the task.  Stores result,
// flips done, and unparks the waiter if one has registered.
static void completion_signal(completion_t *c, int code, int64_t v, void *ptr, const char *err) {
    pthread_mutex_lock(&c->mu);
    c->result_code = code;
    c->result_i64  = v;
    c->result_ptr  = ptr;
    if (err) c->result_err = strdup(err);
    c->done = 1;

    int64_t pid = c->waiter_pid;
    void   *fib = c->waiter_fib;
    pthread_mutex_unlock(&c->mu);

    if (pid >= 0) {
        _tin_fiber_unpark_fib(fib, pid, NULL);
    }
}

// ---------------------------------------------------------------------------
// Task queue: singly-linked MPSC list protected by conn->qmu / qcv.
// ---------------------------------------------------------------------------

typedef struct task {
    op_kind_t        op;
    char            *sql;        // strdup'd; freed by worker after exec
    // Params are deep-copied at submit time so the Tin caller can
    // drop the source `[SqlValue]` before the worker picks the task
    // up.  Four parallel arrays match the wire format from
    // sqlite.tin::exec.  texts[i] is strdup'd when tag=3, NULL otherwise.
    int64_t         *tags;
    int64_t         *ints;
    double          *reals;
    char           **texts;
    int64_t          n_params;
    completion_t    *completion;
    struct task     *next;
} task_t;

static void free_task(task_t *t) {
    if (!t) return;
    if (t->sql) free(t->sql);
    if (t->tags) free(t->tags);
    if (t->ints) free(t->ints);
    if (t->reals) free(t->reals);
    if (t->texts) {
        for (int64_t i = 0; i < t->n_params; i++) {
            if (t->texts[i]) free(t->texts[i]);
        }
        free(t->texts);
    }
    free(t);
}

// stmt_cache: simple linked-list cache of (sql, prepared stmt) pairs
// scoped to a single Connection.  Lives on the worker thread; no
// locking needed (single-threaded access).  Capped at STMT_CACHE_MAX
// entries; on insert past the cap, the oldest (head) is finalized
// and dropped.  Cleared+finalized on connection close.
//
// Effective for hot loops: repeated `db.exec("INSERT ...", [...])`
// at the same SQL skips sqlite3_prepare_v2 (a parse+plan) after the
// first call and only does reset+bind+step.
#define STMT_CACHE_MAX 32

typedef struct cached_stmt {
    char               *sql;   // strdup'd
    sqlite3_stmt       *stmt;
    struct cached_stmt *next;  // MRU at head
} cached_stmt_t;

typedef struct conn {
    sqlite3        *db;
    pthread_t       worker;
    pthread_mutex_t qmu;
    pthread_cond_t  qcv;
    task_t         *head;
    task_t         *tail;
    int             stopped;  // worker exited
    // Prepared-statement cache + sqlite3 access protected by db_mu.
    // The async worker takes db_mu around every task; the sync path
    // (exec_blocking) takes it around its direct sqlite3 call.  In
    // pure-async or pure-sync workloads db_mu is uncontended.
    pthread_mutex_t db_mu;
    cached_stmt_t  *stmt_cache_head;
    int             stmt_cache_count;
} conn_t;

static void enqueue(conn_t *c, task_t *t) {
    pthread_mutex_lock(&c->qmu);
    t->next = NULL;
    if (c->tail) c->tail->next = t;
    else         c->head = t;
    c->tail = t;
    pthread_cond_signal(&c->qcv);
    pthread_mutex_unlock(&c->qmu);
}

// Look up a cached stmt for `sql`; on hit, move to MRU (head) and
// return it reset+ready to bind.  On miss, prepare a fresh stmt,
// install at head, evict oldest if past cap.  All called from worker
// thread, no locking.
static sqlite3_stmt *stmt_cache_get(conn_t *c, const char *sql, int *out_rc) {
    cached_stmt_t **link = &c->stmt_cache_head;
    while (*link) {
        cached_stmt_t *e = *link;
        if (strcmp(e->sql, sql) == 0) {
            // Hit: detach and re-insert at head (MRU).  Reset clears
            // bindings + execution state but keeps the parsed plan.
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
    // Install at head.
    cached_stmt_t *e = (cached_stmt_t *)calloc(1, sizeof(*e));
    e->sql  = strdup(sql);
    e->stmt = stmt;
    e->next = c->stmt_cache_head;
    c->stmt_cache_head = e;
    c->stmt_cache_count++;
    // Evict oldest past the cap.
    if (c->stmt_cache_count > STMT_CACHE_MAX) {
        cached_stmt_t **t = &c->stmt_cache_head;
        for (int i = 0; i < STMT_CACHE_MAX && *t; i++) {
            t = &(*t)->next;
        }
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

static task_t *dequeue(conn_t *c) {
    pthread_mutex_lock(&c->qmu);
    while (!c->head) pthread_cond_wait(&c->qcv, &c->qmu);
    task_t *t = c->head;
    c->head = t->next;
    if (!c->head) c->tail = NULL;
    pthread_mutex_unlock(&c->qmu);
    return t;
}

// ---------------------------------------------------------------------------
// Op execution (runs on worker thread).
// ---------------------------------------------------------------------------

// Bind one param to the prepared stmt at the given 1-based index.
// Uses SQLITE_TRANSIENT so sqlite copies TEXT data into stmt storage;
// the param's strdup'd buffer is freed by free_task after step+finalize.
static int bind_one(sqlite3_stmt *stmt, int idx, int64_t tag,
                    int64_t int_val, double real_val, char *text_val) {
    switch (tag) {
    case 1: return sqlite3_bind_int64(stmt, idx, int_val);
    case 2: return sqlite3_bind_double(stmt, idx, real_val);
    case 3: return sqlite3_bind_text(stmt, idx, text_val ? text_val : "", -1, SQLITE_TRANSIENT);
    case 5: return sqlite3_bind_null(stmt, idx);
    default: return SQLITE_MISUSE;
    }
}

// exec_one_locked: caller must hold c->db_mu.  Runs prepare-via-cache
// + bind + step + reset on the stmt, signals the task's completion.
static void exec_one_locked(conn_t *c, task_t *t) {
    int rc;
    sqlite3_stmt *stmt = stmt_cache_get(c, t->sql, &rc);
    if (rc != SQLITE_OK || !stmt) {
        completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
        return;
    }

    for (int64_t i = 0; i < t->n_params; i++) {
        rc = bind_one(stmt, (int)(i + 1),
                      t->tags[i],
                      t->ints[i], t->reals ? t->reals[i] : 0.0,
                      t->texts ? t->texts[i] : NULL);
        if (rc != SQLITE_OK) {
            completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
            sqlite3_reset(stmt);
            return;
        }
    }

    rc = sqlite3_step(stmt);
    if (rc != SQLITE_DONE && rc != SQLITE_ROW) {
        completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
        sqlite3_reset(stmt);
        return;
    }
    // For OP_EXEC we don't iterate rows; report changes() and reset
    // (stmt stays in cache for re-use).
    int64_t changes = (int64_t)sqlite3_changes(c->db);
    sqlite3_reset(stmt);
    completion_signal(t->completion, SQLITE_OK, changes, NULL, NULL);
}

// query_one runs a SELECT-like statement: prepare, bind params, step
// each row into the cell buffer, finalize.  Storage is malloc'd in
// chunks of n_cols rows; we double when exhausted so realloc churn is
// O(log n).
static void query_one_locked(conn_t *c, task_t *t) {
    int rc;
    sqlite3_stmt *stmt = stmt_cache_get(c, t->sql, &rc);
    if (rc != SQLITE_OK || !stmt) {
        completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
        return;
    }

    for (int64_t i = 0; i < t->n_params; i++) {
        rc = bind_one(stmt, (int)(i + 1),
                      t->tags[i], t->ints[i], t->reals ? t->reals[i] : 0.0,
                      t->texts ? t->texts[i] : NULL);
        if (rc != SQLITE_OK) {
            completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
            sqlite3_reset(stmt);
            return;
        }
    }

    int64_t n_cols = (int64_t)sqlite3_column_count(stmt);
    int64_t cap_rows = 8;
    int64_t n_rows = 0;
    cell_t *cells = (cell_t *)calloc((size_t)(cap_rows * n_cols), sizeof(cell_t));

    for (;;) {
        rc = sqlite3_step(stmt);
        if (rc == SQLITE_DONE) break;
        if (rc != SQLITE_ROW) {
            // Step error: free the partial buffer and report.
            for (int64_t i = 0; i < n_rows * n_cols; i++) {
                if (cells[i].text_ptr) free(cells[i].text_ptr);
            }
            free(cells);
            completion_signal(t->completion, rc, 0, NULL, sqlite3_errmsg(c->db));
            sqlite3_reset(stmt);
            return;
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

    result_set_t *rs = (result_set_t *)calloc(1, sizeof(*rs));
    rs->n_rows = n_rows;
    rs->n_cols = n_cols;
    rs->cells  = cells;
    completion_signal(t->completion, SQLITE_OK, 0, rs, NULL);
}

// exec_one / query_one: thin wrappers that grab db_mu around the
// _locked body.  Worker uses these; the blocking entry point bypasses
// the worker but takes the same lock around its own _locked call.
static void exec_one(conn_t *c, task_t *t) {
    pthread_mutex_lock(&c->db_mu);
    exec_one_locked(c, t);
    pthread_mutex_unlock(&c->db_mu);
}

static void query_one(conn_t *c, task_t *t) {
    pthread_mutex_lock(&c->db_mu);
    query_one_locked(c, t);
    pthread_mutex_unlock(&c->db_mu);
}

static void *worker_loop(void *arg) {
    conn_t *c = (conn_t *)arg;
    for (;;) {
        task_t *t = dequeue(c);
        if (t->op == OP_SHUTDOWN) {
            completion_signal(t->completion, SQLITE_OK, 0, NULL, NULL);
            free_task(t);
            break;
        }
        if (t->op == OP_QUERY) {
            query_one(c, t);
        } else {
            exec_one(c, t);
        }
        free_task(t);
    }
    return NULL;
}

// ---------------------------------------------------------------------------
// Externs called from Tin.
// ---------------------------------------------------------------------------

// _tin_sqlite_open: open the database file and spawn its worker.
// Returns the conn_t* (opaque to Tin) or NULL on open failure.  The
// caller (open() in sqlite.tin) wraps this in a Result and owns the
// pointer via *rc::Cell with _tin_sqlite_close as the dtor.
void *_tin_sqlite_open(const char *path) {
    conn_t *c = (conn_t *)calloc(1, sizeof(*c));
    if (!c) return NULL;
    if (sqlite3_open(path, &c->db) != SQLITE_OK) {
        if (c->db) sqlite3_close(c->db);
        free(c);
        return NULL;
    }
    pthread_mutex_init(&c->qmu, NULL);
    pthread_cond_init(&c->qcv, NULL);
    pthread_mutex_init(&c->db_mu, NULL);
    if (pthread_create(&c->worker, NULL, worker_loop, c) != 0) {
        sqlite3_close(c->db);
        pthread_mutex_destroy(&c->qmu);
        pthread_cond_destroy(&c->qcv);
        pthread_mutex_destroy(&c->db_mu);
        free(c);
        return NULL;
    }
    return c;
}

// _tin_sqlite_close: queue a shutdown task, await the worker, close
// the db, tear down the queue.  Idempotent on NULL.
void _tin_sqlite_close(void *conn) {
    if (!conn) return;
    conn_t *c = (conn_t *)conn;

    completion_t *done = completion_new();
    task_t *t = (task_t *)calloc(1, sizeof(*t));
    t->op = OP_SHUTDOWN;
    t->completion = done;
    enqueue(c, t);

    // Synchronous wait: close() is called from a dtor / explicit close,
    // both fiber-context paths.  Use the same park-then-take pattern.
    pthread_mutex_lock(&done->mu);
    while (!done->done) {
        // Foreign-thread style: capture pid + fib, unlock, park.
        // The dtor runs on a Tin fiber's worker thread, so these
        // primitives are safe.
        int64_t pid = _tin_current_pid();
        void   *fib = _tin_current_fib();
        done->waiter_pid = pid;
        done->waiter_fib = fib;
        pthread_mutex_unlock(&done->mu);
        _tin_fiber_park(pid);
        pthread_mutex_lock(&done->mu);
    }
    pthread_mutex_unlock(&done->mu);

    pthread_join(c->worker, NULL);
    // Finalize all cached prepared statements before closing the db
    // (sqlite3_close errors if any stmt is still open).
    stmt_cache_clear(c);
    sqlite3_close(c->db);
    pthread_mutex_destroy(&c->qmu);
    pthread_cond_destroy(&c->qcv);
    pthread_mutex_destroy(&c->db_mu);
    completion_free(done);
    free(c);
}

// _tin_sqlite_submit_exec: queue an exec task and return the
// completion handle.  Tin awaits on the completion via the
// SqliteCall awaitable.
//
// `tags` / `ints` / `texts` are borrowed pointers into the Tin
// caller's parallel arrays; the contents (including any TEXT data
// the texts pointers reference) are deep-copied into the task so
// the caller may drop its source arrays immediately.
void *_tin_sqlite_submit_exec(void *conn, const char *sql,
                              const int64_t *tags, const int64_t *ints,
                              const double *reals,
                              const char *const *texts, int64_t n_params) {
    conn_t *c = (conn_t *)conn;
    completion_t *done = completion_new();
    task_t *t = (task_t *)calloc(1, sizeof(*t));
    t->op = OP_EXEC;
    t->sql = sql ? strdup(sql) : NULL;
    t->n_params = n_params;
    if (n_params > 0) {
        t->tags  = (int64_t *)calloc((size_t)n_params, sizeof(int64_t));
        t->ints  = (int64_t *)calloc((size_t)n_params, sizeof(int64_t));
        t->reals = (double *)calloc((size_t)n_params, sizeof(double));
        t->texts = (char **)calloc((size_t)n_params, sizeof(char *));
        for (int64_t i = 0; i < n_params; i++) {
            t->tags[i]  = tags[i];
            t->ints[i]  = ints[i];
            t->reals[i] = reals[i];
            if (tags[i] == 3 && texts[i]) {
                t->texts[i] = strdup(texts[i]);
            }
        }
    }
    t->completion = done;
    enqueue(c, t);
    return done;
}

// _tin_sqlite_exec_blocking: synchronous exec on the caller's thread.
// Bypasses the worker queue + completion + fiber park/unpark cycle.
// Takes db_mu (uncontended when no async traffic is in flight) and
// uses the same stmt cache the worker does.
//
// Returns sqlite3_changes() on success, or -(sqlite_error_code) on
// failure.  Callers that need the error string can fetch it via
// _tin_sqlite_last_blocking_err -- but for the common cached-stmt
// fast path the rc itself is enough.
//
// Use only when the caller knows the call will be fast (cached stmt,
// no long-running SELECT).  Otherwise the calling fiber's worker
// thread is stuck inside sqlite for the duration.
int64_t _tin_sqlite_exec_blocking(void *conn, const char *sql,
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
                      texts ? (char *)texts[i] : NULL);
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

// _tin_sqlite_completion_ready: non-blocking poll for the
// awaitable::ready check.
int _tin_sqlite_completion_ready(void *p) {
    completion_t *c = (completion_t *)p;
    pthread_mutex_lock(&c->mu);
    int d = c->done;
    pthread_mutex_unlock(&c->mu);
    return d;
}

// _tin_sqlite_completion_take: park the caller until the completion is
// done, then return the result code packed with the i64 result.
// Returns the result_i64 on success, or -(result_code) on error so the
// Tin side can dispatch.  err msg is read separately via _err.
int64_t _tin_sqlite_completion_take(void *p) {
    completion_t *c = (completion_t *)p;
    pthread_mutex_lock(&c->mu);
    while (!c->done) {
        int64_t pid = _tin_current_pid();
        void   *fib = _tin_current_fib();
        c->waiter_pid = pid;
        c->waiter_fib = fib;
        pthread_mutex_unlock(&c->mu);
        _tin_fiber_park(pid);
        pthread_mutex_lock(&c->mu);
    }
    int64_t rv = c->result_i64;
    int code = c->result_code;
    pthread_mutex_unlock(&c->mu);
    if (code != SQLITE_OK) return -(int64_t)code;
    return rv;
}

// _tin_sqlite_completion_err: return the error string from the
// completion (transfers ownership: caller must free, or pair with
// _tin_string_from_bytes which copies).  Returns NULL if no error.
const char *_tin_sqlite_completion_err(void *p) {
    completion_t *c = (completion_t *)p;
    pthread_mutex_lock(&c->mu);
    const char *e = c->result_err;
    pthread_mutex_unlock(&c->mu);
    return e ? e : "";
}

// _tin_sqlite_completion_free: release the completion after the Tin
// awaitable has consumed the result.  The completion is rc::Cell-
// managed on the Tin side.  The contained result_ptr (a query
// ResultSet) is NOT freed here; ownership transferred to the Tin
// Rows handle via _tin_sqlite_completion_take_ptr.
void _tin_sqlite_completion_free(void *p) {
    completion_free((completion_t *)p);
}

// _tin_sqlite_submit_query: queue a SELECT-shaped task.  Same param
// marshalling as submit_exec.  Result is a `result_set_t*` retrieved
// via _tin_sqlite_completion_take_ptr after the awaitable resolves.
void *_tin_sqlite_submit_query(void *conn, const char *sql,
                               const int64_t *tags, const int64_t *ints,
                               const double *reals,
                               const char *const *texts, int64_t n_params) {
    conn_t *c = (conn_t *)conn;
    completion_t *done = completion_new();
    task_t *t = (task_t *)calloc(1, sizeof(*t));
    t->op = OP_QUERY;
    t->sql = sql ? strdup(sql) : NULL;
    t->n_params = n_params;
    if (n_params > 0) {
        t->tags  = (int64_t *)calloc((size_t)n_params, sizeof(int64_t));
        t->ints  = (int64_t *)calloc((size_t)n_params, sizeof(int64_t));
        t->reals = (double *)calloc((size_t)n_params, sizeof(double));
        t->texts = (char **)calloc((size_t)n_params, sizeof(char *));
        for (int64_t i = 0; i < n_params; i++) {
            t->tags[i]  = tags[i];
            t->ints[i]  = ints[i];
            t->reals[i] = reals[i];
            if (tags[i] == 3 && texts[i]) {
                t->texts[i] = strdup(texts[i]);
            }
        }
    }
    t->completion = done;
    enqueue(c, t);
    return done;
}

// _tin_sqlite_completion_take_ptr: park on the completion (like
// _take) and return the raw result_ptr (the result_set_t*).  Used by
// the SqliteCall[Rows] await path.  Ownership transfers to the
// caller; the matching _free is _tin_sqlite_rows_free.
void *_tin_sqlite_completion_take_ptr(void *p) {
    completion_t *c = (completion_t *)p;
    pthread_mutex_lock(&c->mu);
    while (!c->done) {
        int64_t pid = _tin_current_pid();
        void   *fib = _tin_current_fib();
        c->waiter_pid = pid;
        c->waiter_fib = fib;
        pthread_mutex_unlock(&c->mu);
        _tin_fiber_park(pid);
        pthread_mutex_lock(&c->mu);
    }
    void *rv = c->result_ptr;
    pthread_mutex_unlock(&c->mu);
    return rv;
}

// ---------------------------------------------------------------------------
// ResultSet accessors (called by Rows.get_* on the Tin side).
// All reads are pure memcpy; no awaits, no mutex.
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

// Writes a fresh Tin `string` copy of the cell's TEXT into *out_str.
// Out-param matches the runtime convention for C functions returning
// a `string` (the TinString fat-ptr return doesn't round-trip through
// Tin's `string`-return extern ABI).  See stdlib/net/dns/dns.c for
// the same pattern.
typedef struct { char *ptr; int64_t len; int64_t cap; } _TinString;
extern _TinString _tin_string_from_bytes(const char *ptr, int64_t len);

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
