package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

// -Wunclosed-closeable
//
// A local binding produced by a function whose return type implements
// `io::Closeable` should have `.close()` called somewhere in the function
// body, or be transferred (returned, passed as an argument, stored in a
// struct/array, re-assigned). Otherwise the resource leaks at scope exit.
//
// Limitations (v1, conservative):
//
//   - Best-effort, NOT branch-aware: a single `.close()` anywhere in the
//     function body silences the warning for that binding, even if it only
//     fires on one branch. Use `defer name.close()` for guaranteed coverage.
//   - Only fires for `let name = ...` or `let name type = ...` where the
//     RHS is a call (or an awaited call) resolvable to a top-level FuncDecl
//     whose return type names a struct that implements Closeable. Tuple
//     destructuring (`let (conn, err) = dial(...)`) is intentionally
//     skipped - the err handling pattern in stdlib uses tuples and a
//     stricter check there would generate noise until we extend it.
//   - Treats *any* reference to the name (return, arg, struct/array field,
//     re-assign) as a transfer. Prefers false-negative over false-positive.

// checkAllUnclosedCloseables runs the unclosed-Closeable warning over every
// top-level FuncDecl and struct method. No-op if the user has suppressed
// -Wunclosed-closeable, or no struct in the program implements Closeable.
func (cg *CodeGen) checkAllUnclosedCloseables(prog *ast.Program) {
	if cg.diagSuppressed(DiagUnclosedCloseable) {
		return
	}

	closeable := cg.collectCloseableStructNames()
	if len(closeable) == 0 {
		return
	}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			cg.checkUnclosedInFunc(v, closeable)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.checkUnclosedInFunc(m, closeable)
			}
		}
	}
}

// collectCloseableStructNames builds the set of (bare) struct names known
// to implement the Closeable trait. Both the package-prefixed key
// (`tls__TlsConn`) and the bare tail (`TlsConn`) are recorded so call-site
// type-name matching works regardless of whether the user qualified the
// type or imported it directly.
//
// Note: rc-cell-backed Closeable types (the default in stdlib) auto-clean
// at last drop, so explicit .close() is genuinely optional for them. The
// warning is opt-in via -Wpedantic for codebases that want explicit close
// discipline regardless.
func (cg *CodeGen) collectCloseableStructNames() map[string]bool {
	out := map[string]bool{}

	// Track which bare suffixes are claimed by which qualified name(s).
	// If two packages contribute the same bare name (e.g. net::Conn and
	// db::Conn) and only one is Closeable, the bare lookup must be
	// disambiguated -- inserting it into `out` blindly would flip
	// across builds because Go map iteration is randomized.  Resolve:
	// only insert the bare suffix when it is unambiguous (claimed by
	// at most one qualified Closeable, AND no other struct of the
	// same bare name exists that does NOT implement Closeable).
	bareCloseable := map[string]int{} // bare -> #closeable owners
	bareTotal := map[string]int{}     // bare -> #total owners

	for key, sd := range cg.structDeclsByName {
		if sd == nil {
			continue
		}

		hasCloseable := false

		for _, impl := range sd.Implements {
			if impl == nil {
				continue
			}

			if traitBaseName(impl) == "Closeable" {
				hasCloseable = true

				break
			}
		}

		bare := key
		if idx := strings.LastIndex(key, "__"); idx >= 0 {
			bare = key[idx+2:]
		}

		bareTotal[bare]++

		if hasCloseable {
			out[key] = true
			bareCloseable[bare]++
		}
	}

	for bare, n := range bareCloseable {
		if n == 1 && bareTotal[bare] == 1 {
			out[bare] = true
		}
	}

	return out
}

// checkUnclosedInFunc scans one function body for Closeable-typed let
// bindings and emits a warning for each one that's neither closed nor
// transferred anywhere in the body.
func (cg *CodeGen) checkUnclosedInFunc(fn *ast.FuncDecl, closeable map[string]bool) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	body, ok := fn.Body.(*ast.Block)
	if !ok {
		return
	}

	type binding struct {
		name string
		pos  ast.Pos
		typ  string
	}

	var bindings []binding

	walkAST(body, func(n ast.Node) {
		vd, ok := n.(*ast.VarDecl)
		if !ok || vd.Name == "_" || vd.Name == "" {
			return
		}

		typeName := cg.bindingCloseableType(vd, closeable)
		if typeName == "" {
			return
		}

		bindings = append(bindings, binding{name: vd.Name, pos: vd.Pos(), typ: typeName})
	})

	if len(bindings) == 0 {
		return
	}

	for _, b := range bindings {
		if cg.bindingClosedOrTransferred(body, b.name) {
			continue
		}

		cg.warn(DiagUnclosedCloseable, b.pos,
			"binding %q (%s) is never closed; add `defer %s.close()` or close on every exit path",
			b.name, b.typ, b.name)
	}
}

