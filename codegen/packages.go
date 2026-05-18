package codegen

import (
	"fmt"
	"math/big"
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
		// genUseDecl is reached twice per UseDecl - once during the
		// dedicated "load packages" pass, again when codegen iterates
		// top-level statements. The actual load is dedup'd by
		// importedPkgs / loadedSrcPaths, but the progress message has
		// no such guard. Track per-codegen-run so each import surfaces
		// exactly once in the -v stream.
		if cg.reportedImports == nil {
			cg.reportedImports = make(map[string]bool)
		}

		if !cg.reportedImports[n.Path] {
			cg.reportedImports[n.Path] = true

			cg.progress("import " + n.Path)
		}

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

		var (
			tinRetType irtypes.Type = irtypes.Void
			cRetType   irtypes.Type = irtypes.Void
		)

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

		// sret: structs > 16 bytes use a hidden pointer argument (AMD64: rdi, ARM64: x8).
		var pkgCRetSRetSt *irtypes.StructType

		if cg.targetIsAMD64() || cg.targetIsARM64() {
			if nativeSt, ok := cRetType.(*irtypes.StructType); ok && nativeStructNeedsByval(nativeSt) {
				pkgCRetSRetSt = nativeSt
				sretParam := ir.NewParam(".sret", irtypes.NewPointer(nativeSt))
				sretParam.Attrs = append(sretParam.Attrs, ir.SRet{Typ: nativeSt})
				cParams = append([]*ir.Param{sretParam}, cParams...)
				cRetType = irtypes.Void
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

		// Check if any parameter is a named Tin struct needing conversion.
		needsStructConv := false

		for _, p := range ft.Params {
			if _, isStruct := cg.isNamedTinStruct(p); isStruct {
				needsStructConv = true

				break
			}
		}

		if ft.RetType != nil {
			if _, isStruct := cg.isNamedTinStruct(ft.RetType); isStruct {
				needsStructConv = true
			}
		}

		// If no conversion needed, expose C function directly.
		if cRetType.Equal(tinRetType) && !needsStructConv {
			cg.curScope.set(imp.LocalName, &scopeEntry{val: cFunc, isAlloc: false})

			continue
		}

		// Generate a wrapper that converts Tin structs to C-native layout
		// and/or wraps the return type.
		wrapperName := "__tinwrap_" + imp.LocalName

		var wrapperFn *ir.Func

		for _, f := range cg.mod.Funcs {
			if f.Name() == wrapperName {
				wrapperFn = f

				break
			}
		}

		if wrapperFn == nil {
			// Wrapper params use Tin-level types for struct params so callers
			// pass full Tin structs; the wrapper converts to native layout.
			wrapperParams := make([]*ir.Param, len(cParams))

			for i, p := range ft.Params {
				if sName, isStruct := cg.isNamedTinStruct(p); isStruct {
					tinType, _ := cg.tinTypeToLLVM(p)
					wrapperParams[i] = ir.NewParam(sName, tinType)
				} else {
					wrapperParams[i] = cParams[i]
				}
			}

			wrapperFn = cg.mod.NewFunc(wrapperName, tinRetType, wrapperParams...)
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")

			// sret offset: regular args start at index 1 when sret is used.
			sretOff := 0

			var pkgSretAlloca value.Value

			if pkgCRetSRetSt != nil {
				sretOff = 1
			}

			callArgs := make([]value.Value, len(wrapperFn.Params)+sretOff)

			if pkgCRetSRetSt != nil {
				pkgSretAlloca = entry.NewAlloca(pkgCRetSRetSt)
				callArgs[0] = ir.NewArg(pkgSretAlloca, ir.SRet{Typ: pkgCRetSRetSt})
			}

			for i, p := range wrapperFn.Params {
				// cParams index for regular args: i + sretOff (sret is at cParams[0]).
				cIdx := i + sretOff
				if sName, isStruct := cg.isNamedTinStruct(ft.Params[i]); isStruct {
					native, convErr := cg.wrapStructToExtern(entry, p, sName)
					if convErr != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return convErr
					}
					// If the C param was coerced to an integer (small
					// all-integer struct), bitcast through memory.
					if intTy, isInt := cParams[cIdx].Type().(*irtypes.IntType); isInt {
						if nativeSt, ok2 := native.Type().(*irtypes.StructType); ok2 {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							if structBits < intTy.BitSize {
								// Coerced type is wider than struct (ARM64: <=8-byte
								// struct -> i64). Load at struct's natural bit size
								// to avoid an out-of-bounds read, then zero-extend.
								smallTy := irtypes.NewInt(structBits)
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(smallTy))
								small := entry.NewLoad(smallTy, ip)
								native = entry.NewZExt(small, intTy)
							} else {
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(intTy))
								native = entry.NewLoad(intTy, ip)
							}
						}
					}

					callArgs[i+sretOff] = native
				} else {
					callArgs[i+sretOff] = p
				}
			}

			if irtypes.IsVoid(cRetType) && pkgCRetSRetSt == nil {
				entry.NewCall(cFunc, callArgs...)
				entry.NewRet(nil)
			} else {
				rawCall := entry.NewCall(cFunc, callArgs...)
				// AMD64 sret: load the actual result from the pre-allocated buffer.
				var raw value.Value = rawCall
				if pkgCRetSRetSt != nil {
					raw = entry.NewLoad(pkgCRetSRetSt, pkgSretAlloca)
				}

				if sName, isStruct := cg.isNamedTinStruct(ft.RetType); isStruct {
					// If C returned a coerced integer (ARM64: i64, AMD64: i32),
					// convert it back to the native struct type before wrapping.
					nativeRaw := raw
					if intTy, isInt := raw.Type().(*irtypes.IntType); isInt {
						if nativeSt, err2 := cg.tinStructNativeLLVM(sName); err2 == nil {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							nativeAlloca := entry.NewAlloca(nativeSt)

							if structBits < intTy.BitSize {
								smallTy := irtypes.NewInt(structBits)
								truncated := entry.NewTrunc(raw, smallTy)
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(smallTy))
								entry.NewStore(truncated, ip)
							} else {
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(intTy))
								entry.NewStore(raw, ip)
							}

							nativeRaw = entry.NewLoad(nativeSt, nativeAlloca)
						}
					}

					tinResult, convErr := cg.wrapNativeStructToTin(entry, nativeRaw, sName)
					if convErr != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return convErr
					}

					entry.NewRet(tinResult)
				} else if pkgCRetSRetSt == nil {
					entry.NewRet(cg.wrapFromExtern(entry, raw, tinRetType, false))
				} else {
					// Void-returning wrapper after sret load: shouldn't normally
					// reach here unless the return type has no named struct.
					entry.NewRet(cg.wrapFromExtern(entry, raw, tinRetType, false))
				}
			}

			cg.curFn = prevFn
			cg.curScope = prevScope
		}

		cg.curScope.set(imp.LocalName, &scopeEntry{val: wrapperFn, isAlloc: false})
	}

	return nil
}

