package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/metadata"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func newModuleWithTriple() *ir.Module {
	mod := ir.NewModule()
	mod.TargetTriple = detectTargetTriple()
	// Pin the source-filename to a stable string so opt + ld.lld
	// produce deterministic bitcode metadata regardless of the
	// random temp-file path tin writes the IR to. Without this, the
	// random-suffixed temp name leaks into the binary's symbol table
	// and breaks reproducible-build tests.
	mod.SourceFilename = "tin"

	return mod
}

// SetTargetTriple lets the caller (typically main, after consulting
// the disk-cached host-info record) hand us a precomputed triple so
// codegen doesn't itself spawn clang. The setter is one-shot per
// process; subsequent calls are no-ops, matching the sync.Once
// semantics of the original auto-probe path.
func SetTargetTriple(t string) {
	targetTripleOnce.Do(func() { targetTripleCache = t })
}

// detectTargetTriple returns the LLVM target triple this build should
// emit. Prefers the value supplied via SetTargetTriple (the disk-
// cached host-info path). Falls back to TIN_TARGET_TRIPLE, then to a
// live `clang -x c -` probe, then to a hardcoded GOOS/GOARCH map.
func detectTargetTriple() string {
	targetTripleOnce.Do(func() {
		// TIN_TARGET_TRIPLE env var overrides the triple (for cross-target tests).
		if override := os.Getenv("TIN_TARGET_TRIPLE"); override != "" {
			targetTripleCache = override

			return
		}
		// Compile an empty C TU to LLVM IR and extract the triple that
		// clang actually emits. This is the only way to get the
		// normalized macosx-style triple (rather than the darwin-style
		// one from -dumpmachine).
		if out, err := exec.Command("clang", "-x", "c", "-", "-S", "-emit-llvm", "-o", "-").
			Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, `target triple = "`) {
					triple := strings.TrimPrefix(line, `target triple = "`)

					triple = strings.TrimSuffix(triple, `"`)
					if triple != "" {
						targetTripleCache = triple

						return
					}
				}
			}
		}
		// Fallback by GOOS/GOARCH when clang is unavailable.
		switch runtime.GOOS + "/" + runtime.GOARCH {
		case "linux/amd64":
			targetTripleCache = "x86_64-pc-linux-gnu"
		case "linux/arm64":
			targetTripleCache = "aarch64-unknown-linux-gnu"
		case "darwin/amd64":
			targetTripleCache = "x86_64-apple-macosx11.0.0"
		case "darwin/arm64":
			targetTripleCache = "arm64-apple-macosx11.0.0"
		default:
			targetTripleCache = "x86_64-pc-linux-gnu"
		}
	})

	return targetTripleCache
}

var (
	targetTripleCache string
	targetTripleOnce  sync.Once
)

