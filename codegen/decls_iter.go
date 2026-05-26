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

// isRefIterGetImpl returns true when decl is a `fn ref_iter::get(this, i)`
// trait-impl method on some struct.  Used to opt that one method out of the
// usual entry-retain-of-`this` and retain-on-return passes: the caller is a
// for-ref loop that already keeps the source container alive for the entire
// iteration, so the borrow's two retain/release pairs are dead weight.
func isRefIterGetImpl(decl *ast.FuncDecl) bool {
	if decl == nil || decl.Name != "get" {
		return false
	}

	tq := decl.TraitQualifier
	if tq == "" {
		return false
	}
	// Match "ref_iter" exactly or "ref_iter[...]" (instantiated form).
	if tq == "ref_iter" {
		return true
	}

	return len(tq) > len("ref_iter") && tq[:len("ref_iter")] == "ref_iter" && tq[len("ref_iter")] == '['
}

func (cg *CodeGen) tryCoerceToIter(block *ir.Block, iterVal value.Value) (value.Value, string, bool, bool) {
	return cg.tryCoerceToNamedIter(block, iterVal, "iter")
}

// tryCoerceToRefIter mirrors tryCoerceToIter but matches against the
// `ref_iter` trait family.  Used by genForIn for `for ref x in custom`.
func (cg *CodeGen) tryCoerceToRefIter(block *ir.Block, iterVal value.Value) (value.Value, string, bool, bool) {
	return cg.tryCoerceToNamedIter(block, iterVal, "ref_iter")
}

func (cg *CodeGen) tryCoerceToNamedIter(block *ir.Block, iterVal value.Value, wantBase string) (value.Value, string, bool, bool) {
	// Case 1: already a trait fat pointer.
	if instKey, ok := cg.isTraitFatPtr(iterVal.Type()); ok {
		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}

		if baseTrait == wantBase {
			return iterVal, instKey, true, false
		}

		return nil, "", false, false
	}

	// Case 2: concrete struct that has a matching vtable registered.
	structName := cg.typeNameOf(iterVal.Type())
	if structName == "" {
		return nil, "", false, false
	}

	// coerceToTrait heap-allocates the iface data ptr unconditionally
	// (both value-source and pointer-source branches), so the caller
	// always owns the resulting ARC ref and must release it on loop exit.
	ownsData := true

	for vtableKey := range cg.traitVtableGlobals {
		// vtableKey format: "structName__instKey"
		prefix := structName + "__"
		if len(vtableKey) <= len(prefix) || vtableKey[:len(prefix)] != prefix {
			continue
		}

		instKey := vtableKey[len(prefix):]

		baseTrait := instKey
		if base, exists := cg.traitInstKeys[instKey]; exists {
			baseTrait = base
		}

		if baseTrait != wantBase {
			continue
		}
		// Coerce to fat pointer. Suppress the deferred scope-exit
		// release that coerceToTrait would otherwise register --
		// genForIterTrait emits its own release at the loop's exit
		// block, scoped tightly to the iteration rather than the
		// surrounding fn.
		prevSuppress := cg.suppressIfaceScopeRelease
		cg.suppressIfaceScopeRelease = true

		fatPtr, err := cg.coerceToTrait(block, iterVal, instKey)
		cg.suppressIfaceScopeRelease = prevSuppress

		if err != nil {
			continue
		}

		return fatPtr, instKey, true, ownsData
	}

	return nil, "", false, false
}

