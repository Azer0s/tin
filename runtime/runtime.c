// tin runtime library
//
// Helper functions that the LLVM IR emitted by the tin compiler calls for
// things that are easier to implement in C than raw IR:
//   - typed echo helpers
//   - string/array operations
//   - memory helpers
//
// All symbols are prefixed with _tin_ to avoid collisions

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdbool.h>
#include <setjmp.h>
#include <pthread.h>

// -- Typed echo helpers

void _tin_echo_i64(int64_t v)  { printf("%lld\n",  (long long)v); }
void _tin_echo_u64(uint64_t v) { printf("%llu\n",  (unsigned long long)v); }
void _tin_echo_f64(double v)   { printf("%g\n",    v); }
void _tin_echo_bool(int32_t v) { puts(v ? "true" : "false"); }
void _tin_echo_char(uint8_t v) { printf("%c\n",    v); }

// string representation: { i8* ptr, i64 len }
typedef struct { const char *ptr; int64_t len; } TinString;

void _tin_echo_string(TinString s) {
    printf("%.*s\n", (int)s.len, s.ptr);
}

// Print without trailing newline (used for interpolation pieces)
void _tin_print_i64(int64_t v)  { printf("%lld",  (long long)v); }
void _tin_print_u64(uint64_t v) { printf("%llu",  (unsigned long long)v); }
void _tin_print_f64(double v)   { printf("%g",    v); }
void _tin_print_bool(int32_t v) { fputs(v ? "true" : "false", stdout); }
void _tin_print_char(uint8_t v) { putchar(v); }
void _tin_print_string(TinString s) { printf("%.*s", (int)s.len, s.ptr); }
void _tin_print_newline(void)   { putchar('\n'); }

// -- String operations

// Concatenate two strings; returns a heap-allocated TinString
TinString _tin_str_concat(TinString a, TinString b) {
    int64_t len = a.len + b.len;
    char *buf = (char *)malloc((size_t)(len + 1));
    if (!buf) { fputs("tin: out of memory\n", stderr); exit(1); }
    memcpy(buf, a.ptr, (size_t)a.len);
    memcpy(buf + a.len, b.ptr, (size_t)b.len);
    buf[len] = '\0';
    return (TinString){ buf, len };
}

// Wrap a C string literal in a TinString (no copy)
TinString _tin_str_from_cstr(const char *s) {
    return (TinString){ s, (int64_t)strlen(s) };
}

// Compare two strings
int32_t _tin_str_eq(TinString a, TinString b) {
    if (a.len != b.len) return 0;
    return memcmp(a.ptr, b.ptr, (size_t)a.len) == 0;
}

// Forward declaration for ARC alloc used by slice operations below
void *_tin_rc_alloc(int64_t size);

// -- Dynamic array (slice) operations

// slice representation: { T* ptr, i64 len } -- same layout as TinString
typedef struct { void *ptr; int64_t len; } TinSlice;

// Append one element to a slice (elem_size bytes); returns a new slice
// The old slice data is NOT freed -- the caller manages it via ARC
TinSlice _tin_slice_append(TinSlice s, const void *elem, int64_t elem_size) {
    int64_t new_len = s.len + 1;
    void *new_ptr = _tin_rc_alloc(new_len * elem_size);
    if (s.ptr) memcpy(new_ptr, s.ptr, (size_t)(s.len * elem_size));
    memcpy((char *)new_ptr + s.len * elem_size, elem, (size_t)elem_size);
    return (TinSlice){ new_ptr, new_len };
}

// Concatenate two slices. Neither input is freed -- the caller manages them via ARC
TinSlice _tin_slice_concat(TinSlice a, TinSlice b, int64_t elem_size) {
    int64_t new_len = a.len + b.len;
    void *buf = _tin_rc_alloc((int64_t)(new_len * elem_size));
    if (a.ptr) memcpy(buf,                             a.ptr, (size_t)(a.len * elem_size));
    if (b.ptr) memcpy((char *)buf + a.len * elem_size, b.ptr, (size_t)(b.len * elem_size));
    return (TinSlice){ buf, new_len };
}

// Return the length of a slice
int64_t _tin_slice_len(TinSlice s) { return s.len; }

