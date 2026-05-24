package codegen

// DWARF debug info emission for tin build -g.
//
// Uses the llir/llvm pure-Go metadata API (direct struct instantiation,
// no DIBuilder).  The coro split step (clang -O1) automatically re-scopes
// DILocalVariable nodes from the pre-split $coro function to the generated
// .resume subprogram and converts dbg.declare on frame-spilled allocas to
// #dbg_value with DW_OP_plus_uconst + DW_OP_deref expressions - no extra
// work needed here (Option A).
//
// Key rule: every metadata node that is referenced from IR (via !dbg, from
// NamedMetadataDefs, or as a field of another node) must appear in
// m.MetadataDefs so that AssignMetadataIDs() can assign a unique numeric ID
// to it and include it in the output.  Nodes must be created with
// MetadataID: -1 so they are treated as "unassigned" by AssignMetadataIDs.

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	"github.com/llir/llvm/ir/metadata"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// regMD adds a metadata definition to m.MetadataDefs so it gets a unique ID.
func (cg *CodeGen) regMD(md metadata.Definition) {
	cg.mod.MetadataDefs = append(cg.mod.MetadataDefs, md)
}

// initDebugInfo creates the module-level debug metadata: DICompileUnit, DIFile,
// and the !llvm.dbg.cu / !llvm.module.flags named metadata nodes.
// Must be called once after the module is created when debugMode is true.
func (cg *CodeGen) initDebugInfo() {
	absFile, err := filepath.Abs(cg.filename)
	if err != nil {
		absFile = cg.filename
	}

	dir := filepath.Dir(absFile)
	base := filepath.Base(absFile)

	diFile := &metadata.DIFile{MetadataID: -1, Filename: base, Directory: dir}
	cg.diFiles[cg.filename] = diFile
	cg.regMD(diFile)

	diCU := &metadata.DICompileUnit{
		MetadataID:   -1,
		Distinct:     true,
		Language:     enum.DwarfLangC99,
		File:         diFile,
		Producer:     "tin",
		IsOptimized:  false,
		EmissionKind: enum.EmissionKindFullDebug,
	}
	cg.diCU = diCU
	cg.regMD(diCU)

	if cg.mod.NamedMetadataDefs == nil {
		cg.mod.NamedMetadataDefs = make(map[string]*metadata.NamedDef)
	}

	cg.mod.NamedMetadataDefs["llvm.dbg.cu"] = &metadata.NamedDef{
		Name:  "llvm.dbg.cu",
		Nodes: []metadata.Node{diCU},
	}

	// Module flags: use DWARF 4 (not 5) to avoid issues with LLVM's coro split
	// pass creating DILabel nodes that require a `line` field in DWARF 5 mode.
	flagsTuple1 := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{
		constant.NewInt(irtypes.I32, 7),
		&metadata.String{Value: "Dwarf Version"},
		constant.NewInt(irtypes.I32, 4),
	}}
	flagsTuple2 := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{
		constant.NewInt(irtypes.I32, 2),
		&metadata.String{Value: "Debug Info Version"},
		constant.NewInt(irtypes.I32, 3),
	}}

	cg.regMD(flagsTuple1)
	cg.regMD(flagsTuple2)

	cg.mod.NamedMetadataDefs["llvm.module.flags"] = &metadata.NamedDef{
		Name:  "llvm.module.flags",
		Nodes: []metadata.Node{flagsTuple1, flagsTuple2},
	}
}

// diFileFor returns the DIFile for the given source file path, creating and
// caching it if not already present.
func (cg *CodeGen) diFileFor(filename string) *metadata.DIFile {
	if f, ok := cg.diFiles[filename]; ok {
		return f
	}

	absFile, err := filepath.Abs(filename)
	if err != nil {
		absFile = filename
	}

	diFile := &metadata.DIFile{
		MetadataID: -1,
		Filename:   filepath.Base(absFile),
		Directory:  filepath.Dir(absFile),
	}

	cg.diFiles[filename] = diFile
	cg.regMD(diFile)

	return diFile
}

// typeExprName extracts a canonical debug-type-name string from a TypeExpr.
// The result is used as the key for diTypeFor.
func typeExprName(te ast.TypeExpr) string {
	if te == nil {
		return "i64"
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.PointerType:
		return "*" + typeExprName(t.Elem)
	case *ast.GenericType:
		// e.g. List[i64] -> "List__i64"
		key := t.Name
		for _, p := range t.TypeParams {
			key += "__" + typeExprName(p)
		}

		return key
	case *ast.ArrayType:
		if t.Size < 0 {
			return "[" + typeExprName(t.Elem) + "]"
		}

		return fmt.Sprintf("[%s;%d]", typeExprName(t.Elem), t.Size)
	default:
		return "i64"
	}
}

