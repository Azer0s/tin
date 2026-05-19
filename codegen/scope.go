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

// coerceConvEntry records one S -> coerce[T] conversion function.  S
// is the implementer struct (key into the registry); fn is the static
// `::coerce(this S) T` method that runs at `<s_val> as T`.
type coerceConvEntry struct {
	tgtLLVM irtypes.Type // target type T
	fn      *ir.Func     // static fn(S) T
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
	// aliasedFromName is set when this binding was declared as `let b = a`
	// from another fat-pointer binding (`string` / `[T]`).  Indexed-write
	// and `++=` sites consult it to emit the -Walias-mutation warning so
	// the user gets pointed at `copy(...)` when they almost certainly
	// meant to break the alias.
	aliasedFromName string
	// holdsFreshRCPtr is true when the binding was initialized from an
	// expression that produces a fresh `_tin_rc_alloc`'d pointer (notably
	// `&StructLit{...}` and ADT constructors).  Without this flag, a
	// subsequent `return s` from such a binding loses the RC provenance
	// at the coerce-to-trait site (LLVM Load doesn't trace back through
	// the alloca's store chain), and `coerceToTrait` falls back to the
	// borrow vtable whose data-release slot is a no-op -- leaking the
	// heap block on every caller scope-exit.
	holdsFreshRCPtr bool
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
	// declPos is the source position of the original binding for this
	// entry. Set by the let/var/param decl sites that have access to the
	// AST node; left as the zero Pos for synthetic bindings (e.g. compiler
	// scaffolding, monomorphization, where-pattern destructuring) where
	// "where it was declared in source" isn't meaningful. Consumed by
	// `sourcepos(symbol)` to report the binding location.
	declPos ast.Pos

	// declaredConst / declaredLet record whether the binding's source-
	// level keyword was `const` / `let`. Used by `for ref` to refuse
	// aliasing into immutable storage. `var` bindings have both flags
	// false (the default). Synthetic bindings without a source keyword
	// also have both false; ref-iteration over those is allowed because
	// the compiler vouches for them being writable storage.
	declaredConst bool
	declaredLet   bool

	// constInitExpr captures the initializer AST of a non-mutated `let` binding
	// when that initializer can be statically analyzed for compile-time
	// folding. Used by tryFoldExpr to follow `let t = typeof(v)` -> 'bool /
	// 'i64 / ... when v's static type is known. Set ONLY for bindings whose
	// init expression is itself a fold-friendly node (typeof, atom literal,
	// bool literal, integer literal); a later mutation invalidates this and
	// the field is cleared by genAssign.
	constInitExpr ast.Node

	// ownsIfaceData is true for trait-iface let-bindings whose data ptr was
	// freshly heap-allocated by coerceToTrait. emitScopeRelease emits an
	// extra _tin_release on the iface's data field for these so the heap
	// block is reclaimed. Both value-source and pointer-source coerceToTrait
	// branches heap-copy the source struct now, so every value-to-iface
	// let-binding sets this flag.
	ownsIfaceData bool

	// releaseRawPtr is set on synthetic alloca entries that hold a raw i8*
	// heap pointer (e.g. anonymous iface temporaries from coerceToTrait
	// passed inline as call arguments). emitScopeRelease loads the pointer
	// and calls _tin_release on it. Used to defer cleanup of intermediate
	// heap blocks until the enclosing scope exits, after any spawned
	// fiber that captured the pointer has had a chance to complete via
	// the scope's await.
	releaseRawPtr bool

	// isEarlyHeap is true when this `let` binding's storage was allocated
	// via _tin_rc_alloc instead of stack alloca because escape analysis
	// determined that the variable's address would outlive the function
	// frame. entry.val IS the heap pointer (typed *T), so &x naturally
	// produces a stable heap pointer and reads/writes through it work
	// without any extra indirection. Scope-exit calls _tin_release on
	// entry.val to drop the heap block.
	isEarlyHeap bool

	// ownsHeapIfaceData is true when this binding holds the result of a
	// function that returned a *Trait whose `data` field is an escape-
	// promoted heap pointer (the callee's source `&b` came from an
	// escaping local). On scope exit, the iface block is released as
	// usual AND the data field is released too -- neither would happen
	// otherwise because nothing in the iface struct's static layout
	// reveals that data points at heap memory.
	ownsHeapIfaceData bool

	// ownsHeapPromotedFields is the set of LLVM field offsets whose
	// pointer values are heap-promoted blocks owned by this binding.
	// Populated when the receiving call site looks up
	// cg.fnReturnsHeapPromotedFields for the callee and inherits its
	// metadata.  Scope-exit emits a _tin_release for the value loaded
	// from each listed field so the heap blocks freed by the per-
	// struct release helper (which treats raw *T fields as borrows)
	// don't leak.
	ownsHeapPromotedFields []int

	// ownsPtrViaRetain is set when emitOwningPtrRetainIfApplicable
	// bumped the heap RC for this binding's source value (a copy
	// expression: identifier, field access, index, deref).  The
	// matching scope-exit release_ptr fires off this flag.  Plain
	// `&local` bindings, function parameters, and call-result
	// bindings (isHeapOwned) DO NOT set this - their release
	// (or non-release) is handled elsewhere.
	ownsPtrViaRetain bool

	// pointsToBorrowedStorage is true when this binding holds a pointer
	// (`*T`) that was initialized by taking the address of a stack
	// alloca or module-level global -- i.e. the pointee has NO TinRCHdr
	// prefix.  Downstream coercions like `let a *Trait = thisBinding`
	// must use the borrow vtable (no-op data release) and skip the
	// _tin_retain on the data field, otherwise the iface release path
	// reads (data - sizeof(TinRCHdr)) bytes that don't exist and either
	// reports an uninit-read under valgrind or atomically corrupts
	// adjacent stack / .bss memory.
	pointsToBorrowedStorage bool
}

