package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// findEscapingAddressTakenVars performs a lightweight escape analysis on a
// function body.  It returns the set of local variable names whose addresses
// escape the function frame - i.e. a pointer to them is returned from the
// function.  These variables will be heap-promoted: genVarDecl allocates them
// with malloc instead of alloca so the memory remains valid after the callee
// returns.
//
// Patterns detected:
//
//	return &varName            -- address of local returned directly
//	let alias = &varName
//	return alias               -- address returned via an alias variable
func findEscapingAddressTakenVars(body ast.Node) (map[string]bool, map[string]string) {
	if body == nil {
		return nil, nil
	}
	// Pass 1: collect address-of aliases.  aliases[alias] = source variable name.
	aliases := make(map[string]string)
	walkForAliases(body, aliases)

	// Pass 2: collect escaping variables by inspecting return statements.
	escaping := make(map[string]bool)
	walkForEscapes(body, aliases, escaping)

	if len(escaping) == 0 {
		return nil, nil
	}

	return escaping, aliases
}

// walkForAliases walks node and populates aliases: for every
//
//	let name = &ident
//
// it records aliases[name] = ident.Name.
func walkForAliases(node ast.Node, aliases map[string]string) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.VarDecl:
		if n.Value != nil {
			switch v := n.Value.(type) {
			case *ast.AddressOfExpr:
				// `let alias = &ident`: alias targets ident's storage.
				if ident, ok := v.Expr.(*ast.Identifier); ok {
					aliases[n.Name] = ident.Name
				}
			case *ast.Identifier:
				// `let alias2 = alias1`: alias2 walks back through alias1
				// to whatever alias1 ultimately targets. Only record when
				// alias1 is itself in the chain - otherwise the binding
				// is just a value copy with no escape implications.
				if src, ok := aliases[v.Name]; ok {
					aliases[n.Name] = src
				}
			}
		}
	case *ast.Block:
		for _, s := range n.Stmts {
			walkForAliases(s, aliases)
		}
	case *ast.IfStmt:
		if n.Then != nil {
			walkForAliases(n.Then, aliases)
		}

		for _, elif := range n.ElseIfs {
			walkForAliases(elif.Body, aliases)
		}

		if n.Else != nil {
			walkForAliases(n.Else, aliases)
		}
	case *ast.ForStmt:
		if n.Body != nil {
			walkForAliases(n.Body, aliases)
		}
	case *ast.MatchStmt:
		for _, c := range n.Cases {
			walkForAliases(c.Body, aliases)
		}

		if n.Default != nil {
			walkForAliases(n.Default, aliases)
		}
	}
}

// walkForEscapes walks node and populates escaping: for every ReturnStmt
// whose value is &ident or an alias of &ident, marks the source variable.
func walkForEscapes(node ast.Node, aliases map[string]string, escaping map[string]bool) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.ReturnStmt:
		if n.Value == nil {
			return
		}

		markEscapeVal(n.Value, aliases, escaping)
	case *ast.Block:
		for _, s := range n.Stmts {
			walkForEscapes(s, aliases, escaping)
		}
	case *ast.IfStmt:
		if n.Then != nil {
			walkForEscapes(n.Then, aliases, escaping)
		}

		for _, elif := range n.ElseIfs {
			walkForEscapes(elif.Body, aliases, escaping)
		}

		if n.Else != nil {
			walkForEscapes(n.Else, aliases, escaping)
		}
	case *ast.ForStmt:
		if n.Body != nil {
			walkForEscapes(n.Body, aliases, escaping)
		}
	case *ast.MatchStmt:
		for _, c := range n.Cases {
			walkForEscapes(c.Body, aliases, escaping)
		}

		if n.Default != nil {
			walkForEscapes(n.Default, aliases, escaping)
		}
	}
}

// markEscapeVal marks variables in aliases that escape via the given return value.
// Handles identifiers, address-of expressions, tuples, and struct literals
// containing those -- `return &Box{p: &x}` recursively walks the struct
// initializers and marks &x's source as escaping. Alias chains are followed
// transitively: if `ppx = &px` and `px = &x`, returning ppx marks both px
// and x as escaping.
func markEscapeVal(val ast.Node, aliases map[string]string, escaping map[string]bool) {
	if val == nil {
		return
	}

	switch rv := val.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok {
			markEscapeChain(ident.Name, aliases, escaping)
		}
		// `return &StructLit{...}`: also walk the struct's field initializers.
		// The struct itself is heap-promoted via existing &StructLit handling;
		// any `&local` inside its fields would otherwise stay stack-allocated
		// and dangle once the function returns.
		if sl, ok := rv.Expr.(*ast.StructLit); ok {
			markEscapeStructLit(sl, aliases, escaping)
		}
	case *ast.StructLit:
		// `return Struct{...}` (by value): same as above, the struct's
		// returned fields outlive the frame, so any &local inside them
		// must be promoted.
		markEscapeStructLit(rv, aliases, escaping)
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok {
			markEscapeChain(src, aliases, escaping)
		}
	case *ast.TupleLit:
		for _, elem := range rv.Elems {
			markEscapeVal(elem, aliases, escaping)
		}
	}
}

// markEscapeStructLit walks every field initializer of sl, treating each
// as if it were itself a return value: an &Identifier or alias-of-&Ident
// in any field marks the underlying var as escaping.
func markEscapeStructLit(sl *ast.StructLit, aliases map[string]string, escaping map[string]bool) {
	for _, f := range sl.Fields {
		markEscapeVal(f.Value, aliases, escaping)
	}

	for _, p := range sl.Positional {
		markEscapeVal(p, aliases, escaping)
	}
}

