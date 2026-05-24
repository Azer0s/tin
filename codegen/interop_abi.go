package codegen

import (
	"fmt"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) classifyInteropParam(t ast.TypeExpr) string {
	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "string":
			return "string"
		case "bool":
			return "bool"
		}

		if cg.interopPackedStructs[st.Name] {
			return "packed"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	if _, ok := t.(*ast.FuncType); ok {
		return "callback"
	}

	return ""
}

// classifyInteropReturn returns the marshaling shape for a Tin return
// type. "void" / "string" / "slice" / "bool" / "packed" /
// "callback" (Tin closure -> mmap'd C trampoline) / "" (passthrough).
func (cg *CodeGen) classifyInteropReturn(t ast.TypeExpr) string {
	if t == nil {
		return "void"
	}

	if st, ok := t.(*ast.SimpleType); ok {
		switch st.Name {
		case "void":
			return "void"
		case "string":
			return "string"
		case "bool":
			return "bool"
		}

		if cg.interopPackedStructs[st.Name] {
			return "packed"
		}
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		return "slice"
	}

	if _, ok := t.(*ast.FuncType); ok {
		return "callback"
	}

	return ""
}

// packedABIShape categorizes a #packed Tin struct for the SysV
// x86_64 / AAPCS64 boundary:
//
//	"i8" / "i16" / "i32" / "i64": all fields naturally align AND total
//	                              size <= 8 - clang would coerce to
//	                              the matching integer register.
//	"two_i64":                    same but 9-16 bytes.
//	"byval":                      anything else (mixed alignment, or
//	                              size > 16). Passed via byval ptr,
//	                              returned via sret hidden ptr.
//
// Returns ("", err) if the struct is not eligible (e.g. has trait
// vtables - the wrapper would not know how to fill them).
func (cg *CodeGen) packedABIShape(structName string) (string, int, error) {
	if cg.vtableOffset(structName) != 0 {
		return "", 0, fmt.Errorf("packed struct %s implements traits; pass-by-value at the interop boundary requires no vtable pointers", structName)
	}

	userTypes := cg.structFieldLLVMTypes[structName]
	if userTypes == nil {
		return "", 0, fmt.Errorf("packed struct %s: layout not yet computed", structName)
	}

	size := 0
	for _, t := range userTypes {
		size += int(llvmTypeSize(t))
	}

	if size == 0 {
		return "", 0, fmt.Errorf("packed struct %s has zero size", structName)
	}
	// SSE-class single field: clang lowers a struct holding exactly
	// one f32/f64 to the bare float/double ABI (XMM register). Match
	// it so the cross-language signature lines up.
	if size <= 8 && naturallyAligned(userTypes) && len(userTypes) == 1 {
		if irtypes.IsFloat(userTypes[0]) {
			switch userTypes[0].(*irtypes.FloatType).Kind {
			case irtypes.FloatKindFloat:
				return "f32", size, nil
			case irtypes.FloatKindDouble:
				return "f64", size, nil
			case irtypes.FloatKindHalf,
				irtypes.FloatKindFP128,
				irtypes.FloatKindX86_FP80,
				irtypes.FloatKindPPC_FP128:
				// f16/f128/x86_fp80/ppc_fp128 don't have a SSE single-
				// register ABI fast-path here; fall through to the
				// generic integer-eightbyte handling below.
			}
		}
	}
	// Multi-float same-eightbyte (e.g. 2x f32) would need <2 x float>
	// vector ABI; not implemented in v1.
	if size <= 8 && naturallyAligned(userTypes) && allFloatFields(userTypes) && len(userTypes) > 1 {
		return "", size, fmt.Errorf("packed struct %s contains multiple float fields in one eightbyte; v1 does not implement the SSE vector ABI - pass *%s instead", structName, structName)
	}
	// Single-register integer coercion: works when the packed layout
	// matches the natural byte layout AND the struct fits in one
	// integer eightbyte. Both LLVM and clang lower it to the same
	// integer type so the cross-language ABI agrees.
	if size <= 8 && naturallyAligned(userTypes) {
		switch size {
		case 1:
			return "i8", size, nil
		case 2:
			return "i16", size, nil
		case 3, 4:
			return "i32", size, nil
		case 5, 6, 7, 8:
			return "i64", size, nil
		}
	}
	// Two-eightbyte coercion (9-16 byte naturally-aligned structs).
	// Each chunk gets its own LLVM type matching what clang would emit.
	// Multi-float-per-eightbyte (would need <2 x float> vector ABI) is
	// rejected here.
	if size > 8 && size <= 16 && naturallyAligned(userTypes) {
		_, _, err := classifyTwoEightbytes(userTypes, structName)
		if err != nil {
			return "", size, err
		}

		return "two_eightbyte", size, nil
	}
	// Non-natural alignment of any size: clang uses byval/sret.
	if !naturallyAligned(userTypes) {
		return "byval", size, nil
	}

	return "byval", size, nil
}

