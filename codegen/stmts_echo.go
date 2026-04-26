package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// exprByteArrayElem returns the element type name ("byte", "u8", "char") when
// the AST expression is statically known to be a [byte]/[u8]/[char] fat array,
// and "" otherwise.
func (cg *CodeGen) exprByteArrayElem(node ast.Node) string {
	switch n := node.(type) {
	case *ast.AsExpr:
		return byteArrayElemType(n.Type)
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.byteArrayElem
		}
	}

	return ""
}

// exprByte8Type returns the Tin type name for an 8-bit or 128-bit scalar
// expression: "char", "byte", "u8", "i8", "i128", "u128", or "f128".
// Returns "" for all other types.
// Handles identifiers (scope lookup), function parameters, and struct field
// accesses (e.g. this.age where age is declared u8).
func (cg *CodeGen) exprByte8Type(node ast.Node) string {
	switch n := node.(type) {
	case *ast.AsExpr:
		if s := scalar8BitTypeName(n.Type); s != "" {
			return s
		}

		return scalar128BitTypeName(n.Type)
	case *ast.ScopeAccess:
		joined := strings.Join(n.Path, ".")
		if se, ok := cg.curScope.lookup(joined); ok {
			return se.scalarTypeName
		}
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.scalarTypeName
		}
	case *ast.FieldAccess:
		if ident, ok := n.Expr.(*ast.Identifier); ok {
			se, ok2 := cg.curScope.lookup(ident.Name)
			if !ok2 {
				break
			}

			// se.val is an alloca; its element type is the struct (possibly via *)
			var elemT irtypes.Type
			if pt, ok3 := se.val.Type().(*irtypes.PointerType); ok3 {
				elemT = pt.ElemType
				// Handle pointer-receiver: *Struct -> Struct
				if pt2, ok4 := elemT.(*irtypes.PointerType); ok4 {
					elemT = pt2.ElemType
				}
			}

			structName := cg.typeNameOf(elemT)
			if structName == "" {
				break
			}

			fields := cg.structFields[structName]
			tinTypes := cg.structFieldTinTypes[structName]

			for i, fname := range fields {
				if fname == n.Field && i < len(tinTypes) {
					return scalar8BitTypeName(tinTypes[i])
				}
			}
		}
	}

	return ""
}

// exprElemIsUnsigned returns true when the AST expression produces an unsigned
// integer value, including array/pointer indexing into unsigned element types.
// Used by genAsExpr to choose zext vs sext when widening integer values.
func (cg *CodeGen) exprElemIsUnsigned(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.isUnsigned
		}
	case *ast.ScopeAccess:
		joined := strings.Join(n.Path, ".")
		if se, ok := cg.curScope.lookup(joined); ok {
			return se.isUnsigned
		}
	case *ast.AsExpr:
		return isUnsignedTinType(n.Type)
	case *ast.IndexExpr:
		// arr[i] or ptr[i]: check the element type from the array/pointer declaration.
		switch base := n.Expr.(type) {
		case *ast.Identifier:
			if se, ok := cg.curScope.lookup(base.Name); ok {
				if se.tinType != nil {
					switch t := se.tinType.(type) {
					case *ast.PointerType:
						if inner, ok2 := t.Elem.(*ast.SimpleType); ok2 {
							return isUnsignedTinType(inner)
						}
					case *ast.ArrayType:
						if inner, ok2 := t.Elem.(*ast.SimpleType); ok2 {
							return isUnsignedTinType(inner)
						}
					}
				}
			}
		}
	case *ast.BinExpr:
		// Arithmetic/bitwise binary ops propagate unsigned-ness from the left operand.
		// E.g. (s0 ^ s4) as u64 must zext when s0 is u32.
		return cg.exprElemIsUnsigned(n.Left)
	}

	return false
}

