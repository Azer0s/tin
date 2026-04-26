package repl

// #cgo LDFLAGS: -ldl
// #include <dlfcn.h>
// #include <stdint.h>
// #include <stdlib.h>
//
// // Runtime function pointers resolved once from libtin_runtime.so.
// static int64_t (*_repl_fiber_spawn_joinable)(void *) = NULL;
// static void    (*_repl_fiber_sync_await)(int64_t)    = NULL;
// static void    (*_repl_fiber_init)(void)             = NULL;
// static void    (*_repl_fiber_run)(void)              = NULL;
// static void    (*_repl_io_init)(void)                = NULL;
//
// void repl_setup_runtime(void *handle) {
//     _repl_fiber_init           = dlsym(handle, "_tin_fiber_init");
//     _repl_fiber_run            = dlsym(handle, "_tin_fiber_run");
//     _repl_fiber_spawn_joinable = dlsym(handle, "_tin_fiber_spawn_joinable");
//     _repl_fiber_sync_await     = dlsym(handle, "_tin_fiber_sync_await");
//     _repl_io_init              = dlsym(handle, "_tin_io_init");
// }
//
// void repl_fiber_init(void) {
//     if (_repl_fiber_init)  _repl_fiber_init();
//     if (_repl_io_init)     _repl_io_init();
// }
//
// void repl_fiber_run(void) {
//     if (_repl_fiber_run) _repl_fiber_run();
// }
//
// // Spawn the cell's $coro coroutine as a fiber and sync-await it.
// // coro_fn must be the cell's _repl_cell_N$coro function (void args, i8* return).
// void repl_run_cell(void *coro_fn) {
//     if (!coro_fn || !_repl_fiber_spawn_joinable || !_repl_fiber_sync_await) return;
//     typedef void *(*cell_coro_t)(void);
//     void *hdl = ((cell_coro_t)coro_fn)();
//     int64_t pid = _repl_fiber_spawn_joinable(hdl);
//     _repl_fiber_sync_await(pid);
// }
//
// void *repl_dlopen(const char *path) {
//     return dlopen(path, RTLD_NOW | RTLD_GLOBAL);
// }
//
// void *repl_dlopen_local(const char *path) {
//     return dlopen(path, RTLD_NOW);
// }
//
// void *repl_dlsym(void *handle, const char *name) {
//     return dlsym(handle, name);
// }
//
// const char *repl_dlerror(void) {
//     return dlerror();
// }
import "C"

import (
	"fmt"
	"unsafe"
)

// lib is a loaded shared library handle.
type lib struct {
	handle unsafe.Pointer
	path   string
}

func openLib(path string, global bool) (*lib, error) {
	cPath := C.CString(path)

	defer C.free(unsafe.Pointer(cPath))

	var handle unsafe.Pointer
	if global {
		handle = C.repl_dlopen(cPath)
	} else {
		handle = C.repl_dlopen_local(cPath)
	}

	if handle == nil {
		msg := C.GoString(C.repl_dlerror())

		return nil, fmt.Errorf("dlopen %s: %s", path, msg)
	}

	return &lib{handle: handle, path: path}, nil
}

func (l *lib) lookup(name string) (unsafe.Pointer, error) {
	cName := C.CString(name)

	defer C.free(unsafe.Pointer(cName))

	sym := C.repl_dlsym(l.handle, cName)
	if sym == nil {
		return nil, fmt.Errorf("%s: symbol %q not found", l.path, name)
	}

	return sym, nil
}

// setupRuntime resolves runtime function pointers from the loaded runtime lib.
func setupRuntime(rtLib *lib) {
	C.repl_setup_runtime(rtLib.handle)
}

// fiberInit starts the fiber scheduler and I/O thread.
func fiberInit() { C.repl_fiber_init() }

// fiberRun drains remaining fibers (called at REPL exit).
func fiberRun() { C.repl_fiber_run() }

// runCell spawns the cell's $coro as a joinable fiber and blocks until it
// completes. Background fibers spawned by the cell continue running after this
// returns (the scheduler workers keep running between cells).
func runCell(coroFn unsafe.Pointer) { C.repl_run_cell(coroFn) }
