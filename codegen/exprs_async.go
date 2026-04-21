package codegen

import (
	"fmt"
	"os"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genLambdaExpr(block *ir.Block, e *ast.LambdaExpr) (value.Value, error) {
	name := fmt.Sprintf("lambda.%d", cg.strCount)
	cg.strCount++

	// Step 1: identify free variables
	localNames := map[string]bool{}
	for _, p := range e.Params {
		localNames[p.Name] = true
	}

	freeNames := collectFreeVars(e.Body, localNames)

	// Resolve each free name in the current (outer) scope. Skip names that
	// resolve to module-level IR functions (not allocas) - those are callable
	// directly by name and don't need capturing.
	var captures []closureCapture

	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}

		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
		}

		var (
			val value.Value
			ty  irtypes.Type
		)

		if entry.isAlloc {
			pt := entry.val.Type().(*irtypes.PointerType)
			ty = pt.ElemType
			val = block.NewLoad(ty, entry.val)
		} else {
			val = entry.val
			ty = val.Type()
		}

		captures = append(captures, closureCapture{name: n, val: val, llvmTy: ty})
	}

	// Step 2: generate per-closure dtor (releases RC-tracked captures when env RC=0),
	// then build an RC-managed env struct with the dtor stored at field 0.
	var dtorFn *ir.Func

	for _, c := range captures {
		if isRCTrackedType(c.llvmTy) {
			dtorFn = cg.genClosureDtor(name+".dtor", captures)

			break
		}
	}

	envI8Ptr, envStructType := cg.buildClosureEnv(block, captures, dtorFn)

	// Step 3: create the lambda IR function with (i8* env, params...) sig
	llParams := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}

	for _, p := range e.Params {
		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}

		llParams = append(llParams, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void

	if e.RetType != nil {
		var err error

		retType, err = cg.tinTypeToLLVM(e.RetType)
		if err != nil {
			return nil, err
		}
	}

	f := cg.mod.NewFunc(name, retType, llParams...)
	entry := f.NewBlock("entry")

	prevCtx := cg.pushClosureCtx(f)

	// Step 4: unpack captures from env inside the lambda body.
	// unpackClosureEnv uses GEPs directly (env persists across calls) and retains
	// each RC-tracked capture so the body's scope-exit release is balanced.
	cg.unpackClosureEnv(entry, f, envStructType, captures)

	// Register lambda params (skip index 0 = env).
	for i, p := range e.Params {
		param := f.Params[i+1]

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return nil, err
		}

		// *cLayoutStruct params: C passes a native pointer, but the lambda body
		// expects a Tin wrapper pointer (with c_data_ptr). Build a stack-allocated
		// wrapper on the fly so that field accesses through the parameter work.
		if ptrTe, isPtrType := p.Type.(*ast.PointerType); isPtrType {
			if stTe, isSimple := ptrTe.Elem.(*ast.SimpleType); isSimple && cg.cLayoutStructs[stTe.Name] {
				wrapperSt := cg.structTypes[stTe.Name]
				wrapperAlloca := entry.NewAlloca(wrapperSt)
				entry.NewStore(constant.NewZeroInitializer(wrapperSt), wrapperAlloca)

				// Set type_id (field 0).
				typeID := cg.structTypeIDs[stTe.Name]
				typeIDGep := entry.NewGetElementPtr(wrapperSt, wrapperAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				entry.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)

				// Zero vtable fields (1..userFieldOffset-1).
				ufo := cg.userFieldOffset(stTe.Name)
				for v := int64(1); v < int64(ufo); v++ {
					vtGep := entry.NewGetElementPtr(wrapperSt, wrapperAlloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
					fieldType := wrapperSt.Fields[v]
					entry.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
				}

				// Store the incoming C pointer into c_data_ptr.
				cDataIdx := int64(cg.cDataPtrIndex(stTe.Name))
				cDataGep := entry.NewGetElementPtr(wrapperSt, wrapperAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
				i8Param := entry.NewBitCast(param, irtypes.I8Ptr)
				entry.NewStore(i8Param, cDataGep)

				// Store the wrapper address in the param alloca (type: *(*exvec_wrapper)).
				alloca := entry.NewAlloca(pt)
				entry.NewStore(wrapperAlloca, alloca)
				cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true})

				continue
			}
		}

		alloca := entry.NewAlloca(pt)
		entry.NewStore(param, alloca)
		// ARC: retain RC-tracked params so scope-exit release is balanced.
		// Same convention as genFuncDeclAs: callee owns a reference.
		cg.emitRetain(entry, param)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRCTrackedType(pt)})
	}

	// For where-list bodies, the match subject is the first parameter so that
	// atom and comparison conditions compare against it (mirroring genFuncDeclAs).
	prevMatchSubject := cg.matchSubject

	if _, isWhere := e.Body.(*ast.WhereList); isWhere && len(e.Params) > 0 {
		firstParamName := e.Params[0].Name
		if se, ok := cg.curScope.lookup(firstParamName); ok && se.isAlloc {
			pt := se.val.Type().(*irtypes.PointerType)
			cg.matchSubject = entry.NewLoad(pt.ElemType, se.val)
		}
	}

	term, err := cg.genBody(entry, e.Body, retType)
	cg.matchSubject = prevMatchSubject

	if err != nil {
		return nil, err
	}

	if !term {
		lastBlock := f.Blocks[len(f.Blocks)-1]
		if lastBlock.Term == nil {
			if irtypes.IsVoid(retType) {
				lastBlock.NewRet(nil)
			} else {
				lastBlock.NewRet(cg.zeroValue(retType))
			}
		}
	}

	cg.popClosureCtx(prevCtx)

	// Step 5: build and return fat pointer { fn_ptr, env_i8_ptr } using insertvalue
	// so no stack alloca is needed.
	fatStructType := irtypes.NewStruct(irtypes.NewPointer(f.Sig), irtypes.I8Ptr)
	fat0 := block.NewInsertValue(constant.NewUndef(fatStructType), f, 0)
	fat1 := block.NewInsertValue(fat0, envI8Ptr, 1)

	// Signal to genVarDecl whether this closure has captured variables so it can
	// skip the _tin_release_closure(null) call for non-capturing closures.
	cg.lastLambdaHadCaptures = len(captures) > 0

	return fat1, nil
}

// genClosureDtor generates a per-closure destructor IR function that releases
// any RC-tracked captures stored in the closure env (built by buildClosureEnv).
// The dtor signature is void(i8* env). The env layout matches buildClosureEnv:
// field 0 = i8* dtor_ptr, fields 1..N = captures.
func (cg *CodeGen) genClosureDtor(name string, captures []closureCapture) *ir.Func {
	// Reconstruct the env struct type (must match buildClosureEnv layout).
	fields := make([]irtypes.Type, len(captures)+1)

	fields[0] = irtypes.I8Ptr
	for i, c := range captures {
		fields[i+1] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)

	dtorFn := cg.mod.NewFunc(name, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))
	entry := dtorFn.NewBlock("entry")

	envTypedPtr := entry.NewBitCast(dtorFn.Params[0], irtypes.NewPointer(envStructType))

	for i, c := range captures {
		if !isRCTrackedType(c.llvmTy) {
			continue
		}

		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))
		fieldVal := entry.NewLoad(c.llvmTy, gep)
		cg.emitRelease(entry, fieldVal)
	}

	entry.NewRet(nil)

	return dtorFn
}

