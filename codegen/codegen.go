// Package codegen translates a tin AST into LLVM IR using the llir/llvm library.
package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// ── CodeGen ────────────────────────────────────────────────────────────────────

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

	// struct type registry: name → LLVM struct type
	structTypes map[string]*irtypes.StructType
	// struct field order: name → []fieldName
	structFields map[string][]string
	// generic struct templates: name → AST node (not compiled directly)
	genericStructs map[string]*ast.StructDecl

	// trait vtable struct types: instKey → LLVM struct type for vtable
	// instKey = traitName for non-generic, "traitName_typeArg" for generic
	traitVtableStructTypes map[string]*irtypes.StructType
	// trait fat-pointer types: instKey → LLVM struct {i8*, vtable*}
	traitFatPtrTypes map[string]*irtypes.StructType
	// trait method order: traitName → []method name (shared across instantiations)
	traitMethodOrder map[string][]string
	// vtable globals: "structName__instKey" → ir.Global
	traitVtableGlobals map[string]*ir.Global
	// instKey → base trait name (for generic traits)
	traitInstKeys map[string]string
	// implicit conversion registry: struct name → []entry
	implicitConvFns map[string][]implicitConvEntry
	// structVtableOrder: struct name → ordered instKeys embedded as leading fields
	structVtableOrder map[string][]string

	// enum value registry: "EnumName.Member" → int64
	enumValues map[string]int64
	// enum type registry: name → base LLVM type
	enumTypes map[string]irtypes.Type

	// type alias registry: alias name → TypeExpr
	typeAliases map[string]ast.TypeExpr

	// trait registry: trait name → TraitDecl
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

	// ARC runtime functions (lazily declared).
	rcAllocFn *ir.Func // _tin_rc_alloc(size i64) i8*
	retainFn  *ir.Func // _tin_retain(ptr i8*)
	releaseFn *ir.Func // _tin_release(ptr i8*)

	// module system
	// exports: localName → packageName  (from ExportDecl)
	exports map[string]string
	// importedPkgs: packageName → true  (to avoid double-loading)
	importedPkgs map[string]bool

	// constrained generic functions
	// constrainedFuncs: funcName → FuncDecl template (has Constraints)
	constrainedFuncs map[string]*ast.FuncDecl
	// constrainedFuncInstances: "funcName__typeArg" → compiled *ir.Func
	constrainedFuncInstances map[string]*ir.Func

	// data type registry: name → DataDecl AST node (non-generic)
	dataDecls map[string]*ast.DataDecl
	// generic data type templates: name → DataDecl AST (has TypeParams)
	genericDataDecls map[string]*ast.DataDecl
	// data type tag assignments: "TypeName.VariantIndex" → i8 tag value
	// None is always tag 0, typed variants start at 1.
	dataVariantTags map[string]int8

	// Universal runtime type ID registry.
	// Primitives use anyTag* constants (0–5).  Every named struct, data type,
	// and unique function signature gets a unique i32 starting at 6.
	structTypeIDs map[string]int32 // struct name → compile-time type ID
	dataTypeIDs   map[string]int32 // data type name → compile-time type ID
	fnTypeIDs     map[string]int32 // fn signature string → compile-time type ID
	nextTypeID    int32            // counter; starts at 6

	// Reflection metadata.
	// structImpls: struct name → []trait name strings (for traitof/typeof)
	structImpls map[string][]string
	// structFieldLLVMTypes: struct name → []LLVM type per user field (for getfield/setfield)
	structFieldLLVMTypes map[string][]irtypes.Type

	// match subject: set before entering genWhereList when the function body
	// is a pure where-list pattern match. Used to compare atom conditions.
	matchSubject value.Value

	// strcmpFn: lazily declared C strcmp
	strcmpFn *ir.Func
	// anyEqFn: lazily declared _tin_any_eq runtime helper
	anyEqFn *ir.Func

	// macros: macro name → MacroDecl AST
	macros map[string]*ast.MacroDecl

	// linkLibs: libraries to pass to the linker (from `use extern` lib entries)
	linkLibs []string

	// test mode: when true, TestDecl blocks are compiled into test functions
	// and a test-runner main is generated instead of the normal implicit main.
	testMode  bool
	testDecls []*ast.TestDecl
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
	return &CodeGen{
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
		structTypeIDs:            make(map[string]int32),
		dataTypeIDs:              make(map[string]int32),
		fnTypeIDs:                make(map[string]int32),
		nextTypeID:               6, // 0–5 reserved for anyTag* primitives (fn=5)
		structImpls:              make(map[string][]string),
		structFieldLLVMTypes:     make(map[string][]irtypes.Type),
	}
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

	// Second pass: pre-declare all functions (signatures only) so forward calls work.
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			if err := cg.predeclareFunc(fd); err != nil {
				return nil, err
			}
		}
		if sd, ok := node.(*ast.StructDecl); ok {
			aug := cg.augmentStructFromTraits(sd)
			for _, m := range aug.Methods {
				if err := cg.predeclareMethod(aug.Name, m); err != nil {
					return nil, err
				}
			}
		}
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
			// skip for now
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
			wb.NewCall(userMainFn)
			wb.NewRet(constant.NewInt(irtypes.I32, 0))
		}
	}

	// Write module file for any package exports in this source file.
	if err := cg.writeModuleFiles(prog); err != nil {
		return nil, err
	}

	return cg.mod, nil
}

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

// ── Pre-registration pass ──────────────────────────────────────────────────────

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
	case *ast.TypeDecl:
		// Simple type aliases (type char = u8) go straight into typeAliases.
		// Struct-monomorphization aliases (type point = tuple[f32]) are handled
		// in genTypeDecl so that all struct templates are known first.
		if _, isGeneric := n.Type.(*ast.GenericType); !isGeneric {
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
	}
	return nil
}

// ── Top-level declarations ─────────────────────────────────────────────────────

// traitBaseName returns the bare name of a trait TypeExpr (ignoring type params).
func traitBaseName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		return t.Name
	}
	return ""
}

// structHasMethod checks whether a struct directly defines a method named name.
func structHasMethod(s *ast.StructDecl, name string) bool {
	for _, m := range s.Methods {
		if m.Name == name {
			return true
		}
	}
	return false
}

// structHasField checks whether a struct directly declares a field named name.
func structHasField(s *ast.StructDecl, name string) bool {
	for _, f := range s.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// augmentStructFromTraits returns a copy of the struct with forward fields and
// default methods injected from implemented traits.
func (cg *CodeGen) augmentStructFromTraits(n *ast.StructDecl) *ast.StructDecl {
	if len(n.Implements) == 0 {
		return n
	}

	aug := &ast.StructDecl{
		Name:       n.Name,
		TypeParams: n.TypeParams,
		Fields:     append([]ast.StructField{}, n.Fields...),
		Methods:    append([]*ast.FuncDecl{}, n.Methods...),
		Tags:       n.Tags,
	}

	for _, impl := range n.Implements {
		name := traitBaseName(impl)
		trait, ok := cg.traits[name]
		if !ok {
			continue
		}

		// Inject forward fields that the struct doesn't already have.
		for _, ff := range trait.ForwardFields {
			if !structHasField(aug, ff.Name) {
				aug.Fields = append(aug.Fields, ff)
			}
		}

		// Inject default (non-virtual) methods the struct doesn't override.
		for _, m := range trait.Methods {
			if m.IsVirtual || m.Body == nil {
				continue // virtual — struct must provide its own
			}
			if !structHasMethod(aug, m.Name) {
				// Bind "this" parameter to this struct type.
				injected := *m
				if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
					injected.Params = append([]ast.Param{
						{Name: "this", Type: &ast.SimpleType{Name: n.Name}},
					}, injected.Params...)
				} else {
					// Fix this param to use this struct's type.
					newParams := make([]ast.Param, len(injected.Params))
					copy(newParams, injected.Params)
					newParams[0].Type = &ast.SimpleType{Name: n.Name}
					injected.Params = newParams
				}
				aug.Methods = append(aug.Methods, &injected)
			}
		}
	}

	return aug
}

func (cg *CodeGen) genStructDecl(n *ast.StructDecl) error {
	if len(n.TypeParams) > 0 {
		return nil // generic template — only compiled when monomorphized
	}
	orig := n // keep original for Implements list
	n = cg.augmentStructFromTraits(n)
	n.Implements = orig.Implements // preserve for vtable generation
	st, ok := cg.structTypes[n.Name]
	if !ok {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// ── Prepend vtable pointer fields for each non-implicit implemented trait ──
	// buildTraitFatPtrTypeInst is idempotent; calling it here ensures the vtable
	// struct type is registered before we reference its pointer type.
	var vtableInstKeys []string
	var vtableFieldTypes []irtypes.Type
	for _, impl := range orig.Implements {
		traitName := traitBaseName(impl)
		if traitName == "implicit" {
			continue
		}
		if _, ok := cg.traits[traitName]; !ok {
			continue
		}
		instKey := traitImplKey(impl)
		typeSubst := map[string]irtypes.Type{}
		if gt, ok2 := impl.(*ast.GenericType); ok2 {
			td := cg.traits[traitName]
			for i, tpName := range td.TypeParams {
				if i < len(gt.TypeParams) {
					lt, err := cg.tinTypeToLLVM(gt.TypeParams[i])
					if err != nil {
						return err
					}
					typeSubst[tpName] = lt
				}
			}
		}
		if _, err := cg.buildTraitFatPtrTypeInst(traitName, instKey, typeSubst); err != nil {
			return err
		}
		vtableSt := cg.traitVtableStructTypes[instKey]
		vtableInstKeys = append(vtableInstKeys, instKey)
		vtableFieldTypes = append(vtableFieldTypes, irtypes.NewPointer(vtableSt))
	}
	cg.structVtableOrder[n.Name] = vtableInstKeys

	// ── Build user field types ────────────────────────────────────────────────
	var userFieldTypes []irtypes.Type
	var fieldNames []string
	for _, f := range n.Fields {
		ft, err := cg.tinTypeToLLVM(f.Type)
		if err != nil {
			return err
		}
		userFieldTypes = append(userFieldTypes, ft)
		fieldNames = append(fieldNames, f.Name)
	}
	// Assign a compile-time type ID for this struct (used by any boxing /
	// runtime type checks).  IDs are stable within a compilation unit.
	if _, exists := cg.structTypeIDs[n.Name]; !exists {
		cg.structTypeIDs[n.Name] = cg.nextTypeID
		cg.nextTypeID++
	}
	// Final layout: [i32 type_id, vtable_0*, vtable_1*, …, user_field_0, …]
	// The leading i32 is always field 0; a *struct can be bitcast to *any
	// and the type read directly from field 0.
	st.Fields = append([]irtypes.Type{irtypes.I32}, append(vtableFieldTypes, userFieldTypes...)...)
	cg.structFields[n.Name] = fieldNames // user-visible names only
	cg.structFieldLLVMTypes[n.Name] = userFieldTypes

	// Record which traits this struct implements (for typeof/traitof).
	var implNames []string
	for _, impl := range n.Implements {
		implNames = append(implNames, impl.String())
	}
	cg.structImpls[n.Name] = implNames

	// Generate methods as top-level functions with struct-qualified names.
	for _, m := range n.Methods {
		if err := cg.genStructMethod(n.Name, m); err != nil {
			return err
		}
	}

	// For qualified methods (e.g. fn iter[char]::idx), also register them
	// under the plain name (e.g. struct_idx) when no other method with that
	// plain name already exists. This lets non-disambiguated call sites work.
	plainMethodNames := map[string]bool{}
	for _, m := range n.Methods {
		if m.TraitQualifier == "" {
			plainMethodNames[m.Name] = true
		}
	}
	for _, m := range n.Methods {
		if m.TraitQualifier == "" {
			continue
		}
		if plainMethodNames[m.Name] {
			continue // a plain method already covers this name
		}
		plainName := n.Name + "_" + m.Name
		if _, exists := cg.curScope.lookup(plainName); !exists {
			qualName := methodScopeName(n.Name, m)
			if entry, ok := cg.curScope.lookup(qualName); ok {
				cg.curScope.set(plainName, entry)
				plainMethodNames[m.Name] = true // mark so only first qualifier wins
			}
		}
	}

	// Generate vtable wrappers and global constants for each implemented trait.
	if err := cg.genTraitVtables(n); err != nil {
		return err
	}
	return nil
}

// ── Type-alias / monomorphization ─────────────────────────────────────────────

// substituteTypeInTypeExpr replaces named type parameters in a TypeExpr
// using the provided substitution map (param name → replacement TypeExpr).
func substituteTypeInTypeExpr(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil || len(subst) == 0 {
		return te
	}
	switch t := te.(type) {
	case *ast.SimpleType:
		if rep, ok := subst[t.Name]; ok {
			return rep
		}
	case *ast.GenericType:
		changed := false
		newParams := make([]ast.TypeExpr, len(t.TypeParams))
		for i, p := range t.TypeParams {
			newP := substituteTypeInTypeExpr(p, subst)
			newParams[i] = newP
			if newP != p {
				changed = true
			}
		}
		if changed {
			return &ast.GenericType{Name: t.Name, TypeParams: newParams}
		}
	case *ast.PointerType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.PointerType{Elem: newElem}
		}
	case *ast.ArrayType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.ArrayType{Elem: newElem, Size: t.Size}
		}
	}
	return te
}

// substituteMethod returns a copy of m with type params substituted and
// the self-parameter type renamed from genericName to concreteName.
func substituteMethod(m *ast.FuncDecl, genericName, concreteName string, subst map[string]ast.TypeExpr) *ast.FuncDecl {
	newParams := make([]ast.Param, len(m.Params))
	for i, p := range m.Params {
		newType := substituteTypeInTypeExpr(p.Type, subst)
		// rename the self parameter from the generic struct name to concrete
		if st, ok := newType.(*ast.SimpleType); ok && st.Name == genericName {
			newType = &ast.SimpleType{Name: concreteName}
		}
		newParams[i] = ast.Param{Name: p.Name, Type: newType, IsConst: p.IsConst, IsVarArgs: p.IsVarArgs}
	}
	newRet := substituteTypeInTypeExpr(m.RetType, subst)
	return &ast.FuncDecl{
		Name:     m.Name,
		Params:   newParams,
		RetType:  newRet,
		Body:     m.Body,
		Tags:     m.Tags,
		IsStatic: m.IsStatic,
	}
}

// genTypeDecl handles "type X = SomeType [override = ...]" declarations.
// For simple aliases (type char = u8) the alias was already recorded in
// preregister; this function handles the struct-monomorphization case
// (type point = tuple[f32]) which requires actual LLVM type generation.
func (cg *CodeGen) genTypeDecl(n *ast.TypeDecl) error {
	gt, ok := n.Type.(*ast.GenericType)
	if !ok {
		// Simple alias — already registered in preregister. Nothing to do.
		return nil
	}

	tmpl, isTmpl := cg.genericStructs[gt.Name]
	if !isTmpl {
		// GenericType refers to something other than a generic struct
		// (e.g. a generic trait instantiation used as a type alias).
		cg.typeAliases[n.Name] = n.Type
		return nil
	}

	// Build type-parameter substitution: tmpl.TypeParams[i] → gt.TypeParams[i]
	subst := make(map[string]ast.TypeExpr)
	for i, paramName := range tmpl.TypeParams {
		if i < len(gt.TypeParams) {
			subst[paramName] = gt.TypeParams[i]
		}
	}

	// Build the concrete struct by substituting type params in every field.
	concrete := &ast.StructDecl{
		Name:       n.Name,
		Implements: tmpl.Implements,
	}
	for _, f := range tmpl.Fields {
		concrete.Fields = append(concrete.Fields, ast.StructField{
			Name:      f.Name,
			Type:      substituteTypeInTypeExpr(f.Type, subst),
			Tags:      f.Tags,
			IsForward: f.IsForward,
		})
	}

	// Build method set: start with template methods (substituted), then
	// apply overrides from the TypeDecl.
	overrideSet := make(map[string]*ast.FuncDecl)
	for _, ov := range n.Overrides {
		overrideSet[ov.Name] = ov
	}
	for _, m := range tmpl.Methods {
		if ov, ok := overrideSet[m.Name]; ok {
			concrete.Methods = append(concrete.Methods, ov)
			delete(overrideSet, m.Name)
		} else {
			concrete.Methods = append(concrete.Methods, substituteMethod(m, tmpl.Name, n.Name, subst))
		}
	}
	// Any overrides that don't shadow a template method are appended.
	for _, ov := range n.Overrides {
		if _, already := overrideSet[ov.Name]; !already {
			continue // already applied above
		}
		concrete.Methods = append(concrete.Methods, ov)
	}

	// Register the concrete struct type (opaque first, just like preregister).
	if _, exists := cg.structTypes[n.Name]; !exists {
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	return cg.genStructDecl(concrete)
}

// buildTraitFatPtrType computes (and caches) the LLVM fat-pointer type for a
// trait: { i8*, vtable_struct* }.  The vtable struct has one fn-ptr slot per
// trait method, each with signature (i8* self, …) → ret.
// traitImplKey returns a unique string key for a trait impl TypeExpr.
// For "named" → "named"; for "iter[i64]" → "iter_i64".
func traitImplKey(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		key := t.Name
		for _, tp := range t.TypeParams {
			key += "_" + traitImplKey(tp)
		}
		return key
	}
	return "unknown"
}

// resolveTypeWithSubst converts a TypeExpr to LLVM type, substituting any

// buildTraitFatPtrType computes (and caches) the fat-pointer type for a
// non-generic trait by instKey == traitName.
func (cg *CodeGen) buildTraitFatPtrType(traitName string) (*irtypes.StructType, error) {
	return cg.buildTraitFatPtrTypeInst(traitName, traitName, nil)
}

// buildTraitFatPtrTypeInst computes and caches the fat-pointer type for a
// (possibly generic) trait instantiation.
//   traitName — base trait name (e.g. "iter")
//   instKey   — unique instance key (e.g. "iter_i64")
//   typeSubst — map from trait type-param name → concrete LLVM type
func (cg *CodeGen) buildTraitFatPtrTypeInst(traitName, instKey string, typeSubst map[string]irtypes.Type) (*irtypes.StructType, error) {
	if fp, ok := cg.traitFatPtrTypes[instKey]; ok {
		return fp, nil
	}
	td, ok := cg.traits[traitName]
	if !ok {
		return nil, fmt.Errorf("unknown trait: %s", traitName)
	}

	var methodNames []string
	var fnPtrTypes []irtypes.Type

	if td.IsAlias {
		// "trait X as fn(params) ret" — single method whose name is the trait name
		// and whose signature comes from the alias function type.
		ft, ok := td.AliasType.(*ast.FuncType)
		if !ok {
			return nil, fmt.Errorf("trait %s: alias type is not a function type", traitName)
		}
		methodNames = []string{traitName}
		params := []irtypes.Type{irtypes.I8Ptr} // implicit self
		for _, p := range ft.Params {
			pt, err := cg.resolveTypeWithSubst(p, typeSubst)
			if err != nil {
				return nil, err
			}
			params = append(params, pt)
		}
		var ret irtypes.Type = irtypes.Void
		if ft.RetType != nil {
			var err error
			ret, err = cg.resolveTypeWithSubst(ft.RetType, typeSubst)
			if err != nil {
				return nil, err
			}
		}
		fnPtrTypes = []irtypes.Type{irtypes.NewPointer(irtypes.NewFunc(ret, params...))}
	} else {
		for _, m := range td.Methods {
			methodNames = append(methodNames, m.Name)
			// Wrapper signature: (i8* self, non-self params…) → ret
			params := []irtypes.Type{irtypes.I8Ptr}
			for i, p := range m.Params {
				if i == 0 {
					continue // skip self
				}
				pt, err := cg.resolveTypeWithSubst(p.Type, typeSubst)
				if err != nil {
					return nil, err
				}
				params = append(params, pt)
			}
			var ret irtypes.Type = irtypes.Void
			if m.RetType != nil {
				var err error
				ret, err = cg.resolveTypeWithSubst(m.RetType, typeSubst)
				if err != nil {
					return nil, err
				}
			}
			ft := irtypes.NewFunc(ret, params...)
			fnPtrTypes = append(fnPtrTypes, irtypes.NewPointer(ft))
		}
	}

	vtableSt := irtypes.NewStruct(fnPtrTypes...)
	vtableSt.SetName(instKey + "_vtable")
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, vtableSt)

	fatPtr := irtypes.NewStruct(irtypes.I8Ptr, irtypes.NewPointer(vtableSt))
	fatPtr.SetName(instKey + "_iface")
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, fatPtr)

	cg.traitVtableStructTypes[instKey] = vtableSt
	cg.traitFatPtrTypes[instKey] = fatPtr
	cg.traitMethodOrder[traitName] = methodNames // shared across instantiations
	cg.traitInstKeys[instKey] = traitName
	return fatPtr, nil
}

