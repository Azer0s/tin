package codegen

// ctfe_dispatch.go - libffi-backed dispatch from the CTFE evaluator into a
// pre-compiled per-fn .so. Uses the `__tin_pure_shim_<name>` symbol that
// emitPureFnCtfeShims attached to every wrappable #pure function, so the
// signature exposed to libffi is the same C ABI emitInteropWrapperFor
// produces for any user-tagged #interop function.
//
// libffi handles the calling convention for arbitrary arities and types
// at runtime; we describe the shim's signature with ffi_type pointers
// derived from fd.Params + fd.RetType, marshal each ctfeVal into a raw
// argument cell, and unmarshal the result. No more per-arity cgo stubs,
// no more bespoke i64 adapter - one path covers i64 / f64 / bool / string.

/*
#cgo linux LDFLAGS: -ldl -lffi
// macOS: libffi is keg-only Homebrew. Pick the prefix per-arch so the
// linker doesn't warn about non-existent -L paths (clang is silent on
// stray -I, but ld64 warns on stray -L). Users with a custom prefix
// (MacPorts, manual install) can override via CGO_CFLAGS / CGO_LDFLAGS.
#cgo darwin,arm64  CFLAGS:  -I/opt/homebrew/opt/libffi/include
#cgo darwin,arm64  LDFLAGS: -L/opt/homebrew/opt/libffi/lib -lffi
#cgo darwin,amd64  CFLAGS:  -I/usr/local/opt/libffi/include
#cgo darwin,amd64  LDFLAGS: -L/usr/local/opt/libffi/lib -lffi

#include <dlfcn.h>
#include <ffi.h>
#include <stdint.h>
#include <stdlib.h>

// tin_ffi_invoke prepares a CIF for the given signature and calls `fn`.
// `atypes` is an array of nargs ffi_type pointers; `avals` is the parallel
// array of pointers to argument storage (each entry points to memory the
// caller owns). `result_buf` must be large enough for rtype's value (the
// caller allocates it sized to the return shape).
//
// Returns 0 on success, non-zero on ffi_prep_cif failure (rare; usually
// indicates an inconsistent ffi_type list).
static int tin_ffi_invoke(
    void* fn, ffi_type* rtype, ffi_type** atypes,
    void** avals, unsigned nargs, void* result_buf
) {
    ffi_cif cif;
    ffi_status st = ffi_prep_cif(&cif, FFI_DEFAULT_ABI, nargs, rtype, atypes);
    if (st != FFI_OK) return (int)st;
    ffi_call(&cif, FFI_FN(fn), result_buf, avals);
    return 0;
}

// tin_dispatch_free releases p via free_fn if non-NULL, else libc free.
// We bounce through this single point so the dispatcher's string-return
// branch can hand back a tin_extern_alloc'd buffer to the runtime's
// configured deallocator without re-implementing the trampoline in cgo
// each time.
typedef void (*tin_extern_free_fn)(void *);
static void tin_dispatch_free(void *free_fn, void *p) {
    if (!p) return;
    if (free_fn) {
        ((tin_extern_free_fn)free_fn)(p);
        return;
    }
    free(p);
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/Azer0s/tin/ast"
)

// pureFnHandle holds the dlopen handle and the resolved shim symbol for a
// single cached .so. Both pointers stay valid for the life of the process;
// we never dlclose so callers can reuse the handle without synchronizing.
//
// freeFn is the .so's `tin_extern_free` if it exposes one (i.e. the .so
// embeds runtime/interop.c). Otherwise it's nil and the dispatcher falls
// back to libc free for string returns. We MUST NOT mix free implementations
// with tin_extern_alloc: a #pure shim that returns a string allocates via
// the runtime's configured tin_alloc_fn, which a non-malloc deployment can
// override - in that case calling libc free corrupts the heap.
type pureFnHandle struct {
	handle unsafe.Pointer // dlopen result
	sym    unsafe.Pointer // dlsym(__tin_pure_shim_<name>)
	freeFn unsafe.Pointer // dlsym(tin_extern_free), nil if not exported
}

var (
	pureFnHandlesMu sync.Mutex
	pureFnHandles   = map[string]*pureFnHandle{}
)

// LoadPureFn dlopens the cached .so for the given Merkle hash and resolves
// the named shim symbol within it. The .so must exist on disk (Phase C2
// cache); returns an error if missing or the symbol is absent. Successful
// loads are cached process-wide.
func LoadPureFn(hash, shimName string) (*pureFnHandle, error) {
	pureFnHandlesMu.Lock()
	defer pureFnHandlesMu.Unlock()

	if h, ok := pureFnHandles[hash]; ok {
		return h, nil
	}

	// Verify the (hash -> shim) binding the cache claims. An empty manifest
	// means a legacy/hand-crafted entry (e.g. test fixtures) - accept it
	// for backward compatibility. A non-empty mismatch means we'd be about
	// to dlopen a .so whose symbol pedigree disagrees with what the
	// caller expected; refuse instead of returning a wrong handle.
	if recorded := readCacheManifest(hash); recorded != "" && recorded != shimName {
		return nil, fmt.Errorf("pure-fn cache manifest mismatch for %s: cache holds %s, caller expected %s", hash, recorded, shimName)
	}

	soPath := ctfeCacheBinPath(hash)
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	// RTLD_LAZY: defer extern-symbol resolution until first call. The
	// emitInteropWrapperFor wrapper body references runtime helpers
	// (tin_runtime_init, tin_interop_str_in, ...) that the Tin COMPILER
	// binary doesn't link. With RTLD_NOW the entire dlopen would fail at
	// load time even for primitive #pure fns whose wrapper happens to
	// call tin_runtime_init in its preamble. With RTLD_LAZY the load
	// succeeds, and only ACTUAL invocation of an unresolved symbol traps
	// - which the libffi dispatcher would catch via ffi_call's stub if
	// it ever fired (it doesn't for i64/f64/bool round-trips).
	handle := C.dlopen(cPath, C.RTLD_LAZY|C.RTLD_LOCAL)
	if handle == nil {
		return nil, fmt.Errorf("dlopen %s: %s", soPath, C.GoString(C.dlerror()))
	}

	cSym := C.CString(shimName)
	defer C.free(unsafe.Pointer(cSym))
	sym := C.dlsym(handle, cSym)
	if sym == nil {
		// Close the handle we just opened so a missing symbol doesn't leak
		// the dlopen state. Read dlerror BEFORE dlclose: some libcs reset
		// the per-thread error string on dlclose.
		errMsg := C.GoString(C.dlerror())
		C.dlclose(handle)

		return nil, fmt.Errorf("dlsym %s: %s", shimName, errMsg)
	}

	// Best-effort lookup of tin_extern_free so the dispatcher can release
	// string-return buffers via the runtime's configured deallocator. May
	// be nil for .so's that don't embed runtime/interop.c - the dispatcher
	// falls back to libc free in that case (correct as long as the runtime
	// hasn't been pointed at a non-malloc allocator). Clear dlerror first
	// so a stale message from an unrelated lookup doesn't leak through if
	// this dlsym returns NULL legitimately.
	C.dlerror()
	cFreeName := C.CString("tin_extern_free")
	freeSym := C.dlsym(handle, cFreeName)
	C.free(unsafe.Pointer(cFreeName))

	h := &pureFnHandle{
		handle: handle,
		sym:    sym,
		freeFn: freeSym,
	}
	pureFnHandles[hash] = h

	return h, nil
}

// InvokePureShim calls the cached shim with the supplied ctfeVal args and
// returns the unmarshaled ctfeVal result. Signature information comes from
// fd (param + return Tin types); libffi handles the calling convention.
//
// Supported types:
//
//	i8 / i16 / i32 / i64 (signed and unsigned, including bool/byte/char)
//	f32 / f64
//	string  - shim parameter is `char*`; return is `char*` heap-allocated
//	          via tin_extern_alloc (we copy + free here)
//
// Returns ok=false silently for any signature element outside that subset
// - the caller falls back to AST evaluation.
func InvokePureShim(h *pureFnHandle, fd *ast.FuncDecl, args []ctfeVal) (ctfeVal, bool) {
	if fd == nil || fd.RetType == nil {
		return ctfeVal{}, false
	}

	// Build ffi_type list + argument storage. Each storage cell needs a
	// stable address through the call, so we keep the slices alive in the
	// caller frame and pin pointers via cgo.Handle / unsafe.Pointer below.
	atypes := make([]*C.ffi_type, len(args))

	// avals points to caller-owned storage holding each argument's value.
	avals := make([]unsafe.Pointer, len(args))

	// Backing storage for each argument, indexed by arg position. We keep a
	// per-kind slice so the Go GC can't move the bytes between marshal and
	// ffi_call. C strings created here are freed at the end of the call.
	var (
		i8Cells  = make([]int8, len(args))
		i16Cells = make([]int16, len(args))
		i32Cells = make([]int32, len(args))
		i64Cells = make([]int64, len(args))
		u8Cells  = make([]uint8, len(args))
		u16Cells = make([]uint16, len(args))
		u32Cells = make([]uint32, len(args))
		u64Cells = make([]uint64, len(args))
		f32Cells = make([]float32, len(args))
		f64Cells = make([]float64, len(args))
		ptrCells = make([]unsafe.Pointer, len(args))
		cStrings []*C.char
	)

	// All Go-memory pointers we hand to C (atypes/avals slice headers, plus
	// each per-arg cell address pointed to by avals[i]) must remain at
	// fixed addresses through ffi_call. Pin them all on a single Pinner so
	// the unpin happens together when InvokePureShim returns.
	var pinner runtime.Pinner

	defer pinner.Unpin()
	defer func() {
		for _, p := range cStrings {
			C.free(unsafe.Pointer(p))
		}
	}()

	for i, p := range fd.Params {
		val := args[i]

		t, kind := tinTypeToFfi(p.Type)
		if t == nil {
			return ctfeVal{}, false
		}

		atypes[i] = t

		switch kind {
		case "i8":
			i8Cells[i] = int8(val.i)
			pinner.Pin(&i8Cells[i])
			avals[i] = unsafe.Pointer(&i8Cells[i])
		case "i16":
			i16Cells[i] = int16(val.i)
			pinner.Pin(&i16Cells[i])
			avals[i] = unsafe.Pointer(&i16Cells[i])
		case "i32":
			i32Cells[i] = int32(val.i)
			pinner.Pin(&i32Cells[i])
			avals[i] = unsafe.Pointer(&i32Cells[i])
		case "i64":
			i64Cells[i] = val.i
			pinner.Pin(&i64Cells[i])
			avals[i] = unsafe.Pointer(&i64Cells[i])
		case "u8":
			u8Cells[i] = uint8(val.i)
			pinner.Pin(&u8Cells[i])
			avals[i] = unsafe.Pointer(&u8Cells[i])
		case "u16":
			u16Cells[i] = uint16(val.i)
			pinner.Pin(&u16Cells[i])
			avals[i] = unsafe.Pointer(&u16Cells[i])
		case "u32":
			u32Cells[i] = uint32(val.i)
			pinner.Pin(&u32Cells[i])
			avals[i] = unsafe.Pointer(&u32Cells[i])
		case "u64":
			u64Cells[i] = uint64(val.i)
			pinner.Pin(&u64Cells[i])
			avals[i] = unsafe.Pointer(&u64Cells[i])
		case "bool":
			if val.b {
				u8Cells[i] = 1
			}

			pinner.Pin(&u8Cells[i])
			avals[i] = unsafe.Pointer(&u8Cells[i])
		case "f32":
			f32Cells[i] = float32(val.f)
			pinner.Pin(&f32Cells[i])
			avals[i] = unsafe.Pointer(&f32Cells[i])
		case "f64":
			f64Cells[i] = val.f
			pinner.Pin(&f64Cells[i])
			avals[i] = unsafe.Pointer(&f64Cells[i])
		case "string":
			if val.kind != "string" {
				return ctfeVal{}, false
			}

			cStr := C.CString(val.s)
			cStrings = append(cStrings, cStr)
			ptrCells[i] = unsafe.Pointer(cStr)
			pinner.Pin(&ptrCells[i])
			avals[i] = unsafe.Pointer(&ptrCells[i])
		default:
			return ctfeVal{}, false
		}
	}

	rtype, retKind := tinTypeToFfi(fd.RetType)
	if rtype == nil {
		return ctfeVal{}, false
	}

	// Result buffer sized for the largest primitive we support; libffi
	// requires at least sizeof(ffi_arg) (== sizeof(long)) so 16 bytes is
	// always enough on the LP64 / LLP64 targets Tin supports (x86_64 and
	// arm64). The narrow-int return reads below assume LITTLE-ENDIAN: on a
	// big-endian host, libffi promotes a returned i32 into the high half
	// of the 64-bit ffi_arg slot, so reading byte 0 as int32 would yield
	// zero. Tin's supported targets are all little-endian; if a big-endian
	// port is added, switch the narrow-int reads below to read the full
	// ffi_arg cell and then truncate.
	var resultBuf [16]byte

	var atypesPtr **C.ffi_type
	if len(atypes) > 0 {
		pinner.Pin(&atypes[0])

		atypesPtr = (**C.ffi_type)(unsafe.Pointer(&atypes[0]))
	}

	var avalsPtr *unsafe.Pointer
	if len(avals) > 0 {
		pinner.Pin(&avals[0])

		avalsPtr = &avals[0]
	}

	pinner.Pin(&resultBuf[0])

	st := C.tin_ffi_invoke(
		h.sym,
		rtype, atypesPtr,
		(*unsafe.Pointer)(unsafe.Pointer(avalsPtr)),
		C.uint(len(args)),
		unsafe.Pointer(&resultBuf[0]),
	)
	if st != 0 {
		return ctfeVal{}, false
	}

	switch retKind {
	case "i8":
		return ctfeVal{kind: "i64", i: int64(*(*int8)(unsafe.Pointer(&resultBuf[0])))}, true
	case "i16":
		return ctfeVal{kind: "i64", i: int64(*(*int16)(unsafe.Pointer(&resultBuf[0])))}, true
	case "i32":
		return ctfeVal{kind: "i64", i: int64(*(*int32)(unsafe.Pointer(&resultBuf[0])))}, true
	case "i64":
		return ctfeVal{kind: "i64", i: *(*int64)(unsafe.Pointer(&resultBuf[0]))}, true
	case "u8":
		return ctfeVal{kind: "i64", i: int64(*(*uint8)(unsafe.Pointer(&resultBuf[0])))}, true
	case "u16":
		return ctfeVal{kind: "i64", i: int64(*(*uint16)(unsafe.Pointer(&resultBuf[0])))}, true
	case "u32":
		return ctfeVal{kind: "i64", i: int64(*(*uint32)(unsafe.Pointer(&resultBuf[0])))}, true
	case "u64":
		return ctfeVal{kind: "i64", i: int64(*(*uint64)(unsafe.Pointer(&resultBuf[0])))}, true
	case "bool":
		return ctfeVal{kind: "bool", b: *(*uint8)(unsafe.Pointer(&resultBuf[0])) != 0}, true
	case "f32":
		return ctfeVal{kind: "f64", f: float64(*(*float32)(unsafe.Pointer(&resultBuf[0])))}, true
	case "f64":
		return ctfeVal{kind: "f64", f: *(*float64)(unsafe.Pointer(&resultBuf[0]))}, true
	case "string":
		// The shim returned a heap-allocated char* via tin_extern_alloc.
		// We copy into a Go string and release the C buffer through the
		// runtime's matching deallocator (h.freeFn = tin_extern_free if
		// the .so exposes it, else libc free). MUST go through the
		// configured deallocator: a non-malloc allocator's metadata
		// would corrupt if we called libc free directly.
		cStr := *(**C.char)(unsafe.Pointer(&resultBuf[0]))
		if cStr == nil {
			return ctfeVal{kind: "string", s: ""}, true
		}

		s := C.GoString(cStr)
		C.tin_dispatch_free(h.freeFn, unsafe.Pointer(cStr))

		return ctfeVal{kind: "string", s: s}, true
	}

	return ctfeVal{}, false
}

// tinTypeToFfi maps a Tin TypeExpr to (libffi type pointer, marshal kind).
// Returns (nil, "") for types outside the supported subset.
func tinTypeToFfi(t ast.TypeExpr) (*C.ffi_type, string) {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return nil, ""
	}

	switch st.Name {
	case "i8", "byte", "char":
		return &C.ffi_type_sint8, "i8"
	case "i16":
		return &C.ffi_type_sint16, "i16"
	case "i32":
		return &C.ffi_type_sint32, "i32"
	case "i64":
		return &C.ffi_type_sint64, "i64"
	case "u8":
		return &C.ffi_type_uint8, "u8"
	case "u16":
		return &C.ffi_type_uint16, "u16"
	case "u32":
		return &C.ffi_type_uint32, "u32"
	case "u64":
		return &C.ffi_type_uint64, "u64"
	case "bool":
		return &C.ffi_type_uint8, "bool"
	case "f32":
		return &C.ffi_type_float, "f32"
	case "f64":
		return &C.ffi_type_double, "f64"
	case "string":
		return &C.ffi_type_pointer, "string"
	}

	return nil, ""
}
