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

	isFutureLike := isFutureType(fd.RetType) || cg.isAwaitableType(fd.RetType)
	isResult := isResultType(fd.RetType)

	// `#allow_drop` opts out of the "did you forget `await`?" warning
	// for fire-and-forget Future returns -- channel.send is the
	// motivating case.  It MUST NOT silence the Result-discard warning:
	// dropping an unobserved error is a different and dangerous bug,
	// and a future maintainer who tags an `#allow_drop` fn returning
	// Result should not get the error-discard check turned off behind
	// their back.
	if hasTag(fd.Tags, "allow_drop") && isFutureLike && !isResult {
		return false
	}

	return isResult || isFutureLike
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
		// For qualified calls like `time::sleep`, prefer the
		// package-qualified entry so a user-defined `fn sleep()` at
		// top level doesn't shadow the imported `time::sleep`.  Try
		// `<pkg>__<bare>` first, then fall through to the bare lookup
		// below as a last resort.
		if len(fn.Path) >= 2 {
			pkg := fn.Path[len(fn.Path)-2]

			if d, ok := cg.funcDecls[pkg+"__"+bare]; ok && d != nil {
				return d
			}

			keys := make([]string, 0, len(cg.funcDecls))
			for k := range cg.funcDecls {
				keys = append(keys, k)
			}

			sort.Strings(keys)

			for _, k := range keys {
				d := cg.funcDecls[k]
				if d != nil && d.Name == bare && strings.HasPrefix(k, pkg+"__") {
					return d
				}
			}
		}
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
		// Skip the suffix scan when the bare name doesn't start with
		// an ASCII uppercase letter.  Tin's PascalCase convention
		// guarantees struct names start uppercase; lowercase bares
		// like `i64`, `byte`, `bool`, `string` can only be scalar /
		// builtin types.  Without this gate, a query for `i64` would
		// match monomorphised generics such as `Future__i64` and
		// falsely classify i64 as Awaitable, which then fires the
		// "future returned by ..." must-use warning for any
		// i64-returning libc extern (fwrite, read, write, ...).
		if len(bare) > 0 && (bare[0] < 'A' || bare[0] > 'Z') {
			// No struct can have this bare name; bail out.
		} else {
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
	}

	if decl == nil {
		return false
	}

	for _, impl := range decl.Implements {
		traitName := traitBaseName(impl)
		if idx := strings.LastIndex(traitName, "::"); idx >= 0 {
			traitName = traitName[idx+2:]
		}

		if traitName == "awaitable" {
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
			cg.checkAllowDropTag(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.checkUnusedInFunc(m)
				cg.checkAllowDropTag(m)
			}
		}
	}

	cg.checkUnusedImports(prog)
}

// checkAllowDropTag warns when `#allow_drop` is applied to a function
// whose return type is not must-use (Result / Future / Awaitable).
// The tag only suppresses the -Wmust-use diagnostic; on regular
// i64- / void- / struct-returning fns it has no effect, so its
// presence is dead weight at best and a misunderstanding of the tag
// at worst.
func (cg *CodeGen) checkAllowDropTag(fd *ast.FuncDecl) {
	if fd == nil || !hasTag(fd.Tags, "allow_drop") {
		return
	}

	if fd.RetType != nil {
		if isResultType(fd.RetType) || isFutureType(fd.RetType) || cg.isAwaitableType(fd.RetType) {
			return
		}
	}

	cg.warn(DiagIneffectiveAllowDrop, fd.Pos(),
		"`#allow_drop` on `%s` has no effect: the function does not "+
			"return a must-use value (Result, Future, or Awaitable). "+
			"Remove the tag, or change the return type to the value "+
			"callers were meant to drop.",
		fd.Name)
}

