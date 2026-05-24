package codegen

// atoms.go - compile-time atom registration and IR generation for the
// atom type (%__atom = type { i32 }).
//
// Each unique atom name is assigned a CRC32 code at compile time.  Collisions
// are resolved by incrementing the code until a free slot is found.  The
// global @__tin_atom_table (array of {i32, i8*} pairs) and helper functions
// __tin_atom_to_string / __tin_string_to_atom are emitted at the end of
// Generate() once all atoms are known.

import (
	"hash/crc32"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// registerAtom records the atom name and returns its CRC32 code.
// Collisions are resolved by incrementing the code until a unique slot is found.
func (cg *CodeGen) registerAtom(name string) int32 {
	if code, ok := cg.atomCodes[name]; ok {
		return code
	}

	code := int32(crc32.ChecksumIEEE([]byte(name)))
	for {
		if existing, ok := cg.atomCodeToName[code]; !ok || existing == name {
			break
		}

		code++
	}

	cg.atomCodes[name] = code
	cg.atomCodeToName[code] = name
	cg.atomOrder = append(cg.atomOrder, name)

	return code
}

// atomConstant returns a compile-time constant %__atom { i32 code } value.
func (cg *CodeGen) atomConstant(code int32) value.Value {
	return constant.NewStruct(cg.atomType, constant.NewInt(irtypes.I32, int64(code)))
}

// extractAtomCode extracts the i32 CRC32 code field from a %__atom value.
func (cg *CodeGen) extractAtomCode(block *ir.Block, atomVal value.Value) value.Value {
	alloca := block.NewAlloca(cg.atomType)
	block.NewStore(atomVal, alloca)
	gep := block.NewGetElementPtr(cg.atomType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	return block.NewLoad(irtypes.I32, gep)
}

// ensureAtomToString lazily declares __tin_atom_to_string(i32) {i8*,i64}.
// The body is filled in by emitAtomTable() at the end of Generate().
func (cg *CodeGen) ensureAtomToString() *ir.Func {
	if cg.atomToStrFn != nil {
		return cg.atomToStrFn
	}

	cg.atomToStrFn = cg.mod.NewFunc("__tin_atom_to_string", stringFatPtrType(),
		ir.NewParam("code", irtypes.I32))

	return cg.atomToStrFn
}

// ensureStringToAtom lazily declares __tin_string_to_atom(i8*) %__atom.
// The body is filled in by emitAtomTable() at the end of Generate().
func (cg *CodeGen) ensureStringToAtom() *ir.Func {
	if cg.strToAtomFn != nil {
		return cg.strToAtomFn
	}

	cg.strToAtomFn = cg.mod.NewFunc("__tin_string_to_atom", cg.atomType,
		ir.NewParam("ptr", irtypes.I8Ptr))

	return cg.strToAtomFn
}

// ensureRtAtomToStr lazily declares the C runtime helper
// _tin_rt_atom_to_str(i32) i8* (returns NULL when not found).
func (cg *CodeGen) ensureRtAtomToStr() *ir.Func {
	return cg.ensureExternDecl("_tin_rt_atom_to_str", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("code", irtypes.I32)}, false)
}

// ensureLearnAtom lazily declares the C runtime helper
// _tin_learn_atom(i8*) i32 (computes CRC32, stores in runtime table).
func (cg *CodeGen) ensureLearnAtom() *ir.Func {
	return cg.ensureExternDecl("_tin_learn_atom", irtypes.I32,
		[]*ir.Param{ir.NewParam("str", irtypes.I8Ptr)}, false)
}

// ensureLearnAtomHandover lazily declares _tin_learn_atom_handover(i8*) i32.
// Like _tin_learn_atom but takes ownership of (and frees) str after use.
func (cg *CodeGen) ensureLearnAtomHandover() *ir.Func {
	return cg.ensureExternDecl("_tin_learn_atom_handover", irtypes.I32,
		[]*ir.Param{ir.NewParam("str", irtypes.I8Ptr)}, false)
}

// ensureHandoverFree lazily declares _tin_handover_free(i8*) void.
// Frees ptr if it was malloc'd; no-op for static/stack/NULL pointers.
func (cg *CodeGen) ensureHandoverFree() *ir.Func {
	return cg.ensureExternDecl("_tin_handover_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
}

// ensureStringToAtomHandover lazily declares the LLVM IR helper
// __tin_string_to_atom_handover(i8*) %__atom.
// Like __tin_string_to_atom but takes ownership of the input char* (frees it
// after the atom code is resolved, using _tin_handover_free / _tin_learn_atom_handover).
func (cg *CodeGen) ensureStringToAtomHandover() *ir.Func {
	if cg.strToAtomHandoverFn != nil {
		return cg.strToAtomHandoverFn
	}

	cg.strToAtomHandoverFn = cg.mod.NewFunc("__tin_string_to_atom_handover", cg.atomType,
		ir.NewParam("str", irtypes.I8Ptr))

	return cg.strToAtomHandoverFn
}

// emitAtomTable emits @__tin_atom_table and @__tin_atom_count globals, then
// fills in the bodies of any lazily-declared atom helper functions.
// Called at the end of Generate() after all atoms are registered.
func (cg *CodeGen) emitAtomTable() {
	n := int64(len(cg.atomOrder))
	entryType := irtypes.NewStruct(irtypes.I32, irtypes.I8Ptr)
	tableArrType := irtypes.NewArray(uint64(n), entryType)

	// Build table entries: each entry is {i32 code, i8* name}.
	entries := make([]constant.Constant, n)

	for i, name := range cg.atomOrder {
		code := cg.atomCodes[name]
		// String is the bare atom name (no leading apostrophe).
		strPtr := cg.newGlobalString(name).(constant.Constant)
		entries[i] = constant.NewStruct(entryType,
			constant.NewInt(irtypes.I32, int64(code)),
			strPtr,
		)
	}

	// Emit @__tin_atom_table.
	var tableConst constant.Constant
	if n > 0 {
		tableConst = constant.NewArray(tableArrType, entries...)
	} else {
		tableConst = constant.NewArray(tableArrType)
	}

	tableGlobal := cg.mod.NewGlobalDef("__tin_atom_table", tableConst)
	tableGlobal.Immutable = true

	// Emit @__tin_atom_count.
	countGlobal := cg.mod.NewGlobalDef("__tin_atom_count", constant.NewInt(irtypes.I64, n))
	countGlobal.Immutable = true

	// Fill in function bodies if they were requested during codegen.
	if cg.atomToStrFn != nil {
		cg.buildAtomToStringBody(cg.atomToStrFn, tableGlobal, countGlobal, tableArrType)
	}

	if cg.strToAtomFn != nil {
		cg.buildStringToAtomBody(cg.strToAtomFn, tableGlobal, countGlobal, tableArrType)
	}

	if cg.strToAtomHandoverFn != nil {
		cg.buildStringToAtomHandoverBody(cg.strToAtomHandoverFn, tableGlobal, countGlobal, tableArrType)
	}
}

// buildAtomToStringBody generates the body of __tin_atom_to_string.
// It iterates @__tin_atom_table looking for the matching code and returns
// the corresponding {i8*, i64} string fat-pointer (with leading apostrophe).
func (cg *CodeGen) buildAtomToStringBody(fn *ir.Func, tableGlobal *ir.Global, countGlobal *ir.Global, tableArrType *irtypes.ArrayType) {
	strType := stringFatPtrType()
	codeParam := fn.Params[0]
	zeroStr := constant.NewStruct(strType,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewInt(irtypes.I64, 0),
		constant.NewInt(irtypes.I64, 0),
	)

	entry := fn.NewBlock("entry")

	// Even when the static table is empty, the runtime atom table can
	// still hold names registered through _tin_learn_atom (e.g. by the
	// stacktrace runtime when no source-level atom literals exist), so
	// the static-miss block (now renamed `static.miss`) remains the
	// only return path even in that case.
	staticMiss := fn.NewBlock("static.miss")
	i32z := constant.NewInt(irtypes.I32, 0)
	i32o := constant.NewInt(irtypes.I32, 1)

	if len(cg.atomOrder) == 0 {
		entry.NewBr(staticMiss)
	} else {
		loopHeader := fn.NewBlock("loop.header")
		loopBody := fn.NewBlock("loop.body")
		found := fn.NewBlock("found")
		loopCont := fn.NewBlock("loop.continue")

		// entry: load count, init i = 0
		countVal := entry.NewLoad(irtypes.I64, countGlobal)
		iAlloca := entry.NewAlloca(irtypes.I64)
		entry.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
		entry.NewBr(loopHeader)

		// loop.header: if i == count goto exit else goto body
		iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
		done := loopHeader.NewICmp(enum.IPredEQ, iVal, countVal)
		loopHeader.NewCondBr(done, staticMiss, loopBody)

		// loop.body: compare table[i].code with input code
		i64z := constant.NewInt(irtypes.I64, 0)
		gepCode := loopBody.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32z)
		entryCode := loopBody.NewLoad(irtypes.I32, gepCode)
		match := loopBody.NewICmp(enum.IPredEQ, entryCode, codeParam)
		loopBody.NewCondBr(match, found, loopCont)

		// found: load table[i].str, compute strlen, build fat-ptr, return
		gepStr := found.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32o)
		strPtr := found.NewLoad(irtypes.I8Ptr, gepStr)
		length := found.NewCall(cg.ensureStrlenDecl(), strPtr)
		fatAlloca := found.NewAlloca(strType)
		gep0 := found.NewGetElementPtr(strType, fatAlloca, i32z, i32z)
		found.NewStore(strPtr, gep0)
		gep1 := found.NewGetElementPtr(strType, fatAlloca, i32z, i32o)
		found.NewStore(length, gep1)
		found.NewRet(found.NewLoad(strType, fatAlloca))

		// loop.continue: i++, back to header
		iNext := loopCont.NewAdd(iVal, constant.NewInt(irtypes.I64, 1))
		loopCont.NewStore(iNext, iAlloca)
		loopCont.NewBr(loopHeader)
	}

	// Local alias so the rest of the function reads naturally.
	loopExit := staticMiss
	rtPtr := loopExit.NewCall(cg.ensureRtAtomToStr(), codeParam)
	rtFound := fn.NewBlock("rt.found")
	retZero := fn.NewBlock("ret.zero")
	isNull := loopExit.NewICmp(enum.IPredEQ, rtPtr, constant.NewNull(irtypes.I8Ptr))
	loopExit.NewCondBr(isNull, retZero, rtFound)

	// rt.found: build fat-ptr from runtime string pointer.  Runtime
	// strings live in the atom table (immortal storage), so cap = -1.
	rtLen := rtFound.NewCall(cg.ensureStrlenDecl(), rtPtr)
	borrowed := constant.NewInt(irtypes.I64, -1)
	rtFound.NewRet(cg.buildFatArrayValue(rtFound, irtypes.I8, rtPtr, rtLen, borrowed))

	// ret.zero: return zeroinitializer
	exitAlloca := retZero.NewAlloca(strType)
	retZero.NewStore(zeroStr, exitAlloca)
	retZero.NewRet(retZero.NewLoad(strType, exitAlloca))
}

