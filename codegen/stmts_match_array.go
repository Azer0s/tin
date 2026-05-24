package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genArrayMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if !isFatArrayPtr(scrutinee.Type()) {
		return nil, fmt.Errorf("array pattern match requires an array type, got %s", cg.fmtArgType(scrutinee.Type()))
	}

	arrType := scrutinee.Type().(*irtypes.StructType)
	elemPtrType := arrType.Fields[0].(*irtypes.PointerType)
	elemType := elemPtrType.ElemType

	// Spill scrutinee to alloca so we can GEP into it.
	arrAlloca := block.NewAlloca(arrType)
	block.NewStore(scrutinee, arrAlloca)

	// Load length: GEP field 1.
	lenGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	// Load data pointer: GEP field 0.
	ptrGep := block.NewGetElementPtr(arrType, arrAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dataPtr := block.NewLoad(elemPtrType, ptrGep)

	afterBlock := cg.newBlock("amatch.after")
	anyFallthrough := false
	curCheckBlock := block

	for i, c := range s.Cases {
		ap, ok := c.Pattern.(*ast.ArrayPattern)
		if !ok {
			return nil, fmt.Errorf("genArrayMatch: non-array pattern in case %d", i)
		}

		nextBlock := cg.newBlock(fmt.Sprintf("amatch.next.%d", i))
		bodyBlock := cg.newBlock(fmt.Sprintf("amatch.case.%d", i))

		// Count regular (non-rest) elements and find rest index.
		regularCount := 0
		restIdx := -1

		for j, e := range ap.Elems {
			if e.IsRest {
				restIdx = j
			} else {
				regularCount++
			}
		}

		// Emit length check.
		var lenCond value.Value

		checkBlock := curCheckBlock
		nConst := constant.NewInt(irtypes.I64, int64(regularCount))

		if restIdx >= 0 {
			// Rest semantics: the rest slot may bind zero or more elements, so
			// `[x, ...xs]` matches `[3]` (x=3, xs=[]). The only constraint is
			// that the array must have at least `regularCount` elements AND be
			// non-empty (so `[...xs]` does not overlap with `[]`).
			//
			// Use `[]` to match the empty array; use exact-length patterns
			// `[x]`, `[x, y]`, ... when no rest slot is needed.
			minLen := int64(regularCount)
			if minLen < 1 {
				minLen = 1
			}

			lenCond = checkBlock.NewICmp(enum.IPredSGE, arrLen, constant.NewInt(irtypes.I64, minLen))
		} else {
			// len == regularCount
			lenCond = checkBlock.NewICmp(enum.IPredEQ, arrLen, nConst)
		}

		afterLenCheck := cg.newBlock(fmt.Sprintf("amatch.lenok.%d", i))
		checkBlock.NewCondBr(lenCond, afterLenCheck, nextBlock)
		checkBlock = afterLenCheck

		// Open scope for bindings.
		cg.curScope = newScope(cg.curScope)

		// Bind regular elements.
		regIdx := 0

		for _, e := range ap.Elems {
			if e.IsRest {
				continue
			}

			if !e.IsWild && e.Name != "" {
				idxVal := constant.NewInt(irtypes.I64, int64(regIdx))
				elemGep := checkBlock.NewGetElementPtr(elemType, dataPtr, idxVal)
				loaded := checkBlock.NewLoad(elemType, elemGep)
				alloca := checkBlock.NewAlloca(elemType)
				checkBlock.NewStore(loaded, alloca)
				cg.curScope.set(e.Name, &scopeEntry{val: alloca, isAlloc: true})
			}

			regIdx++
		}

		// Bind rest element.
		if restIdx >= 0 {
			e := ap.Elems[restIdx]

			if !e.IsWild && e.Name != "" {
				// Build {i8*, i64} raw slice then subslice from regularCount.
				var elemSzBytes int64 = 8
				if sz := llvmTypeSize(elemType); sz > 0 {
					elemSzBytes = int64(sz)
				}

				// `{i8*, i64 len, i64 cap}` raw slice for the runtime
				// helper.  cap == len since this view exists only for
				// the duration of the subslice call.
				sliceType := fatArrayPtrType(irtypes.I8)
				dataPtrAsI8 := checkBlock.NewBitCast(dataPtr, irtypes.I8Ptr)
				rawSlice := cg.buildFatArrayValue(checkBlock, irtypes.I8, dataPtrAsI8, arrLen, arrLen)

				subFn := cg.ensureSliceSubslice()
				subResult := cg.callExtern(checkBlock, subFn, rawSlice,
					constant.NewInt(irtypes.I64, int64(regularCount)),
					constant.NewInt(irtypes.I64, elemSzBytes))

				// Reinterpret {i8*, i64} as the original fat-array type.
				tmpAlloca := checkBlock.NewAlloca(sliceType)
				checkBlock.NewStore(subResult, tmpAlloca)
				castPtr := checkBlock.NewBitCast(tmpAlloca, irtypes.NewPointer(arrType))
				restVal := checkBlock.NewLoad(arrType, castPtr)
				restAlloca := checkBlock.NewAlloca(arrType)
				checkBlock.NewStore(restVal, restAlloca)
				cg.curScope.set(e.Name, &scopeEntry{val: restAlloca, isAlloc: true})
			}
		}

		// Optional guard.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(checkBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			checkBlock.NewCondBr(cg.toBoolImplicit(checkBlock, guardVal), bodyBlock, nextBlock)
		} else {
			checkBlock.NewBr(bodyBlock)
		}

		// Emit body.
		var bodyErr error

		_, bodyErr = cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
		cg.curScope = cg.curScope.parent

		if bodyErr != nil {
			return nil, bodyErr
		}

		curCheckBlock = nextBlock
	}

	// Default arm or exhaustiveness.
	var defaultErr error

	_, defaultErr = cg.emitMatchDefaultArm(s, curCheckBlock, afterBlock, resAlloca, &anyFallthrough, cg.isExhaustiveArrayMatch(s))
	if defaultErr != nil {
		return nil, defaultErr
	}

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) isExhaustiveEnumMatch(s *ast.MatchStmt) bool {
	if len(s.Cases) == 0 {
		return false
	}

	var enumName string

	for _, c := range s.Cases {
		fa, ok := c.Pattern.(*ast.FieldAccess)
		if !ok {
			return false
		}

		id, ok := fa.Expr.(*ast.Identifier)
		if !ok {
			return false
		}

		key := id.Name + "." + fa.Field
		if _, isEnum := cg.enumValues[key]; !isEnum {
			return false
		}

		if enumName == "" {
			enumName = id.Name
		} else if enumName != id.Name {
			return false
		}
	}

	if enumName == "" {
		return false
	}
	// Count total members registered for this enum.
	prefix := enumName + "."
	total := 0

	for key := range cg.enumValues {
		if strings.HasPrefix(key, prefix) {
			total++
		}
	}

	return len(s.Cases) == total
}