// genTraitVtables generates, for each trait that structName implements:
//  1. One wrapper function per trait method: structName__instKey__methodName(i8* self, …)
//  2. One vtable global constant referencing those wrappers.
func (cg *CodeGen) genTraitVtables(n *ast.StructDecl) error {
	for _, impl := range n.Implements {
		traitName := traitBaseName(impl)

		// ── Special trait: implicit ────────────────────────────────────────────
		// No vtable: find the static method whose first-param type matches T,
		// then register it as an implicit conversion function.
		if traitName == "implicit" {
			gt, ok := impl.(*ast.GenericType)
			if ok && len(gt.TypeParams) > 0 {
				srcLLVM, err := cg.tinTypeToLLVM(gt.TypeParams[0])
				if err == nil {
					for _, m := range n.Methods {
						if !m.IsStatic || len(m.Params) != 1 {
							continue
						}
						paramLLVM, err2 := cg.tinTypeToLLVM(m.Params[0].Type)
						if err2 != nil {
							continue
						}
						if paramLLVM.Equal(srcLLVM) {
							if fnEntry, ok2 := cg.curScope.lookup(methodScopeName(n.Name, m)); ok2 {
								if fn, ok3 := fnEntry.val.(*ir.Func); ok3 {
									cg.implicitConvFns[n.Name] = append(
										cg.implicitConvFns[n.Name],
										implicitConvEntry{srcLLVM: srcLLVM, fn: fn},
									)
								}
							}
							break
						}
					}
				}
			}
			continue // no vtable for implicit
		}

		td, ok := cg.traits[traitName]
		if !ok {
			continue
		}
		instKey := traitImplKey(impl)
		vtableKey := n.Name + "__" + instKey
		if _, ok := cg.traitVtableGlobals[vtableKey]; ok {
			continue // already generated
		}

		// Build type substitution for generic traits.
		typeSubst := map[string]irtypes.Type{}
		if gt, ok := impl.(*ast.GenericType); ok {
			for i, tpName := range td.TypeParams {
				if i < len(gt.TypeParams) {
					lt, err := cg.tinTypeToLLVM(gt.TypeParams[i])
					if err != nil {
						return err
					}
					typeSubst[tpName] = lt
				}
			}
		}

		// Ensure the fat-pointer type (and vtable struct) is built.
		if _, err := cg.buildTraitFatPtrTypeInst(traitName, instKey, typeSubst); err != nil {
			return err
		}
		vtableSt := cg.traitVtableStructTypes[instKey]
		methodNames := cg.traitMethodOrder[traitName]

		structSt := cg.structTypes[n.Name]
		if structSt == nil {
			continue
		}
		structPtrType := irtypes.NewPointer(structSt)

		// Generate one wrapper per trait method.
		var wrappers []constant.Constant
		for i, methodName := range methodNames {
			wrapSlot := vtableSt.Fields[i].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			wrapperName := n.Name + "__" + instKey + "__" + methodName
			wrapParams := make([]*ir.Param, len(wrapSlot.Params))
			wrapParams[0] = ir.NewParam("self", irtypes.I8Ptr)
			for pi := 1; pi < len(wrapSlot.Params); pi++ {
				wrapParams[pi] = ir.NewParam(fmt.Sprintf("a%d", pi), wrapSlot.Params[pi])
			}
			wrapFn := cg.mod.NewFunc(wrapperName, wrapSlot.RetType, wrapParams...)

			entry := wrapFn.NewBlock("entry")
			// Cast i8* self → structType*, load struct value.
			selfPtr := entry.NewBitCast(wrapParams[0], structPtrType)
			selfVal := entry.NewLoad(structSt, selfPtr)

			// Look up concrete method.
			// First try the trait-qualified name "Struct_traitKey_method",
			// then fall back to the plain "Struct_method".
			qualifiedName := n.Name + "_" + traitQualifierKey(instKey) + "_" + methodName
			concreteName := n.Name + "_" + methodName
			concreteFn, ok := cg.curScope.lookup(qualifiedName)
			if ok {
				concreteName = qualifiedName
			} else {
				concreteFn, ok = cg.curScope.lookup(concreteName)
			}
			if !ok {
				return fmt.Errorf("trait vtable: missing concrete method %s (also tried %s)", concreteName, qualifiedName)
			}
			concreteFunc := concreteFn.val.(*ir.Func)

			// Build call args: selfVal + extra params.
			callArgs := []value.Value{selfVal}
			for pi := 1; pi < len(wrapParams); pi++ {
				callArgs = append(callArgs, wrapParams[pi])
			}
			callArgs = cg.adaptArgs(entry, callArgs, concreteFunc.Sig)
			result := entry.NewCall(concreteFunc, callArgs...)
			if irtypes.IsVoid(result.Type()) {
				entry.NewRet(nil)
			} else {
				entry.NewRet(result)
			}
			wrappers = append(wrappers, wrapFn)
		}

		// Build vtable global constant.
		vtableConst := constant.NewStruct(vtableSt, wrappers...)
		vtableGlobal := cg.mod.NewGlobalDef(vtableKey+"_vtable_data", vtableConst)
		vtableGlobal.Immutable = true
		cg.traitVtableGlobals[vtableKey] = vtableGlobal
	}
	return nil
}

// isTraitFatPtr reports whether t is a trait fat-pointer {i8*, vtable_struct*}.
func (cg *CodeGen) isTraitFatPtr(t irtypes.Type) (string, bool) {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return "", false
	}
	if st.Fields[0] != irtypes.I8Ptr {
		return "", false
	}
	pt, ok := st.Fields[1].(*irtypes.PointerType)
	if !ok {
		return "", false
	}
	vst, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return "", false
	}
	// Check it's a known trait vtable struct (returns instKey, not traitName).
	for instKey, vs := range cg.traitVtableStructTypes {
		if vs == vst {
			return instKey, true
		}
	}
	return "", false
}

// tryCoerceToIter detects whether iterVal implements iter[T] (either already a
// fat pointer or a concrete struct with an iter vtable) and returns the fat
// pointer and instKey if so.
func (cg *CodeGen) tryCoerceToIter(block *ir.Block, iterVal value.Value) (value.Value, string, bool) {
	// Case 1: already a trait fat pointer.
	if instKey, ok := cg.isTraitFatPtr(iterVal.Type()); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}
		if baseTrait == "iter" {
			return iterVal, instKey, true
		}
		return nil, "", false
	}

	// Case 2: concrete struct that has an iter[T] vtable registered.
	structName := cg.typeNameOf(iterVal.Type())
	if structName == "" {
		return nil, "", false
	}
	for vtableKey := range cg.traitVtableGlobals {
		// vtableKey format: "structName__instKey"
		prefix := structName + "__"
		if len(vtableKey) <= len(prefix) || vtableKey[:len(prefix)] != prefix {
			continue
		}
		instKey := vtableKey[len(prefix):]
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}
		if baseTrait != "iter" {
			continue
		}
		// Coerce to iter fat pointer.
		fatPtr, err := cg.coerceToTrait(block, iterVal, instKey)
		if err != nil {
			continue
		}
		return fatPtr, instKey, true
	}
	return nil, "", false
}

// genForIterTrait generates a for-in loop over a value that implements iter[T].
// It calls len() (vtable slot 0) for the count, and get(i) (vtable slot 1) for
// each element.
func (cg *CodeGen) genForIterTrait(block *ir.Block, s *ast.ForStmt, iterFatPtr value.Value, instKey string) (*ir.Block, error) {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	// Look up method order: ["len", "get"]
	methodOrder := cg.traitMethodOrder[baseTrait]
	lenSlot, getSlot := -1, -1
	for i, name := range methodOrder {
		switch name {
		case "len":
			lenSlot = i
		case "get":
			getSlot = i
		}
	}
	if lenSlot < 0 || getSlot < 0 {
		return nil, fmt.Errorf("iter trait %s missing len/get methods", instKey)
	}

	vtableSt := cg.traitVtableStructTypes[instKey]

	// Determine element type from get's return type (vtable slot getSlot).
	getFnType := vtableSt.Fields[getSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	elemType := getFnType.RetType

	// Helper to load a function pointer from a vtable slot.
	loadSlot := func(b *ir.Block, vtablePtr value.Value, slot int) value.Value {
		slotFnType := vtableSt.Fields[slot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		gep := b.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slot)))
		return b.NewLoad(irtypes.NewPointer(slotFnType), gep)
	}

	// Extract components of fat pointer.
	dataPtr := block.NewExtractValue(iterFatPtr, 0)
	vtablePtr := block.NewExtractValue(iterFatPtr, 1)

	// Call len().
	lenFnType := vtableSt.Fields[lenSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	lenFnPtr := loadSlot(block, vtablePtr, lenSlot)
	totalLen := block.NewCall(lenFnPtr, cg.adaptArgs(block, []value.Value{dataPtr}, lenFnType)...)

	// Alloca for index.
	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	condBlock := cg.newBlock("iterfor.cond")
	bodyBlock := cg.newBlock("iterfor.body")
	afterBlock := cg.newBlock("iterfor.after")

	block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenI64 := cg.coerce(condBlock, totalLen, irtypes.I64)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenI64)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	// Body: call get(idx).
	cg.curScope = newScope(cg.curScope)

	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	getFnPtr := loadSlot(bodyBlock, vtablePtr, getSlot)
	getArgs := cg.adaptArgs(bodyBlock, []value.Value{dataPtr, bodyIdx}, getFnType)
	elemVal := bodyBlock.NewCall(getFnPtr, getArgs...)

	// Register loop variable.
	if s.VarName != "" {
		elemAlloca := bodyBlock.NewAlloca(elemType)
		bodyBlock.NewStore(elemVal, elemAlloca)
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true})
	}

	var bodyErr error
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.curScope = cg.curScope.parent
	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

// coerceToTrait constructs a trait fat pointer {i8* data, vtable*} from a
// concrete struct value or pointer, given the target instKey (e.g. "named" or "iter_i64").
// If structVal is already a *struct (e.g. from malloc), the heap pointer is
// used directly as the data pointer instead of allocating new stack space.
func (cg *CodeGen) coerceToTrait(block *ir.Block, structVal value.Value, instKey string) (value.Value, error) {
	structType := structVal.Type()

	var dataPtr value.Value
	var concreteType irtypes.Type

	if pt, ok := structType.(*irtypes.PointerType); ok {
		// Already a pointer (e.g. from malloc + bitcast). Use it directly.
		dataPtr = block.NewBitCast(structVal, irtypes.I8Ptr)
		concreteType = pt.ElemType
	} else {
		// Value type: alloca to get a stable pointer.
		alloca := block.NewAlloca(structType)
		block.NewStore(structVal, alloca)
		dataPtr = block.NewBitCast(alloca, irtypes.I8Ptr)
		concreteType = structType
	}

	// The vtable global is a compile-time constant that is always correct for
	// the (struct, trait) pair — including malloc'd structs whose embedded
	// vtable field has not yet been initialized.
	structName := cg.typeNameOf(concreteType)
	vtableKey := structName + "__" + instKey
	vtableGlobal, ok := cg.traitVtableGlobals[vtableKey]
	if !ok {
		return nil, fmt.Errorf("no vtable for %s implementing %s", structName, instKey)
	}

	fatPtrType, fpOk := cg.traitFatPtrTypes[instKey]
	if !fpOk {
		return nil, fmt.Errorf("no fat-ptr type for trait %s", instKey)
	}

	// Build fat pointer {i8* data, vtable*}.
	ifaceAlloca := block.NewAlloca(fatPtrType)
	dataGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, dataGep)
	vtableGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(vtableGlobal, vtableGep)
	return block.NewLoad(fatPtrType, ifaceAlloca), nil
}

func (cg *CodeGen) genEnumDecl(n *ast.EnumDecl) error {
	// Determine base LLVM type.
	var baseType irtypes.Type = irtypes.I32
	if n.BaseType != nil {
		bt, err := cg.tinTypeToLLVM(n.BaseType)
		if err != nil {
			return err
		}
		baseType = bt
	}
	cg.enumTypes[n.Name] = baseType

	// Register member values.
	var nextVal int64 = 0
	for _, m := range n.Members {
		var val int64
		if m.Value != nil {
			// Evaluate constant expression.
			if il, ok := m.Value.(*ast.IntLit); ok {
				val = il.Value
			} else {
				val = nextVal
			}
		} else {
			val = nextVal
		}
		key := n.Name + "." + m.Name
		cg.enumValues[key] = val
		nextVal = val + 1
	}
	return nil
}

// genDataDecl registers a non-generic data type as a tagged union.
// Layout: { i32 type_id, i8 variant_tag, [payload_bytes x i8] payload }
// Field 0: i32  – compile-time type ID (same registry as structs, for any boxing)
// Field 1: i8   – variant discriminant: None=0, typed variants 1, 2, …
// Field 2: payload bytes (size = max variant payload)
// Having type_id as field 0 means *data can be bitcast to *any.
func (cg *CodeGen) genDataDecl(n *ast.DataDecl) error {
	if len(n.TypeParams) > 0 {
		// Generic — stored as template; instantiated lazily in tinTypeToLLVM.
		cg.genericDataDecls[n.Name] = n
		return nil
	}
	// Compute max payload size across all typed (non-None) variants.
	var maxSize uint64
	for _, v := range n.Variants {
		if v.Type == nil {
			continue // None
		}
		lt, err := cg.tinTypeToLLVM(v.Type)
		if err != nil {
			return err
		}
		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}
	if maxSize == 0 {
		maxSize = 1
	}
	// Build { i32, i8, [maxSize x i8] }
	payloadType := irtypes.NewArray(maxSize, irtypes.I8)
	// Reuse the opaque placeholder registered in preregister (if any), so any
	// pointers already referencing it remain valid after we fill in the fields.
	st := cg.structTypes[n.Name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}
	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I8, payloadType}
	// Assign a compile-time type ID.
	if _, exists := cg.dataTypeIDs[n.Name]; !exists {
		cg.dataTypeIDs[n.Name] = cg.nextTypeID
		cg.nextTypeID++
	}
	cg.dataDecls[n.Name] = n
	// Assign tag constants.
	tagIdx := int8(0)
	noneTagSet := false
	for i, v := range n.Variants {
		if v.Name == "None" || v.Type == nil && v.Name == "None" {
			key := fmt.Sprintf("%s.%d", n.Name, i)
			cg.dataVariantTags[key] = 0
			cg.enumValues[n.Name+".None"] = 0
			noneTagSet = true
		}
	}
	if !noneTagSet {
		tagIdx = 1
	} else {
		tagIdx = 1
	}
	for i, v := range n.Variants {
		if v.Name == "None" {
			continue
		}
		key := fmt.Sprintf("%s.%d", n.Name, i)
		cg.dataVariantTags[key] = tagIdx
		tagIdx++
	}
	return nil
}

// instantiateDataType creates a concrete tagged-union struct for a generic
// data type instantiated with specific type parameters.
func (cg *CodeGen) instantiateDataType(dd *ast.DataDecl, typeArgs []ast.TypeExpr) (irtypes.Type, error) {
	// Build substitution map: type param name → concrete TypeExpr.
	subst := map[string]ast.TypeExpr{}
	for i, tp := range dd.TypeParams {
		if i < len(typeArgs) {
			subst[tp] = typeArgs[i]
		}
	}
	// Compute max payload size.
	var maxSize uint64
	for _, v := range dd.Variants {
		if v.Type == nil {
			continue
		}
		concreteType := substituteTypeExpr(v.Type, subst)
		lt, err := cg.tinTypeToLLVM(concreteType)
		if err != nil {
			return nil, err
		}
		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}
	if maxSize == 0 {
		maxSize = 1
	}
	payloadType := irtypes.NewArray(maxSize, irtypes.I8)
	// Layout: { i32 type_id, i8 variant_tag, [maxSize x i8] payload }
	st := irtypes.NewStruct(irtypes.I32, irtypes.I8, payloadType)
	// Build instance key (e.g. "maybe__string").
	var keyParts []string
	for _, ta := range typeArgs {
		keyParts = append(keyParts, typeExprName(ta))
	}
	instName := dd.Name + "__" + strings.Join(keyParts, "__")
	// Reuse existing named struct if already instantiated.
	if existing := cg.structTypes[instName]; existing != nil {
		return existing, nil
	}
	st.SetName(instName)
	cg.structTypes[instName] = st
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	// Store a concrete DataDecl (with substituted variants) for is-checking.
	concretDecl := &ast.DataDecl{Name: instName, Variants: make([]ast.DataVariant, len(dd.Variants))}
	for i, v := range dd.Variants {
		if v.Type == nil {
			concretDecl.Variants[i] = v
		} else {
			concretDecl.Variants[i] = ast.DataVariant{Name: v.Name, Type: substituteTypeExpr(v.Type, subst)}
		}
	}
	cg.dataDecls[instName] = concretDecl
	// Also register under the original name if not already there.
	if _, exists := cg.dataDecls[dd.Name]; !exists {
		cg.dataDecls[dd.Name] = concretDecl
	}
	// Assign compile-time type IDs for the instantiated type.
	if _, exists := cg.dataTypeIDs[instName]; !exists {
		cg.dataTypeIDs[instName] = cg.nextTypeID
		cg.nextTypeID++
	}
	if _, exists := cg.dataTypeIDs[dd.Name]; !exists {
		cg.dataTypeIDs[dd.Name] = cg.dataTypeIDs[instName]
	}
	// Populate dataVariantTags for the instantiated type (mirrors genDataDecl logic).
	tagIdx := int8(1)
	for i, v := range concretDecl.Variants {
		key := fmt.Sprintf("%s.%d", instName, i)
		if v.Name == "None" || v.Type == nil {
			cg.dataVariantTags[key] = 0
		} else {
			cg.dataVariantTags[key] = tagIdx
			tagIdx++
		}
	}
	return st, nil
}

// substituteTypeExpr replaces simple type names according to subst map.
func substituteTypeExpr(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil {
		return nil
	}
	switch t := te.(type) {
	case *ast.SimpleType:
		if replacement, ok := subst[t.Name]; ok {
			return replacement
		}
	}
	return te
}

// wrapDataVariant wraps a value into a data type tagged union.
// Returns nil if wrapping is not possible (type mismatch).
func (cg *CodeGen) wrapDataVariant(block *ir.Block, val value.Value, targetSt *irtypes.StructType, dataName string) value.Value {
	dd, ok := cg.dataDecls[dataName]
	if !ok {
		return nil
	}
	// Find the typed variant whose LLVM type matches the value's type.
	for i, v := range dd.Variants {
		if v.Type == nil {
			continue // None
		}
		lt, err := cg.tinTypeToLLVM(v.Type)
		if err != nil {
			continue
		}
		if !lt.Equal(val.Type()) {
			// Try coercion check: same structure
			if llvmTypeSize(lt) != llvmTypeSize(val.Type()) {
				continue
			}
		}
		// Found matching variant.
		key := fmt.Sprintf("%s.%d", dataName, i)
		tag := cg.dataVariantTags[key]
		alloca := block.NewAlloca(targetSt)
		// Store type_id at field 0.
		if typeID, ok := cg.dataTypeIDs[dataName]; ok {
			typeIDGEP := block.NewGetElementPtr(targetSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGEP)
		}
		// Store variant tag at field 1.
		tagGEP := block.NewGetElementPtr(targetSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I8, int64(tag)), tagGEP)
		// Store payload at field 2: bitcast the payload field pointer to the value's type.
		payloadGEP := block.NewGetElementPtr(targetSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(val.Type()))
		block.NewStore(val, payloadPtr)
		return block.NewLoad(targetSt, alloca)
	}
	return nil
}

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
	// Normalise path separator: "std::math" → "std/math"
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
	// Source files take precedence: they are compiled inline so no separate
	// linking step is needed, enabling pure-Tin stdlib implementations.
	tinSrc := strings.TrimSuffix(modFile, ".tin.mod") + ".tin"
	if _, statErr := os.Stat(tinSrc); statErr != nil {
		// Not found relative to source; try stdlib locations.
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
				// (e.g. stdlib/io/io.tin.mod — matches the stdlib source layout).
				p2 := filepath.Join(execDir, "stdlib", pkgName, pkgName) + ".tin.mod"
				mf, err = ReadModFile(p2)
			}
		}
	}
	if err != nil {
		// Module file not found — not an error if the file simply doesn't exist
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

// ── Constrained generic function monomorphization ─────────────────────────────

// monomorphizeFunc compiles a concrete instance of a constrained generic
// function by substituting type-parameter names with concrete struct names.
//
// instKey is the unique suffix, e.g. "animal" for fn foo[t] with t→animal.
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
			Name:     p.Name,
			Type:     substituteTypeInTypeExpr(p.Type, astSubst),
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

		// Return type needs wrapping (e.g. char* → string fat-ptr).
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

		// Call _tin_test_finish(N) → i64 exit code.
		total := constant.NewInt(irtypes.I64, int64(len(cg.testDecls)))
		rc64 := cur.NewCall(finishFn, total)
		rc32 := cur.NewTrunc(rc64, irtypes.I32)
		cur.NewRet(rc32)
	}

	cg.curFn = prevFn
	cg.curScope = prevScope
	return nil
}

// ── Body generation ────────────────────────────────────────────────────────────

