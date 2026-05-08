package codegen

import (
	"sort"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// isVoidType reports whether t is a void or zero-bit return type. Calls
// returning such a type are not subject to the discarded-result warning.
func isVoidType(t irtypes.Type) bool {
	if t == nil {
		return true
	}

	if _, ok := t.(*irtypes.VoidType); ok {
		return true
	}
	// `Unit` is sometimes lowered to {} or i1 that's never read.
	if st, ok := t.(*irtypes.StructType); ok && len(st.Fields) == 0 {
		return true
	}

	return false
}

// isCalleePure reports whether a CallExpr's target function is tagged
// `#pure`. Pure calls have no observable side effects, so discarding their
// result is always a mistake (the call could be deleted entirely).
func (cg *CodeGen) isCalleePure(c *ast.CallExpr) bool {
	id, ok := c.Func.(*ast.Identifier)
	if !ok {
		return false
	}
	// Built-ins like len/sizeof are inherently pure.
	if isPureBuiltin(id.Name) {
		return true
	}

	for _, fd := range cg.funcDecls {
		if fd.Name == id.Name && hasTag(fd.Tags, "pure") {
			return true
		}
	}

	return false
}

// calleeReturnsMustUse reports whether the callee's declared return type is
// a `Result[t, e]` (the canonical must-use type in Tin). When true, dropping
// the result silently elides the error path.
//
// Best-effort: only matches when the callee can be resolved to a FuncDecl by
// bare name or qualified scope path. Higher-order calls and method calls on
// values whose decl can't be located fall through to the default-off
// -Wunused-result.
func (cg *CodeGen) calleeReturnsMustUse(c *ast.CallExpr) bool {
	fd := cg.resolveCalleeFuncDecl(c)
	if fd == nil || fd.RetType == nil {
		return false
	}

	// Functions that opt out of the "must await / must use" rule via
	// the `#allow_drop` tag are exempt regardless of return type.
	// Channel send is the motivating case: posting a value to a
	// channel is the canonical fire-and-forget pattern, and warning
	// on every call site would force `let _ = ...` boilerplate that
	// drowns out the useful "you forgot await" cases on functions
	// like time::sleep.
	if hasTag(fd.Tags, "allow_drop") {
		return false
	}

	return isResultType(fd.RetType) || isFutureType(fd.RetType) || cg.isAwaitableType(fd.RetType)
}

// mustUseMessage formats the discarded-result warning so the message
// names both the call site and the kind of value being thrown away,
// and points the user at the right fix.  Result -> "handle the
// error", Future / Awaitable -> "did you forget `await`?".
func (cg *CodeGen) mustUseMessage(c *ast.CallExpr) string {
	name := callDisplayName(c)
	fd := cg.resolveCalleeFuncDecl(c)

	if fd != nil && fd.RetType != nil {
		switch {
		case isFutureType(fd.RetType) || cg.isAwaitableType(fd.RetType):
			return "the future returned by `" + name +
				"` is dropped without `await`; the work runs in the " +
				"background but the caller does not wait for it. " +
				"Write `await " + name + "(...)` to wait, or " +
				"`let _ = " + name + "(...)` to silence."
		case isResultType(fd.RetType):
			return "the Result returned by `" + name +
				"` is dropped without inspection; handle the error " +
				"with a match / unwrap / try, or `let _ = " + name +
				"(...)` to silence."
		}
	}

	return "the result of `" + name + "` is discarded; bind with " +
		"`let _ = ...` to silence."
}

// resolveCalleeFuncDecl looks up the FuncDecl behind a CallExpr in a
// deterministic way -- iterating cg.funcDecls directly with a "first
// matching .Name" pick is order-dependent (Go map iteration is
// randomized) and would make the warning's text and gate flicker
// build-to-build when overloads share a bare name.  We instead try
// exact-key hits first, then fall back to a sorted-key suffix scan
// so multiple builds always pick the same decl.
func (cg *CodeGen) resolveCalleeFuncDecl(c *ast.CallExpr) *ast.FuncDecl {
	bare := ""

	switch fn := c.Func.(type) {
	case *ast.Identifier:
		bare = fn.Name
	case *ast.ScopeAccess:
		if len(fn.Path) == 0 {
			return nil
		}

		bare = fn.Path[len(fn.Path)-1]
	default:
		return nil
	}

	if d, ok := cg.funcDecls[bare]; ok && d != nil {
		return d
	}

	keys := make([]string, 0, len(cg.funcDecls))
	for k := range cg.funcDecls {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		d := cg.funcDecls[k]
		if d != nil && d.Name == bare {
			return d
		}
	}

	return nil
}

// isResultType reports whether te names the Result ADT (with or without a
// package qualifier). Generic args don't matter: any Result[t, e] qualifies.
func isResultType(te ast.TypeExpr) bool {
	return typeNameMatches(te, "Result")
}

// isFutureType reports whether te names a Future (sync::Future[t] or any
// other module's Future).  Dropping a Future without awaiting spawns a
// background fiber whose work happens but whose result is lost; the
// caller almost always wants `await fn(...)` instead.
func isFutureType(te ast.TypeExpr) bool {
	return typeNameMatches(te, "Future")
}

// isAwaitableType reports whether te names a struct that implements
// the Awaitable[T] trait.  Lazy futures (e.g. time::SleepFuture) sit
// outside the canonical `Future` shape but still represent deferred
// work that needs `await` to actually run; flag a dropped result so
// the user gets the same diagnostic as for plain Futures.
func (cg *CodeGen) isAwaitableType(te ast.TypeExpr) bool {
	name := simpleTypeName(te)
	if name == "" {
		if gt, ok := te.(*ast.GenericType); ok {
			name = gt.Name
		}
	}

	if name == "" {
		return false
	}
	// structDeclsByName is keyed by the canonical (module-prefixed)
	// struct name -- e.g. `time__SleepFuture`.  Try the user-written
	// form first, then map `pkg::Name` -> `pkg__Name`, then sweep
	// every key whose suffix matches the bare name (covers the
	// common "user wrote `SleepFuture` but the decl lives in
	// `time__SleepFuture`" case without doing a full import-trail
	// resolution).
	bare := name
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		bare = name[idx+2:]
	}

	mangled := strings.ReplaceAll(name, "::", "__")

	var decl *ast.StructDecl

	if d, ok := cg.structDeclsByName[name]; ok && d != nil {
		decl = d
	} else if d, ok := cg.structDeclsByName[mangled]; ok && d != nil {
		decl = d
	} else if d, ok := cg.structDeclsByName[bare]; ok && d != nil {
		decl = d
	} else {
		// Last resort: scan for a key that ends in `__<bare>`.
		// Two structs in different packages can share a bare name
		// (`pkgA::Future` vs `pkgB::Future`), so we must NOT pick
		// one arbitrarily -- map iteration order is non-
		// deterministic and would make the warning flicker
		// build-to-build.  Collect every match; only fall through
		// to the await-able check when exactly one survives.
		suffix := "__" + bare

		var matches []*ast.StructDecl

		for k, d := range cg.structDeclsByName {
			if strings.HasSuffix(k, suffix) {
				matches = append(matches, d)
			}
		}

		if len(matches) == 1 {
			decl = matches[0]
		}
	}

	if decl == nil {
		return false
	}

	for _, impl := range decl.Implements {
		traitName := traitBaseName(impl)
		if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
			traitName = traitName[idx+2:]
		}

		if traitName == "Awaitable" {
			return true
		}
	}

	return false
}

