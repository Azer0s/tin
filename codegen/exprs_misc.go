package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genMatchAsExpr(block *ir.Block, s *ast.MatchStmt) (value.Value, error) {
	// Validate: each arm must be a single expression OR a divergent
	// terminator (return / break / panic). Divergent arms don't produce a
	// value but unblock the common `let x = match ...: case Ok(v): v;
	// case Err(e): return Err(e)` propagation pattern.
	for i, c := range s.Cases {
		if c.Body == nil || len(c.Body.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: case %d has no body", i)
		}

		if len(c.Body.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: case %d has multiple statements; match expressions allow exactly one expression per arm", i)
		}

		if armExprNode(c.Body.Stmts[0]) == nil && !isExplicitTerminator(c.Body.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: case %d body is not an expression or terminator (use 'return match ...' for statement arms)", i)
		}
	}

	if s.Default != nil {
		if len(s.Default.Stmts) == 0 {
			return nil, cg.nodeErr(s, "match expression: default arm has no body")
		}

		if len(s.Default.Stmts) > 1 {
			return nil, cg.nodeErr(s, "match expression: default arm has multiple statements; match expressions allow exactly one expression per arm")
		}

		if armExprNode(s.Default.Stmts[0]) == nil && !isExplicitTerminator(s.Default.Stmts[0]) {
			return nil, cg.nodeErr(s, "match expression: default arm body is not an expression or terminator (use 'return match ...' for statement arms)")
		}
	}

	// Determine result type from the first non-divergent arm. Divergent arms
	// (return/break/panic) don't yield a value, so they can't drive type
	// inference; they're skipped here and emitted without a result store.
	var resType irtypes.Type

	for _, c := range s.Cases {
		if isExplicitTerminator(c.Body.Stmts[0]) {
			continue
		}

		if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
			resType = cg.astInferTypeWithPattern(expr, c.Pattern)
		}

		if resType != nil {
			break
		}
	}

	if resType == nil && s.Default != nil && !isExplicitTerminator(s.Default.Stmts[0]) {
		if expr := armExprNode(s.Default.Stmts[0]); expr != nil {
			resType = cg.astInferType(expr)
		}
	}

	// Fall back to the caller's expected type (set by genVarDecl when the let
	// has an explicit annotation, or by genReturn for `return match ...`).
	// This rescues cases where every arm body refers to a pattern-bound name
	// the inference doesn't see (e.g. `case Ok(v): v` from an ADT pattern).
	if resType == nil {
		resType = cg.returnTypeHint
	}

	if resType == nil {
		return nil, cg.nodeErr(s, "match expression: cannot infer result type; annotate the variable or use 'return match ...'")
	}

	resAlloca := block.NewAlloca(resType)

	afterBlock, err := cg.genMatchWithResult(block, s, resAlloca)
	if err != nil {
		return nil, err
	}

	if afterBlock == nil {
		afterBlock = cg.newBlock("match.after")
	}

	cg.curBlock = afterBlock

	return afterBlock.NewLoad(resType, resAlloca), nil
}

// isKnownTypeName returns true when name resolves to a primitive,
// struct, trait, union, data, or generic type registered in this
// codegen.  Used by genIdentifier to emit a sharper error when the
// user wrote a type name in expression position (e.g. `case T:` in a
// match arm) -- the redirect points them at `match scrutinee.(type)`.
func (cg *CodeGen) isKnownTypeName(name string) bool {
	switch name {
	case "i8", "i16", "i32", "i64", "i128",
		"u8", "u16", "u32", "u64", "u128",
		"f32", "f64", "f128",
		"bool", "byte", "char", "string", "atom", "any", "void":
		return true
	}

	if _, ok := cg.structTypeIDs[name]; ok {
		return true
	}

	if _, ok := cg.unionTypeIDs[name]; ok {
		return true
	}

	if _, ok := cg.traitInstKeys[name]; ok {
		return true
	}

	if _, ok := cg.dataTypeIDs[name]; ok {
		return true
	}

	return false
}

func (cg *CodeGen) genIdentifier(block *ir.Block, e *ast.Identifier) (value.Value, error) {
	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		// Nullary ADT variant: bare `None`, `Leaf`, etc.
		if v, err := cg.genDataNullaryConstructor(block, e.Name); err != nil {
			return nil, err
		} else if v != nil {
			return v, nil
		}
		// Common confusion: the user wrote `case T:` in a match arm,
		// thinking T is a pattern that matches values of type T.  The
		// match parser treats T as an expression (since it has no `is`
		// keyword) so we end up here.  Detect known type names and
		// redirect to the `match a.(type)` form Tin uses for runtime
		// type matching.
		if cg.isKnownTypeName(e.Name) {
			return nil, cg.nodeErr(e,
				"`%s` is a type, not a value - to match by type, write "+
					"`match <scrutinee>.(type)` (the case arms then bind the "+
					"matched type, e.g. `case x i64: ...`)", e.Name)
		}

		return nil, cg.nodeErr(e, "undefined identifier: %s", e.Name)
	}

	if entry.isAlloc {
		ptrType := entry.val.Type().(*irtypes.PointerType)

		return block.NewLoad(ptrType.ElemType, entry.val), nil
	}

	return entry.val, nil
}

