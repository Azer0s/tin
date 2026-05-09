package codegen

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// predeclareFunc adds a function to the module and registers it in the global
// scope without generating the body. This enables forward references and recursion.
func (cg *CodeGen) predeclareFunc(n *ast.FuncDecl) error {
	// Constrained generic functions are compiled on demand at call sites.
	if len(n.Constraints) > 0 {
		return nil
	}
	// Unconstrained generic functions (TypeParams only) are also compiled on demand.
	if len(n.TypeParams) > 0 {
		cg.genericFuncs[n.Name] = n
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
// "AsyncReader". Type-arg suffixes are preserved: "io::Reader[byte]" -> "Reader[byte]".
func stripQualifierModule(q string) string {
	idx := strings.LastIndex(q, "::")
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
					n.Name, p.Name, prettyStructName(name), prettyStructName(name))
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
				n.Name, prettyStructName(name), prettyStructName(name), prettyStructName(name))
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

func (cg *CodeGen) preregister(node ast.Node) error {
	switch n := node.(type) {
	case *ast.StructDecl:
		// Tag-driven side maps must populate during preregister (pass 1)
		// so later passes can consult them. genStructLayout (pass 3 phase
		// A) used to be the only writer, but predeclareFuncAs (pass 2)
		// also needs to see #no_copy to reject by-value parameters and
		// returns -- previously those slipped through and only let-binding
		// rejection caught them. Mirror the writes here for both bare and
		// pkg-qualified keys so cross-package lookups work.
		if hasTag(n.Tags, "no_copy") {
			cg.noCopyStructs[n.Name] = true
			cg.noCopyStructs[cg.pkgStructKey(n.Name)] = true
		}

		if hasTag(n.Tags, "closed") {
			cg.closedStructs[n.Name] = true
			cg.closedStructs[cg.pkgStructKey(n.Name)] = true
		}

		if len(n.TypeParams) > 0 {
			// Generic struct - store as template keyed by arity; concrete types
			// are created when a "type X = GenericStruct[T, R]" alias is processed.
			// Templates are always keyed by bare name so that tinTypeToLLVM can look
			// them up using the stripped (bare) name component.
			if cg.genericStructsByArity[n.Name] == nil {
				cg.genericStructsByArity[n.Name] = make(map[int]*ast.StructDecl)
			}

			cg.genericStructsByArity[n.Name][len(n.TypeParams)] = n

			// Tag the template's source file so monomorphizations can
			// inherit it for `//!-Wno-` lookup. Without this, every
			// `Channel[T]` instantiated in user code would lose the
			// suppression that lives in stdlib/sync/channel.tin.
			if cg.filename != "" {
				cg.genericStructTmplFiles[n.Name] = cg.filename
				if cg.currentPkg != "" {
					cg.genericStructTmplFiles[cg.currentPkg+"__"+n.Name] = cg.filename
				}
			}
		} else {
			// Register an opaque struct so recursive types work.
			// Use the canonical package-prefixed name as both the map key and the
			// LLVM IR struct name so that structs from different packages never
			// collide (e.g. sync__Unit, io__Reader).
			structKey := cg.pkgStructKey(n.Name)
			st := irtypes.NewStruct()
			st.SetName(structKey)
			cg.structTypes[structKey] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			// Register a bare-name alias so that code referencing the short form
			// (e.g. "Parser" inside yaml, "Unit" inside sync) resolves to the
			// canonical name.  Always overwrite so the currently-compiling package's
			// definition takes precedence over a same-named type from an earlier
			// package loaded in the same scope.
			if cg.currentPkg != "" {
				cg.typeAliases[n.Name] = &ast.SimpleType{Name: structKey}
			}
		}
	case *ast.EnumDecl:
		// Register enum values early so they are available during on-demand
		// struct monomorphization triggered from pass 2 (predeclare).
		if err := cg.genEnumDecl(n); err != nil {
			return err
		}
	case *ast.UnionDecl:
		// Register an opaque struct so forward references work.
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.structTypes[n.Name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	case *ast.DataDecl:
		// Register an opaque struct for non-generic ADTs so forward references
		// in function signatures resolve. Layout is filled in by genDataDecl
		// during pre-pass 3. Generic ADTs are monomorphized on demand; their
		// variant names are registered against the concrete instance at
		// monomorphization time (see monomorphizeDataDecl).
		if len(n.TypeParams) == 0 {
			st := irtypes.NewStruct()
			st.SetName(n.Name)
			cg.structTypes[n.Name] = st
			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)

			for _, v := range n.Variants {
				cg.dataVariantLookup[v.Name] = appendUnique(cg.dataVariantLookup[v.Name], n.Name)
			}
		}

		cg.dataDecls[n.Name] = n
	case *ast.TypeDecl:
		// Simple type aliases (type char = u8) go straight into typeAliases.
		// Tagged union aliases (type u = i8 | string) get a placeholder struct so
		// forward references work; full layout is filled in genTypeDecl.
		// Struct-monomorphization aliases (type point = tuple[f32]) are handled
		// in genTypeDecl so that all struct templates are known first.
		if ut, isUnion := n.Type.(*ast.UnionTypeExpr); isUnion {
			if _, exists := cg.structTypes[n.Name]; !exists {
				st := irtypes.NewStruct()
				st.SetName(n.Name)
				cg.structTypes[n.Name] = st
				cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			}
			// Populate unionTypeMembers here so that downstream constraint
			// checks (`where t is X` against a tagged-union alias) work
			// even when the TypeDecl was declared in an imported package
			// -- packages.go doesn't run pass-2's genTypeDecl, so without
			// this preregister write the membership is invisible across
			// package boundaries and method-level where guards silently
			// dead-strip every method.
			if _, already := cg.unionTypeMembers[n.Name]; !already {
				cg.unionTypeMembers[n.Name] = ut.Types
			}
		} else if _, isGeneric := n.Type.(*ast.GenericType); !isGeneric {
			cg.typeAliases[n.Name] = n.Type
		}
	case *ast.TraitDecl:
		cg.traits[n.Name] = n
		if cg.currentPkg != "" {
			qualInstKey := cg.currentPkg + "__" + n.Name
			cg.traitBareToQualInstKey[n.Name] = qualInstKey
		}
	case *ast.MacroDecl:
		cg.macros[n.Name] = n
	case *ast.VarDecl:
		if !n.IsConst || n.Value == nil {
			break
		}
		// Preregister top-level constants so test-block scopes can see them.
		// Only simple literal values are evaluated here; complex expressions
		// are left to the normal genVarDecl pass.
		var cv value.Value

		switch lit := n.Value.(type) {
		case *ast.IntLit:
			if lit.Big != nil {
				cv = &constant.Int{Typ: irtypes.I128, X: new(big.Int).Set(lit.Big)}
			} else {
				cv = constant.NewInt(irtypes.I64, lit.Value)
			}
		case *ast.FloatLit:
			cv = constant.NewFloat(irtypes.Double, lit.Value)
		case *ast.BoolLit:
			if lit.Value {
				cv = constant.NewInt(irtypes.I1, 1)
			} else {
				cv = constant.NewInt(irtypes.I1, 0)
			}
		case *ast.StringLit:
			raw := cg.newGlobalString(lit.Value).(constant.Constant)
			strType := stringFatPtrType()
			lenVal := constant.NewInt(irtypes.I64, int64(len(lit.Value)))
			cv = constant.NewStruct(strType, raw, lenVal)
		case *ast.AtomLit:
			cv = cg.atomConstant(cg.registerAtom(lit.Name))
		}

		if cv != nil {
			if n.Type != nil {
				if lt, err := cg.tinTypeToLLVM(n.Type); err == nil {
					cv = cg.constCoerce(cv, lt)
				}
			}

			cg.curScope.set(n.Name, &scopeEntry{val: cv, isAlloc: false})
		}
	}

	return nil
}

// hasDeferStmt reports whether body contains any DeferStmt (recursively).
// Nested fn/lambda declarations are not descended into.
// NOTE: concrete nil pointers (e.g. (*ast.Block)(nil)) passed as ast.Node are
// non-nil interfaces; all concrete-pointer cases must guard against nil n.
func hasDeferStmt(body ast.Node) bool {
	if body == nil {
		return false
	}

	switch n := body.(type) {
	case *ast.DeferStmt:
		return n != nil
	case *ast.FuncDecl, *ast.LambdaExpr:
		return false // defers inside nested fns don't affect outer's ret slot
	case *ast.Block:
		if n == nil {
			return false
		}

		for _, s := range n.Stmts {
			if hasDeferStmt(s) {
				return true
			}
		}
	case *ast.ExprStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Expr)
	case *ast.VarDecl:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.AssignStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.ReturnStmt:
		if n == nil {
			return false
		}

		return hasDeferStmt(n.Value)
	case *ast.IfStmt:
		if n == nil {
			return false
		}

		if n.Then != nil && hasDeferStmt(n.Then) {
			return true
		}

		if n.Else != nil && hasDeferStmt(n.Else) {
			return true
		}

		for _, elif := range n.ElseIfs {
			if elif.Body != nil && hasDeferStmt(elif.Body) {
				return true
			}
		}
	case *ast.ForStmt:
		if n == nil {
			return false
		}

		return n.Body != nil && hasDeferStmt(n.Body)
	case *ast.MatchStmt:
		if n == nil {
			return false
		}

		for _, arm := range n.Cases {
			if arm.Body != nil && hasDeferStmt(arm.Body) {
				return true
			}
		}
	}

	return false
}

