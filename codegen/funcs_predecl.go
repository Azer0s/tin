package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) predeclareFunc(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	if len(n.Constraints) > 0 {
		// Reject obviously-unsatisfiable shapes (`where t is *t`,
		// `where t is List[t]`, etc.) at declaration time so the
		// user sees one clear error at the source instead of an
		// opaque "undefined: <fn>" later at every call site.
		if err := checkConstraintsReferenceDeclared(n.Name, n.TypeParams, nil, n.Constraints); err != nil {
			return err
		}

		return nil
	}
	// Unconstrained generic functions (TypeParams only) are also compiled on demand.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncOverloads[n.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[n.Name], n)
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}
	// Register for #pure transitive side-effect checking.
	cg.funcDecls[n.Name] = n

	irName := n.Name
	if pkg, ok := cg.exports[n.Name]; ok {
		irName = pkg + "__" + n.Name
	}
	// Mirror the rename done in genFuncDecl: any user fn main is _tin_user_main.
	if n.Name == "main" && !n.IsStatic {
		irName = "_tin_user_main"
	}
	// Overloading: if multiple functions share this base name, mangle the IR name
	// and register the variant in the overloads map.
	if cg.overloadedNames[n.Name] && n.IsExtern == "" && len(n.Constraints) == 0 {
		sig := funcParamSig(n.Params)
		mangledName := overloadMangledName(irName, sig)
		// Resolve LLVM param types for later call-site matching.
		paramTypes, err := cg.resolveParamTypes(n.Params, "")
		if err != nil {
			return err
		}

		var retType irtypes.Type
		if n.RetType != nil {
			retType, _ = cg.tinTypeToLLVM(n.RetType)
		}

		cg.overloads[n.Name] = append(cg.overloads[n.Name], &overloadEntry{
			irName:     mangledName,
			paramSig:   sig,
			paramTypes: paramTypes,
			arity:      len(paramTypes),
			returnType: retType,
		})
		irName = mangledName
	}

	return cg.predeclareFuncAs(n, irName)
}

// traitQualifierKey converts a trait qualifier string like "iter[char]" or
// "io::AsyncReader" into a safe identifier segment ("iter__char" /
// "io__AsyncReader") for use in scope/IR names. The output mirrors
// traitImplKey on the equivalent ast.TypeExpr so a user-written qualifier
// (`fn iter[i64]::get`) produces the same scope name as the impl-list entry
// `(iter[i64])`.
func traitQualifierKey(q string) string {
	out := make([]byte, 0, len(q))

	for i := 0; i < len(q); i++ {
		c := q[i]
		switch c {
		case ':':
			// Map "::" -> "__"; runs of `:` collapse via the dedup below.
			if len(out) == 0 || out[len(out)-1] != '_' {
				out = append(out, '_')
			} else if len(out) >= 2 && out[len(out)-2] == '_' {
				// already "__"; skip
			} else {
				out = append(out, '_')
			}
		case '[', ',':
			// Boundary between trait/method name and a type-arg, or between
			// type-args. Encode as "__" to match traitImplKey output.
			if len(out) > 0 {
				// Trim a single trailing underscore so we always emit exactly "__".
				if out[len(out)-1] == '_' && (len(out) < 2 || out[len(out)-2] != '_') {
					out = out[:len(out)-1]
				}

				out = append(out, '_', '_')
			}
		case ']', ' ':
			// Closing bracket and stray spaces are dropped.
		default:
			out = append(out, c)
		}
	}
	// Trim trailing underscore(s).
	for len(out) > 0 && out[len(out)-1] == '_' {
		out = out[:len(out)-1]
	}

	return string(out)
}

// stripQualifierModule drops a leading "module::" prefix (or chain of them)
// from a trait qualifier so that "io::AsyncReader" canonicalises to just
// "AsyncReader". Type-arg suffixes are preserved:
// "io::Reader[byte]"           -> "Reader[byte]"
// "tryable[string, errors::Err]" -> "tryable[string, errors::Err]"
//
//	(the `::` lives inside the type-arg bracket, not in the leading
//	 module prefix, so it must NOT be stripped - earlier the function
//	 used LastIndex on `::`, which incorrectly trimmed everything
//	 before the type-arg's `::` and corrupted substituted qualifiers.)
func stripQualifierModule(q string) string {
	// Find the trait-name boundary: either the position of the first
	// `[` (start of type-args) or the end of the string. Only `::`
	// occurring before that boundary qualifies as a module prefix.
	boundary := strings.IndexByte(q, '[')
	if boundary < 0 {
		boundary = len(q)
	}

	prefix := q[:boundary]

	idx := strings.LastIndex(prefix, "::")
	if idx < 0 {
		return q
	}

	return q[idx+2:]
}