// markEscapeChain transitively marks name and all its alias sources as escaping.
func markEscapeChain(name string, aliases map[string]string, escaping map[string]bool) {
	for name != "" && !escaping[name] {
		escaping[name] = true
		name = aliases[name] // follow the chain: if px = &x, also mark x
	}
}

// hasDirectHeapReturn returns true if any return statement in body returns a
// freshly heap-allocated pointer without going through a named local variable.
// This covers two patterns not caught by findEscapingAddressTakenVars:
//
//	return &StructLit{...}        -- inline heap allocation in return position
//	return heap_fn(args...)       -- forwarding the result of a heap-promoting fn
//
// heapFns is the current heapPromotingFns map so that callee lookups work for
// functions already processed (defined before the current one in the same file).
func hasDirectHeapReturn(body ast.Node, heapFns map[string]bool) bool {
	if body == nil {
		return false
	}

	// isHeapExpr reports whether expr is a direct heap allocation:
	//   &StructLit{...}    -- address of an inline struct literal
	//   heap_fn(args...)   -- call to an already-registered heap-promoting function
	isHeapExpr := func(expr ast.Node) bool {
		switch rv := expr.(type) {
		case *ast.AddressOfExpr:
			_, ok := rv.Expr.(*ast.StructLit)

			return ok
		case *ast.CallExpr:
			switch fn := rv.Func.(type) {
			case *ast.Identifier:
				return heapFns[fn.Name]
			case *ast.ScopeAccess:
				return heapFns[strings.Join(fn.Path, "__")]
			}
		}

		return false
	}

	// Track variables bound to heap-promoting calls: let x = heap_fn()
	heapVars := map[string]bool{}

	found := false

	var walk func(ast.Node)

	walk = func(node ast.Node) {
		if node == nil || found {
			return
		}

		switch n := node.(type) {
		case *ast.VarDecl:
			// let x = heap_fn() -> track x as a heap variable.
			if n.Value != nil && isHeapExpr(n.Value) {
				heapVars[n.Name] = true
			}
		case *ast.ReturnStmt:
			if n.Value == nil {
				return
			}

			if isHeapExpr(n.Value) {
				found = true
			} else if id, isID := n.Value.(*ast.Identifier); isID && heapVars[id.Name] {
				// return x where x was bound to a heap-promoting call.
				found = true
			} else if tl, isTuple := n.Value.(*ast.TupleLit); isTuple {
				for _, elem := range tl.Elems {
					if isHeapExpr(elem) {
						found = true

						break
					}
				}
			}
		case *ast.Block:
			for _, s := range n.Stmts {
				walk(s)
			}
		case *ast.IfStmt:
			if n.Then != nil {
				walk(n.Then)
			}

			for _, elif := range n.ElseIfs {
				if elif.Body != nil {
					walk(elif.Body)
				}
			}

			if n.Else != nil {
				walk(n.Else)
			}
		case *ast.ForStmt:
			if n.Body != nil {
				walk(n.Body)
			}
		case *ast.MatchStmt:
			for _, c := range n.Cases {
				if c.Body != nil {
					walk(c.Body)
				}
			}

			if n.Default != nil {
				walk(n.Default)
			}
		}
	}

	walk(body)

	return found
}

// retainedHeapVars returns the subset of escaping vars that are actually returned
// by retExpr.  Any heap-promoted var NOT in this set can be freed at the return site.
// Uses the same resolution logic as markEscapeVal/markEscapeChain.
func retainedHeapVars(retExpr ast.Node, aliases map[string]string, escaping map[string]bool) map[string]bool {
	kept := make(map[string]bool)
	collectRetained(retExpr, aliases, escaping, kept)

	return kept
}

func collectRetained(node ast.Node, aliases map[string]string, escaping map[string]bool, kept map[string]bool) {
	if node == nil {
		return
	}

	switch rv := node.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok {
			collectChain(ident.Name, aliases, escaping, kept)
		}
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok {
			collectChain(src, aliases, escaping, kept)
		}
	case *ast.TupleLit:
		for _, elem := range rv.Elems {
			collectRetained(elem, aliases, escaping, kept)
		}
	}
}

func collectChain(name string, aliases map[string]string, escaping map[string]bool, kept map[string]bool) {
	for name != "" && escaping[name] && !kept[name] {
		kept[name] = true
		name = aliases[name]
	}
}

// pointerChainDepth counts the number of consecutive pointer dereferences in t.
// Returns 0 for non-pointer types, 1 for *T, 2 for **T, etc.
func pointerChainDepth(t irtypes.Type) int {
	depth := 0

	for {
		pt, ok := t.(*irtypes.PointerType)
		if !ok {
			break
		}

		depth++
		t = pt.ElemType
	}

	return depth
}

