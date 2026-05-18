package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) markOutParamVarsHeapOwned(call *ast.CallExpr) {
	for _, arg := range call.Args {
		addrOf, ok := arg.(*ast.AddressOfExpr)
		if !ok {
			continue
		}

		ident, ok2 := addrOf.Expr.(*ast.Identifier)
		if !ok2 {
			continue
		}

		entry, ok3 := cg.curScope.lookup(ident.Name)
		if !ok3 || entry.tinType == nil {
			continue
		}
		// Count pointer levels and find cLayoutStruct base.
		depth := 0
		cur := entry.tinType

		for {
			pt, ptOk := cur.(*ast.PointerType)
			if !ptOk {
				break
			}

			depth++

			if st, stOk := pt.Elem.(*ast.SimpleType); stOk {
				if cg.cLayoutStructs[st.Name] {
					// varName has type (depth)*S where S is cLayoutStruct.
					// After a write-back from a (depth+1)*S param, varName holds
					// a heap chain of depth levels.
					entry.isHeapOwned = true
					entry.heapOwnedDepth = depth
				}

				break
			}

			cur = pt.Elem
		}
	}
}

func (cg *CodeGen) genAssign(block *ir.Block, s *ast.AssignStmt) (*ir.Block, error) {
	// `_ = expr` is the explicit discard form: evaluate expr for its side
	// effects and throw away the result. Acts like an ExprStmt without
	// triggering the discarded-result warning.
	if id, ok := s.Target.(*ast.Identifier); ok && id.Name == "_" {
		if _, err := cg.genExpr(block, s.Value); err != nil {
			return block, err
		}

		return block, nil
	}
	// Detect `x = x` self-assign: same identifier on both sides.
	if tid, ok := s.Target.(*ast.Identifier); ok {
		if vid, ok2 := s.Value.(*ast.Identifier); ok2 && tid.Name == vid.Name {
			cg.warn(DiagSelfAssign, s.Pos(),
				"self-assignment %q has no effect", tid.Name)
		}
	}

	if err := cg.checkFieldWritable(s.Target); err != nil {
		return block, err
	}
	// Reject direct assignment to a `const` binding, top-level or block-
	// scope. Without this, `const X i64 = 10; X = 99` silently compiles
	// and writes to the read-only global -- LLVM's `constant` qualifier
	// then makes the store unreachable, so the original value persists
	// AND the user is silently lied to about whether the write happened.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if cg.topLevelConstNames[id.Name] {
			return block, cg.nodeErr(s, "cannot assign to top-level const %q (immutable storage)", id.Name)
		}

		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.declaredConst {
			return block, cg.nodeErr(s, "cannot assign to const %q; drop the const if you need to mutate", id.Name)
		}
	}
	// Mutating an identifier invalidates any captured constant init (used
	// by the if-condition folder). Clear it before emitting the store so
	// later folds don't see stale information.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}
	// User struct (or *Struct) target: dispatch to ::index_set trait
	// method when the receiver struct implements index_set[K, V], or
	// emit a SIMD insertelement when the receiver is a vector.  Both
	// branches need the rvalue of the receiver, so emit it once here
	// rather than once per branch -- emitting twice double-runs any
	// side effects in the receiver expression and silently swallowed
	// the genExpr error on the second go.  The third evaluation
	// (genLValue for the SIMD store-back) is unavoidable but only
	// fires for genuinely-addressable LHS expressions.
	if idxExpr, ok := s.Target.(*ast.IndexExpr); ok {
		recv, err2 := cg.genExpr(block, idxExpr.Expr)
		if err2 != nil {
			return block, err2
		}

		if recv != nil {
			if structName := cg.structNameForReceiver(recv.Type()); structName != "" {
				idx, err3 := cg.genExpr(block, idxExpr.Index)
				if err3 != nil {
					return block, err3
				}

				val, err4 := cg.genExpr(block, s.Value)
				if err4 != nil {
					return block, err4
				}

				if fn := cg.lookupOpMethod(structName, "index_set",
					[]irtypes.Type{idx.Type(), val.Type()}); fn != nil {
					_, dErr := cg.emitOpDispatch(block, fn, recv, []value.Value{idx, val})
					if dErr != nil {
						return block, dErr
					}

					return block, nil
				}

				return block, cg.nodeErr(s,
					"type %s has no `::index_set` impl for (key %s, value %s); declare `fn ::index_set(this %s, k %s, v %s)`",
					cg.tinTypeDisplay(recv.Type()), cg.tinTypeDisplay(idx.Type()), cg.tinTypeDisplay(val.Type()),
					cg.tinTypeDisplay(recv.Type()), cg.tinTypeDisplay(idx.Type()), cg.tinTypeDisplay(val.Type()))
			}

			if vecType, isVec := recv.Type().(*irtypes.VectorType); isVec {
				idxVal, err3 := cg.genExpr(block, idxExpr.Index)
				if err3 != nil {
					return block, err3
				}

				newElem, err4 := cg.genExpr(block, s.Value)
				if err4 != nil {
					return block, err4
				}

				newElem = cg.coerce(block, newElem, vecType.ElemType)
				idx32 := cg.coerce(block, idxVal, irtypes.I32)
				updated := block.NewInsertElement(recv, newElem, idx32)

				vecPtr, err5 := cg.genLValue(block, idxExpr.Expr)
				if err5 != nil {
					return block, err5
				}

				block.NewStore(updated, vecPtr)

				return block, nil
			}
		}
	}

	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return block, err
	}

	cg.curBlock = block

	// Plumb the target's element type so that callee-side generators
	// (notably empty-array literals `[]`) can pick the right shape up
	// front instead of leaving a `{i8*, i64}` for coerce to massage.
	ptrType := ptr.Type().(*irtypes.PointerType)

	val, err := cg.genArgWithTargetType(block, s.Value, ptrType.ElemType)
	if err != nil {
		return block, err
	}
	// If genExpr advanced the current block (e.g. await inside rhs), use
	// the continuation block for all subsequent emissions.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	srcType := val.Type()

	val = cg.coerce(block, val, ptrType.ElemType)
	if !val.Type().Equal(ptrType.ElemType) {
		return block, cg.nodeErr(s,
			"cannot assign value of type %s (declared type %s)",
			cg.tinTypeDisplay(srcType), cg.tinTypeDisplay(ptrType.ElemType))
	}
	// ARC: for RC-tracked types, retain new value (if copy) then release old.
	// Skip retain if coerce just boxed a non-any value to any: the new box is
	// a fresh _tin_rc_alloc (rc=1) and is already owned.
	// Weak field targets are non-owning: skip both retain and release.
	isWeakTarget := false

	if fa, ok2 := s.Target.(*ast.FieldAccess); ok2 {
		// Unwrap an explicit dereference: (*x).field -> look up x.
		innerExpr := fa.Expr
		if de, ok3 := innerExpr.(*ast.DerefExpr); ok3 {
			innerExpr = de.Expr
		}

		if ident, ok3 := innerExpr.(*ast.Identifier); ok3 {
			if se, ok4 := cg.curScope.lookup(ident.Name); ok4 {
				if pt, ok5 := se.val.Type().(*irtypes.PointerType); ok5 {
					parentName := cg.typeNameOf(pt.ElemType)
					// pt.ElemType is the variable's declared type (e.g. *Node).
					// If it is itself a pointer, unwrap one more level to reach the struct.
					if parentName == "" {
						if pt2, ok6 := pt.ElemType.(*irtypes.PointerType); ok6 {
							parentName = cg.typeNameOf(pt2.ElemType)
						}
					}

					if parentName != "" {
						isWeakTarget = cg.structWeakFields[parentName][fa.Field]
					}
				}
			}
		} else if innerFA, ok3 := innerExpr.(*ast.FieldAccess); ok3 {
			// Handle chained field access like (*this.head).prev:
			// innerExpr = FieldAccess{this, "head"} -> resolve this -> get head's type.
			baseIdent, ok4 := innerFA.Expr.(*ast.Identifier)
			if !ok4 {
				if de2, ok5 := innerFA.Expr.(*ast.DerefExpr); ok5 {
					baseIdent, ok4 = de2.Expr.(*ast.Identifier)
				}
			}

			if ok4 {
				if se, ok5 := cg.curScope.lookup(baseIdent.Name); ok5 {
					// se.val is *ParentStruct or **ParentStruct; unwrap to get ParentStruct name.
					baseType := se.val.Type()
					if pt, ok6 := baseType.(*irtypes.PointerType); ok6 {
						baseType = pt.ElemType
						if pt2, ok7 := baseType.(*irtypes.PointerType); ok7 {
							baseType = pt2.ElemType
						}
					}

					if baseSt, ok6 := baseType.(*irtypes.StructType); ok6 && baseSt.Name() != "" {
						// Now look up the type of innerFA.Field within baseSt.
						fieldIdx := cg.fieldIndex(baseSt.Name(), innerFA.Field)
						if fieldIdx >= 0 && fieldIdx < len(baseSt.Fields) {
							fieldType := baseSt.Fields[fieldIdx]
							// Unwrap pointer to get the struct pointed to by this field.
							if fpt, ok7 := fieldType.(*irtypes.PointerType); ok7 {
								if innerSt, ok8 := fpt.ElemType.(*irtypes.StructType); ok8 && innerSt.Name() != "" {
									isWeakTarget = cg.structWeakFields[innerSt.Name()][fa.Field]
								}
							}
						}
					}
				}
			}
		}
	}

	// Heap-owned pointer reassign: when the target is an Identifier whose
	// scope entry was marked isHeapOwned (e.g. `let head = make_chain(...)`
	// returning *Node), the binding owns the chain and reassigning must
	// release the prior chain before the new value overwrites it. Mirrors
	// what emitScopeRelease does at scope exit.
	//
	// Only applied to Identifier targets: FieldAccess writes are handled by
	// the isTinStructPtrElem branch below (with its own retain logic), and
	// pointer dereferences are raw stores by design.
	if id, isID := s.Target.(*ast.Identifier); isID {
		if entry, ok := cg.curScope.lookup(id.Name); ok && entry.isHeapOwned {
			oldVal := block.NewLoad(ptrType.ElemType, ptr)

			if entry.heapOwnedDepth > 1 {
				structName := cLayoutStructBaseName(entry.tinType)
				if structName != "" {
					relFn := cg.ensureHeapChainReleaseFn(structName, entry.heapOwnedDepth)
					block.NewCall(relFn, oldVal)
				} else {
					cg.emitHeapChainRelease(block, oldVal, entry.heapOwnedDepth)
				}
			} else {
				cg.emitHeapChainRelease(block, oldVal, entry.heapOwnedDepth)
			}
		}
	}

	// Check if the element is a pointer to a known Tin struct (ARC-managed
	// via &Struct{} allocation).  Only for struct FIELD assignments (e.g.
	// this.head = n), not for arbitrary pointer dereferences (*pp = target)
	// which are raw pointer stores, not ownership transfers.
	isTinStructPtrElem := false
	// isTraitIfacePtrElem: the target field is `*Trait_iface` (a
	// pointer to a trait fat-pointer struct).  These ARE RC-tracked
	// blocks (allocated via _tin_rc_alloc inside
	// buildPtrToTraitBorrow / coerceToTrait), but isRCTrackedType
	// only recognizes the iface STRUCT type, not the pointer-to-
	// iface shape that appears in struct fields and bindings.
	// Without retain-on-store + release-of-old, reassigning a
	// `*Trait` field would dangle on the next reader (the
	// caller-side release in emitCallArgReleaseForRet frees the
	// iface block the field still pointed at).
	isTraitIfacePtrElem := false

	if _, isFieldTarget := s.Target.(*ast.FieldAccess); isFieldTarget {
		if ept, ok6 := ptrType.ElemType.(*irtypes.PointerType); ok6 {
			if innerSt, ok7 := ept.ElemType.(*irtypes.StructType); ok7 && innerSt.Name() != "" {
				isTinStructPtrElem = cg.structTypeFor(CanonKey(innerSt.Name())) != nil
			}
		}

		isTraitIfacePtrElem = cg.isTraitFatPtrPtrType(ptrType.ElemType)
	}

	if (isRCTrackedType(ptrType.ElemType) || isTinStructPtrElem || isTraitIfacePtrElem) && !isWeakTarget {
		boxedToAny := isAnyType(ptrType.ElemType) && !isAnyType(srcType)
		if isCopyExpr(s.Value) && !boxedToAny && !isFreshBytesAlloc(val) {
			if isTinStructPtrElem || isTraitIfacePtrElem {
				// Direct _tin_retain for *TinStruct and
				// *Trait_iface pointers (emitRetain doesn't
				// handle these because the leading-ptr
				// classification only matches the iface STRUCT
				// shape, not the *iface pointer shape).
				ptrI8 := block.NewBitCast(val, irtypes.I8Ptr)
				block.NewCall(cg.ensureRetain(), ptrI8)
			} else {
				cg.emitRetain(block, val)
			}
		}

		oldVal := block.NewLoad(ptrType.ElemType, ptr)
		cg.emitRelease(block, oldVal)
	} else if !isWeakTarget {
		// Struct values: release the previous value if it has any RC-tracked
		// fields (string, [T], any, fn, nested struct) or an explicit deinit.
		// Without this the old value's RC fields leak whenever a struct is
		// reassigned. Mirrors the gate used by emitScopeRelease.
		//
		// On the same gate, retain the NEW value's RC fields when it's a
		// borrowed copy of another binding (isCopyExpr): without the
		// retain, both the source binding and this slot release the same
		// underlying buffers when their scopes exit. Fresh callee-returned
		// structs (isFreshBytesAlloc) already carry an unbalanced retain
		// from the callee, so we move ownership instead of retaining.
		if cg.typeNameOf(ptrType.ElemType) != "" && cg.elemNeedsRelease(ptrType.ElemType) {
			if isCopyExpr(s.Value) && !isFreshBytesAlloc(val) {
				cg.emitRetain(block, val)
			}

			oldVal := block.NewLoad(ptrType.ElemType, ptr)
			cg.emitRelease(block, oldVal)
		}
	}

	block.NewStore(val, ptr)

	return block, nil
}

