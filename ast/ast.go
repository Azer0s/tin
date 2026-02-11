// Package ast defines the Abstract Syntax Tree for the tin language
// Every node carries source position information for error reporting
package ast

import "fmt"

// Pos is a source position
type Pos struct {
	Line int
	Col  int
}

func (p Pos) String() string {
	return fmt.Sprintf("%d:%d", p.Line, p.Col)
}

// Node is the base interface for all AST nodes
type Node interface {
	Pos() Pos
	nodeMarker()
}

// base embeds position; all nodes embed this
type base struct{ pos Pos }

func (b base) Pos() Pos      { return b.pos }
func (b base) nodeMarker()   {}

// Top-level

// Program is the root node
type Program struct {
	base
	Stmts []Node
}

// Declarations

type VarDecl struct {
	base
	IsConst bool
	Name    string
	Type    TypeExpr // nil = infer
	Value   Node     // nil = zero value
}

type FuncDecl struct {
	base
	Name           string
	TraitQualifier string   // non-empty for qualified impls: "iter[char]" -> fn iter[char]::method
	TypeParams     []string // generic: [t, r]
	Constraints    []TypeConstraint // generic type constraints: where t is Labeled+Sized
	Params         []Param
	RetType        TypeExpr // nil = void/infer
	Body           Node     // *Block or *WhereList or expression
	Tags           []string // control tags: #pure, #recurse, …
	IsStatic       bool
	IsExtern    string // non-empty = extern symbol name
	IsVirtual   bool   // true for "fn f() T = virtual" in trait declarations
}

// TypeConstraint bounds a type parameter to one or more required traits
// e.g. "where t is labeled+sized" -> {TypeParam:"t", Traits:[labeled, sized]}
// e.g. "where t is iter[i64]"   -> {TypeParam:"t", Traits:[iter[i64]]}
type TypeConstraint struct {
	TypeParam string
	Traits    []TypeExpr // each may be SimpleType or GenericType
}

type StructDecl struct {
	base
	Name       string
	TypeParams []string
	Fields     []StructField
	Methods    []*FuncDecl
	Implements []TypeExpr // trait impls listed in parens
	Tags       []string
}

type TraitDecl struct {
	base
	Name          string
	TypeParams    []string
	Methods       []*FuncDecl  // virtual or default methods
	ForwardFields []StructField // "s size_t forward" – injected into implementing structs
	IsAlias       bool
	AliasType     TypeExpr // "trait print as fn() [char]"
}

type TypeDecl struct {
	base
	Name       string
	TypeParams []string
	Type       TypeExpr
	Overrides  []*FuncDecl // "override = fn show …"
}

type EnumDecl struct {
	base
	BaseType TypeExpr // nil = smallest int
	IsAtom   bool     // "enum atom status"
	Name     string
	Members  []EnumMember
}

type UnionDecl struct {
	base
	Name    string
	Members []UnionMember
	IsNamed bool // "union u_named = as_i8 i8 | as_string string"
}

type DataDecl struct {
	base
	Name       string
	TypeParams []string
	Variants   []DataVariant
}

type UseDecl struct {
	base
	Path     string       // "io" or "extern"
	IsExtern bool
	Imports  []UseImport  // for "use extern (...)"
}

type ExportDecl struct {
	base
	Names  []string
	AsName string
}

// TestDecl is a named test block: test "description" = body
type TestDecl struct {
	base
	Desc string
	Body Node
}

type MacroDecl struct {
	base
	Name   string
	Tags   []string
	Params []string
	Body   Node
}


// Statements

type Block struct {
	base
	Stmts []Node
}

type ReturnStmt struct {
	base
	Value Node // nil = return void
}

type BreakStmt struct{ base }

type DeferStmt struct {
	base
	Call Node
}

type IfStmt struct {
	base
	Cond     Node
	Then     *Block
	ElseIfs  []ElseIfClause
	Else     *Block // nil if no else
}

