package codegen

// borrow_analyzer.go - intraprocedural borrow inference.
//
// For each function body, classify let-bindings that can be safely
// marked Borrowed (no retain at creation, no release at scope exit)
// because (a) the initializer is a pure alias of another binding
// (`let t = s`), and (b) the rest of the body only consumes the
// binding through patterns the ARC runtime can serve from the source
// rc-block alone -- field reads, function call arguments, comparisons,
// echo, etc.
//
// Conservative by design: any pattern the analyzer cannot reason about
// (return-of-binding, address-of, mutation, capture into closure or
// fiber, generic dispatch, AS / IS / TypeAssert coercions) falls back
// to Owned. The runtime behavior of an Owned binding is exactly the
// non-borrow baseline, so misclassification reduces optimization but
// never breaks correctness.
//
// Behind --ownership-borrow flag, default OFF until the analyzer has
// soaked on real workloads. See notes/ownership-implementation.md
// for the rollout.

import (
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// analyzeFunctionBorrows walks a function body and returns the set of
// let-binding names that can be classified as Borrowed.  Borrow
// analysis is a core language feature and always runs.
func (cg *CodeGen) analyzeFunctionBorrows(body ast.Node) map[string]bool {
	if body == nil {
		return nil
	}
	// Step 1: collect candidate bindings -- `let t = Identifier(s)`.
	// Each candidate carries its source name (the `s` in `let t = s`)
	// so we can verify s is itself a locally-declared, never-assigned
	// binding before classifying t as borrow.
	candidates := cg.collectBorrowCandidates(body)
	// Index of names that are declared via `let` in this body.  Used
	// to reject candidates whose source is external -- globals,
	// captures, parameters -- because external storage can be
	// mutated by code the analyzer cannot see.
	localBindings := cg.collectLocalBindingNames(body)
	// Names that get reassigned in this body.  A binding whose
	// source is reassigned in the same body cannot be borrowed:
	// the assignment releases the source's prior heap block, and
	// our alias would be left holding a dangling fat-ptr.
	mutated := cg.collectMutatedTargets(body)
	// Step 2: for each candidate, classify uses.
	result := map[string]bool{}

	for target, source := range candidates {
		if !localBindings[source] {
			// Source is external -- global / parameter / capture.
			// Globals are admitted when the call-graph analyzer
			// proved no transitively-called function mutates this
			// global (and the current body does not mutate it
			// directly either).  Other external sources -- params
			// and captures -- still fall back to Owned because we
			// have no analog for them yet.
			if !cg.globalAliasIsBorrowSafe(body, source) {
				continue
			}
		}

		if mutated[source] {
			// Source is reassigned in this body; the alias would
			// dangle after the reassignment's release.  Owned.
			continue
		}

		if mutated[target] {
			// The alias itself is reassigned; treat as Owned so
			// the first value's release fires.
			continue
		}

		if cg.bindingIsBorrowSafe(body, target) {
			result[target] = true
		}
	}

	return result
}

// globalAliasIsBorrowSafe reports whether `source` names a module-level
// global that no callee in `body` (transitively) mutates.  Returns
// false for non-globals so callers fall through to the existing
// localBindings rejection.
func (cg *CodeGen) globalAliasIsBorrowSafe(body ast.Node, source string) bool {
	if _, isGlobal := cg.topLevelVarPos[source]; !isGlobal {
		return false
	}

	mutators := cg.computeGlobalMutators()[source]
	if len(mutators) == 0 {
		return true
	}
	// Walk body for direct callees; if any callee is a mutator of
	// the global, reject the borrow.  Indirect calls (fn-ptr,
	// vtable dispatch) currently fall through as conservative-
	// unknown.
	bad := false

	walkAST(body, func(n ast.Node) {
		if bad {
			return
		}

		call, ok := n.(*ast.CallExpr)
		if !ok || call == nil {
			return
		}

		name := directCalleeName(call.Func)
		if name == "" {
			return
		}

		if mutators[name] {
			bad = true
		}
	})

	return !bad
}

// directCalleeName extracts a flat callee name from a call expression
// when the callee is recognizable from a single AST node.  Returns ""
// for indirect calls (loaded fn-ptrs, lambda invocations, etc.) so
// the caller treats them as conservative-unknown.
func directCalleeName(fn ast.Node) string {
	switch v := fn.(type) {
	case *ast.Identifier:
		if v == nil {
			return ""
		}

		return v.Name
	case *ast.ScopeAccess:
		if v == nil {
			return ""
		}
		// "a::b::c" -> "a__b__c" -- the call-graph keys mirror this.
		joined := ""

		for i, p := range v.Path {
			if i > 0 {
				joined += "__"
			}

			joined += p
		}

		return joined
	case *ast.FieldAccess:
		if v == nil {
			return ""
		}
		// Method call on a value: lookup by bare field name; the
		// mutators set carries struct-qualified scope keys and we
		// match conservatively (a same-named method on any struct
		// could be a mutator).
		return v.Field
	}

	return ""
}

// analyzeFunctionParamConventions classifies each parameter as
// transparent / consumes / retains based on what the body does with
// it.  The convention drives caller-side rc emission at every call
// site: transparent params get no caller-side rc work; consumes /
// retains both get retain-before / release-after.  The split between
// the two matters for `move arg` -- consumes can absorb the moved rc
// into the sink so no post-call release fires.
//
// See docs/15-ownership.md "The call site is where rc work happens"
// for the user-facing description.
func (cg *CodeGen) analyzeFunctionParamConventions(body ast.Node, paramNames []string) map[string]ParamConvention {
	if body == nil || len(paramNames) == 0 {
		return nil
	}
	// Param mutation check: only `p = newptr` (direct reassign of the
	// binding) disqualifies the transparent classification; writes
	// through the pointer (`*p = x`, `p.f = x`, `p[i] = x`) mutate
	// the pointee, not the param itself, so the caller's rc invariant
	// holds.  Without this distinction, `fn inc(p *i64) = *p = *p + 1`
	// would always force the caller's pre-call retain even when the
	// body never reassigns p.
	reassigned := cg.collectReassignedNames(body)
	sinks := cg.collectParamSinks(body)
	result := map[string]ParamConvention{}

	for _, name := range paramNames {
		if name == "" {
			continue
		}

		if reassigned[name] {
			// Reassigning the param itself is an opaque rc
			// transition; conservatively classify as retains so
			// the caller keeps its rc alive across the call.
			result[name] = paramRetains

			continue
		}

		if cg.bindingIsBorrowSafe(body, name) {
			result[name] = paramTransparent

			continue
		}

		if sinks[name] {
			result[name] = paramConsumes

			continue
		}

		result[name] = paramRetains
	}

	return result
}

// paramConventionsFor returns the cached per-param convention map
// for fnName, populating it from funcDecls on first lookup.  Returns
// nil when the function is not in funcDecls (extern, lambda, trait
// dispatch target whose impl is unknown at the call site).  Callers
// treat nil as "convention unknown" -- ref-arg checks stay silent in
// that case rather than rejecting valid code.
func (cg *CodeGen) paramConventionsFor(fnName string) map[string]ParamConvention {
	if cg.fnParamConventions == nil {
		cg.fnParamConventions = map[string]map[string]ParamConvention{}
	}

	if cached, ok := cg.fnParamConventions[fnName]; ok {
		return cached
	}

	decl := cg.funcDecls[fnName]
	if decl == nil || decl.Body == nil {
		return nil
	}

	paramNames := make([]string, 0, len(decl.Params))
	for _, p := range decl.Params {
		if p.Name != "" && !p.IsVarArgs {
			paramNames = append(paramNames, p.Name)
		}
	}

	result := cg.analyzeFunctionParamConventions(decl.Body, paramNames)
	cg.fnParamConventions[fnName] = result

	return result
}

// paramMutatedFor returns the cached per-param mutation map for
// fnName, populating it from funcDecls on first lookup.  A param is
// "mutated" when the body contains an AssignStmt/AugAssignStmt/
// PostfixStmt whose target chain roots at that param name -- direct
// reassign, field-write, index-write, or write-through-pointer.
// Read by call-site dispatch to decide when a struct-typed arg
// flowing into a mutating callee needs deep_copy to preserve the
// caller's value-semantics view.  Returns nil for unknown callees.
func (cg *CodeGen) paramMutatedFor(fnName string) map[string]bool {
	if cg.fnParamMutated == nil {
		cg.fnParamMutated = map[string]map[string]bool{}
	}

	if cached, ok := cg.fnParamMutated[fnName]; ok {
		return cached
	}

	decl := cg.funcDecls[fnName]
	if decl == nil || decl.Body == nil {
		return nil
	}

	result := cg.collectMutatedTargets(decl.Body)
	cg.fnParamMutated[fnName] = result

	return result
}

// collectParamSinks walks `body` and returns the set of identifier
// names that escape via a "sink" the analyzer recognizes.  A sink is
// a use site where the body transfers an rc into a recipient that
// outlives the function call: a return value, an assignment into a
// struct field of another rc-tracked binding, a channel send, or a
// closure / spawn capture.  When every escape path for a param is a
// sink, the convention can be paramConsumes (the body has somewhere
// to put the moved rc) rather than paramRetains.
//
// Conservative on uncertainty: a name not in the set defaults to
// retains, which is the safe higher-traffic side.
func (cg *CodeGen) collectParamSinks(body ast.Node) map[string]bool {
	out := map[string]bool{}

	if body == nil {
		return out
	}

	walkAST(body, func(n ast.Node) {
		switch v := n.(type) {
		case *ast.ReturnStmt:
			if v == nil || v.Value == nil {
				return
			}

			if id, ok := v.Value.(*ast.Identifier); ok && id != nil {
				out[id.Name] = true
			}
		case *ast.AssignStmt:
			if v == nil || v.Value == nil {
				return
			}
			// `target.field = id`, `target[i] = id`, `*target = id`:
			// the rvalue identifier flows into a sink controlled by
			// another binding (target's storage).
			switch v.Target.(type) {
			case *ast.FieldAccess, *ast.IndexExpr, *ast.DerefExpr:
				if id, ok := v.Value.(*ast.Identifier); ok && id != nil {
					out[id.Name] = true
				}
			}
		case *ast.SpawnExpr:
			if v == nil {
				return
			}
			// Anything used inside a spawn body is captured into
			// the closure env and outlives the call boundary.
			walkAST(v, func(n2 ast.Node) {
				if id, ok := n2.(*ast.Identifier); ok && id != nil {
					out[id.Name] = true
				}
			})
		case *ast.LambdaExpr:
			if v == nil {
				return
			}
			// Same reasoning as SpawnExpr: lambda captures persist
			// past the call when the lambda escapes.
			walkAST(v.Body, func(n2 ast.Node) {
				if id, ok := n2.(*ast.Identifier); ok && id != nil {
					out[id.Name] = true
				}
			})
		}
	})

	return out
}

// collectReassignedNames returns the set of identifier names that are
// REASSIGNED in `node` (`p = x` or `p op= x` -- the identifier
// itself, not its pointee).  Distinct from `collectMutatedTargets`,
// which conservatively walks through every wrapper (FieldAccess,
// IndexExpr, DerefExpr) and lumps reads-through-write together.
func (cg *CodeGen) collectReassignedNames(node ast.Node) map[string]bool {
	out := map[string]bool{}

	if node == nil {
		return out
	}

	record := func(target ast.Node) {
		if id, isID := target.(*ast.Identifier); isID && id != nil {
			out[id.Name] = true
		}
	}

	walkAST(node, func(n ast.Node) {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if v != nil {
				record(v.Target)
			}
		case *ast.AugAssignStmt:
			if v != nil {
				record(v.Target)
			}
		case *ast.PostfixStmt:
			if v != nil {
				record(v.Expr)
			}
		}
	})

	return out
}

