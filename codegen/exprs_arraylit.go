package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genArrayLit(block *ir.Block, e *ast.ArrayLit) (value.Value, error) {
	return cg.genArrayLitWithElemType(block, e, nil)
}

// genArrayLitWithElemType generates an array literal, optionally coercing each element to targetElemType.
// Used when the declared array type is known (e.g. let fns [fn{#async}(i64) i64] = [double]).
func (cg *CodeGen) genArrayLitWithElemType(block *ir.Block, e *ast.ArrayLit, targetElemType irtypes.Type) (value.Value, error) {
	if len(e.Elems) == 0 {
		// Empty dynamic array: {null, 0, 0} typed against targetElemType
		// when known.  Cap=0 means "first append must allocate".  When
		// no target is known the caller gets the untyped {i8*, i64}
		// (string-shaped) form and the coerce path later swaps it for
		// a correctly-typed zero value -- the constant null in the
		// data field is what discriminates it from a real string at
		// runtime.
		if targetElemType != nil {
			return fatArrayConst(targetElemType,
				constant.NewNull(irtypes.NewPointer(targetElemType)),
				0, 0), nil
		}

		fat := stringFatPtrType() // {i8*, i64 len, i64 cap}

		return constant.NewStruct(fat,
			constant.NewNull(irtypes.I8Ptr),
			constant.NewInt(irtypes.I64, 0),
			constant.NewInt(irtypes.I64, 0)), nil
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
			if !v.Type().Equal(targetElemType) {
				return nil, cg.nodeErr(elem,
					"array element %d: cannot store %s where %s is expected",
					i, cg.tinTypeDisplay(v.Type()),
					cg.tinTypeDisplay(targetElemType))
			}
		}

		vals[i] = v
	}

	elemType := vals[0].Type()

	if targetElemType != nil {
		elemType = targetElemType
	}
	// All elements must agree on a single LLVM type before we hand
	// them to ir.NewStore.  When there is no target hint, the first
	// element pins the type and subsequent mismatches are caught here.
	for i, v := range vals {
		if !v.Type().Equal(elemType) {
			return nil, cg.nodeErr(e.Elems[i],
				"array element %d: cannot store %s where %s is expected",
				i, cg.tinTypeDisplay(v.Type()),
				cg.tinTypeDisplay(elemType))
		}
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

	// Return as fat pointer {T*, i64, i64}. Cap == n: a literal has no
	// preallocated headroom, so the first `++=` triggers a grow.
	lenConst := constant.NewInt(irtypes.I64, n)

	return cg.buildFatArrayValue(block, elemType, dataPtr, lenConst, lenConst), nil
}

// genArrayLitAsFixed materializes an array literal `[e0, e1, ..., eN]` as a
// fixed-size aggregate of LLVM type `[N x T]`.  Used when the target slot
// (struct field, parameter) is declared with explicit length `[T; N]`.
// Element count must match exactly; the function rejects under/overflow with
// a positioned diagnostic.
func (cg *CodeGen) genArrayLitAsFixed(block *ir.Block, e *ast.ArrayLit, at *irtypes.ArrayType) (value.Value, error) {
	if uint64(len(e.Elems)) != at.Len {
		return nil, cg.nodeErr(e,
			"fixed-size array [_; %d] needs exactly %d elements, got %d",
			at.Len, at.Len, len(e.Elems))
	}

	alloca := block.NewAlloca(at)
	for i, elem := range e.Elems {
		v, err := cg.genArgWithTargetType(block, elem, at.ElemType)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		v = cg.coerce(block, v, at.ElemType)
		if !v.Type().Equal(at.ElemType) {
			return nil, cg.nodeErr(elem,
				"array element %d: cannot store %s where %s is expected",
				i, cg.tinTypeDisplay(v.Type()), cg.tinTypeDisplay(at.ElemType))
		}

		gep := block.NewGetElementPtr(at, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, int64(i)))
		block.NewStore(v, gep)
	}

	return block.NewLoad(at, alloca), nil
}

