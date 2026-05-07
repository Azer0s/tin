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
// Used by genAsExpr to choose zext vs sext when widening integer values, and
// by genBinExpr to choose urem/udiv/ult etc. when the LHS is unsigned.
//
// Keep CallExpr / ParenExpr handled here - without them, patterns like
// `(fn_returning_u64() % span) as i64` fall through to srem because the
// LHS Tin type can't be inferred from the AST shape alone.
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
	case *ast.CallExpr:
		// Look up the callee in scope and consult the registered return-
		// signedness map, mirroring exprIsUnsigned.
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

// elemPrintfFmt returns the printf format specifier and (optional) coerce
// instruction kind needed to print one element of an array via snprintf.
// Returns ("", _) when the element type is not directly formattable.
func elemPrintfFmt(elemType irtypes.Type) string {
	switch t := elemType.(type) {
	case *irtypes.IntType:
		switch t.BitSize {
		case 1:
			return "%s" // bool: caller must select "true"/"false"
		case 8:
			return "%d" // u8/i8 default; specific char/byte handled separately
		case 16, 32, 64:
			return "%lld"
		}
	case *irtypes.FloatType:
		return "%g"
	case *irtypes.StructType:
		// strings are {i8*, i64}
		if isStringType(t) {
			return "%s"
		}
	case *irtypes.PointerType:
		return "%p"
	}

	return ""
}

