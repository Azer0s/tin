package codegen

// types.go - LLVM type mapping, type helpers, and type query utilities.

import (
	"fmt"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// typeExprCanonicalKey returns a canonical string key for a TypeExpr that is
// suitable for use as a monomorphization concrete-type name component.
//
// For non-generic (simple) types:
//   - Qualified names like "sync::Unit" are converted to "sync__Unit".
//   - Bare names like "Unit" are looked up in cg.typeAliases; if the alias
//     resolves to a SimpleType (e.g. "sync__Unit"), that canonical name is
//     returned.  This ensures that bare names used inside a package body
//     produce the same key as the fully-qualified names used from the outside.
//
// For generic types:
//   - The template name has its package qualifier stripped (bare template name).
//   - Type parameters are recursively canonicalized.
//
// This guarantees that Future[sync::Unit] and Future[Unit] (inside sync)
// coalesce to the same concrete key "Future__sync__Unit".
func (cg *CodeGen) typeExprCanonicalKey(te ast.TypeExpr) string {
	return cg.typeExprCanonicalKeyN(te, 0)
}

func (cg *CodeGen) typeExprCanonicalKeyN(te ast.TypeExpr, depth int) string {
	if depth > 64 {
		// Alias chain too deep (likely a cycle); return the bare type string.
		return te.String()
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		name := t.Name
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			// Qualified name: check typeAliases first (e.g. "collections::HashMap" may
			// alias back to the bare "HashMap" used in genericStructsByArity).
			if alias, ok := cg.typeAliases[name]; ok {
				return cg.typeExprCanonicalKeyN(alias, depth+1)
			}

			return strings.ReplaceAll(name, "::", "__")
		}
		// Bare name: look up in typeAliases for the canonical form.
		// Recurse to handle alias chains (e.g. t -> Unit -> sync__Unit, or t -> [byte]).
		if alias, ok := cg.typeAliases[name]; ok {
			return cg.typeExprCanonicalKeyN(alias, depth+1)
		}

		return name
	case *ast.GenericType:
		name := t.Name
		if strings.Contains(name, "::") {
			// Qualified name: check typeAliases first (e.g. "collections::HashMap" -> "HashMap").
			if alias, ok := cg.typeAliases[name]; ok {
				name = cg.typeExprCanonicalKeyN(alias, depth+1)
			} else {
				name = strings.ReplaceAll(name, "::", "__")
			}
		} else if alias, ok := cg.typeAliases[name]; ok {
			// Bare name: resolve through alias (e.g. bare "Channel" -> "sync__Channel").
			name = cg.typeExprCanonicalKeyN(alias, depth+1)
		}
		// else: bare name not in aliases - user-local generic struct or builtin.

		parts := make([]string, len(t.TypeParams))
		for i, tp := range t.TypeParams {
			parts[i] = cg.typeExprCanonicalKeyN(tp, depth+1)
		}

		return name + "__" + strings.Join(parts, "__")
	case *ast.PointerType:
		return "*" + cg.typeExprCanonicalKeyN(t.Elem, depth+1)
	case *ast.ArrayType:
		return "[]" + cg.typeExprCanonicalKeyN(t.Elem, depth+1)
	}

	return te.String()
}

// Type mapping

