package codegen

// globals.go - top-level variable (var) and fiber-init codegen.

import (
	"math/big"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// preregisterPkgTopLevelVar is the package-aware variant. It mirrors
// preregisterTopLevelVar but uses an IR name of `pkg__name` and binds the
// scope entry under the bare `name` so the package's own functions can
// reference it. When `name` is in exportedNames, the parent scope also
// gets `pkg::name` / `pkg.name` aliases.
func (cg *CodeGen) preregisterPkgTopLevelVar(tv *ast.TopLevelVar, pkgName string, exportedNames map[string]bool, parentScope *scope) error {
	lt, err := cg.tinTypeToLLVM(tv.Type)
	if err != nil {
		return err
	}

	var initVal constant.Constant
	if tv.Value != nil {
		initVal = cg.tryConstantFold(tv.Value, lt)
	}

	if initVal == nil {
		initVal = cg.zeroConstant(lt)
	}

	irName := pkgName + "__" + tv.Name

	g := cg.activeModule().NewGlobal(irName, lt)
	g.Init = initVal

	entry := &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(lt), isGlobal: true}
	cg.curScope.set(tv.Name, entry)

	if cg.topLevelVarBareNames == nil {
		cg.topLevelVarBareNames = map[string]bool{}
	}

	cg.topLevelVarBareNames[tv.Name] = true

	if exportedNames[tv.Name] && parentScope != nil {
		parentScope.set(pkgName+"::"+tv.Name, entry)
		parentScope.set(pkgName+"."+tv.Name, entry)

		if cg.moduleScope != nil && cg.moduleScope != parentScope {
			cg.moduleScope.set(pkgName+"::"+tv.Name, entry)
			cg.moduleScope.set(pkgName+"."+tv.Name, entry)
		}
	}

	cg.allTopLevelVars = append(cg.allTopLevelVars, topLevelVarInit{
		name:    irName,
		global:  g,
		pkgName: pkgName,
	})

	if tv.Value != nil && cg.tryConstantFold(tv.Value, lt) == nil {
		cg.topLevelVarInits = append(cg.topLevelVarInits, topLevelVarInit{
			name:     irName,
			global:   g,
			initExpr: tv.Value,
			pkgName:  pkgName,
		})
	}

	return nil
}

// preregisterTopLevelVar declares a top-level `var name Type [= expr]` as an
// LLVM global with a zeroinitializer. If the initializer is a runtime
// expression (non-constant), it is deferred to topLevelVarInits so that it can
// be emitted at the top of main().
func (cg *CodeGen) preregisterTopLevelVar(tv *ast.TopLevelVar) error {
	lt, err := cg.tinTypeToLLVM(tv.Type)
	if err != nil {
		return err
	}

	// In REPL mode, globals from previous cells must be external references so
	// RTLD_GLOBAL resolves them to the canonical first-loaded copy, not a new zero copy.
	if cg.replMode && cg.replExternalGlobals[tv.Name] {
		g := cg.activeModule().NewGlobal(tv.Name, lt)
		g.Linkage = enum.LinkageExternal
		cg.curScope.set(tv.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(lt), isGlobal: true})

		return nil
	}

	// Determine whether the initializer is a compile-time constant.
	var initVal constant.Constant
	if tv.Value != nil {
		initVal = cg.tryConstantFold(tv.Value, lt)
	}

	if initVal == nil {
		initVal = cg.zeroConstant(lt)
	}

	g := cg.activeModule().NewGlobal(tv.Name, lt)
	g.Init = initVal

	// Register in global scope as a pointer (alloc-style) so that loads/stores work.
	// isGlobal=true prevents per-function scope release from deiniting the global.
	cg.curScope.set(tv.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(lt), isGlobal: true})

	if cg.topLevelVarBareNames == nil {
		cg.topLevelVarBareNames = map[string]bool{}
	}

	cg.topLevelVarBareNames[tv.Name] = true

	// Track every top-level var for deinit-at-exit (regardless of init type).
	// pkgName is cg.currentPkg (empty for entry program top-level vars).
	cg.allTopLevelVars = append(cg.allTopLevelVars, topLevelVarInit{
		name:    tv.Name,
		global:  g,
		pkgName: cg.currentPkg,
	})

	// If the initializer needs runtime evaluation, queue it.
	if tv.Value != nil && cg.tryConstantFold(tv.Value, lt) == nil {
		cg.topLevelVarInits = append(cg.topLevelVarInits, topLevelVarInit{
			name:     tv.Name,
			global:   g,
			pkgName:  cg.currentPkg,
			initExpr: tv.Value,
		})
	}

	return nil
}

