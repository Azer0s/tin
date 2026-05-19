package codegen

import (
	"fmt"
	"math/big"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) toBool(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		return constant.NewInt(irtypes.I1, 0)
	}

	t := val.Type()
	if t.Equal(irtypes.I1) {
		return val
	}

	if irtypes.IsInt(t) {
		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), t)

		return block.NewICmp(enum.IPredNE, val, zero)
	}

	if irtypes.IsFloat(t) {
		zero := constant.NewFloat(t.(*irtypes.FloatType), 0)

		return block.NewFCmp(enum.FPredONE, val, zero)
	}

	if irtypes.IsPointer(t) {
		null := constant.NewNull(t.(*irtypes.PointerType))

		return block.NewICmp(enum.IPredNE, val, null)
	}

	return constant.NewInt(irtypes.I1, 1)
}

// rejectStructAsBoolOperand fires at struct operands of `&&`, `||`,
// or `!` and rejects them with a diagnostic at e's position.  The
// boolean operators have their own op-trait family (`not[ret]` for
// `!`; `&&` / `||` are not currently overloadable but the policy is
// the same to leave room for that), so silently coercing a struct
// via `coerce[bool]` would mask user intent.  The user must write
// `<struct> as bool && other` to opt in.
func (cg *CodeGen) rejectStructAsBoolOperand(e ast.Node, t irtypes.Type) error {
	if !isStructType(t) {
		return nil
	}
	// Trait fat-pointers, fat-array pointers, strings, and other
	// "fat" pointer-shaped structs are runtime aggregates that
	// nonetheless carry a meaningful truthiness (nil / empty).
	// toBool already lowers them; only *user* structs need to opt
	// in via an explicit `as bool`.
	if isFatPtrType(t) || isFatArrayPtr(t) || isTraitFatPtrShape(t) {
		return nil
	}

	if isAnyType(t) {
		return nil
	}

	return cg.nodeErr(e,
		"cannot use %s as a boolean operand directly; "+
			"add `as bool` (requires the struct to implement `coerce[bool]`)",
		cg.tinTypeDisplay(t))
}

// toBoolImplicit lowers val to i1 in a *condition* context where Tin
// is allowed to call user-defined `coerce[bool]` automatically.  The
// canonical sites are `if val:`, `for val:`, ternary `val ? a : b`,
// and match / where guards.  Boolean-operator sites (`&&`, `||`, `!`)
// stay on toBool() because the user may have overloaded those traits
// on the struct -- silently coercing would mask the overload.
//
// When val is a struct whose decl includes `coerce[bool]`, the
// registered `static fn ::coerce(this S) bool` is called and its
// result feeds the conditional branch.  All other types fall through
// to the existing toBool primitive lowering.  Structs without a
// `coerce[bool]` impl reach the existing fallback (constant true)
// for now -- callers should report that statically as an error if
// they want strict coverage; the implicit hook never silently fires
// when a real coercion path is missing.
func (cg *CodeGen) toBoolImplicit(block *ir.Block, val value.Value) value.Value {
	if val == nil {
		return constant.NewInt(irtypes.I1, 0)
	}

	if structName := cg.typeNameOf(val.Type()); structName != "" {
		for _, entry := range cg.coerceConvFns[structName] {
			if entry.tgtLLVM.Equal(irtypes.I1) {
				return block.NewCall(entry.fn, val)
			}
		}
	}

	return cg.toBool(block, val)
}