// normalizePkgDisplayPath returns the canonical user-facing package path for
// typeof() display names.  File-path imports ("./foo.tin") use just the bare
// package name.  "std::" prefix is stripped so `use std::io` and `use io`
// both display as "io".  Multi-part paths are preserved: "encoding::base16"
// stays "encoding::base16".
func normalizePkgDisplayPath(pkgPath, pkgName string) string {
	if strings.ContainsAny(pkgPath, "/\\") || strings.HasSuffix(pkgPath, ".tin") {
		return pkgName
	}

	if strings.HasPrefix(pkgPath, "std::") {
		return pkgPath[len("std::"):]
	}

	return pkgPath
}

// loadPackage resolves and compiles the .tin source file for the given package
// path. Search order: stdlib/ (always first), libs/ roots, then local directory
// next to the importing source file.
//
// pkgPath uses "::" as separator, e.g. "io", "std::io", "encoding::json".
// Bare names ("io") and "std::"-prefixed names ("std::io") are equivalent and
// both resolve into stdlib/ first.
func (cg *CodeGen) loadPackage(pkgPath string) error {
	if pkgPath == "" {
		return nil
	}

	parts := strings.Split(pkgPath, "::")
	pkgName := parts[len(parts)-1] // last segment

	// Normalize dedup key: bare "io" == "std::io" both stored as "io".
	stdParts := parts
	if len(parts) > 1 && parts[0] == "std" {
		stdParts = parts[1:]
	}

	dedupKey := strings.Join(stdParts, "::")

	if cg.importedPkgs[dedupKey] {
		return nil
	}

	cg.importedPkgs[dedupKey] = true

	// Find the .tin source file using the 3-tier search.
	tinSrc := resolvePackageSrc(pkgPath, cg.stdlibBase(), filepath.Dir(cg.filename), cg.libsRoots)

	if tinSrc != "" {
		return cg.loadPackageFromSource(pkgPath, pkgName, tinSrc)
	}

	// No direct file found for a multi-part path (e.g. hash::fnv).
	// Load the parent module (e.g. hash) which may re-export fnv as a
	// sub-namespace. If even the parent doesn't exist, surface a clear
	// error pointing at the original path (closer to the user's intent
	// than the parent name).
	if len(parts) > 1 {
		parentPath := strings.Join(parts[:len(parts)-1], "::")
		if err := cg.loadPackage(parentPath); err != nil {
			return fmt.Errorf("package not found: %s (also tried parent %s)", pkgPath, parentPath)
		}

		return nil
	}

	// Package not found at all. Pre-2026 the compiler silently ignored
	// these on the theory "errors surface when the symbol is used", but
	// imports for side effects (macros, top-level inits, runtime
	// registrations) never reference a symbol, and typos in the REPL
	// went undetected. Hard error is the safe default.
	return fmt.Errorf("package not found: %s", pkgPath)
}

