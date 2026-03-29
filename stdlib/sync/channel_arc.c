// tin stdlib/sync - Full channel implementation with all state in one heap block.
//
// Channel[T] in Tin is a thin wrapper around a TinChannel* (void*). Copying the
// Tin struct copies only the pointer, so all copies share the same ring buffer,
// mutex, and condition variables. This gives Go-channel-style reference semantics
// within Tin's value-type system.
//
// Thread safety: all operations are protected by the embedded pthread mutex.
// The `is_rc` flag is 1 when T is an RC-tracked type; retain/release is applied
// when elements are stored into or loaded from the ring buffer.

#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>
#include <pthread.h>
#include <stdio.h>

// Forward-declare ARC functions from runtime.h
void _tin_retain(void *ptr);
void _tin_release(void *ptr);

// ---------------------------------------------------------------------------
// Internal channel control block
// ---------------------------------------------------------------------------

// Sentinel value returned by _tin_channel_recv_park when the channel was empty
// and the caller should yield and retry. Never a valid data pointer.
#define TIN_CHAN_BLOCKED ((void*)(intptr_t)-1)

typedef struct TinChannel {
    pthread_mutex_t mu;
    pthread_cond_t  not_full;
    pthread_cond_t  not_empty;
    int64_t         cap;
    int64_t         elem_size;
    int64_t         count;
    int64_t         head;
    int64_t         tail;
    bool            closed;
    int             is_rc;    // whether T is RC-tracked
    char            buf[];    // flexible array member: cap * elem_size bytes
} TinChannel;

// Allocate and initialise a new channel control block.
// is_rc must be 1 when T is an ARC-tracked type (string, array, any) so that
// send retains and recv transfers ownership correctly.
void *_tin_channel_new(int64_t cap, int64_t elem_size, int is_rc) {
    if (cap <= 0) cap = 1;
    size_t total = sizeof(TinChannel) + (size_t)(cap * elem_size);
    TinChannel *ch = (TinChannel *)calloc(1, total);
    if (!ch) { fputs("tin: channel alloc failed\n", stderr); exit(1); }
    pthread_mutex_init(&ch->mu, NULL);
    pthread_cond_init(&ch->not_full, NULL);
    pthread_cond_init(&ch->not_empty, NULL);
    ch->cap       = cap;
    ch->elem_size = elem_size;
    ch->count     = 0;
    ch->head      = 0;
    ch->tail      = 0;
    ch->closed    = false;
    ch->is_rc     = is_rc;
    return ch;
}

// Free the channel control block, releasing any RC items still in the buffer.
void _tin_channel_free(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;
    // Drain remaining RC elements.
    if (ch->is_rc) {
        for (int64_t i = 0; i < ch->count; i++) {
            int64_t slot = (ch->head + i) % ch->cap;
            void *slot_ptr = ch->buf + slot * ch->elem_size;
            void *rc_ptr;
            memcpy(&rc_ptr, slot_ptr, sizeof(void *));
            if (rc_ptr) _tin_release(rc_ptr);
        }
    }
    pthread_cond_destroy(&ch->not_empty);
    pthread_cond_destroy(&ch->not_full);
    pthread_mutex_destroy(&ch->mu);
    free(ch);
}

// ---------------------------------------------------------------------------
// Per-thread scratch buffer for _tin_channel_recv_park.
//
// We use a pthread_key_t so the destructor runs when the worker thread exits,
// freeing the buffer without any extra call from the worker loop.
// ---------------------------------------------------------------------------

typedef struct RecvBuf {
    uint8_t *data;
    size_t   sz;
} RecvBuf;

static pthread_key_t _recv_buf_key;
static pthread_once_t _recv_buf_once = PTHREAD_ONCE_INIT;

static void _recv_buf_dtor(void *p) {
    if (!p) return;
    RecvBuf *rb = (RecvBuf *)p;
    free(rb->data);
    free(rb);
}

