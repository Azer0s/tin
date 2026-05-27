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
    OP_EXEC = 1,
    OP_SHUTDOWN = 99,
} op_kind_t;

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
    int64_t result_i64;   // exec: changes(), query: row count, etc.
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
static void completion_signal(completion_t *c, int code, int64_t v, const char *err) {
    pthread_mutex_lock(&c->mu);
    c->result_code = code;
    c->result_i64  = v;
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
    // up.  Three parallel arrays match the wire format from
    // sqlite.tin::exec.  texts[i] is strdup'd when tag=3, NULL otherwise.
    int64_t         *tags;
    int64_t         *ints;
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
    if (t->texts) {
        for (int64_t i = 0; i < t->n_params; i++) {
            if (t->texts[i]) free(t->texts[i]);
        }
        free(t->texts);
    }
    free(t);
}

typedef struct conn {
    sqlite3        *db;
    pthread_t       worker;
    pthread_mutex_t qmu;
    pthread_cond_t  qcv;
    task_t         *head;
    task_t         *tail;
    int             stopped;  // worker exited
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
static int bind_one(sqlite3_stmt *stmt, int idx, int64_t tag, int64_t int_val, char *text_val) {
    switch (tag) {
    case 1: return sqlite3_bind_int64(stmt, idx, int_val);
    case 3: return sqlite3_bind_text(stmt, idx, text_val ? text_val : "", -1, SQLITE_TRANSIENT);
    case 5: return sqlite3_bind_null(stmt, idx);
    default: return SQLITE_MISUSE;
    }
}

static void exec_one(conn_t *c, task_t *t) {
    sqlite3_stmt *stmt = NULL;
    int rc = sqlite3_prepare_v2(c->db, t->sql, -1, &stmt, NULL);
    if (rc != SQLITE_OK) {
        completion_signal(t->completion, rc, 0, sqlite3_errmsg(c->db));
        if (stmt) sqlite3_finalize(stmt);
        return;
    }

    for (int64_t i = 0; i < t->n_params; i++) {
        rc = bind_one(stmt, (int)(i + 1),
                      t->tags[i],
                      t->ints[i],
                      t->texts ? t->texts[i] : NULL);
        if (rc != SQLITE_OK) {
            completion_signal(t->completion, rc, 0, sqlite3_errmsg(c->db));
            sqlite3_finalize(stmt);
            return;
        }
    }

    rc = sqlite3_step(stmt);
    if (rc != SQLITE_DONE && rc != SQLITE_ROW) {
        completion_signal(t->completion, rc, 0, sqlite3_errmsg(c->db));
        sqlite3_finalize(stmt);
        return;
    }
    // For OP_EXEC we don't iterate rows; report changes() and finalize.
    int64_t changes = (int64_t)sqlite3_changes(c->db);
    sqlite3_finalize(stmt);
    completion_signal(t->completion, SQLITE_OK, changes, NULL);
}

static void *worker_loop(void *arg) {
    conn_t *c = (conn_t *)arg;
    for (;;) {
        task_t *t = dequeue(c);
        if (t->op == OP_SHUTDOWN) {
            completion_signal(t->completion, SQLITE_OK, 0, NULL);
            free_task(t);
            break;
        }
        exec_one(c, t);
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
    if (pthread_create(&c->worker, NULL, worker_loop, c) != 0) {
        sqlite3_close(c->db);
        pthread_mutex_destroy(&c->qmu);
        pthread_cond_destroy(&c->qcv);
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
    sqlite3_close(c->db);
    pthread_mutex_destroy(&c->qmu);
    pthread_cond_destroy(&c->qcv);
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
        t->texts = (char **)calloc((size_t)n_params, sizeof(char *));
        for (int64_t i = 0; i < n_params; i++) {
            t->tags[i] = tags[i];
            t->ints[i] = ints[i];
            if (tags[i] == 3 && texts[i]) {
                t->texts[i] = strdup(texts[i]);
            }
        }
    }
    t->completion = done;
    enqueue(c, t);
    return done;
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
// managed on the Tin side.
void _tin_sqlite_completion_free(void *p) {
    completion_free((completion_t *)p);
}
