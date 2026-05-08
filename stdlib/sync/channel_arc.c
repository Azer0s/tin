// tin stdlib/sync - Vyukov MPMC channel.
//
// Fast path: lock-free CAS on per-slot sequence counters (no mutex in common case).
// Slow path (full/empty): TinFastMutex waiter queues.
//
// The fast path scales linearly with the number of producers/consumers because
// each producer/consumer claims a different ring slot via a CAS on enq_pos/deq_pos.
// There is no centralized lock and no cache-line ping-pong between workers.
//
// Slow path: when the buffer is full (send) or empty (recv), the fiber registers
// itself in a waiter queue protected by wq_fmu and parks.  The wq_fmu critical
// section is very short (just waiter-list manipulation) so it does not become
// the bottleneck even under heavy contention.
//
// Missed-wakeup prevention: after incrementing recv_wq_cnt / send_wq_cnt the
// fiber does one final lf_enqueue / lf_dequeue.  The increment and the fast-path
// re-check on the other side both use memory_order_seq_cst to form a total order
// (Peterson-style): either the re-check sees the increment (and wakes the waiter)
// or the final lf_op sees the other side's data (and the waiter never parks).
// release/acquire is not sufficient: on ARM64, STLR (store-release) can sit in
// the store buffer so a concurrent LDAR (load-acquire) on another core returns a
// stale zero, causing both sides to miss each other.  On x86 (TSO) LOCK XADD is
// already a full fence, so seq_cst compiles to identical code.
//
// Reference: Dmitry Vyukov, "1024cores Bounded MPMC Queue".

#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>
#include <pthread.h>
#include <stdio.h>
#include <stdatomic.h>

void _tin_retain(void *ptr);
void _tin_release(void *ptr);
void _tin_release_any(int32_t tag, void *data);
void _tin_release_closure(void *env);
void _tin_panic(const char *msg);

// rc_kind values must mirror codegen/runtime.go rcKind. Channel uses
// the kind discriminator to pick the right shape inside each slot:
//   0 = none, 1 = leading-pointer, 2 = any (ptr@8), 3 = fn (env@8)
#define TIN_RC_NONE         0
#define TIN_RC_LEADING_PTR  1
#define TIN_RC_ANY          2
#define TIN_RC_FN           3

typedef struct { int32_t tag; void *ptr; } TinAnyVal;
typedef struct { void   *fn;  void *env; } TinFnVal;

// rc_retain_slot retains the payload pointer at the right offset for kind.
static inline void rc_retain_slot(void *slot, int kind) {
    if (kind == TIN_RC_NONE || !slot) return;

    switch (kind) {
    case TIN_RC_LEADING_PTR: {
        void *p; memcpy(&p, slot, sizeof(void *));
        if (p) _tin_retain(p);
        break;
    }
    case TIN_RC_ANY: {
        TinAnyVal a; memcpy(&a, slot, sizeof(a));
        if (a.ptr) _tin_retain(a.ptr);
        break;
    }
    case TIN_RC_FN: {
        TinFnVal f; memcpy(&f, slot, sizeof(f));
        if (f.env) _tin_retain(f.env);
        break;
    }
    }
}

// rc_release_slot releases the payload at the right offset for kind,
// dispatching to type-specific entry-points so closures inside any /
// fn release their captured env via the right deinit chain.
static inline void rc_release_slot(void *slot, int kind) {
    if (kind == TIN_RC_NONE || !slot) return;

    switch (kind) {
    case TIN_RC_LEADING_PTR: {
        void *p; memcpy(&p, slot, sizeof(void *));
        if (p) _tin_release(p);
        break;
    }
    case TIN_RC_ANY: {
        TinAnyVal a; memcpy(&a, slot, sizeof(a));
        if (a.ptr) _tin_release_any(a.tag, a.ptr);
        break;
    }
    case TIN_RC_FN: {
        TinFnVal f; memcpy(&f, slot, sizeof(f));
        if (f.env) _tin_release_closure(f.env);
        break;
    }
    }
}
void _tin_fiber_park(int64_t pid);
void _tin_fiber_unpark(int64_t pid);
void _tin_fiber_unpark_hdl(int64_t pid, void *hdl);
extern __thread void *_current_hdl;
static inline void *_tin_current_coro_hdl(void) { return _current_hdl; }
struct TinFiber;
extern __thread struct TinFiber *_current_fib;
static inline void *_tin_current_fib(void) { return (void *)_current_fib; }
extern __thread int _direct_recv_flag;
void _tin_fiber_set_direct_recv(void *fib);
void _tin_fiber_unpark_fib(void *fib, int64_t pid, void *hdl);
void _tin_fiber_mark_handoff_yield(void);
void  _tin_set_recv_hint(void *ch, void *out);
void *_tin_get_recv_hint_ch(void);
void *_tin_get_recv_hint_out(void);
void  _tin_clear_recv_hint(void);
void  _tin_set_preregistered_ch(void *ch);
void *_tin_get_preregistered_ch(void);
void  _tin_clear_preregistered_ch(void);
void  _tin_clear_advisory_state(void);

#include "../../runtime/fastmutex.h"

#define TIN_CHAN_BLOCKED ((void*)(intptr_t)-1)

// ---------------------------------------------------------------------------
// Fast element copy - avoids calling library memcpy for common small sizes.
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
// Waiter queue - grows on demand up to TINMAXCHANWAITERS.
// outs is non-NULL for recv queues (direct-delivery target buffers).
// ---------------------------------------------------------------------------

#define TIN_CHAN_WAITERS_INIT        8
#define TIN_CHAN_WAITERS_DEFAULT_MAX (1 << 16)

