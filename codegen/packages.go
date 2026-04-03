package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

func (cg *CodeGen) genUseDecl(n *ast.UseDecl) error {
	if !n.IsExtern {
		if n.FromSyntax {
			return cg.loadPackageSelective(n.Path, n.Names, n.IsFile)
		}
		if n.IsFile {
			return cg.loadPackageFromFilePath(n.Path)
		}

		return cg.loadPackage(n.Path)
	}
	for _, imp := range n.Imports {
		if imp.Type == nil {
			continue
		}
		ft, ok := imp.Type.(*ast.FuncType)
		if !ok {
			continue
		}
		// Build C-level parameter list.
		cParams := make([]*ir.Param, len(ft.Params))
		for i, p := range ft.Params {
			ct, err := cg.tinTypeToExternLLVM(p, false)
			if err != nil {
				return err
			}
			cParams[i] = ir.NewParam(fmt.Sprintf("p%d", i), ct)
		}
		var tinRetType irtypes.Type = irtypes.Void
		var cRetType irtypes.Type = irtypes.Void
		if ft.RetType != nil {
			var err error
			tinRetType, err = cg.tinTypeToLLVM(ft.RetType)
			if err != nil {
				return err
			}
			cRetType, err = cg.tinTypeToExternLLVM(ft.RetType, true)
			if err != nil {
				return err
			}
		}
		cName := imp.ExternName
		if cName == "" {
			cName = imp.LocalName
		}
		cFunc := cg.ensureExternDecl(cName, cRetType, cParams, ft.IsVarArgs)

		if cg.curScope == nil {
			continue
		}

		// If the return type doesn't need wrapping, expose C function directly.
		// Fat-ptr parameters are coerced at call sites by coerce().
		if cRetType.Equal(tinRetType) {
			cg.curScope.set(imp.LocalName, &scopeEntry{val: cFunc, isAlloc: false})

			continue
		}

		// Return type needs wrapping: generate a small wrapper.
		wrapperName := "__tinwrap_" + imp.LocalName
		var wrapperFn *ir.Func
		for _, f := range cg.mod.Funcs {
			if f.Name() == wrapperName {
				wrapperFn = f

				break
			}
		}
		if wrapperFn == nil {
			wrapperFn = cg.mod.NewFunc(wrapperName, tinRetType, cParams...)
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")
			callArgs := make([]value.Value, len(wrapperFn.Params))
			for i, p := range wrapperFn.Params {
				callArgs[i] = p
			}
			raw := entry.NewCall(cFunc, callArgs...)
			entry.NewRet(cg.wrapFromExtern(entry, raw, tinRetType))
			cg.curFn = prevFn
			cg.curScope = prevScope
		}
		cg.curScope.set(imp.LocalName, &scopeEntry{val: wrapperFn, isAlloc: false})
	}

	return nil
}

