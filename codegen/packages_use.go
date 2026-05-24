package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
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