func (cg *CodeGen) genAugAssign(block *ir.Block, s *ast.AugAssignStmt) (*ir.Block, error) {
	if err := cg.checkFieldWritable(s.Target); err != nil {
		return block, err
	}
	// Reject compound-assign to a `const` binding (same reason as
	// genAssign -- the underlying global lives in read-only storage).
	if id, ok := s.Target.(*ast.Identifier); ok {
		if cg.topLevelConstNames[id.Name] {
			return block, cg.nodeErr(s, "cannot %s top-level const %q (immutable storage)", s.Op, id.Name)
		}

		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.declaredConst {
			return block, cg.nodeErr(s, "cannot %s const %q; drop the const if you need to mutate", s.Op, id.Name)
		}
	}
	// Mutating an identifier invalidates any captured constant init.
	if id, ok := s.Target.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}

	ptr, err := cg.genLValue(block, s.Target)
	if err != nil {
		return block, err
	}

	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	cg.curBlock = block

	rhs, err := cg.genExpr(block, s.Value)
	if err != nil {
		return block, err
	}
	// If genExpr advanced the current block (e.g. await inside rhs), use
	// the continuation block for all subsequent emissions.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	// Operator overloading: `a OP= b` on a user struct desugars to
	// `a = a.OP(b)` via the corresponding op trait. Falls through to the
	// primitive switch when the LHS is not a struct.
	if isStructType(elemType) {
		if traitName := compoundAssignTraitName(s.Op); traitName != "" {
			structName := cg.typeNameOf(elemType)
			if fn := cg.lookupOpMethod(structName, traitName, []irtypes.Type{rhs.Type()}); fn != nil {
				res, derr := cg.emitOpDispatch(block, fn, current, []value.Value{rhs})
				if derr != nil {
					return block, derr
				}

				if res != nil {
					// Release the previous value before overwriting so any
					// RC-tracked fields (strings, fat arrays, ...) are not
					// leaked. Mirrors the regular assign path above.
					if cg.elemNeedsRelease(elemType) {
						cg.emitRelease(block, current)
					}

					block.NewStore(cg.coerce(block, res, elemType), ptr)
				}

				return block, nil
			}

			return block, cg.nodeErr(s, "compound assignment %q is not defined for operands of type %s and %s",
				s.Op, cg.fmtArgType(elemType), cg.fmtArgType(rhs.Type()))
		}
	}

	// Coerce the rhs to the LHS slot's type.  For scalar ops this is a
	// no-op when both sides share the same numeric type; for `++=` the
	// LHS slot is a slice (`{T*, i64}`) and the rhs must also be a
	// `[T]` literal/value -- mismatches produce a compile error inside
	// the ++= branch (see below).
	rhs = cg.coerce(block, rhs, elemType)

	var result value.Value

	switch s.Op {
	case "+=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			idx := cg.coerce(block, rhs, irtypes.I64)
			result = block.NewGetElementPtr(pt.ElemType, current, idx)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFAdd(current, rhs)
		} else {
			result = block.NewAdd(current, rhs)
		}
	case "-=":
		if pt, ok := elemType.(*irtypes.PointerType); ok {
			idx := cg.coerce(block, rhs, irtypes.I64)
			neg := block.NewSub(constant.NewInt(irtypes.I64, 0), idx)
			result = block.NewGetElementPtr(pt.ElemType, current, neg)
		} else if irtypes.IsFloat(elemType) {
			result = block.NewFSub(current, rhs)
		} else {
			result = block.NewSub(current, rhs)
		}
	case "*=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFMul(current, rhs)
		} else {
			result = block.NewMul(current, rhs)
		}
	case "/=":
		if irtypes.IsFloat(elemType) {
			result = block.NewFDiv(current, rhs)
		} else {
			result = block.NewSDiv(current, rhs)
		}
	case "++=":
		// Slice concat-assign: `xs ++= ys` extends `xs : [T]` with all
		// elements of `ys : [T]`.  The right-hand side must be the same
		// slice type as the left -- to append a single value, wrap it
		// as a one-element literal: `xs ++= [v]`.  Emission mirrors the
		// `++` binary operator at exprs.go:1908:
		//
		//   new_len = old_len + rhs_len
		//   new_ptr = rc_alloc(new_len * sizeof(T))
		//   memcpy(new_ptr,             old_ptr, old_len * sizeof(T))
		//   memcpy(new_ptr + old_bytes, rhs_ptr, rhs_len * sizeof(T))
		//
		// For non-temporary RHS, retain each copied element so the new
		// buffer co-owns them.  Always release the old buffer; for a
		// temporary RHS, also release its backing buffer (the contained
		// element refs have been transferred to the new buffer).
		if !isFatArrayPtr(elemType) {
			return block, cg.nodeErr(s,
				"`++=` requires a slice ([T]) on the left-hand side, got %s",
				cg.fmtArgType(elemType))
		}

		fatType := elemType.(*irtypes.StructType)
		dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
		elemT := dataPtrType.ElemType

		if !rhs.Type().Equal(elemType) {
			return block, cg.nodeErr(s,
				"`++=` expects a `[%s]` on the right-hand side; got %s. "+
					"`++=` is concat-assign (not append-one) - to append a "+
					"single value, wrap it: `xs ++= [v]`",
				cg.fmtArgType(elemT), cg.fmtArgType(rhs.Type()))
		}

		oldPtr := block.NewExtractValue(current, 0)
		oldLen := block.NewExtractValue(current, 1)
		rhsPtr := block.NewExtractValue(rhs, 0)
		rhsLen := block.NewExtractValue(rhs, 1)
		newLen := block.NewAdd(oldLen, rhsLen)

		// sizeof(elemT) via GEP trick.
		nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
		sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
		elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
		newBytes := block.NewMul(newLen, elemSize)

		newI8Ptr := block.NewCall(cg.ensureRCAlloc(), newBytes)
		newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

		// memcpy old data (ownership transfers from old_buf to new_buf).
		oldBytes := block.NewMul(oldLen, elemSize)
		oldI8Ptr := block.NewBitCast(oldPtr, irtypes.I8Ptr)
		block.NewCall(cg.ensureMemcpy(), newI8Ptr, oldI8Ptr, oldBytes, constant.NewInt(irtypes.I1, 0))

		// memcpy RHS data at offset oldLen*elemSize.
		rhsOffset := block.NewMul(oldLen, elemSize)
		rhsDst := block.NewGetElementPtr(irtypes.I8, newI8Ptr, rhsOffset)
		rhsI8Ptr := block.NewBitCast(rhsPtr, irtypes.I8Ptr)
		rhsBytes := block.NewMul(rhsLen, elemSize)
		block.NewCall(cg.ensureMemcpy(), rhsDst, rhsI8Ptr, rhsBytes, constant.NewInt(irtypes.I1, 0))

		// ARC: when the RHS is an alias to existing storage, the new
		// buffer shares element pointers with the source.  Retain each
		// copied element so the two buffers can release independently.
		// `elemNeedsRelease` returns false for raw pointer types, but
		// pointer elements stored inside `[*T]` arrays DO need
		// retain/release -- check that case explicitly.
		_, elemIsPtr := elemT.(*irtypes.PointerType)
		needsElemRetain := cg.elemNeedsRelease(elemT) || isRCTrackedType(elemT) || elemIsPtr

		if !isTemporaryProducer(s.Value) && needsElemRetain {
			cg.emitRetainElemSlice(block, rhsDst, rhsLen, elemT)
		}

		// Build the new fat-ptr value.
		fatAlloca := block.NewAlloca(fatType)
		ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(newPtr, ptrGep)
		lenGep := block.NewGetElementPtr(fatType, fatAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(newLen, lenGep)
		result = block.NewLoad(fatType, fatAlloca)

		// ARC: drop the old buffer (its element refs transferred to
		// new_buf via memcpy; releasing the old buffer's outer rc
		// frees the bytes without touching embedded refs).
		block.NewCall(cg.ensureRelease(), oldI8Ptr)

		// ARC: when the RHS was a temporary (fresh literal or call
		// result), the temp buffer's element refs were transferred
		// into the new buffer.  Release the temp's outer buffer to
		// reclaim its bytes without touching elements.
		if isTemporaryProducer(s.Value) {
			block.NewCall(cg.ensureRelease(), rhsI8Ptr)
		}
	default:
		result = rhs
	}

	block.NewStore(result, ptr)

	return block, nil
}