// buildStringToAtomBody generates the body of __tin_string_to_atom.
// It iterates @__tin_atom_table comparing via strcmp and returns the
// corresponding %__atom, or zeroinitializer if not found.
func (cg *CodeGen) buildStringToAtomBody(fn *ir.Func, tableGlobal *ir.Global, countGlobal *ir.Global, tableArrType *irtypes.ArrayType) {
	ptrParam := fn.Params[0]
	zeroAtom := constant.NewStruct(cg.atomType, constant.NewInt(irtypes.I32, 0))

	entry := fn.NewBlock("entry")

	// Even when the static table is empty, runtime atoms can still come
	// in via _tin_learn_atom (e.g. the libunwind-backed stacktrace
	// runtime registers atoms whose names contain colons / pluses that
	// don't appear in the source as literals). Skip the static loop and
	// drop straight into the rt.miss block in that case so the runtime
	// fallback still produces the right code.
	staticMiss := fn.NewBlock("static.miss")
	i32z := constant.NewInt(irtypes.I32, 0)

	if len(cg.atomOrder) == 0 {
		entry.NewBr(staticMiss)
	} else {
		loopHeader := fn.NewBlock("loop.header")
		loopBody := fn.NewBlock("loop.body")
		found := fn.NewBlock("found")
		loopCont := fn.NewBlock("loop.continue")

		// entry
		countVal := entry.NewLoad(irtypes.I64, countGlobal)
		iAlloca := entry.NewAlloca(irtypes.I64)
		entry.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
		entry.NewBr(loopHeader)

		// loop.header
		iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
		done := loopHeader.NewICmp(enum.IPredEQ, iVal, countVal)
		loopHeader.NewCondBr(done, staticMiss, loopBody)

		// loop.body: strcmp(input, table[i].str)
		i64z := constant.NewInt(irtypes.I64, 0)
		i32o := constant.NewInt(irtypes.I32, 1)
		gepStr := loopBody.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32o)
		tableStr := loopBody.NewLoad(irtypes.I8Ptr, gepStr)
		cmpResult := loopBody.NewCall(cg.ensureStrcmp(), ptrParam, tableStr)
		match := loopBody.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
		loopBody.NewCondBr(match, found, loopCont)

		// found: load table[i].code, build %__atom, return
		gepCode := found.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32z)
		code := found.NewLoad(irtypes.I32, gepCode)
		atomAlloca := found.NewAlloca(cg.atomType)
		found.NewStore(zeroAtom, atomAlloca)
		atomGep := found.NewGetElementPtr(cg.atomType, atomAlloca, i32z, i32z)
		found.NewStore(code, atomGep)
		found.NewRet(found.NewLoad(cg.atomType, atomAlloca))

		// loop.continue: i++
		iNext := loopCont.NewAdd(iVal, constant.NewInt(irtypes.I64, 1))
		loopCont.NewStore(iNext, iAlloca)
		loopCont.NewBr(loopHeader)
	}

	// static.miss: fall back to runtime - learn the atom via _tin_learn_atom.
	rtCode := staticMiss.NewCall(cg.ensureLearnAtom(), ptrParam)
	rtAtomAlloca := staticMiss.NewAlloca(cg.atomType)
	staticMiss.NewStore(zeroAtom, rtAtomAlloca)
	rtAtomGep := staticMiss.NewGetElementPtr(cg.atomType, rtAtomAlloca, i32z, i32z)
	staticMiss.NewStore(rtCode, rtAtomGep)
	staticMiss.NewRet(staticMiss.NewLoad(cg.atomType, rtAtomAlloca))
}