// llvmTypeAlignBytes returns the ABI alignment (in bytes) for an LLVM type,
// using standard x86_64/ARM64 alignment rules.
func llvmTypeAlignBytes(t irtypes.Type) int {
	switch v := t.(type) {
	case *irtypes.IntType:
		n := int((v.BitSize + 7) / 8)
		switch {
		case n <= 1:
			return 1
		case n <= 2:
			return 2
		case n <= 4:
			return 4
		default:
			return 8
		}
	case *irtypes.FloatType:
		switch v.Kind {
		case irtypes.FloatKindHalf:
			return 2
		case irtypes.FloatKindFloat:
			return 4
		case irtypes.FloatKindDouble:
			return 8
		case irtypes.FloatKindFP128, irtypes.FloatKindPPC_FP128:
			return 16
		case irtypes.FloatKindX86_FP80:
			return 16
		}
	case *irtypes.PointerType:
		return 8
	case *irtypes.StructType:
		return llvmStructAlignBytes(v)
	case *irtypes.ArrayType:
		return llvmTypeAlignBytes(v.ElemType)
	default:
		return 8
	}

	return 8
}

func llvmStructAlignBytes(st *irtypes.StructType) int {
	align := 1

	for _, f := range st.Fields {
		if a := llvmTypeAlignBytes(f); a > align {
			align = a
		}
	}

	return align
}

// llvmTypeSizeBytes returns the size (in bytes) of an LLVM type including any
// internal padding that the layout algorithm inserts for nested structs.
func llvmTypeSizeBytes(t irtypes.Type) int {
	switch v := t.(type) {
	case *irtypes.IntType:
		return int((v.BitSize + 7) / 8)
	case *irtypes.FloatType:
		switch v.Kind {
		case irtypes.FloatKindHalf:
			return 2
		case irtypes.FloatKindFloat:
			return 4
		case irtypes.FloatKindDouble:
			return 8
		case irtypes.FloatKindFP128, irtypes.FloatKindPPC_FP128:
			return 16
		case irtypes.FloatKindX86_FP80:
			return 10 // 80-bit stored in 10 bytes
		}
	case *irtypes.PointerType:
		return 8
	case *irtypes.StructType:
		offset := 0

		for _, f := range v.Fields {
			a := llvmTypeAlignBytes(f)
			offset = (offset + a - 1) &^ (a - 1)
			offset += llvmTypeSizeBytes(f)
		}

		if a := llvmStructAlignBytes(v); a > 1 {
			offset = (offset + a - 1) &^ (a - 1)
		}

		return offset
	case *irtypes.ArrayType:
		return int(v.Len) * llvmTypeSizeBytes(v.ElemType)
	default:
		return 8
	}

	return 8
}

// fieldBitOffset returns the bit offset of field at index fieldIdx in st,
// using standard LLVM data layout alignment rules.
func fieldBitOffset(st *irtypes.StructType, fieldIdx int) uint64 {
	offset := 0

	for i, f := range st.Fields {
		a := llvmTypeAlignBytes(f)
		offset = (offset + a - 1) &^ (a - 1)

		if i == fieldIdx {
			return uint64(offset * 8)
		}

		offset += llvmTypeSizeBytes(f)
	}

	return 0
}

// llvmTypeTotalBits returns the total size of an LLVM type in bits.
func llvmTypeTotalBits(t irtypes.Type) uint64 {
	return uint64(llvmTypeSizeBytes(t) * 8)
}

