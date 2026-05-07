package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genArrayDestructDecl handles:
//
//	let [a, b] [T] = arr          - uniform typed (compile-time indexing)
//	let [a, b] [T1, T2] = arr     - per-slot typed from [any]  (runtime bounds check)
//	let [x, ...xs] [T] = arr      - rest split
//	let [a, b] res = arr          - named type alias resolved to per-slot types
func (cg *CodeGen) genArrayDestructDecl(block *ir.Block, s *ast.ArrayDestructDecl) (*ir.Block, error) {
	// Resolve named type alias (e.g. `type res = @[i32, bool]`)
	if s.NamedType != nil && len(s.ElemTypes) == 0 {
		// Look up the named type in typeAliases
		typeName := ""
		if st, ok := s.NamedType.(*ast.SimpleType); ok {
			typeName = st.Name
		}

		if typeName != "" {
			if aliasedTE, ok2 := cg.typeAliases[typeName]; ok2 {
				if tat, ok3 := aliasedTE.(*ast.TupleArrayType); ok3 {
					s = &ast.ArrayDestructDecl{
						Names:     s.Names,
						ElemTypes: tat.ElemTypes,
						IsAny:     true,
						Value:     s.Value,
					}
				}
			}
		}
	}

	arrVal, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if arrVal == nil {
		return block, nil
	}

	// Count regular (non-rest) names and find rest name index
	regularCount := 0
	restIdx := -1

	for i, n := range s.Names {
		if len(n) > 3 && n[:3] == "..." {
			restIdx = i
		} else {
			regularCount++
		}
	}

	// Runtime length check. Destructuring is an assertion about the array's
	// shape: with no rest, the array must have exactly `regularCount` elements;
	// with a rest slot, the array must have STRICTLY MORE than `regularCount`
	// (the rest binds at least one element). On mismatch we panic.
	{
		arrAlloca := block.NewAlloca(arrVal.Type())
		block.NewStore(arrVal, arrAlloca)
		lenGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		arrLen := block.NewLoad(irtypes.I64, lenGep)

		needed := constant.NewInt(irtypes.I64, int64(regularCount))

		var (
			badCond value.Value
			msgText string
		)

		if restIdx >= 0 {
			// rest slot binds >= 1 element, so len must be > regularCount.
			badCond = block.NewICmp(enum.IPredSLE, arrLen, needed)
			msgText = fmt.Sprintf("array destructuring: need at least %d elements (rest binds >= 1), got fewer", regularCount+1)
		} else {
			// No rest: exact length match required.
			badCond = block.NewICmp(enum.IPredNE, arrLen, needed)
			msgText = fmt.Sprintf("array destructuring: need exactly %d elements, length differs", regularCount)
		}

		id := cg.labelCount
		cg.labelCount++
		panicBlock := cg.curFn.NewBlock(fmt.Sprintf("destruct.panic.%d", id))
		okBlock := cg.curFn.NewBlock(fmt.Sprintf("destruct.ok.%d", id))
		block.NewCondBr(badCond, panicBlock, okBlock)

		msg := cg.newGlobalString(msgText)
		panicBlock.NewCall(cg.ensurePanicFn(), msg)
		// If _tin_panic returns (recover was called), clean up pending defer envs
		// and release ARC-tracked scope variables (e.g. the array being destructured).
		for _, env := range cg.pendingDeferEnvs {
			if _, isNull := env.(*constant.Null); !isNull {
				panicBlock.NewCall(cg.ensureFree(), env)
			}
		}

		cg.emitAllScopeReleases(panicBlock, "")
		// Use a proper ret (not unreachable) so that recovered panics can return.
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			panicBlock.NewRet(nil)
		} else {
			panicBlock.NewRet(cg.zeroValue(retType))
		}

		block = okBlock
	}

	_ = s.IsAny // length check now applies to both [any] and typed destructuring

	// Determine uniform element LLVM type (used when ElemTypes has 1 entry or is empty)
	var elemLLType irtypes.Type = anyFatPtrType()
	if len(s.ElemTypes) == 1 {
		elemLLType, err = cg.tinTypeToLLVM(s.ElemTypes[0])
		if err != nil {
			return nil, err
		}
	}

	// Extract data pointer from fat array {elemPtr*, i64}
	arrAlloca := block.NewAlloca(arrVal.Type())
	block.NewStore(arrVal, arrAlloca)
	ptrFieldGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ptrField := block.NewLoad(arrVal.Type().(*irtypes.StructType).Fields[0], ptrFieldGep)

	// Extract each regular element
	regIdx := 0

	for _, name := range s.Names {
		if len(name) > 3 && name[:3] == "..." {
			continue
		}

		// Per-slot type or uniform type
		var slotType irtypes.Type
		if len(s.ElemTypes) > 1 {
			slotType, err = cg.tinTypeToLLVM(s.ElemTypes[regIdx])
			if err != nil {
				return nil, err
			}
		} else {
			slotType = elemLLType
		}

		idxVal := constant.NewInt(irtypes.I64, int64(regIdx))
		if pt, ok := ptrField.Type().(*irtypes.PointerType); ok {
			elemGep := block.NewGetElementPtr(pt.ElemType, ptrField, idxVal)
			loaded := block.NewLoad(pt.ElemType, elemGep)
			coerced := cg.coerce(block, loaded, slotType)
			alloca := block.NewAlloca(slotType)
			block.NewStore(coerced, alloca)
			cg.curScope.set(name, &scopeEntry{val: alloca, isAlloc: true})
		}

		regIdx++
	}

	// Handle rest: create a sub-slice starting at regularCount
	if restIdx >= 0 {
		restName := s.Names[restIdx][3:] // strip "..."

		var elemSzBytes int64 = 8

		if pt, ok := ptrField.Type().(*irtypes.PointerType); ok {
			if sz := llvmTypeSize(pt.ElemType); sz > 0 {
				elemSzBytes = int64(sz)
			}
		}

		// Build a generic {i8*, i64} slice for _tin_slice_subslice
		sliceType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
		rawAlloca := block.NewAlloca(sliceType)

		dataPtrAsI8 := block.NewBitCast(ptrField, irtypes.I8Ptr)
		rawPtrGep := block.NewGetElementPtr(sliceType, rawAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(dataPtrAsI8, rawPtrGep)

		lenGep := block.NewGetElementPtr(arrVal.Type(), arrAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		arrLen := block.NewLoad(irtypes.I64, lenGep)
		rawLenGep := block.NewGetElementPtr(sliceType, rawAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(arrLen, rawLenGep)
		rawSlice := block.NewLoad(sliceType, rawAlloca)

		subFn := cg.ensureSliceSubslice()
		subResult := block.NewCall(subFn, rawSlice,
			constant.NewInt(irtypes.I64, int64(regularCount)),
			constant.NewInt(irtypes.I64, elemSzBytes))

		// Cast the {i8*, i64} result back to the original fat-array type
		restType := arrVal.Type()
		tmpAlloca := block.NewAlloca(sliceType)
		block.NewStore(subResult, tmpAlloca)
		castPtr := block.NewBitCast(tmpAlloca, irtypes.NewPointer(restType))
		restVal := block.NewLoad(restType, castPtr)
		restAlloca := block.NewAlloca(restType)
		block.NewStore(restVal, restAlloca)
		cg.curScope.set(restName, &scopeEntry{val: restAlloca, isAlloc: true})
	}

	return block, nil
}

// genStructDestructDecl handles: let {x, y} TypeName = expr
func (cg *CodeGen) genStructDestructDecl(block *ir.Block, s *ast.StructDestructDecl) (*ir.Block, error) {
	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return block, nil
	}

	// Resolve the struct type name
	typeName := ""

	switch t := s.StructType.(type) {
	case *ast.SimpleType:
		typeName = t.Name
	case *ast.GenericType:
		typeName = t.Name
	}

	if typeName == "" {
		return nil, fmt.Errorf("struct destructuring: cannot determine struct type name")
	}

	concreteName := typeName
	if aliasedType, ok := cg.typeAliases[typeName]; ok {
		if st, ok2 := aliasedType.(*ast.SimpleType); ok2 {
			concreteName = st.Name
		}
	}

	fields, ok := cg.structFields[concreteName]
	if !ok {
		return nil, fmt.Errorf("struct destructuring: unknown struct type '%s'", concreteName)
	}

	llType, err := cg.tinTypeToLLVM(s.StructType)
	if err != nil {
		return nil, err
	}

	structAlloca := block.NewAlloca(llType)
	block.NewStore(val, structAlloca)

	_ = fields // validated above; actual indices computed via fieldIndex (includes hidden fields)

	for i, fieldName := range s.Names {
		varName := fieldName
		if i < len(s.VarNames) && s.VarNames[i] != "" {
			varName = s.VarNames[i]
		}

		fieldIdx := cg.fieldIndex(concreteName, fieldName)
		if fieldIdx < 0 {
			return nil, fmt.Errorf("struct destructuring: field '%s' not found in struct '%s'", fieldName, concreteName)
		}

		var fieldGep value.Value
		if cg.cLayoutStructs[concreteName] {
			// fieldIdx is native 0-based for cLayoutStructs.
			fieldGep = cg.emitCLayoutFieldPtr(block, structAlloca, concreteName, fieldIdx)
		} else {
			fieldGep = block.NewGetElementPtr(llType, structAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, int64(fieldIdx)))
		}

		if pt, ok := fieldGep.Type().(*irtypes.PointerType); ok {
			fieldVal := block.NewLoad(pt.ElemType, fieldGep)
			alloca := block.NewAlloca(pt.ElemType)
			block.NewStore(fieldVal, alloca)
			// Determine if this field's Tin type is unsigned so `as` casts zext.
			var fieldUnsigned bool

			if tinTypes, ok2 := cg.structFieldTinTypes[concreteName]; ok2 {
				var userIdx int
				if cg.cLayoutStructs[concreteName] {
					// fieldIdx is already the native 0-based index.
					userIdx = fieldIdx
				} else {
					// fieldIdx includes the leading i32 type-id; user fields start at offset 1+vtables.
					userOffset := 1 + len(cg.structVtableOrder[concreteName])
					userIdx = fieldIdx - userOffset
				}

				if userIdx >= 0 && userIdx < len(tinTypes) {
					fieldUnsigned = isUnsignedTinType(tinTypes[userIdx])
				}
			}

			cg.curScope.set(varName, &scopeEntry{val: alloca, isAlloc: true, isUnsigned: fieldUnsigned})
		}
	}

	return block, nil
}