typedef struct {
    int64_t *pids;
    void   **hdls;
    void   **fibs;
    void   **outs;
    int      cnt;
    int      cap;
} TinWaiterQueue;

static int             _chan_waiter_max  = 0;
static pthread_once_t  _chan_waiter_once = PTHREAD_ONCE_INIT;

static void _chan_waiter_init_once(void) {
    const char *env = getenv("TINMAXCHANWAITERS");
    _chan_waiter_max = (env && *env) ? atoi(env) : TIN_CHAN_WAITERS_DEFAULT_MAX;
    if (_chan_waiter_max < 0) _chan_waiter_max = TIN_CHAN_WAITERS_DEFAULT_MAX;
}

static void _wq_alloc(TinWaiterQueue *wq, int has_outs) {
    wq->pids = (int64_t *)malloc((size_t)TIN_CHAN_WAITERS_INIT * sizeof(int64_t));
    wq->hdls = (void   **)malloc((size_t)TIN_CHAN_WAITERS_INIT * sizeof(void *));
    wq->fibs = (void   **)malloc((size_t)TIN_CHAN_WAITERS_INIT * sizeof(void *));
    wq->outs = has_outs
        ? (void **)calloc((size_t)TIN_CHAN_WAITERS_INIT, sizeof(void *))
        : NULL;
    wq->cnt = 0;
    wq->cap = TIN_CHAN_WAITERS_INIT;
    if (!wq->pids || !wq->hdls || !wq->fibs || (has_outs && !wq->outs)) {
        fputs("tin: channel waiter queue OOM\n", stderr);
        exit(1);
    }
}

static void _wq_free(TinWaiterQueue *wq) {
    free(wq->pids); wq->pids = NULL;
    free(wq->hdls); wq->hdls = NULL;
    free(wq->fibs); wq->fibs = NULL;
    free(wq->outs); wq->outs = NULL;
    wq->cnt = wq->cap = 0;
}

static void _wq_grow_or_panic(TinWaiterQueue *wq, TinFastMutex *fmu, int has_outs) {
    pthread_once(&_chan_waiter_once, _chan_waiter_init_once);
    if (_chan_waiter_max > 0 && wq->cap >= _chan_waiter_max) {
        tin_fmutex_unlock(fmu);
        _tin_panic("channel: waiter queue full - raise TINMAXCHANWAITERS");
        return;
    }
    int new_cap = wq->cap * 2;
    if (_chan_waiter_max > 0 && new_cap > _chan_waiter_max) new_cap = _chan_waiter_max;

    int64_t *np = (int64_t *)malloc((size_t)new_cap * sizeof(int64_t));
    void   **nh = (void   **)malloc((size_t)new_cap * sizeof(void *));
    void   **nf = (void   **)malloc((size_t)new_cap * sizeof(void *));
    void   **no = (has_outs && wq->outs)
        ? (void **)calloc((size_t)new_cap, sizeof(void *))
        : NULL;
    if (!np || !nh || !nf || (has_outs && wq->outs && !no)) {
        free(np); free(nh); free(nf); free(no);
        tin_fmutex_unlock(fmu);
        _tin_panic("channel: waiter queue OOM");
        return;
    }
    memcpy(np, wq->pids, (size_t)wq->cnt * sizeof(int64_t));
    memcpy(nh, wq->hdls, (size_t)wq->cnt * sizeof(void *));
    memcpy(nf, wq->fibs, (size_t)wq->cnt * sizeof(void *));
    if (no) memcpy(no, wq->outs, (size_t)wq->cnt * sizeof(void *));
    free(wq->pids); wq->pids = np;
    free(wq->hdls); wq->hdls = nh;
    free(wq->fibs); wq->fibs = nf;
    free(wq->outs); wq->outs = no;
    wq->cap = new_cap;
}

// ---------------------------------------------------------------------------
// TinChannel - Vyukov MPMC ring buffer + fiber waiter queues.
//
// Cache-line layout:
//   [0..N]:      ref_count + wq_fmu (waiter-queue lock, uncontended on fast path)
//   [N..N+64]:   cap, cap_mask, elem_size, rc_kind, closed, wq counters, wq pointers
//   [aligned]:   enq_pos on its own 64-byte cache line (producer hot)
//   [aligned]:   deq_pos on its own 64-byte cache line (consumer hot)
//   [separate]:  seq_buf (aligned_alloc, cap * 8 bytes) - per-slot seq counters
//   [separate]:  data_buf (cap * elem_size bytes)
// ---------------------------------------------------------------------------
typedef struct TinChannel {
    atomic_int       ref_count;
    TinFastMutex     wq_fmu;     // waiter-queue lock (slow path only)

    int64_t          cap;
    int64_t          cap_mask;   // cap - 1 for bitwise AND wrap
    int64_t          elem_size;
    atomic_bool      closed;
    int              rc_kind;

    // Atomic counters for parked waiters.  Checked outside wq_fmu on the fast
    // path: if 0, no wakeup is needed and wq_fmu is never touched.
    _Atomic(int32_t) recv_wq_cnt;
    _Atomic(int32_t) send_wq_cnt;

    TinWaiterQueue   recv_wq;    // protected by wq_fmu
    TinWaiterQueue   send_wq;    // protected by wq_fmu

    // Vyukov MPMC ring buffer state.
    // enq_pos and deq_pos are on separate cache lines to prevent producer-
    // consumer false sharing.  The 56-byte pads round each out to 64 bytes.
    _Atomic(int64_t) enq_pos;
    char             _pad_enq[56];
    _Atomic(int64_t) deq_pos;
    char             _pad_deq[56];

    _Atomic(int64_t) *seq_buf;   // cap entries; seq[i] initialized to i
    char             *data_buf;  // cap * elem_size bytes
} TinChannel;

