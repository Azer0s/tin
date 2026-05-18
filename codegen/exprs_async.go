package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) callGenericFromMap(
	block *ir.Block,
	args []ast.Node,
	bareName string,
	m map[string]*ast.FuncDecl,
) (value.Value, *ir.Block, bool, error) {
	tmpl, ok := m[bareName]
	if !ok {
		return nil, block, false, nil
	}
	// When the bare-name entry came from genericFuncs, prefer the
	// overload whose arity matches the call so two same-name
	// generics (e.g. `unwrap[t](r)` and `unwrap[t](r, msg string)`)
	// route to their own templates.  The single-entry case in
	// pickGenericFuncOverload short-circuits, leaving non-overloaded
	// callers unaffected.  When the overload set has >1 candidates we
	// eval args first so the shape-aware picker can disambiguate by
	// the LLVM type of each arg (array vs trait-ptr, etc.).
	var argVals []value.Value
	if ovs := cg.genericFuncOverloads[bareName]; len(ovs) > 1 {
		argVals = make([]value.Value, 0, len(args))

		for _, arg := range args {
			av, err := cg.genExpr(block, arg)
			if err != nil {
				return nil, block, true, err
			}

			argVals = append(argVals, av)

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}
		}

		argTypes := make([]irtypes.Type, len(argVals))

		for i, v := range argVals {
			if v != nil {
				argTypes[i] = v.Type()
			}
		}

		if ov := pickGenericFuncOverloadHinted(ovs, len(args), argTypes, cg.pipeCurriedRetHint); ov != nil {
			tmpl = ov
		}
	} else if ov := pickGenericFuncOverloadHinted(cg.genericFuncOverloads[bareName], len(args), nil, cg.pipeCurriedRetHint); ov != nil {
		tmpl = ov
	}

	if argVals == nil {
		argVals = make([]value.Value, 0, len(args))
		for _, arg := range args {
			av, err := cg.genExpr(block, arg)
			if err != nil {
				return nil, block, true, err
			}

			argVals = append(argVals, av)

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}
		}
	}

	typeSubst := cg.inferTypeArgs(tmpl, argVals)
	instKey := ""

	for i, tp := range tmpl.TypeParams {
		if i > 0 {
			instKey += "__"
		}

		if name, found := typeSubst[tp]; found {
			instKey += name.Canon
		} else {
			instKey += tp
		}
	}

	// When bareName is a qualified key (e.g. "yaml__encode"), use it as the
	// template name so the monomorphized IR name includes the package prefix
	// (e.g. "yaml__encode__point"). Without this, identically-named generics
	// from different packages (json::encode and yaml::encode both have bare
	// name "encode") would produce the same IR name and the cache would return
	// the first-compiled version for every subsequent package's call.
	monoTmpl := tmpl
	if bareName != tmpl.Name {
		copy := *tmpl
		copy.Name = bareName
		monoTmpl = &copy
	}

	concreteFunc, err := cg.monomorphizeFunc(monoTmpl, instKey, typeSubst)
	if err != nil {
		return nil, block, true, err
	}

	argValsPreCoerce := append([]value.Value(nil), argVals...)
	argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)

	result := block.NewCall(concreteFunc, argVals...)

	// ARC: release temporary RC-tracked arguments (same logic as the general
	// call path).  Without this, temporaries like join() results or concat
	// results passed directly as arguments leak.
	for i, astArg := range args {
		if i >= len(argValsPreCoerce) {
			break
		}

		preCoerce := argValsPreCoerce[i]
		postCoerce := argVals[i]

		if isAnyType(postCoerce.Type()) && !isAnyType(preCoerce.Type()) {
			cg.emitRelease(block, postCoerce)

			continue
		}

		if !isRCTrackedType(preCoerce.Type()) {
			// ADT-by-value rvalue: same logic as
			// emitCallArgReleaseForRet on the non-generic call
			// path.  A temp ADT (e.g. `result::unwrap(pipe())`)
			// owns rc=1 of its active-variant payload fields;
			// without this release the callee's match-arm
			// `transferredFromBorrow` retain stays unbalanced
			// and leaks the payload's rc::Cell pointers.
			if isTemporaryProducer(astArg) && cg.isDataType(preCoerce.Type()) {
				cg.emitDataValueRelease(block, preCoerce)
			}

			continue
		}

		if isCopyExpr(astArg) {
			continue
		}

		cg.emitRelease(block, preCoerce)
	}

	return result, block, true, nil
}
