package repl

import (
	"os"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// completer implements readline.AutoCompleter for the REPL. Tab fires after
// `pkg::` to list a package's exports, and after `obj.` to list methods on
// the receiver's type. Falls back to no completions for other contexts so
// the user can still type freely.
type completer struct{ s *session }

// Do is called by readline when Tab is pressed. line[:pos] is the buffer up
// to the cursor. Returns candidate suffixes plus the length (in runes) of
// the existing prefix that each candidate replaces.
func (c *completer) Do(line []rune, pos int) ([][]rune, int) {
	if pos > len(line) {
		pos = len(line)
	}

	src := string(line[:pos])

	if cands, n := c.completeScope(src); cands != nil {
		return cands, n
	}

	if cands, n := c.completeField(src); cands != nil {
		return cands, n
	}

	return nil, 0
}

// completeScope handles `pkg::partial` -> {exported names of pkg starting with partial}.
func (c *completer) completeScope(src string) ([][]rune, int) {
	// Walk backwards through trailing identifier characters to get `partial`.
	end := len(src)
	partialStart := end

	for partialStart > 0 && isIdentByte(src[partialStart-1]) {
		partialStart--
	}

	partial := src[partialStart:end]

	// Expect "::" immediately before the partial.
	if partialStart < 2 || src[partialStart-2:partialStart] != "::" {
		return nil, 0
	}
	// Walk backwards over the package name.
	pkgEnd := partialStart - 2
	pkgStart := pkgEnd

	for pkgStart > 0 && isIdentByte(src[pkgStart-1]) {
		pkgStart--
	}

	pkgName := src[pkgStart:pkgEnd]
	if pkgName == "" {
		return nil, 0
	}

	// Either a known module (`use pkg`) or a known struct (for static fns).
	exports := c.scopeMembersForName(pkgName)
	if len(exports) == 0 {
		return nil, 0
	}

	return filterAndOrder(exports, partial), len([]rune(partial))
}

// completeField handles `obj.partial` -> {method/field names on obj's type}.
func (c *completer) completeField(src string) ([][]rune, int) {
	end := len(src)
	partialStart := end

	for partialStart > 0 && isIdentByte(src[partialStart-1]) {
		partialStart--
	}

	partial := src[partialStart:end]

	if partialStart < 1 || src[partialStart-1] != '.' {
		return nil, 0
	}

	objEnd := partialStart - 1
	objStart := objEnd

	for objStart > 0 && isIdentByte(src[objStart-1]) {
		objStart--
	}

	objName := src[objStart:objEnd]
	if objName == "" {
		return nil, 0
	}

	typeName := c.s.typeNameOfGlobal(objName)
	if typeName == "" {
		return nil, 0
	}

	members := c.s.methodsAndFieldsOf(typeName)
	if len(members) == 0 {
		return nil, 0
	}

	return filterAndOrder(members, partial), len([]rune(partial))
}

// scopeMembersForName returns the exported symbol names available under
// `name::` - either the exports of a `use`d package or the static methods
// of a known struct.
func (c *completer) scopeMembersForName(name string) []string {
	if c.s == nil {
		return nil
	}

	if _, ok := c.s.declMap["use__"+name]; ok {
		if path := c.s.findModuleSrc(name); path != "" {
			data, err := os.ReadFile(path) //nolint:gosec
			if err == nil {
				return moduleExportNames(string(data), name)
			}
		}
	}

	// Struct static methods: name might be a session struct.
	if src, ok := c.s.declMap[name]; ok {
		prog, err := parseSrc(src)
		if err != nil {
			return nil
		}

		for _, n := range prog.Stmts {
			if sd, ok := n.(*ast.StructDecl); ok && sd.Name == name {
				var out []string

				for _, m := range sd.Methods {
					if m.IsStatic {
						out = append(out, m.Name)
					}
				}

				return out
			}
		}
	}

	return nil
}

// typeNameOfGlobal returns the bare struct name (e.g. "Channel") of a
// session-level global, or "" if unknown.
func (s *session) typeNameOfGlobal(name string) string {
	src, ok := s.prevGlobals[name]
	if !ok {
		return ""
	}
	// Saved form: "var <name> <type>" -- take everything after the second space.
	parts := strings.SplitN(src, " ", 3)
	if len(parts) < 3 {
		return ""
	}

	t := strings.TrimSpace(parts[2])
	// Strip generic type-args ("Channel[string]" -> "Channel").
	if idx := strings.IndexByte(t, '['); idx >= 0 {
		t = t[:idx]
	}
	// Strip leading pointer markers.
	t = strings.TrimLeft(t, "*")
	// Strip module prefix ("sync::Channel" -> "Channel").
	if idx := strings.LastIndex(t, "::"); idx >= 0 {
		t = t[idx+2:]
	}

	return t
}

// methodsAndFieldsOf returns the field and method names declared on a struct
// known to the session - either via a session-level struct decl or via a
// loaded package.
func (s *session) methodsAndFieldsOf(structName string) []string {
	if structName == "" {
		return nil
	}
	// Check session-level decl first.
	if src, ok := s.declMap[structName]; ok {
		if names := scanStructMembers(src, structName); len(names) > 0 {
			return names
		}
	}
	// Fall back to package-loaded structs. A package's entry .tin often only
	// re-exports symbols defined in sibling files (e.g. sync.tin re-exports
	// Channel from channel.tin), so walk all .tin files under the package
	// directory.
	for _, key := range s.declOrder {
		if !strings.HasPrefix(key, "use__") {
			continue
		}

		pkgName := strings.TrimPrefix(key, "use__")

		entry := s.findModuleSrc(pkgName)
		if entry == "" {
			continue
		}

		for _, p := range siblingTinFiles(entry) {
			data, err := os.ReadFile(p) //nolint:gosec
			if err != nil {
				continue
			}

			if names := scanStructMembers(string(data), structName); len(names) > 0 {
				return names
			}
		}
	}

	return nil
}

// siblingTinFiles returns every .tin file in the directory containing entry
// (non-recursive). Used to scan all files of a package without parsing its
// `use "./..."` imports - directory-local enumeration is robust enough for
// completion purposes.
func siblingTinFiles(entry string) []string {
	dir := filepathDir(entry)

	out := []string{entry}

	files, err := os.ReadDir(dir)
	if err != nil {
		return out
	}

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".tin") {
			continue
		}

		full := dir + "/" + f.Name()
		if full == entry {
			continue
		}

		out = append(out, full)
	}

	return out
}