// diTypeFromLLVM derives a DWARF type from an LLVM type by reverse-mapping
// through the struct registry and well-known fat-pointer layouts.
// Used when the Tin source type annotation is absent (inferred variables).
func (cg *CodeGen) diTypeFromLLVM(t irtypes.Type) metadata.Field {
	if t == nil {
		return cg.diTypeFor("i64")
	}

	switch v := t.(type) {
	case *irtypes.IntType:
		switch v.BitSize {
		case 1:
			return cg.diTypeFor("bool")
		case 8:
			return cg.diTypeFor("i8")
		case 16:
			return cg.diTypeFor("i16")
		case 32:
			return cg.diTypeFor("i32")
		case 64:
			return cg.diTypeFor("i64")
		}
	case *irtypes.FloatType:
		switch v.Kind {
		case irtypes.FloatKindFloat:
			return cg.diTypeFor("f32")
		case irtypes.FloatKindDouble:
			return cg.diTypeFor("f64")
		case irtypes.FloatKindHalf, irtypes.FloatKindFP128, irtypes.FloatKindX86_FP80, irtypes.FloatKindPPC_FP128:
			return cg.diTypeFor("f64")
		}
	case *irtypes.PointerType:
		return cg.diTypeFor("*u8")
	case *irtypes.StructType:
		// Check for string fat pointer: {i8*, i64 len, i64 cap}
		if len(v.Fields) == 3 {
			if pt, ok := v.Fields[0].(*irtypes.PointerType); ok {
				if it, ok2 := pt.ElemType.(*irtypes.IntType); ok2 && it.BitSize == 8 {
					if it2, ok3 := v.Fields[1].(*irtypes.IntType); ok3 && it2.BitSize == 64 {
						if it3, ok4 := v.Fields[2].(*irtypes.IntType); ok4 && it3.BitSize == 64 {
							return cg.diTypeFor("string")
						}
					}
				}
			}
		}
		// Search struct registry for a matching type pointer.
		for canon, r := range cg.types {
			if r.LLVM == v {
				return cg.diTypeFor(string(canon))
			}
		}
	}

	return cg.diTypeFor("i64")
}

// diTypeFor maps a Tin type name (e.g. "i64", "bool", "*i64", "string",
// "[i64]", "[i64;4]", "point") to a DWARF metadata type node.
// Results are cached in cg.diTypeCache.
func (cg *CodeGen) diTypeFor(tinTypeName string) metadata.Field {
	if t, ok := cg.diTypeCache[tinTypeName]; ok {
		return t
	}

	var t metadata.Field

	switch tinTypeName {
	case "i8":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "i8", Size: 8, Encoding: enum.DwarfAttEncodingSigned}
		cg.regMD(bt)
		t = bt
	case "i16":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "i16", Size: 16, Encoding: enum.DwarfAttEncodingSigned}
		cg.regMD(bt)
		t = bt
	case "i32":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "i32", Size: 32, Encoding: enum.DwarfAttEncodingSigned}
		cg.regMD(bt)
		t = bt
	case "i64":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "i64", Size: 64, Encoding: enum.DwarfAttEncodingSigned}
		cg.regMD(bt)
		t = bt
	case "u8", "byte", "char":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: tinTypeName, Size: 8, Encoding: enum.DwarfAttEncodingUnsigned}
		cg.regMD(bt)
		t = bt
	case "u16":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "u16", Size: 16, Encoding: enum.DwarfAttEncodingUnsigned}
		cg.regMD(bt)
		t = bt
	case "u32", "rune":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: tinTypeName, Size: 32, Encoding: enum.DwarfAttEncodingUnsigned}
		cg.regMD(bt)
		t = bt
	case "u64":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "u64", Size: 64, Encoding: enum.DwarfAttEncodingUnsigned}
		cg.regMD(bt)
		t = bt
	case "f32":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "f32", Size: 32, Encoding: enum.DwarfAttEncodingFloat}
		cg.regMD(bt)
		t = bt
	case "f64":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "f64", Size: 64, Encoding: enum.DwarfAttEncodingFloat}
		cg.regMD(bt)
		t = bt
	case "bool":
		bt := &metadata.DIBasicType{MetadataID: -1, Name: "bool", Size: 8, Encoding: enum.DwarfAttEncodingBoolean}
		cg.regMD(bt)
		t = bt
	case "string":
		// string is a {i8* data, i64 len} fat pointer - expose both fields.
		t = cg.diStringType()
	default:
		switch {
		case strings.HasPrefix(tinTypeName, "*"):
			inner := cg.diTypeFor(tinTypeName[1:])
			dt := &metadata.DIDerivedType{
				MetadataID: -1,
				Tag:        enum.DwarfTagPointerType,
				BaseType:   inner,
				Size:       64,
			}
			cg.regMD(dt)
			t = dt

		case strings.HasPrefix(tinTypeName, "["):
			t = cg.diArrayOrSliceType(tinTypeName)

		default:
			// Try struct registry (normalize :: -> __ for package-qualified names).
			structKey := strings.ReplaceAll(tinTypeName, "::", "__")
			if st := cg.structTypeFor(CanonKey(structKey)); st != nil {
				t = cg.diStructTypeFromRegistry(structKey, st)
			} else {
				// Unknown type: opaque i64-sized scalar so the variable is at
				// least visible in the debugger with a reasonable size.
				bt := &metadata.DIBasicType{MetadataID: -1, Name: tinTypeName, Size: 64, Encoding: enum.DwarfAttEncodingSigned}
				cg.regMD(bt)
				t = bt
			}
		}
	}

	cg.diTypeCache[tinTypeName] = t

	return t
}