// resolvePackageSrc finds the .tin source file for pkgPath using a 3-tier search:
// 1. stdlib/ (always searched first; bare names and std:: prefixed both resolve here)
// 2. libs/ roots
// 3. localDir (next to the importing source file)
//
// Returns the absolute path of the first match, or "" if not found.
func resolvePackageSrc(pkgPath, stdlibBase, localDir string, libsRoots []string) string {
	// Auto-detect defaults when not provided.
	if stdlibBase == "" {
		if ex, err := os.Executable(); err == nil {
			stdlibBase = filepath.Join(filepath.Dir(ex), "stdlib")
		}
	}

	if libsRoots == nil {
		if ex, err := os.Executable(); err == nil {
			libsRoots = []string{filepath.Join(filepath.Dir(ex), "libs")}
		}
	}

	parts := strings.Split(pkgPath, "::")
	pkgName := parts[len(parts)-1]

	// For stdlib and local searches, strip a leading "std::" prefix so that
	// "std::io" and "io" both resolve to stdlib/io.
	stdParts := parts
	if len(parts) > 1 && parts[0] == "std" {
		stdParts = parts[1:]
	}

	var candidates []string

	// 1. stdlib/ - always searched first.
	if stdlibBase != "" {
		parentParts := stdParts[:len(stdParts)-1]
		stdDir := filepath.Join(append([]string{stdlibBase}, parentParts...)...)
		candidates = append(candidates,
			filepath.Join(stdDir, pkgName+".tin"),
			filepath.Join(stdDir, pkgName, pkgName+".tin"),
		)
	}

	// 2. libs/ roots - searched for all names using the original (non-stripped) path.
	for _, root := range libsRoots {
		parentParts := parts[:len(parts)-1]
		libDir := filepath.Join(append([]string{root}, parentParts...)...)
		candidates = append(candidates,
			filepath.Join(libDir, pkgName+".tin"),
			filepath.Join(libDir, pkgName, pkgName+".tin"),
		)
	}

	// 3. Local - next to the importing source file.
	if localDir != "" {
		parentParts := stdParts[:len(stdParts)-1]
		locDir := filepath.Join(append([]string{localDir}, parentParts...)...)
		candidates = append(candidates,
			filepath.Join(locDir, pkgName+".tin"),
			filepath.Join(locDir, pkgName, pkgName+".tin"),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	return ""
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
			cg.typeAliases[sd.Name] = &ast.SimpleType{Name: cg.pkgStructKey(sd.Name)}
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

	cg.curScope = prevScope
	cg.filename = prevFilename

	return nil
}

// loadPackageFromSource compiles a .tin source file inline into the current
// module, prefixing all function IR names with pkgName+"__". This lets stdlib
// packages be written in pure Tin (with only the truly native bits remaining in
// runtime.c) without requiring a separate linking step.
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
		if _, stOk := cg.structTypes[structKey]; stOk {
			// Non-generic struct with canonical key.
			prevScope.set(pkgName+"::"+sd.Name, &scopeEntry{val: nil, isAlloc: false})
			cg.typeAliases[pkgName+"::"+sd.Name] = &ast.SimpleType{Name: structKey}
			cg.typeAliases[pkgName+"."+sd.Name] = &ast.SimpleType{Name: structKey}
			// Always update the bare-name alias to the current package's struct so
			// intra-package code (pass 3 bodies) resolves the correct type even
			// when multiple packages loaded in the same scope share a type name.
			cg.typeAliases[sd.Name] = &ast.SimpleType{Name: structKey}
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

	// Pass 1.7: register top-level var declarations in the package as LLVM
	// globals now that struct layouts are complete, so any struct-typed
	// initializer can fold with the final field layout. Exported vars also
	// become reachable to the caller as `pkg::name` / `pkg.name`.
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
		if _, isStruct := cg.structTypes[canonicalKey]; isStruct {
			// Register package-qualified type alias for the caller.
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: canonicalKey}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: canonicalKey}
			}
			// Propagate any methods found in our child scope to prevScope.
			// Methods are registered under the canonical key.
			cg.curScope.each(func(key string, entry *scopeEntry) {
				if strings.HasPrefix(key, canonicalKey+"_") {
					prevScope.set(key, entry)
				}
			})
		} else if _, isStruct2 := cg.structTypes[name]; isStruct2 {
			// Bare-name struct (e.g. file-path import that ran before currentPkg was set).
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
			}

			cg.curScope.each(func(key string, entry *scopeEntry) {
				if strings.HasPrefix(key, name+"_") {
					prevScope.set(key, entry)
				}
			})
		}

		if _, isGeneric := cg.genericStructsByArity[name]; isGeneric {
			if _, alreadySet := cg.typeAliases[pkgName+"::"+name]; !alreadySet {
				cg.typeAliases[pkgName+"::"+name] = &ast.SimpleType{Name: name}
				cg.typeAliases[pkgName+"."+name] = &ast.SimpleType{Name: name}
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
func (cg *CodeGen) validatePackageExports(pkgName string, exportedNames map[string]bool) error {
	for name := range exportedNames {
		if cg.isVariantOfExportedAdt(name, exportedNames) {
			return fmt.Errorf("export of %q: ADT variant constructors are inferred from the parent ADT; remove %q from `export { ... } as %s`",
				name, name, pkgName)
		}

		if cg.isExportable(name, pkgName) {
			continue
		}

		return fmt.Errorf("export of %q: no top-level decl (struct, data, enum, trait, fn, macro) named %q is visible in package %q",
			name, name, pkgName)
	}

	return nil
}

// isVariantOfExportedAdt reports whether name is a variant of some
// ADT in the same export list. Used to flag redundant variant
// exports.
func (cg *CodeGen) isVariantOfExportedAdt(name string, exportedNames map[string]bool) bool {
	owners := cg.dataVariantLookup[name]
	for _, adt := range owners {
		// Strip the leading "pkg__" prefix so we compare against the
		// user-facing source name.
		bare := adt
		if idx := strings.Index(adt, "__"); idx >= 0 {
			bare = adt[idx+2:]
		}

		if exportedNames[bare] || exportedNames[adt] {
			return true
		}
	}

	return false
}

// isExportable reports whether name refers to a top-level decl that
// can appear in an export list.
func (cg *CodeGen) isExportable(name, pkgName string) bool {
	if _, ok := cg.structTypes[name]; ok {
		return true
	}

	if _, ok := cg.structTypes[pkgName+"__"+name]; ok {
		return true
	}

	if _, ok := cg.genericStructsByArity[name]; ok {
		return true
	}

	if _, ok := cg.dataDecls[name]; ok {
		return true
	}

	if _, ok := cg.dataDecls[pkgName+"__"+name]; ok {
		return true
	}

	if _, ok := cg.enumTypes[name]; ok {
		return true
	}

	if _, ok := cg.traits[name]; ok {
		return true
	}

	if _, ok := cg.funcDecls[name]; ok {
		return true
	}

	if _, ok := cg.overloads[name]; ok {
		return true
	}

	if _, ok := cg.constrainedFuncs[name]; ok {
		return true
	}

	if _, ok := cg.genericFuncs[name]; ok {
		return true
	}

	if _, ok := cg.macros[name]; ok {
		return true
	}
	// Macros are stored in cg.macros under multiple keys depending
	// on the load path: bare name (from direct file-path imports),
	// `pkg::name`, `pkg.name`, with and without a trailing `!`.
	// Look through every shape that could match this export name.
	bare := strings.TrimSuffix(name, "!")
	if _, ok := cg.macros[bare]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"::"+bare]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"::"+bare+"!"]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"."+bare]; ok {
		return true
	}

	if _, ok := cg.typeAliases[name]; ok {
		return true
	}

	if _, ok := cg.curScope.lookup(name); ok {
		return true
	}
	// Re-exported child-package names (e.g. `export { log } as std`
	// where log is itself a package): a name matches if any macro or
	// type alias is registered under `name::`.
	for k := range cg.macros {
		if strings.HasPrefix(k, name+"::") {
			return true
		}
	}

	for k := range cg.typeAliases {
		if strings.HasPrefix(k, name+"::") {
			return true
		}
	}

	return false
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
		// Mark bareName visible as a type so the strict-bare-type
		// resolver lets `Name` resolve through cg.dataDecls /
		// cg.structTypes / cg.enumTypes / cg.traits. Without this,
		// `use { Result } from result` only registers an alias for
		// non-ADT shapes; ADTs live in cg.dataDecls (flat global
		// namespace) and only the visibility set gates bare access.
		if cg.isTypeName(bareName, pkgName) {
			cg.curScope.markTypeVisible(bareName)
		}
	}

	return nil
}