// New creates a new CodeGen instance.
func New(filename string) *CodeGen {
	cg := &CodeGen{
		filename:                 filename,
		mod:                      newModuleWithTriple(),
		structFields:             make(map[string][]string),
		structFieldTags:          make(map[string]map[string]string),
		structFieldTinTypes:      make(map[string][]ast.TypeExpr),
		genericStructsByArity:    make(map[string]map[int]*ast.StructDecl),
		genericStructTmplFiles:   make(map[string]string),
		traitMethodOrder:         make(map[string][]string),
		traitVtableGlobals:       make(map[string]*ir.Global),
		traitBorrowVtableGlobals: make(map[string]*ir.Global),
		traitDataReleaseThunks:   make(map[string]*ir.Func),
		wildcardMonos:            make(map[string]*ir.Func),
		traitInstKeys:            make(map[string]string),
		traitAsyncMethodNames:    make(map[string][]string),
		traitBareToQualInstKey:   make(map[string]string),
		implicitConvFns:          make(map[string][]implicitConvEntry),
		coerceConvFns:            make(map[string][]coerceConvEntry),
		structVtableOrder:        make(map[string][]string),
		enumValues:               make(map[string]int64),
		genericTypeAliases:       make(map[string]*ast.TypeDecl),
		opTraitImpls:             make(map[string][]opTraitImplEntry),
		exports:                  make(map[string]string),
		importedPkgs:             make(map[string]bool),
		loadedSrcPaths:           make(map[string]bool),
		constrainedFuncs:         make(map[string]*ast.FuncDecl),
		genericFuncs:             make(map[string]*ast.FuncDecl),
		genericFuncOverloads:     make(map[string][]*ast.FuncDecl),
		genericFuncHomeScopes:    make(map[string]*scope),
		constrainedFuncInstances: make(map[string]*ir.Func),
		genericMethodTemplates:   make(map[string]*ast.FuncDecl),
		macros:                   make(map[string]*ast.MacroDecl),
		funcDecls:                make(map[string]*ast.FuncDecl),
		ctfeCache:                make(map[string]ctfeMemoEntry),
		topLevelVarPos:           make(map[string]ast.Pos),
		externTLSVars:            make(map[string]*ir.Global),
		structTypeIDs:            make(map[string]int32),
		fnTypeIDs:                make(map[string]int32),
		nextTypeID:               6, // 0-5 reserved for anyTag* primitives (fn=5)
		structImpls:              make(map[string][]string),
		deadStrippedMethods:      make(map[string]map[string][]string),
		structFieldLLVMTypes:     make(map[string][]irtypes.Type),
		traitChainedInits:        make(map[string][]*ir.Func),
		traitChainedDeinits:      make(map[string][]*ir.Func),
		atomCodes:                make(map[string]int32),
		atomCodeToName:           make(map[int32]string),
		unionTypeMembers:         make(map[string][]ast.TypeExpr),
		nativeUnionDecls:         make(map[string]*ast.UnionDecl),
		unionTypeIDs:             make(map[string]int32),
		dataTypeIDs:              make(map[string]int32),
		dataVariants:             make(map[string]map[string]*dataVariantInfo),
		dataVariantLookup:        make(map[string][]string),
		dataValueReleaseFns:      make(map[string]*ir.Func),
		dataValueRetainFns:       make(map[string]*ir.Func),
		coroCallable:             make(map[string]bool),
		coloredCallable:          make(map[string]bool),
		boxedFns:                 make(map[string]bool),
		fnParkingClass:           make(map[string]bool),
		knownParkingExterns: map[string]bool{
			// Channel ops: park on empty / full + closed.
			"tin_channel_recv_blocking":  true,
			"tin_channel_send_blocking":  true,
			"tin_channel_recv_park":      true,
			"_tin_channel_recv_blocking": true,
			"_tin_channel_send_blocking": true,
			"_tin_channel_recv_park":     true,
			// Timer / sleep: park until elapsed.
			"tin_sleep_ms_c": true,
			"_tin_sleep_ms":  true,
			// Async I/O: park on EAGAIN.
			"tin_async_read_c":  true,
			"tin_async_write_c": true,
			"_tin_async_read":   true,
			"_tin_async_write":  true,
			// Mutex / cond / fiber join: explicit park.
			"_tin_fmutex2_lock":      true,
			"_tin_fcond2_add_waiter": true,
			"_tin_fiber_sync_await":  true,
			"_tin_fiber_join":        true,
		},
		callGraph:                     make(map[string][]string),
		funcHeuristics:                make(map[string]*FuncHeuristicInfo),
		overloadedNames:               make(map[string]bool),
		overloads:                     make(map[string][]*overloadEntry),
		genericMethodsSetUp:           make(map[string]bool),
		funcReturnUnsigned:            make(map[string]bool),
		heapPromotingFns:              make(map[string]bool),
		cLayoutWrapperNativeReturnFns: make(map[string]string),
		fnReturnsHeapPromotedFields:   make(map[string][]int),
		structWeakFields:              make(map[string]map[string]bool),
		structOwningRawPtrFields:      make(map[string]map[string]bool),
		fnReturnsOwningIface:          make(map[string]bool),
		structConstFields:             make(map[string]map[string]bool),
		cLayoutStructs:                make(map[string]bool),
		nativeStructTypes:             make(map[string]*irtypes.StructType),
		packedStructs:                 make(map[string]bool),
		noCopyStructs:                 make(map[string]bool),
		closedStructs:                 make(map[string]bool),
		structDeclsByName:             make(map[string]*ast.StructDecl),
		structDeclFiles:               make(map[string]string),
		elemReleaseHelpers:            make(map[string]*ir.Func),
		elemRetainHelpers:             make(map[string]*ir.Func),
		heapBlockReleaseFns:           make(map[string]*ir.Func),
		structPtrReleaseFns:           make(map[string]*ir.Func),
		chainReleaseFns:               make(map[string]*ir.Func),
		diFiles:                       make(map[string]*metadata.DIFile),
		diTypeCache:                   make(map[string]metadata.Field),
	}
	atomType := irtypes.NewStruct(irtypes.I32)
	atomType.SetName("__atom")
	cg.atomType = atomType
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, atomType)
	cg.initBuiltinTupleTemplates()

	// Default libs root: <execDir>/libs next to the tin binary.
	if ex, err := os.Executable(); err == nil {
		cg.libsRoots = []string{filepath.Join(filepath.Dir(ex), "libs")}
	}

	// Seed builtin type aliases that exist outside of any source package:
	// rune is a Unicode codepoint represented as i32; `for r rune in s`
	// triggers UTF-8 decoding in the for-in loop.  Setting it via
	// recordAliasType lands it in the registry so cg.aliasTypeFor finds
	// it from any call site.
	cg.recordAliasType(CanonKey("rune"), &ast.SimpleType{Name: "i32"})

	return cg
}

