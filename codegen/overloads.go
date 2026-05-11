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
// as part of the mangled overload name.
func typeExprMangle(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		// Replace non-identifier chars with underscore.
		return strings.Map(func(r rune) rune {
			if r == ':' || r == '.' {
				return '_'
			}

			return r
		}, t.Name)
	case *ast.PointerType:
		if t.IsConst {
			return "cptr_" + typeExprMangle(t.Elem)
		}

		return "ptr_" + typeExprMangle(t.Elem)
	case *ast.ArrayType:
		return "arr_" + typeExprMangle(t.Elem)
	case *ast.GenericType:
		parts := []string{t.Name}
		for _, tp := range t.TypeParams {
			parts = append(parts, typeExprMangle(tp))
		}

		return strings.Join(parts, "_")
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
func funcParamSig(params []ast.Param) string {
	var parts []string

	for _, p := range params {
		if p.IsVarArgs {
			continue
		}

		parts = append(parts, typeExprMangle(p.Type))
	}

	return strings.Join(parts, "__")
}

// methodParamSig returns the mangled parameter-type signature for a method,
// skipping the implicit "this" first parameter.
func methodParamSig(m *ast.FuncDecl, structName string) string {
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

	sig := funcParamSig(params)
	// `coerce[T]` op-trait methods have shape `static fn ::coerce(this S) T`.
	// Param sig collapses to "" after stripping `this`, so two coerce impls
	// for the same struct would mangle to the same name and clobber each
	// other.  Mix the return type into the sig so coerce[i64] and
	// coerce[string] survive overload-mangling on the same struct.
	if m.Name == "coerce" && m.IsStatic && m.RetType != nil {
		sig += "->" + m.RetType.String()
	}

	return sig
}

// appendGenericFuncOverload appends fd to overloads, replacing any existing
// entry whose param signature matches (an idempotent re-registration -- the
// same template can be registered multiple times under bare + qualified
// keys, or across re-entrant passes).  Different signatures coexist so call
// sites can disambiguate generic free-fn overloads by arity / param shape.
func appendGenericFuncOverload(overloads []*ast.FuncDecl, fd *ast.FuncDecl) []*ast.FuncDecl {
	if fd == nil {
		return overloads
	}

	sig := funcParamSig(fd.Params)
	for i, existing := range overloads {
		if existing == fd {
			return overloads
		}

		if funcParamSig(existing.Params) == sig && len(existing.TypeParams) == len(fd.TypeParams) {
			overloads[i] = fd

			return overloads
		}
	}

	return append(overloads, fd)
}

// pickGenericFuncOverload returns the entry from overloads whose declared
// param arity matches argCount.  Returns nil when no overload matches or
// when the choice would be ambiguous (multiple matches of the same arity).
// The single-entry case short-circuits so the common non-overloaded path
// stays a single pointer read.
func pickGenericFuncOverload(overloads []*ast.FuncDecl, argCount int) *ast.FuncDecl {
	if len(overloads) == 0 {
		return nil
	}

	if len(overloads) == 1 {
		return overloads[0]
	}

	var (
		match  *ast.FuncDecl
		hits   int
		strict int
	)

	for _, fd := range overloads {
		fixed := 0

		for _, p := range fd.Params {
			if !p.IsVarArgs {
				fixed++
			}
		}

		if fixed == argCount {
			match = fd
			strict++

			continue
		}

		if fixed < argCount {
			// Variadic candidate.
			for _, p := range fd.Params {
				if p.IsVarArgs {
					match = fd
					hits++

					break
				}
			}
		}
	}

	if strict == 1 {
		return match
	}

	if strict == 0 && hits == 1 {
		return match
	}

	return nil
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
	return scanOverloadedNamesPkg(nodes, "")
}

// scanOverloadedNamesPkg counts duplicate method names per struct using the
// package-qualified struct key (`pkg__Name_method`) when a package context
// is active. Without this, methods on stdlib structs that share a name
// (e.g. five `static fn ::implicit(...)` overloads on decimal::Value) end
// up keyed by their bare struct name and never get marked as overloads in
// the package-loaded scope, which breaks IR-name mangling.
func scanOverloadedNamesPkg(nodes []ast.Node, pkgName string) map[string]bool {
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

			structName := n.Name
			if pkgName != "" {
				structName = pkgName + "__" + structName
			}

			for _, m := range n.Methods {
				if m.IsExtern != "" {
					continue
				}

				key := methodScopeName(structName, m)
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
//     to the corresponding trait fat-ptr parameter (struct implements that trait).
//  2. Arity-only match: arity matches, types are ignored (fallback).
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
	// Pass 2: arity-only fallback. Used only when the caller provided
	// no resolved arg types (every entry in argTypes is nil), as can
	// happen for nested generics during early-resolution passes. With
	// any concrete arg type known, falling through here would silently
	// pick a variant whose param type doesn't match, producing wrong
	// results (e.g. `add(1.0, 2.0)` calling an `add(i64, i64)` overload
	// after f64->i64 truncation). Force the user to disambiguate.
	allArgsUnknown := true

	for _, t := range argTypes {
		if t != nil {
			allArgsUnknown = false

			break
		}
	}

	if allArgsUnknown {
		for _, v := range variants {
			if v.arity == len(argVals) {
				return v
			}
		}
	}

	return nil
}

// isIntLLVMType returns true when t is any LLVM integer type (i1, i8, i16, i32, i64, ...).
func isIntLLVMType(t irtypes.Type) bool {
	_, ok := t.(*irtypes.IntType)

	return ok
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
