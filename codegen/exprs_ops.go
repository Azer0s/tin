package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genScopeAccess(block *ir.Block, e *ast.ScopeAccess) (value.Value, error) {
	// e.g. weather.sunny -> look up "weather.sunny" in enum registry.
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
	// For 3+ segment paths like std::math::floor, try dropping the first segment:
	// "math.floor" after failing "std.math.floor".
	if len(e.Path) >= 3 {
		tail := strings.Join(e.Path[1:], ".")

		entry, ok = cg.curScope.lookup(tail)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)

				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}

			return entry.val, nil
		}
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
	// Try struct static method: TypeName::method or TypeName[T]::method
	// Scope key is "TypeName_method" (set when struct is compiled with static methods).
	if len(e.Path) >= 2 {
		baseName := e.Path[0]

		typeParamStr := ""
		if i := strings.Index(baseName, "["); i >= 0 {
			typeParamStr = strings.TrimSuffix(baseName[i+1:], "]")
			baseName = baseName[:i]
		}

		staticKey := baseName + "_" + last

		entry, ok = cg.curScope.lookup(staticKey)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)

				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}

			return entry.val, nil
		}
		// On-demand monomorphization: if baseName is a generic struct template and
		// we have a concrete type param, monomorphize now and retry.
		if typeParamStr != "" {
			if _, isGeneric := cg.genericStructsByArity[baseName]; isGeneric {
				// Resolve typeParamStr through type aliases (e.g. "r" → "string" inside a
				// generic method body where cg.typeAliases["r"] = string).
				resolvedTypeParam := typeParamStr
				if alias, ok2 := cg.typeAliases[typeParamStr]; ok2 {
					if simple, ok3 := alias.(*ast.SimpleType); ok3 {
						resolvedTypeParam = simple.Name
					}
				}

				concreteName := baseName + "__" + resolvedTypeParam
				if _, alreadyDone := cg.structTypes[concreteName]; !alreadyDone {
					typeParamTE := parseTypeParamStr(resolvedTypeParam)
					synthDecl := &ast.TypeDecl{
						Name: concreteName,
						Type: &ast.GenericType{Name: baseName, TypeParams: []ast.TypeExpr{typeParamTE}},
					}
					_ = cg.genTypeDecl(synthDecl)
				}

				concreteStaticKey := concreteName + "_" + last

				entry, ok = cg.curScope.lookup(concreteStaticKey)
				if ok {
					if entry.isAlloc {
						ptrType := entry.val.Type().(*irtypes.PointerType)

						return block.NewLoad(ptrType.ElemType, entry.val), nil
					}

					return entry.val, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("undefined: %s", strings.Join(e.Path, "::"))
}

// exprToTypeParamKey converts a parsed expression node into a canonical type-key
// string (same format as typeExprCanonicalKey). Used when generic type params are
// parsed as expressions (e.g. Channel[*T] -> IndexExpr with UnaryExpr{"*","T"}).
// Returns "" when the expression cannot be interpreted as a type.
func (cg *CodeGen) exprToTypeParamKey(node ast.Node) string {
	switch n := node.(type) {
	case *ast.Identifier:
		return n.Name
	case *ast.UnaryExpr:
		if n.Op == "*" {
			inner := cg.exprToTypeParamKey(n.Expr)
			if inner != "" {
				return "*" + inner
			}
		}
	case *ast.DerefExpr:
		// In expression context, *T is parsed as DerefExpr{Expr: T}.
		// When T is used as a type parameter (e.g. Channel[*counter_t]),
		// convert back to the canonical pointer type key "*counter_t".
		inner := cg.exprToTypeParamKey(n.Expr)
		if inner != "" {
			return "*" + inner
		}
	case *ast.ArrayLit:
		// []T represented as an empty array literal of one element - best-effort.
	case *ast.ScopeAccess:
		return strings.Join(n.Path, "::")
	}

	return ""
}

// chanElemTypeFromName extracts the channel element LLVM type from a concrete
// channel struct name, e.g. "sync__Channel__*counter_t" -> *%counter_t.
// Returns nil if the name doesn't follow the Channel__<elemKey> pattern or
// if the element type cannot be resolved.
func (cg *CodeGen) chanElemTypeFromName(structName string) irtypes.Type {
	const sep = "Channel__"

	idx := strings.LastIndex(structName, sep)
	if idx < 0 {
		return nil
	}

	elemKey := structName[idx+len(sep):]
	if elemKey == "" {
		return nil
	}

	te := parseTypeParamStr(elemKey)

	lt, err := cg.tinTypeToLLVM(te)
	if err != nil || lt == nil {
		return nil
	}

	return lt
}

// parseTypeParamStr converts a canonical type-key string (as produced by
// typeExprCanonicalKey) back into an ast.TypeExpr for use in synthetic decls.
// Examples:
//
//	"*foo"  -> &ast.PointerType{Elem: &ast.SimpleType{Name: "foo"}}
//	"[]foo" -> &ast.ArrayType{Elem: &ast.SimpleType{Name: "foo"}, Size: -1}
//	"foo"   -> &ast.SimpleType{Name: "foo"}
//
// Handles recursive nesting (e.g. "*[]foo" -> PointerType{ArrayType{...}}).
func parseTypeParamStr(s string) ast.TypeExpr {
	if strings.HasPrefix(s, "*") {
		return &ast.PointerType{Elem: parseTypeParamStr(s[1:])}
	}

	if strings.HasPrefix(s, "[]") {
		return &ast.ArrayType{Elem: parseTypeParamStr(s[2:]), Size: -1}
	}

	return &ast.SimpleType{Name: s}
}

// tryResolveStructTypeName tries to interpret expr as a struct (or generic struct)
// type name, returning (structName, typeArgStr). structName is the base struct
// name registered in cg.structTypes or cg.genericStructsByArity; typeArgStr is the
// concrete type parameter (e.g. "i64" for Channel[i64]) or "" for non-generic.
// Returns ("", "") when expr does not resolve to a known struct type.
func (cg *CodeGen) tryResolveStructTypeName(expr ast.Node) (string, string) {
	switch e := expr.(type) {
	case *ast.Identifier:
		if _, ok := cg.structTypes[e.Name]; ok {
			return e.Name, ""
		}

		if _, ok := cg.genericStructsByArity[e.Name]; ok {
			return e.Name, ""
		}
		// Check type alias.
		if ta, ok := cg.typeAliases[e.Name]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.ScopeAccess:
		// pkg.Type or pkg::Type - resolve via type alias.
		key := strings.Join(e.Path, ".")
		if ta, ok := cg.typeAliases[key]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}

		key2 := strings.Join(e.Path, "::")
		if ta, ok := cg.typeAliases[key2]; ok {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if _, ok3 := cg.structTypes[st.Name]; ok3 {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.IndexExpr:
		// Generic instantiation: Channel[i64] or sync::Channel[i64]
		base, _ := cg.tryResolveStructTypeName(e.Expr)
		if base == "" {
			return "", ""
		}

		if typeArgID, ok := e.Index.(*ast.Identifier); ok {
			return base, typeArgID.Name
		}

		// Non-identifier index (e.g. *counter_t -> DerefExpr of "counter_t" in
		// expression context). Convert to canonical type-key string so Channel[*T]
		// resolves correctly.
		if typeArgStr := cg.exprToTypeParamKey(e.Index); typeArgStr != "" {
			return base, typeArgStr
		}

		return base, ""
	}

	return "", ""
}

// isStaticMethodIR returns true when fn's first parameter is NOT a receiver
// of structName's type (meaning the method is a static/constructor method).
func (cg *CodeGen) isStaticMethodIR(fn *ir.Func, structName string) bool {
	if len(fn.Sig.Params) == 0 {
		return true
	}

	st, ok := cg.structTypes[structName]
	if !ok {
		return true
	}

	first := fn.Sig.Params[0]
	if first.Equal(st) {
		return false // instance method: first param is the struct value
	}

	if pt, isPtr := first.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(st) {
		return false // pointer receiver
	}

	return true
}

func (cg *CodeGen) genArrayLit(block *ir.Block, e *ast.ArrayLit) (value.Value, error) {
	return cg.genArrayLitWithElemType(block, e, nil)
}

// genArrayLitWithElemType generates an array literal, optionally coercing each element to targetElemType.
// Used when the declared array type is known (e.g. let fns [fn{#async}(i64) i64] = [double]).
func (cg *CodeGen) genArrayLitWithElemType(block *ir.Block, e *ast.ArrayLit, targetElemType irtypes.Type) (value.Value, error) {
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
		v, err := cg.genArgWithTargetType(block, elem, targetElemType)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if targetElemType != nil {
			v = cg.coerce(block, v, targetElemType)
		}

		vals[i] = v
	}

	elemType := vals[0].Type()

	if targetElemType != nil {
		elemType = targetElemType
	}

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
		// ARC: retain copy expressions (identifiers, field accesses, etc.) so that
		// both the array and the original owner hold an independent RC reference.
		// Temporaries (call results, literals) are already owned by the array at RC=1.
		if isRCTrackedType(elemType) && isCopyExpr(e.Elems[i]) {
			cg.emitRetain(block, v)
		}
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

// genArrayFillLit generates code for [value; count] fill array literals.
// Returns a heap-allocated fat-ptr {T*, i64} dynamic array of `count` copies of `value`.
func (cg *CodeGen) genArrayFillLit(block *ir.Block, e *ast.ArrayFillLit) (value.Value, error) {
	v, err := cg.genExpr(block, e.Value)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	n := int64(e.Count)
	elemType := v.Type()

	// Compute allocation size.
	nullPtr := constant.NewNull(irtypes.NewPointer(elemType))
	sizeGep := block.NewGetElementPtr(elemType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSize := block.NewMul(elemSize, constant.NewInt(irtypes.I64, n))

	mallocI8 := block.NewCall(cg.ensureRCAlloc(), totalSize)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(elemType))

	// Store value into each element. For zero integer/byte fills we could use
	// memset, but storing is correct for all types.
	for i := int64(0); i < n; i++ {
		gep := block.NewGetElementPtr(elemType, dataPtr, constant.NewInt(irtypes.I64, i))
		block.NewStore(v, gep)

		if isRCTrackedType(elemType) {
			cg.emitRetain(block, v)
		}
	}

	// Return fat pointer {T*, i64}.
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
	typeName := e.TypeName
	// Generic struct literal with explicit type args: Name[T1, T2]{...}
	// Monomorphize to the concrete name Name__T1__T2 (resolving type aliases).
	if len(e.TypeArgs) > 0 {
		parts := make([]string, len(e.TypeArgs))

		resolvedTypeArgs := make([]ast.TypeExpr, len(e.TypeArgs))
		for i, ta := range e.TypeArgs {
			// Use typeExprCanonicalKey for naming: correctly distinguishes [byte] from string
			// (both have the same {i8*,i64} LLVM layout, so llvmTypeToTinName can't tell them apart).
			parts[i] = cg.typeExprCanonicalKey(ta)
			// For synthDecl: resolve SimpleType aliases to their actual AST type so that
			// genTypeDecl substitutes the real type (e.g. ArrayType) into struct fields.
			resolved := ta
			if st2, ok2 := ta.(*ast.SimpleType); ok2 {
				if alias, ok3 := cg.typeAliases[st2.Name]; ok3 {
					resolved = alias
				}
			}

			resolvedTypeArgs[i] = resolved
		}

		concreteName := typeName + "__" + strings.Join(parts, "__")
		if _, done := cg.structTypes[concreteName]; !done {
			synthDecl := &ast.TypeDecl{
				Name: concreteName,
				Type: &ast.GenericType{Name: typeName, TypeParams: resolvedTypeArgs},
			}
			if err := cg.genTypeDecl(synthDecl); err != nil {
				return nil, err
			}
		}

		typeName = concreteName
		// Rewrite the StructLit to use the concrete name for the rest of genStructLit.
		e = &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
	}
	// Resolve through type aliases to the canonical struct name
	// (e.g., bare "Mutex" -> "sync__Mutex" after canonical naming).
	if _, exists := cg.structTypes[typeName]; !exists {
		if alias, ok2 := cg.typeAliases[typeName]; ok2 {
			if simple, ok3 := alias.(*ast.SimpleType); ok3 {
				typeName = simple.Name
				e = &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
			}
		}
	}

	// SIMD vector literal: f32x4{ 1.0, 2.0, 3.0, 4.0 }
	if resolvedVecType, err2 := cg.tinTypeToLLVM(&ast.SimpleType{Name: typeName}); err2 == nil {
		if vecType, isVec := resolvedVecType.(*irtypes.VectorType); isVec {
			return cg.genSIMDLit(block, e, vecType)
		}
	}

	st, ok := cg.structTypes[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown struct type: %s", typeName)
	}

	// cLayoutStructs need special handling: the wrapper type has no user fields.
	// We stack-allocate both the wrapper and native data, then wire c_data_ptr.
	if cg.cLayoutStructs[typeName] {
		return cg.genCLayoutStructLit(block, e, typeName, st)
	}

	alloca := block.NewAlloca(st)
	// Zero-initialize the struct so unspecified fields start as 0/nil.
	// Without this, ARC retain/release on array or string fields (which are
	// fat-ptrs) would operate on garbage stack values and segfault.
	block.NewStore(constant.NewZeroInitializer(st), alloca)

	fieldNames := cg.structFields[e.TypeName]
	userOff := cg.userFieldOffset(e.TypeName)

	// Initialize the leading i32 type_id field (index 0).
	if typeID, ok := cg.structTypeIDs[e.TypeName]; ok {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	// Initialize embedded vtable pointer fields (indices 1 ... vtableOff).
	for i, instKey := range cg.structVtableOrder[e.TypeName] {
		vtableKey := e.TypeName + "__" + instKey
		if vg, ok := cg.traitVtableGlobals[vtableKey]; ok {
			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(1+i)))
			block.NewStore(vg, gep)
		}
	}

	weakSet := cg.structWeakFields[e.TypeName]

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
			// ARC: retain RC-tracked values that are copied from existing owners.
			// Weak fields are non-owning: skip retain so only one owner counts.
			fieldName := ""
			if i < len(fieldNames) {
				fieldName = fieldNames[i]
			}

			if isCopyExpr(v) && !weakSet[fieldName] {
				cg.emitRetain(block, val)
			}
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
			// ARC: retain RC-tracked values copied from existing owners.
			// Weak fields are non-owning: skip retain.
			if isCopyExpr(f.Value) && !weakSet[f.Name] {
				cg.emitRetain(block, val)
			}
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

	// Call chained trait init methods (for traits that also define fn init).
	for _, traitInitFn := range cg.traitChainedInits[e.TypeName] {
		args := cg.adaptArgs(block, []value.Value{result}, traitInitFn.Sig)
		block.NewCall(traitInitFn, args...)
	}

	return result, nil
}

// genSIMDLit handles SIMD vector literals: f32x4{ 1.0, 2.0, 3.0, 4.0 }.
// Emits a sequence of insertelement instructions starting from undef.
func (cg *CodeGen) genSIMDLit(block *ir.Block, e *ast.StructLit, vecType *irtypes.VectorType) (value.Value, error) {
	var v value.Value = constant.NewUndef(vecType)

	args := e.Positional
	if len(args) == 0 {
		// Named fields not supported for SIMD; return zero vector.
		return constant.NewZeroInitializer(vecType), nil
	}

	for i, arg := range args {
		if uint64(i) >= vecType.Len {
			return nil, fmt.Errorf("too many elements for %s (got %d, want %d)",
				llvmTypeName(vecType), len(args), vecType.Len)
		}

		elem, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}

		elem = cg.coerce(block, elem, vecType.ElemType)
		v = block.NewInsertElement(v, elem, constant.NewInt(irtypes.I32, int64(i)))
	}

	return v, nil
}

// genCLayoutStructLit handles struct literal creation for cLayoutStructs.
// Stack-allocates both a wrapper and a native-layout data region, wires
// c_data_ptr to point at the native alloca, stores fields, then returns the
// wrapper struct value. c_data_ptr stays valid for the lifetime of the function.
func (cg *CodeGen) genCLayoutStructLit(block *ir.Block, e *ast.StructLit, typeName string, st *irtypes.StructType) (value.Value, error) {
	nativeSt := cg.nativeStructTypes[typeName]
	if nativeSt == nil {
		return nil, fmt.Errorf("cLayoutStruct %s: missing native type", typeName)
	}

	// Stack-allocate wrapper and native data.
	wrapperAlloca := block.NewAlloca(st)
	nativeAlloca := block.NewAlloca(nativeSt)
	block.NewStore(constant.NewZeroInitializer(st), wrapperAlloca)
	block.NewStore(constant.NewZeroInitializer(nativeSt), nativeAlloca)

	// Set type_id.
	if typeID, ok := cg.structTypeIDs[typeName]; ok {
		typeIDGep := block.NewGetElementPtr(st, wrapperAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}
	// Zero vtable pointer fields.
	offset := cg.userFieldOffset(typeName)
	for v := int64(1); v < int64(offset); v++ {
		vtGep := block.NewGetElementPtr(st, wrapperAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
		fieldType := st.Fields[v]
		block.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
	}
	// Wire vtable pointers (if trait implementations exist).
	for i, instKey := range cg.structVtableOrder[typeName] {
		vtableKey := typeName + "__" + instKey
		if vg, ok := cg.traitVtableGlobals[vtableKey]; ok {
			gep := block.NewGetElementPtr(st, wrapperAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(1+i)))
			block.NewStore(vg, gep)
		}
	}
	// Set c_data_ptr = pointer to native alloca.
	cDataIdx := int64(cg.cDataPtrIndex(typeName))
	cDataGep := block.NewGetElementPtr(st, wrapperAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
	nativeI8 := block.NewBitCast(nativeAlloca, irtypes.I8Ptr)
	block.NewStore(nativeI8, cDataGep)

	// Store user fields into native alloca via emitCLayoutFieldPtr.
	fieldNames := cg.structFields[typeName]
	weakSet := cg.structWeakFields[typeName]

	storeField := func(rawIdx int, valExpr ast.Node, fieldName string) error {
		if rawIdx < 0 || rawIdx >= len(nativeSt.Fields) {
			return nil
		}

		val, err := cg.genExpr(block, valExpr)
		if err != nil {
			return err
		}

		gep := cg.emitCLayoutFieldPtr(block, wrapperAlloca, typeName, rawIdx)
		val = cg.coerce(block, val, nativeSt.Fields[rawIdx])
		block.NewStore(val, gep)

		if isCopyExpr(valExpr) && !weakSet[fieldName] {
			cg.emitRetain(block, val)
		}

		return nil
	}

	if len(e.Positional) > 0 {
		for i, v := range e.Positional {
			fn := ""
			if i < len(fieldNames) {
				fn = fieldNames[i]
			}

			if err := storeField(i, v, fn); err != nil {
				return nil, err
			}
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

			if err := storeField(rawIdx, f.Value, f.Name); err != nil {
				return nil, err
			}
		}
	}

	return block.NewLoad(st, wrapperAlloca), nil
}

// genTupleLit generates code for a tuple literal (e1, e2, ...).
// expectedType is the LLVM struct type of the target if known (e.g. from a
// variable declaration or return type); pass nil to infer from element types.
func (cg *CodeGen) genTupleLit(block *ir.Block, tup *ast.TupleLit, expectedType irtypes.Type) (value.Value, error) {
	if len(tup.Elems) < 2 {
		return nil, fmt.Errorf("tuple literal requires at least 2 elements")
	}

	// Evaluate all element expressions.
	vals := make([]value.Value, len(tup.Elems))
	for i, elem := range tup.Elems {
		v, err := cg.genExpr(block, elem)
		if err != nil {
			return nil, err
		}

		vals[i] = v
	}

	// Determine concrete Tuple struct name.
	// If expectedType is a known named struct, use it directly.
	var concreteName string

	if expectedType != nil {
		if st, ok := expectedType.(*irtypes.StructType); ok {
			if n := st.Name(); n != "" {
				if _, known := cg.structTypes[n]; known {
					concreteName = n
				}
			}
		}
	}

	if concreteName == "" {
		// Infer from element LLVM types.
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = llvmTypeToTinName(v.Type())
		}

		concreteName = "Tuple__" + strings.Join(parts, "__")
		// Trigger monomorphization for this concrete name.
		if _, done := cg.structTypes[concreteName]; !done {
			typeParams := make([]ast.TypeExpr, len(parts))
			for i, p := range parts {
				typeParams[i] = &ast.SimpleType{Name: p}
			}

			synthDecl := &ast.TypeDecl{
				Name: concreteName,
				Type: &ast.GenericType{Name: "Tuple", TypeParams: typeParams},
			}
			_ = cg.genTypeDecl(synthDecl)
		}
	}

	st, ok := cg.structTypes[concreteName]
	if !ok {
		return nil, fmt.Errorf("failed to monomorphize Tuple type %q", concreteName)
	}

	alloca := block.NewAlloca(st)
	block.NewStore(constant.NewZeroInitializer(st), alloca)

	// Set type_id field (index 0).
	if typeID, has := cg.structTypeIDs[concreteName]; has {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	userOff := cg.userFieldOffset(concreteName)

	// Store each element positionally into fields a, b, c, ...
	for i, v := range vals {
		idx := userOff + i
		if idx >= len(st.Fields) {
			break
		}

		fieldType := st.Fields[idx]
		v = cg.coerce(block, v, fieldType)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		block.NewStore(v, gep)
		// ARC: retain any RC-tracked elements.
		if isCopyExpr(tup.Elems[i]) {
			cg.emitRetain(block, v)
		}
	}

	return block.NewLoad(st, alloca), nil
}

// genSliceExpr generates code for a slice expression arr[start:end].
func (cg *CodeGen) genSliceExpr(block *ir.Block, e *ast.SliceExpr) (value.Value, error) {
	// Fixed-size byte arrays [byte; N]: heap-copy the slice to produce a [byte].
	// Use genLValue to get the alloca pointer directly (no spurious full-array load).
	if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
		if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
			if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 && at.ElemType.Equal(irtypes.I8) {
				var startVal, endVal value.Value

				if e.Start != nil {
					sv, err := cg.genExpr(block, e.Start)
					if err != nil {
						return nil, err
					}

					startVal = cg.coerce(block, sv, irtypes.I64)
				} else {
					startVal = constant.NewInt(irtypes.I64, 0)
				}

				if e.End != nil {
					ev, err := cg.genExpr(block, e.End)
					if err != nil {
						return nil, err
					}

					endVal = cg.coerce(block, ev, irtypes.I64)
				} else {
					endVal = constant.NewInt(irtypes.I64, int64(at.Len))
				}

				length := block.NewSub(endVal, startVal)
				elemPtr := block.NewGetElementPtr(at, arrPtr,
					constant.NewInt(irtypes.I32, 0), startVal)
				srcPtr := block.NewBitCast(elemPtr, irtypes.I8Ptr)

				return block.NewCall(cg.ensureBytesFromBuf(), srcPtr, length), nil
			}
		}
	}

	arrVal, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// Only fat-pointer arrays {T*, i64} are supported for slicing.
	arrType, ok := arrVal.Type().(*irtypes.StructType)
	if !ok || len(arrType.Fields) < 2 {
		return nil, fmt.Errorf("slice expression requires a fat-array type, got %s", arrVal.Type())
	}

	ptrField := arrType.Fields[0]

	ptrType, isPtrType := ptrField.(*irtypes.PointerType)
	if !isPtrType {
		return nil, fmt.Errorf("slice expression: first field must be a pointer, got %s", ptrField)
	}

	elemType := ptrType.ElemType

	alloca := block.NewAlloca(arrType)
	block.NewStore(arrVal, alloca)

	// Extract data pointer and length from fat-array.
	dataGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	lenGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataPtr := block.NewLoad(ptrType, dataGep)
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	var startVal, endVal value.Value

	if e.Start != nil {
		sv, err := cg.genExpr(block, e.Start)
		if err != nil {
			return nil, err
		}

		startVal = cg.coerce(block, sv, irtypes.I64)
	} else {
		startVal = constant.NewInt(irtypes.I64, 0)
	}

	if e.End != nil {
		ev, err := cg.genExpr(block, e.End)
		if err != nil {
			return nil, err
		}

		endVal = cg.coerce(block, ev, irtypes.I64)
	} else {
		endVal = arrLen
	}

	// newDataPtr = GEP(elemType, dataPtr, startVal)
	newDataPtr := block.NewGetElementPtr(elemType, dataPtr, startVal)
	// newLen = endVal - startVal
	newLen := block.NewSub(endVal, startVal)

	// Build new fat-array {T*, i64}.
	resultAlloca := block.NewAlloca(arrType)
	newDataGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	newLenGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(newDataPtr, newDataGep)
	block.NewStore(newLen, newLenGep)

	// Expose the BASE allocation pointer (before the GEP offset) so that genVarDecl
	// can retain/release the actual ARC block rather than a possibly-interior pointer.
	// For start==0 newDataPtr==dataPtr; for start>0 newDataPtr is interior.
	cg.lastSliceBase = block.NewBitCast(dataPtr, irtypes.I8Ptr)

	return block.NewLoad(arrType, resultAlloca), nil
}

func (cg *CodeGen) genAsExpr(block *ir.Block, e *ast.AsExpr) (value.Value, error) {
	targetType, err := cg.tinTypeToLLVM(e.Type)
	if err != nil {
		return nil, err
	}

	// [byte; N] as [byte] or [byte; N] as string: heap-copy the fixed-size byte
	// array into a new ARC-managed fat slice / string.
	// Use genLValue to get the alloca pointer without loading the full array.
	if isFatArrayPtr(targetType) || isStringType(targetType) {
		if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
			if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
				if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 && at.ElemType.Equal(irtypes.I8) {
					n := constant.NewInt(irtypes.I64, int64(at.Len))
					elemPtr := block.NewGetElementPtr(at, arrPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, 0))
					srcPtr := block.NewBitCast(elemPtr, irtypes.I8Ptr)

					return block.NewCall(cg.ensureBytesFromBuf(), srcPtr, n), nil
				}
			}
		}
	}

	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// For unsigned source types, integer widening must use zext, not sext.
	// Determine signedness from the source expression's Tin type.
	if irtypes.IsInt(val.Type()) && irtypes.IsInt(targetType) {
		sBits := val.Type().(*irtypes.IntType).BitSize

		tBits := targetType.(*irtypes.IntType).BitSize
		if sBits < tBits {
			// IntLit values are always non-negative as written in source code.
			// Large literals (e.g. 18446744073709551615) that exceed i64::MAX are
			// stored as their two's-complement i64 bit pattern (negative). Using
			// zext recovers the correct unsigned magnitude; sext would sign-extend
			// the raw i64 storage and produce a wrong i128/u128 value.
			_, isIntLit := e.Expr.(*ast.IntLit)
			srcUnsigned := isIntLit || cg.exprElemIsUnsigned(e.Expr)

			if srcUnsigned {
				return block.NewZExt(val, targetType), nil
			}

			return block.NewSExt(val, targetType), nil
		}
	}

	result := cg.coerce(block, val, targetType)
	// Release the `any` box after unboxing when it is a temporary (fresh allocation
	// from a call/getfield).  Identifiers and field accesses are copy-borrows that
	// are owned by their parent scope/struct and must NOT be released here.
	if isAnyType(val.Type()) && !isAnyType(targetType) && !isCopyExpr(e.Expr) {
		cg.emitRelease(block, val)
	}

	return result, nil
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
		loaded := block.NewLoad(pt.ElemType, val)

		// ARC move semantics: if the pointer is a temporary (e.g. *parse_value(&p)),
		// the caller owns the RC block but no variable will release it. Free the
		// outer allocation now. Do NOT call emitRelease (which walks struct fields) -
		// the fields are transferred to the loaded copy and will be released there.
		if isTemporaryProducer(e.Expr) {
			rcPtr := block.NewBitCast(val, irtypes.I8Ptr)
			block.NewCall(cg.ensureRelease(), rcPtr)
		}

		return loaded, nil
	}

	return val, nil
}