// genBody generates a function body from a node (Block, WhereList, or expression).
// Returns whether the block was terminated.
func (cg *CodeGen) genBody(block *ir.Block, body ast.Node, retType irtypes.Type) (bool, error) {
	addDefaultRet := func(b *ir.Block) {
		if b != nil && b.Term == nil {
			_ = cg.emitDefers(b)
			cg.emitAllScopeReleases(b, "")
			if irtypes.IsVoid(retType) {
				b.NewRet(nil)
			} else {
				b.NewRet(cg.zeroValue(retType))
			}
		}
	}
	switch b := body.(type) {
	case *ast.Block:
		newBlock, term, err := cg.genBlock(block, b)
		if err != nil {
			return false, err
		}
		if !term {
			addDefaultRet(newBlock)
		}
		return true, nil
	case *ast.WhereList:
		return cg.genWhereList(block, b, retType)
	case nil:
		return false, nil
	case *ast.ExprStmt:
		// Single expression-statement body (e.g. fn foo() = someCall())
		// For void functions, generate the call and add a default return.
		// For value-returning functions, unwrap and treat as an expression.
		inner := b.Expr
		if !irtypes.IsVoid(retType) {
			val, err := cg.genExpr(block, inner)
			if err != nil {
				return false, err
			}
			if val != nil {
				val = cg.coerce(block, val, retType)
				_ = cg.emitDefers(block)
				retSkip := ""
				if ident, ok := inner.(*ast.Identifier); ok {
					retSkip = ident.Name
				}
				cg.emitAllScopeReleases(block, retSkip)
				block.NewRet(val)
			} else {
				addDefaultRet(block)
			}
			return true, nil
		}
		// Void: generate as statement.
		newBlock, terminated, err := cg.genStmt(block, b)
		if err != nil {
			return false, err
		}
		if !terminated {
			addDefaultRet(newBlock)
		}
		return true, nil
	case *ast.ReturnStmt, *ast.EchoStmt, *ast.AssignStmt, *ast.PostfixStmt,
		*ast.VarDecl, *ast.IfStmt, *ast.ForStmt, *ast.MatchStmt, *ast.DeferStmt:
		// Single statement body (e.g. fn foo() T = return expr)
		newBlock, terminated, err := cg.genStmt(block, body)
		if err != nil {
			return false, err
		}
		if !terminated {
			addDefaultRet(newBlock)
		}
		return true, nil
	default:
		// Single expression body (e.g. fn foo() = expr)
		val, err := cg.genExpr(block, body)
		if err != nil {
			return false, err
		}
		if !irtypes.IsVoid(retType) && val != nil {
			val = cg.coerce(block, val, retType)
			_ = cg.emitDefers(block)
			retSkip := ""
			if ident, ok := body.(*ast.Identifier); ok {
				retSkip = ident.Name
			}
			cg.emitAllScopeReleases(block, retSkip)
			block.NewRet(val)
		} else {
			_ = cg.emitDefers(block)
			cg.emitAllScopeReleases(block, "")
			block.NewRet(nil)
		}
		return true, nil
	}
}

// genBlock generates a sequence of statements in the given block.
// Returns (currentBlock, terminated, error). currentBlock is the block that
// should receive the next instruction after the block's statements; it may
// differ from the incoming block when nested control-flow (if/for/match)
// creates new merge blocks.
func (cg *CodeGen) genBlock(block *ir.Block, b *ast.Block) (*ir.Block, bool, error) {
	var err error
	for _, stmt := range b.Stmts {
		var terminated bool
		block, terminated, err = cg.genStmt(block, stmt)
		if err != nil {
			return nil, false, err
		}
		if terminated || block == nil {
			return nil, true, nil
		}
	}
	return block, false, nil
}

// isStmtNode reports whether an AST node is inherently a statement (not an
// expression that also appears as a statement).
func isStmtNode(node ast.Node) bool {
	switch node.(type) {
	case *ast.Block, *ast.ReturnStmt, *ast.EchoStmt, *ast.AssignStmt,
		*ast.AugAssignStmt, *ast.PostfixStmt, *ast.VarDecl,
		*ast.IfStmt, *ast.ForStmt, *ast.MatchStmt, *ast.DeferStmt,
		*ast.BreakStmt, *ast.FuncDecl, *ast.TaggedBlock:
		return true
	}
	return false
}

// genWhereBody generates the body of a where clause (which may be an
// expression, a statement, or a block) and emits an appropriate terminator.
func (cg *CodeGen) genWhereBody(block *ir.Block, body ast.Node, retType irtypes.Type) error {
	// If the body is an ExprStmt wrapping an expression, unwrap it so we
	// can capture the return value.
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}

	if isStmtNode(body) {
		newBlock, terminated, err := cg.genStmt(block, body)
		if err != nil {
			return err
		}
		if !terminated && newBlock != nil && newBlock.Term == nil {
			_ = cg.emitDefers(newBlock)
			newBlock.NewRet(nil)
		}
		return nil
	}

	// Expression body: evaluate and return value.
	bodyVal, err := cg.genExpr(block, body)
	if err != nil {
		return err
	}
	if !irtypes.IsVoid(retType) && bodyVal != nil {
		bodyVal = cg.coerce(block, bodyVal, retType)
		_ = cg.emitDefers(block)
		block.NewRet(bodyVal)
	} else {
		_ = cg.emitDefers(block)
		block.NewRet(nil)
	}
	return nil
}

// genWhereCondition generates an i1 condition for a where clause condition.
// When the condition is an AtomLit and a match subject is set, it emits a
// string comparison against the subject.
func (cg *CodeGen) genWhereCondition(block *ir.Block, condNode ast.Node) (value.Value, error) {
	if atom, ok := condNode.(*ast.AtomLit); ok && cg.matchSubject != nil {
		// Compare matchSubject (string fat-ptr) against the atom string.
		atomStr := cg.buildStringFatPtr(block, "'"+atom.Name)
		subjectPtr := cg.extractStringPtr(block, cg.matchSubject)
		atomPtr := cg.extractStringPtr(block, atomStr)
		cmpResult := block.NewCall(cg.ensureStrcmp(), subjectPtr, atomPtr)
		return block.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0)), nil
	}
	cond, err := cg.genExpr(block, condNode)
	if err != nil {
		return nil, err
	}
	return cg.toBool(block, cond), nil
}

// genWhereList generates a chain of if/else blocks for where clauses.
func (cg *CodeGen) genWhereList(block *ir.Block, wl *ast.WhereList, retType irtypes.Type) (bool, error) {
	// mergeBlock is created lazily so it only gets added to the function
	// if actually needed (when no wildcard catches everything).
	var mergeBlock *ir.Block
	getMerge := func() *ir.Block {
		if mergeBlock == nil {
			mergeBlock = cg.newBlock("where.merge")
		}
		return mergeBlock
	}

	for i, clause := range wl.Clauses {
		if clause.Cond == nil {
			// Wildcard: always executes. Ensure merge has a terminator if it exists.
			if mergeBlock != nil && mergeBlock.Term == nil {
				mergeBlock.NewUnreachable()
			}
			if err := cg.genWhereBody(block, clause.Body, retType); err != nil {
				return false, err
			}
			return true, nil
		}

		// Evaluate condition.
		cond, err := cg.genWhereCondition(block, clause.Cond)
		if err != nil {
			return false, err
		}

		thenBlock := cg.newBlock(fmt.Sprintf("where.then.%d", i))
		var elseBlock *ir.Block
		if i == len(wl.Clauses)-1 {
			elseBlock = getMerge()
		} else {
			elseBlock = cg.newBlock(fmt.Sprintf("where.else.%d", i))
		}

		block.NewCondBr(cond, thenBlock, elseBlock)

		// Generate then body.
		if err := cg.genWhereBody(thenBlock, clause.Body, retType); err != nil {
			return false, err
		}

		block = elseBlock
	}

	// Fallthrough: unreachable.
	m := getMerge()
	if m.Term == nil {
		m.NewUnreachable()
	}
	return true, nil
}


// ── Statement generation ──────────────────────────────────────────────────────

// genStmt generates a single statement. Returns (currentBlock, terminated, error).
// If the block was terminated (ret/br), currentBlock may be nil.
func (cg *CodeGen) genStmt(block *ir.Block, node ast.Node) (*ir.Block, bool, error) {
	switch s := node.(type) {

	case *ast.Block:
		newBlock, term, err := cg.genBlock(block, s)
		if err != nil {
			return nil, false, err
		}
		if term {
			return nil, true, nil
		}
		return newBlock, false, nil

	case *ast.VarDecl:
		block, err := cg.genVarDecl(block, s)
		return block, false, err

	case *ast.ReturnStmt:
		if err := cg.genReturn(block, s); err != nil {
			return nil, false, err
		}
		return nil, true, nil

	case *ast.BreakStmt:
		// We handle break by returning nil; the loop handler deals with it.
		return nil, true, nil

	case *ast.EchoStmt:
		var err error
		block, err = cg.genEcho(block, s)
		return block, false, err

	case *ast.ExprStmt:
		_, err := cg.genExpr(block, s.Expr)
		return block, false, err

	case *ast.AssignStmt:
		err := cg.genAssign(block, s)
		return block, false, err

	case *ast.AugAssignStmt:
		err := cg.genAugAssign(block, s)
		return block, false, err

	case *ast.PostfixStmt:
		err := cg.genPostfix(block, s)
		return block, false, err

	case *ast.IfStmt:
		newBlock, term, err := cg.genIf(block, s)
		return newBlock, term, err

	case *ast.ForStmt:
		newBlock, err := cg.genFor(block, s)
		return newBlock, false, err

	case *ast.MatchStmt:
		newBlock, err := cg.genMatch(block, s)
		return newBlock, false, err

	case *ast.DeferStmt:
		// 1. Generate a zero-param thunk that captures free variables from the
		//    current scope by value (same semantics as a closure).
		fnI8, envI8, err := cg.genDeferThunk(block, s.Call)
		if err != nil {
			return nil, false, err
		}
		// 2. Push thunk + env onto the runtime defer chain so that _tin_panic
		//    can run it during cross-frame stack unwinding.
		cg.ensureDeferChain()
		entryAlloca := block.NewAlloca(cg.deferEntryType)
		entryI8 := block.NewBitCast(entryAlloca, irtypes.I8Ptr)
		block.NewCall(cg.deferPushFn, entryI8, fnI8, envI8)
		cg.pendingDeferFrames = append(cg.pendingDeferFrames, entryI8)
		// 3. Also record the original call for inline LIFO emission on normal return.
		cg.pendingDefers = append(cg.pendingDefers, s.Call)
		return block, false, nil

	case *ast.FuncDecl:
		// Nested function declaration - hoist to top level.
		if err := cg.genFuncDecl(s); err != nil {
			return nil, false, err
		}
		return block, false, nil

	case *ast.TaggedBlock:
		return cg.genStmt(block, s.Body)

	default:
		// Unknown statement - try as expression.
		_, err := cg.genExpr(block, node)
		if err != nil {
			return nil, false, err
		}
		// If genExpr terminated the block (e.g. via panic builtin), signal that.
		if block.Term != nil {
			return nil, true, nil
		}
		return block, false, nil
	}
}

func (cg *CodeGen) genVarDecl(block *ir.Block, s *ast.VarDecl) (*ir.Block, error) {
	var llType irtypes.Type
	var err error

	if s.Type != nil {
		llType, err = cg.tinTypeToLLVM(s.Type)
		if err != nil {
			return nil, err
		}
	}

	var initVal value.Value
	if s.Value != nil {
		// Special-case: `let m maybe[T] = None` → zero-tagged struct.
		if _, isNone := s.Value.(*ast.NoneLit); isNone && llType != nil {
			if noneVal := cg.makeNoneValue(block, llType); noneVal != nil {
				alloca := block.NewAlloca(llType)
				block.NewStore(noneVal, alloca)
				cg.curScope.set(s.Name, &scopeEntry{val: alloca, isAlloc: true})
				return block, nil
			}
		}
		initVal, err = cg.genExpr(block, s.Value)
		if err != nil {
			return nil, err
		}
		if llType == nil {
			llType = initVal.Type()
		}
	}

	if llType == nil {
		llType = irtypes.I64
	}

	alloca := block.NewAlloca(llType)
	isRC := isRCTrackedType(llType)
	if initVal != nil {
		// If the init value is an empty array {i8*, i64} but the declared type
		// is a typed fat array {T*, i64}, use a properly-typed zero value.
		if !initVal.Type().Equal(llType) {
			if isFatArrayPtr(initVal.Type()) && isFatArrayPtr(llType) {
				initVal = cg.zeroValue(llType)
			}
		}
		initVal = cg.coerce(block, initVal, llType)
		block.NewStore(initVal, alloca)
		// ARC: retain when copying from an existing variable (identifier).
		// emitRetain handles RC-tracked values (fat arrays, strings, any) and
		// named structs with RC-tracked fields, and is a no-op for everything else.
		if isCopyExpr(s.Value) {
			cg.emitRetain(block, initVal)
		}
	} else {
		// Zero-initialize.
		block.NewStore(cg.zeroValue(llType), alloca)
	}
	cg.curScope.set(s.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC})
	return block, nil
}

// emitDefers emits all pending deferred calls in LIFO order into block.
// For each defer, it pops that single entry from the runtime chain before
// executing it inline.  This ensures that if a deferred call itself panics,
// the remaining (not-yet-run) defers are still in the chain and will be
// executed by _tin_panic.
func (cg *CodeGen) emitDefers(block *ir.Block) error {
	n := len(cg.pendingDefers)
	if n == 0 {
		return nil
	}
	for i := n - 1; i >= 0; i-- {
		// Deregister this one entry before running it.
		if cg.deferPopFn != nil {
			block.NewCall(cg.deferPopFn, constant.NewInt(irtypes.I64, 1))
		}
		if _, err := cg.genExpr(block, cg.pendingDefers[i]); err != nil {
			return err
		}
	}
	cg.pendingDeferFrames = nil
	cg.pendingDefers = nil
	return nil
}

func (cg *CodeGen) genReturn(block *ir.Block, s *ast.ReturnStmt) error {
	if s.Value == nil {
		if err := cg.emitDefers(block); err != nil {
			return err
		}
		cg.emitAllScopeReleases(block, "")
		block.NewRet(nil)
		return nil
	}
	if cg.curFn != nil {
		retType := cg.curFn.Sig.RetType
		// Special-case: `return None` for a data-type function → zero-tagged struct.
		if _, isNone := s.Value.(*ast.NoneLit); isNone {
			if noneVal := cg.makeNoneValue(block, retType); noneVal != nil {
				cg.emitAllScopeReleases(block, "")
				block.NewRet(noneVal)
				return nil
			}
		}
	}
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
	}
	if cg.curFn != nil {
		retType := cg.curFn.Sig.RetType
		if !irtypes.IsVoid(retType) {
			val = cg.coerce(block, val, retType)
		}
	}
	if err := cg.emitDefers(block); err != nil {
		return err
	}
	// ARC: release all RC locals except the one being returned
	// (to transfer its rc=1 ownership to the caller).
	retSkipName := ""
	if ident, ok := s.Value.(*ast.Identifier); ok {
		retSkipName = ident.Name
	}
	cg.emitAllScopeReleases(block, retSkipName)
	block.NewRet(val)
	return nil
}

// makeNoneValue builds a None data-union struct value for the given target
// type. Returns nil if the target is not a data type.
// Layout: { i32 type_id, i8 variant_tag=0, [n x i8] payload=zeros }
func (cg *CodeGen) makeNoneValue(block *ir.Block, target irtypes.Type) value.Value {
	st, ok := target.(*irtypes.StructType)
	if !ok {
		return nil
	}
	name := cg.typeNameOf(target)
	if _, isData := cg.dataDecls[name]; !isData {
		return nil
	}
	alloca := block.NewAlloca(st)
	block.NewStore(cg.zeroValue(st), alloca)
	// Set the type_id field (field 0) to the data type's compile-time ID.
	if typeID, ok := cg.dataTypeIDs[name]; ok {
		typeIDGEP := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGEP)
	}
	// Variant tag (field 1) stays 0 = None.
	return block.NewLoad(st, alloca)
}

// genBuiltinLen implements the len(expr) built-in: returns the i64 length of
// strings, dynamic arrays, or the constant size of static arrays.
func (cg *CodeGen) genBuiltinLen(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}
	t := val.Type()
	// String fat-ptr {i8*, i64}: extract field 1.
	if isStringType(t) {
		return cg.extractStringLen(block, val), nil
	}
	// Dynamic array fat-ptr {T*, i64}: extract field 1.
	if isFatArrayPtr(t) {
		st := t.(*irtypes.StructType)
		alloca := block.NewAlloca(st)
		block.NewStore(val, alloca)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		return block.NewLoad(irtypes.I64, gep), nil
	}
	// Static array [N x T]: constant length.
	if at, ok := t.(*irtypes.ArrayType); ok {
		return constant.NewInt(irtypes.I64, int64(at.Len)), nil
	}
	return nil, fmt.Errorf("len() not supported for type %s", t)
}

// ── Defer chain helpers ────────────────────────────────────────────────────────

// ensureDeferChain lazily declares the runtime defer-chain functions and
// initialises the TinDeferEntry LLVM struct type.
func (cg *CodeGen) ensureDeferChain() {
	if cg.deferPushFn != nil {
		return
	}
	// { i8* prev, i8* fn, i8* env }  mirrors TinDeferEntry in runtime.c
	cg.deferEntryType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr)
	cg.deferPushFn = cg.mod.NewFunc("_tin_defer_push", irtypes.Void,
		ir.NewParam("entry", irtypes.I8Ptr),
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("env", irtypes.I8Ptr),
	)
	cg.deferPopFn = cg.mod.NewFunc("_tin_defer_pop", irtypes.Void,
		ir.NewParam("n", irtypes.I64),
	)
}

// genDeferThunk generates a zero-param thunk function that, when called,
// executes the deferred call expression.  Free variables referenced by the
// call are captured by value into a heap-allocated env struct (same mechanics
// as genLambdaExpr).  Returns (fn as i8*, env as i8*).
func (cg *CodeGen) genDeferThunk(block *ir.Block, call ast.Node) (value.Value, value.Value, error) {
	name := fmt.Sprintf("defer.thunk.%d", cg.strCount)
	cg.strCount++

	// ── Step 1: collect free variables ──────────────────────────────────────
	freeNames := collectFreeVars(call, map[string]bool{})

	type capture struct {
		name   string
		val    value.Value
		llvmTy irtypes.Type
	}
	var captures []capture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function — reachable by name, no capture needed
		}
		var val value.Value
		var ty irtypes.Type
		if entry.isAlloc {
			pt := entry.val.Type().(*irtypes.PointerType)
			ty = pt.ElemType
			val = block.NewLoad(ty, entry.val)
		} else {
			val = entry.val
			ty = val.Type()
		}
		captures = append(captures, capture{n, val, ty})
	}

	// ── Step 2: build env struct and heap-allocate it ────────────────────────
	var envI8 value.Value = constant.NewNull(irtypes.I8Ptr)
	var envStructType *irtypes.StructType

	if len(captures) > 0 {
		fields := make([]irtypes.Type, len(captures))
		for i, c := range captures {
			fields[i] = c.llvmTy
		}
		envStructType = irtypes.NewStruct(fields...)

		nullEnvPtr := constant.NewNull(irtypes.NewPointer(envStructType))
		oneGEP := block.NewGetElementPtr(envStructType, nullEnvPtr, constant.NewInt(irtypes.I32, 1))
		envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
		envI8 = block.NewCall(cg.ensureMalloc(), envSize)

		envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := block.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			block.NewStore(c.val, gep)
		}
	}

	// ── Step 3: create the thunk IR function void(i8* env) ──────────────────
	f := cg.mod.NewFunc(name, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))
	entryBlock := f.NewBlock("entry")

	// Save and reset context so the thunk body doesn't inherit the caller's
	// pending defers or scope.
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevDefers := cg.pendingDefers
	prevDeferFrames := cg.pendingDeferFrames

	cg.curFn = f
	cg.pendingDefers = nil
	cg.pendingDeferFrames = nil

	// Root the scope at the global level so top-level functions remain
	// reachable, but local variables from the outer scope are NOT visible
	// (they are accessed exclusively through the env struct below).
	global := prevScope
	for global.parent != nil {
		global = global.parent
	}
	cg.curScope = newScope(global)

	// ── Step 4: unpack captures from env ────────────────────────────────────
	if len(captures) > 0 {
		envRaw := f.Params[0]
		envTypedPtr := entryBlock.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := entryBlock.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			alloca := entryBlock.NewAlloca(c.llvmTy)
			loaded := entryBlock.NewLoad(c.llvmTy, gep)
			entryBlock.NewStore(loaded, alloca)
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true})
		}
	}

	// ── Step 5: emit the deferred call ──────────────────────────────────────
	if _, err := cg.genExpr(entryBlock, call); err != nil {
		return nil, nil, err
	}
	entryBlock.NewRet(nil)

	// Restore context.
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.pendingDefers = prevDefers
	cg.pendingDeferFrames = prevDeferFrames

	// Return fn as i8* and env as i8*.
	fnI8 := block.NewBitCast(f, irtypes.I8Ptr)
	return fnI8, envI8, nil
}

// ── panic builtin ──────────────────────────────────────────────────────────────

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable

// expandMacro performs AST-level substitution and evaluates the macro body.
// Each macro parameter is bound to the corresponding argument AST node;
// identifiers in the body that match a parameter name are replaced.
func (cg *CodeGen) expandMacro(block *ir.Block, macro *ast.MacroDecl, args []ast.Node) (value.Value, error) {
	if len(args) != len(macro.Params) {
		return nil, fmt.Errorf("macro %s: expected %d args, got %d",
			macro.Name, len(macro.Params), len(args))
	}
	// Build substitution map: param name → argument AST node.
	subst := make(map[string]ast.Node, len(macro.Params))
	for i, p := range macro.Params {
		subst[p] = args[i]
	}
	// Unwrap ExprStmt if the body was parsed as a statement.
	body := macro.Body
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}
	// Substitute into the body and evaluate.
	expanded := substituteMacroNode(body, subst)
	return cg.genExpr(block, expanded)
}