// genArrayFillBinExpr lowers the `[v] * n` infix form to a fresh `[T]`
// of `n` copies of `v`.  Compile-time integer counts collapse to the
// same compile-time path `genArrayFillLit` uses for `[v; N]`; runtime
// counts emit one `_tin_rc_alloc` plus a per-element store loop.  The
// two forms share the same lowering -- only the parsing differs.
func (cg *CodeGen) genArrayFillBinExpr(block *ir.Block, valNode, countNode ast.Node) (value.Value, error) {
	if lit, ok := countNode.(*ast.IntLit); ok {
		return cg.genArrayFillLit(block, &ast.ArrayFillLit{
			Value: valNode,
			Count: int(lit.Value),
		})
	}

	v, err := cg.genExpr(block, valNode)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	cnt, err := cg.genExpr(block, countNode)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	cnt = cg.coerce(block, cnt, irtypes.I64)
	elemType := v.Type()

	nullPtr := constant.NewNull(irtypes.NewPointer(elemType))
	sizeGep := block.NewGetElementPtr(elemType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSize := block.NewMul(elemSize, cnt)

	mallocI8 := block.NewCall(cg.ensureRCAlloc(), totalSize)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(elemType))

	// Per-element fill loop driven by `cnt`.  Retains for RC element
	// types use _tin_retain so immortal fill values stay at rc=-1.
	loopHeader := cg.newBlock("fill.head")
	loopBody := cg.newBlock("fill.body")
	loopExit := cg.newBlock("fill.exit")

	iAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
	block.NewBr(loopHeader)

	iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
	cond := loopHeader.NewICmp(enum.IPredSLT, iVal, cnt)
	loopHeader.NewCondBr(cond, loopBody, loopExit)

	iv := loopBody.NewLoad(irtypes.I64, iAlloca)
	gep := loopBody.NewGetElementPtr(elemType, dataPtr, iv)
	loopBody.NewStore(v, gep)

	if isRCTrackedType(elemType) {
		cg.emitRetain(loopBody, v)
	}

	next := loopBody.NewAdd(iv, constant.NewInt(irtypes.I64, 1))
	loopBody.NewStore(next, iAlloca)
	loopBody.NewBr(loopHeader)

	cg.curBlock = loopExit

	return cg.buildFatArrayValue(loopExit, elemType, dataPtr, cnt, cnt), nil
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

	// Return fat pointer {T*, i64, i64} with cap == len.
	lenConst := constant.NewInt(irtypes.I64, n)

	return cg.buildFatArrayValue(block, elemType, dataPtr, lenConst, lenConst), nil
}

// inferStructTypeArgs infers type arguments for a generic struct literal from
// the provided field values. Each StructLit field is matched by name against
// the template's declared fields; the field value's LLVM type is unified
// against the declared field type to bind the template's type parameters.
// Returns the inferred TypeExpr list on success, or (nil, false) when
// inference is ambiguous or incomplete.
//
// Two-pass like inferTypeArgs for functions: runtime values first, then
// literal constants, so a literal `1` doesn't pin T to i64 when a sibling
// field provides a concrete i32 value.
func (cg *CodeGen) inferStructTypeArgs(block *ir.Block, e *ast.StructLit, arityMap map[int]*ast.StructDecl) ([]ast.TypeExpr, bool) {
	// We don't know the arity up front when there are no explicit TypeArgs.
	// Try templates in ascending arity and return the first that yields a
	// fully-bound substitution. In practice generics with different arities
	// and the same name are extremely rare (Tin's genericStructsByArity is
	// keyed on both) so this is a small search.
	var arities []int
	for a := range arityMap {
		arities = append(arities, a)
	}

	sort.Ints(arities)

	for _, arity := range arities {
		tmpl := arityMap[arity]

		subst := make(map[string]TypeName)
		fromConst := make(map[string]bool)

		fieldIndex := make(map[string]ast.TypeExpr, len(tmpl.Fields))
		for _, tf := range tmpl.Fields {
			fieldIndex[tf.Name] = tf.Type
		}

		for pass := 0; pass < 2; pass++ {
			for _, f := range e.Fields {
				declType, ok := fieldIndex[f.Name]
				if !ok {
					continue
				}

				val, err := cg.genExpr(block, f.Value)
				if err != nil || val == nil {
					continue
				}

				_, isConst := val.(constant.Constant)
				if pass == 0 && isConst {
					continue
				}

				if pass == 1 && !isConst {
					continue
				}

				cg.inferTypeArgsFromParamPrio(declType, val.Type(), tmpl.TypeParams, subst, fromConst, isConst)
			}
		}
		// Every type parameter must be bound; otherwise inference is
		// ambiguous and we bail to let the existing "unknown struct"
		// error fire with a hint about missing annotations.
		if len(subst) != len(tmpl.TypeParams) {
			continue
		}

		inferred := make([]ast.TypeExpr, len(tmpl.TypeParams))
		for i, tp := range tmpl.TypeParams {
			inferred[i] = &ast.SimpleType{Name: subst[tp].Canon}
		}

		return inferred, true
	}

	return nil, false
}