// SetStdlibOverride overrides the default stdlib/ search path.
// Used by the --stdlib CLI flag.
func (cg *CodeGen) SetStdlibOverride(path string) { cg.stdlibOverride = path }

// AddLibsRoot prepends an additional libs root directory to the search path.
// Used by the --lib-root CLI flag.
func (cg *CodeGen) AddLibsRoot(path string) {
	cg.libsRoots = append([]string{path}, cg.libsRoots...)
}

// stdlibBase returns the effective stdlib directory.
func (cg *CodeGen) stdlibBase() string {
	if cg.stdlibOverride != "" {
		return cg.stdlibOverride
	}

	if ex, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(ex), "stdlib")
	}

	return "stdlib"
}

// runtimeBuiltinBase returns the path to runtime/builtin/, the
// directory holding language-defined traits (tryable, awaitable,
// etc.) that are auto-loaded into every Tin program. Sibling of
// stdlib so a custom-stdlib build still gets the built-in traits.
func (cg *CodeGen) runtimeBuiltinBase() string {
	if ex, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(ex), "runtime", "builtin")
	}

	return filepath.Join("runtime", "builtin")
}

// initBuiltinTupleTemplates pre-populates the Tuple generic struct templates
// for arities 2-10. Fields are named alphabetically: a, b, c, ...
func (cg *CodeGen) initBuiltinTupleTemplates() {
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	cg.genericStructsByArity["Tuple"] = make(map[int]*ast.StructDecl)
	for arity := 2; arity <= 10; arity++ {
		typeParams := make([]string, arity)
		copy(typeParams, letters[:arity])

		fields := make([]ast.StructField, arity)
		for i, name := range typeParams {
			fields[i] = ast.StructField{Name: name, Type: &ast.SimpleType{Name: name}}
		}

		cg.genericStructsByArity["Tuple"][arity] = &ast.StructDecl{
			Name:       "Tuple",
			TypeParams: typeParams,
			Fields:     fields,
		}
	}
}

