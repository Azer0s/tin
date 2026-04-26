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
func (b *base) SetPos(p Pos) { b.pos = p }

// makeBase returns a base with the given source position.
// Used by the node constructors below to record locations.
func makeBase(line, col int) base { return base{pos: Pos{Line: line, Col: col}} }

// AtLine creates a Pos for use in node constructors.
func AtLine(line, col int) Pos { return Pos{Line: line, Col: col} }

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
	TraitQualifier string           // non-empty for qualified impls: "iter[char]" -> fn iter[char]::method
	TypeParams     []string         // generic: [t, r]
	Constraints    []TypeConstraint // generic type constraints: where t is Labeled+Sized
	Params         []Param
	RetType        TypeExpr // nil = void/infer
	Body           Node     // *Block or *WhereList or expression
	Tags           []string // control tags: #pure, #recurse, ...
	IsStatic       bool
	IsExtern       string // non-empty = extern symbol name
	IsVirtual      bool   // true for "fn f() T = virtual" in trait declarations
}

// TypeConstraint bounds a type parameter with a boolean expression of trait
// checks (Form A from the design doc).
//
// Grammar (per type parameter):
//
//	bound := or
//	or    := and ('||' and)*
//	and   := unary ('&&' unary)*
//	unary := 'not' atom | atom
//	atom  := '(' bound ')' | <type-expr>
//
// Inside `where T is <bound>` each <type-expr> atom is implicitly checked
// against T; `+` (legacy shorthand for conjunction) lowers to TBAnd chains
// so existing programs continue to parse.
//
// Examples:
//
//	where t is ord                            -> TBAtom(ord, Neg:false)
//	where t is labeled+sized                  -> TBAnd(TBAtom(labeled), TBAtom(sized))
//	where t is ord && not bool                -> TBAnd(TBAtom(ord), TBAtom(bool, Neg:true))
//	where t is i64 || f64                     -> TBOr(TBAtom(i64),   TBAtom(f64))
//	where t is addable && (not bool || char)  -> TBAnd(TBAtom(addable),
//	                                                    TBOr(TBAtom(bool, Neg:true),
//	                                                         TBAtom(char)))
type TypeConstraint struct {
	Pos       Pos
	TypeParam string
	Bound     TypeBound
}

// TypeBound is the boolean expression in a TypeConstraint.
type TypeBound interface {
	typeBoundMarker()
	Pos() Pos
}

// TBAtom is a leaf bound: `is <trait>` when Neg is false, `is not <trait>`
// when true.
type TBAtom struct {
	NodePos Pos
	Trait   TypeExpr // SimpleType, GenericType, or any other type expression
	Neg     bool
}

// TBAnd is `left && right`.
type TBAnd struct {
	NodePos     Pos
	Left, Right TypeBound
}

// TBOr is `left || right`.
type TBOr struct {
	NodePos     Pos
	Left, Right TypeBound
}

func (*TBAtom) typeBoundMarker() {}
func (*TBAnd) typeBoundMarker()  {}
func (*TBOr) typeBoundMarker()   {}

func (b *TBAtom) Pos() Pos { return b.NodePos }
func (b *TBAnd) Pos() Pos  { return b.NodePos }
func (b *TBOr) Pos() Pos   { return b.NodePos }

type StructDecl struct {
	base
	Name        string
	TypeParams  []string
	Constraints []TypeConstraint // generic type constraints: where t is addable
	Fields      []StructField
	Methods     []*FuncDecl
	Implements  []TypeExpr // trait impls listed in parens
	Tags        []string   // unscoped tags (e.g. "packed"); applied to the struct itself
	// ScopedTags are tags written with an `@scope` qualifier in the struct's
	// `{#tag@scope}` header (e.g. `#pure@fn`). Propagation happens in codegen
	// before any tag-consuming pass runs: members matching the scope receive
	// the tag; existing member-level tags take precedence on conflicts.
	ScopedTags []ScopedTag
}

// ScopedTag is a struct-level control tag tagged with a member-scope
// qualifier. Scope is one of: "fn", "method", "static_fn", "field".
type ScopedTag struct {
	Name  string
	Scope string
}

type TraitDecl struct {
	base
	Name          string
	TypeParams    []string
	Methods       []*FuncDecl   // virtual or default methods
	ForwardFields []StructField // "s size_t forward" - injected into implementing structs
	IsAlias       bool
	IsStaticAlias bool     // "trait[k] T[t] as static fn(val t) k"
	AliasType     TypeExpr // "trait print as fn() [char]"
}

