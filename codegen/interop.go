package codegen

// `#interop` control tag - validation pass.
//
// `fn{#interop} name(...)` requests a C-callable wrapper alongside the
// Tin-internal entry point. v1 only validates that the function is in
// a shape we will actually be able to wrap; codegen for the wrapper
// itself comes in a later phase.
//
// Phase A (declaration-level):
//   - Cannot also be `#async` (an async fn is a coroutine; C cannot drive one)
//   - Return type must not contain `Future[T]` (no way for C to await)
//   - No parameter type may contain `any` (no stable C representation)
//   - Cannot be generic (no concrete name for the wrapper symbol)
//   - Cannot be a struct method (v1: top-level functions only)
//   - Cannot be `extern` (already C, has its own symbol)
//   - Cannot be named `main` (would clobber the binary's entry point)
//   - Two `#interop` functions sharing a name are rejected here rather
//     than letting the linker speak.
//
// Phase B (type whitelist):
//   - Each parameter must be a primitive, pointer, `string`, or fat
//     array `[T]`.
//   - Return type must be a primitive, pointer, `string`, fat array,
//     or `void`.
//   - Anything else (struct, trait object, ADT, union, fn, tuple)
//     rejected with a per-position diagnostic.

// reservedInteropNames are symbols whose external collision would
// either break linking or, worse, silently shadow a runtime helper.
// `main` is the obvious one; the `tin_*` set covers the public C
// boundary helpers shipped in runtime/interop.c. Note: any name with
// the `__tin_interop_` prefix would also collide with a wrapper's
// hidden internal symbol, but matching by prefix lives in
// validateInteropFunc since it is structural rather than a fixed
// list.
var reservedInteropNames = map[string]bool{
	"main":                  true,
	"tin_runtime_init":      true,
	"tin_release":           true,
	"tin_set_extern_alloc":  true,
	"tin_extern_alloc":      true,
	"tin_interop_str_in":    true,
	"tin_interop_str_out":   true,
	"tin_interop_slice_in":  true,
	"tin_interop_slice_out": true,
}

// checkAllInteropFuncs walks the program AST and validates every
// `#interop`-tagged function. Methods on structs are walked separately
// from top-level functions so the diagnostic can say "method" rather
// than "function" for a method-level violation.