// Bounds-checked index
void *_tin_slice_idx(TinSlice s, int64_t i, int64_t elem_size) {
    if (i < 0 || i >= s.len) {
        fprintf(stderr, "tin: index %lld out of bounds (len=%lld)\n",
                (long long)i, (long long)s.len);
        exit(1);
    }
    return (char *)s.ptr + i * elem_size;
}

// -- Memory helpers

void *_tin_malloc(int64_t size) {
    void *p = malloc((size_t)size);
    if (!p) { fputs("tin: out of memory\n", stderr); exit(1); }
    return p;
}

void _tin_free(void *p) { free(p); }

// -- ARC (Automatic Reference Counting)

// Every heap block managed by ARC is preceded by a TinRCHdr with a reference
// count. The public pointer (stored in fat-pointer data fields) points past
// the header to the actual data
//
// Static string literals embed an immortal sentinel rc = TIN_IMMORTAL_RC (-1)
// right before their data so _tin_retain/_tin_release never touch them
#define TIN_IMMORTAL_RC ((int64_t)-1)

typedef struct { int64_t rc; } TinRCHdr;

static inline TinRCHdr *_rc_hdr(void *ptr) {
    return (TinRCHdr *)((char *)ptr - sizeof(TinRCHdr));
}

// Allocate an ARC-managed block of `size` bytes; starts with rc = 1
void *_tin_rc_alloc(int64_t size) {
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + (size_t)size);
    if (!hdr) { fputs("tin: out of memory\n", stderr); exit(1); }
    hdr->rc = 1;
    return hdr + 1;
}

// Increment reference count; skips immortal (rc == -1) and NULL pointers
void _tin_retain(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (hdr->rc == TIN_IMMORTAL_RC) return;
    hdr->rc++;
}

// Decrement reference count; frees the block when it reaches zero
void _tin_release(void *ptr) {
    if (!ptr) return;
    TinRCHdr *hdr = _rc_hdr(ptr);
    if (hdr->rc == TIN_IMMORTAL_RC) return;
    if (--hdr->rc == 0) free(hdr);
}

// -- Defer chain for cross-frame stack unwinding

// Each deferred call is a TinDeferEntry allocated on the registering function's
// stack, linked newest-first so _tin_panic can walk them in LIFO order
// Layout: { prev*, fn*, env* } -- matches the three-pointer struct in LLVM IR
typedef struct TinDeferEntry {
    struct TinDeferEntry *prev;   // previous entry in chain (older)
    void (*fn)(void *env);        // thunk that executes the deferred call
    void *env;                    // heap-allocated env captured at defer-site
} TinDeferEntry;

static TinDeferEntry *_tin_defer_chain = NULL;

// Called at every `defer` statement; entry points to a TinDeferEntry on the calling frame's stack
void _tin_defer_push(TinDeferEntry *entry, void (*fn)(void *), void *env) {
    entry->fn   = fn;
    entry->env  = env;
    entry->prev = _tin_defer_chain;
    _tin_defer_chain = entry;
}

// Called before a normal return to deregister the n most-recent defers (already run inline)
void _tin_defer_pop(int64_t n) {
    for (int64_t i = 0; i < n && _tin_defer_chain != NULL; i++)
        _tin_defer_chain = _tin_defer_chain->prev;
}

// -- Panic / assert

// Return a sub-slice starting at index `start`, with `elem_size`-byte elements.
// Allocates a fresh copy with an ARC header (rc=1) so retain/release work correctly.
TinSlice _tin_slice_subslice(TinSlice s, int64_t start, int64_t elem_size) {
    if (start >= s.len) return (TinSlice){NULL, 0};
    int64_t new_len = s.len - start;
    size_t data_size = (size_t)(new_len * elem_size);
    char *block = (char *)malloc(sizeof(int64_t) + data_size);
    *(int64_t *)block = 1;  /* rc = 1 */
    if (data_size > 0) memcpy(block + sizeof(int64_t), (char *)s.ptr + start * elem_size, data_size);
    return (TinSlice){block + sizeof(int64_t), new_len};
}

// recover() support: stores the current panic message so a deferred function
// can retrieve and clear it via _tin_recover().
static const char *_tin_panic_msg = NULL;

// An immortal (rc=-1) empty string returned by _tin_recover() when not panicking.
// The TIN_IMMORTAL_RC sentinel is placed immediately before the data byte so that
// _tin_release() treats this pointer as a static and skips deallocation.
static const struct { int64_t rc; char c; } _tin_empty_str_sentinel = { TIN_IMMORTAL_RC, '\0' };

