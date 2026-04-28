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

	g := cg.mod.NewGlobal(irName, lt)
	g.Init = initVal

	entry := &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(lt), isGlobal: true}
	cg.curScope.set(tv.Name, entry)

	if exportedNames[tv.Name] && parentScope != nil {
		parentScope.set(pkgName+"::"+tv.Name, entry)
		parentScope.set(pkgName+"."+tv.Name, entry)

		if cg.moduleScope != nil && cg.moduleScope != parentScope {
			cg.moduleScope.set(pkgName+"::"+tv.Name, entry)
			cg.moduleScope.set(pkgName+"."+tv.Name, entry)
		}
	}

	cg.allTopLevelVars = append(cg.allTopLevelVars, topLevelVarInit{
		name:   irName,
		global: g,
	})

	if tv.Value != nil && cg.tryConstantFold(tv.Value, lt) == nil {
		cg.topLevelVarInits = append(cg.topLevelVarInits, topLevelVarInit{
			name:     irName,
			global:   g,
			initExpr: tv.Value,
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
		g := cg.mod.NewGlobal(tv.Name, lt)
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

	g := cg.mod.NewGlobal(tv.Name, lt)
	g.Init = initVal

	// Register in global scope as a pointer (alloc-style) so that loads/stores work.
	// isGlobal=true prevents per-function scope release from deiniting the global.
	cg.curScope.set(tv.Name, &scopeEntry{val: g, isAlloc: true, isRC: isRCTrackedType(lt), isGlobal: true})

	// Track every top-level var for deinit-at-exit (regardless of init type).
	cg.allTopLevelVars = append(cg.allTopLevelVars, topLevelVarInit{
		name:   tv.Name,
		global: g,
	})

	// If the initializer needs runtime evaluation, queue it.
	if tv.Value != nil && cg.tryConstantFold(tv.Value, lt) == nil {
		cg.topLevelVarInits = append(cg.topLevelVarInits, topLevelVarInit{
			name:     tv.Name,
			global:   g,
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

// tryConstantFold attempts to evaluate an AST node as a compile-time constant.
// Returns nil if the expression cannot be folded at compile time.
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