// tinTypeToLLVM converts an ast.TypeExpr to an LLVM type.
func (cg *CodeGen) tinTypeToLLVM(te ast.TypeExpr) (irtypes.Type, error) {
	if te == nil {
		return irtypes.Void, nil
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		return cg.resolveSimpleType(t.Name)
	case *ast.VoidType:
		return irtypes.Void, nil
	case *ast.PointerType:
		// *void is invalid in LLVM IR - use i8* (opaque pointer convention)
		if st, ok := t.Elem.(*ast.SimpleType); ok && st.Name == "void" {
			return irtypes.I8Ptr, nil
		}

		inner, err := cg.tinTypeToLLVM(t.Elem)
		if err != nil {
			return nil, err
		}

		return irtypes.NewPointer(inner), nil
	case *ast.ArrayType:
		elem, err := cg.tinTypeToLLVM(t.Elem)
		if err != nil {
			return nil, err
		}

		if t.Size < 0 {
			// Dynamic array: {elem*, i64}
			return irtypes.NewStruct(irtypes.NewPointer(elem), irtypes.I64), nil
		}

		return irtypes.NewArray(uint64(t.Size), elem), nil
	case *ast.FuncType:
		// Function values are fat pointers: { fn(i8* env, params...) ret *, i8* }
		// The i8* env carries the closure environment; non-capturing lambdas use null
		llParams := []irtypes.Type{irtypes.I8Ptr} // env is always first

		for _, p := range t.Params {
			pt, err := cg.tinTypeToLLVM(p)
			if err != nil {
				return nil, err
			}

			llParams = append(llParams, pt)
		}

		var ret irtypes.Type = irtypes.Void

		if t.RetType != nil {
			var err error

			ret, err = cg.tinTypeToLLVM(t.RetType)
			if err != nil {
				return nil, err
			}
		}

		// Async fat-fn-ptr: the inner function is the $coro variant which always
		// returns i8* (coroutine handle), regardless of the declared return type.
		if t.IsAsync {
			ret = irtypes.I8Ptr
		}

		ft := irtypes.NewFunc(ret, llParams...)
		ft.Variadic = t.IsVarArgs
		// Fat pointer struct: { fn_ptr*, i8* }

		return irtypes.NewStruct(irtypes.NewPointer(ft), irtypes.I8Ptr), nil
	case *ast.GenericType:
		// Handle known generic types
		if t.Name == "fn" && len(t.TypeParams) >= 1 {
			return cg.tinTypeToLLVM(&ast.FuncType{})
		}
		// Generic trait instantiation (e.g. iter[i64]) -> fat pointer type
		if td, ok := cg.traits[t.Name]; ok {
			instKey := traitImplKey(t)
			typeSubst := map[string]irtypes.Type{}

			for i, tpName := range td.TypeParams {
				if i < len(t.TypeParams) {
					lt, err := cg.tinTypeToLLVM(t.TypeParams[i])
					if err != nil {
						return nil, err
					}

					typeSubst[tpName] = lt
				}
			}

			return cg.buildTraitFatPtrTypeInst(t.Name, instKey, typeSubst)
		}
		// On-demand monomorphization of generic struct (e.g. result[u32]).
		// The type name may be package-qualified (e.g. "sync::Channel"); resolve
		// using the fully-qualified name for the generic struct lookup.
		qualTypeName := cg.typeExprCanonicalKey(&ast.SimpleType{Name: t.Name})
		// Only triggered when the type params are concrete types (not template vars).
		arity := len(t.TypeParams)

		// Generic ADT: `Option[i32]`, `Result[i32, string]`, ... Monomorphize
		// on demand to a concrete data type named <adt>__<part1>__<part2>.
		// Pkg-qualified names (`option::Option`, `result::Result`) resolve
		// through the same template registered under the bare name; ADTs
		// live in a flat namespace today, so qualification is a hint, not
		// a separate type.
		adtLookupName := t.Name

		nameWasQualified := strings.Contains(t.Name, "::")

		if _, ok := cg.dataDecls[adtLookupName]; !ok {
			if idx := strings.LastIndex(adtLookupName, "::"); idx >= 0 {
				bare := adtLookupName[idx+2:]
				if _, ok2 := cg.dataDecls[bare]; ok2 {
					adtLookupName = bare
				}
			}
		}

		if !nameWasQualified && !cg.suppressBareTypeCheck &&
			cg.curScope != nil && !cg.curScope.typeNameVisible(adtLookupName) {
			if _, isAdt := cg.dataDecls[adtLookupName]; isAdt && isUserAdtName(adtLookupName) {
				return nil, fmt.Errorf(
					"type %q is not in scope as a bare name; either qualify (`<pkg>::%s[...]`) or import selectively (`use { %s } from <pkg>`)",
					adtLookupName, adtLookupName, adtLookupName)
			}
		}

		if dd, ok2 := cg.dataDecls[adtLookupName]; ok2 && len(dd.TypeParams) == arity && arity > 0 {
			parts := make([]string, arity)

			isTemplateVar := false

			for i, tp := range t.TypeParams {
				parts[i] = cg.typeExprCanonicalKey(tp)

				for _, tpName := range dd.TypeParams {
					if tpName == parts[i] {
						isTemplateVar = true
					}
				}
			}

			if !isTemplateVar {
				// Use the bare-resolved name so `option::Option[i64]`
				// and `Option[i64]` share the same concrete
				// monomorphization (and the same Some/None ctors).
				concreteName := adtLookupName + "__" + strings.Join(parts, "__")
				if _, done := cg.structTypes[concreteName]; !done {
					if err := cg.monomorphizeDataDecl(dd, t.TypeParams, concreteName); err != nil {
						return nil, err
					}
				} else if concreteDecl, ok := cg.dataDecls[concreteName]; ok {
					// Re-register plain method aliases AND plain-method
					// scope entries in the current scope. The IR
					// functions live in the module (emitted during the
					// first monomorphization), but their scope
					// registrations sit in whatever scope was active at
					// first sight - when that was a foreign package's
					// or a sibling tin-test wrapper's scope, those
					// registrations die with it. Without rewiring, a
					// later `let _ = r.method()` on the same concrete
					// instantiation can't find the method even though
					// the IR func is fine.
					cg.registerPlainMethodAliases(concreteName, concreteDecl.Methods)
					cg.rebindAdtMethodsInScope(concreteName, concreteDecl.Methods)
				}

				cg.dataInstTypeArgs[concreteName] = parts

				if st, ok3 := cg.structTypes[concreteName]; ok3 {
					return st, nil
				}
			}
		}

		if arityMap, isGenericStruct := cg.genericStructsByArity[qualTypeName]; isGenericStruct && arity > 0 {
			if tmplStruct, hasArity := arityMap[arity]; hasArity {
				// Build concrete name from ALL type params joined with __.
				// Use typeExprCanonicalKey (method) to produce canonical part names
				// so that e.g. Future[sync::Unit] and Future[Unit] coalesce to the
				// same concrete LLVM struct type "Future__sync__Unit".
				parts := make([]string, arity)
				for i, tp := range t.TypeParams {
					parts[i] = cg.typeExprCanonicalKey(tp)
				}
				// Detect if any part is a template type-param name of this struct.
				// If so, skip monomorphization - we're inside the template body itself.
				isTemplateVar := false

			outer:
				for _, part := range parts {
					for _, tpName := range tmplStruct.TypeParams {
						if tpName == part {
							isTemplateVar = true

							break outer
						}
					}
				}

				if !isTemplateVar {
					concreteName := qualTypeName + "__" + strings.Join(parts, "__")
					if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
						synthDecl := &ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: qualTypeName, TypeParams: t.TypeParams},
						}

						if synthErr := cg.genTypeDecl(synthDecl); synthErr != nil {
							// Surface the synthesis failure instead of
							// silently returning the pre-registered opaque
							// struct (whose Fields slice would be empty,
							// causing downstream destructure / field-load to
							// produce nonsense IR).  Pre-2026 this swallowed
							// the error which left users with an empty
							// `%Tuple__... = type {}` and an indecipherable
							// "undefined identifier" miles away from the
							// real cause.
							return nil, synthErr
						}
					}

					if st, ok2 := cg.structTypes[concreteName]; ok2 {
						return st, nil
					}
				}
			}
		}

		return cg.resolveSimpleType(t.Name)
	case *ast.UnionTypeExpr:
		// Anonymous tagged union: { i8 tag, [maxSize x i8] payload }
		var maxSize uint64 = 1

		for _, te := range t.Types {
			lt, err := cg.tinTypeToLLVM(te)
			if err != nil {
				return nil, err
			}

			if sz := llvmTypeSize(lt); sz > maxSize {
				maxSize = sz
			}
		}

		return irtypes.NewStruct(irtypes.I8, irtypes.NewArray(maxSize, irtypes.I8)), nil
	case *ast.TupleArrayType:
		// @[T1, T2, ...] resolves to [any] - fat array of any values.
		return irtypes.NewStruct(irtypes.NewPointer(anyFatPtrType()), irtypes.I64), nil
	}

	return irtypes.I64, nil
}