// diTypeForExpr returns the DWARF type for a Tin AST TypeExpr.
func (cg *CodeGen) diTypeForExpr(te ast.TypeExpr) metadata.Field {
	if te == nil {
		return cg.diTypeFor("i64")
	}

	return cg.diTypeFor(typeExprName(te))
}

// diStringType builds the DICompositeType for the built-in string type:
//
//	struct string { i8* data; i64 len; }
func (cg *CodeGen) diStringType() *metadata.DICompositeType {
	u8Type := cg.diTypeFor("u8")
	dataPtr := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagPointerType, BaseType: u8Type, Size: 64}
	cg.regMD(dataPtr)

	dataMember := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagMember, Name: "data", BaseType: dataPtr, Size: 64, Offset: 0}
	cg.regMD(dataMember)

	lenMember := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagMember, Name: "len", BaseType: cg.diTypeFor("i64"), Size: 64, Offset: 64}
	cg.regMD(lenMember)

	elems := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{dataMember, lenMember}}
	cg.regMD(elems)

	ct := &metadata.DICompositeType{
		MetadataID: -1,
		Tag:        enum.DwarfTagStructureType,
		Name:       "string",
		Size:       128,
		Elements:   elems,
	}
	cg.regMD(ct)

	return ct
}

// diArrayOrSliceType builds the DWARF type for "[T]" (slice) or "[T;N]" (fixed array).
func (cg *CodeGen) diArrayOrSliceType(name string) metadata.Field {
	// Parse the name: "[T]" or "[T;N]"
	inner := name[1 : len(name)-1] // strip outer [ ]

	if idx := strings.Index(inner, ";"); idx >= 0 {
		// Fixed array: [T;N]
		elemName := strings.TrimSpace(inner[:idx])

		var count int64

		_, _ = fmt.Sscanf(strings.TrimSpace(inner[idx+1:]), "%d", &count)

		elemType := cg.diTypeFor(elemName)
		// Compute element size in bits.
		var elemBits uint64
		if bt, ok := elemType.(*metadata.DIBasicType); ok {
			elemBits = bt.Size
		} else {
			elemBits = 64
		}

		subrange := &metadata.DISubrange{MetadataID: -1, Count: metadata.IntLit(count)}
		cg.regMD(subrange)

		rangeTuple := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{subrange}}
		cg.regMD(rangeTuple)

		ct := &metadata.DICompositeType{
			MetadataID: -1,
			Tag:        enum.DwarfTagArrayType,
			BaseType:   elemType,
			Size:       uint64(count) * elemBits,
			Elements:   rangeTuple,
		}
		cg.regMD(ct)

		return ct
	}

	// Dynamic slice: [T] - represented as {T*, i64}
	elemName := inner
	elemType := cg.diTypeFor(elemName)

	dataPtr := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagPointerType, BaseType: elemType, Size: 64}
	cg.regMD(dataPtr)

	dataMember := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagMember, Name: "data", BaseType: dataPtr, Size: 64, Offset: 0}
	cg.regMD(dataMember)

	lenMember := &metadata.DIDerivedType{MetadataID: -1, Tag: enum.DwarfTagMember, Name: "len", BaseType: cg.diTypeFor("i64"), Size: 64, Offset: 64}
	cg.regMD(lenMember)

	elems := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{dataMember, lenMember}}
	cg.regMD(elems)

	ct := &metadata.DICompositeType{
		MetadataID: -1,
		Tag:        enum.DwarfTagStructureType,
		Name:       name,
		Size:       128,
		Elements:   elems,
	}
	cg.regMD(ct)

	return ct
}

