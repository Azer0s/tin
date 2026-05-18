package codegen

import (
	"fmt"
	"os"
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) collectAwaitedOrSpawnedCalls(prog *ast.Program) map[*ast.CallExpr]bool {
	out := map[*ast.CallExpr]bool{}

	for _, stmt := range prog.Stmts {
		walkAST(stmt, func(n ast.Node) {
			switch w := n.(type) {
			case *ast.AwaitExpr:
				if ce, ok := w.Future.(*ast.CallExpr); ok {
					out[ce] = true
				}
			case *ast.SpawnExpr:
				if ce, ok := w.Call.(*ast.CallExpr); ok {
					out[ce] = true
				}
			}
		})
	}

	return out
}

// asyncFuncDeclForCall returns the AST FuncDecl for ce's callee if the
// callee resolves to a `fn{#async}`-tagged declaration (free fn, struct
// method, or trait-qualified method).  Returns nil otherwise.  Used by
// the bare-async-call lints to identify candidate call sites without
// needing type-resolved receiver info.
func (cg *CodeGen) asyncFuncDeclForCall(ce *ast.CallExpr) *ast.FuncDecl {
	switch fn := ce.Func.(type) {
	case *ast.Identifier:
		if fd, ok := cg.funcDecls[fn.Name]; ok && fd != nil && isAsyncTag(fd.Tags) {
			return fd
		}
	case *ast.FieldAccess:
		// Method call: search every funcDecl whose key is exactly
		// `<StructName>_<field>` for an async-tagged match.  We don't have
		// receiver-type resolution at AST-check time, so we conservatively
		// scan struct-method keys.  The prefix must be a known struct name
		// (registered in cg.structTypes) -- without that guard, a free fn
		// like `cond_producer_broadcast` would falsely match a call to
		// `cond.broadcast()` by suffix alone.
		suffix := "_" + fn.Field

		for key, fd := range cg.funcDecls {
			if fd == nil || !isAsyncTag(fd.Tags) {
				continue
			}

			if !strings.HasSuffix(key, suffix) {
				continue
			}

			prefix := strings.TrimSuffix(key, suffix)
			if cg.structTypeFor(CanonKey(prefix)) != nil {
				return fd
			}
		}
	case *ast.ScopeAccess:
		// `pkg::name` form -- the last path segment is the fn name.
		if len(fn.Path) > 0 {
			last := fn.Path[len(fn.Path)-1]
			if fd, ok := cg.funcDecls[last]; ok && fd != nil && isAsyncTag(fd.Tags) {
				return fd
			}
		}
	}

	return nil
}

// fnBodyMayPark reports whether fd's body transitively reaches a
// suspension point: a direct `yield` / `await` expression, or a call to
// a known-parking C extern, or a call to another fn we have already
// classified as parking.  Used by `-Wbare-parking-async-call` to gate
// the safety warning so compute-only `#async` fns stay silent under the
// default lint set.  Conservative: when the analysis can't resolve a
// callee it answers "yes" (false-positive over false-negative for
// safety).
//
// Implementation walks fd.Body once and caches the result on
// cg.fnParkingClass; the cache breaks recursion and keeps repeated
// queries cheap.  Seeded with the runtime's known-parking primitives.
func (cg *CodeGen) fnBodyMayPark(fd *ast.FuncDecl) bool {
	if fd == nil || fd.Body == nil {
		// No body to analyze (e.g. extern decl): assume parking unless
		// the runtime seeds say otherwise.  Currently every extern reaches
		// here without a seed and is treated conservatively.
		return true
	}

	if cls, ok := cg.fnParkingClass[fd.Name]; ok {
		return cls
	}
	// Mark in-progress as "may park" to break recursion safely.
	cg.fnParkingClass[fd.Name] = true

	parks := false

	walkAST(fd.Body, func(n ast.Node) {
		if parks {
			return
		}

		switch v := n.(type) {
		case *ast.AwaitExpr:
			parks = true
		case *ast.YieldStmt:
			parks = true
		case *ast.CallExpr:
			if calleeFd := cg.asyncFuncDeclForCall(v); calleeFd != nil && calleeFd != fd {
				if cg.fnBodyMayPark(calleeFd) {
					parks = true
				}
			}

			if id, ok := v.Func.(*ast.Identifier); ok {
				if cg.knownParkingExterns[id.Name] {
					parks = true
				}
			}
		}
	})

	cg.fnParkingClass[fd.Name] = parks

	return parks
}