func (cg *CodeGen) resolveSimpleType(name string) (irtypes.Type, error) {
	switch name {
	case "void":
		return irtypes.Void, nil
	case "bool":
		return irtypes.I1, nil
	case "i8":
		return irtypes.I8, nil
	case "i16":
		return irtypes.I16, nil
	case "i32":
		return irtypes.I32, nil
	case "i64":
		return irtypes.I64, nil
	case "u8", "char", "byte":
		return irtypes.I8, nil
	case "u16":
		return irtypes.I16, nil
	case "u32", "uint32":
		return irtypes.I32, nil
	case "u64", "size_t":
		return irtypes.I64, nil
	case "i128":
		return irtypes.I128, nil
	case "u128":
		return irtypes.I128, nil
	case "f32":
		return irtypes.Float, nil
	case "f64":
		return irtypes.Double, nil
	case "f128":
		if cg.useDoubleForF128 {
			return irtypes.Double, nil
		}

		return irtypes.FP128, nil

	// SIMD vector types - 128-bit
	case "u8x16", "i8x16":
		return irtypes.NewVector(16, irtypes.I8), nil
	case "u16x8", "i16x8":
		return irtypes.NewVector(8, irtypes.I16), nil
	case "u32x4", "i32x4":
		return irtypes.NewVector(4, irtypes.I32), nil
	case "u64x2", "i64x2":
		return irtypes.NewVector(2, irtypes.I64), nil
	case "f32x4":
		return irtypes.NewVector(4, irtypes.Float), nil
	case "f64x2":
		return irtypes.NewVector(2, irtypes.Double), nil

	// SIMD vector types - 256-bit
	case "u8x32", "i8x32":
		return irtypes.NewVector(32, irtypes.I8), nil
	case "u16x16", "i16x16":
		return irtypes.NewVector(16, irtypes.I16), nil
	case "u32x8", "i32x8":
		return irtypes.NewVector(8, irtypes.I32), nil
	case "u64x4", "i64x4":
		return irtypes.NewVector(4, irtypes.I64), nil
	case "f32x8":
		return irtypes.NewVector(8, irtypes.Float), nil
	case "f64x4":
		return irtypes.NewVector(4, irtypes.Double), nil

	case "string":
		// fat pointer: {i8*, i64}
		return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64), nil
	case "atom", "__atom":
		// Atoms are represented as %__atom = type { i32 } (CRC32 of name).
		// Both spellings appear: user-facing Tin code uses `atom`, while
		// generic monomorphization records the LLVM struct name (`__atom`)
		// in the typeAliases substitution map. Without `__atom` here, an
		// inferred `t = __atom` substitution falls through to the default
		// i64 case, which silently misaligns generic slice strides for
		// atom arrays.
		return cg.atomType, nil
	case "any":
		// fat pointer: {i8*, i32}  (type-tagged box)
		return anyFatPtrType(), nil
	}
	// Check trait types - represented as fat pointers {i8*, vtable*}
	if _, ok := cg.traits[name]; ok {
		// If the trait belongs to a package, use the qualified instKey so that
		// bare-name references (e.g. "JsonSerializable" inside json.tin) produce
		// the same fat-ptr/vtable LLVM types as qualified references (e.g.
		// "json::JsonSerializable" from call sites outside the package).
		if qualInstKey, ok2 := cg.traitBareToQualInstKey[name]; ok2 {
			fp, err := cg.buildTraitFatPtrTypeInst(name, qualInstKey, nil)
			if err != nil {
				return nil, err
			}

			return fp, nil
		}

		fp, err := cg.buildTraitFatPtrType(name)
		if err != nil {
			return nil, err
		}

		return fp, nil
	}
	// Strip package qualifier (e.g. "sync::AtomicI64" -> "AtomicI64") and retry.
	bareName := name
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		bareName = name[idx+2:]
	}
	// For package-qualified names (e.g. "udp::Conn"), try the canonical
	// pkg__Name key BEFORE falling back to the bare name.  This prevents a
	// same-named type from a later-loaded package (e.g. unix::Conn registered
	// as the bare alias "Conn") from shadowing the intended type.
	if bareName != name {
		pkgQualKey := strings.ReplaceAll(name, "::", "__")
		if st, ok := cg.structTypes[pkgQualKey]; ok {
			return st, nil
		}

		if et, ok := cg.enumTypes[pkgQualKey]; ok {
			return et, nil
		}
	}
	// Also check traits with bare name (e.g. "io::AsyncReader" -> "AsyncReader").
	if bareName != name {
		if _, ok := cg.traits[bareName]; ok {
			qualInstKey := strings.ReplaceAll(name, "::", "__")

			fp, err := cg.buildTraitFatPtrTypeInst(bareName, qualInstKey, nil)
			if err != nil {
				return nil, err
			}

			return fp, nil
		}
	}
	// Check struct types
	if st, ok := cg.structTypes[bareName]; ok {
		return st, nil
	}
	// Check enum types
	if et, ok := cg.enumTypes[bareName]; ok {
		return et, nil
	}
	// Check type aliases
	if alias, ok := cg.typeAliases[bareName]; ok {
		return cg.tinTypeToLLVM(alias)
	}
	// Unknown identifier in a type position. Tin's convention is that
	// real type names are capitalized (Result, Option, Future) and
	// type parameters are lowercase (t, e, K). We only flag the
	// capitalized case as an unknown-type error so generic templates,
	// whose signatures still mention parameter names like `t` before
	// substitution, can lower without complaint. The previous silent
	// fall-through to i64 for capitalized names masked typos and let
	// callers reference types from un-imported packages.
	if bareName != "" && bareName[0] >= 'A' && bareName[0] <= 'Z' {
		// One last shot: if the bare name is in dataDecls, accept it.
		// Predeclare phase may resolve types before genDataDecl sets up
		// the package-qualified entries we usually look at for ADTs.
		if _, ok := cg.dataDecls[bareName]; ok {
			// dataDecls has the template; concrete monomorphization
			// happens via the GenericType branch when type args are
			// supplied. A bare reference to a generic ADT (no args) is
			// rarely meaningful but safe to fall through to i64 for
			// historical compatibility - the eventual user error
			// surfaces at the actual use site.
			return irtypes.I64, nil
		}
		// If the name matches a type parameter of an enclosing generic
		// template (struct, data, fn, or type alias), treat it as a
		// template variable and fall through to i64 so monomorphization
		// can substitute it at usage time. Pre-fix this bare-error
		// branch fired during generic-alias body resolution because the
		// alias's own type parameters (e.g. `T` in
		// `type StrPair[T] = Pair[string, T]`) weren't recognized as
		// template vars yet.
		if cg.curScope != nil && cg.curScope.typeNameVisible(bareName) {
			return irtypes.I64, nil
		}

		return nil, fmt.Errorf("unknown type %q (did you forget to `use` the package that defines it?)", name)
	}

	return irtypes.I64, nil
}