// diStructTypeFromRegistry builds a DICompositeType for a Tin struct, with
// one DIMember per user field at the correct byte offset.
func (cg *CodeGen) diStructTypeFromRegistry(structKey string, st *irtypes.StructType) *metadata.DICompositeType {
	// Insert a shell into the cache before recursing into field types so that
	// self-referential pointer fields (e.g. next *Node) do not loop.
	shell := &metadata.DICompositeType{
		MetadataID: -1,
		Tag:        enum.DwarfTagStructureType,
		Name:       strings.ReplaceAll(structKey, "__", "::"),
		Size:       llvmTypeTotalBits(st),
	}
	cg.regMD(shell)
	cg.diTypeCache[structKey] = shell
	// Also cache under the :: form so both lookup paths hit the same node.
	displayName := strings.ReplaceAll(structKey, "__", "::")
	if displayName != structKey {
		cg.diTypeCache[displayName] = shell
	}

	fieldNames := cg.structFields[structKey]
	fieldTinTypes := cg.structFieldTinTypes[structKey]
	ufo := cg.userFieldOffset(structKey)

	var members []metadata.Field

	for i, fieldName := range fieldNames {
		llvmIdx := ufo + i
		offsetBits := fieldBitOffset(st, llvmIdx)

		var fieldDIType metadata.Field
		if i < len(fieldTinTypes) && fieldTinTypes[i] != nil {
			fieldDIType = cg.diTypeForExpr(fieldTinTypes[i])
		} else if llvmIdx < len(st.Fields) {
			fieldDIType = cg.diTypeFromLLVM(st.Fields[llvmIdx])
		} else {
			fieldDIType = cg.diTypeFor("i64")
		}

		var fieldSizeBits uint64
		if llvmIdx < len(st.Fields) {
			fieldSizeBits = llvmTypeTotalBits(st.Fields[llvmIdx])
		} else {
			fieldSizeBits = 64
		}

		member := &metadata.DIDerivedType{
			MetadataID: -1,
			Tag:        enum.DwarfTagMember,
			Name:       fieldName,
			BaseType:   fieldDIType,
			Size:       fieldSizeBits,
			Offset:     offsetBits,
		}
		cg.regMD(member)
		members = append(members, member)
	}

	if len(members) > 0 {
		elemsTuple := &metadata.Tuple{MetadataID: -1, Fields: members}
		cg.regMD(elemsTuple)
		shell.Elements = elemsTuple
	}

	return shell
}

// ensureDbgDeclareFn lazily declares the llvm.dbg.declare intrinsic.
func (cg *CodeGen) ensureDbgDeclareFn() *ir.Func {
	if cg.dbgDeclareFn != nil {
		return cg.dbgDeclareFn
	}

	metaTy := irtypes.Metadata
	f := cg.mod.NewFunc("llvm.dbg.declare", irtypes.Void,
		ir.NewParam("", metaTy),
		ir.NewParam("", metaTy),
		ir.NewParam("", metaTy),
	)
	cg.dbgDeclareFn = f

	return f
}

// emitDbgSubprogram creates a DISubprogram for function fn from AST node n,
// attaches it to the IR function, and sets cg.diCurrentScope.
// Returns the DISubprogram so the caller can restore the previous scope.
func (cg *CodeGen) emitDbgSubprogram(n *ast.FuncDecl, f *ir.Func, filename string) *metadata.DISubprogram {
	if !cg.debugMode || cg.diCU == nil {
		return nil
	}

	diFile := cg.diFileFor(filename)
	line := int64(n.Pos().Line)

	// Build subroutine type: {retType, param0, param1, ...}
	// First element is return type (null for void).
	typeFields := make([]metadata.Field, 0, len(n.Params)+1)
	if n.RetType != nil {
		typeFields = append(typeFields, cg.diTypeForExpr(n.RetType))
	} else {
		typeFields = append(typeFields, metadata.Null)
	}

	for _, p := range n.Params {
		if !p.IsVarArgs {
			typeFields = append(typeFields, cg.diTypeForExpr(p.Type))
		}
	}

	typesTuple := &metadata.Tuple{MetadataID: -1, Fields: typeFields}

	cg.regMD(typesTuple)

	subroutineType := &metadata.DISubroutineType{
		MetadataID: -1,
		Types:      typesTuple,
	}
	cg.regMD(subroutineType)

	// Use the user-visible function name (strip package prefixes).
	displayName := strings.ReplaceAll(n.Name, "__", "::")
	linkageName := f.Name()

	subprog := &metadata.DISubprogram{
		MetadataID:   -1,
		Distinct:     true,
		Name:         displayName,
		LinkageName:  linkageName,
		Scope:        diFile,
		File:         diFile,
		Line:         line,
		Type:         subroutineType,
		IsDefinition: true,
		ScopeLine:    line,
		Flags:        enum.DIFlagPrototyped,
		SPFlags:      enum.DISPFlagDefinition,
		Unit:         cg.diCU,
	}
	cg.regMD(subprog)

	f.Metadata = append(f.Metadata, &metadata.Attachment{Name: "dbg", Node: subprog})
	cg.diCurrentScope = subprog

	return subprog
}