// ForStmt covers all three for variants:
//   C-style:  for let i T ; cond ; post : body
//   For-in:   for let i T in iter : body
//   For-range: for let i T in start..end : body  (handled as for-in over range)
type ForStmt struct {
	base
	Kind    ForKind
	VarName string
	VarType TypeExpr
	// C-style
	Init Node
	Cond Node
	Post Node
	// For-in
	Iter Node
	Body *Block
}

type ForKind int

const (
	ForCStyle ForKind = iota
	ForIn
)

type MatchStmt struct {
	base
	Expr    Node
	IsType  bool // "match a.(type)"
	Cases   []MatchCase
	Default *Block
}

type WhereList struct {
	base
	Clauses []WhereClause
}

type AssignStmt struct {
	base
	Target Node
	Value  Node
}

type AugAssignStmt struct {
	base
	Target Node
	Op     string // +=, -=, *=, /=, %=, ++=
	Value  Node
}

type PostfixStmt struct {
	base
	Expr Node
	Op   string // ++ or --
}

type ExprStmt struct {
	base
	Expr Node
}

// EchoStmt represents the "echo" builtin statement
type EchoStmt struct {
	base
	Value Node
}

// TaggedBlock: { #tag } { body }
type TaggedBlock struct {
	base
	Tags []string
	Body *Block
}

// Expressions

type BinExpr struct {
	base
	Left  Node
	Op    string
	Right Node
}

type UnaryExpr struct {
	base
	Op   string
	Expr Node
	Post bool // true for postfix ++/--
}

type CallExpr struct {
	base
	Func      Node
	TypeArgs  []TypeExpr // explicit generic type args
	Args      []Node
	IsVarArgs bool
}

type IndexExpr struct {
	base
	Expr  Node
	Index Node
}

type FieldAccess struct {
	base
	Expr  Node
	Field string
	IsPtr bool // -> operator
}

type ScopeAccess struct {
	base
	Path []string // e.g. ["io", "print"]
}

type PipeExpr struct {
	base
	Left  Node
	Right Node // should be a call expression
}

type StructLit struct {
	base
	TypeName   string
	TypeArgs   []TypeExpr
	Fields     []StructLitField
	Positional []Node // positional initializers
}

type ArrayLit struct {
	base
	Elems []Node
}

type RangeExpr struct {
	base
	Start Node
	End   Node
}

type LambdaExpr struct {
	base
	TypeParams []string
	Params     []Param
	RetType    TypeExpr
	Body       Node // *Block or *WhereList or expression
	Tags       []string
}

type IsExpr struct {
	base
	Expr    Node
	VarName string   // variable to bind matched value to
	Type    TypeExpr // or nil for "is None"
	IsNone  bool
}

type TypeAssertExpr struct {
	base
	Expr   Node
	Type   TypeExpr // x.(Type)
	IsType bool     // x.(type) - used in match
}

type AsExpr struct {
	base
	Expr Node
	Type TypeExpr
}

type SizeofExpr struct {
	base
	Type TypeExpr
}

type TypeofExpr struct {
	base
	Expr Node
}

type TraitofExpr struct {
	base
	Expr Node
}

type FieldnamesExpr struct {
	base
	Expr Node
}

type FieldtypesExpr struct {
	base
	Expr Node
}

type FieldtagExpr struct {
	base
	Expr  Node
	Field Node // string expression — the field name
}

type GetfieldExpr struct {
	base
	Expr  Node
	Field Node // string expression — the field name
}

type SetfieldExpr struct {
	base
	Expr  Node
	Field Node // string expression — the field name
	Val   Node // value to set
}

type AddrExpr struct {
	base
	Val Node
}

type DerefExpr struct {
	base
	Expr Node
}

type AddressOfExpr struct {
	base
	Expr Node
}

type TernaryExpr struct {
	base
	Cond Node
	Then Node
	Else Node
}