// substituteMacroNode replaces identifier nodes matching a macro parameter
// with the corresponding argument AST node.
func substituteMacroNode(node ast.Node, subst map[string]ast.Node) ast.Node {
	if node == nil {
		return nil
	}
	switch n := node.(type) {
	case *ast.Identifier:
		if replacement, ok := subst[n.Name]; ok {
			return replacement
		}
		return n
	case *ast.BinExpr:
		return &ast.BinExpr{
			Left:  substituteMacroNode(n.Left, subst),
			Right: substituteMacroNode(n.Right, subst),
			Op:    n.Op,
		}
	case *ast.UnaryExpr:
		return &ast.UnaryExpr{
			Expr: substituteMacroNode(n.Expr, subst),
			Op:   n.Op,
		}
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substituteMacroNode(a, subst)
		}
		return &ast.CallExpr{
			Func:    substituteMacroNode(n.Func, subst),
			Args:    newArgs,
			TypeArgs: n.TypeArgs,
		}
	case *ast.FieldAccess:
		return &ast.FieldAccess{
			Expr:  substituteMacroNode(n.Expr, subst),
			Field: n.Field,
			IsPtr: n.IsPtr,
		}
	case *ast.IndexExpr:
		return &ast.IndexExpr{
			Expr:  substituteMacroNode(n.Expr, subst),
			Index: substituteMacroNode(n.Index, subst),
		}
	case *ast.ExprStmt:
		return &ast.ExprStmt{Expr: substituteMacroNode(n.Expr, subst)}
	}
	return node
}

func (cg *CodeGen) genEcho(block *ir.Block, s *ast.EchoStmt) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return block, nil
	}

	t := val.Type()
	switch {
	case isAnyType(t):
		return cg.genEchoAny(block, val)

	case isStringType(t):
		// Extract data pointer and call printf("%s\n", ptr).
		ptr := cg.extractStringPtr(block, val)
		fmtStr := cg.newGlobalString("%s\n")
		block.NewCall(printf, fmtStr, ptr)

	case irtypes.IsInt(t):
		it := t.(*irtypes.IntType)
		var fmtStr value.Value
		if it.BitSize == 1 {
			// bool: print 0 or 1 via printf
			fmtStr = cg.newGlobalString("%d\n")
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, fmtStr, zext)
			return block, nil
		}
		if it.BitSize == 8 {
			// char/u8: print as character
			fmtStr = cg.newGlobalString("%c\n")
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, fmtStr, zext)
			return block, nil
		}
		fmtStr = cg.newGlobalString("%lld\n")
		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, fmtStr, ext)

	case irtypes.IsFloat(t):
		fmtStr := cg.newGlobalString("%g\n")
		var ext value.Value
		if t == irtypes.Double {
			ext = val
		} else {
			ext = block.NewFPExt(val, irtypes.Double)
		}
		block.NewCall(printf, fmtStr, ext)

	case irtypes.IsPointer(t):
		fmtStr := cg.newGlobalString("%p\n")
		block.NewCall(printf, fmtStr, val)

	default:
		// print trait: struct or fat-pointer with a print() method.
		if strVal, ok := cg.callPrintTrait(block, val); ok {
			ptr := cg.extractStringPtr(block, strVal)
			fmtStr := cg.newGlobalString("%s\n")
			block.NewCall(printf, fmtStr, ptr)
			break
		}
		// Fallback: print as integer.
		fmtStr := cg.newGlobalString("%lld\n")
		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, fmtStr, ext)
	}

	// ARC: release fresh RC-tracked values produced by function calls or
	// concatenation that are not stored in a named variable (temporaries).
	// Named variables are released by their scope entry at scope exit.
	if isRCTrackedType(t) && isTemporaryProducer(s.Value) {
		cg.emitRelease(block, val)
	}

	return block, nil
}

func (cg *CodeGen) genAssign(block *ir.Block, s *ast.AssignStmt) error {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return err
	}
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
	}
	// Get the element type of the pointer.
	ptrType := ptr.Type().(*irtypes.PointerType)
	val = cg.coerce(block, val, ptrType.ElemType)
	// ARC: for RC-tracked types, retain new value (if copy) then release old.
	if isRCTrackedType(ptrType.ElemType) {
		if isCopyExpr(s.Value) {
			cg.emitRetain(block, val)
		}
		oldVal := block.NewLoad(ptrType.ElemType, ptr)
		cg.emitRelease(block, oldVal)
	}
	block.NewStore(val, ptr)
	return nil
}

func (cg *CodeGen) genAugAssign(block *ir.Block, s *ast.AugAssignStmt) error {
	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return err
	}
	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	rhs, err := cg.genExpr(block, s.Value)
	if err != nil {
		return err
	}
	rhs = cg.coerce(block, rhs, elemType)

	var result value.Value
	switch s.Op {
	case "+=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			result = block.NewGetElementPtr(pt.ElemType, current, rhs)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFAdd(current, rhs)
		} else {
			result = block.NewAdd(current, rhs)
		}
	case "-=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			neg := block.NewSub(constant.NewInt(irtypes.I64, 0), rhs)
			result = block.NewGetElementPtr(pt.ElemType, current, neg)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFSub(current, rhs)
		} else {
			result = block.NewSub(current, rhs)
		}
	case "*=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFMul(current, rhs)
		} else {
			result = block.NewMul(current, rhs)
		}
	case "/=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFDiv(current, rhs)
		} else {
			result = block.NewSDiv(current, rhs)
		}
	case "++=":
		// Append element to fat array {T*, i64}.
		// current = {old_ptr, old_len}; rhs = new element of type T.
		// new_len = old_len + 1
		// new_ptr = malloc(new_len * sizeof(T))
		// memcpy(new_ptr, old_ptr, old_len * sizeof(T))
		// new_ptr[old_len] = rhs
		// result = {new_ptr, new_len}
		if isFatArrayPtr(elemType) {
			fatType := elemType.(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			oldPtr := block.NewExtractValue(current, 0)
			oldLen := block.NewExtractValue(current, 1)
			newLen := block.NewAdd(oldLen, constant.NewInt(irtypes.I64, 1))

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
			newBytes := block.NewMul(newLen, elemSize)

			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), newBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy old data.
			oldBytes := block.NewMul(oldLen, elemSize)
			oldI8Ptr := block.NewBitCast(oldPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, oldI8Ptr, oldBytes, constant.NewInt(irtypes.I1, 0))

			// Store new element at index old_len.
			newElemGep := block.NewGetElementPtr(elemT, newPtr, oldLen)
			newElem := cg.coerce(block, rhs, elemT)
			block.NewStore(newElem, newElemGep)

			// Build new fat ptr.
			fatAlloca := block.NewAlloca(fatType)
			ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(newPtr, ptrGep)
			lenGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(newLen, lenGep)
			result = block.NewLoad(fatType, fatAlloca)

			// ARC: release old array data (rc goes to 0 → free) before
			// overwriting the alloca with the new fat ptr.
			block.NewCall(cg.ensureRelease(), oldI8Ptr)
		} else {
			result = rhs
		}
	default:
		result = rhs
	}
	block.NewStore(result, ptr)
	return nil
}

func (cg *CodeGen) genPostfix(block *ir.Block, s *ast.PostfixStmt) error {
	ptr, err := cg.genLValue(block, s.Expr)
	if err != nil {
		return err
	}
	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	one := cg.coerce(block, constant.NewInt(irtypes.I64, 1), elemType)
	var result value.Value
	switch s.Op {
	case "++":
		result = block.NewAdd(current, one)
	case "--":
		result = block.NewSub(current, one)
	default:
		result = current
	}
	block.NewStore(result, ptr)
	return nil
}

func (cg *CodeGen) genIf(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	mergeBlock := cg.newBlock("if.merge")

	cond, err := cg.genExpr(block, s.Cond)
	if err != nil {
		return nil, false, err
	}
	cond = cg.toBool(block, cond)

	thenBlock := cg.newBlock("if.then")
	var elseStart *ir.Block
	if s.Else != nil || len(s.ElseIfs) > 0 {
		elseStart = cg.newBlock("if.else")
	} else {
		elseStart = mergeBlock
	}

	block.NewCondBr(cond, thenBlock, elseStart)

	// Then branch.
	cg.curScope = newScope(cg.curScope)
	thenCurBlock, thenTerm, err := cg.genBlock(thenBlock, s.Then)
	if thenCurBlock == nil {
		thenCurBlock = thenBlock
	}
	// ARC: release scope before popping (only when not already terminated).
	cg.emitScopeRelease(thenCurBlock, cg.curScope)
	cg.curScope = cg.curScope.parent
	if err != nil {
		return nil, false, err
	}
	thenTerminated := thenTerm || thenCurBlock.Term != nil
	if !thenTerminated {
		thenCurBlock.NewBr(mergeBlock)
	}

	// ElseIf chains.
	allElifTerminated := true
	currentElse := elseStart
	for _, elif := range s.ElseIfs {
		nextBlock := cg.newBlock("elif.next")
		elifCond, err := cg.genExpr(currentElse, elif.Cond)
		if err != nil {
			return nil, false, err
		}
		elifCond = cg.toBool(currentElse, elifCond)
		elifThen := cg.newBlock("elif.then")
		currentElse.NewCondBr(elifCond, elifThen, nextBlock)

		cg.curScope = newScope(cg.curScope)
		elifCurBlock, elifTerm, err := cg.genBlock(elifThen, elif.Body)
		if elifCurBlock == nil {
			elifCurBlock = elifThen
		}
		cg.emitScopeRelease(elifCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, false, err
		}
		elifTerminated := elifTerm || elifCurBlock.Term != nil
		if !elifTerminated {
			elifCurBlock.NewBr(mergeBlock)
			allElifTerminated = false
		}
		currentElse = nextBlock
	}

	// Else branch.
	elseTerminated := false
	if s.Else != nil {
		cg.curScope = newScope(cg.curScope)
		elseCurBlock, elseTerm, err := cg.genBlock(currentElse, s.Else)
		if elseCurBlock == nil {
			elseCurBlock = currentElse
		}
		cg.emitScopeRelease(elseCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, false, err
		}
		elseTerminated = elseTerm || elseCurBlock.Term != nil
		if !elseTerminated {
			elseCurBlock.NewBr(mergeBlock)
		}
	} else if currentElse != mergeBlock && currentElse.Term == nil {
		currentElse.NewBr(mergeBlock)
	}

	// Only add unreachable to mergeBlock if ALL branches terminated (returned/
	// branched elsewhere). When there is no else clause, the false path always
	// reaches mergeBlock, so it can never be unreachable.
	allTerminated := thenTerminated && allElifTerminated && (s.Else != nil && elseTerminated)
	if mergeBlock.Term == nil && allTerminated {
		mergeBlock.NewUnreachable()
	}

	return mergeBlock, false, nil
}


func (cg *CodeGen) genFor(block *ir.Block, s *ast.ForStmt) (*ir.Block, error) {
	f := cg.curFn

	switch s.Kind {
	case ast.ForCStyle:
		return cg.genForCStyle(block, s, f)
	case ast.ForIn:
		return cg.genForIn(block, s, f)
	}
	return block, nil
}

func (cg *CodeGen) genForCStyle(block *ir.Block, s *ast.ForStmt, f *ir.Func) (*ir.Block, error) {
	condBlock := cg.newBlock("for.cond")
	bodyBlock := cg.newBlock("for.body")
	postBlock := cg.newBlock("for.post")
	afterBlock := cg.newBlock("for.after")

	// Init: push a scope so the loop variable is scoped to the loop.
	cg.curScope = newScope(cg.curScope)
	if s.Init != nil {
		var err error
		block, _, err = cg.genStmt(block, s.Init)
		if err != nil {
			return nil, err
		}
	}
	if block.Term == nil {
		block.NewBr(condBlock)
	}

	// Cond
	if s.Cond != nil {
		cond, err := cg.genExpr(condBlock, s.Cond)
		if err != nil {
			return nil, err
		}
		cond = cg.toBool(condBlock, cond)
		condBlock.NewCondBr(cond, bodyBlock, afterBlock)
	} else {
		condBlock.NewBr(bodyBlock)
	}

	// Body
	cg.curScope = newScope(cg.curScope)
	var err error
	bodyBlock, _, err = cg.genStmt(bodyBlock, s.Body)
	// ARC: release loop body scope vars before back-edge.
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent
	if err != nil {
		return nil, err
	}
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyBlock.NewBr(postBlock)
	}

	// Post
	if s.Post != nil {
		_, _, err = cg.genStmt(postBlock, s.Post)
		if err != nil {
			return nil, err
		}
	}
	if postBlock.Term == nil {
		postBlock.NewBr(condBlock)
	}

	// ARC: release init scope vars (e.g. loop counter) in the after block.
	cg.emitScopeRelease(afterBlock, cg.curScope)
	cg.curScope = cg.curScope.parent // pop loop scope

	return afterBlock, nil
}

func (cg *CodeGen) genForIn(block *ir.Block, s *ast.ForStmt, f *ir.Func) (*ir.Block, error) {
	// Check if iter is a RangeExpr or a BinExpr with op ".." (start..end).
	if rng, ok := s.Iter.(*ast.RangeExpr); ok {
		return cg.genForRange(block, s, rng, f)
	}
	if bin, ok := s.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
		return cg.genForRange(block, s, &ast.RangeExpr{Start: bin.Left, End: bin.Right}, f)
	}

	// Iterate over a dynamic array: {ptr*, len}.
	iterVal, err := cg.genExpr(block, s.Iter)
	if err != nil {
		return nil, err
	}

	// iter[t] trait: struct (or fat-ptr) implementing iter[T] — use vtable.
	if iterFatPtr, instKey, ok := cg.tryCoerceToIter(block, iterVal); ok {
		return cg.genForIterTrait(block, s, iterFatPtr, instKey)
	}

	// Get element type.
	var elemType irtypes.Type = irtypes.I64
	if s.VarType != nil {
		elemType, err = cg.tinTypeToLLVM(s.VarType)
		if err != nil {
			return nil, err
		}
	}

	condBlock := cg.newBlock("forin.cond")
	bodyBlock := cg.newBlock("forin.body")
	afterBlock := cg.newBlock("forin.after")

	// Extract length and data pointer from fat ptr.
	fatPtrType := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)

	// Alloca to store the fat ptr.
	fatAlloca := block.NewAlloca(iterVal.Type())
	block.NewStore(iterVal, fatAlloca)

	// Extract len.
	// Try to get it as struct.
	lenAlloca := block.NewAlloca(irtypes.I64)
	ptrAlloca := block.NewAlloca(irtypes.NewPointer(elemType))

	if st, ok := iterVal.Type().(*irtypes.StructType); ok && len(st.Fields) >= 2 {
		_ = fatPtrType
		dataGep := block.NewGetElementPtr(iterVal.Type(), fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		lenGep := block.NewGetElementPtr(iterVal.Type(), fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dataPtr := block.NewLoad(irtypes.NewPointer(elemType), dataGep)
		lenVal := block.NewLoad(irtypes.I64, lenGep)
		block.NewStore(dataPtr, ptrAlloca)
		block.NewStore(lenVal, lenAlloca)
	} else {
		// Unknown structure, store zero len.
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenAlloca)
		block.NewStore(constant.NewNull(irtypes.NewPointer(elemType)), ptrAlloca)
	}

	// Loop counter.
	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenVal := condBlock.NewLoad(irtypes.I64, lenAlloca)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenVal)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	bodyPtr := bodyBlock.NewLoad(irtypes.NewPointer(elemType), ptrAlloca)
	elemGep := bodyBlock.NewGetElementPtr(elemType, bodyPtr, bodyIdx)
	elemVal := bodyBlock.NewLoad(elemType, elemGep)

	// Register loop variable.
	elemAlloca := bodyBlock.NewAlloca(elemType)
	bodyBlock.NewStore(elemVal, elemAlloca)
	isElemRC := isRCTrackedType(elemType)
	// ARC: each iteration copies an element — retain to claim ownership.
	if isElemRC {
		cg.emitRetain(bodyBlock, elemVal)
	}
	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true, isRC: isElemRC})
	}

	var bodyErr error
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	// ARC: release loop body scope before back-edge.
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent
	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

func (cg *CodeGen) genForRange(block *ir.Block, s *ast.ForStmt, rng *ast.RangeExpr, f *ir.Func) (*ir.Block, error) {
	start, err := cg.genExpr(block, rng.Start)
	if err != nil {
		return nil, err
	}
	end, err := cg.genExpr(block, rng.End)
	if err != nil {
		return nil, err
	}

	var varType irtypes.Type = irtypes.I64
	if s.VarType != nil {
		varType, err = cg.tinTypeToLLVM(s.VarType)
		if err != nil {
			return nil, err
		}
	}

	start = cg.coerce(block, start, varType)
	end = cg.coerce(block, end, varType)

	// Alloca loop var.
	loopVar := block.NewAlloca(varType)
	block.NewStore(start, loopVar)

	condBlock := cg.newBlock("range.cond")
	bodyBlock := cg.newBlock("range.body")
	afterBlock := cg.newBlock("range.after")

	block.NewBr(condBlock)

	// Cond: i < end.
	iVal := condBlock.NewLoad(varType, loopVar)
	endLoad := cg.coerce(condBlock, end, varType)
	cond := condBlock.NewICmp(enum.IPredSLT, iVal, endLoad)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	// Body.
	cg.curScope = newScope(cg.curScope)
	if s.VarName != "" {
		cg.curScope.set(s.VarName, &scopeEntry{val: loopVar, isAlloc: true})
	}
	var bodyErr error
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.curScope = cg.curScope.parent
	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		iVal2 := bodyBlock.NewLoad(varType, loopVar)
		one := cg.coerce(bodyBlock, constant.NewInt(irtypes.I64, 1), varType)
		newI := bodyBlock.NewAdd(iVal2, one)
		bodyBlock.NewStore(newI, loopVar)
		bodyBlock.NewBr(condBlock)
	}

	return afterBlock, nil
}

func (cg *CodeGen) genMatch(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	expr, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	afterBlock := cg.newBlock("match.after")
	defaultBlock := afterBlock
	if s.Default != nil {
		defaultBlock = cg.newBlock("match.default")
	}

	// Build cases.
	var cases []*ir.Case
	var caseBlocks []*ir.Block
	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)
		pat, err := cg.genExpr(block, c.Pattern)
		if err != nil {
			return nil, err
		}
		if constPat, ok := pat.(constant.Constant); ok {
			intPat := cg.toConstInt(constPat, expr.Type())
			cases = append(cases, ir.NewCase(intPat, caseBlock))
		}
	}

	// Build switch.
	switchExpr := cg.coerce(block, expr, irtypes.I64)
	block.NewSwitch(switchExpr, defaultBlock, cases...)

	// Generate case bodies.
	for i, c := range s.Cases {
		var caseBlock *ir.Block
		if i < len(caseBlocks) {
			caseBlock = caseBlocks[i]
		} else {
			caseBlock = cg.newBlock(fmt.Sprintf("match.case.%d", i))
		}
		cg.curScope = newScope(cg.curScope)
		caseBlock, _, err = cg.genStmt(caseBlock, c.Body)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, err
		}
		if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)
		}
	}

	// Default.
	if s.Default != nil {
		cg.curScope = newScope(cg.curScope)
		defaultBlock, _, err = cg.genStmt(defaultBlock, s.Default)
		cg.curScope = cg.curScope.parent
		if err != nil {
			return nil, err
		}
		if defaultBlock != nil && defaultBlock.Term == nil {
			defaultBlock.NewBr(afterBlock)
		}
	}

	// If afterBlock was never jumped to (all arms terminated), mark unreachable.
	if afterBlock.Term == nil {
		afterBlock.NewUnreachable()
	}

	return afterBlock, nil
}

func (cg *CodeGen) toConstInt(c constant.Constant, targetType irtypes.Type) *constant.Int {
	if ci, ok := c.(*constant.Int); ok {
		if it, ok2 := targetType.(*irtypes.IntType); ok2 {
			return constant.NewInt(it, ci.X.Int64())
		}
		return ci
	}
	return constant.NewInt(irtypes.I64, 0)
}


// ── Expression generation ──────────────────────────────────────────────────────