// emitDbgSubprogramForSynthetic creates a DISubprogram for a compiler-
// generated function (e.g. the implicit `main` wrapping top-level statements,
// or the test runner). Unlike emitDbgSubprogram it does not require an
// ast.FuncDecl; the caller supplies the user-visible name and source line.
func (cg *CodeGen) emitDbgSubprogramForSynthetic(f *ir.Func, name string, line int) *metadata.DISubprogram {
	if !cg.debugMode || cg.diCU == nil {
		return nil
	}

	diFile := cg.diFileFor(cg.filename)

	typesTuple := &metadata.Tuple{MetadataID: -1, Fields: []metadata.Field{cg.diTypeFor("i32")}}
	cg.regMD(typesTuple)

	subroutineType := &metadata.DISubroutineType{MetadataID: -1, Types: typesTuple}
	cg.regMD(subroutineType)

	subprog := &metadata.DISubprogram{
		MetadataID:   -1,
		Distinct:     true,
		Name:         name,
		LinkageName:  f.Name(),
		Scope:        diFile,
		File:         diFile,
		Line:         int64(line),
		Type:         subroutineType,
		IsDefinition: true,
		ScopeLine:    int64(line),
		Flags:        enum.DIFlagPrototyped,
		SPFlags:      enum.DISPFlagDefinition,
		Unit:         cg.diCU,
	}
	cg.regMD(subprog)

	f.Metadata = append(f.Metadata, &metadata.Attachment{Name: "dbg", Node: subprog})
	cg.diCurrentScope = subprog

	return subprog
}

// emitDbgDeclare emits a call to llvm.dbg.declare for a local variable.
// alloca is the stack alloca for the variable.
// name is the variable name, line is the source line, argNo is the 1-based
// parameter index (0 for local variables).
// tinType is the declared Tin type expression (may be nil for inferred types).
// llFallback is the resolved LLVM type used when tinType is nil.
func (cg *CodeGen) emitDbgDeclare(block *ir.Block, alloca *ir.InstAlloca, name string, line int, argNo uint64, tinType ast.TypeExpr, llFallback irtypes.Type) {
	if !cg.debugMode || cg.diCU == nil || cg.diCurrentScope == nil || block == nil || alloca == nil {
		return
	}

	var diType metadata.Field
	if tinType != nil {
		diType = cg.diTypeForExpr(tinType)
	} else {
		diType = cg.diTypeFromLLVM(llFallback)
	}

	diFile := cg.diFileFor(cg.filename)
	diVar := &metadata.DILocalVariable{
		MetadataID: -1,
		Name:       name,
		Scope:      cg.diCurrentScope,
		File:       diFile,
		Line:       int64(line),
		Type:       diType,
		Arg:        argNo,
	}
	cg.regMD(diVar)

	diExpr := &metadata.DIExpression{MetadataID: -1}
	cg.regMD(diExpr)

	dbgDeclareFn := cg.ensureDbgDeclareFn()
	call := block.NewCall(dbgDeclareFn,
		&metadata.Value{Value: alloca},
		&metadata.Value{Value: diVar},
		&metadata.Value{Value: diExpr},
	)
	// Mark this call with line=0 so the debugger doesn't stop on it.
	cg.attachDbgLoc(call, 0, 0)
}

// pushLexicalBlock creates a DILexicalBlock child of the current scope and
// sets it as the active scope. Returns a restore function that must be called
// (typically via defer) when the block exits.
func (cg *CodeGen) pushLexicalBlock(line int) func() {
	if !cg.debugMode || cg.diCurrentScope == nil {
		return func() {}
	}

	prev := cg.diCurrentScope
	lb := &metadata.DILexicalBlock{
		MetadataID: -1,
		Distinct:   true,
		Scope:      cg.diCurrentScope,
		File:       cg.diFileFor(cg.filename),
		Line:       int64(line),
	}
	cg.regMD(lb)
	cg.diCurrentScope = lb

	return func() { cg.diCurrentScope = prev }
}

// attachDbgLoc attaches a DILocation !dbg metadata to an instruction.
// line/col 0 means "compiler-generated, don't stop here". Idempotency
// for !dbg specifically is enforced inside attachMetadataToInst, so the
// broadened "attach to every new instruction" loop in genStmt
// (stmts.go) and explicit per-call attaches (e.g. genBuiltinStacktrace)
// coexist without producing the LLVM-verifier-rejected "multiple !dbg
// attachments per instruction" error.
func (cg *CodeGen) attachDbgLoc(inst ir.Instruction, line, col int64) {
	if !cg.debugMode || cg.diCurrentScope == nil {
		return
	}

	diLoc := &metadata.DILocation{
		MetadataID: -1,
		Line:       line,
		Column:     col,
		Scope:      cg.diCurrentScope,
	}

	cg.regMD(diLoc)

	attachMetadataToInst(inst, &metadata.Attachment{Name: "dbg", Node: diLoc})
}