func (cg *CodeGen) genPostfix(block *ir.Block, s *ast.PostfixStmt) error {
	if err := cg.checkFieldWritable(s.Expr); err != nil {
		return err
	}
	// Mutation invalidates any captured constant init.
	if id, ok := s.Expr.(*ast.Identifier); ok {
		if entry, ok2 := cg.curScope.lookup(id.Name); ok2 {
			entry.constInitExpr = nil
			entry.staticArrayLen = 0
		}
	}

	ptr, err := cg.genLValue(block, s.Expr)
	if err != nil {
		return err
	}

	ptrType := ptr.Type().(*irtypes.PointerType)
	elemType := ptrType.ElemType
	current := block.NewLoad(elemType, ptr)

	one := cg.coerce(block, constant.NewInt(irtypes.I64, 1), elemType)

	var result value.Value

	switch s.Op {
	case "++":
		result = block.NewAdd(current, one)
	case "--":
		result = block.NewSub(current, one)
	default:
		result = current
	}

	block.NewStore(result, ptr)

	return nil
}

// genFoldedIf emits only the live branch of an if/elif/else chain whose
// initial cond was folded to known. The folded path is generated as a
// straight-line block; the dead branches are dropped entirely.
//
// When the initial condition is true, only the `then` block is emitted
// (subsequent elifs and else are dead).
//
// When the initial condition is false, the chain is "rotated": we step
// through elifs in declaration order, folding each. The first elif whose
// cond folds to true becomes the live branch; if any elif's cond is
// non-foldable we abandon folding and let the regular genIf path handle
// the remainder (rebuilding a fresh IfStmt with the unresolved tail).
// If every cond folds to false, we emit only the else branch (or
// fall-through when there isn't one).
