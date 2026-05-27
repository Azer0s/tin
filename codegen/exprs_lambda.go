package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
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

	// Record source file + display name so pclntab attributes lambda
	// frames to the user's source file with a sensible synthetic name
	// (e.g. `<lambda>@file:line:col` instead of `lambda.0+0x21`).
	if cg.filename != "" {
		if cg.fnSourceFiles == nil {
			cg.fnSourceFiles = map[string]string{}
		}

		cg.fnSourceFiles[f.Name()] = cg.filename
	}

	if cg.fnDisplayNames == nil {
		cg.fnDisplayNames = map[string]string{}
	}

	cg.fnDisplayNames[f.Name()] = "<lambda>"

	prevCtx := cg.pushClosureCtx(f)
	// Scope the cLayout escape walker's AST lookups to this lambda's
	// body, not the enclosing fn's -- otherwise a let-decl inside the
	// lambda would walk the outer body and mismatch on shadowed names.
	cg.curFnAstBody = e.Body
	// Recursive lambda self-reference: when genVarDecl plumbed a
	// `lambdaSelfName`, register a fat-fn-ptr value built from this
	// lambda's own IR func + its env arg under that name in the new
	// scope.  Recursive calls inside the body resolve to this fat
	// value and dispatch through callFatFn -> slot 0 -> the IR func
	// with the proper env.  We clear cg.lambdaSelfName up front so
	// nested lambdas don't accidentally inherit the outer binding.
	selfName := cg.lambdaSelfName
	cg.lambdaSelfName = ""

	if selfName != "" && e.RetType != nil {
		// Build the fat-fn-ptr value: slots 0/1 = this fn (we don't
		// emit a separate $colored variant for the self-reference; the
		// colored-variant emission below copies the same self-ref so
		// recursive calls inside a colored body also route through it).
		// Slot 2 = synth coro wrapper via ensureCoroWrapperFor.
		// Slot 3 = the lambda's own env arg (p0).
		envForSelf := f.Params[0]
		fatVal := cg.buildFatFnPtrValue(entry, f, envForSelf)
		fatSlot := entry.NewAlloca(fatVal.Type())
		entry.NewStore(fatVal, fatSlot)
		cg.curScope.set(selfName, &scopeEntry{val: fatSlot, isAlloc: true, noDeinit: true, noRelease: true})
	}

	// -Wparam-mutation: same check as the named-fn path in funcs.go;
	// surface lambda bodies that write to their own params (silent
	// no-op for the caller after autocopy).  Pointer-typed params are
	// exempt (intent is explicit).  Default-off via -Wpedantic; the
	// diagnostic's own default-off level handles the gating.
	mutated := cg.collectMutatedTargets(e.Body)
	for _, p := range e.Params {
		if p.Name == "" {
			continue
		}

		if _, isPtr := p.Type.(*ast.PointerType); isPtr {
			continue
		}

		if mutated[p.Name] {
			cg.warn(DiagParamMutation, e.Pos(),
				"parameter `%s` of lambda is mutated inside the body but caller's binding is untouched (autocopy); switch to a `*T` receiver if the caller should see the write",
				p.Name)
		}
	}

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
				wrapperSt := cg.structTypeFor(CanonKey(stTe.Name))
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

				// Mark the wrapper as borrowed: c_data_ptr is the C-side
				// pointer the caller handed us; we do not own its rc-block.
				flagsIdx := int64(cg.clayoutFlagsIndex(stTe.Name))
				flagsGep := entry.NewGetElementPtr(wrapperSt, wrapperAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, flagsIdx))
				entry.NewStore(constant.NewInt(irtypes.I32, cLayoutFlagBorrowed), flagsGep)

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

	// Step 4.5: emit a $colored variant of the lambda body so slot 1
	// of the fat-fn-ptr can route cooperative-context invocations to
	// a yield-instrumented body.  Same signature as the sync lambda,
	// same body; curFnAutoYield + curFnColoredSync flip the yield
	// lowering to runtime-driven via _tin_fiber_yield_coro.
	coloredName := coloredVersionName(name)

	coloredParams := make([]*ir.Param, len(llParams))
	for i, p := range llParams {
		coloredParams[i] = ir.NewParam(p.Name(), p.Type())
	}

	coloredFn := cg.mod.NewFunc(coloredName, retType, coloredParams...)
	cg.curScope.set(coloredName, &scopeEntry{val: coloredFn, isAlloc: false})

	if cg.filename != "" {
		cg.fnSourceFiles[coloredFn.Name()] = cg.filename
	}

	cg.fnDisplayNames[coloredFn.Name()] = "<lambda$colored>"

	coloredEntry := coloredFn.NewBlock("entry")

	prevCtxC := cg.pushClosureCtx(coloredFn)
	cg.curFnAstBody = e.Body
	prevAutoYield := cg.curFnAutoYield
	prevColoredSync := cg.curFnColoredSync
	cg.curFnAutoYield = true
	cg.curFnColoredSync = true
	// Recursive lambda self-reference (colored variant): mirror the
	// sync variant's self-ref registration so recursive calls inside
	// the colored body also resolve through a fat-fn-ptr.  Slot 1 of
	// that fat-fn-ptr is coloredFn itself, so cooperative-context
	// recursion stays cooperative.
	if selfName != "" && e.RetType != nil {
		envForSelf := coloredFn.Params[0]
		fatVal := cg.buildFatFnPtrValue(coloredEntry, coloredFn, envForSelf)
		fatSlot := coloredEntry.NewAlloca(fatVal.Type())
		coloredEntry.NewStore(fatVal, fatSlot)
		cg.curScope.set(selfName, &scopeEntry{val: fatSlot, isAlloc: true, noDeinit: true, noRelease: true})
	}

	cg.unpackClosureEnv(coloredEntry, coloredFn, envStructType, captures)

	for i, p := range e.Params {
		param := coloredFn.Params[i+1]

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			cg.curFnAutoYield = prevAutoYield
			cg.curFnColoredSync = prevColoredSync
			cg.popClosureCtx(prevCtxC)

			return nil, err
		}

		alloca := coloredEntry.NewAlloca(pt)
		coloredEntry.NewStore(param, alloca)
		cg.emitRetain(coloredEntry, param)
		cg.curScope.set(p.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRCTrackedType(pt)})
	}

	prevMatchSubjectC := cg.matchSubject

	if _, isWhere := e.Body.(*ast.WhereList); isWhere && len(e.Params) > 0 {
		firstParamName := e.Params[0].Name
		if se, ok := cg.curScope.lookup(firstParamName); ok && se.isAlloc {
			pt := se.val.Type().(*irtypes.PointerType)
			cg.matchSubject = coloredEntry.NewLoad(pt.ElemType, se.val)
		}
	}

	termC, errC := cg.genBody(coloredEntry, e.Body, retType)
	cg.matchSubject = prevMatchSubjectC
	cg.curFnAutoYield = prevAutoYield
	cg.curFnColoredSync = prevColoredSync
	cg.popClosureCtx(prevCtxC)

	if errC != nil {
		return nil, errC
	}

	if !termC {
		lastBlock := coloredFn.Blocks[len(coloredFn.Blocks)-1]
		if lastBlock.Term == nil {
			if irtypes.IsVoid(retType) {
				lastBlock.NewRet(nil)
			} else {
				lastBlock.NewRet(cg.zeroValue(retType))
			}
		}
	}

	// Step 4.75: when the lambda carries the `#async` tag, emit a real
	// $coro body alongside the sync + colored variants.  Slot 2 of the
	// fat-fn-ptr then targets this body directly via
	// lookupRealCoroVariant instead of going through the synth coro
	// wrapper (which costs an extra frame allocation per spawn and
	// internally targets slot 1's $colored body).  Matches the slot-2
	// shape declared `fn{#async}` named fns have.
	if hasTag(e.Tags, "async") {
		// Register the sync `name -> f` mapping in the outer
		// scope so genCoroFuncBody's recursive-self-ref builder
		// can find the sync IR func to wrap into a fat-fn-ptr.
		// Removed at the end of this block so the let binding
		// (which holds the FAT-FN-PTR value, not the IR func)
		// installs cleanly via genVarDecl.
		hadPriorSelf := false

		var priorSelf *scopeEntry

		if selfName != "" {
			if prev, ok := cg.curScope.vars[name]; ok {
				hadPriorSelf = true
				priorSelf = prev
			}

			cg.curScope.set(name, &scopeEntry{val: f, isAlloc: false})
		}

		synth := &ast.FuncDecl{
			Name:    name,
			Params:  e.Params,
			RetType: e.RetType,
			Tags:    []string{"async"},
			Body:    e.Body,
		}
		cg.coroCallable[name] = true

		if err := cg.predeclareCoroVariant(synth, name, true); err != nil {
			return nil, fmt.Errorf("async lambda: predeclare coro: %w", err)
		}

		// Plumb the self-name through to genCoroFuncBody so it
		// can register a fat-fn-ptr self-ref in the coro body's
		// scope (recursive `count(n-1)` inside an #async lambda).
		if selfName != "" {
			cg.lambdaSelfName = selfName
		}

		if err := cg.genCoroFuncBody(synth, coroVersionName(name), captures, envStructType); err != nil {
			return nil, fmt.Errorf("async lambda: gen coro body: %w", err)
		}

		// Restore the outer scope: the let binding will overwrite
		// `name` with the fat-fn-ptr value when genVarDecl
		// finishes, but for nested async-lambda emission we must
		// not leave a stale entry around.
		if selfName != "" {
			if hadPriorSelf {
				cg.curScope.set(name, priorSelf)
			} else {
				delete(cg.curScope.vars, name)
			}
		}
	}

	// Step 5: build and return the 4-slot fat-fn-ptr value.
	// buildFatFnPtrValue's lookupColoredVariant picks up the colored
	// variant we just emitted and wires it into slot 1.  When this
	// lambda emitted a real $coro body (step 4.75), buildFatFnPtrValue's
	// lookupRealCoroVariant picks that up for slot 2 in place of the
	// synth coro wrapper.
	fatVal := cg.buildFatFnPtrValue(block, f, envI8Ptr)

	// Signal to genVarDecl whether this closure has captured variables so it can
	// skip the _tin_release_closure(null) call for non-capturing closures.
	cg.lastLambdaHadCaptures = len(captures) > 0

	return fatVal, nil
}

// buildFatFnPtrValue assembles a 4-slot fat-fn-ptr value from a sync
// fn ptr and an env pointer.  Slot 0 = sync body (canonical).
// Slot 1 = $colored variant when one was emitted, else slot 0 (the
// caller is non-cooperative; no cooperation is needed anyway).
// Slot 2 = a per-fn coro wrapper lazily emitted via
// ensureCoroWrapperFor; the wrapper internally targets slot 1's body
// so a spawned sync fn cooperates at the same coloring points as a
// bare cooperative-context call.  Slot 3 = env.