// coerce converts a value to the target type, inserting casts as needed.
func (cg *CodeGen) coerce(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	if val == nil || target == nil {
		return val
	}

	src := val.Type()
	if src.Equal(target) {
		return val
	}

	// Tagged union: wrap a value into a tagged union type (type u = i8 | string).
	if targetSt, ok := target.(*irtypes.StructType); ok {
		if targetName := cg.typeNameOf(target); targetName != "" {
			if _, isUnion := cg.unionTypeMembers[targetName]; isUnion {
				if !src.Equal(target) {
					if wrapped := cg.wrapTaggedUnionVariant(block, val, targetSt, targetName); wrapped != nil {
						return wrapped
					}
				}
			}
		}
	}
	// Native union: store value into union storage (union u_named = ...).
	if targetSt, ok := target.(*irtypes.StructType); ok {
		if targetName := cg.typeNameOf(target); targetName != "" {
			if _, isNative := cg.nativeUnionDecls[targetName]; isNative {
				if !src.Equal(target) {
					return cg.wrapNativeUnion(block, val, targetSt)
				}
			}
		}
	}
	// Trait fat-pointer: coerce a concrete struct or `any` into the trait iface.
	if traitName, ok := cg.isTraitFatPtr(target); ok {
		if _, srcIsTrait := cg.isTraitFatPtr(src); !srcIsTrait {
			if isAnyType(src) {
				if result, err := cg.coerceAnyToTrait(block, val, traitName); err == nil {
					return result
				}
			} else {
				result, err := cg.coerceToTrait(block, val, traitName)
				if err == nil {
					return result
				}
			}
		}
	}

	// Pointer-to-trait fat-pointer: `let a *Fooable = &b` where b is a
	// struct that implements Fooable. Source is `*Struct`, target is
	// `*FatPtr`. Build a stack-temp fat ptr {data: &b, vtable: vtable},
	// return its address. The fat ptr borrows &b directly so methods
	// called via *a mutate b. Lifetime is caller-managed: b must
	// outlive the *Trait the same way any other `*T` borrow does.
	//
	// Without this path, llir auto-emits a wrong bitcast `*Box ->
	// *Fooable_iface` that reinterprets Box's first field (i32 type_id)
	// as the fat-ptr's i8* data field -- methods called via the bogus
	// fat ptr then dereference garbage and segfault.
	if tgtPt, ok := target.(*irtypes.PointerType); ok {
		if traitName, isTrait := cg.isTraitFatPtr(tgtPt.ElemType); isTrait {
			// nil literal -> *Trait: typed null pointer of the trait
			// fat-ptr's pointer type.  Lets `(t, *Trait) = (val, nil)`
			// and similar tuple/Result returns work without an explicit
			// cast, which is the natural "no error" idiom.
			if _, isNull := val.(*constant.Null); isNull {
				return constant.NewNull(tgtPt)
			}

			if srcPt, isPtr := src.(*irtypes.PointerType); isPtr {
				if srcInner, isStruct := srcPt.ElemType.(*irtypes.StructType); isStruct {
					// Identity widen: src is already `*<same iface>`.  Pre-fix
					// this still ran buildPtrToTraitBorrow, which allocated a
					// fresh iface block sharing the original's data ptr -- the
					// original's scope-exit release would then fire its
					// data-release thunk and free the data while the freshly-
					// allocated copy was being returned, leaving the caller
					// with a dangling iface.  Identity coerce must just
					// return val.
					if srcInner == tgtPt.ElemType {
						return val
					}
					// `*Concrete -> *Trait`: the inner fat-ptr's
					// `data` slot is always a pointer, so handing
					// it a `*Concrete` is the natural shape.
					// Value-form sources flow through
					// coerceToTrait's value-source path, not here.
					if result := cg.buildPtrToTraitBorrow(block, val, traitName, tgtPt.ElemType); result != nil {
						return result
					}
				}
			}
		}
	}

	// implicit[T] conversion: struct S implements implicit[T], call static fn.
	if targetName := cg.typeNameOf(target); targetName != "" {
		for _, entry := range cg.implicitConvFns[targetName] {
			if entry.srcLLVM.Equal(src) {
				return block.NewCall(entry.fn, val)
			}
		}
	}

	// coerce[T] op-trait: source struct S declared `coerce[T]` and provides
	// `static fn ::coerce(this S) T`.  Dispatched at every implicit-coercion
	// site (function args, let bindings, array elements, struct fields) so
	// `let v T = s` auto-fires the user's conversion without an `as T`.
	// genAsExpr already routes explicit `s as T` here too; this branch is
	// the implicit-coercion twin of that path.
	if structName := cg.typeNameOf(src); structName != "" {
		for _, entry := range cg.coerceConvFns[structName] {
			if entry.tgtLLVM.Equal(target) {
				return block.NewCall(entry.fn, val)
			}
		}
	}

	// Named function pointer -> fat-fn-ptr: wrap in a thin shim with (i8* env, params...).
	// This enables passing named functions (including extern) to higher-order functions.
	// For async fat-fn-ptrs (inner fn returns i8*), wrap the $coro variant instead.
	if isFatFnPtr(target) && !isFatFnPtr(src) {
		if _, ok := src.(*irtypes.PointerType); ok {
			if isAsyncFatFnPtr(target) {
				return cg.wrapAsyncFnAsFatPtr(block, val, target)
			}

			return cg.wrapFnAsFatPtr(block, val, target)
		}
	}

	// Fat-array coercion is deliberately narrow: only the untyped empty-array
	// literal ({i8*, i64} produced by `[]` with no known target element type)
	// is silently retyped to the target's element type. Any other cross-type
	// fat-array coercion is REJECTED here - callers must either pass the right
	// element type to begin with (see genArrayLitWithElemType plumbing in
	// genArgWithTargetType and call-site args), or write an explicit cast:
	//   let xs [i64] = [1, 2, 3]
	//   consume(xs as [i32])     // element-wise narrowing via genAsExpr
	// Silent implicit narrowing would hide precision-loss bugs (the original
	// motivation for removing the auto-convert path: it was converting
	// non-empty [i64] literals to zero-length [i32] without any user feedback).
	if isFatArrayPtr(src) && isFatArrayPtr(target) {
		srcPt := src.(*irtypes.StructType).Fields[0].(*irtypes.PointerType)
		tgtPt := target.(*irtypes.StructType).Fields[0].(*irtypes.PointerType)

		if srcPt.ElemType.Equal(tgtPt.ElemType) {
			return val // same element type: already correct
		}

		if srcPt.ElemType.Equal(irtypes.I8) && !tgtPt.ElemType.Equal(irtypes.I8) {
			// At the LLVM-type level a string and a `[i8]` slice are
			// indistinguishable -- both are `{i8*, i64}`.  The
			// previous "silently retype to the target" shortcut here
			// turned `let xs [i64] = "abc"` into a zero-length
			// `[i64]`, hiding the real type error.  The new policy:
			// only retype when the source is a syntactic empty-array
			// literal, which we recognize by a constant.Struct whose
			// data pointer is a null constant (genArrayLit emits
			// exactly that for `[]`).  Real string values reach the
			// caller's store-time type check unchanged so the user
			// gets a precise error.
			if cv, ok := val.(*constant.Struct); ok && len(cv.Fields) == 3 {
				if _, isNull := cv.Fields[0].(*constant.Null); isNull {
					return cg.zeroValue(target)
				}
			}

			return val
		}
		// Cross-type fat arrays (e.g. [i64] -> [i32]): leave val unchanged.
		// adaptArgs / call-site validation reports this as a compile error with
		// a hint about `x as [T]`.
		return val
	}

	// %__atom -> string fat-ptr or i8*: convert via __tin_atom_to_string.
	if isAtomType(src) {
		code := cg.extractAtomCode(block, val)

		strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
		if isFatPtrType(target) {
			return strFatPtr
		}

		if _, ok := target.(*irtypes.PointerType); ok {
			rawPtr := cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}

			return block.NewBitCast(rawPtr, target)
		}
	}

	// Fat-pointer (string / dynamic array) -> raw C pointer: extract
	// data ptr.  Allowed when:
	//   - the call site is an explicit `as` cast
	//   - the destination's element type is `i8` (the "raw bytes"
	//     pointer used for `*char` / `*byte` extern boundaries; keeps
	//     Tin strings flowing into extern C functions transparently)
	//   - the destination's element type matches the fat-array's own
	//     element type (e.g. `[i32] -> int32_t*`): this is exactly
	//     what every `extern("...") fn f(xs [T], n i64)` needs at the
	//     C boundary, so it's safe to do unconditionally; cross-elem
	//     casts (e.g. `[i64] -> i32*`) still require explicit `as`.
	//
	// Without this last branch the fat-array struct gets passed
	// inline to C, and AAPCS64 (Apple ARM / Linux ARM) lowers the
	// 24-byte composite to an INDIRECT pointer.  C then sees a
	// pointer-to-stack-copy where it expected a data pointer -- writes
	// land on the caller's stack copy of {ptr, len, cap}, never
	// reaching the actual array.  Reads happen to work by accident
	// (the first 8 bytes are the data pointer), which is why simple
	// "sum" tests pass but in-place mutation silently no-ops.
	if isFatPtrType(src) {
		if pt, ok := target.(*irtypes.PointerType); ok {
			srcEl := src.(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType
			tgtElIsI8 := pt.ElemType.Equal(irtypes.I8)
			tgtElMatchesSrcEl := pt.ElemType.Equal(srcEl)

			if cg.allowExplicitPtrCoerce || tgtElIsI8 || tgtElMatchesSrcEl {
				rawPtr := cg.extractFatPtrData(block, val, src.(*irtypes.StructType))
				if rawPtr.Type().Equal(target) {
					return rawPtr
				}

				return block.NewBitCast(rawPtr, target)
			}
		}
	}

	// Fixed-size array `[T; N]` -> raw C pointer (`T*`, `*i8` for
	// chars, or any *U via explicit cast): decay to the first-element
	// pointer.  Mirrors C's own array-to-pointer decay at function
	// boundaries.  Requires the value to be addressable; if `val` is
	// an rvalue we materialize it through a stack alloca first.
	if srcArr, ok := src.(*irtypes.ArrayType); ok {
		if pt, ok2 := target.(*irtypes.PointerType); ok2 {
			tgtElIsI8 := pt.ElemType.Equal(irtypes.I8)
			tgtElMatches := pt.ElemType.Equal(srcArr.ElemType)

			if cg.allowExplicitPtrCoerce || tgtElIsI8 || tgtElMatches {
				slot := cg.hoistAlloca(block, srcArr)
				block.NewStore(val, slot)

				gep := block.NewGetElementPtr(srcArr, slot,
					constant.NewInt(irtypes.I32, 0),
					constant.NewInt(irtypes.I32, 0))

				if gep.Type().Equal(target) {
					return gep
				}

				return block.NewBitCast(gep, target)
			}
		}
	}

	switch {
	// Any type: box the value.
	case isAnyType(target) && !isAnyType(src):
		return cg.boxToAny(block, val)

	// Int -> Int: extend or truncate.
	case irtypes.IsInt(src) && irtypes.IsInt(target):
		sBits := src.(*irtypes.IntType).BitSize

		tBits := target.(*irtypes.IntType).BitSize
		if sBits < tBits {
			return block.NewSExt(val, target)
		} else if sBits > tBits {
			return block.NewTrunc(val, target)
		}

		return val

	// Float -> Float.
	case irtypes.IsFloat(src) && irtypes.IsFloat(target):
		sBits := floatBits(src.(*irtypes.FloatType))

		tBits := floatBits(target.(*irtypes.FloatType))
		if sBits < tBits {
			return block.NewFPExt(val, target)
		} else if sBits > tBits {
			return block.NewFPTrunc(val, target)
		}

		return val

	// Int -> Float.
	case irtypes.IsInt(src) && irtypes.IsFloat(target):
		return block.NewSIToFP(val, target)

	// Float -> Int.
	case irtypes.IsFloat(src) && irtypes.IsInt(target):
		return block.NewFPToSI(val, target)

	// Pointer -> Pointer.  Same-elem identity is always implicit.
	// `*i8` (Tin's untyped pointer; what `nil` and string data point
	// at) widens to any typed pointer because it is structurally a
	// "raw byte" alias and shows up in legitimate places (typed null
	// literals, string-buffer escapes for C interop arguments).
	// Cross-elem casts between two *non-i8* typed pointers (e.g.
	// `*Foo` -> `*Bar`) are reserved for explicit `as`; the implicit
	// path keeps them apart so two structurally unrelated types
	// cannot pun by accident in a let-binding or arg pass.
	case irtypes.IsPointer(src) && irtypes.IsPointer(target):
		srcEl := src.(*irtypes.PointerType).ElemType

		tgtEl := target.(*irtypes.PointerType).ElemType
		if srcEl.Equal(tgtEl) {
			return val
		}

		if srcEl.Equal(irtypes.I8) || tgtEl.Equal(irtypes.I8) {
			return block.NewBitCast(val, target)
		}

		if cg.allowExplicitPtrCoerce {
			return block.NewBitCast(val, target)
		}
	// Int -> Pointer and Pointer -> Int are likewise gated on the
	// explicit-cast flag.  They remain available via `as *T` / `as
	// i64` (raw-address punning, mostly inside `{#unsafe}` blocks),
	// but a stray `let p *i64 = 5` reaches the binding's type check
	// instead of producing a bogus pointer.
	case irtypes.IsInt(src) && irtypes.IsPointer(target):
		if cg.allowExplicitPtrCoerce {
			return block.NewIntToPtr(val, target)
		}
	case irtypes.IsPointer(src) && irtypes.IsInt(target):
		if cg.allowExplicitPtrCoerce {
			return block.NewPtrToInt(val, target)
		}
	}

	// Unbox any to a scalar (int, float), struct, or string fat-ptr.
	// Extract the data pointer from the any fat-ptr and load the value.
	if isAnyType(src) && (irtypes.IsInt(target) || irtypes.IsFloat(target) || isStructType(target) || isStringType(target) || isVectorType(target)) {
		anyType := anyFatPtrType()
		anyAlloca := block.NewAlloca(anyType)
		block.NewStore(val, anyAlloca)
		ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)
		typedPtr := block.NewBitCast(dataPtr, irtypes.NewPointer(target))

		return block.NewLoad(target, typedPtr)
	}

	// Pointer-to-struct -> struct value: load the pointed-to value.
	// This handles value-receiver methods called on a pointer (e.g. p.method()
	// where method takes 'this T' but p is '*T').
	//
	// Refuse for trait fat-pointer targets: silently loading a `*Trait`
	// into a `Trait` value-form drops the outer iface block's rc=1 on
	// the floor (the block was heap-alloc'd by coerceToTrait /
	// buildPtrToTraitBorrow and nobody else owns it) but transfers the
	// inner data ptr into the loaded value -- on the next ARC release
	// site the heap block leaks and the inner data races against the
	// loaded copy.  Require an explicit `*expr` so the user routes
	// through genDerefExpr, which handles the temp-vs-binding ARC
	// transfer correctly.
	if pt, ok := src.(*irtypes.PointerType); ok {
		if pt.ElemType.Equal(target) {
			if targetSt, isStruct := target.(*irtypes.StructType); isStruct && isTraitFatPtrShape(targetSt) {
				cg.coerceLastErr = fmt.Errorf(
					"cannot implicitly deref `%s` to its value form `%s`: the outer iface block owns rc=1 and would leak.  Write `*expr` explicitly so the compiler can move ownership out of the temporary, or change the receiving type to `%s` (pointer form)",
					cg.tinTypeDisplay(src), cg.tinTypeDisplay(target), cg.tinTypeDisplay(src))

				return val
			}

			return block.NewLoad(target, val)
		}
	}

	// Last resort: bitcast if same size.

	return val
}