// collectBorrowCandidates walks `node` and records every let-binding
// whose initializer is a plain Identifier reference (the canonical
// `let t = s` alias pattern).  Returns a map from the new binding's
// name to the source identifier's name.
func (cg *CodeGen) collectBorrowCandidates(node ast.Node) map[string]string {
	out := map[string]string{}

	if node == nil {
		return out
	}

	walkAST(node, func(n ast.Node) {
		vd, ok := n.(*ast.VarDecl)
		if !ok || vd == nil {
			return
		}

		if vd.Name == "" {
			return
		}

		if id, isIdent := vd.Value.(*ast.Identifier); isIdent && id != nil {
			out[vd.Name] = id.Name

			return
		}
		// `let t = &s[i]` -- raw `*T` view into an indexed
		// element of a local array.  Borrow-safe iff the rest of
		// the analyzer's checks (no escape, source not mutated)
		// hold; the downstream isSafeBorrowUse walk catches
		// returns, captures into closures/fibers, etc.  Other
		// alias shapes (&s, &s.f, s.f, s[i], *s) are deliberately
		// not included -- each interacts with iface-coerce /
		// per-struct release paths in ways the borrow walk
		// doesn't safely cover yet.
		if ao, isAddr := vd.Value.(*ast.AddressOfExpr); isAddr && ao != nil {
			if ix, isIx := ao.Expr.(*ast.IndexExpr); isIx && ix != nil {
				if id, isID := ix.Expr.(*ast.Identifier); isID && id != nil {
					out[vd.Name] = id.Name
				}
			}
		}
	})

	return out
}