func (cg *CodeGen) genStructLit(block *ir.Block, e *ast.StructLit) (value.Value, error) {
	// Capture the original source position before any rewrite below. The
	// rewrites construct fresh StructLit nodes without a Pos, which would
	// otherwise leak through to error messages as "0:0" or whatever
	// cg.currentPos last held (typically the position of an unrelated
	// monomorphized method body).
	origPos := e.Pos()

	typeName := e.TypeName
	// Generic struct literal WITHOUT explicit type args: `Box{value: "hi"}`
	// -- infer type arguments from the provided field values when `Box` is
	// a generic struct template. The inference unifies each field-value's
	// LLVM type against the template's declared field type (which may name
	// one of the template's type parameters). Ambiguities (no type param
	// mentioned in any named field) fall through to the existing error.
	if len(e.TypeArgs) == 0 {
		if cg.structTypeFor(CanonKey(typeName)) == nil {
			if arityMap, isGeneric := cg.genericStructsByArity[typeName]; isGeneric {
				if inferred, ok := cg.inferStructTypeArgs(block, e, arityMap); ok {
					next := &ast.StructLit{TypeName: e.TypeName, TypeArgs: inferred, Fields: e.Fields, Positional: e.Positional}
					next.SetPos(origPos)
					e = next
				}
			}
		}
	}
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
				if alias := cg.aliasTypeFor(CanonKey(st2.Name)); alias != nil {
					resolved = alias
				}
			}

			resolvedTypeArgs[i] = resolved
		}

		concreteName := typeName + "__" + strings.Join(parts, "__")
		if cg.structTypeFor(CanonKey(concreteName)) == nil {
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
		next := &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
		next.SetPos(origPos)
		e = next
	}
	// Resolve through type aliases to the canonical struct name
	// (e.g., bare "Mutex" -> "sync__Mutex" after canonical naming).
	// Also handles package-qualified names like "http::Request" -> "http__Request"
	// and deep paths like "net::tcp::Conn" -> "tcp::Conn" -> "tcp__Conn".
	if cg.structTypeFor(CanonKey(typeName)) == nil {
		resolved := false

		if alias := cg.aliasTypeFor(CanonKey(typeName)); alias != nil {
			if simple, ok3 := alias.(*ast.SimpleType); ok3 {
				typeName = simple.Name
				next := &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
				next.SetPos(origPos)
				e = next
				resolved = true
			}
		}

		// For multi-level qualified names (e.g. "net::tcp::Conn"), strip leading
		// package components one at a time until an alias is found.
		if !resolved && strings.Contains(typeName, "::") {
			parts := strings.Split(typeName, "::")
			for i := 1; i < len(parts); i++ {
				shorter := strings.Join(parts[i:], "::")

				if alias := cg.aliasTypeFor(CanonKey(shorter)); alias != nil {
					if simple, ok3 := alias.(*ast.SimpleType); ok3 {
						typeName = simple.Name
						next := &ast.StructLit{TypeName: typeName, Fields: e.Fields, Positional: e.Positional}
						next.SetPos(origPos)
						e = next

						break
					}
				}
			}
		}
	}

	// SIMD vector literal: f32x4{ 1.0, 2.0, 3.0, 4.0 }
	if resolvedVecType, err2 := cg.tinTypeToLLVM(&ast.SimpleType{Name: typeName}); err2 == nil {
		if vecType, isVec := resolvedVecType.(*irtypes.VectorType); isVec {
			return cg.genSIMDLit(block, e, vecType)
		}
	}

	st := cg.structTypeFor(CanonKey(typeName))
	if st == nil {
		return nil, cg.nodeErr(e, "unknown struct type: %s", cg.diagStructName(typeName))
	}

	// #closed enforcement: a closed struct's literal `S{...}` may only
	// appear inside one of S's own static methods. External callers must go
	// through the constructor -- that's the whole point of #closed.
	if cg.closedStructs[typeName] && !cg.curFnOwnsStruct(typeName) {
		hint := cg.closedConstructorHint(typeName)

		return nil, cg.nodeErr(e,
			"%s is #closed: construct via one of its static methods (%s) - direct struct literals are not allowed outside the type's own methods",
			cg.diagStructName(typeName), hint)
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
			// Refresh block after short-circuit / await / coro split
			// operands that park curBlock on a merge.
			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))

			val = cg.coerce(block, val, st.Fields[idx])
			if !val.Type().Equal(st.Fields[idx]) {
				return nil, cg.nodeErr(v,
					"struct %s field %d: cannot store %s where %s is expected",
					e.TypeName, i, cg.tinTypeDisplay(val.Type()),
					cg.tinTypeDisplay(st.Fields[idx]))
			}

			block.NewStore(val, gep)
			// ARC: retain RC-tracked values that are copied from existing owners.
			// Weak fields are non-owning: skip retain so only one owner counts.
			fieldName := ""
			if i < len(fieldNames) {
				fieldName = fieldNames[i]
			}
			// Track owning raw `*T` fields (see field-named branch below for
			// the rationale).
			if !weakSet[fieldName] && fieldName != "" {
				cg.markOwningRawPtrField(typeName, fieldName, v, val.Type())
			}

			if isCopyExpr(v) && !weakSet[fieldName] {
				if pt, ok2 := val.Type().(*irtypes.PointerType); ok2 {
					if innerSt, ok3 := pt.ElemType.(*irtypes.StructType); ok3 && innerSt.Name() != "" {
						if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
							ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
							block.NewCall(cg.ensureRetain(), ptrI8)
						}
					}
				} else {
					cg.emitRetain(block, val)
				}
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

			val, err := cg.genArgWithTargetType(block, f.Value, st.Fields[idx])
			if err != nil {
				return nil, err
			}
			// Refresh block after short-circuit operands (`a || b`) that
			// advanced curBlock to a merge -- otherwise the subsequent
			// GEP/store land in a block where `val` does not dominate.
			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			gep := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))

			val = cg.coerce(block, val, st.Fields[idx])
			if !val.Type().Equal(st.Fields[idx]) {
				return nil, cg.nodeErr(f.Value,
					"struct %s field %s: cannot store %s where %s is expected",
					e.TypeName, f.Name, cg.tinTypeDisplay(val.Type()),
					cg.tinTypeDisplay(st.Fields[idx]))
			}

			block.NewStore(val, gep)
			// `&escape_promoted_local` flowing into a raw `*T` field -- the
			// local was heap-allocated by escape analysis, and Tin would
			// otherwise leak the heap block when the containing struct
			// drops (no owner left). Mark this struct/field pair as
			// owning so the per-struct release helper cascades through it.
			if !weakSet[f.Name] {
				cg.markOwningRawPtrField(typeName, f.Name, f.Value, val.Type())
			}
			// ARC: retain RC-tracked values copied from existing owners.
			// Weak fields are non-owning: skip retain.
			if isCopyExpr(f.Value) && !weakSet[f.Name] {
				if pt, ok2 := val.Type().(*irtypes.PointerType); ok2 {
					if innerSt, ok3 := pt.ElemType.(*irtypes.StructType); ok3 && innerSt.Name() != "" {
						if cg.structTypeFor(CanonKey(innerSt.Name())) != nil {
							ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
							block.NewCall(cg.ensureRetain(), ptrI8)
						}
					}
				} else {
					cg.emitRetain(block, val)
				}
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
		// Suppress coerceToTrait's deferred scope-exit release: we emit
		// our own release after the call so the iface temporary lives
		// only as long as the trait init invocation.
		prevSuppress := cg.suppressIfaceScopeRelease
		cg.suppressIfaceScopeRelease = true

		args := cg.adaptArgs(block, []value.Value{result}, traitInitFn.Sig)
		cg.suppressIfaceScopeRelease = prevSuppress

		block.NewCall(traitInitFn, args...)
		// Release any iface temporaries adaptArgs constructed via
		// coerceToTrait. Without this the init iface leaks on every
		// struct construction whose trait chain runs init/deinit
		// through the iface ABI.
		for _, a := range args {
			if isTraitFatPtrShape(a.Type()) {
				dataField := block.NewExtractValue(a, 0)
				block.NewCall(cg.ensureRelease(), dataField)
			}
		}
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

	// Pre-extract per-element target types from the expectedType so
	// each element's gen can have a hint for ADT constructor
	// disambiguation. expectedType is a named Tuple struct with
	// fields at userOff..userOff+N-1 holding the element types.
	var elemHints []irtypes.Type

	if expectedType != nil {
		if st, ok := expectedType.(*irtypes.StructType); ok {
			if n := st.Name(); n != "" {
				if cg.structTypeFor(CanonKey(n)) != nil {
					off := cg.userFieldOffset(n)
					if off+len(tup.Elems) <= len(st.Fields) {
						elemHints = make([]irtypes.Type, len(tup.Elems))
						for i := range tup.Elems {
							elemHints[i] = st.Fields[off+i]
						}
					}
				}
			}
		}
	}

	// Evaluate all element expressions.
	vals := make([]value.Value, len(tup.Elems))
	for i, elem := range tup.Elems {
		var v value.Value

		var err error

		if elemHints != nil && elemHints[i] != nil {
			prevHint := cg.returnTypeHint
			cg.returnTypeHint = elemHints[i]
			v, err = cg.genExpr(block, elem)
			cg.returnTypeHint = prevHint
		} else {
			v, err = cg.genExpr(block, elem)
		}

		if err != nil {
			return nil, err
		}
		// Short-circuit operands (`a || b`, `a && b`) park the IR
		// insertion point on a merge block; pick it up before the next
		// element's evaluation (and before the struct fill below) so
		// every operation lands in a block that dominates its uses.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		vals[i] = v
	}

	// Determine concrete Tuple struct name.
	// If expectedType is a known named struct, use it directly.
	var concreteName string

	if expectedType != nil {
		if st, ok := expectedType.(*irtypes.StructType); ok {
			if n := st.Name(); n != "" {
				if cg.structTypeFor(CanonKey(n)) != nil {
					concreteName = n
				}
			}
		}
	}

	if concreteName == "" {
		// Infer from element LLVM types.
		parts := make([]string, len(vals))

		typeParams := make([]ast.TypeExpr, len(vals))
		for i, v := range vals {
			// Use the raw mangled struct name for the name part so the
			// resulting Tuple__... is a single valid identifier. The
			// demangled form `Result[i64, errors, Err]` contains
			// brackets/spaces that break downstream name parsing and
			// also fail to resolve back to a registered concrete type
			// (the monomorphization keyed off the mangled
			// `Result__i64__errors__Err`). Fall back to
			// llvmTypeToTinName for non-struct slots.
			parts[i] = llvmRawPartForTuple(v.Type())
			// Reconstruct a structural TypeExpr from the LLVM type so
			// the synthesized monomorphization preserves PointerType /
			// ArrayType nuance (e.g. `*errors::Err` is a PointerType
			// wrapping a SimpleType, not a SimpleType whose name is
			// "*errors::Err"). Tuple-slot variant keeps named struct
			// references as their raw mangled name so the synth
			// monomorphization re-resolves to the SAME concrete struct
			// already registered (rather than going through demangle
			// -> re-monomorphize, which can synthesize duplicate
			// instantiations like Option[Option[i64]] when the user
			// only wanted Option[i64]).
			typeParams[i] = llvmTypeToTupleSlotTypeExpr(v.Type())
		}

		concreteName = "Tuple__" + strings.Join(parts, "__")
		// Trigger monomorphization for this concrete name.
		if cg.structTypeFor(CanonKey(concreteName)) == nil {
			synthDecl := &ast.TypeDecl{
				Name: concreteName,
				Type: &ast.GenericType{Name: "Tuple", TypeParams: typeParams},
			}
			_ = cg.genTypeDecl(synthDecl)
		}
	}

	// genTypeDecl may have registered concreteName as a typeAlias to
	// a canonical-key form (e.g. `Tuple__udp::Conn__*errors::Error`
	// -> `Tuple__udp__Conn__*errors__Error`). Follow the alias chain
	// (with a safety bound) before looking up the struct type so
	// multi-hop aliases also resolve. The 64-step cap mirrors
	// typeExprCanonicalKeyN's recursion guard.
	for i := 0; i < 64; i++ {
		alias := cg.aliasTypeFor(CanonKey(concreteName))
		if alias == nil {
			break
		}

		st, isSimple := alias.(*ast.SimpleType)
		if !isSimple {
			break
		}

		if st.Name == concreteName {
			break
		}

		concreteName = st.Name
	}

	st := cg.structTypeFor(CanonKey(concreteName))
	if st == nil {
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
		if !v.Type().Equal(fieldType) {
			return nil, cg.nodeErr(tup.Elems[i],
				"tuple element %d expects %s, got %s", i,
				cg.tinTypeDisplay(fieldType), cg.tinTypeDisplay(v.Type()))
		}

		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		block.NewStore(v, gep)
		// ARC: retain any RC-tracked elements borrowed from an outer
		// owner.  Fresh allocations (e.g. `buf[0..n] as string`, which
		// lowers to _tin_bytes_from_buf, or any call returning an
		// rc=1 string/array/iface) already own their reference; an
		// extra retain here would leave rc=2 with only one matching
		// release ever fired and the field would leak by exactly one
		// allocation per construction.  Mirrors the fresh-alloc
		// exemption in genDataScopeCtorCall / genDataConstructorCall
		// and the freshIface / freshCallResult gates in genVarDecl.
		if isCopyExpr(tup.Elems[i]) && !isFreshBytesAlloc(v) && !isFreshCallResult(v) {
			cg.emitRetain(block, v)
		}
	}

	return block.NewLoad(st, alloca), nil
}

// genPtrRangeSlice handles ptr[lo..hi] on a raw pointer, returning a fat [T].
// For *byte it calls _tin_bytes_from_buf (ARC-managed copy).
// For other *T it builds a non-owning fat pointer {ptr+lo, hi-lo}.
//
// If ptrExpr resolves to a fixed-size array `[T; N]` (an addressable
// alloca), the array is implicitly decayed to its first-element pointer
// so `buf[0..n]` reads as `(&buf[0])[0..n]` - the natural way to splice
// out an ARC-managed slice without a separate `&` and an extern call.
