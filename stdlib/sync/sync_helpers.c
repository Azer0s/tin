// tin stdlib/sync - pthread wrappers for Mutex, RWMutex, Cond, AtomicI64
//
// All synchronization primitives use opaque heap-allocated handles so the
// Tin struct definitions don't need to know the underlying sizes.

#include <stdlib.h>
#include <stdio.h>
#include <stdint.h>
#include <pthread.h>

// --- Mutex ---

void *_tin_mutex_new(void) {
    pthread_mutex_t *m = (pthread_mutex_t *)malloc(sizeof(pthread_mutex_t));
    if (!m) { fputs("tin: mutex alloc failed\n", stderr); exit(1); }
    pthread_mutex_init(m, NULL);
    return m;
}
void _tin_mutex_lock(void *m)   { pthread_mutex_lock((pthread_mutex_t *)m); }
void _tin_mutex_unlock(void *m) { pthread_mutex_unlock((pthread_mutex_t *)m); }
void _tin_mutex_free(void *m)   {
    pthread_mutex_destroy((pthread_mutex_t *)m);
    free(m);
}

// --- RWMutex ---

void *_tin_rwmutex_new(void) {
    pthread_rwlock_t *m = (pthread_rwlock_t *)malloc(sizeof(pthread_rwlock_t));
    if (!m) { fputs("tin: rwmutex alloc failed\n", stderr); exit(1); }
    pthread_rwlock_init(m, NULL);
    return m;
}
void _tin_rwmutex_rlock(void *m)   { pthread_rwlock_rdlock((pthread_rwlock_t *)m); }
void _tin_rwmutex_runlock(void *m) { pthread_rwlock_unlock((pthread_rwlock_t *)m); }
void _tin_rwmutex_lock(void *m)    { pthread_rwlock_wrlock((pthread_rwlock_t *)m); }
void _tin_rwmutex_unlock(void *m)  { pthread_rwlock_unlock((pthread_rwlock_t *)m); }
void _tin_rwmutex_free(void *m) {
    pthread_rwlock_destroy((pthread_rwlock_t *)m);
    free(m);
}

// --- Cond ---

void *_tin_cond_new(void) {
    pthread_cond_t *c = (pthread_cond_t *)malloc(sizeof(pthread_cond_t));
    if (!c) { fputs("tin: cond alloc failed\n", stderr); exit(1); }
    pthread_cond_init(c, NULL);
    return c;
}
// m is the raw pthread_mutex_t* stored as *void in the Mutex struct.
void _tin_cond_wait(void *c, void *m) {
    pthread_cond_wait((pthread_cond_t *)c, (pthread_mutex_t *)m);
}
void _tin_cond_signal(void *c)    { pthread_cond_signal((pthread_cond_t *)c); }
void _tin_cond_broadcast(void *c) { pthread_cond_broadcast((pthread_cond_t *)c); }
void _tin_cond_free(void *c) {
    pthread_cond_destroy((pthread_cond_t *)c);
    free(c);
}

// --- AtomicI64 ---

void *_tin_atomic_new_i64(int64_t v) {
    int64_t *p = (int64_t *)malloc(sizeof(int64_t));
    if (!p) { fputs("tin: atomic alloc failed\n", stderr); exit(1); }
    __atomic_store_n(p, v, __ATOMIC_RELAXED);
    return p;
}
int64_t _tin_atomic_load_i64(void *a) {
    return __atomic_load_n((int64_t *)a, __ATOMIC_ACQUIRE);
}
void _tin_atomic_store_i64(void *a, int64_t v) {
    __atomic_store_n((int64_t *)a, v, __ATOMIC_RELEASE);
}
int64_t _tin_atomic_add_i64(void *a, int64_t delta) {
    return __atomic_fetch_add((int64_t *)a, delta, __ATOMIC_ACQ_REL) + delta;
}
int64_t _tin_atomic_cas_i64(void *a, int64_t old_val, int64_t new_val) {
    __atomic_compare_exchange_n((int64_t *)a, &old_val, new_val,
        0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE);
    return old_val;
}
