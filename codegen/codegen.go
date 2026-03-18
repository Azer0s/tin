// Package codegen translates a tin AST into LLVM IR using the llir/llvm library.
package codegen

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// CodeGen

// CodeGen holds all state needed during code generation.
type CodeGen struct {
	filename string
	mod      *ir.Module

	// declared C functions
	printfFn  *ir.Func
	putsF     *ir.Func
	sprintfFn *ir.Func
	mallocFn  *ir.Func
	memcpyFn  *ir.Func

	// struct type registry: name -> LLVM struct type
	structTypes map[string]*irtypes.StructType
	// struct field order: name -> []fieldName
	structFields map[string][]string
	// generic struct templates: name -> AST node (not compiled directly)
	genericStructs map[string]*ast.StructDecl

	// trait vtable struct types: instKey -> LLVM struct type for vtable
	// instKey = traitName for non-generic, "traitName_typeArg" for generic
	traitVtableStructTypes map[string]*irtypes.StructType
	// trait fat-pointer types: instKey -> LLVM struct {i8*, vtable*}
	traitFatPtrTypes map[string]*irtypes.StructType
	// trait method order: traitName -> []method name (shared across instantiations)
	traitMethodOrder map[string][]string
	// vtable globals: "structName__instKey" -> ir.Global
	traitVtableGlobals map[string]*ir.Global
	// instKey -> base trait name (for generic traits)
	traitInstKeys map[string]string
	// implicit conversion registry: struct name -> []entry
	implicitConvFns map[string][]implicitConvEntry
	// structVtableOrder: struct name -> ordered instKeys embedded as leading fields
	structVtableOrder map[string][]string

	// enum value registry: "EnumName.Member" -> int64
	enumValues map[string]int64
	// enum type registry: name -> base LLVM type
	enumTypes map[string]irtypes.Type

	// type alias registry: alias name -> TypeExpr
	typeAliases map[string]ast.TypeExpr

	// trait registry: trait name -> TraitDecl
	traits map[string]*ast.TraitDecl

	// global string counter
	strCount int
	// general-purpose block label counter
	labelCount int

	// current function being built
	curFn    *ir.Func
	curScope *scope

	// pendingDefers holds calls deferred in the current function (LIFO on return).
	pendingDefers []ast.Node

	// pendingDeferFrames holds the i8* pointers to the TinDeferEntry allocas
	// pushed onto the runtime defer chain for this function.  They are popped
	// (without calling) before each normal return so that _tin_panic only runs
	// defers from frames that have not yet returned.
	pendingDeferFrames []value.Value

	// Defer chain runtime functions (lazily declared).
	deferPushFn    *ir.Func             // _tin_defer_push(entry i8*, fn i8*, env i8*)
	deferPopFn     *ir.Func             // _tin_defer_pop(n i64)
	deferEntryType *irtypes.StructType  // { i8*, i8*, i8* } = TinDeferEntry layout

	// tinPanicFn is the lazily declared _tin_panic(msg i8*) extern.
	tinPanicFn *ir.Func
	// tinRecoverFn is the lazily declared _tin_recover() -> TinString extern.
	tinRecoverFn *ir.Func
	// sliceSubsliceFn is the lazily declared _tin_slice_subslice extern.
	sliceSubsliceFn *ir.Func

	// ARC runtime functions (lazily declared).
	rcAllocFn *ir.Func // _tin_rc_alloc(size i64) i8*
	retainFn  *ir.Func // _tin_retain(ptr i8*)
	releaseFn *ir.Func // _tin_release(ptr i8*)

	// module system
	// exports: localName -> packageName  (from ExportDecl)
	exports map[string]string
	// importedPkgs: packageName -> true  (to avoid double-loading)
	importedPkgs map[string]bool

	// constrained generic functions
	// constrainedFuncs: funcName -> FuncDecl template (has Constraints)
	constrainedFuncs map[string]*ast.FuncDecl
	// constrainedFuncInstances: "funcName__typeArg" -> compiled *ir.Func
	constrainedFuncInstances map[string]*ir.Func

	// data type registry: name -> DataDecl AST node (non-generic)
	dataDecls map[string]*ast.DataDecl
	// generic data type templates: name -> DataDecl AST (has TypeParams)
	genericDataDecls map[string]*ast.DataDecl
	// data type tag assignments: "TypeName.VariantIndex" -> i8 tag value
	// None is always tag 0, typed variants start at 1.
	dataVariantTags map[string]int8

	// Universal runtime type ID registry.
	// Primitives use anyTag* constants (0–5).  Every named struct, data type,
	// and unique function signature gets a unique i32 starting at 6.
	structTypeIDs map[string]int32 // struct name -> compile-time type ID
	dataTypeIDs   map[string]int32 // data type name -> compile-time type ID
	fnTypeIDs     map[string]int32 // fn signature string -> compile-time type ID
	nextTypeID    int32            // counter; starts at 6

	// Reflection metadata.
	// structImpls: struct name -> []trait name strings (for traitof/typeof)
	structImpls map[string][]string
	// structFieldLLVMTypes: struct name -> []LLVM type per user field (for getfield/setfield)
	structFieldLLVMTypes map[string][]irtypes.Type

	// match subject: set before entering genWhereList when the function body
	// is a pure where-list pattern match. Used to compare atom conditions.
	matchSubject value.Value

	// strcmpFn: lazily declared C strcmp
	strcmpFn *ir.Func
	// anyEqFn: lazily declared _tin_any_eq runtime helper
	anyEqFn *ir.Func

	// macros: macro name -> MacroDecl AST
	macros map[string]*ast.MacroDecl

	// funcDecls: function name -> FuncDecl AST, populated during predeclaration.
	// Used by the #pure transitive side-effect checker.
	funcDecls map[string]*ast.FuncDecl

	// externIRNames: IR names of C extern functions. Populated by ensureExternDecl.
	// Used to detect collisions when a Tin user function has the same name as a C symbol.
	externIRNames map[string]bool

	// linkLibs: libraries to pass to the linker (from `use extern` lib entries)
	linkLibs []string

	// test mode: when true, TestDecl blocks are compiled into test functions
	// and a test-runner main is generated instead of the normal implicit main.
	testMode  bool
	testDecls []*ast.TestDecl

	// Atom type and registry.
	// atomType is the named LLVM struct %__atom = type { i32 }.
	// atomCodes maps atom name -> CRC32 code (collision-resolved).
	// atomCodeToName is the reverse map for collision detection.
	// atomOrder holds insertion order for stable @__tin_atom_table output.
	atomType      *irtypes.StructType
	atomCodes     map[string]int32
	atomCodeToName map[int32]string
	atomOrder     []string
	atomToStrFn   *ir.Func // __tin_atom_to_string(i32) {i8*,i64}
	strToAtomFn   *ir.Func // __tin_string_to_atom(i8*) %__atom

	// Tagged union registry: type name -> ordered variant TypeExprs (index = tag).
	// Created by "type u = i8 | string" declarations.
	unionTypeMembers map[string][]ast.TypeExpr

	// Native union registry: struct name -> UnionDecl AST.
	// Created by "union u = as_i8 i8 | as_string string" declarations.
	nativeUnionDecls map[string]*ast.UnionDecl

	// Tagged union type ID registry: union name -> compile-time i32 type ID.
	// Same purpose as structTypeIDs/dataTypeIDs — used for any boxing and typeof.
	unionTypeIDs map[string]int32
}