// genExpr generates code for an expression and returns the resulting value.
func (cg *CodeGen) genExpr(block *ir.Block, node ast.Node) (value.Value, error) {
	if node == nil {
		return nil, nil
	}
	switch e := node.(type) {
	case *ast.IntLit:
		return constant.NewInt(irtypes.I64, e.Value), nil

	case *ast.FloatLit:
		return constant.NewFloat(irtypes.Double, e.Value), nil

	case *ast.BoolLit:
		if e.Value {
			return constant.NewInt(irtypes.I1, 1), nil
		}
		return constant.NewInt(irtypes.I1, 0), nil

	case *ast.CharLit:
		return constant.NewInt(irtypes.I8, int64(e.Value)), nil

	case *ast.NoneLit:
		return constant.NewInt(irtypes.I64, 0), nil

	case *ast.AtomLit:
		// Emit atom name as a string fat-pointer prefixed with apostrophe.
		return cg.buildStringFatPtr(block, "'"+e.Name), nil

	case *ast.StringLit:
		return cg.buildStringFatPtr(block, e.Value), nil

	case *ast.InterpolatedString:
		return cg.genInterpolatedString(block, e)

	case *ast.Identifier:
		return cg.genIdentifier(block, e)

	case *ast.BinExpr:
		return cg.genBinExpr(block, e)

	case *ast.UnaryExpr:
		return cg.genUnaryExpr(block, e)

	case *ast.CallExpr:
		return cg.genCallExpr(block, e)

	case *ast.FieldAccess:
		return cg.genFieldAccess(block, e)

	case *ast.IndexExpr:
		return cg.genIndexExpr(block, e)

	case *ast.ScopeAccess:
		return cg.genScopeAccess(block, e)

	case *ast.ArrayLit:
		return cg.genArrayLit(block, e)

	case *ast.StructLit:
		return cg.genStructLit(block, e)

	case *ast.AsExpr:
		return cg.genAsExpr(block, e)

	case *ast.AddrExpr:
		return cg.genAddrExpr(block, e)

	case *ast.AddressOfExpr:
		return cg.genAddrOfExpr(block, e)

	case *ast.DerefExpr:
		return cg.genDerefExpr(block, e)

	case *ast.PipeExpr:
		return cg.genPipeExpr(block, e)

	case *ast.TernaryExpr:
		return cg.genTernaryExpr(block, e)

	case *ast.IsExpr:
		return cg.genIsExpr(block, e)

	case *ast.RangeExpr:
		// RangeExpr in expression context returns start value.
		return cg.genExpr(block, e.Start)

	case *ast.LambdaExpr:
		return cg.genLambdaExpr(block, e)

	case *ast.WildcardExpr:
		return constant.NewInt(irtypes.I1, 1), nil

	case *ast.DefaultExpr:
		if e.Type != nil {
			lt, err := cg.tinTypeToLLVM(e.Type)
			if err != nil {
				return nil, err
			}
			return cg.zeroValue(lt), nil
		}
		return constant.NewInt(irtypes.I64, 0), nil

	case *ast.SizeofExpr:
		if e.Type == nil {
			return constant.NewInt(irtypes.I64, 0), nil
		}
		lt, err := cg.tinTypeToLLVM(e.Type)
		if err != nil {
			return nil, err
		}
		if irtypes.IsVoid(lt) {
			return constant.NewInt(irtypes.I64, 0), nil
		}
		// GEP trick: sizeof(T) = (i64) &((T*)null)[1]
		nullPtr := constant.NewNull(irtypes.NewPointer(lt))
		gepOne := block.NewGetElementPtr(lt, nullPtr, constant.NewInt(irtypes.I32, 1))
		return block.NewPtrToInt(gepOne, irtypes.I64), nil

	case *ast.TypeAssertExpr:
		return cg.genExpr(block, e.Expr)

	case *ast.TypeofExpr:
		return cg.genTypeof(block, e)

	case *ast.TraitofExpr:
		return cg.genTraitof(block, e)

	case *ast.FieldnamesExpr:
		return cg.genFieldnames(block, e)

	case *ast.FieldtypesExpr:
		return cg.genFieldtypes(block, e)

	case *ast.FieldtagExpr:
		return cg.genFieldtag(block, e)

	case *ast.GetfieldExpr:
		return cg.genGetfield(block, e)

	case *ast.SetfieldExpr:
		return cg.genSetfield(block, e)

	case *ast.VarDecl:
		_, err := cg.genVarDecl(block, e)
		if err != nil {
			return nil, err
		}
		// Return the alloca'd value.
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, nil
		}
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil

	default:
		return nil, nil
	}
}

func (cg *CodeGen) genIdentifier(block *ir.Block, e *ast.Identifier) (value.Value, error) {
	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		return nil, fmt.Errorf("undefined identifier: %s", e.Name)
	}
	if entry.isAlloc {
		ptrType := entry.val.Type().(*irtypes.PointerType)
		return block.NewLoad(ptrType.ElemType, entry.val), nil
	}
	return entry.val, nil
}

func (cg *CodeGen) genBinExpr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// Short-circuit for && and ||.
	switch e.Op {
	case "&&":
		return cg.genLogicalAnd(block, e)
	case "||":
		return cg.genLogicalOr(block, e)
	}

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}

	if left == nil || right == nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	// Unify types.
	lt := left.Type()
	rt := right.Type()

	// Type promotion.
	if irtypes.IsInt(lt) && irtypes.IsInt(rt) {
		lBits := lt.(*irtypes.IntType).BitSize
		rBits := rt.(*irtypes.IntType).BitSize
		if lBits < rBits {
			left = block.NewSExt(left, rt)
			lt = rt
		} else if rBits < lBits {
			right = block.NewSExt(right, lt)
		}
	} else if irtypes.IsFloat(lt) && irtypes.IsInt(rt) {
		right = block.NewSIToFP(right, lt)
	} else if irtypes.IsInt(lt) && irtypes.IsFloat(rt) {
		left = block.NewSIToFP(left, rt)
		lt = rt
	}

	isFloat := irtypes.IsFloat(lt)

	switch e.Op {
	case "+":
		if isFloat {
			return block.NewFAdd(left, right), nil
		}
		return block.NewAdd(left, right), nil
	case "-":
		if isFloat {
			return block.NewFSub(left, right), nil
		}
		return block.NewSub(left, right), nil
	case "*":
		if isFloat {
			return block.NewFMul(left, right), nil
		}
		return block.NewMul(left, right), nil
	case "/":
		if isFloat {
			return block.NewFDiv(left, right), nil
		}
		return block.NewSDiv(left, right), nil
	case "%":
		return block.NewSRem(left, right), nil
	case "==":
		if isFloat {
			return block.NewFCmp(enum.FPredOEQ, left, right), nil
		}
		// any equality: dynamically dispatched by runtime.
		if isAnyType(lt) || isAnyType(rt) {
			if !isAnyType(lt) {
				left = cg.boxToAny(block, left)
			}
			if !isAnyType(rt) {
				right = cg.boxToAny(block, right)
			}
			cmp := block.NewCall(cg.ensureAnyEq(), left, right)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I64, 0)), nil
		}
		// String / atom equality: compare via strcmp.
		if isFatPtrType(lt) {
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		return block.NewICmp(enum.IPredEQ, left, right), nil
	case "!=":
		if isFloat {
			return block.NewFCmp(enum.FPredONE, left, right), nil
		}
		// any inequality: dynamically dispatched by runtime.
		if isAnyType(lt) || isAnyType(rt) {
			if !isAnyType(lt) {
				left = cg.boxToAny(block, left)
			}
			if !isAnyType(rt) {
				right = cg.boxToAny(block, right)
			}
			cmp := block.NewCall(cg.ensureAnyEq(), left, right)
			return block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I64, 0)), nil
		}
		// String / atom inequality: compare via strcmp.
		if isFatPtrType(lt) {
			lptr := cg.extractStringPtr(block, left)
			rptr := cg.extractStringPtr(block, right)
			cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)
			return block.NewICmp(enum.IPredNE, cmp, constant.NewInt(irtypes.I32, 0)), nil
		}
		return block.NewICmp(enum.IPredNE, left, right), nil
	case "<":
		if isFloat {
			return block.NewFCmp(enum.FPredOLT, left, right), nil
		}
		return block.NewICmp(enum.IPredSLT, left, right), nil
	case "<=":
		if isFloat {
			return block.NewFCmp(enum.FPredOLE, left, right), nil
		}
		return block.NewICmp(enum.IPredSLE, left, right), nil
	case ">":
		if isFloat {
			return block.NewFCmp(enum.FPredOGT, left, right), nil
		}
		return block.NewICmp(enum.IPredSGT, left, right), nil
	case ">=":
		if isFloat {
			return block.NewFCmp(enum.FPredOGE, left, right), nil
		}
		return block.NewICmp(enum.IPredSGE, left, right), nil
	case "&":
		return block.NewAnd(left, right), nil
	case "|":
		return block.NewOr(left, right), nil
	case "^":
		return block.NewXor(left, right), nil
	case "<<":
		return block.NewShl(left, right), nil
	case ">>":
		return block.NewAShr(left, right), nil
	case "++":
		// Typed array concatenation: {T*, i64} ++ {T*, i64} → {T*, i64}
		// (strings {i8*, i64} are handled by the string path below)
		if isFatArrayPtr(left.Type()) && !isStringType(left.Type()) {
			fatType := left.Type().(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			leftDataPtr := block.NewExtractValue(left, 0)
			leftLen := block.NewExtractValue(left, 1)
			rightDataPtr := block.NewExtractValue(right, 0)
			rightLen := block.NewExtractValue(right, 1)
			totalLen := block.NewAdd(leftLen, rightLen)

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)

			// new_ptr = _tin_rc_alloc(totalLen * elemSize)
			totalBytes := block.NewMul(totalLen, elemSize)
			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), totalBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy left data
			leftBytes := block.NewMul(leftLen, elemSize)
			leftI8Ptr := block.NewBitCast(leftDataPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, leftI8Ptr, leftBytes, constant.NewInt(irtypes.I1, 0))

			// memcpy right data at offset leftLen*elemSize
			rightOffset := block.NewMul(leftLen, elemSize)
			rightDst := block.NewGetElementPtr(irtypes.I8, newI8Ptr, rightOffset)
			rightI8Ptr := block.NewBitCast(rightDataPtr, irtypes.I8Ptr)
			rightBytes := block.NewMul(rightLen, elemSize)
			block.NewCall(cg.ensureMemcpy(), rightDst, rightI8Ptr, rightBytes, constant.NewInt(irtypes.I1, 0))

			// Build new fat ptr {T*, i64}
			fatAlloca := block.NewAlloca(fatType)
			ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(newPtr, ptrGep)
			lenGep := block.NewGetElementPtr(fatType, fatAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(totalLen, lenGep)
			return block.NewLoad(fatType, fatAlloca), nil
		}

		// String concatenation: both operands are {i8*, i64} fat-ptrs.
		leftPtr := cg.extractStringPtr(block, left)
		leftLen := cg.extractStringLen(block, left)
		rightPtr := cg.extractStringPtr(block, right)
		rightLen := cg.extractStringLen(block, right)
		totalLen := block.NewAdd(leftLen, rightLen)
		// rc_alloc(totalLen + 1) for null terminator; ARC manages the result.
		allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
		buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
		// memcpy(buf, leftPtr, leftLen)
		block.NewCall(cg.ensureMemcpy(), buf, leftPtr, leftLen, constant.NewInt(irtypes.I1, 0))
		// memcpy(buf + leftLen, rightPtr, rightLen)
		rightDst := block.NewGetElementPtr(irtypes.I8, buf, leftLen)
		block.NewCall(cg.ensureMemcpy(), rightDst, rightPtr, rightLen, constant.NewInt(irtypes.I1, 0))
		// null-terminate
		nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
		block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)
		// build {i8*, i64} fat-ptr result
		fatPtrType := stringFatPtrType()
		alloca := block.NewAlloca(fatPtrType)
		gep0 := block.NewGetElementPtr(fatPtrType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(buf, gep0)
		gep1 := block.NewGetElementPtr(fatPtrType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(totalLen, gep1)
		return block.NewLoad(fatPtrType, alloca), nil
	}

	return constant.NewInt(irtypes.I64, 0), nil
}

func (cg *CodeGen) genLogicalAnd(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// genExpr does not thread the current block through return values, so we
	// cannot use real branches here (the caller would keep using the original
	// block which is already terminated, leaving the merge block without a
	// terminator).  Use `select` instead: semantics are identical for pure
	// operands, and side-effectful short-circuit can be revisited later.
	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	leftBool := cg.toBool(block, left)

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	rightBool := cg.toBool(block, right)

	// true && x = x;  false && _ = false
	return block.NewSelect(leftBool, rightBool, constant.NewInt(irtypes.I1, 0)), nil
}

func (cg *CodeGen) genLogicalOr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}
	leftBool := cg.toBool(block, left)

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	rightBool := cg.toBool(block, right)

	// false || x = x;  true || _ = true
	return block.NewSelect(leftBool, constant.NewInt(irtypes.I1, 1), rightBool), nil
}

func (cg *CodeGen) genUnaryExpr(block *ir.Block, e *ast.UnaryExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	switch e.Op {
	case "-":
		if irtypes.IsFloat(val.Type()) {
			return block.NewFNeg(val), nil
		}
		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), val.Type())
		return block.NewSub(zero, val), nil
	case "!":
		b := cg.toBool(block, val)
		return block.NewXor(b, constant.NewInt(irtypes.I1, 1)), nil
	case "~":
		minusOne := cg.coerce(block, constant.NewInt(irtypes.I64, -1), val.Type())
		return block.NewXor(val, minusOne), nil
	case "*":
		// Dereference
		if pt, ok := val.Type().(*irtypes.PointerType); ok {
			return block.NewLoad(pt.ElemType, val), nil
		}
		return val, nil
	}
	return val, nil
}


func (cg *CodeGen) genCallExpr(block *ir.Block, e *ast.CallExpr) (value.Value, error) {
	// Resolve callee.
	var callee value.Value
	var calleeType *irtypes.FuncType

	switch fn := e.Func.(type) {
	case *ast.Identifier:
		// Macro expansion: check before scope lookup.
		macroName := fn.Name
		if macro, ok := cg.macros[macroName]; ok {
			return cg.expandMacro(block, macro, e.Args)
		}
		// Also check with trailing ! stripped (for macro! call syntax).
		if strings.HasSuffix(fn.Name, "!") {
			baseName := fn.Name[:len(fn.Name)-1]
			if macro, ok := cg.macros[baseName+"!"]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}
			if macro, ok := cg.macros[baseName]; ok {
				return cg.expandMacro(block, macro, e.Args)
			}
		}
		// Built-in: len(expr)
		if fn.Name == "len" && len(e.Args) == 1 {
			return cg.genBuiltinLen(block, e.Args[0])
		}
		// Built-in: panic(msg)
		if fn.Name == "panic" && len(e.Args) == 1 {
			return cg.genBuiltinPanic(block, e.Args[0])
		}
		// Check if this is a constrained generic function call — monomorphize it.
		if tmpl, ok := cg.constrainedFuncs[fn.Name]; ok {
			// Evaluate arguments first to infer concrete types.
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}
				argVals = append(argVals, av)
			}
			typeSubst := cg.inferTypeArgs(tmpl, argVals)
			// Build instance key from substituted types.
			instKey := ""
			for i, tp := range tmpl.TypeParams {
				if i > 0 {
					instKey += "__"
				}
				if name, found := typeSubst[tp]; found {
					instKey += name
				} else {
					instKey += tp
				}
			}
			concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
			if err2 != nil {
				return nil, err2
			}
			// Adapt args if needed and call.
			argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
			return block.NewCall(concreteFunc, argVals...), nil
		}
		entry, ok := cg.curScope.lookup(fn.Name)
		if !ok {
			return nil, fmt.Errorf("undefined function: %s", fn.Name)
		}
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			loaded := block.NewLoad(ptrType.ElemType, entry.val)
			// If it's a closure fat pointer, call through it.
			if isFatFnPtr(loaded.Type()) {
				return cg.callFatFn(block, loaded, e.Args)
			}
			callee = loaded
		} else {
			callee = entry.val
		}

	case *ast.FieldAccess:
		// Method call: obj.method(args...)
		objVal, err := cg.genExpr(block, fn.Expr)
		if err != nil {
			return nil, err
		}

		// Trait fat-pointer dispatch: if obj is {i8*, vtable*}, use vtable.
		if traitName, ok := cg.isTraitFatPtr(objVal.Type()); ok {
			return cg.callTraitMethod(block, objVal, traitName, fn.Field, e.Args)
		}

		// Concrete struct method: resolve as StructName_method.
		structName := cg.typeNameOf(objVal.Type())
		methodName := structName + "_" + fn.Field
		entry, ok := cg.curScope.lookup(methodName)
		if !ok {
			// Also check without prefix.
			entry, ok = cg.curScope.lookup(fn.Field)
		}
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)
				callee = block.NewLoad(ptrType.ElemType, entry.val)
			} else {
				callee = entry.val
			}
			// Determine the first argument: if the method expects a pointer
			// receiver (*Struct), pass the address of the object rather than
			// its value so that mutations through `this` are visible to the caller.
			var thisArg value.Value = objVal
			if f, ok2 := callee.(*ir.Func); ok2 && len(f.Sig.Params) > 0 {
				firstParam := f.Sig.Params[0]
				if pt, isPtr := firstParam.(*irtypes.PointerType); isPtr {
					if pt.ElemType.Equal(objVal.Type()) {
						// Try to get the lvalue (alloca) for the receiver expression.
						if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
							thisArg = lv
						} else {
							// Fallback: store to a temp alloca (mutations are lost,
							// but this keeps the call type-correct).
							tmp := block.NewAlloca(objVal.Type())
							block.NewStore(objVal, tmp)
							thisArg = tmp
						}
					}
				}
			}
			// Build args with obj first.
			llArgs := make([]value.Value, 0, len(e.Args)+1)
			llArgs = append(llArgs, thisArg)
			for _, arg := range e.Args {
				av, err := cg.genExpr(block, arg)
				if err != nil {
					return nil, err
				}
				llArgs = append(llArgs, av)
			}
			// Adapt arg types to function signature.
			if f, ok2 := callee.(*ir.Func); ok2 {
				calleeType = f.Sig
				llArgs = cg.adaptArgs(block, llArgs, calleeType)
			}
			return block.NewCall(callee, llArgs...), nil
		}
		_ = objVal
		return nil, fmt.Errorf("undefined method: %s.%s", structName, fn.Field)

	case *ast.ScopeAccess:
		// e.g. weather.sunny used as function - probably an error, but handle gracefully.
		v, err := cg.genScopeAccess(block, fn)
		if err != nil {
			return nil, err
		}
		callee = v

	default:
		var err error
		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}
		// If the expression evaluated to a fat fn pointer, call through it.
		if callee != nil && isFatFnPtr(callee.Type()) {
			return cg.callFatFn(block, callee, e.Args)
		}
	}

	if callee == nil {
		return nil, fmt.Errorf("nil callee")
	}

	// Build arguments. Keep pre-coercion values for ARC temporary release.
	llArgs := make([]value.Value, 0, len(e.Args))
	llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
	for _, arg := range e.Args {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		if av != nil {
			llArgs = append(llArgs, av)
			llArgsPreCoerce = append(llArgsPreCoerce, av)
		}
	}

	// Adapt argument types.
	if f, ok := callee.(*ir.Func); ok {
		calleeType = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			calleeType = ft
		}
	}
	if calleeType != nil {
		llArgs = cg.adaptArgs(block, llArgs, calleeType)
	}

	result := block.NewCall(callee, llArgs...)

	// ARC: release temporary RC-tracked arguments.  Fresh allocations (array
	// literals, concat results, function-call return values, etc.) that are
	// passed directly without being stored in a named variable have nobody to
	// release them after the callee finishes.  The callee retains on entry and
	// releases on exit, so the net rc after the call is still 1.  We drop our
	// owning reference here to reach rc=0 and free the block.
	argIdx := 0
	for _, astArg := range e.Args {
		if argIdx >= len(llArgsPreCoerce) {
			break
		}
		av := llArgsPreCoerce[argIdx]
		argIdx++
		if !isRCTrackedType(av.Type()) {
			continue
		}
		if isCopyExpr(astArg) {
			// Named variable: its scope entry will release it at scope exit.
			continue
		}
		// Temporary fresh allocation: release our reference.
		cg.emitRelease(block, av)
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) adaptArgs(block *ir.Block, args []value.Value, sig *irtypes.FuncType) []value.Value {
	if sig == nil {
		return args
	}
	result := make([]value.Value, len(args))
	for i, arg := range args {
		if i < len(sig.Params) {
			result[i] = cg.coerce(block, arg, sig.Params[i])
		} else if sig.Variadic && arg != nil && isFatPtrType(arg.Type()) {
			// Variadic position: fat-ptrs are not valid C varargs — unwrap to
			// the underlying raw pointer so printf-style calls work correctly.
			result[i] = cg.extractFatPtrData(block, arg, arg.Type().(*irtypes.StructType))
		} else {
			result[i] = arg
		}
	}
	return result
}

func (cg *CodeGen) genFieldAccess(block *ir.Block, e *ast.FieldAccess) (value.Value, error) {
	// Check if this is an enum member access: EnumName.Member
	if id, ok := e.Expr.(*ast.Identifier); ok {
		key := id.Name + "." + e.Field
		if val, ok2 := cg.enumValues[key]; ok2 {
			baseType := cg.enumTypes[id.Name]
			if it, ok3 := baseType.(*irtypes.IntType); ok3 {
				return constant.NewInt(it, val), nil
			}
			return constant.NewInt(irtypes.I32, val), nil
		}
	}

	obj, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}

	// If pointer, dereference first.
	objType := obj.Type()
	if e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			obj = block.NewLoad(pt.ElemType, obj)
			objType = pt.ElemType
		}
	}
	// Auto-deref: when obj is a pointer-to-named-struct, dereference it even
	// without the -> operator.  This handles pointer receiver methods where
	// `this *Foo` fields are accessed with `this.field` rather than `this->field`.
	if !e.IsPtr {
		if pt, ok := objType.(*irtypes.PointerType); ok {
			if cg.typeNameOf(pt.ElemType) != "" {
				obj = block.NewLoad(pt.ElemType, obj)
				objType = pt.ElemType
			}
		}
	}

	// Handle .len on dynamic arrays {T*, i64} and strings {i8*, i64}.
	if e.Field == "len" && (isFatArrayPtr(objType) || isStringType(objType)) {
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)
		gep := block.NewGetElementPtr(objType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		return block.NewLoad(irtypes.I64, gep), nil
	}

	structName := cg.typeNameOf(objType)
	fieldIdx := cg.fieldIndex(structName, e.Field)
	if fieldIdx < 0 {
		return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
	}

	// We need a pointer to the struct to do GEP.
	alloca := block.NewAlloca(objType)
	block.NewStore(obj, alloca)
	gep := block.NewGetElementPtr(objType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	// Load the field.
	if st, ok := objType.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
		return block.NewLoad(st.Fields[fieldIdx], gep), nil
	}
	return block.NewLoad(irtypes.I64, gep), nil
}

