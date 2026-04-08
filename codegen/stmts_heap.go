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
			if addrOf, ok := n.Value.(*ast.AddressOfExpr); ok {
				if ident, ok2 := addrOf.Expr.(*ast.Identifier); ok2 {
					aliases[n.Name] = ident.Name
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
// Handles identifiers, address-of expressions, and tuples containing those.
// Alias chains are followed transitively: if `ppx = &px` and `px = &x`, returning
// ppx marks both px and x as escaping.
func markEscapeVal(val ast.Node, aliases map[string]string, escaping map[string]bool) {
	if val == nil {
		return
	}

	switch rv := val.(type) {
	case *ast.AddressOfExpr:
		if ident, ok := rv.Expr.(*ast.Identifier); ok {
			markEscapeChain(ident.Name, aliases, escaping)
		}
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
			// let x = heap_fn() → track x as a heap variable.
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

	cg.emitAllScopeReleases(block, "")
	block.NewRet(retVal)

	return nil
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
			tinSt := cg.structTypes[sName]
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