// convertFatArray converts a {T1*, i64} fat-array to a {T2*, i64}.
//
//   - Same-size element types: reinterpret the data pointer, keep the length.
//     No copy. Covers different signedness (i32 <-> u32), pointer-type changes.
//   - Integer elements of different size: delegates to the _tin_slice_convert_int
//     runtime helper which allocates a fresh buffer and truncates/sign-extends
//     element-wise. Keeping the loop in the runtime avoids introducing control
//     flow inside `coerce`, which would break callers that use the static
//     `block` parameter to continue emitting after the coerce returns.
//   - Anything else (float<->int, struct repacking): returns val unchanged,
//     which will fail LLVM verification at the call and surface loudly.
func (cg *CodeGen) convertFatArray(block *ir.Block, val value.Value, srcSt, tgtSt *irtypes.StructType) value.Value {
	srcPt := srcSt.Fields[0].(*irtypes.PointerType)
	tgtPt := tgtSt.Fields[0].(*irtypes.PointerType)
	srcElem := srcPt.ElemType
	tgtElem := tgtPt.ElemType

	srcSz := llvmTypeSize(srcElem)
	tgtSz := llvmTypeSize(tgtElem)

	// Extract ptr/len directly from the source fat-pointer struct.
	srcLen := block.NewExtractValue(val, 1)
	srcData := block.NewExtractValue(val, 0)

	if srcSz == tgtSz {
		// Same-width reinterpret: bitcast the data pointer, keep len.
		// Cap == len (we treat the result as owned, fresh-shaped).
		newData := block.NewBitCast(srcData, tgtPt)

		return cg.buildFatArrayValue(block, tgtElem, newData, srcLen, srcLen)
	}

	if !irtypes.IsInt(srcElem) || !irtypes.IsInt(tgtElem) {
		return val
	}

	// Build a 3-field raw slice {i8*, i64 len, i64 cap} matching the
	// runtime TinSlice layout, then call `_tin_slice_convert_int`.
	rawSlice := fatArrayPtrType(irtypes.I8)
	srcDataI8 := block.NewBitCast(srcData, irtypes.I8Ptr)
	rawVal := cg.buildFatArrayValue(block, irtypes.I8, srcDataI8, srcLen, srcLen)

	srcSigned := int64(1)
	if isUnsignedIntLLVMType(srcElem) {
		srcSigned = 0
	}

	convResult := cg.callExtern(block, cg.ensureSliceConvertInt(), rawVal,
		constant.NewInt(irtypes.I64, int64(srcSz)),
		constant.NewInt(irtypes.I64, int64(tgtSz)),
		constant.NewInt(irtypes.I32, srcSigned))

	// Reinterpret the raw {i8*, i64, i64} result as {T2*, i64, i64}.
	resAlloca := block.NewAlloca(rawSlice)
	block.NewStore(convResult, resAlloca)
	castPtr := block.NewBitCast(resAlloca, irtypes.NewPointer(tgtSt))

	return block.NewLoad(tgtSt, castPtr)
}