// Called from a deferred function to retrieve and clear the current panic message.
// Returns an empty TinString if not currently panicking.
TinString _tin_recover(void) {
    if (_tin_panic_msg == NULL)
        return (TinString){ (const char *)&_tin_empty_str_sentinel.c, 0 };
    TinString s = _tin_str_from_cstr(_tin_panic_msg);
    _tin_panic_msg = NULL;  // marks as recovered
    return s;
}

// Run all pending deferred calls in LIFO order, print the panic message, then exit.
// Defers run first so cleanup output appears before the fatal message.
// If a deferred function calls recover(), _tin_panic_msg is cleared and we return.
void _tin_panic(const char *msg) {
    _tin_panic_msg = msg;
    TinDeferEntry *e = _tin_defer_chain;
    _tin_defer_chain = NULL; // prevent re-entrant panic from re-running
    while (e != NULL) {
        TinDeferEntry *next = e->prev;
        if (e->fn) e->fn(e->env);
        e = next;
    }
    if (_tin_panic_msg == NULL) return;  // a deferred fn called recover()
    fflush(stdout);
    fprintf(stderr, "tin panic: %s\n", msg);
    exit(1);
}

void _tin_assert(int32_t cond, const char *msg) {
    if (!cond) _tin_panic(msg);
}

// -- Length builtin

int64_t _tin_len_string(TinString s) { return s.len; }
int64_t _tin_len_slice(TinSlice s)   { return s.len; }

// -- reflect helpers

// Type atoms have a "'" prefix followed by the type spec:
//   'i64, 'f64       -- primitives
//   '*point          -- pointer to point
//   '[i64]           -- array of i64
//   'fn(i64,f64)bool -- function type
//
// Input char* values are Tin string data (ARC header sits 8 bytes before)
// Returned char* values are either immortal statics or RC-allocated (rc=1)

// Immortal string literals for kind() return values
static struct { int64_t rc; char data[12]; } _rk_ptr       = {TIN_IMMORTAL_RC, "ptr"};
static struct { int64_t rc; char data[12]; } _rk_array      = {TIN_IMMORTAL_RC, "array"};
static struct { int64_t rc; char data[12]; } _rk_fn         = {TIN_IMMORTAL_RC, "fn"};
static struct { int64_t rc; char data[12]; } _rk_primitive  = {TIN_IMMORTAL_RC, "primitive"};
static struct { int64_t rc; char data[12]; } _rk_struct     = {TIN_IMMORTAL_RC, "struct"};
static struct { int64_t rc; char data[4];  } _rk_empty      = {TIN_IMMORTAL_RC, ""};

static const char *_tin_primitives[] = {
    "i8","i16","i32","i64","u8","u16","u32","u64",
    "f32","f64","bool","string","char","void","atom","byte","int",NULL
};

static int _tin_is_primitive_name(const char *s) {
    for (int i = 0; _tin_primitives[i]; i++)
        if (strcmp(s, _tin_primitives[i]) == 0) return 1;
    return 0;
}

// Returns the type-spec portion of an atom string.
// Atoms are stored as bare names (no leading apostrophe).
// Complex atoms whose names contain non-identifier chars are stored with
// surrounding double-quotes preserved (e.g. '"fn(i64)bool"').
// This function strips those quotes, writing the result into buf.
static const char *atom_spec(const char *atom, char *buf) {
    if (!atom || atom[0] != '"') return atom; // simple atom: return as-is
    // complex atom: '"fn(i64)bool"' -> strip surrounding double-quotes
    const char *s = atom + 1; // skip opening "
    size_t len = strlen(s);
    if (len > 0 && s[len - 1] == '"') len--; // strip trailing "
    if (len >= 256) len = 255;
    memcpy(buf, s, len);
    buf[len] = '\0';
    return buf;
}

// Returns the category of a type atom: "ptr", "array", "fn", "primitive", or "struct"
const char *_tin_reflect_kind(const char *atom) {
    if (!atom) return _rk_empty.data;
    char buf[256];
    const char *s = atom_spec(atom, buf);
    if (*s == '*')                      return _rk_ptr.data;
    if (*s == '[')                      return _rk_array.data;
    if (strncmp(s, "fn(", 3) == 0)     return _rk_fn.data;
    if (_tin_is_primitive_name(s))      return _rk_primitive.data;
    return _rk_struct.data;
}

