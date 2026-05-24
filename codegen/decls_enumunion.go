package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genEnumDecl(n *ast.EnumDecl) error {
	// Idempotent: skip if already registered (can be called from preregister AND pass 3).
	if cg.isEnumFor(CanonKey(n.Name)) {
		return nil
	}
	// Determine base LLVM type.
	var baseType irtypes.Type = irtypes.I32

	if n.BaseType != nil {
		bt, err := cg.tinTypeToLLVM(n.BaseType)
		if err != nil {
			return err
		}

		baseType = bt
	}

	cg.recordEnum(CanonKey(n.Name), baseType)

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

// genTaggedUnionTypeDecl generates the LLVM layout for a tagged union declared
// via "type u = i8 | string". Layout: { i32 type_id, i8 tag, [maxSize x i8] payload }.
// type_id is a compile-time constant (same pool as structs/data) for any boxing and typeof.
// Tag 0 = first variant, 1 = second, etc.
func (cg *CodeGen) genTaggedUnionTypeDecl(name string, ut *ast.UnionTypeExpr) error {
	var maxSize uint64 = 1

	for _, te := range ut.Types {
		lt, err := cg.tinTypeToLLVM(te)
		if err != nil {
			return err
		}

		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}

	payloadType := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypeFor(CanonKey(name))
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(name)
		cg.recordLLVM(CanonKey(name), st)
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}
	// Assign a compile-time type ID (same pool as structs/data types).
	typeID := cg.nextTypeID
	cg.nextTypeID++
	cg.unionTypeIDs[name] = typeID
	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I8, payloadType}
	cg.unionTypeMembers[name] = ut.Types

	return nil
}

// genUnionDecl generates the LLVM layout for a native C-style union declared
// via "union u = as_i8 i8 | as_string string". Layout: { [maxSize x i8] storage }.
// No tag - members overlap the same memory region.
func (cg *CodeGen) genUnionDecl(n *ast.UnionDecl) error {
	var maxSize uint64 = 1

	for _, m := range n.Members {
		lt, err := cg.tinTypeToLLVM(m.Type)
		if err != nil {
			return err
		}

		if sz := llvmTypeSize(lt); sz > maxSize {
			maxSize = sz
		}
	}

	storageType := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypeFor(CanonKey(n.Name))
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(n.Name)
		cg.recordLLVM(CanonKey(n.Name), st)
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	st.Fields = []irtypes.Type{storageType}
	cg.nativeUnionDecls[n.Name] = n

	return nil
}

// wrapTaggedUnionVariant wraps a value into a tagged union struct.
// Layout: { i32 type_id, i8 tag, [N x i8] payload }. Returns nil if no variant matches.
func (cg *CodeGen) wrapTaggedUnionVariant(block *ir.Block, val value.Value, targetSt *irtypes.StructType, unionName string) value.Value {
	members := cg.unionTypeMembers[unionName]
	tag := int8(-1)
	// First pass: exact type match.
	for i, te := range members {
		lt, err := cg.tinTypeToLLVM(te)
		if err != nil {
			continue
		}

		if lt.Equal(val.Type()) {
			tag = int8(i)

			break
		}
	}
	// Second pass: same size (for int widening), but not float vs int.
	if tag < 0 {
		for i, te := range members {
			lt, err := cg.tinTypeToLLVM(te)
			if err != nil {
				continue
			}

			if irtypes.IsFloat(lt) != irtypes.IsFloat(val.Type()) {
				continue // never conflate float and int variants of same size
			}

			if llvmTypeSize(lt) == llvmTypeSize(val.Type()) {
				tag = int8(i)

				break
			}
		}
	}

	if tag < 0 {
		return nil
	}

	alloca := block.NewAlloca(targetSt)
	// Field 0 = i32 type_id.
	typeIDVal := int32(0)
	if id, ok := cg.unionTypeIDs[unionName]; ok {
		typeIDVal = id
	}

	typeIDGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(typeIDVal)), typeIDGEP)
	// Field 1 = i8 tag.
	tagGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I8, int64(tag)), tagGEP)
	// Field 2 = [N x i8] payload via bitcast.
	payloadGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(val.Type()))
	block.NewStore(val, payloadPtr)

	return block.NewLoad(targetSt, alloca)
}

// wrapNativeUnion stores a value into a native union's storage via bitcast.
// Layout: { [N x i8] storage }.
// The stored value is coerced to the storage size: if val is larger than the
// array, it is truncated to the array's byte length; if smaller, stored as-is.
func (cg *CodeGen) wrapNativeUnion(block *ir.Block, val value.Value, targetSt *irtypes.StructType) value.Value {
	alloca := block.NewAlloca(targetSt)
	storageGEP := block.NewGetElementPtr(targetSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	// Determine storage capacity (array element count = bytes).
	storageBytes := uint64(0)
	if arr, ok := targetSt.Fields[0].(*irtypes.ArrayType); ok {
		storageBytes = arr.Len
	}

	storedVal := val
	// If the value is wider than the storage, truncate to the storage size.
	if storageBytes > 0 {
		valBytes := llvmTypeSize(val.Type())
		if valBytes > storageBytes {
			var storeType irtypes.Type

			switch storageBytes {
			case 1:
				storeType = irtypes.I8
			case 2:
				storeType = irtypes.I16
			case 4:
				storeType = irtypes.I32
			default:
				storeType = irtypes.I64
			}

			if irtypes.IsInt(val.Type()) && irtypes.IsInt(storeType) {
				storedVal = block.NewTrunc(val, storeType)
			}
		}
	}

	valPtr := block.NewBitCast(storageGEP, irtypes.NewPointer(storedVal.Type()))
	block.NewStore(storedVal, valPtr)

	return block.NewLoad(targetSt, alloca)
}

// lookupTemplateFile resolves the source-file path that originally
// declared a generic struct template. Empty string when not found.