// loadPackage loads a .tin.mod module file and registers all its exported
// symbols as extern declarations in the current LLVM module.
// pkgPath is the dot-separated import path, e.g. "io" or "std::math".
// The file is searched relative to the directory of the source file being
// compiled.
func (cg *CodeGen) loadPackage(pkgPath string) error {
	if pkgPath == "" {
		return nil
	}
	// Normalise path separator: "std::math" -> "std/math"
	parts := strings.Split(pkgPath, "::")
	pkgName := parts[len(parts)-1] // last segment = package name used in scope

	if cg.importedPkgs[pkgPath] {
		return nil // already loaded
	}
	cg.importedPkgs[pkgPath] = true

	// Resolve package paths.  Try multiple locations in order:
	// 1. Relative to the source file being compiled.
	// 2. stdlib/ directory next to the tin executable.
	baseDir := filepath.Dir(cg.filename)
	modFile := filepath.Join(append([]string{baseDir}, parts...)...) + ".tin.mod"

	// Check for a companion .tin source file alongside the mod file.
	// A local .tin file is only treated as a package if a .tin.mod file also
	// exists next to it - this prevents plain example files from accidentally
	// shadowing stdlib packages (e.g. examples/math.tin vs stdlib math).
	// Source files take precedence over pre-compiled .tin.mod: they are
	// compiled inline so no separate linking step is needed.
	tinSrc := strings.TrimSuffix(modFile, ".tin.mod") + ".tin"
	_, modExists := os.Stat(modFile)
	_, srcExists := os.Stat(tinSrc)
	if srcExists != nil || modExists != nil {
		// Not a valid local package; try stdlib locations.
		if ex, exErr := os.Executable(); exErr == nil {
			execDir := filepath.Dir(ex)
			p1 := filepath.Join(append([]string{execDir, "stdlib"}, parts...)...) + ".tin"
			p2 := filepath.Join(execDir, "stdlib", pkgName, pkgName) + ".tin"
			if _, e := os.Stat(p1); e == nil {
				tinSrc = p1
			} else if _, e := os.Stat(p2); e == nil {
				tinSrc = p2
			} else {
				tinSrc = ""
			}
		} else {
			tinSrc = ""
		}
	}
	if tinSrc != "" {
		return cg.loadPackageFromSource(pkgPath, pkgName, tinSrc)
	}

	mf, err := ReadModFile(modFile)
	if err != nil {
		// Fall back to stdlib/ next to the compiler executable.
		if ex, exErr := os.Executable(); exErr == nil {
			execDir := filepath.Dir(ex)
			// Try <execDir>/stdlib/<parts...>.tin.mod (e.g. stdlib/io.tin.mod).
			p1 := filepath.Join(append([]string{execDir, "stdlib"}, parts...)...) + ".tin.mod"
			mf, err = ReadModFile(p1)
			if err != nil {
				// Try <execDir>/stdlib/<pkgName>/<pkgName>.tin.mod
				// (e.g. stdlib/io/io.tin.mod - matches the stdlib source layout).
				p2 := filepath.Join(execDir, "stdlib", pkgName, pkgName) + ".tin.mod"
				mf, err = ReadModFile(p2)
			}
		}
	}
	if err != nil {
		// Module file not found - not an error if the file simply doesn't exist
		// yet (the user may be compiling the module for the first time).

		return nil
	}

	// Register exported struct types so they can be used in parameter/return types.
	for _, ms := range mf.Structs {
		// Register the type under both the package-prefixed IR name and local name,
		// allowing both mylib::Point and Point (when explicitly aliased) to work.
		if _, exists := cg.structTypes[ms.IRName]; !exists {
			st := irtypes.NewStruct()
			st.SetName(ms.IRName)
			// Populate fields.
			var fieldTypes []irtypes.Type
			var fieldNames []string
			for _, f := range ms.Fields {
				te, err2 := parseTypeString(f.Type)
				if err2 != nil {
					return fmt.Errorf("use %s: struct %s field %s: %w", pkgPath, ms.LocalName, f.Name, err2)
				}
				ft, err2 := cg.tinTypeToLLVM(te)
				if err2 != nil {
					return fmt.Errorf("use %s: struct %s field %s: %w", pkgPath, ms.LocalName, f.Name, err2)
				}
				fieldTypes = append(fieldTypes, ft)
				fieldNames = append(fieldNames, f.Name)
			}
			st.Fields = fieldTypes
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			cg.structTypes[ms.IRName] = st
			cg.structFields[ms.IRName] = fieldNames
			// Register under pkgName.localName as a type alias.
			cg.typeAliases[pkgName+"::"+ms.LocalName] = &ast.SimpleType{Name: ms.IRName}
			cg.typeAliases[pkgName+"."+ms.LocalName] = &ast.SimpleType{Name: ms.IRName}
			// If the local name doesn't already exist, register it too.
			if _, exists2 := cg.structTypes[ms.LocalName]; !exists2 {
				cg.structTypes[ms.LocalName] = st
				cg.structFields[ms.LocalName] = fieldNames
			}
			// Declare all struct methods as extern functions.
			for _, mfn := range ms.Methods {
				mparams := make([]*ir.Param, len(mfn.Params))
				for i, p := range mfn.Params {
					te, err2 := parseTypeString(p.Type)
					if err2 != nil {
						continue
					}
					pt, err2 := cg.tinTypeToLLVM(te)
					if err2 != nil {
						continue
					}
					mparams[i] = ir.NewParam(p.Name, pt)
				}
				var mret irtypes.Type = irtypes.Void
				if mfn.RetType != "" {
					te, err2 := parseTypeString(mfn.RetType)
					if err2 == nil {
						mret, _ = cg.tinTypeToLLVM(te)
					}
				}
				irMeth := cg.mod.NewFunc(mfn.IRName, mret, mparams...)
				irMeth.Blocks = nil
				cg.curScope.set(mfn.IRName, &scopeEntry{val: irMeth, isAlloc: false})
			}
		}
	}

	// Register type aliases from the module.
	for _, ta := range mf.Types {
		te, err2 := parseTypeString(ta.Target)
		if err2 != nil {
			return fmt.Errorf("use %s: type %s: %w", pkgPath, ta.Name, err2)
		}
		cg.typeAliases[pkgName+"."+ta.Name] = te
		cg.typeAliases[pkgName+"::"+ta.Name] = te
		// Also register locally if not already defined.
		if _, exists := cg.typeAliases[ta.Name]; !exists {
			cg.typeAliases[ta.Name] = te
		}
	}

	// Register exported functions.
	for _, fn := range mf.Funcs {
		scopeKey := pkgName + "." + fn.LocalName

		if fn.ExternName != "" {
			// Extern-backed function: reconstruct a FuncDecl and use the full
			// extern-wrapper path so fat-pointer unwrapping is applied correctly.
			var astParams []ast.Param
			for _, p := range fn.Params {
				te, err2 := parseTypeString(p.Type)
				if err2 != nil {
					return fmt.Errorf("use %s: param %s: %w", pkgPath, p.Name, err2)
				}
				astParams = append(astParams, ast.Param{Name: p.Name, Type: te})
			}
			if fn.Variadic {
				astParams = append(astParams, ast.Param{Name: "...", IsVarArgs: true})
			}
			var retTE ast.TypeExpr
			if fn.RetType != "" {
				var err2 error
				retTE, err2 = parseTypeString(fn.RetType)
				if err2 != nil {
					return fmt.Errorf("use %s: return type: %w", pkgPath, err2)
				}
			}
			fd := &ast.FuncDecl{
				Name:     fn.LocalName,
				Params:   astParams,
				RetType:  retTE,
				IsExtern: fn.ExternName,
			}
			if err2 := cg.genFuncDeclAs(fd, scopeKey); err2 != nil {
				return fmt.Errorf("use %s: %s: %w", pkgPath, fn.LocalName, err2)
			}

			continue
		}

		// Pre-compiled function: declare as external LLVM symbol by IRName.
		params := make([]*ir.Param, len(fn.Params))
		for i, p := range fn.Params {
			te, err2 := parseTypeString(p.Type)
			if err2 != nil {
				return fmt.Errorf("use %s: param %s: %w", pkgPath, p.Name, err2)
			}
			pt, err2 := cg.tinTypeToLLVM(te)
			if err2 != nil {
				return fmt.Errorf("use %s: param %s: %w", pkgPath, p.Name, err2)
			}
			params[i] = ir.NewParam(p.Name, pt)
		}
		var retType irtypes.Type = irtypes.Void
		if fn.RetType != "" {
			te, err2 := parseTypeString(fn.RetType)
			if err2 != nil {
				return fmt.Errorf("use %s: return type: %w", pkgPath, err2)
			}
			retType, err2 = cg.tinTypeToLLVM(te)
			if err2 != nil {
				return fmt.Errorf("use %s: return type: %w", pkgPath, err2)
			}
		}
		irFunc := cg.mod.NewFunc(fn.IRName, retType, params...)
		irFunc.Sig.Variadic = fn.Variadic
		irFunc.Blocks = nil // declaration only
		cg.curScope.set(scopeKey, &scopeEntry{val: irFunc, isAlloc: false})
	}

	// Register exported macros from the module file.
	// Macros are stored under pkg-qualified keys so loadPackageSelective can find them.
	for _, mm := range mf.Macros {
		if mm.Body == "" && len(mm.Params) == 0 {
			continue
		}
		md := &ast.MacroDecl{
			Name:   mm.Name + "!",
			Tags:   mm.Tags,
			Params: mm.Params,
		}
		if mm.Body != "" {
			md.Body = &ast.BacktickLit{Content: mm.Body}
		}
		// Register under package-qualified keys; bare-name registration happens
		// only when the caller does `use { macroName! } from pkg`.
		cg.macros[pkgName+"."+mm.Name+"!"] = md
		cg.macros[pkgName+"::"+mm.Name+"!"] = md
		cg.macros[pkgName+"."+mm.Name] = md
		cg.macros[pkgName+"::"+mm.Name] = md
	}

	// Handle re-exports: recursively load sub-packages.
	for _, sub := range mf.ReExports {
		subPath := pkgPath + "::" + sub
		if err2 := cg.loadPackage(subPath); err2 != nil {
			return err2
		}
	}

	return nil
}