// genBoundMethod synthesizes a closure fat-pointer for `obj.methodName` where
// obj is of struct type structName.  The closure captures the receiver value
// and, when called, passes it as the first argument to structName_methodName.
// Returns (nil, nil) if no matching method is found (caller falls through to error).
func (cg *CodeGen) genBoundMethod(block *ir.Block, recvExpr ast.Node, obj value.Value, structName, methodName string) (value.Value, error) {
	irName := structName + "_" + methodName

	entry, ok := cg.curScope.lookup(irName)
	if !ok {
		return nil, nil
	}

	irFunc, isFunc := entry.val.(*ir.Func)
	if !isFunc {
		return nil, nil
	}

	// irFunc.Sig.Params[0] is the receiver; Params[1..] are the user-visible params.
	sig := irFunc.Sig
	if len(sig.Params) == 0 {
		return nil, nil // unexpected: static method, no receiver
	}

	// Determine receiver value to store in env.
	recvType := sig.Params[0]

	var recvVal value.Value

	if pt, isPtr := recvType.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(obj.Type()) {
		// Pointer receiver: use the original variable's alloca so mutations via
		// the closure are visible through the original binding.
		if lv, lvErr := cg.genLValue(block, recvExpr); lvErr == nil && lv != nil {
			recvVal = lv
			// When recvExpr is itself a pointer variable (e.g. `let f = &Foo{}`),
			// genLValue returns the alloca that holds *Foo (type **Foo).  The method
			// expects *Foo, so we must load through the extra indirection.
			if lvPt, isLvPtr := recvVal.Type().(*irtypes.PointerType); isLvPtr {
				if lvPt.ElemType.Equal(recvType) {
					recvVal = block.NewLoad(recvType, recvVal)
				}
			}
		} else {
			// Fall back: fresh alloca copy (mutations won't propagate).
			alloca := block.NewAlloca(obj.Type())
			block.NewStore(obj, alloca)
			recvVal = alloca
		}
	} else {
		recvVal = obj
	}

	// Build the closure env capturing just the receiver.
	recvCapture := closureCapture{name: "__recv", val: recvVal, llvmTy: recvVal.Type()}

	var dtorFn *ir.Func
	if isRCTrackedType(recvCapture.llvmTy) {
		dtorFn = cg.genClosureDtor(fmt.Sprintf("bound.%s.%s.%d.dtor", structName, methodName, cg.strCount), []closureCapture{recvCapture})
	}

	envI8Ptr, envStructType := cg.buildClosureEnv(block, []closureCapture{recvCapture}, dtorFn)

	// Build wrapper function: fn(i8* env, userParams...) retType
	wrapperName := fmt.Sprintf("bound.%s.%s.%d", structName, methodName, cg.strCount)
	cg.strCount++

	wrapperParams := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for i := 1; i < len(sig.Params); i++ {
		wrapperParams = append(wrapperParams, ir.NewParam(fmt.Sprintf("p%d", i), sig.Params[i]))
	}

	wrapFn := cg.mod.NewFunc(wrapperName, sig.RetType, wrapperParams...)
	wrapEntry := wrapFn.NewBlock("entry")

	// Unpack receiver from env.
	envTypedPtr := wrapEntry.NewBitCast(wrapFn.Params[0], irtypes.NewPointer(envStructType))
	recvGep := wrapEntry.NewGetElementPtr(envStructType, envTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	receiverArg := wrapEntry.NewLoad(recvVal.Type(), recvGep)

	// Call the original method with receiver + forwarded params.
	callArgs := make([]value.Value, 0, len(sig.Params))
	callArgs = append(callArgs, receiverArg)

	for i := 1; i < len(wrapFn.Params); i++ {
		callArgs = append(callArgs, wrapFn.Params[i])
	}

	result := wrapEntry.NewCall(irFunc, callArgs...)
	if irtypes.IsVoid(result.Type()) {
		wrapEntry.NewRet(nil)
	} else {
		wrapEntry.NewRet(result)
	}

	// Return fat pointer { wrapFn, envI8Ptr }.
	fatStructType := irtypes.NewStruct(irtypes.NewPointer(wrapFn.Sig), irtypes.I8Ptr)
	fat0 := block.NewInsertValue(constant.NewUndef(fatStructType), wrapFn, 0)
	fat1 := block.NewInsertValue(fat0, envI8Ptr, 1)

	cg.lastLambdaHadCaptures = true

	return fat1, nil
}

// Interpolated string

func (cg *CodeGen) genInterpolatedString(block *ir.Block, e *ast.InterpolatedString) (value.Value, error) {
	// Build a format string and argument list for printf/sprintf.
	var (
		fmtParts  []string
		args      []value.Value
		toRelease []value.Value // ARC: RC-tracked temporaries to release after snprintf
	)

	for _, part := range e.Parts {
		if !part.IsExpr {
			// Escape % in literal parts.
			escaped := strings.ReplaceAll(part.Str, "%", "%%")
			fmtParts = append(fmtParts, escaped)
		} else {
			val, err := cg.genExpr(block, part.Expr)
			if err != nil {
				return nil, err
			}

			if val == nil {
				fmtParts = append(fmtParts, "(nil)")

				continue
			}

			t := val.Type()

			// If a format specifier was provided, use it directly.
			if part.Format != "" {
				fmtSpec := part.Format
				lastChar := fmtSpec[len(fmtSpec)-1]
				prefix := fmtSpec[:len(fmtSpec)-1]

				switch lastChar {
				case 'x', 'X', 'o', 'u':
					// Unsigned/hex/octal integer format
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))

							if it.BitSize < 32 {
								val = block.NewZExt(val, irtypes.I32)
							}
						}

						args = append(args, val)

						continue
					}
				case 'd', 'i':
					// Signed decimal format; use zero-extend for unsigned 8-bit types.
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))

							if it.BitSize < 32 {
								ty8 := cg.exprByte8Type(part.Expr)
								if ty8 == "byte" || ty8 == "u8" {
									val = block.NewZExt(val, irtypes.I32)
								} else {
									val = block.NewSExt(val, irtypes.I32)
								}
							}
						}

						args = append(args, val)

						continue
					}
				case 'f', 'e', 'g', 'E', 'G':
					// Floating-point format
					fmtParts = append(fmtParts, "%"+fmtSpec)

					if irtypes.IsFloat(t) {
						if t != irtypes.Double {
							val = block.NewFPExt(val, irtypes.Double)
						}
					} else if irtypes.IsInt(t) {
						val = block.NewSIToFP(val, irtypes.Double)
					}

					args = append(args, val)

					continue
				case 's':
					// String format
					if isStringType(t) {
						fmtParts = append(fmtParts, "%"+fmtSpec)
						args = append(args, cg.extractStringPtr(block, val))

						continue
					}
				}
				// Unknown format specifier - fall through to default handling
			}

			switch {
			case isStringType(t):
				fmtParts = append(fmtParts, "%s")
				ptr := cg.extractStringPtr(block, val)
				args = append(args, ptr)
				// ARC: temporary string (call/concat result) must be released after snprintf.
				if isTemporaryProducer(part.Expr) {
					toRelease = append(toRelease, val)
				}
			case irtypes.IsInt(t):
				it := t.(*irtypes.IntType)
				switch it.BitSize {
				case 1:
					// bool: print "true" or "false"
					truePtr := cg.newGlobalString("true")
					falsePtr := cg.newGlobalString("false")
					selected := block.NewSelect(val, truePtr, falsePtr)

					fmtParts = append(fmtParts, "%s")
					args = append(args, selected)
				case 8:
					// Dispatch by Tin type: char->%c, byte->%x, u8/i8->%d
					// Use format specifiers ({c:d}, {c:x}, {c:c}) to override.
					switch cg.exprByte8Type(part.Expr) {
					case "char":
						fmtParts = append(fmtParts, "%c")
					case "byte":
						fmtParts = append(fmtParts, "%x")
					default: // "u8", "i8", ""
						fmtParts = append(fmtParts, "%d")
					}

					// byte/u8 are unsigned - zero-extend; char/i8 are signed - sign-extend.
					ty8 := cg.exprByte8Type(part.Expr)
					if ty8 == "byte" || ty8 == "u8" {
						val = block.NewZExt(val, irtypes.I32)
					} else {
						val = block.NewSExt(val, irtypes.I32)
					}

					args = append(args, val)
				case 128:
					// i128/u128: pre-convert to C string via runtime helper, use %s
					ty128 := cg.exprByte8Type(part.Expr)

					var cstr value.Value

					if ty128 == "u128" {
						cstr = block.NewCall(cg.ensureU128ToCstr(), val)
					} else {
						cstr = block.NewCall(cg.ensureI128ToCstr(), val)
					}

					fmtParts = append(fmtParts, "%s")
					args = append(args, cstr)
				default:
					if cg.exprIsUnsigned(part.Expr) {
						// Unsigned 16/32/64-bit: zero-extend to i64, use %llu.
						fmtParts = append(fmtParts, "%llu")

						it2 := t.(*irtypes.IntType)

						if it2.BitSize < 64 {
							val = block.NewZExt(val, irtypes.I64)
						}

						args = append(args, val)
					} else {
						fmtParts = append(fmtParts, "%lld")
						val = cg.coerce(block, val, irtypes.I64)

						args = append(args, val)
					}
				}
			case irtypes.IsFloat(t):
				ft := t.(*irtypes.FloatType)
				if ft.Kind == irtypes.FloatKindFP128 {
					// f128: pre-convert to C string via runtime helper, use %s
					cstr := block.NewCall(cg.ensureF128ToCstr(), val)

					fmtParts = append(fmtParts, "%s")
					args = append(args, cstr)
				} else if t == irtypes.Double {
					// f64: use %f (e.g. "1.000000")
					fmtParts = append(fmtParts, "%f")
					args = append(args, val)
				} else {
					// f32: use %g (e.g. "3" for 3.0, "1.5" for 1.5)
					fmtParts = append(fmtParts, "%g")
					val = block.NewFPExt(val, irtypes.Double)
					args = append(args, val)
				}
			default:
				// print trait: struct or fat-pointer with a print() method.
				if strVal, ok := cg.callPrintTrait(block, val); ok {
					fmtParts = append(fmtParts, "%s")
					ptr := cg.extractStringPtr(block, strVal)
					args = append(args, ptr)
					// ARC: ::print returns a fresh string; release after snprintf.
					toRelease = append(toRelease, strVal)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
					args = append(args, val)
				}
			}
		}
	}

	// Build result string using snprintf with a two-pass approach:
	//   1. snprintf(NULL, 0, fmt, ...) -> returns the required length (excluding NUL).
	//   2. _tin_rc_alloc(len+1) -> allocate exact buffer with ARC header.
	//   3. snprintf(buf, len+1, fmt, ...) -> fill buffer.
	// This avoids a fixed-size buffer and handles arbitrarily long interpolations.
	// IMPORTANT: must use _tin_rc_alloc (not malloc) so that the result is ARC-tracked
	// and _tin_release can safely read the RC header 8 bytes before the returned ptr.
	fmtStr := strings.Join(fmtParts, "")
	fmtPtr := cg.newGlobalString(fmtStr)
	snprintfFn := cg.ensureSnprintf()

	// Pass 1: measure required length.
	nullBuf := constant.NewNull(irtypes.I8Ptr)
	sizeZero := constant.NewInt(irtypes.I64, 0)
	measureArgs := []value.Value{nullBuf, sizeZero, fmtPtr}
	measureArgs = append(measureArgs, args...)
	needed := block.NewCall(snprintfFn, measureArgs...) // i32
	neededI64 := block.NewSExt(needed, irtypes.I64)
	allocSize := block.NewAdd(neededI64, constant.NewInt(irtypes.I64, 1)) // +1 for NUL

	// Pass 2: allocate with ARC header and fill.
	buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
	fillArgs := []value.Value{buf, allocSize, fmtPtr}
	fillArgs = append(fillArgs, args...)
	block.NewCall(snprintfFn, fillArgs...)

	fatPtrType := stringFatPtrType()
	fatAlloca := block.NewAlloca(fatPtrType)
	ptrGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(buf, ptrGep)
	lenGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(neededI64, lenGep)

	// ARC: release temporary RC-tracked values (::print strings, concat/call results)
	// now that snprintf has consumed their data pointers.
	for _, sv := range toRelease {
		cg.emitRelease(block, sv)
	}

	return block.NewLoad(fatPtrType, fatAlloca), nil
}

// --------------------------------------------------------------------------
// Fiber expression helpers
// --------------------------------------------------------------------------