// hasSelfTailCall reports whether body contains at least one direct self-call
// to funcName in tail position (as the sole expression of a where clause, or
// as the value of an explicit return statement).  Nested fn/lambda bodies are
// not descended into.
func hasSelfTailCall(funcName string, body ast.Node) bool {
	if body == nil {
		return false
	}

	switch n := body.(type) {
	case *ast.ReturnStmt:
		if n == nil {
			return false
		}

		return isSelfCallExpr(funcName, n.Value)
	case *ast.WhereList:
		if n == nil {
			return false
		}

		for _, c := range n.Clauses {
			if hasSelfTailCall(funcName, c.Body) {
				return true
			}
		}
	case *ast.Block:
		if n == nil {
			return false
		}

		for _, s := range n.Stmts {
			if hasSelfTailCall(funcName, s) {
				return true
			}
		}
	case *ast.IfStmt:
		if n == nil {
			return false
		}

		if hasSelfTailCall(funcName, n.Then) {
			return true
		}

		if hasSelfTailCall(funcName, n.Else) {
			return true
		}

		for _, elif := range n.ElseIfs {
			if hasSelfTailCall(funcName, elif.Body) {
				return true
			}
		}
	case *ast.MatchStmt:
		if n == nil {
			return false
		}

		for _, arm := range n.Cases {
			if hasSelfTailCall(funcName, arm.Body) {
				return true
			}
		}
	case *ast.ExprStmt:
		if n == nil {
			return false
		}

		return isSelfCallExpr(funcName, n.Expr)
	case *ast.FuncDecl, *ast.LambdaExpr:
		return false // don't descend into nested functions
	default:
		return isSelfCallExpr(funcName, n)
	}

	return false
}

// isSelfCallExpr returns true if node is a direct call to funcName.
func isSelfCallExpr(funcName string, node ast.Node) bool {
	if node == nil {
		return false
	}

	ce, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}

	ident, ok := ce.Func.(*ast.Identifier)
	if !ok {
		return false
	}

	return ident.Name == funcName
}

// emitTCOLoopBack handles a tail self-call in a TCO-eligible function:
// it evaluates the new argument values, releases any in-scope RC locals,
// stores the new values into the parameter allocas, and branches back to
// the tco_loop block instead of emitting a recursive call + return.
func (cg *CodeGen) emitTCOLoopBack(block *ir.Block, ce *ast.CallExpr) error {
	// Evaluate all new argument values before touching any alloca so that
	// expressions like fact(n-1, n*acc) can safely read n and acc.
	newVals := make([]value.Value, len(cg.tcoParams))

	for i, astArg := range ce.Args {
		val, err := cg.genExpr(block, astArg)
		if err != nil {
			return err
		}
		// Sync block advance (e.g. coro chain calls can redirect cg.curBlock).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
		// Coerce to the alloca's element type.
		if e, ok := cg.curScope.lookup(cg.tcoParams[i]); ok && e.isAlloc {
			if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 {
				val = cg.coerce(block, val, alloca.ElemType)
			}
		}

		newVals[i] = val
	}

	// Release any RC-tracked locals that are live in the current scope
	// (non-RC params are skipped automatically by emitAllScopeReleases).
	cg.emitAllScopeReleases(block, "")

	// Update the parameter allocas with the new values.
	for i, paramName := range cg.tcoParams {
		if e, ok := cg.curScope.lookup(paramName); ok && e.isAlloc {
			if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 {
				block.NewStore(newVals[i], alloca)
			}
		}
	}

	// Branch back to the loop header.
	block.NewBr(cg.tcoLoopTop)

	return nil
}

// resolveMutualTCOCallee checks whether name refers to a Tin function that can
// receive a musttail call from the current function. Returns the IR function and
// true when eligible; false otherwise.
func (cg *CodeGen) resolveMutualTCOCallee(name string) (*ir.Func, bool) {
	if cg.curFn == nil {
		return nil, false
	}

	entry, ok := cg.curScope.lookup(name)
	if !ok || entry.isAlloc {
		return nil, false
	}

	callee, ok := entry.val.(*ir.Func)
	if !ok {
		return nil, false
	}

	// Exclude C extern symbols (they may use different calling conventions).
	if cg.externIRNames[callee.Name()] {
		return nil, false
	}

	// Coroutine functions have their IR signatures transformed; skip mutual TCO.
	if cg.inCoroFn {
		return nil, false
	}

	// musttail requires identical return types.
	if !callee.Sig.RetType.Equal(cg.curFn.Sig.RetType) {
		return nil, false
	}

	// No variadic callees.
	if callee.Sig.Variadic {
		return nil, false
	}

	// musttail requires matching parameter counts and types (sibling call constraint).
	if len(callee.Params) != len(cg.curFn.Params) {
		return nil, false
	}

	for i, cp := range callee.Params {
		if !cp.Type().Equal(cg.curFn.Params[i].Type()) {
			return nil, false
		}
	}

	// All callee params must be non-RC so scope cleanup before the call is safe.
	for _, p := range callee.Params {
		if isRCTrackedType(p.Type()) {
			return nil, false
		}
	}

	// LLVM's musttail tail-call elimination refuses any caller whose frame
	// still has live allocas at the call site. A trivial pass-through fn
	// like `fn from(v f64) Value = return from_f64_impl(v)` spills `v` to
	// an alloca first, which is enough to break musttail. Skip when we can
	// see allocas in the caller's entry block.
	if hasAllocaInsts(cg.curFn) {
		return nil, false
	}

	return callee, true
}

// hasAllocaInsts reports whether fn currently contains any alloca
// instructions in any of its emitted blocks. Used to gate mutual TCO so
// musttail isn't requested from a frame that LLVM cannot pop.
//
// We scan every block, not just the entry, because allocas can be added
// past the call site (e.g. a deferred string interp builds an alloca in
// a successor block) and any live alloca anywhere in the function would
// keep LLVM from rewriting the musttail into a real tail jump.
func hasAllocaInsts(fn *ir.Func) bool {
	if fn == nil {
		return false
	}

	for _, blk := range fn.Blocks {
		for _, inst := range blk.Insts {
			if _, ok := inst.(*ir.InstAlloca); ok {
				return true
			}
		}
	}

	return false
}