// loadPackageFromFilePath handles `use "./foo.tin"` (or `use "./foo"`) imports.
// It resolves the path relative to the current source file, derives the package
// name from the filename, compiles the file inline, and exposes all exported
// symbols into the current scope as BARE names (without any package prefix).
// This allows one package file to re-export symbols from sibling files.
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
	p := parser.New(tokens)
	// Pre-scan for #no_parens macros from `use { name } from pkg` so the parser
	// can do token substitution before parsing (same pattern as main.go).
	for name, expansion := range ScanImportedNoParensMacros(srcPath, tokens) {
		p.RegisterNoParensMacro(name, expansion)
	}
	prog, parseErr := p.Parse()
	if parseErr != nil {
		return fmt.Errorf("use %q: parse: %w", rawPath, parseErr)
	}

	// Collect all exported names from the file (ignore the "as <pkg>" name).
	exportedNames := map[string]bool{}
	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok {
			for _, name := range exp.Names {
				exportedNames[name] = true
			}
		}
	}

	cg.pkgSrcPaths = append(cg.pkgSrcPaths, srcPath)

	prevFilename := cg.filename
	cg.filename = srcPath

	// Process nested use declarations first.
	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok {
			var loadErr error
			if ud.IsFile {
				loadErr = cg.loadPackageFromFilePath(ud.Path)
			} else {
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
		case *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl, *ast.TraitDecl, *ast.UnionDecl:
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
	// their $coro variants BEFORE genStructDecl generates method bodies.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok || len(sd.TypeParams) > 0 {
			continue
		}
		for _, m := range sd.Methods {
			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}
			scopeKey := methodScopeName(sd.Name, m)
			cg.coroCallable[scopeKey] = true
			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %q: struct %s method %s coro predecl: %w", rawPath, sd.Name, m.Name, preErr)
			}
		}
	}

	// Pass 1.5: generate struct declarations; propagate methods to prevScope.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}
		if compErr := cg.genStructDecl(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: struct %s: %w", rawPath, sd.Name, compErr)
		}
		// Expose the type alias as a bare name in the parent scope.
		// Use the canonical struct key (e.g. "sync__Mutex") as the alias target.
		structKey := cg.pkgStructKey(sd.Name)
		if exportedNames[sd.Name] {
			cg.typeAliases[sd.Name] = &ast.SimpleType{Name: structKey}
		}
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

	// Pass 2: predeclare non-extern functions.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}
		prefixed := pkgName + "__" + fd.Name
		if preErr := cg.predeclareFuncAs(fd, prefixed); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: %s: %w", rawPath, fd.Name, preErr)
		}
	}

	// Pass 3: compile non-extern function bodies.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}
		prefixed := pkgName + "__" + fd.Name
		if compErr := cg.genFuncDeclAs(fd, prefixed); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %q: %s: %w", rawPath, fd.Name, compErr)
		}
	}

	// Propagate exported symbols to prevScope as bare names (no package prefix).
	// This makes file-path imports behave as glob imports: all exported symbols
	// from the file are directly available in the importing file's scope.
	for name := range exportedNames {
		prefixed := pkgName + "__" + name
		if entry, ok2 := cg.curScope.lookup(prefixed); ok2 {
			prevScope.set(name, entry)
		}
	}

	// Register exported macros as bare names AND under pkg-qualified keys so
	// that `use { mymin! } from "./mypkg.tin"` (loadPackageSelective) can find them.
	for _, node := range prog.Stmts {
		md, ok := node.(*ast.MacroDecl)
		if !ok {
			continue
		}
		bareName := strings.TrimSuffix(md.Name, "!")
		if !exportedNames[md.Name] && !exportedNames[bareName] {
			continue
		}
		// Bare name (for plain `use "./file.tin"` imports).
		cg.macros[bareName+"!"] = md
		cg.macros[bareName] = md
		// Pkg-qualified keys (for `use { name } from "./file.tin"` selective imports).
		cg.macros[pkgName+"."+bareName+"!"] = md
		cg.macros[pkgName+"::"+bareName+"!"] = md
		cg.macros[pkgName+"."+bareName] = md
		cg.macros[pkgName+"::"+bareName] = md
	}

	cg.curScope = prevScope
	cg.filename = prevFilename

	return nil
}

