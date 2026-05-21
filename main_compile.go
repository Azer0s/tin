package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azer0s/tin/codegen"
)

func clangMajorVersion() int {
	return hostInfo().ClangMajorVersion
}

// minClangMajor is the lowest clang major version tin's IR pipeline
// has been validated against. LLVM 17 is the floor: it's the first
// release where the new opaque-pointer mode (which our IR emits) is
// the default and the coroutine intrinsics match what we generate.
// Bumping this requires verifying examples/ + stdlib/ pass under the
// new minimum.
const minClangMajor = 17

// ensureBackendReady runs the toolchain probes that gate any compile.
// LookPath-checks clang + opt, version-gates clang, and primes codegen
// with the target triple. Cheap on warm runs (marker files + on-disk
// JSON). The ld.lld check for cross-compile is deferred to the link
// path where useLld actually fires; calling early would force every
// invocation to require lld even for native builds.
func ensureBackendReady() {
	requireTools()

	if t := hostInfo().TargetTriple; t != "" {
		codegen.SetTargetTriple(t)
	}

	if v := clangMajorVersion(); v > 0 && v < minClangMajor {
		_, _ = fmt.Fprintf(os.Stderr,
			"error: clang version %d is too old; tin requires clang >= %d\n",
			v, minClangMajor)

		os.Exit(1)
	}
}

// fixCoroAttrs rewrites the LLVM IR string emitted by the llir library to
// produce valid IR for the installed clang version.
// "presplitcoroutine" must be a keyword attribute, not a string attribute.
// llvm.coro.end changed signature at LLVM 22: <= 21 uses i1 return + ptr arg,
// >= 22 uses void return + ptr arg. llir emits the old void + i8* form; LLVM 22
// accepts that and auto-upgrades i8* to ptr. LLVM 21 expects i1, so we patch.
func fixCoroAttrs(ir string) string {
	ir = strings.ReplaceAll(ir, `"presplitcoroutine"`, "presplitcoroutine")
	if v := clangMajorVersion(); v > 0 && v <= 21 {
		ir = strings.ReplaceAll(ir,
			"declare void @llvm.coro.end(i8*",
			"declare i1 @llvm.coro.end(ptr")
		ir = strings.ReplaceAll(ir,
			"call void @llvm.coro.end(i8*",
			"%_coro_end = call i1 @llvm.coro.end(ptr")
	}

	return ir
}

// stacktraceLinkFlag is the link-time toggle that promotes user fns to
// the dynsym (Linux ELF only) and pulls in libunwind.  See
// docs/plans/stacktrace-libunwind.md: link-time emission is gated on
// cg.StacktraceUsed(); the global below is set in main() before each
// compileIR call.
//
// We can't thread a bool param through compileIR without rewriting its
// signature in five places, so a process-global suffices: the compiler
// is invoked once per build and the flag is decided after Generate()
// returns.
var stacktraceLinkActive bool

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// namedIR pairs a stable label (used in temp-file names + progress
// reporting) with the LLVM IR text for one compilation unit. Used by
// compileIR's per-pkg path: the entry IR (cg.mod) is one TU, and each
// imported package's *ir.Module is an additional TU compiled in
// parallel into its own `.o` and linked alongside.
type namedIR struct {
	label  string
	irText string
	// iface is the per-package interface manifest computed at codegen
	// time; nil for mono modules (anonymous instantiations have no
	// addressable iface) and empty packages. Written next to pkg.o in
	// the per-pkg cache (.iface.json + .iface_hash) so downstream
	// tooling can inspect what shape this pkg presents.
	iface *codegen.PkgIface
}

