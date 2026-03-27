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

	// Handle re-exports: recursively load sub-packages.
	for _, sub := range mf.ReExports {
		subPath := pkgPath + "::" + sub
		if err2 := cg.loadPackage(subPath); err2 != nil {
			return err2
		}
	}

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
	prevFilename := cg.filename
	cg.filename = srcPath
	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok {
			if loadErr := cg.loadPackage(ud.Path); loadErr != nil {
				cg.filename = prevFilename
				return fmt.Errorf("use %s: %w", pkgPath, loadErr)
			}
		}
	}

	// Push a child scope so internal names don't pollute the caller's scope.
	prevScope := cg.curScope
	cg.curScope = newScope(prevScope)

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
		}
	}

	// Pass 2: predeclare non-extern functions (enables mutual recursion).
	for _, node := range prog.Stmts {
		fd, ok := node.(*ast.FuncDecl)
		if !ok || fd.IsExtern != "" {
			continue
		}
		prefixed := pkgName + "__" + fd.Name
		if preErr := cg.predeclareFuncAs(fd, prefixed); preErr != nil {
			cg.curScope = prevScope
			cg.filename = prevFilename
			return fmt.Errorf("use %s: %s: %w", pkgPath, fd.Name, preErr)
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
		}
	}

	cg.curScope = prevScope
	cg.filename = prevFilename
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
		return f, nil // already compiled
	}

	// Validate that each concrete type satisfies its declared constraints.
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
	// We root the new scope at the global (root) scope, not the current scope,
	// so that emitAllScopeReleases inside the monomorphized function doesn't
	// walk up into any enclosing function's local variable scope (e.g. a test
	// block) and emit releases for values that don't exist in this function.
	prevScope := cg.curScope
	rootScope := cg.curScope
	for rootScope.parent != nil {
		rootScope = rootScope.parent
	}
	cg.curScope = newScope(rootScope)

	// Register type aliases so that body expressions referring to the type
	// param (e.g. as a variable type annotation) resolve to the concrete type.
	for param, concrete := range astSubst {
		cg.typeAliases[param] = concrete
	}

	if err := cg.genFuncDeclAs(concrete, irName); err != nil {
		cg.curScope = prevScope
		return nil, err
	}

	cg.curScope = prevScope

	// Find the compiled function.
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
		if st, ok := p.Type.(*ast.SimpleType); ok {
			// If the param type is one of the type params, bind it.
			for _, tp := range tmpl.TypeParams {
				if st.Name == tp {
					name := cg.typeNameOf(argVals[i].Type())
					if name == "" {
						// Try pointee type for pointer args.
						if pt, ok2 := argVals[i].Type().(*irtypes.PointerType); ok2 {
							if st2, ok3 := pt.ElemType.(*irtypes.StructType); ok3 {
								name = st2.Name()
							}
						}
					}
					if name != "" {
						subst[tp] = name
					}
				}
			}
		}
	}
	return subst
}

// writeModuleFiles writes one .tin.mod file per ExportDecl found in prog.
func (cg *CodeGen) writeModuleFiles(prog *ast.Program) error {
	// Group exports by package name.
	type exportGroup struct {
		names     []string
		reExports []string
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

	// Collect all top-level function and struct declarations by name.
	funcsDecl := map[string]*ast.FuncDecl{}
	structsDecl := map[string]*ast.StructDecl{}
	typesDecl := map[string]*ast.TypeDecl{}
	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			funcsDecl[n.Name] = n
		case *ast.StructDecl:
			structsDecl[n.Name] = n
		case *ast.TypeDecl:
			typesDecl[n.Name] = n
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
