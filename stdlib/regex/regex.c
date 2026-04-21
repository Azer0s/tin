// stdlib/regex/regex.c - thin PCRE2 shims for the Tin regex module.
//
// Wraps the PCRE2 8-bit API into a simple interface:
//   tin_pcre2_compile     - compile a pattern; error stored in TLS buffer
//   tin_pcre2_last_error  - retrieve last compile error string
//   tin_pcre2_free        - free a compiled pattern
//   tin_pcre2_capturecount - query number of capture groups
//   tin_pcre2_exec        - run a match; results written to TLS ovector
//   tin_pcre2_ovector     - pointer to TLS (start,end) int32 pair buffer

#define PCRE2_CODE_UNIT_WIDTH 8
#include <pcre2.h>
#include <stdint.h>

static __thread char    _errbuf[256];
static __thread int32_t _ovbuf[60];  // 30 (start,end) pairs

void *tin_pcre2_compile(const char *pattern) {
    int errcode = 0;
    PCRE2_SIZE erroffset = 0;
    pcre2_code *code = pcre2_compile(
        (PCRE2_SPTR)pattern, PCRE2_ZERO_TERMINATED, 0, &errcode, &erroffset, NULL);
    if (!code)
        pcre2_get_error_message(errcode, (PCRE2_UCHAR8 *)_errbuf, sizeof(_errbuf));
    else
        _errbuf[0] = '\0';
    return (void *)code;
}

const char *tin_pcre2_last_error(void) { return _errbuf; }

void tin_pcre2_free(void *code) { pcre2_code_free((pcre2_code *)code); }

int32_t tin_pcre2_capturecount(void *code) {
    uint32_t count = 0;
    pcre2_pattern_info((pcre2_code *)code, PCRE2_INFO_CAPTURECOUNT, &count);
    return (int32_t)count;
}

// Returns number of captures (>=1) or negative on no-match/error.
// Results are written to the TLS _ovbuf; call tin_pcre2_ovector() to read them.
int32_t tin_pcre2_exec(void *code, const char *subject, int32_t sublen, int32_t startoffset) {
    pcre2_match_data *md = pcre2_match_data_create_from_pattern((pcre2_code *)code, NULL);
    if (!md) return -1;
    int rc = pcre2_match((pcre2_code *)code, (PCRE2_SPTR)subject,
                         (PCRE2_SIZE)(uint32_t)sublen, (PCRE2_SIZE)(uint32_t)startoffset,
                         0, md, NULL);
    if (rc > 0) {
        PCRE2_SIZE *ov = pcre2_get_ovector_pointer(md);
        int pairs = rc < 30 ? rc : 30;
        for (int i = 0; i < pairs; i++) {
            _ovbuf[i * 2]     = (int32_t)ov[i * 2];
            _ovbuf[i * 2 + 1] = (int32_t)ov[i * 2 + 1];
        }
    }
    pcre2_match_data_free(md);
    return (int32_t)rc;
}

int32_t *tin_pcre2_ovector(void) { return _ovbuf; }
