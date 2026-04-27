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

// inspectDecl prints a syntax-highlighted signature view of a session-level
// declaration (struct, trait, enum, type alias, or fn) when the user types
// its bare name in the REPL. Function bodies are omitted - only the
// signature is shown for each method/function.
//
// Returns true if a matching decl was found.
//
// The keys checked match what evalCell saves in declMap:
//   - structs/fns: name itself
//   - traits/enums/types: prefixed with "trait__" / "enum__" / "type__"
func (s *session) inspectDecl(name string) bool {
	candidates := []string{
		name,
		"trait__" + name,
		"enum__" + name,
		"type__" + name,
	}

	var src string

	for _, k := range candidates {
		if v, ok := s.declMap[k]; ok {
			src = v

			break
		}
	}

	if src == "" {
		return false
	}

	prog, err := parseSrc(src)
	if err != nil {
		fmt.Println(colorizeSource(src))

		return true
	}

	for _, node := range prog.Stmts {
		if matchesDeclName(node, name) {
			out := renderDeclSig(node)
			fmt.Println(colorizeSource(out))

			return true
		}
	}

	// Fallback: didn't find the matching node by name; print the source.
	fmt.Println(colorizeSource(src))

	return true
}

// matchesDeclName reports whether the AST node is a declaration named `name`.
func matchesDeclName(node ast.Node, name string) bool {
	switch n := node.(type) {
	case *ast.StructDecl:
		return n.Name == name
	case *ast.FuncDecl:
		return n.Name == name
	case *ast.TraitDecl:
		return n.Name == name
	case *ast.EnumDecl:
		return n.Name == name
	case *ast.TypeDecl:
		return n.Name == name
	}

	return false
}

// renderDeclSig formats a declaration as a signature view, omitting function
// bodies. Used by `:list` and bare-name REPL inspection.
func renderDeclSig(node ast.Node) string {
	var sb strings.Builder

	switch n := node.(type) {
	case *ast.StructDecl:
		renderStructSig(&sb, n)
	case *ast.FuncDecl:
		renderFuncSig(&sb, n)
	case *ast.TraitDecl:
		renderTraitSig(&sb, n)
	case *ast.EnumDecl:
		renderEnumSig(&sb, n)
	case *ast.TypeDecl:
		renderTypeAliasSig(&sb, n)
	}

	return strings.TrimRight(sb.String(), "\n")
}

func renderStructSig(sb *strings.Builder, n *ast.StructDecl) {
	sb.WriteString("struct ")
	sb.WriteString(n.Name)

	if len(n.TypeParams) > 0 {
		sb.WriteByte('[')
		sb.WriteString(strings.Join(n.TypeParams, ", "))
		sb.WriteByte(']')
	}

	if len(n.Implements) > 0 {
		sb.WriteString("(")

		for i, t := range n.Implements {
			if i > 0 {
				sb.WriteString(", ")
			}

			sb.WriteString(t.String())
		}

		sb.WriteString(")")
	}

	if len(n.Fields) > 0 || len(n.Methods) > 0 {
		sb.WriteString(" =")
	}

	for _, f := range n.Fields {
		if f.IsForward {
			continue
		}

		sb.WriteString("\n  ")
		sb.WriteString(f.Name)
		sb.WriteString(" ")

		if f.IsWeak {
			sb.WriteString("weak ")
		} else if f.IsOwn {
			sb.WriteString("own ")
		}

		if f.Type != nil {
			sb.WriteString(f.Type.String())
		}
	}

	for _, m := range n.Methods {
		sb.WriteString("\n  ")
		renderFuncSig(sb, m)
	}
}

func renderFuncSig(sb *strings.Builder, n *ast.FuncDecl) {
	if n.IsStatic {
		sb.WriteString("static fn ")
	} else {
		sb.WriteString("fn ")
	}

	if n.TraitQualifier != "" {
		// Alias-trait impls (`fn ::add`) round-trip with TraitQualifier == Name;
		// preserve the original `::name` syntax in that case.
		if n.TraitQualifier == n.Name {
			sb.WriteString("::")
		} else {
			sb.WriteString(n.TraitQualifier)
			sb.WriteString("::")
		}
	}

	sb.WriteString(n.Name)

	if len(n.TypeParams) > 0 {
		sb.WriteByte('[')
		sb.WriteString(strings.Join(n.TypeParams, ", "))
		sb.WriteByte(']')
	}

	sb.WriteByte('(')

	for i, p := range n.Params {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString(p.Name)

		if p.Type != nil {
			sb.WriteByte(' ')
			sb.WriteString(p.Type.String())
		}
	}

	sb.WriteByte(')')

	if n.RetType != nil {
		sb.WriteByte(' ')
		sb.WriteString(n.RetType.String())
	}

	if n.IsVirtual {
		sb.WriteString(" = virtual")
	} else if n.IsExtern != "" {
		sb.WriteString(" = extern(\"" + n.IsExtern + "\")")
	}
}

