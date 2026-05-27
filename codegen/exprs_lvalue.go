package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

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

				if st, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && len(st.Fields) == 3 {
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

		// Auto-deref: `p[i] = ...` where `p` is `*[T]` (pointer to a fat
		// array) or `*string` -- load through the pointer once so the
		// fat-array dispatch below sees the value, not the pointer.
		// Without this the *irtypes.PointerType arm at the bottom did
		// pointer arithmetic in FatArray-sized strides, yielding a
		// `*FatArray` lvalue that fails the type check at the assign
		// site ("cannot assign value of type i64 (declared type [i64])").
		// Mirrors the rvalue auto-deref in genIndexExpr.
		if pt, ok := arr.Type().(*irtypes.PointerType); ok {
			if isFatArrayPtr(pt.ElemType) || isStringType(pt.ElemType) {
				arr = block.NewLoad(pt.ElemType, arr)
			}
		}

		arrType := arr.Type()
		switch at := arrType.(type) {
		case *irtypes.StructType:
			// Fat pointer: either the 2-field legacy shape (kept for
			// constants in helper paths) or the canonical 3-field
			// `{T*, i64 len, i64 cap}` shape -- both index via field 0.
			if len(at.Fields) == 2 || len(at.Fields) == 3 {
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
		// Heap-promoted local escaping into a raw *T field: balance
		// the field-store retain that genStructLit unconditionally
		// emitted.  Layout of the leak this prevents:
		//
		//   fn make() *Box =
		//     let x = 100              // x heap-promoted, rc=1
		//     return &Box{p: &x}       // field-store retain: x.rc=2
		//
		// The local's scope-exit skips its release (isEarlyHeap), and
		// Box's per-struct release_ptr only walks the field once when
		// the heap Box is dropped, so the alloc-owner-rc strands at
		// rc=1.  Releasing the local here (only in the *heap-bound*
		// `&StructLit` path -- not in the by-value StructLit path,
		// which already balances via a temp-copy-plus-binding double-
		// release) brings the cycle back to zero.
		cg.releaseHeapPromotedLocalsInStructLit(block, e)

		return typedPtr, nil

	case *ast.CallExpr:
		// &Variant(args) where Variant is an ADT constructor: construct the
		// ADT value, heap-allocate an RC block, and return a typed pointer.
		// Same rules as &StructLit{}.
		if id, ok := e.Func.(*ast.Identifier); ok && cg.isDataVariant(id.Name) {
			val, err := cg.genDataConstructorCall(block, id.Name, e.Args)
			if err != nil {
				return nil, err
			}

			if val == nil {
				return nil, fmt.Errorf("&%s: could not resolve ADT variant", id.Name)
			}

			st, ok2 := val.Type().(*irtypes.StructType)
			if !ok2 {
				return nil, fmt.Errorf("&%s: ADT wrap did not produce a struct value", id.Name)
			}

			nullPtr := constant.NewNull(irtypes.NewPointer(st))
			gepOne := block.NewGetElementPtr(st, nullPtr, constant.NewInt(irtypes.I32, 1))
			sz := block.NewPtrToInt(gepOne, irtypes.I64)
			heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
			typedPtr := block.NewBitCast(heapI8, irtypes.NewPointer(st))
			block.NewStore(val, typedPtr)

			return typedPtr, nil
		}
		// &call(args) for arbitrary call expressions.  Evaluates the call,
		// heap-allocates an RC block sized for the return value, stores the
		// value into it, and returns the typed pointer.  Callers own the
		// resulting `*T` and are responsible for releasing it; the same rules
		// as `&StructLit{...}` apply.  Used for `&errors::new("...")`-style
		// expressions where the user wants a pointer to a freshly produced
		// value.
		val, err := cg.genExpr(block, e)
		if err != nil {
			return nil, err
		}

		if val == nil || irtypes.IsVoid(val.Type()) {
			return nil, fmt.Errorf("cannot take address of a void-returning call")
		}

		nullPtr := constant.NewNull(irtypes.NewPointer(val.Type()))
		gepOne := block.NewGetElementPtr(val.Type(), nullPtr, constant.NewInt(irtypes.I32, 1))
		sz := block.NewPtrToInt(gepOne, irtypes.I64)
		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		typedPtr := block.NewBitCast(heapI8, irtypes.NewPointer(val.Type()))
		block.NewStore(val, typedPtr)

		return typedPtr, nil
	}

	return nil, fmt.Errorf("not an lvalue: %T", node)
}

// callGenericFromMap looks up bareName in m (either genericFuncs or
// constrainedFuncs), evaluates args, infers type arguments, monomorphizes
// the template, and emits the call.  Returns (result, updatedBlock, found,
// error).  found is false when bareName is not in m.