type scope struct {
	vars               map[string]*scopeEntry
	names              []string // insertion order of `vars` keys; never randomized.
	parent             *scope
	isFunctionBoundary bool // if true, emitAllScopeReleases stops here and does not release parent vars
	// visibleTypes tracks bare type names (data, struct, enum, trait,
	// type alias) that are accessible without package qualification
	// in this scope. Populated by:
	//   - type declarations in the current translation unit (the
	//     declaring scope sees its own types bare),
	//   - `use { Name } from pkg` selective imports (importer sees
	//     Name bare),
	//   - file-path `use "./helper"` imports (helper's decls flow
	//     into the importing scope as if inlined).
	// Plain `use pkg` does NOT populate this - consumers must write
	// `pkg::Name` to reach those types, mirroring how variables /
	// functions already require pkg qualification or selective
	// import. typeNameVisible walks the parent chain.
	visibleTypes map[string]bool
}

// rcRetainedFromCopy is true on a scopeEntry when its let-binding
// took a copy-expression value (Identifier, FieldAccess, IndexExpr,
// non-temporary DerefExpr) AND emitOwningPtrRetainIfApplicable
// bumped the heap block's RC. Marker for emitScopeRelease's
// `*TinStruct` / `*Trait` release path: only emit release_ptr when
// the binding actually took ownership of a heap block.  `let mb = &b`
// (AddressOf of a stack local) does NOT set this flag, so the scope
// release no longer tries to atomic-decrement memory it doesn't own.

func newScope(parent *scope) *scope {
	return &scope{vars: make(map[string]*scopeEntry), parent: parent}
}

// markTypeVisible records that bareName is accessible without
// qualification in this scope. Used by type declarations and
// selective imports. Lazy-allocates the map on first use.
func (s *scope) markTypeVisible(bareName string) {
	if s.visibleTypes == nil {
		s.visibleTypes = make(map[string]bool)
	}

	s.visibleTypes[bareName] = true
}

// typeNameVisible reports whether bareName is reachable as an
// unqualified type identifier from this scope, walking the parent
// chain.
func (s *scope) typeNameVisible(bareName string) bool {
	for cur := s; cur != nil; cur = cur.parent {
		if cur.visibleTypes != nil && cur.visibleTypes[bareName] {
			return true
		}
	}

	return false
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
	if _, existed := s.vars[name]; !existed {
		s.names = append(s.names, name)
	}

	s.vars[name] = e
}

// each iterates the scope's entries in insertion order. Use this instead of
// `range s.vars` whenever iteration order can leak into IR text - Go map
// iteration is randomized per process, which would break the
// content-addressed mono cache and the byte-identical-IR CI gate.
func (s *scope) each(fn func(name string, e *scopeEntry)) {
	for _, name := range s.names {
		if e, ok := s.vars[name]; ok {
			fn(name, e)
		}
	}
}

// eachReverse walks insertion order back-to-front. Used by emitScopeRelease
// so ARC release happens LIFO (a variable that captured a reference to an
// earlier-declared one is torn down first).
func (s *scope) eachReverse(fn func(name string, e *scopeEntry)) {
	for i := len(s.names) - 1; i >= 0; i-- {
		name := s.names[i]
		if e, ok := s.vars[name]; ok {
			fn(name, e)
		}
	}
}