// emitMutualTCO emits a musttail call to callee and returns its result,
// performing scope cleanup BEFORE the call so no instructions appear between
// the musttail call and the immediately following ret.
func (cg *CodeGen) emitMutualTCO(block *ir.Block, ce *ast.CallExpr, callee *ir.Func) error {
	// Evaluate all argument values before releasing scope.
	argVals := make([]value.Value, len(ce.Args))

	for i, arg := range ce.Args {
		cg.curBlock = block // sync before genExpr so stale values don't misdirect block updates

		v, err := cg.genExpr(block, arg)
		if err != nil {
			return err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		if i < len(callee.Params) {
			v = cg.coerce(block, v, callee.Params[i].Type())
		}

		argVals[i] = v
	}

	// Release all RC-tracked locals before the tail call.
	cg.emitAllScopeReleases(block, "")

	// Emit musttail call.
	call := block.NewCall(callee, argVals...)
	call.Tail = enum.TailMustTail

	if cg.tcoReportFn != nil {
		cg.tcoReportFn(cg.curFn.Name(), callee.Name())
	}

	// Return the call result directly (no post-processing, satisfying musttail).
	if irtypes.IsVoid(call.Type()) {
		block.NewRet(nil)
	} else {
		block.NewRet(call)
	}

	return nil
}

// isFutureRetType reports whether t is a Future[T] generic type.
func isFutureRetType(t ast.TypeExpr) bool {
	g, ok := t.(*ast.GenericType)

	return ok && g.Name == "Future"
}

// bodyContainsSpawnOrAwait reports whether any node in the body (recursively)
// is a SpawnExpr or AwaitExpr. Nested fn declarations are not descended into.
func bodyContainsSpawnOrAwait(body []ast.Node) bool {
	var walk func(node ast.Node) bool

	walk = func(node ast.Node) bool {
		if node == nil {
			return false
		}

		switch n := node.(type) {
		case *ast.SpawnExpr, *ast.AwaitExpr:
			return true
		case *ast.FuncDecl:
			return false // don't descend into nested fn declarations
		case *ast.Block:
			for _, s := range n.Stmts {
				if walk(s) {
					return true
				}
			}
		case *ast.ExprStmt:
			return walk(n.Expr)
		case *ast.VarDecl:
			return walk(n.Value)
		case *ast.AssignStmt:
			return walk(n.Value)
		case *ast.AugAssignStmt:
			return walk(n.Value)
		case *ast.ReturnStmt:
			return walk(n.Value)
		case *ast.EchoStmt:
			return walk(n.Value)
		case *ast.IfStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Then != nil && walk(n.Then) {
				return true
			}

			if n.Else != nil && walk(n.Else) {
				return true
			}
		case *ast.ForStmt:
			if walk(n.Cond) {
				return true
			}

			if n.Body != nil && walk(n.Body) {
				return true
			}
		case *ast.CallExpr:
			if walk(n.Func) {
				return true
			}

			for _, a := range n.Args {
				if walk(a) {
					return true
				}
			}
		case *ast.BinExpr:
			return walk(n.Left) || walk(n.Right)
		}

		return false
	}
	for _, s := range body {
		if walk(s) {
			return true
		}
	}

	return false
}

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
func (cg *CodeGen) typeBoundSatisfied(concreteName string, bound ast.TypeBound) (bool, *ast.TBAtom) {
	switch b := bound.(type) {
	case *ast.TBAtom:
		got := cg.structSatisfiesConstraint(concreteName, b.Trait)
		if b.Neg {
			got = !got
		}

		if got {
			return true, nil
		}

		return false, b

	case *ast.TBAnd:
		if ok, w := cg.typeBoundSatisfied(concreteName, b.Left); !ok {
			return false, w
		}

		if ok, w := cg.typeBoundSatisfied(concreteName, b.Right); !ok {
			return false, w
		}

		return true, nil

	case *ast.TBOr:
		if ok, _ := cg.typeBoundSatisfied(concreteName, b.Left); ok {
			return true, nil
		}

		if ok, _ := cg.typeBoundSatisfied(concreteName, b.Right); ok {
			return true, nil
		}
		// Both sides failed; report the right-side failure (the last one
		// tried) so the error message lists a concrete missing trait.
		_, w := cg.typeBoundSatisfied(concreteName, b.Right)

		return false, w
	}

	return true, nil
}

// formatStripWitnesses renders a list of dead-strip witnesses (one per
// stripped overload) for inline use in a single-line error message.
// Single witness: "doesn't match <bound>". Multi: "doesn't match any
// of: <bound>, <bound>, ...".
func formatStripWitnesses(witnesses []string) string {
	if len(witnesses) == 0 {
		return ""
	}

	if len(witnesses) == 1 {
		return "doesn't match " + witnesses[0]
	}

	return "doesn't match any of: " + strings.Join(witnesses, ", ")
}

// prettyStructName renders an IR-mangled generic instantiation name
// (e.g. "Box__bool", "Channel__string") back into the source-syntax
// form ("Box[bool]", "Channel[string]"). Multi-arg generics use the
// double-underscore separator inside the brackets too:
// "HashMap__string__i64" -> "HashMap[string, i64]".
//
// Plain (non-generic) struct names pass through unchanged.
func prettyStructName(s string) string {
	idx := strings.Index(s, "__")
	if idx < 0 {
		return s
	}

	base := s[:idx]
	rest := s[idx+2:]
	args := strings.ReplaceAll(rest, "__", ", ")

	return base + "[" + args + "]"
}

// methodConstraintWitness reports whether every where-clause on a generic
// struct method holds under the given type-parameter substitution. Returns
// an empty string when all constraints hold (the method survives), or a
// human-readable description of the FIRST failing constraint when it
// doesn't (the method is dead-stripped from the concrete struct).
//
// The returned witness format depends on the bound's shape so the
// diagnostic stays informative:
//   - pure leaf:      `where t is X` (t = "Y")
//   - AND with miss:  `where t is X && Z` failed at `Z` (t = "Y")
//   - OR all fail:    `where t is X || Z` matches neither (t = "Y")
//
// A method with no Constraints always survives.
func (cg *CodeGen) methodConstraintWitness(m *ast.FuncDecl, typeSubst map[string]string) string {
	if len(m.Constraints) == 0 {
		return ""
	}

	for _, c := range m.Constraints {
		concreteName, ok := typeSubst[c.TypeParam]
		if !ok {
			// The constraint references a type-param that isn't part
			// of the struct's substitution (e.g. a method-level
			// generic that hasn't been instantiated yet). Defer to
			// the call-site path, which already validates these.
			continue
		}

		ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound)
		if ok {
			continue
		}

		full := typeBoundString(c.Bound)

		// Mixed AND/OR bound -- pointing at the failing AND-conjunct
		// is genuinely informative: that's the specific missing
		// requirement.
		if !isPureOrBound(c.Bound) {
			return fmt.Sprintf("where %s is %s (missing %s)",
				c.TypeParam, full, typeBoundString(witness))
		}

		// Single-leaf or pure-OR bound: the bound itself describes
		// the requirement; the concrete type didn't satisfy it.
		_ = concreteName

		return fmt.Sprintf("where %s is %s", c.TypeParam, full)
	}

	return ""
}

// isPureOrBound reports whether b is built only from atoms and OR
// nodes (no AND). Used by the witness formatter to choose between
// "failed at sub-check" and "matches none of" wording.
func isPureOrBound(b ast.TypeBound) bool {
	switch v := b.(type) {
	case *ast.TBAtom:
		return true
	case *ast.TBOr:
		return isPureOrBound(v.Left) && isPureOrBound(v.Right)
	case *ast.TBAnd:
		return false
	}

	return true
}

// typeArgsContainAnyOf reports whether any type-argument expression in args
// references a name in the `paramNames` list as a top-level SimpleType.
// Used to decide whether a template's type alias still has symbolic type
// parameters (we skip constraint checks in that case).
func typeArgsContainAnyOf(args []ast.TypeExpr, paramNames []string) bool {
	nameSet := make(map[string]bool, len(paramNames))
	for _, n := range paramNames {
		nameSet[n] = true
	}

	for _, a := range args {
		if st, ok := a.(*ast.SimpleType); ok && nameSet[st.Name] {
			return true
		}
	}

	return false
}

// flattenPositiveTraits collects every non-negated leaf trait of a bound.
// Used by the monomorphizer to inject default trait methods the concrete
// type inherits. Negated atoms (`not X`) and OR branches are skipped since
// they describe what the type might lack, not what it must have.
func flattenPositiveTraits(bound ast.TypeBound) []ast.TypeExpr {
	var out []ast.TypeExpr

	var walk func(ast.TypeBound)

	walk = func(b ast.TypeBound) {
		switch v := b.(type) {
		case *ast.TBAtom:
			if !v.Neg {
				out = append(out, v.Trait)
			}
		case *ast.TBAnd:
			walk(v.Left)
			walk(v.Right)
		case *ast.TBOr:
			// OR can't guarantee either leaf holds, so we don't inject
			// default methods from either side.
		}
	}

	walk(bound)

	return out
}

// typeBoundString renders a TypeBound back to its source-level form so
// constraint-violation errors echo the user's own syntax.
func typeBoundString(bound ast.TypeBound) string {
	switch b := bound.(type) {
	case *ast.TBAtom:
		s := typeExprToString(b.Trait)
		if b.Neg {
			return "not " + s
		}

		return s
	case *ast.TBAnd:
		return typeBoundStringParen(b.Left) + " && " + typeBoundStringParen(b.Right)
	case *ast.TBOr:
		return typeBoundStringParen(b.Left) + " || " + typeBoundStringParen(b.Right)
	}

	return "<bound>"
}

// typeBoundStringParen wraps nested And/Or bounds in parens for unambiguous
// rendering. Atoms are rendered bare.
func typeBoundStringParen(bound ast.TypeBound) string {
	if _, ok := bound.(*ast.TBAtom); ok {
		return typeBoundString(bound)
	}

	return "(" + typeBoundString(bound) + ")"
}