// ---------------------------------------------------------------------------
// Allocate / retain / release
// ---------------------------------------------------------------------------

void *_tin_channel_new(int64_t cap, int64_t elem_size, int rc_kind) {
    if (cap <= 0) {
        _tin_panic("Channel.make: capacity must be > 0");
    }
    // Bound the capacity from above: the po2 round-up below shifts a
    // signed int64_t and will wrap into negative or loop past INT64_MAX
    // for adversarial inputs.  A cap above 2^30 (~1 billion slots) is
    // already absurd; reject it loudly so we don't end up doing a
    // tiny malloc and overrunning it via cap_mask.
    if (cap > (int64_t)1 << 30) {
        _tin_panic("Channel.make: capacity too large (max 2^30)");
    }
    if (elem_size <= 0 || elem_size > (int64_t)1 << 16) {
        _tin_panic("Channel.make: elem_size out of range");
    }
    // Round up to power of 2 for bitwise-AND wrap.
    int64_t po2 = 1;
    while (po2 < cap) po2 <<= 1;
    cap = po2;

    TinChannel *ch = (TinChannel *)calloc(1, sizeof(TinChannel));
    if (!ch) { fputs("tin: channel alloc failed\n", stderr); exit(1); }

    atomic_init(&ch->ref_count, 1);
    tin_fmutex_init(&ch->wq_fmu);
    ch->cap       = cap;
    ch->cap_mask  = cap - 1;
    ch->elem_size = elem_size;
    atomic_store_explicit(&ch->closed, false, memory_order_relaxed);
    ch->rc_kind   = rc_kind;
    atomic_store_explicit(&ch->recv_wq_cnt, 0, memory_order_relaxed);
    atomic_store_explicit(&ch->send_wq_cnt, 0, memory_order_relaxed);
    atomic_store_explicit(&ch->enq_pos, 0, memory_order_relaxed);
    atomic_store_explicit(&ch->deq_pos, 0, memory_order_relaxed);

    // seq_buf: aligned to 64 bytes, size rounded to 64-byte multiple.
    // posix_memalign instead of C11 aligned_alloc -- the latter is gated
    // behind macOS 10.15 deployment target in Apple's SDK and breaks
    // cross-compile to darwin from older toolchains.
    size_t seq_bytes = (size_t)cap * sizeof(_Atomic(int64_t));
    seq_bytes = (seq_bytes + 63u) & ~(size_t)63u;
    if (seq_bytes < 64) seq_bytes = 64;

    void *seq_raw = NULL;
    if (posix_memalign(&seq_raw, 64, seq_bytes) != 0 || seq_raw == NULL) {
        fputs("tin: channel seq_buf alloc failed\n", stderr); exit(1);
    }

    ch->seq_buf = (_Atomic(int64_t) *)seq_raw;
    for (int64_t i = 0; i < cap; i++)
        atomic_store_explicit(&ch->seq_buf[i], i, memory_order_relaxed);

    ch->data_buf = (char *)malloc((size_t)(cap * elem_size));
    if (!ch->data_buf) { fputs("tin: channel data_buf alloc failed\n", stderr); exit(1); }
    if (rc_kind != TIN_RC_NONE) memset(ch->data_buf, 0, (size_t)(cap * elem_size));

    _wq_alloc(&ch->recv_wq, 1);
    _wq_alloc(&ch->send_wq, 0);
    return ch;
}

void _tin_channel_retain(void *ptr) {
    if (!ptr) return;
    atomic_fetch_add_explicit(&((TinChannel *)ptr)->ref_count, 1, memory_order_relaxed);
}

void _tin_channel_free(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;
    if (atomic_fetch_sub_explicit(&ch->ref_count, 1, memory_order_acq_rel) > 1)
        return;
    // Last reference: release any buffered RC elements.
    if (ch->rc_kind != TIN_RC_NONE) {
        int64_t deq = atomic_load_explicit(&ch->deq_pos, memory_order_relaxed);
        int64_t enq = atomic_load_explicit(&ch->enq_pos, memory_order_relaxed);
        for (int64_t pos = deq; pos != enq; pos++) {
            int64_t slot = pos & ch->cap_mask;
            void *slot_ptr = ch->data_buf + slot * ch->elem_size;
            rc_release_slot(slot_ptr, ch->rc_kind);
        }
    }
    free(ch->seq_buf);
    free(ch->data_buf);
    _wq_free(&ch->recv_wq);
    _wq_free(&ch->send_wq);
    free(ch);
}

// ---------------------------------------------------------------------------
// Lock-free ring buffer operations (Vyukov algorithm).
//
// lf_enqueue: claim a slot via CAS on enq_pos, write data, publish via seq.
// lf_dequeue: claim a slot via CAS on deq_pos, read data, free via seq.
//
// Returns 1 on success, 0 on full/empty.
// ---------------------------------------------------------------------------

