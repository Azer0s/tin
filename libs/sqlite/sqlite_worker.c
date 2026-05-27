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
    op_kind_t     op;
    char         *sql;        // strdup'd; freed by worker after exec
    completion_t *completion; // signalled by worker; not freed here
    struct task  *next;
} task_t;

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

static void exec_one(conn_t *c, task_t *t) {
    char *err = NULL;
    int rc = sqlite3_exec(c->db, t->sql, NULL, NULL, &err);
    int64_t changes = (int64_t)sqlite3_changes(c->db);
    completion_signal(t->completion, rc, changes, err);
    if (err) sqlite3_free(err);
}

static void *worker_loop(void *arg) {
    conn_t *c = (conn_t *)arg;
    for (;;) {
        task_t *t = dequeue(c);
        if (t->op == OP_SHUTDOWN) {
            completion_signal(t->completion, SQLITE_OK, 0, NULL);
            if (t->sql) free(t->sql);
            free(t);
            break;
        }
        exec_one(c, t);
        if (t->sql) free(t->sql);
        free(t);
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
void *_tin_sqlite_submit_exec(void *conn, const char *sql) {
    conn_t *c = (conn_t *)conn;
    completion_t *done = completion_new();
    task_t *t = (task_t *)calloc(1, sizeof(*t));
    t->op = OP_EXEC;
    t->sql = sql ? strdup(sql) : NULL;
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