// canonicalUnitStructName returns the LLVM struct name for the sync Unit type.
// After canonical naming, this is "sync__Unit" when sync was loaded from source.
// Falls back to "Unit" for pre-compiled .tin.mod scenarios.
func (cg *CodeGen) canonicalUnitStructName() string {
	// Prefer the canonical package-prefixed name.
	if _, ok := cg.structTypes["sync__Unit"]; ok {
		return "sync__Unit"
	}
	// Try the type alias (covers pre-compiled mod scenarios).
	if alias, ok := cg.typeAliases["sync::Unit"]; ok {
		if simple, ok2 := alias.(*ast.SimpleType); ok2 {
			return simple.Name
		}
	}

	return "Unit"
}

// wrapPidInFuture wraps a fiber PID (i64) in a Future[T] struct value.
// calleeName is used to look up the original return type; pass "" for void/do-block spawns.
// Returns an error if the sync module is not available or Future[T] cannot be instantiated.
func (cg *CodeGen) wrapPidInFuture(block *ir.Block, pid value.Value, calleeName string) (value.Value, error) {
	// Determine the type parameter: original function's return type, or Unit for void.
	var retTypeExpr ast.TypeExpr

	if calleeName != "" {
		if origDecl, ok := cg.funcDecls[calleeName]; ok && origDecl.RetType != nil {
			retTypeExpr = origDecl.RetType
		}
	}

	if retTypeExpr == nil {
		retTypeExpr = &ast.SimpleType{Name: "Unit"}
	}

	// Get canonical name for the concrete type parameter (e.g., "i64", "string", "[]byte").
	// Use typeExprCanonicalKey rather than llvmTypeName(tinTypeToLLVM(...)) because
	// llvmTypeName cannot distinguish [byte] from string (both are {i8*, i64} fat ptrs).
	retTypeStr := cg.typeExprCanonicalKey(retTypeExpr)

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if _, exists := cg.structTypes[futureConcreteName]; !exists {
		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			return nil, fmt.Errorf("spawn: cannot instantiate Future[%s]: %w", retTypeStr, monoErr)
		}
	}

	// Call Future[T].make(pid) to construct the struct value properly
	// (sets type_id, vtable pointer, and pid field).
	makeFnName := futureConcreteName + "_make"

	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: sync package failed to load: %w; ensure the tin executable is alongside the stdlib/ directory", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - sync package could not be loaded; ensure the tin executable is alongside the stdlib/ directory, or add `use sync` explicitly before using spawn/await", retTypeStr)
	}

	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// peekStructTypeName returns the LLVM struct type name for a simple identifier
// expression without evaluating it.  Returns "" when the type cannot be
// determined statically (e.g., complex sub-expression).
func (cg *CodeGen) peekStructTypeName(identName string) string {
	se, ok := cg.curScope.lookup(identName)
	if !ok {
		return ""
	}

	t := se.val.Type()
	if se.isAlloc {
		if pt, ok2 := t.(*irtypes.PointerType); ok2 {
			t = pt.ElemType
		}
	}

	if name := cg.typeNameOf(t); name != "" {
		return name
	}

	if pt, ok2 := t.(*irtypes.PointerType); ok2 {
		return cg.typeNameOf(pt.ElemType)
	}

	return ""
}

// directCallHasCoroVariant returns true if callNode is a direct call to an
// {#async} function (i.e., its $coro ramp exists in scope).  Does not evaluate
// any sub-expressions or generate IR.
func (cg *CodeGen) directCallHasCoroVariant(callNode *ast.CallExpr) bool {
	switch fn := callNode.Func.(type) {
	case *ast.FieldAccess:
		if id, ok := fn.Expr.(*ast.Identifier); ok {
			if structName := cg.peekStructTypeName(id.Name); structName != "" {
				_, ok2 := cg.curScope.lookup(structName + "_" + fn.Field + "$coro")

				return ok2
			}
		}
	case *ast.Identifier:
		_, ok := cg.curScope.lookup(fn.Name + "$coro")

		return ok
	}

	return false
}

// genInlineAsyncDrive drives an {#async} function call inline within the
// current coroutine, without spawning a new fiber.
//
// Called when `await asyncFn(args)` is encountered (no spawn) and inCoroFn==true.
// Instead of allocating a fiber and joining it, we:
//  1. Call the $coro ramp to allocate only the inner coroutine frame.
//  2. Resume the inner coroutine until it completes.
//  3. Whenever the inner coroutine yields, yield the outer coroutine too -
//     the scheduler will resume both in turn.
//  4. When the inner coroutine is done, take its result via _tin_coro_take_result.
//
// Returns (nil, nil) when the callee has no $coro variant (not {#async});
// the caller should fall through to the regular spawn+join path.
func (cg *CodeGen) genInlineAsyncDrive(block *ir.Block, callNode *ast.CallExpr) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()

	var (
		coroFn     *ir.Func
		coroArgs   []value.Value
		origFnName string
	)

	switch fn := callNode.Func.(type) {
	case *ast.FieldAccess:
		// Method call: obj.method(args...)
		// Peek at the struct type without evaluating to decide quickly.
		structName := ""

		if id, ok2 := fn.Expr.(*ast.Identifier); ok2 {
			if se, ok3 := cg.curScope.lookup(id.Name); ok3 {
				t := se.val.Type()
				if se.isAlloc {
					if pt, ok4 := t.(*irtypes.PointerType); ok4 {
						t = pt.ElemType
					}
				}

				structName = cg.typeNameOf(t)
				if structName == "" {
					if pt, ok4 := t.(*irtypes.PointerType); ok4 {
						structName = cg.typeNameOf(pt.ElemType)
					}
				}
			}
		}

		if structName == "" {
			return nil, nil // can't determine struct type without evaluation - fall through
		}

		coroName := structName + "_" + fn.Field + "$coro"

		se, ok2 := cg.curScope.lookup(coroName)
		if !ok2 {
			return nil, nil // not {#async} - fall through
		}

		var ok3 bool

		coroFn, ok3 = se.val.(*ir.Func)
		if !ok3 {
			return nil, nil
		}

		origFnName = structName + "_" + fn.Field

		// Evaluate the object expression.
		objVal, err := cg.genExpr(block, fn.Expr)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		// Coerce this arg to the $coro ramp's first param type.
		// If the method expects a pointer receiver (*T) but we have a value T,
		// use genLValue to get the address or fall back to a temp alloca.
		thisArg := objVal

		if len(coroFn.Params) > 0 {
			firstParamTy := coroFn.Params[0].Type()
			if pt, isPtr := firstParamTy.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(objVal.Type()) {
				if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
					thisArg = lv
				} else {
					tmp := block.NewAlloca(objVal.Type())
					block.NewStore(objVal, tmp)
					thisArg = tmp
				}
			} else {
				thisArg = cg.coerce(block, objVal, firstParamTy)
			}
		}

		coroArgs = []value.Value{thisArg}

		for i, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			if i+1 < len(coroFn.Params) {
				av = cg.coerce(block, av, coroFn.Params[i+1].Type())
			}

			coroArgs = append(coroArgs, av)
		}

		// Fast path: Channel[T].send and Channel[T].recv - inline the blocking
		// retry loop directly into the outer coro using the outer coro's own
		// coro.suspend.  This eliminates the inner $coro frame allocation
		// (2 malloc/free per operation, 4 per round trip) at the cost of a
		// slightly larger outer coro frame (pid + blocked_val spilled to frame).
		// Channel[T].send and Channel[T].recv fast path.
		// structName may be bare ("Channel__i64") or package-prefixed ("sync__Channel__i64").
		if strings.HasPrefix(structName, "Channel__") || strings.HasPrefix(structName, "sync__Channel__") {
			if fn.Field == "send" && len(coroArgs) == 2 {
				var sendAstArg ast.Node
				if len(callNode.Args) >= 1 {
					sendAstArg = callNode.Args[0]
				}

				return cg.genDirectChanSend(block, coroArgs[0], coroArgs[1], sendAstArg)
			}

			if fn.Field == "recv" && len(coroArgs) == 1 {
				// Prefer deriving the element type from the concrete struct name
				// (e.g. "sync__Channel__*counter_t" -> elemType = *counter_t).
				// This handles pointer and other complex type parameters correctly
				// because the type-param alias (T -> *counter_t) has already been
				// cleaned up by the time the caller runs genDirectChanRecv.
				if elemType := cg.chanElemTypeFromName(structName); elemType != nil && !irtypes.IsVoid(elemType) {
					return cg.genDirectChanRecv(block, coroArgs[0], elemType)
				}
				// Fallback: origDecl.RetType (works for simple non-aliased types).
				if origDecl, ok4 := cg.funcDecls[origFnName]; ok4 && origDecl.RetType != nil {
					if elemType, err4 := cg.tinTypeToLLVM(origDecl.RetType); err4 == nil && elemType != nil && !irtypes.IsVoid(elemType) {
						return cg.genDirectChanRecv(block, coroArgs[0], elemType)
					}
				}
			}
		}

	case *ast.Identifier:
		// Free function call: fn(args...)
		coroName := fn.Name + "$coro"

		se, ok2 := cg.curScope.lookup(coroName)
		if !ok2 {
			return nil, nil // not {#async} - fall through
		}

		var ok3 bool

		coroFn, ok3 = se.val.(*ir.Func)
		if !ok3 {
			return nil, nil
		}

		origFnName = fn.Name

		for i, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			if i < len(coroFn.Params) {
				av = cg.coerce(block, av, coroFn.Params[i].Type())
			}

			coroArgs = append(coroArgs, av)
		}

	default:
		return nil, nil // unsupported callee shape - fall through
	}

	cg.usesAnyFiber = true

	// Call the $coro ramp: allocates (or stack-allocates if coro-elide fires)
	// the inner coroutine frame and returns i8* handle.
	// Does NOT run the body; body starts on the first coro.resume call.
	innerHdl := block.NewCall(coroFn, coroArgs...)

	// ---------------------------------------------------------------
	// Drive loop:
	//   drive.loop:
	//     _tin_inline_result_mode_begin()   ; arm TLS result buffer for inner coro
	//     llvm.coro.resume(inner)           ; run body until yield or done
	//     done = llvm.coro.done(inner)
	//     br done ? drive.done : drive.yield
	//   drive.yield:
	//     sp = llvm.coro.suspend(outer) ; outer suspends
	//     switch sp: 0 -> drive.loop, 1 -> cleanup
	//   drive.done:
	//     result = _tin_coro_take_result()
	//     llvm.coro.destroy(inner)
	//     _tin_inline_result_mode_end()
	// ---------------------------------------------------------------
	// mode_begin is placed at the TOP of driveLoopBlk so it fires before EVERY
	// coro.resume - including re-entries after the outer fiber was parked and
	// resumed (at which point the worker loop reset _inline_result_mode to 0).
	// This keeps the TLS fast path active across park/unpark cycles.
	inlineBeginFn := cg.ensureExternDecl("_tin_inline_result_mode_begin", irtypes.Void,
		[]*ir.Param{}, false)

	driveLoopBlk := cg.newBlock("coro.drive.loop")
	block.NewBr(driveLoopBlk)

	driveLoopBlk.NewCall(inlineBeginFn)
	driveLoopBlk.NewCall(cg.coroResumeFn, innerHdl)
	done := driveLoopBlk.NewCall(cg.coroDoneFn, innerHdl)
	driveDoneBlk := cg.newBlock("coro.drive.done")
	driveYieldBlk := cg.newBlock("coro.drive.yield")
	driveLoopBlk.NewCondBr(done, driveDoneBlk, driveYieldBlk)

	// Yield path: inner yielded -> outer suspends to let inner run.
	// No _tin_fiber_yield_coro call needed (it's a no-op; worker loop
	// handles re-enqueue when FIBER_RUNNING status after _coro_resume returns).
	sp := driveYieldBlk.NewCall(cg.coroSuspendFn, coroNone, constant.NewInt(irtypes.I1, 0))
	suspBlk := cg.newBlock("coro.drive.suspended")
	cleanupBrBlk := cg.newBlock("coro.drive.cleanup.br")
	driveYieldBlk.NewSwitch(sp, suspBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), driveLoopBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBrBlk),
	)
	suspBlk.NewRet(cg.curCoroHdl)
	cleanupBrBlk.NewBr(cg.curCoroFrame.cleanupEntry)

	// Done path: take result, destroy inner frame.
	// llvm.coro.destroy would run the cleanup path (coro.end + coro.free), but
	// LLVM's coro-split pass generates empty destroy functions for trivially-
	// destructible C/Tin coroutines (no C++ dtors) - the cleanup call is optimized
	// away.  Call _tin_coro_free explicitly to return the heap-allocated frame to
	// the per-thread pool.  _tin_coro_free(null) is a no-op when coro-elide
	// stack-allocated the frame, so this is safe in all cases.
	resultRaw := driveDoneBlk.NewCall(cg.coroTakeResultFn)
	coroFreeFn := cg.ensureExternDecl("_tin_coro_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
	driveDoneBlk.NewCall(coroFreeFn, innerHdl)

	// End inline-result mode (balanced with the begin call before the ramp).
	inlineEndFn := cg.ensureExternDecl("_tin_inline_result_mode_end", irtypes.Void,
		[]*ir.Param{}, false)
	driveDoneBlk.NewCall(inlineEndFn)

	cg.curBlock = driveDoneBlk

	// Mark driveDoneBlk as a yield-resume equivalent so genYieldAutoAt suppresses
	// the redundant autoyield at the enclosing for-loop backedge.  The drive loop
	// already contains its own suspension points (coro.drive.yield) that fire when
	// the inner $coro blocks.  When the drive completes without blocking, the outer
	// fiber's natural park/unpark via the channel wakes the next fiber - no extra
	// autoyield is needed.
	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[driveDoneBlk] = true
	}

	// Determine result type (same lookup as wrapPidInFuture).
	var retTypeExpr ast.TypeExpr

	if origFnName != "" {
		if origDecl, ok2 := cg.funcDecls[origFnName]; ok2 && origDecl.RetType != nil {
			retTypeExpr = origDecl.RetType
		}
	}

	if retTypeExpr == nil {
		// void/Unit result - nothing to free.
		return constant.NewInt(irtypes.I1, 1), nil
	}

	retLLVM, err := cg.tinTypeToLLVM(retTypeExpr)
	if err != nil || retLLVM == nil || irtypes.IsVoid(retLLVM) {
		return constant.NewInt(irtypes.I1, 1), nil
	}

	typedPtr := driveDoneBlk.NewBitCast(resultRaw, irtypes.NewPointer(retLLVM))
	result := driveDoneBlk.NewLoad(retLLVM, typedPtr)
	// Free the result buffer (no-op if TLS, free() if heap-allocated for spawned fibers).
	inlineFreeFn := cg.ensureExternDecl("_tin_inline_result_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
	driveDoneBlk.NewCall(inlineFreeFn, resultRaw)

	return result, nil
}