// collectLocalBindingNames walks `node` and returns the set of names
// declared by `let` / `var` in the body.  Used to distinguish locals
// (whose lifetime the analyzer can reason about) from externals
// (globals, parameters, captures) where mutation could happen outside
// the visible body.
func (cg *CodeGen) collectLocalBindingNames(node ast.Node) map[string]bool {
	out := map[string]bool{}

	if node == nil {
		return out
	}

	walkAST(node, func(n ast.Node) {
		if vd, ok := n.(*ast.VarDecl); ok && vd != nil && vd.Name != "" {
			out[vd.Name] = true
		}
	})

	return out
}

// collectMutatedTargets walks `node` and returns the set of binding
// names that appear as the target of any AssignStmt / AugAssignStmt /
// PostfixStmt -- either directly (`s = x`) OR indirectly via an
// address-of trampoline (`let p = &s; *p = x`).  A borrow whose
// source name appears in this set cannot stay borrowed: the
// assignment will release the source's prior heap block and any
// alias still pointing at it would dangle.
//
// The address-of-then-assign-through-pointer case is detected
// conservatively: any binding whose address is taken anywhere in
// the body is considered mutatable.  This loses some legitimate
// borrow opportunities (`let p = &s; read_only(p)`) in exchange
// for closing a real UAF where `*p = newvalue` would release the
// source's heap block out from under the alias.
func (cg *CodeGen) collectMutatedTargets(node ast.Node) map[string]bool {
	out := map[string]bool{}

	if node == nil {
		return out
	}
	// Pre-scan: does the body contain ANY `*expr = ...` or
	// `*expr op= ...` writeback through a pointer?  Without one,
	// `&x` cannot reach a `*p = newvalue` that would mutate x, so
	// the AddressOf-conservative-mutation branch below can stay
	// inert and read-only patterns (`let p = &xs[i]; v = *(p+1)`)
	// keep their borrow-safe candidate status.
	hasWriteThrough := false

	walkAST(node, func(n ast.Node) {
		if hasWriteThrough {
			return
		}

		switch s := n.(type) {
		case *ast.AssignStmt:
			if s != nil {
				if _, isDeref := s.Target.(*ast.DerefExpr); isDeref {
					hasWriteThrough = true
				}
			}
		case *ast.AugAssignStmt:
			if s != nil {
				if _, isDeref := s.Target.(*ast.DerefExpr); isDeref {
					hasWriteThrough = true
				}
			}
		}
	})

	walkAST(node, func(n ast.Node) {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if v == nil {
				return
			}

			recordTargetRoot(v.Target, out)
		case *ast.AugAssignStmt:
			if v == nil {
				return
			}

			recordTargetRoot(v.Target, out)
		case *ast.PostfixStmt:
			if v == nil {
				return
			}

			recordTargetRoot(v.Expr, out)
		case *ast.AddressOfExpr:
			if v == nil {
				return
			}
			// `&s`, `&s.field`, `&s[i]` open the door to
			// `*p = newvalue` only when the body contains a
			// `*expr = ...` writeback (hasWriteThrough pre-scan).
			// Read-only patterns like `let p = &xs[i]; v = *(p+1)`
			// stay borrow-safe.
			if hasWriteThrough {
				recordTargetRoot(v.Expr, out)
			}
		case *ast.CallExpr:
			if v == nil {
				return
			}
			// Method calls (`t.method(...)`) on a pointer-receiver
			// method may mutate `t` through `*this`; only those
			// definitions land in cg.methodMayMutateReceiver*.
			// When every method with this base name has a value
			// receiver the call cannot mutate the caller's binding,
			// so the analyzer can keep `t` as a candidate borrow
			// (sound: value-receiver methods receive a copy and
			// any mutation lives in the callee's stack frame).
			//
			// When the receiver expression's type can be inferred,
			// dispatch through the per-type map so a value-receiver
			// `foo` on struct A keeps its borrow even if some other
			// struct B has a pointer-receiver `foo`.  Fall back to
			// the bare-name map when inference fails (conservative).
			//
			// Free function calls (`f(t)`, Func not a FieldAccess)
			// fall through and do not feed recordTargetRoot.
			if fa, ok := v.Func.(*ast.FieldAccess); ok && fa != nil {
				mutates := false

				if recvType := cg.astInferType(fa.Expr); recvType != nil {
					recvName := cg.typeNameOf(recvType)
					if recvName == "" {
						if pt, isPtr := recvType.(*irtypes.PointerType); isPtr {
							recvName = cg.typeNameOf(pt.ElemType)
						}
					}

					if recvName != "" {
						if perType, ok := cg.methodMayMutateReceiverByType[recvName]; ok {
							mutates = perType[fa.Field]
							// Per-type map was populated but doesn't list
							// this method.  Fall back to the bare-name map
							// in case a free `fn` (used as UFCS) or a
							// method on a different struct registered the
							// name there.  Conservative correctness beats
							// precision here.
							if !mutates {
								mutates = cg.methodMayMutateReceiver[fa.Field]
							}
						} else {
							mutates = cg.methodMayMutateReceiver[fa.Field]
						}
					} else {
						mutates = cg.methodMayMutateReceiver[fa.Field]
					}
				} else {
					mutates = cg.methodMayMutateReceiver[fa.Field]
				}

				if mutates {
					recordTargetRoot(fa.Expr, out)
				}
			}
		}
	})

	return out
}

