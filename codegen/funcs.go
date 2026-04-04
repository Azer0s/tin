package codegen

import (
	"fmt"
	"os"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// predeclareFunc adds a function to the module and registers it in the global
// scope without generating the body. This enables forward references and recursion.
func (cg *CodeGen) predeclareFunc(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	if len(n.Constraints) > 0 {
		return nil
	}
	// Unconstrained generic functions (TypeParams only) are also compiled on demand.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}
	// Register for #pure transitive side-effect checking.
	cg.funcDecls[n.Name] = n

	irName := n.Name
	if pkg, ok := cg.exports[n.Name]; ok {
		irName = pkg + "__" + n.Name
	}
	// Mirror the rename done in genFuncDecl: any user fn main is _tin_user_main.
	if n.Name == "main" && !n.IsStatic {
		irName = "_tin_user_main"
	}
	// Overloading: if multiple functions share this base name, mangle the IR name
	// and register the variant in the overloads map.
	if cg.overloadedNames[n.Name] && n.IsExtern == "" && len(n.Constraints) == 0 {
		sig := funcParamSig(n.Params)
		mangledName := overloadMangledName(irName, sig)
		// Resolve LLVM param types for later call-site matching.
		paramTypes, err := cg.resolveParamTypes(n.Params, "")
		if err != nil {
			return err
		}

		cg.overloads[n.Name] = append(cg.overloads[n.Name], &overloadEntry{
			irName:     mangledName,
			paramSig:   sig,
			paramTypes: paramTypes,
			arity:      len(paramTypes),
		})
		irName = mangledName
	}

	return cg.predeclareFuncAs(n, irName)
}

// traitQualifierKey converts a trait qualifier string like "iter[char]" into a
// safe identifier segment like "iter_char" for use in scope/IR names.
func traitQualifierKey(q string) string {
	out := make([]byte, 0, len(q))
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch c {
		case '[', ']', ',', ' ':
			if len(out) > 0 && out[len(out)-1] != '_' {
				out = append(out, '_')
			}
		default:
			out = append(out, c)
		}
	}
	// Trim trailing underscore.
	for len(out) > 0 && out[len(out)-1] == '_' {
		out = out[:len(out)-1]
	}

	return string(out)
}

// methodScopeName returns the IR/scope name for a struct method.
// For plain methods: "StructName_methodName".
// For trait-qualified methods: "StructName_traitKey_methodName".
func methodScopeName(structName string, m *ast.FuncDecl) string {
	if m.TraitQualifier != "" {
		return structName + "_" + traitQualifierKey(m.TraitQualifier) + "_" + m.Name
	}

	return structName + "_" + m.Name
}

// predeclareMethod pre-declares a struct method using a struct-qualified name
// ("StructName_methodName") so that methods with the same name on different
// structs don't collide.
func (cg *CodeGen) predeclareMethod(structName string, m *ast.FuncDecl) error {
	// Register in funcDecls so that #pure tag checking applies to methods too.
	key := methodScopeName(structName, m)
	cg.funcDecls[key] = m
	// Overloading: if multiple methods share this base name within the struct,
	// mangle the scope name and register the variant in the overloads map.
	if cg.overloadedNames[key] && m.IsExtern == "" {
		sig := methodParamSig(m, structName)
		mangledKey := overloadMangledName(key, sig)
		// Resolve param types for call-site matching (skip the 'this' receiver).
		paramTypes, err := cg.resolveParamTypes(m.Params, structName)
		if err != nil {
			return err
		}

		cg.overloads[key] = append(cg.overloads[key], &overloadEntry{
			irName:     mangledKey,
			paramSig:   sig,
			paramTypes: paramTypes,
			arity:      len(paramTypes),
		})

		return cg.predeclareFuncAs(m, mangledKey)
	}

	return cg.predeclareFuncAs(m, key)
}

