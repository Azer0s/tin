package codegen

import (
	"fmt"
	"sort"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) Generate(prog *ast.Program) (*ir.Module, error) {
	// Initialize global scope.
	cg.curScope = newScope(nil)
	cg.moduleScope = cg.curScope

	// Register built-in special traits so structs can implement them without
	// an explicit trait declaration in source.
	cg.registerBuiltinTraits()
	cg.registerBuiltinOpTraits()

	// Duplicate-declaration pass: reject same-scope re-declarations before
	// any IR is emitted.  Shadowing (same name in a nested scope) is allowed.
	cg.progress("check declarations")

	if err := checkDuplicateDecls(prog.Stmts); err != nil {
		return nil, fmt.Errorf("%s:%w", cg.filename, err)
	}

	// Reject `pkg::Name` references when the file only imported `pkg`
	// selectively (`use { Name } from pkg`).  Selective imports bring
	// the named symbols into scope but do NOT register `pkg` as a
	// package alias; reaching for the qualified form should be a hard
	// error so users can't accidentally rely on a namespace they did
	// not opt into.
	if err := checkSelectiveImportQualifiers(prog.Stmts); err != nil {
		return nil, fmt.Errorf("%s:%w", cg.filename, err)
	}

	// Stacktrace usage detection: scan for `stacktrace(` calls before any
	// fn is emitted so funcs.go knows whether to keep Tin user fns at
	// internal linkage (default) or promote them to external (so dladdr
	// can resolve them after `-rdynamic` exports them to the dynsym).
	// Empirical check: STB_LOCAL symbols never reach the dynamic symbol
	// table regardless of `-rdynamic`, so promotion is mandatory when
	// stacktrace is reachable. Doing the scan here, after dup-decl checks
	// but before any other pass, keeps every later codegen path observing
	// a single consistent value of cg.stacktraceUsed.
	//
	// Must precede initDebugInfo: when stacktrace is reachable we flip
	// debugMode on so DWARF line tables get emitted into the IR (clang's
	// `-gline-tables-only` flag then preserves only `.debug_line`, which
	// libdwfl reads at runtime to map IPs to "file:line:col"). Without
	// this flip, the runtime resolver would always fall through to the
	// dladdr "<symbol>+<offset>" form for Tin user code.
	cg.detectStacktraceUsage(prog.Stmts)

	// pclntabUsed gates the per-instruction line/col side-map
	// (cg.instLineCol) the pclntab post-pass reads to build per-fn PC
	// tables.  Latched true up-front because we cannot know yet whether
	// any codegen path will hit `ensurePanicFn` (array bounds, cap-check,
	// ADT mismatch...); the side-map is a Go map in cg state and costs
	// nothing in the binary when applyPclntabPostPass later finds
	// stacktraceUsed == false and stays silent.  Unlike debugMode, this
	// does NOT pull in DWARF.
	cg.pclntabUsed = true

	// Initialize DWARF debug metadata only when -g is active. pclntab
	// captures source positions through cg.instLineCol instead, so the
	// runtime resolver works without a DICompileUnit graph in the IR.
	if cg.debugMode {
		cg.initDebugInfo()
	}

	// Zero pass: collect exports and constrained generic function templates
	// before compiling anything.
	cg.progress("collect exports")

	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName != "" {
			for _, name := range exp.Names {
				cg.exports[name] = exp.AsName
			}
		}

		if fd, ok := node.(*ast.FuncDecl); ok && len(fd.Constraints) > 0 {
			cg.constrainedFuncs[fd.Name] = fd
		}

		if fd, ok := node.(*ast.FuncDecl); ok && len(fd.TypeParams) > 0 {
			cg.genericFuncs[fd.Name] = fd
			cg.genericFuncOverloads[fd.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[fd.Name], fd)
		}
	}

	// First pass: register struct / enum / type declarations so forward refs work.
	// Collect concrete struct decls for the cycle check below.
	cg.progress("register types")

	var concreteStructDecls []*ast.StructDecl

	for _, node := range prog.Stmts {
		if err := cg.preregister(node); err != nil {
			return nil, err
		}

		if sd, ok := node.(*ast.StructDecl); ok && len(sd.TypeParams) == 0 {
			concreteStructDecls = append(concreteStructDecls, sd)
		}
	}

	// Validate struct reference cycles: every cycle must have at least one
	// weak edge and at least one strong edge.
	cg.progress("check struct cycles")

	if err := cg.checkStructCycles(concreteStructDecls); err != nil {
		return nil, err
	}

	// Validate complex (block-body) macros: side-effect check.
	// Recursive macros are allowed - the 5-second timeout handles runaway recursion.
	cg.progress("validate macros")

	for _, m := range cg.macros {
		if isMacroComplex(m) {
			if err := checkMacroSideEffects(m); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.5: collect C extern symbol names BEFORE predeclaring Tin user
	// functions. This allows predeclareFuncAs to detect collisions and mangle
	// Tin wrapper names (e.g. `fn printf(...)` -> IR `@_tin__printf`) to avoid
	// redefinition conflicts with C externs declared in the same source file.
	if cg.externIRNames == nil {
		cg.externIRNames = map[string]bool{}
	}

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.IsExtern != "" {
			cg.externIRNames[fd.IsExtern] = true
		}
	}

	// Pre-pass 1.9: load all imported packages BEFORE registering top-level vars
	// and predeclaring function signatures. This ensures struct types (e.g.
	// AtomicI64 from "use sync") are registered before preregisterTopLevelVar
	// tries to resolve types like sync::AtomicI64, and before predeclareFunc
	// tries to resolve parameter types like sync::Channel[i64].
	cg.progress("load packages")

	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok && !ud.IsExtern {
			if err := cg.genUseDecl(ud); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.8: detect overloaded function/method base names so that
	// predeclareFunc and predeclareMethod can mangle their IR names.
	for name, flag := range scanOverloadedNames(prog.Stmts) {
		cg.overloadedNames[name] = flag
	}

	// Pre-pass 1.85: emit struct layouts for the entry program BEFORE
	// predeclareFunc starts walking signatures.  A `fn make() Result[Foo, ...]`
	// signature triggers Result monomorphization via tinTypeToLLVM; if
	// Foo's struct hasn't had its Fields populated yet, the ADT's
	// payload buffer is sized to the smaller variant only and writes
	// of the larger struct payload spill over the buffer.  This
	// hoist mirrors the pkg-import path in loadPackageFromSource
	// (Pass 1.5a runs before its Pass 2 predeclares).
	for _, node := range prog.Stmts {
		if sd, ok := node.(*ast.StructDecl); ok && len(sd.TypeParams) == 0 {
			if err := cg.genStructLayout(sd); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.9: synthesize `fn main() Result[Unit, errors::Err]`
	// when top-level imperative statements use `try` but no explicit
	// `fn main` exists.  Without this, `let v = try foo()` at script
	// scope tries to propagate Err through the implicit main's i32
	// return and fails to typecheck.  Wrapping the script in a Result-
	// returning main lets try desugar against a real Result-shaped
	// return frame; the C-main wrapper unpacks the Result into the
	// process exit code via tryEmitResultMainReturn.
	prog.Stmts = cg.synthesizeImplicitMainForTry(prog.Stmts)

	// Second pass: pre-declare all functions (signatures only) so forward calls work.
	cg.progress("predeclare functions")

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			if err := cg.predeclareFunc(fd); err != nil {
				return nil, err
			}
		}

		if sd, ok := node.(*ast.StructDecl); ok {
			// Skip method predeclaration for generic struct templates - methods
			// will be compiled on demand when the concrete type is instantiated.
			if len(sd.TypeParams) > 0 {
				continue
			}
			// Propagate struct-level scoped tags (#pure@fn, etc.) onto methods
			// BEFORE they are registered in funcDecls. The later #pure /
			// #no_recurse check iterates funcDecls and must see the expanded
			// tag set.
			if err := cg.propagateStructScopedTags(sd); err != nil {
				return nil, err
			}

			aug := cg.augmentStructFromTraits(sd)
			for _, m := range aug.Methods {
				if err := cg.predeclareMethod(aug.Name, m); err != nil {
					return nil, err
				}
			}
		}
	}

	// Validate trait-impl completeness: every struct that declares (T1, T2, ...)
	// must provide qualified impls for each virtual method of each listed trait.
	// Default-bodied methods (e.g. labeled.label) remain optional. Reports all
	// missing impls per struct in one error so users can fix them in one pass.
	if err := cg.checkAllTraitImplsComplete(prog.Stmts); err != nil {
		return nil, err
	}

	// Collect entry-program top-level var bare names (pkg-imported vars are
	// already registered via Pre-pass 1.9). The pure-check below uses this set
	// to reject reads/writes of mutable globals from #pure bodies.
	if cg.topLevelVarBareNames == nil {
		cg.topLevelVarBareNames = map[string]bool{}
	}

	if cg.topLevelConstNames == nil {
		cg.topLevelConstNames = map[string]bool{}
	}

	for _, node := range prog.Stmts {
		if tv, ok := node.(*ast.TopLevelVar); ok {
			cg.topLevelVarBareNames[tv.Name] = true
			if tv.IsConst {
				cg.topLevelConstNames[tv.Name] = true
			}
		}
	}

	// Validate #pure functions: transitive side-effect check.
	// Validate #no_recurse functions: transitive call-graph cycle check.
	// Both run after predeclaration so all function signatures and tags are known.
	if err := cg.checkAllPureFuncs(); err != nil {
		return nil, err
	}

	if err := cg.checkAllNoRecurseFuncs(); err != nil {
		return nil, err
	}

	if err := cg.checkAllInteropFuncs(prog.Stmts); err != nil {
		return nil, err
	}

	cg.checkAllUnused(prog)

	// Run Andersen FIRST so cg.andersenPts is populated before the
	// dataflow pass, which consults the points-to map for the
	// -Wunchecked-returned-nil pedantic check.  Andersen has no
	// dataflow dependency, so the swap is a pure ordering change.
	cg.runAndersen(prog)

	cg.runDataflow(prog)

	cg.runAstChecks(prog)

	// Build call graph and run color propagation for the #async / coro system.
	cg.progress("build call graph")

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.Body != nil {
			cg.buildCallGraphEntry(fd.Name, fd.Body)
		}

		if sd, ok := node.(*ast.StructDecl); ok {
			for _, m := range sd.Methods {
				key := methodScopeName(sd.Name, m)
				cg.buildCallGraphEntry(key, m.Body)
			}
		}
	}

	cg.collectBoxedFns(prog)
	cg.colorCallGraph()
	cg.checkSyncWaitInCoroCallable(prog)
	cg.computeAutoYieldHeuristics(prog)

	// Pre-declare $coro and $colored variants for all colored functions
	// so that mutual references across coro/colored bodies resolve
	// correctly during body emission.
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			// fn main() is renamed to _tin_user_main at IR level; predeclare
			// the $coro stub under that IR name so genFuncDeclAs can find it.
			coroKey := fd.Name
			if fd.Name == "main" && !fd.IsStatic {
				coroKey = "_tin_user_main"
			}

			if cg.coroCallable[coroKey] {
				if err := cg.predeclareCoroVariant(fd, coroKey, false); err != nil {
					return nil, err
				}
			}

			if cg.coloredCallable[coroKey] {
				if err := cg.predeclareColoredVariant(fd, coroKey, false); err != nil {
					return nil, err
				}
			}
		}
	}
	// Predeclare $colored variants for every fn in coloredCallable that
	// wasn't already covered by the prog.Stmts loop above.  In
	// particular: struct methods (registered into cg.funcDecls under
	// their mangled scope name during the struct-decl pass) and
	// package-loaded fns.  Without this pass, a coro caller emitted
	// BEFORE its colored callee would not see `<callee>$colored` in
	// scope and resolveColoredCallee would silently fall back to the
	// plain sync callee -- correct semantics but cooperation is lost
	// on every forward-ref'd colored call site.
	//
	// Iterate in sorted name order: Go map iteration is intentionally
	// non-deterministic, and each iteration calls NewFunc on cg.mod, so
	// the declarations land in whatever order the map happened to walk
	// this run.  That broke TestIRDeterminism (which diffs two runs of
	// `tin ir` and demands byte-identical output).
	colNames := make([]string, 0, len(cg.funcDecls))
	for name := range cg.funcDecls {
		colNames = append(colNames, name)
	}

	sort.Strings(colNames)

	for _, name := range colNames {
		decl := cg.funcDecls[name]
		if !cg.coloredCallable[name] {
			continue
		}

		if decl == nil || decl.IsExtern != "" || decl.Body == nil {
			continue
		}

		if hasTag(decl.Tags, "no_autoyield") {
			continue
		}

		if err := cg.predeclareColoredVariant(decl, name, false); err != nil {
			return nil, err
		}
	}

	// Pre-pass 2.5: scan extern declarations for *StructName pointer types.
	// Structs used as *S in extern signatures must use C-compatible layout
	// (no type_id prefix) so raw pointers can round-trip to/from C.
	cg.scanExternPtrStructs(prog.Stmts)

	// Pre-pass 2.8: register extern functions that use only built-in types.
	// Struct method bodies may call module-level externs before those externs
	// are processed by the Third pass (e.g. AtomicI64.make -> _tin_atomic_new_i64).
	// predeclareFuncAs skips externs, so without this pass they are undefined
	// when Pre-pass 3 compiles method bodies.
	// Only externs with all-primitive types are processed here; externs that
	// reference struct/enum types are skipped (struct types aren't registered
	// until Pre-pass 3, so processing them now would panic).
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.IsExtern != "" && externHasPrimitiveTypes(fd) {
			if err := cg.genFuncDecl(fd); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 3: generate struct/enum/type/union declarations before anything
	// else so that structFieldLLVMTypes is fully populated.  This is needed
	// because use-extern declarations reference struct types for C ABI conversion
	// and may appear before the struct definition in source order.
	cg.progress("generate type declarations")

	// Phase A: struct field layouts (no methods yet); plus enum/type/union
	// declarations whose field types may reference structs.
	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.StructDecl:
			if err := cg.genStructLayout(n); err != nil {
				return nil, err
			}
		case *ast.EnumDecl:
			if err := cg.genEnumDecl(n); err != nil {
				return nil, err
			}
		case *ast.TypeDecl:
			if err := cg.genTypeDecl(n); err != nil {
				return nil, err
			}
		case *ast.UnionDecl:
			if err := cg.genUnionDecl(n); err != nil {
				return nil, err
			}
		}
	}

	// Phase B: ADT layouts now that every struct field type is known, so
	// generic ADTs like Result[LocalStruct, Err] get the correct payload
	// size rather than a placeholder [1 x i8].
	for _, node := range prog.Stmts {
		if n, ok := node.(*ast.DataDecl); ok {
			if err := cg.genDataDecl(n); err != nil {
				return nil, err
			}
		}
	}

	// Pass 2.5: register top-level var declarations as LLVM globals. Runs
	// AFTER pass 2 (function predeclaration) so initializer fold can call
	// pure functions via funcDecls (e.g. `var x i64 = pure_fn(7) + 1`), and
	// BEFORE struct method bodies are generated so methods can reference
	// module-scoped vars by bare name.
	cg.progress("register globals")

	for _, node := range prog.Stmts {
		if tv, ok := node.(*ast.TopLevelVar); ok {
			if err := cg.preregisterTopLevelVar(tv); err != nil {
				return nil, err
			}
			// Record the declaration position so `sourcepos(my_top_var)`
			// can resolve back to the originating let/var/const line.
			cg.topLevelVarPos[tv.Name] = tv.Pos()
		}
	}

	// Phase C: struct method bodies, trait chain shims, and vtables.
	for _, node := range prog.Stmts {
		if n, ok := node.(*ast.StructDecl); ok {
			if err := cg.genStructMethods(n); err != nil {
				return nil, err
			}
		}
	}

	// Third pass: generate full function bodies and other declarations.
	cg.progress("generate code")

	var topStmts []ast.Node

	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if err := cg.genFuncDecl(n); err != nil {
				return nil, err
			}
		case *ast.StructDecl:
			// Already processed in pre-pass 3.
		case *ast.EnumDecl:
			// Already processed in pre-pass 3.
		case *ast.TypeDecl:
			// Already processed in pre-pass 3.
		case *ast.UseDecl:
			if err := cg.genUseDecl(n); err != nil {
				return nil, err
			}
		case *ast.ExportDecl:
			// Already handled in zero pass; ExportDecl itself emits no IR.
		case *ast.TraitDecl:
			// Registered in preregister; no IR to emit.
		case *ast.MacroDecl:
			// Registered in preregister; no IR to emit.
		case *ast.UnionDecl:
			// Already processed in pre-pass 3.
		case *ast.DataDecl:
			// Already processed in pre-pass 3.
		case *ast.TestDecl:
			if cg.testMode {
				cg.testDecls = append(cg.testDecls, n)
			}
			// In normal mode, test blocks are silently ignored.
		case *ast.TopLevelVar:
			// Already registered as an LLVM global in pre-pass 1.7.
			// If the initializer is a runtime expression, it was appended to
			// cg.topLevelVarInits and will be emitted at the top of main().
		default:
			topStmts = append(topStmts, node)
		}
	}

	// Emit C-callable wrappers for #interop functions. Done after the
	// third pass so all internal entry points exist as IR functions
	// the wrapper can reference.
	if err := cg.emitInteropWrappers(prog.Stmts); err != nil {
		return nil, err
	}

	// Emit a parallel #interop-style shim for every wrappable #pure function
	// so the per-fn .so cache (Phase C2) has a single uniform dispatch
	// surface for cgo. The shim shares emitInteropWrapperFor's marshal
	// logic - string/slice/bool widening all go through the same helpers
	// the user-tagged #interop pipeline uses. Shim symbol is
	// `__tin_pure_shim_<fn_name>` so it never collides with the function
	// itself; in the main binary the shim has internal linkage and clang
	// DCEs it; the cache slicer promotes it to external for dlsym.
	if err := cg.emitPureFnCtfeShims(); err != nil {
		return nil, err
	}

	if cg.emitHeaderPath != "" {
		if err := cg.writeInteropHeader(prog.Stmts); err != nil {
			return nil, err
		}
	}

	// In test mode, generate test functions and a test-runner main.
	// Top-level statements that would form the implicit main are intentionally
	// not executed - only test blocks run.
	if cg.testMode && len(cg.testDecls) > 0 {
		if err := cg.genTestRunner(); err != nil {
			return nil, err
		}

		cg.emitAtomTable()
		cg.applyStacktracePostPass()
		cg.finalizeImplSection()
		// extractMonoModules MUST run before applyPclntabPostPass:
		// pclntab emits blockaddress(@fn, %bb) constants and lld
		// rejects them when @fn is only a declare. Moving mono fns
		// first lets pclntab route the pcs entry into the same mono
		// module where the fn definition now lives.
		cg.extractMonoModules()
		cg.applyPclntabPostPass()
		cg.emitLlvmUsedRoots()
		cg.finalizePerPkgModules()

		return cg.mod, nil
	}

	// In REPL mode the cell function is the only entry point; skip main().
	// Skip mono extraction: REPL compiles cg.mod alone (no separate mono
	// .o cache), so monomorphized fn bodies must stay in cg.mod or
	// dlopen will see only `declare`s for them.  For the same reason we
	// also fold per-pkg modules back into cg.mod here -- the non-REPL
	// build compiles each pkg to its own .o and links them together,
	// but the REPL has no separate compile/link pass; everything has
	// to live in the single cg.mod that becomes the cell .so.
	if cg.replMode {
		cg.emitAtomTable()
		cg.applyStacktracePostPass()
		cg.finalizeImplSection()
		cg.applyPclntabPostPass()
		// Merge BEFORE emitLlvmUsedRoots: emitting first would create a
		// per-pkg @llvm.used global that the merge then drops by name
		// dedup, silently losing every impl-section pin entry.  Merge
		// rekeys cg.llvmUsedRoots/Funcs from pkgMod -> cg.mod so the
		// subsequent emit produces a single combined @llvm.used.
		cg.mergePkgModsIntoMain()
		cg.emitLlvmUsedRoots()
		cg.finalizePerPkgModules()

		return cg.mod, nil
	}

	// If there are top-level statements, wrap them in main().
	if len(topStmts) > 0 {
		// Top-level imperative statements form an implicit main. If the
		// user also wrote `fn main()`, both would emit an `i32 @main`
		// wrapper -- the implicit one wins and the user main is never
		// called. Error out so the user picks one model.
		// (Top-level `const` and `var` are TU-level decls and don't
		// reach topStmts, so this only fires for actual statements.)
		if cg.userMainDecl != nil {
			return nil, cg.nodeErr(topStmts[0],
				"top-level statements cannot coexist with an explicit fn main(); "+
					"move the statement inside main, or remove fn main() to use the implicit-main form")
		}

		// Check if main is already defined.
		hasmain := false

		for _, f := range cg.allFuncs() {
			if f.Name() == "_tin_c_main" {
				hasmain = true

				break
			}
		}

		if !hasmain {
			if err := cg.genImplicitMain(topStmts); err != nil {
				return nil, err
			}
		}
	}

	// If the user declared a void `fn main()`, it was compiled as
	// `_tin_user_main`.  Generate a proper `i32 @main()` wrapper that
	// calls it and returns 0 so the process exits cleanly.
	var userMainFn *ir.Func

	for _, f := range cg.allFuncs() {
		if f.Name() == "_tin_user_main" {
			userMainFn = f

			break
		}
	}

	if userMainFn != nil {
		// Only add the wrapper if there is no `i32 @main` already.
		hasMain := false

		for _, f := range cg.allFuncs() {
			if f.Name() == "_tin_c_main" {
				hasMain = true

				break
			}
		}

		if !hasMain {
			// Check whether the user wrote fn{#async} main() - if so we have a
			// $coro ramp and main should run as the first fiber.
			var userMainCoroFn *ir.Func

			for _, f := range cg.allFuncs() {
				if f.Name() == "_tin_user_main$coro" {
					userMainCoroFn = f

					break
				}
			}

			// If the user's main takes a [string] parameter, expose argc/argv.
			wantsArgs := mainTakesStringArgs(cg.userMainDecl)

			wf := cg.newCMainWrapper(wantsArgs)

			wb := wf.NewBlock("entry")

			// Save context so emitTopLevelVarInits can generate expressions.
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wf
			cg.curScope = newScope(cg.curScope)

			// Attach a DISubprogram so `br set -n main` in lldb/gdb lands on
			// the wrapper and shows source. Use line 1 of the primary source
			// file as the scope line; the real user main (compiled as
			// _tin_user_main) carries its own DISubprogram with the exact line.
			prevDbgScope := cg.diCurrentScope
			cg.emitDbgSubprogramForSynthetic(wf, "main", 1)

			defer func() { cg.diCurrentScope = prevDbgScope }()

			// Emit fiber init + io init when the program uses fiber features.
			wb = cg.emitFiberMainWrap(wb)

			// Register the deinit dispatcher with libc atexit BEFORE
			// running user code. atexit guarantees the deinits fire on
			// every clean exit path (return-from-main, libc exit(N),
			// any fn call to std::os::exit) - not only the
			// fall-through-from-main path the inline emit covers.
			wb = cg.emitDeinitAllAtexit(wb)

			// Register per-type-id any-release helpers so that any-boxed
			// structs run their deinit on scope exit instead of just
			// freeing the heap block.
			wb = cg.emitAnyDispatchRegistrations(wb)

			// Emit runtime initializers for top-level var declarations before
			// any fiber runs so that globals are valid from the start.
			var err error

			wb, err = cg.emitTopLevelVarInits(wb)
			if err != nil {
				return nil, err
			}

			cg.emitPkgInitFns(wb)

			// Build the [string] args value from argc/argv if needed.
			var argsSliceVal value.Value

			if wantsArgs {
				strArrType := fatArrayPtrType(stringFatPtrType())
				argvFn := cg.ensureExternDecl("_tin_argv_to_slice", strArrType, []*ir.Param{
					ir.NewParam("argc", irtypes.I32),
					ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)),
				}, false)
				argsSliceVal = wb.NewCall(argvFn, wf.Params[0], wf.Params[1])
			}

			if userMainCoroFn != nil {
				// fn{#async} main(): spawn as the first fiber and block the OS
				// main thread until it completes, then drain remaining fibers.
				// _tin_fiber_run() sends a shutdown signal immediately - if we
				// called it before main's fiber finished, workers would exit too
				// early.  _tin_fiber_sync_await blocks without touching the run
				// queue, so workers continue normally until main is done.
				cg.ensureFiberRuntime()
				syncAwaitFn := cg.ensureExternDecl("_tin_fiber_sync_await", irtypes.Void,
					[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)

				var coroArgs []value.Value

				for i, p := range userMainCoroFn.Params {
					if wantsArgs && i == 0 {
						coroArgs = append(coroArgs, argsSliceVal)
					} else {
						coroArgs = append(coroArgs, constant.NewZeroInitializer(p.Type()))
					}
				}

				coroHdl := wb.NewCall(userMainCoroFn, coroArgs...)
				mainPid := wb.NewCall(cg.fiberSpawnJoinableFn, coroHdl)
				wb.NewCall(syncAwaitFn, mainPid)
				cg.emitFiberMainEnd(wb)
				// Deinits run via atexit(_tin_deinit_all); no inline
				// emit needed here.
				wb.NewRet(constant.NewInt(irtypes.I32, 0))
			} else {
				// fn main(): call synchronously (existing behavior).
				var callArgs []value.Value

				for i, p := range userMainFn.Params {
					if wantsArgs && i == 0 {
						callArgs = append(callArgs, argsSliceVal)
					} else {
						callArgs = append(callArgs, constant.NewZeroInitializer(p.Type()))
					}
				}

				retIsVoid := userMainFn.Sig.RetType.Equal(irtypes.Void)
				if retIsVoid {
					wb.NewCall(userMainFn, callArgs...)
					// Deinits run via atexit(_tin_deinit_all).
					cg.emitFiberMainEnd(wb)
					wb.NewRet(constant.NewInt(irtypes.I32, 0))
				} else {
					ret := wb.NewCall(userMainFn, callArgs...)
					// Deinits run via atexit(_tin_deinit_all).
					cg.emitFiberMainEnd(wb)
					// fn main() Result[Unit, errors::Err] / Result[i64, errors::Err]:
					// unpack the Result.  Ok -> exit 0 (or the inner i64,
					// truncated to i32); Err -> print the message and exit 1.
					if cg.tryEmitResultMainReturn(wb, ret) {
						// terminator already emitted by the helper
					} else {
						// Coerce return value to i32 if needed.
						var retVal value.Value = ret
						if !ret.Type().Equal(irtypes.I32) {
							if ret.Type().Equal(irtypes.I64) {
								retVal = wb.NewTrunc(ret, irtypes.I32)
							} else {
								retVal = constant.NewInt(irtypes.I32, 0)
							}
						}

						wb.NewRet(retVal)
					}
				}
			}

			cg.ensureAllCallsHaveDbg(wf)

			cg.curFn = prevFn
			cg.curScope = prevScope
		}
	}

	// Emit the compile-time atom table and fill in atom helper function bodies.
	cg.emitAtomTable()

	// If no main function was generated (e.g. export-only module), emit a
	// trivial no-op main so the binary links successfully.
	hasMain := false

	for _, f := range cg.allFuncs() {
		if f.Name() == "_tin_c_main" {
			hasMain = true

			break
		}
	}

	if !hasMain && !programHasInteropFunc(prog.Stmts) {
		// No user main and no #interop functions: emit an empty main so
		// the linker has an entry point. When #interop functions exist
		// the program is being built as a library; skip the synthetic
		// main so the C consumer can provide its own.
		wf := cg.newCMainWrapper(false)
		wb := wf.NewBlock("entry")
		wb.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.debugDumpUnterminated()
	cg.applyStacktracePostPass()
	cg.finalizeImplSection()
	cg.applyPclntabPostPass()
	cg.emitLlvmUsedRoots()
	cg.finalizePerPkgModules()

	// -Wunwrapped-c-resource: walks every struct (incl. stdlib) for raw
	// C resource fields whose value crosses an extern boundary. Runs at
	// the very end so all struct decls are registered (genStructDecl
	// populates structDeclsByName lazily) and the call graph is built
	// (computeFnsTouchingExtern needs it for transitive propagation).
	cg.checkAllUnwrappedCResources(prog)

	// -Wunclosed-closeable: warns when a let-bound value whose type
	// implements io::Closeable leaves scope without a .close() call (or
	// transfer). Runs after structDeclsByName is populated so the trait
	// list per struct is complete.
	cg.checkAllUnclosedCloseables(prog)

	// Static analysis: warn on writes that reach a top-level `const`
	// through a pointer alias. Top-level consts live in read-only
	// storage so the write is undefined behavior.
	cg.checkAllWritesToTopLevelConst(prog)

	return cg.mod, nil
}

// synthesizeImplicitMainForTry walks stmts and, if any top-level
// imperative statement uses `try`, replaces those statements with a
// synthesized `fn main() Result[Unit, errors::Err]` whose body is the
// original statements plus a closing `return Ok(Unit{_unit: 0})`.
// The synthesis only fires when there is no explicit `fn main`
// already in stmts; with an explicit main, top-level try is a user
// error that the existing diagnostic handles.
