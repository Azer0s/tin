package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

func (cg *CodeGen) loadPackageFromSource(pkgPath, pkgName, srcPath string) error {
	// Dedup by absolute source path. The caller-side `cg.importedPkgs`
	// map keys on the import path string, which differs between
	// `use net::tcp` ("net::tcp"), `use "./tcp/tcp"` ("file:<path>"),
	// and `use tcp` ("tcp") even when all three resolve to the same
	// .tin file. Without this guard the same package source gets
	// compiled twice into the LLVM module, causing function and
	// struct redefinition errors. Tracked separately from importedPkgs
	// so the macro CTFE shell, which iterates importedPkgs to emit
	// `use <pkg>` lines, never sees raw file paths.
	absPath, absErr := filepath.Abs(srcPath)
	if absErr == nil {
		if cg.loadedSrcPaths[absPath] {
			return nil
		}

		cg.loadedSrcPaths[absPath] = true
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("use %s: read source: %w", pkgPath, err)
	}

	l := lexer.New(string(src))

	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		return fmt.Errorf("use %s: lex: %w", pkgPath, lexErr)
	}

	p := parser.New(tokens, srcPath)
	// Pre-scan for #no_parens macros imported via `use { name } from pkg` so the
	// parser can substitute them as bare tokens (same pattern as main.go).
	for name, expansion := range ScanImportedNoParensMacros(srcPath, tokens, cg.stdlibBase(), cg.libsRoots) {
		p.RegisterNoParensMacro(name, expansion)
	}

	prog, parseErr := p.Parse()
	if parseErr != nil {
		return fmt.Errorf("use %s: parse: %w", pkgPath, parseErr)
	}

	for _, raw := range p.Warnings() {
		_, _ = fmt.Fprintln(os.Stderr, RenderDiagnostic(raw))
	}

	// Collect exported names from the package.
	exportedNames := map[string]bool{}

	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName == pkgName {
			for _, name := range exp.Names {
				exportedNames[name] = true
			}
		}
	}

	// Process `use` declarations inside the package source first.
	// Record this package source path so callers can scan it for //! directives.
	cg.pkgSrcPaths = append(cg.pkgSrcPaths, srcPath)

	prevFilename := cg.filename
	prevPkg := cg.currentPkg
	prevPkgPath := cg.currentPkgPath
	prevActive := cg.activeMod
	cg.filename = srcPath
	// Set currentPkg so that struct preregistration and genStructDecl produce
	// canonical "pkgName__StructName" keys/IR-names for structs defined in this
	// package (including those brought in via `use "./..."` file-path imports).
	cg.currentPkg = pkgName
	// currentPkgPath is the normalized full path used for typeof() display names
	// (e.g. "encoding::base16" instead of just "base16").
	cg.currentPkgPath = normalizePkgDisplayPath(pkgPath, pkgName)
	// Route IR object creation into this package's per-pkg LLVM module so
	// later (incremental compilation step 2) each pkg can be compiled to
	// its own .o in parallel. Today we still merge everything back into
	// cg.mod at the end of Generate via mergeRoutedPkgMods, so the build
	// pipeline is unchanged - the routing just exercises the per-pkg
	// scaffolding so we can spot bugs before flipping the parallel
	// compile on.
	cg.activeMod = cg.pkgMod(pkgName)

	defer func() {
		cg.currentPkg = prevPkg
		cg.currentPkgPath = prevPkgPath
		cg.activeMod = prevActive
	}()

	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok {
			var loadErr error
			// Route selective `use { Name } from pkg` through
			// loadPackageSelective so the named bindings get
			// re-registered as bare aliases (and bare-type-visibility
			// for the strict-bare-type resolver). Without this,
			// nested packages' selective imports degraded into plain
			// `use pkg` and bare references to Result / Option / ...
			// stopped resolving once the bare-visibility check landed.
			switch {
			case ud.FromSyntax:
				loadErr = cg.loadPackageSelective(ud.Path, ud.Names, ud.IsFile)
			case ud.IsFile:
				loadErr = cg.loadPackageFromFilePath(ud.Path)
			default:
				loadErr = cg.loadPackage(ud.Path)
			}

			if loadErr != nil {
				cg.filename = prevFilename

				return fmt.Errorf("use %s: %w", pkgPath, loadErr)
			}
		}
	}

	// Register package-qualified type aliases for structs and generic structs that
	// were compiled via `use "./..."` file-path imports and appear in the export list.
	// This ensures callers can resolve e.g. sync.Mutex even when sync.tin re-exports
	// the type from a sibling file rather than declaring it inline.
	for name := range exportedNames {
		// Non-generic structs use canonical "pkgName__name" keys.
		canonicalKey := pkgName + "__" + name
		if cg.structTypeFor(CanonKey(canonicalKey)) != nil {
			cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: canonicalKey})
			cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: canonicalKey})
			cg.recordAlias(CanonKey(canonicalKey), pkgName+"::"+name)
		} else if cg.structTypeFor(CanonKey(name)) != nil {
			// Struct was loaded without a package prefix (e.g. from a direct file import
			// that ran before currentPkg was set). Fall back to bare-name alias.
			cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: name})
			cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: name})
			cg.recordAlias(CanonKey(name), pkgName+"::"+name)
		}
		// Generic struct templates are always keyed by bare name.
		if _, ok := cg.genericStructsByArity[name]; ok {
			cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: name})
			cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: name})
			cg.recordAlias(CanonKey(name), pkgName+"::"+name)
		}
	}

	// Push a child scope so internal names don't pollute the caller's scope.
	prevScope := cg.curScope
	cg.curScope = newScope(prevScope)

	// Pass 0.5: preregister struct/enum/type/trait nodes so that later passes can
	// resolve type names inside function signatures and struct field types.
	for _, node := range prog.Stmts {
		switch node.(type) {
		case *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl, *ast.TraitDecl, *ast.UnionDecl, *ast.DataDecl:
			if preErr := cg.preregister(node); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %s: preregister: %w", pkgPath, preErr)
			}
		}
	}

	// Pre-pass 0.8: detect overloaded function names BEFORE Pass 1 so that extern
	// overloads (e.g. fn splat(v f32) / fn splat(v f64)) get mangled IR names.
	// Pass the package name so struct method keys are pkg-qualified, otherwise
	// overloads on stdlib structs (multiple `static fn ::implicit(...)` etc.)
	// would never get marked as overloaded under the package-prefixed scope.
	for name, flag := range scanOverloadedNamesPkg(prog.Stmts, pkgName) {
		cg.overloadedNames[name] = flag
	}

	// Pass 1: compile extern-backed functions first so their names are in scope
	// before non-extern bodies reference them.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern == "" {
			continue
		}

		prefixed := pkgName + "__" + fd.Name
		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			prefixed = overloadMangledName(prefixed, sig)
			// Register in overloads map so call-site overload resolution works.
			paramTypes, ptErr := cg.resolveParamTypes(fd.Params, "")
			if ptErr == nil {
				alreadyHave := false

				for _, existing := range cg.overloads[fd.Name] {
					if existing.irName == prefixed {
						alreadyHave = true

						break
					}
				}

				if !alreadyHave {
					var retType irtypes.Type

					if fd.RetType != nil {
						retType, _ = cg.tinTypeToLLVM(fd.RetType)
					}

					cg.overloads[fd.Name] = append(cg.overloads[fd.Name], &overloadEntry{
						irName:     prefixed,
						paramSig:   sig,
						paramTypes: paramTypes,
						arity:      len(paramTypes),
						returnType: retType,
					})
				}
			}
		}

		if compErr := cg.genFuncDeclAs(fd, prefixed); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: %s: %w", pkgPath, fd.Name, compErr)
		}
		// Also register the bare local name so package bodies can call it.
		if entry, ok2 := cg.curScope.lookup(prefixed); ok2 {
			if _, already := cg.curScope.vars[fd.Name]; !already {
				cg.curScope.set(fd.Name, entry)
			}
			// Propagate to the module (global) scope so on-demand monomorphization
			// of generic structs (which runs at module scope) can find these extern
			// helpers (e.g. _tin_chan_buf_store used by Channel[T] methods).
			// Use moduleScope (root) rather than prevScope so that helpers from
			// nested package imports (e.g. sync/channel loaded inside sync) are
			// visible globally.
			if cg.moduleScope != nil {
				if _, already := cg.moduleScope.vars[fd.Name]; !already {
					cg.moduleScope.set(fd.Name, entry)
				}
			} else if _, already := prevScope.vars[fd.Name]; !already {
				prevScope.set(fd.Name, entry)
			}
		}
	}

	// Pass 1.5a: emit struct LAYOUTS (field types only, no method bodies)
	// and register the type aliases. Method bodies are deferred to Pass 1.5b
	// so that Pass 1.6's ADT layouts see fully-laid-out inner structs.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if compErr := cg.genStructLayout(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: struct %s: %w", pkgPath, sd.Name, compErr)
		}
		// Register type aliases so module-qualified types resolve.
		// For non-generic structs: "sync::Unit" -> SimpleType{Name: "sync__Unit"}.
		// For generic struct templates: "sync::Channel" -> SimpleType{Name: "Channel"} (bare).
		structKey := pkgName + "__" + sd.Name
		if cg.structTypeFor(CanonKey(structKey)) != nil {
			// Non-generic struct with canonical key.
			prevScope.set(pkgName+"::"+sd.Name, &scopeEntry{val: nil, isAlloc: false})
			cg.setTypeAlias(pkgName+"::"+sd.Name, &ast.SimpleType{Name: structKey})
			cg.setTypeAlias(pkgName+"."+sd.Name, &ast.SimpleType{Name: structKey})
			// Always update the bare-name alias to the current package's struct so
			// that intra-package code (pass 3 bodies) resolves the correct type even
			// when multiple packages loaded in the same scope share a type name.
			cg.setTypeAlias(sd.Name, &ast.SimpleType{Name: structKey})
		} else if cg.structTypeFor(CanonKey(sd.Name)) != nil {
			// Struct registered under bare name (e.g. from file-path import before
			// currentPkg was set, or user-level struct).
			cg.setTypeAlias(pkgName+"::"+sd.Name, &ast.SimpleType{Name: sd.Name})
			cg.setTypeAlias(pkgName+"."+sd.Name, &ast.SimpleType{Name: sd.Name})
		} else {
			// Generic struct template (not in structTypes) - use bare name alias.
			cg.setTypeAlias(pkgName+"::"+sd.Name, &ast.SimpleType{Name: sd.Name})
			cg.setTypeAlias(pkgName+"."+sd.Name, &ast.SimpleType{Name: sd.Name})
		}
	}

	// Pre-pass 1.8: detect overloaded function names in this package so that
	// passes 2/2.5/3 can mangle IR names for overloaded functions correctly.
	for name, flag := range scanOverloadedNamesPkg(prog.Stmts, pkgName) {
		cg.overloadedNames[name] = flag
	}

	// Pass 1.6: emit concrete layout for non-generic ADTs defined in this
	// package, AFTER all struct layouts are finalized. See the symmetrical
	// block in loadPackage above for rationale.
	for _, node := range prog.Stmts {
		if dd, ok := node.(*ast.DataDecl); ok {
			if compErr := cg.genDataDecl(dd); compErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %s: data %s: %w", pkgPath, dd.Name, compErr)
			}
		}
	}

	// Pass 1.65: register top-level var declarations as LLVM globals. Must
	// run AFTER Pass 1.5a/1.6 (struct + ADT layouts) so that struct-typed
	// initializers like `const RED = Color{r: 255}` fold against the final
	// LLVM struct type. The previous slot (Pass 0.7, before layouts) folded
	// against a placeholder, producing void* globals and a "store operands
	// not compatible" panic in emitTopLevelVarInits. See invariant #4 in
	// docs/internals/codegen-passes.md.
	for _, node := range prog.Stmts {
		tv, ok := node.(*ast.TopLevelVar)
		if !ok {
			continue
		}

		if err := cg.preregisterPkgTopLevelVar(tv, pkgName, exportedNames, prevScope); err != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: var %s: %w", pkgPath, tv.Name, err)
		}
	}

	// Pass 1.2: mark {#async} struct methods as coro-callable and pre-declare
	// their $coro variants BEFORE genStructMethods generates method bodies.
	// This is needed so that genTraitVtables can build vtable global constants
	// referencing the $coro variants. Placed AFTER Pass 1.6 so that any ADT
	// return types on async methods monomorphise with correct payload size.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok || len(sd.TypeParams) > 0 {
			continue
		}

		for _, m := range sd.Methods {
			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}

			scopeKey := methodScopeName(pkgName+"__"+sd.Name, m)
			cg.coroCallable[scopeKey] = true

			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %s: struct %s method %s coro predecl: %w", pkgPath, sd.Name, m.Name, preErr)
			}
		}
	}

	// Pass 1.4: predeclare non-extern module-level functions so that struct
	// methods compiled in Pass 1.5b can call them. Placed AFTER Pass 1.6 so
	// function signatures referencing ADTs like Result[LocalStruct, Err]
	// monomorphise with the correct payload size.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" || len(fd.TypeParams) > 0 || len(fd.Constraints) > 0 {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
		}

		if preErr := cg.predeclareFuncAs(fd, irName); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: predeclare %s: %w", pkgPath, fd.Name, preErr)
		}

		if _, already := cg.funcDecls[fd.Name]; !already {
			cg.funcDecls[fd.Name] = fd
		}

		if entry, ok2 := cg.curScope.lookup(irName); ok2 {
			if _, already := cg.curScope.vars[fd.Name]; !already {
				cg.curScope.set(fd.Name, entry)
			}
		}
	}

	// Pass 1.45: predeclare $coro variants for module-level {#async} functions
	// before Pass 1.5b compiles struct methods (which may spawn them).
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" || !isAsyncTag(fd.Tags) {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
		}

		cg.coroCallable[irName] = true
		if preErr := cg.predeclareCoroVariant(fd, irName, false); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: early coro predecl %s: %w", pkgPath, fd.Name, preErr)
		}
	}

	// Pass 1.5b: emit struct METHOD bodies now that every struct + ADT
	// layout in this package is final. This ordering is what lets method
	// bodies that use ADTs like Result[LocalStruct, Err] monomorphise with
	// the correct payload size.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if compErr := cg.genStructMethods(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: struct %s methods: %w", pkgPath, sd.Name, compErr)
		}

		structKey := pkgName + "__" + sd.Name
		// Propagate all methods to prevScope so that method calls on values of
		// this struct type (loaded by the caller) can be resolved.
		for _, m := range sd.Methods {
			methodKey := structKey + "_" + m.Name
			if entry, ok2 := cg.curScope.lookup(methodKey); ok2 {
				prevScope.set(methodKey, entry)
			}
			// Propagate $coro variant so that `await obj.method()` works in
			// calling code (directCallHasCoroVariant checks prevScope for the $coro).
			coroKey := methodKey + "$coro"
			if entry, ok2 := cg.curScope.lookup(coroKey); ok2 {
				prevScope.set(coroKey, entry)
			}
			// Also register under bare name for backward compatibility within same scope.
			bareMethodKey := sd.Name + "_" + m.Name
			if bareMethodKey != methodKey {
				if entry, ok2 := cg.curScope.lookup(methodKey); ok2 {
					prevScope.set(bareMethodKey, entry)
				}

				bareCoroKey := bareMethodKey + "$coro"
				if entry, ok2 := cg.curScope.lookup(coroKey); ok2 {
					prevScope.set(bareCoroKey, entry)
				}
			}
		}
	}

	// Pass 2: predeclare non-extern functions (enables mutual recursion).
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}
		// Generic functions (TypeParams) are compiled on demand; register the template.
		if len(fd.TypeParams) > 0 {
			cg.genericFuncs[fd.Name] = fd
			cg.genericFuncOverloads[fd.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[fd.Name], fd)
			cg.genericFuncHomeScopes[fd.Name] = cg.curScope // package scope for bare-name resolution

			// Also register with qualified key so cross-package calls prefer the correct template
			// when multiple packages export identically-named generics (e.g. json and yaml both
			// export encode[T] / parse[T]).
			if pkgName != "" {
				qualKey := pkgName + "__" + fd.Name
				cg.genericFuncs[qualKey] = fd
				cg.genericFuncOverloads[qualKey] = appendGenericFuncOverload(cg.genericFuncOverloads[qualKey], fd)
				cg.genericFuncHomeScopes[qualKey] = cg.curScope
			}

			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] && fd.IsExtern == "" {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
			// Register in overloads map for cross-package call-site resolution.
			paramTypes, ptErr := cg.resolveParamTypes(fd.Params, "")
			if ptErr == nil {
				// Avoid duplicate entries (loadPackageFromSource may be called multiple times).
				alreadyHave := false

				for _, existing := range cg.overloads[fd.Name] {
					if existing.irName == irName {
						alreadyHave = true

						break
					}
				}

				if !alreadyHave {
					var retType irtypes.Type

					if fd.RetType != nil {
						retType, _ = cg.tinTypeToLLVM(fd.RetType)
					}

					cg.overloads[fd.Name] = append(cg.overloads[fd.Name], &overloadEntry{
						irName:     irName,
						paramSig:   sig,
						paramTypes: paramTypes,
						arity:      len(paramTypes),
						returnType: retType,
					})
				}
			}
		}

		if preErr := cg.predeclareFuncAs(fd, irName); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: %s: %w", pkgPath, fd.Name, preErr)
		}
	}

	// Pass 2.5: mark {#async} functions as coro-callable and pre-declare their
	// $coro ramp variants so that Pass 3 bodies can chain-await each other.
	// This must run after Pass 2 (sync predeclarations) and before Pass 3 (bodies),
	// because write_all / read_exact call `await async_write` / `await async_read`
	// and need the $coro handle to be registered first.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" || !isAsyncTag(fd.Tags) {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
		}

		cg.coroCallable[irName] = true
		if preErr := cg.predeclareCoroVariant(fd, irName, false); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: predeclare coro %s: %w", pkgPath, fd.Name, preErr)
		}
	}

	// Pass 2.8: register ALL const declarations in scope so that function
	// bodies compiled in Pass 3 can reference them by name.  This mirrors
	// what Pass 4 does for exported names but covers the full set so that
	// internal helpers (e.g. parse using RFC3339) can access the same values.
	//
	// Both *ast.VarDecl{IsConst:true} (block-level form, retained for
	// historical packages that didn't migrate) and *ast.TopLevelVar
	// {IsConst:true} (the canonical form after the parser routes
	// module-scope const through parseTopLevelLetConst) are accepted.
	registerPkgConst := func(name string, value ast.Node, typ ast.TypeExpr) {
		constVal := cg.evalConstExprTyped(value, typ)
		if constVal == nil {
			return
		}

		stn := ""
		isUnsigned := false

		if typ != nil {
			stn = scalar8BitTypeName(typ)

			if stn == "" {
				stn = scalar128BitTypeName(typ)
			}

			isUnsigned = isUnsignedTinType(typ)
		}

		entry := &scopeEntry{val: constVal, isAlloc: false, scalarTypeName: stn, isUnsigned: isUnsigned}
		cg.curScope.set(name, entry)
	}

	for _, node := range prog.Stmts {
		switch d := node.(type) {
		case *ast.VarDecl:
			if d.IsConst {
				registerPkgConst(d.Name, d.Value, d.Type)
			}
		case *ast.TopLevelVar:
			if d.IsConst {
				registerPkgConst(d.Name, d.Value, d.Type)
			}
		}
	}

	// Pass 3: compile non-extern function bodies.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}
		// Generic functions (TypeParams) are compiled on demand; skip here.
		if len(fd.TypeParams) > 0 {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] && fd.IsExtern == "" {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
		}

		if compErr := cg.genFuncDeclAs(fd, irName); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: %s: %w", pkgPath, fd.Name, compErr)
		}
	}

	// After Pass 3: detect and register any `fn init()` declared in this package.
	// init functions run at program startup after top-level var inits, in
	// dependency order (deps compiled first means deps appended first).
	initIRName := pkgName + "__init"
	for _, f := range cg.mod.Funcs {
		if f.Name() == initIRName && len(f.Params) == 0 {
			cg.pkgInitFns = append(cg.pkgInitFns, f)

			break
		}
	}

	// Pass 4: register exported constants. Accepts both *ast.VarDecl
	// {IsConst:true} (legacy block-form) and *ast.TopLevelVar
	// {IsConst:true} (canonical module-scope form). Simple literals
	// register directly; complex constant expressions (casts, shifts,
	// bitwise-NOT, arithmetic, #pure call results) flow through
	// evalConstExprTyped so that limits like I128_MIN / U128_MAX
	// propagate as inline constants instead of zero-init globals.
	exportPkgConst := func(name string, value ast.Node, typ ast.TypeExpr) {
		if !exportedNames[name] {
			return
		}

		constVal := cg.evalConstExprTyped(value, typ)
		if constVal == nil {
			return
		}

		stn := ""

		isUnsigned := false

		if typ != nil {
			stn = scalar8BitTypeName(typ)

			if stn == "" {
				stn = scalar128BitTypeName(typ)
			}

			isUnsigned = isUnsignedTinType(typ)
		}

		entry := &scopeEntry{val: constVal, isAlloc: false, scalarTypeName: stn, isUnsigned: isUnsigned}
		cg.curScope.set(name, entry)
		prevScope.set(pkgName+"."+name, entry)
		prevScope.set(pkgName+"::"+name, entry)
	}

	for _, node := range prog.Stmts {
		switch d := node.(type) {
		case *ast.VarDecl:
			if d.IsConst {
				exportPkgConst(d.Name, d.Value, d.Type)
			}
		case *ast.TopLevelVar:
			if d.IsConst {
				exportPkgConst(d.Name, d.Value, d.Type)
			}
		}
	}

	// Propagate only exported symbols up to the caller's scope.
	for name := range exportedNames {
		prefixed := pkgName + "__" + name
		if entry, ok2 := cg.curScope.lookup(prefixed); ok2 {
			prevScope.set(pkgName+"."+name, entry)
			prevScope.set(pkgName+"::"+name, entry)
			// Also propagate to moduleScope (root) so that generic function
			// monomorphization (which runs at root scope) can see cross-package symbols.
			if cg.moduleScope != nil && cg.moduleScope != prevScope {
				cg.moduleScope.set(pkgName+"."+name, entry)
				cg.moduleScope.set(pkgName+"::"+name, entry)
			}
		} else {
			// name is a re-exported sub-namespace (e.g. "fnv" exported from hash.tin).
			// The sub-namespace entries were already populated into prevScope when the
			// sub-package's file-path import was processed via loadPackageFromSource.
			// Ensure they are also visible in moduleScope.
			prevScope.each(func(key string, entry *scopeEntry) {
				if strings.HasPrefix(key, name+"::") || strings.HasPrefix(key, name+".") {
					if cg.moduleScope != nil && cg.moduleScope != prevScope {
						cg.moduleScope.set(key, entry)
					}
				}
			})
		}
	}

	// Also propagate $coro variants of exported async functions so that callers
	// can use coroutine chaining (`await pkg::fn(args)`) across package boundaries.
	// tryGenCoroChainCall looks up "bareName$coro" (e.g. "async_read$coro").
	for name := range exportedNames {
		coroKey := pkgName + "__" + name + "$coro"
		if se, ok2 := cg.curScope.lookup(coroKey); ok2 {
			prevScope.set(name+"$coro", se)
		}
	}

	// For exported overloaded functions, propagate each variant's entry and its
	// $coro variant so that cross-package overload resolution and coroutine chaining
	// work.  The overloads map (cg.overloads[name]) stores irName = pkg__fn__sig,
	// which is registered in the child scope under that key.
	for name := range exportedNames {
		if variants, hasOverloads := cg.overloads[name]; hasOverloads {
			for _, v := range variants {
				if entry2, ok2 := cg.curScope.lookup(v.irName); ok2 {
					prevScope.set(v.irName, entry2)
				}

				coroKey := v.irName + "$coro"
				if coroEntry, ok3 := cg.curScope.lookup(coroKey); ok3 {
					prevScope.set(coroKey, coroEntry)
				}
			}
		}
	}

	// Also propagate methods and type aliases for exported struct types that were
	// imported via `use "./..."` (not declared inline in this file). These structs
	// are already registered in cg.structTypes by loadPackageFromFilePath, and their
	// methods are in cg.curScope, but the package-qualified aliases and method entries
	// need to be visible to the caller of `use <pkgName>`.
	for name := range exportedNames {
		canonicalKey := pkgName + "__" + name
		// Check for a non-generic struct type that was imported.
		if cg.structTypeFor(CanonKey(canonicalKey)) != nil {
			// Register package-qualified type alias for the caller.
			if cg.aliasTypeFor(CanonKey(pkgName+"::"+name)) == nil {
				cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: canonicalKey})
				cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: canonicalKey})
			}
			// Propagate any methods found in our child scope to prevScope.
			// Methods are registered under the canonical key.
			cg.curScope.each(func(key string, entry *scopeEntry) {
				if strings.HasPrefix(key, canonicalKey+"_") {
					prevScope.set(key, entry)
				}
			})
		} else if cg.structTypeFor(CanonKey(name)) != nil {
			// Bare-name struct (e.g. file-path import that ran before currentPkg was set).
			if cg.aliasTypeFor(CanonKey(pkgName+"::"+name)) == nil {
				cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: name})
				cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: name})
			}

			cg.curScope.each(func(key string, entry *scopeEntry) {
				if strings.HasPrefix(key, name+"_") {
					prevScope.set(key, entry)
				}
			})
		}

		if _, isGeneric := cg.genericStructsByArity[name]; isGeneric {
			if cg.aliasTypeFor(CanonKey(pkgName+"::"+name)) == nil {
				cg.setTypeAlias(pkgName+"::"+name, &ast.SimpleType{Name: name})
				cg.setTypeAlias(pkgName+"."+name, &ast.SimpleType{Name: name})
			}
		}
	}

	// Pass 5: register exported macros under pkg-qualified keys so that
	// loadPackageSelective can find and re-register them as bare names,
	// and so qualified call sites (`log::info!(...)`) resolve via the
	// ScopeAccess macro path in genCallExpr.
	for _, node := range prog.Stmts {
		md, ok := node.(*ast.MacroDecl)
		if !ok {
			continue
		}

		bareName := strings.TrimSuffix(md.Name, "!")
		if !exportedNames[md.Name] && !exportedNames[bareName] {
			continue
		}

		cg.macros[pkgName+"."+bareName+"!"] = md
		cg.macros[pkgName+"::"+bareName+"!"] = md
		cg.macros[pkgName+"."+bareName] = md
		cg.macros[pkgName+"::"+bareName] = md
	}

	// Pass 6: propagate re-exported child packages' macros under this
	// package's namespace. When std.tin says `export { log } as std`,
	// log's `info!` (registered as `log::info!` by its own load) needs
	// to also resolve under `std::log::info!`. Iterate every macro key
	// that begins with an exported child name and clone it under the
	// current pkg's prefix. This composes naturally for arbitrary
	// re-export depth: an outer umbrella exporting std then sees the
	// freshly-added `std::log::info!` keys and clones them again as
	// `outer::std::log::info!`. No lookup-time path stripping needed.
	if len(exportedNames) > 0 {
		// Snapshot keys first - mutating the map while iterating is undefined.
		original := make(map[string]*ast.MacroDecl, len(cg.macros))
		for k, v := range cg.macros {
			original[k] = v
		}

		cascaded := 0

		for k, md := range original {
			for child := range exportedNames {
				for _, sep := range []string{"::", "."} {
					prefix := child + sep
					if strings.HasPrefix(k, prefix) {
						newKey := pkgName + sep + k
						if _, already := cg.macros[newKey]; !already {
							cg.macros[newKey] = md
							cascaded++
						}
					}
				}
			}
		}

		if cascaded > 0 {
			cg.progress(fmt.Sprintf("cascade re-exports %s (%d macros)", pkgName, cascaded))
		}
	}
	// Validate exports: every name in `export { ... } as pkg` must
	// refer to a top-level decl (struct, generic struct, ADT, enum,
	// trait, function, macro, or a name re-exported from a child
	// package). ADT variant constructors are inferred from the
	// ADT itself, so listing them in `export` is a redundant noise
	// signal - the export list silently accepted them before and
	// callers had no warning when they mis-spelled or referenced a
	// stale name. Diagnose those cases here instead of silently
	// dropping them.
	if err := cg.validatePackageExports(pkgName, exportedNames); err != nil {
		return err
	}

	cg.curScope = prevScope
	cg.filename = prevFilename

	return nil
}

// validatePackageExports rejects exports that don't refer to any
// top-level decl. ADT variant constructors are explicitly called
// out because they're a common mistake - the user lists them
// alongside the parent ADT, but the language already makes the
// variants visible once the ADT is exported.