// genDirectChanSend emits an inline channel-send retry loop that uses the outer
// coro's own llvm.coro.suspend instead of allocating an inner send$coro frame.
//
// Equivalent to the generated code for:
//
//	fn{#async #no_autoyield} send(this *Channel[T], val T) =
//	  let pid = _tin_current_pid()
//	  for true:
//	    let r = _tin_channel_send_blocking(this._ptr, &val, sizeof(T), isrc(T), pid)
//	    if r == -1: panic("send on closed channel")
//	    if r == 0: return
//	    yield   <- replaced by outer coro.suspend
//
// Eliminates 1 malloc + 1 free per send (2 per round trip).
func (cg *CodeGen) genDirectChanSend(block *ir.Block, thisPtr value.Value, valArg value.Value, astArg ast.Node) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// Load ch._ptr from the Channel struct.
	// Layout: [i32 type_id, i8* _ptr, ...] so _ptr is at LLVM field index 1.
	// Use fieldIndex for correctness in case the layout changes.
	pt, isPtr := thisPtr.Type().(*irtypes.PointerType)
	if !isPtr {
		_, _ = fmt.Fprintf(os.Stderr, "tin: warning: genDirectChanSend: expected pointer type, got %T - falling back to slow send$coro path\n", thisPtr.Type())

		return nil, nil
	}

	chanStructTy := pt.ElemType

	ptrFieldIdx := int64(cg.fieldIndex(cg.typeNameOf(chanStructTy), "_ptr"))
	if ptrFieldIdx < 0 {
		ptrFieldIdx = 1 // fallback: type_id at 0, _ptr at 1
	}

	ptrFieldGEP := block.NewGetElementPtr(chanStructTy, thisPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, ptrFieldIdx))
	chPtr := block.NewLoad(irtypes.I8Ptr, ptrFieldGEP)

	// Alloca for val so send_blocking can take &val.  Allocated in the outer coro
	// frame - persists across suspensions.  The value is set once and retried
	// until the channel accepts it.
	elemType := valArg.Type()
	valSlot := block.NewAlloca(elemType)
	block.NewStore(valArg, valSlot)
	valPtr := block.NewBitCast(valSlot, irtypes.I8Ptr)

	// sizeof(T) and is_rc - compile-time constants.
	elemSize := cg.llvmSizeOf(block, elemType)

	isRCVal := constant.NewInt(irtypes.I32, 0)
	if isRCTrackedType(elemType) {
		isRCVal = constant.NewInt(irtypes.I32, 1)
	}

	// pid is constant for the lifetime of the fiber - hoist before the retry loop
	// so the TLS lookup is not repeated on every iteration.
	// Load _current_pid directly as a TLS variable (no function call overhead).
	pidVar := cg.ensureExternTLSVar("_current_pid", irtypes.I64)
	pid := block.NewLoad(irtypes.I64, pidVar)

	sendFn := cg.ensureExternDecl("_tin_channel_send_blocking", irtypes.I32,
		[]*ir.Param{
			ir.NewParam("ch", irtypes.I8Ptr),
			ir.NewParam("val", irtypes.I8Ptr),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("is_rc", irtypes.I32),
			ir.NewParam("pid", irtypes.I64),
		}, false)

	retryBlk := cg.newBlock("chan.send.retry")
	block.NewBr(retryBlk)

	r := retryBlk.NewCall(sendFn, chPtr, valPtr, elemSize, isRCVal, pid)

	// r == -1 -> channel closed -> panic.
	isClosed := retryBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, -1))
	checkDoneBlk := cg.newBlock("chan.send.check")
	panicBlk := cg.newBlock("chan.send.panic")
	retryBlk.NewCondBr(isClosed, panicBlk, checkDoneBlk)

	// Panic block - must follow the coro completion path (not a bare ret).
	panicMsg := cg.newGlobalString("send on closed channel")
	panicBlk.NewCall(cg.ensurePanicFn(), panicMsg)
	cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
	cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)

	// r == 0 -> success
	// r == 2 -> handoff: direct delivery to a waiting receiver; yield once then done
	// otherwise -> park and retry
	isDone := checkDoneBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 0))
	doneBlk := cg.newBlock("chan.send.done")
	checkHandoffBlk := cg.newBlock("chan.send.check.handoff")
	checkDoneBlk.NewCondBr(isDone, doneBlk, checkHandoffBlk)

	isHandoff := checkHandoffBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 2))
	handoffBlk := cg.newBlock("chan.send.handoff")
	yieldBlk := cg.newBlock("chan.send.yield")
	checkHandoffBlk.NewCondBr(isHandoff, handoffBlk, yieldBlk)

	// Handoff: attempt pre-registration in the sender's next recv channel so the
	// worker can go directly to BLOCKED after coro.suspend instead of routing via
	// LQ.  If _tin_prepark_next_recv succeeds it sets pending_park, which takes
	// priority over handoff_yield in the worker's yield-path check.  Either way
	// the same coro.suspend is used; the worker picks the right path from flags.
	preparkFn := cg.ensureExternDecl("_tin_prepark_next_recv", irtypes.I32,
		[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)
	handoffBlk.NewCall(preparkFn, pid)
	// On resume the send is already complete - go straight to doneBlk.
	cg.emitInlineChanSuspend("chan.send.handoff", handoffBlk, doneBlk, doneBlk)

	// Park and retry: outer coro suspends until the channel has room.
	cg.emitInlineChanSuspend("chan.send", yieldBlk, retryBlk, doneBlk)
	// cg.curBlock == doneBlk after emitInlineChanSuspend.

	// Release temporary RC-tracked value after the send succeeds.
	// _tin_channel_send_blocking retains the element when is_rc==1, so the
	// sender's original reference must be dropped once the send completes.
	// Named variable arguments are owned by their enclosing scope and must NOT
	// be released here - the scope's exit will handle them.
	if astArg != nil && !isCopyExpr(astArg) && isRCTrackedType(valArg.Type()) {
		cg.emitRelease(doneBlk, valArg)
	}

	return constant.NewInt(irtypes.I1, 1), nil // void send - return sentinel i1 true
}