// isUnsignedIntLLVMType returns true for integer types the codegen prefers
// to treat as unsigned when widening (e.g. u8/char/byte for the runtime's
// signedness flag in slice conversion).
func isUnsignedIntLLVMType(t irtypes.Type) bool {
	// llir/ir doesn't track signedness on IntType, so infer from bit width:
	// we conservatively treat i8 as unsigned (char/byte/u8 all lower to i8)
	// and rely on Tin's narrowing rules on the source side. The impact is
	// only on sign/zero extension when widening; for truncation and same-width
	// conversions there is no difference.
	if it, ok := t.(*irtypes.IntType); ok && it.BitSize == 8 {
		return true
	}

	return false
}

// coerceAnyToTrait constructs a trait fat-pointer {i8* data, vtable*} from an
// `any` value, selecting the correct vtable at runtime via the any's type_id.
// The select chain iterates all structs that implement the trait; the data
// pointer is extracted directly from the any's heap block so mutations through
// the fat-pointer persist (supporting pointer-receiver trait methods).
func (cg *CodeGen) coerceAnyToTrait(block *ir.Block, anyVal value.Value, instKey string) (value.Value, error) {
	fatPtrType := cg.ifaceFor(CanonKey(instKey))
	if fatPtrType == nil {
		return nil, fmt.Errorf("coerceAnyToTrait: no fat-ptr type for trait %s", instKey)
	}

	vtableSt := cg.vtableFor(CanonKey(instKey))
	if vtableSt == nil {
		return nil, fmt.Errorf("coerceAnyToTrait: no vtable struct type for trait %s", instKey)
	}

	vtablePtrType := irtypes.NewPointer(vtableSt)

	// Extract type_id from the any value.
	typeIDVal := cg.extractAnyTypeID(block, anyVal)

	// Extract the raw i8* data pointer from the any value.
	anyType := anyFatPtrType()
	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, anyAlloca)
	ptrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataPtr := block.NewLoad(irtypes.I8Ptr, ptrGep)

	// Build select chain: type_id -> correct vtable pointer.
	var vtableResult value.Value = constant.NewNull(vtablePtrType)

	for _, st0 := range cg.sortedStructTypeIDs() {
		sn := st0.name
		typeID := st0.id
		vtableKey := sn + "__" + instKey

		vg, hasVtable := cg.traitVtableGlobals[vtableKey]
		if !hasVtable {
			continue
		}

		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))
		vtableResult = block.NewSelect(isMatch, vg, vtableResult)
	}

	// Construct the trait fat-pointer {i8* data, vtable*}.
	ifaceAlloca := block.NewAlloca(fatPtrType)
	dataGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, dataGep)
	vtableGep := block.NewGetElementPtr(fatPtrType, ifaceAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(vtableResult, vtableGep)

	return block.NewLoad(fatPtrType, ifaceAlloca), nil
}