// loadPackageFromSource compiles a .tin source file inline into the current
// module, prefixing all function IR names with pkgName+"__". This lets stdlib
// packages be written in pure Tin (with only the truly native bits remaining in
// runtime.c) without requiring a separate linking step.
func (cg *CodeGen) loadPackageFromSource(pkgPath, pkgName, srcPath string) error {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("use %s: read source: %w", pkgPath, err)
	}

	l := lexer.New(string(src))
	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		return fmt.Errorf("use %s: lex: %w", pkgPath, lexErr)
	}
	p := parser.New(tokens)
	// Pre-scan for #no_parens macros imported via `use { name } from pkg` so the
	// parser can substitute them as bare tokens (same pattern as main.go).
	for name, expansion := range ScanImportedNoParensMacros(srcPath, tokens) {
		p.RegisterNoParensMacro(name, expansion)
	}
	prog, parseErr := p.Parse()
	if parseErr != nil {
		return fmt.Errorf("use %s: parse: %w", pkgPath, parseErr)
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
	cg.filename = srcPath
	// Set currentPkg so that struct preregistration and genStructDecl produce
	// canonical "pkgName__StructName" keys/IR-names for structs defined in this
	// package (including those brought in via `use "./..."` file-path imports).
	cg.currentPkg = pkgName
	defer func() { cg.currentPkg = prevPkg }()
	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok {
			var loadErr error
			if ud.IsFile {
				loadErr = cg.loadPackageFromFilePath(ud.Path)
			} else {
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
		if _, ok := cg.structTypes[canonicalKey]; ok {
			cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: canonicalKey}
			cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: canonicalKey}
		} else if _, ok2 := cg.structTypes[name]; ok2 {
			// Struct was loaded without a package prefix (e.g. from a direct file import
			// that ran before currentPkg was set). Fall back to bare-name alias.
			cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
			cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
		}
		// Generic struct templates are always keyed by bare name.
		if _, ok := cg.genericStructsByArity[name]; ok {
			cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
			cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
		}
	}

	// Push a child scope so internal names don't pollute the caller's scope.
	prevScope := cg.curScope
	cg.curScope = newScope(prevScope)

	// Pass 0.5: preregister struct/enum/type/trait nodes so that later passes can
	// resolve type names inside function signatures and struct field types.
	for _, node := range prog.Stmts {
		switch node.(type) {
		case *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl, *ast.TraitDecl, *ast.UnionDecl:
			if preErr := cg.preregister(node); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %s: preregister: %w", pkgPath, preErr)
			}
		}
	}

	// Pass 1: compile extern-backed functions first so their names are in scope
	// before non-extern bodies reference them.
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern == "" {
			continue
		}
		prefixed := pkgName + "__" + fd.Name
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

	// Pass 1.2: mark {#async} struct methods as coro-callable and pre-declare
	// their $coro variants BEFORE genStructDecl generates method bodies.
	// This is needed so that genStructDecl -> genStructMethod -> genFuncDeclAs
	// generates $coro variants for async methods, which are then available
	// when genTraitVtables tries to build vtable global constants.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok || len(sd.TypeParams) > 0 {
			continue
		}
		for _, m := range sd.Methods {
			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}
			// Build the scope key for this method (same as genStructMethod/methodScopeName).
			// Use canonical struct key so it matches what genStructDecl registers.
			scopeKey := methodScopeName(pkgName+"__"+sd.Name, m)
			cg.coroCallable[scopeKey] = true
			// Predeclare the $coro variant so it's registered in scope before vtable gen.
			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.curScope = prevScope
				cg.filename = prevFilename

				return fmt.Errorf("use %s: struct %s method %s coro predecl: %w", pkgPath, sd.Name, m.Name, preErr)
			}
		}
	}

	// Pass 1.4: detect overloaded function names and predeclare non-extern
	// functions BEFORE struct methods are compiled, so that struct methods can
	// call module-level helper functions defined later in the same file.
	for name, flag := range scanOverloadedNames(prog.Stmts) {
		cg.overloadedNames[name] = flag
	}
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
		// Also register the bare local name so struct methods can call it.
		if entry, ok2 := cg.curScope.lookup(irName); ok2 {
			if _, already := cg.curScope.vars[fd.Name]; !already {
				cg.curScope.set(fd.Name, entry)
			}
		}
	}

	// Pass 1.5: generate struct declarations (field layouts + method bodies).
	// Non-generic structs are fully compiled; generic structs are stored as
	// templates in cg.genericStructsByArity and compiled on demand when instantiated.
	for _, node := range prog.Stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}
		if compErr := cg.genStructDecl(sd); compErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename

			return fmt.Errorf("use %s: struct %s: %w", pkgPath, sd.Name, compErr)
		}
		// Register type aliases so module-qualified types resolve.
		// For non-generic structs: "sync::Unit" -> SimpleType{Name: "sync__Unit"}.
		// For generic struct templates: "sync::Channel" -> SimpleType{Name: "Channel"} (bare).
		structKey := pkgName + "__" + sd.Name
		if _, stOk := cg.structTypes[structKey]; stOk {
			// Non-generic struct with canonical key.
			prevScope.set(pkgName+"::"+sd.Name, &scopeEntry{val: nil, isAlloc: false})
			cg.typeAliases[pkgName+"::"+sd.Name] = &ast.SimpleType{Name: structKey}
			cg.typeAliases[pkgName+"."+sd.Name] = &ast.SimpleType{Name: structKey}
		} else if _, stOk2 := cg.structTypes[sd.Name]; stOk2 {
			// Struct registered under bare name (e.g. from file-path import before
			// currentPkg was set, or user-level struct).
			cg.typeAliases[pkgName+"::"+sd.Name] = &ast.SimpleType{Name: sd.Name}
			cg.typeAliases[pkgName+"."+sd.Name] = &ast.SimpleType{Name: sd.Name}
		} else {
			// Generic struct template (not in structTypes) - use bare name alias.
			cg.typeAliases[pkgName+"::"+sd.Name] = &ast.SimpleType{Name: sd.Name}
			cg.typeAliases[pkgName+"."+sd.Name] = &ast.SimpleType{Name: sd.Name}
		}
		// Propagate all methods to prevScope so that method calls on values of
		// this struct type (loaded by the caller) can be resolved.
		// Methods were registered under structKey (canonical), so use that prefix.
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

	// Pre-pass 1.8: detect overloaded function names in this package so that
	// passes 2/2.5/3 can mangle IR names for overloaded functions correctly.
	for name, flag := range scanOverloadedNames(prog.Stmts) {
		cg.overloadedNames[name] = flag
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
			cg.genericFuncHomeScopes[fd.Name] = cg.curScope // package scope for bare-name resolution

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
					cg.overloads[fd.Name] = append(cg.overloads[fd.Name], &overloadEntry{
						irName:     irName,
						paramSig:   sig,
						paramTypes: paramTypes,
						arity:      len(paramTypes),
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

	// Pass 4: register exported constants (VarDecl with IsConst=true and literal values).
	for _, node := range prog.Stmts {
		vd, ok := node.(*ast.VarDecl)
		if !ok || !vd.IsConst || !exportedNames[vd.Name] {
			continue
		}
		var constVal value.Value
		switch lit := vd.Value.(type) {
		case *ast.FloatLit:
			constVal = constant.NewFloat(irtypes.Double, lit.Value)
		case *ast.IntLit:
			constVal = constant.NewInt(irtypes.I64, lit.Value)
		}
		if constVal != nil {
			entry := &scopeEntry{val: constVal, isAlloc: false}
			cg.curScope.set(vd.Name, entry)
			prevScope.set(pkgName+"."+vd.Name, entry)
			prevScope.set(pkgName+"::"+vd.Name, entry)
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
		if _, isStruct := cg.structTypes[canonicalKey]; isStruct {
			// Register package-qualified type alias for the caller.
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: canonicalKey}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: canonicalKey}
			}
			// Propagate any methods found in our child scope to prevScope.
			// Methods are registered under the canonical key.
			for key, entry := range cg.curScope.vars {
				if strings.HasPrefix(key, canonicalKey+"_") {
					prevScope.set(key, entry)
				}
			}
		} else if _, isStruct2 := cg.structTypes[name]; isStruct2 {
			// Bare-name struct (e.g. file-path import that ran before currentPkg was set).
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
			}
			for key, entry := range cg.curScope.vars {
				if strings.HasPrefix(key, name+"_") {
					prevScope.set(key, entry)
				}
			}
		}
		if _, isGeneric := cg.genericStructsByArity[name]; isGeneric {
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
			}
		}
	}

	// Pass 5: register exported macros under pkg-qualified keys so that
	// loadPackageSelective can find and re-register them as bare names.
	for _, node := range prog.Stmts {
		md, ok := node.(*ast.MacroDecl)
		if !ok {
			continue
		}
		bareName := strings.TrimSuffix(md.Name, "!")
		if !exportedNames[md.Name] && !exportedNames[bareName] {
			continue
		}
		// Register under pkg-qualified keys.
		cg.macros[pkgName+"."+bareName+"!"] = md
		cg.macros[pkgName+"::"+bareName+"!"] = md
		cg.macros[pkgName+"."+bareName] = md
		cg.macros[pkgName+"::"+bareName] = md
	}

	cg.curScope = prevScope
	cg.filename = prevFilename

	return nil
}