// genDirectChanRecv emits an inline channel-recv retry loop that uses the outer
// coro's own llvm.coro.suspend instead of allocating an inner recv$coro frame.
//
// Equivalent to the generated code for:
//
//	fn{#async #no_autoyield} recv(this *Channel[T]) T =
//	  let blocked = _tin_channel_recv_blocked_val()
//	  let pid = _tin_current_pid()
//	  for true:
//	    let r = _tin_channel_recv_blocking(this._ptr, pid)
//	    if r == null: panic("recv on closed channel")
//	    if (r as i64) != blocked: return *(r as *T)
//	    yield   <- replaced by outer coro.suspend
//
// Eliminates 1 malloc + 1 free per recv (2 per round trip).
func (cg *CodeGen) genDirectChanRecv(block *ir.Block, thisPtr value.Value, elemType irtypes.Type) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// Load ch._ptr from the Channel struct.
	pt, isPtr := thisPtr.Type().(*irtypes.PointerType)
	if !isPtr {
		_, _ = fmt.Fprintf(os.Stderr, "tin: warning: genDirectChanRecv: expected pointer type, got %T - falling back to slow recv$coro path\n", thisPtr.Type())

		return nil, nil
	}

	chanStructTy := pt.ElemType

	ptrFieldIdx := int64(cg.fieldIndex(cg.typeNameOf(chanStructTy), "_ptr"))
	if ptrFieldIdx < 0 {
		ptrFieldIdx = 1 // fallback: type_id at 0, _ptr at 1
	}

	ptrFieldGEP := block.NewGetElementPtr(chanStructTy, thisPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, ptrFieldIdx))
	chPtr := block.NewLoad(irtypes.I8Ptr, ptrFieldGEP)

	// Alloca for result - written by _tin_channel_recv_direct, persists across
	// suspensions so the retry loop can safely re-use the slot on wakeup.
	outSlot := block.NewAlloca(elemType)
	outPtr := block.NewBitCast(outSlot, irtypes.I8Ptr)

	// pid is constant for the lifetime of the fiber - hoist before the retry loop
	// so the TLS lookup is not repeated on every iteration.
	// Load _current_pid directly as a TLS variable (no function call overhead).
	pidVar := cg.ensureExternTLSVar("_current_pid", irtypes.I64)
	pid := block.NewLoad(irtypes.I64, pidVar)

	// _tin_channel_recv_direct writes directly into caller's alloca, eliminating
	// the per-thread TLS scratch buffer and pthread_getspecific overhead.
	// Returns: 0 = dequeued, 1 = blocked/contended (yield+retry), -1 = closed.
	recvFn := cg.ensureExternDecl("_tin_channel_recv_direct", irtypes.I32,
		[]*ir.Param{
			ir.NewParam("ch", irtypes.I8Ptr),
			ir.NewParam("pid", irtypes.I64),
			ir.NewParam("out", irtypes.I8Ptr),
		}, false)

	retryBlk := cg.newBlock("chan.recv.retry")
	block.NewBr(retryBlk)

	r := retryBlk.NewCall(recvFn, chPtr, pid, outPtr)

	// r == -1 -> channel closed and drained -> panic.
	isClosed := retryBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, -1))
	checkBlk := cg.newBlock("chan.recv.check")
	panicBlk := cg.newBlock("chan.recv.panic")
	retryBlk.NewCondBr(isClosed, panicBlk, checkBlk)

	panicMsg := cg.newGlobalString("recv on closed channel")
	panicBlk.NewCall(cg.ensurePanicFn(), panicMsg)
	cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
	cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)

	// r == 1 -> yield and retry; r == 0 -> value written to outSlot.
	isBlocked := checkBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 1))
	doneBlk := cg.newBlock("chan.recv.done")
	yieldBlk := cg.newBlock("chan.recv.yield")
	checkBlk.NewCondBr(isBlocked, yieldBlk, doneBlk)

	// Yield: outer coro suspends until the channel has data.
	cg.emitInlineChanSuspend("chan.recv", yieldBlk, retryBlk, doneBlk)

	// Done: load T from the alloca that recv_direct wrote into.
	result := doneBlk.NewLoad(elemType, outSlot)

	return result, nil
}

// activeSpawnFn returns the spawn function for the current context.
//
// All spawns use _tin_fiber_spawn_joinable (prejoined=1) by default so that a
// spawned fiber's slot cannot be ff_reclaimed and reused before the spawner
// calls _tin_fiber_join.  This is correct for:
//   - stored futures: `let f = spawn fn()` or `futures ++= spawn fn()` (awaited later)
//   - immediately awaited: `await spawn fn()` (auto-spawn path)
//   - non-coro context: test bodies, non-async main (TOCTOU fix)
//
// The only exception is a statement-level spawn (ExprStmt wrapping SpawnExpr)
// where the result is explicitly discarded.  In that case spawnFireForget=true
// allows _tin_fiber_spawn (prejoined=0) so the fiber can be ff_reclaimed at
// completion, keeping its slot available for reuse.
func (cg *CodeGen) activeSpawnFn() *ir.Func {
	if cg.spawnFireForget {
		return cg.fiberSpawnFn
	}

	return cg.fiberSpawnJoinableFn
}

