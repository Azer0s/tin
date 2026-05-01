// tin runtime - link-time impl table walker.
//
// Each `impl Trait for Struct` declared in a Tin package is materialized
// by codegen as one TinImplEntry global in a custom section. At program
// start the runtime walks every entry in that section and folds them
// into `_tin_impl_table`, the (type_id -> [trait_atom_code]) table that
// `traitof()` reads.
//
// Section name varies by platform:
//   ELF (Linux, FreeBSD)    "tin_impl"   - linker auto-synthesizes the
//                                          __start_tin_impl/__stop_tin_impl
//                                          symbols when the name is a valid
//                                          C identifier.
//   Mach-O (macOS, iOS)     "__DATA,__tin_impl" - read at runtime via
//                                          getsectiondata().
//
// Impl entries are emitted as `__attribute__((used))` (mirrored in IR via
// @llvm.compiler.used), so neither --gc-sections (lld) nor -dead_strip
// (ld64) prunes them.

#include "runtime.h"
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>

typedef struct {
    int32_t     type_id;
    int32_t     _reserved;     // placeholder for future stable trait id
    const char *trait_name;    // NUL-terminated string in .rodata
} TinImplEntry;

// Per-type bucket: small-vector of resolved trait atom codes.
typedef struct {
    int32_t   type_id;
    int32_t   count;
    int32_t   cap;
    int32_t  *atoms;           // resolved atom codes
} TinImplBucket;

static TinImplBucket *_tin_impl_buckets    = NULL;
static int32_t        _tin_impl_bucket_n   = 0;
static int32_t        _tin_impl_bucket_cap = 0;
static pthread_once_t _tin_impl_table_once = PTHREAD_ONCE_INIT;

static TinImplBucket *_tin_impl_bucket_for(int32_t type_id, int create) {
    for (int32_t i = 0; i < _tin_impl_bucket_n; i++) {
        if (_tin_impl_buckets[i].type_id == type_id) {
            return &_tin_impl_buckets[i];
        }
    }
    if (!create) return NULL;
    if (_tin_impl_bucket_n == _tin_impl_bucket_cap) {
        int32_t ncap = _tin_impl_bucket_cap == 0 ? 16 : _tin_impl_bucket_cap * 2;
        TinImplBucket *nb = (TinImplBucket *)realloc(
            _tin_impl_buckets, (size_t)ncap * sizeof(TinImplBucket));
        if (nb == NULL) {
            fputs("tin: impl table OOM\n", stderr);
            exit(1);
        }
        _tin_impl_buckets    = nb;
        _tin_impl_bucket_cap = ncap;
    }
    TinImplBucket *b = &_tin_impl_buckets[_tin_impl_bucket_n++];
    b->type_id = type_id;
    b->count   = 0;
    b->cap     = 0;
    b->atoms   = NULL;
    return b;
}

static void _tin_impl_bucket_push(TinImplBucket *b, int32_t atom) {
    // Linear-scan dedup: same impl can't appear twice for one (S, T)
    // pair (link error), but the same trait atom can show up via multiple
    // structs sharing trait names. Bucket is per-type, so dedup within.
    for (int32_t i = 0; i < b->count; i++) {
        if (b->atoms[i] == atom) return;
    }
    if (b->count == b->cap) {
        int32_t ncap = b->cap == 0 ? 4 : b->cap * 2;
        int32_t *na  = (int32_t *)realloc(b->atoms, (size_t)ncap * sizeof(int32_t));
        if (na == NULL) {
            fputs("tin: impl table OOM\n", stderr);
            exit(1);
        }
        b->atoms = na;
        b->cap   = ncap;
    }
    b->atoms[b->count++] = atom;
}

#ifdef __APPLE__
#include <mach-o/getsect.h>
#include <mach-o/dyld.h>

extern const struct mach_header_64 _mh_execute_header;

static void _tin_iter_impl_section(void (*cb)(const TinImplEntry *)) {
    unsigned long sz = 0;
    const TinImplEntry *base = (const TinImplEntry *)getsectiondata(
        (const struct mach_header_64 *)&_mh_execute_header,
        "__DATA", "__tin_impl", &sz);
    if (base == NULL || sz == 0) return;
    size_t n = sz / sizeof(TinImplEntry);
    for (size_t i = 0; i < n; i++) cb(&base[i]);
}
#else
// Weak refs so a binary that emits no impls (no `impl X for Y` anywhere)
// still links: the section is empty and __start_/__stop_ resolve to NULL.
extern const TinImplEntry __start_tin_impl[] __attribute__((weak));
extern const TinImplEntry __stop_tin_impl[]  __attribute__((weak));

static void _tin_iter_impl_section(void (*cb)(const TinImplEntry *)) {
    if (__start_tin_impl == NULL || __stop_tin_impl == NULL) return;
    for (const TinImplEntry *e = __start_tin_impl; e < __stop_tin_impl; e++) {
        cb(e);
    }
}
#endif

static void _tin_impl_record(const TinImplEntry *e) {
    if (e == NULL || e->trait_name == NULL) return;
    int32_t atom = _tin_learn_atom(e->trait_name);
    TinImplBucket *b = _tin_impl_bucket_for(e->type_id, 1);
    _tin_impl_bucket_push(b, atom);
}

static void _tin_impl_table_init(void) {
    _tin_iter_impl_section(_tin_impl_record);
}

void _tin_build_impl_table(void) {
    pthread_once(&_tin_impl_table_once, _tin_impl_table_init);
}

// Query: number of trait atoms for a given struct type id (0 if unknown).
int32_t _tin_impl_count_for_type(int32_t type_id) {
    _tin_build_impl_table();
    TinImplBucket *b = _tin_impl_bucket_for(type_id, 0);
    return b ? b->count : 0;
}

// Query: i-th trait atom for a given struct type id (0 if out of range).
int32_t _tin_impl_atom_for_type(int32_t type_id, int32_t idx) {
    _tin_build_impl_table();
    TinImplBucket *b = _tin_impl_bucket_for(type_id, 0);
    if (b == NULL || idx < 0 || idx >= b->count) return 0;
    return b->atoms[idx];
}

__attribute__((destructor)) static void _tin_impl_table_cleanup(void) {
    for (int32_t i = 0; i < _tin_impl_bucket_n; i++) {
        free(_tin_impl_buckets[i].atoms);
    }
    free(_tin_impl_buckets);
    _tin_impl_buckets    = NULL;
    _tin_impl_bucket_n   = 0;
    _tin_impl_bucket_cap = 0;
}
