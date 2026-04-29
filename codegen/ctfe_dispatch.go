package codegen

// ctfe_dispatch.go - cgo bridge that dlopens a per-fn .so produced by
// PureFnsForCache and invokes its tin_ctfe_<hash> adapter directly.
// Phase C3 of the CTFE pipeline: gives the AST evaluator a path that
// exercises the actual compiled function (including stdlib calls and
// types the interpreter cannot model) at near-native speed.

/*
#cgo linux  LDFLAGS: -ldl
#cgo darwin LDFLAGS:

#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

// Shape every per-fn .so exports under the tin_ctfe_<hash> symbol. See
// codegen/ctfe_emit.go::buildI64Adapter for the IR that produces it.
typedef int64_t (*tin_ctfe_fn)(int64_t* args, int64_t nargs);

static int64_t tin_ctfe_invoke(void* sym, int64_t* args, int64_t nargs) {
    return ((tin_ctfe_fn)sym)(args, nargs);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// pureFnHandle holds the dlopen handle and the resolved adapter symbol for a
// single cached .so. Both pointers are valid for the lifetime of the
// process; we never dlclose so callers can reuse the handle without
// synchronizing.
type pureFnHandle struct {
	handle unsafe.Pointer // dlopen result
	sym    unsafe.Pointer // dlsym(tin_ctfe_<hash>)
}

var (
	pureFnHandlesMu sync.Mutex
	pureFnHandles   = map[string]*pureFnHandle{}
)

// LoadPureFn dlopens the cached .so for the given Merkle hash and resolves
// its tin_ctfe_<hash> adapter. The .so must exist on disk (Phase C2 cache);
// returns an error if missing or the symbol is absent. Successful loads are
// cached process-wide; subsequent calls with the same hash are O(1).
func LoadPureFn(hash, adapterSym string) (*pureFnHandle, error) {
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

	cSym := C.CString(adapterSym)
	defer C.free(unsafe.Pointer(cSym))

	sym := C.dlsym(handle, cSym)
	if sym == nil {
		return nil, fmt.Errorf("dlsym %s: %s", adapterSym, C.GoString(C.dlerror()))
	}

	h := &pureFnHandle{handle: unsafe.Pointer(handle), sym: unsafe.Pointer(sym)}
	pureFnHandles[hash] = h

	return h, nil
}

// InvokePureFn calls the cached adapter with i64 args, returning the i64
// result. The adapter is responsible for truncating/extending each arg to
// its native parameter width and zero-extending the return.
//
// If the underlying function panics, traps, or executes UB, the entire
// compiler process dies — Phase C5 will add subprocess isolation. For now,
// trust the .so we just compiled out of our own IR.
func InvokePureFn(h *pureFnHandle, args []int64) int64 {
	var argsPtr *C.int64_t
	if len(args) > 0 {
		argsPtr = (*C.int64_t)(unsafe.Pointer(&args[0]))
	}

	return int64(C.tin_ctfe_invoke(h.sym, argsPtr, C.int64_t(len(args))))
}