func renderTraitSig(sb *strings.Builder, n *ast.TraitDecl) {
	sb.WriteString("trait ")
	sb.WriteString(n.Name)

	if len(n.TypeParams) > 0 {
		sb.WriteByte('[')
		sb.WriteString(strings.Join(n.TypeParams, ", "))
		sb.WriteByte(']')
	}

	if n.IsAlias {
		sb.WriteString(" as ")

		if n.IsStaticAlias {
			sb.WriteString("static ")
		}

		if ft, ok := n.AliasType.(*ast.FuncType); ok {
			sb.WriteString("fn(")

			for i, p := range ft.Params {
				if i > 0 {
					sb.WriteString(", ")
				}

				sb.WriteString(p.String())
			}

			sb.WriteByte(')')

			if ft.RetType != nil {
				sb.WriteByte(' ')
				sb.WriteString(ft.RetType.String())
			}
		} else if n.AliasType != nil {
			sb.WriteString(n.AliasType.String())
		}

		return
	}

	if len(n.Methods) > 0 || len(n.ForwardFields) > 0 {
		sb.WriteString(" =")
	}

	for _, f := range n.ForwardFields {
		sb.WriteString("\n  ")
		sb.WriteString(f.Name)
		sb.WriteByte(' ')

		if f.Type != nil {
			sb.WriteString(f.Type.String())
		}

		sb.WriteString(" forward")
	}

	for _, m := range n.Methods {
		sb.WriteString("\n  ")
		renderFuncSig(sb, m)
	}
}

func renderEnumSig(sb *strings.Builder, n *ast.EnumDecl) {
	sb.WriteString("enum ")

	if n.IsAtom {
		sb.WriteString("atom ")
	}

	sb.WriteString(n.Name)

	if n.BaseType != nil {
		sb.WriteByte(' ')
		sb.WriteString(n.BaseType.String())
	}

	if len(n.Members) > 0 {
		sb.WriteString(" =")

		for _, m := range n.Members {
			sb.WriteString("\n  ")
			sb.WriteString(m.Name)
		}
	}
}

func renderTypeAliasSig(sb *strings.Builder, n *ast.TypeDecl) {
	sb.WriteString("type ")
	sb.WriteString(n.Name)

	if len(n.TypeParams) > 0 {
		sb.WriteByte('[')
		sb.WriteString(strings.Join(n.TypeParams, ", "))
		sb.WriteByte(']')
	}

	if n.Type != nil {
		sb.WriteString(" = ")
		sb.WriteString(n.Type.String())
	}
}

// colorizeSource tokenises a Tin source string and wraps each token in the
// same ANSI escapes the input painter uses, so REPL output matches input
// styling. Whitespace between tokens is preserved verbatim.
func colorizeSource(src string) string {
	l := lexer.New(src)

	tokens, err := l.Tokenize()
	if err != nil || len(tokens) == 0 {
		return src
	}

	runes := []rune(src)

	var (
		out strings.Builder
		pos int
	)

	for _, tok := range tokens {
		if tok.Type == lexer.EOF || tok.Type == lexer.NEWLINE ||
			tok.Type == lexer.INDENT || tok.Type == lexer.DEDENT {
			continue
		}

		lit := []rune(tok.Literal)
		if len(lit) == 0 {
			continue
		}

		// Find token's rune offset in the source by linear scan from pos.
		start := -1

		for j := pos; j <= len(runes)-len(lit); j++ {
			match := true

			for k, r := range lit {
				if runes[j+k] != r {
					match = false

					break
				}
			}

			if match {
				start = j

				break
			}
		}

		if start == -1 {
			continue
		}

		out.WriteString(string(runes[pos:start]))

		color := colorFor(tok.Type, tok.Literal)
		if color != "" {
			out.WriteString(color)
		}

		out.WriteString(string(lit))

		if color != "" {
			out.WriteString(ansiReset)
		}

		pos = start + len(lit)
	}

	out.WriteString(string(runes[pos:]))

	return out.String()
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