static inline int lf_enqueue(TinChannel *ch, const void *val, size_t esz, int rc_kind) {
    int64_t pos = atomic_load_explicit(&ch->enq_pos, memory_order_relaxed);
    for (;;) {
        _Atomic(int64_t) *pseq = ch->seq_buf + (pos & ch->cap_mask);
        int64_t seq = atomic_load_explicit(pseq, memory_order_acquire);
        int64_t diff = seq - pos;
        if (diff == 0) {
            if (atomic_compare_exchange_weak_explicit(
                    &ch->enq_pos, &pos, pos + 1,
                    memory_order_relaxed, memory_order_relaxed)) {
                void *dst = ch->data_buf + (pos & ch->cap_mask) * (int64_t)esz;
                // Release stale slot then copy + retain new payload. Doing
                // it in this order keeps RC math correct even if val and
                // dst alias (rc_retain_slot reads val's payload pointer
                // before _chan_elem_copy clobbers dst).
                rc_release_slot(dst, rc_kind);
                _chan_elem_copy(dst, val, esz);
                rc_retain_slot(dst, rc_kind);
                atomic_store_explicit(pseq, pos + 1, memory_order_release);
                return 1;
            }
        } else if (diff < 0) {
            return 0;  // full
        } else {
            pos = atomic_load_explicit(&ch->enq_pos, memory_order_relaxed);
        }
    }
}

static inline int lf_dequeue(TinChannel *ch, void *out, size_t esz, int rc_kind) {
    int64_t pos = atomic_load_explicit(&ch->deq_pos, memory_order_relaxed);
    for (;;) {
        _Atomic(int64_t) *pseq = ch->seq_buf + (pos & ch->cap_mask);
        int64_t seq = atomic_load_explicit(pseq, memory_order_acquire);
        int64_t diff = seq - (pos + 1);
        if (diff == 0) {
            if (atomic_compare_exchange_weak_explicit(
                    &ch->deq_pos, &pos, pos + 1,
                    memory_order_relaxed, memory_order_relaxed)) {
                void *src = ch->data_buf + (pos & ch->cap_mask) * (int64_t)esz;
                _chan_elem_copy(out, src, esz);
                // Hand ownership to the consumer: the slot transfers its
                // RC to `out` (no retain needed, the slot's retain becomes
                // out's retain). Zero the slot so a future _tin_channel_free
                // doesn't double-release.
                if (rc_kind != TIN_RC_NONE) memset(src, 0, esz);
                atomic_store_explicit(pseq, pos + ch->cap, memory_order_release);
                return 1;
            }
        } else if (diff < 0) {
            return 0;  // empty
        } else {
            pos = atomic_load_explicit(&ch->deq_pos, memory_order_relaxed);
        }
    }
}

// ---------------------------------------------------------------------------
// Wake helpers: pop one waiter from wq under wq_fmu and unpark it.
// Called after a successful fast-path enqueue/dequeue when wq_cnt > 0.
// ---------------------------------------------------------------------------

static void _wake_one_recv(TinChannel *ch) {
    tin_fmutex_lock_spin(&ch->wq_fmu);
    if (ch->recv_wq.cnt == 0) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return;
    }
    ch->recv_wq.cnt--;
    atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
    int64_t rpid = ch->recv_wq.pids[ch->recv_wq.cnt];
    void   *rhdl = ch->recv_wq.hdls[ch->recv_wq.cnt];
    void   *rfib = ch->recv_wq.fibs[ch->recv_wq.cnt];
    tin_fmutex_unlock(&ch->wq_fmu);
    _tin_fiber_unpark_fib(rfib, rpid, rhdl);
}

static void _wake_one_send(TinChannel *ch) {
    tin_fmutex_lock_spin(&ch->wq_fmu);
    if (ch->send_wq.cnt == 0) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return;
    }
    ch->send_wq.cnt--;
    atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
    int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
    void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
    void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
    tin_fmutex_unlock(&ch->wq_fmu);
    _tin_fiber_unpark_fib(sfib, spid, shdl);
}

// ---------------------------------------------------------------------------
// Per-thread scratch buffer for _tin_channel_recv_blocking / _tin_channel_recv_park.
// Freed automatically when the worker thread exits.
// ---------------------------------------------------------------------------

typedef struct RecvBuf { uint8_t *data; size_t sz; } RecvBuf;

static pthread_key_t  _recv_buf_key;
static pthread_once_t _recv_buf_once = PTHREAD_ONCE_INIT;
static __thread int   _recv_buf_key_ready = 0;

static void _recv_buf_dtor(void *p) {
    if (!p) return;
    RecvBuf *rb = (RecvBuf *)p;
    free(rb->data);
    free(rb);
}

