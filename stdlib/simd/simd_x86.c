// stdlib/simd/simd_x86.c - SSE/SSE4.2 SIMD intrinsics for x86_64
//
// Compiled with -msse4.2. LLVM passes <16 x i8> and <4 x float> in XMM
// registers with the same ABI as __m128i / __m128.

#include <immintrin.h>
#include <stdint.h>

// -- splat: broadcast scalar to all lanes --

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

u8x16  _tin_simd_splat_u8x16(uint8_t v)  { return (u8x16)_mm_set1_epi8((int8_t)v); }
i8x16  _tin_simd_splat_i8x16(int8_t v)   { return (i8x16)_mm_set1_epi8(v); }
u32x4  _tin_simd_splat_u32x4(uint32_t v) { return (u32x4)_mm_set1_epi32((int32_t)v); }
i32x4  _tin_simd_splat_i32x4(int32_t v)  { return (i32x4)_mm_set1_epi32(v); }
u64x2  _tin_simd_splat_u64x2(uint64_t v) { return (u64x2)_mm_set1_epi64x((int64_t)v); }
f32x4  _tin_simd_splat_f32x4(float v)    { return (f32x4)_mm_set1_ps(v); }
f64x2  _tin_simd_splat_f64x2(double v)   { return (f64x2)_mm_set1_pd(v); }

// -- loadu: unaligned load --

u8x16  _tin_simd_loadu_u8x16(const uint8_t *ptr)  { return (u8x16)_mm_loadu_si128((const __m128i *)ptr); }
u32x4  _tin_simd_loadu_u32x4(const uint32_t *ptr) { return (u32x4)_mm_loadu_si128((const __m128i *)ptr); }
f32x4  _tin_simd_loadu_f32x4(const float *ptr)    { return (f32x4)_mm_loadu_ps(ptr); }

// -- storeu: unaligned store --

void _tin_simd_storeu_u8x16(uint8_t *ptr, u8x16 v)  { _mm_storeu_si128((__m128i *)ptr, (__m128i)v); }
void _tin_simd_storeu_u32x4(uint32_t *ptr, u32x4 v) { _mm_storeu_si128((__m128i *)ptr, (__m128i)v); }
void _tin_simd_storeu_f32x4(float *ptr, f32x4 v)    { _mm_storeu_ps(ptr, (__m128)v); }

// -- cmpeq: element-wise equality (returns all-1s mask for matching lanes) --

u8x16  _tin_simd_cmpeq_u8x16(u8x16 a,  u8x16 b)  { return (u8x16)_mm_cmpeq_epi8((__m128i)a, (__m128i)b); }
u32x4  _tin_simd_cmpeq_u32x4(u32x4 a,  u32x4 b)  { return (u32x4)_mm_cmpeq_epi32((__m128i)a, (__m128i)b); }
f32x4  _tin_simd_cmpeq_f32x4(f32x4 a,  f32x4 b)  { return (f32x4)_mm_cmpeq_ps((__m128)a, (__m128)b); }

// -- movemask: collapse MSBs to an integer bitmask --

uint32_t _tin_simd_movemask_u8x16(u8x16 a) { return (uint32_t)_mm_movemask_epi8((__m128i)a); }

// -- hadd: horizontal add all lanes --

uint32_t _tin_simd_hadd_u32x4(u32x4 a) {
    __m128i v = (__m128i)a;
    __m128i h = _mm_hadd_epi32(v, v);
    h = _mm_hadd_epi32(h, h);
    return (uint32_t)_mm_cvtsi128_si32(h);
}

float _tin_simd_hadd_f32x4(f32x4 a) {
    __m128 v = (__m128)a;
    __m128 h = _mm_hadd_ps(v, v);
    h = _mm_hadd_ps(h, h);
    return _mm_cvtss_f32(h);
}

// -- dot: dot product of two f32x4 vectors --

float _tin_simd_dot_f32x4(f32x4 a, f32x4 b) {
    // _mm_dp_ps: dot product with all lanes contributing, result in all lanes
    return _mm_cvtss_f32(_mm_dp_ps((__m128)a, (__m128)b, 0xF1));
}

// -- rotate_left: rotate 32-bit lanes left by n bits --

u32x4 _tin_simd_rotl_u32x4(u32x4 a, int32_t n) {
    n &= 31;
    if (n == 0) return a;
    __m128i v = (__m128i)a;
    return (u32x4)_mm_or_si128(_mm_slli_epi32(v, n), _mm_srli_epi32(v, 32 - n));
}
