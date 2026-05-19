package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// isCharPtrPtrType reports whether t is `i8**` -- the C-side shape
// for `char **` / `const char **` arguments.  Used to detect extern
// params that want the array-of-strings marshal.
func isCharPtrPtrType(t irtypes.Type) bool {
	pt, ok := t.(*irtypes.PointerType)
	if !ok {
		return false
	}

	inner, ok2 := pt.ElemType.(*irtypes.PointerType)
	if !ok2 {
		return false
	}

	return inner.ElemType.Equal(irtypes.I8)
}

// needsCstrArrMarshal reports whether `arg`, a Tin fat-array value
// of `[string]` or `[atom]`, needs to be marshaled to a C `char**`
// to match the target param type `tgt`.  Both predicates must hold:
//   - source is a fat-array of `string` or `atom` elements
//   - target is `i8**` (matches what tinTypeToExternLLVM emits for
//     `[string]` / `[atom]` parameters of an extern)
func (cg *CodeGen) needsCstrArrMarshal(arg value.Value, tgt irtypes.Type) bool {
	if !isCharPtrPtrType(tgt) {
		return false
	}

	if !isFatArrayPtr(arg.Type()) {
		return false
	}

	srcSt := arg.Type().(*irtypes.StructType)
	srcElem := srcSt.Fields[0].(*irtypes.PointerType).ElemType

	return isStringType(srcElem) || isAtomType(srcElem)
}

// marshalArrayToCstrArr emits the inline IR to convert a Tin
// `[string]` or `[atom]` fat-array value into a `char**` array
// suitable for handing to a C function that takes `char **xs`.
// No runtime helper call -- the loop runs directly in the caller's
// frame.
//
// Layout: an `i8*[len]` alloca on the caller's stack.  For each
// element i in 0..len, the slot is filled with:
//   - [string]: the TinString's `.ptr` field
//   - [atom]:   the result of __tin_atom_to_string(code).ptr
//
// No null terminator is appended; the C caller must know the
// length out-of-band, mirroring the existing `[i32] -> int32_t*`
// convention.  Returns (i8**, exitBlock); the caller must continue
// IR emission in exitBlock.
func (cg *CodeGen) marshalArrayToCstrArr(block *ir.Block, arg value.Value, isAtom bool) (value.Value, *ir.Block) {
	srcSt := arg.Type().(*irtypes.StructType)
	dataPtr := block.NewExtractValue(arg, 0)
	length := block.NewExtractValue(arg, 1)

	// Stack-allocate the output `char*[len]` array.  Dynamic length
	// via NElems; the slot lives until the enclosing fn returns.
	outAlloca := cg.hoistAlloca(block, irtypes.I8Ptr)
	outAlloca.NElems = length

	// for (i = 0; i < length; i++) out[i] = elementCharPtr(src[i])
	fn := block.Parent
	iAlloca := cg.hoistAlloca(block, irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	header := fn.NewBlock("cstrarr.header")
	body := fn.NewBlock("cstrarr.body")
	exit := fn.NewBlock("cstrarr.exit")

	block.NewBr(header)

	iVal := header.NewLoad(irtypes.I64, iAlloca)
	cond := header.NewICmp(enum.IPredSLT, iVal, length)
	header.NewCondBr(cond, body, exit)

	iVal2 := body.NewLoad(irtypes.I64, iAlloca)
	slotPtr := body.NewGetElementPtr(irtypes.I8Ptr, outAlloca, iVal2)

	var elemCharPtr value.Value

	if isAtom {
		// src[i] is `{i32 code}`.  Extract the code, look up the
		// atom's interned name (a TinString), then take its .ptr.
		atomElemTy := srcSt.Fields[0].(*irtypes.PointerType).ElemType
		atomGep := body.NewGetElementPtr(atomElemTy, dataPtr, iVal2)
		codeGep := body.NewGetElementPtr(atomElemTy, atomGep,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		code := body.NewLoad(irtypes.I32, codeGep)
		strFatPtr := body.NewCall(cg.ensureAtomToString(), code)
		elemCharPtr = body.NewExtractValue(strFatPtr, 0)
	} else {
		// src[i] is `{i8* ptr, i64 len, i64 cap}`.  Take .ptr.
		strElemTy := srcSt.Fields[0].(*irtypes.PointerType).ElemType
		strGep := body.NewGetElementPtr(strElemTy, dataPtr, iVal2)
		ptrGep := body.NewGetElementPtr(strElemTy, strGep,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		elemCharPtr = body.NewLoad(irtypes.I8Ptr, ptrGep)
	}

	body.NewStore(elemCharPtr, slotPtr)
	iNext := body.NewAdd(iVal2, constant.NewInt(irtypes.I64, 1))
	body.NewStore(iNext, iAlloca)
	body.NewBr(header)

	cg.curBlock = exit

	return outAlloca, exit
}