// llvmTypeToTinName returns the canonical tin type name for a given LLVM type.
// Used when building Tuple concrete names from element types.
// For named struct types, uses the LLVM struct name directly.
// Falls back to "any" for types that don't have a canonical mapping.
// demangleStructName converts an LLVM-level struct name back to user-facing
// Tin syntax. The convention used elsewhere in codegen is:
//
//	`pkg__Name`         -> `pkg::Name`
//	`Name__t1__t2`      -> `Name[t1, t2]`        (generic instantiation)
//	`pkg__Name__t1__t2` -> `pkg::Name[t1, t2]`
//
// The package prefix and generic type-args use the same `__` separator, so
// the demangler relies on heuristic: lowercase first segment is treated as
// a package, uppercase-first as the type. Returns "" if the name doesn't
// look mangled (no `__`).
// llvmRawPartForTuple returns the identifier-safe name component to
// stitch into a `Tuple__...` monomorphization key. For named LLVM
// struct types (including their pointer / array wrappers) returns the
// raw mangled name; falls back to llvmTypeToTinName for everything
// else (scalars, anonymous structs).
func llvmRawPartForTuple(t irtypes.Type) string {
	if pt, ok := t.(*irtypes.PointerType); ok {
		return "*" + llvmRawPartForTuple(pt.ElemType)
	}

	if st, ok := t.(*irtypes.StructType); ok {
		if n := st.Name(); n != "" {
			return n
		}
	}

	return llvmTypeToTinName(t)
}

