package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Helper utilities

// hoistAlloca emits `alloca ty` at the start of the function that owns
// `block`, then returns the alloca.  USE THIS instead of
// `block.NewAlloca` whenever the call site might be reached from inside
// a loop body.
//
// Allocas placed in a non-entry block emit as `sub sp, ...` at that
// block's PC; the matching `add sp, ...` only fires when the function
// returns -- there's no `add sp` on the loop back-edge.  An alloca
// inside a for-loop body therefore silently leaks one slot per
// iteration onto the C stack, until the thread's stack guard page is
// hit (SIGBUS on macOS, where worker threads have 544 KB default
// stacks; Linux's 8 MB default hides the same bug as a slowdown).
//
// Derives the owning function from block.Parent rather than cg.curFn:
// several codegen paths build synthetic helper functions (deinit
// thunks, trait release wrappers, coro splits) without updating
// cg.curFn, and trusting cg.curFn there would hoist into the outer
// caller's entry block, producing IR that references slots which
// never exist in the helper.
func (cg *CodeGen) hoistAlloca(block *ir.Block, ty irtypes.Type) *ir.InstAlloca {
	if block != nil && block.Parent != nil && len(block.Parent.Blocks) > 0 {
		return block.Parent.Blocks[0].NewAlloca(ty)
	}

	if block != nil {
		return block.NewAlloca(ty)
	}

	if cg.curFn != nil && len(cg.curFn.Blocks) > 0 {
		return cg.curFn.Blocks[0].NewAlloca(ty)
	}

	return ir.NewAlloca(ty)
}

// closureCapture describes a variable captured from the enclosing scope.
type closureCapture struct {
	name   string
	val    value.Value
	llvmTy irtypes.Type
	byRef  bool // true: store the alloca pointer; false: store the loaded value
	// skipRetain is true when buildClosureEnv must NOT bump the
	// capture's RC (set by genBoundMethod for anonymous heap-receiver
	// shapes like `(&Foo{}).method` where the closure env takes the
	// only rc=1 reference and the dtor's release_ptr is the matching
	// drop).  Default false preserves the safer "retain at capture"
	// shape every other caller relies on.
	skipRetain bool
}

// closureCtx saves the mutable per-function state so it can be restored after
// emitting a nested closure / thunk function.
type closureCtx struct {
	fn                *ir.Func
	scope             *scope
	curBlock          *ir.Block
	deferFnI8s        []value.Value
	deferFrames       []value.Value
	deferEnvs         []value.Value
	deferRetSlotParam value.Value
	fnDeferRetAlloca  value.Value
	deferThunkRetType irtypes.Type
	inCoroFn          bool
	autoYield         bool
	coloredSync       bool
	coroHdl           value.Value
	coroID            value.Value
	coroCleanup       *ir.Block
	coroFrame         *coroFrame
	coroRetType       irtypes.Type
}

// pushClosureCtx saves the current function context, switches cg to f, and
// roots the new scope at the module-level (global) scope.  All coroutine /
// autoyield / colored-sync flags are saved and reset to the "plain sync fn"
// defaults so the closure body doesn't inherit the enclosing function's
// coro frame (which would produce cross-function SSA references via
// llvm.coro.suspend) or autoyield instrumentation.

func (cg *CodeGen) posStr(node ast.Node) string {
	var p ast.Pos
	if node != nil {
		p = node.Pos()
	}

	if p.Line == 0 {
		p = cg.currentPos
	}

	if p.Line == 0 {
		return cg.filename
	}

	return fmt.Sprintf("%s:%d:%d", cg.filename, p.Line, p.Col)
}

// nodeErr returns an error prefixed with the source location of node.
func (cg *CodeGen) nodeErr(node ast.Node, format string, args ...interface{}) error {
	return fmt.Errorf("%s: %s", cg.posStr(node), latexToUnicode(fmt.Sprintf(format, args...)))
}

