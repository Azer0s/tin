package codegen

// ctfe_dispatch.go - cgo bridge that dlopens a per-fn .so produced by
// PureFnsForCache and invokes the cached function directly.
//
// Phase C3 of the CTFE pipeline: gives the AST evaluator a path that
// exercises the actual compiled function (including stdlib calls and
// types the interpreter cannot model) at near-native speed.
//
// Each per-fn .so exports the function under its bare Tin name with the
// same C-ABI shape `#interop` would produce — for primitive params and
// returns this is just `T (*)(T0, T1, ...)`. The dispatcher picks the
// right shape-specific cgo entry based on arity (richer types will route
// through the full #interop wrapper marshalling once we wire it).

/*
#cgo linux  LDFLAGS: -ldl
#cgo darwin LDFLAGS:

#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

// Shape-specific call entries. cgo signatures are static, so we declare
// one per arity (0..6 i64 args -> i64 result, the common practical bound).
// Each entry casts the dlsym result to the matching function-pointer
// signature and invokes it.
typedef int64_t (*tin_fn0)(void);
typedef int64_t (*tin_fn1)(int64_t);
typedef int64_t (*tin_fn2)(int64_t,int64_t);
typedef int64_t (*tin_fn3)(int64_t,int64_t,int64_t);
typedef int64_t (*tin_fn4)(int64_t,int64_t,int64_t,int64_t);
typedef int64_t (*tin_fn5)(int64_t,int64_t,int64_t,int64_t,int64_t);
typedef int64_t (*tin_fn6)(int64_t,int64_t,int64_t,int64_t,int64_t,int64_t);

static int64_t tin_call0(void* fn) { return ((tin_fn0)fn)(); }
static int64_t tin_call1(void* fn, int64_t a0) { return ((tin_fn1)fn)(a0); }
static int64_t tin_call2(void* fn, int64_t a0, int64_t a1) { return ((tin_fn2)fn)(a0, a1); }
static int64_t tin_call3(void* fn, int64_t a0, int64_t a1, int64_t a2) {
    return ((tin_fn3)fn)(a0, a1, a2);
}
static int64_t tin_call4(void* fn, int64_t a0, int64_t a1, int64_t a2, int64_t a3) {
    return ((tin_fn4)fn)(a0, a1, a2, a3);
}
static int64_t tin_call5(void* fn, int64_t a0, int64_t a1, int64_t a2, int64_t a3, int64_t a4) {
    return ((tin_fn5)fn)(a0, a1, a2, a3, a4);
}
static int64_t tin_call6(void* fn, int64_t a0, int64_t a1, int64_t a2, int64_t a3, int64_t a4, int64_t a5) {
    return ((tin_fn6)fn)(a0, a1, a2, a3, a4, a5);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// pureFnHandle holds the dlopen handle and the resolved function symbol for
// a single cached .so. Both pointers are valid for the lifetime of the
// process; we never dlclose so callers can reuse the handle without
// synchronizing.
type pureFnHandle struct {
	handle unsafe.Pointer // dlopen result
	sym    unsafe.Pointer // dlsym(<fn_name>) — the function itself
}

var (
	pureFnHandlesMu sync.Mutex
	pureFnHandles   = map[string]*pureFnHandle{}
)

// LoadPureFn dlopens the cached .so for the given Merkle hash and resolves
// the named function symbol within it. The .so must exist on disk
// (Phase C2 cache); returns an error if missing or the symbol is absent.
// Successful loads are cached process-wide; subsequent calls with the same
// hash are O(1).
func LoadPureFn(hash, fnName string) (*pureFnHandle, error) {
	pureFnHandlesMu.Lock()
	defer pureFnHandlesMu.Unlock()

	if h, ok := pureFnHandles[hash]; ok {
		return h, nil
	}

	soPath := ctfeCacheBinPath(hash)
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.dlopen(cPath, C.RTLD_NOW|C.RTLD_LOCAL)
	if handle == nil {
		return nil, fmt.Errorf("dlopen %s: %s", soPath, C.GoString(C.dlerror()))
	}

	cSym := C.CString(fnName)
	defer C.free(unsafe.Pointer(cSym))

	sym := C.dlsym(handle, cSym)
	if sym == nil {
		return nil, fmt.Errorf("dlsym %s: %s", fnName, C.GoString(C.dlerror()))
	}

	h := &pureFnHandle{handle: unsafe.Pointer(handle), sym: unsafe.Pointer(sym)}
	pureFnHandles[hash] = h

	return h, nil
}

// InvokePureFn calls the cached function with i64 args (after caller-side
// truncation/widening for narrower primitives), returning the i64 result.
// Picks the cgo entry that matches the argument arity. Returns an error
// when arity exceeds the cgo-declared maximum (currently 6); callers should
// bail out and fall back to AST evaluation.
//
// If the underlying function panics, traps, or executes UB, the entire
// compiler process dies — Phase C5 will add subprocess isolation. For now,
// trust the .so we just compiled out of our own IR.
func InvokePureFn(h *pureFnHandle, args []int64) (int64, error) {
	switch len(args) {
	case 0:
		return int64(C.tin_call0(h.sym)), nil
	case 1:
		return int64(C.tin_call1(h.sym, C.int64_t(args[0]))), nil
	case 2:
		return int64(C.tin_call2(h.sym, C.int64_t(args[0]), C.int64_t(args[1]))), nil
	case 3:
		return int64(C.tin_call3(h.sym,
			C.int64_t(args[0]), C.int64_t(args[1]), C.int64_t(args[2]))), nil
	case 4:
		return int64(C.tin_call4(h.sym,
			C.int64_t(args[0]), C.int64_t(args[1]), C.int64_t(args[2]),
			C.int64_t(args[3]))), nil
	case 5:
		return int64(C.tin_call5(h.sym,
			C.int64_t(args[0]), C.int64_t(args[1]), C.int64_t(args[2]),
			C.int64_t(args[3]), C.int64_t(args[4]))), nil
	case 6:
		return int64(C.tin_call6(h.sym,
			C.int64_t(args[0]), C.int64_t(args[1]), C.int64_t(args[2]),
			C.int64_t(args[3]), C.int64_t(args[4]), C.int64_t(args[5]))), nil
	}

	return 0, fmt.Errorf("ctfe dispatch: arity %d exceeds cgo entries (max 6)", len(args))
}
