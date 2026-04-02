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
#include <stdatomic.h>

// Forward-declare ARC functions from runtime.h
void _tin_retain(void *ptr);
void _tin_release(void *ptr);

// Forward-declare fiber park/unpark/hdl from fiber.h
void    _tin_fiber_park(int64_t pid);
void    _tin_fiber_unpark(int64_t pid);
void    _tin_fiber_unpark_hdl(int64_t pid, void *hdl);
// Inline TLS reads for current coro hdl and fib — avoids function-call
// overhead in the hot send/recv path (cross-TU, so otherwise not inlinable).
// Eliminates 2 PLT calls + double-load of recv_waiter_cnt per park operation.
extern __thread void *_current_hdl;
static inline void *_tin_current_coro_hdl(void) { return _current_hdl; }
struct TinFiber;  // opaque forward; only used as void* in waiter arrays
extern __thread struct TinFiber *_current_fib;
static inline void *_tin_current_fib(void) { return (void *)_current_fib; }
// Direct-recv flag: set by the worker loop (fiber.c) when the fiber being
// resumed had data delivered directly to its out buffer by a sender.
// recv_direct checks this at entry to skip mutex+dequeue on the retry path.
extern __thread int _direct_recv_flag;
void    _tin_fiber_set_direct_recv(void *fib);
void    _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl);

// TinFastMutex: coroutine-aware atomic spinlock (no OS primitives in fast path).
// Defined in runtime/fastmutex.c, included via the runtime umbrella.
#include "../../runtime/fastmutex.h"

// ---------------------------------------------------------------------------
// Fast element copy — avoids calling library memcpy for common small sizes.
// For 8-byte elements (int64, pointers, f64), the compiler inlines this as a
// single 64-bit load+store, eliminating the __memcpy_avx_unaligned_erms overhead.
// ---------------------------------------------------------------------------
static inline void _chan_elem_copy(void *dst, const void *src, size_t sz) {
    if (__builtin_expect(sz == 8, 1))
        __builtin_memcpy(dst, src, 8);
    else if (sz == 4)
        __builtin_memcpy(dst, src, 4);
    else if (sz == 16)
        __builtin_memcpy(dst, src, 16);
    else
        memcpy(dst, src, sz);
}

// ---------------------------------------------------------------------------
// Internal channel control block
// ---------------------------------------------------------------------------

// Sentinel returned when a channel operation parked the fiber.
// The caller must yield immediately after receiving this.  Never a valid data pointer.
#define TIN_CHAN_BLOCKED ((void*)(intptr_t)-1)

// Maximum fibers that can wait on a single channel at once.
// Excess waiters fall back to the yield-retry path (still correct, just slower).
// Keep this small (8) so the TinChannel struct fits in a few cache lines —
// the hot fields (lock state, count, head, tail) must stay within the first
// 64-byte cache line to avoid costly cache misses on every operation.
#define TIN_CHAN_MAX_WAITERS 8

