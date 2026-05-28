// libs/redis/redis.c -- blocking Redis client bindings on top of
// hiredis's synchronous API.
//
// Both connect and each command roundtrip block the worker thread
// they run on; fibers sharing the same worker pause until the
// roundtrip returns.  hiredis ships an event-loop-shaped async API
// too, but bolting it onto Tin's M:N scheduler is a separate
// project: the runtime would need a per-command yield point that
// can be expressed from C, and we'd need {#async} Tin wrappers that
// drive a state machine.  Out of scope for the first cut; matches
// the libs/sqlite design (which is also frankly blocking).

#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <hiredis/hiredis.h>

// Runtime helpers (see runtime/runtime.h).
typedef struct { char *ptr; int64_t len; int64_t cap; } _TinString;

extern _TinString _tin_string_from_bytes(const char *ptr, int64_t len);
extern void      *_tin_rc_alloc(int64_t size);
extern void       _tin_release(void *p);

// Connection lifecycle

void *_tin_redis_connect(const char *host, int32_t port) {
    struct timeval tv = { .tv_sec = 5, .tv_usec = 0 };
    redisContext *c = redisConnectWithTimeout(host, (int)port, tv);
    if (!c) return NULL;
    if (c->err) {
        redisFree(c);
        return NULL;
    }
    return c;
}

void _tin_redis_close(void *ctx) {
    if (ctx) redisFree((redisContext *)ctx);
}

const char *_tin_redis_errstr(void *ctx) {
    redisContext *c = (redisContext *)ctx;
    if (!c) return "redis: null context";
    return c->errstr[0] ? c->errstr : "";
}

// Reply marshalling: hiredis reply -> TinRedisReply
//
// TinRedisReply mirrors REDIS_REPLY_* variants without leaking the
// hiredis layout to the Tin side.  Tin's `data Value` matches on
// `kind`:
//
//   kind=0  Nil
//   kind=1  Status(str)
//   kind=2  Err(str)              (lifted to Result::Err by translate_reply)
//   kind=3  Int(i64)
//   kind=4  Str(str)              (REDIS_REPLY_STRING / VERB / BIGNUM / DOUBLE-as-string)
//   kind=5  List([Value])
//
// We collapse Array / Set / Map / Push to kind=5; HGETALL ships an
// alternating [k1, v1, k2, v2, ...] list that Tin folds into pairs
// via Value::as_string_pairs.
typedef struct TinRedisReply TinRedisReply;
struct TinRedisReply {
    int32_t kind;
    int64_t ival;
    _TinString sval;
    TinRedisReply **items;
    int64_t        nitems;
};

static TinRedisReply *_translate_reply(redisReply *r);

static TinRedisReply **_translate_array(redisReply *r) {
    if (r->elements == 0) return NULL;
    TinRedisReply **out = (TinRedisReply **)calloc(r->elements, sizeof(*out));
    if (!out) return NULL;
    for (size_t i = 0; i < r->elements; i++) {
        out[i] = _translate_reply(r->element[i]);
    }
    return out;
}

static TinRedisReply *_translate_reply(redisReply *r) {
    TinRedisReply *out = (TinRedisReply *)_tin_rc_alloc((int64_t)sizeof(TinRedisReply));
    if (!out) return NULL;
    out->kind = 0;
    out->ival = 0;
    out->sval.ptr = NULL;
    out->sval.len = 0;
    out->sval.cap = 0;
    out->items = NULL;
    out->nitems = 0;
    if (!r) {
        out->kind = 2;
        out->sval = _tin_string_from_bytes("redis: no reply", 15);
        return out;
    }
    switch (r->type) {
    case REDIS_REPLY_NIL:
        out->kind = 0;
        break;
    case REDIS_REPLY_STATUS:
        out->kind = 1;
        out->sval = _tin_string_from_bytes(r->str, (int64_t)r->len);
        break;
    case REDIS_REPLY_ERROR:
        out->kind = 2;
        out->sval = _tin_string_from_bytes(r->str, (int64_t)r->len);
        break;
    case REDIS_REPLY_INTEGER:
        out->kind = 3;
        out->ival = (int64_t)r->integer;
        break;
    case REDIS_REPLY_STRING:
    case REDIS_REPLY_VERB:
    case REDIS_REPLY_DOUBLE:
    case REDIS_REPLY_BIGNUM:
        out->kind = 4;
        out->sval = _tin_string_from_bytes(r->str, (int64_t)r->len);
        break;
    case REDIS_REPLY_ARRAY:
    case REDIS_REPLY_SET:
    case REDIS_REPLY_MAP:
    case REDIS_REPLY_ATTR:
    case REDIS_REPLY_PUSH:
        out->kind = 5;
        out->items = _translate_array(r);
        out->nitems = (int64_t)r->elements;
        break;
    default:
        out->kind = 2;
        out->sval = _tin_string_from_bytes("redis: unknown reply kind", 25);
        break;
    }
    return out;
}

// _tin_redis_command runs one Redis command described by an argv
// array.  Empty argv (argc=0) drives `redisGetReply` -- used by the
// subscription reader to drain the next pending push.  On hard
// transport error returns NULL and sets *err_out to -errno; the
// Tin side surfaces this as a RedisError variant.
TinRedisReply *_tin_redis_command(void *ctx, int32_t argc,
                                   const char **argv, const int64_t *arglens,
                                   int64_t *err_out) {
    redisContext *c = (redisContext *)ctx;
    *err_out = 0;
    if (!c) { *err_out = -EINVAL; return NULL; }

    redisReply *r = NULL;
    if (argc == 0) {
        if (redisGetReply(c, (void **)&r) == REDIS_ERR || !r) {
            *err_out = -(int64_t)EPROTO;
            return NULL;
        }
    } else {
        size_t *sizes = (size_t *)malloc(sizeof(size_t) * (size_t)argc);
        if (!sizes) { *err_out = -ENOMEM; return NULL; }
        for (int i = 0; i < argc; i++) sizes[i] = (size_t)arglens[i];
        r = (redisReply *)redisCommandArgv(c, (int)argc, argv, sizes);
        free(sizes);
        if (!r) {
            *err_out = -(int64_t)EPROTO;
            return NULL;
        }
    }

    TinRedisReply *out = _translate_reply(r);
    freeReplyObject(r);
    return out;
}

// _tin_redis_reply_free is the rc destructor for TinRedisReply.
void _tin_redis_reply_free(void *p) {
    if (!p) return;
    TinRedisReply *r = (TinRedisReply *)p;
    if (r->items) {
        for (int64_t i = 0; i < r->nitems; i++) {
            _tin_release(r->items[i]);
        }
        free(r->items);
    }
}

// Reply accessors -- called from Tin to drain a TinRedisReply into a
// Value ADT without exposing the C struct layout.

int32_t _tin_redis_reply_kind(void *p)  { return p ? ((TinRedisReply *)p)->kind  : 2; }
int64_t _tin_redis_reply_int(void *p)   { return p ? ((TinRedisReply *)p)->ival  : 0; }
int64_t _tin_redis_reply_count(void *p) { return p ? ((TinRedisReply *)p)->nitems: 0; }

void _tin_redis_reply_str(void *p, _TinString *out) {
    TinRedisReply *r = (TinRedisReply *)p;
    if (!r) { *out = _tin_string_from_bytes("", 0); return; }
    *out = _tin_string_from_bytes(r->sval.ptr, r->sval.len);
}

void *_tin_redis_reply_child(void *p, int64_t i) {
    TinRedisReply *r = (TinRedisReply *)p;
    if (!r || !r->items || i < 0 || i >= r->nitems) return NULL;
    return r->items[i];
}
