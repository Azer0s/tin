package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// predeclareFunc adds a function to the module and registers it in the global
// scope without generating the body. This enables forward references and recursion.
func (cg *CodeGen) predeclareFunc(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	if len(n.Constraints) > 0 {
		return nil
	}
	irName := n.Name
	if pkg, ok := cg.exports[n.Name]; ok {
		irName = pkg + "__" + n.Name
	}
	// Mirror the rename done in genFuncDecl for user-declared void main.
	if n.Name == "main" && n.RetType == nil && !n.IsStatic {
		irName = "_tin_user_main"
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
	return cg.predeclareFuncAs(m, methodScopeName(structName, m))
}

// predeclareFuncAs is the common implementation for predeclareFunc / predeclareMethod.
func (cg *CodeGen) predeclareFuncAs(n *ast.FuncDecl, scopeName string) error {
	// Skip extern declarations - they will be handled in genFuncDecl.
	if n.IsExtern != "" {
		return nil
	}
	params := make([]*ir.Param, len(n.Params))
	for i, p := range n.Params {
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return err
		}
		params[i] = ir.NewParam(p.Name, pt)
	}
	var retType irtypes.Type = irtypes.Void
	if n.RetType != nil {
		var err error
		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}
	// Check if already declared under this scope name.
	if existing, ok := cg.curScope.vars[scopeName]; ok {
		if _, isFunc := existing.val.(*ir.Func); isFunc {
			return nil // already declared
		}
	}
	// Add function to module (declaration) using the IR name == scopeName.
	f := cg.mod.NewFunc(scopeName, retType, params...)
	f.Blocks = nil // no body yet
	cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	// If this was registered under an export-mangled name (pkg__foo), also
	// register the bare name so that local callsites still resolve.
	if idx := strings.Index(scopeName, "__"); idx >= 0 {
		localName := scopeName[idx+2:]
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
			// Generic struct — store as template; concrete types are created
			// when a "type X = GenericStruct[T]" alias is processed.
			cg.genericStructs[n.Name] = n
		} else {
			// Register an opaque struct so recursive types work.
			st := irtypes.NewStruct()
			st.SetName(n.Name)
			cg.structTypes[n.Name] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
		}
	case *ast.EnumDecl:
		// Will be fully registered in genEnumDecl.
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
	case *ast.DataDecl:
		if len(n.TypeParams) > 0 {
			// Generic data type — register template immediately so that
			// function signatures predeclared in pass 2 can resolve it.
			cg.genericDataDecls[n.Name] = n
		} else {
			// Register a placeholder struct so forward references work.
			st := irtypes.NewStruct()
			st.SetName(n.Name)
			cg.structTypes[n.Name] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
		}
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
func (cg *CodeGen) genFuncDecl(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	if len(n.Constraints) > 0 {
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
	// A user-declared void `fn main()` is compiled as `_tin_user_main` so we
	// can generate a proper `i32 @main()` wrapper that returns 0.
	if n.Name == "main" && n.RetType == nil && !n.IsStatic {
		irName = "_tin_user_main"
		// Keep `main` resolvable from Tin source (e.g. for recursion).
		defer func() {
			if entry, ok2 := cg.curScope.lookup("_tin_user_main"); ok2 {
				cg.curScope.set("main", entry)
			}
		}()
	}
	return cg.genFuncDeclAs(n, irName)
}

// genStructMethod generates a struct method body using a struct-qualified IR name.
func (cg *CodeGen) genStructMethod(structName string, m *ast.FuncDecl) error {
	return cg.genFuncDeclAs(m, methodScopeName(structName, m))
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
		return false
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

// structImplementsTrait is a convenience wrapper for simple (non-generic) traits.
func (cg *CodeGen) structImplementsTrait(structName, traitName string) bool {
	return cg.structSatisfiesConstraint(structName, &ast.SimpleType{Name: traitName})
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
		// Collect non-varargs parameters with their C-level types.
		isVariadic := false
		var cParams []*ir.Param
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
			cParams = append(cParams, ir.NewParam(p.Name, ct))
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

		// If the return type does not need wrapping, expose the C function
		// directly.  Fat-ptr parameters are handled by coerce() at call sites.
		if cRetType.Equal(retType) {
			cg.curScope.set(scopeName, &scopeEntry{val: cFunc, isAlloc: false})
			return nil
		}

		// Return type needs wrapping (e.g. char* -> string fat-ptr).
		// Generate a thin wrapper: same fixed params as C, but wraps the return.
		wrapperName := "__tinwrap_" + scopeName
		var wrapperFn *ir.Func
		for _, f := range cg.mod.Funcs {
			if f.Name() == wrapperName {
				wrapperFn = f
				break
			}
		}
		if wrapperFn == nil {
			wrapperFn = cg.mod.NewFunc(wrapperName, retType, cParams...)
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")
			callArgs := make([]value.Value, len(wrapperFn.Params))
			for i, p := range wrapperFn.Params {
				callArgs[i] = p
			}
			rawResult := entry.NewCall(cFunc, callArgs...)
			entry.NewRet(cg.wrapFromExtern(entry, rawResult, retType))
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

	// Save context (including defer lists — each function has its own).
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevDefers := cg.pendingDefers
	prevDeferFrames := cg.pendingDeferFrames
	cg.pendingDefers = nil
	cg.pendingDeferFrames = nil
	cg.curFn = f
	cg.curScope = newScope(cg.curScope)

	// Register function in current scope so recursion works.
	cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

	// Alloca parameters and register them in scope.
	var firstParamAlloca *ir.InstAlloca
	for i, p := range f.Params {
		alloca := entry.NewAlloca(p.Type())
		entry.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		// ARC: the caller passes the value without retaining; we retain here
		// so that the param alloca owns one reference independently.
		// emitRetain handles RC-tracked values and named structs with RC fields.
		cg.emitRetain(entry, p)
		cg.curScope.set(n.Params[i].Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC})
		if i == 0 {
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
	_, err := cg.genBody(entry, n.Body, retType)
	cg.matchSubject = prevMatchSubject
	if err != nil {
		return err
	}

	// Restore context.
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.pendingDefers = prevDefers
	cg.pendingDeferFrames = prevDeferFrames

	// Enforce #noRecurse: walk IR to find any call to self.
	for _, tag := range n.Tags {
		if tag == "noRecurse" {
			for _, blk := range f.Blocks {
				for _, instr := range blk.Insts {
					if call, ok := instr.(*ir.InstCall); ok {
						if callee, ok2 := call.Callee.(*ir.Func); ok2 && callee == f {
							return fmt.Errorf("fn %s: #noRecurse violation — function calls itself", n.Name)
						}
					}
				}
			}
			break
		}
	}

	// Ensure function is registered in current scope.
	if cg.curScope != nil {
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}

	return nil
}

// genImplicitMain creates a main() function containing the top-level statements.
func (cg *CodeGen) genImplicitMain(stmts []ast.Node) error {
	f := cg.mod.NewFunc("main", irtypes.I32)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	cg.curScope = newScope(cg.curScope)

	for _, stmt := range stmts {
		var err error
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
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.curFn = prevFn
	cg.curScope = prevScope
	return nil
}


// genTestRunner generates one __tin_test_N function per TestDecl, plus a
// main() that:
//   1. Runs any top-level setup statements (non-test stmts).
//   2. Calls _tin_run_test(desc, fn_ptr) for each test.
//   3. Returns the exit code from _tin_test_finish(total_count).
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
		prevDefers := cg.pendingDefers
		prevDeferFrames := cg.pendingDeferFrames
		cg.curFn = fn
		cg.curScope = newScope(cg.curScope)
		cg.pendingDefers = nil
		cg.pendingDeferFrames = nil
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
		cg.pendingDefers = prevDefers
		cg.pendingDeferFrames = prevDeferFrames

		testFuncs[i] = fn
	}

	// Generate main().
	mainFn := cg.mod.NewFunc("main", irtypes.I32)
	entry := mainFn.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = mainFn
	cg.curScope = newScope(cg.curScope)

	// Run setup statements (top-level non-test code).
	cur := entry
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