func (cg *CodeGen) genPipeExpr(block *ir.Block, e *ast.PipeExpr) (value.Value, error) {
	// a |> f(args) = f(args)(a)  - curried style: call f(args) first, then call
	// the returned function with a.
	// a |> f         = f(a)      - plain function value on the right.
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
	// Call through the function (fat-pointer or plain).
	var result value.Value

	if isFatFnPtr(rightFn.Type()) {
		fnPtr := block.NewExtractValue(rightFn, 0)
		envPtr := block.NewExtractValue(rightFn, 1)
		fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		llArgs := cg.adaptArgs(block, []value.Value{envPtr, leftVal}, fnType)
		result = block.NewCall(fnPtr, llArgs...)
	} else {
		result = block.NewCall(rightFn, leftVal)
	}
	// ARC: release the left-hand value if it is a temporary RC allocation.
	if isRCTrackedType(leftVal.Type()) && !isCopyExpr(e.Left) {
		cg.emitRelease(block, leftVal)
	}
	// ARC: release the right-hand closure if it is a temporary RC allocation
	// (e.g. `nums |> filter(fn)` where filter returns a fresh closure).
	if isRCTrackedType(rightFn.Type()) && !isCopyExpr(e.Right) {
		cg.emitRelease(block, rightFn)
	}

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

	result := block.NewSelect(cond, thenVal, elseVal)

	// ARC: both branches are evaluated eagerly before select.  If a branch
	// produces a fresh RC-tracked value (call, concat, etc.) that is not
	// selected, it must be released.  Use a second select to identify the
	// discarded value at runtime without actual conditional branching.
	// Releasing a zero-initialized fat struct is safe: extractRCDataPtr returns
	// a null ptr, and _tin_release(null) is a no-op.
	t := result.Type()
	if isRCTrackedType(t) {
		zero := cg.zeroValue(t)
		thenIsTemp := isTemporaryProducer(e.Then)
		elseIsTemp := isTemporaryProducer(e.Else)

		if thenIsTemp {
			// Release thenVal when the else branch was selected (cond == false).
			discarded := block.NewSelect(cond, zero, thenVal)
			cg.emitRelease(block, discarded)
		}

		if elseIsTemp {
			// Release elseVal when the then branch was selected (cond == true).
			discarded := block.NewSelect(cond, elseVal, zero)
			cg.emitRelease(block, discarded)
		}
	}

	return result, nil
}