// typeNameMatches checks whether the bare (module-stripped) name of te
// equals want.  Used by both the Result and Future #must_use checks.
func typeNameMatches(te ast.TypeExpr, want string) bool {
	switch t := te.(type) {
	case *ast.GenericType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name == want
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name == want
	}

	return false
}

// callDisplayName returns a short human-readable description of a call site
// for use in diagnostic messages.
func callDisplayName(c *ast.CallExpr) string {
	switch fn := c.Func.(type) {
	case *ast.Identifier:
		return fn.Name
	case *ast.FieldAccess:
		return fn.Field
	case *ast.ScopeAccess:
		// Module / type-qualified call like `time::sleep`: rejoin the
		// path so the warning shows the user-written form instead of
		// the opaque `<call>` placeholder.
		return strings.Join(fn.Path, "::")
	}

	return "<call>"
}

// checkAllUnused walks every top-level FuncDecl (including struct methods)
// and emits unused-let / unused-param warnings for names that are never
// read in the body. Default-off; gated by -W<name>, -Wall, -Wpedantic.
//
// Also runs the unused-import scan over top-level UseDecls, which is
// default-on (a stale `use` is essentially dead weight in the file).
//
// All scans are no-ops in REPL mode: each cell only sees its own
// statements, so a `let` or `use` defined in one cell looks unused even
// when later cells will reference it.
func (cg *CodeGen) checkAllUnused(prog *ast.Program) {
	if cg.replMode {
		return
	}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			cg.checkUnusedInFunc(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.checkUnusedInFunc(m)
			}
		}
	}

	cg.checkUnusedImports(prog)
}