// InterpolatedString: "hello {name}!" broken into parts
type InterpolatedString struct {
	base
	Parts []StringPart
}

type Identifier struct {
	base
	Name string
}

type IntLit struct {
	base
	Value int64
}

type FloatLit struct {
	base
	Value float64
}

type StringLit struct {
	base
	Value string
}

type CharLit struct {
	base
	Value byte
}

type BoolLit struct {
	base
	Value bool
}

type AtomLit struct {
	base
	Name string // 'ok -> "ok"
}

type NoneLit struct{ base }

type WildcardExpr struct{ base } // "_" in where/match patterns

type DefaultExpr struct {
	base
	Type TypeExpr
}

// Helper types

type Param struct {
	Name      string
	Type      TypeExpr
	IsConst   bool
	IsVarArgs bool // ...
}

type StructField struct {
	Name      string
	Type      TypeExpr
	Tags      []string
	IsForward bool
}

type EnumMember struct {
	Name  string
	Value Node // nil = auto (previous+1 or iota)
	IsAtom bool
}

type UnionMember struct {
	FieldName string   // for named unions ("as_i8")
	Type      TypeExpr
}

type DataVariant struct {
	Name string   // "None" or empty for typed variant
	Type TypeExpr // nil if just "None"
}

type UseImport struct {
	ExternName string
	LocalName  string
	Type       TypeExpr
}

type WhereClause struct {
	Pos  Pos
	Cond Node // nil = wildcard "_"
	Body Node // expression or *Block
}

type ElseIfClause struct {
	Cond Node
	Body *Block
}

type MatchCase struct {
	Pos     Pos
	Pattern Node   // expression or type pattern
	VarName string // "i" in "case i i8:"
	VarType TypeExpr
	Body    *Block
}

type StructLitField struct {
	Name  string
	Value Node
}

type StringPart struct {
	IsExpr bool
	Str    string // if !IsExpr
	Expr   Node   // if IsExpr
}

// Type expressions

// TypeExpr represents a type annotation in source
type TypeExpr interface {
	typeExprMarker()
	String() string
}

type SimpleType struct {
	Name string
}

func (s *SimpleType) typeExprMarker() {}
func (s *SimpleType) String() string  { return s.Name }

type GenericType struct {
	Name       string
	TypeParams []TypeExpr
}

func (g *GenericType) typeExprMarker() {}
func (g *GenericType) String() string {
	params := ""
	for i, p := range g.TypeParams {
		if i > 0 {
			params += ", "
		}
		params += p.String()
	}
	return g.Name + "[" + params + "]"
}

type ArrayType struct {
	Elem TypeExpr
	Size int // -1 = dynamic
}

func (a *ArrayType) typeExprMarker() {}
func (a *ArrayType) String() string {
	if a.Size < 0 {
		return "[" + a.Elem.String() + "]"
	}
	return fmt.Sprintf("[%s; %d]", a.Elem.String(), a.Size)
}

type PointerType struct {
	Elem    TypeExpr
	IsConst bool
}

func (p *PointerType) typeExprMarker() {}
func (p *PointerType) String() string {
	if p.IsConst {
		return "const *" + p.Elem.String()
	}
	return "*" + p.Elem.String()
}

type FuncType struct {
	Params    []TypeExpr
	RetType   TypeExpr // nil = void
	IsVarArgs bool
}

func (f *FuncType) typeExprMarker() {}
func (f *FuncType) String() string  { return "fn(...)" }

type UnionTypeExpr struct {
	Types []TypeExpr
}

func (u *UnionTypeExpr) typeExprMarker() {}
func (u *UnionTypeExpr) String() string {
	s := ""
	for i, t := range u.Types {
		if i > 0 {
			s += " | "
		}
		s += t.String()
	}
	return s
}

type VoidType struct{}

func (v *VoidType) typeExprMarker() {}
func (v *VoidType) String() string  { return "void" }
