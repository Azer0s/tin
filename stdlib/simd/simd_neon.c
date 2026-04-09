// stdlib/simd/simd_neon.c - ARM NEON SIMD intrinsics for aarch64
//
// NEON is always available on AArch64 (both Linux ARM and Apple Silicon).
// No alignment requirements for NEON loads/stores.

#include <arm_neon.h>
#include <stdint.h>

typedef int8_t   i8x16  __attribute__((vector_size(16)));
typedef uint8_t  u8x16  __attribute__((vector_size(16)));
typedef int16_t  i16x8  __attribute__((vector_size(16)));
typedef uint16_t u16x8  __attribute__((vector_size(16)));
typedef int32_t  i32x4  __attribute__((vector_size(16)));
typedef uint32_t u32x4  __attribute__((vector_size(16)));
typedef int64_t  i64x2  __attribute__((vector_size(16)));
typedef uint64_t u64x2  __attribute__((vector_size(16)));
typedef float    f32x4  __attribute__((vector_size(16)));
typedef double   f64x2  __attribute__((vector_size(16)));

// -- splat --

u8x16  _tin_simd_splat_u8x16(uint8_t v)  { return (u8x16)vdupq_n_u8(v); }
i8x16  _tin_simd_splat_i8x16(int8_t v)   { return (i8x16)vdupq_n_s8(v); }
u32x4  _tin_simd_splat_u32x4(uint32_t v) { return (u32x4)vdupq_n_u32(v); }
i32x4  _tin_simd_splat_i32x4(int32_t v)  { return (i32x4)vdupq_n_s32(v); }
u64x2  _tin_simd_splat_u64x2(uint64_t v) { return (u64x2)vdupq_n_u64(v); }
f32x4  _tin_simd_splat_f32x4(float v)    { return (f32x4)vdupq_n_f32(v); }
f64x2  _tin_simd_splat_f64x2(double v)   { return (f64x2)vdupq_n_f64(v); }

// -- loadu --

u8x16  _tin_simd_loadu_u8x16(const uint8_t *ptr)  { return (u8x16)vld1q_u8(ptr); }
u32x4  _tin_simd_loadu_u32x4(const uint32_t *ptr) { return (u32x4)vld1q_u32(ptr); }
f32x4  _tin_simd_loadu_f32x4(const float *ptr)    { return (f32x4)vld1q_f32(ptr); }

// -- storeu --

void _tin_simd_storeu_u8x16(uint8_t *ptr, u8x16 v)  { vst1q_u8(ptr, (uint8x16_t)v); }
void _tin_simd_storeu_u32x4(uint32_t *ptr, u32x4 v) { vst1q_u32(ptr, (uint32x4_t)v); }
void _tin_simd_storeu_f32x4(float *ptr, f32x4 v)    { vst1q_f32(ptr, (float32x4_t)v); }

// -- cmpeq --

u8x16  _tin_simd_cmpeq_u8x16(u8x16 a,  u8x16 b)  { return (u8x16)vceqq_u8((uint8x16_t)a, (uint8x16_t)b); }
u32x4  _tin_simd_cmpeq_u32x4(u32x4 a,  u32x4 b)  { return (u32x4)vceqq_u32((uint32x4_t)a, (uint32x4_t)b); }
f32x4  _tin_simd_cmpeq_f32x4(f32x4 a,  f32x4 b)  { return (f32x4)vceqq_f32((float32x4_t)a, (float32x4_t)b); }

// -- movemask_u8: SSE2-style movemask emulation using NEON --
// Extract the MSB of each byte, pack into a 16-bit integer.
// Strategy: isolate each MSB with a fixed vshrq_n_u8(v, 7) -> 0 or 1 per byte.
// Then multiply by position weights {1,2,4,8,16,32,64,128} to place each bit
// at its correct position.  Three rounds of vpadd_u8 accumulate into lane 0
// (low byte = bits 0-7) and lane 1 (high byte = bits 8-15).

uint32_t _tin_simd_movemask_u8x16(u8x16 a) {
    static const uint8_t weight_data[8] = { 1, 2, 4, 8, 16, 32, 64, 128 };
    uint8x16_t msbs    = vshrq_n_u8((uint8x16_t)a, 7);
    uint8x8_t  weights = vld1_u8(weight_data);
    uint8x8_t  lo      = vmul_u8(vget_low_u8(msbs), weights);
    uint8x8_t  hi      = vmul_u8(vget_high_u8(msbs), weights);
    uint8x8_t  paired  = vpadd_u8(lo, hi);
    paired = vpadd_u8(paired, paired);
    paired = vpadd_u8(paired, paired);
    return (uint32_t)vget_lane_u16(vreinterpret_u16_u8(paired), 0);
}

// -- hadd --

uint32_t _tin_simd_hadd_u32x4(u32x4 a) {
    uint32x4_t v = (uint32x4_t)a;
    uint32x2_t sum = vadd_u32(vget_low_u32(v), vget_high_u32(v));
    return vget_lane_u32(vpadd_u32(sum, sum), 0);
}

float _tin_simd_hadd_f32x4(f32x4 a) {
    float32x4_t v = (float32x4_t)a;
    float32x2_t sum = vadd_f32(vget_low_f32(v), vget_high_f32(v));
    return vget_lane_f32(vpadd_f32(sum, sum), 0);
}

// -- dot --

float _tin_simd_dot_f32x4(f32x4 a, f32x4 b) {
    float32x4_t prod = vmulq_f32((float32x4_t)a, (float32x4_t)b);
    float32x2_t sum = vadd_f32(vget_low_f32(prod), vget_high_f32(prod));
    return vget_lane_f32(vpadd_f32(sum, sum), 0);
}

// -- rotate_left --
// vshlq_u32 accepts a signed integer vector: positive = left shift, negative = right shift.

u32x4 _tin_simd_rotl_u32x4(u32x4 a, int32_t n) {
    n &= 31;
    if (n == 0) return a;
    uint32x4_t v = (uint32x4_t)a;
    int32x4_t  ln = vdupq_n_s32(n);
    int32x4_t  rn = vdupq_n_s32(-(32 - n));
    return (u32x4)vorrq_u32(vshlq_u32(v, ln), vshlq_u32(v, rn));
}