// checkUnusedImports warns for `use pkg` / `use { name } from pkg` /
// `use "./file"` declarations whose imported names are never referenced
// anywhere else in the program. Skipped in REPL mode where each cell sees
// only its own statements - a `use` in cell N legitimately gets used in
// isNoParensMacroName reports whether name resolves to a #no_parens
// macro imported from pkgPath.  Used to suppress the unused-import
// warning for macros whose invocation site is erased by token
// substitution before the AST walk sees it.  Selective imports of
// macros that don't end in `!` route through loadPackageSelective's
// "function" branch and never get re-bound as a bare macro key, so we
// fall back to the pkg-qualified entry registered by the loader.
func (cg *CodeGen) isNoParensMacroName(name, pkgPath string) bool {
	// Bare `name` keys would over-match: a `use { foo } from regular`
	// where the importer happens to share a name with an unrelated
	// no_parens macro foo from another package would silently
	// suppress the unused-import warning.  Require a pkg-prefixed hit
	// so the check is anchored to the import we're actually looking
	// at.
	if pkgPath == "" {
		return false
	}

	parts := strings.Split(pkgPath, "::")
	short := parts[len(parts)-1]

	candidates := []string{
		pkgPath + "::" + name, pkgPath + "::" + name + "!",
		pkgPath + "." + name, pkgPath + "." + name + "!",
		short + "::" + name, short + "::" + name + "!",
		short + "." + name, short + "." + name + "!",
	}

	for _, key := range candidates {
		if m, ok := cg.macros[key]; ok && macroHasTag(m, "no_parens") {
			return true
		}
	}

	return false
}