// atexit handler: pthread_key_t destructors do not run for the main thread
// when exit() is called via return from main(). Free the main thread's buffer
// here so valgrind and sanitizers see a clean heap at exit.
static void _recv_buf_cleanup_atexit(void) {
    void *p = pthread_getspecific(_recv_buf_key);
    if (p) {
        pthread_setspecific(_recv_buf_key, NULL);
        _recv_buf_dtor(p);
    }
}

static void _recv_buf_init_key(void) {
    pthread_key_create(&_recv_buf_key, _recv_buf_dtor);
    atexit(_recv_buf_cleanup_atexit);
}

static RecvBuf *_recv_buf_get(void) {
    RecvBuf *rb = (RecvBuf *)pthread_getspecific(_recv_buf_key);
    if (!rb) {
        rb = (RecvBuf *)calloc(1, sizeof(RecvBuf));
        pthread_setspecific(_recv_buf_key, rb);
    }
    return rb;
}

// Dequeue one element from ch into the per-thread scratch buffer.
// Must be called with ch->mu held and ch->count > 0.
// Returns pointer to the thread-local scratch buffer containing the element.
static void *_chan_dequeue(TinChannel *ch) {
    pthread_once(&_recv_buf_once, _recv_buf_init_key);
    RecvBuf *rb = _recv_buf_get();
    // Grow scratch buffer if needed.
    if ((size_t)ch->elem_size > rb->sz) {
        free(rb->data);
        rb->data = malloc((size_t)ch->elem_size);
        rb->sz   = (size_t)ch->elem_size;
    }
    uint8_t *buf = rb->data;
    void *src = ch->buf + ch->head * ch->elem_size;
    memcpy(buf, src, (size_t)ch->elem_size);
    // Transfer ownership: zero the buffer slot so ARC doesn't double-release.
    if (ch->is_rc) {
        memset(src, 0, (size_t)ch->elem_size);
    }
    ch->head  = (ch->head + 1) % ch->cap;
    ch->count--;
    return buf;
}

// Fiber-aware recv. Takes a pid from the calling fiber (or -1 if not in a fiber).
// Fiber-aware recv: try to dequeue one element from the channel.
// pid is the calling fiber's pid (used for future park/unpark optimization;
// currently unused to avoid a park-before-suspend double-resume race).
//
// Returns:
//   - pointer to element data  - element was dequeued successfully
//   - NULL                     - channel is closed and empty (caller should panic)
//   - TIN_CHAN_BLOCKED          - channel was empty; caller should yield and retry
//
// Safety note on park/unpark: we intentionally do NOT call _tin_fiber_park here.
// Calling park before the coroutine's coro.suspend opens a double-resume window:
// a producer can call _tin_fiber_unpark between the park call and coro.suspend,
// making another worker pick up the fiber handle before the current worker's
// _coro_resume returns.  The yield-retry loop in recv_fiber is safe without park.
void *_tin_channel_recv_park(void *ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;
    (void)pid;  // reserved for future optimization

    // Use trylock so the calling OS worker thread is never parked.
    // If the mutex is contended, return BLOCKED so the fiber yields and retries.
    if (pthread_mutex_trylock(&ch->mu) != 0) {
        return TIN_CHAN_BLOCKED;  // mutex contended - yield and retry
    }

    if (ch->count > 0) {
        void *data = _chan_dequeue(ch);
        pthread_mutex_unlock(&ch->mu);
        return data;
    }

    if (ch->closed) {
        pthread_mutex_unlock(&ch->mu);
        return NULL;  // caller will panic
    }

    // Channel is empty and open: tell caller to yield and retry.
    pthread_mutex_unlock(&ch->mu);
    return TIN_CHAN_BLOCKED;
}

// Returns the sentinel value TIN_CHAN_BLOCKED cast to i64.
// Used by Tin code to compare the return of _tin_channel_recv_park.
int64_t _tin_channel_recv_blocked_val(void) {
    return (int64_t)(intptr_t)TIN_CHAN_BLOCKED;  // -1
}