// attachCurrentDbgLoc attaches the current source position as a !dbg location
// to inst (when -g is active) AND records the position in cg.instLineCol
// (when pclntab is in use). The two paths are independent: pclntab's
// per-fn PC tables are built from instLineCol; DWARF sections are emitted
// from the !dbg attachments. Release builds with stacktrace get the side
// map only - no DWARF.
func (cg *CodeGen) attachCurrentDbgLoc(inst ir.Instruction) {
	if inst == nil {
		return
	}

	if cg.pclntabUsed && cg.currentPos.Line != 0 {
		if cg.instLineCol == nil {
			cg.instLineCol = map[ir.Instruction]ast.Pos{}
		}

		cg.instLineCol[inst] = cg.currentPos
	}

	if !cg.debugMode || cg.diCurrentScope == nil {
		return
	}

	cg.attachDbgLoc(inst, int64(cg.currentPos.Line), int64(cg.currentPos.Col))
}

// attachCurrentDbgLocToTerm attaches the current source position as a !dbg
// location to a terminator instruction (ret, br, condbr, etc.).
func (cg *CodeGen) attachCurrentDbgLocToTerm(term ir.Terminator) {
	if term == nil {
		return
	}

	// Terminators don't go into the per-fn PC table (the table is keyed
	// by basic-block start; the terminator is the BB's last
	// instruction). So no instLineCol entry here - just the !dbg
	// attachment for -g consumers.

	if !cg.debugMode || cg.diCurrentScope == nil {
		return
	}

	diLoc := &metadata.DILocation{
		MetadataID: -1,
		Line:       int64(cg.currentPos.Line),
		Column:     int64(cg.currentPos.Col),
		Scope:      cg.diCurrentScope,
	}
	cg.regMD(diLoc)

	att := &metadata.Attachment{Name: "dbg", Node: diLoc}

	switch t := term.(type) {
	case *ir.TermRet:
		t.Metadata = append(t.Metadata, att)
	case *ir.TermCondBr:
		t.Metadata = append(t.Metadata, att)
	case *ir.TermBr:
		t.Metadata = append(t.Metadata, att)
	}
}

// attachForLoopDbg attaches the for statement's source line to:
//   - brToCondTerm: the branch terminator that enters the condition block
//   - condBlock: the first instruction and the terminator of the condition block
//
// This ensures the debugger visits the for-loop line when (re-)evaluating the
// condition, both on first entry and on the back-edge from the loop body.
func (cg *CodeGen) attachForLoopDbg(pos ast.Pos, brToCondTerm ir.Terminator, condBlock *ir.Block) {
	if !cg.debugMode || cg.diCurrentScope == nil {
		return
	}

	line := int64(pos.Line)
	col := int64(pos.Col)

	diLoc := &metadata.DILocation{MetadataID: -1, Line: line, Column: col, Scope: cg.diCurrentScope}
	cg.regMD(diLoc)
	att := &metadata.Attachment{Name: "dbg", Node: diLoc}

	switch t := brToCondTerm.(type) {
	case *ir.TermBr:
		t.Metadata = append(t.Metadata, att)
	case *ir.TermCondBr:
		t.Metadata = append(t.Metadata, att)
	}

	if condBlock == nil {
		return
	}

	if len(condBlock.Insts) > 0 {
		attachMetadataToInst(condBlock.Insts[0], att)
	}

	if condBlock.Term != nil {
		switch t := condBlock.Term.(type) {
		case *ir.TermBr:
			t.Metadata = append(t.Metadata, att)
		case *ir.TermCondBr:
			t.Metadata = append(t.Metadata, att)
		}
	}
}

// ensureAllCallsHaveDbg walks all instructions in fn and attaches a line=0
// !dbg location to any call instruction that is missing one.  LLVM requires
// that every call instruction in a function that has a !dbg attachment on the
// function itself must also have a !dbg on the call (otherwise it emits
// "inlinable function call in a function with debug info must have a !dbg").
func (cg *CodeGen) ensureAllCallsHaveDbg(fn *ir.Func) {
	if !cg.debugMode || cg.diCurrentScope == nil {
		return
	}

	zeroLoc := &metadata.DILocation{MetadataID: -1, Line: 0, Scope: cg.diCurrentScope}

	cg.regMD(zeroLoc)

	att := &metadata.Attachment{Name: "dbg", Node: zeroLoc}

	for _, block := range fn.Blocks {
		for _, inst := range block.Insts {
			call, ok := inst.(*ir.InstCall)
			if !ok {
				continue
			}

			hasDbg := false

			for _, m := range call.Metadata {
				if m.Name == "dbg" {
					hasDbg = true

					break
				}
			}

			if !hasDbg {
				call.Metadata = append(call.Metadata, att)
			}
		}
	}
}