// isTypeName reports whether bareName names a top-level type (data,
// struct, enum, trait, type alias) - either as the raw bare key or as
// the package-prefixed key. Used by selective-import to decide whether
// to add the name to the importer's visibleTypes set.
func (cg *CodeGen) isTypeName(bareName, pkgName string) bool {
	if _, ok := cg.dataDecls[bareName]; ok {
		return true
	}

	if _, ok := cg.dataDecls[pkgName+"__"+bareName]; ok {
		return true
	}

	if _, ok := cg.structTypes[bareName]; ok {
		return true
	}

	if _, ok := cg.structTypes[pkgName+"__"+bareName]; ok {
		return true
	}

	if _, ok := cg.genericStructsByArity[bareName]; ok {
		return true
	}

	if _, ok := cg.enumTypes[bareName]; ok {
		return true
	}

	if _, ok := cg.traits[bareName]; ok {
		return true
	}

	if _, ok := cg.typeAliases[bareName]; ok {
		return true
	}

	if _, ok := cg.typeAliases[pkgName+"::"+bareName]; ok {
		return true
	}

	if _, ok := cg.typeAliases[pkgName+"."+bareName]; ok {
		return true
	}

	return false
}

// ScanImportedNoParensMacros scans a token stream for `use { names } from path`
// patterns, loads the corresponding .tin.mod files, and returns a map of
// macroName -> backtick_expansion for any #no_parens macros that would be
// selectively imported. Call this before Parse() so the parser can do token
// substitution for these macros.
func ScanImportedNoParensMacros(currentFile string, tokens []lexer.Token, stdlibBase string, libsRoots []string) map[string]string {
	result := map[string]string{}

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

		nameSet := map[string]bool{}
		for _, n := range names {
			nameSet[strings.TrimSuffix(n, "!")] = true
		}

		localDir := ""
		if currentFile != "" {
			localDir = filepath.Dir(currentFile)
		}

		srcFile := resolvePackageSrc(pkgPath, stdlibBase, localDir, libsRoots)
		if srcFile != "" {
			if srcBytes, readErr := os.ReadFile(srcFile); readErr == nil {
				scanNoParensMacrosFromSource(srcBytes, nameSet, result)
			}
		}
	}

	return result
}