// genSpawnExpr generates code for `spawn callExpr`.
// The callee must be a function marked {#async} (in coroCallable).
// Returns Future[T] wrapping the fiber PID.
func (cg *CodeGen) genSpawnExpr(block *ir.Block, e *ast.SpawnExpr) (value.Value, error) {
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// spawn do: block -> synthesize an anonymous {#async} function and spawn it.
	if e.DoBlock != nil {
		return cg.genSpawnDoBlock(block, e.DoBlock)
	}

	// Determine the call node and callee name.
	callNode, ok := e.Call.(*ast.CallExpr)
	if !ok {
		return nil, fmt.Errorf("spawn: expected function call expression")
	}

	// Handle method calls: spawn obj.method(args)
	if fa, ok2 := callNode.Func.(*ast.FieldAccess); ok2 {
		return cg.genSpawnMethodExpr(block, callNode, fa)
	}

	var (
		calleeName string
		scopeKey   string
	)

	switch fn := callNode.Func.(type) {
	case *ast.Identifier:
		calleeName = fn.Name
		scopeKey = fn.Name
	case *ast.ScopeAccess:
		// e.g. io::async_write -> bareName="async_write", scopeKey="io.async_write"
		calleeName = fn.Path[len(fn.Path)-1]
		scopeKey = strings.Join(fn.Path, ".")
	}

	if calleeName == "" {
		// Callee is not a simple name (e.g. fns[0], obj.field, a closure variable).
		// Evaluate it and check if it's an async fat-fn-ptr.
		fatVal, evalErr := cg.genExpr(block, callNode.Func)
		if evalErr != nil {
			return nil, fmt.Errorf("spawn: cannot determine callee name; only named function calls are supported")
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if fatVal != nil && isAsyncFatFnPtr(fatVal.Type()) {
			// Try to recover the Tin FuncType for proper Future[T] wrapping.
			// For fns[i](args) where fns: [fn{#async}(T) R], look up fns's tinType.
			var tinFnType ast.TypeExpr

			if ie, ok2 := callNode.Func.(*ast.IndexExpr); ok2 {
				if id, ok3 := ie.Expr.(*ast.Identifier); ok3 {
					if se2, ok4 := cg.curScope.lookup(id.Name); ok4 {
						if at, ok5 := se2.tinType.(*ast.ArrayType); ok5 {
							tinFnType = at.Elem
						}
					}
				}
			}

			return cg.genSpawnAsyncFatPtr(block, fatVal, callNode.Args, tinFnType)
		}

		return nil, fmt.Errorf("spawn: cannot determine callee name; only named function calls are supported")
	}

	// Evaluate arguments first so we can do overload resolution if needed.
	var callArgs []value.Value

	for _, arg := range callNode.Args {
		val, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, err
		}

		callArgs = append(callArgs, val)

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	// Look up the sync function first to derive its IR name (which may differ from calleeName)
	// and to get its return type for wrapPidInFuture.
	// e.g. for bare "async_write" inside io.tin, the scope entry points to "io__async_write".
	var (
		syncIRName    string
		syncFnRetType irtypes.Type
	)

	for _, key := range []string{scopeKey, calleeName} {
		if se2, ok3 := cg.curScope.lookup(key); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				syncIRName = fn2.Name()
				syncFnRetType = fn2.Sig.RetType

				break
			}
		}
	}

	// Look up the $coro variant of the callee.
	// Try bare name, scope-qualified name, and sync IR name (for cross-package).
	var coroFn *ir.Func

	coroKeys := []string{calleeName + "$coro", scopeKey + "$coro"}
	if syncIRName != "" && syncIRName != calleeName && syncIRName != scopeKey {
		coroKeys = append(coroKeys, syncIRName+"$coro")
	}

	for _, coroKey := range coroKeys {
		if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
			if fn2, ok4 := se2.val.(*ir.Func); ok4 {
				coroFn = fn2

				break
			}
		}
	}

	// resolvedCalleeName is the key for funcDecls (always the bare name for return-type lookup).
	resolvedCalleeName := calleeName

	// Try overload resolution if direct lookup failed.
	if coroFn == nil && len(cg.overloads[calleeName]) > 0 {
		best := cg.resolveOverload(cg.overloads[calleeName], callArgs)
		if best != nil {
			// Also capture the sync function's return type for wrapPidInFuture.
			if se3, ok3 := cg.curScope.lookup(best.irName); ok3 {
				if fn3, ok4 := se3.val.(*ir.Func); ok4 && syncFnRetType == nil {
					syncFnRetType = fn3.Sig.RetType
				}
			}

			for _, coroKey := range []string{best.irName + "$coro", calleeName + "$coro"} {
				if se2, ok3 := cg.curScope.lookup(coroKey); ok3 {
					if fn2, ok4 := se2.val.(*ir.Func); ok4 {
						coroFn = fn2
						resolvedCalleeName = best.irName

						break
					}
				}
			}
		}
	}

	// If direct lookup failed, try monomorphizing a generic async template.
	if coroFn == nil {
		if tmpl, isGeneric := cg.genericFuncs[calleeName]; isGeneric && hasTag(tmpl.Tags, "async") {
			typeSubst := cg.inferTypeArgs(tmpl, callArgs)
			instKey := ""

			for i, tp := range tmpl.TypeParams {
				if i > 0 {
					instKey += "__"
				}

				if name, found := typeSubst[tp]; found {
					instKey += name
				} else {
					instKey += tp
				}
			}

			monoName := tmpl.Name + "__" + instKey
			coroName := monoName + "$coro"
			// Monomorphize the concrete variant. genFuncDeclAs will call
			// predeclareCoroVariant + genCoroFuncBody for async functions
			// (because no $coro stub exists for the monomorphized name yet).
			if concreteFn, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst); err2 == nil {
				if syncFnRetType == nil {
					syncFnRetType = concreteFn.Sig.RetType
				}

				resolvedCalleeName = monoName
				// Find the $coro variant in the module (generated as side effect).
				for _, f := range cg.mod.Funcs {
					if f.Name() == coroName {
						coroFn = f
						cg.curScope.set(coroName, &scopeEntry{val: f, isAlloc: false})

						break
					}
				}
			}
		}
	}

	if coroFn == nil {
		// Last resort: check if calleeName is a variable whose type is an async
		// fat-fn-ptr.  This handles `spawn x(args)` where x: fn{#async}(...).
		if se, ok2 := cg.curScope.lookup(calleeName); ok2 && se.isAlloc {
			if pt, ok3 := se.val.Type().(*irtypes.PointerType); ok3 && isAsyncFatFnPtr(pt.ElemType) {
				loaded := block.NewLoad(pt.ElemType, se.val)
				fnPtr := block.NewExtractValue(loaded, 0)
				envPtr := block.NewExtractValue(loaded, 1)
				fatFnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

				// Build args: env first, then actual params.
				spawnArgs := []value.Value{envPtr}

				for i, val := range callArgs {
					// Params[0] is env; i-th tin arg maps to Params[i+1].
					if i+1 < len(fatFnType.Params) {
						spawnArgs = append(spawnArgs, cg.coerce(block, val, fatFnType.Params[i+1]))
					} else {
						spawnArgs = append(spawnArgs, val)
					}
				}

				hdl := block.NewCall(fnPtr, spawnArgs...)
				pid := block.NewCall(cg.activeSpawnFn(), hdl)
				retType := cg.asyncFatPtrRetType(se.tinType)

				return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
			}
		}

		return nil, fmt.Errorf("spawn: function %q does not have an {#async} variant; add {#async} tag", calleeName)
	}

	// Coerce arguments to match coro function params.
	// Note: no ARC retain here - the $coro ramp block retains RC-tracked
	// params before the initial suspend (see genCoroFuncBody).  A caller-side
	// retain would double-count and produce a leak.
	preCoerceCallArgs := append([]value.Value(nil), callArgs...)
	for i, val := range callArgs {
		if i < len(coroFn.Params) {
			callArgs[i] = cg.coerce(block, val, coroFn.Params[i].Type())
		}
	}

	// Call the ramp function: hdl = callee$coro(args...)
	hdl := block.NewCall(coroFn, callArgs...)

	// Spawn the fiber: pid = _tin_fiber_spawn(hdl)
	pid := block.NewCall(cg.activeSpawnFn(), hdl)

	// Release temporary RC-tracked arguments after spawning.  The $coro ramp
	// retains them before the initial suspend, so the caller's own reference
	// (RC=1 from construction) must be dropped after spawn.  Named variable
	// references are skipped via isCopyExpr - they are owned by their
	// declaration scope and must not be released by the call site.
	//
	// Placed AFTER _tin_fiber_spawn so that LLVM's optimizer does not pair
	// the ramp's retain with this release and eliminate both (which would
	// produce a use-after-free in the fiber if the array is freed before the
	// fiber ever reads it).
	for i, astArg := range callNode.Args {
		if i >= len(preCoerceCallArgs) {
			break
		}

		pre := preCoerceCallArgs[i]
		post := callArgs[i]

		if isCopyExpr(astArg) {
			continue
		}

		if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
			cg.emitRelease(block, post)

			continue
		}

		if isRCTrackedType(pre.Type()) {
			cg.emitRelease(block, pre)
		} else if cg.typeNameOf(pre.Type()) != "" {
			// Named struct value: the coro ramp retained its RC-tracked fields via
			// walkRCStructFields.  Release those fields here (without deinit - the
			// fiber still owns the struct and will call deinit at scope exit).
			cg.emitReleaseNoDeinit(block, pre)
		}
	}

	// Wrap pid in Future[t] where t is the original function's return type.
	// Prefer the funcDecl lookup (bare name), fall back to sync function's LLVM return type.
	if _, hasFuncDecl := cg.funcDecls[resolvedCalleeName]; hasFuncDecl {
		return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
	}

	if syncFnRetType != nil {
		return cg.wrapPidInFutureWithLLVMType(block, pid, syncFnRetType)
	}

	return cg.wrapPidInFuture(block, pid, resolvedCalleeName)
}

// asyncFatPtrRetType extracts the actual Tin return type from a declared FuncType
// for use when wrapping a Future after spawning an async fat-fn-ptr.
// Returns nil if the type is unknown or not a FuncType.
func (cg *CodeGen) asyncFatPtrRetType(tinFnType ast.TypeExpr) irtypes.Type {
	if tinFnType == nil {
		return nil
	}

	ft, ok := tinFnType.(*ast.FuncType)
	if !ok || ft.RetType == nil {
		return nil
	}

	llRet, err := cg.tinTypeToLLVM(ft.RetType)
	if err != nil {
		return nil
	}

	return llRet
}