// TinChannel layout — cache-line conscious:
//   Bytes   0..3   : ref_count      (ARC, rarely touched in fast path)
//   Bytes   4..7   : fmu.state      (lock CAS — hit on every lock/unlock)
//   Bytes   8..11  : fmu.wl_lock    (waiter-list spinlock — rare contended path)
//   Bytes  12..15  : fmu.wcnt       (mutex waiter count)
//   Bytes  16..79  : fmu.wpid[4]    (mutex waiters pids — rarely used)
//   Bytes  80..111 : fmu.whdl[4]    (mutex waiter hdls — rarely used)
//   — after TinFastMutex (FMUTEX_MAX_WAITERS=4 → 112 bytes total for fmu) —
//   Bytes 112..119 : cap            (hot: read on every enqueue/dequeue check)
//   Bytes 120..127 : elem_size
//   Bytes 128..135 : count          (hot)
//   Bytes 136..143 : head           (hot)
//   Bytes 144..151 : tail           (hot)
//   Bytes 152..153 : closed, is_rc
//   Bytes 154+     : waiter arrays, buf[]
//
// With FMUTEX_MAX_WAITERS=4 the fmu is 4+4+4+4*8+4*8 = 80 bytes.
// The hot ring-buffer fields sit at offset ~80–152, inside the first 3 cache lines.
typedef struct TinChannel {
    atomic_int      ref_count; // number of Channel[T] struct copies alive
    TinFastMutex    fmu;       // protects all fields below (no OS primitives)
    int64_t         cap;
    int64_t         cap_mask;  // cap - 1 (cap is always power-of-2); use & instead of %
    int64_t         elem_size;
    int64_t         count;
    int64_t         head;
    int64_t         tail;
    bool            closed;
    int             is_rc;    // whether T is RC-tracked
    // Park/unpark waiter queues.  Fibers park here instead of spin-yielding.
    // pid + hdl + fib are stored so _tin_fiber_unpark_fib can bypass _table_mu
    // entirely on the unpark hot path (only the per-fiber spinlock is needed).
    int64_t recv_waiters[TIN_CHAN_MAX_WAITERS];
    void   *recv_waiter_hdls[TIN_CHAN_MAX_WAITERS];
    void   *recv_waiter_fibs[TIN_CHAN_MAX_WAITERS];
    void   *recv_waiter_outs[TIN_CHAN_MAX_WAITERS]; // direct delivery target buffers
    int     recv_waiter_cnt;
    int64_t send_waiters[TIN_CHAN_MAX_WAITERS];
    void   *send_waiter_hdls[TIN_CHAN_MAX_WAITERS];
    void   *send_waiter_fibs[TIN_CHAN_MAX_WAITERS];
    int     send_waiter_cnt;
    char            buf[];    // flexible array member: cap * elem_size bytes
} TinChannel;

// Allocate and initialise a new channel control block.
// is_rc must be 1 when T is an ARC-tracked type (string, array, any) so that
// send retains and recv transfers ownership correctly.
void *_tin_channel_new(int64_t cap, int64_t elem_size, int is_rc) {
    if (cap <= 0) cap = 1;
    // Round cap up to the next power of 2 so head/tail wrap can use bitwise AND
    // instead of integer division (idivq = ~30-90 cycles vs andq = 1 cycle).
    int64_t po2 = 1;
    while (po2 < cap) po2 <<= 1;
    cap = po2;
    size_t total = sizeof(TinChannel) + (size_t)(cap * elem_size);
    TinChannel *ch = (TinChannel *)calloc(1, total);
    if (!ch) { fputs("tin: channel alloc failed\n", stderr); exit(1); }
    atomic_init(&ch->ref_count, 1);
    tin_fmutex_init(&ch->fmu);
    ch->cap       = cap;
    ch->cap_mask  = cap - 1;
    ch->elem_size = elem_size;
    ch->count     = 0;
    ch->head      = 0;
    ch->tail      = 0;
    ch->closed    = false;
    ch->is_rc     = is_rc;
    return ch;
}

// Increment the reference count. Called when a Channel[T] value crosses a
// fiber boundary (e.g. passed as a spawn argument) so the fiber owns its own
// reference independent of the caller's local variable.
void _tin_channel_retain(void *ptr) {
    if (!ptr) return;
    TinChannel *ch = (TinChannel *)ptr;
    atomic_fetch_add_explicit(&ch->ref_count, 1, memory_order_relaxed);
}

