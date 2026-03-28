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
    /* Compute code with collision resolution */
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