// collectPkgIRs serializes every per-pkg LLVM module the codegen built
// (excluding cg.mod itself, which is serialized separately) and returns
// them as namedIR entries for compileIRWithPkgs. Sanitizes pkg names
// for use in temp filenames (`::` -> `_`, etc.). Also appends the
// content-addressed mono modules (step 5 of incremental compilation):
// each carries one or more monomorphized fn bodies and gets compiled
// to its own .build/mono/<hash>/bin.o so distinct consumers of the
// same `parse[Point]` instantiation share one cached object.
func collectPkgIRs(cg *codegen.CodeGen) []namedIR {
	mods := cg.PkgModules()
	names := cg.PkgModuleNames()
	monoMods := cg.MonoModules()
	monoHashes := cg.MonoModuleHashes()

	if len(mods) == 0 && len(monoMods) == 0 {
		return nil
	}

	out := make([]namedIR, 0, len(mods)+len(monoMods))

	for i, m := range mods {
		label := strings.NewReplacer("::", "_", "/", "_", " ", "_").Replace(names[i])
		out = append(out, namedIR{
			label:  label,
			irText: fixCoroAttrs(m.String()),
			iface:  cg.BuildPkgIface(names[i]),
		})
	}

	for i, m := range monoMods {
		// Tag mono modules so the build progress / temp-file names are
		// distinguishable from per-pkg ones. The hash prefix is the
		// content-addressed key already; truncate for readability.
		short := monoHashes[i]
		if len(short) > 12 {
			short = short[:12]
		}

		out = append(out, namedIR{
			label:  "mono_" + short,
			irText: fixCoroAttrs(m.String()),
		})
	}

	return out
}

