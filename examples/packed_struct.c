#include <stdint.h>
#include <stdbool.h>

// Packed struct: no padding between tag (u8) and value (u32).
// Without packing: sizeof = 8 (4 bytes padding after tag).
// With packing:    sizeof = 5.
typedef struct __attribute__((packed)) {
    uint8_t  tag;
    uint32_t value;
} packed_hdr;

// Nested packed struct
typedef struct __attribute__((packed)) {
    uint8_t    kind;
    packed_hdr hdr;
    uint16_t   extra;
} packed_msg;

static packed_hdr g_hdr = {1, 0x12345678};
static packed_msg g_msg = {2, {3, 99999}, 1234};

packed_hdr *get_hdr(void)                                    { return &g_hdr; }
void        set_hdr(packed_hdr *h, uint8_t tag, uint32_t v)  { h->tag = tag; h->value = v; }
packed_msg *get_msg(void)                                    { return &g_msg; }
bool        verify_hdr(packed_hdr *h, uint8_t tag, uint32_t v) {
    return h->tag == tag && h->value == v;
}
int sizeof_packed_hdr(void) { return (int)sizeof(packed_hdr); }
int sizeof_packed_msg(void) { return (int)sizeof(packed_msg); }