// scanNoParensMacrosFromSource scans raw .tin source bytes for macro declarations
// with a #no_parens tag, populating result with name->backtick_body for any
// names in the nameSet. Used as a fallback when no .tin.mod file is available.
func scanNoParensMacrosFromSource(src []byte, nameSet map[string]bool, result map[string]string) {
	l := lexer.New(string(src))

	tokens, err := l.Tokenize()
	if err != nil {
		return
	}

	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type != lexer.KW_MACRO {
			continue
		}

		// Expect optional tag block: { #tag ... }
		i++
		if i >= len(tokens) || tokens[i].Type != lexer.LBRACE {
			continue
		}

		var hasNoParens bool

		i++
		for i < len(tokens) && tokens[i].Type != lexer.RBRACE {
			if tokens[i].Type == lexer.CONTROL_TAG && tokens[i].Literal == "no_parens" {
				hasNoParens = true
			}

			i++
		}

		if i >= len(tokens) {
			break
		}

		// Consume `}` then read macro name.
		i++
		if i >= len(tokens) || tokens[i].Type != lexer.IDENT {
			continue
		}

		macroName := tokens[i].Literal
		if !nameSet[macroName] {
			continue
		}

		// Scan forward for `=` then a BACKTICK_LIT body.
		for i < len(tokens) && tokens[i].Type != lexer.ASSIGN {
			i++
		}

		i++
		if i >= len(tokens) || tokens[i].Type != lexer.BACKTICK_LIT {
			continue
		}

		if hasNoParens && tokens[i].Literal != "" {
			result[macroName] = tokens[i].Literal
		}
	}
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
	// Disambiguate generic free-fn overloads that share a base name + type
	// args but differ in arity / param shape (e.g. `unwrap[t](r)` vs
	// `unwrap[t](r, msg)`).  Without the suffix, both monomorphizations
	// collapse to the same IR symbol and the cache returns whichever was
	// compiled first, ignoring the caller's arg count.
	if overloads := cg.genericFuncOverloads[tmpl.Name]; len(overloads) > 1 {
		irName += "__" + funcParamSig(tmpl.Params)
	}

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

		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return nil, fmt.Errorf("%d:%d: fn %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, tmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
		// Inject default (non-virtual) trait methods for every positive leaf
		// of the bound so the instantiation has the methods it needs.
		for _, traitExpr := range flattenPositiveTraits(c.Bound) {
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
	// Walk allFuncs() (cg.mod + per-pkg modules) because predeclareFuncAs routes
	// new fns through cg.activeModule(), which lands them in the per-pkg module
	// for the package currently being compiled.
	for _, f := range cg.allFuncs() {
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

	// Find the compiled function (now has a body). Walk allFuncs() so we
	// see fns in per-pkg modules too - the body emit went via the same
	// activeModule() routing as the forward declaration above.
	var compiled *ir.Func

	for _, f := range cg.allFuncs() {
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
	// Track whether each bound type param came from a constant (literal) argument.
	// Non-constant (runtime) bindings take priority over constant-derived ones.
	fromConst := make(map[string]bool)

	// Two-pass: first bind from runtime expressions, then fill gaps from constants.
	for pass := 0; pass < 2; pass++ {
		for i, p := range tmpl.Params {
			if i >= len(argVals) {
				break
			}

			_, isConst := argVals[i].(constant.Constant)

			if pass == 0 && isConst {
				continue // first pass: skip constants
			}

			if pass == 1 && !isConst {
				continue // second pass: skip non-constants
			}

			cg.inferTypeArgsFromParamPrio(p.Type, argVals[i].Type(), tmpl.TypeParams, subst, fromConst, isConst)
		}
	}

	return subst
}

// inferTypeArgsFromParamPrio is like inferTypeArgsFromParam but respects a priority rule:
// a binding derived from a runtime (non-constant) argument always wins over one derived
// from a literal constant.
func (cg *CodeGen) inferTypeArgsFromParamPrio(paramType ast.TypeExpr, argType irtypes.Type, typeParams []string, subst map[string]string, fromConst map[string]bool, isConst bool) {
	switch pt := paramType.(type) {
	case *ast.SimpleType:
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
					// Non-const always wins; const only fills a gap or replaces another const.
					// Additionally, string wins over __atom even if atom came from a non-const
					// argument: atoms are coercible to string, so mixed (atom, string) calls
					// should resolve t = string rather than t = __atom.
					existingIsAtom := subst[tp] == "__atom"

					currentIsString := name == "string"
					if existing, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) || (existingIsAtom && currentIsString) {
						_ = existing
						subst[tp] = name
						fromConst[tp] = isConst
					}
				}
			}
		}
	case *ast.PointerType:
		if ptr, ok := argType.(*irtypes.PointerType); ok {
			cg.inferTypeArgsFromParamPrio(pt.Elem, ptr.ElemType, typeParams, subst, fromConst, isConst)
		}
	case *ast.GenericType:
		structName := ""

		if st, ok2 := argType.(*irtypes.StructType); ok2 {
			structName = st.Name()
		}

		if structName == "" {
			break
		}

		prefix := pt.Name + "__"
		if !strings.HasPrefix(structName, prefix) {
			break
		}

		innerName := strings.TrimPrefix(structName, prefix)

		// Prefer the arg list recorded at monomorphization time, because type
		// parts themselves may contain `__` (e.g. package-qualified names like
		// json__Value). If no record exists, fall back to a `__`-split, which
		// is correct when every arg is a bare name.
		var parts []string
		if recorded, ok := cg.dataInstTypeArgs[structName]; ok && len(recorded) == len(pt.TypeParams) {
			parts = recorded
		} else if len(pt.TypeParams) == 1 {
			// Single type arg: the whole remainder is the arg (preserves
			// embedded `__` from package-qualified names).
			parts = []string{innerName}
		} else {
			parts = strings.Split(innerName, "__")
		}

		if len(parts) == 1 && len(pt.TypeParams) == 1 {
			innerParam := pt.TypeParams[0]

			if simpleInner, ok := innerParam.(*ast.SimpleType); ok {
				for _, tp := range typeParams {
					if simpleInner.Name == tp {
						if _, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) {
							subst[tp] = parts[0]
							fromConst[tp] = isConst
						}

						break
					}
				}
			} else {
				if innerST, ok := cg.structTypes[parts[0]]; ok {
					cg.inferTypeArgsFromParamPrio(innerParam, innerST, typeParams, subst, fromConst, isConst)
				}
			}

			break
		}

		if len(parts) != len(pt.TypeParams) {
			break
		}

		for i, innerParam := range pt.TypeParams {
			part := parts[i]
			simpleInner, ok := innerParam.(*ast.SimpleType)

			if !ok {
				continue
			}

			for _, tp := range typeParams {
				if simpleInner.Name == tp {
					if _, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) {
						subst[tp] = part
						fromConst[tp] = isConst
					}

					break
				}
			}
		}
	case *ast.ArrayType:
		if st, ok := argType.(*irtypes.StructType); ok && len(st.Fields) >= 2 {
			if ptrField, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
				cg.inferTypeArgsFromParamPrio(pt.Elem, ptrField.ElemType, typeParams, subst, fromConst, isConst)
			}
		}
	case *ast.FuncType:
		// Two argType shapes are accepted:
		//   1. Fat-fn-ptr {fn(i8*, ...)*, i8*}  - a wrapped closure
		//   2. Raw func pointer fn(...)*       - a bare named function
		// reference (e.g. `is_pos` passed directly to `filter(is_pos)`)
		// before any closure shim is built. Falling through on shape (2)
		// would skip inference and leave subst[t] unset, causing the
		// caller to monomorphize with the literal type-param name (e.g.
		// `@filter__t`) - all callers would then share one IR instance
		// and read its slice with the wrong stride for any element type
		// that doesn't happen to be 8 bytes (atom struct{i32}, i32 array,
		// etc).
		var (
			innerFnType *irtypes.FuncType
			envOffset   int
		)

		if isFatFnPtr(argType) {
			st := argType.(*irtypes.StructType)

			fn, ok := st.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			if !ok {
				break
			}

			innerFnType = fn
			envOffset = 1 // skip the i8* env in fat-fn-ptr inner sig
		} else if rawPtr, ok := argType.(*irtypes.PointerType); ok {
			fn, ok2 := rawPtr.ElemType.(*irtypes.FuncType)
			if !ok2 {
				break
			}

			innerFnType = fn
			envOffset = 0 // raw fn pointer carries no env slot
		} else {
			break
		}

		if pt.RetType != nil && innerFnType.RetType != nil {
			cg.inferTypeArgsFromParamPrio(pt.RetType, innerFnType.RetType, typeParams, subst, fromConst, isConst)
		}

		for i, astParam := range pt.Params {
			llIdx := i + envOffset

			if llIdx < len(innerFnType.Params) {
				cg.inferTypeArgsFromParamPrio(astParam, innerFnType.Params[llIdx], typeParams, subst, fromConst, isConst)
			}
		}
	}
}