// checkBareAsyncCall fires the bare-async-call lints when `ce` calls a
// `fn{#async}` without `await` or `spawn` wrapping it.  Two-tier:
//   - `-Wbare-parking-async-call` (default on) only if the callee body
//     may park -- this is the safety net.
//   - `-Wbare-async-call` (pedantic, default off) on every bare async
//     call regardless of body content.
func (cg *CodeGen) checkBareAsyncCall(ce *ast.CallExpr, awaitedOrSpawned map[*ast.CallExpr]bool) {
	if awaitedOrSpawned[ce] {
		return
	}

	fd := cg.asyncFuncDeclForCall(ce)
	if fd == nil {
		return
	}

	name := fd.Name

	if cg.fnBodyMayPark(fd) {
		cg.warn(DiagBareParkingAsyncCall, ce.Pos(),
			"calling `%s` directly may park the calling thread; use `await spawn %s(...)` to run it on a fiber, or `spawn %s(...)` for fire-and-forget",
			name, name, name)
	}

	cg.warn(DiagBareAsyncCall, ce.Pos(),
		"`%s` is `#async`; consider `await spawn %s(...)` to make the spawn explicit, or `spawn %s(...)` for fire-and-forget",
		name, name, name)
}

// checkSyncUsesAwait fires `-Wsync-uses-await` on every sync fn body
// that contains a literal `await` expression.  Doesn't follow inlined
// or generated awaits -- only the user's source-level keyword, since
// the lint's purpose is documentation: pedantic projects want sync
// signatures to disclose parking behavior.
//
// Async fns (`fn{#async}`) skip the walk -- their use of await is
// expected.  Test blocks are sync functions today; if/when they're
// promoted to implicit `#async`, the resolver should set a flag on
// the FuncDecl rather than special-casing here.
func (cg *CodeGen) checkSyncUsesAwait(fd *ast.FuncDecl) {
	if fd == nil || fd.Body == nil {
		return
	}

	if isAsyncTag(fd.Tags) {
		return
	}

	walkAST(fd.Body, func(n ast.Node) {
		if aw, ok := n.(*ast.AwaitExpr); ok {
			cg.warn(DiagSyncUsesAwait, aw.Pos(),
				"`await` inside sync fn %q drives the scheduler at runtime; "+
					"prefer `sync::wait(future)` to make the sync->async bridge "+
					"explicit, or promote this fn to `fn{#async}`",
				fd.Name)
		}
	})
}

// checkSyncWaitInCoroCallable emits a HARD ERROR (not warning) when
// `sync::wait(...)` appears inside any function whose body may
// execute on a worker thread -- i.e. any fn in cg.coroCallable
// (#async fns themselves AND sync fns transitively reachable from
// #async).  Calling sync::wait there silently crashes at runtime
// (target buffer reads garbage before the target fiber runs)
// because sync::wait relies on a `coro.suspend` IR following its
// `_tin_fiber_join`, and a regular sync function body has none.
//
// The right fix is to use `await` from within the async chain:
// promote the enclosing sync fn to `fn{#async}` and replace
// `sync::wait(f)` with `await f`.  sync::wait is the BOOTSTRAP
// bridge from main / pure-sync code -- not a way to wait inside
// an async-callable function.
func (cg *CodeGen) checkSyncWaitInCoroCallable(prog *ast.Program) {
	walkFn := func(fnName string, body ast.Node) {
		if body == nil {
			return
		}

		if !cg.coroCallable[fnName] {
			return
		}

		walkAST(body, func(n ast.Node) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}

			if !isSyncWaitCall(call.Func) {
				return
			}

			cg.emitSyncWaitInAsyncError(call.Pos(), fnName, call.Args)
		})
	}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			walkFn(v.Name, v.Body)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				walkFn(methodScopeName(v.Name, m), m.Body)
			}
		}
	}
}