// is_ptr: 1 if atom is a pointer type, 0 otherwise
int64_t _tin_reflect_is_ptr(const char *atom) {
    if (!atom) return 0;
    char buf[256];
    return atom_spec(atom, buf)[0] == '*' ? 1 : 0;
}

// is_array: 1 if atom is an array type, 0 otherwise
int64_t _tin_reflect_is_array(const char *atom) {
    if (!atom) return 0;
    char buf[256];
    return atom_spec(atom, buf)[0] == '[' ? 1 : 0;
}

// is_fn: 1 if atom is a function type, 0 otherwise
int64_t _tin_reflect_is_fn(const char *atom) {
    if (!atom) return 0;
    char buf[256];
    return strncmp(atom_spec(atom, buf), "fn(", 3) == 0 ? 1 : 0;
}

// is_primitive: 1 if atom is a primitive type, 0 otherwise
int64_t _tin_reflect_is_primitive(const char *atom) {
    if (!atom) return 0;
    char buf[256];
    return _tin_is_primitive_name(atom_spec(atom, buf)) ? 1 : 0;
}

// elem: inner type atom for pointer or array
//   "'*point" -> "'point"
//   "'[i64]"  -> "'i64"
//   otherwise -> ""
const char *_tin_reflect_elem(const char *atom) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (*s == '*') {
        // "*T" -> "T"
        const char *inner = s + 1;
        size_t len = strlen(inner);
        char *buf = (char *)_tin_rc_alloc((int64_t)(len + 1));
        memcpy(buf, inner, len);
        buf[len] = '\0';
        return buf;
    }
    if (*s == '[') {
        // "[T]" -> "T": strip '[' and trailing ']'
        const char *inner = s + 1;
        size_t len = strlen(inner);
        if (len > 0 && inner[len - 1] == ']') len--;
        char *buf = (char *)_tin_rc_alloc((int64_t)(len + 1));
        memcpy(buf, inner, len);
        buf[len] = '\0';
        return buf;
    }
    return _rk_empty.data;
}

// _reflect_find_params_end: given a pointer to the '(' that opens the param
// list of a fn type, scan forward with depth tracking and return a pointer to
// the character AFTER the matching ')'.  Returns NULL on malformed input
static const char *_reflect_find_params_end(const char *open_paren) {
    const char *p = open_paren;
    int depth = 0;
    while (*p) {
        if (*p == '(') depth++;
        else if (*p == ')') {
            depth--;
            if (depth == 0) return p + 1;
        }
        p++;
    }
    return NULL;
}

// fn_ret: return type atom for a function type
//   "fn(i64,f64)bool" -> "bool"
//   non-fn types      -> ""
const char *_tin_reflect_fn_ret(const char *atom) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return _rk_empty.data;
    const char *after = _reflect_find_params_end(s + 2); // s+2 points to '('
    if (!after || *after == '\0') return _rk_empty.data;
    size_t len = strlen(after);
    if (len == 0) return _rk_empty.data;
    char *buf = (char *)_tin_rc_alloc((int64_t)(len + 1));
    memcpy(buf, after, len);
    buf[len] = '\0';
    return buf;
}

// fn_arity: number of parameters in a function type atom
//   "'fn()"          -> 0
//   "'fn(i64)"       -> 1
//   "'fn(i64,f64)"   -> 2
//   non-fn types     -> 0
int64_t _tin_reflect_fn_arity(const char *atom) {
    if (!atom) return 0;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return 0;
    const char *p = s + 3; // first char inside '('
    if (*p == ')') return 0;
    int64_t count = 1;
    int depth = 0;
    while (*p && !(*p == ')' && depth == 0)) {
        if (*p == '(') depth++;
        else if (*p == ')') depth--;
        else if (*p == ',' && depth == 0) count++;
        p++;
    }
    return count;
}

// fn_param: type atom of the idx-th parameter (0-based)
//   "'fn(i64,f64)bool", 0 -> "'i64"
//   "'fn(i64,f64)bool", 1 -> "'f64"
//   out of range or non-fn -> ""
const char *_tin_reflect_fn_param(const char *atom, int64_t idx) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return _rk_empty.data;
    const char *p = s + 3;
    if (*p == ')') return _rk_empty.data; // no params

    int64_t cur = 0;
    const char *start = p;
    int depth = 0;

    while (*p && !(*p == ')' && depth == 0)) {
        if (*p == '(') depth++;
        else if (*p == ')') depth--;
        else if (*p == ',' && depth == 0) {
            if (cur == idx) goto found;
            cur++;
            start = p + 1;
        }
        p++;
    }
    if (cur != idx) return _rk_empty.data;