func (cg *CodeGen) genIsExpr(block *ir.Block, e *ast.IsExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// Typed is-check: "x is v T" - check the tag and optionally bind the payload.
	if st, ok := val.Type().(*irtypes.StructType); ok {
		typeName := cg.typeNameOf(val.Type())

		// Tagged union is-check: "a is i i8" where a is type u = i8 | string.
		if members, isUnion := cg.unionTypeMembers[typeName]; isUnion && e.Type != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(e.Type)
			if err2 != nil {
				return nil, err2
			}

			tag := int8(-1)

			for i, te := range members {
				lt, err3 := cg.tinTypeToLLVM(te)
				if err3 != nil {
					continue
				}

				if lt.Equal(targetLLVM) {
					tag = int8(i)

					break
				}
			}

			if tag < 0 {
				tag = 0
			}

			alloca := block.NewAlloca(st)
			block.NewStore(val, alloca)
			// Field 1 = i8 tag (field 0 is i32 type_id).
			tagGEP := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			tagVal := block.NewLoad(irtypes.I8, tagGEP)

			cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I8, int64(tag)))
			if e.VarName != "" {
				// Field 2 = [N x i8] payload.
				payloadGEP := block.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := block.NewAlloca(targetLLVM)
				payloadVal := block.NewLoad(targetLLVM, payloadPtr)
				block.NewStore(payloadVal, payloadAlloca)
				// noRelease: the binding is a borrow from the union -- the union
				// owns the ARC reference.  The scope exit must not release it
				// because (a) no retain was performed and (b) in the non-match
				// path the alloca contains the union data interpreted as the
				// wrong type, so releasing it would corrupt memory.
				cg.curScope.set(e.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true, noRelease: true})
			}

			return cmp, nil
		}
	}
	// any type check: "x is dog" where x is any - compare type_id (field 0).
	if isAnyType(val.Type()) && e.Type != nil {
		targetName := ""

		switch t := e.Type.(type) {
		case *ast.SimpleType:
			targetName = t.Name
		}

		if targetName != "" {
			var (
				targetID int32
				found    bool
			)

			if id, ok := cg.structTypeIDs[targetName]; ok {
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

	var (
		result []string
		walk   func(ast.Node)
	)

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
			// Collect free vars of nested lambda that the current lambda needs to
			// capture so they're available in scope when the nested lambda is compiled.
			// Example: fn(b) = return fn(c) = return a+b+c
			// The outer lambda must capture 'a' even though 'a' only appears in the inner lambda.
			nestedLocals := map[string]bool{}
			for _, p := range v.Params {
				nestedLocals[p.Name] = true
			}

			for _, nf := range collectFreeVars(v.Body, nestedLocals) {
				if !localNames[nf] && !seen[nf] {
					seen[nf] = true
					result = append(result, nf)
				}
			}
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
		case *ast.ArrayFillLit:
			walk(v.Value)
		case *ast.TupleLit:
			for _, el := range v.Elems {
				walk(el)
			}
		case *ast.SliceExpr:
			walk(v.Expr)
			walk(v.Start)
			walk(v.End)
		case *ast.AddrExpr:
			walk(v.Val)
		case *ast.AddressOfExpr:
			walk(v.Expr)
		case *ast.DerefExpr:
			walk(v.Expr)
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
		case *ast.AwaitExpr:
			walk(v.Future)
		case *ast.SpawnExpr:
			if v.Call != nil {
				walk(v.Call)
			}
			// Don't descend into DoBlock of nested spawn do: blocks; they capture independently.
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

// isAsyncTraitMethod reports whether methodName is a {#async} virtual method
// of the trait identified by instKey.
func (cg *CodeGen) isAsyncTraitMethod(instKey, methodName string) bool {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	for _, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return true
		}
	}

	return false
}

// asyncCoroSlotIndex returns the vtable field index of the $coro slot for
// methodName in instKey's vtable.  Returns -1 if not found.
// $coro slots are appended after all sync slots:
//
//	index = len(syncMethods) + position_in_asyncMethods_list
func (cg *CodeGen) asyncCoroSlotIndex(instKey, methodName string) int {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	syncCount := len(cg.traitMethodOrder[baseTrait])
	for i, name := range cg.traitAsyncMethodNames[baseTrait] {
		if name == methodName {
			return syncCount + i
		}
	}

	return -1
}

// traitMethodRetType returns the sync LLVM return type for methodName in
// instKey's vtable (slot 0..N-1).  Returns nil if not found.
func (cg *CodeGen) traitMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	methodOrder := cg.traitMethodOrder[baseTrait]

	vtableSt := cg.traitVtableStructTypes[instKey]
	if vtableSt == nil {
		return nil
	}

	for i, name := range methodOrder {
		if name == methodName {
			fnPtr, ok := vtableSt.Fields[i].(*irtypes.PointerType)
			if !ok {
				return nil
			}

			ft, ok := fnPtr.ElemType.(*irtypes.FuncType)
			if !ok {
				return nil
			}

			return ft.RetType
		}
	}

	return nil
}