// isSyncWaitCall reports whether `callee` is the `sync::wait` (or
// `sync::wait_all`) function reference.  Accepts the `pkg::name`
// ScopeAccess shape that imports of `sync` produce.  Does NOT
// recognize rebinds via `let mywait = sync::wait` -- the wrapper
// would have to be analyzed through Andersen flow to detect that,
// which is out of scope for this check.
func isSyncWaitCall(callee ast.Node) bool {
	sa, ok := callee.(*ast.ScopeAccess)
	if !ok {
		return false
	}

	if len(sa.Path) != 2 {
		return false
	}

	return sa.Path[0] == "sync" && (sa.Path[1] == "wait" || sa.Path[1] == "wait_all")
}

// emitSyncWaitInAsyncError prints a clear compile error at `pos` and
// sets `cg.hadWarnError` so Generate() returns failure.  Not routed
// through `cg.warn` because this is an unconditional error (not
// suppressible via -Wno-*).  The user must restructure: promote the
// enclosing sync fn to `fn{#async}` and use `await` instead.
func (cg *CodeGen) emitSyncWaitInAsyncError(pos ast.Pos, fnName string, args []ast.Node) {
	file := cg.filename
	if file == "" {
		file = "<unknown>"
	}

	raw := fmt.Sprintf("%s:%d:%d: error: sync::wait inside fn %q which is reachable from `#async`; "+
		"sync::wait is the sync-to-async bootstrap bridge for main / non-async code, "+
		"not a cooperative wait.  Promote this fn to `fn{#async}` and use `await %s` instead",
		file, pos.Line, pos.Col, fnName, renderForMessage(args))

	_, _ = fmt.Fprintln(os.Stderr, RenderDiagnostic(raw))
	cg.hadWarnError = true
}

// renderForMessage produces a short, single-line argument list for
// diagnostic strings.  Caller passes call.Args; we render up to one
// arg (the typical sync::wait shape is `sync::wait(future)`) and
// fall back to `...` for anything more elaborate.
func renderForMessage(args []ast.Node) string {
	if len(args) != 1 {
		return "<future>"
	}

	switch a := args[0].(type) {
	case *ast.Identifier:
		return a.Name
	}

	return "<future>"
}

// checkDroppableFiber fires `-Wdroppable-fiber` on a statement whose
// expression is a `SpawnExpr` -- the resulting `Future[T]` is neither
// stored, returned, nor awaited.  Fire-and-forget is a legitimate
// pattern but easy to write by accident (forgetting `let _ =` or
// `await`).  Suppress by binding to `_` or by adding `#allow_drop` to
// the spawned fn.
func (cg *CodeGen) checkDroppableFiber(es *ast.ExprStmt) {
	if es == nil {
		return
	}

	if se, ok := es.Expr.(*ast.SpawnExpr); ok {
		cg.warn(DiagDroppableFiber, se.Pos(),
			"the `Future[T]` returned by this `spawn` is discarded; "+
				"bind to `let _ =` if intentional, or `await` it for the result")
	}
}

// checkNonTinThread fires `-Wnon-tin-thread` on `#interop`-tagged fns
// whose body transitively reaches `await` or `spawn`.  The C-interop
// boundary means callers may be arbitrary OS threads that don't own
// scheduler state; awaiting or spawning from such a thread has
// undefined scheduling behavior.
//
// Implementation: piggybacks on the fnBodyMayPark cache -- a fn whose
// body may park is also "uses scheduler".  False positives on
// compute-only `#async` calls (which fnBodyMayPark classifies safe)
// are accepted: we want to flag every scheduler touch, not just the
// parking ones.
func (cg *CodeGen) checkNonTinThread(fd *ast.FuncDecl) {
	if fd == nil || fd.Body == nil {
		return
	}

	if !hasTag(fd.Tags, "interop") {
		return
	}

	touches := false

	walkAST(fd.Body, func(n ast.Node) {
		if touches {
			return
		}

		switch n.(type) {
		case *ast.AwaitExpr, *ast.SpawnExpr:
			touches = true
		}
	})

	if !touches {
		return
	}

	cg.warn(DiagNonTinThread, fd.Pos(),
		"`#interop` fn %q awaits or spawns; calls from non-Tin threads "+
			"will execute against scheduler state they do not own",
		fd.Name)
}

