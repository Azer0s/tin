package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genFieldAccess(block *ir.Block, e *ast.FieldAccess) (value.Value, error) {
	if _, isNil := e.Expr.(*ast.NilLit); isNil {
		return nil, cg.nodeErr(e, "field access on nil literal")
	}

	// Check if this is an enum member access: EnumName.Member or pkg::EnumName.Member
	var enumBaseName string

	switch base := e.Expr.(type) {
	case *ast.Identifier:
		enumBaseName = base.Name
	case *ast.ScopeAccess:
		// pkg::EnumName.Member - use the last path element as the enum name.
		if len(base.Path) > 0 {
			enumBaseName = base.Path[len(base.Path)-1]
		}
	}

	if enumBaseName != "" {
		key := enumBaseName + "." + e.Field
		if val, ok2 := cg.enumValues[key]; ok2 {
			baseType := cg.enumTypeFor(CanonKey(enumBaseName))
			if it, ok3 := baseType.(*irtypes.IntType); ok3 {
				return constant.NewInt(it, val), nil
			}
			// Atom enum: wrap i32 code in %__atom struct.
			if isAtomType(baseType) {
				return cg.atomConstant(int32(val)), nil
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
	// Save the pre-auto-deref pointer so the bound-method fallback below
	// can recover it when the FieldAccess resolves to `x.method` on a
	// heap-allocated receiver -- the auto-deref otherwise loads the
	// value and the bound-method codegen copies it to a fresh stack
	// alloca, leaking the heap block on every capture.
	preDerefObj := obj
	preDerefType := objType
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

	_ = preDerefType

	// Handle .len on dynamic arrays {T*, i64} and strings {i8*, i64}.
	if e.Field == "len" && (isFatArrayPtr(objType) || isStringType(objType)) {
		return block.NewExtractValue(obj, 1), nil
	}

	structName := cg.typeNameOf(objType)

	// Native union field access: bitcast storage to member type and load.
	if ud, isNative := cg.nativeUnionDecls[structName]; isNative {
		for _, m := range ud.Members {
			if m.FieldName == e.Field {
				memberLLVM, err2 := cg.tinTypeToLLVM(m.Type)
				if err2 != nil {
					return nil, err2
				}

				alloca := block.NewAlloca(objType)
				block.NewStore(obj, alloca)
				storageGEP := block.NewGetElementPtr(objType, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				memberPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(memberLLVM))

				return block.NewLoad(memberLLVM, memberPtr), nil
			}
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", cg.diagStructName(structName), e.Field)
	}

	// Handle field access on %S.native values: embedded cLayoutStruct fields.
	// These arise when reading a cLayoutStruct field that itself is a cLayoutStruct
	// (e.g. outer_t.a where a is inner_t, both cLayoutStructs). We already have the
	// native value; GEP directly without going through c_data_ptr.
	if strings.HasSuffix(structName, ".native") {
		baseName := strings.TrimSuffix(structName, ".native")

		fieldIdx := cg.nativeFieldIndex(baseName, e.Field)
		if fieldIdx < 0 {
			return nil, cg.nodeErr(e, "unknown field %s.%s", cg.diagStructName(structName), e.Field)
		}

		nativeSt := cg.nativeStructTypes[baseName]
		if nativeSt != nil {
			alloca := block.NewAlloca(nativeSt)
			block.NewStore(obj, alloca)

			gep := block.NewGetElementPtr(nativeSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			if fieldIdx < len(nativeSt.Fields) {
				return block.NewLoad(nativeSt.Fields[fieldIdx], gep), nil
			}
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", cg.diagStructName(structName), e.Field)
	}

	if cg.cLayoutStructs[structName] {
		// cLayoutStruct: store to alloca then access through c_data_ptr.
		alloca := block.NewAlloca(objType)
		block.NewStore(obj, alloca)

		fieldIdx := cg.nativeFieldIndex(structName, e.Field)
		if fieldIdx < 0 {
			return nil, cg.nodeErr(e, "unknown field %s.%s", cg.diagStructName(structName), e.Field)
		}

		gep := cg.emitCLayoutFieldPtr(block, alloca, structName, fieldIdx)

		nativeSt := cg.nativeStructTypes[structName]
		if nativeSt != nil && fieldIdx < len(nativeSt.Fields) {
			return block.NewLoad(nativeSt.Fields[fieldIdx], gep), nil
		}

		return block.NewLoad(irtypes.I64, gep), nil
	}

	fieldIdx := cg.fieldIndex(structName, e.Field)
	if fieldIdx < 0 {
		// Not a struct field -- check if it is a bound method reference.
		// `f.method` where f is of struct type Foo synthesizes a closure that
		// captures the receiver and calls Foo_method(receiver, args...).
		// Pass the pre-auto-deref pointer so anon receivers like
		// `(&counter{}).add` capture the heap pointer rather than a
		// fresh stack copy that orphans the heap block.
		recvObj := obj
		if _, isPtr := preDerefObj.Type().(*irtypes.PointerType); isPtr {
			recvObj = preDerefObj
		}

		if bm, err2 := cg.genBoundMethod(block, e.Expr, recvObj, structName, e.Field); err2 == nil && bm != nil {
			return bm, nil
		}

		return nil, cg.nodeErr(e, "unknown field %s.%s", cg.diagStructName(structName), e.Field)
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
	// `arr[lo..hi]` is the canonical range-slice form -- routes to
	// the unified slice helper that copies into a fresh `_tin_rc_alloc`'d
	// buffer for fat arrays, raw pointers, and fixed-size arrays.
	if bin, ok := e.Index.(*ast.BinExpr); ok && bin.Op == ".." {
		return cg.genPtrRangeSlice(block, e.Expr, bin.Left, bin.Right)
	}

	if length, ok := cg.staticArrayLen(e.Expr); ok {
		cg.checkConstIndexBounds(e, length)
	}

	// For addressable fixed-size arrays: GEP directly into the original alloca
	// without loading/copying the entire array. This is critical for arrays
	// accessed inside loops - the load+alloca+store path allocates N*sizeof(T)
	// bytes on the stack on every iteration, which is never freed until the
	// function returns, causing a stack overflow over time.
	if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil && arrPtr != nil {
		if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
			if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 {
				idx, err3 := cg.genExpr(block, e.Index)
				if err3 != nil {
					return nil, err3
				}

				if idx == nil {
					return nil, nil
				}

				idx = cg.coerce(block, idx, irtypes.I64)
				gep := block.NewGetElementPtr(at, arrPtr,
					constant.NewInt(irtypes.I32, 0), idx)

				return block.NewLoad(at.ElemType, gep), nil
			}
		}
	}

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

	// SIMD vector: extractelement
	if _, ok := arrType.(*irtypes.VectorType); ok {
		idx32 := cg.coerce(block, idx, irtypes.I32)

		return block.NewExtractElement(arr, idx32), nil
	}

	switch at := arrType.(type) {
	case *irtypes.StructType:
		// Fat-ptr: `{T*, i64}` (string) or `{T*, i64, i64}` (dynamic array).
		// Either shape is indexed by extracting the data pointer and
		// GEPing into it; the cap slot is irrelevant for read access.
		if len(at.Fields) == 2 || len(at.Fields) == 3 {
			elemPtrType := at.Fields[0]

			dataPtr := block.NewExtractValue(arr, 0)
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

	// User struct (or *Struct) receiver: dispatch to ::index trait method
	// when the struct implements index[K, R]. Mirror of dispatchBinOp's
	// path - look up an op-trait impl keyed by (structName, "index")
	// whose param type matches the index value, then emit the call.
	//
	// Comma-ok return convention (recommended): impls return a 2-tuple
	// `(V, bool)` where the bool reports whether the key was present.
	// At a tuple-destructure call site (`let (v, ok) = t[k]`) the raw
	// tuple is passed through. At any other call site, codegen auto-
	// unwraps: emits `if !ok: panic("...")` and substitutes V. Impls
	// that return plain V (no comma-ok) are also accepted - value
	// flows through unchanged.
	if structName := cg.structNameForReceiver(arrType); structName != "" {
		if fn := cg.lookupOpMethod(structName, "index", []irtypes.Type{idx.Type()}); fn != nil {
			result, derr := cg.emitOpDispatch(block, fn, arr, []value.Value{idx})
			if derr != nil {
				return nil, derr
			}

			return cg.maybeUnwrapIndexTuple(block, e, result)
		}

		return nil, cg.nodeErr(e,
			"type %s has no `::index` impl for index of type %s; declare `fn ::index(this %s, k %s) (V, bool)`",
			cg.tinTypeDisplay(arrType), cg.tinTypeDisplay(idx.Type()),
			cg.tinTypeDisplay(arrType), cg.tinTypeDisplay(idx.Type()))
	}

	return nil, cg.nodeErr(e, "type %s does not support index expressions", arrType)
}

// maybeUnwrapIndexTuple handles the comma-ok return convention from a
// user `::index` impl. If `result` is a 2-field struct shaped like
// `(V, bool)`, the function:
//
//   - returns it as-is when codegen is currently inside a tuple-
//     destructure VarDecl (cg.indexExprRawTuple); the destructure
//     step will bind both halves.
//   - otherwise extracts field 0 (V) and field 1 (bool), emits a
//     branch that panics with a descriptive message when the bool
//     is false, and returns V on the success path.
//
// If `result` is not shaped like `(V, bool)` (e.g. an impl that
// returns plain V without comma-ok), it's returned unchanged.
func (cg *CodeGen) maybeUnwrapIndexTuple(block *ir.Block, e *ast.IndexExpr, result value.Value) (value.Value, error) {
	if result == nil {
		return nil, nil
	}

	// Tin tuples are `{ i32 type_tag, T1, T2 }` - the (V, bool) pair
	// lives at fields 1 and 2.
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 3 {
		return result, nil
	}

	okField := st.Fields[2]
	if it, isInt := okField.(*irtypes.IntType); !isInt || it.BitSize != 1 {
		return result, nil
	}

	if cg.indexExprRawTuple {
		return result, nil
	}

	val := block.NewExtractValue(result, 1)
	okVal := block.NewExtractValue(result, 2)

	panicBlock := cg.newBlock("idx.miss")
	contBlock := cg.newBlock("idx.ok")

	block.NewCondBr(okVal, contBlock, panicBlock)

	msgPtr := cg.newGlobalString(indexMissMessage(e))
	panicBlock.NewCall(cg.ensurePanicFn(), msgPtr)
	panicBlock.NewUnreachable()

	cg.curBlock = contBlock

	return val, nil
}

// indexMissMessage formats the panic string for an unwrapped index miss.
// Includes the AST source position to make the source line obvious.
func indexMissMessage(e *ast.IndexExpr) string {
	pos := e.Pos()

	return fmt.Sprintf("index miss at %d:%d (no `(_, ok)` destructure to handle absent key)",
		pos.Line, pos.Col)
}

// resolveColoredCallee picks the $colored variant of a callee when
// the current body is in cooperative context (cg.inCoroFn -- true
// inside $coro and $colored emissions) and the callee has a $colored
// variant available in scope.  Falls through to the plain callee
// otherwise.  Used at the named-call site in genCallExpr.
//
// The colored variant has the same signature as the plain sync entry
// (returns T, takes the same params), so the surrounding call-emission