found:;
    size_t plen = (size_t)(p - start);
    char *buf = (char *)_tin_rc_alloc((int64_t)(plen + 1));
    memcpy(buf, start, plen);
    buf[plen] = '\0';
    return buf;
}

// -- fn_params
// Returns a TinString array ({ TinString*, int64_t }) of type atoms for each
// parameter. Memory is one _tin_rc_alloc block: TinString[N] followed by N
// immortal records { int64_t rc=-1; char data[plen+2]; }.
// Each TinString.ptr points into its immortal record; releasing the outer
// array frees everything. Non-fn types and zero-param fns return { NULL, 0 }
typedef struct { TinString *ptr; int64_t len; } TinStringArray;

TinStringArray _tin_reflect_fn_params(const char *atom) {
    TinStringArray empty = { NULL, 0 };
    if (!atom) return empty;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return empty;
    const char *p = s + 3;
    if (*p == ')') return empty; // zero-param fn

    // First pass: count params and collect (start,len) for each
    #define MAX_PARAMS 64
    const char *starts[MAX_PARAMS];
    size_t lens[MAX_PARAMS];
    int64_t arity = 0;
    const char *start = p;
    int depth = 0;

    while (*p && !(*p == ')' && depth == 0)) {
        if (*p == '(') depth++;
        else if (*p == ')') depth--;
        else if (*p == ',' && depth == 0) {
            if (arity < MAX_PARAMS) {
                starts[arity] = start;
                lens[arity] = (size_t)(p - start);
            }
            arity++;
            start = p + 1;
        }
        p++;
    }
    // last param
    if (arity < MAX_PARAMS) {
        starts[arity] = start;
        lens[arity] = (size_t)(p - start);
    }
    arity++;

    if (arity > MAX_PARAMS) arity = MAX_PARAMS;

    // Total buffer: TinString[arity] + N immortal records, each 8-byte aligned
    size_t arr_size = (size_t)arity * sizeof(TinString);
    size_t total = arr_size;
    for (int64_t i = 0; i < arity; i++) {
        size_t rec_size = sizeof(int64_t) + lens[i] + 1; // rc + data + '\0'
        // round up to 8-byte alignment
        rec_size = (rec_size + 7) & ~(size_t)7;
        total += rec_size;
    }

    char *block = (char *)_tin_rc_alloc((int64_t)total);
    TinString *arr = (TinString *)block;
    char *rec = block + arr_size;

    for (int64_t i = 0; i < arity; i++) {
        // write immortal sentinel
        int64_t *rc_field = (int64_t *)rec;
        *rc_field = -1; // TIN_IMMORTAL_RC
        char *data = rec + sizeof(int64_t);
        memcpy(data, starts[i], lens[i]);
        data[lens[i]] = '\0';

        arr[i].ptr = data;
        arr[i].len = (int64_t)lens[i];

        size_t rec_size = sizeof(int64_t) + lens[i] + 1;
        rec_size = (rec_size + 7) & ~(size_t)7;
        rec += rec_size;
    }

    return (TinStringArray){ arr, arity };
}

// -- Test runner
// _tin_run_test runs a single test via setjmp/longjmp so assertion failures
// are caught and reported without killing the whole run.
// _tin_test_finish prints a summary and returns 0 (all passed) or 1.
// Both are called from the generated test-runner main()

static jmp_buf  _tin_test_jmpbuf;
static int      _tin_test_active   = 0;
static int64_t  _tin_tests_failed  = 0;

void _tin_run_test(TinString desc, void *fn) {
    printf("test: %.*s ... ", (int)desc.len, desc.ptr);
    fflush(stdout);

    _tin_test_active = 1;
    if (setjmp(_tin_test_jmpbuf) == 0) {
        // Run the test body.
        void (*f)(void) = (void (*)(void))fn;
        f();
        _tin_test_active = 0;
        printf("ok\n");
    } else {
        // Jumped here by a failing assertion.
        _tin_test_active = 0;
        _tin_tests_failed++;
    }
}