// structSatisfiesConstraint checks that structName satisfies a trait expression.
// traitExpr may be a SimpleType ("labeled"), GenericType ("iter[i64]"), or a
// type alias that expands to a union ("addable" = i8|i16|i32|...).
func (cg *CodeGen) structSatisfiesConstraint(structName string, traitExpr ast.TypeExpr) bool {
	var traitName string

	switch te := traitExpr.(type) {
	case *ast.SimpleType:
		traitName = te.Name
	case *ast.GenericType:
		traitName = te.Name
	default:
		return false
	}

	// Built-in type-set shortcut for ord/comp on primitive types. Falls through
	// to the trait-impl path for non-primitives so user-defined ord/comp impls
	// can also satisfy these constraints.
	if traitName == "ord" && isOrdType(structName) {
		return true
	}

	if traitName == "comp" && isCompType(structName) {
		return true
	}

	// If the name is a tagged union type, the literal tagged-union type
	// itself satisfies the bound (`where t is num` matches t = num, the
	// whole union value), and so does any of its structural variants
	// (`where t is num` matches t = i64 when num = i64 | f64).
	if members, ok := cg.unionTypeMembers[traitName]; ok {
		if structName == traitName {
			return true
		}

		for _, member := range members {
			if cg.typeExprContains(member, structName) {
				return true
			}
		}

		return false
	}

	td, ok := cg.traits[traitName]
	if !ok {
		// Not a declared trait or union alias: type-equality constraint.
		// "where t is i64" is satisfied iff concreteName == "i64".
		return traitName == structName
	}

	// Where-shorthand: a bare reference to a single-type-param trait defaults
	// its parameter to the constrained type variable. So `where t is ord`
	// means `where t is ord[t]`. Multi-param traits (e.g. add[rhs, ret])
	// require explicit args.
	if _, isSimple := traitExpr.(*ast.SimpleType); isSimple && len(td.TypeParams) == 1 {
		traitExpr = &ast.GenericType{
			Name:       traitName,
			TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: structName}},
		}
	}

	bareKey := traitQualifierKey(bareTraitImplKey(traitExpr))

	if td.IsAlias {
		// Alias-form trait: the single method name equals the trait name.
		// Accept any of: explicit-args qualified form, base trait-name form,
		// or the plain alias registered by registerPlainMethodAliases.
		candidates := []string{
			structName + "_" + bareKey + "_" + traitName,
			structName + "_" + traitName + "_" + traitName,
			structName + "_" + traitName,
		}

		for _, c := range candidates {
			if _, found := cg.curScope.lookup(c); found {
				return true
			}
		}

		return false
	}

	for _, m := range td.Methods {
		if !m.IsVirtual {
			continue
		}

		qualName := structName + "_" + bareKey + "_" + m.Name
		plainName := structName + "_" + m.Name
		_, hasQual := cg.curScope.lookup(qualName)

		_, hasPlain := cg.curScope.lookup(plainName)
		if !hasQual && !hasPlain {
			return false
		}
	}

	return true
}

// typeExprContains reports whether the type named target is a member of te,
// recursively expanding tagged union types.
func (cg *CodeGen) typeExprContains(te ast.TypeExpr, target string) bool {
	switch t := te.(type) {
	case *ast.SimpleType:
		if t.Name == target {
			return true
		}

		// Recurse into tagged union members.
		if members, ok := cg.unionTypeMembers[t.Name]; ok {
			for _, member := range members {
				if cg.typeExprContains(member, target) {
					return true
				}
			}
		}

		return false
	case *ast.UnionTypeExpr:
		for _, member := range t.Types {
			if cg.typeExprContains(member, target) {
				return true
			}
		}

		return false
	default:
		return false
	}
}

// isOrdType reports whether typeName is an ordered type that supports <, <=, >, >=.
// Covers all integer and float primitives.
func isOrdType(typeName string) bool {
	switch typeName {
	case "i8", "i16", "i32", "i64", "i128",
		"u8", "u16", "u32", "u64", "u128",
		"f32", "f64", "f128",
		"byte", "char":
		return true
	}

	return false
}

// isCompType reports whether typeName is a comparable type that supports ==, !=.
// Covers all ordered types plus string, bool, and atoms.
func isCompType(typeName string) bool {
	return isOrdType(typeName) || typeName == "string" || typeName == "bool" || typeName == "__atom"
}