// checkUnusedImports warns for `use pkg` / `use { name } from pkg` /
// `use "./file"` declarations whose imported names are never referenced
// anywhere else in the program. Skipped in REPL mode where each cell sees
// only its own statements - a `use` in cell N legitimately gets used in
// cell N+1.
func (cg *CodeGen) checkUnusedImports(prog *ast.Program) {
	if cg.replMode {
		return
	}

	// Collect every name referenced anywhere - identifiers, scope-access
	// roots (pkg::), field-access roots (pkg.), and type expressions
	// embedded in fn signatures, let declarations, and struct fields
	// (so `fn show(v decimal::Value)` counts decimal as used).
	used := map[string]bool{}

	var visitType func(t ast.TypeExpr)

	visitType = func(t ast.TypeExpr) {
		if t == nil {
			return
		}

		switch tt := t.(type) {
		case *ast.SimpleType:
			name := tt.Name
			if idx := strings.Index(name, "::"); idx >= 0 {
				name = name[:idx]
			}

			used[name] = true
		case *ast.GenericType:
			name := tt.Name
			if idx := strings.Index(name, "::"); idx >= 0 {
				name = name[:idx]
			}

			used[name] = true

			for _, p := range tt.TypeParams {
				visitType(p)
			}
		case *ast.PointerType:
			visitType(tt.Elem)
		case *ast.ArrayType:
			visitType(tt.Elem)
		case *ast.FuncType:
			for _, p := range tt.Params {
				visitType(p)
			}

			visitType(tt.RetType)
		case *ast.UnionTypeExpr:
			for _, p := range tt.Types {
				visitType(p)
			}
		}
	}

	visit := func(n ast.Node) {
		switch v := n.(type) {
		case *ast.Identifier:
			used[v.Name] = true
		case *ast.ScopeAccess:
			if len(v.Path) > 0 {
				// Generic-type method calls fold the type-arg list into the
				// first path segment, e.g. ScopeAccess{Path: ["pkg::T[U]",
				// "method"]}. Split on "::" to recover the import root.
				root := v.Path[0]
				if idx := strings.Index(root, "::"); idx >= 0 {
					root = root[:idx]
				}

				used[root] = true
			}
		case *ast.FieldAccess:
			if id, ok := v.Expr.(*ast.Identifier); ok {
				used[id.Name] = true
			}
		case *ast.FuncDecl:
			for _, p := range v.Params {
				visitType(p.Type)
			}

			visitType(v.RetType)
		case *ast.LambdaExpr:
			for _, p := range v.Params {
				visitType(p.Type)
			}

			visitType(v.RetType)
		case *ast.VarDecl:
			visitType(v.Type)
		case *ast.StructDecl:
			for _, f := range v.Fields {
				visitType(f.Type)
			}
		case *ast.AsExpr:
			visitType(v.Type)
		case *ast.IsExpr:
			visitType(v.Type)
		}
	}

	for _, n := range prog.Stmts {
		walkAST(n, visit)
	}

	for _, n := range prog.Stmts {
		ud, ok := n.(*ast.UseDecl)
		if !ok || ud.IsExtern {
			continue
		}

		if ud.FromSyntax && len(ud.Names) > 0 {
			// `use { a, b } from pkg`: each name lands in scope directly.
			for _, name := range ud.Names {
				if !used[name] {
					cg.warn(DiagUnusedImport, ud.Pos(),
						"imported name %q is never used", name)
				}
			}

			continue
		}

		// `use pkg` / `use "./file"` brings the package handle into scope
		// under its base name (last `::` or `/` segment).
		base := importBaseName(ud.Path)
		if base == "" || used[base] {
			continue
		}

		cg.warn(DiagUnusedImport, ud.Pos(),
			"import %q is never used", base)
	}
}

// importBaseName returns the bare identifier under which a `use` declaration
// is referenced. For `use io` it's "io"; for `use std::math` it's "math";
// for `use "./foo/bar"` it's "bar".
func importBaseName(path string) string {
	clean := path
	if i := lastIndexAny(clean, "/:"); i >= 0 {
		clean = clean[i+1:]
	}

	return clean
}

func lastIndexAny(s, chars string) int {
	out := -1

	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] && i > out {
				out = i
			}
		}
	}

	return out
}

func (cg *CodeGen) checkUnusedInFunc(fn *ast.FuncDecl) {
	if fn.Body == nil {
		return
	}
	// Externs and virtual decls have no body to scan.
	if fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Collect every identifier referenced anywhere in the body. This
	// over-approximates "use" - both reads and writes count - so a binding
	// that is only ever assigned to and never read still avoids the warning.
	// A stricter "never read" check would require scope-aware tracking that
	// the simple AST walker can't model in the presence of shadowing.
	used := map[string]bool{}

	walkAST(fn.Body, func(n ast.Node) {
		if id, ok := n.(*ast.Identifier); ok {
			used[id.Name] = true
		}
	})

	for _, p := range fn.Params {
		if p.Name == "" || p.Name == "_" || p.Name == "this" {
			continue
		}

		if used[p.Name] {
			continue
		}

		cg.warn(DiagUnusedParam, fn.Pos(),
			"parameter %q is never read; rename to `_` if intentional", p.Name)
	}

	walkAST(fn.Body, func(n ast.Node) {
		v, ok := n.(*ast.VarDecl)
		if !ok {
			return
		}

		if v.Name == "" || v.Name == "_" || v.IsConst {
			return
		}

		if used[v.Name] {
			return
		}

		cg.warn(DiagUnusedLet, v.Pos(),
			"let-binding %q is never read; rename to `_` if intentional", v.Name)
	})
}