// Release one reference to the channel. When the last reference is dropped the
// control block is destroyed and freed. Matching function for _tin_channel_retain
// and the implicit retain performed by Channel[T].make().
void _tin_channel_free(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;
    if (atomic_fetch_sub_explicit(&ch->ref_count, 1, memory_order_acq_rel) > 1) {
        return; // other references still alive
    }
    // Last reference dropped: drain buffered RC elements and destroy.
    if (ch->is_rc) {
        for (int64_t i = 0; i < ch->count; i++) {
            int64_t slot = (ch->head + i) & ch->cap_mask;
            void *slot_ptr = ch->buf + slot * ch->elem_size;
            void *rc_ptr;
            memcpy(&rc_ptr, slot_ptr, sizeof(void *));
            if (rc_ptr) _tin_release(rc_ptr);
        }
    }
    // TinFastMutex uses only atomic fields; no destructor needed.
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

static pthread_key_t  _recv_buf_key;
static pthread_once_t _recv_buf_once = PTHREAD_ONCE_INIT;
// TLS flag: set to 1 after _recv_buf_init_key has run on this thread.
// Avoids calling pthread_once (a full memory barrier) on every dequeue.
static __thread int   _recv_buf_key_ready = 0;

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
// Must be called with ch->fmu locked and ch->count > 0.
// Returns pointer to the thread-local scratch buffer containing the element.
static void *_chan_dequeue(TinChannel *ch) {
    if (__builtin_expect(!_recv_buf_key_ready, 0)) {
        pthread_once(&_recv_buf_once, _recv_buf_init_key);
        _recv_buf_key_ready = 1;
    }
    RecvBuf *rb = _recv_buf_get();
    // Grow scratch buffer if needed.
    if ((size_t)ch->elem_size > rb->sz) {
        free(rb->data);
        rb->data = malloc((size_t)ch->elem_size);
        rb->sz   = (size_t)ch->elem_size;
    }
    uint8_t *buf = rb->data;
    size_t esz = (size_t)ch->elem_size;
    void *src = ch->buf + ch->head * esz;
    _chan_elem_copy(buf, src, esz);
    // Transfer ownership: zero the buffer slot so ARC doesn't double-release.
    if (ch->is_rc) {
        memset(src, 0, esz);
    }
    ch->head  = (ch->head + 1) & ch->cap_mask;
    ch->count--;
    return buf;
}

// ---------------------------------------------------------------------------
// Blocking send/recv using TinFastMutex + fiber park/unpark.
//
// The channel's lock (ch->fmu) is a coroutine-aware atomic spinlock.
// Uncontended acquire is a single CAS — no OS call, no syscall.
// When contended after a brief spin, the fiber parks into the mutex's own
// waiter list and is resumed directly by the unlocker (runnext fast path).
//
// Both pid AND hdl are stored in the data-waiter lists (recv_waiters /
// send_waiters) so that the unpark hot path (_tin_fiber_unpark_hdl) can skip
// the fiber-table lookup that _tin_fiber_unpark needs.
//
// Common paths (no contention, data available): 0 yields.
// Blocked paths: 1 yield (data park) + 1 yield-free retry (dequeue).
// ---------------------------------------------------------------------------

// Blocking recv.  Returns:
//   data pointer (per-thread scratch buf) - element dequeued
//   NULL                                  - channel closed and empty (panic)
//   TIN_CHAN_BLOCKED                       - fiber parked; caller must yield
void *_tin_channel_recv_blocking(void *ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;

    void *hdl = _tin_current_coro_hdl();
    if (!tin_fmutex_lock_coro(&ch->fmu, pid, hdl))
        return TIN_CHAN_BLOCKED;  // parked in mutex wait queue; caller yields

    if (ch->count > 0) {
        void *data = _chan_dequeue(ch);
        // Wake one parked sender now that there is space.
        if (ch->send_waiter_cnt > 0) {
            ch->send_waiter_cnt--;
            int64_t spid = ch->send_waiters[ch->send_waiter_cnt];
            void   *shdl = ch->send_waiter_hdls[ch->send_waiter_cnt];
            void   *sfib = ch->send_waiter_fibs[ch->send_waiter_cnt];
            tin_fmutex_unlock(&ch->fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
            return data;
        }
        tin_fmutex_unlock(&ch->fmu);
        return data;
    }

    if (ch->closed) {
        tin_fmutex_unlock(&ch->fmu);
        return NULL;
    }

    // Channel empty: register as a data waiter and park.
    // _tin_fiber_park uses the deferred-BLOCKED pattern (sets pending_park so
    // the worker loop transitions to FIBER_BLOCKED after coro.suspend fires).
    // This eliminates the double-resume race if a sender unparks us between
    // _tin_fiber_park and the subsequent yield.
    if (pid > 0 && hdl && ch->recv_waiter_cnt < TIN_CHAN_MAX_WAITERS) {
        ch->recv_waiters[ch->recv_waiter_cnt]     = pid;
        ch->recv_waiter_hdls[ch->recv_waiter_cnt] = hdl;
        ch->recv_waiter_fibs[ch->recv_waiter_cnt] = _tin_current_fib();
        ch->recv_waiter_cnt++;
        tin_fmutex_unlock(&ch->fmu);
        _tin_fiber_park(pid);
        return TIN_CHAN_BLOCKED;
    }

    // Waiter list full or no pid/hdl: fall back to yield-retry.
    tin_fmutex_unlock(&ch->fmu);
    return TIN_CHAN_BLOCKED;
}

// Blocking send.  Returns:
//   0   - element enqueued
//  -1   - channel closed (caller should panic)
//   1   - fiber parked or mutex contended; caller must yield and retry
int _tin_channel_send_blocking(void *ptr, const void *val,
                                int64_t elem_size, int is_rc, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;

    void *hdl = _tin_current_coro_hdl();
    if (!tin_fmutex_lock_coro(&ch->fmu, pid, hdl))
        return 1;  // parked in mutex wait queue; caller yields

    if (ch->closed) {
        tin_fmutex_unlock(&ch->fmu);
        return -1;
    }

    if (ch->count < ch->cap) {
        size_t esz  = (size_t)elem_size;

        // Direct delivery: if a receiver parked via recv_direct (rout != NULL),
        // skip the ring buffer and write directly to its out buffer.  Eliminates
        // the receiver's retry call (2 LOCK CAS + mutex overhead).
        // If rout == NULL the receiver used recv_blocking: enqueue to ring buffer
        // so it can dequeue on its normal retry path, then unpark it.
        if (ch->recv_waiter_cnt > 0) {
            ch->recv_waiter_cnt--;
            int64_t rpid = ch->recv_waiters[ch->recv_waiter_cnt];
            void   *rhdl = ch->recv_waiter_hdls[ch->recv_waiter_cnt];
            void   *rfib = ch->recv_waiter_fibs[ch->recv_waiter_cnt];
            void   *rout = ch->recv_waiter_outs[ch->recv_waiter_cnt];
            if (rout) {
                // recv_direct receiver: write directly to its alloca.
                _chan_elem_copy(rout, val, esz);
                if (is_rc) {
                    void *new_ptr;
                    memcpy(&new_ptr, rout, sizeof(void *));
                    if (new_ptr) _tin_retain(new_ptr);
                }
                tin_fmutex_unlock(&ch->fmu);
                _tin_fiber_set_direct_recv(rfib);  // must be before unpark for ordering
                _tin_fiber_unpark_fib(rfib, rpid, rhdl);
                return 0;
            }
            // recv_blocking receiver: enqueue, then wake so it can dequeue.
            {
                size_t tail = (size_t)ch->tail;
                void *dest = ch->buf + tail * esz;
                if (is_rc) {
                    void *old_ptr;
                    memcpy(&old_ptr, dest, sizeof(void *));
                    if (old_ptr) _tin_release(old_ptr);
                }
                _chan_elem_copy(dest, val, esz);
                if (is_rc) {
                    void *new_ptr;
                    memcpy(&new_ptr, dest, sizeof(void *));
                    if (new_ptr) _tin_retain(new_ptr);
                }
                ch->tail  = (int64_t)((tail + 1) & (size_t)ch->cap_mask);
                ch->count++;
            }
            tin_fmutex_unlock(&ch->fmu);
            _tin_fiber_unpark_fib(rfib, rpid, rhdl);
            return 0;
        }

        // No waiting receiver: enqueue to ring buffer.
        // Cache tail in a local so the compiler doesn't reload ch->tail after
        // the copy (which may alias ch in its conservative aliasing analysis).
        size_t tail = (size_t)ch->tail;
        void *dest = ch->buf + tail * esz;
        if (is_rc) {
            void *old_ptr;
            memcpy(&old_ptr, dest, sizeof(void *));
            if (old_ptr) _tin_release(old_ptr);
        }
        _chan_elem_copy(dest, val, esz);
        if (is_rc) {
            void *new_ptr;
            memcpy(&new_ptr, dest, sizeof(void *));
            if (new_ptr) _tin_retain(new_ptr);
        }
        ch->tail  = (int64_t)((tail + 1) & (size_t)ch->cap_mask);
        ch->count++;
        tin_fmutex_unlock(&ch->fmu);
        return 0;
    }

    // Channel full: register as a data waiter and park.
    if (pid > 0 && hdl && ch->send_waiter_cnt < TIN_CHAN_MAX_WAITERS) {
        ch->send_waiters[ch->send_waiter_cnt]     = pid;
        ch->send_waiter_hdls[ch->send_waiter_cnt] = hdl;
        ch->send_waiter_fibs[ch->send_waiter_cnt] = _tin_current_fib();
        ch->send_waiter_cnt++;
        tin_fmutex_unlock(&ch->fmu);
        _tin_fiber_park(pid);
        return 1;
    }

    // Waiter list full: fall back to yield-retry.
    tin_fmutex_unlock(&ch->fmu);
    return 1;
}

// Direct recv: writes element to a caller-allocated buffer (e.g. an LLVM alloca)
// instead of the per-thread TLS scratch buffer.  Eliminates pthread_getspecific
// and the scratch-buffer management overhead on the hot path.
//
// Returns:
//   0   - element dequeued into `out`
//  -1   - channel closed and empty (caller should panic)
//   1   - fiber parked or mutex contended; caller must yield and retry
int _tin_channel_recv_direct(void *ptr, int64_t pid, void *out) {
    // Fast direct-delivery path: a sender already wrote the element to `out`
    // and set _direct_recv_flag via the worker loop.  Skip mutex + dequeue.
    if (__builtin_expect(_direct_recv_flag, 0)) {
        _direct_recv_flag = 0;
        return 0;
    }

    TinChannel *ch = (TinChannel *)ptr;

    void *hdl = _tin_current_coro_hdl();
    if (!tin_fmutex_lock_coro(&ch->fmu, pid, hdl))
        return 1;

    if (ch->count > 0) {
        // Cache head in a local so the compiler doesn't reload ch->head after
        // the copy (which may alias ch in its conservative aliasing analysis).
        size_t esz  = (size_t)ch->elem_size;
        size_t head = (size_t)ch->head;
        void *src = ch->buf + head * esz;
        _chan_elem_copy(out, src, esz);
        if (ch->is_rc) memset(src, 0, esz);
        ch->head = (int64_t)((head + 1) & (size_t)ch->cap_mask);
        ch->count--;

        if (ch->send_waiter_cnt > 0) {
            ch->send_waiter_cnt--;
            int64_t spid = ch->send_waiters[ch->send_waiter_cnt];
            void   *shdl = ch->send_waiter_hdls[ch->send_waiter_cnt];
            void   *sfib = ch->send_waiter_fibs[ch->send_waiter_cnt];
            tin_fmutex_unlock(&ch->fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
            return 0;
        }
        tin_fmutex_unlock(&ch->fmu);
        return 0;
    }

    if (ch->closed) {
        tin_fmutex_unlock(&ch->fmu);
        return -1;
    }

    if (pid > 0 && hdl && ch->recv_waiter_cnt < TIN_CHAN_MAX_WAITERS) {
        ch->recv_waiters[ch->recv_waiter_cnt]     = pid;
        ch->recv_waiter_hdls[ch->recv_waiter_cnt] = hdl;
        ch->recv_waiter_fibs[ch->recv_waiter_cnt] = _tin_current_fib();
        ch->recv_waiter_outs[ch->recv_waiter_cnt] = out;  // for direct delivery
        ch->recv_waiter_cnt++;
        tin_fmutex_unlock(&ch->fmu);
        _tin_fiber_park(pid);
        return 1;
    }

    tin_fmutex_unlock(&ch->fmu);
    return 1;
}

// Fiber-aware recv: try to dequeue one element from the channel.
// pid is the calling fiber's pid (kept for compatibility; recv_blocking is preferred).
//
// Returns:
//   - pointer to element data  - element was dequeued successfully
//   - NULL                     - channel is closed and empty (caller should panic)
//   - TIN_CHAN_BLOCKED          - channel was empty; caller should yield and retry
//
// Safety note on park/unpark: we intentionally do NOT call _tin_fiber_park here.
// The yield-retry loop in recv_fiber is safe without park.
void *_tin_channel_recv_park(void *ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;
    (void)pid;  // reserved for future optimization

    // Atomic trylock: never blocks the OS worker thread.
    if (!tin_fmutex_trylock(&ch->fmu))
        return TIN_CHAN_BLOCKED;  // contended - yield and retry

    if (ch->count > 0) {
        void *data = _chan_dequeue(ch);
        tin_fmutex_unlock(&ch->fmu);
        return data;
    }

    if (ch->closed) {
        tin_fmutex_unlock(&ch->fmu);
        return NULL;  // caller will panic
    }

    // Channel is empty and open: tell caller to yield and retry.
    tin_fmutex_unlock(&ch->fmu);
    return TIN_CHAN_BLOCKED;
}

// Returns the sentinel value TIN_CHAN_BLOCKED cast to i64.
// Used by Tin code to compare the return of _tin_channel_recv_park.
int64_t _tin_channel_recv_blocked_val(void) {
    return (int64_t)(intptr_t)TIN_CHAN_BLOCKED;  // -1
}

// Fiber-aware try-send: attempt to enqueue one element without blocking.
// Atomic trylock so the calling OS worker thread is never parked.
//
// Returns:
//   0   - element enqueued successfully
//   1   - channel full or mutex contended; caller should yield and retry
//  -1   - channel is closed; caller should panic
int _tin_channel_send_try(void *ptr, const void *val, int64_t elem_size, int is_rc) {
    TinChannel *ch = (TinChannel *)ptr;

    if (!tin_fmutex_trylock(&ch->fmu))
        return 1;  // contended - yield and retry

    if (ch->closed) {
        tin_fmutex_unlock(&ch->fmu);
        return -1;
    }

    if (ch->count >= ch->cap) {
        tin_fmutex_unlock(&ch->fmu);
        return 1;  // full - yield and retry
    }

    void *dest = ch->buf + ch->tail * elem_size;
    if (is_rc) {
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
    ch->tail  = (ch->tail + 1) & ch->cap_mask;
    ch->count++;
    tin_fmutex_unlock(&ch->fmu);
    return 0;
}

// Returns 1 if the channel is closed and empty, 0 otherwise.
int _tin_channel_is_closed_empty(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return 1;
    tin_fmutex_lock_spin(&ch->fmu);
    int r = (ch->closed && ch->count == 0) ? 1 : 0;
    tin_fmutex_unlock(&ch->fmu);
    return r;
}

// Close the channel. After this, sends return -1; pending and future recvs
// drain remaining items then return zero values.
// All parked waiters are unparked so they can observe the closed state.
void _tin_channel_close(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;

    // Collect all parked {fib, pid, hdl} triples before unlocking so we can
    // unpark without holding ch->fmu (avoids lock-order issues with fiber.c).
    int64_t wake_pids[TIN_CHAN_MAX_WAITERS * 2];
    void   *wake_hdls[TIN_CHAN_MAX_WAITERS * 2];
    void   *wake_fibs[TIN_CHAN_MAX_WAITERS * 2];
    int     nwake = 0;

    tin_fmutex_lock_spin(&ch->fmu);
    ch->closed = true;
    while (ch->recv_waiter_cnt > 0) {
        ch->recv_waiter_cnt--;
        wake_pids[nwake] = ch->recv_waiters[ch->recv_waiter_cnt];
        wake_hdls[nwake] = ch->recv_waiter_hdls[ch->recv_waiter_cnt];
        wake_fibs[nwake] = ch->recv_waiter_fibs[ch->recv_waiter_cnt];
        nwake++;
    }
    while (ch->send_waiter_cnt > 0) {
        ch->send_waiter_cnt--;
        wake_pids[nwake] = ch->send_waiters[ch->send_waiter_cnt];
        wake_hdls[nwake] = ch->send_waiter_hdls[ch->send_waiter_cnt];
        wake_fibs[nwake] = ch->send_waiter_fibs[ch->send_waiter_cnt];
        nwake++;
    }
    tin_fmutex_unlock(&ch->fmu);

    for (int i = 0; i < nwake; i++)
        _tin_fiber_unpark_fib(wake_fibs[i], wake_pids[i], wake_hdls[i]);
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