// byteToStringFatPtr wraps a single i8 value in a `{i8*, i64 len,
// i64 cap}` fat-pointer so that it can be used on either side of a
// `string ++ byte` concatenation.  cap = -1 (immortal/borrowed-style)
// because the backing storage is a stack alloca that the receiver
// must not free.
func byteToStringFatPtr(block *ir.Block, b value.Value) value.Value {
	byteAlloca := block.NewAlloca(irtypes.I8)
	block.NewStore(b, byteAlloca)

	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), byteAlloca, 0)
	v1 := block.NewInsertValue(v0, constant.NewInt(irtypes.I64, 1), 1)

	return block.NewInsertValue(v1, constant.NewInt(irtypes.I64, -1), 2)
}

// isStringConcatNode reports whether node is a `++` BinExpr whose two sides
// are both string-typed (i.e. an internal node of a fusable string-concat
// chain).  Byte-coerced concats and array concats return false so the
// existing 2-way path handles them.
func (cg *CodeGen) isStringConcatNode(node ast.Node) bool {
	be, ok := node.(*ast.BinExpr)
	if !ok || be.Op != "++" {
		return false
	}

	lt := cg.astInferType(be.Left)
	rt := cg.astInferType(be.Right)

	return lt != nil && rt != nil && isStringType(lt) && isStringType(rt)
}

// flattenStringConcat walks a `++` chain on strings and returns the leaf AST
// nodes in left-to-right source order.  When node is not a string `++` it
// returns a single-element slice containing node itself.
func (cg *CodeGen) flattenStringConcat(node ast.Node) []ast.Node {
	if cg.isStringConcatNode(node) {
		be := node.(*ast.BinExpr)

		return append(cg.flattenStringConcat(be.Left), cg.flattenStringConcat(be.Right)...)
	}

	return []ast.Node{node}
}

// genFusedStringConcat lowers `a ++ b ++ ... ++ z` to a single _tin_rc_alloc
// + N memcpys.  Without fusion, codegen emits N-1 nested 2-way concats, each
// allocating an intermediate buffer that is immediately released by the next
// concat.  For workload-bench's `header ++ " | " ++ trailer` this saves one
// alloc per item (200k allocs on a 200k-item run).
//
// Each leaf is evaluated left-to-right via genExpr (preserving source-order
// side effects), then the fat-pointer's ptr+len is extracted.  After the
// memcpy phase, leaves whose AST is a temporary-producer (call result,
// interpolation, ...) are released; non-temp leaves are not -- their data
// has been copied into the new buffer, and their own RC remains untouched.
func (cg *CodeGen) genFusedStringConcat(block *ir.Block, leaves []ast.Node) (value.Value, error) {
	type part struct {
		node   ast.Node
		val    value.Value
		ptr    value.Value
		length value.Value
	}

	parts := make([]part, 0, len(leaves))

	for _, leaf := range leaves {
		cg.curBlock = block

		v, err := cg.genExpr(block, leaf)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if v == nil || !isStringType(v.Type()) {
			// Shouldn't happen given isStringConcatNode's type guard; fall
			// back to a clear error rather than emitting malformed IR.
			return nil, cg.nodeErr(leaf,
				"`++` chain leaf must be a string, got %s", cg.fmtArgType(v.Type()))
		}

		parts = append(parts, part{
			node:   leaf,
			val:    v,
			ptr:    cg.extractStringPtr(block, v),
			length: cg.extractStringLen(block, v),
		})
	}

	totalLen := parts[0].length
	for i := 1; i < len(parts); i++ {
		totalLen = block.NewAdd(totalLen, parts[i].length)
	}

	allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
	buf := block.NewCall(cg.ensureRCAlloc(), allocSize)

	var offset value.Value = constant.NewInt(irtypes.I64, 0)

	for i, p := range parts {
		var dst value.Value
		if i == 0 {
			dst = buf
		} else {
			dst = block.NewGetElementPtr(irtypes.I8, buf, offset)
		}

		block.NewCall(cg.ensureMemcpy(), dst, p.ptr, p.length, constant.NewInt(irtypes.I1, 0))

		if i < len(parts)-1 {
			offset = block.NewAdd(offset, p.length)
		}
	}

	nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
	block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)

	// Build {i8*, i64 len, i64 cap} -- cap == len, no headroom.
	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), buf, 0)
	v1 := block.NewInsertValue(v0, totalLen, 1)
	result := block.NewInsertValue(v1, totalLen, 2)

	for _, p := range parts {
		if isTemporaryProducer(p.node) {
			cg.emitRelease(block, p.val)
		}
	}

	cg.curBlock = block

	return result, nil
}