func (cg *CodeGen) genFuncDeclAs(n *ast.FuncDecl, scopeName string) error {
	// Generic functions are compiled on demand; register as template and skip.
	if len(n.TypeParams) > 0 && n.IsExtern == "" {
		cg.genericFuncs[n.Name] = n
		cg.genericFuncHomeScopes[n.Name] = cg.curScope

		return nil
	}

	// Mirror this monomorphized FuncDecl in funcDecls under the IR
	// scope name so call-site machinery (e.g. wildcard call-site
	// generics) can look up the original FuncDecl by the same key the
	// scope uses.
	if scopeName != "" {
		if _, present := cg.funcDecls[scopeName]; !present {
			cg.funcDecls[scopeName] = n
		}
	}

	// Build the mutated-names set for the if-condition folder. Restored
	// after the body so nested function generations don't leak names
	// across each other.
	prevMutated := cg.mutatedNames
	cg.mutatedNames = collectMutatedNames(n.Body)

	// {#unsafe} is a lexical block scope - a function defined inside an
	// unsafe block must NOT inherit the depth into its body. Reset the
	// counter on every function-body boundary and restore on exit.
	prevUnsafe := cg.unsafeDepth
	cg.unsafeDepth = 0

	defer func() {
		cg.mutatedNames = prevMutated
		cg.unsafeDepth = prevUnsafe
	}()

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		var err error

		retType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}

	if n.IsExtern != "" {
		// Extern functions are always side-effectful; ensure the tag is present.
		if !hasTag(n.Tags, "sideffect") {
			n.Tags = append(n.Tags, "sideffect")
		}
		// Collect non-varargs parameters with their C-level types.
		isVariadic := false

		var cParams []*ir.Param
		// cParamByval[i] is non-nil when cParams[i] uses byval (AMD64 large struct > 16 bytes).
		var cParamByval []*irtypes.StructType
		// tinParamToCIdx maps Tin parameter index (ignoring varargs) to the
		// starting index in cParams. Normally 1:1, but 2-register struct splits
		// insert an extra C param so subsequent indices shift.
		var tinParamToCIdx []int
		// cParam2RegNative[cIdx] is non-nil when cParams[cIdx] is the FIRST
		// of a 2-register split pair (9-16 byte all-integer struct, AMD64/ARM64).
		var cParam2RegNative []*irtypes.StructType
		// cParamARM64Indirect[cIdx] is non-nil when cParams[cIdx] is a plain
		// pointer (*T) for ARM64 non-HFA large struct indirect passing. The C
		// function receives a pointer to a stack copy of the struct.
		var cParamARM64Indirect []*irtypes.StructType

		for _, p := range n.Params {
			if p.IsVarArgs {
				isVariadic = true

				continue
			}

			if vt, ok := p.Type.(*ast.SimpleType); ok && vt.Name == "..." {
				isVariadic = true

				continue
			}

			ct, err := cg.tinTypeToExternLLVM(p.Type, false)
			if err != nil {
				return err
			}

			tinParamToCIdx = append(tinParamToCIdx, len(cParams))

			// Large struct passing (>16 bytes) is ABI-dependent:
			//   AMD64 x86-64 SysV: all large structs use byval (implicit pointer copy).
			//   ARM64 AAPCS64:
			//     - HFA (1-4 identical float fields, any size): pass directly in VFP regs.
			//     - Non-HFA large: pass as plain *T pointer (not byval) matching AAPCS64
			//       "composite type passed indirectly" rule without the LLVM byval alignment
			//       complications that cause crashes on ARM64 Linux.
			// For 9-16 byte all-integer structs, both ABIs use two integer registers.
			nativeSt, isNativeSt := ct.(*irtypes.StructType)

			if isNativeSt && nativeStructNeedsByval(nativeSt) && cg.targetIsAMD64() {
				// AMD64: use byval for large non-HFA structs.
				bvParam := ir.NewParam(p.Name, irtypes.I8Ptr)
				bvParam.Attrs = append(bvParam.Attrs, ir.Byval{Typ: nativeSt})
				cParams = append(cParams, bvParam)
				cParamByval = append(cParamByval, nativeSt)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			} else if isNativeSt && nativeStructNeedsByval(nativeSt) && cg.targetIsARM64() && !isNativeStructHFA(nativeSt) {
				// ARM64 non-HFA large struct: pass as plain pointer (*T).
				// Callee (Clang) receives the pointer in an integer register (x0/x1...).
				// This matches AAPCS64 composite indirect passing without byval alignment issues.
				ptrParam := ir.NewParam(p.Name, irtypes.NewPointer(nativeSt))
				cParams = append(cParams, ptrParam)
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nativeSt)
			} else if isNativeSt && coerceNativeStructForABI2Reg(nativeSt) && (cg.targetIsAMD64() || cg.targetIsARM64()) {
				// 9-16 byte all-integer struct: split into two i64 params.
				// x86-64 SysV: two integer eightbytes in rdi/rsi etc.
				// AAPCS64: two consecutive x-registers (x0/x1 etc.).
				// Both ABIs represent this as (i64, i64) in LLVM IR.
				cParams = append(cParams, ir.NewParam(p.Name+".lo", irtypes.I64))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nativeSt)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)

				cParams = append(cParams, ir.NewParam(p.Name+".hi", irtypes.I64))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			} else {
				// Direct pass: small structs, HFA structs (ARM64 VFP regs), primitives.
				cParams = append(cParams, ir.NewParam(p.Name, ct))
				cParamByval = append(cParamByval, nil)
				cParam2RegNative = append(cParam2RegNative, nil)
				cParamARM64Indirect = append(cParamARM64Indirect, nil)
			}
		}
		// Compute C-level return type.
		var cRetType irtypes.Type = irtypes.Void

		if n.RetType != nil {
			var err error

			cRetType, err = cg.tinTypeToExternLLVM(n.RetType, true)
			if err != nil {
				return err
			}
		}

		// sret: structs > 16 bytes are returned via a hidden pointer argument.
		// AMD64 (x86-64 SysV): hidden pointer in rdi.
		// ARM64 (AAPCS64): hidden pointer in x8 (indirect result register).
		// In both cases the LLVM IR uses void return + sret first parameter;
		// the backend maps it to rdi or x8 respectively. Without this, LLVM
		// generates incorrect multi-register returns that mismatch the C callee.
		var cRetSRetSt *irtypes.StructType

		if cg.targetIsAMD64() || cg.targetIsARM64() {
			if nativeSt, ok := cRetType.(*irtypes.StructType); ok && nativeStructNeedsByval(nativeSt) {
				cRetSRetSt = nativeSt
				sretParam := ir.NewParam(".sret", irtypes.NewPointer(nativeSt))
				sretParam.Attrs = append(sretParam.Attrs, ir.SRet{Typ: nativeSt})
				cParams = append([]*ir.Param{sretParam}, cParams...)
				cParamByval = append([]*irtypes.StructType{nil}, cParamByval...)
				cParam2RegNative = append([]*irtypes.StructType{nil}, cParam2RegNative...)
				cParamARM64Indirect = append([]*irtypes.StructType{nil}, cParamARM64Indirect...)

				for i := range tinParamToCIdx {
					tinParamToCIdx[i]++
				}

				cRetType = irtypes.Void
			}
		}

		// Create (or reuse) the raw C declaration with C-level types.
		cFunc := cg.ensureExternDecl(n.IsExtern, cRetType, cParams, isVariadic)

		if cg.curScope == nil {
			cg.curScope = newScope(nil)
		}

		// Detect if any parameter or return type is a named Tin struct that needs
		// Tin->C conversion at the call boundary.
		needsStructConv := false

		for _, p := range n.Params {
			if p.IsVarArgs {
				continue
			}

			if _, isStruct := cg.isNamedTinStruct(p.Type); isStruct {
				needsStructConv = true

				break
			}

			// *S pointer params where S has a hidden C pointer field.
			if cg.isExternPtrParam(p.Type) {
				needsStructConv = true

				break
			}

			// N*S output-parameter pattern: C writes (N-1)*S.native into N*S.
			if _, _, isDbl := cg.isExternOutPtrParam(p.Type); isDbl {
				needsStructConv = true

				break
			}
		}

		if n.RetType != nil {
			if _, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
				needsStructConv = true
			}
		}

		// If the return type does not need wrapping and no struct params, expose
		// the C function directly.  Fat-ptr parameters are handled by coerce().
		// #handover always needs a wrapper to RC-ify the returned pointer.
		if cRetType.Equal(retType) && !needsStructConv && !hasTag(n.Tags, "handover") {
			cg.curScope.set(scopeName, &scopeEntry{val: cFunc, isAlloc: false})

			return nil
		}

		// Generate a thin wrapper that handles type conversions.
		// For struct interop: wrapper takes Tin-level params (full struct), converts
		// to C-native layout, calls C, converts result back to Tin layout.
		// For other types (e.g. char* -> string): same as before.
		wrapperName := "__tinwrap_" + scopeName

		var wrapperFn *ir.Func

		for _, f := range cg.allFuncs() {
			if f.Name() == wrapperName {
				wrapperFn = f

				break
			}
		}

		if wrapperFn == nil {
			// Build wrapper params: one per Tin parameter (not per C param, since
			// 2-register splits create extra C params for a single Tin param).
			var wrapperParams []*ir.Param

			tinNonVarargIdx := 0

			for _, p := range n.Params {
				if p.IsVarArgs {
					continue
				}

				if vt, ok := p.Type.(*ast.SimpleType); ok && vt.Name == "..." {
					continue
				}

				cIdx := tinParamToCIdx[tinNonVarargIdx]

				if sName, isStruct := cg.isNamedTinStruct(p.Type); isStruct {
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(sName, tinType))
				} else if cg.isExternPtrParam(p.Type) {
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(cParams[cIdx].Name(), tinType))
				} else if _, _, isDbl := cg.isExternOutPtrParam(p.Type); isDbl {
					// N*S output-parameter: wrapper receives N*%S.wrapper from Tin caller.
					tinType, _ := cg.tinTypeToLLVM(p.Type)
					wrapperParams = append(wrapperParams, ir.NewParam(cParams[cIdx].Name(), tinType))
				} else {
					wrapperParams = append(wrapperParams, cParams[cIdx])
				}

				tinNonVarargIdx++
			}

			wrapperFn = cg.mod.NewFunc(wrapperName, retType, wrapperParams...)
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wrapperFn
			cg.curScope = newScope(prevScope)
			entry := wrapperFn.NewBlock("entry")

			// Build C-level call args: convert struct params to native, pass others as-is.
			callArgs := make([]value.Value, len(cParams))

			// AMD64 sret: pre-allocate the result buffer and put its address at index 0.
			var sretResultAlloca value.Value
			if cRetSRetSt != nil {
				sretResultAlloca = entry.NewAlloca(cRetSRetSt)
				callArgs[0] = ir.NewArg(sretResultAlloca, ir.SRet{Typ: cRetSRetSt})
			}

			// dblPtrWritebacks records N*S (N>=2) params that need post-call write-back:
			// after C writes (N-1)*S.native to the slot, we wrap the chain and store
			// the result into the Tin caller's location.
			type dblPtrWriteback struct {
				wrapperParamIdx int
				slot            value.Value // alloca holding (depth-1)*S.native
				structName      string
				depth           int // total Tin param depth N (>= 2)
			}

			var dblPtrWritebacks []dblPtrWriteback

			tinNonVarargIdx = 0
			wrapperPIdx := 0

			for _, tinParam := range n.Params {
				if tinParam.IsVarArgs {
					continue
				}

				if vt, ok := tinParam.Type.(*ast.SimpleType); ok && vt.Name == "..." {
					continue
				}

				cIdx := tinParamToCIdx[tinNonVarargIdx]
				p := wrapperFn.Params[wrapperPIdx]

				if sName, isStruct := cg.isNamedTinStruct(tinParam.Type); isStruct {
					native, err := cg.wrapStructToExtern(entry, p, sName)
					if err != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return err
					}
					// For byval params (AMD64 large structs > 16 bytes): alloca native
					// struct, store the converted value, then pass a byval-attributed pointer.
					if cParamByval[cIdx] != nil {
						nativeAlloca := entry.NewAlloca(cParamByval[cIdx])
						entry.NewStore(native, nativeAlloca)
						ptr := entry.NewBitCast(nativeAlloca, irtypes.I8Ptr)
						callArgs[cIdx] = ir.NewArg(ptr, ir.Byval{Typ: cParamByval[cIdx]})
					} else if cParamARM64Indirect[cIdx] != nil {
						// ARM64 non-HFA large struct: alloca + pass plain pointer.
						nativeAlloca := entry.NewAlloca(cParamARM64Indirect[cIdx])
						entry.NewStore(native, nativeAlloca)
						callArgs[cIdx] = nativeAlloca
					} else if cParam2RegNative[cIdx] != nil {
						// 9-16 byte all-integer struct: split into two i64 halves
						// to match clang's x86-64 SysV / AAPCS64 (i64, i64) coercion.
						nativeSt := cParam2RegNative[cIdx]
						a := entry.NewAlloca(nativeSt)
						entry.NewStore(native, a)
						loPtr := entry.NewBitCast(a, irtypes.NewPointer(irtypes.I64))
						lo := entry.NewLoad(irtypes.I64, loPtr)
						hiRaw := entry.NewGetElementPtr(irtypes.I8, entry.NewBitCast(a, irtypes.I8Ptr),
							constant.NewInt(irtypes.I64, 8))
						hiPtr := entry.NewBitCast(hiRaw, irtypes.NewPointer(irtypes.I64))
						hi := entry.NewLoad(irtypes.I64, hiPtr)
						callArgs[cIdx] = lo
						callArgs[cIdx+1] = hi
					} else if intTy, isInt := cParams[cIdx].Type().(*irtypes.IntType); isInt {
						// Small all-integer struct coerced to integer register.
						if nativeSt, ok2 := native.Type().(*irtypes.StructType); ok2 {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							if structBits < intTy.BitSize {
								// Coerced type is wider than the struct (ARM64: <=8-byte
								// struct -> i64). Load at the struct's natural bit size
								// to avoid an out-of-bounds read, then zero-extend.
								smallTy := irtypes.NewInt(structBits)
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(smallTy))
								small := entry.NewLoad(smallTy, ip)
								native = entry.NewZExt(small, intTy)
							} else {
								a := entry.NewAlloca(nativeSt)
								entry.NewStore(native, a)
								ip := entry.NewBitCast(a, irtypes.NewPointer(intTy))
								native = entry.NewLoad(intTy, ip)
							}
						}

						callArgs[cIdx] = native
					} else {
						callArgs[cIdx] = native
					}
				} else if cg.isExternPtrParam(tinParam.Type) {
					// *S param with hidden C pointer: extract it and pass to C.
					callArgs[cIdx] = cg.extractCSrcPtr(entry, p, tinParam.Type, cParams[cIdx].Type())
				} else if sName, depth, isDbl := cg.isExternOutPtrParam(tinParam.Type); isDbl {
					// N*S output-parameter: allocate a (depth-1)*S.native slot.
					// Pass &slot to C as (depth)*S.native; after the call wrap and write back.
					nativeSt, _ := cg.tinStructNativeLLVM(sName)
					// Build (depth-1)*S.native type for the slot content.
					// depth >= 2, so after the loop contentType is always a pointer type.
					var contentType irtypes.Type = nativeSt
					for j := 0; j < depth-1; j++ {
						contentType = irtypes.NewPointer(contentType)
					}

					contentPtrType := contentType.(*irtypes.PointerType)
					slot := entry.NewAlloca(contentPtrType)
					entry.NewStore(constant.NewNull(contentPtrType), slot)
					callArgs[cIdx] = slot
					dblPtrWritebacks = append(dblPtrWritebacks, dblPtrWriteback{wrapperPIdx, slot, sName, depth})
				} else {
					callArgs[cIdx] = p
				}

				tinNonVarargIdx++
				wrapperPIdx++
			}

			rawCall := entry.NewCall(cFunc, callArgs...)

			// AMD64 sret: load the actual result from the pre-allocated buffer.
			var rawResult value.Value = rawCall
			if cRetSRetSt != nil {
				rawResult = entry.NewLoad(cRetSRetSt, sretResultAlloca)
			}

			// Convert result: if C returned a native struct, wrap back to Tin.
			var finalResult value.Value

			if n.RetType != nil {
				if sName, isStruct := cg.isNamedTinStruct(n.RetType); isStruct {
					// If C returned a coerced integer (ARM64: i64, AMD64: i32),
					// convert it back to the native struct type before wrapping.
					nativeResult := rawResult
					if intTy, isInt := rawResult.Type().(*irtypes.IntType); isInt {
						if nativeSt, err2 := cg.tinStructNativeLLVM(sName); err2 == nil {
							structBits := uint64(nativeStructByteSize(nativeSt)) * 8
							nativeAlloca := entry.NewAlloca(nativeSt)

							if structBits < intTy.BitSize {
								// Wider coercion (ARM64: i64 -> struct); truncate first.
								smallTy := irtypes.NewInt(structBits)
								truncated := entry.NewTrunc(rawResult, smallTy)
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(smallTy))
								entry.NewStore(truncated, ip)
							} else {
								ip := entry.NewBitCast(nativeAlloca, irtypes.NewPointer(intTy))
								entry.NewStore(rawResult, ip)
							}

							nativeResult = entry.NewLoad(nativeSt, nativeAlloca)
						}
					}

					tinResult, err := cg.wrapNativeStructToTin(entry, nativeResult, sName)
					if err != nil {
						cg.curFn = prevFn
						cg.curScope = prevScope

						return err
					}

					finalResult = tinResult
				} else {
					finalResult = cg.wrapFromExtern(entry, rawResult, retType, hasTag(n.Tags, "handover"))
				}
			}

			// Post-call write-backs for N*S output parameters (N >= 2).
			// For each param, C may have written (N-1)*S.native into the slot.
			// Read what C wrote; if non-null build a Tin wrapper chain and store
			// it into the Tin caller's location; if null store null.
			curBlock := entry

			for i, wb := range dblPtrWritebacks {
				nativeSt, _ := cg.tinStructNativeLLVM(wb.structName)
				// Build (depth-1)*S.native type to load from slot.
				var contentType irtypes.Type = nativeSt

				for j := 0; j < wb.depth-1; j++ {
					contentType = irtypes.NewPointer(contentType)
				}

				nativeVal := curBlock.NewLoad(contentType, wb.slot)

				wbNull := wrapperFn.NewBlock(fmt.Sprintf("wb%d_null", i))
				wbWrap := wrapperFn.NewBlock(fmt.Sprintf("wb%d_wrap", i))
				wbDone := wrapperFn.NewBlock(fmt.Sprintf("wb%d_done", i))

				isNull := curBlock.NewICmp(enum.IPredEQ,
					curBlock.NewBitCast(nativeVal, irtypes.I8Ptr),
					constant.NewNull(irtypes.I8Ptr))
				curBlock.NewCondBr(isNull, wbNull, wbWrap)

				// Null path: write a null of (depth-1)*S Tin type.
				var innerTinType ast.TypeExpr = &ast.SimpleType{Name: wb.structName}

				for j := 0; j < wb.depth-1; j++ {
					innerTinType = &ast.PointerType{Elem: innerTinType}
				}

				tinPtrTypeRaw, _ := cg.tinTypeToLLVM(innerTinType)
				tgtPt := tinPtrTypeRaw.(*irtypes.PointerType)
				wbParam := wrapperFn.Params[wb.wrapperParamIdx]
				wbNull.NewStore(constant.NewNull(tgtPt), wbParam)
				wbNull.NewBr(wbDone)

				// Non-null path: recursively build Tin wrapper chain for depth-1 levels.
				wrapperVal, wbErr := cg.emitWrapNativeChain(wbWrap, nativeVal, wb.structName, wb.depth-1)
				if wbErr != nil {
					cg.curFn = prevFn
					cg.curScope = prevScope

					return wbErr
				}

				wbWrap.NewStore(wrapperVal, wbParam)
				wbWrap.NewBr(wbDone)

				curBlock = wbDone
			}

			if irtypes.IsVoid(retType) {
				curBlock.NewRet(nil)
			} else {
				curBlock.NewRet(finalResult)
			}

			cg.curFn = prevFn
			cg.curScope = prevScope
		}

		// Pointer returns from extern wrappers: mark as heap-promoting so that
		// genLetStmt sets isHeapOwned on the bound variable, enabling scope-exit
		// release via emitHeapChainRelease / ensureStructPtrReleaseFn.
		// String/fat-ptr returns are already RC-tracked; only raw pointer types need this.
		// Applies to both #handover (C frees original) and non-handover borrow
		// (Tin owns the RC copy).
		if _, isPtr := retType.(*irtypes.PointerType); isPtr {
			cg.heapPromotingFns[wrapperName] = true
			cg.heapPromotingFns[scopeName] = true
		}

		cg.curScope.set(scopeName, &scopeEntry{val: wrapperFn, isAlloc: false})

		return nil
	}

	// Look up pre-declared function in global scope (by qualified name), or create.
	var f *ir.Func

	if entry, ok := cg.curScope.vars[scopeName]; ok {
		if fn, isFunc := entry.val.(*ir.Func); isFunc {
			f = fn
		}
	}

	if f == nil {
		// Not pre-declared - create now (e.g. nested or struct method).
		params := make([]*ir.Param, len(n.Params))
		for i, p := range n.Params {
			pt, err := cg.tinTypeToLLVM(p.Type)
			if err != nil {
				return err
			}

			params[i] = ir.NewParam(p.Name, pt)
		}

		f = cg.mod.NewFunc(scopeName, retType, params...)
	}

	if n.Body == nil {
		f.Blocks = nil // Forward declaration - no body.

		return nil
	}

	// If function already has a body (re-declaration), skip.
	if len(f.Blocks) > 0 {
		return nil
	}

	// Create entry block.
	entry := f.NewBlock("entry")

	// Save context (including defer lists - each function has its own).
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevBlock := cg.curBlock
	prevDeferFnI8s := cg.pendingDeferFnI8s
	prevDeferFrames := cg.pendingDeferFrames
	prevDeferEnvs := cg.pendingDeferEnvs
	prevAutoYield := cg.curFnAutoYield
	prevDeferRetSlotParam := cg.curDeferRetSlotParam
	prevFnDeferRetAlloca := cg.curFnDeferRetAlloca
	prevDeferThunkRetType := cg.curDeferThunkRetType
	prevEscapingVars := cg.curFnEscapingVars
	prevEscapingAliases := cg.curFnEscapingAliases
	prevDiScope := cg.diCurrentScope
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.curBlock = nil
	cg.curFnAutoYield = false // sync variant never auto-yields
	cg.curDeferRetSlotParam = nil
	cg.curDeferThunkRetType = nil

	cg.curFnEscapingVars, cg.curFnEscapingAliases = findEscapingAddressTakenVars(n.Body)

	heapPromoting := len(cg.curFnEscapingVars) > 0 || hasDirectHeapReturn(n.Body, cg.heapPromotingFns)

	if heapPromoting {
		cg.heapPromotingFns[scopeName] = true
		// Also store under the actual IR function name (which may include a
		// parameter-type suffix, e.g. "json__parse_value__ptr_Parser") so that
		// genLetStmt can find it via the scope-resolved *ir.Func lookup.
		if f != nil {
			cg.heapPromotingFns[f.Name()] = true
		}
	}

	cg.curFn = f
	cg.curScope = newScope(cg.curScope)
	cg.curScope.isFunctionBoundary = true

	// Record the source file for this fn. pclntab.go uses this at the
	// post-pass to emit per-fn header entries with correct file paths
	// even when imports from other files were processed earlier (which
	// would leave cg.filename pointing at a different .tin source).
	if f != nil && cg.filename != "" {
		if cg.fnSourceFiles == nil {
			cg.fnSourceFiles = map[string]string{}
		}

		cg.fnSourceFiles[f.Name()] = cg.filename
	}

	// Record the user-visible display name (`pkg::name` for top-level
	// fns, `pkg::Struct.method` when cg.curMethodReceiverStruct is set
	// before calling here). pclntab.go's unmangleTinName consults this
	// map at trace render time so users see source-level names instead
	// of IR-mangled ones (`sync__AtomicI64_deinit` vs `sync::AtomicI64.deinit`).
	if f != nil && f.Name() != "" {
		cg.recordFnDisplayName(f.Name(), n)
	}

	// Emit DISubprogram for debug builds, and seed currentPos so that the
	// parameter allocas and first body instruction are tagged with the
	// function declaration's line rather than line 0.
	cg.emitDbgSubprogram(n, f, cg.filename)

	if cg.debugMode && n.Pos().Line != 0 {
		cg.currentPos = n.Pos()
	}

	// For non-void functions that contain defer stmts: alloca a {i8, retType} slot
	// so a defer thunk can override the return value.  Skip when no defer is present
	// to avoid generating dead code in the common case.
	if !irtypes.IsVoid(retType) && hasDeferStmt(n.Body) {
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotAlloca := entry.NewAlloca(slotType)
		// Zero-initialize the valid byte.
		validGep := entry.NewGetElementPtr(slotType, slotAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
		cg.curFnDeferRetAlloca = entry.NewBitCast(slotAlloca, irtypes.I8Ptr)
	} else {
		cg.curFnDeferRetAlloca = nil
	}

	// Always restore context, even on error paths (e.g. when genBody returns
	// an error during on-demand monomorphization of a generic struct method).
	prevTCOFuncName := cg.tcoFuncName
	prevTCOLoopTop := cg.tcoLoopTop
	prevTCOParams := cg.tcoParams
	prevMutualTCO := cg.mutualTCOEligible

	defer func() {
		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs
		cg.curFnAutoYield = prevAutoYield
		cg.curDeferRetSlotParam = prevDeferRetSlotParam
		cg.curFnDeferRetAlloca = prevFnDeferRetAlloca
		cg.curDeferThunkRetType = prevDeferThunkRetType
		cg.curFnEscapingVars = prevEscapingVars
		cg.curFnEscapingAliases = prevEscapingAliases
		cg.diCurrentScope = prevDiScope
		cg.tcoFuncName = prevTCOFuncName
		cg.tcoLoopTop = prevTCOLoopTop
		cg.tcoParams = prevTCOParams
		cg.mutualTCOEligible = prevMutualTCO
	}()

	// Register function in current scope so recursion works.
	cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

	// Mark the LLVM function as variadic if any tin param is varargs.
	for _, p := range n.Params {
		if p.IsVarArgs {
			f.Sig.Variadic = true

			break
		}
	}

	// Alloca parameters and register them in scope.
	// Iterate tin params; skip varargs (no LLVM parameter), but register a
	// null placeholder so the name is defined inside the body.
	var firstParamAlloca *ir.InstAlloca

	llIdx := 0

	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			if astParam.Name != "" {
				// Register as null i8* placeholder; true forwarding needs va_list.
				null := constant.NewNull(irtypes.NewPointer(irtypes.I8))
				cg.curScope.set(astParam.Name, &scopeEntry{val: null, isAlloc: false})
			}

			continue
		}

		p := f.Params[llIdx]
		llIdx++
		alloca := entry.NewAlloca(p.Type())
		entry.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		cg.emitRetain(entry, p)
		// Emit dbg.declare for this parameter in debug builds.
		cg.emitDbgDeclare(entry, alloca, astParam.Name, n.Pos().Line, uint64(llIdx), astParam.Type, p.Type())
		// Function parameters receive a by-value copy of the caller's struct.
		// The parameter is not the owner of the value; the caller is.  Mark
		// noDeinit so that scope-exit release of the parameter copy does not
		// invoke deinit (which would be a spurious call from the callee's
		// perspective and could double-free external resources).
		cg.curScope.set(astParam.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, noDeinit: true, isUnsigned: isUnsignedTinType(astParam.Type), scalarTypeName: scalar8BitTypeName(astParam.Type), tinType: astParam.Type, declPos: n.Pos()})
		cg.warnIfBuiltinShadow("param", astParam.Name, n.Pos())

		if llIdx == 1 {
			firstParamAlloca = alloca
		}
	}

	// TCO eligibility: direct, sync, non-extern, non-overloaded function whose
	// body contains at least one direct self tail call and has no defers. Params
	// must all be non-RC (no strings, arrays, any, fn) so we can update allocas
	// safely. Overloaded functions are excluded because a same-name call in the
	// body may target a sibling overload with different parameter types, not the
	// function itself.
	isTCO := n.IsExtern == "" &&
		!isAsyncTag(n.Tags) &&
		!hasDeferStmt(n.Body) &&
		!cg.overloadedNames[n.Name] &&
		len(n.Params) > 0 &&
		hasSelfTailCall(n.Name, n.Body)

	if isTCO {
		for _, astP := range n.Params {
			if astP.IsVarArgs {
				isTCO = false

				break
			}

			if e, ok := cg.curScope.lookup(astP.Name); ok && e.isAlloc {
				if alloca, ok2 := e.val.(*ir.InstAlloca); ok2 && isRCTrackedType(alloca.ElemType) {
					isTCO = false

					break
				}
			}
		}
	}

	// startBlock is where the function body and match-subject load are emitted.
	// For TCO functions, entry only holds param allocas then jumps to tco_loop.
	startBlock := entry

	if isTCO {
		tcoLoop := f.NewBlock("tco_loop")
		entry.NewBr(tcoLoop)

		cg.tcoFuncName = n.Name
		cg.tcoLoopTop = tcoLoop
		cg.tcoParams = nil

		for _, astP := range n.Params {
			if !astP.IsVarArgs {
				cg.tcoParams = append(cg.tcoParams, astP.Name)
			}
		}

		startBlock = tcoLoop

		if cg.tcoReportFn != nil {
			cg.tcoReportFn(n.Name, "")
		}
	}

	// Mutual TCO eligibility: same as self-TCO but for calls to OTHER functions.
	// Requires non-RC return type so the musttail call result can be returned
	// directly without retain/release between the call and the ret instruction.
	// Async functions (including those that implicitly return Future[T]) are excluded
	// because their IR signatures change during the coro split pass.
	cg.mutualTCOEligible = n.IsExtern == "" &&
		!isAsyncTag(n.Tags) &&
		!isFutureRetType(n.RetType) &&
		!hasDeferStmt(n.Body) &&
		!isRCTrackedType(retType)

	// For where-list bodies, set the match subject to the first parameter so
	// that atom conditions (e.g. `where 'ok:`) compare against it.
	// The load is placed in startBlock so it re-executes on every loop iteration.
	prevMatchSubject := cg.matchSubject

	if _, isWhere := n.Body.(*ast.WhereList); isWhere && firstParamAlloca != nil {
		loadInst := startBlock.NewLoad(firstParamAlloca.ElemType, firstParamAlloca)
		cg.attachCurrentDbgLoc(loadInst)
		cg.matchSubject = loadInst
	}

	// Generate body (genBody ensures a terminator is added to the current block).
	_, bodyErr := cg.genBody(startBlock, n.Body, retType)
	cg.matchSubject = prevMatchSubject

	// Ensure all call instructions have !dbg (LLVM requires this when the
	// function has a DISubprogram attached).
	cg.ensureAllCallsHaveDbg(f)

	if bodyErr != nil {
		// Even on error, register the (partially compiled) function so it
		// appears in scope for callers that check for it. The error typically
		// occurs during on-demand monomorphization triggered from inside another
		// function body; the caller discards the error but still needs the fn.
		prevScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})

		return bodyErr
	}

	// Restore context explicitly here (the defer is a safety net for error paths).
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.curBlock = prevBlock
	cg.pendingDeferFnI8s = prevDeferFnI8s
	cg.pendingDeferFrames = prevDeferFrames
	cg.pendingDeferEnvs = prevDeferEnvs
	cg.curFnEscapingVars = prevEscapingVars
	cg.curFnEscapingAliases = prevEscapingAliases
	cg.diCurrentScope = prevDiScope
	cg.tcoFuncName = prevTCOFuncName
	cg.tcoLoopTop = prevTCOLoopTop
	cg.tcoParams = prevTCOParams

	// Note: #no_recurse is enforced by checkAllNoRecurseFuncs (AST-level,
	// transitive) before this function is ever compiled. No IR walk needed.

	// Ensure function is registered in current scope.
	if cg.curScope != nil {
		cg.curScope.set(scopeName, &scopeEntry{val: f, isAlloc: false})
	}

	// If this function is in the async-callable set (or has #async tag directly),
	// generate its $coro variant. The #async tag check catches local functions
	// that were not discovered by the pre-pass call graph analysis.
	if cg.coroCallable[scopeName] || hasTag(n.Tags, "async") {
		if !cg.coroCallable[scopeName] {
			cg.coroCallable[scopeName] = true
		}

		coroKey := coroVersionName(scopeName)
		// Ensure the $coro stub exists in the current scope's vars before calling
		// genCoroFuncBody. For top-level functions the pre-pass already registered
		// the stub - predeclareCoroVariant is a no-op when vars[coroKey] is set.
		// For local/monomorphized async functions (not in the pre-pass), this
		// creates the stub so genCoroFuncBody can find it.
		if err := cg.predeclareCoroVariant(n, scopeName, false); err != nil {
			return err
		}

		if err := cg.genCoroFuncBody(n, coroKey, nil, nil); err != nil {
			return err
		}
	}

	return nil
}