// genSpawnAsyncFatPtr spawns a fiber from an already-evaluated async fat-fn-ptr
// value.  It extracts the $coro fn-ptr and env, calls it with (env, args...)
// to get the coroutine handle, then spawns the fiber and returns Future[T].
// tinFnType is the declared Tin FuncType for the callee (may be nil, falls back to Future[Unit]).
func (cg *CodeGen) genSpawnAsyncFatPtr(block *ir.Block, fatVal value.Value, argNodes []ast.Node, tinFnType ast.TypeExpr) (value.Value, error) {
	fnPtr := block.NewExtractValue(fatVal, 0)
	envPtr := block.NewExtractValue(fatVal, 1)
	fatFnType := fnPtr.Type().(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	// Build arg list: env first, then actual params (type-guided for fn values).
	llArgs := []value.Value{envPtr}

	for i, argNode := range argNodes {
		var targetType irtypes.Type
		// Params[0] is env; i-th tin arg maps to Params[i+1].
		if i+1 < len(fatFnType.Params) {
			targetType = fatFnType.Params[i+1]
		}

		av, err := cg.genArgWithTargetType(block, argNode, targetType)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if targetType != nil {
			av = cg.coerce(block, av, targetType)
		}

		llArgs = append(llArgs, av)
	}

	hdl := block.NewCall(fnPtr, llArgs...)
	pid := block.NewCall(cg.activeSpawnFn(), hdl)

	retType := cg.asyncFatPtrRetType(tinFnType)

	return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
}

// genSpawnMethodExpr handles `spawn obj.method(args)` - spawns a method call as a fiber.
func (cg *CodeGen) genSpawnMethodExpr(block *ir.Block, callNode *ast.CallExpr, fa *ast.FieldAccess) (value.Value, error) {
	objVal, err := cg.genExpr(block, fa.Expr)
	if err != nil {
		return nil, err
	}

	// Trait fat-ptr method: spawn traitObj.method(args)
	if instKey, isTrait := cg.isTraitFatPtr(objVal.Type()); isTrait {
		if !cg.isAsyncTraitMethod(instKey, fa.Field) {
			return nil, fmt.Errorf("spawn: trait method %q is not {#async}", fa.Field)
		}

		coroSlotIdx := cg.asyncCoroSlotIndex(instKey, fa.Field)
		if coroSlotIdx < 0 {
			return nil, fmt.Errorf("spawn: no $coro slot for trait method %q", fa.Field)
		}

		dataPtr := block.NewExtractValue(objVal, 0)
		vtablePtr := block.NewExtractValue(objVal, 1)

		vtableSt := cg.traitVtableStructTypes[instKey]
		fnPtrGep := block.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(coroSlotIdx)))
		coroSlotFnPtrType := vtableSt.Fields[coroSlotIdx].(*irtypes.PointerType)
		coroSlotFnType := coroSlotFnPtrType.ElemType.(*irtypes.FuncType)
		fnPtr := block.NewLoad(coroSlotFnPtrType, fnPtrGep)

		// Evaluate args
		llArgs := []value.Value{dataPtr}

		for _, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			llArgs = append(llArgs, av)
		}

		llArgs = cg.adaptArgs(block, llArgs, coroSlotFnType)

		hdl := block.NewCall(fnPtr, llArgs...)
		pid := block.NewCall(cg.activeSpawnFn(), hdl)

		// Get the actual return type of the async method (not the coro wrapper's i8*).
		// For async-only traits, traitMethodRetType returns nil (no sync slot), so we
		// fall back to looking up the method's return type from the trait declaration.
		retType := cg.traitMethodRetType(instKey, fa.Field)
		if retType == nil {
			retType = cg.traitAsyncMethodRetType(instKey, fa.Field)
		}

		return cg.wrapPidInFutureWithLLVMType(block, pid, retType)
	}

	// Concrete struct method: look up structName_method$coro
	// Handle both value receivers (StructType) and pointer receivers (*StructType).
	structName := cg.typeNameOf(objVal.Type())
	if structName == "" {
		if pt, ok := objVal.Type().(*irtypes.PointerType); ok {
			structName = cg.typeNameOf(pt.ElemType)
		}
	}

	if structName == "" {
		return nil, fmt.Errorf("spawn: cannot determine struct type for method call on %s", objVal.Type())
	}

	// Check if fa.Field is an async fat-fn-ptr struct field (not a method).
	// e.g. struct Handler = { handle fn{#async}(i64) i64 }; spawn h.handle(10)
	if fieldIdx := cg.fieldIndex(structName, fa.Field); fieldIdx >= 0 {
		// Determine the struct LLVM type.
		structLLVM := objVal.Type()
		if pt, ok := structLLVM.(*irtypes.PointerType); ok {
			structLLVM = pt.ElemType
		}

		if st, ok := structLLVM.(*irtypes.StructType); ok && fieldIdx < len(st.Fields) {
			fieldTy := st.Fields[fieldIdx]

			if isAsyncFatFnPtr(fieldTy) {
				// Load the field value (need a pointer to the struct for GEP).
				var fieldVal value.Value

				if _, isPtr := objVal.Type().(*irtypes.PointerType); isPtr {
					gep := block.NewGetElementPtr(structLLVM, objVal,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fieldVal = block.NewLoad(fieldTy, gep)
				} else {
					alloca := block.NewAlloca(structLLVM)
					block.NewStore(objVal, alloca)
					gep := block.NewGetElementPtr(structLLVM, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fieldVal = block.NewLoad(fieldTy, gep)
				}

				// Recover the Tin FuncType from structFieldTinTypes for proper Future[T].
				var tinFnType ast.TypeExpr

				if tinFields, hasTF := cg.structFieldTinTypes[structName]; hasTF {
					fieldNames := cg.structFields[structName]

					for i, fn := range fieldNames {
						if fn == fa.Field && i < len(tinFields) {
							tinFnType = tinFields[i]

							break
						}
					}
				}

				return cg.genSpawnAsyncFatPtr(block, fieldVal, callNode.Args, tinFnType)
			}
		}
	}

	coroName := structName + "_" + fa.Field + "$coro"

	se2, ok3 := cg.curScope.lookup(coroName)
	if !ok3 {
		return nil, fmt.Errorf("spawn: method %s.%s does not have a $coro variant; is it {#async}?", structName, fa.Field)
	}

	coroFn2, ok4 := se2.val.(*ir.Func)
	if !ok4 {
		return nil, fmt.Errorf("spawn: %s is not a function", coroName)
	}

	// Build call args: (obj, args...).
	// If the $coro expects a pointer receiver (*T) but we have a value T,
	// use genLValue to get the address or fall back to a temp alloca.
	thisArg2 := objVal

	if len(coroFn2.Params) > 0 {
		firstParamTy2 := coroFn2.Params[0].Type()
		if pt2, isPtr2 := firstParamTy2.(*irtypes.PointerType); isPtr2 && pt2.ElemType.Equal(objVal.Type()) {
			if lv, err2 := cg.genLValue(block, fa.Expr); err2 == nil {
				thisArg2 = lv
			} else {
				tmp2 := block.NewAlloca(objVal.Type())
				block.NewStore(objVal, tmp2)
				thisArg2 = tmp2
			}
		}
	}

	coroArgs := []value.Value{thisArg2}
	preCoerceArgVals := make([]value.Value, 0, len(callNode.Args))

	for i, arg := range callNode.Args {
		av, err2 := cg.genExpr(block, arg)
		if err2 != nil {
			return nil, err2
		}

		preCoerceArgVals = append(preCoerceArgVals, av)

		if i+1 < len(coroFn2.Params) {
			av = cg.coerce(block, av, coroFn2.Params[i+1].Type())
		}

		coroArgs = append(coroArgs, av)
	}

	hdl2 := block.NewCall(coroFn2, coroArgs...)
	pid2 := block.NewCall(cg.activeSpawnFn(), hdl2)

	// Release temporary RC-tracked args after spawning (same as genSpawnExpr).
	// The receiver (coroArgs[0]) is handled separately below.
	for i, astArg := range callNode.Args {
		if i >= len(preCoerceArgVals) {
			break
		}

		pre := preCoerceArgVals[i]
		post := coroArgs[i+1] // coroArgs[0] is thisArg; user args start at 1

		if isCopyExpr(astArg) {
			continue
		}

		if isAnyType(post.Type()) && !isAnyType(pre.Type()) {
			cg.emitRelease(block, post)

			continue
		}

		if isRCTrackedType(pre.Type()) {
			cg.emitRelease(block, pre)
		} else if cg.typeNameOf(pre.Type()) != "" {
			cg.emitReleaseNoDeinit(block, pre)
		}
	}
	// Release temporary receiver if it is not a named variable.
	if !isCopyExpr(fa.Expr) {
		if isRCTrackedType(objVal.Type()) {
			cg.emitRelease(block, objVal)
		} else if cg.typeNameOf(objVal.Type()) != "" {
			cg.emitReleaseNoDeinit(block, objVal)
		}
	}

	// Use the original method name for return type lookup.
	fnName := structName + "_" + fa.Field

	return cg.wrapPidInFuture(block, pid2, fnName)
}

// wrapPidInFutureWithLLVMType wraps a fiber PID in Future[T] using the LLVM type directly.
// Used when we have the concrete LLVM return type but no funcDecl entry (e.g., trait method spawns).
func (cg *CodeGen) wrapPidInFutureWithLLVMType(block *ir.Block, pid value.Value, retType irtypes.Type) (value.Value, error) {
	var retTypeStr string
	if retType == nil || retType.Equal(irtypes.Void) {
		// Resolve the canonical name of the Unit struct.  After the canonical
		// naming change, the Unit LLVM struct may be registered as "sync__Unit".
		retTypeStr = cg.canonicalUnitStructName()
	} else {
		retTypeStr = llvmTypeName(retType)
	}

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if _, exists := cg.structTypes[futureConcreteName]; !exists {
		retTypeExpr := &ast.SimpleType{Name: retTypeStr}

		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			// Try Unit as fallback (use canonical name)
			futureConcreteName = "Future__" + cg.canonicalUnitStructName()
		}
	}

	makeFnName := futureConcreteName + "_make"

	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: sync package failed to load: %w", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - sync package could not be loaded", retTypeStr)
	}

	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// genSpawnDoBlock synthesizes an anonymous {#async} function from a `spawn do:` body block,
// predeclares and generates its $coro variant, then spawns it as a fiber.
func (cg *CodeGen) genSpawnDoBlock(block *ir.Block, doBlock *ast.Block) (value.Value, error) {
	// Generate a unique name for the anonymous async function.
	anonName := fmt.Sprintf("__spawn_do_%d", cg.spawnDoCounter)
	cg.spawnDoCounter++

	// Collect free variables referenced in the do-block body that come from the
	// enclosing function's scope.  These need to be captured by value into an env
	// struct so that the synthesized $coro function can access them safely.
	freeNames := collectFreeVars(doBlock, map[string]bool{})

	var captures []closureCapture

	for _, n := range freeNames {
		entry, ok := cg.curScope.lookup(n)
		if !ok {
			continue
		}

		if _, isFunc := entry.val.(*ir.Func); isFunc {
			continue // global function - reachable by name, no capture needed
		}

		if entry.isGlobal {
			continue // module-level global - reachable directly
		}

		if !entry.isAlloc {
			continue // not an alloca - skip
		}

		pt, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			continue
		}

		ty := pt.ElemType
		val := block.NewLoad(ty, entry.val)
		captures = append(captures, closureCapture{name: n, val: val, llvmTy: ty})
	}

	// ARC: retain every RC-tracked capture before packing it into the env struct.
	// The coroutine runs asynchronously, after the parent scope's locals are
	// released.  Without this extra retain the captured strings could be freed
	// while the env still holds a reference to them.
	// The matching release happens in genCoroFuncBody after unpackEnv.
	for _, c := range captures {
		if isRCTrackedType(c.llvmTy) {
			cg.emitRetain(block, c.val)
		}
	}

	// Pack captured values into a heap-allocated env struct.  buildEnv returns a
	// null i8* and nil struct type when there are no captures.
	envI8Ptr, envStructType := cg.buildEnv(block, captures)

	// Synthesize an ast.FuncDecl with no params, void return, and {#async} tag.
	synth := &ast.FuncDecl{
		Name:   anonName,
		Params: nil,
		Tags:   []string{"async"},
		Body:   doBlock,
	}

	// Mark as coro-callable and predeclare the $coro variant (with env pointer
	// as the first parameter so unpackEnv can find it at coroFn.Params[0]).
	cg.coroCallable[anonName] = true
	if err := cg.predeclareCoroVariant(synth, anonName, true); err != nil {
		return nil, fmt.Errorf("spawn do: predeclare failed: %w", err)
	}

	// Generate the $coro body, passing the captures so unpackEnv can restore them.
	if err := cg.genCoroFuncBody(synth, coroVersionName(anonName), captures, envStructType); err != nil {
		return nil, fmt.Errorf("spawn do: coro body generation failed: %w", err)
	}

	// Look up the generated $coro function.
	coroName := coroVersionName(anonName)

	se, ok := cg.curScope.lookup(coroName)
	if !ok || se == nil {
		return nil, fmt.Errorf("spawn do: %s$coro not found after generation", anonName)
	}

	coroFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn do: %s$coro is not a function", anonName)
	}

	// Call the ramp function with the env pointer and spawn the fiber.
	hdl := block.NewCall(coroFn, envI8Ptr)
	pid := block.NewCall(cg.activeSpawnFn(), hdl)

	// Void do-block spawn: wrap in Future[Unit]

	return cg.wrapPidInFuture(block, pid, "")
}