// methodScopeName returns the IR/scope name for a struct method.
// For plain methods: "StructName_methodName".
// For trait-qualified methods: "StructName_traitKey_methodName".
//
// The trait qualifier is canonicalised to its base name (module prefix
// stripped) so that "fn io::AsyncReader::read" and "fn AsyncReader::read"
// produce the same scope name.
func methodScopeName(structName string, m *ast.FuncDecl) string {
	if m.TraitQualifier != "" {
		bare := stripQualifierModule(m.TraitQualifier)
		// Prefer parsing the qualifier into a TypeExpr and using
		// traitImplKey on it - that is the same canonicalisation
		// genTraitVtables uses on the impl-bound side, so qualifier
		// strings with array / wildcard / pointer slots produce
		// matching keys on both ends. Fall back to the legacy
		// string-based traitQualifierKey when parsing fails (e.g.
		// during predeclare passes that hand us a malformed
		// fragment).
		if te, err := parseTypeExprFromString(bare); err == nil {
			return structName + "_" + traitImplKey(te) + "_" + m.Name
		}

		return structName + "_" + traitQualifierKey(bare) + "_" + m.Name
	}

	return structName + "_" + m.Name
}

// predeclareMethod pre-declares a struct method using a struct-qualified name
// ("StructName_methodName") so that methods with the same name on different
// structs don't collide.
func (cg *CodeGen) predeclareMethod(structName string, m *ast.FuncDecl) error {
	// Register in funcDecls so that #pure tag checking applies to methods too.
	key := methodScopeName(structName, m)
	cg.funcDecls[key] = m
	// Track receiver-mutation so the borrow analyzer can keep `t` as a
	// candidate borrow when no method named `m.Name` actually writes to
	// `this`.  The bare-name map is the conservative fallback for call
	// sites where the receiver type can't be inferred; the per-type map
	// records the precise (structName, methodName) shape so resolvable
	// sites avoid the over-approximation -- e.g. value-receiver `foo`
	// on A keeps its borrow even when pointer-receiver `foo` on B
	// exists elsewhere.
	//
	// Pointer-receiver alone does NOT imply mutation: methods like
	// `fn payload(this *Cell[T]) T = return this._payload` borrow
	// `this` to dodge an autocopy but never write through it.  Probe
	// the body for an actual mutation rooted at the receiver name.
	if len(m.Params) > 0 && m.Body != nil {
		if _, isPtr := m.Params[0].Type.(*ast.PointerType); isPtr {
			recv := m.Params[0].Name
			if recv != "" && cg.collectMutatedTargets(m.Body)[recv] {
				cg.methodMayMutateReceiver[m.Name] = true

				if cg.methodMayMutateReceiverByType[structName] == nil {
					cg.methodMayMutateReceiverByType[structName] = map[string]bool{}
				}

				cg.methodMayMutateReceiverByType[structName][m.Name] = true
			}
		}
	}

	var (
		err    error
		irName string
	)

	// Overloading: if multiple methods share this base name within the struct,
	// mangle the scope name and register the variant in the overloads map.
	if cg.overloadedNames[key] && m.IsExtern == "" {
		sig := methodParamSig(m, structName)
		mangledKey := overloadMangledName(key, sig)
		// Resolve param types for call-site matching (skip the 'this' receiver).
		paramTypes, perr := cg.resolveParamTypes(m.Params, structName)
		if perr != nil {
			return perr
		}

		var retType irtypes.Type
		if m.RetType != nil {
			retType, _ = cg.tinTypeToLLVM(m.RetType)
		}

		cg.overloads[key] = append(cg.overloads[key], &overloadEntry{
			irName:     mangledKey,
			paramSig:   sig,
			paramTypes: paramTypes,
			arity:      len(paramTypes),
			returnType: retType,
		})

		err = cg.predeclareFuncAs(m, mangledKey)
		irName = mangledKey
	} else {
		err = cg.predeclareFuncAs(m, key)
		irName = key
	}

	if err != nil {
		return err
	}

	// Operator overloading: index struct methods that implement built-in op
	// traits so genBinExpr / genUnaryExpr can dispatch to the right variant
	// when a struct overloads the same op for multiple right-hand types.
	if traitName := extractOpTraitName(m.TraitQualifier); traitName != "" {
		if entry, ok := cg.curScope.lookup(irName); ok {
			if fn, ok := entry.val.(*ir.Func); ok {
				cg.recordOpTraitImpl(structName, traitName, fn)
			}
		}
	}

	return nil
}

