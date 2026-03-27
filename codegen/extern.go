package codegen

// extern.go - helpers for mapping Tin types to C-compatible LLVM types,
// declaring extern C functions, and wrapping/unwrapping fat-pointer arguments.

import (
	"github.com/Azer0s/tin/ast"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// tinTypeToExternLLVM returns the C-compatible LLVM type for a Tin type.
// Fat-pointer types (string, atom, dynamic arrays) are unwrapped to their
// underlying raw pointer when used as parameters.
// When forReturn is true (return-type context), dynamic arrays keep their
// full fat-ptr type so the C function can return the struct directly.
func (cg *CodeGen) tinTypeToExternLLVM(te ast.TypeExpr, forReturn bool) (irtypes.Type, error) {
	if te == nil {
		return irtypes.Void, nil
	}
	// string / atom -> i8*
	if st, ok := te.(*ast.SimpleType); ok {
		if st.Name == "string" || st.Name == "atom" {
			return irtypes.I8Ptr, nil
		}
	}
	// []T (dynamic array):
	//   - as parameter: unwrap to *T (C receives the data pointer)
	//   - as return type: keep the full fat-ptr {*T, i64} so C returns the struct
	if at, ok := te.(*ast.ArrayType); ok && at.Size < 0 {
		if forReturn {
			return cg.tinTypeToLLVM(te)
		}
		elem, err := cg.tinTypeToLLVM(at.Elem)
		if err != nil {
			return nil, err
		}
		return irtypes.NewPointer(elem), nil
	}
	return cg.tinTypeToLLVM(te)
}

// ensureExternDecl returns (or creates) a bare LLVM function declaration for a
// C extern symbol. Re-uses an existing declaration if one with a matching
// signature already exists.
func (cg *CodeGen) ensureExternDecl(cName string, retType irtypes.Type, params []*ir.Param, variadic bool) *ir.Func {
	for _, f := range cg.mod.Funcs {
		if f.Name() == cName {
			return f
		}
	}
	f := cg.mod.NewFunc(cName, retType, params...)
	f.Sig.Variadic = variadic
	f.Blocks = nil
	// Track that this IR name is a C extern symbol so that Tin user functions
	// with the same name can be mangled to avoid redefinition conflicts.
	if cg.externIRNames == nil {
		cg.externIRNames = map[string]bool{}
	}
	cg.externIRNames[cName] = true
	return f
}

// ensureStrlenDecl lazily creates the bare `declare i64 @strlen(i8*)` for use
// inside wrapFromExtern when a C function returns a char* that we wrap into a
// Tin string fat-pointer.
func (cg *CodeGen) ensureStrlenDecl() *ir.Func {
	return cg.ensureExternDecl("strlen", irtypes.I64,
		[]*ir.Param{ir.NewParam("s", irtypes.I8Ptr)}, false)
}

// unwrapForExtern extracts a raw C value from a Tin fat-pointer or atom.
// If val is already the target type it is returned unchanged.
func (cg *CodeGen) unwrapForExtern(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	src := val.Type()
	if src.Equal(target) {
		return val
	}
	// %__atom -> i8*: call __tin_atom_to_string then extract the data pointer.
	if isAtomType(src) {
		if _, ok := target.(*irtypes.PointerType); ok {
			code := cg.extractAtomCode(block, val)
			strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
			rawPtr := cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}
			return block.NewBitCast(rawPtr, target)
		}
	}
	// {ptr, i64} fat-pointer -> extract field 0 (the raw pointer)
	if isFatPtrType(src) {
		if _, ok := target.(*irtypes.PointerType); ok {
			rawPtr := cg.extractFatPtrData(block, val, src.(*irtypes.StructType))
			if rawPtr.Type().Equal(target) {
				return rawPtr
			}
			return block.NewBitCast(rawPtr, target)
		}
	}
	return val
}

// extractFatPtrData extracts field 0 (the raw data pointer) from a fat-pointer
// struct value. Works whether the value is a struct value or a pointer to one.
func (cg *CodeGen) extractFatPtrData(block *ir.Block, val value.Value, st *irtypes.StructType) value.Value {
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)
	gep := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return block.NewLoad(st.Fields[0], gep)
}

// wrapFromExtern wraps a raw C return value into a Tin fat-pointer or atom.
// For char* -> string, it calls strlen to obtain the length.
// For char* -> atom, it calls __tin_string_to_atom.
func (cg *CodeGen) wrapFromExtern(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	src := val.Type()
	if src.Equal(target) {
		return val
	}
	// i8* -> %__atom: find atom in table via strcmp.
	if _, ok := src.(*irtypes.PointerType); ok {
		if isAtomType(target) {
			return block.NewCall(cg.ensureStringToAtom(), val)
		}
	}
	// raw pointer -> fat-pointer: build {ptr, len}
	if _, ok := src.(*irtypes.PointerType); ok {
		if tgtSt, ok2 := target.(*irtypes.StructType); ok2 && isFatPtrType(target) {
			// Coerce pointer to the type expected by field 0
			var ptr value.Value
			if src.Equal(tgtSt.Fields[0]) {
				ptr = val
			} else {
				ptr = block.NewBitCast(val, tgtSt.Fields[0])
			}
			// Use strlen to get the length (treat as a null-terminated string)
			strlenFn := cg.ensureStrlenDecl()
			rawI8Ptr := ptr
			if !src.Equal(irtypes.I8Ptr) {
				rawI8Ptr = block.NewBitCast(val, irtypes.I8Ptr)
			}
			length := block.NewCall(strlenFn, rawI8Ptr)
			// Build the fat-pointer struct
			alloca := block.NewAlloca(tgtSt)
			gep0 := block.NewGetElementPtr(tgtSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(ptr, gep0)
			gep1 := block.NewGetElementPtr(tgtSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(length, gep1)
			return block.NewLoad(tgtSt, alloca)
		}
	}
	return val
}