// predeclareFuncAs is the common implementation for predeclareFunc / predeclareMethod.
func (cg *CodeGen) predeclareFuncAs(n *ast.FuncDecl, scopeName string) error {
	// Skip extern declarations - they will be handled in genFuncDecl.
	if n.IsExtern != "" {
		return nil
	}

	var params []*ir.Param

	for _, p := range n.Params {
		if p.IsVarArgs {
			continue // varargs is not an LLVM-level named parameter
		}

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return err
		}

		params = append(params, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		var err error

		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}
	// If this Tin function has the same name as a C extern symbol, mangle the
	// IR name to avoid a redefinition conflict.  Both the mangled and bare names
	// are registered in scope so that Tin call sites resolve to the wrapper.
	irName := scopeName
	if cg.externIRNames[scopeName] {
		irName = "_tin__" + scopeName
	}
	// Check if already declared under the (possibly mangled) IR name.
	if existing, ok := cg.curScope.vars[irName]; ok {
		if _, isFunc := existing.val.(*ir.Func); isFunc {
			if irName != scopeName {
				// Ensure the original Tin name also resolves.
				cg.curScope.set(scopeName, &scopeEntry{val: existing.val, isAlloc: false})
			}

			return nil // already declared
		}
	}
	// Add function to module (declaration) using the IR name.
	f := cg.mod.NewFunc(irName, retType, params...)
	f.Blocks = nil // no body yet
	cg.curScope.set(irName, &scopeEntry{val: f, isAlloc: false})

	if irName != scopeName {
		// Register original Tin name so call sites resolve to the wrapper.
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}
	// If this was registered under an export-mangled name (pkg__foo), also
	// register the bare name so that local callsites still resolve.
	if idx := strings.Index(irName, "__"); idx >= 0 {
		localName := irName[idx+2:]
		if _, already := cg.curScope.vars[localName]; !already {
			cg.curScope.set(localName, &scopeEntry{val: f, isAlloc: false})
		}
	}

	return nil
}

// Pre-registration pass

func (cg *CodeGen) preregister(node ast.Node) error {
	switch n := node.(type) {
	case *ast.StructDecl:
		if len(n.TypeParams) > 0 {
			// Generic struct - store as template keyed by arity; concrete types
			// are created when a "type X = GenericStruct[T, R]" alias is processed.
			// Templates are always keyed by bare name so that tinTypeToLLVM can look
			// them up using the stripped (bare) name component.
			if cg.genericStructsByArity[n.Name] == nil {
				cg.genericStructsByArity[n.Name] = make(map[int]*ast.StructDecl)
			}

			cg.genericStructsByArity[n.Name][len(n.TypeParams)] = n
		} else {
			// Register an opaque struct so recursive types work.
			// Use the canonical package-prefixed name as both the map key and the
			// LLVM IR struct name so that structs from different packages never
			// collide (e.g. sync__Unit, io__Reader).
			structKey := cg.pkgStructKey(n.Name)
			st := irtypes.NewStruct()
			st.SetName(structKey)
			cg.structTypes[structKey] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			// Register a permanent bare-name alias so that code referencing the
			// short form (e.g. "Unit" inside sync, or after "use sync") resolves to
			// the canonical name.  A later definition in user code would override.
			if cg.currentPkg != "" {
				if _, alreadyAliased := cg.typeAliases[n.Name]; !alreadyAliased {
					cg.typeAliases[n.Name] = &ast.SimpleType{Name: structKey}
				}
			}
		}
	case *ast.EnumDecl:
		// Register enum values early so they are available during on-demand
		// struct monomorphization triggered from pass 2 (predeclare).
		if err := cg.genEnumDecl(n); err != nil {
			return err
		}
	case *ast.UnionDecl:
		// Register an opaque struct so forward references work.
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	case *ast.TypeDecl:
		// Simple type aliases (type char = u8) go straight into typeAliases.
		// Tagged union aliases (type u = i8 | string) get a placeholder struct so
		// forward references work; full layout is filled in genTypeDecl.
		// Struct-monomorphization aliases (type point = tuple[f32]) are handled
		// in genTypeDecl so that all struct templates are known first.
		if _, isUnion := n.Type.(*ast.UnionTypeExpr); isUnion {
			st := irtypes.NewStruct()
			st.SetName(n.Name)
			cg.structTypes[n.Name] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
		} else if _, isGeneric := n.Type.(*ast.GenericType); !isGeneric {
			cg.typeAliases[n.Name] = n.Type
		}
	case *ast.TraitDecl:
		cg.traits[n.Name] = n
	case *ast.MacroDecl:
		cg.macros[n.Name] = n
	case *ast.VarDecl:
		if !n.IsConst || n.Value == nil {
			break
		}
		// Preregister top-level constants so test-block scopes can see them.
		// Only simple literal values are evaluated here; complex expressions
		// are left to the normal genVarDecl pass.
		var cv value.Value

		switch lit := n.Value.(type) {
		case *ast.IntLit:
			cv = constant.NewInt(irtypes.I64, lit.Value)
		case *ast.FloatLit:
			cv = constant.NewFloat(irtypes.Double, lit.Value)
		case *ast.BoolLit:
			if lit.Value {
				cv = constant.NewInt(irtypes.I1, 1)
			} else {
				cv = constant.NewInt(irtypes.I1, 0)
			}
		case *ast.StringLit:
			raw := cg.newGlobalString(lit.Value).(constant.Constant)
			strType := stringFatPtrType()
			lenVal := constant.NewInt(irtypes.I64, int64(len(lit.Value)))
			cv = constant.NewStruct(strType, raw, lenVal)
		case *ast.AtomLit:
			cv = cg.atomConstant(cg.registerAtom(lit.Name))
		}

		if cv != nil {
			if n.Type != nil {
				if lt, err := cg.tinTypeToLLVM(n.Type); err == nil {
					cv = cg.constCoerce(cv, lt)
				}
			}

			cg.curScope.set(n.Name, &scopeEntry{val: cv, isAlloc: false})
		}
	}

	return nil
}