// emitTopLevelVarDeinits releases/deinits all top-level globals in reverse
// declaration order. Call this in the main wrapper just before program exit.
// For primitive types (i64, bool, etc.) emitRelease is a no-op, so it is safe
// to call unconditionally on every global.
func (cg *CodeGen) emitTopLevelVarDeinits(block *ir.Block) {
	for i := len(cg.allTopLevelVars) - 1; i >= 0; i-- {
		vi := cg.allTopLevelVars[i]
		lt := vi.global.ContentType
		loaded := block.NewLoad(lt, vi.global)
		cg.emitRelease(block, loaded)
	}
}

// emitDeinitAllFn lazily synthesizes `void _tin_deinit_all(void)` in
// cg.mod containing the same RC release sequence as emitTopLevelVarDeinits.
// Returns nil when there are no top-level vars to release (no deinit needed).
//
// Step 3 of incremental compilation: extracting the deinit sequence into
// its own fn is the prerequisite for `atexit(_tin_deinit_all)` registration
// in the C wrapper, which makes deinits run on EVERY clean exit path
// (return-from-main, libc `exit(N)`, panic-with-recover) rather than
// only the fall-through-from-main path the inline emit covers today.
//
// The dispatcher LIVES in cg.mod (not a per-pkg module) because it's a
// whole-program artifact: it iterates every top-level var across every
// imported pkg in reverse declaration order. Per-pkg `_tin_deinit_<pkg>`
// fns + topo dispatcher come in a follow-up commit.
func (cg *CodeGen) emitDeinitAllFn() *ir.Func {
	if len(cg.allTopLevelVars) == 0 {
		return nil
	}

	if cg.deinitAllFn != nil {
		return cg.deinitAllFn
	}

	fn := cg.mod.NewFunc("_tin_deinit_all", irtypes.Void)
	fn.Linkage = enum.LinkageInternal
	entry := fn.NewBlock("entry")
	cg.emitTopLevelVarDeinits(entry)
	entry.NewRet(nil)

	cg.deinitAllFn = fn

	return fn
}

// emitDeinitAllAtexit registers _tin_deinit_all via libc atexit() so the
// deinit sequence fires on every clean process exit - including
// `exit(N)` from anywhere in the program. Without this hook, deinits
// only run when user main falls through to the wrapper's tail; an
// `os::exit(1)` from inside a fiber bypasses the entire teardown.
//
// Returns the new "current block" the caller should continue emitting
// into (the post-cmp continuation). The arming check is a single
// load + compare-and-branch; if armed already, skip the atexit call
// (idempotency guard against double-registration when a #interop-mode
// build calls tin_runtime_init from C twice).
//
// Caller emits this BEFORE user main runs in the C wrapper.
func (cg *CodeGen) emitDeinitAllAtexit(block *ir.Block) *ir.Block {
	deinitFn := cg.emitDeinitAllFn()
	if deinitFn == nil {
		return block
	}

	if cg.atexitFn == nil {
		atexitFnTy := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
		cg.atexitFn = cg.ensureExternDecl("atexit", irtypes.I32,
			[]*ir.Param{ir.NewParam("fn", atexitFnTy)}, false)
	}

	if cg.deinitArmedGlobal == nil {
		cg.deinitArmedGlobal = cg.mod.NewGlobalDef("_tin_deinit_armed",
			constant.NewInt(irtypes.I32, 0))
		cg.deinitArmedGlobal.Linkage = enum.LinkageInternal
	}

	armed := block.NewLoad(irtypes.I32, cg.deinitArmedGlobal)
	notArmed := block.NewICmp(enum.IPredEQ, armed, constant.NewInt(irtypes.I32, 0))

	armBlk := cg.curFn.NewBlock("deinit.arm")
	contBlk := cg.curFn.NewBlock("deinit.cont")
	block.NewCondBr(notArmed, armBlk, contBlk)

	armBlk.NewStore(constant.NewInt(irtypes.I32, 1), cg.deinitArmedGlobal)
	armBlk.NewCall(cg.atexitFn, deinitFn)
	armBlk.NewBr(contBlk)

	return contBlk
}