// recordTargetRoot walks an assignment target expression and adds the
// root identifier name to `out`.  Handles compound targets so the
// borrow analyzer sees ALL forms of mutation:
//
//   - `t = x`           -> "t" (direct)
//   - `t.f = x`         -> "t" (write through field)
//   - `t.f.g = x`       -> "t" (nested)
//   - `t[i] = x`        -> "t" (write through index)
//   - `t[i].f = x`      -> "t" (mixed)
//   - `*t = x`          -> "t" (write through deref)
//
// A write through a compound target mutates the storage rooted at the
// identifier, so any borrow of that identifier would either skip a
// rc-affecting side effect or release a buffer it does not own.
// Conservative: treat the root as mutated.
func recordTargetRoot(target ast.Node, out map[string]bool) {
	for {
		switch t := target.(type) {
		case *ast.Identifier:
			if t != nil {
				out[t.Name] = true
			}

			return
		case *ast.FieldAccess:
			if t == nil {
				return
			}

			target = t.Expr
		case *ast.IndexExpr:
			if t == nil {
				return
			}

			target = t.Expr
		case *ast.DerefExpr:
			if t == nil {
				return
			}

			target = t.Expr
		default:
			return
		}
	}
}

// bindingIsBorrowSafe walks `body` and returns true when every use of
// `name` is a pattern the runtime can serve from the source rc-block
// alone.  Conservative: any unrecognized use is treated as escape.
//
// Closure / fiber / defer captures are treated as escapes: the
// captured binding may outlive the source's scope, so the source must
// hold its own +1 reference.  Any Identifier match found inside a
// LambdaExpr / SpawnExpr / DeferStmt body marks the binding as
// not-borrow-safe; field-narrowed captures that would let a subset of
// fields stay safely borrowed require env restructuring not yet
// implemented.
func (cg *CodeGen) bindingIsBorrowSafe(body ast.Node, name string) bool {
	return !cg.findEscapeOrUnsafeUse(body, nil, name, false)
}