// genLatePromotedReturn handles return statements in functions where one or more
// local variables escape via their address.  The key invariant: defers may modify
// the stack copies of promoted variables, so we run defers FIRST, then copy the
// post-defer values into fresh _tin_rc_alloc blocks and return those.
//
// For tuple returns, non-promoted elements are latched BEFORE defers run so that
// the caller sees the pre-defer values (the same semantics as early-promotion).
func (cg *CodeGen) genLatePromotedReturn(block *ir.Block, s *ast.ReturnStmt, promoted map[string]bool) error {
	retType := cg.curFn.Sig.RetType

	if tup, ok := s.Value.(*ast.TupleLit); ok {
		structType, ok2 := retType.(*irtypes.StructType)
		if !ok2 {
			return fmt.Errorf("genLatePromotedReturn: expected struct type for tuple return, got %v", retType)
		}

		concreteName := structType.Name()
		userOff := cg.userFieldOffset(concreteName)

		// Phase 1: latch non-promoted elements BEFORE defers run.
		type latched struct {
			val      value.Value
			retained bool
		}

		preLatch := make([]latched, len(tup.Elems))
		for i, elem := range tup.Elems {
			if isPromotedTupleElem(elem, cg.curFnEscapingAliases, promoted) {
				continue
			}

			v, err := cg.genExpr(block, elem)
			if err != nil {
				return err
			}

			fi := userOff + i
			if v != nil && fi < len(structType.Fields) {
				v = cg.coerce(block, v, structType.Fields[fi])
			}

			retained := false

			if isCopyExpr(elem) {
				cg.emitRetain(block, v)

				retained = true
			}

			preLatch[i] = latched{val: v, retained: retained}
		}

		// Run defers (may modify stack copies of promoted vars).
		if err := cg.emitDefers(block); err != nil {
			return err
		}

		// Phase 2: build the result tuple.
		alloca := block.NewAlloca(structType)
		block.NewStore(constant.NewZeroInitializer(structType), alloca)

		if typeID, has := cg.structTypeIDs[concreteName]; has {
			typeIDGep := block.NewGetElementPtr(structType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
		}

		for i, elem := range tup.Elems {
			fi := userOff + i
			if fi >= len(structType.Fields) {
				break
			}

			var v value.Value

			if isPromotedTupleElem(elem, cg.curFnEscapingAliases, promoted) {
				rootVar := promotedTupleElemVar(elem, cg.curFnEscapingAliases, promoted)

				var err error

				v, err = cg.emitChainedHeapPromotion(block, rootVar)
				if err != nil {
					return err
				}
			} else {
				v = preLatch[i].val
			}

			if v == nil {
				continue
			}

			v = cg.coerce(block, v, structType.Fields[fi])
			gep := block.NewGetElementPtr(structType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			block.NewStore(v, gep)
		}

		retVal := block.NewLoad(structType, alloca)
		cg.emitAllScopeReleases(block, "")
		block.NewRet(retVal)

		return nil
	}

	// Non-tuple: `return &x` or `return p` (p = &x).
	// No pre-defer latching needed - just run defers and build the RC block.
	if err := cg.emitDefers(block); err != nil {
		return err
	}

	rootVar := latePromotionRootVar(s.Value, cg.curFnEscapingAliases, promoted)
	if rootVar == "" {
		return fmt.Errorf("genLatePromotedReturn: cannot find promoted root var in %T", s.Value)
	}

	retVal, err := cg.emitChainedHeapPromotion(block, rootVar)
	if err != nil {
		return err
	}

	if cg.curFn != nil && !irtypes.IsVoid(retType) {
		retVal = cg.coerce(block, retVal, retType)
	}

	// Build the set of names in the returned chain so we don't release
	// the var(s) whose ownership we just transferred to the caller.
	chainSet := map[string]bool{rootVar: true}
	cur := rootVar

	for {
		next, ok := cg.curFnEscapingAliases[cur]
		if !ok || next == "" || !promoted[next] {
			break
		}

		chainSet[next] = true
		cur = next
	}
	// Release any early-heap'd locals NOT in the return chain.  This
	// covers patterns like `if cond: return &yes else: return &no`
	// where both `yes` and `no` were heap-allocated at let-decl time
	// (escape analysis flags both because EITHER could be returned),
	// but only one is actually returned on each path -- the other's
	// heap block would otherwise leak.
	cg.releaseUnreturned(block, chainSet)

	cg.emitAllScopeReleases(block, "")
	block.NewRet(retVal)

	return nil
}

// releaseUnreturned releases heap-allocated locals (early-heap'd by
// escape analysis) that are NOT part of the returned chain on this
// path.  Used at late-promoted return sites to avoid leaking variables
// whose address was taken on a different control-flow path.
func (cg *CodeGen) releaseUnreturned(block *ir.Block, transferred map[string]bool) {
	if cg.curScope == nil {
		return
	}

	for name := range cg.curFnEscapingVars {
		if transferred[name] {
			continue
		}

		entry, ok := cg.curScope.lookup(name)
		if !ok || !entry.isAlloc || !entry.isEarlyHeap {
			continue
		}

		// entry.val is the heap pointer (rc=1 from earlyHeap alloc).
		// Pick the most specific release helper for the element type
		// so RC-tracked sub-fields are torn down on free.  Pre-fix the
		// non-named-struct path called raw _tin_release, which freed
		// the outer block but never released sub-fields -- a real
		// leak for fat-array / string / any / fat-fn elements (e.g.
		// `let xs [string] = [..]` early-heap'd because `&xs` is
		// taken on a sibling branch).
		ptrType, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			continue
		}

		if innerSt, isStruct := ptrType.ElemType.(*irtypes.StructType); isStruct && innerSt.Name() != "" {
			relFn := cg.ensureStructPtrReleaseFn(innerSt.Name(), innerSt)
			block.NewCall(relFn, entry.val)

			continue
		}

		// Element type has no per-struct helper.  If the type carries
		// any RC-tracked subfield, route through the generic
		// heap-block helper (load + decrement + emitRelease on free).
		// Pure-i64 / pure-pointer / pure-bool elements have nothing to
		// release beyond the outer block, so fall through to raw
		// _tin_release for those.
		if cg.elemNeedsRelease(ptrType.ElemType) {
			relFn := cg.ensureHeapBlockReleaseFn(ptrType.ElemType)
			block.NewCall(relFn, entry.val)

			continue
		}

		i8 := block.NewBitCast(entry.val, irtypes.I8Ptr)
		block.NewCall(cg.ensureRelease(), i8)
	}
}

// emitChainedHeapPromotion promotes rootVar (and all variables in its alias chain
// that are in curFnEscapingVars) from stack to ARC heap blocks.  Inner variables
// (further down the alias chain) are promoted first; each parent's alloca is then
// updated to hold the child's heap pointer before the parent is promoted.  This
// ensures that returned pointer chains are fully heap-resident.
func (cg *CodeGen) emitChainedHeapPromotion(block *ir.Block, rootVar string) (value.Value, error) {
	aliases := cg.curFnEscapingAliases
	promoted := cg.curFnEscapingVars

	// Build the chain from rootVar following alias links in promoted.
	chain := []string{rootVar}
	for {
		cur := chain[len(chain)-1]

		next, ok := aliases[cur]
		if !ok || next == "" || !promoted[next] {
			break
		}

		chain = append(chain, next)
	}

	// heapPtrs maps varName -> its heap block pointer (typed as *T for T = element type).
	heapPtrs := make(map[string]value.Value)

	// Promote from leaf (last in chain) to root (first in chain).
	for i := len(chain) - 1; i >= 0; i-- {
		varName := chain[i]

		entry, ok := cg.curScope.lookup(varName)
		if !ok || !entry.isAlloc {
			return nil, fmt.Errorf("emitChainedHeapPromotion: var %q not found in scope", varName)
		}

		ptrType, ok2 := entry.val.Type().(*irtypes.PointerType)
		if !ok2 {
			return nil, fmt.Errorf("emitChainedHeapPromotion: var %q alloca not a pointer type", varName)
		}

		elemType := ptrType.ElemType

		// If this var points to a child that was just promoted, update the alloca
		// so it holds the child's heap pointer instead of the child's stack address.
		if i < len(chain)-1 {
			childHeapPtr := heapPtrs[chain[i+1]]
			childCast := block.NewBitCast(childHeapPtr, elemType)
			block.NewStore(childCast, entry.val)
		}

		// If this var was already early-heap-allocated at let-decl time
		// (entry.val IS the heap pointer with rc=1), reuse it directly
		// rather than copying into a fresh second heap block.  Without
		// this, the original heap allocation is orphaned and leaks while
		// only the new copy is returned.  The retain below still fires
		// (load value + retain its sub-fields) so that the local scope's
		// release balances out without dropping ARC sub-fields to zero.
		//
		// Extra heap-pointer retain: when the function ALSO constructs an
		// owning *Trait iface that borrows this same heap (`let p *Trait
		// = &pt; return p` -- the iface dtor's data-release thunk
		// decrements the heap block on scope exit), we need an explicit
		// _tin_retain on the heap so the iface release doesn't free the
		// block out from under the returned chain.  Detected via
		// cg.fnReturnsOwningIface, set by buildPtrToTraitBorrow when it
		// fires inside a fn with escaping vars.  Plain `return &local`
		// (no iface) doesn't need this retain because the local scope
		// already skips isEarlyHeap vars at scope exit.
		if entry.isEarlyHeap {
			stackVal := block.NewLoad(elemType, entry.val)
			cg.emitRetain(block, stackVal)

			if cg.curFn != nil && cg.fnReturnsOwningIface[cg.curFn.Name()] {
				heapI8 := block.NewBitCast(entry.val, irtypes.I8Ptr)
				block.NewCall(cg.ensureRetain(), heapI8)
			}

			heapPtrs[varName] = entry.val

			continue
		}

		// Load the (potentially updated) value from the stack alloca.
		stackVal := block.NewLoad(elemType, entry.val)

		// Allocate ARC block and copy the value into it.
		// For cLayoutStructs, also allocate overflow space for the native data
		// so that c_data_ptr does not dangle after the stack frame exits.
		sz := cg.llvmSizeOf(block, elemType)
		sName := cg.typeNameOf(elemType)
		isCLayout := sName != "" && cg.cLayoutStructs[sName]

		var nativeSt *irtypes.StructType

		var nativeSize value.Value

		if isCLayout {
			nativeSt = cg.nativeStructTypes[sName]
			if nativeSt != nil {
				nativeSize = cg.llvmSizeOf(block, nativeSt)
				sz = block.NewAdd(sz, nativeSize)
			} else {
				isCLayout = false
			}
		}

		heapI8 := block.NewCall(cg.ensureRCAlloc(), sz)
		heapPtr := block.NewBitCast(heapI8, irtypes.NewPointer(elemType))
		block.NewStore(stackVal, heapPtr)

		if isCLayout {
			// Load c_data_ptr from the just-copied wrapper.
			tinSt := cg.structTypeFor(CanonKey(sName))
			cDataIdx := int64(cg.cDataPtrIndex(sName))
			cDataGep := block.NewGetElementPtr(tinSt, heapPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
			oldCDataPtr := block.NewLoad(irtypes.I8Ptr, cDataGep)

			// Overflow area sits just past the wrapper in the same RC block.
			overflowGEP := block.NewGetElementPtr(tinSt, heapPtr, constant.NewInt(irtypes.I64, 1))
			overflowI8 := block.NewBitCast(overflowGEP, irtypes.I8Ptr)

			// Copy native data from the old location (stack alloca or C pointer)
			// into the overflow area, then update c_data_ptr.
			block.NewCall(cg.ensureMemcpy(), overflowI8, oldCDataPtr, nativeSize, constant.NewInt(irtypes.I1, 0))
			block.NewStore(overflowI8, cDataGep)
		}

		// Retain ARC sub-fields (strings, arrays) so scope cleanup on the stack
		// copy is balanced.  For plain i64/pointers this is a no-op.
		cg.emitRetain(block, stackVal)

		heapPtrs[varName] = heapPtr
	}

	return heapPtrs[rootVar], nil
}

// heapPromotedFieldIndices walks a struct literal and returns the
// LLVM field offset (factoring in the leading i32 type_id slot at
// offset 0) for every field whose value is `&local` where local was
// flagged as escaping by the current function's escape analysis,
// AND whose declared field type is a raw pointer to a primitive --
// `*KnownTinStruct` fields go through walkRCStructFields's
// isTinStructPtr path on the regular release helper, so emitting a
// second `_tin_release` here would walk freed memory.  Used by the
// genReturn path to record per-function metadata that callers
// consume to cascade-release the heap blocks.
func (cg *CodeGen) heapPromotedFieldIndices(sl *ast.StructLit) []int {
	if sl == nil {
		return nil
	}

	structName := sl.TypeName

	var out []int

	for i, f := range sl.Fields {
		ao, ok := f.Value.(*ast.AddressOfExpr)
		if !ok {
			continue
		}

		id, ok := ao.Expr.(*ast.Identifier)
		if !ok {
			continue
		}
		// Direct heap-promoted var, OR an alias whose source is.
		name := id.Name
		if src, isAlias := cg.curFnEscapingAliases[name]; isAlias && src != "" {
			name = src
		}

		if !cg.curFnEscapingVars[name] {
			continue
		}
		// Skip fields whose LLVM type is a pointer to a known Tin
		// struct -- the per-struct release helper already cascades
		// through them via isTinStructPtr.  Releasing here would
		// double-free the underlying block.
		if cg.fieldIsKnownStructPointer(structName, i) {
			continue
		}
		// Field 0 is the type_id; declared field i lives at LLVM
		// offset i+1 in the struct's IR layout.
		out = append(out, i+1)
	}

	return out
}

// fieldIsKnownStructPointer reports whether the i'th declared field
// of structName has type `*<known Tin struct>` -- i.e. the per-
// struct release helper already walks it via `walkRCStructFields`'s
// isTinStructPtr path.  Returns false on every other shape so the
// cascade keeps releasing raw `*primitive` heap-promoted pointers.
func (cg *CodeGen) fieldIsKnownStructPointer(structName string, fieldIdx int) bool {
	if structName == "" {
		return false
	}

	fts := cg.structFieldLLVMTypes[structName]
	if fieldIdx < 0 || fieldIdx >= len(fts) {
		return false
	}

	pt, ok := fts[fieldIdx].(*irtypes.PointerType)
	if !ok {
		return false
	}

	innerSt, ok := pt.ElemType.(*irtypes.StructType)
	if !ok || innerSt.Name() == "" {
		return false
	}

	return cg.structTypeFor(CanonKey(innerSt.Name())) != nil
}

// mergeFieldIndices unions two []int field-index lists, preserving
// order and dropping duplicates.  Used so multiple return paths in
// the same function accumulate every heap-promoted field they touch.
func mergeFieldIndices(a, b []int) []int {
	seen := map[int]bool{}

	out := make([]int, 0, len(a)+len(b))
	for _, v := range a {
		if !seen[v] {
			seen[v] = true

			out = append(out, v)
		}
	}

	for _, v := range b {
		if !seen[v] {
			seen[v] = true

			out = append(out, v)
		}
	}

	return out
}

// latePromotionRootVar extracts the name of the underlying escaping variable from
// a simple return expression: `return &x` -> "x", `return p` (p=&x) -> "x".
func latePromotionRootVar(node ast.Node, aliases map[string]string, promoted map[string]bool) string {
	switch rv := node.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok && promoted[ident.Name] {
			return ident.Name
		}
	case *ast.Identifier:
		if src, ok := aliases[rv.Name]; ok && promoted[src] {
			return src
		}
		// Also handle direct identifier that is itself the promoted var
		if promoted[rv.Name] {
			return rv.Name
		}
	}

	return ""
}

// isPromotedTupleElem reports whether a tuple element is a promoted pointer
// (either &x where x is promoted, or an alias identifier p where aliases[p] is promoted).
func isPromotedTupleElem(elem ast.Node, aliases map[string]string, promoted map[string]bool) bool {
	return promotedTupleElemVar(elem, aliases, promoted) != ""
}

// promotedTupleElemVar returns the root escaping variable name for a promoted
// tuple element, or "" if the element is not promoted.
func promotedTupleElemVar(elem ast.Node, aliases map[string]string, promoted map[string]bool) string {
	switch e := elem.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := e.Expr.(*ast.Identifier); ok && promoted[ident.Name] {
			return ident.Name
		}
	case *ast.Identifier:
		if src, ok := aliases[e.Name]; ok && promoted[src] {
			return src
		}
	}

	return ""
}

