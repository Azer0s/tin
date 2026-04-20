package codegen

// overloads.go - Function overloading by parameter arity and type.
//
// When multiple functions (or struct methods) share the same name, the compiler
// mangles each variant's IR name by appending the parameter type signature:
//
//   fn foo(n i64) = ...            -> IR: foo__i64
//   fn foo(s string) = ...         -> IR: foo__string
//   fn foo(n i64, s string) = ...  -> IR: foo__i64__string
//
// Call sites resolve to the best-matching overload using the evaluated argument
// types.  The original plain name is removed from scope; all lookups go through
// the overload registry.
//
// The same scheme applies to struct methods:
//
//   fn (MyStruct) bar(n i64)     -> scope key: MyStruct_bar__i64
//   fn (MyStruct) bar(s string)  -> scope key: MyStruct_bar__string

import (
	"strings"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// overloadEntry represents one variant of an overloaded function.
type overloadEntry struct {
	irName     string         // mangled IR / scope name
	paramSig   string         // raw signature string used for mangling
	paramTypes []irtypes.Type // LLVM types of the non-this parameters (resolved after predecl)
	arity      int            // number of explicit (non-this) parameters
	returnType irtypes.Type   // LLVM return type (nil if unknown/void); used for hint-guided selection
}

// typeExprMangle converts a Tin TypeExpr to a safe identifier fragment used
// as part of the mangled overload name.  It produces fully-qualified names for
// package types (e.g. "sync::Client" -> "sync__Client") so that same-named
// types from different packages never produce colliding overload signatures.
func (cg *CodeGen) typeExprMangle(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		return cg.typeExprCanonicalKey(te)
	case *ast.PointerType:
		if t.IsConst {
			return "cptr_" + cg.typeExprMangle(t.Elem)
		}

		return "ptr_" + cg.typeExprMangle(t.Elem)
	case *ast.ArrayType:
		return "arr_" + cg.typeExprMangle(t.Elem)
	case *ast.GenericType:
		name := cg.typeExprCanonicalKey(&ast.SimpleType{Name: t.Name})

		parts := []string{name}
		for _, tp := range t.TypeParams {
			parts = append(parts, cg.typeExprMangle(tp))
		}

		return strings.Join(parts, "__")
	case *ast.FuncType:
		return "fn"
	case *ast.VoidType:
		return "void"
	case *ast.UnionTypeExpr:
		return "union"
	case *ast.TupleArrayType:
		return "tuple"
	}

	return "t"
}

// funcParamSig returns the mangled parameter-type signature for a function
// declaration, e.g. "i64__string" for (n i64, s string).
// Vararg parameters are excluded.
func (cg *CodeGen) funcParamSig(params []ast.Param) string {
	var parts []string

	for _, p := range params {
		if p.IsVarArgs {
			continue
		}

		parts = append(parts, cg.typeExprMangle(p.Type))
	}

	return strings.Join(parts, "__")
}

// methodParamSig returns the mangled parameter-type signature for a method,
// skipping the implicit "this" first parameter.
func (cg *CodeGen) methodParamSig(m *ast.FuncDecl, structName string) string {
	params := m.Params
	// Skip the first parameter if it is the "this" receiver.
	// It may be declared as `this StructName` (value) or `this *StructName` (pointer).
	if len(params) > 0 {
		switch pt := params[0].Type.(type) {
		case *ast.SimpleType:
			if pt.Name == structName {
				params = params[1:]
			}
		case *ast.PointerType:
			if inner, ok := pt.Elem.(*ast.SimpleType); ok && inner.Name == structName {
				params = params[1:]
			}
		}
	}

	return cg.funcParamSig(params)
}

// overloadMangledName returns the mangled IR name for a function/method when
// it is part of an overload set.
func overloadMangledName(baseName, sig string) string {
	if sig == "" {
		return baseName + "__"
	}

	return baseName + "__" + sig
}

// scanOverloadedNames scans AST nodes and returns the set of function/method
// base names that appear more than once in the same scope (and thus require
// overload mangling).
//
// For top-level functions the key is the function name.
// For struct methods the key is "StructName_methodName".
func scanOverloadedNames(nodes []ast.Node) map[string]bool {
	// Count occurrences of each name (excluding constrained generics).
	// Extern functions ARE included so that packages with multiple extern overloads
	// (e.g. splat(f32) and splat(f64)) get their IR names mangled correctly.
	counts := make(map[string]int)

	for _, node := range nodes {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if len(n.Constraints) > 0 {
				continue
			}

			counts[n.Name]++
		case *ast.StructDecl:
			if len(n.TypeParams) > 0 {
				continue // generic templates handled separately
			}

			for _, m := range n.Methods {
				if m.IsExtern != "" {
					continue
				}

				key := methodScopeName(n.Name, m)
				counts[key]++
			}
		}
	}

	overloaded := make(map[string]bool)

	for name, count := range counts {
		if count > 1 {
			overloaded[name] = true
		}
	}

	return overloaded
}