// bodyWritesToGlobal reports whether `body` contains an assignment whose
// target's root identifier matches a top-level (module-level) var.  Used
// by the biased-RC analyzer to disqualify functions that escape values
// into a globally-visible slot from using the local (shared=0) allocator.
func (cg *CodeGen) bodyWritesToGlobal(body ast.Node) bool {
	if body == nil || len(cg.topLevelVarPos) == 0 {
		return false
	}

	found := false

	walkAST(body, func(n ast.Node) {
		if found {
			return
		}

		switch v := n.(type) {
		case *ast.AssignStmt:
			if v == nil {
				return
			}

			if name := rootIdentifierName(v.Target); name != "" {
				if _, isGlobal := cg.topLevelVarPos[name]; isGlobal {
					found = true
				}
			}
		case *ast.AugAssignStmt:
			if v == nil {
				return
			}

			if name := rootIdentifierName(v.Target); name != "" {
				if _, isGlobal := cg.topLevelVarPos[name]; isGlobal {
					found = true
				}
			}
		}
	})

	return found
}

// bodyCallsExtern reports whether `body` contains a direct call to an
// extern (foreign C) function.  Extern callees are opaque to the
// analyzer: the foreign code may stash a pointer in thread-local
// state, hand it to a separate OS thread, or otherwise publish it.
// Any function that calls extern is therefore treated as a publishing
// site by the biased-RC analyzer.
func (cg *CodeGen) bodyCallsExtern(body ast.Node) bool {
	if body == nil {
		return false
	}

	found := false

	walkAST(body, func(n ast.Node) {
		if found {
			return
		}

		call, ok := n.(*ast.CallExpr)
		if !ok || call == nil {
			return
		}

		name := directCalleeName(call.Func)
		if name == "" {
			return
		}

		if decl, ok2 := cg.funcDecls[name]; ok2 && decl != nil && decl.IsExtern != "" {
			found = true
		}
	})

	return found
}