// classifyTwoEightbytes decides the LLVM type clang would assign to
// each of the two 8-byte chunks of a 9-16 byte naturally-aligned
// packed struct. For each chunk:
//   - sole float field -> that float type (SSE class)
//   - any mix or multi-integer -> integer of the chunk's used byte count
//   - multiple floats in one chunk -> reject (vector ABI not in v1)
//
// Returns (lo type, hi type, error).
func classifyTwoEightbytes(userTypes []irtypes.Type, structName string) (irtypes.Type, irtypes.Type, error) {
	type fieldRange struct {
		ty       irtypes.Type
		off, end int
	}

	off := 0
	fields := make([]fieldRange, len(userTypes))

	for i, t := range userTypes {
		sz := int(llvmTypeSize(t))
		fields[i] = fieldRange{ty: t, off: off, end: off + sz}
		off += sz
	}

	totalSize := off

	classify := func(lo, hi int) (irtypes.Type, error) {
		// Collect fields that overlap [lo, hi).
		var inChunk []fieldRange

		for _, f := range fields {
			if f.off < hi && f.end > lo {
				inChunk = append(inChunk, f)
			}
		}

		if len(inChunk) == 0 {
			return nil, nil
		}
		// Float-only chunks: clang uses float/double if a single
		// float field fills the chunk; otherwise vector (rejected).
		floatCount := 0

		for _, f := range inChunk {
			if irtypes.IsFloat(f.ty) {
				floatCount++
			}
		}

		if floatCount > 0 && floatCount == len(inChunk) {
			if len(inChunk) > 1 {
				return nil, fmt.Errorf("packed struct %s has multiple float fields in one eightbyte; v1 does not implement the SSE vector ABI - pass *%s instead", structName, structName)
			}

			return inChunk[0].ty, nil
		}
		// Mixed or all-integer: use iN where N = chunk's used bytes.
		usedBits := (hi - lo) * 8

		// For the trailing chunk, the actual data may not fill the
		// full eightbyte; clang emits the integer type matching the
		// real used size (e.g. i32 for the trailing 4 bytes of a
		// 12-byte struct).
		if hi > totalSize {
			usedBits = (totalSize - lo) * 8
		}

		switch usedBits {
		case 8:
			return irtypes.I8, nil
		case 16:
			return irtypes.I16, nil
		case 24, 32:
			return irtypes.I32, nil
		case 40, 48, 56, 64:
			return irtypes.I64, nil
		}

		return nil, fmt.Errorf("packed struct %s: unable to coerce eightbyte covering %d bits", structName, usedBits)
	}

	loTy, err := classify(0, 8)
	if err != nil {
		return nil, nil, err
	}

	hiTy, err := classify(8, 16)
	if err != nil {
		return nil, nil, err
	}

	return loTy, hiTy, nil
}

// allFloatFields reports whether every field is a float type.
func allFloatFields(types []irtypes.Type) bool {
	for _, t := range types {
		if !irtypes.IsFloat(t) {
			return false
		}
	}

	return true
}

// naturallyAligned reports whether a packed list of LLVM types would
// have the SAME byte layout under natural alignment - i.e. packed
// would not actually change anything. When true, clang treats the
// struct the same as its non-packed counterpart and chooses register
// passing for small sizes; when false, clang chooses byval.
func naturallyAligned(types []irtypes.Type) bool {
	off := uint64(0)

	for _, t := range types {
		sz, align := llvmTypeSizeAlign(t)
		if align == 0 {
			align = 1
		}

		if off%align != 0 {
			return false
		}

		off += sz
	}

	return true
}

// abiRegisterType returns the LLVM type matching one of the
// register-passed ABI shapes ("i8".."i64", "f32", "f64").
func abiRegisterType(shape string) irtypes.Type {
	switch shape {
	case "i8":
		return irtypes.I8
	case "i16":
		return irtypes.I16
	case "i32":
		return irtypes.I32
	case "i64":
		return irtypes.I64
	case "f32":
		return irtypes.Float
	case "f64":
		return irtypes.Double
	}

	return nil
}

// userFieldsByteOffset returns the byte offset where a struct's user
// fields start, accounting for the i32 type_id at index 0 and any
// vtable pointer slots. For #packed structs (no padding), this is the
// offset our register-coerced ABI shape stores into.
func (cg *CodeGen) userFieldsByteOffset(structName string) int {
	off := 4 // i32 type_id
	for range cg.structVtableOrder[structName] {
		off += 8 // each vtable pointer is 8 bytes on 64-bit targets
	}

	return off
}

// packedUserStructType returns a packed LLVM struct type containing
// just the user fields of a #packed Tin struct. Used as the byval/sret
// target type so the wrapper signature matches what clang would emit
// for the equivalent C `__attribute__((packed))` struct argument.
func (cg *CodeGen) packedUserStructType(structName string) *irtypes.StructType {
	st := irtypes.NewStruct(cg.structFieldLLVMTypes[structName]...)
	st.Packed = true

	return st
}

// emitMemcpy emits a `llvm.memcpy.p0i8.p0i8.i64(dst, src, n, false)`
