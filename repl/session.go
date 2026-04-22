package repl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

// session holds all accumulated state for a running REPL session.
type session struct {
	// declMap: declaration name -> source text (last write wins for re-defs).
	declMap   map[string]string
	declOrder []string // insertion order for stable compilation

	// prevGlobals: name -> "var name type" source for cross-cell `let` vars.
	// Injected as TopLevelVar declarations into each subsequent compilation.
	prevGlobals  map[string]string
	globalsOrder []string

	// Loaded shared libraries (kept open so their symbols remain available).
	loadedLibs []*lib

	// darwinLinkLibs holds pre-compiled C libraries (sync prereq + pkgext)
	// that cell .so files must link against explicitly on Darwin. Cell .so
	// files are NOT included here to avoid duplicate-symbol SIGTRAPs.
	darwinLinkLibs []*lib

	// Runtime shared library (fiber scheduler, echo, etc.)
	runtimeLib *lib

	// Working directory for temp .ll and .so files.
	workDir string

	// Paths to the tin runtime and stdlib directories.
	runtimeDir     string
	stdlibOverride string
	libsRoots      []string

	// Shared macro registry (also held by the inputReader's highlighter/completer).
	macros *macroRegistry

	// compiledCSrcPaths tracks C source files already compiled into pkg extras .so files.
	compiledCSrcPaths map[string]bool

	cellCount int
}

// newSession creates a new session and compiles the runtime shared library.
func newSession(runtimeDir, stdlibOverride string, libsRoots []string, macros *macroRegistry) (*session, error) {
	workDir, err := os.MkdirTemp("", "tin-repl-*")
	if err != nil {
		return nil, fmt.Errorf("cannot create work dir: %w", err)
	}

	s := &session{
		declMap:           make(map[string]string),
		prevGlobals:       make(map[string]string),
		workDir:           workDir,
		runtimeDir:        runtimeDir,
		stdlibOverride:    stdlibOverride,
		libsRoots:         libsRoots,
		macros:            macros,
		compiledCSrcPaths: make(map[string]bool),
	}

	// Build libtin_runtime.so from runtime/runtime.c.
	rtSo := filepath.Join(workDir, "libtin_runtime.so")
	if err := s.buildRuntime(rtSo); err != nil {
		_ = os.RemoveAll(workDir)

		return nil, fmt.Errorf("build runtime: %w", err)
	}

	rtLib, err := openLib(rtSo, true)
	if err != nil {
		_ = os.RemoveAll(workDir)

		return nil, fmt.Errorf("load runtime: %w", err)
	}

	s.runtimeLib = rtLib
	setupRuntime(rtLib)
	fiberInit()

	// On Darwin the static linker rejects undefined symbols, so cell .so files
	// must be linked against every lib that provides symbols they reference.
	// The sync module is auto-loaded for every cell (coroutines need Future[T]),
	// so sync_helpers.c and channel_arc.c are always required - pre-compile them
	// here so they're in s.loadedLibs before the first cell is built.
	if runtime.GOOS == "darwin" {
		syncSo := filepath.Join(workDir, "libtin_sync_prereq.so")
		if err := s.buildSyncPrereq(syncSo); err != nil {
			_ = os.RemoveAll(workDir)

			return nil, fmt.Errorf("build sync prereq: %w", err)
		}

		syncLib, err := openLib(syncSo, true)
		if err != nil {
			_ = os.RemoveAll(workDir)

			return nil, fmt.Errorf("load sync prereq: %w", err)
		}

		s.loadedLibs = append(s.loadedLibs, syncLib)
		s.darwinLinkLibs = append(s.darwinLinkLibs, syncLib)
	}

	return s, nil
}

// buildSyncPrereq compiles sync_helpers.c and channel_arc.c into a shared lib.
// On Darwin this must happen before the first cell compilation because every
// cell auto-loads the sync module (for Future[T] / coroutine support).
func (s *session) buildSyncPrereq(outSo string) error {
	stdlibDir := s.stdlibOverride
	if stdlibDir == "" {
		if ex, err := os.Executable(); err == nil {
			stdlibDir = filepath.Join(filepath.Dir(ex), "stdlib")
		}
	}

	syncDir := filepath.Join(stdlibDir, "sync")
	cSrcs := []pkgCSource{
		{path: filepath.Join(syncDir, "sync_helpers.c")},
		{path: filepath.Join(syncDir, "channel_arc.c")},
	}

	for _, cs := range cSrcs {
		s.compiledCSrcPaths[cs.path] = true
	}

	return s.compilePkgExtras(cSrcs, nil, outSo)
}