// loadPackageSelective loads a package and then registers only the named symbols
// as bare names in the current scope (no pkg:: prefix required).
// Used by `use { name1, name2! } from pkg` declarations.
func (cg *CodeGen) loadPackageSelective(path string, names []string, isFile bool) error {
	// Load the package normally (registers pkg-qualified names).
	var err error
	if isFile {
		err = cg.loadPackageFromFilePath(path)
	} else {
		err = cg.loadPackage(path)
	}
	if err != nil {
		return err
	}

	// Determine package name for scope lookups.
	parts := strings.Split(path, "::")
	pkgName := parts[len(parts)-1]
	// For file paths, derive from basename.
	if isFile {
		base := filepath.Base(path)
		pkgName = strings.TrimSuffix(strings.TrimSuffix(base, ".tin"), "./")
		if pkgName == "" {
			pkgName = base
		}
	}

	// Re-register each requested name as a bare (unqualified) name.
	for _, name := range names {
		bareName := strings.TrimSuffix(name, "!")
		isMacroCall := strings.HasSuffix(name, "!")

		if isMacroCall {
			// Macro: look up by pkg-qualified key and register as bare name.
			found := false
			for _, key := range []string{
				pkgName + "." + bareName + "!",
				pkgName + "::" + bareName + "!",
				pkgName + "." + bareName,
				pkgName + "::" + bareName,
			} {
				if m, ok := cg.macros[key]; ok {
					cg.macros[bareName+"!"] = m
					cg.macros[bareName] = m
					found = true

					break
				}
			}
			if !found {
				// Macro may not be in cg.macros yet; try to find it bare.
				if m, ok := cg.macros[bareName+"!"]; ok {
					cg.macros[bareName] = m
				}
			}

			continue
		}

		// Function: look up by pkg-qualified key and register as bare name.
		for _, key := range []string{
			pkgName + "." + bareName,
			pkgName + "::" + bareName,
		} {
			if entry, ok := cg.curScope.lookup(key); ok {
				cg.curScope.set(bareName, entry)

				break
			}
		}

		// Type/struct alias: look up pkg-qualified and register as bare.
		for _, key := range []string{
			pkgName + "::" + bareName,
			pkgName + "." + bareName,
		} {
			if te, ok := cg.typeAliases[key]; ok {
				if _, already := cg.typeAliases[bareName]; !already {
					cg.typeAliases[bareName] = te
				}

				break
			}
		}
	}

	return nil
}

// ScanImportedNoParensMacros scans a token stream for `use { names } from path`
// patterns, loads the corresponding .tin.mod files, and returns a map of
// macroName -> backtick_expansion for any #no_parens macros that would be
// selectively imported. Call this before Parse() so the parser can do token
// substitution for these macros.
func ScanImportedNoParensMacros(filename string, tokens []lexer.Token) map[string]string {
	result := map[string]string{}
	baseDir := filepath.Dir(filename)

	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type != lexer.KW_USE {
			continue
		}
		i++
		if i >= len(tokens) || tokens[i].Type != lexer.LBRACE {
			continue
		}
		i++
		// Collect names until RBRACE.
		var names []string
		for i < len(tokens) && tokens[i].Type != lexer.RBRACE {
			if tokens[i].Type == lexer.IDENT {
				name := tokens[i].Literal
				if i+1 < len(tokens) && tokens[i+1].Type == lexer.NOT {
					name += "!"
					i++
				}
				names = append(names, name)
			}
			i++ // advance (also skips commas)
		}
		if i >= len(tokens) || tokens[i].Type != lexer.RBRACE {
			continue
		}
		i++ // consume }
		// Expect soft keyword "from".
		if i >= len(tokens) || tokens[i].Type != lexer.IDENT || tokens[i].Literal != "from" {
			continue
		}
		i++ // consume "from"
		// Read module path.
		if i >= len(tokens) {
			continue
		}
		var pkgPath string
		if tokens[i].Type == lexer.STRING_LIT {
			pkgPath = tokens[i].Literal
		} else {
			var parts []string
			for i < len(tokens) && (tokens[i].Type == lexer.IDENT || tokens[i].Type == lexer.DCOLON) {
				if tokens[i].Type == lexer.IDENT {
					parts = append(parts, tokens[i].Literal)
				}
				i++
			}
			i-- // will be incremented by outer loop
			pkgPath = strings.Join(parts, "::")
		}
		if pkgPath == "" {
			continue
		}

		// Try to load the .tin.mod file and find #no_parens macros.
		pathParts := strings.Split(pkgPath, "::")
		pkgName := pathParts[len(pathParts)-1]

		// Build candidate .tin.mod paths (same logic as loadPackage).
		modFile := filepath.Join(append([]string{baseDir}, pathParts...)...) + ".tin.mod"
		var mf *ModFile
		mf, _ = ReadModFile(modFile)
		if mf == nil {
			if ex, exErr := os.Executable(); exErr == nil {
				execDir := filepath.Dir(ex)
				p1 := filepath.Join(append([]string{execDir, "stdlib"}, pathParts...)...) + ".tin.mod"
				mf, _ = ReadModFile(p1)
				if mf == nil {
					p2 := filepath.Join(execDir, "stdlib", pkgName, pkgName) + ".tin.mod"
					mf, _ = ReadModFile(p2)
				}
			}
		}
		if mf == nil {
			continue
		}

		// Find requested names that are #no_parens macros.
		nameSet := map[string]bool{}
		for _, n := range names {
			nameSet[strings.TrimSuffix(n, "!")] = true
		}
		for _, mm := range mf.Macros {
			if !nameSet[mm.Name] {
				continue
			}
			for _, tag := range mm.Tags {
				if tag == "no_parens" && mm.Body != "" {
					result[mm.Name] = mm.Body

					break
				}
			}
		}
	}

	return result
}

