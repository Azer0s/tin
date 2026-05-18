package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// program is tagged `#interop`. Used by the synthetic-main suppression
// in codegen so library-mode programs don't ship an unwanted main
// symbol that would collide with the C consumer's entry point.
func programHasInteropFunc(stmts []ast.Node) bool {
	for _, node := range stmts {
		if fn, ok := node.(*ast.FuncDecl); ok && hasTag(fn.Tags, "interop") {
			return true
		}
	}

	return false
}

// emitInteropWrappers walks the program for #interop-tagged functions
// and emits a C-callable wrapper for each. The wrapper:
//  1. Calls _tin_runtime_init_once() (idempotent).
//  2. Marshals each argument from its C ABI shape to the Tin shape
//     (string, fat array, bool widening; primitives passthrough).
//  3. Calls the Tin-internal entry point.
//  4. Marshals the return value back to a C-friendly shape.
//  5. Releases any temporary ARC allocations created at the boundary.
func (cg *CodeGen) emitInteropWrappers(stmts []ast.Node) error {
	var wrappers []*ir.Func

	for _, node := range stmts {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || !hasTag(fn.Tags, "interop") {
			continue
		}

		if err := cg.emitInteropWrapperFor(fn); err != nil {
			return err
		}

		// Capture the just-emitted wrapper so we can pin it against
		// -Wl,--gc-sections below. Even at external linkage the linker
		// will drop a wrapper whose only Tin caller was constant-folded
		// (typical for #pure #interop functions); @llvm.used keeps it.
		for _, f := range cg.allFuncs() {
			if f.Name() == fn.Name {
				wrappers = append(wrappers, f)

				break
			}
		}
	}

	cg.pinInteropWrappers(wrappers)

	return nil
}

// pinInteropWrappers emits an `@llvm.used` global listing every #interop
// wrapper. This appending-linkage symbol is honored by both the LLVM
// optimizer and the system linker (GNU ld --gc-sections, ld64
// -dead_strip, lld), preventing them from DCEing wrappers whose only
// in-program caller was eliminated by CTFE folding or other optimization
// passes. The user's contract is "this function is callable from C";
// linker DCE breaks that contract.
func (cg *CodeGen) pinInteropWrappers(wrappers []*ir.Func) {
	for _, f := range wrappers {
		cg.registerLlvmUsedFunc(cg.mod, f)
	}
}

// emitInteropWrapperFor emits a single wrapper. Assumes the validation
// pass already proved the signature is wrappable.
//
// Marshaling layer:
//   - string param: C `const char*` -> ARC TinString (released after).
//   - string return: TinString -> C buffer via tin_extern_alloc.
//   - slice [T] param: C (T*, i64) -> ARC TinSlice (released after).
//   - slice [T] return: TinSlice -> C out-params via tin_extern_alloc;
//     wrapper return becomes i32 status (0=OK, nonzero=OOM).
//   - bool: C uint8_t -> i1 (icmp ne 0); reverse for returns.
//   - everything else: passthrough (primitives, pointers).
func (cg *CodeGen) emitInteropWrapperFor(fn *ast.FuncDecl) error {
	return cg.emitInteropWrapperWithName(fn, fn.Name)
}