// constCoerce coerces a compile-time constant to the target type without a
// block (used for const preregistration). Handles int/float narrowing/widening.
func (cg *CodeGen) constCoerce(v value.Value, target irtypes.Type) value.Value {
	if v == nil || target == nil || v.Type().Equal(target) {
		return v
	}

	c, ok := v.(constant.Constant)
	if !ok {
		return v
	}

	src := v.Type()
	switch {
	case irtypes.IsInt(src) && irtypes.IsInt(target):
		if ci, ok2 := c.(*constant.Int); ok2 {
			return constant.NewInt(target.(*irtypes.IntType), ci.X.Int64())
		}
	case irtypes.IsFloat(src) && irtypes.IsFloat(target):
		if cf, ok2 := c.(*constant.Float); ok2 {
			ft := target.(*irtypes.FloatType)
			fv, _ := cf.X.Float64()

			// FP128 / Half: llir emits the hex literal with the two
			// 64-bit halves in the WRONG order versus what LLVM expects
			// (high-first vs low-first), producing wildly wrong values.
			// Return nil so the caller falls back to runtime init via
			// fpext from a Double constant.
			if ft.Kind == irtypes.FloatKindFP128 || ft.Kind == irtypes.FloatKindHalf {
				return nil
			}

			f := constant.NewFloat(ft, fv)

			// Re-snap the big.Float to the target kind's precision so
			// the emitter writes a literal LLVM accepts for the target
			// type. Without this, narrowing to `float` hits the
			// non-exact path in llir and produces a hex literal whose
			// trailing bits clang rejects.
			f.X = new(big.Float).SetPrec(uint(floatPrec(ft.Kind))).SetFloat64(fv)

			// For single-precision specifically, round through float32
			// so the value is guaranteed bit-exactly representable.
			if ft.Kind == irtypes.FloatKindFloat {
				f.X = new(big.Float).SetPrec(24).SetFloat64(float64(float32(fv)))
			}

			return f
		}

		return c
	case irtypes.IsInt(src) && irtypes.IsFloat(target):
		if ci, ok2 := c.(*constant.Int); ok2 {
			return constant.NewFloat(target.(*irtypes.FloatType), float64(ci.X.Int64()))
		}
	case irtypes.IsFloat(src) && irtypes.IsInt(target):
		if cf, ok2 := c.(*constant.Float); ok2 {
			fv, _ := cf.X.Float64()

			return constant.NewInt(target.(*irtypes.IntType), int64(fv))
		}
	}

	return v
}

