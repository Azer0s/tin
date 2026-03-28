// stdlib/reflect - C implementation of type-atom reflection functions
//
// Compile alongside reflect.tin via: //!+reflect.c -- -I ../../runtime
// Depends on: _tin_rc_alloc, TIN_IMMORTAL_RC, TinString, TinStringArray (runtime.h)

#include "runtime.h"
#include <string.h>
#include <stdlib.h>

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
// surrounding double-quotes (e.g. '"fn(i64)bool"'). This function strips them.
static const char *atom_spec(const char *atom, char *buf) {
    if (!atom || atom[0] != '"') return atom; // simple atom: return as-is
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
//   "*point" -> "point"
//   "[i64]"  -> "i64"
//   otherwise -> ""
const char *_tin_reflect_elem(const char *atom) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (*s == '*') {
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

// Given a pointer to the '(' that opens a fn param list, return a pointer to
// the character after the matching ')'. Returns NULL on malformed input.
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
int64_t _tin_reflect_fn_arity(const char *atom) {
    if (!atom) return 0;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return 0;
    const char *p = s + 3;
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
const char *_tin_reflect_fn_param(const char *atom, int64_t idx) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return _rk_empty.data;
    const char *p = s + 3;
    if (*p == ')') return _rk_empty.data;

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

// fn_params: returns a TinStringArray of parameter type atoms.
// Memory is one _tin_rc_alloc block: TinString[N] + N immortal records.
TinStringArray _tin_reflect_fn_params(const char *atom) {
    TinStringArray empty = { NULL, 0 };
    if (!atom) return empty;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return empty;
    const char *p = s + 3;
    if (*p == ')') return empty;

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

    // Total buffer: TinString[arity] + N immortal records (8-byte aligned)
    size_t arr_size = (size_t)arity * sizeof(TinString);
    size_t total = arr_size;
    for (int64_t i = 0; i < arity; i++) {
        size_t rec_size = sizeof(int64_t) + lens[i] + 1;
        rec_size = (rec_size + 7) & ~(size_t)7;
        total += rec_size;
    }

    char *block = (char *)_tin_rc_alloc((int64_t)total);
    TinString *arr = (TinString *)block;
    char *rec = block + arr_size;

    for (int64_t i = 0; i < arity; i++) {
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