// exprIsUnsigned reports whether expr represents an unsigned integer value
// (u16, u32, u64, u128 - not u8/byte which are handled via exprByte8Type).
// Checks scope entries for identifiers and scope-access expressions.
func (cg *CodeGen) exprIsUnsigned(node ast.Node) bool {
	switch n := node.(type) {
	case *ast.Identifier:
		if se, ok := cg.curScope.lookup(n.Name); ok {
			return se.isUnsigned
		}
	case *ast.ScopeAccess:
		joined := strings.Join(n.Path, ".")
		if se, ok := cg.curScope.lookup(joined); ok {
			return se.isUnsigned
		}
	case *ast.AsExpr:
		return isUnsignedTinType(n.Type)
	case *ast.CallExpr:
		// Look up the callee function in scope to find its IR name, then check
		// whether that function was registered with an unsigned return type.
		var calleeName string

		switch c := n.Func.(type) {
		case *ast.Identifier:
			calleeName = c.Name
		case *ast.ScopeAccess:
			calleeName = strings.Join(c.Path, "::")
		}

		if calleeName != "" {
			if se, ok := cg.curScope.lookup(calleeName); ok {
				if f, ok2 := se.val.(*ir.Func); ok2 {
					return cg.funcReturnUnsigned[f.Name()]
				}
			}
		}
	case *ast.BinExpr:
		// Arithmetic/bitwise binary ops propagate unsigned-ness from the left operand.
		return cg.exprIsUnsigned(n.Left)
	}

	return false
}

func (cg *CodeGen) genEcho(block *ir.Block, s *ast.EchoStmt) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	cg.curBlock = block

	val, err := cg.genExpr(block, s.Value)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if val == nil {
		return block, nil
	}

	t := val.Type()
	switch {
	case isAnyType(t):
		return cg.genEchoAny(block, val)

	case irtypes.IsVector(t):
		block, err = cg.genEchoVector(block, val, true)
		if err != nil {
			return nil, err
		}

	case isAtomType(t):
		// Convert atom to its string representation then print.
		code := cg.extractAtomCode(block, val)
		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		ptr := cg.extractStringPtr(block, strFatPtr)
		fmtStr := cg.newGlobalString("'%s\n")
		block.NewCall(printf, fmtStr, ptr)

	case isStringType(t):
		// [byte]/[u8]/[char] arrays share {i8*, i64} layout with string.
		// Dispatch by element type: byte -> hex, u8 -> decimal, char -> %c.
		// Plain strings fall through to %s.
		if elem := cg.exprByteArrayElem(s.Value); elem != "" {
			var perElemFmt string

			switch elem {
			case "byte":
				perElemFmt = "%02x"
			case "u8":
				perElemFmt = "%u"
			default: // "char"
				perElemFmt = "%c"
			}

			var printErr error

			block, printErr = cg.genPrintByteArray(block, val, perElemFmt)
			if printErr != nil {
				return nil, printErr
			}

			block.NewCall(printf, cg.newGlobalString("\n"))

			break
		}
		// Extract data pointer + length and call the escape helper.
		ptr := cg.extractStringPtr(block, val)
		length := cg.extractStringLen(block, val)
		block.NewCall(cg.ensureEchoStringEscaped(), ptr, length)

	case irtypes.IsInt(t):
		it := t.(*irtypes.IntType)

		var fmtStr value.Value
		if it.BitSize == 1 {
			// bool: print "true" or "false" via printf and a select.
			fmtStr = cg.newGlobalString("%s\n")
			trueStr := cg.newGlobalString("true")
			falseStr := cg.newGlobalString("false")
			selected := block.NewSelect(val, trueStr, falseStr)
			block.NewCall(printf, fmtStr, selected)

			return block, nil
		}

		if it.BitSize == 8 {
			// Dispatch format by Tin type: char->%c, byte->%02x, u8/i8->%d
			ext := block.NewZExt(val, irtypes.I32)

			switch cg.exprByte8Type(s.Value) {
			case "char":
				fmtStr = cg.newGlobalString("%c\n")
			case "byte":
				fmtStr = cg.newGlobalString("%02x\n")
			default: // "u8", "i8", ""
				fmtStr = cg.newGlobalString("%d\n")
			}

			block.NewCall(printf, fmtStr, ext)

			return block, nil
		}

		if it.BitSize == 128 {
			// i128/u128: call dedicated runtime function (no printf format for __int128)
			switch cg.exprByte8Type(s.Value) {
			case "u128":
				block.NewCall(cg.ensureEchoU128(), val)
			default: // "i128" or unknown 128-bit int
				block.NewCall(cg.ensureEchoI128(), val)
			}

			return block, nil
		}

		if cg.exprIsUnsigned(s.Value) {
			// Unsigned 16/32/64-bit: zero-extend and print as unsigned decimal.
			fmtStr = cg.newGlobalString("%llu\n")

			var ext value.Value

			if it.BitSize < 64 {
				ext = block.NewZExt(val, irtypes.I64)
			} else {
				ext = val // already i64; no extension needed
			}

			block.NewCall(printf, fmtStr, ext)
		} else {
			fmtStr = cg.newGlobalString("%lld\n")
			ext := cg.coerce(block, val, irtypes.I64)

			block.NewCall(printf, fmtStr, ext)
		}

	case irtypes.IsFloat(t):
		ft := t.(*irtypes.FloatType)

		if ft.Kind == irtypes.FloatKindFP128 {
			// f128: call dedicated runtime function
			block.NewCall(cg.ensureEchoF128(), val)

			return block, nil
		}

		fmtStr := cg.newGlobalString("%g\n")

		var ext value.Value
		if t == irtypes.Double {
			ext = val
		} else {
			ext = block.NewFPExt(val, irtypes.Double)
		}

		block.NewCall(printf, fmtStr, ext)

	case irtypes.IsPointer(t):
		if strVal, ok := cg.callPrintTrait(block, val); ok {
			ptr := cg.extractStringPtr(block, strVal)
			fmtStr := cg.newGlobalString("%s\n")
			block.NewCall(printf, fmtStr, ptr)
			// ARC: ::print returns a fresh string; release it after use.
			cg.emitRelease(block, strVal)
		} else {
			fmtStr := cg.newGlobalString("%p\n")
			block.NewCall(printf, fmtStr, val)
		}

	default:
		// print trait: struct or fat-pointer with a print() method.
		if strVal, ok := cg.callPrintTrait(block, val); ok {
			ptr := cg.extractStringPtr(block, strVal)
			fmtStr := cg.newGlobalString("%s\n")
			block.NewCall(printf, fmtStr, ptr)
			// ARC: ::print returns a fresh string; release it after use.
			cg.emitRelease(block, strVal)

			break
		}
		// Struct or array: Go-style formatting.
		var printErr error

		block, printErr = cg.genPrintValue(block, val)
		if printErr != nil {
			return nil, printErr
		}

		block.NewCall(printf, cg.newGlobalString("\n"))
	}

	// ARC: release fresh RC-tracked values produced by function calls or
	// concatenation that are not stored in a named variable (temporaries).
	// Named variables are released by their scope entry at scope exit.
	if isRCTrackedType(t) && isTemporaryProducer(s.Value) {
		cg.emitRelease(block, val)
	}

	return block, nil
}