// inferTypeArgsFromParam recursively matches an AST parameter type against an
// LLVM argument type to infer type-parameter bindings.  Handles:
//   - Direct type-param: fn foo[t](x t)   arg: i64      -> t=i64
//   - Pointer-to-param:  fn foo[t](x *t)  arg: *struct  -> t=struct
//   - Generic struct:    fn foo[t](x S[t]) arg: S__i64   -> t=i64
//   - Pointer-to-generic fn foo[t](x *S[t]) arg: *S__i64 -> t=i64

// evalConstExpr attempts to evaluate a Tin AST expression as a compile-time
// LLVM constant integer or float. Handles literals, type casts (as), bitwise
// NOT (~), unary negation (-), and integer arithmetic / shifts (+, -, <<).
// Returns nil for any expression that cannot be fully reduced to a constant.
//
// Integer values are computed using math/big so that operations like
//
//	const I128_MIN i128 = 1 as i128 << 127
//
// produce real constant.Int values rather than LLVM constant-expression nodes
// (which newer LLVM backends reject for shift/bitwise operators).
//
// This is used by Pass 4 of loadPackageFromSource so that complex package
// constants such as limits::I128_MIN are propagated to callers.
// evalConstExprTyped is like evalConstExpr but uses the declared Tin type as an
// integer-type hint so that typed constants (e.g. const T u32 = 0xd76aa478)
// are created with the correct LLVM bit-width rather than defaulting to i64.
// Also handles struct-literal constants (e.g. const C Color = Color{r:200,...}).
func (cg *CodeGen) evalConstExprTyped(expr ast.Node, declType ast.TypeExpr) constant.Constant {
	if declType != nil {
		if llType, err := cg.tinTypeToLLVM(declType); err == nil {
			switch lt := llType.(type) {
			case *irtypes.IntType:
				if intTyp, bigVal := cg.evalConstExprInt(expr, lt); intTyp != nil && bigVal != nil {
					return &constant.Int{Typ: intTyp, X: bigVal}
				}

			case *irtypes.StructType:
				if lit, ok := expr.(*ast.StructLit); ok {
					return cg.evalStructLitConst(lit)
				}
			}
		}
	}

	return cg.evalConstExpr(expr)
}