// checkConstantCompatible returns an error if a constant LLVM value cannot be
// safely coerced to targetType.  Specifically it rejects:
//   - A negative integer literal coercing to an unsigned integer type.
//   - An integer literal that exceeds the maximum value for the target type.
//
// Float truncation (f64 -> f32) is always allowed; precision loss is acceptable.
func checkConstantCompatible(c constant.Constant, targetType irtypes.Type) error {
	intConst, ok := c.(*constant.Int)
	if !ok {
		return nil // floats and other constants are fine
	}

	targetInt, ok2 := targetType.(*irtypes.IntType)
	if !ok2 {
		return nil // not an integer target
	}

	bits := int(targetInt.BitSize)
	val := intConst.X // *big.Int

	// Negative literal -> unsigned type.
	// In Tin, all unsigned widths are tracked as signed bit patterns in i8/i16/i32/i64.
	// We detect "intended unsigned" by checking whether the source constant came from
	// a clearly signed context.  For now we simply reject negative values coercing
	// into any sub-64-bit integer (u8/u16/u32) where the result would truncate sign.
	if val.Sign() < 0 && bits < 64 {
		return fmt.Errorf("constant %s cannot be coerced to %d-bit integer: negative value would lose sign", val.String(), bits)
	}

	// Integer literal overflow check (positive values only).
	if val.Sign() >= 0 && bits < 64 {
		maxVal := (int64(1) << bits) - 1
		if val.IsInt64() && val.Int64() > maxVal {
			return fmt.Errorf("constant %s overflows %d-bit integer type", val.String(), bits)
		}
	}

	return nil
}