// cell N+1.
//
// Also runs the redundant-import-prefix check: if a file does
// `use net::dns` (binding the short alias `dns`) but then writes
// `net::dns::lookup_host`, suggest the short form.
func (cg *CodeGen) checkUnusedImports(prog *ast.Program) {
	if cg.replMode {
		return
	}

	// nestedImportAliases maps the *prefix tuple* of a nested import
	// (e.g. ["net", "dns"]) to its bare alias ("dns").  Populated from
	// `use pkg::nested` / `use pkg::nested::deep` declarations and
	// consulted on every ScopeAccess so we can warn when the file
	// reaches for the symbol via the long prefix.  Joined with "::" so
	// we can compare against ScopeAccess path prefixes cheaply.
	nestedImportAliases := map[string]string{}

	// selectiveOnlyPkgNames maps a package path to the set of names that
	// were brought into scope via `use { X, Y } from pkg`.  Populated
	// only when the same file did NOT also `use pkg` -- in that case
	// the package-level alias is intentionally available and writing
	// `pkg::X` is not redundant.
	pkgImported := map[string]bool{}
	selectiveBindings := map[string]map[string]bool{} // pkg -> set of names

	for _, n := range prog.Stmts {
		ud, ok := n.(*ast.UseDecl)
		if !ok || ud.IsExtern || ud.IsFile {
			continue
		}

		if ud.FromSyntax {
			// Selective: `use { X, Y } from pkg`.
			if selectiveBindings[ud.Path] == nil {
				selectiveBindings[ud.Path] = map[string]bool{}
			}

			for _, name := range ud.Names {
				selectiveBindings[ud.Path][strings.TrimSuffix(name, "!")] = true
			}

			continue
		}

		pkgImported[ud.Path] = true

		if !strings.Contains(ud.Path, "::") {
			continue
		}

		nestedImportAliases[ud.Path] = importBaseName(ud.Path)
	}

	// shadowedNames tracks selective imports for packages that the user
	// ALSO imported plainly (`use pkg; use { X } from pkg`).  In that
	// case writing `pkg::X` works but is redundant -- X is already in
	// scope as a bare name -- so warn and point the user at the short
	// form.  The hard error for selective-only imports lives in
	// checkSelectiveImportQualifiers (codegen/resolve.go); this map
	// only covers the both-imported case.
	shadowedNames := map[string]map[string]bool{}

	for pkg, names := range selectiveBindings {
		if !pkgImported[pkg] {
			continue
		}

		shadowedNames[pkg] = names
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
			// Mark every path segment as used.  A call to
			// `net::dns::lookup_host` is a usage of BOTH `net` (in case
			// the file did `use net`) AND `dns` (in case the file did
			// `use net::dns`).  Marking only the first segment would
			// falsely warn that `dns` is unused for the second form.
			//
			// Generic-type method calls fold the type-arg list into a
			// path segment, e.g. ScopeAccess{Path: ["pkg::T[U]", "m"]};
			// split each segment on "::" so the import-root and any
			// inner namespace names are recovered.
			for _, seg := range v.Path {
				if idx := strings.Index(seg, "::"); idx >= 0 {
					used[seg[:idx]] = true
				} else {
					used[seg] = true
				}
			}
			// Redundant-import-prefix: if any prefix of this path's
			// leading namespace segments matches a `use pkg::sub`
			// alias the file already bound, point the user at the
			// shorter form.  The check only fires for path lengths
			// strictly greater than the alias depth so we don't
			// flag the alias usage itself.
			for prefixLen := 2; prefixLen < len(v.Path); prefixLen++ {
				joined := strings.Join(v.Path[:prefixLen], "::")

				alias, ok := nestedImportAliases[joined]
				if !ok {
					continue
				}

				shortPath := alias + "::" + strings.Join(v.Path[prefixLen:], "::")
				longPath := strings.Join(v.Path, "::")

				cg.warn(DiagRedundantImportPrefix, v.Pos(),
					"`%s` already binds `%s`; write `%s` instead of `%s`",
					joined, alias, shortPath, longPath)

				break
			}
			// Selective-shadow redundant-prefix: when the file did
			// `use { X } from pkg` (selective only -- no plain
			// `use pkg`), writing `pkg::X` reaches through a qualifier
			// the file did not opt into.  Suggest the bare form.  Only
			// fires when the trailing name was actually one of the
			// selective imports; unrelated `pkg::Y` references still
			// surface the usual "package not imported" error from the
			// resolver instead.
			if len(v.Path) >= 2 {
				// Selective-shadow redundant-prefix.  The path can have
				// arbitrary depth -- `pkg::Adt::method`,
				// `pkg::sub::Adt::Ctor`, `pkg::sub::nested::fn`, or
				// `pkg::Adt[T,U]::method` (where the [T,U] sometimes
				// gets folded into the leading segment).  Normalize
				// into a flat list of bare segments and try every
				// prefix as the candidate package path.
				flat := flattenScopeAccessSegments(v.Path)

				warned := false

				for k := 1; k < len(flat) && !warned; k++ {
					pkgPath := strings.Join(flat[:k], "::")

					names, has := shadowedNames[pkgPath]
					if !has {
						continue
					}
					// Look one segment past the prefix -- that is what
					// the user typically imported (the type, ctor, or
					// fn name).  Suggest rewriting with that bare name.
					candidate := flat[k]
					if !names[candidate] {
						continue
					}

					longPath := strings.Join(v.Path, "::")
					shortPath := strings.Join(append([]string{candidate}, flat[k+1:]...), "::")

					cg.warn(DiagRedundantImportPrefix, v.Pos(),
						"`use { %s } from %s` already binds `%s`; write `%s` instead of `%s`",
						candidate, pkgPath, candidate, shortPath, longPath)

					warned = true
				}
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
				if used[name] {
					continue
				}
				// #no_parens macros are erased by token-substitution in
				// the parser before the AST is walked, so a real
				// invocation leaves no trace for `used[name]` to catch.
				// Suppress the warning when the import resolves to one.
				if cg.isNoParensMacroName(name, ud.Path) {
					continue
				}

				cg.warn(DiagUnusedImport, ud.Pos(),
					"imported name %q is never used", name)
			}

			continue
		}

		// `use "./file"` flat-imports every exported symbol into the
		// current scope under its bare name -- there is no namespace
		// alias to reference, so a "is the base name used?" check
		// would always fail.  Skip the unused diagnostic for this
		// import shape; the symbol-level unused checks above already
		// catch dead code referenced through the file import.
		if ud.IsFile {
			continue
		}

		// `use pkg` brings the package handle into scope under its
		// base name (last `::` segment).
		base := importBaseName(ud.Path)
		if base == "" || used[base] {
			continue
		}

		cg.warn(DiagUnusedImport, ud.Pos(),
			"import %q is never used", base)
	}
}

// flattenScopeAccessSegments splits each segment of a ScopeAccess path on
// `::` (since the parser sometimes folds `pkg::Adt[T]` into one segment
// to keep the type-arg list attached to its name) and strips any trailing
// `[T,U]` type-arg suffix from each piece.  Returns the flat list of bare
// identifier segments, which is what the redundant-import-prefix walk
// needs to try every possible package-prefix split without caring about
// how deep the namespace nesting is.
func flattenScopeAccessSegments(path []string) []string {
	flat := make([]string, 0, len(path))

	for _, seg := range path {
		for _, piece := range strings.Split(seg, "::") {
			if i := strings.IndexByte(piece, '['); i >= 0 {
				piece = piece[:i]
			}

			flat = append(flat, piece)
		}
	}

	return flat
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