// genForIterTrait generates a for-in loop over a value that implements iter[T].
// It calls len() (vtable slot 0) for the count, and get(i) (vtable slot 1) for
// each element.
func (cg *CodeGen) genForIterTrait(block *ir.Block, s *ast.ForStmt, iterFatPtr value.Value, instKey string, ownsData bool) (*ir.Block, error) {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	// Look up method order: ["len", "get"]
	methodOrder := cg.traitMethodOrder[baseTrait]
	lenSlot, getSlot := -1, -1

	for i, name := range methodOrder {
		switch name {
		case "len":
			lenSlot = i
		case "get":
			getSlot = i
		}
	}

	if lenSlot < 0 || getSlot < 0 {
		return nil, fmt.Errorf("iter trait %s missing len/get methods", cg.traitDisplayName(instKey))
	}

	vtableSt := cg.vtableFor(CanonKey(instKey))

	// Determine element type from get's return type (vtable slot getSlot).
	getFnType := vtableSt.Fields[getSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	elemType := getFnType.RetType

	// Helper to load a function pointer from a vtable slot.
	loadSlot := func(b *ir.Block, vtablePtr value.Value, slot int) value.Value {
		slotFnType := vtableSt.Fields[slot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		gep := b.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slot)))

		return b.NewLoad(irtypes.NewPointer(slotFnType), gep)
	}

	// Extract components of fat pointer.
	dataPtr := block.NewExtractValue(iterFatPtr, 0)
	vtablePtr := block.NewExtractValue(iterFatPtr, 1)

	// The iterFatPtr was constructed by tryCoerceToIter -> coerceToTrait,
	// which heap-allocates the source struct via _tin_rc_alloc when the
	// iter value is by-value (not already a pointer). The for-loop owns
	// that ARC reference and must release it on exit so the storage is
	// reclaimed. Without this, every for-in over a value-typed iter
	// leaked the heap-allocated source.

	// Call len().
	lenFnType := vtableSt.Fields[lenSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	lenFnPtr := loadSlot(block, vtablePtr, lenSlot)
	totalLen := block.NewCall(lenFnPtr, cg.adaptArgs(block, []value.Value{dataPtr}, lenFnType)...)

	// Alloca for index.
	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	condBlock := cg.newBlock("iterfor.cond")
	bodyBlock := cg.newBlock("iterfor.body")
	afterBlock := cg.newBlock("iterfor.after")

	brToCond := block.NewBr(condBlock)

	// Cond: idx < len.
	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenI64 := cg.coerce(condBlock, totalLen, irtypes.I64)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenI64)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	// Body: call get(idx).
	cg.curScope = newScope(cg.curScope)

	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	getFnPtr := loadSlot(bodyBlock, vtablePtr, getSlot)
	getArgs := cg.adaptArgs(bodyBlock, []value.Value{dataPtr, bodyIdx}, getFnType)
	elemVal := bodyBlock.NewCall(getFnPtr, getArgs...)

	// Register loop variable.
	if s.VarName != "" {
		elemAlloca := bodyBlock.NewAlloca(elemType)
		bodyBlock.NewStore(elemVal, elemAlloca)
		cg.curScope.set(s.VarName, &scopeEntry{val: elemAlloca, isAlloc: true})
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	// Increment.
	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	// Release the iter iface's data ptr at loop exit if we own it (i.e.,
	// tryCoerceToIter heap-allocated it from a value-source struct).
	// Pointer-source iterVals are borrowed and the original *T owner
	// handles cleanup -- releasing here would corrupt non-RC malloc'd
	// blocks (see malloc_dispatch.tin pattern).
	if ownsData {
		afterBlock.NewCall(cg.ensureRelease(), dataPtr)
	}

	return afterBlock, nil
}