// bindingCloseableType returns the bare struct name when vd binds a value
// whose static type implements Closeable, otherwise "". The annotation on
// the let takes priority; without one, the callee's declared return type
// is consulted (peeling Future[T] for awaited calls).
func (cg *CodeGen) bindingCloseableType(vd *ast.VarDecl, closeable map[string]bool) string {
	if vd.Type != nil {
		if name := typeExprStructName(vd.Type); closeable[name] {
			return name
		}
	}

	if vd.Value == nil {
		return ""
	}

	call, ok := unwrapAwait(vd.Value).(*ast.CallExpr)
	if !ok {
		return ""
	}

	fd := cg.resolveCalleeDecl(call)
	if fd == nil || fd.RetType == nil {
		return ""
	}

	ret := fd.RetType
	if gt, ok := ret.(*ast.GenericType); ok && gt.Name == "Future" && len(gt.TypeParams) == 1 {
		ret = gt.TypeParams[0]
	}

	if name := typeExprStructName(ret); closeable[name] {
		return name
	}

	return ""
}

// resolveCalleeDecl tries to locate the FuncDecl backing a direct or
// qualified call expression. Best-effort; returns nil for higher-order
// calls and method calls we can't pin down.
func (cg *CodeGen) resolveCalleeDecl(c *ast.CallExpr) *ast.FuncDecl {
	switch fn := c.Func.(type) {
	case *ast.Identifier:
		for _, d := range cg.funcDecls {
			if d != nil && d.Name == fn.Name {
				return d
			}
		}
	case *ast.ScopeAccess:
		if len(fn.Path) == 0 {
			return nil
		}

		bare := fn.Path[len(fn.Path)-1]
		for _, d := range cg.funcDecls {
			if d != nil && d.Name == bare {
				return d
			}
		}
	}

	return nil
}

// unwrapAwait peels an `await x` wrapper to expose the underlying call.
func unwrapAwait(n ast.Node) ast.Node {
	if aw, ok := n.(*ast.AwaitExpr); ok {
		return aw.Future
	}

	return n
}

// typeExprStructName returns the bare struct name from a type expression,
// stripping any package qualifier (`tls::TlsConn` -> `TlsConn`). Returns
// "" for non-named types.
func typeExprStructName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	case *ast.GenericType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	}

	return ""
}

// bindingClosedOrTransferred returns true when the function body either
// calls `.close()` on the binding or "transfers" the binding away
// (returns it, passes it to a function, stores it in a struct/array, or
// re-assigns the name to a new value). Any of these silences the warning.
func (cg *CodeGen) bindingClosedOrTransferred(blk *ast.Block, name string) bool {
	found := false

	walkAST(blk, func(n ast.Node) {
		if found {
			return
		}

		switch v := n.(type) {
		case *ast.CallExpr:
			if recv, method, ok := extractMethodCall(v); ok && recv == name && method == "close" {
				found = true

				return
			}

			for _, a := range v.Args {
				if id, ok := a.(*ast.Identifier); ok && id.Name == name {
					found = true

					return
				}
			}
		case *ast.ReturnStmt:
			if v.Value != nil && exprReferencesName(v.Value, name) {
				found = true
			}
		case *ast.StructLit:
			for _, f := range v.Fields {
				if id, ok := f.Value.(*ast.Identifier); ok && id.Name == name {
					found = true

					return
				}
			}

			for _, p := range v.Positional {
				if id, ok := p.(*ast.Identifier); ok && id.Name == name {
					found = true

					return
				}
			}
		case *ast.ArrayLit:
			for _, el := range v.Elems {
				if id, ok := el.(*ast.Identifier); ok && id.Name == name {
					found = true

					return
				}
			}
		case *ast.AssignStmt:
			if id, ok := v.Target.(*ast.Identifier); ok && id.Name == name {
				found = true

				return
			}
			// `self.handle = f` (field store) is also a transfer --
			// the surrounding aggregate now holds the Closeable, so
			// flagging the let binding would be a false positive.
			// Match identifier on either side: target is a field
			// access whose value-side mentions name, OR the value
			// references name in any expression position.
			if exprReferencesName(v.Value, name) {
				found = true
			}
		}
	})

	return found
}

// exprReferencesName reports whether expr contains a bare reference to name.
func exprReferencesName(expr ast.Node, name string) bool {
	found := false

	walkAST(expr, func(n ast.Node) {
		if found {
			return
		}

		if id, ok := n.(*ast.Identifier); ok && id.Name == name {
			found = true
		}
	})

	return found
}