// genImplicitMain creates a main() function containing the top-level statements.
func (cg *CodeGen) genImplicitMain(stmts []ast.Node) error {
	if bodyContainsSpawnOrAwait(stmts) && len(stmts) > 0 {
		cg.warn(DiagAsyncMain, stmts[0].Pos(),
			"top-level statements use 'spawn' or 'await' but there is no async main(); "+
				"each await at the top level creates a temporary fiber, which is slower and "+
				"bypasses inline channel optimizations. Fix: wrap your code in 'fn{#async} main() = ...' instead")
	}

	f := cg.newCMainWrapper(false)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	cg.curScope = newScope(cg.curScope)

	// Attach a DISubprogram so lldb/gdb can resolve `main` to the user's
	// source file. The implicit main spans all top-level statements; use the
	// first statement's line as the scope line.
	mainLine := 1
	if len(stmts) > 0 && stmts[0].Pos().Line > 0 {
		mainLine = stmts[0].Pos().Line
	}

	prevDbgScope := cg.diCurrentScope
	cg.emitDbgSubprogramForSynthetic(f, "main", mainLine)

	defer func() { cg.diCurrentScope = prevDbgScope }()

	// Seed currentPos so that preamble instructions (fiber init, var inits)
	// carry the first statement's source line rather than line 0. Without this
	// `br set -n main` in lldb lands on line 0 before the user's code.
	if cg.debugMode && len(stmts) > 0 {
		cg.currentPos = stmts[0].Pos()
	}

	// Emit fiber init if the program uses any fiber features.
	entry = cg.emitFiberMainWrap(entry)

	// Register the deinit dispatcher with libc atexit BEFORE running
	// any user code. See codegen.go's main wrapper for rationale.
	entry = cg.emitDeinitAllAtexit(entry)

	// Register per-type-id any-release helpers so that any-boxed
	// structs run their deinit on scope exit instead of just freeing
	// the heap block.
	entry = cg.emitAnyDispatchRegistrations(entry)

	// Emit top-level var runtime initializations (deferred from pre-pass 1.7).
	var err error

	entry, err = cg.emitTopLevelVarInits(entry)
	if err != nil {
		return err
	}

	cg.emitPkgInitFns(entry)

	// Seed cg.mutatedNames from the union of every top-level statement so
	// the if-condition folder treats reassigned top-level lets as non-const.
	// Without this, `let alive bool = true` followed by `alive = false`
	// later in the implicit main produces phantom "always true" warnings on
	// any `if alive:` between the binding and the first mutation.
	prevMutated := cg.mutatedNames
	cg.mutatedNames = collectMutatedNamesFromStmts(stmts)

	defer func() { cg.mutatedNames = prevMutated }()

	for _, stmt := range stmts {
		entry, _, err = cg.genStmt(entry, stmt)
		if err != nil {
			return err
		}

		if entry == nil {
			break
		}
	}

	if entry != nil && entry.Term == nil {
		_ = cg.emitDefers(entry)
		cg.emitAllScopeReleases(entry, "")
		cg.emitFiberMainEnd(entry)
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.ensureAllCallsHaveDbg(f)

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}

// genTestRunner generates one __tin_test_N function per TestDecl, plus a
// main() that:
//  1. Initializes top-level var globals (topLevelVarInits).
//  2. Calls _tin_run_test(desc, fn_ptr) for each test.
//  3. Returns the exit code from _tin_test_finish(total_count).
//
// Top-level statements that would form the implicit main are NOT executed;
// only test blocks run.
//
// _tin_run_test and _tin_test_finish are C helpers in runtime.c that use
// setjmp/longjmp to isolate test failures and accumulate pass/fail counts.
func (cg *CodeGen) genTestRunner() error {
	stringType, err := cg.tinTypeToLLVM(&ast.SimpleType{Name: "string"})
	if err != nil {
		return err
	}

	// Declare C runtime helpers.
	// void _tin_run_test(string desc, i8* fn)
	runTestFn := cg.ensureExternDecl("_tin_run_test", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("desc", stringType),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	// i64 _tin_test_finish(i64 total)
	finishFn := cg.ensureExternDecl("_tin_test_finish", irtypes.I64,
		[]*ir.Param{ir.NewParam("total", irtypes.I64)},
		false)

	// Generate one void function per test.
	testFuncs := make([]*ir.Func, len(cg.testDecls))
	for i, td := range cg.testDecls {
		name := fmt.Sprintf("__tin_test_%d", i)
		fn := cg.mod.NewFunc(name, irtypes.Void)
		entry := fn.NewBlock("entry")

		prevFn := cg.curFn
		prevScope := cg.curScope
		prevCurBlock := cg.curBlock
		prevDeferFnI8s := cg.pendingDeferFnI8s
		prevDeferFrames := cg.pendingDeferFrames
		prevDeferEnvs := cg.pendingDeferEnvs
		cg.curFn = fn
		cg.curScope = newScope(cg.curScope)
		cg.curBlock = nil
		cg.pendingDeferFnI8s = nil
		cg.pendingDeferFrames = nil
		cg.pendingDeferEnvs = nil
		cg.labelCount = 0

		prevMutated := cg.mutatedNames
		cg.mutatedNames = collectMutatedNames(td.Body)

		terminated, err := cg.genBody(entry, td.Body, irtypes.Void)
		if err != nil {
			return fmt.Errorf("test %q: %w", td.Desc, err)
		}
		// Ensure the entry block is terminated.
		if !terminated {
			for _, b := range fn.Blocks {
				if b.Term == nil {
					_ = cg.emitDefers(b)
					cg.emitAllScopeReleases(b, "")
					b.NewRet(nil)
				}
			}
		}

		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevCurBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs
		cg.mutatedNames = prevMutated

		testFuncs[i] = fn
	}

	// Generate main().
	mainFn := cg.newCMainWrapper(false)
	entry := mainFn.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = mainFn
	cg.curScope = newScope(cg.curScope)

	// Initialize fiber runtime (workers + I/O thread) so tests can use spawn/await.
	cur := cg.emitFiberMainWrap(entry)

	// Register the deinit dispatcher with libc atexit BEFORE running
	// any test code (matches the pattern in genImplicitMain / the
	// codegen.go main wrapper).
	cur = cg.emitDeinitAllAtexit(cur)

	// Register per-type-id any-release helpers so that any-boxed
	// structs in tests run their deinit on scope exit.
	cur = cg.emitAnyDispatchRegistrations(cur)

	// Initialize top-level var globals so tests can reference them.
	cur, err = cg.emitTopLevelVarInits(cur)
	if err != nil {
		return err
	}

	cg.emitPkgInitFns(cur)

	// Call _tin_run_test for each test.
	if cur != nil {
		for i, td := range cg.testDecls {
			descVal := cg.buildStringFatPtr(cur, td.Desc)
			fnPtr := cur.NewBitCast(testFuncs[i], irtypes.I8Ptr)
			cur.NewCall(runTestFn, descVal, fnPtr)
		}

		// Drain the run queue and shut down workers.
		cg.emitFiberMainEnd(cur)

		// Release RC-tracked locals (e.g. from topLevelVarInits).
		cg.emitAllScopeReleases(cur, "")

		// Deinit top-level globals: registered with atexit at the top
		// of the test runner main; runs automatically on clean exit.
		// Inline emit removed (was duplicating the atexit hook).

		// Call _tin_test_finish(N) -> i64 exit code.
		total := constant.NewInt(irtypes.I64, int64(len(cg.testDecls)))
		rc64 := cur.NewCall(finishFn, total)
		rc32 := cur.NewTrunc(rc64, irtypes.I32)
		cur.NewRet(rc32)
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}
