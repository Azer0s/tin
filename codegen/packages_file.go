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

func (cg *CodeGen) loadPackageFromFilePath(rawPath string) error {
	baseDir := filepath.Dir(cg.filename)

	// Normalise: strip leading "./"
	cleanPath := rawPath
	cleanPath = strings.TrimPrefix(cleanPath, "./")

	// Derive package name from basename, stripping ".tin" if present.
	base := filepath.Base(cleanPath)
	pkgName := strings.TrimSuffix(base, ".tin")

	// Resolve actual file path, appending ".tin" if needed.
	srcPath := filepath.Join(baseDir, cleanPath)
	if _, err := os.Stat(srcPath); err != nil {
		srcPath = filepath.Join(baseDir, cleanPath+".tin")
		if _, err2 := os.Stat(srcPath); err2 != nil {
			return fmt.Errorf("use %q: file not found", rawPath)
		}
	}

	// Deduplication key uses the resolved absolute path.
	dedupeKey := "file:" + srcPath
	if cg.importedPkgs[dedupeKey] {
		return nil
	}

	cg.importedPkgs[dedupeKey] = true

	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("use %q: read: %w", rawPath, err)
	}

	l := lexer.New(string(src))

	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		return fmt.Errorf("use %q: lex: %w", rawPath, lexErr)
	}

	p := parser.New(tokens, srcPath)
	// Pre-scan for #no_parens macros from `use { name } from pkg` so the parser
	// can do token substitution before parsing (same pattern as main.go).
	for name, expansion := range ScanImportedNoParensMacros(srcPath, tokens, cg.stdlibBase(), cg.libsRoots) {
		p.RegisterNoParensMacro(name, expansion)
	}

	prog, parseErr := p.Parse()
	if parseErr != nil {
		return fmt.Errorf("use %q: parse: %w", rawPath, parseErr)
	}

	for _, raw := range p.Warnings() {
		_, _ = fmt.Fprintln(os.Stderr, RenderDiagnostic(raw))
	}

	cg.pkgSrcPaths = append(cg.pkgSrcPaths, srcPath)

	prevFilename := cg.filename

	// If the file has an export declaration it is a named sub-package, not a flat
	// helper. Delegate to loadPackageFromSource so that exports are processed
	// correctly and sub-namespace entries (e.g. fnv::fnv1a_32) are registered.
	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok {
			cg.filename = prevFilename

			return cg.loadPackageFromSource(rawPath, exp.AsName, srcPath)
		}
	}

	cg.filename = srcPath

	// Process nested use declarations first. Selective imports
	// (`use { Name } from pkg`) flow through loadPackageSelective so
	// the bare aliases AND the bare-type-visibility set get
	// registered.
	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok {
			var loadErr error

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

				return fmt.Errorf("use %q: %w", rawPath, loadErr)
			}
		}
	}

	prevScope := cg.curScope
	cg.curScope = newScope(prevScope)

	// Pass 0.5: preregister struct/enum/type/trait nodes.
	for _, node := range prog.Stmts {
		switch node.(type) {
		case *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl, *ast.TraitDecl, *ast.UnionDecl, *ast.DataDecl:
			if preErr := cg.preregister(node); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %q: preregister: %w", rawPath, preErr)
			}
		}
	}

	// Pass 1: compile extern-backed functions.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern == "" {
			continue
		}

		prefixed := pkgName + "__" + fd.Name
		if compErr := cg.genFuncDeclAs(fd, prefixed); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: %s: %w", rawPath, fd.Name, compErr)
		}

		if entry, ok2 := cg.curScope.lookup(prefixed); ok2 {
			if _, already := cg.curScope.vars[fd.Name]; !already {
				cg.curScope.set(fd.Name, entry)
			}
			// Propagate to module scope so on-demand monomorphization of generic
			// structs (e.g. Channel[T]) can find these helpers when running at
			// module scope.
			if cg.moduleScope != nil {
				if _, already := cg.moduleScope.vars[fd.Name]; !already {
					cg.moduleScope.set(fd.Name, entry)
				}
			}
		}
	}

	// Pass 1.2: mark {#async} struct methods as coro-callable and pre-declare
	// their $coro variants BEFORE genStructDecl generates method bodies.  The
	// scope key uses the package-qualified struct name so it matches the
	// spawn-method lookup site, which builds its key from typeNameOf(receiver)
	// -- that always returns the qualified form (e.g. `sync__Cond`).  Using
	// the bare sd.Name here would register `Cond_method$coro` while the call
	// site searches for `sync__Cond_method$coro`, and any intra-struct
	// `spawn this.other_method(...)` would fail to resolve unless the source
	// happened to declare the methods in an order the body emitter never
	// reaches the spawn before the inner predeclares -- a fragile invariant
	// that hit Cond.wait/wait_impl.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok || len(sd.TypeParams) > 0 {
			continue
		}

		structKey := cg.pkgStructKey(sd.Name)

		for _, m := range sd.Methods {
			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}

			scopeKey := methodScopeName(structKey, m)

			cg.coroCallable[scopeKey] = true
			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %q: struct %s method %s coro predecl: %w", rawPath, sd.Name, m.Name, preErr)
			}
		}
	}

	// Pass 1.5a: emit struct LAYOUTS for every non-generic struct in this
	// package, expose type aliases. Method bodies are deferred until Pass
	// 1.5b so that ADT layouts (Pass 1.6) can see completed inner struct
	// layouts before any ADT payload size is baked into the IR.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if compErr := cg.genStructLayout(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: struct %s: %w", rawPath, sd.Name, compErr)
		}
		// Expose the type alias as a bare name in the parent scope.
		// Use the canonical struct key (e.g. "sync__Mutex") as the alias target.
		// Skip generic structs: their templates are stored by bare name in
		// genericStructsByArity and must not be aliased to "pkg__Name" here,
		// or genTypeDecl will fail to find the template under the qualified key.
		if len(sd.TypeParams) == 0 {
			cg.setTypeAlias(sd.Name, &ast.SimpleType{Name: cg.pkgStructKey(sd.Name)})
		}
	}

	// Pass 1.6: emit concrete layout for non-generic ADTs defined in this
	// package, AFTER all struct layouts are finalized. The payload size
	// computation depends on the inner struct types having complete
	// field lists (opaque structs have size 0, which would under-size
	// the ADT's payload buffer). Generic ADTs are monomorphized on demand.
	for _, node := range prog.Stmts {
		if dd, ok := node.(*ast.DataDecl); ok {
			if compErr := cg.genDataDecl(dd); compErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %q: data %s: %w", rawPath, dd.Name, compErr)
			}
		}
	}

	// Pass 0.8: detect overloaded function names so Pass 2/3 can mangle IR
	// names for functions sharing the same base name (e.g. get(url) vs
	// get(client, url)). Mirrors the same pass in loadPackageFromSource.
	// File imports inherit the enclosing package context (cg.currentPkg),
	// so struct method keys must be scanned under that prefix or
	// overloads on the imported file's structs won't be mangled and end
	// up colliding under their bare scope name.
	//
	// Hoisted ahead of Pass 1.5b so struct method bodies that spawn or
	// directly call free fns (Result[T,E] returners, async helpers) see
	// the correctly mangled handle/$coro variant.
	for name, flag := range scanOverloadedNamesPkg(prog.Stmts, cg.currentPkg) {
		cg.overloadedNames[name] = flag
	}

	// Pass 1.45: pre-declare $coro variants of overloaded {#async} module-level
	// functions so that Pass 1.5b struct method bodies (and Pass 3 free
	// fn bodies) can chain-await them.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" || !isAsyncTag(fd.Tags) || !cg.overloadedNames[fd.Name] {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		sig := funcParamSig(fd.Params)
		irName := overloadMangledName(baseName, sig)

		cg.coroCallable[irName] = true
		if preErr := cg.predeclareCoroVariant(fd, irName, false); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: coro predecl %s: %w", rawPath, fd.Name, preErr)
		}
	}

	// Pass 1.5c: pre-declare $coro variants for ALL module-level
	// {#async} free functions.  Must run BEFORE Pass 1.5b so struct
	// methods that spawn a same-file async free fn (e.g. BufWriter's
	// AsyncWriter::write spawning a flush_then_write helper) see the
	// $coro handle when their bodies compile.
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

			return fmt.Errorf("use %q: early coro predecl %s: %w", rawPath, fd.Name, preErr)
		}
	}

	// Pass 2: predeclare non-extern functions.  Hoisted ahead of Pass
	// 1.5b so struct methods can call free fns directly (not only via
	// spawn).
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
			// Register in overloads map so cross-package call-site resolution works.
			paramTypes, ptErr := cg.resolveParamTypes(fd.Params, "")
			if ptErr == nil {
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

			return fmt.Errorf("use %q: %s: %w", rawPath, fd.Name, preErr)
		}
	}

	// Pass 1.5b: emit struct METHODS now that every struct + ADT layout
	// is complete AND every free-fn predeclare ($coro included) is
	// registered.  Method bodies are free to spawn / await / directly
	// call any other helper in the file.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if compErr := cg.genStructMethods(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: struct %s methods: %w", rawPath, sd.Name, compErr)
		}

		structKey := cg.pkgStructKey(sd.Name)

		// Propagate methods to prevScope so callers can call them.
		// Methods are registered under canonicalKey_methodName in curScope.
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
			// Also expose under bare key for backward compatibility.
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

	// Pass 2.8: register top-level const declarations in scope so that
	// function bodies compiled in Pass 3 below can reference them by
	// bare name. Mirrors the same pass in loadPackageFromSource. Without
	// it, file-imported sub-files (use "./helper") silently treat any
	// const reference inside a fn body as an undefined identifier.
	registerPkgConst := func(name string, value ast.Node, typ ast.TypeExpr) {
		// Mirror the entry-program path: when no explicit type annotation
		// is given, infer from a literal-form initializer so that
		// `const RED = Color{...}` folds against the real LLVM struct
		// type instead of bailing out.
		if typ == nil && value != nil {
			typ = cg.inferTopLevelVarType(value)
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

		baseName := pkgName + "__" + fd.Name
		irName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			irName = overloadMangledName(baseName, sig)
		}

		if compErr := cg.genFuncDeclAs(fd, irName); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: %s: %w", rawPath, fd.Name, compErr)
		}
	}

	// Propagate ALL defined symbols to prevScope as bare names.
	// use "./file" embeds the file: every function, type alias, and macro it
	// defines becomes directly available in the importing scope without any
	// package prefix. No export declaration is needed (or allowed) in helper files.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok {
			continue
		}

		baseName := pkgName + "__" + fd.Name
		lookupName := baseName

		if cg.overloadedNames[fd.Name] {
			sig := funcParamSig(fd.Params)
			lookupName = overloadMangledName(baseName, sig)
		}

		if entry, ok2 := cg.curScope.lookup(lookupName); ok2 {
			if cg.overloadedNames[fd.Name] {
				// Propagate the mangled variant directly so scope lookups by irName work.
				prevScope.set(lookupName, entry)
				// Also register a fallback under the bare and parent-prefixed names
				// (last registered wins; callers should use cg.overloads for resolution).
				if _, already := prevScope.vars[fd.Name]; !already {
					prevScope.set(fd.Name, entry)
				}

				if cg.currentPkg != "" {
					if _, already := prevScope.vars[cg.currentPkg+"__"+fd.Name]; !already {
						prevScope.set(cg.currentPkg+"__"+fd.Name, entry)
					}
				}
			} else {
				prevScope.set(fd.Name, entry)
				// Also register under the file's own pkg-prefixed key
				// (e.g. "eager__sum" when this file was loaded as
				// `use "./eager"`) so the parent's cross-file overload
				// resolver in genCallExpr can `lookup(best.irName)` and
				// reach the concrete IR func.  Without this, the entry
				// only lives in the popped sub-file scope and
				// pickOverload looks like it succeeded but the
				// subsequent scope lookup falls through, dispatching to
				// a wrong-shape callee and segfaulting at runtime.
				prevScope.set(lookupName, entry)
				// Also register under the parent package prefix (e.g. "os__stat") so that
				// loadPackageFromSource can find and re-export this symbol under the package
				// namespace (e.g. as "os::stat"). Without this, only the file-local prefix
				// ("os_stat__stat") is known and the re-export lookup fails.
				if cg.currentPkg != "" {
					prevScope.set(cg.currentPkg+"__"+fd.Name, entry)
				}
			}
		}

		// Also propagate $coro variant for {#async} functions.
		coroKey := lookupName + "$coro"
		if coroEntry, ok3 := cg.curScope.lookup(coroKey); ok3 {
			if cg.overloadedNames[fd.Name] {
				// Propagate the mangled $coro variant for overload resolution.
				prevScope.set(coroKey, coroEntry)
			} else {
				prevScope.set(fd.Name+"$coro", coroEntry)

				if cg.currentPkg != "" {
					prevScope.set(cg.currentPkg+"__"+fd.Name+"$coro", coroEntry)
				}
			}
		}
	}

	// Propagate top-level const declarations to prevScope so that the
	// importing file can both reference them in expressions AND list
	// them in its `export { ... } as pkg` block.  Pass 2.8 above only
	// registers consts into the local file scope; without this loop a
	// const defined in a helper file is invisible to its importer,
	// even though every other top-level decl flows through transparently.
	for _, node := range prog.Stmts {
		var constName string

		switch d := node.(type) {
		case *ast.VarDecl:
			if d.IsConst {
				constName = d.Name
			}
		case *ast.TopLevelVar:
			if d.IsConst {
				constName = d.Name
			}
		}

		if constName == "" {
			continue
		}

		entry, ok := cg.curScope.lookup(constName)
		if !ok {
			continue
		}
		// Bare name: lets fn bodies in the importing file reference
		// the const directly (`if x == ROW_MAJOR: ...`).
		prevScope.set(constName, entry)
		// Parent-package-prefixed name: lets loadPackageFromSource's
		// export-propagation loop find the const under "<pkg>__<name>"
		// when the importing file re-exports it as part of the package.
		if cg.currentPkg != "" {
			prevScope.set(cg.currentPkg+"__"+constName, entry)
		}
	}

	// Register ALL macros defined in the file as bare names.
	for _, node := range prog.Stmts {
		md, ok := node.(*ast.MacroDecl)
		if !ok {
			continue
		}

		bareName := strings.TrimSuffix(md.Name, "!")
		// Bare name (for plain `use "./file.tin"` imports).
		cg.macros[bareName+"!"] = md
		cg.macros[bareName] = md
		// Pkg-qualified keys (for `use { name } from "./file.tin"` selective imports).
		cg.macros[pkgName+"."+bareName+"!"] = md
		cg.macros[pkgName+"::"+bareName+"!"] = md
		cg.macros[pkgName+"."+bareName] = md
		cg.macros[pkgName+"::"+bareName] = md
	}

	// When this helper file is being loaded INSIDE an outer package
	// (e.g. seq.tin doing `use "./eager"` / `use "./lazy"`), mirror the
	// generic-function templates under the outer package's qualified
	// key so callers can resolve `seq::filter` against templates that
	// physically live in eager.tin or lazy.tin.  Also register every
	// non-generic free function in the shared cg.overloads table so
	// that two siblings declaring the same name (e.g. eager `fn sum
	// (xs [i64])` and lazy `fn sum(s *Seq[i64])`) coexist as
	// arg-type-resolved overloads -- without this, the bare name `sum`
	// in scope is overwritten by whichever file loads last and any
	// `xs |> seq::sum` segfaults by dispatching to the wrong shape.
	if cg.currentPkg != "" && cg.currentPkg != pkgName {
		for _, node := range prog.Stmts {
			fd, ok := node.(*ast.FuncDecl)
			if !ok || fd.IsExtern != "" {
				continue
			}

			if len(fd.TypeParams) > 0 {
				qualKey := cg.currentPkg + "__" + fd.Name
				if _, already := cg.genericFuncs[qualKey]; !already {
					cg.genericFuncs[qualKey] = fd
				}

				cg.genericFuncOverloads[qualKey] = appendGenericFuncOverload(cg.genericFuncOverloads[qualKey], fd)

				if _, already := cg.genericFuncHomeScopes[qualKey]; !already {
					cg.genericFuncHomeScopes[qualKey] = cg.curScope
				}

				continue
			}

			if len(fd.Constraints) > 0 {
				continue
			}

			irName := pkgName + "__" + fd.Name
			if cg.overloadedNames[fd.Name] {
				irName = overloadMangledName(irName, funcParamSig(fd.Params))
			}

			alreadyHave := false

			for _, existing := range cg.overloads[fd.Name] {
				if existing.irName == irName {
					alreadyHave = true

					break
				}
			}

			if alreadyHave {
				continue
			}

			paramTypes, ptErr := cg.resolveParamTypes(fd.Params, "")
			if ptErr != nil {
				continue
			}

			var retType irtypes.Type

			if fd.RetType != nil {
				retType, _ = cg.tinTypeToLLVM(fd.RetType)
			}

			cg.overloads[fd.Name] = append(cg.overloads[fd.Name], &overloadEntry{
				irName:     irName,
				paramSig:   funcParamSig(fd.Params),
				paramTypes: paramTypes,
				arity:      len(paramTypes),
				returnType: retType,
			})
		}
	}

	cg.curScope = prevScope
	cg.filename = prevFilename

	return nil
}

// loadPackageFromSource compiles a .tin source file inline into the current
// module, prefixing all function IR names with pkgName+"__". This lets stdlib
// packages be written in pure Tin (with only the truly native bits remaining in
// runtime.c) without requiring a separate linking step.
