#define _GNU_SOURCE
#include <unistd.h>
#include <stdint.h>
#include <time.h>
#include <stdio.h>
#include <string.h>
#include "runtime.h"

void sleep_ms(long long ms) { usleep((unsigned int)(ms * 1000)); }

int64_t _tin_now_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

int64_t _tin_now_us(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000 + ts.tv_nsec / 1000;
}

// _tin_now_ns returns monotonic nanoseconds.
int64_t _tin_now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

// _tin_now_ns_real returns wall-clock (REALTIME) nanoseconds since Unix epoch.
int64_t _tin_now_ns_real(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

// _tin_instant_rfc3339 formats ns (Unix epoch nanoseconds) as RFC3339 into buf.
// buf must be at least 32 bytes. Returns the number of characters
// actually written (clamped to 31 so the caller's buf[0..n] slice can
// never read past the buffer for years > 9999).
int _tin_instant_rfc3339(int64_t ns, char *buf) {
    time_t sec = (time_t)(ns / 1000000000LL);
    long long frac = ns % 1000000000LL;
    if (frac < 0) { sec--; frac += 1000000000LL; }
    struct tm t;
    gmtime_r(&sec, &t);
    int n = snprintf(buf, 32, "%04d-%02d-%02dT%02d:%02d:%02d.%09lldZ",
                     t.tm_year + 1900, t.tm_mon + 1, t.tm_mday,
                     t.tm_hour, t.tm_min, t.tm_sec, frac);
    // snprintf returns the would-have-been length on truncation; cap
    // to the actual buffer payload (31 bytes plus NUL).
    if (n < 0) return 0;
    if (n > 31) n = 31;
    return n;
}

// _tin_from_rfc3339 parses an RFC3339 string into nanoseconds since Unix epoch.
// Supports: YYYY-MM-DDTHH:MM:SS[.nnnnnnnnn][Z|+HH:MM|-HH:MM]
// Returns 0 on success, -1 on parse error. Writes result to *ns_out.
int _tin_from_rfc3339(const char *s, int64_t *ns_out) {
    if (!s || !ns_out) return -1;
    int year = 0, month = 0, day = 0, hour = 0, min = 0, sec = 0;
    if (sscanf(s, "%d-%d-%dT%d:%d:%d", &year, &month, &day, &hour, &min, &sec) != 6)
        return -1;
    if (year < 1970 || month < 1 || month > 12 || day < 1 || day > 31) return -1;
    // Per-month day check: timegm() will silently normalise an
    // out-of-range day (Feb 30 -> Mar 2 etc.).  Reject up front so
    // callers see a parse error instead of a quietly-shifted date.
    static const int days_per_month[] = {31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31};
    int dim = days_per_month[month - 1];
    int is_leap = ((year % 4 == 0 && year % 100 != 0) || year % 400 == 0);
    if (month == 2 && is_leap) dim = 29;
    if (day > dim) return -1;
    if (hour > 23 || min > 59 || sec > 60) return -1; // sec=60 covers leap second

    long long frac_ns = 0;
    const char *p = s + 19;  // after YYYY-MM-DDTHH:MM:SS
    if (*p == '.') {
        p++;
        long long mult = 100000000LL;
        int digits = 0;
        while (*p >= '0' && *p <= '9' && digits < 9) {
            frac_ns += (*p - '0') * mult;
            mult /= 10;
            digits++;
            p++;
        }
        while (*p >= '0' && *p <= '9') p++;  // skip extra digits
    }
    int tz_sign = 1, tz_hour = 0, tz_min = 0;
    if (*p == 'Z' || *p == 'z') {
        /* UTC */
    } else if (*p == '+' || *p == '-') {
        tz_sign = (*p == '+') ? 1 : -1;
        p++;
        int tzh = 0, tzm = 0;
        if (sscanf(p, "%d:%d", &tzh, &tzm) != 2) return -1;
        // RFC 3339 sec 5.6 caps offsets at +/-23:59.  sscanf would
        // otherwise accept "+99:99" and shift the epoch by an
        // arbitrary amount, masking malformed input.
        if (tzh < 0 || tzh > 23 || tzm < 0 || tzm > 59) return -1;
        tz_hour = tzh;
        tz_min  = tzm;
    }
    struct tm t;
    memset(&t, 0, sizeof(t));
    t.tm_year = year - 1900;
    t.tm_mon  = month - 1;
    t.tm_mday = day;
    t.tm_hour = hour;
    t.tm_min  = min;
    t.tm_sec  = sec;
    time_t epoch = timegm(&t);
    if (epoch == (time_t)-1) return -1;
    epoch -= tz_sign * (tz_hour * 3600 + tz_min * 60);
    // Final ns multiply: timegm produces a time_t in the i64 range,
    // but multiplying by 1e9 must stay representable.  Reject inputs
    // whose nanosecond value would overflow rather than silently
    // wrapping the result.
    int64_t epoch_i64 = (int64_t)epoch;
    if (epoch_i64 > 9223372036LL || epoch_i64 < -9223372036LL) return -1;
    *ns_out = epoch_i64 * 1000000000LL + frac_ns;
    return 0;
}
