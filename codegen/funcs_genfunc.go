package codegen

import (
	"github.com/llir/llvm/ir"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genFuncDecl(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	// Register them in constrainedFuncs so call-site monomorphization can find them
	// even when defined locally inside a test or function body.
	if len(n.Constraints) > 0 {
		cg.constrainedFuncs[n.Name] = n

		return nil
	}
	// Unconstrained generic functions (TypeParams only) are also compiled on demand.
	// Register them in genericFuncs so call-site monomorphization can find them.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncOverloads[n.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[n.Name], n)
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}

	irName := n.Name
	if pkg, ok := cg.exports[n.Name]; ok {
		irName = pkg + "__" + n.Name
		// Also register the unqualified name so local calls work.
		defer func() {
			if entry, ok2 := cg.curScope.lookup(irName); ok2 {
				cg.curScope.set(n.Name, entry)
			}
		}()
	}
	// Any user-declared `fn main(...)` is compiled as `_tin_user_main` so we
	// can generate a proper C `i32 @main()` wrapper that passes default args
	// and returns the result (or 0 for void).
	if n.Name == "main" && !n.IsStatic {
		if !isAsyncTag(n.Tags) && bodyContainsSpawnOrAwait([]ast.Node{n.Body}) {
			cg.warn(DiagAsyncMain, n.Pos(),
				"main() uses 'spawn' or 'await' but is not marked async; "+
					"each await creates a temporary fiber, which is slower and bypasses "+
					"inline channel optimizations. Fix: change 'fn main()' to 'fn{#async} main()'")
		}

		irName = "_tin_user_main"
		cg.userMainDecl = n
		// Keep `main` resolvable from Tin source (e.g. for recursion).
		defer func() {
			if entry, ok2 := cg.curScope.lookup("_tin_user_main"); ok2 {
				cg.curScope.set("main", entry)
			}
		}()
	}
	// If this user-defined function has the same name as an already-declared
	// C extern symbol, mangle the IR name to avoid a redefinition conflict.
	// We only mangle against EXTERN declarations (not against the function's own
	// predeclared stub in the IR - that is handled by genFuncDeclAs reuse logic).
	if n.IsExtern == "" && cg.externIRNames[irName] {
		mangledName := "_tin__" + irName
		tinName := irName // capture for deferred closure
		irName = mangledName

		defer func() {
			if entry, ok2 := cg.curScope.lookup(mangledName); ok2 {
				cg.curScope.set(tinName, entry)
			}
		}()
	}
	// Overloading: if this function is part of an overload set, use the mangled name.
	if cg.overloadedNames[n.Name] && n.IsExtern == "" && len(n.Constraints) == 0 {
		sig := funcParamSig(n.Params)
		irName = overloadMangledName(irName, sig)
	}

	if n.Body != nil || n.IsExtern != "" {
		label := "fn " + n.Name
		if n.IsExtern != "" {
			label = "extern " + n.Name
		}

		cg.progress(label)
	}

	return cg.genFuncDeclAs(n, irName)
}

// genStructMethod generates a struct method body using a struct-qualified IR name.
func (cg *CodeGen) genStructMethod(structName string, m *ast.FuncDecl) error {
	key := methodScopeName(structName, m)

	var irName string

	// Save / restore the receiver struct so genFuncDecl's display-name
	// recording (recordFnDisplayName, called from inside genFuncDeclAs)
	// renders the user-visible form `Struct.method` instead of just
	// the bare method name.
	prevRecv := cg.curMethodReceiverStruct
	cg.curMethodReceiverStruct = structName

	defer func() { cg.curMethodReceiverStruct = prevRecv }()

	// Overloading: use the mangled name when this method belongs to an overload set.
	if cg.overloadedNames[key] && m.IsExtern == "" {
		sig := methodParamSig(m, structName)
		irName = overloadMangledName(key, sig)

		if err := cg.genFuncDeclAs(m, irName); err != nil {
			return err
		}
	} else {
		irName = key

		if err := cg.genFuncDeclAs(m, irName); err != nil {
			return err
		}
	}

	// Index op-trait impls so genBinExpr / genUnaryExpr can dispatch to
	// methods declared inside imported packages. Without this, structs
	// loaded via `use pkg` would only have their op-trait impls visible
	// when the struct is declared at top level (where predeclareMethod
	// runs the same registration).
	if traitName := extractOpTraitName(m.TraitQualifier); traitName != "" {
		if entry, ok := cg.curScope.lookup(irName); ok {
			if fn, ok2 := entry.val.(*ir.Func); ok2 {
				cg.recordOpTraitImpl(structName, traitName, fn)
			}
		}
	}

	return nil
}

// genFuncDeclAs generates a function using scopeName as the IR/scope name.
// typeBoundSatisfied evaluates a TypeBound expression against a concrete
// type name. The boolean combinators short-circuit so the sub-check witness
// is the FIRST failure on an AND chain and the LAST failure on an OR chain
// (i.e. when every alternative failed).
//
// Returns (ok, witness):
//
//	ok == true   -> bound holds; witness is nil
//	ok == false  -> bound failed; witness points at the failing TBAtom so
//	                the caller can format a specific error
