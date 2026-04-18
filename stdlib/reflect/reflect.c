// stdlib/reflect - C implementation of type-atom reflection functions
//
// Compile alongside reflect.tin via: //!+reflect.c -- -I $TIN_RUNTIME
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
    "i8","i16","i32","i64","i128",
    "u8","u16","u32","u64","u128",
    "f32","f64","f128",
    "bool","string","char","void","atom","byte","int",NULL
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

// make_immortal_string: registers the string in the runtime atom table and
// returns a pointer to the table's own copy of the string.  The runtime table
// owns the memory (via strdup) and it remains reachable for the program's
// lifetime, so valgrind does not report it as a leak.
// Callers pass the raw type name (no apostrophe prefix); wrapFromExtern will
// call __tin_string_to_atom on the returned pointer to get the atom value.
static const char *make_immortal_string(const char *s, size_t len) {
    // Build a temporary null-terminated copy (stack-allocated for small sizes).
    char tmp[512];
    if (len >= sizeof(tmp)) len = sizeof(tmp) - 1;
    memcpy(tmp, s, len);
    tmp[len] = '\0';
    // Register in the runtime table; the table strdup's the string so the
    // storage persists until the process exits (and is reachable, not "lost").
    int32_t code = _tin_learn_atom(tmp);
    const char *stored = _tin_rt_atom_to_str(code);
    return stored ? stored : _rk_empty.data;
}

// elem: inner type atom for pointer or array
//   "*point" -> "point"
//   "[i64]"  -> "i64"
//   otherwise -> ""
// Returns an immortal string so wrapFromExtern can convert i8* -> atom safely.
const char *_tin_reflect_elem(const char *atom) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (*s == '*') {
        const char *inner = s + 1;
        return make_immortal_string(inner, strlen(inner));
    }
    if (*s == '[') {
        // "[T]" -> "T": strip '[' and trailing ']'
        const char *inner = s + 1;
        size_t len = strlen(inner);
        if (len > 0 && inner[len - 1] == ']') len--;
        return make_immortal_string(inner, len);
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
// Returns an immortal string so wrapFromExtern can convert i8* -> atom safely.
const char *_tin_reflect_fn_ret(const char *atom) {
    if (!atom) return _rk_empty.data;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return _rk_empty.data;
    const char *after = _reflect_find_params_end(s + 2); // s+2 points to '('
    if (!after || *after == '\0') return _rk_empty.data;
    size_t len = strlen(after);
    if (len == 0) return _rk_empty.data;
    return make_immortal_string(after, len);
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
// Returns an immortal string so wrapFromExtern can convert i8* -> atom safely.
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
    return make_immortal_string(start, plen);
}

// Cache node for _tin_reflect_fn_params results.
typedef struct _AtomArrNode {
    char                *spec;  // owning copy of the atom spec string
    TinRCHdr            *hdr;   // the allocation (hdr+1 is the TinAtom array)
    int64_t              arity;
    struct _AtomArrNode *next;
} AtomArrNode;

static AtomArrNode *_atom_arr_head = NULL;

// Free all cached fn_params entries at program exit.
static void _free_atom_arr_cache(void) {
    AtomArrNode *n = _atom_arr_head;
    while (n) {
        AtomArrNode *next = n->next;
        free(n->spec);
        free(n->hdr);
        free(n);
        n = next;
    }
    _atom_arr_head = NULL;
}

// fn_params: returns a TinAtomArray of parameter type atoms.
// Each element is a TinAtom { int32_t code } matching Tin's %__atom type.
// Atom codes are registered in the runtime table via _tin_learn_atom so that
// __tin_string_to_atom / __tin_atom_to_string can resolve them later.
// Results are cached: each unique atom spec is allocated exactly once and freed at exit.
TinAtomArray _tin_reflect_fn_params(const char *atom) {
    TinAtomArray empty = { NULL, 0 };
    if (!atom) return empty;
    char sbuf[256];
    const char *s = atom_spec(atom, sbuf);
    if (strncmp(s, "fn(", 3) != 0) return empty;
    const char *p = s + 3;
    if (*p == ')') return empty;

    // Check cache first.
    for (AtomArrNode *n = _atom_arr_head; n; n = n->next) {
        if (strcmp(n->spec, s) == 0)
            return (TinAtomArray){ (TinAtom *)(n->hdr + 1), n->arity };
    }

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

    // Register atexit once to free all cached entries at program exit.
    static int _atexit_registered = 0;
    if (!_atexit_registered) {
        _atexit_registered = 1;
        atexit(_free_atom_arr_cache);
    }

    // Allocate with a full TinRCHdr (16 bytes) so _tin_release reads the
    // immortal sentinel at the correct offset (ptr - 16).
    size_t arr_size = (size_t)arity * sizeof(TinAtom);
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + arr_size);
    if (!hdr) return empty;
    hdr->rc   = TIN_IMMORTAL_RC;
    hdr->_pad = 0;
    TinAtom *arr = (TinAtom *)(hdr + 1);

    for (int64_t i = 0; i < arity; i++) {
        char tmp[256];
        size_t copy_len = lens[i] < 255 ? lens[i] : 255;
        memcpy(tmp, starts[i], copy_len);
        tmp[copy_len] = '\0';
        arr[i].code = _tin_learn_atom(tmp);
    }

    AtomArrNode *node = (AtomArrNode *)malloc(sizeof(AtomArrNode));
    if (node) {
        node->spec  = strdup(s);
        node->hdr   = hdr;
        node->arity = arity;
        node->next  = _atom_arr_head;
        _atom_arr_head = node;
    }

    return (TinAtomArray){ arr, arity };
}