// hasDeferStmt reports whether body contains any DeferStmt (recursively).
// Nested fn/lambda declarations are not descended into.
// NOTE: concrete nil pointers (e.g. (*ast.Block)(nil)) passed as ast.Node are
// non-nil interfaces; all concrete-pointer cases must guard against nil n.
func hasDeferStmt(body ast.Node) bool {
	if body == nil {
		return false
	}

	switch n := body.(type) {
	case *ast.DeferStmt:
		return n != nil
	case *ast.FuncDecl, *ast.LambdaExpr:
		return false // defers inside nested fns don't affect outer's ret slot
	case *ast.Block:
		if n == nil {
			return false
		}

		for _, s := range n.Stmts {
			if hasDeferStmt(s) {
				return true
			}
		}
	case *ast.ExprStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Expr)
	case *ast.VarDecl:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.AssignStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.ReturnStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.IfStmt:
		if n == nil {
			return false
		}

		if n.Then != nil && hasDeferStmt(n.Then) {
			return true
		}

		if n.Else != nil && hasDeferStmt(n.Else) {
			return true
		}

		for _, elif := range n.ElseIfs {
			if elif.Body != nil && hasDeferStmt(elif.Body) {
				return true
			}
		}
	case *ast.ForStmt:
		if n == nil {
			return false
		}

		return n.Body != nil && hasDeferStmt(n.Body)
	case *ast.MatchStmt:
		if n == nil {
			return false
		}

		for _, arm := range n.Cases {
			if arm.Body != nil && hasDeferStmt(arm.Body) {
				return true
			}
		}
	}

	return false
}

// bodyContainsSpawnOrAwait reports whether any node in the body (recursively)
// is a SpawnExpr or AwaitExpr. Nested fn declarations are not descended into.
func bodyContainsSpawnOrAwait(body []ast.Node) bool {
	var walk func(node ast.Node) bool

	walk = func(node ast.Node) bool {
		if node == nil {
			return false
		}

		switch n := node.(type) {
		case *ast.SpawnExpr, *ast.AwaitExpr:
			return true
		case *ast.FuncDecl:
			return false // don't descend into nested fn declarations
		case *ast.Block:
			for _, s := range n.Stmts {
				if walk(s) {
					return true
				}
			}
		case *ast.ExprStmt:
			return walk(n.Expr)
		case *ast.VarDecl:
			return walk(n.Value)
		case *ast.AssignStmt:
			return walk(n.Value)
		case *ast.AugAssignStmt:
			return walk(n.Value)
		case *ast.ReturnStmt:
			return walk(n.Value)
		case *ast.EchoStmt:
			return walk(n.Value)
		case *ast.IfStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Then != nil && walk(n.Then) {
				return true
			}

			if n.Else != nil && walk(n.Else) {
				return true
			}
		case *ast.ForStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Body != nil && walk(n.Body) {
				return true
			}
		case *ast.CallExpr:
			if walk(n.Func) {
				return true
			}

			for _, a := range n.Args {
				if walk(a) {
					return true
				}
			}
		case *ast.BinExpr:
			return walk(n.Left) || walk(n.Right)
		}

		return false
	}
	for _, s := range body {
		if walk(s) {
			return true
		}
	}

	return false
}