// bodyCrossesFiberBoundary reports whether `body` contains a spawn,
// await, or other operation that can hand a value off to a different
// thread.  Such functions cannot be sync-local: any RC block they
// allocate might be retained or released on the spawned-fiber's
// thread, and a non-atomic op there races with the sync caller.
func bodyCrossesFiberBoundary(body ast.Node) bool {
	if body == nil {
		return false
	}

	found := false

	walkAST(body, func(n ast.Node) {
		if found {
			return
		}

		switch n.(type) {
		case *ast.SpawnExpr, *ast.AwaitExpr:
			found = true
		}
	})

	return found
}

// rootIdentifierName walks the LHS of an assignment to return the
// outermost binding name (peels FieldAccess, IndexExpr, DerefExpr).
// Returns "" when the root is not a plain identifier.
func rootIdentifierName(target ast.Node) string {
	for {
		switch t := target.(type) {
		case *ast.Identifier:
			if t == nil {
				return ""
			}

			return t.Name
		case *ast.FieldAccess:
			if t == nil {
				return ""
			}

			target = t.Expr
		case *ast.IndexExpr:
			if t == nil {
				return ""
			}

			target = t.Expr
		case *ast.DerefExpr:
			if t == nil {
				return ""
			}

			target = t.Expr
		default:
			return ""
		}
	}
}

// findEscapeOrUnsafeUse walks `n` and returns true when it finds any
// Identifier referring to `name` that is either an unsafe-borrow use
// (per isSafeBorrowUse) or appears inside a capturing context.
// inCapture is true when the recursion entered a closure / fiber /
// defer body, which forces every match below to be classified as
// escape regardless of its parent context.
func (cg *CodeGen) findEscapeOrUnsafeUse(n, parent ast.Node, name string, inCapture bool) bool {
	if n == nil {
		return false
	}
	// Identifier match: classify based on parent and capture flag.
	if id, ok := n.(*ast.Identifier); ok && id != nil && id.Name == name {
		if inCapture {
			return true
		}

		return !isSafeBorrowUse(parent, id)
	}
	// Capturing contexts: descend into the body with inCapture=true.
	switch v := n.(type) {
	case *ast.LambdaExpr:
		if v == nil {
			return false
		}

		return cg.findEscapeOrUnsafeUse(v.Body, v, name, true)
	case *ast.SpawnExpr:
		if v == nil {
			return false
		}

		if cg.findEscapeOrUnsafeUse(v.Call, v, name, true) {
			return true
		}

		if v.DoBlock != nil {
			return cg.findEscapeOrUnsafeUse(v.DoBlock, v, name, true)
		}

		return false
	case *ast.DeferStmt:
		if v == nil {
			return false
		}

		return cg.findEscapeOrUnsafeUse(v.Call, v, name, true)
	}
	// Generic descent (non-capturing): recurse into children with the
	// current inCapture flag.  Uses the parented walker for parent
	// classification of inner Identifier nodes.
	found := false

	walkASTParented(n, parent, func(child, p ast.Node) bool {
		if found {
			return false
		}

		if child == n {
			// Don't re-classify the root; let recursion handle
			// its direct children.
			return true
		}
		// Identifier classification at this depth.
		if id, ok := child.(*ast.Identifier); ok && id != nil && id.Name == name {
			if inCapture || !isSafeBorrowUse(p, id) {
				found = true

				return false
			}

			return true
		}
		// Hand off capturing contexts to the recursive helper so
		// the inCapture flag flips on for their bodies, then
		// stop walkASTParented from descending further into them
		// (the recursive call did that).
		switch v := child.(type) {
		case *ast.LambdaExpr:
			if cg.findEscapeOrUnsafeUse(v.Body, v, name, true) {
				found = true
			}

			return false
		case *ast.SpawnExpr:
			if v.Call != nil && cg.findEscapeOrUnsafeUse(v.Call, v, name, true) {
				found = true
			}

			if v.DoBlock != nil && cg.findEscapeOrUnsafeUse(v.DoBlock, v, name, true) {
				found = true
			}

			return false
		case *ast.DeferStmt:
			if cg.findEscapeOrUnsafeUse(v.Call, v, name, true) {
				found = true
			}

			return false
		}

		return true
	})

	return found
}