// llvmTypeToTupleSlotTypeExpr is the TypeExpr companion to
// llvmRawPartForTuple: builds a TypeExpr that uses raw mangled names
// for named structs so the synthesized Tuple__... type's slot
// resolves directly to the same struct via structTypes lookup. The
// generic structural reconstructor at llvmTypeToTinTypeExprStructural
// can't do this universally - its callers downstream of method
// monomorphization need the demangled form to drive nested generic
// substitution - so the raw form is local to tuple plumbing.
func llvmTypeToTupleSlotTypeExpr(t irtypes.Type) ast.TypeExpr {
	if pt, ok := t.(*irtypes.PointerType); ok {
		return &ast.PointerType{Elem: llvmTypeToTupleSlotTypeExpr(pt.ElemType)}
	}

	if st, ok := t.(*irtypes.StructType); ok {
		// Trait fat-ptr {i8*, vtable_struct*} -- delegate to
		// llvmTypeToTinTypeExprStructural's iface branch so the
		// `errors__Err_iface` shape demangles to `errors::Err`.
		if len(st.Fields) == 2 && st.Fields[0] == irtypes.I8Ptr {
			if pt2, ok2 := st.Fields[1].(*irtypes.PointerType); ok2 {
				if vst, ok3 := pt2.ElemType.(*irtypes.StructType); ok3 {
					if vname := vst.Name(); strings.HasSuffix(vname, "_vtable") {
						return llvmTypeToTinTypeExprStructural(t)
					}
				}
			}
		}

		if n := st.Name(); n != "" {
			return &ast.SimpleType{Name: n}
		}
	}

	return llvmTypeToTinTypeExprStructural(t)
}

// isUserAdtName reports whether name looks like a user-written ADT
// reference (capitalized, no underscores marking a compiler-
// synthesized monomorphization). The visibility check only fires for
// such names - internal monomorphizations like `Option__i64` or
// underscore-prefixed helpers are exempt.
func isUserAdtName(name string) bool {
	if name == "" {
		return false
	}

	if strings.Contains(name, "__") {
		return false
	}

	if name[0] == '_' {
		return false
	}

	if name[0] < 'A' || name[0] > 'Z' {
		return false
	}

	return true
}

func demangleStructName(s string) string {
	if !strings.Contains(s, "__") {
		return ""
	}

	parts := strings.Split(s, "__")
	if len(parts) < 2 {
		return ""
	}

	// First lowercase-led segment is a package qualifier.
	pkg := ""

	if startsLower(parts[0]) {
		pkg = parts[0]
		parts = parts[1:]
	}

	if len(parts) == 0 {
		return ""
	}

	base := parts[0]
	args := parts[1:]

	var sb strings.Builder

	if pkg != "" {
		sb.WriteString(pkg)
		sb.WriteString("::")
	}

	sb.WriteString(base)

	if len(args) > 0 {
		sb.WriteByte('[')

		for i, a := range args {
			if i > 0 {
				sb.WriteString(", ")
			}

			sb.WriteString(a)
		}

		sb.WriteByte(']')
	}

	return sb.String()
}

