# Internals: Fiber Panic Propagation

## Overview

When a fiber panics, the panic is **not immediately fatal**. The runtime
captures the message, marks the fiber as done, and stores the message on the
`TinFiber` struct. The panic is re-raised lazily - either when the fiber is
awaited, or at process shutdown if nobody ever awaited it.

This gives callers a chance to catch panics from spawned fibers using the
same `defer + recover()` mechanism they use for panics in normal functions.

---

## `panic_msg` on `TinFiber` (ARC-tracked)

```c
typedef struct {
    int64_t      pid;
    void        *hdl;          // LLVM coroutine handle
    FiberStatus  status;
    void        *result;       // heap-allocated result (set on FIBER_DONE)
    ...
    // If the fiber panicked and the panic was caught by the worker loop,
    // this field holds the message as an ARC-managed buffer (via _tin_rc_alloc)
    // so it can be safely wrapped in a TinString by the awaiting fiber.
    // NULL means the fiber completed normally.
    // Freed via _tin_release() in fiber cleanup.
    char    *panic_msg;
    // Set to 1 when _tin_fiber_get_panic_msg reads a non-NULL panic_msg.
    // Used at shutdown to detect fire-and-forget panics nobody awaited.
    int      panic_checked;
} TinFiber;
```

`panic_msg` is allocated with `_tin_rc_alloc` (rc = 1) so it participates in
ARC like any other managed value. The reference is owned by the `TinFiber`.
`_tin_fiber_get_panic_msg` retains it (rc -> 2) for the caller; that caller
passes the pointer to `_tin_panic`, which releases it when its defer chain
finishes running.

---

## Worker Loop - Catching Fiber Panics

Each worker thread runs fibers through `_tin_panic_catch_begin` /
`_tin_panic_catch_end` so that a fiber panic does not kill the whole process:

```c
_tin_panic_catch_begin();
_coro_resume(r.hdl);
const char *panicked = _tin_panic_catch_end();

if (panicked) {
    // Fiber panicked - destroy the coro frame and mark FIBER_DONE.
    _coro_destroy(r.hdl);
    free(r.hdl);

    size_t plen = strlen(panicked);
    char *pmsg_buf = (char *)_tin_rc_alloc((int64_t)(plen + 1));
    memcpy(pmsg_buf, panicked, plen + 1);

    f->panic_msg = pmsg_buf;   // ARC-managed, rc = 1
    f->status    = FIBER_DONE;
    _fire_done_waiters(f);     // wake any fiber waiting on this one
    ...
    continue;                  // do NOT re-enqueue; fiber is done
}
```

The fiber's coro frame is destroyed immediately because it cannot be resumed
after a panic. The fiber record (`TinFiber`) remains alive (rc-tracked by the
`Future[T]` value held by the spawning fiber) so the panic message can be
retrieved later.

---

## `_tin_fiber_get_panic_msg`

```c
const char *_tin_fiber_get_panic_msg(int64_t pid) {
    pthread_mutex_lock(&_table_mu);
    TinFiber *f = (pid > 0 && pid < _fiber_cnt) ? _fibers[pid] : NULL;
    const char *msg = f ? f->panic_msg : NULL;
    if (msg && f) {
        f->panic_checked = 1;
        _tin_retain((void *)msg);   // caller now holds a reference
    }
    pthread_mutex_unlock(&_table_mu);
    return msg;   // NULL if no panic
}
```

Setting `panic_checked = 1` tells the shutdown scanner (see below) that this
panic was observed by a caller, even if the caller chose not to re-raise it.

---

## `await` - Panic Re-raise

The codegen for every `await` expression emits a check immediately after the
`_tin_fiber_join` returns:

```
; Pseudo-IR emitted by genAwaitExpr
%pmsg    = call i8* @_tin_fiber_get_panic_msg(i64 %pid)
%hit     = icmp ne i8* %pmsg, null
br i1 %hit, label %await.panic.N, label %await.ok.N

await.panic.N:
  call void @_tin_panic(i8* %pmsg)   ; re-raise in calling fiber's context
  ; if _tin_panic returns (panic was recovered by defer+recover in this fn):
  ;   emit coroComplete(recoverRetVal) + br %coro.final   [coro variant]
  ;   OR ret <zero>                                        [sync variant]

await.ok.N:
  ; ... unbox and return result ...
```

The `_tin_panic` call happens **in the calling fiber's context** - the same
function that has `defer + recover()` entries on the stack. This means the
panic from an inner fiber is fully catchable by the outer fiber's defers.

### ARC lifecycle of `panic_msg`

```
Worker thread:
  _tin_rc_alloc(len+1)         -> rc = 1  (fiber owns)

_tin_fiber_get_panic_msg:
  _tin_retain(pmsg)            -> rc = 2  (caller holds)

_tin_panic:
  stores pmsg in _tin_panic_msg
  walks defer chain (LIFO)
  if recover() called: _tin_str_from_cstr wraps pmsg -> TinString
    TinString released by caller      -> rc = 1
  fiber cleanup: _tin_release(pmsg)  -> rc = 0 -> freed
```

---

## `defer + recover()` in `{#async}` Functions

When an `{#async}` function itself catches a panic via `defer + recover()`,
`_tin_panic` returns instead of exiting. The compiler must emit a valid
coroutine terminator after the `_tin_panic` call.

### The `recoverRetVal` helper

```go
// codegen/coro.go
// Returns the value to pass to emitCoroComplete after _tin_panic returns.
// Priority:
//   1. defer return-override slot (if a thunk wrote a valid value)
//   2. zero value of the coro's declared return type
//   3. nil (void return type)
func (cg *CodeGen) recoverRetVal(block *ir.Block) value.Value
```