static void _recv_buf_cleanup_atexit(void) {
    void *p = pthread_getspecific(_recv_buf_key);
    if (p) { pthread_setspecific(_recv_buf_key, NULL); _recv_buf_dtor(p); }
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

// Dequeue into the per-thread scratch buffer.  Returns pointer on success, NULL on empty.
static void *_lf_dequeue_tls(TinChannel *ch) {
    if (__builtin_expect(!_recv_buf_key_ready, 0)) {
        pthread_once(&_recv_buf_once, _recv_buf_init_key);
        _recv_buf_key_ready = 1;
    }
    RecvBuf *rb = _recv_buf_get();
    size_t esz = (size_t)ch->elem_size;
    if (esz > rb->sz) {
        free(rb->data);
        rb->data = (uint8_t *)malloc(esz);
        rb->sz   = esz;
    }
    if (!lf_dequeue(ch, rb->data, esz, ch->rc_kind)) return NULL;
    return rb->data;
}

// ---------------------------------------------------------------------------
// Blocking send.
//
// Fast path (recv_wq_cnt == 0 and channel not full): lock-free enqueue, O(1).
// Handoff path (recv_direct receiver parked):  direct delivery to receiver's
//   alloca, set direct_recv_flag, unpark, mark handoff_yield, return 2.
// Slow path (full): register in send_wq, park, return 1 (caller yields).
//
// Returns:
//   0   - enqueued (fast path or recv_blocking receiver woken via ring buffer)
//   2   - handoff: direct delivery to recv_direct receiver (caller must yield once)
//  -1   - channel closed
//   1   - parked or yielded; caller must yield and retry
// ---------------------------------------------------------------------------
int _tin_channel_send_blocking(void *ptr, const void *val,
                                int64_t elem_size, int rc_kind, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;
    size_t esz = (size_t)elem_size;

    // Closed check has to happen on the fast path too.  Skipping it here
    // (and only checking inside the slow path lock) lets a closed channel
    // silently buffer values when there's room, contradicting the
    // "send on closed channel panics" contract documented in channel.tin.
    // The load is acquire-ordered so close()'s store is visible.
    if (__builtin_expect(atomic_load_explicit(&ch->closed, memory_order_acquire), 0)) {
        return -1;
    }

    // Fast path: no parked receivers, not closed, channel has space.
    int32_t rcnt = atomic_load_explicit(&ch->recv_wq_cnt, memory_order_relaxed);
    if (__builtin_expect(rcnt == 0, 1)) {
        if (lf_enqueue(ch, val, esz, rc_kind)) {
            // Check again for newly-parked receivers.
            if (__builtin_expect(
                    atomic_load_explicit(&ch->recv_wq_cnt, memory_order_seq_cst) > 0, 0))
                _wake_one_recv(ch);
            return 0;
        }
    }

    // Slow path: either a parked receiver or the buffer is full.
    void *hdl = _tin_current_coro_hdl();
    tin_fmutex_lock_spin(&ch->wq_fmu);

    if (ch->closed) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return -1;
    }

    // Receivers only park when the buffer is empty.  Two wakeup strategies:
    //
    //  cap=1 + recv_direct receiver (rout != NULL):
    //    Direct delivery to receiver's alloca.  Receiver resumes with
    //    _direct_recv_flag=1 and returns in 1 TLS load (no ring ops).
    //    No handoff yield - sender parks immediately on its next recv.
    //
    //  cap>1 or recv_blocking receiver:
    //    Ring-buffer enqueue.  Enables batching (sender never yields).
    if (ch->recv_wq.cnt > 0) {
        int idx = ch->recv_wq.cnt - 1;
        void *rout = ch->recv_wq.outs ? ch->recv_wq.outs[idx] : NULL;

        if (rout && ch->cap == 1) {
            // cap=1 recv_direct: direct delivery + handoff yield.
            // Handoff lets the receiver run immediately; the sender parks on its
            // next recv (which is always empty for cap=1 pipelines).
            // This reduces round-trip parks from 2 to 1 for latency-sensitive paths.
            ch->recv_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
            int64_t rpid = ch->recv_wq.pids[idx];
            void   *rhdl = ch->recv_wq.hdls[idx];
            void   *rfib = ch->recv_wq.fibs[idx];
            _chan_elem_copy(rout, val, esz);
            // Direct delivery skips the slot, so retain the payload here
            // for the receiver's RC=1.
            rc_retain_slot(rout, rc_kind);
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_set_direct_recv(rfib);
            _tin_fiber_unpark_fib(rfib, rpid, rhdl);
            _tin_fiber_mark_handoff_yield();
            return 2;
        }

        // Ring-buffer enqueue for all other cases.
        int enqueued = lf_enqueue(ch, val, esz, rc_kind);
        if (enqueued) {
            ch->recv_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
            int64_t rpid = ch->recv_wq.pids[idx];
            void   *rhdl = ch->recv_wq.hdls[idx];
            void   *rfib = ch->recv_wq.fibs[idx];
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_unpark_fib(rfib, rpid, rhdl);
            return 0;
        }
        // Enqueue failed: shouldn't happen (parked receivers imply empty buffer).
        // Fall through to park as a safety measure.
    } else {
        // No parked receivers - try lf_enqueue one more time under the lock.
        if (lf_enqueue(ch, val, esz, rc_kind)) {
            tin_fmutex_unlock(&ch->wq_fmu);
            return 0;
        }
    }

    // Channel full: register as send waiter and park.
    if (pid > 0 && hdl) {
        if (ch->send_wq.cnt >= ch->send_wq.cap)
            _wq_grow_or_panic(&ch->send_wq, &ch->wq_fmu, 0);
        ch->send_wq.pids[ch->send_wq.cnt] = pid;
        ch->send_wq.hdls[ch->send_wq.cnt] = hdl;
        ch->send_wq.fibs[ch->send_wq.cnt] = _tin_current_fib();
        ch->send_wq.cnt++;
        atomic_fetch_add_explicit(&ch->send_wq_cnt, 1, memory_order_seq_cst);

        // Final check: a consumer may have dequeued between our failed lf_enqueue
        // and the send_wq_cnt increment, saw cnt==0, and didn't wake us.
        if (lf_enqueue(ch, val, esz, rc_kind)) {
            ch->send_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
            tin_fmutex_unlock(&ch->wq_fmu);
            return 0;
        }

        tin_fmutex_unlock(&ch->wq_fmu);
        _tin_fiber_park(pid);
        return 1;
    }

    tin_fmutex_unlock(&ch->wq_fmu);
    return 1;
}

