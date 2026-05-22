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

// nameLooksCrossContext reports whether an IR scope name carries the
// double-underscore marker the codegen uses for symbols whose call
// graph spans multiple compilation contexts: package fns
// (`pkg__name`), generic monomorphizations (`Type__Arg_method`),
// trait-qualified methods.  Used by the biased-RC gate to default such
// functions to the shared (atomic) allocator -- the per-body call
// graph cannot prove non-escape for them.
func nameLooksCrossContext(name string) bool {
	for i := 0; i+1 < len(name); i++ {
		if name[i] == '_' && name[i+1] == '_' {
			return true
		}
	}

	return false
}

// predeclareFunc adds a function to the module and registers it in the global
// scope without generating the body. This enables forward references and recursion.

func (cg *CodeGen) genFuncDeclAs(n *ast.FuncDecl, scopeName string) error {
	// Generic functions are compiled on demand; register as template and skip.
	if len(n.TypeParams) > 0 && n.IsExtern == "" {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncOverloads[n.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[n.Name], n)
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}

	// Mirror this monomorphized FuncDecl in funcDecls under the IR
	// scope name so call-site machinery (e.g. wildcard call-site
	// generics) can look up the original FuncDecl by the same key the
	// scope uses.
	if scopeName != "" {
		if _, present := cg.funcDecls[scopeName]; !present {
			cg.funcDecls[scopeName] = n
		}
	}

	// Build the mutated-names set for the if-condition folder. Restored
	// after the body so nested function generations don't leak names
	// across each other.
	prevMutated := cg.mutatedNames
	cg.mutatedNames = collectMutatedNames(n.Body)

	// {#unsafe} is a lexical block scope - a function defined inside an
	// unsafe block must NOT inherit the depth into its body. Reset the
	// counter on every function-body boundary and restore on exit.
	prevUnsafe := cg.unsafeDepth
	cg.unsafeDepth = 0

	defer func() {
		cg.mutatedNames = prevMutated
		cg.unsafeDepth = prevUnsafe
	}()

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		var err error

		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}

	if n.IsExtern != "" {
		// Extern functions are always side-effectful; ensure the tag is present.
		if !hasTag(n.Tags, "sideffect") {
			n.Tags = append(n.Tags, "sideffect")
		}
		// Collect non-varargs parameters with their C-level types.
		isVariadic := false

		var cParams []*ir.Param
		// cParamByval[i] is non-nil when cParams[i] uses byval (AMD64 large struct > 16 bytes).
		var cParamByval []*irtypes.StructType
		// tinParamToCIdx maps Tin parameter index (ignoring varargs) to the
		// starting index in cParams. Normally 1:1, but 2-register struct splits
		// insert an extra C param so subsequent indices shift.
		var tinParamToCIdx []int
		// cParam2RegNative[cIdx] is non-nil when cParams[cIdx] is the FIRST
		// of a 2-register split pair (9-16 byte all-integer struct, AMD64/ARM64).
		var cParam2RegNative []*irtypes.StructType
		// cParamARM64Indirect[cIdx] is non-nil when cParams[cIdx] is a plain
		// pointer (*T) for ARM64 non-HFA large struct indirect passing. The C
		// function receives a pointer to a stack copy of the struct.
		var cParamARM64Indirect []*irtypes.StructType

		for _, p := range n.Params {
			if p.IsVarArgs {
				isVariadic = true

				continue
			}

			if vt, ok := p.Type.(*ast.SimpleType); ok && vt.Name == "..." {
				isVariadic = true

				continue
			}

			ct, err := cg.tinTypeToExternLLVM(p.Type, false)
			if err != nil {
				return err
			}

			tinParamToCIdx = append(tinParamToCIdx, len(cParams))

			// Large struct passing (>16 bytes) is ABI-dependent:
			//   AMD64 x86-64 SysV: all large structs use byval (implicit pointer copy).
			//   ARM64 AAPCS64:
			//     - HFA (1-4 identical float fields, any size): pass directly in VFP regs.
			//     - Non-HFA large: pass as plain *T pointer (not byval) matching AAPCS64
			//       "composite type passed indirectly" rule without the LLVM byval alignment
			//       complications that cause crashes on ARM64 Linux.
			// For 9-16 byte all-integer structs, both ABIs use two integer registers.
			nativeSt, isNativeSt := ct.(*irtypes.StructType)

			if isNativeSt && nativeStructNeedsByval(nativeSt) && cg.targetIsAMD64() {
				// AMD64: use byval for large non-HFA structs.
				bvParam := ir.NewParam(p.Name, irtypes.I8Ptr)
				bvParam.Attrs = append(bvParam.Attrs, ir.Byval{Typ: nativeSt})
				cParams = append(cParams, bvParam)
				cParamByval = append(cParamByval, nativeSt)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			} else if isNativeSt && nativeStructNeedsByval(nativeSt) && cg.targetIsARM64() && !isNativeStructHFA(nativeSt) {
				// ARM64 non-HFA large struct: pass as plain pointer (*T).
				// Callee (Clang) receives the pointer in an integer register (x0/x1...).
				// This matches AAPCS64 composite indirect passing without byval alignment issues.
				ptrParam := ir.NewParam(p.Name, irtypes.NewPointer(nativeSt))
				cParams = append(cParams, ptrParam)
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nativeSt)
			} else if isNativeSt && coerceNativeStructForABI2Reg(nativeSt) && (cg.targetIsAMD64() || cg.targetIsARM64()) {
				// 9-16 byte all-integer struct: split into two i64 params.
				// x86-64 SysV: two integer eightbytes in rdi/rsi etc.
				// AAPCS64: two consecutive x-registers (x0/x1 etc.).
				// Both ABIs represent this as (i64, i64) in LLVM IR.
				cParams = append(cParams, ir.NewParam(p.Name+".lo", irtypes.I64))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nativeSt)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)

				cParams = append(cParams, ir.NewParam(p.Name+".hi", irtypes.I64))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			} else {
				// Direct pass: small structs, HFA structs (ARM64 VFP regs), primitives.
				cParams = append(cParams, ir.NewParam(p.Name, ct))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			}
		}
		// Compute C-level return type.
		var cRetType irtypes.Type = irtypes.Void

		if n.RetType != nil {
			var err error

			cRetType, err = cg.tinTypeToExternLLVM(n.RetType, true)
			if err != nil {
				return err
			}
		}

		// sret: structs > 16 bytes are returned via a hidden pointer argument.
		// AMD64 (x86-64 SysV): hidden pointer in rdi.
		// ARM64 (AAPCS64): hidden pointer in x8 (indirect result register).
		// In both cases the LLVM IR uses void return + sret first parameter;
		// the backend maps it to rdi or x8 respectively. Without this, LLVM
		// generates incorrect multi-register returns that mismatch the C callee.
		var cRetSRetSt *irtypes.StructType

		if cg.targetIsAMD64() || cg.targetIsARM64() {
			if nativeSt, ok := cRetType.(*irtypes.StructType); ok && nativeStructNeedsByval(nativeSt) {
				cRetSRetSt = nativeSt
				sretParam := ir.NewParam(".sret", irtypes.NewPointer(nativeSt))
				sretParam.Attrs = append(sretParam.Attrs, ir.SRet{Typ: nativeSt})
				cParams = append([]*ir.Param{sretParam}, cParams...)
				cParamByval = append([]*irtypes.StructType{nil}, cParamByval...)
				cParam2RegNative = append([]*irtypes.StructType{nil}, cParam2RegNative...)
				cParamARM64Indirect = append([]*irtypes.StructType{nil}, cParamARM64Indirect...)

				for i := range tinParamToCIdx {
					tinParamToCIdx[i]++
				}

				cRetType = irtypes.Void
			}
		}

		// Create (or reuse) the raw C declaration with C-level types.
		cFunc := cg.ensureExternDecl(n.IsExtern, cRetType, cParams, isVariadic)

		if cg.curScope == nil {
			cg.curScope = newScope(nil)
		}

		// Detect if any parameter or return type is a named Tin struct that needs
		// Tin->C conversion at the call boundary.
		needsStructConv := false

		for _, p := range n.Params {
			if p.IsVarArgs {
				continue
			}

			if _, isStruct := cg.isNamedTinStruct(p.Type); isStruct {
				needsStructConv = true

				break
			}

			// *S pointer params where S has a hidden C pointer field.
			if cg.isExternPtrParam(p.Type) {
				needsStructConv = true

				break
			}

			// N*S output-parameter pattern: C writes (N-1)*S.native into N*S.
			if _, _, isDbl := cg.isExternOutPtrParam(p.Type); isDbl {
				needsStructConv = true

				break
			}
		}

		if n.RetType != nil {
			if _, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
				needsStructConv = true
			}
		}

		// If the return type does not need wrapping and no struct params, expose
		// the C function directly.  Fat-ptr parameters are handled by coerce().
		// #handover always needs a wrapper to RC-ify the returned pointer.
		if cRetType.Equal(retType) && !needsStructConv && !hasTag(n.Tags, "handover") {
			cg.curScope.set(scopeName, &scopeEntry{val: cFunc, isAlloc: false})

			return nil
		}

		// Generate a thin wrapper that handles type conversions.
		// For struct interop: wrapper takes Tin-level params (full struct), converts
		// to C-native layout, calls C, converts result back to Tin layout.
		// For other types (e.g. char* -> string): same as before.
		wrapperName := "__tinwrap_" + scopeName

		var wrapperFn *ir.Func

		for _, f := range cg.allFuncs() {
			if f.Name() == wrapperName {
				wrapperFn = f

				break
			}
		}

		if wrapperFn == nil {
			// Build wrapper params: one per Tin parameter (not per C param, since
			// 2-register splits create extra C params for a single Tin param).
			var wrapperParams []*ir.Param

			tinNonVarargIdx := 0

			for _, p := range n.Params {
				if p.IsVarArgs {
					continue
				}

				if vt, ok := p.Type.(*ast.SimpleType); ok && vt.Name == "..." {
					continue
				}

				cIdx := tinParamToCIdx[tinNonVarargIdx]

				if sName, isStruct := cg.isNamedTinStruct(p.Type); isStruct {
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(sName, tinType))
				} else if cg.isExternPtrParam(p.Type) {
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(cParams[cIdx].Name(), tinType))
				} else if _, _, isDbl := cg.isExternOutPtrParam(p.Type); isDbl {
					// N*S output-parameter: wrapper receives N*%S.wrapper from Tin caller.
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(cParams[cIdx].Name(), tinType))
				} else {
					wrapperParams = append(wrapperParams, cParams[cIdx])
				}

				tinNonVarargIdx++
			}

			// cLayoutStruct value return: the wrapper returns the C-layout
			// %Native struct directly (no Tin-wrapper construction inside).
			// The call site allocates the storage and stamps the Tin wrapper
			// (typeid + vtables + c_data_ptr) inline -- which keeps the
			// wrapper's LLVM signature 1:1 with the user's Tin declaration.
			cLayoutNativeReturnStruct := ""
			wrapperRetType := retType

			if n.RetType != nil {
				if sName, isStruct := cg.isNamedTinStruct(n.RetType); isStruct && cg.cLayoutStructs[sName] {
					nativeSt, _ := cg.tinStructNativeLLVM(sName)
					if nativeSt != nil {
						wrapperRetType = nativeSt
						cLayoutNativeReturnStruct = sName
					}
				}
			}

			wrapperFn = cg.mod.NewFunc(wrapperName, wrapperRetType, wrapperParams...)

			if cLayoutNativeReturnStruct != "" {
				cg.cLayoutWrapperNativeReturnFns[wrapperName] = cLayoutNativeReturnStruct
				cg.cLayoutWrapperNativeReturnFns[scopeName] = cLayoutNativeReturnStruct
			}

			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")

			// Build C-level call args: convert struct params to native, pass others as-is.
			callArgs := make([]value.Value, len(cParams))

			// AMD64 sret: pre-allocate the result buffer and put its address at index 0.
			var sretResultAlloca value.Value
			if cRetSRetSt != nil {
				sretResultAlloca = entry.NewAlloca(cRetSRetSt)
				callArgs[0] = ir.NewArg(sretResultAlloca, ir.SRet{Typ: cRetSRetSt})
			}

			// dblPtrWritebacks records N*S (N>=2) params that need post-call write-back:
			// after C writes (N-1)*S.native to the slot, we wrap the chain and store
			// the result into the Tin caller's location.
			type dblPtrWriteback struct {
				wrapperParamIdx int
				slot            value.Value // alloca holding (depth-1)*S.native
				structName      string
				depth           int // total Tin param depth N (>= 2)
			}

			var dblPtrWritebacks []dblPtrWriteback

			tinNonVarargIdx = 0
			wrapperPIdx := 0

			for _, tinParam := range n.Params {
				if tinParam.IsVarArgs {
					continue
				}

				if vt, ok := tinParam.Type.(*ast.SimpleType); ok && vt.Name == "..." {
					continue
				}

				cIdx := tinParamToCIdx[tinNonVarargIdx]
				p := wrapperFn.Params[wrapperPIdx]

				if sName, isStruct := cg.isNamedTinStruct(tinParam.Type); isStruct {
					native, err := cg.wrapStructToExtern(entry, p, sName)
					if err != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return err
					}
					// For byval params (AMD64 large structs > 16 bytes): alloca native
					// struct, store the converted value, then pass a byval-attributed pointer.
					if cParamByval[cIdx] != nil {
						nativeAlloca := entry.NewAlloca(cParamByval[cIdx])
						entry.NewStore(native, nativeAlloca)
						ptr := entry.NewBitCast(nativeAlloca, irtypes.I8Ptr)
						callArgs[cIdx] = ir.NewArg(ptr, ir.Byval{Typ: cParamByval[cIdx]})
					} else if cParamARM64Indirect[cIdx] != nil {
						// ARM64 non-HFA large struct: alloca + pass plain pointer.
						nativeAlloca := entry.NewAlloca(cParamARM64Indirect[cIdx])
						entry.NewStore(native, nativeAlloca)
						callArgs[cIdx] = nativeAlloca
					} else if cParam2RegNative[cIdx] != nil {
						// 9-16 byte all-integer struct: split into two i64 halves
						// to match clang's x86-64 SysV / AAPCS64 (i64, i64) coercion.
						nativeSt := cParam2RegNative[cIdx]
						a := entry.NewAlloca(nativeSt)
						entry.NewStore(native, a)
						loPtr := entry.NewBitCast(a, irtypes.NewPointer(irtypes.I64))
						lo := entry.NewLoad(irtypes.I64, loPtr)
						hiRaw := entry.NewGetElementPtr(irtypes.I8, entry.NewBitCast(a, irtypes.I8Ptr),
							constant.NewInt(irtypes.I64, 8))
						hiPtr := entry.NewBitCast(hiRaw, irtypes.NewPointer(irtypes.I64))
						hi := entry.NewLoad(irtypes.I64, hiPtr)
						callArgs[cIdx] = lo
						callArgs[cIdx+1] = hi
					} else if intTy, isInt := cParams[cIdx].Type().(*irtypes.IntType); isInt {
						// Small all-integer struct coerced to integer register.
						if nativeSt, ok2 := native.Type().(*irtypes.StructType); ok2 {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							if structBits < intTy.BitSize {
								// Coerced type is wider than the struct (ARM64: <=8-byte
								// struct -> i64). Load at the struct's natural bit size
								// to avoid an out-of-bounds read, then zero-extend.
								smallTy := irtypes.NewInt(structBits)
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(smallTy))
								small := entry.NewLoad(smallTy, ip)
								native = entry.NewZExt(small, intTy)
							} else {
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(intTy))
								native = entry.NewLoad(intTy, ip)
							}
						}

						callArgs[cIdx] = native
					} else {
						callArgs[cIdx] = native
					}
				} else if cg.isExternPtrParam(tinParam.Type) {
					// *S param with hidden C pointer: extract it and pass to C.
					callArgs[cIdx] = cg.extractCSrcPtr(entry, p, tinParam.Type, cParams[cIdx].Type())
				} else if sName, depth, isDbl := cg.isExternOutPtrParam(tinParam.Type); isDbl {
					// N*S output-parameter: allocate a (depth-1)*S.native slot.
					// Pass &slot to C as (depth)*S.native; after the call wrap and write back.
					nativeSt, _ := cg.tinStructNativeLLVM(sName)
					// Build (depth-1)*S.native type for the slot content.
					// depth >= 2, so after the loop contentType is always a pointer type.
					var contentType irtypes.Type = nativeSt
					for j := 0; j < depth-1; j++ {
						contentType = irtypes.NewPointer(contentType)
					}

					contentPtrType := contentType.(*irtypes.PointerType)
					slot := entry.NewAlloca(contentPtrType)
					entry.NewStore(constant.NewNull(contentPtrType), slot)
					callArgs[cIdx] = slot
					dblPtrWritebacks = append(dblPtrWritebacks, dblPtrWriteback{wrapperPIdx, slot, sName, depth})
				} else {
					callArgs[cIdx] = p
				}

				tinNonVarargIdx++
				wrapperPIdx++
			}

			rawCall := entry.NewCall(cFunc, callArgs...)

			// AMD64 sret: load the actual result from the pre-allocated buffer.
			var rawResult value.Value = rawCall
			if cRetSRetSt != nil {
				rawResult = entry.NewLoad(cRetSRetSt, sretResultAlloca)
			}

			// Convert result: if C returned a native struct, wrap back to Tin.
			var finalResult value.Value

			if n.RetType != nil {
				if sName, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
					// If C returned a coerced integer (ARM64: i64, AMD64: i32),
					// convert it back to the native struct type before wrapping.
					nativeResult := rawResult
					if intTy, isInt := rawResult.Type().(*irtypes.IntType); isInt {
						if nativeSt, err2 := cg.tinStructNativeLLVM(sName); err2 == nil {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							nativeAlloca := entry.NewAlloca(nativeSt)

							if structBits < intTy.BitSize {
								// Wider coercion (ARM64: i64 -> struct); truncate first.
								smallTy := irtypes.NewInt(structBits)
								truncated := entry.NewTrunc(rawResult, smallTy)
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(smallTy))
								entry.NewStore(truncated, ip)
							} else {
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(intTy))
								entry.NewStore(rawResult, ip)
							}

							nativeResult = entry.NewLoad(nativeSt, nativeAlloca)
						}
					}

					if cLayoutNativeReturnStruct == sName {
						// Native-return shape: just hand the C-layout struct
						// back to the call site, which handles allocation and
						// Tin wrapper stamping (typeid + vtables + c_data_ptr).
						finalResult = nativeResult
					} else {
						tinResult, err := cg.wrapNativeStructToTin(entry, nativeResult, sName)
						if err != nil {
							cg.curFn = prevFn
							cg.curScope = prevScope

							return err
						}

						finalResult = tinResult
					}
				} else {
					finalResult = cg.wrapFromExtern(entry, rawResult, retType, hasTag(n.Tags, "handover"))
				}
			}

			// Post-call write-backs for N*S output parameters (N >= 2).
			// For each param, C may have written (N-1)*S.native into the slot.
			// Read what C wrote; if non-null build a Tin wrapper chain and store
			// it into the Tin caller's location; if null store null.
			curBlock := entry

			for i, wb := range dblPtrWritebacks {
				nativeSt, _ := cg.tinStructNativeLLVM(wb.structName)
				// Build (depth-1)*S.native type to load from slot.
				var contentType irtypes.Type = nativeSt

				for j := 0; j < wb.depth-1; j++ {
					contentType = irtypes.NewPointer(contentType)
				}

				nativeVal := curBlock.NewLoad(contentType, wb.slot)

				wbNull := wrapperFn.NewBlock(fmt.Sprintf("wb%d_null", i))
				wbWrap := wrapperFn.NewBlock(fmt.Sprintf("wb%d_wrap", i))
				wbDone := wrapperFn.NewBlock(fmt.Sprintf("wb%d_done", i))

				isNull := curBlock.NewICmp(enum.IPredEQ,
					curBlock.NewBitCast(nativeVal, irtypes.I8Ptr),
					constant.NewNull(irtypes.I8Ptr))
				curBlock.NewCondBr(isNull, wbNull, wbWrap)

				// Null path: write a null of (depth-1)*S Tin type.
				var innerTinType ast.TypeExpr = &ast.SimpleType{Name: wb.structName}

				for j := 0; j < wb.depth-1; j++ {
					innerTinType = &ast.PointerType{Elem: innerTinType}
				}

				tinPtrTypeRaw, _ := cg.tinTypeToLLVM(innerTinType)
				tgtPt := tinPtrTypeRaw.(*irtypes.PointerType)
				wbParam := wrapperFn.Params[wb.wrapperParamIdx]
				wbNull.NewStore(constant.NewNull(tgtPt), wbParam)
				wbNull.NewBr(wbDone)

				// Non-null path: recursively build Tin wrapper chain for depth-1 levels.
				wrapperVal, wbErr := cg.emitWrapNativeChain(wbWrap, nativeVal, wb.structName, wb.depth-1)
				if wbErr != nil {
					cg.curFn = prevFn
					cg.curScope = prevScope

					return wbErr
				}

				wbWrap.NewStore(wrapperVal, wbParam)
				wbWrap.NewBr(wbDone)

				curBlock = wbDone
			}

			if irtypes.IsVoid(retType) {
				curBlock.NewRet(nil)
			} else {
				curBlock.NewRet(finalResult)
			}

			cg.curFn = prevFn
			cg.curScope = prevScope
		}

		// Pointer returns from extern wrappers: mark as heap-promoting so that
		// genLetStmt sets isHeapOwned on the bound variable, enabling scope-exit
		// release via emitHeapChainRelease / ensureStructPtrReleaseFn.
		// String/fat-ptr returns are already RC-tracked; only raw pointer types need this.
		// Applies to both #handover (C frees original) and non-handover borrow
		// (Tin owns the RC copy).
		if _, isPtr := retType.(*irtypes.PointerType); isPtr {
			cg.heapPromotingFns[wrapperName] = true
			cg.heapPromotingFns[scopeName] = true
		}

		cg.curScope.set(scopeName, &scopeEntry{val: wrapperFn, isAlloc: false})

		return nil
	}

	// Look up pre-declared function in global scope (by qualified name), or create.
	var f *ir.Func

	if entry, ok := cg.curScope.vars[scopeName]; ok {
		if fn, isFunc := entry.val.(*ir.Func); isFunc {
			f = fn
		}
	}

	if f == nil {
		// Not pre-declared - create now (e.g. nested or struct method).
		params := make([]*ir.Param, len(n.Params))
		for i, p := range n.Params {
			pt, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			params[i] = ir.NewParam(p.Name, pt)
		}

		f = cg.mod.NewFunc(scopeName, retType, params...)
	}

	if n.Body == nil {
		f.Blocks = nil // Forward declaration - no body.

		return nil
	}

	// If function already has a body (re-declaration), skip.
	if len(f.Blocks) > 0 {
		return nil
	}

	// Create entry block.
	entry := f.NewBlock("entry")

	// Save context (including defer lists - each function has its own).
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevBlock := cg.curBlock
	prevDeferFnI8s := cg.pendingDeferFnI8s
	prevDeferFrames := cg.pendingDeferFrames
	prevDeferEnvs := cg.pendingDeferEnvs
	prevAutoYield := cg.curFnAutoYield
	prevDeferRetSlotParam := cg.curDeferRetSlotParam
	prevFnDeferRetAlloca := cg.curFnDeferRetAlloca
	prevDeferThunkRetType := cg.curDeferThunkRetType
	prevEscapingVars := cg.curFnEscapingVars
	prevEscapingAliases := cg.curFnEscapingAliases
	prevDiScope := cg.diCurrentScope
	// lastSliceBase is a single-slot side channel from genSliceExpr to
	// genVarDecl.  When a slice expr inside this function's body never
	// gets paired with an outer let-binding (e.g. `return xs[0:m]`) the
	// stale value would otherwise be picked up by an unrelated let-
	// binding in the OUTER function -- producing a retain whose operand
	// is an SSA value from a foreign function's block.
	prevLastSliceBase := cg.lastSliceBase
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.curBlock = nil
	cg.curFnAutoYield = false // sync variant never auto-yields
	cg.curDeferRetSlotParam = nil
	cg.curDeferThunkRetType = nil
	cg.lastSliceBase = nil

	cg.curFnEscapingVars, cg.curFnEscapingAliases = findEscapingAddressTakenVars(n.Body)

	heapPromoting := len(cg.curFnEscapingVars) > 0 || hasDirectHeapReturn(n.Body, cg.heapPromotingFns)

	if heapPromoting {
		cg.heapPromotingFns[scopeName] = true
		// Also store under the actual IR function name (which may include a
		// parameter-type suffix, e.g. "json__parse_value__ptr_Parser") so that
		// genLetStmt can find it via the scope-resolved *ir.Func lookup.
		if f != nil {
			cg.heapPromotingFns[f.Name()] = true
		}
	}

	cg.curFn = f

	prevFnAstBody := cg.curFnAstBody
	cg.curFnAstBody = n.Body

	defer func() { cg.curFnAstBody = prevFnAstBody }()

	// Run the borrow analyzer over this function body once; the result
	// (set of names to classify as Borrowed) is consulted by
	// maybeMarkBindingBorrowed during let-decl codegen. No-op when
	// --ownership-borrow is off.
	prevBorrowSet := cg.currentFnBorrowSet
	cg.currentFnBorrowSet = cg.analyzeFunctionBorrows(n.Body)
	prevMovedBindings := cg.movedBindings
	cg.movedBindings = nil
	// Biased RC: route _tin_rc_alloc to the local (shared=0) variant
	// when the call-graph analyzer proved this function is NOT
	// reachable from an {#async} root AND the body does not write
	// to any module-level global.  Async unreachability prevents
	// fiber escape; the global-write check prevents a different
	// thread from concurrently rc-touching a value this function
	// just stored into a globally-visible slot.  Sync extern calls
	// stay local -- they execute synchronously on the caller's
	// thread.  See docs/15-ownership.md "Biased reference counting".
	prevSyncLocal := cg.curFnSyncLocal
	// Biased RC: route _tin_rc_alloc through the local (shared=0)
	// variant when the call-graph analyzer proved this function is
	// NOT reachable from any {#async} root AND does not write to a
	// global.  recordCallees now covers ScopeAccess and FieldAccess
	// callees so package fns and method dispatch land in the graph.
	//
	// Conservative gate: any scopeName containing `__` is a package
	// fn, generic monomorphization, or trait-qualifier-decorated
	// method.  These cross compilation contexts the per-package call
	// graph doesn't fully see, so they default to non-local.  Plain
	// user-defined fns and user struct methods use single underscores
	// and remain eligible.  Refining this requires unifying the
	// funcDecls keying (task #75 stretch); the heuristic is sound
	// (over-tags some functions) and unblocks the perf win on user
	// code today.
	spawnerSet := cg.computeSpawnerReachable()
	cg.curFnSyncLocal = scopeName != "" &&
		!cg.coroCallable[scopeName] &&
		!spawnerSet[scopeName] &&
		!nameLooksCrossContext(scopeName)

	defer func() {
		cg.currentFnBorrowSet = prevBorrowSet
		cg.movedBindings = prevMovedBindings
		cg.curFnSyncLocal = prevSyncLocal
	}()

	cg.curScope = newScope(cg.curScope)
	cg.curScope.isFunctionBoundary = true

	// Record the source file for this fn. pclntab.go uses this at the
	// post-pass to emit per-fn header entries with correct file paths
	// even when imports from other files were processed earlier (which
	// would leave cg.filename pointing at a different .tin source).
	if f != nil && cg.filename != "" {
		if cg.fnSourceFiles == nil {
			cg.fnSourceFiles = map[string]string{}
		}

		cg.fnSourceFiles[f.Name()] = cg.filename
	}

	// Record the user-visible display name (`pkg::name` for top-level
	// fns, `pkg::Struct.method` when cg.curMethodReceiverStruct is set
	// before calling here). pclntab.go's unmangleTinName consults this
	// map at trace render time so users see source-level names instead
	// of IR-mangled ones (`sync__AtomicI64_deinit` vs `sync::AtomicI64.deinit`).
	if f != nil && f.Name() != "" {
		cg.recordFnDisplayName(f.Name(), n)
	}

	// Emit DISubprogram for debug builds, and seed currentPos so that the
	// parameter allocas and first body instruction are tagged with the
	// function declaration's line rather than line 0.
	cg.emitDbgSubprogram(n, f, cg.filename)

	if cg.debugMode && n.Pos().Line != 0 {
		cg.currentPos = n.Pos()
	}

	// For non-void functions that contain defer stmts: alloca a {i8, retType} slot
	// so a defer thunk can override the return value.  Skip when no defer is present
	// to avoid generating dead code in the common case.
	if !irtypes.IsVoid(retType) && hasDeferStmt(n.Body) {
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotAlloca := entry.NewAlloca(slotType)
		// Zero-initialize the valid byte.
		validGep := entry.NewGetElementPtr(slotType, slotAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
		cg.curFnDeferRetAlloca = entry.NewBitCast(slotAlloca, irtypes.I8Ptr)
	} else {
		cg.curFnDeferRetAlloca = nil
	}

	// Always restore context, even on error paths (e.g. when genBody returns
	// an error during on-demand monomorphization of a generic struct method).
	prevTCOFuncName := cg.tcoFuncName
	prevTCOLoopTop := cg.tcoLoopTop
	prevTCOParams := cg.tcoParams
	prevMutualTCO := cg.mutualTCOEligible

	defer func() {
		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs
		cg.curFnAutoYield = prevAutoYield
		cg.curDeferRetSlotParam = prevDeferRetSlotParam
		cg.curFnDeferRetAlloca = prevFnDeferRetAlloca
		cg.curDeferThunkRetType = prevDeferThunkRetType
		cg.curFnEscapingVars = prevEscapingVars
		cg.curFnEscapingAliases = prevEscapingAliases
		cg.diCurrentScope = prevDiScope
		cg.tcoFuncName = prevTCOFuncName
		cg.tcoLoopTop = prevTCOLoopTop
		cg.tcoParams = prevTCOParams
		cg.mutualTCOEligible = prevMutualTCO
	}()

	// Register function in current scope so recursion works.
	cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

	// Mark the LLVM function as variadic if any tin param is varargs.
	for _, p := range n.Params {
		if p.IsVarArgs {
			f.Sig.Variadic = true

			break
		}
	}

	// Run the borrow analyzer over each parameter so the entry retain
	// and scope-exit release pair can be elided when the body uses the
	// parameter read-only.  Tin's calling convention puts both ops on
	// the callee side, so dropping both stays balanced and the
	// caller's binding rc is preserved through the call.  Skips
	// method receivers (parameters named "this") because trait
	// dispatch expects an owned receiver -- demoting that would race
	// with the trait fat-pointer release path.
	paramNames := make([]string, 0, len(n.Params))
	for _, p := range n.Params {
		if p.Name != "" && !p.IsVarArgs {
			paramNames = append(paramNames, p.Name)
		}
	}

	paramBorrows := cg.analyzeFunctionParamBorrows(n.Body, paramNames)

	// Alloca parameters and register them in scope.
	// Iterate tin params; skip varargs (no LLVM parameter), but register a
	// null placeholder so the name is defined inside the body.
	var firstParamAlloca *ir.InstAlloca

	llIdx := 0

	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			if astParam.Name != "" {
				// Register as null i8* placeholder; true forwarding needs va_list.
				null := constant.NewNull(irtypes.NewPointer(irtypes.I8))
				cg.curScope.set(astParam.Name, &scopeEntry{val: null, isAlloc: false})
			}

			continue
		}

		p := f.Params[llIdx]
		llIdx++
		alloca := entry.NewAlloca(p.Type())
		entry.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		// Borrowed parameters skip the callee's entry retain.
		// emitScopeRelease in runtime_scope.go matches it by also
		// skipping the release, so the pair stays balanced.
		paramBorrowed := paramBorrows[astParam.Name]
		if !paramBorrowed {
			cg.emitRetain(entry, p)
		}
		// Emit dbg.declare for this parameter in debug builds.
		cg.emitDbgDeclare(entry, alloca, astParam.Name, n.Pos().Line, uint64(llIdx), astParam.Type, p.Type())

		paramOwnership := ownershipOwned
		if paramBorrowed {
			paramOwnership = ownershipBorrowed
		}
		// Function parameters receive a by-value copy of the caller's struct.
		// The parameter is not the owner of the value; the caller is.  Mark
		// noDeinit so that scope-exit release of the parameter copy does not
		// invoke deinit (which would be a spurious call from the callee's
		// perspective and could double-free external resources).
		cg.curScope.set(astParam.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, noDeinit: true, isUnsigned: isUnsignedTinType(astParam.Type), scalarTypeName: scalar8BitTypeName(astParam.Type), tinType: astParam.Type, declPos: n.Pos(), ownership: paramOwnership})
		cg.warnIfBuiltinShadow("param", astParam.Name, n.Pos())
		// Record the parameter in --explain-ownership so the user
		// can see whether the analyzer demoted it from Owned to
		// Borrowed.
		cg.recordOwnership(astParam.Name, paramOwnership, "parameter")

		if llIdx == 1 {
			firstParamAlloca = alloca
		}
	}

	// TCO eligibility: direct, sync, non-extern, non-overloaded function whose
	// body contains at least one direct self tail call and has no defers. Params
	// must all be non-RC (no strings, arrays, any, fn) so we can update allocas
	// safely. Overloaded functions are excluded because a same-name call in the
	// body may target a sibling overload with different parameter types, not the
	// function itself.
	isTCO := n.IsExtern == "" &&
		!isAsyncTag(n.Tags) &&
		!hasDeferStmt(n.Body) &&
		!cg.overloadedNames[n.Name] &&
		len(n.Params) > 0 &&
		hasSelfTailCall(n.Name, n.Body)

	if isTCO {
		for _, astP := range n.Params {
			if astP.IsVarArgs {
				isTCO = false

				break
			}

			if e, ok := cg.curScope.lookup(astP.Name); ok && e.isAlloc {
				if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 && isRCTrackedType(alloca.ElemType) {
					isTCO = false

					break
				}
			}
		}
	}

	// startBlock is where the function body and match-subject load are emitted.
	// For TCO functions, entry only holds param allocas then jumps to tco_loop.
	startBlock := entry

	if isTCO {
		tcoLoop := f.NewBlock("tco_loop")
		entry.NewBr(tcoLoop)

		cg.tcoFuncName = n.Name
		cg.tcoLoopTop = tcoLoop
		cg.tcoParams = nil

		for _, astP := range n.Params {
			if !astP.IsVarArgs {
				cg.tcoParams = append(cg.tcoParams, astP.Name)
			}
		}

		startBlock = tcoLoop

		if cg.tcoReportFn != nil {
			cg.tcoReportFn(n.Name, "")
		}
	}

	// Mutual TCO eligibility: same as self-TCO but for calls to OTHER functions.
	// Requires non-RC return type so the musttail call result can be returned
	// directly without retain/release between the call and the ret instruction.
	// Async functions (including those that implicitly return Future[T]) are excluded
	// because their IR signatures change during the coro split pass.
	cg.mutualTCOEligible = n.IsExtern == "" &&
		!isAsyncTag(n.Tags) &&
		!isFutureRetType(n.RetType) &&
		!hasDeferStmt(n.Body) &&
		!isRCTrackedType(retType)

	// For where-list bodies, set the match subject to the first parameter so
	// that atom conditions (e.g. `where 'ok:`) compare against it.
	// The load is placed in startBlock so it re-executes on every loop iteration.
	prevMatchSubject := cg.matchSubject

	if _, isWhere := n.Body.(*ast.WhereList); isWhere && firstParamAlloca != nil {
		loadInst := startBlock.NewLoad(firstParamAlloca.ElemType, firstParamAlloca)
		cg.attachCurrentDbgLoc(loadInst)
		cg.matchSubject = loadInst
	}

	// Generate body (genBody ensures a terminator is added to the current block).
	_, bodyErr := cg.genBody(startBlock, n.Body, retType)
	cg.matchSubject = prevMatchSubject

	// Ensure all call instructions have !dbg (LLVM requires this when the
	// function has a DISubprogram attached).
	cg.ensureAllCallsHaveDbg(f)

	if bodyErr != nil {
		// Even on error, register the (partially compiled) function so it
		// appears in scope for callers that check for it. The error typically
		// occurs during on-demand monomorphization triggered from inside another
		// function body; the caller discards the error but still needs the fn.
		prevScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

		return bodyErr
	}

	// Restore context explicitly here (the defer is a safety net for error paths).
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.curBlock = prevBlock
	cg.pendingDeferFnI8s = prevDeferFnI8s
	cg.pendingDeferFrames = prevDeferFrames
	cg.pendingDeferEnvs = prevDeferEnvs
	cg.curFnEscapingVars = prevEscapingVars
	cg.curFnEscapingAliases = prevEscapingAliases
	cg.diCurrentScope = prevDiScope
	cg.tcoFuncName = prevTCOFuncName
	cg.tcoLoopTop = prevTCOLoopTop
	cg.tcoParams = prevTCOParams
	cg.lastSliceBase = prevLastSliceBase

	// Note: #no_recurse is enforced by checkAllNoRecurseFuncs (AST-level,
	// transitive) before this function is ever compiled. No IR walk needed.

	// Ensure function is registered in current scope.
	if cg.curScope != nil {
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}

	// If this function is in the async-callable set (or has #async tag directly),
	// generate its $coro variant. The #async tag check catches local functions
	// that were not discovered by the pre-pass call graph analysis.
	if cg.coroCallable[scopeName] || hasTag(n.Tags, "async") {
		if !cg.coroCallable[scopeName] {
			cg.coroCallable[scopeName] = true
		}

		coroKey := coroVersionName(scopeName)
		// Ensure the $coro stub exists in the current scope's vars before calling
		// genCoroFuncBody. For top-level functions the pre-pass already registered
		// the stub - predeclareCoroVariant is a no-op when vars[coroKey] is set.
		// For local/monomorphized async functions (not in the pre-pass), this
		// creates the stub so genCoroFuncBody can find it.
		if err := cg.predeclareCoroVariant(n, scopeName, false); err != nil {
			return err
		}

		if err := cg.genCoroFuncBody(n, coroKey, nil, nil); err != nil {
			return err
		}
	}
	// Emit the $colored variant when this fn is in the colored set (reached
	// from a coro body or boxed into a fat-fn-ptr value).  Same body as
	// sync, but with auto-yield enabled and lowered to a runtime yield
	// call -- callers in cooperative context route here instead of the
	// plain sync entry so loop back-edges / heavy calls cooperate with
	// the scheduler.  See docs/internals/fn-coloring.md.
	//
	// `#no_autoyield` opts the fn out: with yields suppressed, the
	// colored body would be byte-identical to the sync entry, so we
	// skip the emission entirely.  lookupColoredVariant returns nil for
	// the fn and slot 1 of its fat-fn-ptr falls back to slot 0.  Bare
	// calls from cooperative context similarly fall through (still
	// gated on funcHeuristics for the pre-call yield).
	if cg.coloredCallable[scopeName] && !hasTag(n.Tags, "no_autoyield") {
		if err := cg.predeclareColoredVariant(n, scopeName, false); err != nil {
			return err
		}

		if err := cg.genColoredFuncBody(n, coloredVersionName(scopeName)); err != nil {
			return err
		}
	}

	return nil
}

// genImplicitMain creates a main() function containing the top-level statements.