// instDbgLineCol returns the (line, col) of the !dbg DILocation attached
// to inst, plus ok=true. Returns (0, 0, false) when no dbg attachment
// exists or the attachment node isn't a DILocation. Used by pclntab.go to
// build a per-call source-position table without having to thread per-
// instruction line/col tracking through every codegen path.
//
// Walks the Metadata slice via reflection to dodge the 55-case type
// switch - every concrete llir/llvm instruction type embeds Metadata as
// a field literally named "Metadata".
func instDbgLineCol(inst ir.Instruction) (int, int, bool) {
	if inst == nil {
		return 0, 0, false
	}

	v := reflect.ValueOf(inst)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return 0, 0, false
	}

	field := v.FieldByName("Metadata")
	if !field.IsValid() || field.Kind() != reflect.Slice {
		return 0, 0, false
	}

	for i := 0; i < field.Len(); i++ {
		att, ok := field.Index(i).Interface().(*metadata.Attachment)
		if !ok || att == nil || att.Name != "dbg" {
			continue
		}

		loc, ok := att.Node.(*metadata.DILocation)
		if !ok || loc == nil {
			continue
		}

		// Skip line=0 entries - those are codegen's
		// "compiler-generated" placeholder for instructions emitted
		// outside genStmt (ARC sequences, fiber preamble). Carrying
		// these into pclntab would render frames as "fn@file:0:0".
		if loc.Line == 0 {
			continue
		}

		return int(loc.Line), int(loc.Column), true
	}

	return 0, 0, false
}

// instHasMetadata reports whether inst already carries a metadata
// attachment with the given name. Reads the `Metadata` field via
// reflection so we don't have to mirror the 55-case type switch in
// attachMetadataToInst - every concrete llir/llvm instruction type
// embeds the attachment slice as a field literally named `Metadata`.
//
// Defensive against future llir/llvm changes that might rename or
// retype the field: any reflect.Kind mismatch returns false (treated
// as "no existing dbg") rather than panicking.
func instHasMetadata(inst ir.Instruction, name string) bool {
	if inst == nil {
		return false
	}

	v := reflect.ValueOf(inst)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return false
	}

	field := v.FieldByName("Metadata")
	if !field.IsValid() || field.Kind() != reflect.Slice {
		return false
	}

	for i := 0; i < field.Len(); i++ {
		att, ok := field.Index(i).Interface().(*metadata.Attachment)
		if ok && att != nil && att.Name == name {
			return true
		}
	}

	return false
}

// attachMetadataToInst attaches a metadata attachment to any concrete
// instruction type.  Because ir.Instruction is an interface and the
// embedded ir.Metadata value field is not accessible through the
// interface, a type switch over all concrete instruction types is
// required.
//
// `!dbg` attachments are deduped per LLVM LangRef ("Only one !dbg
// attachment is allowed per instruction"). Without this, the broadened
// genStmt attach-loop and explicit per-call attaches (e.g. in
// genBuiltinStacktrace) would race to attach two DILocations and
// produce verifier-rejected IR. The dedup is gated to `dbg` only; all
// other attachment kinds (tbaa, range, llvm.loop, etc.) still append.
func attachMetadataToInst(inst ir.Instruction, att *metadata.Attachment) {
	if inst == nil || att == nil {
		return
	}

	if att.Name == "dbg" && instHasMetadata(inst, "dbg") {
		return
	}

	switch v := inst.(type) {
	case *ir.InstAlloca:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstLoad:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstStore:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstCall:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstGetElementPtr:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstBitCast:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstICmp:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFCmp:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstAdd:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSub:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstMul:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSDiv:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstUDiv:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFAdd:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFSub:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFMul:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFDiv:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSRem:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstURem:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFRem:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstShl:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstLShr:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstAShr:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstAnd:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstOr:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstXor:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSExt:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstZExt:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstTrunc:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFPExt:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFPTrunc:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFPToSI:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFPToUI:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSIToFP:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstUIToFP:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstPtrToInt:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstIntToPtr:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstAddrSpaceCast:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstPhi:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstSelect:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstExtractValue:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstInsertValue:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFNeg:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstFreeze:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstAtomicRMW:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstShuffleVector:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstExtractElement:
		v.Metadata = append(v.Metadata, att)
	case *ir.InstInsertElement:
		v.Metadata = append(v.Metadata, att)
	}
}