func (cg *CodeGen) genFuncDecl(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	// Register them in constrainedFuncs so call-site monomorphization can find them
	// even when defined locally inside a test or function body.
	if len(n.Constraints) > 0 {
		cg.constrainedFuncs[n.Name] = n

		return nil
	}
	// Unconstrained generic functions (TypeParams only) are also compiled on demand.
	// Register them in genericFuncs so call-site monomorphization can find them.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}

	irName := n.Name
	if pkg, ok := cg.exports[n.Name]; ok {
		irName = pkg + "__" + n.Name
		// Also register the unqualified name so local calls work.
		defer func() {
			if entry, ok2 := cg.curScope.lookup(irName); ok2 {
				cg.curScope.set(n.Name, entry)
			}
		}()
	}
	// Any user-declared `fn main(...)` is compiled as `_tin_user_main` so we
	// can generate a proper C `i32 @main()` wrapper that passes default args
	// and returns the result (or 0 for void).
	if n.Name == "main" && !n.IsStatic {
		if !isAsyncTag(n.Tags) && bodyContainsSpawnOrAwait([]ast.Node{n.Body}) && !cg.noWarnAsyncMain {
			_, _ = fmt.Fprintf(os.Stderr,
				"tin: warning: main() uses 'spawn' or 'await' but is not marked async.\n"+
					"    Each await in a non-async main() creates a temporary fiber, which is slower\n"+
					"    and bypasses inline channel optimizations.\n"+
					"    Fix: change 'fn main()' to 'fn{#async} main()'\n")
		}

		irName = "_tin_user_main"
		// Keep `main` resolvable from Tin source (e.g. for recursion).
		defer func() {
			if entry, ok2 := cg.curScope.lookup("_tin_user_main"); ok2 {
				cg.curScope.set("main", entry)
			}
		}()
	}
	// If this user-defined function has the same name as an already-declared
	// C extern symbol, mangle the IR name to avoid a redefinition conflict.
	// We only mangle against EXTERN declarations (not against the function's own
	// predeclared stub in the IR - that is handled by genFuncDeclAs reuse logic).
	if n.IsExtern == "" && cg.externIRNames[irName] {
		mangledName := "_tin__" + irName
		tinName := irName // capture for deferred closure
		irName = mangledName

		defer func() {
			if entry, ok2 := cg.curScope.lookup(mangledName); ok2 {
				cg.curScope.set(tinName, entry)
			}
		}()
	}
	// Overloading: if this function is part of an overload set, use the mangled name.
	if cg.overloadedNames[n.Name] && n.IsExtern == "" && len(n.Constraints) == 0 {
		sig := funcParamSig(n.Params)
		irName = overloadMangledName(irName, sig)
	}

	return cg.genFuncDeclAs(n, irName)
}

// genStructMethod generates a struct method body using a struct-qualified IR name.
func (cg *CodeGen) genStructMethod(structName string, m *ast.FuncDecl) error {
	key := methodScopeName(structName, m)
	// Overloading: use the mangled name when this method belongs to an overload set.
	if cg.overloadedNames[key] && m.IsExtern == "" {
		sig := methodParamSig(m, structName)

		return cg.genFuncDeclAs(m, overloadMangledName(key, sig))
	}

	return cg.genFuncDeclAs(m, key)
}

// genFuncDeclAs generates a function using scopeName as the IR/scope name.
// structSatisfiesConstraint checks that structName satisfies a trait expression.
// traitExpr may be a SimpleType ("labeled") or a GenericType ("iter[i64]").
func (cg *CodeGen) structSatisfiesConstraint(structName string, traitExpr ast.TypeExpr) bool {
	var traitName string

	switch te := traitExpr.(type) {
	case *ast.SimpleType:
		traitName = te.Name
	case *ast.GenericType:
		traitName = te.Name
	default:
		return false
	}

	td, ok := cg.traits[traitName]
	if !ok {
		// Not a declared trait: treat as a type-equality constraint.
		// "where t is i64" is satisfied iff concreteName == "i64".
		return traitName == structName
	}

	instKey := traitImplKey(traitExpr)

	for _, m := range td.Methods {
		if !m.IsVirtual {
			continue
		}

		qualName := structName + "_" + traitQualifierKey(instKey) + "_" + m.Name
		plainName := structName + "_" + m.Name
		_, hasQual := cg.curScope.lookup(qualName)

		_, hasPlain := cg.curScope.lookup(plainName)
		if !hasQual && !hasPlain {
			return false
		}
	}

	return true
}