type TypeDecl struct {
	base
	Name        string
	TypeParams  []string
	Constraints []TypeConstraint // generic type constraints: where t is addable
	Type        TypeExpr
	Overrides   []*FuncDecl // "override = fn show ..."
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

// DataDecl is an algebraic data type (sum type) with named constructors.
//
//	data Option[t] =
//	  Some(t)
//	  None
//
// Each variant carries zero or more positional or named fields.
// At least one variant must carry a payload (pure-nullary shapes should
// use `enum` instead).
type DataDecl struct {
	base
	Name        string
	TypeParams  []string
	Constraints []TypeConstraint
	Variants    []DataVariant
}

type DataVariant struct {
	Pos    Pos
	Name   string
	Fields []StructField // empty -> nullary
}

// ArrayDestructDecl let [a, b] [T] = expr
//
//	let [a, b] [T1, T2] = expr  (per-slot types, implies [any] source)
//	let [x, ...xs] [T] = expr   (rest split)
//	let [a, b] res = expr        (named type alias, resolved at codegen)
type ArrayDestructDecl struct {
	base
	Names     []string   // variable names; rest name is prefixed with "..."
	ElemTypes []TypeExpr // len==1 for uniform [T]; len>1 for per-slot types
	IsAny     bool       // true -> runtime bounds check required
	NamedType TypeExpr   // non-nil when a named type alias is used (e.g. `type res = @[i32, bool]`)
	Value     Node
}

// StructDestructDecl let {x, y} TypeName = expr  or  let {x: a, y: b} TypeName = expr
type StructDestructDecl struct {
	base
	Names      []string // field names to extract
	VarNames   []string // variable names to bind (if nil, same as Names)
	StructType TypeExpr
	Value      Node
}

// TupleArrayType @[T1, T2, ...] - a typed-destructuring annotation
// indicating a [any] array whose elements are typed per-slot.
type TupleArrayType struct {
	ElemTypes []TypeExpr
}

func (t *TupleArrayType) typeExprMarker() {}
func (t *TupleArrayType) String() string {
	s := "@["

	for i, e := range t.ElemTypes {
		if i > 0 {
			s += ", "
		}

		s += e.String()
	}

	return s + "]"
}

type UseDecl struct {
	base
	Path       string // "io" or "std::math" (module path) or "./<file>" (file path)
	IsExtern   bool
	Imports    []UseImport // for "use extern (...)"
	IsFile     bool        // true when path was given as a string literal: use "./foo.tin"
	Names      []string    // selective import: names from `use { name1, name2 } from path`
	FromSyntax bool        // true when `use { ... } from` syntax was used
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
	Name       string
	Tags       []string
	Params     []string
	ParamTypes []string // parallel to Params; "" means untyped (infer from call site)
	Body       Node
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
	Cond    Node
	Then    *Block
	ElseIfs []ElseIfClause
	Else    *Block // nil if no else
}

// ForStmt covers all three for variants:
//
//	C-style:  for let i T ; cond ; post : body
//	For-in:   for let i T in iter : body
//	For-range: for let i T in start..end : body  (handled as for-in over range)
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
	ForWhile // condition-only: for <bool-expr>:
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

// TaggedBlock { #tag } { body }
type TaggedBlock struct {
	base
	Tags []string
	Body *Block
}

// TupleDestructDecl represents: let (x, y) = expr
// Destructures a Tuple value into named local variables.
type TupleDestructDecl struct {
	base
	IsConst bool
	Names   []string
	Value   Node
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

// ArrayFillLit represents [value; count] - fill an array with `count` copies of `value`.
type ArrayFillLit struct {
	base
	Value Node
	Count int
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
	Type    TypeExpr // nil only if used as bare type-check
	// Pattern is set for ADT-variant is-checks: `x is Ok(v)`.
	// When non-nil, Type and VarName are left unset; the AST shape stored in
	// Pattern is either *CallExpr (e.g. Ok(v)) or *Identifier (nullary like
	// None). Codegen inspects Pattern first for ADT variant dispatch.
	Pattern Node
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

// IsRCExpr evaluates to i32 1 if the type is ARC-tracked (string, array, any), 0 otherwise.
type IsRCExpr struct {
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
	Field Node // string expression - the field name
}

type GetfieldExpr struct {
	base
	Expr  Node
	Field Node // string expression - the field name
}

type SetfieldExpr struct {
	base
	Expr  Node
	Field Node // string expression - the field name
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

// InterpolatedString "hello {name}!" broken into parts
type InterpolatedString struct {
	base
	Parts []StringPart
}

type Identifier struct {
	base
	Name string
}

// NewIdent creates an Identifier with source position set.
func NewIdent(name string, line, col int) *Identifier {
	return &Identifier{base: makeBase(line, col), Name: name}
}

// NewCallExpr creates a CallExpr with source position set.
func NewCallExpr(fn Node, args []Node, line, col int) *CallExpr {
	return &CallExpr{base: makeBase(line, col), Func: fn, Args: args}
}

// NewFieldAccess creates a FieldAccess with source position set.
func NewFieldAccess(expr Node, field string, isPtr bool, line, col int) *FieldAccess {
	return &FieldAccess{base: makeBase(line, col), Expr: expr, Field: field, IsPtr: isPtr}
}

// NewScopeAccess creates a ScopeAccess with source position set.
func NewScopeAccess(path []string, line, col int) *ScopeAccess {
	return &ScopeAccess{base: makeBase(line, col), Path: path}
}

// NewBinExpr creates a BinExpr with source position set.
func NewBinExpr(left Node, op string, right Node, line, col int) *BinExpr {
	return &BinExpr{base: makeBase(line, col), Left: left, Op: op, Right: right}
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

// BacktickLit is a code-splice literal: `expr`.
// In macro context it expands to the parsed tin expression/identifier inside the backticks.
// Outside macros it compiles to a string constant with the backtick delimiters preserved.
type BacktickLit struct {
	base
	Content string // raw source between the backticks
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

type NilLit struct{ base } // nil - null pointer literal

type WildcardExpr struct{ base } // "_" in where/match patterns

// TupleLit is a tuple literal: (e1, e2, ...) - sugar for Tuple[T1,T2,...]{a:e1, b:e2, ...}
type TupleLit struct {
	base
	Elems []Node
}

// SliceExpr is an array slice: arr[start:end], arr[start:], arr[:end], arr[:]
// Start and End are nil when omitted.
type SliceExpr struct {
	base
	Expr  Node
	Start Node // nil = from beginning (0)
	End   Node // nil = to end (len)
}

type DefaultExpr struct {
	base
	Type   TypeExpr
	OfExpr Node // non-nil: derive zero value from this expression's compile-time type
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
	IsWeak    bool // non-owning: field does not retain/release its value
	// IsOwn marks an owning tree-edge field.  At runtime it is identical to a
	// plain strong field (retain on assign, release on free).  The difference
	// is compile-time only: the cycle checker allows a detected cycle that
	// contains at least one `own` edge without requiring a `weak` back-ref.
	// The programmer declares the data reachable through this field is acyclic
	// (forms a tree/DAG).  No runtime enforcement is performed; a future
	// debug-mode build option will add an acyclicity check on assignment.
	IsOwn bool
	// IsConst marks a field as immutable after struct construction.  The parser
	// sets this from a leading `const` keyword on the field line.  Codegen
	// rejects writes (plain assign, aug-assign, postfix, setfield, address-of)
	// to const fields.  Construction paths (struct literal, positional init,
	// destructuring let, match bindings) are unaffected.  `IsVar` exists only
	// to record that the user wrote an explicit `var` keyword, which is
	// redundant today but carries meaning under a future `#const@field`
	// default-flip.
	IsConst bool
	IsVar   bool
}

type EnumMember struct {
	Name   string
	Value  Node // nil = auto (previous+1 or iota)
	IsAtom bool
}

type UnionMember struct {
	FieldName string // for named unions ("as_i8")
	Type      TypeExpr
}

type UseImport struct {
	ExternName string
	LocalName  string
	Type       TypeExpr
}

type WhereClause struct {
	Pos Pos
	// Cond: bool expression for bool-guard clauses. nil means bare "_" wildcard,
	// which is a bool-mode catch-all (and is also accepted inside a pattern
	// where-list as the universal catch-all).
	Cond Node
	// Pattern: set for pattern clauses (`where (pat): ...`). When non-nil,
	// Cond must be nil; this is enforced in the parser. A pattern may be a
	// literal, Identifier binder, ArrayPattern, StructPattern, or TuplePattern.
	Pattern Node
	// Guard: optional `if <expr>` after a pattern (`where (pat) if guard: ...`).
	// Only valid when Pattern is non-nil.
	Guard Node
	Body  Node // expression or *Block
}

type ElseIfClause struct {
	Cond Node
	Body *Block
}

type MatchCase struct {
	Pos     Pos
	Pattern Node   // expression, type pattern, or *StructPattern
	VarName string // "i" in "case i i8:"
	VarType TypeExpr
	Guard   Node // optional "if expr" after pattern; nil = no guard
	Body    *Block
}

// StructPatternField is one slot in a struct destructuring pattern.
// Literal == nil means bind the field value to Name in the arm scope.
// Literal != nil means the field must equal that value (constraint).
// IsWild == true means "_": match but discard (no binding).
// BindTo non-empty means rename: bind field Name to variable BindTo.
type StructPatternField struct {
	Name    string
	Literal Node
	IsWild  bool
	BindTo  string // "x: px" -> Name="x", BindTo="px"
}

// StructPattern is used in match case arms to destructure a struct value:
//
//	case TypeName{field: literal, bound}:
type StructPattern struct {
	base
	TypeName string
	Fields   []StructPatternField
}

// ArrayPatternElement is one slot in an array destructuring pattern.
// IsRest == true means this is a "...name" rest element (always last).
// IsWild == true means "_" or unnamed rest: match but discard (no binding).
// Name is the variable to bind (empty or "_" = discard).
type ArrayPatternElement struct {
	Name   string
	IsRest bool // "...xs" or "..." -- must be last element if present
	IsWild bool // "_" wildcard
}

// ArrayPattern is used in match case arms to destructure a slice/array value:
//
//	case []:          empty array
//	case [x]:         exactly 1 element, bind to x
//	case [x, y]:      exactly 2 elements
//	case [x, ...xs]:  first element + rest slice bound to xs
//	case [_, _]:      2 elements, wildcards (discard)
//	case [...xs]:     catch-all -- all elements as xs
type ArrayPattern struct {
	base
	Elems []ArrayPatternElement
}

// TuplePattern is used for multi-arg where-clause destructuring:
//
//	where (0, "hello"):      ->  TuplePattern{IntLit 0, StringLit "hello"}
//	where (0, _, [x, ...]):  ->  TuplePattern{IntLit 0, Identifier "_", ArrayPattern}
//
// Single-arg patterns don't produce a TuplePattern; the parser unwraps the
// single inner element so `where (0):` stores an IntLit directly as Pattern.
type TuplePattern struct {
	base
	Elems []Node
}

type StructLitField struct {
	Name  string
	Value Node
}

type StringPart struct {
	IsExpr bool
	Str    string // if !IsExpr
	Expr   Node   // if IsExpr
	Format string // printf-style specifier without leading %, e.g. "08x", ".2f" (empty = default)
}

// TopLevelVar is a mutable module-scoped variable: var name Type [= expr]
type TopLevelVar struct {
	base
	Name  string
	Type  TypeExpr
	Value Node // nil = zero-initialized
}

// SpawnExpr spawns a fiber: spawn expr  or  spawn do: block
type SpawnExpr struct {
	base
	Call    Node   // expression to spawn (nil if DoBlock set)
	DoBlock *Block // spawn do: body (nil if Call set)
}

// AwaitExpr waits for a fiber future: await expr
type AwaitExpr struct {
	base
	Future Node
}

// YieldStmt voluntarily yields the current fiber's time slice.
type YieldStmt struct{ base }

// AwaitMatchCase is one arm of an await match statement.
// SlotIdx is the index of the one non-wildcard slot in the array pattern.
// BindName is the variable that receives the result (empty = wildcard, currently rejected).
type AwaitMatchCase struct {
	Pos      Pos
	SlotIdx  int
	BindName string
	Guard    Node
	Body     *Block
}

// AwaitMatchStmt selects among multiple futures:
//
//	await match [a, b, c]:
//	  case [x, _, _]: ...   // fires when a completes; x = a's result
//	  case [_, y, _]: ...   // fires when b completes; y = b's result
//	  default: ...          // non-blocking: runs if nothing is actionable
//
// Futures is always a fixed-length inline list (no array variables).
// Without default: blocks until one future fires and a guard passes; panics if all exhausted.
// With default: one non-blocking check; default runs if nothing is actionable.
type AwaitMatchStmt struct {
	base
	Futures []Node
	Cases   []AwaitMatchCase
	Default *Block
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
	IsAsync   bool // true for fn{#async}(...) type expressions
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