// LValue generation

// genLValue returns a pointer to the storage location of an lvalue.
func (cg *CodeGen) genLValue(block *ir.Block, node ast.Node) (value.Value, error) {
	switch e := node.(type) {
	case *ast.Identifier:
		entry, ok := cg.curScope.lookup(e.Name)
		if !ok {
			return nil, cg.nodeErr(e, "undefined identifier: %s", e.Name)
		}

		if entry.isAlloc {
			return entry.val, nil
		}
		// Not an alloca - wrap in alloca.
		alloca := block.NewAlloca(entry.val.Type())
		block.NewStore(entry.val, alloca)

		return alloca, nil

	case *ast.IndexExpr:
		idx, err := cg.genExpr(block, e.Index)
		if err != nil {
			return nil, err
		}

		idx = cg.coerce(block, idx, irtypes.I64)

		// For addressable array lvalues: GEP directly through the stored pointer
		// without loading the array value first (avoids spurious full-array copies).
		//
		//   Fixed-size [N x T]: GEP(alloca, 0, idx)
		//   Fat array  {T*, i64}: load data-ptr field, then GEP(data_ptr, idx)
		//
		// Both paths require the expr to be an addressable lvalue (alloca or prior GEP).
		if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
			if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
				if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 {
					// Fixed-size array.
					return block.NewGetElementPtr(at, arrPtr,
						constant.NewInt(irtypes.I32, 0), idx), nil
				}

				if st, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && len(st.Fields) == 2 {
					// Fat array: load the data pointer (field 0) and GEP into it.
					ptrGep := block.NewGetElementPtr(st, arrPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
					elemPtrType := st.Fields[0]

					dataPtr := block.NewLoad(elemPtrType, ptrGep)

					if ept, ok3 := elemPtrType.(*irtypes.PointerType); ok3 {
						return block.NewGetElementPtr(ept.ElemType, dataPtr, idx), nil
					}
				}
			}
		}

		// Fat arrays and other types: load the value first, then index.
		arr, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}

		arrType := arr.Type()
		switch at := arrType.(type) {
		case *irtypes.StructType:
			if len(at.Fields) == 2 {
				// Fat pointer: {T*, i64} - extract data pointer directly without alloca.
				elemPtrType := at.Fields[0]

				dataPtr := block.NewExtractValue(arr, 0)
				if pt, ok := elemPtrType.(*irtypes.PointerType); ok {
					return block.NewGetElementPtr(pt.ElemType, dataPtr, idx), nil
				}
			}
		case *irtypes.ArrayType:
			alloca := block.NewAlloca(arrType)
			block.NewStore(arr, alloca)

			return block.NewGetElementPtr(arrType, alloca,
				constant.NewInt(irtypes.I32, 0), idx), nil
		case *irtypes.PointerType:
			return block.NewGetElementPtr(at.ElemType, arr, idx), nil
		}

		return nil, fmt.Errorf("cannot index type %s", arrType)

	case *ast.FieldAccess:
		// Use genLValue recursively so we obtain a pointer into the *original*
		// storage (alloca, heap, etc.) rather than a copy.  Writing through the
		// returned GEP pointer then actually mutates the variable.
		objPtr, err := cg.genLValue(block, e.Expr)
		if err != nil {
			// genLValue failed for the sub-expression (e.g. a non-lvalue like a
			// function call return value).  Fall back to a temporary alloca; this
			// means field-writes on temporaries are discarded, but that is the
			// pre-existing behavior for such expressions.
			obj, err2 := cg.genExpr(block, e.Expr)
			if err2 != nil {
				return nil, err2
			}

			objType := obj.Type()
			if e.IsPtr {
				if pt, ok := objType.(*irtypes.PointerType); ok {
					structName := cg.typeNameOf(pt.ElemType)

					gep := cg.emitFieldGEP(block, obj, structName, e.Field)
					if gep == nil {
						return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
					}

					return gep, nil
				}
			}

			alloca := block.NewAlloca(objType)
			block.NewStore(obj, alloca)

			structName := cg.typeNameOf(objType)

			gep := cg.emitFieldGEP(block, alloca, structName, e.Field)
			if gep == nil {
				return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
			}

			return gep, nil
		}
		// objPtr is a pointer to the containing struct (or pointer-to-struct for IsPtr).
		objPtrType, ok := objPtr.Type().(*irtypes.PointerType)
		if !ok {
			return nil, fmt.Errorf("genLValue: expected pointer for field access")
		}

		objType := objPtrType.ElemType
		if e.IsPtr {
			// e.Expr is a variable holding a *struct - dereference once.
			structPtrVal := block.NewLoad(objType, objPtr)
			if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
				structName := cg.typeNameOf(pt.ElemType)

				gep := cg.emitFieldGEP(block, structPtrVal, structName, e.Field)
				if gep == nil {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}

				return gep, nil
			}
		}
		// Auto-deref: when the alloca holds a *struct (pointer receiver pattern),
		// dereference once so that `this.field` works the same as `this->field`.
		if pt, ok2 := objType.(*irtypes.PointerType); ok2 {
			if cg.typeNameOf(pt.ElemType) != "" {
				structPtrVal := block.NewLoad(objType, objPtr)
				structName := cg.typeNameOf(pt.ElemType)

				gep := cg.emitFieldGEP(block, structPtrVal, structName, e.Field)
				if gep == nil {
					return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
				}

				return gep, nil
			}
		}

		structName := cg.typeNameOf(objType)

		gep := cg.emitFieldGEP(block, objPtr, structName, e.Field)
		if gep == nil {
			return nil, fmt.Errorf("unknown field %s.%s", structName, e.Field)
		}

		return gep, nil

	case *ast.DerefExpr:
		val, err := cg.genExpr(block, e.Expr)
		if err != nil {
			return nil, err
		}

		if irtypes.IsPointer(val.Type()) {
			return val, nil
		}

		return nil, fmt.Errorf("cannot deref non-pointer")

	case *ast.StructLit:
		// &StructLit{...} - heap-allocate the struct and return a typed pointer.
		// The struct value is constructed normally (with init, field stores, and
		// ARC retains on RC-tracked fields), then stored into malloc'd memory.
		// The caller owns the raw memory; they must release RC fields and call
		// mem::free before the pointer goes out of scope.
		val, err := cg.genStructLit(block, e)
		if err != nil {
			return nil, err
		}

		st, ok2 := val.Type().(*irtypes.StructType)
		if !ok2 {
			return nil, fmt.Errorf("&struct{} requires a struct literal")
		}
		// sizeof(T) via GEP trick on null pointer.
		nullPtr := constant.NewNull(irtypes.NewPointer(st))
		gepOne := block.NewGetElementPtr(st, nullPtr, constant.NewInt(irtypes.I32, 1))
		sz := block.NewPtrToInt(gepOne, irtypes.I64)
		// Use _tin_rc_alloc so the block is ARC-managed: scope exit can call
		// _tin_release to free it without a manual mem::free.
		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		typedPtr := block.NewBitCast(heapI8, irtypes.NewPointer(st))
		block.NewStore(val, typedPtr)

		return typedPtr, nil
	}

	return nil, fmt.Errorf("not an lvalue: %T", node)
}

// callGenericFromMap looks up bareName in m (either genericFuncs or
// constrainedFuncs), evaluates args, infers type arguments, monomorphizes
// the template, and emits the call.  Returns (result, updatedBlock, found,
// error).  found is false when bareName is not in m.
func (cg *CodeGen) callGenericFromMap(
	block *ir.Block,
	args []ast.Node,
	bareName string,
	m map[string]*ast.FuncDecl,
) (value.Value, *ir.Block, bool, error) {
	tmpl, ok := m[bareName]
	if !ok {
		return nil, block, false, nil
	}

	argVals := make([]value.Value, 0, len(args))
	for _, arg := range args {
		av, err := cg.genExpr(block, arg)
		if err != nil {
			return nil, block, true, err
		}

		argVals = append(argVals, av)

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	typeSubst := cg.inferTypeArgs(tmpl, argVals)
	instKey := ""

	for i, tp := range tmpl.TypeParams {
		if i > 0 {
			instKey += "__"
		}

		if name, found := typeSubst[tp]; found {
			instKey += name
		} else {
			instKey += tp
		}
	}

	// When bareName is a qualified key (e.g. "yaml__encode"), use it as the
	// template name so the monomorphized IR name includes the package prefix
	// (e.g. "yaml__encode__point"). Without this, identically-named generics
	// from different packages (json::encode and yaml::encode both have bare
	// name "encode") would produce the same IR name and the cache would return
	// the first-compiled version for every subsequent package's call.
	monoTmpl := tmpl
	if bareName != tmpl.Name {
		copy := *tmpl
		copy.Name = bareName
		monoTmpl = &copy
	}

	concreteFunc, err := cg.monomorphizeFunc(monoTmpl, instKey, typeSubst)
	if err != nil {
		return nil, block, true, err
	}

	argValsPreCoerce := append([]value.Value(nil), argVals...)
	argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)

	result := block.NewCall(concreteFunc, argVals...)

	// ARC: release temporary RC-tracked arguments (same logic as the general
	// call path).  Without this, temporaries like join() results or concat
	// results passed directly as arguments leak.
	for i, astArg := range args {
		if i >= len(argValsPreCoerce) {
			break
		}

		preCoerce := argValsPreCoerce[i]
		postCoerce := argVals[i]

		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}

		if !isRCTrackedType(preCoerce.Type()) {
			continue
		}

		if isCopyExpr(astArg) {
			continue
		}

		cg.emitRelease(block, preCoerce)
	}

	return result, block, true, nil
}