// isSafeBorrowUse returns true when `id` appears in `parent` at a
// position where the binding's value is read without escaping.
//
// Safe patterns:
//   - FieldAccess.X = id (reading a field of the binding)
//   - CallExpr.Args[i] = id (function call argument; callee retains at
//     entry per Tin's calling convention, which is independent of
//     whether the caller owns)
//   - CallExpr.Func = id (calling a fn-typed binding)
//   - BinExpr.Left / Right = id (comparison or arithmetic; consumed)
//   - EchoStmt.Value = id (printed)
//   - TernaryExpr branches = id (selected then consumed)
//   - PipeExpr operand = id (pipe argument; same shape as call arg)
//   - WhereStmt subject = id (where-pattern reads, no mutation)
//   - VarDecl.Value at a fresh binding = id (`let u = t` chains; the
//     new binding may itself be Borrowed)
//
// Escape patterns (return false):
//   - ReturnStmt.Value = id (would propagate the borrow past this
//     function's frame)
//   - AssignStmt.Target / .Value = id (mutating the binding or
//     storing it into longer-lived state)
//   - AugAssignStmt.Target / .Value = id (same)
//   - AddressOfExpr.Expr = id (`&t` may escape)
//   - LambdaExpr body containing id (closure capture)
//   - SpawnExpr / AwaitExpr containing id (fiber capture)
//   - AsExpr / IsExpr / TypeAssertExpr (coercions; conservative)
//   - nil parent (id is the root of an expression; might be the
//     implicit return value of a block)
func isSafeBorrowUse(parent, id ast.Node) bool {
	if parent == nil {
		return false
	}

	switch p := parent.(type) {
	case *ast.FieldAccess:
		return p.Expr == id
	case *ast.IndexExpr:
		// `t[i]` ALONE is a read, but the parent walker only sees
		// the IndexExpr and cannot tell whether it is the target
		// of an assignment (`t[i] = x`).  A write through a
		// borrowed alias mutates the source's buffer, but the
		// underlying buffer's rc was only bumped once (for the
		// source), so a later release fires twice or the buffer
		// frees while the alias still has uses pending --
		// surfaces as cross-test bus errors in the runner.
		// Conservative: t in Expr position of IndexExpr is NOT
		// safe-borrow. t as the subscript (`x[t]`) is a pure
		// read of t's value and stays safe.
		return p.Index == id
	case *ast.CallExpr:
		if p.Func == id {
			return true
		}

		for _, a := range p.Args {
			if a == id {
				return true
			}
		}

		return false
	case *ast.BinExpr:
		return p.Left == id || p.Right == id
	case *ast.UnaryExpr:
		// `!t`, `-t`, `~t` consume the value; safe.
		return p.Expr == id
	case *ast.DerefExpr:
		// `*t` reads through the borrowed pointer; safe as long
		// as the source binding is alive (LIFO scope-release
		// keeps the source's storage live until after this
		// borrow's scope exit).
		return p.Expr == id
	case *ast.EchoStmt:
		return p.Value == id
	case *ast.TernaryExpr:
		return p.Then == id || p.Else == id || p.Cond == id
	case *ast.PipeExpr:
		return p.Left == id || p.Right == id
	case *ast.VarDecl:
		return p.Value == id
	case *ast.ReturnStmt:
		// `return id` propagates the value out of the function.
		// For RC-tracked values the caller takes an owned ref;
		// borrowing here would skip the retain the caller needs.
		// Stay conservative: return is an escape.
		return false
	}

	return false
}

