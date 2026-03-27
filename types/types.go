// Package types defines the tin language type system and provides utilities
// for converting AST type expressions into resolved runtime types that the
// LLVM code generator can consume
package types

import (
	"fmt"
	"strings"
)

// Kind classifies a tin type
type Kind int

const (
	KindVoid     Kind = iota
	KindBool          // bool (i1)
	KindInt           // i8, i16, i32, i64, u8, u16, u32, u64, char
	KindFloat         // f32, f64
	KindString        // string = [char] (fat pointer: {i8*, i64})
	KindArray         // [T] dynamic or [T; N] fixed
	KindPointer       // *T
	KindFunction      // fn(T...) R  (bare function pointer)
	KindClosure       // fn with captured environment
	KindStruct        // struct
	KindUnion         // tagged union: type u = A | B
	KindData          // data maybe[t] = t | None
	KindEnum          // enum
	KindGeneric       // unresolved type parameter
	KindAtom          // atom literal type
)

// Type represents a resolved tin type
type Type struct {
	Kind Kind
	Name string // canonical name used as LLVM struct/alias name

	// Integers
	BitSize int  // 1, 8, 16, 32, 64
	Signed  bool // false for u* types and char

	// Float
	// BitSize used here too (32 or 64)

	// Array
	Elem *Type
	Len  int // -1 = dynamic slice

	// Pointer
	PointsTo *Type
	ConstPtr bool

	// Function
	Params    []*Type
	Return    *Type
	IsVarArgs bool

	// Struct
	Fields  []FieldInfo
	Methods []*FuncInfo

	// Union / Data
	Variants    []*Type
	VariantTags []string // tag name for each variant
	HasNone     bool     // data types may carry a None variant

	// Enum
	BaseType *Type
	Members  []EnumMember

	// Generics
	TypeParams []string // parameter names for generic definitions
	TypeArgs   []*Type  // resolved args for a specific instantiation

	// Control tags
	Tags map[string]bool // #pure, #sideffect, #no_recurse, #no_thread, ...
}

// FieldInfo describes a struct field
type FieldInfo struct {
	Name      string
	Type      *Type
	IsForward bool
	Offset    int // field index (for GEP)
}

// FuncInfo describes a function/method signature
type FuncInfo struct {
	Name     string
	Params   []*Type
	Return   *Type
	IsStatic bool
	Tags     map[string]bool
}

// EnumMember holds one enum variant
type EnumMember struct {
	Name  string
	Value int64
}

// Built-in types

var (
	Void   = &Type{Kind: KindVoid, Name: "void"}
	Bool   = &Type{Kind: KindBool, Name: "bool", BitSize: 1}
	I8     = &Type{Kind: KindInt, Name: "i8", BitSize: 8, Signed: true}
	I16    = &Type{Kind: KindInt, Name: "i16", BitSize: 16, Signed: true}
	I32    = &Type{Kind: KindInt, Name: "i32", BitSize: 32, Signed: true}
	I64    = &Type{Kind: KindInt, Name: "i64", BitSize: 64, Signed: true}
	U8     = &Type{Kind: KindInt, Name: "u8", BitSize: 8, Signed: false}
	U16    = &Type{Kind: KindInt, Name: "u16", BitSize: 16, Signed: false}
	U32    = &Type{Kind: KindInt, Name: "u32", BitSize: 32, Signed: false}
	U64    = &Type{Kind: KindInt, Name: "u64", BitSize: 64, Signed: false}
	F32    = &Type{Kind: KindFloat, Name: "f32", BitSize: 32}
	F64    = &Type{Kind: KindFloat, Name: "f64", BitSize: 64}
	Char   = &Type{Kind: KindInt, Name: "char", BitSize: 8, Signed: false}
	SizeT  = &Type{Kind: KindInt, Name: "size_t", BitSize: 64, Signed: false}
	UInt32 = &Type{Kind: KindInt, Name: "uint32", BitSize: 32, Signed: false}
	TinStr = &Type{Kind: KindString, Name: "string"}
)

// Builtins maps tin type names to their pre-resolved *Type
var Builtins = map[string]*Type{
	"void": Void, "bool": Bool,
	"i8": I8, "i16": I16, "i32": I32, "i64": I64,
	"u8": U8, "u16": U16, "u32": U32, "u64": U64,
	"f32": F32, "f64": F64,
	"char": Char, "string": TinStr,
	"size_t": SizeT, "uint32": UInt32, "uint": U64,
	"int": I64,
}