// Fiber-aware try-send: attempt to enqueue one element without blocking.
// Uses pthread_mutex_trylock so the calling OS worker thread is never parked.
//
// Returns:
//   0   - element enqueued successfully
//   1   - channel full or mutex contended; caller should yield and retry
//  -1   - channel is closed; caller should panic
int _tin_channel_send_try(void *ptr, const void *val, int64_t elem_size, int is_rc) {
    TinChannel *ch = (TinChannel *)ptr;

    if (pthread_mutex_trylock(&ch->mu) != 0) {
        return 1;  // mutex contended - yield and retry
    }

    if (ch->closed) {
        pthread_mutex_unlock(&ch->mu);
        return -1;
    }

    if (ch->count >= ch->cap) {
        pthread_mutex_unlock(&ch->mu);
        return 1;  // full - yield and retry
    }

    void *dest = ch->buf + ch->tail * elem_size;
    if (is_rc) {
        // Release any previous occupant (should be zeroed, but be defensive).
        void *old_ptr;
        memcpy(&old_ptr, dest, sizeof(void *));
        if (old_ptr) _tin_release(old_ptr);
    }
    memcpy(dest, val, (size_t)elem_size);
    if (is_rc) {
        void *new_ptr;
        memcpy(&new_ptr, dest, sizeof(void *));
        if (new_ptr) _tin_retain(new_ptr);
    }
    ch->tail  = (ch->tail + 1) % ch->cap;
    ch->count++;
    pthread_mutex_unlock(&ch->mu);
    return 0;
}

// Returns 1 if the channel is closed and empty, 0 otherwise.
int _tin_channel_is_closed_empty(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return 1;
    pthread_mutex_lock(&ch->mu);
    int r = (ch->closed && ch->count == 0) ? 1 : 0;
    pthread_mutex_unlock(&ch->mu);
    return r;
}

// Close the channel. After this, sends return -1; pending and future recvs
// drain remaining items then return zero values.
void _tin_channel_close(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;
    pthread_mutex_lock(&ch->mu);
    ch->closed = true;
    pthread_cond_broadcast(&ch->not_empty);
    pthread_cond_broadcast(&ch->not_full);
    pthread_mutex_unlock(&ch->mu);
}

// ---------------------------------------------------------------------------
// Legacy ring-buffer helpers (kept for compatibility with existing codegen)
// These operate on a raw pre-allocated buffer, not a TinChannel control block.
// ---------------------------------------------------------------------------

void _tin_chan_buf_store(void *buf, int64_t slot, const void *elem,
                         int64_t elem_size, int is_rc) {
    void *dest = (char *)buf + slot * elem_size;
    if (is_rc) {
        void *old_ptr;
        memcpy(&old_ptr, dest, sizeof(void *));
        if (old_ptr) _tin_release(old_ptr);
    }
    memcpy(dest, elem, (size_t)elem_size);
    if (is_rc) {
        void *new_ptr;
        memcpy(&new_ptr, dest, sizeof(void *));
        if (new_ptr) _tin_retain(new_ptr);
    }
}

void _tin_chan_buf_load(const void *buf, int64_t slot, void *out,
                        int64_t elem_size, int is_rc) {
    const void *src = (const char *)buf + slot * elem_size;
    memcpy(out, src, (size_t)elem_size);
    if (is_rc) {
        memset((void *)src, 0, (size_t)elem_size);
    }
}

void _tin_chan_buf_drain(void *buf, int64_t cap, int64_t head, int64_t count,
                         int64_t elem_size, int is_rc) {
    if (!is_rc) return;
    for (int64_t i = 0; i < count; i++) {
        int64_t slot = (head + i) % cap;
        void *slot_ptr = (char *)buf + slot * elem_size;
        void *rc_ptr;
        memcpy(&rc_ptr, slot_ptr, sizeof(void *));
        if (rc_ptr) _tin_release(rc_ptr);
    }
}
