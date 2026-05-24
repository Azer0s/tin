package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genAdtIsExpr(block *ir.Block, scrut value.Value, e *ast.IsExpr) (value.Value, bool, error) {
	st, ok := scrut.Type().(*irtypes.StructType)
	if !ok {
		return nil, false, nil
	}

	adtName := st.Name()

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil, false, nil
	}

	variantName := ""

	var binders []string

	// Explicit constructor pattern `is Ok(v)` stored in Pattern.
	if e.Pattern != nil {
		switch p := e.Pattern.(type) {
		case *ast.CallExpr:
			if id, ok2 := p.Func.(*ast.Identifier); ok2 {
				variantName = id.Name
				binders = dataPatternBinders(p)
			}
		case *ast.Identifier:
			variantName = p.Name
		}
	}

	// Nullary variant parsed through the Type path: `is None`, `is Leaf`.
	if variantName == "" && e.Type != nil {
		if simple, ok2 := e.Type.(*ast.SimpleType); ok2 {
			if _, isVariant := variants[simple.Name]; isVariant {
				variantName = simple.Name
			}
		}
	}

	if variantName == "" {
		return nil, false, nil
	}

	vi := variants[variantName]
	if vi == nil {
		return nil, true, fmt.Errorf("data %s: unknown variant %q in is-check", adtName, variantName)
	}

	if len(binders) != 0 && len(binders) != len(vi.Fields) {
		return nil, true, fmt.Errorf("data %s: variant %s expects %d binding(s), got %d",
			adtName, variantName, len(vi.Fields), len(binders))
	}

	alloca := block.NewAlloca(st)
	block.NewStore(scrut, alloca)

	tagGEP := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagVal := block.NewLoad(irtypes.I64, tagGEP)
	cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I64, vi.Tag))

	if len(vi.Fields) > 0 && len(binders) == len(vi.Fields) {
		payloadGEP := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi := range vi.Fields {
			name := binders[fi]
			if name == "" || name == "_" {
				continue
			}

			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldTy := vi.PayloadType.Fields[fi]
			fieldVal := block.NewLoad(fieldTy, fieldPtr)

			bindAlloca := block.NewAlloca(fieldTy)
			block.NewStore(fieldVal, bindAlloca)

			// IsExpr is side-effecting across all clauses of `where`/`if`
			// chains: the load runs even when the tag check fails. Any retain
			// here would operate on mis-interpreted bytes (another variant's
			// payload), so we bind as a borrow (noRelease: true, no retain),
			// matching the semantics of union type-check `if v is n i64`.
			//
			// Callers that need to own the payload should prefer `match`,
			// whose per-case scope only evaluates bindings after the tag has
			// already matched.
			cg.curScope.set(name, &scopeEntry{val: bindAlloca, isAlloc: true, noRelease: true})
		}
	}

	return cmp, true, nil
}

// isExhaustiveDataMatch returns true when every variant of the ADT named by
// the scrutinee is covered by some case arm (and no arm has a guard). Guards
// make exhaustiveness unprovable at compile time, so we conservatively return
// false when any arm is guarded.
func (cg *CodeGen) isExhaustiveDataMatch(s *ast.MatchStmt, adtName string) bool {
	if s.Default != nil {
		return true
	}

	if len(s.Cases) == 0 {
		return false
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return false
	}

	covered := make(map[string]bool, len(variants))

	for _, c := range s.Cases {
		if c.Guard != nil {
			return false
		}

		name := dataPatternVariantName(c.Pattern)
		if name == "" {
			return false
		}

		if _, ok := variants[name]; !ok {
			return false
		}

		covered[name] = true
	}

	for name := range variants {
		if !covered[name] {
			return false
		}
	}

	return true
}

// dataPatternBinders returns the list of binder names (from `case Ok(v):`
// the binders are ["v"]). For nullary patterns, returns nil. An identifier
// pattern is treated as "_" if it is "_" or "nil"; otherwise it is a
// fresh binding with that name.
func dataPatternBinders(pat ast.Node) []string {
	call, ok := pat.(*ast.CallExpr)
	if !ok {
		return nil
	}

	out := make([]string, len(call.Args))

	for i, a := range call.Args {
		switch v := a.(type) {
		case *ast.Identifier:
			out[i] = v.Name
		default:
			out[i] = "_"
		}
	}

	return out
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}

	return append(xs, s)
}
