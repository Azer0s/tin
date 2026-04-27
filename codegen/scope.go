package codegen

// scope.go - implicit conversion registry entry and lexical scope types/functions.

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Implicit conversion registry

// implicitConvEntry records one implicit[T] -> S conversion function.
type implicitConvEntry struct {
	srcLLVM irtypes.Type // source type T
	fn      *ir.Func     // static fn(T) S
}

// Scope

type scopeEntry struct {
	val        value.Value // alloca pointer (for locals) or *ir.Func (for functions)
	isAlloc    bool        // true if val is an alloca (needs load/store)
	isRC       bool        // true if the alloca holds an ARC-managed value ([T] or any)
	noDeinit   bool        // true for the `this` parameter of a deinit method (prevents recursive deinit)
	noRelease  bool        // true for borrowed bindings (e.g. union `is` vars) -- scope exit skips all release
	isGlobal   bool        // true for module-level globals; skip in per-function scope release
	isUnsigned bool        // true if the variable's Tin type is unsigned (u8/u16/u32/u64)
	// byteArrayElem is the element type name ("byte", "u8", "char") when this
	// variable holds a [byte]/[u8]/[char] fat array.  Empty string otherwise.
	// Used by genEcho to choose the per-element printf format.
	byteArrayElem string
	// scalarTypeName is the Tin type name for 8-bit scalar variables: "char", "byte",
	// "u8", or "i8".  Empty for all other types.
	// Used by interpolation/echo to choose the per-value printf format.
	scalarTypeName string
	// isHeapOwned: true when this variable holds the result of a late-promoted
	// heap allocation (returned via _tin_rc_alloc from a callee that heap-promotes).
	// Scope-exit calls emitHeapChainRelease(depth) to walk the chain of RC blocks
	// and free all of them (including inner blocks for nested promotions like **i64).
	isHeapOwned    bool
	heapOwnedDepth int // number of RC-promoted pointer levels (1 for *T, 2 for **T, ...)
	// basePtr is set for slice variables (arr[start:end]).  Because the fat-ptr
	// stores an interior pointer (offset into the allocation), ARC retain/release
	// must operate on the base allocation pointer, not the possibly-interior field 0.
	// When non-nil, scope-exit releases basePtr directly instead of the fat-ptr.
	basePtr value.Value // i8* base allocation pointer for slice variables; nil otherwise
	// tinType is the declared Tin AST type for this variable (nil if unknown/inferred).
	// Used for type-guided overload resolution and async fat-ptr return-type recovery.
	tinType ast.TypeExpr
	// staticArrayLen records the compile-time element count of an array /
	// string `let` initializer (ArrayLit, ArrayFillLit, StringLit). 0 means
	// unknown. A later mutation invalidates this and clears the field.
	staticArrayLen int64
	// constInitExpr captures the initializer AST of a non-mutated `let` binding
	// when that initializer can be statically analyzed for compile-time
	// folding. Used by tryFoldExpr to follow `let t = typeof(v)` -> 'bool /
	// 'i64 / ... when v's static type is known. Set ONLY for bindings whose
	// init expression is itself a fold-friendly node (typeof, atom literal,
	// bool literal, integer literal); a later mutation invalidates this and
	// the field is cleared by genAssign.
	constInitExpr ast.Node
}

type scope struct {
	vars               map[string]*scopeEntry
	parent             *scope
	isFunctionBoundary bool // if true, emitAllScopeReleases stops here and does not release parent vars
}

func newScope(parent *scope) *scope {
	return &scope{vars: make(map[string]*scopeEntry), parent: parent}
}

func (s *scope) lookup(name string) (*scopeEntry, bool) {
	if e, ok := s.vars[name]; ok {
		return e, true
	}

	if s.parent != nil {
		return s.parent.lookup(name)
	}

	return nil, false
}

func (s *scope) set(name string, e *scopeEntry) {
	s.vars[name] = e
}