// tryConstantFold attempts to evaluate an AST node as a compile-time constant.
// Returns nil if the expression cannot be folded at compile time.
//
// First tries the literal-only fast path (covers the common case at zero AST-
// walker cost), then falls back to the full AST evaluator for anything more
// complex (BinExpr / UnaryExpr / pure-fn calls / identifiers bound to consts).
func (cg *CodeGen) tryConstantFold(n ast.Node, targetType irtypes.Type) constant.Constant {
	switch v := n.(type) {
	case *ast.IntLit:
		var c constant.Constant
		if v.Big != nil {
			c = &constant.Int{Typ: irtypes.I128, X: new(big.Int).Set(v.Big)}
		} else {
			c = constant.NewInt(irtypes.I64, v.Value)
		}

		return cg.constCoerce(c, targetType).(constant.Constant)
	case *ast.FloatLit:
		c := constant.NewFloat(irtypes.Double, v.Value)

		return cg.constCoerce(c, targetType).(constant.Constant)
	case *ast.BoolLit:
		if v.Value {
			return constant.NewInt(irtypes.I1, 1)
		}

		return constant.NewInt(irtypes.I1, 0)
	case *ast.StringLit:
		raw := cg.newGlobalString(v.Value).(constant.Constant)
		strType := stringFatPtrType()
		lenVal := constant.NewInt(irtypes.I64, int64(len(v.Value)))

		s := constant.NewStruct(strType, raw, lenVal)
		if targetType.Equal(strType) {
			return s
		}

		return nil
	}

	// Fallback: route through the AST evaluator. This picks up BinExpr,
	// UnaryExpr, AsExpr, identifiers bound to const, and #pure call results.
	return cg.evalAsConstant(n, targetType)
}

// evalAsConstant runs the CTFE AST evaluator on n and returns the resulting
// LLVM constant coerced to targetType, or nil if the evaluator can't reduce n
// to a literal of i64 / f64 / bool kind.
func (cg *CodeGen) evalAsConstant(n ast.Node, targetType irtypes.Type) constant.Constant {
	if n == nil {
		return nil
	}

	val, err := evalNode(n, map[string]ctfeVal{}, cg, 0)
	if err != nil {
		return nil
	}

	switch val.kind {
	case "i64":
		c := constant.NewInt(irtypes.I64, val.i)

		coerced, ok := cg.constCoerce(c, targetType).(constant.Constant)
		if !ok {
			return nil
		}

		return coerced
	case "f64":
		c := constant.NewFloat(irtypes.Double, val.f)

		coerced, ok := cg.constCoerce(c, targetType).(constant.Constant)
		if !ok {
			return nil
		}

		return coerced
	case "bool":
		b := int64(0)
		if val.b {
			b = 1
		}

		return constant.NewInt(irtypes.I1, b)
	}

	return nil
}

// zeroConstant returns a zeroinitializer constant for the given LLVM type.
func (cg *CodeGen) zeroConstant(t irtypes.Type) constant.Constant {
	switch v := t.(type) {
	case *irtypes.IntType:
		return constant.NewInt(v, 0)
	case *irtypes.FloatType:
		return constant.NewFloat(v, 0)
	case *irtypes.PointerType:
		return constant.NewNull(v)
	case *irtypes.StructType:
		fields := make([]constant.Constant, len(v.Fields))
		for i, ft := range v.Fields {
			fields[i] = cg.zeroConstant(ft)
		}

		return constant.NewStruct(v, fields...)
	}

	return constant.NewZeroInitializer(t)
}

// emitTopLevelVarInits emits the runtime initializers for top-level vars into
// the provided block. Returns the (possibly advanced) current block.
func (cg *CodeGen) emitTopLevelVarInits(block *ir.Block) (*ir.Block, error) {
	for _, vi := range cg.topLevelVarInits {
		val, err := cg.genExpr(block, vi.initExpr)
		if err != nil {
			return block, err
		}

		if val != nil {
			lt := vi.global.ContentType
			val = cg.coerce(block, val, lt)
			cg.emitRetain(block, val)
			block.NewStore(val, vi.global)
		}
	}

	return block, nil
}

// emitPkgInitFns calls each package init function (declared as `fn init()`)
// in the order they were collected (dependency order: deps before dependents).
func (cg *CodeGen) emitPkgInitFns(block *ir.Block) {
	for _, f := range cg.pkgInitFns {
		block.NewCall(f)
	}
}

// emitFiberMainWrap wraps the body of a main function with fiber init/run calls
// when the program uses any fiber construct. The block passed in is the entry
// block of main; it returns the block where the original main code should start.
func (cg *CodeGen) emitFiberMainWrap(block *ir.Block) *ir.Block {
	if !cg.usesAnyFiber {
		return block
	}

	cg.ensureFiberRuntime()
	block.NewCall(cg.fiberInitFn)
	block.NewCall(cg.ioInitFn)

	return block
}

// emitFiberMainEnd appends the fiber run loop call before the main return.
func (cg *CodeGen) emitFiberMainEnd(block *ir.Block) {
	if !cg.usesAnyFiber {
		return
	}

	block.NewCall(cg.fiberRunFn)
}