func (cg *CodeGen) genFuncDeclAs(n *ast.FuncDecl, scopeName string) error {
	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		var err error

		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}

	if n.IsExtern != "" {
		// Extern functions are always side-effectful; ensure the tag is present.
		if !hasTag(n.Tags, "sideffect") {
			n.Tags = append(n.Tags, "sideffect")
		}
		// Collect non-varargs parameters with their C-level types.
		isVariadic := false

		var cParams []*ir.Param
		// cParamByval[i] is non-nil when cParams[i] uses byval (large struct > 16 bytes).
		var cParamByval []*irtypes.StructType

		for _, p := range n.Params {
			if p.IsVarArgs {
				isVariadic = true

				continue
			}

			if vt, ok := p.Type.(*ast.SimpleType); ok && vt.Name == "..." {
				isVariadic = true

				continue
			}

			ct, err := cg.tinTypeToExternLLVM(p.Type, false)
			if err != nil {
				return err
			}
			// Named Tin struct params > 16 bytes must use byval to match AMD64 ABI.
			// On ARM64, LLVM handles struct-by-value calling convention automatically
			// (HFAs go in SIMD registers, non-HFAs via memory), so byval is not used.
			if nativeSt, isNative := ct.(*irtypes.StructType); isNative && nativeStructNeedsByval(nativeSt) && cg.targetIsAMD64() {
				bvParam := ir.NewParam(p.Name, irtypes.I8Ptr)
				bvParam.Attrs = append(bvParam.Attrs, ir.Byval{Typ: nativeSt})
				cParams = append(cParams, bvParam)
				cParamByval = append(cParamByval, nativeSt)
			} else {
				cParams = append(cParams, ir.NewParam(p.Name, ct))
				cParamByval = append(cParamByval, nil)
			}
		}
		// Compute C-level return type.
		var cRetType irtypes.Type = irtypes.Void

		if n.RetType != nil {
			var err error

			cRetType, err = cg.tinTypeToExternLLVM(n.RetType, true)
			if err != nil {
				return err
			}
		}

		// Create (or reuse) the raw C declaration with C-level types.
		cFunc := cg.ensureExternDecl(n.IsExtern, cRetType, cParams, isVariadic)

		if cg.curScope == nil {
			cg.curScope = newScope(nil)
		}

		// Detect if any parameter or return type is a named Tin struct that needs
		// Tin->C conversion at the call boundary.
		needsStructConv := false

		for _, p := range n.Params {
			if p.IsVarArgs {
				continue
			}

			if _, isStruct := cg.isNamedTinStruct(p.Type); isStruct {
				needsStructConv = true

				break
			}
		}

		if n.RetType != nil {
			if _, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
				needsStructConv = true
			}
		}

		// If the return type does not need wrapping and no struct params, expose
		// the C function directly.  Fat-ptr parameters are handled by coerce().
		if cRetType.Equal(retType) && !needsStructConv {
			cg.curScope.set(scopeName, &scopeEntry{val: cFunc, isAlloc: false})

			return nil
		}

		// Generate a thin wrapper that handles type conversions.
		// For struct interop: wrapper takes Tin-level params (full struct), converts
		// to C-native layout, calls C, converts result back to Tin layout.
		// For other types (e.g. char* -> string): same as before.
		wrapperName := "__tinwrap_" + scopeName

		var wrapperFn *ir.Func

		for _, f := range cg.mod.Funcs {
			if f.Name() == wrapperName {
				wrapperFn = f

				break
			}
		}

		if wrapperFn == nil {
			// Build wrapper params: use Tin-level types for struct params, C-level
			// for all others (so call sites don't need special coerce for structs).
			wrapperParams := make([]*ir.Param, len(cParams))

			tinParamIdx := 0
			for i, cp := range cParams {
				// Find the corresponding AST param (skip varargs already filtered out).
				tinParam := n.Params[tinParamIdx]
				for tinParam.IsVarArgs {
					tinParamIdx++
					tinParam = n.Params[tinParamIdx]
				}

				if sName, isStruct := cg.isNamedTinStruct(tinParam.Type); isStruct {
					tinType, _ := cg.tinTypeToLLVM(tinParam.Type)
					wrapperParams[i] = ir.NewParam(sName, tinType)
				} else {
					wrapperParams[i] = cp
				}

				tinParamIdx++
			}

			wrapperFn = cg.mod.NewFunc(wrapperName, retType, wrapperParams...)
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")

			// Build C-level call args: convert struct params to native, pass others as-is.
			callArgs := make([]value.Value, len(wrapperFn.Params))

			tinParamIdx = 0
			for i, p := range wrapperFn.Params {
				tinParam := n.Params[tinParamIdx]
				for tinParam.IsVarArgs {
					tinParamIdx++
					tinParam = n.Params[tinParamIdx]
				}

				if sName, isStruct := cg.isNamedTinStruct(tinParam.Type); isStruct {
					native, err := cg.wrapStructToExtern(entry, p, sName)
					if err != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return err
					}
					// For byval params (large structs > 16 bytes): alloca native struct,
					// store the converted value, then pass a byval-attributed pointer.
					if cParamByval[i] != nil {
						nativeAlloca := entry.NewAlloca(cParamByval[i])
						entry.NewStore(native, nativeAlloca)
						ptr := entry.NewBitCast(nativeAlloca, irtypes.I8Ptr)
						callArgs[i] = ir.NewArg(ptr, ir.Byval{Typ: cParamByval[i]})
					} else {
						callArgs[i] = native
					}
				} else {
					callArgs[i] = p
				}

				tinParamIdx++
			}

			rawResult := entry.NewCall(cFunc, callArgs...)

			// Convert result: if C returned a native struct, wrap back to Tin.
			var finalResult value.Value

			if n.RetType != nil {
				if sName, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
					tinResult, err := cg.wrapNativeStructToTin(entry, rawResult, sName)
					if err != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return err
					}

					finalResult = tinResult
				} else {
					finalResult = cg.wrapFromExtern(entry, rawResult, retType)
				}
			}

			if irtypes.IsVoid(retType) {
				entry.NewRet(nil)
			} else {
				entry.NewRet(finalResult)
			}

			cg.curFn = prevFn
			cg.curScope = prevScope
		}

		cg.curScope.set(scopeName, &scopeEntry{val: wrapperFn, isAlloc: false})

		return nil
	}

	// Look up pre-declared function in global scope (by qualified name), or create.
	var f *ir.Func

	if entry, ok := cg.curScope.vars[scopeName]; ok {
		if fn, isFunc := entry.val.(*ir.Func); isFunc {
			f = fn
		}
	}

	if f == nil {
		// Not pre-declared - create now (e.g. nested or struct method).
		params := make([]*ir.Param, len(n.Params))
		for i, p := range n.Params {
			pt, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			params[i] = ir.NewParam(p.Name, pt)
		}

		f = cg.mod.NewFunc(scopeName, retType, params...)
	}

	if n.Body == nil {
		f.Blocks = nil // Forward declaration - no body.

		return nil
	}

	// If function already has a body (re-declaration), skip.
	if len(f.Blocks) > 0 {
		return nil
	}

	// Create entry block.
	entry := f.NewBlock("entry")

	// Save context (including defer lists - each function has its own).
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevBlock := cg.curBlock
	prevDeferFnI8s := cg.pendingDeferFnI8s
	prevDeferFrames := cg.pendingDeferFrames
	prevDeferEnvs := cg.pendingDeferEnvs
	prevAutoYield := cg.curFnAutoYield
	prevDeferRetSlotParam := cg.curDeferRetSlotParam
	prevFnDeferRetAlloca := cg.curFnDeferRetAlloca
	prevDeferThunkRetType := cg.curDeferThunkRetType
	prevEscapingVars := cg.curFnEscapingVars
	prevEscapingAliases := cg.curFnEscapingAliases
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.curBlock = nil
	cg.curFnAutoYield = false // sync variant never auto-yields
	cg.curDeferRetSlotParam = nil
	cg.curDeferThunkRetType = nil

	cg.curFnEscapingVars, cg.curFnEscapingAliases = findEscapingAddressTakenVars(n.Body)
	if len(cg.curFnEscapingVars) > 0 || hasDirectHeapReturn(n.Body, cg.heapPromotingFns) {
		cg.heapPromotingFns[scopeName] = true
		// Also store under the actual IR function name (which may include a
		// parameter-type suffix, e.g. "json__parse_value__ptr_Parser") so that
		// genLetStmt can find it via the scope-resolved *ir.Func lookup.
		if f != nil {
			cg.heapPromotingFns[f.Name()] = true
		}
	}

	cg.curFn = f
	cg.curScope = newScope(cg.curScope)
	cg.curScope.isFunctionBoundary = true

	// For non-void functions that contain defer stmts: alloca a {i8, retType} slot
	// so a defer thunk can override the return value.  Skip when no defer is present
	// to avoid generating dead code in the common case.
	if !irtypes.IsVoid(retType) && hasDeferStmt(n.Body) {
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotAlloca := entry.NewAlloca(slotType)
		// Zero-initialize the valid byte.
		validGep := entry.NewGetElementPtr(slotType, slotAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
		cg.curFnDeferRetAlloca = entry.NewBitCast(slotAlloca, irtypes.I8Ptr)
	} else {
		cg.curFnDeferRetAlloca = nil
	}

	// Always restore context, even on error paths (e.g. when genBody returns
	// an error during on-demand monomorphization of a generic struct method).
	defer func() {
		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs
		cg.curFnAutoYield = prevAutoYield
		cg.curDeferRetSlotParam = prevDeferRetSlotParam
		cg.curFnDeferRetAlloca = prevFnDeferRetAlloca
		cg.curDeferThunkRetType = prevDeferThunkRetType
		cg.curFnEscapingVars = prevEscapingVars
		cg.curFnEscapingAliases = prevEscapingAliases
	}()

	// Register function in current scope so recursion works.
	cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

	// Mark the LLVM function as variadic if any tin param is varargs.
	for _, p := range n.Params {
		if p.IsVarArgs {
			f.Sig.Variadic = true

			break
		}
	}

	// Alloca parameters and register them in scope.
	// Iterate tin params; skip varargs (no LLVM parameter), but register a
	// null placeholder so the name is defined inside the body.
	var firstParamAlloca *ir.InstAlloca

	llIdx := 0

	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			if astParam.Name != "" {
				// Register as null i8* placeholder; true forwarding needs va_list.
				null := constant.NewNull(irtypes.NewPointer(irtypes.I8))
				cg.curScope.set(astParam.Name, &scopeEntry{val: null, isAlloc: false})
			}

			continue
		}

		p := f.Params[llIdx]
		llIdx++
		alloca := entry.NewAlloca(p.Type())
		entry.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		cg.emitRetain(entry, p)
		// Function parameters receive a by-value copy of the caller's struct.
		// The parameter is not the owner of the value; the caller is.  Mark
		// noDeinit so that scope-exit release of the parameter copy does not
		// invoke deinit (which would be a spurious call from the callee's
		// perspective and could double-free external resources).
		cg.curScope.set(astParam.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, noDeinit: true, isUnsigned: isUnsignedTinType(astParam.Type), scalarTypeName: scalar8BitTypeName(astParam.Type), tinType: astParam.Type})

		if llIdx == 1 {
			firstParamAlloca = alloca
		}
	}

	// For where-list bodies, set the match subject to the first parameter so
	// that atom conditions (e.g. `where 'ok:`) compare against it.
	prevMatchSubject := cg.matchSubject
	if _, isWhere := n.Body.(*ast.WhereList); isWhere && firstParamAlloca != nil {
		cg.matchSubject = entry.NewLoad(firstParamAlloca.ElemType, firstParamAlloca)
	}

	// Generate body (genBody ensures a terminator is added to the current block).
	_, bodyErr := cg.genBody(entry, n.Body, retType)
	cg.matchSubject = prevMatchSubject

	if bodyErr != nil {
		// Even on error, register the (partially compiled) function so it
		// appears in scope for callers that check for it. The error typically
		// occurs during on-demand monomorphization triggered from inside another
		// function body; the caller discards the error but still needs the fn.
		prevScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

		return bodyErr
	}

	// Restore context explicitly here (the defer is a safety net for error paths).
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.curBlock = prevBlock
	cg.pendingDeferFnI8s = prevDeferFnI8s
	cg.pendingDeferFrames = prevDeferFrames
	cg.pendingDeferEnvs = prevDeferEnvs
	cg.curFnEscapingVars = prevEscapingVars
	cg.curFnEscapingAliases = prevEscapingAliases

	// Note: #no_recurse is enforced by checkAllNoRecurseFuncs (AST-level,
	// transitive) before this function is ever compiled. No IR walk needed.

	// Ensure function is registered in current scope.
	if cg.curScope != nil {
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}

	// If this function is in the async-callable set (or has #async tag directly),
	// generate its $coro variant. The #async tag check catches local functions
	// that were not discovered by the pre-pass call graph analysis.
	if cg.coroCallable[scopeName] || hasTag(n.Tags, "async") {
		if !cg.coroCallable[scopeName] {
			cg.coroCallable[scopeName] = true
		}

		coroKey := coroVersionName(scopeName)
		// Ensure the $coro stub exists in the current scope's vars before calling
		// genCoroFuncBody. For top-level functions the pre-pass already registered
		// the stub - predeclareCoroVariant is a no-op when vars[coroKey] is set.
		// For local/monomorphized async functions (not in the pre-pass), this
		// creates the stub so genCoroFuncBody can find it.
		if err := cg.predeclareCoroVariant(n, scopeName, false); err != nil {
			return err
		}

		if err := cg.genCoroFuncBody(n, coroKey, nil, nil); err != nil {
			return err
		}
	}

	return nil
}