// buildStringToAtomHandoverBody generates the body of __tin_string_to_atom_handover.
// Identical to __tin_string_to_atom except:
//   - On static table hit: frees the input char* via _tin_handover_free (it was malloc'd).
//   - On static miss: delegates to _tin_learn_atom_handover which takes ownership and
//     frees the input string internally.
func (cg *CodeGen) buildStringToAtomHandoverBody(fn *ir.Func, tableGlobal *ir.Global, countGlobal *ir.Global, tableArrType *irtypes.ArrayType) {
	ptrParam := fn.Params[0]
	zeroAtom := constant.NewStruct(cg.atomType, constant.NewInt(irtypes.I32, 0))
	i64z := constant.NewInt(irtypes.I64, 0)
	i32z := constant.NewInt(irtypes.I32, 0)
	i32o := constant.NewInt(irtypes.I32, 1)

	entry := fn.NewBlock("entry")

	if len(cg.atomOrder) == 0 {
		// No static atoms: delegate directly to _tin_learn_atom_handover.
		rtCode := entry.NewCall(cg.ensureLearnAtomHandover(), ptrParam)
		retAlloca := entry.NewAlloca(cg.atomType)
		entry.NewStore(zeroAtom, retAlloca)
		retGep := entry.NewGetElementPtr(cg.atomType, retAlloca, i32z, i32z)
		entry.NewStore(rtCode, retGep)
		entry.NewRet(entry.NewLoad(cg.atomType, retAlloca))

		return
	}

	loopHeader := fn.NewBlock("loop.header")
	loopBody := fn.NewBlock("loop.body")
	found := fn.NewBlock("found")
	loopCont := fn.NewBlock("loop.continue")
	loopExit := fn.NewBlock("loop.exit")

	// entry
	countVal := entry.NewLoad(irtypes.I64, countGlobal)
	iAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)
	entry.NewBr(loopHeader)

	// loop.header
	iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
	done := loopHeader.NewICmp(enum.IPredEQ, iVal, countVal)
	loopHeader.NewCondBr(done, loopExit, loopBody)

	// loop.body: strcmp(input, table[i].str)
	gepStr := loopBody.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32o)
	tableStr := loopBody.NewLoad(irtypes.I8Ptr, gepStr)
	cmpResult := loopBody.NewCall(cg.ensureStrcmp(), ptrParam, tableStr)
	match := loopBody.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	loopBody.NewCondBr(match, found, loopCont)

	// found: free the input (it was malloc'd, we own it), then return atom.
	gepCode := found.NewGetElementPtr(tableArrType, tableGlobal, i64z, iVal, i32z)
	code := found.NewLoad(irtypes.I32, gepCode)
	found.NewCall(cg.ensureHandoverFree(), ptrParam)
	atomAlloca := found.NewAlloca(cg.atomType)
	found.NewStore(zeroAtom, atomAlloca)
	atomGep := found.NewGetElementPtr(cg.atomType, atomAlloca, i32z, i32z)
	found.NewStore(code, atomGep)
	found.NewRet(found.NewLoad(cg.atomType, atomAlloca))

	// loop.continue: i++
	iNext := loopCont.NewAdd(iVal, constant.NewInt(irtypes.I64, 1))
	loopCont.NewStore(iNext, iAlloca)
	loopCont.NewBr(loopHeader)

	// static miss: delegate to _tin_learn_atom_handover (takes ownership, frees str).
	rtCode := loopExit.NewCall(cg.ensureLearnAtomHandover(), ptrParam)
	rtAtomAlloca := loopExit.NewAlloca(cg.atomType)
	loopExit.NewStore(zeroAtom, rtAtomAlloca)
	rtAtomGep := loopExit.NewGetElementPtr(cg.atomType, rtAtomAlloca, i32z, i32z)
	loopExit.NewStore(rtCode, rtAtomGep)
	loopExit.NewRet(loopExit.NewLoad(cg.atomType, rtAtomAlloca))
}