// ---------------------------------------------------------------------------
// Blocking recv (non-inline path via fn{#async} recv method).
//
// Returns:
//   data pointer (per-thread scratch buf)  - element dequeued
//   NULL                                   - channel closed and empty
//   TIN_CHAN_BLOCKED                        - parked; caller must yield
// ---------------------------------------------------------------------------
void *_tin_channel_recv_blocking(void *ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;
    void *hdl = _tin_current_coro_hdl();

    // Fast path: no parked senders, try lock-free dequeue.
    if (__builtin_expect(
            atomic_load_explicit(&ch->send_wq_cnt, memory_order_relaxed) == 0, 1)) {
        void *data = _lf_dequeue_tls(ch);
        if (data) {
            if (__builtin_expect(
                    atomic_load_explicit(&ch->send_wq_cnt, memory_order_seq_cst) > 0, 0))
                _wake_one_send(ch);
            return data;
        }
    }

    // Slow path.
    tin_fmutex_lock_spin(&ch->wq_fmu);

    void *data = _lf_dequeue_tls(ch);
    if (data) {
        if (ch->send_wq.cnt > 0) {
            ch->send_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
            int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
            void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
            void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
            return data;
        }
        tin_fmutex_unlock(&ch->wq_fmu);
        return data;
    }

    if (ch->closed) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return NULL;
    }

    if (pid > 0 && hdl) {
        if (ch->recv_wq.cnt >= ch->recv_wq.cap)
            _wq_grow_or_panic(&ch->recv_wq, &ch->wq_fmu, 1);
        ch->recv_wq.pids[ch->recv_wq.cnt] = pid;
        ch->recv_wq.hdls[ch->recv_wq.cnt] = hdl;
        ch->recv_wq.fibs[ch->recv_wq.cnt] = _tin_current_fib();
        ch->recv_wq.outs[ch->recv_wq.cnt] = NULL;
        ch->recv_wq.cnt++;
        atomic_fetch_add_explicit(&ch->recv_wq_cnt, 1, memory_order_seq_cst);

        // Final check: sender may have enqueued after our failed dequeue but
        // before the recv_wq_cnt increment, saw cnt==0, and skipped waking us.
        data = _lf_dequeue_tls(ch);
        if (data) {
            ch->recv_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
            if (ch->send_wq.cnt > 0) {
                ch->send_wq.cnt--;
                atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
                int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
                void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
                void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
                tin_fmutex_unlock(&ch->wq_fmu);
                _tin_fiber_unpark_fib(sfib, spid, shdl);
                return data;
            }
            tin_fmutex_unlock(&ch->wq_fmu);
            return data;
        }

        tin_fmutex_unlock(&ch->wq_fmu);
        _tin_fiber_park(pid);
        return TIN_CHAN_BLOCKED;
    }

    tin_fmutex_unlock(&ch->wq_fmu);
    return TIN_CHAN_BLOCKED;
}

// ---------------------------------------------------------------------------
// Direct recv (inline await recv path via genDirectChanRecv).
// Writes element into caller-allocated `out` buffer to avoid TLS scratch overhead.
//
// Returns:
//   0   - element dequeued (or directly delivered) into `out`
//  -1   - channel closed and empty
//   1   - parked or yielded; caller must yield and retry
// ---------------------------------------------------------------------------
int _tin_channel_recv_direct(void *ptr, int64_t pid, void *out) {
    // Direct delivery fast path: sender already wrote to `out` and set the flag.
    // Also clear advisory state (preregistered_ch + stale pending_wakeup).
    if (__builtin_expect(_direct_recv_flag, 0)) {
        _direct_recv_flag = 0;
        _tin_clear_advisory_state();
        return 0;
    }

    TinChannel *ch = (TinChannel *)ptr;
    size_t esz = (size_t)ch->elem_size;
    void *hdl = _tin_current_coro_hdl();

    // Fast path: no parked senders, try lock-free dequeue.
    if (__builtin_expect(
            atomic_load_explicit(&ch->send_wq_cnt, memory_order_relaxed) == 0, 1)) {
        if (lf_dequeue(ch, out, esz, ch->rc_kind)) {
            if (__builtin_expect(
                    atomic_load_explicit(&ch->send_wq_cnt, memory_order_seq_cst) > 0, 0))
                _wake_one_send(ch);
            return 0;
        }
    }

    // Slow path.
    tin_fmutex_lock_spin(&ch->wq_fmu);

    if (lf_dequeue(ch, out, esz, ch->rc_kind)) {
        if (ch->send_wq.cnt > 0) {
            ch->send_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
            int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
            void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
            void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
            return 0;
        }
        tin_fmutex_unlock(&ch->wq_fmu);
        return 0;
    }

    if (ch->closed) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return -1;
    }

    if (pid > 0 && hdl) {
        // Advisory pre-registration: fiber was registered in recv_wq by
        // _tin_prepark_next_recv.  Skip re-registration and just park.
        // Keep preregistered_ch set so that spurious re-queues (from a stale
        // pending_wakeup) retry this path instead of double-registering.
        void *preg_ch = _tin_get_preregistered_ch();
        if (preg_ch == ptr) {
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_park(pid);
            return 1;
        }

        if (ch->recv_wq.cnt >= ch->recv_wq.cap)
            _wq_grow_or_panic(&ch->recv_wq, &ch->wq_fmu, 1);
        ch->recv_wq.pids[ch->recv_wq.cnt] = pid;
        ch->recv_wq.hdls[ch->recv_wq.cnt] = hdl;
        ch->recv_wq.fibs[ch->recv_wq.cnt] = _tin_current_fib();
        ch->recv_wq.outs[ch->recv_wq.cnt] = out;
        ch->recv_wq.cnt++;
        atomic_fetch_add_explicit(&ch->recv_wq_cnt, 1, memory_order_seq_cst);

        // Final check: sender may have enqueued and not woken us.
        if (lf_dequeue(ch, out, esz, ch->rc_kind)) {
            ch->recv_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
            if (ch->send_wq.cnt > 0) {
                ch->send_wq.cnt--;
                atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
                int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
                void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
                void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
                tin_fmutex_unlock(&ch->wq_fmu);
                _tin_fiber_unpark_fib(sfib, spid, shdl);
                return 0;
            }
            tin_fmutex_unlock(&ch->wq_fmu);
            return 0;
        }

        tin_fmutex_unlock(&ch->wq_fmu);
        _tin_fiber_park(pid);
        return 1;
    }

    tin_fmutex_unlock(&ch->wq_fmu);
    return 1;
}