// traitAsyncMethodRetType returns the LLVM return type for an {#async} virtual method
// by looking up the trait declaration.  Used when the method has no sync vtable slot
// (async-only traits like io::AsyncReader).
func (cg *CodeGen) traitAsyncMethodRetType(instKey, methodName string) irtypes.Type {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	td, ok := cg.traits[baseTrait]
	if !ok {
		return nil
	}

	for _, m := range td.Methods {
		if m.Name != methodName || !isAsyncTag(m.Tags) {
			continue
		}

		if m.RetType == nil {
			return irtypes.Void
		}

		lt, err := cg.tinTypeToLLVM(m.RetType)
		if err != nil {
			return nil
		}

		return lt
	}

	return nil
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
			// Wrap return value if needed (e.g., raw i8* -> string fat-ptr).
			ret := cg.wrapFromExtern(entry, result, wrapperFnType.RetType, false)
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

// wrapAsyncFnAsFatPtr wraps an {#async} function's $coro variant into an async
// fat-fn-ptr { fn(i8* env, params...) i8* *, i8* } with a null environment.
// The shim ignores its env parameter and forwards to <name>$coro(params...).
// Falls back to wrapFnAsFatPtr (with a sync shim) when no $coro is found.
// Shims are cached per function name to avoid duplicate definitions.
func (cg *CodeGen) wrapAsyncFnAsFatPtr(block *ir.Block, fnVal value.Value, targetFatType irtypes.Type) value.Value {
	fatSt := targetFatType.(*irtypes.StructType)
	wrapperFnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Derive the name of the function so we can look up its $coro variant.
	fnName := ""
	if named, ok := fnVal.(interface{ Name() string }); ok {
		fnName = named.Name()
	}

	if fnName == "" {
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	// Find the $coro variant in scope.
	coroName := fnName + "$coro"

	coroEntry, ok := cg.curScope.lookup(coroName)
	if !ok {
		// Also try stripping a package prefix (pkg__foo → foo$coro).
		if idx := strings.Index(fnName, "__"); idx >= 0 {
			coroEntry, ok = cg.curScope.lookup(fnName[idx+2:] + "$coro")
		}
	}

	if !ok {
		// No $coro variant - fall back to sync shim (type mismatch at runtime).
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	coroFn, ok := coroEntry.val.(*ir.Func)
	if !ok {
		return cg.wrapFnAsFatPtr(block, fnVal, targetFatType)
	}

	shimName := "__ashim_" + fnName

	// Reuse cached shim.
	var shim *ir.Func

	for _, f := range cg.mod.Funcs {
		if f.Name() == shimName {
			shim = f

			break
		}
	}

	if shim == nil {
		// Build shim: fn(i8* env, tin_param_0, ...) i8*
		// wrapperFnType.Params[0] is i8* (env); Params[1..] are actual types.
		shimParams := make([]*ir.Param, len(wrapperFnType.Params))
		for i, pt := range wrapperFnType.Params {
			name := "env"
			if i > 0 {
				name = fmt.Sprintf("p%d", i-1)
			}

			shimParams[i] = ir.NewParam(name, pt)
		}

		shim = cg.mod.NewFunc(shimName, irtypes.I8Ptr, shimParams...)
		entry := shim.NewBlock("entry")

		// Forward call to $coro: skip env (index 0), pass and adapt remaining args.
		n := len(coroFn.Params)
		if n > len(shim.Params)-1 {
			n = len(shim.Params) - 1
		}

		callArgs := make([]value.Value, n)
		for i := 0; i < n; i++ {
			callArgs[i] = cg.coerce(entry, shim.Params[i+1], coroFn.Params[i].Type())
		}

		hdl := entry.NewCall(coroFn, callArgs...)
		entry.NewRet(hdl)
	}

	// Return async fat-fn-ptr { shim*, null }.
	alloca := block.NewAlloca(fatSt)
	gep0 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(shim, gep0)
	gep1 := block.NewGetElementPtr(fatSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewNull(irtypes.I8Ptr), gep1)

	return block.NewLoad(fatSt, alloca)
}

// genArgWithTargetType evaluates an argument expression with a known target
// parameter type, enabling type-guided overload resolution for function-value
// arguments.  When the target is a fat-fn-ptr and the argument is a plain
// identifier that names overloaded functions, the overload whose arity matches
// the fat-ptr's parameter count is selected and wrapped appropriately.
// Falls through to a normal genExpr when the heuristic does not apply.
func (cg *CodeGen) genArgWithTargetType(block *ir.Block, argNode ast.Node, targetType irtypes.Type) (value.Value, error) {
	if isFatFnPtr(targetType) {
		if id, ok := argNode.(*ast.Identifier); ok {
			if variants, hasOverloads := cg.overloads[id.Name]; hasOverloads {
				// Extract expected arity from the fat-ptr: Params[0] is env, rest are actual.
				fatSt := targetType.(*irtypes.StructType)
				fnType := fatSt.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
				expectedArity := len(fnType.Params) - 1 // subtract env

				var best *overloadEntry

				for _, v := range variants {
					if v.arity == expectedArity {
						best = v

						break
					}
				}

				if best != nil {
					if se, seOk := cg.curScope.lookup(best.irName); seOk {
						var fnVal value.Value

						if se.isAlloc {
							pt := se.val.Type().(*irtypes.PointerType)
							fnVal = block.NewLoad(pt.ElemType, se.val)
						} else {
							fnVal = se.val
						}

						if isAsyncFatFnPtr(targetType) {
							return cg.wrapAsyncFnAsFatPtr(block, fnVal, targetType), nil
						}

						return cg.wrapFnAsFatPtr(block, fnVal, targetType), nil
					}
				}
			}
		}
	}

	return cg.genExpr(block, argNode)
}

// callFatFn emits a call through a closure fat pointer { fn(i8*,params...)*, i8* }.
func (cg *CodeGen) callFatFn(block *ir.Block, fatPtr value.Value, argNodes []ast.Node) (value.Value, error) {
	fnPtr := block.NewExtractValue(fatPtr, 0)
	envPtr := block.NewExtractValue(fatPtr, 1)

	// Build args (index 0 = env, indices 1..N = actual params).
	llArgs := []value.Value{envPtr}
	llArgsPreCoerce := []value.Value{envPtr}

	// Derive target param types for type-guided resolution (Params[0] is env).
	fnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	for i, arg := range argNodes {
		// Params[0] is env; the i-th tin arg maps to Params[i+1].
		var targetType irtypes.Type
		if i+1 < len(fnType.Params) {
			targetType = fnType.Params[i+1]
		}

		av, err := cg.genArgWithTargetType(block, arg, targetType)
		if err != nil {
			return nil, err
		}

		llArgs = append(llArgs, av)
		llArgsPreCoerce = append(llArgsPreCoerce, av)
	}

	llArgs = cg.adaptArgs(block, llArgs, fnType)

	result := block.NewCall(fnPtr, llArgs...)

	// ARC: release temporary RC-tracked arguments (skip index 0 = env).
	for i, astArg := range argNodes {
		argIdx := i + 1 // offset by 1 for the env slot
		preCoerce := llArgsPreCoerce[argIdx]
		postCoerce := llArgs[argIdx]

		// Case 1: adaptArgs boxed a non-any value to any.
		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}
		// Case 2: RC-tracked temporary argument.
		if !isRCTrackedType(preCoerce.Type()) {
			continue
		}

		if isCopyExpr(astArg) {
			continue
		}

		cg.emitRelease(block, preCoerce)
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}
