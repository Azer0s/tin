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

func (cg *CodeGen) genCallExpr(block *ir.Block, e *ast.CallExpr) (value.Value, error) {
	// Resolve callee.
	var (
		callee     value.Value
		calleeType *irtypes.FuncType
	)

	switch fn := e.Func.(type) {
	case *ast.Identifier:
		// CTFE: evaluate #pure #no_recurse calls with constant arguments at compile time.
		if ctfeResult, err := cg.tryEvalPureCall(e); err != nil {
			return nil, err
		} else if ctfeResult != nil {
			return ctfeResult, nil
		}
		// Macro expansion: check before scope lookup.
		macroName := fn.Name
		if macro, ok := cg.macros[macroName]; ok {
			return cg.expandMacro(block, macro, e.Args, fn.Pos())
		}
		// Also check with trailing ! stripped (for macro! call syntax).
		if strings.HasSuffix(fn.Name, "!") {
			baseName := fn.Name[:len(fn.Name)-1]
			if macro, ok := cg.macros[baseName+"!"]; ok {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}

			if macro, ok := cg.macros[baseName]; ok {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}
		}
		// #no_excl: allow calling macro! as plain function name (without !).
		// Only applies when the macro has the "no_excl" tag.
		if !strings.HasSuffix(fn.Name, "!") {
			if macro, ok := cg.macros[fn.Name+"!"]; ok && macroHasTag(macro, "no_excl") {
				return cg.expandMacro(block, macro, e.Args, fn.Pos())
			}
		}
		// Built-in: len(expr).  Skipped when a user binding shadows the
		// name in lexical scope -- the binding wins (matches sourcepos).
		// Pre-fix the builtin always won and the user's `let len = ...`
		// was silently dead.
		if fn.Name == "len" && len(e.Args) == 1 {
			if _, shadowed := cg.curScope.lookup("len"); !shadowed {
				return cg.genBuiltinLen(block, e.Args[0])
			}
		}
		// Built-in: cap(expr) returns the capacity (allocated headroom)
		// of a string or dynamic array.  Only a LOCAL let-binding
		// shadows the builtin; function-name collisions (e.g. an stdlib
		// `cap` import) fall through to the builtin because the call
		// shape unambiguously says cap-of-slice when the arg is one
		// string or fat-ptr.
		if fn.Name == "cap" && len(e.Args) == 1 {
			if entry, ok := cg.curScope.lookup("cap"); !ok || !entry.isAlloc {
				return cg.genBuiltinCap(block, e.Args[0])
			}
		}
		// Built-in: copy(expr) returns a fresh, independently-owned
		// duplicate of a string or dynamic array.  Only a LOCAL
		// let-binding shadows; stdlib's `io::copy(dst, src)` doesn't
		// collide because arity differs and copy(arr) is the natural
		// one-arg form for the deep-copy builtin.
		if fn.Name == "copy" && len(e.Args) == 1 {
			if entry, ok := cg.curScope.lookup("copy"); !ok || !entry.isAlloc {
				return cg.genBuiltinCopy(block, e.Args[0])
			}
		}
		// Built-in: nlen(expr) returns the dimensions of a multidim
		// array as an [i64].  Same shadowing rule as len.
		if fn.Name == "nlen" && len(e.Args) == 1 {
			if _, shadowed := cg.curScope.lookup("nlen"); !shadowed {
				return cg.genBuiltinNlen(block, e.Args[0])
			}
		}
		// Built-in: nrect(expr) reports rectangularity of a nested
		// array (every sub-array at the same depth has the same
		// length).  Same shadowing rule as len.
		if fn.Name == "nrect" && len(e.Args) == 1 {
			if _, shadowed := cg.curScope.lookup("nrect"); !shadowed {
				return cg.genBuiltinNrect(block, e.Args[0])
			}
		}
		// Built-in: panic(msg).  Same shadowing rule.
		if fn.Name == "panic" && len(e.Args) == 1 {
			if _, shadowed := cg.curScope.lookup("panic"); !shadowed {
				return cg.genBuiltinPanic(block, e.Args[0])
			}
		}
		// Built-in: recover() returns the panic message string; the
		// `recover('trace)` opt-in form returns a `(string, [atom])`
		// tuple so a deferred function can pair the recovered text
		// with the panic-site backtrace.  Same shadowing rule.
		if fn.Name == "recover" && len(e.Args) <= 1 {
			if _, shadowed := cg.curScope.lookup("recover"); !shadowed {
				if len(e.Args) == 0 {
					return cg.genBuiltinRecover(block)
				}

				if atomLit, ok2 := e.Args[0].(*ast.AtomLit); ok2 && atomLit.Name == "trace" {
					return cg.genBuiltinRecoverTrace(block)
				}

				return nil, cg.nodeErr(e.Args[0],
					"recover(arg) only accepts the atom literal 'trace; "+
						"call `recover()` for the plain message form")
			}
		}
		// Built-in: default(TypeName) - returns the zero value for a type.
		// Used in generic code to produce a typed zero without knowing the concrete type.
		// Same shadowing rule.
		if fn.Name == "default" && len(e.Args) == 1 {
			if _, shadowed := cg.curScope.lookup("default"); !shadowed {
				return cg.genBuiltinDefault(block, e.Args[0])
			}
		}
		// Built-in: sourcepos(symbol_or_expr) - returns the atom for
		// "<name>@<file>:<line>:<col>" (identifier arg) or
		// "<file>:<line>:<col>" (expression arg or no arg). Recognized
		// only when the name "sourcepos" is not lexically shadowed;
		// otherwise the shadowing binding wins and we fall through.
		//
		// The no-arg form is the natural call site form: useful inside
		// a macro body where retagMacroBody has already pointed every
		// node to the macro CALL site, so `sourcepos()` returns the
		// caller's position without needing an arg to thread through.
		if fn.Name == "sourcepos" && len(e.Args) <= 1 {
			if _, shadowed := cg.curScope.lookup("sourcepos"); !shadowed {
				var arg ast.Node
				if len(e.Args) == 1 {
					arg = e.Args[0]
				}
				// fn.Pos() rather than e.Pos(): the parser doesn't
				// always tag the CallExpr itself, but the function-name
				// identifier is reliably tagged by the lexer. After
				// macro retag the identifier reflects the macro CALL
				// site, which is exactly what sourcepos() needs.
				return cg.genBuiltinSourcepos(block, arg, fn.Pos())
			}
		}
		// Built-in: stacktrace([cap [, opts]]) - returns [atom] of the
		// live call stack, top-of-stack first. Optional cap clamps the
		// trace length (runtime saturates to [1, 1024]); optional opts
		// is a literal `[atom]` of TIN_ST_HIDE_* filter atoms (parsed
		// at codegen). Same shadow rule as sourcepos.
		if fn.Name == "stacktrace" && len(e.Args) <= 2 {
			if _, shadowed := cg.curScope.lookup("stacktrace"); !shadowed {
				var capArg, optsArg ast.Node
				if len(e.Args) >= 1 {
					capArg = e.Args[0]
				}

				if len(e.Args) == 2 {
					optsArg = e.Args[1]
				}

				return cg.genBuiltinStacktrace(block, capArg, optsArg, e.Pos())
			}
		}
		// ADT constructor call: `Some(42)`, `Ok(42)`, `Rgb(r, g, b)`.
		// Only intercept when the name is a known variant AND is not shadowed
		// by a local binding or regular function of the same name.
		if cg.isDataVariant(fn.Name) {
			if _, shadowed := cg.curScope.lookup(fn.Name); !shadowed {
				if v, err := cg.genDataConstructorCall(block, fn.Name, e.Args); err != nil {
					return nil, err
				} else if v != nil {
					return v, nil
				}
			}
		}
		// Check if this is a generic or constrained function call - monomorphize it.
		{
			var (
				gTmpl    *ast.FuncDecl
				argVals  []value.Value
				argsEval bool
			)
			if t, ok2 := cg.constrainedFuncs[fn.Name]; ok2 {
				gTmpl = t
			} else if t, ok2 := cg.genericFuncs[fn.Name]; ok2 {
				// Prefer a concrete compiled version over the template when one exists in
				// scope (e.g. the non-generic parse() inside json::parse[T]).
				// Also skip the generic when concrete overloads exist for this name:
				// overloaded functions are registered with mangled names (e.g. parse__string)
				// so the bare name lookup below would fail, but the overload resolution
				// path handles them correctly. Using the generic template in this case
				// causes infinite self-recursion (parse[T] calling parse[T] again).
				_, concreteOk := cg.curScope.lookup(fn.Name)
				_, hasOverloads := cg.overloads[fn.Name]

				if !concreteOk && !hasOverloads {
					gTmpl = t
					// Multiple generic free-fn overloads share the bare-name
					// entry; the latest registration wins.  Pick the one
					// whose arity matches the call so e.g.
					// `result::unwrap(r)` (1 arg) and
					// `result::unwrap(r, msg)` (2 args) route to their
					// respective templates.  When arity ties (e.g.
					// `fn poke[t](xs [t])` vs `fn poke[t](p *Trait[t])`)
					// we eval args once up front and pass the LLVM types
					// so pickGenericFuncOverload can disambiguate by
					// param shape.
					if ovs := cg.genericFuncOverloads[fn.Name]; len(ovs) > 1 {
						argVals = make([]value.Value, 0, len(e.Args))
						for _, arg := range e.Args {
							av, err2 := cg.genExpr(block, arg)
							if err2 != nil {
								return nil, err2
							}

							argVals = append(argVals, av)
						}

						argsEval = true

						argTypes := make([]irtypes.Type, len(argVals))
						for i, v := range argVals {
							if v != nil {
								argTypes[i] = v.Type()
							}
						}

						if ov := pickGenericFuncOverloadHinted(ovs, len(e.Args), argTypes, cg.pipeCurriedRetHint); ov != nil {
							gTmpl = ov
						}
					} else if ov := pickGenericFuncOverloadHinted(ovs, len(e.Args), nil, cg.pipeCurriedRetHint); ov != nil {
						gTmpl = ov
					}
				}
			}

			if gTmpl != nil {
				tmpl := gTmpl
				// Evaluate arguments first to infer concrete types.
				if !argsEval {
					argVals = make([]value.Value, 0, len(e.Args))
					for _, arg := range e.Args {
						av, err2 := cg.genExpr(block, arg)
						if err2 != nil {
							return nil, err2
						}

						argVals = append(argVals, av)
					}
				}

				typeSubst := cg.inferTypeArgs(tmpl, argVals)
				// Build instance key from substituted types.
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

				concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
				if err2 != nil {
					return nil, err2
				}
				// Constant compatibility check: reject literals that can't be
				// represented in the inferred target type (e.g. negative value -> unsigned).
				for i, argVal := range argVals {
					if i >= len(concreteFunc.Sig.Params) {
						break
					}

					constVal, isConst := argVal.(constant.Constant)
					if !isConst {
						continue
					}

					if err3 := checkConstantCompatible(constVal, concreteFunc.Sig.Params[i]); err3 != nil {
						return nil, err3
					}
				}
				// Adapt args if needed and call.
				preCoerceVals := argVals
				argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
				result := block.NewCall(cg.resolveColoredFn(concreteFunc), argVals...)
				// ARC: release temporary RC-tracked arguments (same as regular call path).
				for i, astArg := range e.Args {
					if i >= len(preCoerceVals) {
						break
					}

					cg.emitCallArgReleaseForRet(block, astArg, preCoerceVals[i], argVals[i], concreteFunc.Sig.RetType)
				}

				if irtypes.IsVoid(result.Type()) {
					return nil, nil
				}

				return result, nil
			}
		}
		// Overload resolution: if this name has multiple variants, evaluate args
		// first to pick the best match by type, then call it directly.
		if variants, hasOverloads := cg.overloads[fn.Name]; hasOverloads {
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}

				argVals = append(argVals, av)

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}

			// Prefer overload variants that exist in the most local scope to avoid
			// cross-package interference: e.g. yaml's internal parse() should call
			// yaml's own parse, not json's, even though both are in cg.overloads["parse"].
			// Traverse the scope chain from innermost outward; stop at the first level
			// that contains any matching variant (by irName in that scope's own vars).
			localVariants := variants

			for s := cg.curScope; s != nil; s = s.parent {
				var found []*overloadEntry

				for _, v := range variants {
					entry, ok := s.vars[v.irName]
					if !ok {
						continue
					}

					// Skip the self-reference that genFuncDeclAs registers in the
					// body scope (line "cg.curScope.set(scopeName, ...)").  Stopping
					// at a self-entry would hide sibling overloads in the parent scope
					// (e.g. fnv1a_32(*u8,i64) hidden from inside fnv1a_32(string)).
					if cg.curFn != nil {
						if fn, isFunc := entry.val.(*ir.Func); isFunc && fn == cg.curFn {
							continue
						}
					}

					found = append(found, v)
				}

				if len(found) > 0 {
					localVariants = found

					break
				}
			}

			best := cg.resolveOverload(localVariants, argVals)
			if best == nil {
				return nil, cg.nodeErr(e, "no matching overload for %s (got %d arg(s))", fn.Name, len(argVals))
			}

			oEntry, oOk := cg.curScope.lookup(best.irName)
			if !oOk {
				return nil, cg.nodeErr(e, "overload %s not found in scope", best.irName)
			}

			var ovCallee value.Value

			if oEntry.isAlloc {
				ptrType := oEntry.val.Type().(*irtypes.PointerType)
				ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
			} else {
				ovCallee = oEntry.val
			}

			argValsPreCoerce := append([]value.Value(nil), argVals...)
			if f, ok2 := ovCallee.(*ir.Func); ok2 {
				argVals = cg.adaptArgs(block, argVals, f.Sig)
			}

			result := block.NewCall(ovCallee, argVals...)

			for i, astArg := range e.Args {
				if i >= len(argValsPreCoerce) {
					break
				}

				cg.emitCallArgReleaseForRet(block, astArg, argValsPreCoerce[i], argVals[i], result.Type())
			}

			if irtypes.IsVoid(result.Type()) {
				return nil, nil
			}

			return result, nil
		}

		entry, ok := cg.curScope.lookup(fn.Name)
		if !ok {
			return nil, cg.nodeErr(fn, "undefined function: %s", fn.Name)
		}
		// Warn when a {#blocking} extern is called inside an {#async} function.
		if cg.curCoroHdl != nil {
			if origDecl, found := cg.funcDecls[fn.Name]; found {
				if origDecl.IsExtern != "" && hasTag(origDecl.Tags, "blocking") {
					cg.warn("blocking-in-async", e.Pos(),
						"calling blocking extern %q inside an {#async} function; use async_read/async_write instead",
						fn.Name)
				}
			}
		}

		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)
			loaded := block.NewLoad(ptrType.ElemType, entry.val)
			// If it's a closure fat pointer, call through it.
			if isFatFnPtr(loaded.Type()) {
				return cg.callFatFn(block, loaded, e.Args)
			}

			callee = loaded
		} else {
			// Route to the $colored variant of the callee when emitting
			// inside a cooperative body ($coro or $colored).  See
			// docs/internals/fn-coloring.md "Call routing".  Falls
			// through to the plain sync entry when no colored variant
			// was emitted for the callee (callee not in coloredCallable
			// or its body was elided).
			callee = cg.resolveColoredCallee(fn.Name, entry.val)
		}

	case *ast.FieldAccess:
		return cg.genCallFieldAccess(block, e, fn)

	case *ast.ScopeAccess:
		// Macro call through a qualified path (e.g. `log::info!(l, "x")`,
		// `std::log::info!(l, "x")`, etc.). The parser stores the trailing
		// `!` on the LAST path element. cg.macros is populated by
		// packages.go's pass-5 with the immediate `pkg::name!` keys, and
		// pass-6 cascades those under every re-export alias's namespace,
		// so a single lookup on the literal joined path resolves at any
		// re-export depth.
		if len(fn.Path) >= 2 {
			fullKey := strings.Join(fn.Path, "::")

			altKey := strings.Join(fn.Path, ".")
			for _, key := range []string{fullKey, altKey} {
				if macro, ok := cg.macros[key]; ok {
					return cg.expandMacro(block, macro, e.Args, fn.Pos())
				}

				if strings.HasSuffix(key, "!") {
					if macro, ok := cg.macros[key[:len(key)-1]]; ok {
						return cg.expandMacro(block, macro, e.Args, fn.Pos())
					}
				}
			}
		}

		// ADT constructor call: `Option::Some(42)` or `Option[i32]::Some(42)`
		// and similarly `Result[i32, string]::Ok(42)`.
		if v, handled, err := cg.genDataScopeCtorCall(block, fn, e.Args); handled {
			return v, err
		}

		// Static method call on a generic struct: Type[K,V]::method(args) or
		// pkg::Type[K,V]::method(args).  The ScopeAccess path looks like
		// ["collections::HashMap[string,string]", "make"].
		// Resolve the concrete name and apply overload resolution when needed.
		if len(fn.Path) >= 2 {
			methodField := fn.Path[len(fn.Path)-1]
			typePart := fn.Path[0]

			if len(fn.Path) == 3 {
				typePart = fn.Path[1]
			}

			typeParamStr := ""
			if i := strings.Index(typePart, "["); i >= 0 {
				typeParamStr = strings.TrimSuffix(typePart[i+1:], "]")
				typePart = typePart[:i]
			}

			bareBaseName := typePart
			if idx2 := strings.LastIndex(bareBaseName, "::"); idx2 >= 0 {
				bareBaseName = bareBaseName[idx2+2:]
			}

			if typeParamStr != "" {
				if _, isGeneric := cg.genericStructsByArity[bareBaseName]; isGeneric {
					// Each piece of typeParamStr can be a nested generic
					// (`*rc::Cell[i64]`), a type alias, or a qualified
					// name. Parse to a TypeExpr first so the canonical-
					// key step handles all shapes (alias chains, pointers,
					// packages) uniformly.
					rawParts := splitTopLevelTypeArgs(typeParamStr)
					resolvedParts := make([]string, len(rawParts))
					resolvedTEs := make([]ast.TypeExpr, len(rawParts))

					for i, te := range rawParts {
						resolvedParts[i] = cg.typeExprCanonicalKey(te)
						resolvedTEs[i] = te
					}

					concreteName := bareBaseName + "__" + strings.Join(resolvedParts, "__")
					if cg.structTypeFor(CanonKey(concreteName)) == nil {
						if mErr := cg.genTypeDecl(&ast.TypeDecl{
							Name: concreteName,
							Type: &ast.GenericType{Name: bareBaseName, TypeParams: resolvedTEs},
						}); mErr != nil {
							return nil, cg.nodeErr(e, "instantiating %s: %v", cg.diagStructName(concreteName), mErr)
						}
					}

					concreteMethodKey := concreteName + "_" + methodField
					if variants, hasOL := cg.overloads[concreteMethodKey]; hasOL {
						olArgs := make([]value.Value, 0, len(e.Args))
						for _, arg := range e.Args {
							av, err2 := cg.genExpr(block, arg)
							if err2 != nil {
								return nil, err2
							}

							olArgs = append(olArgs, av)

							if cg.curBlock != nil && cg.curBlock != block {
								block = cg.curBlock
							}
						}

						best := cg.resolveOverload(variants, olArgs)
						if best == nil {
							return nil, cg.nodeErr(e, "no matching overload for %s[%s]::%s (got %d arg(s))",
								cg.diagStructName(bareBaseName), strings.Join(resolvedParts, ", "), methodField, len(olArgs))
						}

						oEntry, oOk := cg.curScope.lookup(best.irName)
						if !oOk {
							return nil, cg.nodeErr(e, "overload %s not found in scope", best.irName)
						}

						var ovCallee value.Value

						if oEntry.isAlloc {
							ptrType := oEntry.val.Type().(*irtypes.PointerType)
							ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
						} else {
							ovCallee = oEntry.val
						}

						preCoerceVals := append([]value.Value(nil), olArgs...)
						if f2, ok2 := ovCallee.(*ir.Func); ok2 {
							olArgs = cg.adaptArgs(block, olArgs, f2.Sig)
						}

						result := block.NewCall(ovCallee, olArgs...)
						// ARC: release temporary RC-tracked arguments (boxed-to-any temps,
						// fresh string concats, etc.) so generic static methods don't leak
						// the values their callers passed in. Mirrors the other call paths.
						for i, astArg := range e.Args {
							if i >= len(preCoerceVals) || i >= len(olArgs) {
								break
							}

							cg.emitCallArgReleaseForRet(block, astArg, preCoerceVals[i], olArgs[i], result.Type())
						}

						if irtypes.IsVoid(result.Type()) {
							return nil, nil
						}

						return result, nil
					}
				}
			}
		}

		// Overload resolution for cross-package calls: pkg::overloadedFn(args).
		bareName := fn.Path[len(fn.Path)-1]
		if variants, hasOverloads := cg.overloads[bareName]; hasOverloads {
			argVals := make([]value.Value, 0, len(e.Args))
			for _, arg := range e.Args {
				av, err2 := cg.genExpr(block, arg)
				if err2 != nil {
					return nil, err2
				}

				argVals = append(argVals, av)

				if cg.curBlock != nil && cg.curBlock != block {
					block = cg.curBlock
				}
			}

			// When the call is qualified (pkg::fn), restrict candidates to overloads
			// that belong to that package so that identically-signed functions from
			// different packages (e.g. json::parse vs yaml::parse) don't interfere.
			filteredVariants := variants

			if len(fn.Path) > 1 {
				pkgPrefix := strings.Join(fn.Path[:len(fn.Path)-1], "__") + "__"

				var pkg []*overloadEntry

				for _, v := range variants {
					if strings.HasPrefix(v.irName, pkgPrefix) {
						pkg = append(pkg, v)
					}
				}

				if len(pkg) > 0 {
					filteredVariants = pkg
				}
			}

			best := cg.resolveOverload(filteredVariants, argVals)
			if best != nil {
				if oEntry, oOk := cg.curScope.lookup(best.irName); oOk {
					var ovCallee value.Value

					if oEntry.isAlloc {
						ptrType := oEntry.val.Type().(*irtypes.PointerType)
						ovCallee = block.NewLoad(ptrType.ElemType, oEntry.val)
					} else {
						ovCallee = oEntry.val
					}

					argValsPreCoerce := append([]value.Value(nil), argVals...)
					if f, ok2 := ovCallee.(*ir.Func); ok2 {
						argVals = cg.adaptArgs(block, argVals, f.Sig)
					}

					result := block.NewCall(ovCallee, argVals...)

					for i, astArg := range e.Args {
						if i >= len(argValsPreCoerce) {
							break
						}

						cg.emitCallArgReleaseForRet(block, astArg, argValsPreCoerce[i], argVals[i], result.Type())
					}

					if irtypes.IsVoid(result.Type()) {
						return nil, nil
					}

					return result, nil
				}
			}
			// No matching overload found.  Release the side-effecting
			// argument values we just emitted before falling through to
			// the generic path -- otherwise the generic path re-evaluates
			// every arg and the first set's allocations leak (e.g.
			// `assert::equals(json::encode(...), ...)` where assert::equals
			// is generic but errors::equals also exists as a same-named
			// overload, so cg.overloads["equals"] is non-empty here).
			for i, astArg := range e.Args {
				if i >= len(argVals) || argVals[i] == nil {
					continue
				}

				cg.emitCallArgReleaseForRet(block, astArg, argVals[i], argVals[i], irtypes.Void)
			}
		}
		// Generic function call without explicit type arg: infer type and monomorphize.
		// When multiple packages export identically-named generics (e.g. json::encode and
		// yaml::encode both map to bare "encode"), prefer the qualified key pkg__fn so the
		// correct package's template is used.
		qualBareName := strings.Join(fn.Path, "__")
		if qualBareName != bareName {
			for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
				if tmplQ, ok := m[qualBareName]; ok {
					// monomorphizeFunc looks up the home scope by tmpl.Name (the bare function
					// name), but the bare-name entry may point to a different package's scope
					// (e.g. yaml overwrote json's "encode" home scope). Temporarily fix this
					// by setting the bare-name home scope to the qualified-key home scope
					// before calling into the generic dispatch machinery.
					qualHome := cg.genericFuncHomeScopes[qualBareName]
					prevHome := cg.genericFuncHomeScopes[tmplQ.Name]

					if qualHome != nil {
						cg.genericFuncHomeScopes[tmplQ.Name] = qualHome
					}

					result, _, found, err2 := cg.callGenericFromMap(block, e.Args, qualBareName, m)

					cg.genericFuncHomeScopes[tmplQ.Name] = prevHome

					if err2 != nil {
						return nil, err2
					}

					if found {
						return result, nil
					}
				}
			}
		}

		// Bare-name fallback for genericFuncs / constrainedFuncs.
		// Skipped when the call is qualified (len(fn.Path) > 1): a qualified path
		// uniquely names the package, so we must not fall back to a same-bare-name
		// template from a different package (e.g. assert::ok(bool) silently
		// monomorphizing result::ok[t,e](Result[t,e])).
		if len(fn.Path) <= 1 {
			for _, m := range []map[string]*ast.FuncDecl{cg.genericFuncs, cg.constrainedFuncs} {
				result, _, found, err2 := cg.callGenericFromMap(block, e.Args, bareName, m)
				if err2 != nil {
					return nil, err2
				}

				if found {
					return result, nil
				}
			}
		}
		// e.g. weather.sunny used as function - probably an error, but handle gracefully.
		v, err := cg.genScopeAccess(block, fn)
		if err != nil {
			return nil, err
		}

		callee = v

	case *ast.IndexExpr:
		// Explicit generic instantiation: fn[TypeArg](args) or pkg::fn[TypeArg](args)
		// The parser represents decode[person](src) as
		//   CallExpr{Func: IndexExpr{Expr: decode_or_scope, Index: type_ident}, Args: [src]}
		if typeArgID, ok := fn.Index.(*ast.Identifier); ok {
			// Get the function name (bare or scope-qualified)
			var funcName string

			var qualFuncName string

			switch inner := fn.Expr.(type) {
			case *ast.Identifier:
				funcName = inner.Name
			case *ast.ScopeAccess:
				funcName = inner.Path[len(inner.Path)-1]
				// Build qualified key for disambiguation when multiple packages export
				// identically-named generics (e.g. json::parse[T] vs yaml::parse[T]).
				qualFuncName = strings.Join(inner.Path, "__")
				if qualFuncName == funcName {
					qualFuncName = ""
				}
			}

			typeArgName := typeArgID.Name
			// If the explicit type argument refers to a type parameter that has been
			// substituted (e.g. a recursive call like _encode_any[T](...) inside
			// _encode_any__jt_rect), resolve it to the concrete type so we don't
			// create a self-referential alias ("T" -> "T") that causes infinite recursion.
			if alias := cg.aliasTypeFor(CanonKey(typeArgName)); alias != nil {
				if st, ok2 := alias.(*ast.SimpleType); ok2 && st.Name != typeArgName {
					typeArgName = st.Name
				}
			}

			if funcName != "" {
				// Generic struct positional construction: StructName[T](field1, field2, ...)
				// e.g. fooStruct[i32](42) where fooStruct is a generic struct template.
				if _, isStruct := cg.genericStructsByArity[funcName]; isStruct {
					synthLit := &ast.StructLit{
						TypeName:   funcName,
						TypeArgs:   []ast.TypeExpr{&ast.SimpleType{Name: typeArgName}},
						Positional: e.Args,
					}

					return cg.genStructLit(block, synthLit)
				}

				// Look up the generic function template; prefer the qualified key
				// (pkg__fn) when available so that identically-named generics in
				// different packages don't shadow each other.
				var tmpl *ast.FuncDecl

				var isGeneric bool

				usedQual := false

				// When the candidate set under either key has >1 entry
				// the arity-only picker can't disambiguate.  Eval args
				// once up front and pass LLVM types so pickGenericFunc-
				// Overload can rank by param shape (matches the bare-
				// name path above).  The eval'd argVals are stashed in
				// preEvaledArgVals so the later monomorphization path
				// can reuse them without re-running side effects.
				var preEvaledArgTypes []irtypes.Type

				if (qualFuncName != "" && len(cg.genericFuncOverloads[qualFuncName]) > 1) ||
					len(cg.genericFuncOverloads[funcName]) > 1 {
					if cg.preEvaledArgVals == nil {
						vals := make([]value.Value, 0, len(e.Args))
						for _, arg := range e.Args {
							av, err2 := cg.genExpr(block, arg)
							if err2 != nil {
								return nil, err2
							}

							vals = append(vals, av)

							if cg.curBlock != nil && cg.curBlock != block {
								block = cg.curBlock
							}
						}

						cg.preEvaledArgVals = vals
					}

					preEvaledArgTypes = make([]irtypes.Type, len(cg.preEvaledArgVals))
					for i, v := range cg.preEvaledArgVals {
						if v != nil {
							preEvaledArgTypes[i] = v.Type()
						}
					}
				}

				if qualFuncName != "" {
					if ov := pickGenericFuncOverloadHinted(cg.genericFuncOverloads[qualFuncName], len(e.Args), preEvaledArgTypes, cg.pipeCurriedRetHint); ov != nil {
						tmpl = ov
						isGeneric = true
						usedQual = true
					} else if g, ok := cg.genericFuncs[qualFuncName]; ok {
						tmpl = g
						isGeneric = true
						usedQual = true
					}
				}

				if !isGeneric {
					if ov := pickGenericFuncOverloadHinted(cg.genericFuncOverloads[funcName], len(e.Args), preEvaledArgTypes, cg.pipeCurriedRetHint); ov != nil {
						tmpl = ov
						isGeneric = true
					} else if g, ok := cg.genericFuncs[funcName]; ok {
						tmpl = g
						isGeneric = true
					}
				}

				if !isGeneric {
					tmpl, isGeneric = cg.constrainedFuncs[funcName]
				}

				// Fix home scope: monomorphizeFunc looks up by tmpl.Name (bare name),
				// but the bare-name entry may point to a different package's scope when
				// two packages export identically-named generics. When we found the
				// template via the qualified key, temporarily redirect the bare-name
				// home scope entry to the correct package's scope.
				var savedHome *scope

				if usedQual {
					if qualHome := cg.genericFuncHomeScopes[qualFuncName]; qualHome != nil {
						savedHome = cg.genericFuncHomeScopes[tmpl.Name]
						cg.genericFuncHomeScopes[tmpl.Name] = qualHome
					}
				}

				if isGeneric && len(tmpl.TypeParams) > 0 {
					// Multi-arg generic instantiation: `f[T1, T2](args)` is
					// parsed as IndexExpr{Index: Identifier{Name: "T1, T2"}}.
					// Split on commas so every template param gets a
					// substitution; without this only TypeParams[0] was
					// bound, leaving the rest pointing at stale aliases
					// (or unresolved) and downstream resolves to garbage.
					typeArgParts := splitTopLevelTypeArgs(typeArgName)
					typeSubst := make(map[string]TypeName, len(tmpl.TypeParams))
					instKey := ""

					for i, tp := range tmpl.TypeParams {
						if i >= len(typeArgParts) {
							break
						}

						partTE := typeArgParts[i]
						partCanon := cg.typeExprCanonicalKey(partTE)
						typeSubst[tp] = cg.typeNameFromCanon(partCanon)

						if i > 0 {
							instKey += "__"
						}

						instKey += partCanon
					}

					if instKey == "" {
						instKey = typeArgName
					}

					concreteFunc, err2 := cg.monomorphizeFunc(tmpl, instKey, typeSubst)
					if usedQual && savedHome != nil {
						cg.genericFuncHomeScopes[tmpl.Name] = savedHome
					}

					if err2 != nil {
						return nil, err2
					}
					// Build argument list and call.  Reuse args eval'd
					// during overload disambiguation when available so
					// side effects don't fire twice.
					var argVals []value.Value
					if cg.preEvaledArgVals != nil {
						argVals = cg.preEvaledArgVals
						cg.preEvaledArgVals = nil
					} else {
						argVals = make([]value.Value, 0, len(e.Args))
						for _, arg := range e.Args {
							av, err3 := cg.genExpr(block, arg)
							if err3 != nil {
								return nil, err3
							}

							argVals = append(argVals, av)

							if cg.curBlock != nil && cg.curBlock != block {
								block = cg.curBlock
							}
						}
					}

					preCoerceVals := append([]value.Value(nil), argVals...)
					argVals = cg.adaptArgs(block, argVals, concreteFunc.Sig)
					result2 := block.NewCall(cg.resolveColoredFn(concreteFunc), argVals...)
					// ARC: release temporary RC-tracked arguments.
					for i, astArg := range e.Args {
						if i >= len(preCoerceVals) {
							break
						}

						cg.emitCallArgReleaseForRet(block, astArg, preCoerceVals[i], argVals[i], result2.Type())
					}

					if irtypes.IsVoid(result2.Type()) {
						return nil, nil
					}

					return result2, nil
				}
			}
		}
		// Fallthrough: evaluate as regular index expression used as function
		var err error

		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}

		if callee != nil && isFatFnPtr(callee.Type()) {
			result, err2 := cg.callFatFn(block, callee, e.Args)
			// ARC: release a temporary callee closure after the call.
			if isRCTrackedType(callee.Type()) && !isCopyExpr(e.Func) {
				cg.emitRelease(block, callee)
			}

			return result, err2
		}

	default:
		var err error

		callee, err = cg.genExpr(block, e.Func)
		if err != nil {
			return nil, err
		}
		// If the expression evaluated to a fat fn pointer, call through it.
		if callee != nil && isFatFnPtr(callee.Type()) {
			result, err2 := cg.callFatFn(block, callee, e.Args)
			// ARC: release a temporary callee closure after the call.
			if isRCTrackedType(callee.Type()) && !isCopyExpr(e.Func) {
				cg.emitRelease(block, callee)
			}

			return result, err2
		}
	}

	if callee == nil {
		return nil, fmt.Errorf("nil callee")
	}

	// Build arguments. Keep pre-coercion values for ARC temporary release.
	// When the callee's signature is already resolved (common case for bare
	// function names), pass each parameter's type down to arg codegen so
	// ArrayLits are generated at the target element type directly. Without
	// this plumbing an integer array literal takes its inferred (usually i64)
	// element type and the call-site coerce has no way to fix up the mismatch
	// without silent narrowing - which `coerce` now refuses.
	var preCalleeSig *irtypes.FuncType

	if f, ok := callee.(*ir.Func); ok {
		preCalleeSig = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			preCalleeSig = ft
		}
	}

	llArgs := make([]value.Value, 0, len(e.Args))

	llArgsPreCoerce := make([]value.Value, 0, len(e.Args))
	for i, arg := range e.Args {
		var tType irtypes.Type
		if preCalleeSig != nil && i < len(preCalleeSig.Params) {
			tType = preCalleeSig.Params[i]
		}

		av, err := cg.genArgWithTargetType(block, arg, tType)
		if err != nil {
			return nil, err
		}

		if av != nil {
			llArgs = append(llArgs, av)
			llArgsPreCoerce = append(llArgsPreCoerce, av)
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
	}

	// Validate callee: must be a function pointer type.  A non-function value
	// (e.g. calling an integer variable) should be a compile error, not a panic.
	if f, ok := callee.(*ir.Func); ok {
		calleeType = f.Sig
	} else if pt, ok := callee.Type().(*irtypes.PointerType); ok {
		if ft, ok2 := pt.ElemType.(*irtypes.FuncType); ok2 {
			calleeType = ft
		} else {
			return nil, cg.nodeErr(e, "cannot call non-function value (type %s)", pt.ElemType)
		}
	} else {
		return nil, cg.nodeErr(e, "cannot call non-function value (type %s)", cg.fmtArgType(callee.Type()))
	}

	// Arity check: non-variadic functions must receive exactly the declared number of args.
	if calleeType != nil && !calleeType.Variadic && len(llArgs) != len(calleeType.Params) {
		calleeName := ""

		if f, ok := callee.(*ir.Func); ok {
			// Convert internal IR name (pkg__fn or pkg__fn__sig) to source form (pkg::fn).
			// Strip any overload signature suffix (last __<sig> segment) first, then
			// replace remaining __ separators with ::.
			name := f.Name()
			if idx := strings.Index(name, "__"); idx >= 0 {
				base := name[:idx] + "__" + strings.SplitN(name[idx+2:], "__", 2)[0]
				calleeName = strings.ReplaceAll(base, "__", "::")
			} else {
				calleeName = name
			}
		}

		// Anchor the diagnostic on the function name (e.Func) so the
		// underline starts at the identifier, not the open-paren. Falls
		// back to the call expression's position when Func has no Pos.
		anchor := ast.Node(e)
		if e.Func != nil && e.Func.Pos().Line > 0 {
			anchor = e.Func
		}

		endCol := cg.sourceLineEndCol(anchor.Pos().Line)
		if calleeName != "" {
			return nil, cg.nodeErrSpan(anchor, endCol, "wrong number of arguments to %q: got %d, want %d",
				calleeName, len(llArgs), len(calleeType.Params))
		}

		return nil, cg.nodeErrSpan(anchor, endCol, "wrong number of arguments: got %d, want %d",
			len(llArgs), len(calleeType.Params))
	}

	if calleeType != nil {
		// Pre-marshal `[string]` / `[atom]` args targeting a C
		// `char**` param.  The inline marshaler emits a stack
		// alloca + fill-loop and updates the current block to the
		// loop-exit, so subsequent arg-prep, the call itself, and
		// any post-call cleanup all land after the marshal.
		for i, arg := range llArgs {
			if i >= len(calleeType.Params) {
				continue
			}

			if cg.needsCstrArrMarshal(arg, calleeType.Params[i]) {
				isAtom := isAtomType(
					arg.Type().(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType,
				)
				llArgs[i], block = cg.marshalArrayToCstrArr(block, arg, isAtom)
			}
		}

		llArgs = cg.adaptArgs(block, llArgs, calleeType)

		// Strict per-argument type check. Fires for clear mismatches
		// (passing a string where an int is expected, etc.) and reports
		// them with file:line and human-readable Tin type names rather
		// than letting them fall through to LLVM-level panics.
		//
		// Generic monomorphizations are not specially exempted any more:
		// dead branches in `fn enc[T](v T) string` whose runtime guard
		// `if typeof(v) == 'string` is statically false for the current
		// instantiation are now elided by the if-condition folder
		// (codegen/fold.go) BEFORE this check sees them, so the type-
		// incorrect calls in those branches no longer exist in IR.
		//
		// Fat-array element-type mismatches get the explicit
		// `arg as [T]` cast hint; everything else falls through to the
		// generic implicit-coercion allowlist in argTypeImplicitlyOK.
		for i, arg := range llArgs {
			if i >= len(calleeType.Params) {
				break
			}

			pt := calleeType.Params[i]
			if arg.Type().Equal(pt) {
				continue
			}

			if isFatArrayPtr(arg.Type()) && isFatArrayPtr(pt) {
				srcEl := arg.Type().(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType
				tgtEl := pt.(*irtypes.StructType).Fields[0].(*irtypes.PointerType).ElemType

				if !srcEl.Equal(tgtEl) && !srcEl.Equal(irtypes.I8) {
					return nil, cg.nodeErr(e,
						"argument %d: cannot pass [%s] where [%s] is expected; use %q to convert",
						i+1, cg.fmtArgType(srcEl), cg.fmtArgType(tgtEl),
						"arg as ["+cg.fmtArgType(tgtEl)+"]")
				}

				continue
			}
			// Tin fn -> C extern fn-ptr arg.  `tinTypeToExternLLVM`
			// lowers a Tin `fn(...) T` param to `i8*` at the C ABI
			// boundary (a raw C function pointer).  At the call site,
			// we still have the fat-fn-ptr value; route it through
			// `tin_make_trampoline` to produce a C-callable thunk
			// that bakes in the closure env via the runtime
			// register-stash mechanism.  Same mechanism as the
			// `#interop` return-fn path at codegen/interop.go:970.
			if isFatFnPtr(arg.Type()) && pt.Equal(irtypes.I8Ptr) {
				wrapped, wrapErr := cg.wrapFatFnPtrAsCCallback(block, callee, i, arg, e)
				if wrapErr != nil {
					return nil, wrapErr
				}

				if wrapped != nil {
					llArgs[i] = wrapped

					continue
				}
			}
			// Tin *fn(...) T -> C `fn_t *cbp` arg.  Lowered to `i8**`.
			// Source is `&cb` (LLVM `*{fn(...), env}`).  Load the
			// fat-fn-ptr, build the trampoline, store the trampoline
			// `i8*` in a fresh stack slot, pass the slot's address.
			if argPt, isPtr := arg.Type().(*irtypes.PointerType); isPtr {
				if isFatFnPtr(argPt.ElemType) && pt.Equal(irtypes.NewPointer(irtypes.I8Ptr)) {
					wrapped, wrapErr := cg.wrapFatFnPtrAddrAsCCallbackPtr(block, callee, i, arg, e)
					if wrapErr != nil {
						return nil, wrapErr
					}

					if wrapped != nil {
						llArgs[i] = wrapped

						continue
					}
				}
			}

			// *TinStruct -> *<S>.native: pass a pointer to the user-
			// fields region of the Tin allocation.  The fields appear
			// in the same order and types in both layouts (Tin just
			// prefixes a typeid + vtable header), so a GEP to the
			// first user field + bitcast to the native pointer type
			// is bit-identical to what C expects.  C writes hit the
			// real Tin storage so out-params propagate.  Limited to
			// ABI-compat structs (no fat-ptr / fn fields, which have
			// different shapes in the two layouts).
			if adapted := cg.adaptTinPtrToNativePtr(block, arg, pt); adapted != nil {
				llArgs[i] = adapted

				continue
			}

			if cg.argTypeImplicitlyOK(arg.Type(), pt) {
				continue
			}

			return nil, cg.nodeErr(e,
				"argument %d: cannot pass %s where %s is expected",
				i+1, cg.fmtArgType(arg.Type()), cg.fmtArgType(pt))
		}
	}

	// Auto-yield before calling a heavy or recursive Tin function.
	// Uses the IR function name (matches funcDecls keys for user-defined Tin fns).
	if f, ok := callee.(*ir.Func); ok {
		block = cg.genCallSiteYieldFor(block, f.Name())
	}

	// cLayoutStruct-value-returning wrapper: the wrapper returns the
	// C-layout %Native struct directly.  The call site allocates the
	// storage (stack composite + IMMORTAL_RC sentinel for non-escape,
	// _tin_rc_alloc'd [wrapper | native] block for escape), stores the
	// native into it, and stamps the Tin wrapper value (typeid + zero
	// vtables + c_data_ptr) inline.  The release path uses
	// _tin_release(c_data_ptr - sizeof(wrapper)), which lands on the
	// sentinel for stack mode and on the rc-alloc header for heap mode.
	var cLayoutNativeReturnStruct string

	if f, ok := callee.(*ir.Func); ok {
		if sName, has := cg.cLayoutWrapperNativeReturnFns[f.Name()]; has {
			cLayoutNativeReturnStruct = sName
		}
	}

	var result value.Value = block.NewCall(callee, llArgs...)

	if cLayoutNativeReturnStruct != "" {
		nativePtr := cg.allocCLayoutReturnBuffer(block, cLayoutNativeReturnStruct, e)
		block.NewStore(result, nativePtr)
		result = cg.buildCLayoutWrapperValue(block, nativePtr, cLayoutNativeReturnStruct)
	}

	// Auto-suspend after externs that put the calling fiber in a
	// pending-park state (e.g. `_tin_sleep_ms` registers a timer and
	// flips `pending_park` to 1).  The runtime relies on a
	// coro.suspend firing afterwards so the worker observes the park
	// and switches; without it the body would race past the call,
	// reach FIBER_DONE, and the timer would fire into a dead fiber.
	// Channel send/recv handle their own suspends inline; the small
	// list below covers the remaining park-on-call externs.
	if cg.inCoroFn && cg.curCoroFrame != nil {
		if f, ok := callee.(*ir.Func); ok && externYieldsAfter(f.Name()) {
			block = cg.emitSuspendPoint(block, cg.curCoroFrame)
			cg.curBlock = block
		}
	}

	// ARC: release temporary RC-tracked arguments.  Fresh allocations (array
	// literals, concat results, function-call return values, etc.) that are
	// passed directly without being stored in a named variable have nobody to
	// release them after the callee finishes.  The callee retains on entry and
	// releases on exit, so the net rc after the call is still 1.  We drop our
	// owning reference here to reach rc=0 and free the block.
	for i, astArg := range e.Args {
		if i >= len(llArgsPreCoerce) {
			break
		}

		cg.emitCallArgReleaseForRet(block, astArg, llArgsPreCoerce[i], llArgs[i], result.Type())
	}

	if irtypes.IsVoid(result.Type()) {
		return nil, nil
	}

	return result, nil
}

// argTypeImplicitlyOK reports whether passing a value of type src where the
// callee expects pt is one of the runtime-compatible-but-LLVM-unequal
// shapes the codegen tolerates. Used by the post-coerce strict type check
// at concrete call sites.
//
// Allowed without further conversion (these are wrapped at runtime by the
// callee or by emitted shim code):
//   - target is `any`: arbitrary src is boxed by the callee glue
//   - target is a trait fat-pointer: registered impls produce the iface
//   - target or source is a raw C pointer (i8*) when the other side is a
//     fat-ptr (string/byte-slice extraction for extern calls)
//   - target accepts an implicit-conversion fn registered for src
//   - target is a fat-fn-ptr and src is a function pointer (closure shim)
//   - both sides are pointer types of compatible underlying shape (ABI is
//     the pointer width either way)

// allocCLayoutReturnBuffer allocates the storage backing a cLayoutStruct
// extern value return.  The wrapper writes the native bytes into the
// returned pointer; c_data_ptr in the Tin wrapper value will point there.
//
// Stack mode (cg.nextCLayoutStackBind == structName): hoistAlloca a
// {TinRCHdr | wrapper | native} composite on the caller's entry block,
// write TIN_IMMORTAL_RC (-1) into the RCHdr slot once, and hand the
// native portion's address to the wrapper.  The emitRelease(c_data_ptr -
// sizeof(wrapper)) path reads the sentinel and skips, so the standard
// release walker is correct without per-call rc traffic.
//
// Heap mode (default): _tin_rc_alloc the [wrapper | native] block exactly
// as the pre-out-param wrapper used to do internally; layout and release
// math are unchanged.
func (cg *CodeGen) allocCLayoutReturnBuffer(block *ir.Block, structName string, _ *ast.CallExpr) value.Value {
	tinSt := cg.structTypeFor(CanonKey(structName))
	nativeSt := cg.nativeStructTypes[structName]

	if tinSt == nil || nativeSt == nil {
		panic(fmt.Sprintf("allocCLayoutReturnBuffer: missing IR types for %q", structName))
	}

	if cg.nextCLayoutStackBind == structName {
		compositeTy := irtypes.NewStruct(irtypes.I64, tinSt, nativeSt)
		composite := cg.hoistAlloca(block, compositeTy)
		// Write the IMMORTAL_RC sentinel into slot 0.  hoistAlloca leaves
		// us in the entry block at fn start; the store goes in the caller's
		// current block since the slot must be set before each call (the
		// alloca itself is reused across loop iterations).
		rcSlotGep := block.NewGetElementPtr(compositeTy, composite,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I64, -1), rcSlotGep)

		nativePtr := block.NewGetElementPtr(compositeTy, composite,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

		return nativePtr
	}

	wrapperSize := cg.llvmSizeOf(block, tinSt)
	nativeSize := cg.llvmSizeOf(block, nativeSt)
	totalSize := block.NewAdd(wrapperSize, nativeSize)
	rcRaw := block.NewCall(cg.ensureRCAlloc(), totalSize)
	tinPtr := block.NewBitCast(rcRaw, irtypes.NewPointer(tinSt))
	overflow := block.NewGetElementPtr(tinSt, tinPtr, constant.NewInt(irtypes.I64, 1))

	return block.NewBitCast(overflow, irtypes.NewPointer(nativeSt))
}