// cLayoutBindingEscapesForType reports whether `name` appears in any
// position other than `name.field` / `name[idx]` receiver, or
// `name = newval` assignment target, within body.  Conservative: any
// node type the walker doesn't explicitly understand is treated as
// escape, since the binding's name might appear inside it.  Used by
// genVarDecl to decide whether the hidden out_native buffer for a
// cLayoutStruct extern wrapper return can be stack-allocated
// (non-escape) or must be _tin_rc_alloc'd (escape).
//
// When structTypeName is non-empty AND its fields include cLayoutStruct
// values, `name.<cLayoutField>` reads are treated as escape sites: the
// field-access result is a cLayoutStruct value whose c_data_ptr points
// into name's storage, so consuming the value in any context (let-bind,
// return, call-arg, struct-lit field, ...) propagates that storage
// reference past name's intended scope.  Scalar-typed fields stay in
// receiver position because their values don't carry storage refs.
//
// For CallExpr-RHS bindings (where the binding type IS the cLayoutStruct
// itself) pass structTypeName="" -- cLayoutStructs only carry primitive
// fields in current Tin code, so the simpler receiver-treat-all-fields
// rule applies.
func (cg *CodeGen) cLayoutBindingEscapesForType(name, structTypeName string, body ast.Node) bool {
	if body == nil {
		return false
	}

	// Build a set of cLayoutStruct-typed field names on the binding's
	// struct type, if known.  Field accesses into these fields propagate
	// the binding's storage (the field value carries c_data_ptr into the
	// binding's composite), so they must be treated as escape when the
	// value is read into an escape position.  Unresolved field types
	// (generic type parameters, forward refs) are conservatively marked
	// as cLayout so a Holder[T] whose T monomorphizes to a cLayoutStruct
	// can't silently slip past the check.
	var cLayoutFields map[string]bool

	if structTypeName != "" {
		fieldNames := cg.structFields[structTypeName]
		fieldTypes := cg.structFieldTinTypes[structTypeName]

		if len(fieldNames) > 0 && len(fieldTypes) == len(fieldNames) {
			cLayoutFields = make(map[string]bool, len(fieldNames))

			for i, fn := range fieldNames {
				if cg.fieldTypeCarriesCLayoutStorage(fieldTypes[i]) {
					cLayoutFields[fn] = true
				}
			}
		}
	}

	var found bool

	var walk func(n ast.Node)

	var receiverWalk func(n ast.Node)

	// receiverWalk is the receiver-position variant of walk: a chain like
	// `name.f1.f2.f3` ends in three nested FieldAccess nodes whose
	// innermost Expr is the binding identifier; every level is a receiver
	// of its parent and none of them consume the binding's value, so
	// `name` and `name.cLayoutField` are BOTH allowed here without
	// triggering escape.  The cLayoutField rule only escapes when the
	// FieldAccess sits in escape position (walk's FieldAccess branch) --
	// i.e. its result is the value of a let-decl, return, call arg,
	// struct-lit field initializer, etc.  Calling receiverWalk on
	// anything other than an Identifier / FieldAccess / IndexExpr falls
	// back to the general walk and triggers normal escape semantics.
	receiverWalk = func(n ast.Node) {
		if n == nil {
			return
		}

		switch r := n.(type) {
		case *ast.Identifier:
			if r == nil || r.Name == name {
				return
			}

			walk(r)
		case *ast.FieldAccess:
			if r == nil {
				return
			}

			receiverWalk(r.Expr)
		case *ast.IndexExpr:
			if r == nil {
				return
			}

			receiverWalk(r.Expr)
			walk(r.Index)
		default:
			walk(n)
		}
	}

	walk = func(n ast.Node) {
		if n == nil || found {
			return
		}

		switch v := n.(type) {
		case *ast.Identifier:
			if v == nil {
				return
			}

			if v.Name == name {
				found = true
			}
		case *ast.FieldAccess:
			if v == nil {
				return
			}

			// `name.cLayoutField` reads the inner cLayout struct value
			// out of the binding -- the resulting value's c_data_ptr is
			// still pointing into name's storage.  Consuming it anywhere
			// (let-bind, return, call-arg, ...) propagates the storage
			// reference past name's stack-bound frame, so this is an
			// escape regardless of context.  Scalar / non-cLayout fields
			// stay in receiver position (their values don't carry the
			// storage ref).
			if id, ok := v.Expr.(*ast.Identifier); ok && id.Name == name {
				if cLayoutFields[v.Field] {
					found = true

					return
				}

				return
			}

			receiverWalk(v.Expr)
		case *ast.IndexExpr:
			if v == nil {
				return
			}

			receiverWalk(v.Expr)
			walk(v.Index)
		case *ast.AssignStmt:
			if v == nil {
				return
			}

			receiverWalk(v.Target)
			walk(v.Value)
		case *ast.AugAssignStmt:
			if v == nil {
				return
			}

			receiverWalk(v.Target)
			walk(v.Value)
		case *ast.PostfixStmt:
			if v == nil {
				return
			}

			receiverWalk(v.Expr)
		case *ast.Block:
			if v == nil {
				return
			}

			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.ExprStmt:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.VarDecl:
			if v == nil {
				return
			}

			walk(v.Value)
		case *ast.ReturnStmt:
			if v == nil {
				return
			}

			walk(v.Value)
		case *ast.DeferStmt:
			if v == nil {
				return
			}

			walk(v.Call)
		case *ast.IfStmt:
			if v == nil {
				return
			}

			walk(v.Cond)
			walk(v.Then)

			for _, ei := range v.ElseIfs {
				walk(ei.Cond)
				walk(ei.Body)
			}

			walk(v.Else)
		case *ast.ForStmt:
			if v == nil {
				return
			}

			walk(v.Init)
			walk(v.Cond)
			walk(v.Post)
			walk(v.Iter)
			walk(v.Body)
		case *ast.MatchStmt:
			if v == nil {
				return
			}

			walk(v.Expr)

			for _, c := range v.Cases {
				walk(c.Pattern)
				walk(c.Guard)
				walk(c.Body)
			}

			walk(v.Default)
		case *ast.EchoStmt:
			if v == nil {
				return
			}

			walk(v.Value)
		case *ast.CallExpr:
			if v == nil {
				return
			}

			walk(v.Func)

			for _, a := range v.Args {
				walk(a)
			}
		case *ast.BinExpr:
			if v == nil {
				return
			}

			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.TernaryExpr:
			if v == nil {
				return
			}

			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.PipeExpr:
			if v == nil {
				return
			}

			walk(v.Left)
			walk(v.Right)
		case *ast.LambdaExpr:
			if v == nil {
				return
			}

			walk(v.Body)
		case *ast.AddressOfExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.DerefExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.AsExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.IsExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.TypeAssertExpr:
			if v == nil {
				return
			}

			walk(v.Expr)
		case *ast.AwaitExpr:
			if v == nil {
				return
			}

			walk(v.Future)
		case *ast.SpawnExpr:
			if v == nil {
				return
			}

			walk(v.Call)
			walk(v.DoBlock)
		case *ast.StructLit:
			if v == nil {
				return
			}

			for _, f := range v.Fields {
				walk(f.Value)
			}

			for _, p := range v.Positional {
				walk(p)
			}
		case *ast.ArrayLit:
			if v == nil {
				return
			}

			for _, e := range v.Elems {
				walk(e)
			}
		case *ast.ArrayFillLit:
			if v == nil {
				return
			}

			walk(v.Value)
		case *ast.TupleLit:
			if v == nil {
				return
			}

			for _, e := range v.Elems {
				walk(e)
			}
		case *ast.RangeExpr:
			if v == nil {
				return
			}

			walk(v.Start)
			walk(v.End)
		case *ast.TaggedBlock:
			if v == nil {
				return
			}

			walk(v.Body)
		case *ast.InterpolatedString:
			if v == nil {
				return
			}

			for _, p := range v.Parts {
				if p.IsExpr {
					walk(p.Expr)
				}
			}
		case *ast.WhereList:
			if v == nil {
				return
			}

			for _, c := range v.Clauses {
				walk(c.Pattern)
				walk(c.Body)
			}
		case *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit,
			*ast.NilLit, *ast.AtomLit, *ast.CharLit, *ast.BacktickLit,
			*ast.BreakStmt, *ast.ScopeAccess, *ast.TypeRefNode,
			*ast.SizeofExpr, *ast.IsRCExpr, *ast.TypeofExpr,
			*ast.TraitofExpr, *ast.FieldnamesExpr, *ast.FieldtypesExpr,
			*ast.FieldtagExpr:
			// Leaf or type-introspection nodes -- no identifier escape route.
		default:
			// Unknown node type -- conservative: assume the binding might
			// be reachable through it.
			found = true
		}
	}

	walk(body)

	return found
}