// nodeErrSpan is nodeErr with an explicit end column, so the snippet
// renderer underlines the entire `[startCol, endCol]` range instead of
// applying its identifier/operator heuristic. Use when the offending
// region is wider than a single token (e.g. a whole call site, a let
// declaration spanning the line).
//
// endCol is 1-indexed and inclusive. When endCol <= startCol the range
// degrades to a single-column caret -- callers can pass a sentinel
// like `len(line)` to extend to end-of-line.
func (cg *CodeGen) nodeErrSpan(node ast.Node, endCol int, format string, args ...interface{}) error {
	var p ast.Pos
	if node != nil {
		p = node.Pos()
	}

	if p.Line == 0 {
		p = cg.currentPos
	}

	msg := latexToUnicode(fmt.Sprintf(format, args...))

	if p.Line == 0 {
		return fmt.Errorf("%s: %s", cg.filename, msg)
	}

	if endCol <= p.Col {
		return fmt.Errorf("%s:%d:%d: %s", cg.filename, p.Line, p.Col, msg)
	}

	return fmt.Errorf("%s:%d:%d-%d: %s", cg.filename, p.Line, p.Col, endCol, msg)
}

// sourceLineEndCol returns the 1-indexed end column of `lineNum` in
// the current source file. Used for "underline to end-of-line" spans
// (e.g. let declarations whose AST node only carries the start
// position). Returns 0 when the file isn't readable -- the caller
// should fall back to a single-column caret.
func (cg *CodeGen) sourceLineEndCol(lineNum int) int {
	if cg.filename == "" || lineNum <= 0 {
		return 0
	}

	src, ok := readSourceLine(cg.filename, lineNum)
	if !ok {
		return 0
	}
	// Strip trailing whitespace so the caret doesn't extend over
	// blank padding the user can't see.
	src = strings.TrimRight(src, " \t")

	return len(src)
}

// displayStructName returns the user-facing name for a struct canonical key.
// Package-qualified structs like "http__Client" are presented as "http::Client".
// Bare names (user-level structs) are returned unchanged.
func (cg *CodeGen) displayStructName(canonicalKey string) string {
	if dn := cg.displayFor(CanonKey(canonicalKey)); dn != "" {
		return dn
	}

	return canonicalKey
}

// diagStructName returns the user-facing Pretty form of a canonical key,
// routing through TypeName so monomorphized generics with package-qualified
// args render with `::` separators and structured `[...]` brackets, and so
// trait fat-pointer struct names (`pkg__Trait_iface`) lose their internal
// `_iface` suffix.  Reflection helpers (typeof, etc.) keep the raw key via
// displayStructName -- the canonical key doubles as a stable id.
func (cg *CodeGen) diagStructName(canonicalKey string) string {
	return cg.typeNameFromCanon(canonicalKey).Pretty
}

// tinTypeDisplay returns a user-facing description of an LLVM type using
// Tin syntax: `decimal::Value` rather than the internal `%decimal__Value`,
// `*Box` rather than `%Box*`, `[decimal::Value]` for fat arrays, and so
// on. Used in diagnostic strings so errors don't leak the package-mangling
// scheme back at the user.
func (cg *CodeGen) tinTypeDisplay(t irtypes.Type) string {
	if t == nil {
		return "void"
	}

	switch tt := t.(type) {
	case *irtypes.PointerType:
		return "*" + cg.tinTypeDisplay(tt.ElemType)
	case *irtypes.ArrayType:
		return "[" + cg.tinTypeDisplay(tt.ElemType) + "]"
	case *irtypes.VectorType:
		return cg.tinTypeDisplay(tt.ElemType) + "x" + fmt.Sprintf("%d", tt.Len)
	case *irtypes.StructType:
		// Anonymous structs that the compiler uses for fat pointers: surface
		// them as the user-facing equivalent.
		if tt.Name() == "" {
			if isStringType(tt) {
				return "string"
			}

			if isAnyType(tt) {
				return "any"
			}

			if isFatArrayPtr(tt) && len(tt.Fields) == 3 {
				if pt, ok := tt.Fields[0].(*irtypes.PointerType); ok {
					return "[" + cg.tinTypeDisplay(pt.ElemType) + "]"
				}
			}
		}
	}

	name := llvmTypeName(t)
	// Route through TypeName so monomorphized generic struct names with
	// package-qualified args ("Box__pkg__Inner__i64") render as
	// "Box[pkg::Inner, i64]" instead of the lossy "Box[pkg, Inner, i64]"
	// that prettyStructName's `__`-split alone produces.
	if pretty := cg.typeNameFromCanon(name).Pretty; pretty != name {
		return pretty
	}

	return name
}

// buildClosureEnv heap-allocates an RC-managed env struct for lambda closure captures.
// Layout: { i8* dtor_fn_ptr, capture_0, capture_1, ... } (dtor at field 0).
// All RC-tracked captures are retained so the env independently owns them.
// dtorFn may be nil if there are no RC-tracked captures (dtor slot is set to null).