// genArrayToFatStr emits IR that builds the Go-style "[e1 e2 ...]" string
// representation of a fat-array value as a fresh _tin_rc_alloc'd fat-string.
// Used for string interpolation of array values ("{arr}"). Returns the new
// fat-string value plus the (possibly updated) current block.
//
// Two-pass strategy:
//
//	pass 1: total = 2 + len + sum(snprintf(NULL, 0, fmt, e_i)) - 1   (brackets, spaces)
//	pass 2: rc_alloc(total+1); fill sequentially via snprintf into the buffer.
func (cg *CodeGen) genArrayToFatStr(block *ir.Block, val value.Value) (value.Value, *ir.Block, error) {
	fatType := val.Type().(*irtypes.StructType)
	elemPtrType := fatType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	elemFmt := elemPrintfFmt(elemType)
	if elemFmt == "" {
		// Not directly formattable; return a placeholder fat-string "[...]".
		return cg.makeFatStrLiteral(block, "[...]"), block, nil
	}

	snprintfFn := cg.ensureSnprintf()
	dataPtr := block.NewExtractValue(val, 0)
	length := block.NewExtractValue(val, 1)

	// Pass 1: measure total length.
	// total starts at 2 (for "[" and "]"). For each element add snprintf-NULL length.
	// Add (len-1) for separator spaces when len > 0.
	totalAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 2), totalAlloca)

	// Add separator-space count: max(len-1, 0) but easier: if len > 0 add (len-1).
	hasAnyBlock := cg.newBlock("arr2str.has_any")
	skipSepBlock := cg.newBlock("arr2str.skip_sep")
	zeroI64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	gtZero := block.NewICmp(enum.IPredSGT, length, zeroI64)
	block.NewCondBr(gtZero, hasAnyBlock, skipSepBlock)

	sepCount := hasAnyBlock.NewSub(length, one64)
	curTot := hasAnyBlock.NewLoad(irtypes.I64, totalAlloca)
	newTot := hasAnyBlock.NewAdd(curTot, sepCount)
	hasAnyBlock.NewStore(newTot, totalAlloca)
	hasAnyBlock.NewBr(skipSepBlock)

	// Measurement loop: for i in 0..len: total += snprintf(NULL, 0, fmt, arr[i])
	measCondBlock := cg.newBlock("arr2str.meas.cond")
	measBodyBlock := cg.newBlock("arr2str.meas.body")
	measEndBlock := cg.newBlock("arr2str.meas.end")

	iAlloca := skipSepBlock.NewAlloca(irtypes.I64)
	skipSepBlock.NewStore(zeroI64, iAlloca)
	skipSepBlock.NewBr(measCondBlock)

	mi := measCondBlock.NewLoad(irtypes.I64, iAlloca)
	mcmp := measCondBlock.NewICmp(enum.IPredSLT, mi, length)
	measCondBlock.NewCondBr(mcmp, measBodyBlock, measEndBlock)

	mi2 := measBodyBlock.NewLoad(irtypes.I64, iAlloca)
	mElemPtr := measBodyBlock.NewGetElementPtr(elemType, dataPtr, mi2)
	mElemVal := measBodyBlock.NewLoad(elemType, mElemPtr)
	mFmtArg, mElemArg := cg.coerceForElemFmt(measBodyBlock, mElemVal, elemType, elemFmt)
	mNeeded := measBodyBlock.NewCall(snprintfFn,
		constant.NewNull(irtypes.I8Ptr), zeroI64, cg.newGlobalString(mFmtArg), mElemArg)
	mNeededI64 := measBodyBlock.NewSExt(mNeeded, irtypes.I64)
	curTot2 := measBodyBlock.NewLoad(irtypes.I64, totalAlloca)
	newTot2 := measBodyBlock.NewAdd(curTot2, mNeededI64)
	measBodyBlock.NewStore(newTot2, totalAlloca)
	miNext := measBodyBlock.NewAdd(mi2, one64)
	measBodyBlock.NewStore(miNext, iAlloca)
	measBodyBlock.NewBr(measCondBlock)

	// Pass 2: allocate buffer of size total+1, fill it.
	totalLen := measEndBlock.NewLoad(irtypes.I64, totalAlloca)
	allocSize := measEndBlock.NewAdd(totalLen, one64)
	buf := measEndBlock.NewCall(cg.ensureRCAlloc(), allocSize)

	// Write opening "[" and initialize cursor.
	posAlloca := measEndBlock.NewAlloca(irtypes.I64)
	measEndBlock.NewStore(zeroI64, posAlloca)

	openBracket := constant.NewInt(irtypes.I8, int64('['))
	measEndBlock.NewStore(openBracket, buf)
	measEndBlock.NewStore(one64, posAlloca)

	// Fill loop: for i in 0..len: write separator if i>0; snprintf into buf+pos.
	fillCondBlock := cg.newBlock("arr2str.fill.cond")
	fillBodyBlock := cg.newBlock("arr2str.fill.body")
	fillSepBlock := cg.newBlock("arr2str.fill.sep")
	fillElemBlock := cg.newBlock("arr2str.fill.elem")
	fillEndBlock := cg.newBlock("arr2str.fill.end")

	measEndBlock.NewStore(zeroI64, iAlloca) // reuse iAlloca for fill loop
	measEndBlock.NewBr(fillCondBlock)

	fi := fillCondBlock.NewLoad(irtypes.I64, iAlloca)
	fcmp := fillCondBlock.NewICmp(enum.IPredSLT, fi, length)
	fillCondBlock.NewCondBr(fcmp, fillBodyBlock, fillEndBlock)

	fi2 := fillBodyBlock.NewLoad(irtypes.I64, iAlloca)
	isFirst := fillBodyBlock.NewICmp(enum.IPredEQ, fi2, zeroI64)
	fillBodyBlock.NewCondBr(isFirst, fillElemBlock, fillSepBlock)

	curPos := fillSepBlock.NewLoad(irtypes.I64, posAlloca)
	sepDest := fillSepBlock.NewGetElementPtr(irtypes.I8, buf, curPos)
	spaceChar := constant.NewInt(irtypes.I8, int64(' '))
	fillSepBlock.NewStore(spaceChar, sepDest)
	posAfterSep := fillSepBlock.NewAdd(curPos, one64)
	fillSepBlock.NewStore(posAfterSep, posAlloca)
	fillSepBlock.NewBr(fillElemBlock)

	curPos2 := fillElemBlock.NewLoad(irtypes.I64, posAlloca)
	dest := fillElemBlock.NewGetElementPtr(irtypes.I8, buf, curPos2)
	remaining := fillElemBlock.NewSub(allocSize, curPos2)
	fElemPtr := fillElemBlock.NewGetElementPtr(elemType, dataPtr, fi2)
	fElemVal := fillElemBlock.NewLoad(elemType, fElemPtr)
	fFmtArg, fElemArg := cg.coerceForElemFmt(fillElemBlock, fElemVal, elemType, elemFmt)
	written := fillElemBlock.NewCall(snprintfFn, dest, remaining, cg.newGlobalString(fFmtArg), fElemArg)
	writtenI64 := fillElemBlock.NewSExt(written, irtypes.I64)
	posAfterElem := fillElemBlock.NewAdd(curPos2, writtenI64)
	fillElemBlock.NewStore(posAfterElem, posAlloca)
	fiNext := fillElemBlock.NewAdd(fi2, one64)
	fillElemBlock.NewStore(fiNext, iAlloca)
	fillElemBlock.NewBr(fillCondBlock)

	// Write closing "]" and NUL.
	endPos := fillEndBlock.NewLoad(irtypes.I64, posAlloca)
	closeDest := fillEndBlock.NewGetElementPtr(irtypes.I8, buf, endPos)
	closeBracket := constant.NewInt(irtypes.I8, int64(']'))
	fillEndBlock.NewStore(closeBracket, closeDest)
	nulPos := fillEndBlock.NewAdd(endPos, one64)
	nulDest := fillEndBlock.NewGetElementPtr(irtypes.I8, buf, nulPos)
	fillEndBlock.NewStore(constant.NewInt(irtypes.I8, 0), nulDest)

	// Build fat-string {buf, total}.
	fatPtrType := stringFatPtrType()
	fatAlloca := fillEndBlock.NewAlloca(fatPtrType)
	ptrGep := fillEndBlock.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	fillEndBlock.NewStore(buf, ptrGep)
	lenGep := fillEndBlock.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	fillEndBlock.NewStore(totalLen, lenGep)

	return fillEndBlock.NewLoad(fatPtrType, fatAlloca), fillEndBlock, nil
}