// structHasPointerReceiverMethod reports whether any method bound to
// structName -- either declared directly on the struct or inherited via
// a trait it implements -- takes a pointer receiver (`fn ::m(this *S)` or
// `fn ::m(this *Trait)`).  Such a method, when called on a binding,
// materializes `&binding` to satisfy the receiver, so the binding's
// storage must outlive the callee.  Stack-binding such a value would let
// the callee write through a dangling pointer once the caller's frame
// ends, so escape analysis conservatively heap-allocates.
//
// Looks in two places:
//  1. funcDecls, for own methods and for explicit trait-impl methods
//     (those carry `this *S` after monomorphization).
//  2. structImpls, for traits the struct claims to implement -- their
//     TraitDecl.Methods describe the virtual / default-implemented
//     signature with `this *Trait`, which is the shape the caller sees
//     at the call site regardless of whether the struct supplied an
//     override or inherits the default.
//
// Considers any receiver whose pointee canonicalizes to structName, so
// generic instantiations (`Matrix[f64]` -> `Matrix__f64`) match through
// CanonKey.
func (cg *CodeGen) structHasPointerReceiverMethod(structName string) bool {
	// Flush stale cache entries when any input has changed shape since
	// the cache was last populated.  The three signals together catch:
	//   - len(funcDecls)        : new methods registered (adds).
	//   - len(structImpls)      : new structs registered as trait impls.
	//   - sum-of-impl-slice-len : new trait added to an existing
	//                             struct's impl slice (mutation of the
	//                             same key -- outer len unchanged but
	//                             surface grew).
	// Pure overwrites (same key, swapped value) aren't caught by any
	// of these and require an explicit InvalidateStructPtrReceiverCache
	// call (REPL flow).
	implsSumLen := 0
	for _, impls := range cg.structImpls {
		implsSumLen += len(impls)
	}

	if cg.structPtrReceiverCache != nil &&
		(cg.structPtrReceiverCacheFuncDeclsLen != len(cg.funcDecls) ||
			cg.structPtrReceiverCacheStructImplsN != len(cg.structImpls) ||
			cg.structPtrReceiverCacheStructImplsSumLen != implsSumLen) {
		cg.structPtrReceiverCache = nil
	}

	if cg.structPtrReceiverCache != nil {
		if hit, ok := cg.structPtrReceiverCache[structName]; ok {
			return hit
		}
	} else {
		cg.structPtrReceiverCache = make(map[string]bool)
		cg.structPtrReceiverCacheFuncDeclsLen = len(cg.funcDecls)
		cg.structPtrReceiverCacheStructImplsN = len(cg.structImpls)
		cg.structPtrReceiverCacheStructImplsSumLen = implsSumLen
	}

	result := cg.structHasPointerReceiverMethodUncached(structName)
	cg.structPtrReceiverCache[structName] = result

	return result
}