func startsLower(s string) bool {
	if s == "" {
		return false
	}

	c := s[0]

	return c >= 'a' && c <= 'z'
}

// llvmTypeToTinTypeExprStructural reconstructs a TypeExpr from an
// LLVM type, preserving pointer / array / string / trait-iface shape.
// Used by genTupleLit to synthesize a monomorphization decl that
// round-trips through the type-resolver -- the flat-name form
// produced by llvmTypeToTinName collapses pointers into a SimpleType
// whose Name starts with `*`, which the AST treats as a literal
// identifier and downstream type lookups silently fall back to i64.
func llvmTypeToTinTypeExprStructural(t irtypes.Type) ast.TypeExpr {
	if pt, ok := t.(*irtypes.PointerType); ok {
		return &ast.PointerType{Elem: llvmTypeToTinTypeExprStructural(pt.ElemType)}
	}

	if st, ok := t.(*irtypes.StructType); ok && len(st.Fields) == 2 {
		// Trait fat-ptr {i8*, vtable_struct*} -- demangle the
		// vtable's struct name (`<pkg>__<Trait>_vtable`) into the
		// user-visible `<pkg>::<Trait>` so the synthesized
		// monomorphization re-resolves to the right iface type
		// instead of falling through to i64.
		if st.Fields[0] == irtypes.I8Ptr {
			if pt, ok2 := st.Fields[1].(*irtypes.PointerType); ok2 {
				if vst, ok3 := pt.ElemType.(*irtypes.StructType); ok3 {
					vname := vst.Name()
					if strings.HasSuffix(vname, "_vtable") {
						bare := strings.TrimSuffix(vname, "_vtable")

						if i := strings.LastIndex(bare, "__"); i >= 0 {
							bare = bare[:i] + "::" + bare[i+2:]
						}

						return &ast.SimpleType{Name: bare}
					}
				}
			}
		}
		// Fat-pointer arrays / strings: {ElemPtr, i64}.  We cannot
		// distinguish `[u8]` from `string` at the LLVM level (both
		// are `{i8*, i64}`); both resolve to the same LLVM type
		// downstream so the choice is cosmetic.  We pick `string`
		// because it is the more common appearance for this shape
		// in real code.
		if pt, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 && st.Fields[1].Equal(irtypes.I64) {
			if pt.ElemType.Equal(irtypes.I8) {
				return &ast.SimpleType{Name: "string"}
			}

			return &ast.ArrayType{Elem: llvmTypeToTinTypeExprStructural(pt.ElemType), Size: -1}
		}
	}

	return &ast.SimpleType{Name: llvmTypeToTinName(t)}
}

func llvmTypeToTinName(t irtypes.Type) string {
	switch t {
	case irtypes.I1:
		return "bool"
	case irtypes.I8:
		return "i8"
	case irtypes.I16:
		return "i16"
	case irtypes.I32:
		return "i32"
	case irtypes.I64:
		return "i64"
	case irtypes.Float:
		return "f32"
	case irtypes.Double:
		return "f64"
	}

	if st, ok := t.(*irtypes.StructType); ok {
		if n := st.Name(); n != "" {
			// Demangle package-qualified or generic-instantiated names so the
			// REPL can re-parse them as Tin types: `sync__Channel__string`
			// becomes `sync::Channel[string]`. Without this, the next REPL
			// cell sees a bare identifier `Channel__string` that the parser
			// accepts but no method/trait can be resolved against.
			if demangled := demangleStructName(n); demangled != "" {
				return demangled
			}

			return n
		}
		// Anonymous struct - detect by shape:
		// {i8*, i64}  = string
		// {i32, i8*}  = any
		if len(st.Fields) == 2 {
			if st.Fields[0].Equal(irtypes.I8Ptr) && st.Fields[1].Equal(irtypes.I64) {
				return "string"
			}

			if st.Fields[0].Equal(irtypes.I32) && st.Fields[1].Equal(irtypes.I8Ptr) {
				return "any"
			}
			// Fat array pointer: anonymous {ElemType*, i64}.
			if pt, ok := st.Fields[0].(*irtypes.PointerType); ok && st.Fields[1].Equal(irtypes.I64) {
				elem := llvmTypeToTinName(pt.ElemType)
				if elem != "" && elem != "any" {
					return "[" + elem + "]"
				}
			}
		}
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		inner := llvmTypeToTinName(pt.ElemType)

		return "*" + inner
	}

	return "any"
}

// stringFatPtrType returns the {i8*, i64} type used for tin strings.
func stringFatPtrType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
}