func (cg *CodeGen) genIndexExpr(block *ir.Block, e *ast.IndexExpr) (value.Value, error) {
	arr, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	idx, err := cg.genExpr(block, e.Index)
	if err != nil {
		return nil, err
	}
	if arr == nil || idx == nil {
		return nil, nil
	}

	idx = cg.coerce(block, idx, irtypes.I64)

	// Check if it's a fat-ptr (dynamic array) or regular array.
	arrType := arr.Type()
	switch at := arrType.(type) {
	case *irtypes.StructType:
		if len(at.Fields) == 2 {
			// Fat pointer: {T*, i64}
			elemPtrType := at.Fields[0]
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)
			ptrGep := block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			dataPtr := block.NewLoad(elemPtrType, ptrGep)
			if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
				elemGep := block.NewGetElementPtr(pt.ElemType, dataPtr, idx)
				return block.NewLoad(pt.ElemType, elemGep), nil
			}
		}
	case *irtypes.ArrayType:
		alloca := block.NewAlloca(arrType)
		block.NewStore(arr, alloca)
		gep := block.NewGetElementPtr(arrType, alloca,
			constant.NewInt(irtypes.I32, 0), idx)
		return block.NewLoad(at.ElemType, gep), nil
	case *irtypes.PointerType:
		gep := block.NewGetElementPtr(at.ElemType, arr, idx)
		return block.NewLoad(at.ElemType, gep), nil
	}
	return nil, nil
}

func (cg *CodeGen) genScopeAccess(block *ir.Block, e *ast.ScopeAccess) (value.Value, error) {
	// e.g. weather.sunny → look up "weather.sunny" in enum registry.
	if len(e.Path) == 2 {
		key := e.Path[0] + "." + e.Path[1]
		if val, ok := cg.enumValues[key]; ok {
			baseType := cg.enumTypes[e.Path[0]]
			if it, ok2 := baseType.(*irtypes.IntType); ok2 {
				return constant.NewInt(it, val), nil
			}
			return constant.NewInt(irtypes.I32, val), nil
		}
	}
	// Try identifier lookup.
	joined := strings.Join(e.Path, ".")
	entry, ok := cg.curScope.lookup(joined)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil
	}
	// Try last element.
	last := e.Path[len(e.Path)-1]
	entry, ok = cg.curScope.lookup(last)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}
		return entry.val, nil
	}
	return nil, fmt.Errorf("undefined: %s", strings.Join(e.Path, "::"))
}

func (cg *CodeGen) genArrayLit(block *ir.Block, e *ast.ArrayLit) (value.Value, error) {
	if len(e.Elems) == 0 {
		// Empty dynamic array: {null, 0}
		fat := stringFatPtrType() // {i8*, i64} - reuse structure
		alloca := block.NewAlloca(fat)
		ptrGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewNull(irtypes.I8Ptr), ptrGep)
		lenGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenGep)
		return block.NewLoad(fat, alloca), nil
	}

	vals := make([]value.Value, len(e.Elems))
	for i, elem := range e.Elems {
		v, err := cg.genExpr(block, elem)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}

	elemType := vals[0].Type()
	n := int64(len(vals))

	// Compute element size via GEP trick: sizeof(elemType) = gep(null, 1) as i64.
	nullPtr := constant.NewNull(irtypes.NewPointer(elemType))
	sizeGep := block.NewGetElementPtr(elemType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSize := block.NewMul(elemSize, constant.NewInt(irtypes.I64, n))

	// Heap-allocate array data (ARC-managed so rc=1 initially).
	mallocI8 := block.NewCall(cg.ensureRCAlloc(), totalSize)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(elemType))

	// Store elements into heap memory.
	for i, v := range vals {
		gep := block.NewGetElementPtr(elemType, dataPtr, constant.NewInt(irtypes.I64, int64(i)))
		block.NewStore(v, gep)
	}

	// Return as fat pointer {T*, i64}.
	fatType := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)
	fatAlloca := block.NewAlloca(fatType)
	ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, ptrGep)
	lenGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, n), lenGep)
	return block.NewLoad(fatType, fatAlloca), nil
}

func (cg *CodeGen) genStructLit(block *ir.Block, e *ast.StructLit) (value.Value, error) {
	st, ok := cg.structTypes[e.TypeName]
	if !ok {
		return nil, fmt.Errorf("unknown struct type: %s", e.TypeName)
	}

	alloca := block.NewAlloca(st)
	fieldNames := cg.structFields[e.TypeName]
	vtableOff := cg.vtableOffset(e.TypeName)
	// userOff = 1 (type_id) + vtable fields
	userOff := 1 + vtableOff

	// Initialise the leading i32 type_id field (index 0).
	if typeID, ok := cg.structTypeIDs[e.TypeName]; ok {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	// Initialise embedded vtable pointer fields (indices 1 … vtableOff).
	for i, instKey := range cg.structVtableOrder[e.TypeName] {
		vtableKey := e.TypeName + "__" + instKey
		if vg, ok := cg.traitVtableGlobals[vtableKey]; ok {
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(1+i)))
			block.NewStore(vg, gep)
		}
	}

	if len(e.Positional) > 0 {
		for i, v := range e.Positional {
			idx := userOff + i
			if idx >= len(st.Fields) {
				break
			}
			val, err := cg.genExpr(block, v)
			if err != nil {
				return nil, err
			}
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
			val = cg.coerce(block, val, st.Fields[idx])
			block.NewStore(val, gep)
		}
	} else {
		for _, f := range e.Fields {
			rawIdx := -1
			for i, fn := range fieldNames {
				if fn == f.Name {
					rawIdx = i
					break
				}
			}
			if rawIdx < 0 {
				continue
			}
			idx := userOff + rawIdx
			val, err := cg.genExpr(block, f.Value)
			if err != nil {
				return nil, err
			}
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
			val = cg.coerce(block, val, st.Fields[idx])
			block.NewStore(val, gep)
		}
	}
	result := block.NewLoad(st, alloca)

	// Call the struct's init method (if defined) for side-effects.
	// Per spec: "fn init(this S) = ..." is called on every struct literal
	// except those created via malloc.
	initName := e.TypeName + "_init"
	if initFn, ok := cg.curScope.lookup(initName); ok {
		if fn, ok2 := initFn.val.(*ir.Func); ok2 {
			args := cg.adaptArgs(block, []value.Value{result}, fn.Sig)
			block.NewCall(fn, args...)
		}
	}

	return result, nil
}

func (cg *CodeGen) genAsExpr(block *ir.Block, e *ast.AsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	targetType, err := cg.tinTypeToLLVM(e.Type)
	if err != nil {
		return nil, err
	}
	return cg.coerce(block, val, targetType), nil
}

func (cg *CodeGen) genAddrExpr(block *ir.Block, e *ast.AddrExpr) (value.Value, error) {
	// addr(N) where N is an integer literal: treat as inttoptr cast (raw address).
	if il, ok := e.Val.(*ast.IntLit); ok {
		v := constant.NewInt(irtypes.I64, il.Value)
		return block.NewIntToPtr(v, irtypes.I8Ptr), nil
	}
	return cg.genLValue(block, e.Val)
}

func (cg *CodeGen) genAddrOfExpr(block *ir.Block, e *ast.AddressOfExpr) (value.Value, error) {
	return cg.genLValue(block, e.Expr)
}

func (cg *CodeGen) genDerefExpr(block *ir.Block, e *ast.DerefExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	if pt, ok := val.Type().(*irtypes.PointerType); ok {
		return block.NewLoad(pt.ElemType, val), nil
	}
	return val, nil
}

func (cg *CodeGen) genPipeExpr(block *ir.Block, e *ast.PipeExpr) (value.Value, error) {
	// a |> f(args) = f(args)(a)  — curried style: call f(args) first, then call
	// the returned function with a.
	// a |> f         = f(a)      — plain function value on the right.
	leftVal, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	// Evaluate the right-hand side completely (including any call arguments),
	// yielding the function to apply to leftVal.
	rightFn, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}
	if rightFn == nil {
		return leftVal, nil
	}
	// If rightFn is a closure fat pointer {fn*, i8*}, call through it.
	if isFatFnPtr(rightFn.Type()) {
		fnPtr := block.NewExtractValue(rightFn, 0)
		envPtr := block.NewExtractValue(rightFn, 1)
		fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		llArgs := cg.adaptArgs(block, []value.Value{envPtr, leftVal}, fnType)
		result := block.NewCall(fnPtr, llArgs...)
		if irtypes.IsVoid(result.Type()) {
			return nil, nil
		}
		return result, nil
	}
	// Plain function pointer.
	result := block.NewCall(rightFn, leftVal)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) genTernaryExpr(block *ir.Block, e *ast.TernaryExpr) (value.Value, error) {
	cond, err := cg.genExpr(block, e.Cond)
	if err != nil {
		return nil, err
	}
	cond = cg.toBool(block, cond)

	thenVal, err := cg.genExpr(block, e.Then)
	if err != nil {
		return nil, err
	}
	elseVal, err := cg.genExpr(block, e.Else)
	if err != nil {
		return nil, err
	}

	if thenVal == nil {
		thenVal = constant.NewInt(irtypes.I64, 0)
	}
	if elseVal == nil {
		elseVal = constant.NewInt(irtypes.I64, 0)
	}

	// Unify types.
	elseVal = cg.coerce(block, elseVal, thenVal.Type())
	return block.NewSelect(cond, thenVal, elseVal), nil
}

func (cg *CodeGen) genIsExpr(block *ir.Block, e *ast.IsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	if e.IsNone {
		// For a data-type tagged union: check variant_tag (field 1) == 0.
		if st, ok := val.Type().(*irtypes.StructType); ok {
			if dataName := cg.typeNameOf(val.Type()); dataName != "" {
				if _, isData := cg.dataDecls[dataName]; isData {
					alloca := block.NewAlloca(st)
					block.NewStore(val, alloca)
					// Field 0 = i32 type_id, field 1 = i8 variant_tag.
					tagGEP := block.NewGetElementPtr(st, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
					tag := block.NewLoad(irtypes.I8, tagGEP)
					return block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I8, 0)), nil
				}
			}
		}
		// Fallback: check for zero / null.
		zero := cg.zeroValue(val.Type())
		if irtypes.IsPointer(val.Type()) {
			null := constant.NewNull(val.Type().(*irtypes.PointerType))
			return block.NewICmp(enum.IPredEQ, val, null), nil
		}
		return block.NewICmp(enum.IPredEQ, val, zero), nil
	}

	// Typed is-check: "x is v T" — check the tag and optionally bind the payload.
	if st, ok := val.Type().(*irtypes.StructType); ok {
		dataName := cg.typeNameOf(val.Type())
		if dd, isData := cg.dataDecls[dataName]; isData {
			// Find the variant matching e.Type.
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}
			variantTag := int8(-1)
			for i, v := range dd.Variants {
				if v.Type == nil {
					continue
				}
				lt, err3 := cg.tinTypeToLLVM(v.Type)
				if err3 != nil {
					continue
				}
				if lt.Equal(targetLLVM) || llvmTypeSize(lt) == llvmTypeSize(targetLLVM) {
					key := fmt.Sprintf("%s.%d", dataName, i)
					variantTag = cg.dataVariantTags[key]
					break
				}
			}
			if variantTag < 0 {
				variantTag = 1 // default first typed variant
			}
			alloca := block.NewAlloca(st)
			block.NewStore(val, alloca)
			// Field 0 = i32 type_id, field 1 = i8 variant_tag, field 2 = payload.
			tagGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			tag := block.NewLoad(irtypes.I8, tagGEP)
			cmp := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I8, int64(variantTag)))
			// Bind variable if requested.
			if e.VarName != "" {
				payloadGEP := block.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := block.NewAlloca(targetLLVM)
				payloadVal := block.NewLoad(targetLLVM, payloadPtr)
				block.NewStore(payloadVal, payloadAlloca)
				cg.curScope.set(e.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true})
			}
			return cmp, nil
		}
	}
	// any type check: "x is dog" where x is any — compare type_id (field 0).
	if isAnyType(val.Type()) && e.Type != nil {
		targetName := ""
		switch t := e.Type.(type) {
		case *ast.SimpleType:
			targetName = t.Name
		}
		if targetName != "" {
			var targetID int32
			var found bool
			if id, ok := cg.structTypeIDs[targetName]; ok {
				targetID = id
				found = true
			} else if id, ok := cg.dataTypeIDs[targetName]; ok {
				targetID = id
				found = true
			}
			if found {
				anyType := anyFatPtrType()
				anyAlloca := block.NewAlloca(anyType)
				block.NewStore(val, anyAlloca)
				tagGep := block.NewGetElementPtr(anyType, anyAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				tag := block.NewLoad(irtypes.I32, tagGep)
				cmp := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I32, int64(targetID)))
				// Bind variable: extract data pointer and cast to the target type.
				if e.VarName != "" {
					targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
					if err2 == nil {
						ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
						dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
						typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(targetLLVM))
						typedVal := block.NewLoad(targetLLVM, typedPtr)
						typedAlloca := block.NewAlloca(targetLLVM)
						block.NewStore(typedVal, typedAlloca)
						cg.curScope.set(e.VarName, &scopeEntry{val: typedAlloca, isAlloc: true})
					}
				}
				return cmp, nil
			}
		}
	}
	// Fallback: just return true.
	return constant.NewInt(irtypes.I1, 1), nil
}

/// isFatArrayPtr returns true for anonymous {T*, i64} fat array pointer structs.
// Named structs (user-defined) are excluded to avoid false matches with

// fnSigName formats an LLVM FuncType as a Tin-style signature string such as
// "fn(i64,string)bool".  When skipFirstEnv is true the first parameter (the
// implicit i8* env of a fat-function-pointer) is omitted.
func fnSigName(ft *irtypes.FuncType, skipFirstEnv bool) string {
	var sb strings.Builder
	sb.WriteString("fn(")
	start := 0
	if skipFirstEnv && len(ft.Params) > 0 {
		start = 1
	}
	for i := start; i < len(ft.Params); i++ {
		if i > start {
			sb.WriteString(",")
		}
		sb.WriteString(llvmTypeName(ft.Params[i]))
	}
	sb.WriteString(")")
	if ft.RetType != nil && !irtypes.IsVoid(ft.RetType) {
		sb.WriteString(llvmTypeName(ft.RetType))
	}
	return sb.String()
}

// ensureFnTypeID assigns a unique compile-time type ID to a function signature
// string, reusing the existing ID if the same signature was seen before.
func (cg *CodeGen) ensureFnTypeID(sig string) int32 {
	if id, ok := cg.fnTypeIDs[sig]; ok {
		return id
	}
	id := cg.nextTypeID
	cg.nextTypeID++
	cg.fnTypeIDs[sig] = id
	return id
}

// collectFreeVars walks body and returns the names of Identifier nodes that are
// not already in localNames. VarDecl nodes add their names to localNames as they
// are encountered. Nested LambdaExpr nodes are not recursed into (they have their
// own scope and will capture independently).
func collectFreeVars(body ast.Node, localNames map[string]bool) []string {
	seen := map[string]bool{}
	var result []string
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.Identifier:
			if !localNames[v.Name] && !seen[v.Name] {
				seen[v.Name] = true
				result = append(result, v.Name)
			}
		case *ast.VarDecl:
			walk(v.Value)
			localNames[v.Name] = true
		case *ast.LambdaExpr:
			// Don't descend into nested lambdas; they capture independently.
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.ReturnStmt:
			walk(v.Value)
		case *ast.EchoStmt:
			walk(v.Value)
		case *ast.AssignStmt:
			walk(v.Target)
			walk(v.Value)
		case *ast.AugAssignStmt:
			walk(v.Target)
			walk(v.Value)
		case *ast.ExprStmt:
			walk(v.Expr)
		case *ast.BinExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			walk(v.Expr)
		case *ast.CallExpr:
			walk(v.Func)
			for _, a := range v.Args {
				walk(a)
			}
		case *ast.FieldAccess:
			walk(v.Expr)
		case *ast.IndexExpr:
			walk(v.Expr)
			walk(v.Index)
		case *ast.IfStmt:
			walk(v.Cond)
			walk(v.Then)
			for _, ei := range v.ElseIfs {
				walk(ei.Cond)
				walk(ei.Body)
			}
			if v.Else != nil {
				walk(v.Else)
			}
		case *ast.TernaryExpr:
			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.StructLit:
			for _, f := range v.Fields {
				walk(f.Value)
			}
		case *ast.ArrayLit:
			for _, el := range v.Elems {
				walk(el)
			}
		case *ast.AsExpr:
			walk(v.Expr)
		case *ast.PipeExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.WhereList:
			for _, c := range v.Clauses {
				walk(c.Cond)
				walk(c.Body)
			}
		case *ast.ForStmt:
			walk(v.Init)
			walk(v.Cond)
			walk(v.Post)
			walk(v.Iter)
			walk(v.Body)
		case *ast.MatchStmt:
			walk(v.Expr)
			for _, c := range v.Cases {
				walk(c.Body)
			}
			if v.Default != nil {
				walk(v.Default)
			}
		case *ast.InterpolatedString:
			for _, p := range v.Parts {
				if p.IsExpr {
					walk(p.Expr)
				}
			}
		case *ast.IsExpr:
			walk(v.Expr)
		case *ast.TypeAssertExpr:
			walk(v.Expr)
		}
	}
	walk(body)
	return result
}

// callTraitMethod dispatches x.method(args) where x is a trait fat pointer
// {i8* data, vtable*}.  It looks up the method slot index in the vtable,
// loads the function pointer, and calls it with (data, args...).
// instKey may be "named" or "iter_i64" etc.
func (cg *CodeGen) callTraitMethod(block *ir.Block, ifaceVal value.Value, instKey, methodName string, argNodes []ast.Node) (value.Value, error) {
	// Method order is stored by base trait name.
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}
	methodOrder := cg.traitMethodOrder[baseTrait]
	slotIdx := -1
	for i, n := range methodOrder {
		if n == methodName {
			slotIdx = i
			break
		}
	}
	if slotIdx < 0 {
		return nil, fmt.Errorf("trait %s has no method %s", instKey, methodName)
	}

	// Extract data pointer and vtable pointer from iface fat ptr.
	dataPtr := block.NewExtractValue(ifaceVal, 0)
	vtablePtr := block.NewExtractValue(ifaceVal, 1)

	// Load function pointer from vtable[slotIdx].
	vtableSt := cg.traitVtableStructTypes[instKey]
	fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slotIdx)))
	fnSlotType := vtableSt.Fields[slotIdx].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	fnPtr := block.NewLoad(irtypes.NewPointer(fnSlotType), fnPtrGep)

	// Build call args: (data_ptr, extra_args...).
	llArgs := []value.Value{dataPtr}
	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		llArgs = append(llArgs, av)
	}
	llArgs = cg.adaptArgs(block, llArgs, fnSlotType)
	result := block.NewCall(fnPtr, llArgs...)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

// wrapFnAsFatPtr wraps a named or extern function pointer into a fat-fn-ptr
// { fn(i8* env, params...)*, i8* } with a null environment.
// The shim ignores its env parameter and simply forwards to the wrapped function.
// Shims are cached per function name to avoid duplicate definitions.
func (cg *CodeGen) wrapFnAsFatPtr(block *ir.Block, fnVal value.Value, targetFatType irtypes.Type) value.Value {
	fatSt := targetFatType.(*irtypes.StructType)
	// The fat-fn-ptr stores fn(i8*, params...)* in field 0.
	wrapperFnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Get the original function's type (without the env param).
	srcFnType, ok := fnVal.Type().(*irtypes.PointerType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}
	origFnType, ok := srcFnType.ElemType.(*irtypes.FuncType)
	if !ok {
		return cg.zeroValue(targetFatType)
	}

	// Build a cache key from the function's name.
	shimName := ""
	if named, ok := fnVal.(interface{ Name() string }); ok {
		shimName = "__shim_" + named.Name()
	} else {
		shimName = fmt.Sprintf("__shim_%d", cg.strCount)
		cg.strCount++
	}

	// Reuse cached shim if already generated.
	var shim *ir.Func
	for _, fn := range cg.mod.Funcs {
		if fn.Name() == shimName {
			shim = fn
			break
		}
	}

	if shim == nil {
		// The shim's signature must match wrapperFnType (the fat-fn-ptr's expected
		// function type): (i8* env, tin_param_0, tin_param_1, ...).
		// wrapperFnType.Params[0] is i8* (env); Params[1..] are the tin-level types.
		shimParams := make([]*ir.Param, len(wrapperFnType.Params))
		for i, pt := range wrapperFnType.Params {
			name := "env"
			if i > 0 {
				name = fmt.Sprintf("p%d", i-1)
			}
			shimParams[i] = ir.NewParam(name, pt)
		}
		shim = cg.mod.NewFunc(shimName, wrapperFnType.RetType, shimParams...)
		entry := shim.NewBlock("entry")
		// Forward call: skip env (index 0), adapt remaining args to orig signature.
		callArgs := make([]value.Value, len(origFnType.Params))
		for i := range origFnType.Params {
			callArgs[i] = shim.Params[i+1]
		}
		callArgs = cg.adaptArgs(entry, callArgs, origFnType)
		result := entry.NewCall(fnVal, callArgs...)
		if irtypes.IsVoid(wrapperFnType.RetType) {
			entry.NewRet(nil)
		} else {
			// Wrap return value if needed (e.g., raw i8* → string fat-ptr).
			ret := cg.wrapFromExtern(entry, result, wrapperFnType.RetType)
			entry.NewRet(ret)
		}
	}

	// Return fat-fn-ptr { shim*, null }.
	alloca := block.NewAlloca(fatSt)
	gep0 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(shim, gep0)
	gep1 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewNull(irtypes.I8Ptr), gep1)
	return block.NewLoad(fatSt, alloca)
}