// genImplicitMain creates a main() function containing the top-level statements.
func (cg *CodeGen) genImplicitMain(stmts []ast.Node) error {
	if bodyContainsSpawnOrAwait(stmts) && !cg.noWarnAsyncMain {
		_, _ = fmt.Fprintf(os.Stderr,
			"tin: warning: top-level statements use 'spawn' or 'await' but there is no async main().\n"+
				"    Each await at the top level creates a temporary fiber, which is slower\n"+
				"    and bypasses inline channel optimizations.\n"+
				"    Fix: wrap your code in 'fn{#async} main() = ...' instead\n")
	}

	f := cg.mod.NewFunc("main", irtypes.I32)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	cg.curScope = newScope(cg.curScope)

	// Emit fiber init if the program uses any fiber features.
	entry = cg.emitFiberMainWrap(entry)

	// Emit top-level var runtime initializations (deferred from pre-pass 1.7).
	var err error

	entry, err = cg.emitTopLevelVarInits(entry)
	if err != nil {
		return err
	}

	for _, stmt := range stmts {
		entry, _, err = cg.genStmt(entry, stmt)
		if err != nil {
			return err
		}

		if entry == nil {
			break
		}
	}

	if entry != nil && entry.Term == nil {
		_ = cg.emitDefers(entry)
		cg.emitAllScopeReleases(entry, "")
		cg.emitFiberMainEnd(entry)
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}

// genTestRunner generates one __tin_test_N function per TestDecl, plus a
// main() that:
//  1. Runs any top-level setup statements (non-test stmts).
//  2. Calls _tin_run_test(desc, fn_ptr) for each test.
//  3. Returns the exit code from _tin_test_finish(total_count).
//
// _tin_run_test and _tin_test_finish are C helpers in runtime.c that use
// setjmp/longjmp to isolate test failures and accumulate pass/fail counts.
func (cg *CodeGen) genTestRunner(setupStmts []ast.Node) error {
	stringType, err := cg.tinTypeToLLVM(&ast.SimpleType{Name: "string"})
	if err != nil {
		return err
	}

	// Declare C runtime helpers.
	// void _tin_run_test(string desc, i8* fn)
	runTestFn := cg.ensureExternDecl("_tin_run_test", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("desc", stringType),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	// i64 _tin_test_finish(i64 total)
	finishFn := cg.ensureExternDecl("_tin_test_finish", irtypes.I64,
		[]*ir.Param{ir.NewParam("total", irtypes.I64)},
		false)

	// Generate one void function per test.
	testFuncs := make([]*ir.Func, len(cg.testDecls))
	for i, td := range cg.testDecls {
		name := fmt.Sprintf("__tin_test_%d", i)
		fn := cg.mod.NewFunc(name, irtypes.Void)
		entry := fn.NewBlock("entry")

		prevFn := cg.curFn
		prevScope := cg.curScope
		prevCurBlock := cg.curBlock
		prevDeferFnI8s := cg.pendingDeferFnI8s
		prevDeferFrames := cg.pendingDeferFrames
		prevDeferEnvs := cg.pendingDeferEnvs
		cg.curFn = fn
		cg.curScope = newScope(cg.curScope)
		cg.curBlock = nil
		cg.pendingDeferFnI8s = nil
		cg.pendingDeferFrames = nil
		cg.pendingDeferEnvs = nil
		cg.labelCount = 0

		terminated, err := cg.genBody(entry, td.Body, irtypes.Void)
		if err != nil {
			return fmt.Errorf("test %q: %w", td.Desc, err)
		}
		// Ensure the entry block is terminated.
		if !terminated {
			for _, b := range fn.Blocks {
				if b.Term == nil {
					_ = cg.emitDefers(b)
					b.NewRet(nil)
				}
			}
		}

		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevCurBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs

		testFuncs[i] = fn
	}

	// Generate main().
	mainFn := cg.mod.NewFunc("main", irtypes.I32)
	entry := mainFn.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = mainFn
	cg.curScope = newScope(cg.curScope)

	// Initialize fiber runtime (workers + I/O thread) so tests can use spawn/await.
	cur := cg.emitFiberMainWrap(entry)

	// Run setup statements (top-level non-test code).
	for _, stmt := range setupStmts {
		cur, _, err = cg.genStmt(cur, stmt)
		if err != nil {
			return err
		}

		if cur == nil {
			break
		}
	}

	// Call _tin_run_test for each test.
	if cur != nil {
		for i, td := range cg.testDecls {
			descVal := cg.buildStringFatPtr(cur, td.Desc)
			fnPtr := cur.NewBitCast(testFuncs[i], irtypes.I8Ptr)
			cur.NewCall(runTestFn, descVal, fnPtr)
		}

		// Drain the run queue and shut down workers.
		cg.emitFiberMainEnd(cur)

		// Call _tin_test_finish(N) -> i64 exit code.
		total := constant.NewInt(irtypes.I64, int64(len(cg.testDecls)))
		rc64 := cur.NewCall(finishFn, total)
		rc32 := cur.NewTrunc(rc64, irtypes.I32)
		cur.NewRet(rc32)
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}