// genForRefIterTrait generates a `for ref x in xs` loop where xs implements
// ref_iter[T].  Same shape as genForIterTrait, but get() returns `*T` and the
// loop variable aliases the slot the pointer targets -- reads/writes through
// `x` go to the underlying storage, matching `for ref` over a builtin array.
func (cg *CodeGen) genForRefIterTrait(block *ir.Block, s *ast.ForStmt, iterFatPtr value.Value, instKey string, ownsData bool) (*ir.Block, error) {
	baseTrait := instKey
	if base, ok := cg.traitInstKeys[instKey]; ok {
		baseTrait = base
	}

	methodOrder := cg.traitMethodOrder[baseTrait]
	lenSlot, getSlot := -1, -1

	for i, name := range methodOrder {
		switch name {
		case "len":
			lenSlot = i
		case "get":
			getSlot = i
		}
	}

	if lenSlot < 0 || getSlot < 0 {
		return nil, fmt.Errorf("ref_iter trait %s missing len/get methods", cg.traitDisplayName(instKey))
	}

	vtableSt := cg.vtableFor(CanonKey(instKey))

	getFnType := vtableSt.Fields[getSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)

	elemPtrType, ok := getFnType.RetType.(*irtypes.PointerType)
	if !ok {
		return nil, cg.nodeErr(s, "ref_iter::get must return a pointer; got %s", getFnType.RetType.String())
	}

	elemType := elemPtrType.ElemType

	loadSlot := func(b *ir.Block, vtablePtr value.Value, slot int) value.Value {
		slotFnType := vtableSt.Fields[slot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
		gep := b.NewGetElementPtr(vtableSt, vtablePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(slot)))

		return b.NewLoad(irtypes.NewPointer(slotFnType), gep)
	}

	dataPtr := block.NewExtractValue(iterFatPtr, 0)
	vtablePtr := block.NewExtractValue(iterFatPtr, 1)

	lenFnType := vtableSt.Fields[lenSlot].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
	lenFnPtr := loadSlot(block, vtablePtr, lenSlot)
	totalLen := block.NewCall(lenFnPtr, cg.adaptArgs(block, []value.Value{dataPtr}, lenFnType)...)

	idxAlloca := block.NewAlloca(irtypes.I64)
	block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	condBlock := cg.newBlock("refiterfor.cond")
	bodyBlock := cg.newBlock("refiterfor.body")
	afterBlock := cg.newBlock("refiterfor.after")

	brToCond := block.NewBr(condBlock)

	idx := condBlock.NewLoad(irtypes.I64, idxAlloca)
	lenI64 := cg.coerce(condBlock, totalLen, irtypes.I64)
	cond := condBlock.NewICmp(enum.IPredSLT, idx, lenI64)
	condBlock.NewCondBr(cond, bodyBlock, afterBlock)

	cg.attachForLoopDbg(s.Pos(), brToCond, condBlock)

	cg.curScope = newScope(cg.curScope)

	bodyIdx := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
	getFnPtr := loadSlot(bodyBlock, vtablePtr, getSlot)
	getArgs := cg.adaptArgs(bodyBlock, []value.Value{dataPtr, bodyIdx}, getFnType)
	elemPtr := bodyBlock.NewCall(getFnPtr, getArgs...)

	// No retain/release pair on `elemPtr` per iteration: ref_iter::get
	// is opted out of the borrow's entry- and return-retain (see
	// isRefIterGetImpl + curIsRefIterGet) and the source container is
	// kept alive by dataPtr for the loop's lifetime.

	if s.VarName != "" {
		isElemRC := isRCTrackedType(elemType)
		// Bind x to the *T returned by get(i): reads load through it,
		// assignments store through it (auto-deref alias semantics).
		// noRelease=true because the storage is owned by the source
		// container, not by the loop scope.
		cg.curScope.set(s.VarName, &scopeEntry{
			val: elemPtr, isAlloc: true, isRC: isElemRC,
			declPos:          s.Pos(),
			noRelease:        true,
			isForRefIterator: true,
		})
		cg.warnIfBuiltinShadow("for-in", s.VarName, s.Pos())
	}

	var bodyErr error

	cg.pushBreakTarget(afterBlock)
	bodyBlock, _, bodyErr = cg.genStmt(bodyBlock, s.Body)
	cg.popBreakTarget()
	cg.emitScopeRelease(bodyBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if bodyErr != nil {
		return nil, bodyErr
	}

	if bodyBlock != nil && bodyBlock.Term == nil {
		bodyIdx2 := bodyBlock.NewLoad(irtypes.I64, idxAlloca)
		newIdx := bodyBlock.NewAdd(bodyIdx2, constant.NewInt(irtypes.I64, 1))
		bodyBlock.NewStore(newIdx, idxAlloca)

		if cg.curFnAutoYield {
			cg.genYieldAutoAt(bodyBlock, condBlock)
		} else {
			bodyBlock.NewBr(condBlock)
		}
	}

	if ownsData {
		afterBlock.NewCall(cg.ensureRelease(), dataPtr)
	}

	return afterBlock, nil
}

// coerceToTrait constructs a trait fat pointer {i8* data, vtable*} from a
// concrete struct value or pointer, given the target instKey (e.g. "named" or "iter_i64").
// If structVal is already a *struct (e.g. from malloc), the heap pointer is
// used directly as the data pointer instead of allocating new stack space.
// checkTraitDefaultMutationForms enforces rule 2's pre-condition: a
// trait declared with value-receiver methods (`fn foo(this Trait)`)
// cannot ship default-method bodies that assign to forward fields,
// because the auto-injected impl on every struct now preserves the
// trait def's value receiver (see augmentStructFromTraits) and a
// `this.field = X` against a value receiver doesn't propagate.  The
// user has to declare the trait pointer-receiver (`this *Trait`) up