// Constructors

func NewPointer(to *Type, isConst bool) *Type {
	return &Type{
		Kind:     KindPointer,
		Name:     "*" + to.Name,
		PointsTo: to,
		ConstPtr: isConst,
	}
}

func NewArray(elem *Type, length int) *Type {
	name := "[" + elem.Name + "]"
	if length >= 0 {
		name = fmt.Sprintf("[%s; %d]", elem.Name, length)
	}
	return &Type{Kind: KindArray, Name: name, Elem: elem, Len: length}
}

func NewFunction(params []*Type, ret *Type, varargs bool) *Type {
	var sb strings.Builder
	sb.WriteString("fn(")
	for i, p := range params {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(p.Name)
	}
	if varargs {
		sb.WriteString(", ...")
	}
	sb.WriteString(")")
	if ret != nil && ret.Kind != KindVoid {
		sb.WriteString(" " + ret.Name)
	}
	name := sb.String()
	if ret == nil {
		ret = Void
	}
	return &Type{Kind: KindFunction, Name: name, Params: params, Return: ret, IsVarArgs: varargs}
}

func NewClosure(params []*Type, ret *Type, captured []FieldInfo) *Type {
	base := NewFunction(params, ret, false)
	t := *base
	t.Kind = KindClosure
	t.Fields = captured
	return &t
}

func NewStruct(name string, fields []FieldInfo) *Type {
	return &Type{Kind: KindStruct, Name: name, Fields: fields}
}

func NewUnion(name string, variants []*Type, tags []string) *Type {
	return &Type{Kind: KindUnion, Name: name, Variants: variants, VariantTags: tags}
}

func NewData(name string, variants []*Type, hasNone bool) *Type {
	return &Type{Kind: KindData, Name: name, Variants: variants, HasNone: hasNone}
}

func NewEnum(name string, base *Type, members []EnumMember) *Type {
	return &Type{Kind: KindEnum, Name: name, BaseType: base, Members: members}
}

func NewGenericParam(name string) *Type {
	return &Type{Kind: KindGeneric, Name: name}
}

// Helpers

// IsInteger returns true for all integer kinds including char
func (t *Type) IsInteger() bool {
	return t.Kind == KindInt
}

// IsNumeric returns true for integers and floats
func (t *Type) IsNumeric() bool {
	return t.Kind == KindInt || t.Kind == KindFloat
}

// ByteSize returns the size in bytes of the type (approximate for complex types)
func (t *Type) ByteSize() int {
	switch t.Kind {
	case KindBool:
		return 1
	case KindInt, KindFloat:
		return t.BitSize / 8
	case KindPointer:
		return 8
	case KindString:
		return 16 // ptr(8) + len(8)
	case KindArray:
		if t.Len < 0 {
			return 16 // fat pointer
		}
		if t.Elem != nil {
			return t.Elem.ByteSize() * t.Len
		}
		return 8
	case KindStruct:
		total := 0
		for _, f := range t.Fields {
			total += f.Type.ByteSize()
		}
		return total
	case KindUnion, KindData:
		max := 0
		for _, v := range t.Variants {
			if v != nil {
				if s := v.ByteSize(); s > max {
					max = s
				}
			}
		}
		return max + 4 // + tag i32
	case KindClosure:
		return 16 // fn_ptr(8) + env_ptr(8)
	}
	return 8
}

// String returns a human-readable representation
func (t *Type) String() string {
	if t == nil {
		return "<nil>"
	}
	return t.Name
}

// Substitute replaces generic type parameters with concrete types
func (t *Type) Substitute(mapping map[string]*Type) *Type {
	if t == nil {
		return nil
	}
	if t.Kind == KindGeneric {
		if concrete, ok := mapping[t.Name]; ok {
			return concrete
		}
		return t
	}
	// For compound types, recursively substitute
	switch t.Kind {
	case KindArray:
		return NewArray(t.Elem.Substitute(mapping), t.Len)
	case KindPointer:
		return NewPointer(t.PointsTo.Substitute(mapping), t.ConstPtr)
	case KindFunction:
		params := make([]*Type, len(t.Params))
		for i, p := range t.Params {
			params[i] = p.Substitute(mapping)
		}
		return NewFunction(params, t.Return.Substitute(mapping), t.IsVarArgs)
	}
	return t
}
