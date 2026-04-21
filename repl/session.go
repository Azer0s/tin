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

	return s, nil
}

func (s *session) buildRuntime(outSo string) error {
	rtC := filepath.Join(s.runtimeDir, "runtime.c")
	s.compiledCSrcPaths[rtC] = true

	args := []string{
		"-shared", "-fPIC", "-O2", "-pthread",
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

	inputLL := llFile

	// If the IR uses coroutines, split them at -O1 first (LLVM 22 DCE workaround).
	if strings.Contains(irText, "llvm.coro.") {
		splitLL := filepath.Join(s.workDir, fmt.Sprintf("cell%d_split.ll", s.cellCount))
		splitArgs := []string{"-O1", "-S", "-emit-llvm", llFile, "-o", splitLL}

		out, err := exec.Command("clang", splitArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("coro split: %w\n%s", err, out)
		}
		// Apply the DILabel line patch required for debug builds.
		if data, readErr := os.ReadFile(splitLL); readErr == nil {
			patched := patchMissingDILabelLine(string(data))
			_ = os.WriteFile(splitLL, []byte(patched), 0600)
		}

		inputLL = splitLL
	}

	// Compile to a shared library. Undefined references to runtime symbols are
	// resolved at dlopen time from the RTLD_GLOBAL namespace (libtin_runtime.so).
	// On Darwin, link explicitly against the runtime to satisfy the static linker.
	soArgs := []string{"-shared", "-fPIC", "-O2", inputLL}
	if runtime.GOOS == "darwin" {
		soArgs = append(soArgs, s.runtimeLib.path)
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

func clangMajorVersion() int {
	out, err := exec.Command("clang", "--version").Output()
	if err != nil {
		return 0
	}

	re := regexp.MustCompile(`version (\d+)`)
	m := re.FindSubmatch(out)

	if m == nil {
		return 0
	}

	v, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}

	return v
}

// patchMissingDILabelLine adds 'line: 0' to any !DILabel(...) metadata node
// that is missing the required 'line' field (LLVM 22 coro-split quirk).
func patchMissingDILabelLine(ir string) string {
	lines := strings.Split(ir, "\n")
	for i, line := range lines {
		if strings.Contains(line, "!DILabel(") && !strings.Contains(line, "line:") {
			line = strings.TrimRight(line, ")")
			lines[i] = line + ", line: 0)"
		}
	}

	return strings.Join(lines, "\n")
}