// registerBuiltinTraits pre-populates cg.traits with synthetic declarations for
// built-in special traits (iter[t]) so structs can implement them without an
// explicit "trait iter[t] = ..." declaration in the source file.
func (cg *CodeGen) registerBuiltinTraits() {
	if cg.traitFor(CanonKey("iter")) != nil {
		return // already declared by user
	}
	// iter[t]: fn len(this iter[t]) i64 = virtual
	//          fn get(this iter[t], i i64) t = virtual
	selfType := &ast.GenericType{Name: "iter", TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: "t"}}}
	lenMethod := &ast.FuncDecl{
		Name:      "len",
		IsVirtual: true,
		Params:    []ast.Param{{Name: "this", Type: selfType}},
		RetType:   &ast.SimpleType{Name: "i64"},
	}
	getMethod := &ast.FuncDecl{
		Name:      "get",
		IsVirtual: true,
		Params: []ast.Param{
			{Name: "this", Type: selfType},
			{Name: "i", Type: &ast.SimpleType{Name: "i64"}},
		},
		RetType: &ast.SimpleType{Name: "t"},
	}
	iterTrait := &ast.TraitDecl{
		Name:       "iter",
		TypeParams: []string{"t"},
		Methods:    []*ast.FuncDecl{lenMethod, getMethod},
	}
	cg.recordTrait(CanonKey("iter"), iterTrait)
}

// registerBuiltinOpTraits pre-populates cg.traits with synthetic alias-form
// declarations for the built-in operator traits. Each is a single-method
// `as fn` trait whose method name equals the trait name.
//
// User code implements an operator trait with `fn ::<trait>(this T, ...) ret`.
// genBinExpr / genUnaryExpr then dispatch operator expressions through the
// usual trait-impl lookup (traitImplKey).
func (cg *CodeGen) registerBuiltinOpTraits() {
	mk := func(name string, typeParams []string, params []ast.TypeExpr, ret ast.TypeExpr) {
		if cg.traitFor(CanonKey(name)) != nil {
			return // user (or earlier pass) already declared
		}

		td := &ast.TraitDecl{
			Name:       name,
			TypeParams: typeParams,
			IsAlias:    true,
			AliasType: &ast.FuncType{
				Params:  params,
				RetType: ret,
			},
		}
		cg.recordTrait(CanonKey(name), td)
	}

	t := func(n string) ast.TypeExpr { return &ast.SimpleType{Name: n} }

	// Binary arithmetic: trait <op>[rhs, ret] as fn(rhs) ret
	for _, n := range []string{"add", "sub", "mul", "div", "mod", "concat"} {
		mk(n, []string{"rhs", "ret"}, []ast.TypeExpr{t("rhs")}, t("ret"))
	}

	// Unary: trait <op>[ret] as fn() ret
	for _, n := range []string{"neg", "pos", "not"} {
		mk(n, []string{"ret"}, nil, t("ret"))
	}

	// Comparison: comp[rhs] as fn(rhs) bool
	mk("comp", []string{"rhs"}, []ast.TypeExpr{t("rhs")}, t("bool"))

	// Ordering: ord[rhs] as fn(rhs) i64  (negative/zero/positive)
	mk("ord", []string{"rhs"}, []ast.TypeExpr{t("rhs")}, t("i64"))

	// Index: index[key, ret] as fn(key) ret
	mk("index", []string{"key", "ret"}, []ast.TypeExpr{t("key")}, t("ret"))

	// Index-assign: index_set[key, val] as fn(key, val)
	mk("index_set", []string{"key", "val"}, []ast.TypeExpr{t("key"), t("val")}, nil)
}

// LinkLibs returns the list of libraries that source-level directives
// requested to link against (e.g. from `use extern` lib entries).
// The caller should pass these as -l<lib> flags to the linker.
func (cg *CodeGen) LinkLibs() []string { return cg.linkLibs }

// PackageSrcPaths returns the paths of all .tin source files that were loaded
// as packages during codegen. The caller can scan these for //! directives
// (e.g. //!+file.c) to collect C sources that need to be compiled alongside.
func (cg *CodeGen) PackageSrcPaths() []string { return cg.pkgSrcPaths }

// ReplGlobal records a top-level variable promoted from a `let` binding in
// REPL mode. The session uses this to declare the variable as a `var` in
// subsequent cells so the type checker can resolve cross-cell references.