This is used in two places:

**`genAwaitExpr` (`codegen/exprs.go`)** - panicBlk, after `_tin_panic` call:
```go
if cg.inCoroFn {
    cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
    cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)
} else {
    // sync variant: just return the zero value
    panicBlk.NewRet(cg.zeroValue(cg.curFn.Sig.RetType))
}
```

**`genBuiltinPanic` (`codegen/runtime.go`)** - after `_tin_panic` call:
```go
if cg.inCoroFn {
    cg.emitCoroComplete(block, cg.recoverRetVal(block))
    cg.emitFinalSuspend(block, cg.curCoroFrame)
} else {
    block.NewRet(cg.zeroValue(cg.curFn.Sig.RetType))
}
```

### Return value semantics

| Situation                                | Outer awaiter receives                       |
|------------------------------------------|----------------------------------------------|
| Panic, not recovered                     | Panic re-raised at `await` point             |
| Panic recovered, no return override      | Zero value of `T` (`0`, `false`, `""`, etc.) |
| Panic recovered, defer wrote return slot | Override value from defer thunk              |
| Normal return                            | The returned value                           |

Example - outer awaiter gets `0` for an `i64` async function that swallowed
its inner fiber's panic:

```rust
fn{#async} always_panics() i64 =
  panic("boom from fiber")
  return 0

fn{#async} catches_await_panic() i64 =
  defer do:
    let _ = recover()       // swallow panic; no return override
  let _ = await spawn always_panics()
  return 0

test "recovered await-panic yields zero" =
  let result = await spawn catches_await_panic()
  assert::equals(result, 0)   // zero value of i64
```

---

## Fire-and-Forget Panic Detection

Fibers that panicked but were never awaited are detected at process shutdown.
Unlike normal ff fibers (which are reclaimed immediately at completion),
panicking ff fibers are **not** reclaimed: `panic_msg` must remain readable
by `_tin_fiber_check_panic` until the fiber table is torn down.

`_tin_fiber_run` scans the fiber table after all workers have stopped:

```c
for (int64_t i = 1; i < _fiber_cnt; i++) {
    if (_fibers[i] && _fibers[i]->panic_msg && !_fibers[i]->panic_checked) {
        const char *msg = _fibers[i]->panic_msg;
        _fibers[i]->panic_msg = NULL;  // prevent double-release
        _tin_panic(msg);               // fatal: calls exit(1) on main thread
    }
}
```

This mirrors Go's behavior: a goroutine/fiber panic that nobody handles kills
the process. The `panic_checked` flag (set by `_tin_fiber_get_panic_msg`) is
the distinction between "caller saw the panic and chose to recover" vs.
"nobody ever awaited this fiber."

---

## Codegen Invariants

Three invariants are required for correct panic handling in `{#async}`
functions. Each was missing at some point and caused a distinct failure.

### 1. `curFnDeferRetAlloca` must be initialized per coro body

`genFuncDecl` allocates a `{i8, retType}` slot in the function's entry block
for the defer return-override mechanism. `genCoroFuncBody` compiles a separate
LLVM function (`fn$coro`) and must do the same in its `bodyStart` block.

Without this, `cg.curFnDeferRetAlloca` bleeds in from the outer (non-coro)
function. When the coro's IR is serialized, the cross-function SSA value
becomes whatever happens to be `%2` in the coro function - typically
`llvm.coro.size.i64()` (type `i64`), not `i8*` - causing:

```
error: '%2' defined with type 'i64' but expected 'ptr'
 call void @_tin_defer_push(i8* ..., i8* ..., i8* null, i8* %2)
```

Fix in `genCoroFuncBody` (`codegen/coro.go`):
```go
// Save
prevFnDeferRetAlloca := cg.curFnDeferRetAlloca

// Initialize for this coro body (mirrors genFuncDecl lines 638-651)
if origRetType != nil && !irtypes.IsVoid(origRetType) {
    slotType  := irtypes.NewStruct(irtypes.I8, origRetType)
    slotAlloca := bodyStart.NewAlloca(slotType)
    validGep  := bodyStart.NewGetElementPtr(slotType, slotAlloca,
        constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
    bodyStart.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
    cg.curFnDeferRetAlloca = bodyStart.NewBitCast(slotAlloca, irtypes.I8Ptr)
} else {
    cg.curFnDeferRetAlloca = nil
}

// Restore
cg.curFnDeferRetAlloca = prevFnDeferRetAlloca
```

### 2. `inCoroFn` must be reset inside defer thunks and closures

Defer thunks are plain functions, not coroutines. If `cg.inCoroFn` is `true`
when a thunk is compiled inside a coro body, the thunk would emit
`_tin_fiber_complete` + `br label %coro.final.N` (a cross-function SSA
reference) instead of `ret void`.

`pushClosureCtx` / `popClosureCtx` (`codegen/helpers.go`) save and reset
`cg.inCoroFn = false` before compiling each closure or defer thunk.

### 3. `emitCoroComplete` must not receive `nil` after panic recovery

Before this fix, both `genAwaitExpr` and `genBuiltinPanic` called
`emitCoroComplete(block, nil)` after `_tin_panic`. If the panic was recovered
(i.e., `_tin_panic` returned), the fiber was marked done with a `NULL` result
pointer. The outer awaiter would then:

```
%raw = call i8* @_tin_fiber_get_result(pid)  ; returns NULL
%ptr = bitcast i8* %raw to i64*
%val = load i64, i64* %ptr                   ; SEGFAULT
```

The fix passes `recoverRetVal(block)` instead of `nil`, which provides the
zero value (or defer-override value) as the fiber's result.