func (s *session) buildRuntime(outSo string) error {
	rtC := filepath.Join(s.runtimeDir, "runtime.c")
	s.compiledCSrcPaths[rtC] = true

	args := []string{
		"-shared", "-fPIC", "-O1", "-pthread",
		"-I" + s.runtimeDir,
		rtC,
		"-o", outSo,
	}

	cmd := exec.Command("clang", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	return nil
}

// close cleans up temp files. Does not close loaded libraries
// (they must outlive any JIT'd code still running).
func (s *session) close() {
	_ = os.RemoveAll(s.workDir)
}

// evalCell compiles and executes one REPL cell. source is the raw Tin text.
func (s *session) evalCell(source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil
	}

	// Pre-expand any macro calls in the cell source using the text-level registry.
	// This handles scoped calls like `macros::min!(5, 10)` before the parser sees them.
	for {
		expanded, ok := tryExpandMacro(source, s.macros)
		if !ok {
			break
		}

		source = expanded
	}

	// Parse the cell to separate declarations from imperative statements.
	cellProg, err := parseSrc(source)
	if err != nil {
		return err
	}

	var cellDecls []ast.Node // fn/struct/trait/enum/type/use/export/var

	var cellStmts []ast.Node // let/echo/expressions/...

	// Collect decl key→src pairs without registering yet; addDecl is called
	// after a successful compile so that new declarations don't appear in both
	// the re-parsed accumulated source AND cellDecls (which would cause duplicate
	// TypeDef entries in the LLVM module).
	type pendingDecl struct{ key, src string }

	var pendingDecls []pendingDecl

	for _, node := range cellProg.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			pendingDecls = append(pendingDecls, pendingDecl{n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.StructDecl:
			pendingDecls = append(pendingDecls, pendingDecl{n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.TraitDecl:
			pendingDecls = append(pendingDecls, pendingDecl{"trait__" + n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.EnumDecl:
			pendingDecls = append(pendingDecls, pendingDecl{"enum__" + n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.TypeDecl:
			pendingDecls = append(pendingDecls, pendingDecl{"type__" + n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.UseDecl:
			key := "use__" + strings.ReplaceAll(n.Path, "::", ".")
			pendingDecls = append(pendingDecls, pendingDecl{key, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.ExportDecl:
			pendingDecls = append(pendingDecls, pendingDecl{"export__" + n.AsName, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		case *ast.TopLevelVar:
			pendingDecls = append(pendingDecls, pendingDecl{"var__" + n.Name, extractSrc(source, cellProg.Stmts, node)})
			cellDecls = append(cellDecls, node)
		default:
			cellStmts = append(cellStmts, node)
		}
	}

	// Namespace inspection: if the cell is a single bare identifier that names
	// a loaded `use` module, print its exported symbols and return early.
	if len(cellDecls) == 0 && len(cellStmts) == 1 {
		if es, ok := cellStmts[0].(*ast.ExprStmt); ok {
			if id, ok := es.Expr.(*ast.Identifier); ok {
				if s.inspectModule(id.Name) {
					return nil
				}
			}
		}
	}

	// Auto-print: convert bare expression statements to echo so that typing
	// `a` or `typeof(42)` prints the value without an explicit echo.
	// Skip SpawnExpr: it's a fire-and-forget statement, not something to echo.
	for i, node := range cellStmts {
		if es, ok := node.(*ast.ExprStmt); ok {
			if _, isSpawn := es.Expr.(*ast.SpawnExpr); !isSpawn {
				cellStmts[i] = &ast.EchoStmt{Value: es.Expr}
			}
		}
	}

	// Determine if we have an executable cell.
	cellFuncName := ""
	if len(cellStmts) > 0 {
		cellFuncName = fmt.Sprintf("_repl_cell_%d", s.cellCount)
	}

	// Build the combined AST for this compilation:
	//   1. Previous globals re-injected as TopLevelVar declarations
	//   2. All accumulated declaration source re-parsed for type context
	//   3. New declarations from this cell
	//   4. Synthesized async cell function (if any statements)
	combinedProg, err := s.buildCombinedProgram(cellDecls, cellStmts, cellFuncName)
	if err != nil {
		return err
	}

	// Codegen.
	cg := codegen.New("<repl>")
	if s.stdlibOverride != "" {
		cg.SetStdlibOverride(s.stdlibOverride)
	}

	for _, r := range s.libsRoots {
		cg.AddLibsRoot(r)
	}

	if cellFuncName != "" {
		cg.SetReplMode(cellFuncName)
	}

	if len(s.globalsOrder) > 0 {
		cg.SetReplExternalGlobals(s.globalsOrder)
	}

	mod, cgErr := cg.Generate(combinedProg)
	if cgErr != nil {
		return cgErr
	}

	// Compile any C source files required by newly loaded packages (e.g. tls_impl.c)
	// into a shared library and load it with RTLD_GLOBAL before the cell .so.
	if err := s.ensurePkgCSources(cg.PackageSrcPaths()); err != nil {
		return err
	}

	// Persist declarations now that codegen succeeded (before compile so that a
	// compile error still updates the session for re-declaration scenarios).
	for _, pd := range pendingDecls {
		s.addDecl(pd.key, pd.src)
	}

	// Apply fixCoroAttrs before compiling.
	irText := fixCoroAttrs(mod.String())
	irText = fixLinkOnceOdr(irText)

	// Compile the IR to a shared library.
	cellSo := filepath.Join(s.workDir, fmt.Sprintf("cell%d.so", s.cellCount))
	if err := s.compileToSo(irText, cellSo); err != nil {
		return err
	}

	// Load the cell shared library (RTLD_GLOBAL so its globals are canonical).
	cellLib, err := openLib(cellSo, true)
	if err != nil {
		return fmt.Errorf("load cell: %w", err)
	}

	s.loadedLibs = append(s.loadedLibs, cellLib)

	// Register new globals for subsequent cells.
	for _, g := range cg.ReplNewGlobals() {
		typeName := cg.ReplGlobalTinTypeName(g)
		if typeName != "" {
			varSrc := fmt.Sprintf("var %s %s", g.Name, typeName)
			if _, exists := s.prevGlobals[g.Name]; !exists {
				s.globalsOrder = append(s.globalsOrder, g.Name)
			}

			s.prevGlobals[g.Name] = varSrc
		}
	}

	// Execute the cell function (if any).
	if cellFuncName != "" {
		coroSym := cellFuncName + "$coro"

		coroFn, err := cellLib.lookup(coroSym)
		if err != nil {
			return fmt.Errorf("cell entry not found (%s): %w", coroSym, err)
		}

		runCell(coroFn)
	}

	s.cellCount++

	return nil
}

func (s *session) buildCombinedProgram(cellDecls, cellStmts []ast.Node, cellFuncName string) (*ast.Program, error) {
	// Re-parse accumulated declarations for type context.
	var allSrcParts []string
	for _, key := range s.globalsOrder {
		allSrcParts = append(allSrcParts, s.prevGlobals[key])
	}

	for _, key := range s.declOrder {
		allSrcParts = append(allSrcParts, s.declMap[key])
	}

	combined := &ast.Program{}

	if len(allSrcParts) > 0 {
		fullSrc := strings.Join(allSrcParts, "\n")

		prog, err := parseSrc(fullSrc)
		if err != nil {
			return nil, fmt.Errorf("context parse error: %w", err)
		}

		combined.Stmts = prog.Stmts
	}

	// Append new declarations from this cell.
	combined.Stmts = append(combined.Stmts, cellDecls...)

	// Synthesize the async cell function wrapping the imperative statements.
	if len(cellStmts) > 0 && cellFuncName != "" {
		cellFunc := &ast.FuncDecl{
			Name: cellFuncName,
			Tags: []string{"async"},
			Body: &ast.Block{Stmts: cellStmts},
		}
		combined.Stmts = append(combined.Stmts, cellFunc)
	}

	return combined, nil
}

func (s *session) addDecl(key, src string) {
	isNew := false

	if _, exists := s.declMap[key]; !exists {
		s.declOrder = append(s.declOrder, key)
		isNew = true
	}

	s.declMap[key] = src

	// Register macros from newly loaded use declarations.
	if isNew && strings.HasPrefix(key, "use__") {
		pkgName := strings.TrimPrefix(key, "use__")
		s.registerModuleMacros(pkgName)
	}
}

// compileToSo compiles LLVM IR to a shared library via clang.
func (s *session) compileToSo(irText, outSo string) error {
	llFile := filepath.Join(s.workDir, fmt.Sprintf("cell%d.ll", s.cellCount))
	if err := os.WriteFile(llFile, []byte(irText), 0600); err != nil {
		return err
	}

	// Compile to a shared library. On Linux, undefined symbols are resolved at
	// dlopen time from the RTLD_GLOBAL namespace. On Darwin the static linker
	// rejects undefined symbols; link against the pre-compiled C libs (runtime,
	// sync prereq, pkgext) and allow any remaining cross-cell Tin symbols (e.g.
	// prev-cell globals) to resolve at dlopen time. Do NOT link against previous
	// cell .so files - they redefine accumulated Tin functions and cause
	// duplicate-symbol SIGTRAPs on macOS's strict two-level namespace dyld.
	//
	// O1 is the minimum level that runs the coroutine-split pass; it also avoids
	// the LLVM 22 O2 DCE bug that eliminated coro frames before the split.
	soArgs := []string{"-shared", "-fPIC", "-O1", llFile}
	if runtime.GOOS == "darwin" {
		soArgs = append(soArgs, s.runtimeLib.path)
		for _, lib := range s.darwinLinkLibs {
			soArgs = append(soArgs, lib.path)
		}

		soArgs = append(soArgs, "-undefined", "dynamic_lookup")
	}

	soArgs = append(soArgs, "-o", outSo)

	out, err := exec.Command("clang", soArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("compile: %w\n%s", err, out)
	}

	return nil
}

// reset clears all accumulated session state (but keeps the runtime).
func (s *session) reset() {
	s.declMap = make(map[string]string)
	s.declOrder = nil
	s.prevGlobals = make(map[string]string)
	s.globalsOrder = nil
	// Keep loadedLibs open - RTLD_GLOBAL symbols are already in the process namespace.
	// The canonical globals remain at their current addresses; they'll be
	// re-shadowed by any new cells that define the same names.
	s.cellCount = 0
}

// parseSrc lexes and parses a Tin source string.
func parseSrc(src string) (*ast.Program, error) {
	l := lexer.New(src)

	tokens, err := l.Tokenize()
	if err != nil {
		return nil, err
	}

	p := parser.New(tokens)

	prog, parseErr := p.Parse()
	if parseErr != nil {
		return nil, parseErr
	}

	return prog, nil
}

// extractSrc returns the source text for a node within a parsed program.
// It uses the node's position to find the relevant lines.
func extractSrc(src string, allNodes []ast.Node, target ast.Node) string {
	lines := strings.Split(src, "\n")
	pos := target.Pos()

	startLine := pos.Line - 1 // 1-based to 0-based
	if startLine < 0 {
		startLine = 0
	}

	// Find the end line: either the start of the next top-level node or EOF.
	endLine := len(lines)

	for _, n := range allNodes {
		nLine := n.Pos().Line - 1
		if nLine > startLine && nLine < endLine {
			endLine = nLine
		}
	}

	result := strings.Join(lines[startLine:endLine], "\n")

	return strings.TrimSpace(result)
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
		ir = strings.ReplaceAll(ir, "declare void @llvm.coro.end(i8*", "declare i1 @llvm.coro.end(ptr")
		ir = strings.ReplaceAll(ir, "call void @llvm.coro.end(i8*", "%_coro_end = call i1 @llvm.coro.end(ptr")
	}

	return ir
}

// llLinkageKeywords is the set of LLVM linkage keywords that may appear
// immediately after `define`. If the first token is one of these the definition
// already carries explicit linkage and must not be modified.
var llLinkageKeywords = map[string]bool{
	"private": true, "internal": true, "available_externally": true,
	"linkonce": true, "weak": true, "common": true, "appending": true,
	"extern_weak": true, "linkonce_odr": true, "weak_odr": true,
}

// fixLinkOnceOdr changes every externally-visible function definition in the IR
// to linkonce_odr, except for REPL cell entry points.
//
// The REPL re-compiles all accumulated declarations in every new cell for type
// context, producing duplicate strong symbol definitions across cell .so files.
// On Darwin, dyld SIGTRAPs on duplicate strong symbols in two-level namespace
// libraries. linkonce_odr tells dyld to silently use the first loaded definition,
// matching Linux's first-loaded-wins behavior.
//
// Cell entry points (_repl_cell_N$coro) are excluded: linkonce_odr allows the
// optimizer to discard functions with no callers in the same module, and the
// cell entry is only called externally via dlsym.
func fixLinkOnceOdr(ir string) string {
	lines := strings.Split(ir, "\n")

	for i, line := range lines {
		if !strings.HasPrefix(line, "define ") {
			continue
		}

		rest := strings.TrimPrefix(line, "define ")
		firstWord := rest

		if idx := strings.IndexByte(rest, ' '); idx >= 0 {
			firstWord = rest[:idx]
		}

		if llLinkageKeywords[firstWord] || strings.Contains(line, "@_repl_cell_") {
			continue
		}

		lines[i] = "define linkonce_odr " + rest
	}

	return strings.Join(lines, "\n")
}

var (
	clangVersionOnce  sync.Once
	clangVersionCache int
)

func clangMajorVersion() int {
	clangVersionOnce.Do(func() {
		out, err := exec.Command("clang", "--version").Output()
		if err != nil {
			return
		}

		re := regexp.MustCompile(`version (\d+)`)
		m := re.FindSubmatch(out)

		if m == nil {
			return
		}

		clangVersionCache, _ = strconv.Atoi(string(m[1]))
	})

	return clangVersionCache
}