// newBlock creates a uniquely-named basic block in the current function.
// Sequential if/for/match statements in the same function reuse label base
// names (e.g. "if.merge") which produces duplicate labels in the IR and
// confuses LLVM's loop-deletion pass.  Always routing through this helper
// ensures every block in a function has a distinct name.
func (cg *CodeGen) newBlock(base string) *ir.Block {
	id := cg.labelCount
	cg.labelCount++
	return cg.curFn.NewBlock(fmt.Sprintf("%s.%d", base, id))
}

// SetTestMode enables test-mode compilation: test blocks are compiled into
// test functions and a test-runner main() is generated.
func (cg *CodeGen) SetTestMode(v bool) { cg.testMode = v }

// HasTests reports whether the source contained at least one test block.
// Only meaningful after Generate has been called.
func (cg *CodeGen) HasTests() bool { return len(cg.testDecls) > 0 }

// New creates a new CodeGen instance.
func New(filename string) *CodeGen {
	cg := &CodeGen{
		filename:               filename,
		mod:                    ir.NewModule(),
		structTypes:            make(map[string]*irtypes.StructType),
		structFields:           make(map[string][]string),
		genericStructs:         make(map[string]*ast.StructDecl),
		traitVtableStructTypes: make(map[string]*irtypes.StructType),
		traitFatPtrTypes:       make(map[string]*irtypes.StructType),
		traitMethodOrder:       make(map[string][]string),
		traitVtableGlobals:     make(map[string]*ir.Global),
		traitInstKeys:          make(map[string]string),
		implicitConvFns:        make(map[string][]implicitConvEntry),
		structVtableOrder:      make(map[string][]string),
		enumValues:             make(map[string]int64),
		enumTypes:              make(map[string]irtypes.Type),
		typeAliases:            make(map[string]ast.TypeExpr),
		traits:                 make(map[string]*ast.TraitDecl),
		exports:                  make(map[string]string),
		importedPkgs:             make(map[string]bool),
		constrainedFuncs:         make(map[string]*ast.FuncDecl),
		constrainedFuncInstances: make(map[string]*ir.Func),
		dataDecls:                make(map[string]*ast.DataDecl),
		genericDataDecls:         make(map[string]*ast.DataDecl),
		dataVariantTags:          make(map[string]int8),
		macros:                   make(map[string]*ast.MacroDecl),
		funcDecls:                make(map[string]*ast.FuncDecl),
		structTypeIDs:            make(map[string]int32),
		dataTypeIDs:              make(map[string]int32),
		fnTypeIDs:                make(map[string]int32),
		nextTypeID:               6, // 0–5 reserved for anyTag* primitives (fn=5)
		structImpls:              make(map[string][]string),
		structFieldLLVMTypes:     make(map[string][]irtypes.Type),
		atomCodes:                make(map[string]int32),
		atomCodeToName:           make(map[int32]string),
		unionTypeMembers:         make(map[string][]ast.TypeExpr),
		nativeUnionDecls:         make(map[string]*ast.UnionDecl),
		unionTypeIDs:             make(map[string]int32),
	}
	atomType := irtypes.NewStruct(irtypes.I32)
	atomType.SetName("__atom")
	cg.atomType = atomType
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, atomType)
	return cg
}