// genTupleDestructDecl handles: let (x, y, ...) = expr
// Extracts fields a, b, c, ... from a Tuple struct value by position.
func (cg *CodeGen) genTupleDestructDecl(block *ir.Block, s *ast.TupleDestructDecl) (*ir.Block, error) {
	// Clear any stale curBlock from prior statements before evaluating the RHS.
	// Without this, stale curBlock values (e.g. set by genEcho inside a preceding
	// if-block's body) misdirect instruction emission to the wrong block.
	cg.curBlock = nil

	// Comma-ok destructure on `t[k]`: tell genIndexExpr to skip its
	// auto-unwrap-or-panic step so the raw (V, bool) tuple flows through
	// to the destructure binding below. See maybeUnwrapIndexTuple.
	prevRaw := cg.indexExprRawTuple
	cg.indexExprRawTuple = true

	val, err := cg.genExpr(block, s.Value)
	cg.indexExprRawTuple = prevRaw

	if err != nil {
		return nil, err
	}

	// Update block if genExpr advanced it (e.g. await generates a new resume block).
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	cg.curBlock = nil // clear so subsequent statements don't see a stale value

	if val == nil {
		return block, nil
	}

	concreteName := structNameFromValue(val)
	if concreteName == "" {
		return nil, fmt.Errorf("tuple destructuring: expected a Tuple struct value, got %s", fmtArgType(val.Type()))
	}

	llType, ok := cg.structTypes[concreteName]
	if !ok {
		return nil, fmt.Errorf("tuple destructuring: unknown struct type '%s'", concreteName)
	}

	structAlloca := block.NewAlloca(llType)
	block.NewStore(val, structAlloca)

	// Detect whether the source is a call to a heap-promoting function.
	// Any pointer-type field of the destructured tuple is then treated as
	// caller-owned (isHeapOwned), matching the semantics of returning
	// &Tuple{a: val, b: heap_ptr} from the callee.
	heapPromotingSource := false

	if callInst, isCall := val.(*ir.InstCall); isCall {
		if callee, isFn := callInst.Callee.(*ir.Func); isFn {
			heapPromotingSource = cg.heapPromotingFns[callee.Name()]
		}
	}

	// Tuple fields are named a, b, c, ... (alphabet by position).
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	userOff := cg.userFieldOffset(concreteName)

	for i, name := range s.Names {
		if i >= len(letters) {
			break
		}

		fieldName := letters[i]

		fieldIdx := cg.fieldIndex(concreteName, fieldName)
		if fieldIdx < 0 {
			// Fall back to positional
			fieldIdx = userOff + i
		}

		if fieldIdx >= len(llType.Fields) {
			break
		}

		fieldType := llType.Fields[fieldIdx]
		fieldGep := block.NewGetElementPtr(llType, structAlloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(fieldIdx)))
		fieldVal := block.NewLoad(fieldType, fieldGep)
		alloca := block.NewAlloca(fieldType)
		block.NewStore(fieldVal, alloca)

		isHeapOwned := false
		heapOwnedDepth := 0

		if heapPromotingSource {
			depth := pointerChainDepth(fieldType)
			if depth > 0 {
				isHeapOwned = true
				heapOwnedDepth = depth
			}
		}

		cg.curScope.set(name, &scopeEntry{val: alloca, isAlloc: true, isHeapOwned: isHeapOwned, heapOwnedDepth: heapOwnedDepth})
	}

	return block, nil
}