// ensureDefaultTraitMethods generates default (non-virtual) trait methods for
// concreteName if the struct doesn't already have them.  This is needed when a
// struct satisfies a constraint via the trait's default implementation without
// explicitly listing the trait in its Implements clause.
func (cg *CodeGen) ensureDefaultTraitMethods(concreteName string, traitExpr ast.TypeExpr) error {
	var traitName string
	switch te := traitExpr.(type) {
	case *ast.SimpleType:
		traitName = te.Name
	case *ast.GenericType:
		traitName = te.Name
	default:
		return nil
	}
	td, ok := cg.traits[traitName]
	if !ok {
		return nil
	}
	for _, m := range td.Methods {
		if m.IsVirtual || m.Body == nil {
			continue // virtual methods must be explicitly implemented
		}
		scopeKey := concreteName + "_" + m.Name
		if _, exists := cg.curScope.lookup(scopeKey); exists {
			continue // already generated
		}
		// Create a concrete copy of the method with this param bound to *concreteName.
		injected := *m
		ptrType := &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
		if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
			injected.Params = append([]ast.Param{{Name: "this", Type: ptrType}}, injected.Params...)
		} else {
			newParams := make([]ast.Param, len(injected.Params))
			copy(newParams, injected.Params)
			newParams[0] = ast.Param{Name: "this", Type: ptrType}
			injected.Params = newParams
		}
		// Pre-declare the stub in the current scope so genFuncDeclAs (line 677)
		// finds it via cg.curScope.vars[scopeKey] (direct map lookup, not parent-walk).
		// This also ensures that after generation the function lives in the global
		// scope (not just a temporary inner scope) by registering at every level.
		if err := cg.predeclareFuncAs(&injected, scopeKey); err != nil {
			return fmt.Errorf("ensureDefaultTraitMethods predeclare: %w", err)
		}
		// Walk to global scope and ensure the entry is also there (predeclareFuncAs
		// writes to cg.curScope which may be an inner function scope at this point).
		if entry, ok := cg.curScope.vars[scopeKey]; ok {
			global := cg.curScope
			for global.parent != nil {
				global = global.parent
			}
			global.set(scopeKey, entry)
		}
		if err := cg.genStructMethod(concreteName, &injected); err != nil {
			return fmt.Errorf("ensureDefaultTraitMethods: %w", err)
		}
	}

	return nil
}

// Constrained generic function monomorphization