// registerBuiltinTraits pre-populates cg.traits with synthetic declarations for
// built-in special traits (iter[t]) so structs can implement them without an
// explicit "trait iter[t] = ..." declaration in the source file.
func (cg *CodeGen) registerBuiltinTraits() {
	if _, ok := cg.traits["iter"]; ok {
		return // already declared by user
	}
	// iter[t]: fn len(this iter[t]) i64 = virtual
	//          fn get(this iter[t], i i64) t = virtual
	selfType := &ast.GenericType{Name: "iter", TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: "t"}}}
	lenMethod := &ast.FuncDecl{
		Name:      "len",
		IsVirtual: true,
		Params:    []ast.Param{{Name: "this", Type: selfType}},
		RetType:   &ast.SimpleType{Name: "i64"},
	}
	getMethod := &ast.FuncDecl{
		Name:      "get",
		IsVirtual: true,
		Params: []ast.Param{
			{Name: "this", Type: selfType},
			{Name: "i", Type: &ast.SimpleType{Name: "i64"}},
		},
		RetType: &ast.SimpleType{Name: "t"},
	}
	cg.traits["iter"] = &ast.TraitDecl{
		Name:       "iter",
		TypeParams: []string{"t"},
		Methods:    []*ast.FuncDecl{lenMethod, getMethod},
	}
}

