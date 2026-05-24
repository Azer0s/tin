package codegen

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Azer0s/tin/lexer"
)

func ScanImportedNoParensMacros(currentFile string, tokens []lexer.Token, stdlibBase string, libsRoots []string) map[string]string {
	result := map[string]string{}

	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type != lexer.KW_USE {
			continue
		}

		i++
		if i >= len(tokens) || tokens[i].Type != lexer.LBRACE {
			continue
		}

		i++
		// Collect names until RBRACE.
		var names []string

		for i < len(tokens) && tokens[i].Type != lexer.RBRACE {
			if tokens[i].Type == lexer.IDENT {
				name := tokens[i].Literal
				if i+1 < len(tokens) && tokens[i+1].Type == lexer.NOT {
					name += "!"
					i++
				}

				names = append(names, name)
			}

			i++ // advance (also skips commas)
		}

		if i >= len(tokens) || tokens[i].Type != lexer.RBRACE {
			continue
		}

		i++ // consume }
		// Expect soft keyword "from".
		if i >= len(tokens) || tokens[i].Type != lexer.IDENT || tokens[i].Literal != "from" {
			continue
		}

		i++ // consume "from"
		// Read module path.
		if i >= len(tokens) {
			continue
		}

		var pkgPath string
		if tokens[i].Type == lexer.STRING_LIT {
			pkgPath = tokens[i].Literal
		} else {
			var parts []string

			for i < len(tokens) && (tokens[i].Type == lexer.IDENT || tokens[i].Type == lexer.DCOLON) {
				if tokens[i].Type == lexer.IDENT {
					parts = append(parts, tokens[i].Literal)
				}

				i++
			}

			i-- // will be incremented by outer loop
			pkgPath = strings.Join(parts, "::")
		}

		if pkgPath == "" {
			continue
		}

		nameSet := map[string]bool{}
		for _, n := range names {
			nameSet[strings.TrimSuffix(n, "!")] = true
		}

		localDir := ""
		if currentFile != "" {
			localDir = filepath.Dir(currentFile)
		}

		srcFile := resolvePackageSrc(pkgPath, stdlibBase, localDir, libsRoots)
		if srcFile != "" {
			if srcBytes, readErr := os.ReadFile(srcFile); readErr == nil {
				scanNoParensMacrosFromSource(srcBytes, nameSet, result)
			}
		}
	}

	return result
}

// scanNoParensMacrosFromSource scans raw .tin source bytes for macro declarations
// with a #no_parens tag, populating result with name->backtick_body for any
// names in the nameSet. Used as a fallback when no .tin.mod file is available.
func scanNoParensMacrosFromSource(src []byte, nameSet map[string]bool, result map[string]string) {
	l := lexer.New(string(src))

	tokens, err := l.Tokenize()
	if err != nil {
		return
	}

	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type != lexer.KW_MACRO {
			continue
		}

		// Expect optional tag block: { #tag ... }
		i++
		if i >= len(tokens) || tokens[i].Type != lexer.LBRACE {
			continue
		}

		var hasNoParens bool

		i++
		for i < len(tokens) && tokens[i].Type != lexer.RBRACE {
			if tokens[i].Type == lexer.CONTROL_TAG && tokens[i].Literal == "no_parens" {
				hasNoParens = true
			}

			i++
		}

		if i >= len(tokens) {
			break
		}

		// Consume `}` then read macro name.
		i++
		if i >= len(tokens) || tokens[i].Type != lexer.IDENT {
			continue
		}

		macroName := tokens[i].Literal
		if !nameSet[macroName] {
			continue
		}

		// Scan forward for `=` then a BACKTICK_LIT body.
		for i < len(tokens) && tokens[i].Type != lexer.ASSIGN {
			i++
		}

		i++
		if i >= len(tokens) || tokens[i].Type != lexer.BACKTICK_LIT {
			continue
		}

		if hasNoParens && tokens[i].Literal != "" {
			result[macroName] = tokens[i].Literal
		}
	}
}

// ensureDefaultTraitMethods generates default (non-virtual) trait methods for
// concreteName if the struct doesn't already have them.  This is needed when a
// struct satisfies a constraint via the trait's default implementation without
// explicitly listing the trait in its Implements clause.