// monomorphizeFunc compiles a concrete instance of a constrained generic
// function by substituting type-parameter names with concrete struct names.
//
// instKey is the unique suffix, e.g. "animal" for fn foo[t] with t->animal.
// typeSubst maps type-param names to concrete struct names: {"t": "animal"}.
func (cg *CodeGen) monomorphizeFunc(tmpl *ast.FuncDecl, instKey string, typeSubst map[string]string) (*ir.Func, error) {
	irName := tmpl.Name + "__" + instKey
	if f, ok := cg.constrainedFuncInstances[irName]; ok {
		return f, nil // already compiled (or forward-declared for recursive generics)
	}

	// Validate that each concrete type satisfies its declared constraints, and
	// ensure default (non-virtual) trait methods are available for the concrete type.
	for _, c := range tmpl.Constraints {
		concreteName, ok := typeSubst[c.TypeParam]
		if !ok {
			continue
		}
		for _, traitExpr := range c.Traits {
			if !cg.structSatisfiesConstraint(concreteName, traitExpr) {
				return nil, fmt.Errorf("fn %s: type %q does not satisfy constraint 'where %s is %s'",
					tmpl.Name, concreteName, c.TypeParam, typeExprToString(traitExpr))
			}
			// Inject any default (non-virtual) trait methods the concrete type
			// doesn't already implement (e.g. a struct satisfying a trait via its
			// default method without explicitly listing it in its Implements clause).
			if err := cg.ensureDefaultTraitMethods(concreteName, traitExpr); err != nil {
				return nil, err
			}
		}
	}

	// Build ast.TypeExpr substitution map.
	astSubst := make(map[string]ast.TypeExpr, len(typeSubst))
	for param, concrete := range typeSubst {
		astSubst[param] = &ast.SimpleType{Name: concrete}
	}

	// Substitute params.
	newParams := make([]ast.Param, len(tmpl.Params))
	for i, p := range tmpl.Params {
		newParams[i] = ast.Param{
			Name:      p.Name,
			Type:      substituteTypeInTypeExpr(p.Type, astSubst),
			IsVarArgs: p.IsVarArgs,
		}
	}

	// Substitute return type.
	newRet := substituteTypeInTypeExpr(tmpl.RetType, astSubst)

	// Build the concrete FuncDecl (no constraints, no type params).
	concrete := &ast.FuncDecl{
		Name:    irName,
		Params:  newParams,
		RetType: newRet,
		Body:    tmpl.Body,
		Tags:    tmpl.Tags,
	}

	// Save/restore scope so the monomorphization gets a fresh inner scope.
	// We root the new scope at the function's home scope (the package scope where
	// the template was declared), so that bare local names (e.g. `parse` inside
	// json.tin's decode[T]) resolve correctly. If no home scope is recorded, fall
	// back to moduleScope so that at least package-exported names are visible.
	prevScope := cg.curScope
	baseScope, hasHome := cg.genericFuncHomeScopes[tmpl.Name]
	if !hasHome || baseScope == nil {
		baseScope = cg.moduleScope
	}
	if baseScope == nil {
		baseScope = cg.curScope
		for baseScope.parent != nil {
			baseScope = baseScope.parent
		}
	}
	cg.curScope = newScope(baseScope)

	// Register type aliases so that body expressions referring to the type
	// param (e.g. as a variable type annotation) resolve to the concrete type.
	// Save previous values so they can be restored after compilation - stale
	// aliases from one monomorphization must not bleed into the next.
	prevAliases := make(map[string]ast.TypeExpr, len(astSubst))
	for param, concreteTE := range astSubst {
		if old, had := cg.typeAliases[param]; had {
			prevAliases[param] = old
		}
		cg.typeAliases[param] = concreteTE
	}

	// Pre-declare the function signature (no body yet) so that recursive calls
	// inside the body - e.g. a self-recursive generic like _encode_any[T] - can
	// resolve to a forward declaration rather than triggering recursive instantiation.
	if err := cg.predeclareFuncAs(concrete, irName); err != nil {
		cg.curScope = prevScope
		for param := range astSubst {
			if old, had := prevAliases[param]; had {
				cg.typeAliases[param] = old
			} else {
				delete(cg.typeAliases, param)
			}
		}

		return nil, err
	}
	// Register the forward declaration immediately in constrainedFuncInstances so
	// that any re-entrant monomorphizeFunc call for the same irName returns it.
	for _, f := range cg.mod.Funcs {
		if f.Name() == irName {
			cg.constrainedFuncInstances[irName] = f

			break
		}
	}

	if err := cg.genFuncDeclAs(concrete, irName); err != nil {
		cg.curScope = prevScope
		for param := range astSubst {
			if old, had := prevAliases[param]; had {
				cg.typeAliases[param] = old
			} else {
				delete(cg.typeAliases, param)
			}
		}

		return nil, err
	}

	// Restore type aliases - must happen before restoring curScope so that any
	// scope-sensitive alias lookups during cleanup see the original state.
	for param := range astSubst {
		if old, had := prevAliases[param]; had {
			cg.typeAliases[param] = old
		} else {
			delete(cg.typeAliases, param)
		}
	}

	cg.curScope = prevScope

	// Find the compiled function (now has a body).
	var compiled *ir.Func
	for _, f := range cg.mod.Funcs {
		if f.Name() == irName {
			compiled = f

			break
		}
	}
	if compiled == nil {
		return nil, fmt.Errorf("monomorphize %s: compiled function not found", irName)
	}
	cg.constrainedFuncInstances[irName] = compiled

	return compiled, nil
}

// inferTypeArgs maps type-parameter names to concrete struct names given the
// actual argument LLVM types at a call site.
func (cg *CodeGen) inferTypeArgs(tmpl *ast.FuncDecl, argVals []value.Value) map[string]string {
	subst := make(map[string]string)
	for i, p := range tmpl.Params {
		if i >= len(argVals) {
			break
		}
		cg.inferTypeArgsFromParam(p.Type, argVals[i].Type(), tmpl.TypeParams, subst)
	}

	return subst
}

// inferTypeArgsFromParam recursively matches an AST parameter type against an
// LLVM argument type to infer type-parameter bindings.  Handles:
//   - Direct type-param: fn foo[t](x t)   arg: i64      -> t=i64
//   - Pointer-to-param:  fn foo[t](x *t)  arg: *struct  -> t=struct
//   - Generic struct:    fn foo[t](x S[t]) arg: S__i64   -> t=i64
//   - Pointer-to-generic fn foo[t](x *S[t]) arg: *S__i64 -> t=i64
func (cg *CodeGen) inferTypeArgsFromParam(paramType ast.TypeExpr, argType irtypes.Type, typeParams []string, subst map[string]string) {
	switch pt := paramType.(type) {
	case *ast.SimpleType:
		// Direct type-param binding: fn foo[t](x t)
		for _, tp := range typeParams {
			if pt.Name == tp {
				name := cg.typeNameOf(argType)
				if name == "" {
					if ptr, ok2 := argType.(*irtypes.PointerType); ok2 {
						if st2, ok3 := ptr.ElemType.(*irtypes.StructType); ok3 {
							name = st2.Name()
						}
					}
				}
				if name == "" {
					name = llvmTypeName(argType)
				}
				if name != "" {
					subst[tp] = name
				}
			}
		}
	case *ast.PointerType:
		// Unwrap pointer on both sides and recurse.
		if ptr, ok := argType.(*irtypes.PointerType); ok {
			cg.inferTypeArgsFromParam(pt.Elem, ptr.ElemType, typeParams, subst)
		}
	case *ast.GenericType:
		// Generic struct: fn foo[t](x S[t])  arg LLVM type is "S__i64"
		// Handles nested cases too: fn foo[t](x S[S[t]]) arg "S__S__i64" -> t=i64.
		if len(pt.TypeParams) != 1 {
			break
		}
		// Get the concrete LLVM struct name (e.g. "box__box__i64").
		structName := ""
		if st, ok2 := argType.(*irtypes.StructType); ok2 {
			structName = st.Name()
		}
		if structName == "" {
			break
		}
		// Strip the outer "GenericTypeName__" prefix to get the inner concrete part.
		prefix := pt.Name + "__"
		if !strings.HasPrefix(structName, prefix) {
			break
		}
		innerName := strings.TrimPrefix(structName, prefix)
		innerParam := pt.TypeParams[0]
		if simpleInner, ok := innerParam.(*ast.SimpleType); ok {
			// Direct type param: bind it to the inner concrete name.
			for _, tp := range typeParams {
				if simpleInner.Name == tp {
					subst[tp] = innerName

					break
				}
			}
		} else {
			// Nested generic (e.g. S[S[t]]): look up the inner struct type and recurse.
			if innerST, ok := cg.structTypes[innerName]; ok {
				cg.inferTypeArgsFromParam(innerParam, innerST, typeParams, subst)
			}
		}
	case *ast.ArrayType:
		// [t] → { t*, i64 } in LLVM.  Extract the element type from field 0.
		if st, ok := argType.(*irtypes.StructType); ok && len(st.Fields) >= 2 {
			if ptrField, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
				cg.inferTypeArgsFromParam(pt.Elem, ptrField.ElemType, typeParams, subst)
			}
		}
	case *ast.FuncType:
		// fn(params...) retType - extract type-param bindings from return type and params.
		// The LLVM representation is a fat function pointer {fn_ptr*, i8*}.
		if !isFatFnPtr(argType) {
			break
		}
		st := argType.(*irtypes.StructType)
		innerFnType, ok := st.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		if !ok {
			break
		}
		// Match return type (LLVM env param is at index 0, so user params start at 1).
		if pt.RetType != nil && innerFnType.RetType != nil {
			cg.inferTypeArgsFromParam(pt.RetType, innerFnType.RetType, typeParams, subst)
		}
		// Match parameter types (skip env param at LLVM index 0).
		for i, astParam := range pt.Params {
			llIdx := i + 1
			if llIdx < len(innerFnType.Params) {
				cg.inferTypeArgsFromParam(astParam, innerFnType.Params[llIdx], typeParams, subst)
			}
		}
	}
}

