package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// maybeAutoCopyArgVal implements the modify-driven copy half of the
// call-site auto-selector: when a struct binding flows into a callee
// that mutates the corresponding parameter and this is NOT the
// binding's last use, emit a deep-copy of the struct as a fresh temp
// and pass the temp instead.  Caller's binding remains untouched, so
// any subsequent use of it sees the pre-call value.
//
// The selector skips when any of these holds (in order):
//   - There is no enclosing call-arg context (we're not inside a call).
//   - The callee is dynamic (lambda / trait dispatch / extern) and no
//     param-mutation info is available.
//   - The current arg position's param is not mutated by the body.
//   - The arg is not an Identifier (already a fresh value, literal,
//     field access, call result, etc. - no caller binding to protect).
//   - The arg is the binding's last use (Phase F has flagged it for
//     implicit move; move is strictly cheaper than copy here).
//   - The value's LLVM type is not a named struct that we can recurse
//     through (anonymous fat-ptrs / `any` / iface / fat-fn / bare
//     pointers / scalars are intentionally shared per the language
//     spec - the user wrote `arr` or `*T` when they wanted sharing).
//
// When all the gates pass, the helper calls the per-struct
// `__deep_copy` intrinsic and returns its result.  The caller's
// binding state is unchanged; the callee receives an isolated value.
func (cg *CodeGen) maybeAutoCopyArgVal(block *ir.Block, arg ast.Node, val value.Value) value.Value {
	if val == nil || arg == nil {
		return val
	}

	n := len(cg.callArgContextStack)
	if n == 0 {
		return val
	}

	ctx := cg.callArgContextStack[n-1]
	if ctx.calleeName == "" {
		return val
	}

	paramName := cg.callArgPosition(arg)
	if paramName == "" {
		return val
	}

	mutated := cg.paramMutatedFor(ctx.calleeName)
	if mutated == nil || !mutated[paramName] {
		return val
	}

	id, isID := arg.(*ast.Identifier)
	if !isID || id == nil {
		return val
	}

	if cg.implicitMoveSites[id] {
		return val
	}

	return cg.deepCopyArgValueIfRoutable(block, val)
}

// bindAutoCopyTemp anchors a deep-copy result in a synthetic scope
// alloca so the temp's fresh buffers get released when the enclosing
// scope exits.  Without this anchor, the copy's rc-tracked fields
// leak: the callee's scope-exit drops its bit-copy of the temp, but
// the caller has no other reference that would land on the cloned
// buffers.  noDeinit suppresses user-defined deinit on the synthetic
// clone (deinit fires on the original, the clone is a private temp).
func (cg *CodeGen) bindAutoCopyTemp(block *ir.Block, copied value.Value) value.Value {
	if cg.curScope == nil {
		return copied
	}

	alloca := block.NewAlloca(copied.Type())
	block.NewStore(copied, alloca)

	tmpName := fmt.Sprintf(".autocopy_%d", cg.strCount)
	cg.strCount++
	cg.curScope.set(tmpName, &scopeEntry{
		val:      alloca,
		isAlloc:  true,
		isRC:     true,
		noDeinit: true,
	})

	return block.NewLoad(copied.Type(), alloca)
}

// applyAutoCopyToArgVals post-processes a fully-evaluated argVals
// slice in place, replacing each entry with its auto-copied form if
// the dispatch conditions apply.  Call sites that build argVals via
// per-arg cg.genExpr loops invoke this once after the loop completes
// and before adaptArgs runs, so the coerced values fed into the call
// instruction are the isolated copies when isolation is required.
func (cg *CodeGen) applyAutoCopyToArgVals(block *ir.Block, args []ast.Node, argVals []value.Value) {
	for i, av := range argVals {
		if i >= len(args) {
			break
		}

		argVals[i] = cg.maybeAutoCopyArgVal(block, args[i], av)
	}
}

// maybeAutoCopyReceiverVal is the method-receiver counterpart to
// maybeAutoCopyArgVal: when the call is `value.method(...)` and the
// method body mutates its receiver param (`this.field = x`,
// `this.arr[i] = x`, etc.) AND the receiver Identifier still has
// uses after the call, emit a deep copy of the receiver as a fresh
// temp and pass the temp as the method's `this` slot.  The caller's
// binding for the receiver stays untouched.
//
// Skipped under the same conditions as maybeAutoCopyArgVal plus:
// the param name is hard-coded as "this" since callArgContextStack
// already strips the receiver from paramNames (see
// callContextInfoFor's FieldAccess branch).
func (cg *CodeGen) maybeAutoCopyReceiverVal(block *ir.Block, recvExpr ast.Node, val value.Value) value.Value {
	if val == nil || recvExpr == nil {
		return val
	}

	n := len(cg.callArgContextStack)
	if n == 0 {
		return val
	}

	ctx := cg.callArgContextStack[n-1]
	if ctx.calleeName == "" {
		return val
	}

	mutated := cg.paramMutatedFor(ctx.calleeName)
	if mutated == nil || !mutated["this"] {
		return val
	}

	id, isID := recvExpr.(*ast.Identifier)
	if !isID || id == nil {
		return val
	}

	if cg.implicitMoveSites[id] {
		return val
	}

	return cg.deepCopyArgValueIfRoutable(block, val)
}

// deepCopyArgValueIfRoutable inspects val's LLVM shape and returns
// either an isolated deep copy (anchored in the current scope so
// scope-exit releases the cloned buffers) or val unchanged when the
// shape isn't routable through the deep-copy machinery.
//
// Routing decisions per shape:
//   - Fat fn-ptr / iface / atom / any: intentional sharing
//     (the user wrote a reference shape on purpose).  `any` deep
//     copy via runtime tag dispatch is task #103.
//   - Anonymous fat shapes (string / `[T]`): deep-copy via
//     deepCopyFieldValue, which dispatches to deepCopyStringValue
//     or deepCopyArrayValue (the same helpers used for struct
//     field deep copy).
//   - cLayoutStructs: intentional sharing; the wrapper-block
//     release machinery is not portable to the generic walker.
//   - ADT (`data`): variant-tag-dispatched deep copy.
//   - Named struct: per-struct deep-copy intrinsic.
//   - Anything else: shared.
func (cg *CodeGen) deepCopyArgValueIfRoutable(block *ir.Block, val value.Value) value.Value {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return val
	}

	if isFatFnPtr(st) || isTraitFatPtrShape(st) || isAtomType(st) || isAnyType(st) {
		return val
	}

	name := st.Name()

	if name == "" {
		if !isStringType(st) && !isFatArrayPtr(st) {
			return val
		}

		copied := cg.deepCopyFieldValue(block, val, st)

		return cg.bindAutoCopyTemp(block, copied)
	}

	if cg.structTypeFor(CanonKey(name)) == nil {
		return val
	}

	if cg.cLayoutStructs[name] {
		return val
	}

	if cg.isDataType(st) {
		fn := cg.ensureDataValueDeepCopyFn(name, st)
		if fn == nil {
			return val
		}

		copied := block.NewCall(fn, val)

		return cg.bindAutoCopyTemp(block, copied)
	}

	fn := cg.ensureStructDeepCopyFn(name, st)
	copied := block.NewCall(fn, val)

	return cg.bindAutoCopyTemp(block, copied)
}