// coerceForElemFmt prepares an element value for a snprintf call using the
// chosen format specifier. Returns the (possibly transformed) format string
// (caller passes to newGlobalString) and the coerced element argument.
func (cg *CodeGen) coerceForElemFmt(block *ir.Block, elem value.Value, elemType irtypes.Type, fmt string) (string, value.Value) {
	switch t := elemType.(type) {
	case *irtypes.IntType:
		switch t.BitSize {
		case 1:
			truePtr := cg.newGlobalString("true")
			falsePtr := cg.newGlobalString("false")
			selected := block.NewSelect(elem, truePtr, falsePtr)

			return fmt, selected
		case 8:
			ext := block.NewSExt(elem, irtypes.I32)

			return fmt, ext
		case 16, 32:
			ext := block.NewSExt(elem, irtypes.I64)

			return fmt, ext
		case 64:
			return fmt, elem
		}
	case *irtypes.FloatType:
		if t == irtypes.Double {
			return fmt, elem
		}

		ext := block.NewFPExt(elem, irtypes.Double)

		return fmt, ext
	case *irtypes.StructType:
		if isStringType(t) {
			return fmt, cg.extractStringPtr(block, elem)
		}
	}

	return fmt, elem
}

// makeFatStrLiteral emits a fresh fat-string for the given literal s.
// Used as a fallback when an element's type is not directly formattable.
func (cg *CodeGen) makeFatStrLiteral(block *ir.Block, s string) value.Value {
	fatPtrType := stringFatPtrType()
	fatAlloca := block.NewAlloca(fatPtrType)
	ptr := cg.newGlobalString(s)
	ptrGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(ptr, ptrGep)
	lenGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, int64(len(s))), lenGep)

	return block.NewLoad(fatPtrType, fatAlloca)
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