// InvalidateStructPtrReceiverCache drops the cached struct -> has-*this
// answers.  Callers that swap a funcDecl in place (same key, different
// receiver -- e.g. REPL cell re-evaluation) must call this so the next
// read sees the new shape; the size-based invalidation above doesn't
// notice value swaps that leave the map's len untouched.
func (cg *CodeGen) InvalidateStructPtrReceiverCache() {
	cg.structPtrReceiverCache = nil
}

func (cg *CodeGen) structHasPointerReceiverMethodUncached(structName string) bool {
	canonStruct := CanonKey(structName)

	receiverMatches := func(receiverElem string) bool {
		return receiverElem == structName || CanonKey(receiverElem) == canonStruct
	}

	for _, decl := range cg.funcDecls {
		if decl == nil || len(decl.Params) == 0 {
			continue
		}

		first := decl.Params[0]
		if first.Name != "this" {
			continue
		}

		pt, ok := first.Type.(*ast.PointerType)
		if !ok {
			continue
		}

		elem, ok2 := pt.Elem.(*ast.SimpleType)
		if !ok2 {
			continue
		}

		if receiverMatches(elem.Name) {
			return true
		}
	}

	// Trait-inherited *this methods: every trait the struct implements
	// contributes its method signatures to the struct's surface.  A
	// trait-declared `fn ::m(this *Trait)` becomes `&binding`-call on
	// the binding regardless of whether the struct provided an override
	// (a override's FuncDecl is already covered by the loop above; the
	// no-override / virtual / default-impl case lives in TraitDecl).
	for _, traitName := range cg.structImpls[structName] {
		traitRec, ok := cg.types[CanonKey(traitName)]
		if !ok || traitRec == nil || traitRec.Trait == nil {
			continue
		}

		for _, m := range traitRec.Trait.Methods {
			if m == nil || len(m.Params) == 0 {
				continue
			}

			first := m.Params[0]
			if first.Name != "this" {
				continue
			}

			if pt, ok := first.Type.(*ast.PointerType); ok {
				if _, isSimple := pt.Elem.(*ast.SimpleType); isSimple {
					return true
				}
			}
		}
	}

	return false
}