// ---------------------------------------------------------------------------
// Pre-registration: called from genDirectChanSend handoff block (r==2) before
// coro.suspend.  Reads _recv_hint_ch/_recv_hint_out and pre-registers the
// current fiber in that channel's recv_wq, then calls _tin_fiber_park so the
// worker sets BLOCKED after coro.suspend instead of routing via LQ.
//
// This eliminates the "wasted LQ pop" pattern in pipeline-style workloads where
// a relay fiber's handoff yield would otherwise put it in LQ just to immediately
// park on its next (empty) recv.
//
// Returns: 1 = pre-registered (pending_park set), 0 = skipped (no hint or data
//          already available - fiber proceeds through normal handoff yield path).
// ---------------------------------------------------------------------------
int _tin_prepark_next_recv(int64_t pid) {
    return 0;  // DISABLED
    void *ch_ptr = _tin_get_recv_hint_ch();
    void *out    = _tin_get_recv_hint_out();
    if (!ch_ptr || !out || pid <= 0) return 0;
    // Advisory pre-registration is only beneficial for cap=1 pipelines where the
    // sender always uses direct delivery (not ring-buffer enqueue).  For cap>1,
    // the ring-buffer fast path already avoids parks; advisory adds overhead.
    TinChannel *ch_early = (TinChannel *)ch_ptr;
    if (ch_early->cap != 1) { _tin_clear_recv_hint(); return 0; }

    void *hdl = _tin_current_coro_hdl();
    void *fib = _tin_current_fib();
    if (!hdl || !fib) return 0;

    TinChannel *ch = (TinChannel *)ch_ptr;
    size_t esz = (size_t)ch->elem_size;

    tin_fmutex_lock_spin(&ch->wq_fmu);

    if (ch->closed) {
        tin_fmutex_unlock(&ch->wq_fmu);
        return 0;
    }

    // If data is already available, dequeue it directly into out and signal via
    // direct_recv_done so the fiber resumes with _direct_recv_flag=1 (fast path).
    if (lf_dequeue(ch, out, esz, ch->rc_kind)) {
        if (ch->send_wq.cnt > 0) {
            ch->send_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
            int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
            void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
            void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
        } else {
            tin_fmutex_unlock(&ch->wq_fmu);
        }
        _tin_clear_recv_hint();
        _tin_fiber_set_direct_recv(fib);
        return 0;  // no pending_park; fiber goes via normal handoff LQ path
    }

    _tin_clear_recv_hint();

    if (ch->recv_wq.cnt >= ch->recv_wq.cap)
        _wq_grow_or_panic(&ch->recv_wq, &ch->wq_fmu, 1);
    ch->recv_wq.pids[ch->recv_wq.cnt] = pid;
    ch->recv_wq.hdls[ch->recv_wq.cnt] = hdl;
    ch->recv_wq.fibs[ch->recv_wq.cnt] = fib;
    ch->recv_wq.outs[ch->recv_wq.cnt] = out;
    ch->recv_wq.cnt++;
    atomic_fetch_add_explicit(&ch->recv_wq_cnt, 1, memory_order_release);

    // Missed-wakeup check: sender may have enqueued between our failed dequeue
    // and the recv_wq_cnt increment.
    if (lf_dequeue(ch, out, esz, ch->rc_kind)) {
        ch->recv_wq.cnt--;
        atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
        if (ch->send_wq.cnt > 0) {
            ch->send_wq.cnt--;
            atomic_fetch_sub_explicit(&ch->send_wq_cnt, 1, memory_order_relaxed);
            int64_t spid = ch->send_wq.pids[ch->send_wq.cnt];
            void   *shdl = ch->send_wq.hdls[ch->send_wq.cnt];
            void   *sfib = ch->send_wq.fibs[ch->send_wq.cnt];
            tin_fmutex_unlock(&ch->wq_fmu);
            _tin_fiber_unpark_fib(sfib, spid, shdl);
        } else {
            tin_fmutex_unlock(&ch->wq_fmu);
        }
        _tin_fiber_set_direct_recv(fib);
        return 0;  // data available; no pending_park
    }

    tin_fmutex_unlock(&ch->wq_fmu);
    // Advisory: register without parking.  Fiber goes to LQ via handoff_yield.
    // recv_direct will skip re-registration (preg_ch==ptr path) and just park.
    // _coro_done cleanup removes the entry if the loop exits before recv_direct.
    _tin_set_preregistered_ch(ch_ptr);
    return 1;
}

