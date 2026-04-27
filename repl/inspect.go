package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

// inspectDecl prints the source of a session-level declaration (struct,
// trait, enum, type alias, or fn) when the user types its bare name in
// the REPL. Returns true if a matching decl was found.
//
// The keys checked match what evalCell saves in declMap:
//   - structs/fns: name itself
//   - traits/enums/types: prefixed with "trait__" / "enum__" / "type__"
func (s *session) inspectDecl(name string) bool {
	candidates := []struct{ key, kind string }{
		{name, ""},
		{"trait__" + name, "trait"},
		{"enum__" + name, "enum"},
		{"type__" + name, "type"},
	}

	for _, c := range candidates {
		src, ok := s.declMap[c.key]
		if !ok {
			continue
		}

		fmt.Println(strings.TrimSpace(src))

		return true
	}

	return false
}

// inspectModule prints exported symbols from a loaded module.
// Returns true if the module was found and inspected.
func (s *session) inspectModule(pkgName string) bool {
	// Check if this module was loaded via a use declaration.
	key := "use__" + pkgName
	if _, ok := s.declMap[key]; !ok {
		return false
	}

	src := s.findModuleSrc(pkgName)
	if src == "" {
		fmt.Printf("module %s (no source found)\n", pkgName)

		return true
	}

	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Printf("module %s\n", pkgName)

		return true
	}

	fns, structs, enums := scanModuleExports(string(data), pkgName)
	if len(fns) == 0 && len(structs) == 0 && len(enums) == 0 {
		fmt.Printf("module %s (no exports)\n", pkgName)

		return true
	}

	fmt.Printf("module %s\n", pkgName)

	if len(structs) > 0 {
		sort.Strings(structs)

		for _, s := range structs {
			fmt.Printf("  struct %s\n", s)
		}
	}

	if len(enums) > 0 {
		sort.Strings(enums)

		for _, e := range enums {
			fmt.Printf("  enum %s\n", e)
		}
	}

	if len(fns) > 0 {
		sort.Strings(fns)

		for _, f := range fns {
			fmt.Printf("  %s\n", f)
		}
	}

	return true
}

// findModuleSrc resolves the source file for a package name using stdlib and libs roots.
func (s *session) findModuleSrc(pkgName string) string {
	stdlibDir := s.stdlibOverride
	if stdlibDir == "" {
		if ex, err := os.Executable(); err == nil {
			stdlibDir = filepath.Join(filepath.Dir(ex), "stdlib")
		}
	}

	candidates := []string{
		filepath.Join(stdlibDir, pkgName+".tin"),
		filepath.Join(stdlibDir, pkgName, pkgName+".tin"),
	}
	for _, root := range s.libsRoots {
		candidates = append(candidates,
			filepath.Join(root, pkgName+".tin"),
			filepath.Join(root, pkgName, pkgName+".tin"),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	return ""
}

// registerModuleMacros scans a module source file and adds its exported
// macros to the session's macro registry.
func (s *session) registerModuleMacros(pkgName string) {
	if s.macros == nil {
		return
	}

	srcPath := s.findModuleSrc(pkgName)
	if srcPath == "" {
		return
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return
	}

	l := lexer.New(string(data))

	tokens, err := l.Tokenize()
	if err != nil {
		return
	}

	p := parser.New(tokens)

	prog, err := p.Parse()
	if err != nil {
		return
	}

	exported := map[string]bool{}

	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName == pkgName {
			for _, name := range exp.Names {
				exported[name] = true
			}
		}
	}

	for _, node := range prog.Stmts {
		m, ok := node.(*ast.MacroDecl)
		if !ok {
			continue
		}

		if !exported[m.Name] {
			continue
		}

		bare := strings.TrimSuffix(m.Name, "!")
		hasBang := strings.HasSuffix(m.Name, "!")
		noParens := false

		for _, tag := range m.Tags {
			if tag == "no_parens" {
				noParens = true
			}
		}

		s.macros.register(macroEntry{
			Name:     bare,
			Params:   m.Params,
			HasBang:  hasBang,
			NoParens: noParens,
			Decl:     m,
		})
	}
}

// scanModuleExports parses a .tin source file and returns exported symbols.
func scanModuleExports(src, pkgName string) (fns, structs, enums []string) {
	l := lexer.New(src)

	tokens, err := l.Tokenize()
	if err != nil {
		return
	}

	p := parser.New(tokens)

	prog, err := p.Parse()
	if err != nil {
		return
	}

	// Collect what's exported.
	exported := map[string]bool{}

	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName == pkgName {
			for _, name := range exp.Names {
				exported[name] = true
			}
		}
	}

	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if !exported[n.Name] {
				continue
			}

			fns = append(fns, formatFuncSig(pkgName, n))
		case *ast.MacroDecl:
			if !exported[n.Name] {
				continue
			}

			fns = append(fns, formatMacroSig(pkgName, n))
		case *ast.StructDecl:
			if !exported[n.Name] {
				continue
			}

			structs = append(structs, n.Name)
		case *ast.EnumDecl:
			if !exported[n.Name] {
				continue
			}

			enums = append(enums, n.Name)
		}
	}

	return
}

func formatMacroSig(pkgName string, m *ast.MacroDecl) string {
	var sb strings.Builder

	sb.WriteString("macro ")
	sb.WriteString(pkgName)
	sb.WriteString("::")
	sb.WriteString(m.Name)

	if len(m.Params) > 0 {
		sb.WriteString("(")

		for i, p := range m.Params {
			if i > 0 {
				sb.WriteString(", ")
			}

			sb.WriteString(p)
		}

		sb.WriteString(")")
	}

	return sb.String()
}

func formatFuncSig(pkgName string, f *ast.FuncDecl) string {
	var sb strings.Builder

	sb.WriteString("fn ")
	sb.WriteString(pkgName)
	sb.WriteString("::")
	sb.WriteString(f.Name)
	sb.WriteString("(")

	for i, p := range f.Params {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString(p.Name)
		sb.WriteString(" ")

		if p.Type != nil {
			sb.WriteString(p.Type.String())
		}
	}

	sb.WriteString(")")

	if f.RetType != nil {
		sb.WriteString(" ")
		sb.WriteString(f.RetType.String())
	}

	return sb.String()
}