// currentFnAstBody returns the AST body of the function or test being
// codegen'd, or nil at module scope / synthetic helpers.  Prefers the
// explicitly-tracked curFnAstBody (set for both regular fns and tests),
// falls back to funcDecls lookup by IR name for legacy contexts.
func (cg *CodeGen) currentFnAstBody() ast.Node {
	if cg.curFnAstBody != nil {
		return cg.curFnAstBody
	}

	if cg.curFn == nil {
		return nil
	}

	if decl, ok := cg.funcDecls[cg.curFn.Name()]; ok && decl != nil {
		return decl.Body
	}

	return nil
}

// fieldTypeCarriesCLayoutStorage reports whether reading a field of this
// type from a stack-bound binding yields a value that carries a
// c_data_ptr referencing the binding's storage.  Returns true for known
// cLayoutStructs (including those reached transitively through type
// aliases or via a nested non-cLayout struct that itself carries a
// cLayout field), and conservatively true for unresolved SimpleTypes
// (likely generic type parameters that may monomorphize to a
// cLayoutStruct).  Primitives, strings, pointers, fat-arrays, and other
// named-but-non-cLayout types return false -- their values don't share
// storage with the holder.
func (cg *CodeGen) fieldTypeCarriesCLayoutStorage(t ast.TypeExpr) bool {
	return cg.fieldTypeCarriesCLayoutStorageRec(t, nil)
}