// genPrintValue emits printf calls to print val in Go-style format without a
// trailing newline. Structs print as {f1 f2 ...}, arrays as [e1 e2 ...].
func (cg *CodeGen) genPrintValue(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	t := val.Type()

	switch {
	case isStringType(t):
		ptr := cg.extractStringPtr(block, val)
		length := cg.extractStringLen(block, val)
		block.NewCall(cg.ensurePrintStringEscaped(), ptr, length)

	case isAtomType(t):
		code := cg.extractAtomCode(block, val)
		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		ptr := cg.extractStringPtr(block, strFatPtr)
		block.NewCall(printf, cg.newGlobalString("'%s"), ptr)

	case irtypes.IsInt(t):
		it := t.(*irtypes.IntType)
		switch it.BitSize {
		case 1:
			trueStr := cg.newGlobalString("true")
			falseStr := cg.newGlobalString("false")
			chosen := block.NewSelect(val, trueStr, falseStr)
			block.NewCall(printf, cg.newGlobalString("%s"), chosen)
		case 8:
			zext := block.NewZExt(val, irtypes.I32)
			block.NewCall(printf, cg.newGlobalString("%c"), zext)
		case 128:
			// i128/u128: use the cstr helper then %s
			cstr := block.NewCall(cg.ensureI128ToCstr(), val)
			block.NewCall(printf, cg.newGlobalString("%s"), cstr)
		default:
			ext := cg.coerce(block, val, irtypes.I64)
			block.NewCall(printf, cg.newGlobalString("%lld"), ext)
		}

	case irtypes.IsFloat(t):
		ft := t.(*irtypes.FloatType)

		if ft.Kind == irtypes.FloatKindFP128 {
			cstr := block.NewCall(cg.ensureF128ToCstr(), val)
			block.NewCall(printf, cg.newGlobalString("%s"), cstr)

			break
		}

		var ext value.Value
		if t == irtypes.Double {
			ext = val
		} else {
			ext = block.NewFPExt(val, irtypes.Double)
		}

		block.NewCall(printf, cg.newGlobalString("%g"), ext)

	case irtypes.IsVector(t):
		var err error

		block, err = cg.genEchoVector(block, val, false)
		if err != nil {
			return nil, err
		}

	case irtypes.IsPointer(t):
		block.NewCall(printf, cg.newGlobalString("%p"), val)

	case isFatArrayPtr(t):
		var err error

		block, err = cg.genPrintArray(block, val)
		if err != nil {
			return nil, err
		}

	default:
		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			var err error

			block, err = cg.genPrintStruct(block, val, st)
			if err != nil {
				return nil, err
			}

			break
		}

		ext := cg.coerce(block, val, irtypes.I64)
		block.NewCall(printf, cg.newGlobalString("%lld"), ext)
	}

	return block, nil
}

