package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// isPtrArithBinExpr reports whether node is a `+` / `-` BinExpr that
// is plausibly pointer arithmetic.  We can't fully type-check here
// (the AsExpr emitter runs before the operand has been evaluated to
// know its LLVM type), so the heuristic peels through nested
// arithmetic and looks for the BinExpr shape.  False positives at
// most produce the funny warning on integer-only arithmetic, which
// is acceptable noise; false negatives would silently skip the warn.
func isPtrArithBinExpr(n ast.Node) bool {
	bin, ok := n.(*ast.BinExpr)
	if !ok || bin == nil {
		return false
	}

	if bin.Op != "+" && bin.Op != "-" {
		return false
	}
	// Direct BinExpr arithmetic: this is the candidate.  Whether
	// the operand actually has pointer type is decided by the
	// child BinExpr emitter; the warning fires when it would
	// have been ptr arith and the target is integer.
	return true
}

func (cg *CodeGen) genAsExpr(block *ir.Block, e *ast.AsExpr) (value.Value, error) {
	targetType, err := cg.tinTypeToLLVM(e.Type)
	if err != nil {
		return nil, err
	}

	// [byte; N] as [byte] or [byte; N] as string: heap-copy the fixed-size byte
	// array into a new ARC-managed fat slice / string.
	// Use genLValue to get the alloca pointer without loading the full array.
	if isFatArrayPtr(targetType) || isStringType(targetType) {
		if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
			if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
				if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 && at.ElemType.Equal(irtypes.I8) {
					n := constant.NewInt(irtypes.I64, int64(at.Len))
					elemPtr := block.NewGetElementPtr(at, arrPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, 0))
					srcPtr := block.NewBitCast(elemPtr, irtypes.I8Ptr)

					return cg.callExtern(block, cg.ensureBytesFromBuf(), srcPtr, n), nil
				}
			}
		}
	}

	// `(p + n) as i64`: permitted transient consumption with a
	// warning under -Wtransient-ptr-int-cast.  Stashing the address
	// as an integer is the one "escape" we tolerate from the
	// transient rule because the cast destination is obviously not a
	// pointer -- callers smuggle it through arithmetic, log it,
	// compare it to a fixed sentinel, etc.  Casting back is on the
	// user.  Other AsExpr targets (`as *Trait`, `as any`) stay
	// rejected because they keep pointer semantics and extend the
	// view's lifetime past the expression.
	if irtypes.IsInt(targetType) {
		// Buddy... I see what you're trying to do here and you're
		// fooling no one. But if you really try to shoot yourself in
		// the foot, I won't stop you.
		if isPtrArithBinExpr(e.Expr) {
			cg.warn("transient-ptr-int-cast", e.Pos(),
				"casting a pointer-arithmetic result to an integer "+
					"smuggles the address past the lifetime checker. "+
					"The integer outlives the view; casting back to a "+
					"pointer is unchecked and likely use-after-free.")
		}

		prevTransient := cg.transientPtrAllowed
		cg.transientPtrAllowed = true

		defer func() { cg.transientPtrAllowed = prevTransient }()
	}

	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// Trait-pointer downcast: `e as *Concrete` where e is *Trait_iface.
	// The iface struct's `data` field points at the heap-allocated
	// concrete struct that was widened into the trait at construction
	// time, so the cast loads `(*e).data` and bitcasts to *Concrete.
	// This is unchecked: if the concrete type does not match what the
	// iface was built from the result is a wild pointer.  Callers
	// should only use this when they know the dynamic type (e.g.
	// inside the matching Result::Err arm of a domain-specific
	// signature).  Returning nil when src is nil keeps the cast
	// safe for the explicit guard pattern `if e == nil: return ...`.
	//
	// Casting a *Trait to a non-pointer concrete type is rejected as a
	// compile error: it would silently load garbage from the iface's
	// data field as if it were a value, and there is no use case where
	// that is what the user means.  The intended forms are:
	//   - `e as *Concrete` (pointer downcast, this branch)
	//   - `(*e) as Concrete` (deref first, then value coerce)
	if srcPt, ok := val.Type().(*irtypes.PointerType); ok {
		if _, isTrait := cg.isTraitFatPtr(srcPt.ElemType); isTrait {
			if tgtPt, ok2 := targetType.(*irtypes.PointerType); ok2 {
				if _, isTraitTgt := cg.isTraitFatPtr(tgtPt.ElemType); !isTraitTgt {
					if _, isStruct := tgtPt.ElemType.(*irtypes.StructType); isStruct {
						ifaceStructTy := srcPt.ElemType.(*irtypes.StructType)
						dataGep := block.NewGetElementPtr(ifaceStructTy, val,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
						dataPtr := block.NewLoad(irtypes.I8Ptr, dataGep)

						return block.NewBitCast(dataPtr, targetType), nil
					}
				}
			} else {
				// *Trait -> non-pointer target: reject loud and clear.
				return nil, cg.nodeErr(e,
					"cannot cast a trait pointer to non-pointer type %s; "+
						"use `(*x) as %s` to deref first, or `x as *%s` to "+
						"keep the pointer form",
					cg.tinTypeDisplay(targetType),
					cg.tinTypeDisplay(targetType),
					cg.tinTypeDisplay(targetType))
			}
		}
	}

	// Symmetric: Trait (value iface) -> *Concrete (pointer struct) is also
	// a hard error.  A value trait carries no addressable storage to hand
	// out as a pointer, and the implicit-ptr-to-iface bitcast that llir
	// would otherwise emit produces a wild pointer that crashes the moment
	// the caller dereferences it.  Force the user to take an address
	// (`(&t) as *Concrete`) or restructure to deal in pointer traits from
	// the start.
	if _, isTrait := cg.isTraitFatPtr(val.Type()); isTrait {
		if tgtPt, ok := targetType.(*irtypes.PointerType); ok {
			if _, isTraitTgt := cg.isTraitFatPtr(tgtPt.ElemType); !isTraitTgt {
				if _, isStruct := tgtPt.ElemType.(*irtypes.StructType); isStruct {
					return nil, cg.nodeErr(e,
						"cannot cast a value-form trait to pointer type %s; "+
							"the iface holds the concrete by value with no "+
							"stable address.  Use `*Trait` from the start, "+
							"or `(&t) as %s` if you intend to take the "+
							"caller's stack address",
						cg.tinTypeDisplay(targetType),
						cg.tinTypeDisplay(targetType))
				}
			}
		}
	}

	// Fat-array to fat-array cast: explicit element-wise conversion via the
	// runtime helper. Only this path - `x as [i32]` - performs the conversion;
	// implicit coerce does not (silent narrowing was hiding bugs).
	if isFatArrayPtr(val.Type()) && isFatArrayPtr(targetType) {
		srcSt := val.Type().(*irtypes.StructType)
		tgtSt := targetType.(*irtypes.StructType)
		srcPt := srcSt.Fields[0].(*irtypes.PointerType)
		tgtPt := tgtSt.Fields[0].(*irtypes.PointerType)

		if srcPt.ElemType.Equal(tgtPt.ElemType) {
			return val, nil
		}
		// Empty-array literal shortcut: untyped {i8*, i64} -> typed.
		if srcPt.ElemType.Equal(irtypes.I8) && !tgtPt.ElemType.Equal(irtypes.I8) {
			return cg.zeroValue(targetType), nil
		}

		return cg.convertFatArray(block, val, srcSt, tgtSt), nil
	}

	// For unsigned source types, integer widening must use zext, not sext.
	// Determine signedness from the source expression's Tin type.
	if irtypes.IsInt(val.Type()) && irtypes.IsInt(targetType) {
		sBits := val.Type().(*irtypes.IntType).BitSize

		tBits := targetType.(*irtypes.IntType).BitSize

		// Truncation: warn if the source folds to a constant that doesn't
		// fit the destination type. Caller may have written `let x i32 = 1<<33`.
		// We pin the legal range by the DESTINATION signedness - `0xC3 as
		// byte` is a perfectly valid u8 (195) even though 0xC3 was lexed as
		// a signed i64 literal. Falling back to the source's signedness only
		// when the dest is itself a signed integer type.
		if sBits > tBits {
			isUnsigned := isUnsignedTinType(e.Type) || cg.exprElemIsUnsigned(e.Expr)
			cg.checkCastTruncatesConst(e, tBits, isUnsigned)
		}

		if sBits < tBits {
			// IntLit values are always non-negative as written in source code.
			// Large literals (e.g. 18446744073709551615) that exceed i64::MAX are
			// stored as their two's-complement i64 bit pattern (negative). Using
			// zext recovers the correct unsigned magnitude; sext would sign-extend
			// the raw i64 storage and produce a wrong i128/u128 value.
			_, isIntLit := e.Expr.(*ast.IntLit)
			srcUnsigned := isIntLit || cg.exprElemIsUnsigned(e.Expr)

			if srcUnsigned {
				return block.NewZExt(val, targetType), nil
			}

			return block.NewSExt(val, targetType), nil
		}
	}

	// User-defined `coerce[T]` op-trait: if the source is a struct
	// whose decl includes `coerce[T]` and a matching `static fn
	// ::coerce(this S) T`, dispatch the cast to that function before
	// trying built-in coercions.  Mirrors the existing implicit[T]
	// path in coerce() but going the other direction (struct -> T).
	if structName := cg.typeNameOf(val.Type()); structName != "" {
		for _, entry := range cg.coerceConvFns[structName] {
			if entry.tgtLLVM.Equal(targetType) {
				result := block.NewCall(entry.fn, val)
				// The coerce call returns an rc=1 value -- could be a
				// fat ptr (string / [T] / any / *Trait), or a struct
				// whose fields hold ARC references that genStructLit
				// retained at construction time.  When this AsExpr
				// appears as an argument to another call (e.g.
				// `assert::equals(m as string, ...)`), the parent's
				// emitCallArgRelease short-circuits because
				// isCopyExpr(AsExpr) walks through to the inner
				// identifier and returns true -- treating the cast
				// result as a borrow.  Register a synthetic scope
				// entry so emitAllScopeReleases drops the rc=1
				// reference at scope exit; otherwise it leaks.
				//
				// elemNeedsRelease is recursive (looks through struct
				// fields), so a `coerce` returning Wallet{amount: string}
				// is correctly tracked.  Pre-fix this checked
				// isRCTrackedType, which is false for any non-fat-ptr
				// struct -- silently leaking RC sub-fields of every
				// `m as Wallet` call.
				if cg.curScope != nil && cg.elemNeedsRelease(result.Type()) {
					alloca := block.NewAlloca(result.Type())
					block.NewStore(result, alloca)

					name := fmt.Sprintf(".coerce_tmp_%d", cg.strCount)
					cg.strCount++
					cg.curScope.set(name, &scopeEntry{
						val:     alloca,
						isAlloc: true,
						isRC:    true,
					})

					return block.NewLoad(result.Type(), alloca), nil
				}

				return result, nil
			}
		}
	}

	// Explicit `as` casts are allowed to do raw pointer / fat-ptr
	// punning that would be unsafe to do implicitly.  Toggle the
	// guard while coerce runs and reset after; the implicit
	// let-binding path leaves the flag false so a stray
	// `let p *i64 = 5` still rejects.
	prevExplicit := cg.allowExplicitPtrCoerce
	cg.allowExplicitPtrCoerce = true

	defer func() { cg.allowExplicitPtrCoerce = prevExplicit }()

	result := cg.coerce(block, val, targetType)
	// Coerce-to-any uses boxAsAny, which retains the source's inner
	// ARC ptr so the any data block "owns" one reference (released
	// when the block hits RC=0 in _tin_release_any).  For lvalue
	// sources the matching release comes from the source binding's
	// scope-exit; for rvalue sources there is no such binding, so we
	// emit a balancing release here to transfer the rvalue's RC into
	// the data block's ownership.  Without this, every `<heap> as any`
	// where the source is a transient (string interpolation, fn call
	// result, struct literal, etc.) would leak the inner content.
	if isAnyType(targetType) && !isAnyType(val.Type()) && !isCopyExpr(e.Expr) && isRCTrackedType(val.Type()) {
		cg.emitRelease(block, val)
	}
	// coerce() returns the input unchanged when it cannot find a
	// conversion path, so a result whose type still does not match the
	// requested target means the cast was impossible.  Up to here the
	// compiler used to silently propagate the mismatch and surface it
	// as the next assignment / return / call type error -- which buried
	// the actual problem (the cast itself) under a confusing message at
	// a downstream slot.  Reporting it at the cast site points the user
	// at the right line.
	if !result.Type().Equal(targetType) {
		// int->ptr casts upgrade the result to addrspace(1) /
		// volatile *T even when the user wrote `as *T` (no
		// `volatile` keyword): the integer source carries no
		// provenance, so any rc machinery acting on the result
		// would walk into unmapped memory at the next foreign-free.
		// Returning the volatile result here lets the binding's
		// inferred type carry the volatile, and downstream sinks
		// that demand a non-volatile slot trigger -Wvolatile-loss
		// at the coercion site instead of silently re-enabling rc.
		if ptrTypesAddrSpaceMismatchOnly(result.Type(), targetType) {
			return result, nil
		}

		gotName := cg.fmtArgType(result.Type())
		if gotName == "" || gotName == "<nil>" {
			gotName = cg.diagStructName(cg.typeNameOf(result.Type()))
		}

		wantName := cg.tinTypeDisplay(targetType)
		// Identity casts are not "impossible" -- the existing
		// useless-cast warning already covers them.  Anything else
		// genuinely has no conversion path.
		if !val.Type().Equal(targetType) {
			return nil, cg.nodeErr(e,
				"cannot cast %s to %s: no conversion path", gotName, wantName)
		}
	}
	// Release the `any` box after unboxing when it is a temporary (fresh allocation
	// from a call/getfield).  Identifiers and field accesses are copy-borrows that
	// are owned by their parent scope/struct and must NOT be released here.
	if isAnyType(val.Type()) && !isAnyType(targetType) && !isCopyExpr(e.Expr) {
		cg.emitRelease(block, val)
	}

	return result, nil
}