// evalStructLitConst builds a compile-time LLVM constant for a struct literal
// whose fields are all constant integers. Handles the full Tin struct layout:
// { i32 type_id, vtable_ptrs..., user_field_0, ... }.
// Returns nil if any field is non-constant or non-integer.
func (cg *CodeGen) evalStructLitConst(lit *ast.StructLit) constant.Constant {
	typeName := lit.TypeName
	if typeName == "" {
		return nil
	}

	// Resolve type alias to canonical struct name.
	canonicalName := typeName
	if alias, ok := cg.typeAliases[typeName]; ok {
		if st, ok2 := alias.(*ast.SimpleType); ok2 {
			canonicalName = st.Name
		}
	}

	st, ok := cg.structTypes[canonicalName]
	if !ok {
		return nil
	}

	fieldNames := cg.structFields[canonicalName]
	fieldLLVMTypes := cg.structFieldLLVMTypes[canonicalName]
	typeID := cg.structTypeIDs[canonicalName]
	numVtable := len(cg.structVtableOrder[canonicalName])
	userOff := 1 + numVtable

	// Start with all-zero constant fields matching the LLVM struct layout.
	fields := make([]constant.Constant, len(st.Fields))
	for i, ft := range st.Fields {
		fields[i] = cg.zeroConstant(ft)
	}

	// Slot 0: i32 type_id.
	fields[0] = constant.NewInt(irtypes.I32, int64(typeID))

	// Evaluate each positional field from the literal.
	for i, elem := range lit.Positional {
		if i >= len(fieldLLVMTypes) {
			break
		}

		llIdx := userOff + i
		if llIdx >= len(st.Fields) {
			break
		}

		intType, ok2 := fieldLLVMTypes[i].(*irtypes.IntType)
		if !ok2 {
			return nil // non-integer fields not supported in struct constants
		}

		intTyp, bigVal := cg.evalConstExprInt(elem, intType)
		if intTyp == nil {
			return nil
		}

		fields[llIdx] = &constant.Int{Typ: intTyp, X: bigVal}
	}

	// Evaluate each named field from the literal.
	for _, f := range lit.Fields {
		rawIdx := -1

		for i, fn := range fieldNames {
			if fn == f.Name {
				rawIdx = i

				break
			}
		}

		if rawIdx < 0 || rawIdx >= len(fieldLLVMTypes) {
			continue
		}

		llIdx := userOff + rawIdx
		if llIdx >= len(st.Fields) {
			continue
		}

		intType, ok2 := fieldLLVMTypes[rawIdx].(*irtypes.IntType)
		if !ok2 {
			return nil // non-integer fields not supported in struct constants
		}

		intTyp, bigVal := cg.evalConstExprInt(f.Value, intType)
		if intTyp == nil {
			return nil
		}

		fields[llIdx] = &constant.Int{Typ: intTyp, X: bigVal}
	}

	return constant.NewStruct(st, fields...)
}