// compileIRWithPkgs is the multi-IR variant of compileIR. `pkgIRs` is
// one IR text per imported package; each is written to its own `.ll`,
// compiled to a `.o` in parallel with the entry IR + runtime.c, and
// added to the link inputs. When pkgIRs is empty this is identical to
// the legacy single-IR path.
func compileIRWithPkgs(ir string, pkgIRs []namedIR, outBin string, libMode bool, extraObjs []string, cSources []cSource, extraCFlags []string, prog *compileProgress, debugMode ...bool) error {
	isDebug := len(debugMode) > 0 && debugMode[0]
	// Write IR to temp file
	//goland:noinspection GoResourceLeak
	llFile, err := os.CreateTemp("", "tin-*.ll")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer func(name string) {
		_ = os.Remove(name)
	}(llFile.Name())

	if _, err := llFile.WriteString(ir); err != nil {
		return err
	}

	_ = llFile.Close()

	if dumpPath := os.Getenv("TIN_DUMP_IR"); dumpPath != "" {
		_ = os.WriteFile(dumpPath, []byte(ir), 0644)
	}

	llInputFile := llFile.Name()

	finalOpt := chooseOptLevel(isDebug)

	// LLVM 22's -O2 optimizer breaks coroutine yield paths: the "suspended"
	// default arm of coro.suspend in the resume function is marked unreachable,
	// causing backward DCE to remove the `store index; ret void` that the
	// scheduler depends on.  Work around by splitting coroutines at -O1 first
	// (which produces correct yield paths), then running -O2 on the split IR.
	//
	// Only needed when the final compile goes through aggressive optimization
	// (-O2/-O3); at -O0/-O1 the broken pass is not exercised and we skip the
	// split, which can otherwise cost a minute or more on coroutine-heavy IR.
	needsCoroSplit := finalOpt == "-O2" || finalOpt == "-O3"
	if needsCoroSplit && strings.Contains(ir, "llvm.coro.") {
		splitFile, err := os.CreateTemp("", "tin-split-*.ll")
		if err != nil {
			return fmt.Errorf("cannot create temp file for coro split: %w", err)
		}

		splitName := splitFile.Name()

		_ = splitFile.Close()

		defer func() { _ = os.Remove(splitName) }()

		sourceFile := outBin
		if prog != nil {
			sourceFile = prog.sourceFile
		}

		if prog != nil {
			prog.step(sourceFile, "coro split")
		}

		if err := coroSplit(llInputFile, splitName, compileIROpts{
			optLevel:    "-O1",
			targetFlags: clangTargetFlag(),
		}); err != nil {
			return fmt.Errorf("coro split pass failed: %w", err)
		}

		// LLVM 22's CoroSplitPass emits !DILabel nodes without the required
		// 'line' field when debug info is active. Patch them before the next
		// compile step to avoid "missing required field 'line'" errors.
		// stacktraceLinkActive flips on `-gline-tables-only` for the IR
		// compile, so the patch also has to run in that mode even when the
		// user didn't pass -g explicitly.
		if isDebug || stacktraceLinkActive {
			if data, readErr := os.ReadFile(splitName); readErr == nil {
				if patched := patchMissingDILabelLine(string(data)); patched != string(data) {
					_ = os.WriteFile(splitName, []byte(patched), 0644)
				}
			}
		}

		llInputFile = splitName
	}

	optLevel := chooseOptLevel(isDebug)

	// Find runtime .c alongside the tin binary
	ex, _ := os.Executable()
	rtC := filepath.Join(filepath.Dir(ex), "runtime", "runtime.c")

	if libMode {
		// Library mode: compile to object file(s) with -c, then merge with ld -r.
		// clang -c cannot write multiple inputs to a single -o, so each source is
		// compiled separately and the results are partially linked together.
		irObj, err := os.CreateTemp("", "tin-ir-*.o")
		if err != nil {
			return fmt.Errorf("cannot create temp object file: %w", err)
		}

		irObjName := irObj.Name()
		_ = irObj.Close()

		defer func() { _ = os.Remove(irObjName) }()

		if err := compileIRToNativeObj(llInputFile, irObjName, optLevel); err != nil {
			return err
		}

		objs := []string{irObjName}

		var tmpObjs []string

		for _, pkg := range pkgIRs {
			pkgLL, err := os.CreateTemp("", "tin-pkg-*.ll")
			if err != nil {
				return fmt.Errorf("cannot create temp IR file: %w", err)
			}

			pkgLLName := pkgLL.Name()
			if _, err := pkgLL.WriteString(pkg.irText); err != nil {
				_ = pkgLL.Close()
				_ = os.Remove(pkgLLName)

				return err
			}

			_ = pkgLL.Close()

			pkgObj, err := os.CreateTemp("", "tin-pkg-*.o")
			if err != nil {
				_ = os.Remove(pkgLLName)

				return fmt.Errorf("cannot create temp object file: %w", err)
			}

			pkgObjName := pkgObj.Name()
			_ = pkgObj.Close()

			tmpObjs = append(tmpObjs, pkgLLName, pkgObjName)

			if err := compileIRToNativeObj(pkgLLName, pkgObjName, optLevel); err != nil {
				for _, f := range tmpObjs {
					_ = os.Remove(f)
				}

				_ = os.Remove(pkgLLName)

				return err
			}

			objs = append(objs, pkgObjName)
		}

		for _, cs := range cSources {
			cObj, err := os.CreateTemp("", "tin-c-*.o")
			if err != nil {
				return fmt.Errorf("cannot create temp object file: %w", err)
			}

			cObjName := cObj.Name()
			_ = cObj.Close()

			tmpObjs = append(tmpObjs, cObjName)
			cArgs := append([]string{"-O2", "-c"}, clangTargetFlag()...)
			// -fPIC is required when these objects will be linked into
			// a shared library; harmless on the static path so we add
			// it unconditionally for lib mode.
			cArgs = append(cArgs, "-fPIC")
			cArgs = append(cArgs, cs.flags...)
			cArgs = append(cArgs, cs.path, "-o", cObjName)

			if prog != nil {
				prog.step(cs.path, "compile")
			}

			clangC := exec.Command("clang", cArgs...)
			clangC.Stdout = os.Stdout

			clangC.Stderr = os.Stderr
			if err := clangC.Run(); err != nil {
				for _, f := range tmpObjs {
					_ = os.Remove(f)
				}

				return err
			}

			objs = append(objs, cObjName)
		}

		defer func() {
			for _, f := range tmpObjs {
				_ = os.Remove(f)
			}
		}()

		if prog != nil {
			prog.step(outBin, "link")
		}

		// -static lib: merge into a single relocatable object via
		// `ld -r` (partial link). The output is a `.o` that another
		// link step pulls into a final binary.
		if staticLink {
			ldArgs := append([]string{"-r"}, objs...)
			ldArgs = append(ldArgs, "-o", outBin)
			ld := exec.Command("ld", ldArgs...)
			ld.Stdout = os.Stdout
			ld.Stderr = os.Stderr

			return ld.Run()
		}

		// Shared lib: link with `clang -shared` (Linux) or
		// `-dynamiclib` (macOS). Tin runtime symbols (__tin_release,
		// __tin_rc_alloc, ...) and any other unresolved references
		// are intentionally left undefined -- a `.so`/`.dylib`
		// produced by `tin build --lib` is meant to be loaded into
		// a host that already provides the runtime, mirroring the
		// `ld -r` static path which also leaves these unresolved.
		// macOS' default for `-dynamiclib` rejects undefined symbols
		// outright, so we explicitly opt into dynamic_lookup.
		shArgs := append([]string{}, clangTargetFlag()...)
		if targetGOOS == "darwin" {
			shArgs = append(shArgs, "-dynamiclib", "-Wl,-undefined,dynamic_lookup")
		} else {
			shArgs = append(shArgs, "-shared")
		}

		shArgs = append(shArgs, "-fPIC", "-o", outBin)
		shArgs = append(shArgs, objs...)

		clangSh := exec.Command("clang", shArgs...)
		clangSh.Stdout = os.Stdout
		clangSh.Stderr = os.Stderr

		return clangSh.Run()
	}

	// Split the compile and link phases so each translation unit (IR, runtime.c,
	// any //!+file.c sources) compiles to its own .o in parallel; the final
	// clang invocation only links. This converts the dominant single-threaded
	// clang call into N parallel -c calls + one fast link, scaling with -j.
	var (
		tmpObjs      []string // every temp .o we own; cleaned up on return
		linkInputs   []string // .o files passed to the link step (in stable order)
		cLinkerFlags []string // -l/-L flags pulled out of //!+file.c directives
	)

	defer func() {
		for _, f := range tmpObjs {
			_ = os.Remove(f)
		}
	}()

	mkObj := func(prefix string) (string, error) {
		f, err := os.CreateTemp("", prefix+"-*.o")
		if err != nil {
			return "", fmt.Errorf("cannot create temp object file: %w", err)
		}

		name := f.Name()
		_ = f.Close()

		tmpObjs = append(tmpObjs, name)

		return name, nil
	}

	var jobsList []compileJob

	// IR -> ir.o
	irObjName, err := mkObj("tin-ir")
	if err != nil {
		return err
	}

	linkInputs = append(linkInputs, irObjName)
	{
		a := append([]string{optLevel, "-c", "-flto=thin", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			a = append(a, "-g")

			if targetGOOS == "darwin" {
				a = append(a, "-fstandalone-debug")
			}
		}

		// .eh_frame helps external tools (gdb, perf) walk Tin frames
		// even though Tin's own stacktrace() now uses an FP walker
		// (see runtime/stacktrace.c). Default builds keep paying the
		// 5-10% binary-size tax for unwind info nothing reads on Linux
		// x86_64, so emit the negative path explicitly when stacktrace
		// isn't reachable.  See docs/plans/stacktrace-libunwind.md.
		//
		// Source line resolution no longer goes through DWARF: the
		// codegen post-pass (codegen/pclntab.go) emits a custom
		// `tin_pclntab` section that runtime/pclntab.c reads directly,
		// so we don't need `-gline-tables-only` even when stacktrace
		// is reachable. -g still emits full DWARF (the explicit `-g`
		// branch above) for lldb / gdb consumers, but stacktrace itself
		// uses pclntab in every build.
		if stacktraceLinkActive {
			a = append(a, "-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			a = append(a, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		a = append(a, llInputFile, "-o", irObjName)
		ll, out := llInputFile, irObjName

		jobsList = append(jobsList, compileJob{
			desc: filepath.Base(llInputFile),
			args: a,
			runFn: func() error {
				return compileIRToObj(ll, out, compileIROpts{
					optLevel:    optLevel,
					ltoMode:     "thin",
					targetFlags: clangTargetFlag(),
				})
			},
		})
	}

	// Per-pkg IR -> per-pkg .o. Each imported package's *ir.Module gets
	// its own .ll + clang -c invocation, run in parallel with the entry
	// IR / runtime.c jobs. The .o files are added to linkInputs so the
	// final clang link picks them up. With cross-module declares
	// (codegen/pkgmod.go addCrossModuleDeclares) and shared TypeDefs
	// (echoSharedTypeDefs), each pkg .ll is self-sufficient - its
	// cross-pkg references resolve at link time, not at compile time.
	for _, pkg := range pkgIRs {
		if dir := os.Getenv("TIN_DUMP_PKG_IR_DIR"); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, pkg.label+".ll"), []byte(pkg.irText), 0o644)
		}

		// Compose the canonical clang argv FIRST so the cache key reflects
		// every compile flag. We pass placeholders for the input/output
		// paths since the cache key only cares about content+flags, not
		// the exact temp-file names.
		flagsForKey := append([]string{optLevel, "-c", "-flto=thin", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			flagsForKey = append(flagsForKey, "-g")

			if targetGOOS == "darwin" {
				flagsForKey = append(flagsForKey, "-fstandalone-debug")
			}
		}

		if stacktraceLinkActive {
			flagsForKey = append(flagsForKey, "-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			flagsForKey = append(flagsForKey, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		// Step 7: per-pkg .o cache. Skip clang -c entirely when this
		// pkg's IR + flags hash matches a previously-built object.
		cachedObj, hit, cacheErr := pkgCacheLookup(pkg.irText, flagsForKey)
		if cacheErr != nil {
			return cacheErr
		}

		linkInputs = append(linkInputs, cachedObj)

		// Step 6 (D6): write per-pkg iface manifest next to the cache slot
		// so downstream consumers and tooling can inspect what shape this
		// pkg presents. The manifest is content-addressed alongside pkg.o
		// (same dir, same key prefix) so a cache hit also serves up the
		// matching iface.json. Idempotent - overwrite is a no-op on warm
		// rebuilds since the input is byte-identical.
		if pkg.iface != nil {
			ifacePath := strings.TrimSuffix(cachedObj, ".o") + ".iface.json"
			hashPath := strings.TrimSuffix(cachedObj, ".o") + ".iface_hash"

			if body, mErr := pkg.iface.MarshalCanonical(); mErr == nil {
				_ = os.WriteFile(ifacePath, body, 0o644)
			}

			if h, hErr := pkg.iface.IfaceHash(); hErr == nil {
				_ = os.WriteFile(hashPath, []byte(h), 0o644)
			}
		}

		if hit {
			continue
		}

		pkgLL, err := os.CreateTemp("", "tin-pkg-"+pkg.label+"-*.ll")
		if err != nil {
			return fmt.Errorf("cannot create temp pkg .ll: %w", err)
		}

		pkgLLName := pkgLL.Name()
		if _, err := pkgLL.WriteString(pkg.irText); err != nil {
			_ = pkgLL.Close()
			_ = os.Remove(pkgLLName)

			return err
		}

		_ = pkgLL.Close()

		defer func(name string) { _ = os.Remove(name) }(pkgLLName)

		// Compile to a temp path and rename atomically into the cache so
		// concurrent `tin` runs don't see a half-written .o. The PID alone
		// isn't sufficient when several goroutines inside the same process
		// race to compile the same content-hashed cache slot (mono modules
		// are content-addressed, so two test files using the same generic
		// instantiation hash to the same slot). An atomic counter
		// disambiguates them within the process.
		tempObj := cachedObj + fmt.Sprintf(".tmp.%d.%d",
			os.Getpid(), atomic.AddUint64(&tempCacheCounter, 1))

		a := append([]string{}, flagsForKey...)
		a = append(a, pkgLLName, "-o", tempObj)
		in, out := pkgLLName, tempObj

		jobsList = append(jobsList, compileJob{
			desc:     "pkg:" + pkg.label,
			args:     a,
			renameTo: cachedObj,
			runFn: func() error {
				return compileIRToObj(in, out, compileIROpts{
					optLevel:    optLevel,
					ltoMode:     "thin",
					targetFlags: clangTargetFlag(),
				})
			},
		})
	}

	// runtime.c -> runtime.o (only if rtC exists alongside the tin binary).
	// Globally cached across every Tin compile on this platform: the runtime
	// is identical for every program, so compiling it once per content+flags
	// hash saves ~400ms per invocation when the suite of tests is rebuilt.
	if _, statErr := os.Stat(rtC); statErr == nil {
		rtArgs := append([]string{"-O2", "-c", "-flto=thin", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)

		if isDebug {
			rtArgs = append(rtArgs, "-g")

			if targetGOOS == "darwin" {
				rtArgs = append(rtArgs, "-fstandalone-debug")
			}
		}

		// runtime/stacktrace.c gates its FP-walker body on TIN_STACKTRACE
		// so programs that don't use stacktrace() don't incur the
		// resolver code or the pclntab section overhead. The csrc cache
		// key includes the canonical argv, so this define naturally
		// produces two distinct cached .o entries (one with the stub,
		// one with the real walk) instead of cross-contaminating a
		// single cache slot.
		//
		// Source-line resolution comes from runtime/pclntab.c (always
		// linked via the umbrella) reading the codegen-emitted
		// `tin_pclntab` section. No DWARF / libdw involved at runtime,
		// so no `-gline-tables-only` here either.
		if stacktraceLinkActive {
			// -fno-omit-frame-pointer is REQUIRED for the FP walker:
			// stacktrace.c reads rbp/x29 via inline asm and walks the
			// saved-fp chain. Without this flag clang's Linux x86_64
			// default omits the frame pointer setup, so rbp is
			// whatever the caller's general-purpose state is (often 0
			// from the kernel-cleared startup) and the walk dies on
			// the first iteration. Tin user code already gets
			// `frame-pointer="all"` via codegen.applyStacktracePostPass,
			// but the runtime C is compiled separately and needs the
			// equivalent here.
			rtArgs = append(rtArgs, "-DTIN_STACKTRACE=1",
				"-fno-omit-frame-pointer", "-mno-omit-leaf-frame-pointer",
				"-funwind-tables", "-fasynchronous-unwind-tables")
		} else {
			rtArgs = append(rtArgs, "-fno-unwind-tables", "-fno-asynchronous-unwind-tables")
		}

		// mimalloc: compile runtime.c with -DTIN_USE_MIMALLOC=1 plus
		// the include path for mimalloc.h so the macro-shim in
		// runtime.c redirects malloc/free/realloc/calloc to mi_*.
		// csrc cache key includes argv so mimalloc vs non-mimalloc
		// builds get distinct .o entries.  If the user passed
		// --no-mimalloc, useMimalloc is false here and we skip the
		// shim entirely (libc malloc path).  Missing libmimalloc with
		// useMimalloc=true is surfaced as a link-time error in
		// linkBinary, not a silent skip.
		if useMimalloc {
			rtArgs = append(rtArgs, "-DTIN_USE_MIMALLOC=1")
			if _, incDir, ok := findMimallocInstall(targetGOOS); ok && incDir != "" {
				rtArgs = append(rtArgs, "-I"+incDir)
			}
		}

		cachedPath, hit, err := csrcCacheLookup(rtC, rtArgs)
		if err != nil {
			return err
		}

		linkInputs = append(linkInputs, cachedPath)
		if !hit {
			tempPath := cachedPath + fmt.Sprintf(".tmp.%d.%d", os.Getpid(), atomic.AddUint64(&tempCacheCounter, 1))

			rtArgs = append(rtArgs, rtC, "-o", tempPath)
			jobsList = append(jobsList, compileJob{
				desc: "runtime.c", args: rtArgs, renameTo: cachedPath,
			})
		}
	}

	// Each //!+file.c at -O2; -l/-L flags pulled out for the link step.
	// Cached globally (same .c content + flags == same .o, regardless of which
	// .tin program imports it).
	for _, cs := range cSources {
		var compileFlags []string

		for _, f := range cs.flags {
			if strings.HasPrefix(f, "-l") || strings.HasPrefix(f, "-L") {
				cLinkerFlags = append(cLinkerFlags, f)
			} else {
				compileFlags = append(compileFlags, f)
			}
		}

		baseArgs := append([]string{"-O2", "-c", "-flto=thin", "-ffunction-sections", "-fdata-sections"}, clangTargetFlag()...)
		baseArgs = append(baseArgs, compileFlags...)

		cachedPath, hit, err := csrcCacheLookup(cs.path, baseArgs)
		if err != nil {
			return err
		}

		linkInputs = append(linkInputs, cachedPath)

		if hit {
			continue
		}

		tempPath := cachedPath + fmt.Sprintf(".tmp.%d.%d", os.Getpid(), atomic.AddUint64(&tempCacheCounter, 1))

		a := append([]string{}, baseArgs...)
		a = append(a, cs.path, "-o", tempPath)
		jobsList = append(jobsList, compileJob{
			desc: filepath.Base(cs.path), args: a, renameTo: cachedPath,
		})
	}

	if prog != nil {
		prog.step(outBin, fmt.Sprintf("compile (%d TUs)", len(jobsList)))
	}

	// Run all -c jobs in parallel. parallelJobs() honors -j; default is GOMAXPROCS.
	parStart := time.Now()

	var parCb func(string, string, time.Duration)

	if prog != nil {
		prog.parallelStart(len(jobsList))
		parCb = prog.parallelEvent
	}

	parErr := runParallelClang(jobsList, parCb)

	if prog != nil {
		prog.parallelEnd(time.Since(parStart))
	}

	if parErr != nil {
		return parErr
	}

	// Link step: pull every compiled .o into one binary. With -flto=thin
	// passed at compile time, .o files contain LLVM bitcode and the link
	// runs the ThinLTO pipeline (parallel cross-TU inlining + global
	// dead-code elimination).
	if prog != nil {
		prog.step(outBin, "link")
	}

	allInputs := append([]string{}, linkInputs...)
	allInputs = append(allInputs, extraObjs...)

	useLld := runtime.GOOS != targetGOOS || runtime.GOARCH != targetGOARCH
	if useLld {
		// Cross-compile path: ld.lld is the only linker we can rely on
		// to support foreign target emulations. Fail fast with a
		// per-distro install hint if it's missing, instead of letting
		// clang surface a less-helpful "ld.lld: not found".
		requireCrossCompileTools()
	}

	return linkBinary(allInputs, outBin, linkOpts{
		optLevel:             optLevel,
		ltoMode:              "thin",
		debug:                isDebug,
		standaloneDebugMacOS: targetGOOS == "darwin",
		functionSecs:         true,
		dataSecs:             true,
		gcSections:           true,
		useLld:               useLld,
		rdynamic:             stacktraceLinkActive && targetGOOS != "darwin",
		targetGOOS:           targetGOOS,
		targetFlags:          clangTargetFlag(),
		cLinkerFlags:         cLinkerFlags,
		extraCFlags:          extraCFlags,
		static:               staticLink,
		useMimalloc:          useMimalloc,
	})
}

// chooseOptLevel returns the clang optimization flag for this build. Order of
// precedence:
//
//  1. -O<n> on the command line wins unconditionally.
//  2. -g (debug) selects -O0 so the debugger sees source as written.
//  3. `tin test` / `tin build-test` / `tin ir-test` default to -O0 because
//     optimization buys nothing for test correctness and can take 100x longer
//     than codegen itself on large IR.
//  4. Otherwise -O2.