// checkSyncFnCoercedToAsync fires `-Wsync-fn-coerced-to-async` when a
// sync `fn(...)` value flows into a `fn{#async}(...)` slot.  The 4-slot
// fat-fn-ptr ABI makes the LLVM bytes identical, but the synthesized
// coro wrapper that fills slot 2 simply invokes the sync body inline --
// the spawned fiber never yields.  When the caller `spawn`s such a
// value expecting cooperative scheduling, the fiber runs end-to-end
// without ever cooperating.  This warning surfaces the structural
// coercion so authors can confirm the loss of yield points is intended.
//
// Coverage: call-args (most common -- passing `double` to `cb fn{#async}`)
// and let-bindings with explicit type (`let x fn{#async}(...) = sync_fn`).
// Struct-literal field and array-element forms aren't covered yet --
// extension hooks below mirror the call-arg shape.
func (cg *CodeGen) checkSyncFnCoercedToAsync(node ast.Node) {
	switch n := node.(type) {
	case *ast.CallExpr:
		// Resolve the callee's FuncDecl to learn each param's AST type.
		// Only direct identifier-callees are checked; method/pipe call
		// shapes are handled elsewhere when they land in callFatFn.
		id, ok := n.Func.(*ast.Identifier)
		if !ok {
			return
		}

		decl, ok := cg.funcDecls[id.Name]
		if !ok || decl == nil {
			return
		}

		for i, arg := range n.Args {
			if i >= len(decl.Params) {
				break
			}

			ft, ok := decl.Params[i].Type.(*ast.FuncType)
			if !ok || !ft.IsAsync {
				continue
			}

			cg.warnSyncFnIntoAsyncSlot(arg)
		}
	case *ast.VarDecl:
		if n.Type == nil || n.Value == nil {
			return
		}

		ft, ok := n.Type.(*ast.FuncType)
		if !ok || !ft.IsAsync {
			return
		}

		cg.warnSyncFnIntoAsyncSlot(n.Value)
	}
}

// warnSyncFnIntoAsyncSlot fires the warning when `expr` is an identifier
// (or a fn-ref) referring to a sync FuncDecl.  Splitting the resolver
// from the slot-detection lets the call-arg and let-binding paths share
// the same source-fn lookup logic.
func (cg *CodeGen) warnSyncFnIntoAsyncSlot(expr ast.Node) {
	id, ok := expr.(*ast.Identifier)
	if !ok {
		return
	}

	srcDecl, ok := cg.funcDecls[id.Name]
	if !ok || srcDecl == nil {
		return
	}
	// Source is async already -- no coercion.
	if hasTag(srcDecl.Tags, "async") {
		return
	}
	// `#no_autoyield` suppresses $colored emission: the synth wrapper
	// would call the uncolored body and the spawned fiber really
	// wouldn't yield.  Surface that as the strongest reason to declare
	// `{#async}` (or remove the tag).  Default case is signature drift
	// + one extra synth-wrapper frame per spawn.
	if hasTag(srcDecl.Tags, "no_autoyield") {
		cg.warn(DiagSyncFnCoercedToAsync, expr.Pos(),
			"sync fn %q (tagged `#no_autoyield`) flows into a `fn{#async}` "+
				"slot; the spawned fiber will run end-to-end without yielding. "+
				"Declare %q `fn{#async}` to opt in to cooperation, or remove "+
				"`#no_autoyield`.",
			id.Name, id.Name)

		return
	}

	cg.warn(DiagSyncFnCoercedToAsync, expr.Pos(),
		"sync fn %q flows into a `fn{#async}` slot; spawning will work and "+
			"cooperate at the source's coloring points via a synthesized coro "+
			"wrapper, but each `spawn` pays for an extra wrapper frame. "+
			"Declare %q `fn{#async}` to emit the coro body directly.",
		id.Name, id.Name)
}

// checkRedundantArrayElemCasts walks a VarDecl whose annotated type
// pins the slot type and warns on `<literal> as T` where T already
// matches the slot.  Auto-coercion of literals to the declared slot
// type is unconditional in Tin -- the explicit cast adds nothing:
//
//	let a json5 = "hello"          // works without `as json5`
//	let x [u32; 2] = [0x1, 0x2]    // works without `as u32`
//
// Three slot shapes are checked:
//  1. `let x T = <lit> as T`           (single value)
//  2. `let x [T; N] = [<lit> as T,..]` (fixed array element)
//  3. `let x [T]    = [<lit> as T,..]` (fat array element)
//
// The check is intentionally conservative: only literals are flagged.
// A cast like `let x u32 = my_i64 as u32` is a real conversion and
// must keep the `as`.