// predeclareFuncAs is the common implementation for predeclareFunc / predeclareMethod.
func (cg *CodeGen) predeclareFuncAs(n *ast.FuncDecl, scopeName string) error {
	// Skip extern declarations - they will be handled in genFuncDecl.
	if n.IsExtern != "" {
		return nil
	}
	// Generic functions are compiled on demand; register as template and skip.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncOverloads[n.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[n.Name], n)
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}

	var params []*ir.Param

	for _, p := range n.Params {
		if p.IsVarArgs {
			continue // varargs is not an LLVM-level named parameter
		}
		// Reject by-value #no_copy params: passing such a value would shallow-
		// copy the cell pointer and the callee's scope-exit drop would race
		// with the caller's. Use *S instead. The receiver name `this` is
		// exempt -- Tin's deinit convention is `fn deinit(this S)` and the
		// receiver is the unique owner about to be torn down, not a copy.
		if p.Name != "this" {
			if name := cg.noCopyValueTypeName(p.Type); name != "" {
				return cg.nodeErr(n,
					"function %s parameter %q has type %s which is #no_copy: pass *%s instead",
					n.Name, p.Name, cg.diagStructName(name), cg.diagStructName(name))
			}
		}

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return err
		}

		params = append(params, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		// Returning a #no_copy by value would force the caller to bind it,
		// which is also forbidden. Constructors must return *S.
		if name := cg.noCopyValueTypeName(n.RetType); name != "" {
			return cg.nodeErr(n,
				"function %s returns %s by value, but %s is #no_copy: return *%s instead",
				n.Name, cg.diagStructName(name), cg.diagStructName(name), cg.diagStructName(name))
		}

		var err error

		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}
	// If this Tin function has the same name as a C extern symbol, mangle the
	// IR name to avoid a redefinition conflict.  Both the mangled and bare names
	// are registered in scope so that Tin call sites resolve to the wrapper.
	irName := scopeName
	if cg.externIRNames[scopeName] {
		irName = "_tin__" + scopeName
	}
	// #interop reserves the bare name for the C-callable wrapper emitted
	// in a later pass; the Tin entry point gets a hidden symbol. The
	// scope still resolves the bare name to the entry point so Tin
	// internal callers go through the unwrapped function (avoiding the
	// init / marshal overhead).
	if hasTag(n.Tags, "interop") {
		irName = "__tin_interop_" + scopeName
	}
	// Check if already declared under the (possibly mangled) IR name.
	if existing, ok := cg.curScope.vars[irName]; ok {
		if _, isFunc := existing.val.(*ir.Func); isFunc {
			if irName != scopeName {
				// Ensure the original Tin name also resolves.
				cg.curScope.set(scopeName, &scopeEntry{val: existing.val, isAlloc: false})
			}

			return nil // already declared
		}
	}
	// Route fn creation through activeModule() so each pkg's user fns
	// land in its per-pkg LLVM module. cg.activeMod is set by
	// loadPackageFromSource; outside of pkg loading it's nil and
	// activeModule() returns cg.mod (the entry pkg / runtime helpers).
	// mergeRoutedPkgMods folds per-pkg modules back into cg.mod at end-
	// of-Generate today; a future commit replaces the merge with
	// parallel per-pkg .o compilation.
	f := cg.activeModule().NewFunc(irName, retType, params...)
	f.Blocks = nil // no body yet
	cg.curScope.set(irName, &scopeEntry{val: f, isAlloc: false})

	// Mark Tin-user functions as `internal` so clang can DCE them at -O2
	// when no caller survives optimization, instead of carrying their full
	// optimized body all the way through to the linker's section GC. The
	// only escape hatches are #interop wrappers (which keep the bare name
	// externally callable - emitted separately by emitInteropWrapperFor)
	// and CTFE per-fn .so artifacts, which mark exports back to default
	// visibility on a per-symbol basis.
	//
	// stacktrace() is the third escape hatch: when reachable in the program
	// (cg.stacktraceUsed, set by detectStacktraceUsage before any fn is
	// emitted), Tin user fns must keep external linkage so `-rdynamic`
	// can export them to the dynamic symbol table for dladdr to resolve.
	// STB_LOCAL symbols never reach the dynsym regardless of -rdynamic,
	// so without this gate every Tin frame would render as `??+0xADDR`.
	//
	// Per-pkg compile (incremental compilation step 2) is the fourth
	// escape hatch: when each pkg compiles to its own .o, a fn defined
	// in pkg A and called from pkg B must be linker-visible to B's .o.
	// Internal-linkage symbols are STB_LOCAL and don't cross object
	// boundaries, so cross-pkg calls need external linkage. We always
	// route user fns through per-pkg modules now (predeclareFuncAs
	// uses cg.activeModule()), so external linkage is the safe default;
	// linker DCE (--gc-sections) still strips unused symbols.
	_ = n
	// f.Linkage stays at the default (external).

	// #pure functions get LLVM attributes that unblock the optimizer:
	// alwaysinline so call sites disappear; readnone + nounwind when the
	// body has no {#allow_sideffect} escape hatch, letting LLVM hoist /
	// CSE / dead-store-eliminate around the call.
	cg.applyPureFuncAttrs(f, n)

	// #no_inline forces the LLVM `noinline` attribute. The inliner
	// otherwise treats one-shot internal-linkage callees as free to
	// merge into their caller (and tail-position calls as free to
	// share the caller's stack frame), which makes them invisible to
	// libunwind. Mostly useful as a stacktrace-test stability knob:
	// without this, asserting on a fn's name in a captured trace is
	// a wager against the optimizer.
	if hasTag(n.Tags, "no_inline") {
		f.FuncAttrs = append(f.FuncAttrs,
			enum.FuncAttrNoInline,
			// disable-tail-calls keeps the frame on the stack rather
			// than letting LLVM convert `return f()` into a `jmp` that
			// erases this fn from the unwind chain. Must be a keyed
			// AttrPair, NOT AttrString - AttrString would emit the
			// whole `disable-tail-calls="true"` as one quoted string
			// attribute name, which LLVM stores as an unrecognized
			// attribute and silently ignores. AttrPair produces the
			// canonical `"disable-tail-calls"="true"` shape.
			ir.AttrPair{Key: "disable-tail-calls", Value: "true"},
		)
	}

	if n.RetType != nil && isUnsignedTinType(n.RetType) {
		cg.funcReturnUnsigned[irName] = true
	}

	if irName != scopeName {
		// Register original Tin name so call sites resolve to the wrapper.
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}
	// If this was registered under an export-mangled name (pkg__foo), also
	// register the bare name so that local callsites still resolve.
	if idx := strings.Index(irName, "__"); idx >= 0 {
		localName := irName[idx+2:]
		if _, already := cg.curScope.vars[localName]; !already {
			cg.curScope.set(localName, &scopeEntry{val: f, isAlloc: false})
		}
	}

	return nil
}