func (cg *CodeGen) evalConstExpr(expr ast.Node) constant.Constant {
	// Delegate integer evaluation to the big.Int evaluator; float literals
	// are handled directly since float constants are always concrete values.
	switch e := expr.(type) {
	case *ast.FloatLit:
		return constant.NewFloat(irtypes.Double, e.Value)
	case *ast.StringLit:
		raw := cg.newGlobalString(e.Value).(constant.Constant)
		strType := stringFatPtrType()
		lenVal := constant.NewInt(irtypes.I64, int64(len(e.Value)))

		return constant.NewStruct(strType, raw, lenVal)
	}

	// Try integer path.
	if intTyp, bigVal := cg.evalConstExprInt(expr, nil); intTyp != nil && bigVal != nil {
		return &constant.Int{Typ: intTyp, X: bigVal}
	}

	return nil
}

// evalConstExprInt evaluates a Tin const expression as a (IntType, *big.Int)
// pair. declType provides the target integer type for top-level declarations
// (e.g. to resolve plain IntLit values that appear in a typed const); pass nil
// to infer type from the expression (defaults to i64 for bare literals).
// Returns (nil, nil) when the expression is not a constant integer.
func (cg *CodeGen) evalConstExprInt(expr ast.Node, hint *irtypes.IntType) (*irtypes.IntType, *big.Int) {
	switch e := expr.(type) {
	case *ast.IntLit:
		typ := hint
		if typ == nil {
			if e.Big != nil {
				typ = irtypes.I128
			} else {
				typ = irtypes.I64
			}
		}

		var raw *big.Int
		if e.Big != nil {
			raw = new(big.Int).Set(e.Big)
		} else {
			raw = big.NewInt(e.Value)
		}

		return typ, normIntBig(raw, uint(typ.BitSize))

	case *ast.UnaryExpr:
		it, inner := cg.evalConstExprInt(e.Expr, hint)
		if it == nil {
			return nil, nil
		}

		switch e.Op {
		case "-":
			result := new(big.Int).Neg(inner)

			return it, normIntBig(result, uint(it.BitSize))

		case "~":
			// bitwise NOT: ~x = -(x+1) in two's complement
			result := new(big.Int).Add(inner, big.NewInt(1))
			result.Neg(result)

			return it, normIntBig(result, uint(it.BitSize))
		}

		return nil, nil

	case *ast.AsExpr:
		targetLLVM, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, nil
		}

		toIt, toIsInt := targetLLVM.(*irtypes.IntType)
		if !toIsInt {
			return nil, nil
		}

		// Evaluate inner without hint so we get the raw value.
		_, inner := cg.evalConstExprInt(e.Expr, nil)
		if inner == nil {
			return nil, nil
		}

		return toIt, normIntBig(inner, uint(toIt.BitSize))

	case *ast.BinExpr:
		lt, left := cg.evalConstExprInt(e.Left, hint)
		if lt == nil {
			return nil, nil
		}

		// For shifts the right operand width can differ; use i64 as default.
		_, right := cg.evalConstExprInt(e.Right, irtypes.I64)
		if right == nil {
			return nil, nil
		}

		var result *big.Int

		switch e.Op {
		case "+":
			result = new(big.Int).Add(left, right)
		case "-":
			result = new(big.Int).Sub(left, right)
		case "<<":
			shift := uint(right.Uint64())
			result = new(big.Int).Lsh(left, shift)
		default:
			return nil, nil
		}

		return lt, normIntBig(result, uint(lt.BitSize))
	}

	return nil, nil
}

// normIntBig normalizes a *big.Int to the signed two's-complement range
// for an N-bit integer type: masks to N bits, then sign-extends from bit N-1.
// This ensures that e.g. (1 << 127) becomes -2^127 (I128_MIN) when bits==128.
func normIntBig(x *big.Int, bits uint) *big.Int {
	maxUnsigned := new(big.Int).Lsh(big.NewInt(1), bits) // 2^N
	maxSigned := new(big.Int).Rsh(maxUnsigned, 1)        // 2^(N-1)

	// Mask to N bits (unsigned mod 2^N).
	result := new(big.Int).And(x, new(big.Int).Sub(maxUnsigned, big.NewInt(1)))

	// Convert to signed: if result >= 2^(N-1), subtract 2^N.
	if result.Cmp(maxSigned) >= 0 {
		result.Sub(result, maxUnsigned)
	}

	return result
}