// resolveParamTypes returns the resolved LLVM types for a function's explicit
// parameters (varargs excluded).  When structName is non-empty the first
// parameter whose type matches structName or *structName is treated as the
// "this" receiver and is excluded from the returned slice.
func (cg *CodeGen) resolveParamTypes(params []ast.Param, structName string) ([]irtypes.Type, error) {
	var types []irtypes.Type

	first := true

	for _, p := range params {
		if p.IsVarArgs {
			continue
		}
		// Skip the "this" receiver for methods.
		if first && structName != "" {
			switch pt := p.Type.(type) {
			case *ast.SimpleType:
				if pt.Name == structName {
					first = false

					continue
				}
			case *ast.PointerType:
				if inner, ok := pt.Elem.(*ast.SimpleType); ok && inner.Name == structName {
					first = false

					continue
				}
			}
		}

		first = false

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}

		types = append(types, pt)
	}

	return types, nil
}

// resolveOverload picks the best-matching overload variant for the given
// argument values.  Selection rules (in order):
//  1. Exact match: arity matches AND every parameter type equals the argument type.
//     1.5. Coercible match: arity matches AND every concrete-struct arg can be coerced
//     to the corresponding trait fat-ptr parameter (struct implements that trait),
//     or integer types are widened/narrowed.
//
// Returns nil when no variant matches.
func (cg *CodeGen) resolveOverload(variants []*overloadEntry, argVals []value.Value) *overloadEntry {
	// Build a parallel slice of arg types once.
	argTypes := make([]irtypes.Type, len(argVals))
	allConstants := true

	for i, v := range argVals {
		if v != nil {
			argTypes[i] = v.Type()

			if _, isConst := v.(constant.Constant); !isConst {
				allConstants = false
			}
		}
	}

	// Pass 0: return-type hint from let-binding annotation.
	// Priority: let binding type > concrete arg types > constant arg types.
	// When a hint is set, prefer the overload whose return type matches it.
	// If all args are constants (no concrete type information from args), always
	// apply the hint. If non-constant args are present, only apply when there is
	// exactly one hint-matching candidate to avoid overriding clear parameter matches.
	if cg.returnTypeHint != nil {
		var hintMatch *overloadEntry

		hintMatches := 0

		for _, v := range variants {
			if v.arity != len(argVals) {
				continue
			}

			if v.returnType != nil && v.returnType.Equal(cg.returnTypeHint) {
				hintMatch = v
				hintMatches++
			}
		}

		if hintMatch != nil && (allConstants || hintMatches == 1) {
			return hintMatch
		}
	}

	// Pass 1: exact type match.
	for _, v := range variants {
		if v.arity != len(argVals) {
			continue
		}

		if typesMatchExact(v.paramTypes, argTypes) {
			return v
		}
	}
	// Pass 1.5: struct -> trait fat-ptr coercible match.
	for _, v := range variants {
		if v.arity != len(argVals) {
			continue
		}

		if cg.typesMatchCoercible(v.paramTypes, argTypes) {
			return v
		}
	}

	return nil
}

// isIntLLVMType returns true when t is any LLVM integer type (i1, i8, i16, i32, i64, ...).
func isIntLLVMType(t irtypes.Type) bool {
	_, ok := t.(*irtypes.IntType)

	return ok
}

// overloadSigList formats a slice of overload entries as a human-readable list
// of signatures for use in error messages.  Each entry is printed as
// "name(type, type, ...)" on its own bullet line.
func overloadSigList(name string, variants []*overloadEntry) string {
	var b strings.Builder

	for _, v := range variants {
		b.WriteString("\n  ")
		b.WriteString(name)
		b.WriteByte('(')

		if v.paramSig != "" {
			b.WriteString(strings.ReplaceAll(v.paramSig, "__", ", "))
		}

		b.WriteByte(')')
	}

	return b.String()
}

// typesMatchCoercible returns true when every argument can be coerced to the
// corresponding parameter type - specifically when a concrete-struct arg can be
// widened to a trait fat-ptr parameter because the struct has a registered vtable
// for that trait.
func (cg *CodeGen) typesMatchCoercible(paramTypes, argTypes []irtypes.Type) bool {
	if len(paramTypes) != len(argTypes) {
		return false
	}

	for i, pt := range paramTypes {
		at := argTypes[i]
		if pt == nil || at == nil {
			continue
		}

		if pt.Equal(at) {
			continue
		}
		// Allow integer type coercions (e.g. i64 arg -> i8/i16/i32 param for @'\n' vs byte).
		if isIntLLVMType(pt) && isIntLLVMType(at) {
			continue
		}
		// Parameter is a trait fat-ptr?
		instKey, isTrait := cg.isTraitFatPtr(pt)
		if !isTrait {
			return false
		}
		// Argument must not already be a fat-ptr (would be a different mismatch).
		if _, alreadyTrait := cg.isTraitFatPtr(at); alreadyTrait {
			return false
		}
		// Check that the concrete struct has a vtable for this trait.
		structName := cg.typeNameOf(at)
		if structName == "" {
			return false
		}

		vtableKey := structName + "__" + instKey
		if _, ok := cg.traitVtableGlobals[vtableKey]; !ok {
			return false
		}
	}

	return true
}

// typesMatchExact returns true when all parameter and argument types are equal.
func typesMatchExact(paramTypes []irtypes.Type, argTypes []irtypes.Type) bool {
	if len(paramTypes) != len(argTypes) {
		return false
	}

	for i, pt := range paramTypes {
		at := argTypes[i]
		if pt == nil || at == nil {
			continue
		}

		if !pt.Equal(at) {
			return false
		}
	}

	return true
}