// callFatFn emits a call through a closure fat pointer { fn(i8*,params...)*, i8* }.
func (cg *CodeGen) callFatFn(block *ir.Block, fatPtr value.Value, argNodes []ast.Node) (value.Value, error) {
	fnPtr := block.NewExtractValue(fatPtr, 0)
	envPtr := block.NewExtractValue(fatPtr, 1)

	llArgs := []value.Value{envPtr}
	for _, arg := range argNodes {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}
		llArgs = append(llArgs, av)
	}

	// Adapt args to the underlying function's signature.
	fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	llArgs = cg.adaptArgs(block, llArgs, fnType)

	result := block.NewCall(fnPtr, llArgs...)
	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}
	return result, nil
}

func (cg *CodeGen) genLambdaExpr(block *ir.Block, e *ast.LambdaExpr) (value.Value, error) {
	name := fmt.Sprintf("lambda.%d", cg.strCount)
	cg.strCount++

	// ── Step 1: identify free variables ────────────────────────────────────────
	localNames := map[string]bool{}
	for _, p := range e.Params {
		localNames[p.Name] = true
	}
	freeNames := collectFreeVars(e.Body, localNames)

	// Resolve each free name in the current (outer) scope. Skip names that
	// resolve to module-level IR functions (not allocas) — those are callable
	// directly by name and don't need capturing.
	type capture struct {
		name    string
		val     value.Value // loaded value (not the alloca)
		llvmTy  irtypes.Type
	}
	var captures []capture
	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}
		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function – reachable by name, no capture needed
		}
		var val value.Value
		var ty irtypes.Type
		if entry.isAlloc {
			pt := entry.val.Type().(*irtypes.PointerType)
			ty = pt.ElemType
			val = block.NewLoad(ty, entry.val)
		} else {
			val = entry.val
			ty = val.Type()
		}
		captures = append(captures, capture{n, val, ty})
	}

	// ── Step 2: build env struct and malloc it (if there are captures) ──────────
	var envI8Ptr value.Value = constant.NewNull(irtypes.I8Ptr)
	var envStructType *irtypes.StructType

	if len(captures) > 0 {
		fields := make([]irtypes.Type, len(captures))
		for i, c := range captures {
			fields[i] = c.llvmTy
		}
		envStructType = irtypes.NewStruct(fields...)

		// sizeof(*envStructType): GEP trick — null + 1 element then ptrtoint.
		nullEnvPtr := constant.NewNull(irtypes.NewPointer(envStructType))
		oneGEP := block.NewGetElementPtr(envStructType, nullEnvPtr, constant.NewInt(irtypes.I32, 1))
		envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
		envI8Ptr = block.NewCall(cg.ensureMalloc(), envSize)

		// Store each captured value into the env struct.
		envTypedPtr := block.NewBitCast(envI8Ptr, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := block.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			block.NewStore(c.val, gep)
		}
	}

	// ── Step 3: create the lambda IR function with (i8* env, params...) sig ────
	llParams := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range e.Params {
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}
		llParams = append(llParams, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void
	if e.RetType != nil {
		var err error
		retType, err = cg.tinTypeToLLVM(e.RetType)
		if err != nil {
			return nil, err
		}
	}

	f := cg.mod.NewFunc(name, retType, llParams...)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	// Start a fresh scope (not inheriting outer scope — captured values are
	// explicitly loaded from the env struct below).
	cg.curScope = newScope(nil)

	// Register global scope so functions/enums remain accessible.
	// Walk up to the top-level scope and set it as the parent.
	global := prevScope
	for global.parent != nil {
		global = global.parent
	}
	cg.curScope = newScope(global)

	// ── Step 4: unpack captures from env inside the lambda body ─────────────────
	if len(captures) > 0 {
		envRaw := f.Params[0]
		envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
		for i, c := range captures {
			gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(i)))
			alloca := entry.NewAlloca(c.llvmTy)
			loaded := entry.NewLoad(c.llvmTy, gep)
			entry.NewStore(loaded, alloca)
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true})
		}
	}

	// Register lambda params (skip index 0 = env).
	for i, p := range e.Params {
		param := f.Params[i+1]
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}
		alloca := entry.NewAlloca(pt)
		entry.NewStore(param, alloca)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true})
	}

	// For where-list bodies, the match subject is the first parameter so that
	// atom and comparison conditions compare against it (mirroring genFuncDeclAs).
	prevMatchSubject := cg.matchSubject
	if _, isWhere := e.Body.(*ast.WhereList); isWhere && len(e.Params) > 0 {
		firstParamName := e.Params[0].Name
		if se, ok := cg.curScope.lookup(firstParamName); ok && se.isAlloc {
			pt := se.val.Type().(*irtypes.PointerType)
			cg.matchSubject = entry.NewLoad(pt.ElemType, se.val)
		}
	}

	term, err := cg.genBody(entry, e.Body, retType)
	cg.matchSubject = prevMatchSubject
	if err != nil {
		return nil, err
	}
	if !term {
		lastBlock := f.Blocks[len(f.Blocks)-1]
		if lastBlock.Term == nil {
			if irtypes.IsVoid(retType) {
				lastBlock.NewRet(nil)
			} else {
				lastBlock.NewRet(cg.zeroValue(retType))
			}
		}
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	// ── Step 5: build and return fat pointer { fn_ptr, env_i8_ptr } ─────────────
	fatStructType := irtypes.NewStruct(irtypes.NewPointer(f.Sig), irtypes.I8Ptr)
	alloca := block.NewAlloca(fatStructType)
	gep0 := block.NewGetElementPtr(fatStructType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(f, gep0)
	gep1 := block.NewGetElementPtr(fatStructType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(envI8Ptr, gep1)
	return block.NewLoad(fatStructType, alloca), nil
}


// ── Interpolated string ────────────────────────────────────────────────────────

func (cg *CodeGen) genInterpolatedString(block *ir.Block, e *ast.InterpolatedString) (value.Value, error) {
	// Build a format string and argument list for printf/sprintf.
	var fmtParts []string
	var args []value.Value

	for _, part := range e.Parts {
		if !part.IsExpr {
			// Escape % in literal parts.
			escaped := strings.ReplaceAll(part.Str, "%", "%%")
			fmtParts = append(fmtParts, escaped)
		} else {
			val, err := cg.genExpr(block, part.Expr)
			if err != nil {
				return nil, err
			}
			if val == nil {
				fmtParts = append(fmtParts, "(nil)")
				continue
			}
			t := val.Type()
			switch {
			case isStringType(t):
				fmtParts = append(fmtParts, "%s")
				ptr := cg.extractStringPtr(block, val)
				args = append(args, ptr)
			case irtypes.IsInt(t):
				it := t.(*irtypes.IntType)
				if it.BitSize == 1 {
					fmtParts = append(fmtParts, "%d")
					val = block.NewZExt(val, irtypes.I32)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
				}
				args = append(args, val)
			case irtypes.IsFloat(t):
				fmtParts = append(fmtParts, "%g")
				if t != irtypes.Double {
					val = block.NewFPExt(val, irtypes.Double)
				}
				args = append(args, val)
			default:
				// print trait: struct or fat-pointer with a print() method.
				if strVal, ok := cg.callPrintTrait(block, val); ok {
					fmtParts = append(fmtParts, "%s")
					ptr := cg.extractStringPtr(block, strVal)
					args = append(args, ptr)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
					args = append(args, val)
				}
			}
		}
	}

	// Build result string using snprintf.
	fmtStr := strings.Join(fmtParts, "")
	fmtPtr := cg.newGlobalString(fmtStr)

	// Allocate a buffer (256 bytes for simplicity).
	bufSize := constant.NewInt(irtypes.I64, 256)
	malloc := cg.ensureMalloc()
	buf := block.NewCall(malloc, bufSize)

	// Declare snprintf.
	snprintfFn := cg.ensureSnprintf()
	snprintfArgs := []value.Value{buf, bufSize, fmtPtr}
	snprintfArgs = append(snprintfArgs, args...)
	block.NewCall(snprintfFn, snprintfArgs...)

	// Return as fat pointer.
	// We need the actual length.
	// Simplification: use 256 as max length, then compute strlen.
	// Actually, snprintf returns the number of chars written.
	// Let's use that as our length.
	written := block.NewCall(snprintfFn, snprintfArgs...)
	writtenI64 := block.NewSExt(written, irtypes.I64)

	fatPtrType := stringFatPtrType()
	fatAlloca := block.NewAlloca(fatPtrType)
	ptrGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(buf, ptrGep)
	lenGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(writtenI64, lenGep)
	return block.NewLoad(fatPtrType, fatAlloca), nil
}

// ── LValue generation ──────────────────────────────────────────────────────────

// genLValue returns a pointer to the storage location of an lvalue.
func (cg *CodeGen) genLValue(block *ir.Block, node ast.Node) (value.Value, error) {
	switch e := node.(type) {
	case *ast.Identifier:
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, fmt.Errorf("undefined identifier: %s", e.Name)
		}
		if entry.isAlloc {
			return entry.val, nil
		}
		// Not an alloca - wrap in alloca.
		alloca := block.NewAlloca(entry.val.Type())
		block.NewStore(entry.val, alloca)
		return alloca, nil

	case *ast.IndexExpr:
		arr, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}
		idx, err := cg.genExpr(block, e.Index)
		if err != nil {
			return nil, err
		}
		idx = cg.coerce(block, idx, irtypes.I64)

		arrType := arr.Type()
		switch at := arrType.(type) {
		case *irtypes.StructType:
			if len(at.Fields) == 2 {
				alloca := block.NewAlloca(arrType)
				block.NewStore(arr, alloca)
				ptrGep := block.NewGetElementPtr(arrType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				elemPtrType := at.Fields[0]
				dataPtr := block.NewLoad(elemPtrType, ptrGep)
				if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
					return block.NewGetElementPtr(pt.ElemType, dataPtr, idx), nil
				}
			}
		case *irtypes.ArrayType:
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)
			return block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), idx), nil
		case *irtypes.PointerType:
			return block.NewGetElementPtr(at.ElemType, arr, idx), nil
		}
		return nil, fmt.Errorf("cannot index type %s", arrType)

	case *ast.FieldAccess:
		// Use genLValue recursively so we obtain a pointer into the *original*
		// storage (alloca, heap, etc.) rather than a copy.  Writing through the
		// returned GEP pointer then actually mutates the variable.
		objPtr, err := cg.genLValue(block, e.Expr)
		if err != nil {
			// genLValue failed for the sub-expression (e.g. a non-lvalue like a
			// function call return value).  Fall back to a temporary alloca; this
			// means field-writes on temporaries are discarded, but that is the
			// pre-existing behaviour for such expressions.
			obj, err2 := cg.genExpr(block, e.Expr)
			if err2 != nil {
				return nil, err2
			}
			objType := obj.Type()
			if e.IsPtr {
				if pt, ok := objType.(*irtypes.PointerType); ok {
					structName := cg.typeNameOf(pt.ElemType)
					fieldIdx := cg.fieldIndex(structName, e.Field)
					if fieldIdx < 0 {
						return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
					}
					return block.NewGetElementPtr(pt.ElemType, obj,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
				}
			}
			alloca := block.NewAlloca(objType)
			block.NewStore(obj, alloca)
			structName := cg.typeNameOf(objType)
			fieldIdx := cg.fieldIndex(structName, e.Field)
			if fieldIdx < 0 {
				return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
			}
			return block.NewGetElementPtr(objType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
		}
		// objPtr is a pointer to the containing struct (or pointer-to-struct for IsPtr).
		objPtrType, ok := objPtr.Type().(*irtypes.PointerType)
		if !ok {
			return nil, fmt.Errorf("genLValue: expected pointer for field access")
		}
		objType := objPtrType.ElemType
		if e.IsPtr {
			// e.Expr is a variable holding a *struct — dereference once.
			structPtrVal := block.NewLoad(objType, objPtr)
			if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
				structName := cg.typeNameOf(pt.ElemType)
				fieldIdx := cg.fieldIndex(structName, e.Field)
				if fieldIdx < 0 {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}
				return block.NewGetElementPtr(pt.ElemType, structPtrVal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
			}
		}
		// Auto-deref: when the alloca holds a *struct (pointer receiver pattern),
		// dereference once so that `this.field` works the same as `this->field`.
		if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
			if cg.typeNameOf(pt.ElemType) != "" {
				structPtrVal := block.NewLoad(objType, objPtr)
				structName := cg.typeNameOf(pt.ElemType)
				fieldIdx := cg.fieldIndex(structName, e.Field)
				if fieldIdx < 0 {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}
				return block.NewGetElementPtr(pt.ElemType, structPtrVal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil
			}
		}
		structName := cg.typeNameOf(objType)
		fieldIdx := cg.fieldIndex(structName, e.Field)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
		}
		return block.NewGetElementPtr(objType, objPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx))), nil

	case *ast.DerefExpr:
		val, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}
		if irtypes.IsPointer(val.Type()) {
			return val, nil
		}
		return nil, fmt.Errorf("cannot deref non-pointer")
	}
	return nil, fmt.Errorf("not an lvalue: %T", node)
}

// ── Helper utilities ───────────────────────────────────────────────────────────

// callPrintTrait tries to call the print trait method on val, returning the
// resulting string value and true, or (nil, false) if not applicable.
// Handles both concrete-struct dispatch and print fat-pointer dispatch.
func (cg *CodeGen) callPrintTrait(block *ir.Block, val value.Value) (value.Value, bool) {
	t := val.Type()
	// Case 1: concrete struct — look up structName_print directly.
	if structName := cg.typeNameOf(t); structName != "" {
		if e, ok := cg.curScope.lookup(structName + "_print"); ok {
			if fn, ok2 := e.val.(*ir.Func); ok2 {
				args := cg.adaptArgs(block, []value.Value{val}, fn.Sig)
				return block.NewCall(fn, args...), true
			}
		}
	}
	// Case 2: print trait fat pointer — dispatch through vtable.
	if instKey, ok := cg.isTraitFatPtr(t); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}
		if baseTrait == "print" {
			strVal, err := cg.callTraitMethod(block, val, instKey, instKey, nil)
			if err == nil && strVal != nil {
				return strVal, true
			}
		}
	}
	return nil, false
}

// vtableOffset returns the number of vtable pointer fields prepended to the

// fieldIndex returns the LLVM field index for a named user field, accounting
// for the leading i32 type_id and vtable pointer fields at the front.
// Layout: [i32 type_id, vtable_0*, …, user_field_0, …]

// isStringType returns true if t is the tin string fat-pointer type {i8*, i64}.

// isAnyType returns true if t is the tin `any` fat-pointer type {i32, i8*}.

// boxToAny boxes val into an `any` fat-pointer {i32 type_id, i8* data}.
func (cg *CodeGen) boxToAny(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		val = constant.NewInt(irtypes.I64, 0)
	}
	// If already any, pass through.
	if isAnyType(val.Type()) {
		return val
	}

	anyType := anyFatPtrType()
	alloca := block.NewAlloca(anyType)

	t := val.Type()
	var tag int32
	var dataPtr value.Value

	// Use ARC-managed allocations for boxed data so typeof/release work.
	rcAlloc := cg.ensureRCAlloc()
	switch {
	case isStringType(t):
		tag = anyTagString
		sz := constant.NewInt(irtypes.I64, 16) // {i8*, i64} = 16 bytes
		rawPtr := block.NewCall(rcAlloc, sz)
		strPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, strPtr)
		dataPtr = rawPtr
	case t.Equal(irtypes.I1):
		tag = anyTagBool
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 1))
		boolPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.I1))
		block.NewStore(val, boolPtr)
		dataPtr = rawPtr
	case irtypes.IsFloat(t):
		tag = anyTagFloat
		var f64Val value.Value
		if t == irtypes.Double {
			f64Val = val
		} else {
			f64Val = block.NewFPExt(val, irtypes.Double)
		}
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 8))
		fPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.Double))
		block.NewStore(f64Val, fPtr)
		dataPtr = rawPtr
	case irtypes.IsInt(t):
		tag = anyTagInt
		i64Val := cg.coerce(block, val, irtypes.I64)
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, 8))
		iPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(irtypes.I64))
		block.NewStore(i64Val, iPtr)
		dataPtr = rawPtr
	case isFatFnPtr(t):
		// Fat function pointer { fn(i8*,...)*, i8* }: heap-copy the struct so
		// the any can outlive its stack alloca.
		{
			st2 := t.(*irtypes.StructType)
			innerFnType := st2.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			tag = cg.ensureFnTypeID(fnSigName(innerFnType, true))
		}
		sz := llvmTypeSize(t)
		if sz == 0 {
			sz = 16 // two pointers
		}
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		fnPtrStore := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, fnPtrStore)
		dataPtr = rawPtr
	case irtypes.IsPointer(t):
		// A pointer to a FuncType is a named/extern function reference; give
		// it the fn tag so typeof() returns 'fn(...) instead of 'ptr.
		if pt, ok2 := t.(*irtypes.PointerType); ok2 {
			if fnType, isFnType := pt.ElemType.(*irtypes.FuncType); isFnType {
				tag = cg.ensureFnTypeID(fnSigName(fnType, false))
			} else {
				tag = anyTagPtr
			}
		} else {
			tag = anyTagPtr
		}
		dataPtr = block.NewBitCast(val, irtypes.I8Ptr)
	default:
		// Named struct or data type: heap-allocate so the any can escape.
		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			if id, ok2 := cg.structTypeIDs[st.Name()]; ok2 {
				tag = id
			} else if id, ok2 := cg.dataTypeIDs[st.Name()]; ok2 {
				tag = id
			} else {
				tag = anyTagPtr // unknown named type – treat as opaque pointer
			}
		} else {
			tag = anyTagInt
		}
		sz := llvmTypeSize(t)
		if sz == 0 {
			sz = 8
		}
		rawPtr := block.NewCall(rcAlloc, constant.NewInt(irtypes.I64, int64(sz)))
		vPtr := block.NewBitCast(rawPtr, irtypes.NewPointer(t))
		block.NewStore(val, vPtr)
		dataPtr = rawPtr
	}

	// Layout: {i32 type_id, i8* data}
	tagGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(tag)), tagGep)

	ptrGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(dataPtr, ptrGep)

	return block.NewLoad(anyType, alloca)
}

// genEchoAny emits a runtime type-dispatch printf for an `any` value.
func (cg *CodeGen) genEchoAny(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	anyType := anyFatPtrType()

	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(val, anyAlloca)
	// Layout: {i32 type_id, i8* data}
	tagGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tag := block.NewLoad(irtypes.I32, tagGep)
	dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)

	f := cg.curFn
	id := cg.labelCount
	cg.labelCount++
	intBlock := f.NewBlock(fmt.Sprintf("any.int.%d", id))
	floatBlock := f.NewBlock(fmt.Sprintf("any.float.%d", id))
	strBlock := f.NewBlock(fmt.Sprintf("any.str.%d", id))
	boolBlock := f.NewBlock(fmt.Sprintf("any.bool.%d", id))
	ptrBlock := f.NewBlock(fmt.Sprintf("any.ptr.%d", id))
	doneBlock := f.NewBlock(fmt.Sprintf("any.done.%d", id))

	block.NewSwitch(tag, ptrBlock,
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagInt)), intBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagFloat)), floatBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagString)), strBlock),
		ir.NewCase(constant.NewInt(irtypes.I32, int64(anyTagBool)), boolBlock),
	)

	// int branch
	i64Ptr := intBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.I64))
	ival := intBlock.NewLoad(irtypes.I64, i64Ptr)
	intBlock.NewCall(printf, cg.newGlobalString("%lld\n"), ival)
	intBlock.NewBr(doneBlock)

	// float branch
	f64Ptr := floatBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.Double))
	fval := floatBlock.NewLoad(irtypes.Double, f64Ptr)
	floatBlock.NewCall(printf, cg.newGlobalString("%g\n"), fval)
	floatBlock.NewBr(doneBlock)

	// string branch
	strFatType := stringFatPtrType()
	strFatPtrPtr := strBlock.NewBitCast(dataPtr, irtypes.NewPointer(strFatType))
	strFatVal := strBlock.NewLoad(strFatType, strFatPtrPtr)
	strDataPtr := cg.extractStringPtr(strBlock, strFatVal)
	strBlock.NewCall(printf, cg.newGlobalString("%s\n"), strDataPtr)
	strBlock.NewBr(doneBlock)

	// bool branch
	boolPtr := boolBlock.NewBitCast(dataPtr, irtypes.NewPointer(irtypes.I1))
	bval := boolBlock.NewLoad(irtypes.I1, boolPtr)
	bval32 := boolBlock.NewZExt(bval, irtypes.I32)
	boolBlock.NewCall(printf, cg.newGlobalString("%d\n"), bval32)
	boolBlock.NewBr(doneBlock)

	// ptr branch (default)
	ptrBlock.NewCall(printf, cg.newGlobalString("%p\n"), dataPtr)
	ptrBlock.NewBr(doneBlock)

	return doneBlock, nil
}

// toBool converts a value to i1.
func (cg *CodeGen) toBool(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		return constant.NewInt(irtypes.I1, 0)
	}
	t := val.Type()
	if t.Equal(irtypes.I1) {
		return val
	}
	if irtypes.IsInt(t) {
		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), t)
		return block.NewICmp(enum.IPredNE, val, zero)
	}
	if irtypes.IsFloat(t) {
		zero := constant.NewFloat(t.(*irtypes.FloatType), 0)
		return block.NewFCmp(enum.FPredONE, val, zero)
	}
	if irtypes.IsPointer(t) {
		null := constant.NewNull(t.(*irtypes.PointerType))
		return block.NewICmp(enum.IPredNE, val, null)
	}
	return constant.NewInt(irtypes.I1, 1)
}

