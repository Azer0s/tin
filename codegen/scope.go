package codegen

// scope.go - implicit conversion registry entry and lexical scope types/functions.

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Implicit conversion registry

// implicitConvEntry records one implicit[T] -> S conversion function.
type implicitConvEntry struct {
	srcLLVM irtypes.Type // source type T
	fn      *ir.Func     // static fn(T) S
}

// Scope

type scopeEntry struct {
	val      value.Value // alloca pointer (for locals) or *ir.Func (for functions)
	isAlloc  bool        // true if val is an alloca (needs load/store)
	isRC     bool        // true if the alloca holds an ARC-managed value ([T] or any)
	noDeinit bool        // true for the `this` parameter of a deinit method (prevents recursive deinit)
	isGlobal bool        // true for module-level globals; skip in per-function scope release
	// basePtr is set for slice variables (arr[start:end]).  Because the fat-ptr
	// stores an interior pointer (offset into the allocation), ARC retain/release
	// must operate on the base allocation pointer, not the possibly-interior field 0.
	// When non-nil, scope-exit releases basePtr directly instead of the fat-ptr.
	basePtr value.Value // i8* base allocation pointer for slice variables; nil otherwise
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