// genPrintStruct emits printf calls to print a named struct in Go style:
// {field1 field2 ...}.
func (cg *CodeGen) genPrintStruct(block *ir.Block, val value.Value, st *irtypes.StructType) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	name := st.Name()
	fieldNames := cg.structFields[name]

	block.NewCall(printf, cg.newGlobalString("{"))

	printed := 0

	// cLayoutStructs: fields live in the native layout pointed to by c_data_ptr.
	if cg.cLayoutStructs[name] {
		nativeSt := cg.nativeStructTypes[name]
		if nativeSt == nil {
			block.NewCall(printf, cg.newGlobalString("}"))

			return block, nil
		}
		// Extract c_data_ptr from the wrapper struct value.
		cDataIdx := cg.cDataPtrIndex(name)
		cDataI8 := block.NewExtractValue(val, uint64(cDataIdx))
		nativePtr := block.NewBitCast(cDataI8, irtypes.NewPointer(nativeSt))

		for i, fieldName := range fieldNames {
			if fieldName == "" || i >= len(nativeSt.Fields) {
				continue
			}

			if printed > 0 {
				block.NewCall(printf, cg.newGlobalString(" "))
			}

			gep := block.NewGetElementPtr(nativeSt, nativePtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			fieldVal := block.NewLoad(nativeSt.Fields[i], gep)

			var err error

			block, err = cg.genPrintValue(block, fieldVal)
			if err != nil {
				return nil, err
			}

			printed++
		}

		block.NewCall(printf, cg.newGlobalString("}"))

		return block, nil
	}

	userOff := cg.userFieldOffset(name)

	for i, fieldName := range fieldNames {
		if fieldName == "" {
			continue
		}

		llIdx := userOff + i
		if llIdx >= len(st.Fields) {
			break
		}

		if printed > 0 {
			block.NewCall(printf, cg.newGlobalString(" "))
		}

		fieldVal := block.NewExtractValue(val, uint64(llIdx))

		var err error

		block, err = cg.genPrintValue(block, fieldVal)
		if err != nil {
			return nil, err
		}

		printed++
	}

	block.NewCall(printf, cg.newGlobalString("}"))

	return block, nil
}

// genPrintArray emits a loop that prints a fat-array value as [e1 e2 ...].
func (cg *CodeGen) genPrintArray(block *ir.Block, val value.Value) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	fatType := val.Type().(*irtypes.StructType)
	elemPtrType := fatType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	// Alloca for loop counter.
	iAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	block.NewCall(printf, cg.newGlobalString("["))

	condBlock := cg.newBlock("print.arr.cond")
	bodyBlock := cg.newBlock("print.arr.body")
	endBlock := cg.newBlock("print.arr.end")

	block.NewBr(condBlock)

	// Condition: i < length
	iVal := condBlock.NewLoad(irtypes.I64, iAlloca)
	cmp := condBlock.NewICmp(enum.IPredSLT, iVal, length)
	condBlock.NewCondBr(cmp, bodyBlock, endBlock)

	// Body: print separator if i > 0, print element, increment i.
	iVal2 := bodyBlock.NewLoad(irtypes.I64, iAlloca)
	isFirst := bodyBlock.NewICmp(enum.IPredEQ, iVal2, constant.NewInt(irtypes.I64, 0))
	spaceStr := cg.newGlobalString(" ")
	emptyStr := cg.newGlobalString("")
	sepStr := bodyBlock.NewSelect(isFirst, emptyStr, spaceStr)
	bodyBlock.NewCall(printf, cg.newGlobalString("%s"), sepStr)

	elemPtr := bodyBlock.NewGetElementPtr(elemType, dataPtr, iVal2)
	elemVal := bodyBlock.NewLoad(elemType, elemPtr)

	var err error

	bodyBlock, err = cg.genPrintValue(bodyBlock, elemVal)
	if err != nil {
		return nil, err
	}

	iNext := bodyBlock.NewAdd(iVal2, constant.NewInt(irtypes.I64, 1))
	bodyBlock.NewStore(iNext, iAlloca)
	bodyBlock.NewBr(condBlock)

	endBlock.NewCall(printf, cg.newGlobalString("]"))

	return endBlock, nil
}