// LinkLibs returns the list of libraries that source-level directives
// requested to link against (e.g. from `use extern` lib entries).
// The caller should pass these as -l<lib> flags to the linker.
func (cg *CodeGen) LinkLibs() []string { return cg.linkLibs }

// Generate translates the AST program into an LLVM IR module.
func (cg *CodeGen) Generate(prog *ast.Program) (*ir.Module, error) {
	// Initialize global scope.
	cg.curScope = newScope(nil)

	// Register built-in special traits so structs can implement them without
	// an explicit trait declaration in source.
	cg.registerBuiltinTraits()

	// Zero pass: collect exports and constrained generic function templates
	// before compiling anything.
	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName != "" {
			for _, name := range exp.Names {
				cg.exports[name] = exp.AsName
			}
		}
		if fd, ok := node.(*ast.FuncDecl); ok && len(fd.Constraints) > 0 {
			cg.constrainedFuncs[fd.Name] = fd
		}
	}

	// First pass: register struct / enum / type declarations so forward refs work.
	for _, node := range prog.Stmts {
		if err := cg.preregister(node); err != nil {
			return nil, err
		}
	}

	// Validate complex (block-body) macros: side-effect check.
	// Recursive macros are allowed — the 5-second timeout handles runaway recursion.
	for _, m := range cg.macros {
		if isMacroComplex(m) {
			if err := checkMacroSideEffects(m); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.5: collect C extern symbol names BEFORE predeclaring Tin user
	// functions. This allows predeclareFuncAs to detect collisions and mangle
	// Tin wrapper names (e.g. `fn printf(...)` → IR `@_tin__printf`) to avoid
	// redefinition conflicts with C externs declared in the same source file.
	if cg.externIRNames == nil {
		cg.externIRNames = map[string]bool{}
	}
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.IsExtern != "" {
			cg.externIRNames[fd.IsExtern] = true
		}
	}

	// Second pass: pre-declare all functions (signatures only) so forward calls work.
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			if err := cg.predeclareFunc(fd); err != nil {
				return nil, err
			}
		}
		if sd, ok := node.(*ast.StructDecl); ok {
			// Skip method predeclaration for generic struct templates — methods
			// will be compiled on demand when the concrete type is instantiated.
			if len(sd.TypeParams) > 0 {
				continue
			}
			aug := cg.augmentStructFromTraits(sd)
			for _, m := range aug.Methods {
				if err := cg.predeclareMethod(aug.Name, m); err != nil {
					return nil, err
				}
			}
		}
	}

	// Validate #pure functions: transitive side-effect check.
	// Validate #no_recurse functions: transitive call-graph cycle check.
	// Both run after predeclaration so all function signatures and tags are known.
	if err := cg.checkAllPureFuncs(); err != nil {
		return nil, err
	}
	if err := cg.checkAllNoRecurseFuncs(); err != nil {
		return nil, err
	}

	// Third pass: generate full function bodies and other declarations.
	var topStmts []ast.Node
	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if err := cg.genFuncDecl(n); err != nil {
				return nil, err
			}
		case *ast.StructDecl:
			if err := cg.genStructDecl(n); err != nil {
				return nil, err
			}
		case *ast.EnumDecl:
			if err := cg.genEnumDecl(n); err != nil {
				return nil, err
			}
		case *ast.TypeDecl:
			if err := cg.genTypeDecl(n); err != nil {
				return nil, err
			}
		case *ast.UseDecl:
			if err := cg.genUseDecl(n); err != nil {
				return nil, err
			}
		case *ast.ExportDecl:
			// Already handled in zero pass; ExportDecl itself emits no IR.
		case *ast.TraitDecl:
			// Registered in preregister; no IR to emit.
		case *ast.MacroDecl:
			// Registered in preregister; no IR to emit.
		case *ast.DataDecl:
			if err := cg.genDataDecl(n); err != nil {
				return nil, err
			}
		case *ast.UnionDecl:
			if err := cg.genUnionDecl(n); err != nil {
				return nil, err
			}
		case *ast.TestDecl:
			if cg.testMode {
				cg.testDecls = append(cg.testDecls, n)
			}
			// In normal mode, test blocks are silently ignored.
		default:
			topStmts = append(topStmts, node)
		}
	}

	// In test mode, generate test functions and a test-runner main.
	if cg.testMode && len(cg.testDecls) > 0 {
		if err := cg.genTestRunner(topStmts); err != nil {
			return nil, err
		}
		cg.emitAtomTable()
		if err := cg.writeModuleFiles(prog); err != nil {
			return nil, err
		}
		return cg.mod, nil
	}

	// If there are top-level statements, wrap them in main().
	if len(topStmts) > 0 {
		// Check if main is already defined.
		hasmain := false
		for _, f := range cg.mod.Funcs {
			if f.Name() == "main" {
				hasmain = true
				break
			}
		}
		if !hasmain {
			if err := cg.genImplicitMain(topStmts); err != nil {
				return nil, err
			}
		}
	}

	// If the user declared a void `fn main()`, it was compiled as
	// `_tin_user_main`.  Generate a proper `i32 @main()` wrapper that
	// calls it and returns 0 so the process exits cleanly.
	var userMainFn *ir.Func
	for _, f := range cg.mod.Funcs {
		if f.Name() == "_tin_user_main" {
			userMainFn = f
			break
		}
	}
	if userMainFn != nil {
		// Only add the wrapper if there is no `i32 @main` already.
		hasMain := false
		for _, f := range cg.mod.Funcs {
			if f.Name() == "main" {
				hasMain = true
				break
			}
		}
		if !hasMain {
			wf := cg.mod.NewFunc("main", irtypes.I32)
			wb := wf.NewBlock("entry")
			// Build zero-value args for each parameter of _tin_user_main.
			var callArgs []value.Value
			for _, p := range userMainFn.Params {
				callArgs = append(callArgs, constant.NewZeroInitializer(p.Type()))
			}
			retIsVoid := userMainFn.Sig.RetType.Equal(irtypes.Void)
			if retIsVoid {
				wb.NewCall(userMainFn, callArgs...)
				wb.NewRet(constant.NewInt(irtypes.I32, 0))
			} else {
				ret := wb.NewCall(userMainFn, callArgs...)
				// Coerce return value to i32 if needed.
				var retVal value.Value = ret
				if !ret.Type().Equal(irtypes.I32) {
					if ret.Type().Equal(irtypes.I64) {
						retVal = wb.NewTrunc(ret, irtypes.I32)
					} else {
						retVal = constant.NewInt(irtypes.I32, 0)
					}
				}
				wb.NewRet(retVal)
			}
		}
	}

	// Emit the compile-time atom table and fill in atom helper function bodies.
	cg.emitAtomTable()

	// Write module file for any package exports in this source file.
	if err := cg.writeModuleFiles(prog); err != nil {
		return nil, err
	}

	// If no main function was generated (e.g. export-only module), emit a
	// trivial no-op main so the binary links successfully.
	hasMain := false
	for _, f := range cg.mod.Funcs {
		if f.Name() == "main" {
			hasMain = true
			break
		}
	}
	if !hasMain {
		wf := cg.mod.NewFunc("main", irtypes.I32)
		wb := wf.NewBlock("entry")
		wb.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	return cg.mod, nil
}