// extractBacktickBody returns the raw string from a BacktickLit node, or from
// a ReturnStmt wrapping one. Used when emitting macros to .tin.mod.
func extractBacktickBody(node ast.Node) (string, bool) {
	switch n := node.(type) {
	case *ast.BacktickLit:

		return n.Content, true
	case *ast.ReturnStmt:
		if n.Value != nil {
			return extractBacktickBody(n.Value)
		}
	case *ast.Block:
		if len(n.Stmts) == 1 {
			return extractBacktickBody(n.Stmts[0])
		}
	case *ast.ExprStmt:

		return extractBacktickBody(n.Expr)
	}

	return "", false
}

// writeModuleFiles writes one .tin.mod file per ExportDecl found in prog.
func (cg *CodeGen) writeModuleFiles(prog *ast.Program) error {
	// Group exports by package name.
	type exportGroup struct {
		names []string
	}
	groups := map[string]*exportGroup{}
	for _, node := range prog.Stmts {
		exp, ok := node.(*ast.ExportDecl)
		if !ok || exp.AsName == "" {
			continue
		}
		g := groups[exp.AsName]
		if g == nil {
			g = &exportGroup{}
			groups[exp.AsName] = g
		}
		g.names = append(g.names, exp.Names...)
	}
	if len(groups) == 0 {
		return nil
	}

	// Collect all top-level function, struct, type, and macro declarations by name.
	funcsDecl := map[string]*ast.FuncDecl{}
	structsDecl := map[string]*ast.StructDecl{}
	typesDecl := map[string]*ast.TypeDecl{}
	macrosDecl := map[string]*ast.MacroDecl{}
	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			funcsDecl[n.Name] = n
		case *ast.StructDecl:
			structsDecl[n.Name] = n
		case *ast.TypeDecl:
			typesDecl[n.Name] = n
		case *ast.MacroDecl:
			macrosDecl[n.Name] = n
			// Also register without trailing ! so it can be looked up by bare name.
			bare := strings.TrimSuffix(n.Name, "!")
			if bare != n.Name {
				macrosDecl[bare] = n
			}
		}
	}

	baseDir := filepath.Dir(cg.filename)

	for pkgName, g := range groups {
		mf := &ModFile{Package: pkgName}

		for _, name := range g.names {
			// Check if it's a function.
			if fd, ok := funcsDecl[name]; ok {
				mfn := ModFunc{
					LocalName:  name,
					IRName:     pkgName + "__" + name,
					ExternName: fd.IsExtern,
				}
				for _, p := range fd.Params {
					if p.IsVarArgs {
						mfn.Variadic = true

						continue
					}
					mfn.Params = append(mfn.Params, ModParam{
						Name: p.Name,
						Type: typeExprToString(p.Type),
					})
				}
				mfn.RetType = typeExprToString(fd.RetType)
				mf.Funcs = append(mf.Funcs, mfn)

				continue
			}
			// Check if it's a struct.
			if sd, ok := structsDecl[name]; ok {
				ms := ModStruct{
					LocalName: name,
					IRName:    name, // structs use local name (structural typing for ABI)
				}
				for _, f := range sd.Fields {
					ms.Fields = append(ms.Fields, ModParam{
						Name: f.Name,
						Type: typeExprToString(f.Type),
					})
				}
				// Include methods so importers can call them.
				for _, m := range sd.Methods {
					if m.IsVirtual {
						continue
					}
					mfn := ModFunc{
						LocalName: m.Name,
						IRName:    name + "_" + m.Name, // e.g. Point_show
						Variadic:  false,
					}
					for _, p := range m.Params {
						mfn.Params = append(mfn.Params, ModParam{
							Name: p.Name,
							Type: typeExprToString(p.Type),
						})
					}
					mfn.RetType = typeExprToString(m.RetType)
					ms.Methods = append(ms.Methods, mfn)
				}
				mf.Structs = append(mf.Structs, ms)

				continue
			}
			// Check if it's a type alias.
			if td, ok := typesDecl[name]; ok {
				mf.Types = append(mf.Types, ModTypeAlias{
					Name:   name,
					Target: typeExprToString(td.Type),
				})

				continue
			}
			// Check if it's a macro (bare name or with !).
			md, isMacro := macrosDecl[name]
			if !isMacro {
				md, isMacro = macrosDecl[strings.TrimSuffix(name, "!")]
			}
			if isMacro {
				mm := ModMacro{
					Name:   strings.TrimSuffix(md.Name, "!"),
					Tags:   md.Tags,
					Params: md.Params,
				}
				// Extract backtick body for #no_parens macros.
				if body, ok2 := extractBacktickBody(md.Body); ok2 {
					mm.Body = body
				}
				mf.Macros = append(mf.Macros, mm)

				continue
			}
			// Might be a re-exported package (e.g. export { io, math } as std).
			mf.ReExports = append(mf.ReExports, name)
		}

		outPath := filepath.Join(baseDir, pkgName+".tin.mod")
		if err := WriteModFile(outPath, mf); err != nil {
			return fmt.Errorf("write module file %s: %w", outPath, err)
		}
	}

	return nil
}