// emitInteropWrapperWithName is emitInteropWrapperFor with an explicit
// wrapper symbol name. Lets the CTFE per-fn cache emit a parallel shim
// (`__tin_pure_shim_<name>`) without colliding with the internal Tin
// entry that already occupies the bare name when the function was not
// originally tagged #interop.
func (cg *CodeGen) emitInteropWrapperWithName(fn *ast.FuncDecl, wrapperName string) error {
	entry, ok := cg.curScope.lookup(fn.Name)
	if !ok {
		return cg.nodeErr(fn, "fn %s: #interop wrapper cannot find internal entry point", fn.Name)
	}

	internalFn, ok := entry.val.(*ir.Func)
	if !ok {
		return cg.nodeErr(fn, "fn %s: #interop entry resolved to non-function value", fn.Name)
	}

	// Override the pclntab display name so stacktrace() reports the
	// C-visible symbol (__tin_interop_<name>) rather than the Tin source
	// name. The heuristic would already return the IR name unchanged
	// (it starts with __tin_), but recordFnDisplayName stored the Tin
	// source name earlier; override it here.
	if cg.fnDisplayNames == nil {
		cg.fnDisplayNames = map[string]string{}
	}

	cg.fnDisplayNames[internalFn.Name()] = internalFn.Name()

	// When the active emit target is a sibling module (CTFE shimMod),
	// internalFn lives in cg.mod and is unreachable from the wrapper's
	// blocks. Mirror it as a `declare` in the active module so calls
	// resolve at link time.
	if active := cg.activeModule(); active != cg.mod {
		internalFn = cg.declareInActive(internalFn)
	}

	// Build the wrapper's C-ABI signature, remapping per-param.
	// Each Tin param can expand into 1 or more wrapper params:
	//   primitive / pointer -> 1 wrapper param (passthrough)
	//   string              -> 1 wrapper param (i8*)
	//   slice [T]           -> 2 wrapper params (T*, i64)
	wrapperParams := make([]*ir.Param, 0, len(fn.Params))
	paramKinds := make([]string, 0, len(fn.Params))
	// For slice params, we capture the per-param elem byte size so the
	// runtime helper sees the right copy length.
	sliceElemSizes := make(map[int]uint64)

	for paramIdx, p := range fn.Params {
		kind := cg.classifyInteropParam(p.Type)
		paramKinds = append(paramKinds, kind)

		switch kind {
		case "string":
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8Ptr))
		case "slice":
			at := p.Type.(*ast.ArrayType)

			elemTy, err := cg.tinTypeToLLVM(at.Elem)
			if err != nil {
				return err
			}

			sliceElemSizes[paramIdx] = llvmTypeSize(elemTy)

			wrapperParams = append(wrapperParams,
				ir.NewParam(p.Name, irtypes.NewPointer(elemTy)),
				ir.NewParam(p.Name+"_len", irtypes.I64))
		case "bool":
			// C `_Bool` / `uint8_t` is a full byte; Tin `bool` is i1.
			// Take the byte at the boundary, truncate to i1 before
			// passing to the internal entry.
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8))
		case "callback":
			// Wrapper takes the raw C function pointer as i8*.
			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, irtypes.I8Ptr))
		case "packed":
			structName := p.Type.(*ast.SimpleType).Name

			shape, _, err := cg.packedABIShape(structName)
			if err != nil {
				return cg.nodeErr(fn, "fn %s: #interop parameter %q: %v", fn.Name, p.Name, err)
			}

			switch shape {
			case "i8", "i16", "i32", "i64", "f32", "f64":
				wrapperParams = append(wrapperParams,
					ir.NewParam(p.Name, abiRegisterType(shape)))
			case "two_eightbyte":
				loTy, hiTy, err := classifyTwoEightbytes(
					cg.structFieldLLVMTypes[structName], structName)
				if err != nil {
					return cg.nodeErr(fn, "fn %s: #interop parameter %q: %v", fn.Name, p.Name, err)
				}

				wrapperParams = append(wrapperParams,
					ir.NewParam(p.Name+".lo", loTy))
				if hiTy != nil {
					wrapperParams = append(wrapperParams,
						ir.NewParam(p.Name+".hi", hiTy))
				}
			case "byval":
				rawTy := cg.packedUserStructType(structName)

				if cg.targetIsARM64() {
					// AAPCS64 passes large composites indirectly via a
					// plain *T register; LLVM's byval attribute can
					// trip alignment crashes here, so mirror the
					// existing extern-call convention.
					wrapperParams = append(wrapperParams,
						ir.NewParam(p.Name, irtypes.NewPointer(rawTy)))
				} else {
					bv := ir.NewParam(p.Name, irtypes.I8Ptr)
					bv.Attrs = append(bv.Attrs, ir.Byval{Typ: rawTy})
					wrapperParams = append(wrapperParams, bv)
				}
			}
		default:
			t, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			wrapperParams = append(wrapperParams, ir.NewParam(p.Name, t))
		}
	}

	retKind := cg.classifyInteropReturn(fn.RetType)

	var (
		retType       irtypes.Type = irtypes.Void
		sliceElemSize uint64
	)

	switch retKind {
	case "void":
		retType = irtypes.Void
	case "string":
		retType = irtypes.I8Ptr
	case "callback":
		// Wrapper hands C a function-pointer-shaped i8*; the user casts
		// to the appropriate `R (*)(args)` typedef on their side.
		retType = irtypes.I8Ptr
	case "bool":
		// Match C's byte-sized bool ABI; we zero-extend the i1 from
		// the internal call into this i8.
		retType = irtypes.I8
	case "packed":
		structName := fn.RetType.(*ast.SimpleType).Name

		shape, _, err := cg.packedABIShape(structName)
		if err != nil {
			return cg.nodeErr(fn, "fn %s: #interop return: %v", fn.Name, err)
		}

		switch shape {
		case "i8", "i16", "i32", "i64", "f32", "f64":
			retType = abiRegisterType(shape)
		case "two_eightbyte":
			loTy, hiTy, err := classifyTwoEightbytes(
				cg.structFieldLLVMTypes[structName], structName)
			if err != nil {
				return cg.nodeErr(fn, "fn %s: #interop return: %v", fn.Name, err)
			}

			if hiTy != nil {
				retType = irtypes.NewStruct(loTy, hiTy)
			} else {
				retType = loTy
			}
		case "byval":
			// sret hidden first param; the wrapper's nominal return
			// is void. The sret target is the user-fields-only struct
			// type so the wrapper signature matches the C ABI for the
			// equivalent packed struct. AMD64 uses an explicit sret
			// attribute; ARM64 (AAPCS64) just takes a plain pointer
			// which the backend wires to x8.
			rawTy := cg.packedUserStructType(structName)

			sret := ir.NewParam(".sret", irtypes.NewPointer(rawTy))
			if !cg.targetIsARM64() {
				sret.Attrs = append(sret.Attrs, ir.SRet{Typ: rawTy})
			}

			wrapperParams = append([]*ir.Param{sret}, wrapperParams...)
			retType = irtypes.Void
		}
	case "slice":
		// Reshape: status return + two trailing out-params.
		at := fn.RetType.(*ast.ArrayType)

		elemTy, err := cg.tinTypeToLLVM(at.Elem)
		if err != nil {
			return err
		}

		sliceElemSize = llvmTypeSize(elemTy)
		retType = irtypes.I32

		wrapperParams = append(wrapperParams,
			ir.NewParam("out_data", irtypes.NewPointer(irtypes.NewPointer(elemTy))),
			ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	default:
		if fn.RetType != nil {
			t, err := cg.tinTypeToLLVM(fn.RetType)
			if err != nil {
				return err
			}

			retType = t
		}
	}

	wrapper := cg.activeModule().NewFunc(wrapperName, retType, wrapperParams...)
	wrapper.FuncAttrs = append(wrapper.FuncAttrs, ir.AttrString("noinline"))
	block := wrapper.NewBlock("entry")
	// Skip the tin_runtime_init bootstrap when emitting into the CTFE
	// shim module: the dispatcher (Tin compiler) doesn't link the runtime
	// so the symbol is unresolvable, and CTFE invocations don't need a
	// fiber scheduler. For real #interop wrappers (target = cg.mod) the
	// init call stays as before.
	if cg.activeModule() == cg.mod {
		block.NewCall(cg.ensureRuntimeInitOnce())
	}

	// Per-arg marshaling. We track Tin temporaries (strings, slices)
	// created here so we can release them after the internal call.
	args := make([]value.Value, 0, len(fn.Params))

	var allocatedTemps []value.Value

	// Skip past the prepended sret slot when the return is byval.
	wrapperIdx := 0

	if retKind == "packed" {
		if shape, _, _ := cg.packedABIShape(fn.RetType.(*ast.SimpleType).Name); shape == "byval" {
			wrapperIdx = 1
		}
	}

	for paramIdx, kind := range paramKinds {
		switch kind {
		case "string":
			cstr := wrapperParams[wrapperIdx]
			wrapperIdx++

			tinStr := block.NewCall(cg.ensureInteropStrIn(), cstr)
			args = append(args, tinStr)

			ptrField := block.NewExtractValue(tinStr, 0)
			allocatedTemps = append(allocatedTemps, ptrField)
		case "bool":
			b := wrapperParams[wrapperIdx]
			wrapperIdx++

			// Truncate the byte to i1 (Tin's bool); semantically this
			// matches C's "non-zero is true" rule because LLVM trunc to
			// i1 takes the low bit, but combined with `cmp ne 0` would
			// be more faithful. Use icmp ne 0 for clarity and to
			// preserve the C semantic that 0x02 is true.
			zero := constant.NewInt(irtypes.I8, 0)
			i1Val := block.NewICmp(enum.IPredNE, b, zero)
			args = append(args, i1Val)
		case "packed":
			structName := fn.Params[paramIdx].Type.(*ast.SimpleType).Name

			shape, _, err := cg.packedABIShape(structName)
			if err != nil {
				return cg.nodeErr(fn, "fn %s: #interop parameter %q: %v", fn.Name, fn.Params[paramIdx].Name, err)
			}

			tinTy, err := cg.tinTypeToLLVM(fn.Params[paramIdx].Type)
			if err != nil {
				return err
			}

			tinAlloca := block.NewAlloca(tinTy)

			// Init type_id at field 0.
			tid := constant.NewInt(irtypes.I32, int64(cg.structTypeIDs[structName]))
			tidSlot := block.NewGetElementPtr(tinTy, tinAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			block.NewStore(tid, tidSlot)

			userOffBytes := cg.userFieldsByteOffset(structName)
			tinAllocaI8 := block.NewBitCast(tinAlloca, irtypes.I8Ptr)
			userBase := block.NewGetElementPtr(irtypes.I8, tinAllocaI8,
				constant.NewInt(irtypes.I64, int64(userOffBytes)))

			switch shape {
			case "i8", "i16", "i32", "i64", "f32", "f64":
				v := wrapperParams[wrapperIdx]
				wrapperIdx++

				slot := block.NewBitCast(userBase, irtypes.NewPointer(abiRegisterType(shape)))
				block.NewStore(v, slot)
			case "two_eightbyte":
				loTy, hiTy, _ := classifyTwoEightbytes(
					cg.structFieldLLVMTypes[structName], structName)

				lo := wrapperParams[wrapperIdx]
				wrapperIdx++

				loSlot := block.NewBitCast(userBase, irtypes.NewPointer(loTy))
				block.NewStore(lo, loSlot)

				if hiTy != nil {
					hi := wrapperParams[wrapperIdx]
					wrapperIdx++

					hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
						constant.NewInt(irtypes.I64, 8))
					hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(hiTy))
					block.NewStore(hi, hiSlot)
				}
			case "two_i64":
				lo := wrapperParams[wrapperIdx]
				hi := wrapperParams[wrapperIdx+1]
				wrapperIdx += 2

				loSlot := block.NewBitCast(userBase, irtypes.NewPointer(irtypes.I64))
				block.NewStore(lo, loSlot)

				hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
					constant.NewInt(irtypes.I64, 8))
				hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(irtypes.I64))
				block.NewStore(hi, hiSlot)
			case "byval":
				bvParam := wrapperParams[wrapperIdx]
				wrapperIdx++

				// memcpy the user-fields region from the byval ptr.
				_, totalSize, _ := cg.packedABIShape(structName)
				cg.emitMemcpy(block, userBase, bvParam, int64(totalSize))
			}

			loaded := block.NewLoad(tinTy, tinAlloca)
			args = append(args, loaded)
		case "callback":
			rawCb := wrapperParams[wrapperIdx]
			wrapperIdx++

			ft := fn.Params[paramIdx].Type.(*ast.FuncType)

			thunk, err := cg.getOrCreateCallbackThunk(ft)
			if err != nil {
				return err
			}
			// Build an ARC-managed env block: 16 bytes layout
			//   offset 0: destructor fn (NULL)
			//   offset 8: raw C fn pointer (read by the thunk)
			// The destructor slot is required because Tin's
			// _tin_release_closure unconditionally invokes whatever
			// pointer it finds at env+0 when rc reaches zero.
			envI8 := block.NewCall(cg.ensureRCAlloc(),
				constant.NewInt(irtypes.I64, 16))

			i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)

			dtorSlot := block.NewBitCast(envI8, i8PtrPtr)
			block.NewStore(constant.NewNull(irtypes.I8Ptr), dtorSlot)

			fnSlotPtr := block.NewGetElementPtr(irtypes.I8, envI8,
				constant.NewInt(irtypes.I32, 8))
			fnSlotPtrCast := block.NewBitCast(fnSlotPtr, i8PtrPtr)
			block.NewStore(rawCb, fnSlotPtrCast)

			// Build the Tin 4-slot fat fn-ptr {coro, colored, sync, env}.
			// The thunk is the sync variant (slot 2); buildFatFnPtrValue
			// synthesizes a coro wrapper for slot 0.
			fatTy, err := cg.tinTypeToLLVM(ft)
			if err != nil {
				return err
			}

			st := fatTy.(*irtypes.StructType)

			// Bitcast the thunk to slot-2's expected pointer type before
			// handing to buildFatFnPtrValue.  buildFatFnPtrValue expects
			// *ir.Func so we pass the thunk directly (its sig matches
			// slot 2's inner fn type since getOrCreateCallbackThunk
			// builds it from the same Tin FuncType).
			_ = st
			fatVal := cg.buildFatFnPtrValue(block, thunk, envI8)
			args = append(args, fatVal)

			// The internal entry retains+releases env per Tin's
			// fn-arg convention; we drop our originating reference
			// after the call so the env block is freed at rc=0.
			allocatedTemps = append(allocatedTemps, envI8)
		case "slice":
			dataPtr := wrapperParams[wrapperIdx]
			lenVal := wrapperParams[wrapperIdx+1]
			wrapperIdx += 2

			// Cast the typed C pointer to i8* for the runtime call.
			dataI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
			elemSize := constant.NewInt(irtypes.I64, int64(sliceElemSizes[paramIdx]))
			rawSlice := block.NewCall(cg.ensureInteropSliceIn(), dataI8, lenVal, elemSize)

			// rawSlice is {i8*, i64}. The internal expects {T*, i64}.
			// Extract, bitcast the pointer, and reassemble.
			internalSliceTy, err := cg.tinTypeToLLVM(fn.Params[paramIdx].Type)
			if err != nil {
				return err
			}

			st := internalSliceTy.(*irtypes.StructType)
			rawData := block.NewExtractValue(rawSlice, 0)
			rawLen := block.NewExtractValue(rawSlice, 1)
			typedData := block.NewBitCast(rawData, st.Fields[0])

			alloca := block.NewAlloca(st)
			ptrField := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			lenField := block.NewGetElementPtr(st, alloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 1))
			block.NewStore(typedData, ptrField)
			block.NewStore(rawLen, lenField)
			loaded := block.NewLoad(st, alloca)
			args = append(args, loaded)

			allocatedTemps = append(allocatedTemps, rawData)
		default:
			p := wrapperParams[wrapperIdx]
			wrapperIdx++

			args = append(args, p)
		}
	}

	// Call the internal entry.
	var rawRet value.Value

	if internalFn.Sig.RetType.Equal(irtypes.Void) {
		block.NewCall(internalFn, args...)
	} else {
		rawRet = block.NewCall(internalFn, args...)
	}

	// Marshal the return value out to C.
	var (
		finalRet  value.Value
		retTinPtr value.Value // ARC ptr to release after extraction
	)

	switch retKind {
	case "string":
		finalRet = block.NewCall(cg.ensureInteropStrOut(), rawRet)
		retTinPtr = block.NewExtractValue(rawRet, 0)
	case "callback":
		// rawRet is the 4-slot Tin fat fn-ptr {sync, colored, coro, env}.
		// C trampolines can't run coros -- pull the non-colored sync
		// variant (slot 0).  We OWN one ARC ref on env (Tin convention
		// for fat-fn-ptr returns); transfer it to the trampoline.  The
		// dispatcher reads (fn, env) back out of the trampoline slot.
		fnRaw := block.NewExtractValue(rawRet, 0)
		envRaw := block.NewExtractValue(rawRet, 3)

		fnI8 := block.NewBitCast(fnRaw, irtypes.I8Ptr)

		ftRet := fn.RetType.(*ast.FuncType)

		disp, err := cg.getOrCreateClosureDispatcher(ftRet)
		if err != nil {
			return err
		}

		dispI8 := block.NewBitCast(disp, irtypes.I8Ptr)

		finalRet = block.NewCall(cg.ensureMakeTrampoline(), fnI8, envRaw, dispI8)
	case "bool":
		// Zero-extend the internal i1 to i8 for the C ABI.
		finalRet = block.NewZExt(rawRet, irtypes.I8)
	case "packed":
		structName := fn.RetType.(*ast.SimpleType).Name

		shape, totalSize, err := cg.packedABIShape(structName)
		if err != nil {
			return cg.nodeErr(fn, "fn %s: #interop return: %v", fn.Name, err)
		}

		tinTy, _ := cg.tinTypeToLLVM(fn.RetType)
		tinAlloca := block.NewAlloca(tinTy)
		block.NewStore(rawRet, tinAlloca)

		userOffBytes := cg.userFieldsByteOffset(structName)
		tinAllocaI8 := block.NewBitCast(tinAlloca, irtypes.I8Ptr)
		userBase := block.NewGetElementPtr(irtypes.I8, tinAllocaI8,
			constant.NewInt(irtypes.I64, int64(userOffBytes)))

		switch shape {
		case "i8", "i16", "i32", "i64", "f32", "f64":
			slot := block.NewBitCast(userBase, irtypes.NewPointer(abiRegisterType(shape)))
			finalRet = block.NewLoad(abiRegisterType(shape), slot)
		case "two_eightbyte":
			loTy, hiTy, _ := classifyTwoEightbytes(
				cg.structFieldLLVMTypes[structName], structName)

			loSlot := block.NewBitCast(userBase, irtypes.NewPointer(loTy))
			lo := block.NewLoad(loTy, loSlot)

			if hiTy == nil {
				finalRet = lo
			} else {
				hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
					constant.NewInt(irtypes.I64, 8))
				hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(hiTy))
				hi := block.NewLoad(hiTy, hiSlot)

				pairTy := irtypes.NewStruct(loTy, hiTy)
				pairAlloca := block.NewAlloca(pairTy)
				loP := block.NewGetElementPtr(pairTy, pairAlloca,
					constant.NewInt(irtypes.I32, 0),
					constant.NewInt(irtypes.I32, 0))
				hiP := block.NewGetElementPtr(pairTy, pairAlloca,
					constant.NewInt(irtypes.I32, 0),
					constant.NewInt(irtypes.I32, 1))
				block.NewStore(lo, loP)
				block.NewStore(hi, hiP)
				finalRet = block.NewLoad(pairTy, pairAlloca)
			}
		case "two_i64":
			loSlot := block.NewBitCast(userBase, irtypes.NewPointer(irtypes.I64))
			lo := block.NewLoad(irtypes.I64, loSlot)

			hiBase := block.NewGetElementPtr(irtypes.I8, userBase,
				constant.NewInt(irtypes.I64, 8))
			hiSlot := block.NewBitCast(hiBase, irtypes.NewPointer(irtypes.I64))
			hi := block.NewLoad(irtypes.I64, hiSlot)

			pairTy := irtypes.NewStruct(irtypes.I64, irtypes.I64)
			pairAlloca := block.NewAlloca(pairTy)
			loP := block.NewGetElementPtr(pairTy, pairAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 0))
			hiP := block.NewGetElementPtr(pairTy, pairAlloca,
				constant.NewInt(irtypes.I32, 0),
				constant.NewInt(irtypes.I32, 1))
			block.NewStore(lo, loP)
			block.NewStore(hi, hiP)
			finalRet = block.NewLoad(pairTy, pairAlloca)
		case "byval":
			// sret: copy user fields into the caller-provided slot.
			sretParam := wrapperParams[0] // sret was prepended
			cg.emitMemcpy(block, sretParam, userBase, int64(totalSize))
			// finalRet stays nil; we ret void below.
		}
	case "slice":
		// Pull out the typed-slice fields and rebuild as the i8*-typed
		// slice the runtime helper expects. Then call slice_out which
		// fills the user's out-params and returns 0/1.
		typedData := block.NewExtractValue(rawRet, 0)
		lenVal := block.NewExtractValue(rawRet, 1)
		dataI8 := block.NewBitCast(typedData, irtypes.I8Ptr)

		rawSliceTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
		alloca := block.NewAlloca(rawSliceTy)
		ptrField := block.NewGetElementPtr(rawSliceTy, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 0))
		lenField := block.NewGetElementPtr(rawSliceTy, alloca,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 1))
		block.NewStore(dataI8, ptrField)
		block.NewStore(lenVal, lenField)
		rawSlice := block.NewLoad(rawSliceTy, alloca)

		// out_data and out_len are the last two wrapper params.
		outData := wrapperParams[len(wrapperParams)-2]
		outLen := wrapperParams[len(wrapperParams)-1]
		// out_data is T**; cast to i8** for the helper.
		outDataI8 := block.NewBitCast(outData, irtypes.NewPointer(irtypes.I8Ptr))
		elemSize := constant.NewInt(irtypes.I64, int64(sliceElemSize))

		finalRet = block.NewCall(cg.ensureInteropSliceOut(),
			rawSlice, elemSize, outDataI8, outLen)

		// Release the typed data buffer Tin allocated for the slice.
		retTinPtr = dataI8
	default:
		finalRet = rawRet
	}

	// Release any temporary Tin allocations we created for params.
	releaseFn := cg.ensureRelease()

	for _, ptr := range allocatedTemps {
		block.NewCall(releaseFn, ptr)
	}
	// Release the returned Tin string after we've copied it out.
	if retTinPtr != nil {
		block.NewCall(releaseFn, retTinPtr)
	}

	if retType.Equal(irtypes.Void) {
		block.NewRet(nil)
	} else {
		block.NewRet(finalRet)
	}

	return nil
}

// classifyInteropParam returns the marshaling shape for a Tin
// parameter type. Recognized:
//
//	"string"   - Tin string fat pointer
//	"slice"    - Tin fat array [T]
//	"bool"     - Tin bool (i1) widened to/from C uint8_t
//	"callback" - fn(...) typed; wrapped via per-signature thunk
//	"packed"   - by-value #packed user struct
//	""         - passthrough (primitives, pointers)