// filepathDir returns everything before the last "/" in p, or "." if none.
func filepathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}

	return "."
}

// moduleExportNames parses a module source and returns the bare names listed
// in any `export { ... } as <pkgName>` declaration. This works whether or
// not the declared symbols live in the same file (e.g. sync.tin only
// re-exports symbols defined in mutex.tin, channel.tin, etc.).
func moduleExportNames(src, pkgName string) []string {
	prog, err := parseSrc(src)
	if err != nil {
		return nil
	}

	var out []string

	seen := map[string]bool{}

	for _, n := range prog.Stmts {
		exp, ok := n.(*ast.ExportDecl)
		if !ok || exp.AsName != pkgName {
			continue
		}

		for _, name := range exp.Names {
			if !seen[name] {
				seen[name] = true

				out = append(out, name)
			}
		}
	}

	return out
}

// scanStructMembers parses `src` and returns the field/method names of the
// first StructDecl whose name matches `structName`.
func scanStructMembers(src, structName string) []string {
	prog, err := parseSrc(src)
	if err != nil {
		return nil
	}

	for _, n := range prog.Stmts {
		sd, ok := n.(*ast.StructDecl)
		if !ok || sd.Name != structName {
			continue
		}

		out := make([]string, 0, len(sd.Fields)+len(sd.Methods))
		for _, f := range sd.Fields {
			if !f.IsForward {
				out = append(out, f.Name)
			}
		}

		seen := map[string]bool{}

		for _, m := range sd.Methods {
			if m.IsStatic {
				continue
			}

			if seen[m.Name] {
				continue
			}

			seen[m.Name] = true

			out = append(out, m.Name)
		}

		return out
	}

	return nil
}

// formattedFnName extracts the bare function name from a `fn pkg::name(...)`
// signature returned by formatFuncSig.
func formattedFnName(sig string) string {
	rest := strings.TrimPrefix(sig, "fn ")
	if i := strings.LastIndex(rest, "::"); i >= 0 {
		rest = rest[i+2:]
	}

	if i := strings.IndexByte(rest, '('); i >= 0 {
		rest = rest[:i]
	}

	return strings.TrimSpace(rest)
}

// filterAndOrder returns the names from `all` that have `partial` as a
// case-insensitive prefix, sorted, as the rune-slice form readline expects.
// Each candidate is returned with the prefix stripped (only the suffix).
func filterAndOrder(all []string, partial string) [][]rune {
	lo := strings.ToLower(partial)

	var out []string

	for _, n := range all {
		if strings.HasPrefix(strings.ToLower(n), lo) {
			out = append(out, n)
		}
	}

	if len(out) == 0 {
		return nil
	}

	sort.Strings(out)

	res := make([][]rune, len(out))
	for i, n := range out {
		res[i] = []rune(n[len(partial):])
	}

	return res
}

// isIdentByte reports whether c is part of a Tin identifier (ASCII).
func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}