// anyFatPtrType returns the {i32, i8*} type used for tin `any` values.
// Field 0: i32  - type tag (0=i64, 1=f64, 2=string, 3=bool, 4=ptr, 5+=user).
// Field 1: i8*  - pointer to the boxed value on the stack or heap.
// The type tag is always field 0 so that any pointer can be bitcast to
// *any and the type read from field 0.
func anyFatPtrType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I32, irtypes.I8Ptr)
}

const (
	anyTagInt    = int32(0)
	anyTagFloat  = int32(1)
	anyTagString = int32(2)
	anyTagBool   = int32(3)
	anyTagPtr    = int32(4)
	anyTagFn     = int32(5) // closure / fat function pointer
)

// Type size helpers

// llvmTypeSize returns the byte size of an LLVM type (approximate, for data
// type payload sizing on a 64-bit target).
func llvmTypeSize(t irtypes.Type) uint64 {
	sz, _ := llvmTypeSizeAlign(t)

	return sz
}

// llvmTypeSizeAlign returns (size, alignment) for t on a 64-bit x86 target.
// It accounts for alignment padding so that malloc receives the correct size.
func llvmTypeSizeAlign(t irtypes.Type) (uint64, uint64) {
	switch ty := t.(type) {
	case *irtypes.IntType:
		b := (ty.BitSize + 7) / 8

		return b, b
	case *irtypes.FloatType:
		switch ty.Kind { //nolint:exhaustive // X86_FP80/PPC_FP128 are not used by tin
		case irtypes.FloatKindHalf:
			return 2, 2
		case irtypes.FloatKindFloat:
			return 4, 4
		case irtypes.FloatKindDouble:
			return 8, 8
		case irtypes.FloatKindFP128:
			return 16, 16
		default:
			return 8, 8
		}
	case *irtypes.PointerType:
		return 8, 8
	case *irtypes.StructType:
		isPacked := ty.Packed

		var offset, maxAlign uint64

		for _, f := range ty.Fields {
			fsz, fal := llvmTypeSizeAlign(f)
			if fal > maxAlign {
				maxAlign = fal
			}
			// Align current offset to field's alignment (skip for packed structs).
			if fal > 0 && !isPacked {
				offset = (offset + fal - 1) &^ (fal - 1)
			}

			offset += fsz
		}

		if maxAlign == 0 {
			maxAlign = 1
		}

		if isPacked {
			// Packed structs have no trailing alignment padding; alignment is 1.
			maxAlign = 1
		} else {
			// Pad struct to its own alignment.
			offset = (offset + maxAlign - 1) &^ (maxAlign - 1)
		}

		return offset, maxAlign
	case *irtypes.ArrayType:
		esz, eal := llvmTypeSizeAlign(ty.ElemType)

		return ty.Len * esz, eal
	case *irtypes.VectorType:
		// SIMD vector: contiguous, no padding. Alignment = total size.
		esz, _ := llvmTypeSizeAlign(ty.ElemType)
		size := ty.Len * esz

		return size, size
	default:
		return 8, 8
	}
}

// Type query helpers

// isFatPtrType returns true if t is a two-field struct whose first field
// is a pointer - i.e., a Tin fat-pointer (string, array, etc.).
func isFatPtrType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}

	_, isPtr := st.Fields[0].(*irtypes.PointerType)

	return isPtr && irtypes.IsInt(st.Fields[1])
}

// isStringType returns true if t is the tin string fat-pointer type {i8*, i64}.
// Named structs (user-defined) are never fat-pointers.
func isStringType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}

	return st.Fields[0] == irtypes.I8Ptr && st.Fields[1].Equal(irtypes.I64)
}

// isAnyType returns true if t is the tin `any` fat-pointer type {i32, i8*}.
// Named structs (user-defined) are never fat-pointers.
func isAnyType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}

	return st.Fields[0].Equal(irtypes.I32) && st.Fields[1] == irtypes.I8Ptr
}

// isFatArrayPtr returns true for anonymous {T*, i64} fat array pointer structs.
// Named structs (user-defined) are excluded to avoid false matches with
// structs that embed vtable pointers as their first field.
func isFatArrayPtr(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}

	if !irtypes.IsInt(st.Fields[1]) {
		return false
	}

	_, ok = st.Fields[0].(*irtypes.PointerType)

	return ok
}

// isFatFnPtr returns true when t is a closure fat pointer { fn(i8*,...)*, i8* }.
func isFatFnPtr(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}

	if st.Fields[1] != irtypes.I8Ptr {
		return false
	}

	pt, ok := st.Fields[0].(*irtypes.PointerType)
	if !ok {
		return false
	}

	_, ok = pt.ElemType.(*irtypes.FuncType)

	return ok
}