// ---------------------------------------------------------------------------
// Remove advisory recv_wq entry for `pid` from channel `ch_ptr`.
// Called from fiber.c's _coro_done path when a fiber completes without
// consuming its pre-registration (e.g. last loop iteration, loop body exited).
// ---------------------------------------------------------------------------
void _tin_chan_remove_recv_waiter(void *ch_ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ch_ptr;
    if (!ch) return;
    tin_fmutex_lock_spin(&ch->wq_fmu);
    int n = ch->recv_wq.cnt;
    for (int i = 0; i < n; i++) {
        if (ch->recv_wq.pids[i] == pid) {
            int last = ch->recv_wq.cnt - 1;
            ch->recv_wq.cnt = last;
            atomic_fetch_sub_explicit(&ch->recv_wq_cnt, 1, memory_order_relaxed);
            if (i < last) {
                ch->recv_wq.pids[i] = ch->recv_wq.pids[last];
                ch->recv_wq.hdls[i] = ch->recv_wq.hdls[last];
                ch->recv_wq.fibs[i] = ch->recv_wq.fibs[last];
                if (ch->recv_wq.outs) ch->recv_wq.outs[i] = ch->recv_wq.outs[last];
            }
            break;
        }
    }
    tin_fmutex_unlock(&ch->wq_fmu);
}

// ---------------------------------------------------------------------------
// Legacy recv_park / blocked_val / send_try (used by old async recv/send loops).
// ---------------------------------------------------------------------------

void *_tin_channel_recv_park(void *ptr, int64_t pid) {
    TinChannel *ch = (TinChannel *)ptr;
    (void)pid;
    void *data = _lf_dequeue_tls(ch);
    if (data) return data;
    if (ch->closed) return NULL;
    return TIN_CHAN_BLOCKED;
}

int64_t _tin_channel_recv_blocked_val(void) {
    return (int64_t)(intptr_t)TIN_CHAN_BLOCKED;
}

int _tin_channel_send_try(void *ptr, const void *val, int64_t elem_size, int rc_kind) {
    TinChannel *ch = (TinChannel *)ptr;
    if (ch->closed) return -1;
    if (lf_enqueue(ch, val, (size_t)elem_size, rc_kind)) return 0;
    return 1;
}

// ---------------------------------------------------------------------------
// Close and is_closed_empty.
// ---------------------------------------------------------------------------

int _tin_channel_is_closed_empty(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return 1;
    if (!ch->closed) return 0;
    // Closed: check if any items remain using enq/deq positions.
    int64_t enq = atomic_load_explicit(&ch->enq_pos, memory_order_acquire);
    int64_t deq = atomic_load_explicit(&ch->deq_pos, memory_order_acquire);
    return (enq == deq) ? 1 : 0;
}

void _tin_channel_close(void *ptr) {
    TinChannel *ch = (TinChannel *)ptr;
    if (!ch) return;

    tin_fmutex_lock_spin(&ch->wq_fmu);
    int nwake = ch->recv_wq.cnt + ch->send_wq.cnt;
    int64_t *wake_pids = (nwake > 0) ? (int64_t *)malloc((size_t)nwake * sizeof(int64_t)) : NULL;
    void   **wake_hdls = (nwake > 0) ? (void   **)malloc((size_t)nwake * sizeof(void *))  : NULL;
    void   **wake_fibs = (nwake > 0) ? (void   **)malloc((size_t)nwake * sizeof(void *))  : NULL;
    int w = 0;

    atomic_store_explicit(&ch->closed, true, memory_order_release);
    while (ch->recv_wq.cnt > 0) {
        ch->recv_wq.cnt--;
        if (wake_pids) {
            wake_pids[w] = ch->recv_wq.pids[ch->recv_wq.cnt];
            wake_hdls[w] = ch->recv_wq.hdls[ch->recv_wq.cnt];
            wake_fibs[w] = ch->recv_wq.fibs[ch->recv_wq.cnt];
            w++;
        }
    }
    atomic_store_explicit(&ch->recv_wq_cnt, 0, memory_order_relaxed);
    while (ch->send_wq.cnt > 0) {
        ch->send_wq.cnt--;
        if (wake_pids) {
            wake_pids[w] = ch->send_wq.pids[ch->send_wq.cnt];
            wake_hdls[w] = ch->send_wq.hdls[ch->send_wq.cnt];
            wake_fibs[w] = ch->send_wq.fibs[ch->send_wq.cnt];
            w++;
        }
    }
    atomic_store_explicit(&ch->send_wq_cnt, 0, memory_order_relaxed);
    tin_fmutex_unlock(&ch->wq_fmu);

    for (int i = 0; i < w; i++)
        _tin_fiber_unpark_fib(wake_fibs[i], wake_pids[i], wake_hdls[i]);

    free(wake_pids);
    free(wake_hdls);
    free(wake_fibs);
}

// ---------------------------------------------------------------------------
// Legacy ring-buffer helpers (kept for compatibility with existing codegen).
// ---------------------------------------------------------------------------

void _tin_chan_buf_store(void *buf, int64_t slot, const void *elem,
                         int64_t elem_size, int rc_kind) {
    void *dest = (char *)buf + slot * elem_size;
    rc_release_slot(dest, rc_kind);
    memcpy(dest, elem, (size_t)elem_size);
    rc_retain_slot(dest, rc_kind);
}

void _tin_chan_buf_load(const void *buf, int64_t slot, void *out,
                        int64_t elem_size, int rc_kind) {
    const void *src = (const char *)buf + slot * elem_size;
    memcpy(out, src, (size_t)elem_size);
    if (rc_kind != TIN_RC_NONE) memset((void *)src, 0, (size_t)elem_size);
}

void _tin_chan_buf_drain(void *buf, int64_t cap, int64_t head, int64_t count,
                         int64_t elem_size, int rc_kind) {
    if (rc_kind == TIN_RC_NONE) return;
    for (int64_t i = 0; i < count; i++) {
        int64_t slot = (head + i) % cap;
        void *slot_ptr = (char *)buf + slot * elem_size;
        rc_release_slot(slot_ptr, rc_kind);
    }
}
