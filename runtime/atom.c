// tin runtime - runtime atom table
//
// Atoms not known at compile time can be "learned" at runtime via
// _tin_learn_atom(). Learned atoms are stored in a linked list and can
// be resolved back to their string name via _tin_rt_atom_to_str().

#include "runtime.h"
#include <stdlib.h>
#include <string.h>
#include <pthread.h>

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
    TinRCHdr *hdr; /* head of the malloc block (hdr+1 == str); kept so that
                      valgrind sees a pointer to the allocation head and
                      classifies the block as "still reachable" rather than
                      "possibly lost" */
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

// Like _tin_learn_atom but takes ownership of str: frees it (if malloc'd) once
// the atom code has been resolved or a new entry created.
int32_t _tin_learn_atom_handover(char *str) {
    /* Pre-compute usable size before acquiring the lock. */
    size_t sz = _tin_usable_size(str);
    size_t copy_len = (sz > 0) ? sz : (strlen(str) + 1);

    pthread_mutex_lock(&_tin_rt_atom_mu);
    for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
        if (strcmp(n->str, str) == 0) {
            int32_t code = n->code;
            pthread_mutex_unlock(&_tin_rt_atom_mu);
            if (sz > 0) free(str);
            return code;
        }
    }
    int32_t code = (int32_t)_tin_crc32_str(str);
    if (code == 0) code = 1;
    int collision;
    int spins = 0;
    do {
        collision = 0;
        for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
            if (n->code == code && strcmp(n->str, str) != 0) {
                code++;
                if (code == 0) code = 1;
                collision = 1;
                if (++spins > (1 << 24)) {
                    pthread_mutex_unlock(&_tin_rt_atom_mu);
                    if (sz > 0) free(str);
                    fputs("tin: runtime atom table exhausted\n", stderr);
                    exit(1);
                }
                break;
            }
        }
    } while (collision);
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + copy_len);
    if (hdr == NULL) {
        pthread_mutex_unlock(&_tin_rt_atom_mu);
        if (sz > 0) free(str);
        fputs("tin: atom OOM\n", stderr);
        exit(1);
    }
    hdr->rc = TIN_IMMORTAL_RC;
    char *s = (char *)(hdr + 1);
    memcpy(s, str, copy_len);
    TinRtAtomNode *node = malloc(sizeof(TinRtAtomNode));
    if (node == NULL) {
        free(hdr);
        pthread_mutex_unlock(&_tin_rt_atom_mu);
        if (sz > 0) free(str);
        fputs("tin: atom OOM\n", stderr);
        exit(1);
    }
    node->code = code;
    node->hdr  = hdr;
    node->str  = s;
    node->next = _tin_rt_atom_head;
    _tin_rt_atom_head = node;
    pthread_mutex_unlock(&_tin_rt_atom_mu);
    if (sz > 0) free(str);
    return code;
}

// Free all runtime-learned atoms at program exit.
// The nodes and their TinRCHdr+string blocks were malloc'd by _tin_learn_atom /
// _tin_learn_atom_handover; the immortal RC sentinel prevents _tin_release from
// ever touching them, so an explicit destructor is needed.
__attribute__((destructor)) static void _tin_rt_atom_cleanup(void) {
    pthread_mutex_lock(&_tin_rt_atom_mu);
    TinRtAtomNode *n = _tin_rt_atom_head;
    _tin_rt_atom_head = NULL;
    pthread_mutex_unlock(&_tin_rt_atom_mu);
    while (n) {
        TinRtAtomNode *next = n->next;
        free(n->hdr);  /* frees TinRCHdr + string copy */
        free(n);
        n = next;
    }
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
    /* Compute code with collision resolution. Skip 0 (reserved as
     * "no atom" sentinel by the per-IP TLS cache in stacktrace.c) and
     * cap iterations: in the unreachable case where 2^31 distinct
     * codes are taken we'd otherwise spin forever. */
    int32_t code = (int32_t)_tin_crc32_str(str);
    if (code == 0) code = 1;
    int collision;
    int spins = 0;
    do {
        collision = 0;
        for (TinRtAtomNode *n = _tin_rt_atom_head; n; n = n->next) {
            if (n->code == code && strcmp(n->str, str) != 0) {
                code++;
                if (code == 0) code = 1;
                collision = 1;
                if (++spins > (1 << 24)) {
                    pthread_mutex_unlock(&_tin_rt_atom_mu);
                    fputs("tin: runtime atom table exhausted\n", stderr);
                    exit(1);
                }
                break;
            }
        }
    } while (collision);
    /* Prepend a new node; allocate the string as an immortal ARC block so
     * that _tin_retain/_tin_release on it are safe no-ops. malloc may
     * fail under stress (eg lots of distinct stacktrace frames); abort
     * cleanly rather than dereferencing NULL inside the lock. */
    size_t len = strlen(str);
    TinRCHdr *hdr = (TinRCHdr *)malloc(sizeof(TinRCHdr) + len + 1);
    if (hdr == NULL) {
        pthread_mutex_unlock(&_tin_rt_atom_mu);
        fputs("tin: atom OOM\n", stderr);
        exit(1);
    }
    hdr->rc = TIN_IMMORTAL_RC;
    char *s = (char *)(hdr + 1);
    memcpy(s, str, len + 1);
    TinRtAtomNode *node = malloc(sizeof(TinRtAtomNode));
    if (node == NULL) {
        free(hdr);
        pthread_mutex_unlock(&_tin_rt_atom_mu);
        fputs("tin: atom OOM\n", stderr);
        exit(1);
    }
    node->code = code;
    node->hdr  = hdr;
    node->str  = s;
    node->next = _tin_rt_atom_head;
    _tin_rt_atom_head = node;
    pthread_mutex_unlock(&_tin_rt_atom_mu);
    return code;
}