// coerce converts a value to the target type, inserting casts as needed.
func (cg *CodeGen) coerce(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	if val == nil || target == nil {
		return val
	}
	src := val.Type()
	if src.Equal(target) {
		return val
	}

	// Data type: wrap a value into a tagged union.
	if targetSt, ok := target.(*irtypes.StructType); ok {
		if targetName := cg.typeNameOf(target); targetName != "" {
			if _, isData := cg.dataDecls[targetName]; isData {
				if !src.Equal(target) {
					// If the source is i64 0 (the None sentinel), return a zero-tagged struct.
					if c, ok := val.(*constant.Int); ok && c.X != nil && c.X.Sign() == 0 && irtypes.IsInt(src) {
						if noneVal := cg.makeNoneValue(block, target); noneVal != nil {
							return noneVal
						}
					}
					if wrapped := cg.wrapDataVariant(block, val, targetSt, targetName); wrapped != nil {
						return wrapped
					}
				}
			}
		}
	}
	// Trait fat-pointer: coerce a concrete struct into the trait iface.
	if traitName, ok := cg.isTraitFatPtr(target); ok {
		if _, srcIsTrait := cg.isTraitFatPtr(src); !srcIsTrait {
			result, err := cg.coerceToTrait(block, val, traitName)
			if err == nil {
				return result
			}
		}
	}

	// implicit[T] conversion: struct S implements implicit[T], call static fn.
	if targetName := cg.typeNameOf(target); targetName != "" {
		for _, entry := range cg.implicitConvFns[targetName] {
			if entry.srcLLVM.Equal(src) {
				return block.NewCall(entry.fn, val)
			}
		}
	}

	// Named function pointer → fat-fn-ptr: wrap in a thin shim with (i8* env, params...).
	// This enables passing named functions (including extern) to higher-order functions.
	if isFatFnPtr(target) && !isFatFnPtr(src) {
		if _, ok := src.(*irtypes.PointerType); ok {
			return cg.wrapFnAsFatPtr(block, val, target)
		}
	}

	// Empty array literal {i8*, i64} → typed fat array {T*, i64}: use zero value
	// of the target type so the null data pointer is properly typed.
	if isFatArrayPtr(src) && isFatArrayPtr(target) {
		return cg.zeroValue(target)
	}

	// Fat-pointer (string / dynamic array) → raw C pointer: extract data ptr.
	// This enables passing Tin strings directly to extern C functions.
	if isFatPtrType(src) {
		if _, ok := target.(*irtypes.PointerType); ok {
			rawPtr := cg.extractFatPtrData(block, val, src.(*irtypes.StructType))
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}
			return block.NewBitCast(rawPtr, target)
		}
	}

	switch {
	// Any type: box the value.
	case isAnyType(target) && !isAnyType(src):
		return cg.boxToAny(block, val)

	// Int → Int: extend or truncate.
	case irtypes.IsInt(src) && irtypes.IsInt(target):
		sBits := src.(*irtypes.IntType).BitSize
		tBits := target.(*irtypes.IntType).BitSize
		if sBits < tBits {
			return block.NewSExt(val, target)
		} else if sBits > tBits {
			return block.NewTrunc(val, target)
		}
		return val

	// Float → Float.
	case irtypes.IsFloat(src) && irtypes.IsFloat(target):
		sBits := floatBits(src.(*irtypes.FloatType))
		tBits := floatBits(target.(*irtypes.FloatType))
		if sBits < tBits {
			return block.NewFPExt(val, target)
		} else if sBits > tBits {
			return block.NewFPTrunc(val, target)
		}
		return val

	// Int → Float.
	case irtypes.IsInt(src) && irtypes.IsFloat(target):
		return block.NewSIToFP(val, target)

	// Float → Int.
	case irtypes.IsFloat(src) && irtypes.IsInt(target):
		return block.NewFPToSI(val, target)

	// Pointer → Pointer.
	case irtypes.IsPointer(src) && irtypes.IsPointer(target):
		return block.NewBitCast(val, target)

	// Int → Pointer.
	case irtypes.IsInt(src) && irtypes.IsPointer(target):
		return block.NewIntToPtr(val, target)

	// Pointer → Int.
	case irtypes.IsPointer(src) && irtypes.IsInt(target):
		return block.NewPtrToInt(val, target)
	}

	// Unbox any to a primitive scalar (int or float).
	// Extract the data pointer from the any fat-ptr and load the value.
	if isAnyType(src) && (irtypes.IsInt(target) || irtypes.IsFloat(target)) {
		anyType := anyFatPtrType()
		anyAlloca := block.NewAlloca(anyType)
		block.NewStore(val, anyAlloca)
		ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
		typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(target))
		return block.NewLoad(target, typedPtr)
	}

	// Last resort: bitcast if same size.
	return val
}

func floatBits(t *irtypes.FloatType) int {
	switch t.Kind {
	case irtypes.FloatKindHalf:
		return 16
	case irtypes.FloatKindFloat:
		return 32
	case irtypes.FloatKindDouble:
		return 64
	}
	return 64
}

// zeroValue returns the zero constant for a given type.
func (cg *CodeGen) zeroValue(t irtypes.Type) value.Value {
	switch {
	case irtypes.IsInt(t):
		return constant.NewInt(t.(*irtypes.IntType), 0)
	case irtypes.IsFloat(t):
		return constant.NewFloat(t.(*irtypes.FloatType), 0)
	case irtypes.IsPointer(t):
		return constant.NewNull(t.(*irtypes.PointerType))
	case irtypes.IsStruct(t):
		st := t.(*irtypes.StructType)
		fields := make([]constant.Constant, len(st.Fields))
		for i, f := range st.Fields {
			fields[i] = cg.zeroValue(f).(constant.Constant)
		}
		return constant.NewStruct(st, fields...)
	case irtypes.IsArray(t):
		at := t.(*irtypes.ArrayType)
		elems := make([]constant.Constant, at.Len)
		for i := range elems {
			elems[i] = cg.zeroValue(at.ElemType).(constant.Constant)
		}
		return constant.NewArray(at, elems...)
	}
	return constant.NewInt(irtypes.I64, 0)
}

// ── Reflection builtins ───────────────────────────────────────────────────────

// structNameFromValue returns the LLVM named struct name for a value's type,
// or "" if the value is not a named struct.
func structNameFromValue(v value.Value) string {
	t := v.Type()
	if pt, ok := t.(*irtypes.PointerType); ok {
		t = pt.ElemType
	}
	if st, ok := t.(*irtypes.StructType); ok {
		return st.Name()
	}
	return ""
}

// buildAtomArray allocates a heap array of string fat-ptrs and returns a
// fat-pointer {atom*, i64} representing [atom].
func (cg *CodeGen) buildAtomArray(block *ir.Block, atoms []string) value.Value {
	atomFatType := stringFatPtrType() // {i8*, i64}
	n := int64(len(atoms))

	if n == 0 {
		fat := irtypes.NewStruct(irtypes.NewPointer(atomFatType), irtypes.I64)
		alloca := block.NewAlloca(fat)
		ptrGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewNull(irtypes.NewPointer(atomFatType)), ptrGep)
		lenGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenGep)
		return block.NewLoad(fat, alloca)
	}

	vals := make([]value.Value, n)
	for i, a := range atoms {
		vals[i] = cg.buildStringFatPtr(block, a)
	}

	nullPtr := constant.NewNull(irtypes.NewPointer(atomFatType))
	sizeGep := block.NewGetElementPtr(atomFatType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSz := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSz := block.NewMul(elemSz, constant.NewInt(irtypes.I64, n))

	mallocI8 := block.NewCall(cg.ensureMalloc(), totalSz)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(atomFatType))

	for i, v := range vals {
		gep := block.NewGetElementPtr(atomFatType, dataPtr, constant.NewInt(irtypes.I64, int64(i)))
		block.NewStore(v, gep)
	}

	fat := irtypes.NewStruct(irtypes.NewPointer(atomFatType), irtypes.I64)
	fatAlloca := block.NewAlloca(fat)
	ptrGep := block.NewGetElementPtr(fat, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, ptrGep)
	lenGep := block.NewGetElementPtr(fat, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, n), lenGep)
	return block.NewLoad(fat, fatAlloca)
}

// llvmTypeName returns the tin type name string for any LLVM type,
// including nested pointer and array types.
func llvmTypeName(t irtypes.Type) string {
	if t == nil {
		return "void"
	}
	switch {
	case t.Equal(irtypes.I1):
		return "bool"
	case t.Equal(irtypes.I8):
		return "i8"
	case t.Equal(irtypes.I16):
		return "i16"
	case t.Equal(irtypes.I32):
		return "i32"
	case t.Equal(irtypes.I64):
		return "i64"
	case t.Equal(irtypes.Float):
		return "f32"
	case t.Equal(irtypes.Double):
		return "f64"
	}
	if pt, ok := t.(*irtypes.PointerType); ok {
		if fnType, isFnType := pt.ElemType.(*irtypes.FuncType); isFnType {
			return fnSigName(fnType, false)
		}
		return "*" + llvmTypeName(pt.ElemType)
	}
	if at, ok := t.(*irtypes.ArrayType); ok {
		return "[" + llvmTypeName(at.ElemType) + "]"
	}
	if st, ok := t.(*irtypes.StructType); ok {
		if st.Name() != "" {
			// User-defined struct / data type: use quoted atom format '"name"'.
			return "\"" + st.Name() + "\""
		}
		// Anonymous struct: could be fat ptr, any, etc.
		if isStringType(t) {
			return "string"
		}
		if isAnyType(t) {
			return "any"
		}
		if isFatFnPtr(t) {
			st2 := t.(*irtypes.StructType)
			innerFnType := st2.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			return fnSigName(innerFnType, true)
		}
		if isFatArrayPtr(t) {
			if len(st.Fields) == 2 {
				if pt, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
					return "[" + llvmTypeName(pt.ElemType) + "]"
				}
			}
			return "[unknown]"
		}
	}
	return "unknown"
}

// primitiveTypeName is an alias kept for compatibility with existing callers
// that only deal with simple scalar types.
func primitiveTypeName(t irtypes.Type) string {
	return llvmTypeName(t)
}

// buildTypeNameAtom builds the atom for a known struct/data-type name.
// User-defined names use the quoted format '"name"'.
func (cg *CodeGen) buildTypeNameAtom(block *ir.Block, sn string) value.Value {
	return cg.buildStringFatPtr(block, "'\""+sn+"\"")
}

// runtimeAtomSelectByTypeID generates an inline select chain that picks the
// correct atom from a table keyed by compile-time type IDs.
// table maps type_id → atom string (with leading ').
// typeIDVal is the i32 type_id extracted at runtime.
// defaultAtom is the value used when no type_id matches.
func (cg *CodeGen) runtimeAtomSelectByTypeID(block *ir.Block, typeIDVal value.Value,
	table map[int32]string, defaultAtom value.Value) value.Value {

	result := defaultAtom
	for id, atomStr := range table {
		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(id)))
		candidate := cg.buildStringFatPtr(block, atomStr)
		result = block.NewSelect(isMatch, candidate, result)
	}
	return result
}

// extractAnyTypeID extracts the i32 type_id from an `any` fat-ptr value.
func (cg *CodeGen) extractAnyTypeID(block *ir.Block, anyVal value.Value) value.Value {
	anyType := anyFatPtrType()
	alloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, alloca)
	tagGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return block.NewLoad(irtypes.I32, tagGep)
}

// buildTypeIDToNameTable builds the full type_id → atom name table from
// all registered struct and data types plus the reserved primitive IDs.
func (cg *CodeGen) buildTypeIDToNameTable() map[int32]string {
	table := map[int32]string{
		anyTagInt:    "'i64",
		anyTagFloat:  "'f64",
		anyTagString: "'string",
		anyTagBool:   "'bool",
		anyTagPtr:    "'ptr",
		anyTagFn:     "'fn",
	}
	for sn, id := range cg.structTypeIDs {
		table[id] = "'" + sn
	}
	for dn, id := range cg.dataTypeIDs {
		table[id] = "'" + dn
	}
	for sig, id := range cg.fnTypeIDs {
		table[id] = "'" + sig
	}
	return table
}

// buildTypeIDToTraitsTable builds type_id → []trait atom strings table.
func (cg *CodeGen) buildTypeIDToTraitsTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, tn := range cg.structImpls[sn] {
			atoms = append(atoms, "'"+tn)
		}
		table[id] = atoms
	}
	return table
}

// buildTypeIDToFieldsTable builds type_id → []field name atom strings table.
func (cg *CodeGen) buildTypeIDToFieldsTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, fn := range cg.structFields[sn] {
			atoms = append(atoms, "'"+fn)
		}
		table[id] = atoms
	}
	return table
}

// buildTypeIDToFieldTypesTable builds type_id → []field type name atom strings table.
func (cg *CodeGen) buildTypeIDToFieldTypesTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, ft := range cg.structFieldLLVMTypes[sn] {
			atoms = append(atoms, "'"+primitiveTypeName(ft))
		}
		table[id] = atoms
	}
	return table
}

// genTypeof returns an atom of the type name.
// For concrete types the name is resolved at compile-time.
// For `any` values the actual type_id is inspected at runtime.
func (cg *CodeGen) genTypeof(block *ir.Block, e *ast.TypeofExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildStringFatPtr(block, "'unknown"), nil
	}

	// Runtime dispatch for `any` values.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		table := cg.buildTypeIDToNameTable()
		defaultAtom := cg.buildStringFatPtr(block, "'unknown")
		return cg.runtimeAtomSelectByTypeID(block, typeIDVal, table, defaultAtom), nil
	}

	// Compile-time: any type (including named structs, pointers, arrays, primitives).
	return cg.buildStringFatPtr(block, "'"+llvmTypeName(val.Type())), nil
}

// genTraitof returns a [atom] of trait names.
// For `any` values the type_id is inspected at runtime and the result is
// selected from a per-type compile-time table.
func (cg *CodeGen) genTraitof(block *ir.Block, e *ast.TraitofExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToTraitsTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, tn := range cg.structImpls[sn] {
			atoms = append(atoms, "'"+tn)
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// genFieldnames returns a [atom] of field names.
// For `any` values the type_id is inspected at runtime.
func (cg *CodeGen) genFieldnames(block *ir.Block, e *ast.FieldnamesExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToFieldsTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, fn := range cg.structFields[sn] {
			atoms = append(atoms, "'"+fn)
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// runtimeAtomArraySelectByTypeID selects a [atom] array based on a runtime type_id.
// It builds all candidate arrays at compile-time and uses an alloca + select pattern
// to pick the right one.  The result type is always {atom*, i64}.
func (cg *CodeGen) runtimeAtomArraySelectByTypeID(block *ir.Block, typeIDVal value.Value,
	table map[int32][]string) value.Value {

	// We need a consistent fat-pointer type for the result.
	atomFatType := stringFatPtrType()
	fatType := irtypes.NewStruct(irtypes.NewPointer(atomFatType), irtypes.I64)

	// Build default (empty array).
	def := cg.buildAtomArray(block, nil)
	resultAlloca := block.NewAlloca(fatType)
	block.NewStore(def, resultAlloca)

	for id, atomStrs := range table {
		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(id)))
		candidate := cg.buildAtomArray(block, atomStrs)
		current := block.NewLoad(fatType, resultAlloca)
		// LLVM select works on first-class types including structs.
		selected := block.NewSelect(isMatch, candidate, current)
		block.NewStore(selected, resultAlloca)
	}

	return block.NewLoad(fatType, resultAlloca)
}

// genFieldtypes returns a [atom] of field type names for the compile-time struct type.
// For `any` values the type_id is inspected at runtime.
func (cg *CodeGen) genFieldtypes(block *ir.Block, e *ast.FieldtypesExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToFieldTypesTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, ft := range cg.structFieldLLVMTypes[sn] {
			atoms = append(atoms, "'"+primitiveTypeName(ft))
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// genFieldtag returns the first @"tag" annotation for the named field, or empty atom.
func (cg *CodeGen) genFieldtag(block *ir.Block, e *ast.FieldtagExpr) (value.Value, error) {
	return cg.buildStringFatPtr(block, "'"), nil
}

// genGetfield returns an `any` fat-ptr containing the value of the named field.
// For concrete struct types: generates a compile-time strcmp chain.
// For `any` values: dispatches to the concrete type via type_id, then reads the field.
func (cg *CodeGen) genGetfield(block *ir.Block, e *ast.GetfieldExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	fieldNameVal, err := cg.genExpr(block, e.Field)
	if err != nil {
		return nil, err
	}

	anyType := anyFatPtrType()
	zeroAny := cg.zeroValue(anyType)

	if val == nil {
		return zeroAny, nil
	}

	// If the value is `any`, extract the data pointer and type_id,
	// then dispatch to genGetfieldForStruct for each known struct type.
	if isAnyType(val.Type()) {
		return cg.genGetfieldFromAny(block, val, fieldNameVal), nil
	}

	// Compile-time concrete struct.
	sn := structNameFromValue(val)
	if sn == "" {
		return zeroAny, nil
	}
	return cg.genGetfieldForStruct(block, sn, val, fieldNameVal)
}

// genGetfieldFromAny dispatches getfield for an `any` value over all known struct types.
func (cg *CodeGen) genGetfieldFromAny(block *ir.Block, anyVal value.Value, fieldNameVal value.Value) value.Value {
	anyType := anyFatPtrType()
	resultAlloca := block.NewAlloca(anyType)
	block.NewStore(cg.zeroValue(anyType), resultAlloca)

	typeIDVal := cg.extractAnyTypeID(block, anyVal)

	// Extract the raw data pointer from `any`.
	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, anyAlloca)
	dataPtrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataI8Ptr := block.NewLoad(irtypes.I8Ptr, dataPtrGep)

	strcmp := cg.ensureStrcmp()
	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)

	for sn, typeID := range cg.structTypeIDs {
		st := cg.structTypes[sn]
		if st == nil {
			continue
		}
		fieldNames := cg.structFields[sn]
		fieldTypes := cg.structFieldLLVMTypes[sn]
		vtableOff := cg.vtableOffset(sn)

		isTypeMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))

		// Bitcast data pointer to *struct.
		structPtr := block.NewBitCast(dataI8Ptr, irtypes.NewPointer(st))

		for i, fname := range fieldNames {
			namePtr := cg.newGlobalString(fname)
			cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
			isFieldMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

			isMatch := block.NewAnd(isTypeMatch, isFieldMatch)

			fieldIdx := int64(1 + vtableOff + i)
			fieldGep := block.NewGetElementPtr(st, structPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
			fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
			boxed := cg.boxToAny(block, fieldVal)

			current := block.NewLoad(anyType, resultAlloca)
			selected := block.NewSelect(isMatch, boxed, current)
			block.NewStore(selected, resultAlloca)
		}
	}

	return block.NewLoad(anyType, resultAlloca)
}

// genGetfieldForStruct generates a strcmp chain for a concrete struct type.
func (cg *CodeGen) genGetfieldForStruct(block *ir.Block, sn string, val value.Value, fieldNameVal value.Value) (value.Value, error) {
	anyType := anyFatPtrType()
	zeroAny := cg.zeroValue(anyType)

	fieldNames := cg.structFields[sn]
	fieldTypes := cg.structFieldLLVMTypes[sn]
	st := cg.structTypes[sn]
	if st == nil || len(fieldNames) == 0 {
		return zeroAny, nil
	}

	structAlloca := block.NewAlloca(st)
	block.NewStore(val, structAlloca)

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	vtableOff := cg.vtableOffset(sn)

	resultAlloca := block.NewAlloca(anyType)
	block.NewStore(zeroAny, resultAlloca)

	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		fieldIdx := int64(1 + vtableOff + i)
		fieldGep := block.NewGetElementPtr(st, structAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
		boxed := cg.boxToAny(block, fieldVal)

		current := block.NewLoad(anyType, resultAlloca)
		selected := block.NewSelect(isMatch, boxed, current)
		block.NewStore(selected, resultAlloca)
	}

	return block.NewLoad(anyType, resultAlloca), nil
}

// genSetfield sets the named field of a struct value (via lvalue) from a typed value.
// Generates a compile-time strcmp chain — one comparison per field.
func (cg *CodeGen) genSetfield(block *ir.Block, e *ast.SetfieldExpr) (value.Value, error) {
	structPtr, err := cg.genLValue(block, e.Expr)
	if err != nil {
		return nil, err
	}
	fieldNameVal, err := cg.genExpr(block, e.Field)
	if err != nil {
		return nil, err
	}
	newVal, err := cg.genExpr(block, e.Val)
	if err != nil {
		return nil, err
	}

	if structPtr == nil || newVal == nil {
		return nil, nil
	}

	pt, ok := structPtr.Type().(*irtypes.PointerType)
	if !ok {
		return nil, nil
	}
	st, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return nil, nil
	}
	sn := st.Name()
	if sn == "" {
		return nil, nil
	}

	fieldNames := cg.structFields[sn]
	fieldTypes := cg.structFieldLLVMTypes[sn]
	if len(fieldNames) == 0 {
		return nil, nil
	}

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	vtableOff := cg.vtableOffset(sn)

	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		fieldIdx := int64(1 + vtableOff + i)
		fieldGep := block.NewGetElementPtr(st, structPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))

		coerced := cg.coerce(block, newVal, fieldTypes[i])
		currentField := block.NewLoad(fieldTypes[i], fieldGep)
		selected := block.NewSelect(isMatch, coerced, currentField)
		block.NewStore(selected, fieldGep)
	}

	return nil, nil
}