int64_t _tin_test_finish(int64_t total) {
    int64_t passed = total - _tin_tests_failed;
    printf("\n");
    if (_tin_tests_failed == 0) {
        printf("%lld test%s passed.\n", (long long)total,
               total == 1 ? "" : "s");
    } else {
        printf("%lld/%lld test%s failed.\n",
               (long long)_tin_tests_failed, (long long)total,
               _tin_tests_failed == 1 ? "" : "s");
        (void)passed;
    }
    return _tin_tests_failed > 0 ? 1 : 0;
}

// -- Assert abort
// Sole native primitive used by the assert stdlib. Prints "FAILED", writes
// msg to stderr, then longjmps back to the test runner (or exits standalone)
// All assertion logic (comparisons, message formatting) lives in assert.tin
void _tin_assert_abort(const char *msg) {
    printf("FAILED\n");
    fprintf(stderr, "  %s\n", msg);
    if (_tin_test_active) {
        longjmp(_tin_test_jmpbuf, 1);
    } else {
        exit(1);
    }
}

// -- Runtime atom table
//
// Atoms not known at compile time can be "learned" at runtime via
// _tin_learn_atom().  Learned atoms are stored in a fixed-size table and can
// be resolved back to their string name via _tin_rt_atom_to_str().

static uint32_t _tin_crc32_str(const char *str) {
    uint32_t crc = 0xFFFFFFFFu;
    while (*str) {
        crc ^= (unsigned char)*str++;
        for (int k = 0; k < 8; k++)
            crc = (crc >> 1) ^ (0xEDB88320u & (uint32_t)(-(int32_t)(crc & 1u)));
    }
    return ~crc;
}

typedef struct TinRtAtomNode {
    int32_t code;
    char *str;
    struct TinRtAtomNode *next;
} TinRtAtomNode;

static TinRtAtomNode *_tin_rt_atom_head = NULL;
static pthread_mutex_t _tin_rt_atom_mu = PTHREAD_MUTEX_INITIALIZER;

const char *_tin_rt_atom_to_str(int32_t code) {
    pthread_mutex_lock(&_tin_rt_atom_mu);
    const char *result = NULL;
    for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
        if (n->code == code) { result = n->str; break; }
    }
    pthread_mutex_unlock(&_tin_rt_atom_mu);
    return result;
}

int32_t _tin_learn_atom(const char *str) {
    pthread_mutex_lock(&_tin_rt_atom_mu);
    /* Already learned? */
    for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
        if (strcmp(n->str, str) == 0) {
            int32_t code = n->code;
            pthread_mutex_unlock(&_tin_rt_atom_mu);
            return code;
        }
    }
    /* Compute code with collision resolution against the linked list */
    int32_t code = (int32_t)_tin_crc32_str(str);
    int collision;
    do {
        collision = 0;
        for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
            if (n->code == code && strcmp(n->str, str) != 0) {
                code++;
                collision = 1;
                break;
            }
        }
    } while (collision);
    /* Prepend a new node; strdup so the caller's storage need not persist */
    TinRtAtomNode *node = malloc(sizeof(TinRtAtomNode));
    node->code = code;
    node->str  = strdup(str);
    node->next = _tin_rt_atom_head;
    _tin_rt_atom_head = node;
    pthread_mutex_unlock(&_tin_rt_atom_mu);
    return code;
}

// -- any equality
// Runtime comparison for `any` values - dispatches on the type tag:
//   0=i64, 1=f64, 2=string/atom, 3=bool, else pointer equality
typedef struct { int32_t tag; void *ptr; } _TinAny;

int64_t _tin_any_eq(_TinAny a, _TinAny b) {
    if (a.tag != b.tag) return 0;
    switch (a.tag) {
    case 0: return *(int64_t *)a.ptr == *(int64_t *)b.ptr ? 1 : 0;   // i64
    case 1: return *(double  *)a.ptr == *(double  *)b.ptr ? 1 : 0;   // f64
    case 2: {  // string / atom: ptr -> TinString = {char*, i64}; compare char* *
        const char *sa = *(const char **)a.ptr;
        const char *sb = *(const char **)b.ptr;
        return strcmp(sa, sb) == 0 ? 1 : 0;
    }
    case 3: return *(uint8_t *)a.ptr == *(uint8_t *)b.ptr ? 1 : 0;   // bool
    default: return a.ptr == b.ptr ? 1 : 0;                           // ptr/struct/fn
    }
}