func floatBits(t *irtypes.FloatType) int {
	switch t.Kind { //nolint:exhaustive // X86_FP80/PPC_FP128 are not used by tin
	case irtypes.FloatKindHalf:
		return 16
	case irtypes.FloatKindFloat:
		return 32
	case irtypes.FloatKindDouble:
		return 64
	case irtypes.FloatKindFP128:
		return 128
	default:
		return 64
	}
}

// floatPrec returns the IEEE 754 mantissa precision for a float kind, in
// the format big.Float expects via SetPrec (significand bits including
// the implicit leading 1).
func floatPrec(k irtypes.FloatKind) int {
	switch k { //nolint:exhaustive // X86_FP80/PPC_FP128 are not used by tin
	case irtypes.FloatKindHalf:
		return 11
	case irtypes.FloatKindFloat:
		return 24
	case irtypes.FloatKindDouble:
		return 53
	case irtypes.FloatKindFP128:
		return 113
	default:
		return 53
	}
}

// zeroValue returns the zero constant for a given type.
func (cg *CodeGen) zeroValue(t irtypes.Type) value.Value {
	switch {
	case irtypes.IsInt(t):
		return constant.NewInt(t.(*irtypes.IntType), 0)
	case irtypes.IsFloat(t):
		return constant.NewFloat(t.(*irtypes.FloatType), 0)
	case irtypes.IsPointer(t):
		return constant.NewNull(t.(*irtypes.PointerType))
	case irtypes.IsStruct(t):
		st := t.(*irtypes.StructType)
		// Opaque (forward-declared) struct: NewStruct(st) would emit `{}`
		// (an empty struct literal), which clang rejects when st actually
		// has fields elsewhere. zeroinitializer is type-shape agnostic and
		// expands lazily to whatever shape st settles into.
		if len(st.Fields) == 0 {
			return constant.NewZeroInitializer(st)
		}

		fields := make([]constant.Constant, len(st.Fields))
		for i, f := range st.Fields {
			fields[i] = cg.zeroValue(f).(constant.Constant)
		}

		return constant.NewStruct(st, fields...)
	case irtypes.IsArray(t):
		return constant.NewZeroInitializer(t)
	}

	return constant.NewInt(irtypes.I64, 0)
}