// isAsyncFatFnPtr returns true when t is an async closure fat pointer
// { fn(i8*, params...) i8* *, i8* } - the inner function returns i8*
// (coroutine handle), as produced for fn{#async}(...) type expressions.
func isAsyncFatFnPtr(t irtypes.Type) bool {
	if !isFatFnPtr(t) {
		return false
	}

	st := t.(*irtypes.StructType)
	fnPtr := st.Fields[0].(*irtypes.PointerType)
	ft := fnPtr.ElemType.(*irtypes.FuncType)

	return ft.RetType != nil && ft.RetType.Equal(irtypes.I8Ptr)
}

// isAtomType returns true if t is the %__atom named struct type { i32 }.
func isAtomType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)

	return ok && st.Name() == "__atom"
}

// isStructType returns true if t is a named LLVM struct type (excluding the
// special any, atom, and fat-pointer types so those use their own coercion paths).
func isVectorType(t irtypes.Type) bool {
	_, ok := t.(*irtypes.VectorType)

	return ok
}

func isStructType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() == "" || st.Name() == "__atom" {
		return false
	}
	// Exclude any, string, and dynamic-array fat-pointers.
	if isAnyType(t) || isFatPtrType(t) || isFatArrayPtr(t) || isFatFnPtr(t) {
		return false
	}

	return true
}

// typeNameOf returns the type name for a struct or empty string.
func (cg *CodeGen) typeNameOf(t irtypes.Type) string {
	if st, ok := t.(*irtypes.StructType); ok {
		return st.Name()
	}

	return ""
}

// vtableOffset returns the number of vtable pointer fields prepended to the
// LLVM layout of structName (0 for structs that implement no traits).
func (cg *CodeGen) vtableOffset(structName string) int {
	return len(cg.structVtableOrder[structName])
}

// userFieldOffset returns the LLVM field index where user fields start,
// accounting for the i32 type_id and vtable pointer fields.
func (cg *CodeGen) userFieldOffset(structName string) int {
	return 1 + cg.vtableOffset(structName)
}

// cDataPtrIndex returns the LLVM field index of the i8* c_data_ptr field
// for cLayoutStructs (= userFieldOffset). Returns -1 for non-cLayout structs.
func (cg *CodeGen) cDataPtrIndex(structName string) int {
	if !cg.cLayoutStructs[structName] {
		return -1
	}

	return cg.userFieldOffset(structName)
}

// fieldIndex returns the LLVM struct field index for a named user field.
// For regular structs: returns userFieldOffset + i (wrapper GEP index).
// For cLayoutStructs: returns the native field index (0-based within %S.native).
//
//	Callers that need a GEP must use emitFieldGEP/emitCLayoutFieldPtr instead.
//
// Returns -1 if not found.
func (cg *CodeGen) fieldIndex(structName, fieldName string) int {
	names, ok := cg.structFields[structName]
	if !ok {
		return -1
	}

	for i, n := range names {
		if n == fieldName {
			if cg.cLayoutStructs[structName] {
				return i // native 0-based index; use emitCLayoutFieldPtr for GEP
			}

			return cg.userFieldOffset(structName) + i
		}
	}

	return -1
}

// nativeFieldIndex returns the 0-based index of a named field within %S.native.
// Used for GEP through c_data_ptr in cLayoutStructs.
// Returns -1 if not found.
func (cg *CodeGen) nativeFieldIndex(structName, fieldName string) int {
	names, ok := cg.structFields[structName]
	if !ok {
		return -1
	}

	for i, n := range names {
		if n == fieldName {
			return i
		}
	}

	return -1
}

// resolveTypeWithSubst converts a TypeExpr to LLVM type, substituting any
// type parameter names found in subst.
func (cg *CodeGen) resolveTypeWithSubst(te ast.TypeExpr, subst map[string]irtypes.Type) (irtypes.Type, error) {
	if len(subst) == 0 {
		return cg.tinTypeToLLVM(te)
	}

	if st, ok := te.(*ast.SimpleType); ok {
		if lt, ok2 := subst[st.Name]; ok2 {
			return lt, nil
		}
	}

	return cg.tinTypeToLLVM(te)
}

// llvmElemByteSize returns the size in bytes of a scalar LLVM type, or 0 for
// types whose size is unknown at compile time (structs, fat pointers, etc.).
// Used to compute the byte length for llvm.memset when zero-initializing a
// fixed-size array alloca without generating a huge aggregate-value store.
func llvmElemByteSize(t irtypes.Type) int64 {
	if it, ok := t.(*irtypes.IntType); ok {
		return int64((it.BitSize + 7) / 8)
	}

	switch t {
	case irtypes.Float:
		return 4
	case irtypes.Double:
		return 8
	}

	if _, ok := t.(*irtypes.PointerType); ok {
		return 8 // 64-bit pointers
	}

	return 0
}

// typeExprName returns a short string name for a TypeExpr (used in instance keys).