func (cg *CodeGen) fieldTypeCarriesCLayoutStorageRec(t ast.TypeExpr, visiting map[string]bool) bool {
	switch ty := t.(type) {
	case *ast.SimpleType:
		if ty == nil {
			return true
		}

		if cg.cLayoutStructs[ty.Name] {
			return true
		}

		if cg.isPrimitiveTypeName(ty.Name) {
			return false
		}
		// Resolve type aliases: `type MyDyad = dyad` stores the target
		// AST under cg.types[Canon(MyDyad)].Alias.  Follow one step and
		// re-classify so an alias to a cLayoutStruct doesn't slip past.
		// Share `visiting` with the struct branch so a pathological
		// `type A = B; type B = A` cycle terminates instead of
		// blowing the stack -- the same map serves both alias and
		// struct nodes since either kind keys by name.
		if r, ok := cg.types[CanonKey(ty.Name)]; ok && r != nil && r.Alias != nil {
			if visiting[ty.Name] {
				return false
			}

			if visiting == nil {
				visiting = make(map[string]bool)
			}

			visiting[ty.Name] = true

			defer delete(visiting, ty.Name)

			return cg.fieldTypeCarriesCLayoutStorageRec(r.Alias, visiting)
		}
		// Known concrete Tin struct: walk its fields once, transitively
		// flagging if any of them carries cLayout storage.  A struct
		// like `sub { d dyad }` is non-cLayout but extracting `outer.s`
		// would still yield a value whose embedded d carries h's
		// storage; the recursive check makes the FieldAccess rule
		// pick up that case.  Cycle protection via `visiting` so a
		// self-referential struct (`node { next *node }`) doesn't
		// infinite-loop.
		if _, isStruct := cg.structFields[ty.Name]; isStruct {
			if visiting[ty.Name] {
				return false
			}

			if visiting == nil {
				visiting = make(map[string]bool)
			}

			visiting[ty.Name] = true

			defer delete(visiting, ty.Name)

			fieldTypes := cg.structFieldTinTypes[ty.Name]
			for _, ft := range fieldTypes {
				if cg.fieldTypeCarriesCLayoutStorageRec(ft, visiting) {
					return true
				}
			}

			return false
		}
		// Anything else -- type parameter, forward ref, alias not yet
		// resolved -- could conceivably monomorphize to a cLayoutStruct.
		// Be conservative.
		return true
	default:
		// Pointer / Array / Generic / Wildcard / FnType fields don't
		// embed the holder's storage by-value, so reads of them don't
		// leak the holder's stack composite.  (A Generic[T] inst that
		// monomorphizes to a cLayout struct is handled when the
		// monomorph is the binding type itself, not nested as a field.)
		return false
	}
}

// isPrimitiveTypeName returns true for Tin's built-in scalar / fat-ptr
// type names.  Used by escape analysis to short-circuit field-type
// classification on known-non-cLayout shapes.
func (cg *CodeGen) isPrimitiveTypeName(name string) bool {
	switch name {
	case "i8", "i16", "i32", "i64", "i128",
		"u8", "u16", "u32", "u64", "u128",
		"f16", "f32", "f64", "f128",
		"bool", "char", "byte", "rune",
		"string", "atom", "any":
		return true
	}

	return false
}