// applyPureFuncAttrs sets LLVM function attributes on f based on the Tin
// function's purity annotation:
//
//   - #pure -> alwaysinline (always; the inliner will substitute the body
//     at every call site so LLVM's optimizer sees the math directly).
//   - #pure with no {#allow_sideffect} block in the body -> readnone +
//     nounwind. This tells LLVM the call has no observable side effects
//     and can be CSE'd, hoisted out of loops, or DCE'd when its result is
//     unused.
//
// Functions that contain at least one {#allow_sideffect} block keep the
// alwaysinline hint but skip readnone (the block may touch memory, log,
// etc.). Non-#pure functions get neither attribute and remain at LLVM's
// default heuristics.
func (cg *CodeGen) applyPureFuncAttrs(f *ir.Func, n *ast.FuncDecl) {
	if !hasTag(n.Tags, "pure") {
		return
	}

	f.FuncAttrs = append(f.FuncAttrs, enum.FuncAttrAlwaysInline)

	if !bodyHasAllowSideffect(n.Body) {
		f.FuncAttrs = append(f.FuncAttrs,
			enum.FuncAttrReadNone,
			enum.FuncAttrNoUnwind,
		)
	}
}

// bodyHasAllowSideffect walks an AST subtree looking for any TaggedBlock
// carrying the `allow_sideffect` tag. Used to decide whether a #pure
// function's body is strictly pure or has an escape hatch.
func bodyHasAllowSideffect(body ast.Node) bool {
	if body == nil {
		return false
	}

	found := false

	walkAST(body, func(n ast.Node) {
		if tb, ok := n.(*ast.TaggedBlock); ok && hasTag(tb.Tags, "allow_sideffect") {
			found = true
		}
	})

	return found
}

// Pre-registration pass