// walkASTParented is a recursive AST walker with parent-context
// tracking: fn is called with both the current node and its immediate
// parent.  Returning false skips descending into the node's children.
// Used by the borrow analyzer to classify each Identifier use by its
// containing expression. The unparented walkAST (fold.go) is used for
// non-context-sensitive sweeps.
func walkASTParented(node, parent ast.Node, fn func(n, parent ast.Node) bool) {
	if node == nil {
		return
	}

	if !fn(node, parent) {
		return
	}

	switch v := node.(type) {
	case *ast.Block:
		if v == nil {
			return
		}

		for _, s := range v.Stmts {
			walkASTParented(s, v, fn)
		}
	case *ast.ExprStmt:
		if v == nil {
			return
		}
		// An ExprStmt wraps a tail / standalone expression -- in a
		// where-arm body it represents the arm's implicit return.
		// Descend so the inner identifier reaches the classifier;
		// without an isSafeBorrowUse case for ExprStmt the parent
		// falls through to escape, which matches "return value
		// propagates past this frame".
		walkASTParented(v.Expr, v, fn)
	case *ast.VarDecl:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.AssignStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Target, v, fn)
		walkASTParented(v.Value, v, fn)
	case *ast.AugAssignStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Target, v, fn)
		walkASTParented(v.Value, v, fn)
	case *ast.ReturnStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.IfStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Cond, v, fn)
		walkASTParented(v.Then, v, fn)

		for _, e := range v.ElseIfs {
			walkASTParented(e.Cond, v, fn)
			walkASTParented(e.Body, v, fn)
		}

		walkASTParented(v.Else, v, fn)
	case *ast.ForStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Init, v, fn)
		walkASTParented(v.Cond, v, fn)
		walkASTParented(v.Post, v, fn)
		walkASTParented(v.Iter, v, fn)
		walkASTParented(v.Body, v, fn)
	case *ast.MatchStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)

		for _, c := range v.Cases {
			walkASTParented(c.Pattern, v, fn)
			walkASTParented(c.Guard, v, fn)
			walkASTParented(c.Body, v, fn)
		}

		walkASTParented(v.Default, v, fn)
	case *ast.EchoStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.CallExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Func, v, fn)

		for _, a := range v.Args {
			walkASTParented(a, v, fn)
		}
	case *ast.BinExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Left, v, fn)
		walkASTParented(v.Right, v, fn)
	case *ast.UnaryExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.TernaryExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Cond, v, fn)
		walkASTParented(v.Then, v, fn)
		walkASTParented(v.Else, v, fn)
	case *ast.PipeExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Left, v, fn)
		walkASTParented(v.Right, v, fn)
	case *ast.LambdaExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Body, v, fn)
	case *ast.AddressOfExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.DerefExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.AsExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.IsExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.TypeAssertExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.AwaitExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Future, v, fn)
	case *ast.SpawnExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Call, v, fn)
	case *ast.MoveExpr:
		// `move x` transfers ownership.  Synthesize an Identifier node
		// so the analyzer's same-name use-classification fires; the
		// parent context here is the MoveExpr itself, which
		// isSafeBorrowUse treats as an escape (move is a transfer,
		// not a borrow).
		if v == nil {
			return
		}

		idRef := &ast.Identifier{Name: v.Name}
		walkASTParented(idRef, v, fn)
	case *ast.FieldAccess:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.IndexExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
		walkASTParented(v.Index, v, fn)
	case *ast.StructLit:
		if v == nil {
			return
		}

		for _, f := range v.Fields {
			walkASTParented(f.Value, v, fn)
		}
	case *ast.ArrayLit:
		if v == nil {
			return
		}

		for _, e := range v.Elems {
			walkASTParented(e, v, fn)
		}
	case *ast.InterpolatedString:
		// "{x} + {y}" parses as InterpolatedString with each {expr}
		// part carrying an Expr.  Without descent, identifier uses
		// inside interpolations are invisible to the analyzer, and
		// a parameter whose only "uses" were in interpolated
		// abort-strings would be falsely classified as borrow with
		// no observed uses at all -- unsound when the runtime
		// later calls into the interpolation to print the param.
		if v == nil {
			return
		}

		for _, p := range v.Parts {
			if p.IsExpr {
				walkASTParented(p.Expr, v, fn)
			}
		}
	case *ast.WhereList:
		// `where` clauses can contain identifier references in both
		// the condition and the body; missing them under-classifies
		// uses and overreaches on borrow eligibility.
		if v == nil {
			return
		}

		for _, c := range v.Clauses {
			walkASTParented(c.Cond, v, fn)
			walkASTParented(c.Body, v, fn)
		}
	case *ast.TupleLit:
		if v == nil {
			return
		}

		for _, e := range v.Elems {
			walkASTParented(e, v, fn)
		}
	case *ast.SliceExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
		walkASTParented(v.Start, v, fn)
		walkASTParented(v.End, v, fn)
	case *ast.ArrayFillLit:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.ArrayDestructDecl:
		if v == nil {
			return
		}
		// `let [a, ...rest] = src` -- the RHS is an expression whose
		// identifier uses must be visible to the classifier.  Without
		// this case the param `src` in `let [...] = src` would never
		// be reached and could be misclassified as borrow.
		walkASTParented(v.Value, v, fn)
	case *ast.StructDestructDecl:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.TupleDestructDecl:
		if v == nil {
			return
		}

		walkASTParented(v.Value, v, fn)
	case *ast.PostfixStmt:
		if v == nil {
			return
		}

		walkASTParented(v.Expr, v, fn)
	case *ast.TryExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Inner, v, fn)
	case *ast.RangeExpr:
		if v == nil {
			return
		}

		walkASTParented(v.Start, v, fn)
		walkASTParented(v.End, v, fn)
	case *ast.TaggedBlock:
		if v == nil {
			return
		}

		walkASTParented(v.Body, v, fn)
	}
}
