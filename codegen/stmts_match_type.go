package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genMatchType(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	// s.Expr is TypeAssertExpr{Expr: a, IsType: true}; genExpr just returns a.
	val, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, cg.nodeErr(s, "match .(type): nil expression")
	}

	unionName := cg.typeNameOf(val.Type())

	members, isUnion := cg.unionTypeMembers[unionName]
	if !isUnion {
		// Concrete (non-union) type, e.g. a generic monomorphized from
		// `where t is num` with t = i64. The match is statically
		// resolvable: keep only the arm whose type equals val's type
		// and dead-strip the rest.
		return cg.genMatchTypeConcrete(block, s, val)
	}

	st := val.Type().(*irtypes.StructType)
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)
	// Tagged unions keep the original {i32 type_id, i8 tag, payload}
	// layout -- only ADTs widened the tag to i64.  Load i8 + zext so
	// the switch operand width matches every NewCase below.
	tagGEP := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI8 := block.NewLoad(irtypes.I8, tagGEP)
	tagI64 := block.NewZExt(tagI8, irtypes.I64)

	afterBlock := cg.newBlock("match.after")

	defaultBlock := afterBlock
	if s.Default != nil {
		defaultBlock = cg.newBlock("match.default")
	}

	// Build cases: determine tag for each case from VarType or StructPattern.TypeName.
	var (
		cases      []*ir.Case
		caseBlocks []*ir.Block
	)

	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)
		tag := int64(0)

		// Determine the target type: from VarType or from StructPattern.TypeName.
		var targetType ast.TypeExpr

		if c.VarType != nil {
			targetType = c.VarType
		} else if sp, ok := c.Pattern.(*ast.StructPattern); ok {
			targetType = &ast.SimpleType{Name: sp.TypeName}
		}

		if targetType != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(targetType)
			if err2 == nil {
				for j, te := range members {
					lt, err3 := cg.tinTypeToLLVM(te)
					if err3 != nil {
						continue
					}

					if lt.Equal(targetLLVM) {
						tag = int64(j)

						break
					}
				}
			}
		}

		cases = append(cases, ir.NewCase(constant.NewInt(irtypes.I64, tag), caseBlock))
	}

	block.NewSwitch(tagI64, defaultBlock, cases...)

	// Generate case bodies.
	anyFallthrough := false

	for i, c := range s.Cases {
		caseBlock := caseBlocks[i]
		cg.curScope = newScope(cg.curScope)

		// Bind payload: either VarName+VarType or StructPattern fields.
		if c.VarName != "" && c.VarType != nil {
			targetLLVM, err2 := cg.tinTypeToLLVM(c.VarType)
			if err2 == nil {
				payloadGEP := caseBlock.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(targetLLVM))
				payloadAlloca := caseBlock.NewAlloca(targetLLVM)
				payloadVal := caseBlock.NewLoad(targetLLVM, payloadPtr)
				caseBlock.NewStore(payloadVal, payloadAlloca)
				cg.curScope.set(c.VarName, &scopeEntry{val: payloadAlloca, isAlloc: true})
			}
		} else if sp, ok := c.Pattern.(*ast.StructPattern); ok {
			structLLVM, err2 := cg.tinTypeToLLVM(&ast.SimpleType{Name: sp.TypeName})
			if err2 == nil {
				payloadGEP := caseBlock.NewGetElementPtr(st, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
				payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(structLLVM))
				payloadAlloca := caseBlock.NewAlloca(structLLVM)
				payloadVal := caseBlock.NewLoad(structLLVM, payloadPtr)
				caseBlock.NewStore(payloadVal, payloadAlloca)

				for _, field := range sp.Fields {
					if field.IsWild || field.Literal != nil {
						continue
					}

					fieldIdx := cg.fieldIndex(sp.TypeName, field.Name)
					if fieldIdx < 0 {
						continue
					}

					var fieldType irtypes.Type

					if st2, ok2 := structLLVM.(*irtypes.StructType); ok2 && fieldIdx < len(st2.Fields) {
						fieldType = st2.Fields[fieldIdx]
					} else {
						fieldType = irtypes.I64
					}

					gep := caseBlock.NewGetElementPtr(structLLVM, payloadAlloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
					fv := caseBlock.NewLoad(fieldType, gep)
					fa := caseBlock.NewAlloca(fieldType)
					caseBlock.NewStore(fv, fa)
					cg.curScope.set(field.Name, &scopeEntry{val: fa, isAlloc: true})
				}
			}
		}

		// Apply guard if present.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(caseBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			bodyBlock := cg.newBlock(fmt.Sprintf("match.case.%d.body", i))
			caseBlock.NewCondBr(cg.toBoolImplicit(caseBlock, guardVal), bodyBlock, afterBlock)

			anyFallthrough = true // guard failure goes to afterBlock
			caseBlock = bodyBlock
		}

		caseBlock, _, err = cg.genStmt(caseBlock, c.Body)

		// ARC: release case-body scope vars before falling through to afterBlock.
		cg.emitScopeRelease(caseBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// Default.
	if s.Default != nil {
		cg.curScope = newScope(cg.curScope)
		defaultBlock, _, err = cg.genStmt(defaultBlock, s.Default)

		// ARC: release default-body scope vars before falling through.
		cg.emitScopeRelease(defaultBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if defaultBlock != nil && defaultBlock.Term == nil {
			defaultBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// All arms terminated - afterBlock is unreachable; signal exhaustive termination.
	if !anyFallthrough {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

// genMatchTypeConcrete handles `match v.(type)` when v is a concrete
// (non-tagged-union) type. This happens when a generic with `where t is X`
// is instantiated with a single variant of X (e.g. Box[i64] for
// `where t is num`). Arms whose declared type doesn't match val's type
// are dead at compile time; the surviving arms (all of the same type)
// are emitted in order with guards chaining: a guard miss on arm N
// falls through to arm N+1, and the final unguarded arm (or default)
// catches any leftover.
func (cg *CodeGen) genMatchTypeConcrete(block *ir.Block, s *ast.MatchStmt, val value.Value) (*ir.Block, error) {
	valLLVM := val.Type()

	matching := make([]ast.MatchCase, 0, len(s.Cases))

	for _, c := range s.Cases {
		var targetType ast.TypeExpr

		if c.VarType != nil {
			targetType = c.VarType
		} else if sp, ok := c.Pattern.(*ast.StructPattern); ok {
			targetType = &ast.SimpleType{Name: sp.TypeName}
		}

		if targetType == nil {
			continue
		}

		targetLLVM, err := cg.tinTypeToLLVM(targetType)
		if err != nil || !targetLLVM.Equal(valLLVM) {
			continue
		}

		matching = append(matching, c)
	}

	if len(matching) == 0 {
		if s.Default != nil {
			cg.curScope = newScope(cg.curScope)
			block, _, err := cg.genStmt(block, s.Default)
			cg.emitScopeRelease(block, cg.curScope)
			cg.curScope = cg.curScope.parent

			return block, err
		}

		return nil, cg.nodeErr(s, "match .(type) on concrete type %s: no case matches",
			concreteTypeDisplay(cg, valLLVM))
	}

	afterBlock := cg.newBlock("match.concrete.after")
	anyFallthrough := false
	cur := block

	for i, c := range matching {
		isLast := i == len(matching)-1

		cg.curScope = newScope(cg.curScope)

		if c.VarName != "" {
			alloca := cur.NewAlloca(valLLVM)
			cur.NewStore(val, alloca)
			cg.curScope.set(c.VarName, &scopeEntry{val: alloca, isAlloc: true})
		}

		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(cur, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			bodyBlock := cg.newBlock(fmt.Sprintf("match.concrete.body.%d", i))

			var nextBlock *ir.Block
			if isLast {
				nextBlock = cg.newBlock(fmt.Sprintf("match.concrete.fall.%d", i))
			} else {
				nextBlock = cg.newBlock(fmt.Sprintf("match.concrete.next.%d", i))
			}

			cur.NewCondBr(cg.toBool(cur, guardVal), bodyBlock, nextBlock)

			bodyEnd, _, err3 := cg.genStmt(bodyBlock, c.Body)
			cg.emitScopeRelease(bodyEnd, cg.curScope)
			cg.curScope = cg.curScope.parent

			if err3 != nil {
				return nil, err3
			}

			if bodyEnd != nil && bodyEnd.Term == nil {
				bodyEnd.NewBr(afterBlock)

				anyFallthrough = true
			}

			cur = nextBlock

			continue
		}

		// Unguarded arm: this commits, no further arms can run.
		bodyEnd, _, err := cg.genStmt(cur, c.Body)
		cg.emitScopeRelease(bodyEnd, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if bodyEnd != nil && bodyEnd.Term == nil {
			bodyEnd.NewBr(afterBlock)

			anyFallthrough = true
		}

		if !isLast {
			// Subsequent arms are unreachable.
			cur = nil

			break
		}

		cur = nil
	}

	// If the last guarded arm failed its guard, `cur` still holds the
	// fall-through block. Emit default into it (or just branch to after).
	if cur != nil {
		if s.Default != nil {
			cg.curScope = newScope(cg.curScope)
			defEnd, _, err := cg.genStmt(cur, s.Default)
			cg.emitScopeRelease(defEnd, cg.curScope)
			cg.curScope = cg.curScope.parent

			if err != nil {
				return nil, err
			}

			if defEnd != nil && defEnd.Term == nil {
				defEnd.NewBr(afterBlock)

				anyFallthrough = true
			}
		} else {
			cur.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	if !anyFallthrough {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

// concreteTypeDisplay renders an LLVM type for diagnostics, preferring
// the source-syntax form ("Box[i64]", "[i64]", "*Foo") over the raw
// LLVM string ("%Box__i64", "{ i8*, i64 }", "%Foo*").
func concreteTypeDisplay(cg *CodeGen, t irtypes.Type) string {
	if name := cg.typeNameOf(t); name != "" {
		return cg.diagStructName(name)
	}

	return cg.fmtArgType(t)
}