// isUnsignedTinType returns true when a Tin TypeExpr is one of the unsigned
// integer types: u8, u16, u32, u64 (and their aliases char/byte/uint/size_t).
// byteArrayElemType returns the element type name when t is a [byte], [u8], or
// [char] array type, and "" otherwise.  Used by genEcho to select per-element
// printf format: "byte" -> %02x, "u8" -> %u, "char" -> %c.
func byteArrayElemType(t ast.TypeExpr) string {
	at, ok := t.(*ast.ArrayType)
	if !ok {
		return ""
	}

	st, ok2 := at.Elem.(*ast.SimpleType)
	if !ok2 {
		return ""
	}

	switch st.Name {
	case "byte", "u8", "char":
		return st.Name
	}

	return ""
}

// scalar8BitTypeName returns the Tin type name for 8-bit scalar types:
// "char", "byte", "u8", or "i8".  Returns "" for all other types.
// Used to dispatch printf format in interpolation/echo: char->%c, byte->%x, u8/%u/i8->%d.
func scalar8BitTypeName(t ast.TypeExpr) string {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return ""
	}

	switch st.Name {
	case "char", "byte", "u8", "i8":
		return st.Name
	}

	return ""
}

// scalar128BitTypeName returns "i128", "u128", or "f128" when t is one of those
// types. Returns "" for all other types. Used by echo/interpolation dispatch.
func scalar128BitTypeName(t ast.TypeExpr) string {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return ""
	}

	switch st.Name {
	case "i128", "u128", "f128":
		return st.Name
	}

	return ""
}

func isUnsignedTinType(t ast.TypeExpr) bool {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return false
	}

	switch st.Name {
	case "u8", "char", "byte", "u16", "u32", "uint32", "u64", "uint", "size_t", "u128":
		return true
	}

	return false
}