// genPrintByteArray emits a loop that prints a [byte]/[u8]/[char] fat-array as
// [e1 e2 ...] where each element is formatted with perElemFmt (e.g. "%02x",
// "%u", "%c").  The fat-array must have layout {i8*, i64}.
func (cg *CodeGen) genPrintByteArray(block *ir.Block, val value.Value, perElemFmt string) (*ir.Block, error) {
	printf := cg.ensurePrintf()

	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	iAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	block.NewCall(printf, cg.newGlobalString("["))

	condBlock := cg.newBlock("print.bytes.cond")
	bodyBlock := cg.newBlock("print.bytes.body")
	endBlock := cg.newBlock("print.bytes.end")

	block.NewBr(condBlock)

	// Condition: i < length
	iVal := condBlock.NewLoad(irtypes.I64, iAlloca)
	cmp := condBlock.NewICmp(enum.IPredSLT, iVal, length)
	condBlock.NewCondBr(cmp, bodyBlock, endBlock)

	// Body: print space separator (except before first), print element, increment.
	iVal2 := bodyBlock.NewLoad(irtypes.I64, iAlloca)
	isFirst := bodyBlock.NewICmp(enum.IPredEQ, iVal2, constant.NewInt(irtypes.I64, 0))
	spaceStr := cg.newGlobalString(" ")
	emptyStr := cg.newGlobalString("")
	sepStr := bodyBlock.NewSelect(isFirst, emptyStr, spaceStr)
	bodyBlock.NewCall(printf, cg.newGlobalString("%s"), sepStr)

	elemPtr := bodyBlock.NewGetElementPtr(irtypes.I8, dataPtr, iVal2)
	elemVal := bodyBlock.NewLoad(irtypes.I8, elemPtr)
	zext := bodyBlock.NewZExt(elemVal, irtypes.I32)
	bodyBlock.NewCall(printf, cg.newGlobalString(perElemFmt), zext)

	iNext := bodyBlock.NewAdd(iVal2, constant.NewInt(irtypes.I64, 1))
	bodyBlock.NewStore(iNext, iAlloca)
	bodyBlock.NewBr(condBlock)

	endBlock.NewCall(printf, cg.newGlobalString("]"))

	return endBlock, nil
}

func (cg *CodeGen) genEchoVector(block *ir.Block, val value.Value, withNewline bool) (*ir.Block, error) {
	printf := cg.ensurePrintf()
	vt := val.Type().(*irtypes.VectorType)

	block.NewCall(printf, cg.newGlobalString("<"))

	for i := uint64(0); i < vt.Len; i++ {
		if i > 0 {
			block.NewCall(printf, cg.newGlobalString(" "))
		}

		lane := block.NewExtractElement(val, constant.NewInt(irtypes.I32, int64(i)))
		switch {
		case irtypes.IsFloat(lane.Type()):
			var ext value.Value
			if lane.Type() == irtypes.Double {
				ext = lane
			} else {
				ext = block.NewFPExt(lane, irtypes.Double)
			}

			block.NewCall(printf, cg.newGlobalString("%g"), ext)
		default:
			ext := cg.coerce(block, lane, irtypes.I64)
			block.NewCall(printf, cg.newGlobalString("%lld"), ext)
		}
	}

	if withNewline {
		block.NewCall(printf, cg.newGlobalString(">\n"))
	} else {
		block.NewCall(printf, cg.newGlobalString(">"))
	}

	return block, nil
}
