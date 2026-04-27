package repl

import (
	"os"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// completer implements readline.AutoCompleter for the REPL. Tab fires after
// a `::` or `.` separator and offers candidate suffixes based on what the
// receiver expression resolves to:
//
//	pkg::partial             -> exported names of the package
//	Struct::partial          -> static methods of a session struct
//	pkg::Struct::partial     -> static methods of Struct from pkg
//	obj.partial              -> instance methods/fields of obj's type
//	Type.partial             -> static methods (also accepted via .)
//	pkg::Type.partial        -> static methods of Type from pkg
//	pkg::Type[T...].partial  -> static methods of Type from pkg (any nesting)
//	obj.field.partial        -> methods/fields of field's type (chained)
type completer struct{ s *session }

// Do is called by readline when Tab is pressed. line[:pos] is the buffer up
// to the cursor. Returns candidate suffixes plus the length (in runes) of
// the existing prefix that each candidate replaces.
func (c *completer) Do(line []rune, pos int) ([][]rune, int) {
	if pos > len(line) {
		pos = len(line)
	}

	src := string(line[:pos])

	end := len(src)
	partialStart := end

	for partialStart > 0 && isIdentByte(src[partialStart-1]) {
		partialStart--
	}

	partial := src[partialStart:end]
	partialLen := len([]rune(partial))

	if partialStart >= 2 && src[partialStart-2:partialStart] == "::" {
		recv := strings.TrimSpace(src[walkReceiverBack(src, partialStart-2) : partialStart-2])
		if recv == "" {
			return nil, 0
		}

		cands := c.completeAfterScope(recv, partial)
		if cands == nil {
			return nil, 0
		}

		return cands, partialLen
	}

	if partialStart >= 1 && src[partialStart-1] == '.' {
		recv := strings.TrimSpace(src[walkReceiverBack(src, partialStart-1) : partialStart-1])
		if recv == "" {
			return nil, 0
		}

		cands := c.completeAfterDot(recv, partial)
		if cands == nil {
			return nil, 0
		}

		return cands, partialLen
	}

	return nil, 0
}

// walkReceiverBack walks backwards from `end` over a receiver expression
// (identifier chars, balanced [...], `::` chains, and `.` chains) and
// returns the start byte offset.
func walkReceiverBack(src string, end int) int {
	i := end
	for i > 0 {
		c := src[i-1]

		switch {
		case isIdentByte(c):
			i--
		case c == ']':
			depth := 1
			j := i - 2

			for j >= 0 && depth > 0 {
				switch src[j] {
				case ']':
					depth++
				case '[':
					depth--
				}

				j--
			}

			if depth != 0 {
				return i
			}

			i = j + 1
		case c == ':' && i >= 2 && src[i-2] == ':':
			i -= 2
		case c == '.':
			i--
		default:
			return i
		}
	}

	return i
}

// completeAfterScope handles `recv::partial`. recv may be `pkg`, `Type`,
// `pkg::Type`, possibly with trailing `[generics]`.
func (c *completer) completeAfterScope(recv, partial string) [][]rune {
	pkg, name := splitLastScope(stripGenericsTail(recv))
	if name == "" {
		return filterAndOrder(c.scopeMembersForName(pkg), partial)
	}

	return filterAndOrder(c.staticMethodsOf(pkg, name), partial)
}

// completeAfterDot handles `recv.partial`.
func (c *completer) completeAfterDot(recv, partial string) [][]rune {
	if dotIdx := lastTopLevelDot(recv); dotIdx >= 0 {
		head := recv[:dotIdx]
		field := recv[dotIdx+1:]

		baseType := c.typeOfReceiver(head)
		if baseType == "" {
			return nil
		}

		fieldType := c.s.typeOfStructField(baseType, field)
		if fieldType == "" {
			return nil
		}

		return filterAndOrder(c.s.methodsAndFieldsOf(fieldType), partial)
	}

	pkg, name := splitLastScope(stripGenericsTail(recv))
	if name != "" {
		return filterAndOrder(c.staticMethodsOf(pkg, name), partial)
	}

	if typeName := c.s.typeNameOfGlobal(pkg); typeName != "" {
		return filterAndOrder(c.s.methodsAndFieldsOf(typeName), partial)
	}

	if statics := c.staticMethodsOf("", pkg); len(statics) > 0 {
		return filterAndOrder(statics, partial)
	}

	return nil
}

// typeOfReceiver returns the bare struct name of a receiver expression.
// Resolves identifiers via prevGlobals, type-name expressions to their bare
// name (stripped of generics and module prefix), and chained field access
// recursively.
func (c *completer) typeOfReceiver(recv string) string {
	recv = strings.TrimSpace(recv)
	if recv == "" {
		return ""
	}

	if dotIdx := lastTopLevelDot(recv); dotIdx >= 0 {
		head := recv[:dotIdx]
		field := recv[dotIdx+1:]

		baseType := c.typeOfReceiver(head)
		if baseType == "" {
			return ""
		}

		return c.s.typeOfStructField(baseType, field)
	}

	pkg, name := splitLastScope(stripGenericsTail(recv))
	if name == "" {
		if t := c.s.typeNameOfGlobal(pkg); t != "" {
			return t
		}

		if _, ok := c.s.declMap[pkg]; ok {
			return pkg
		}

		return ""
	}

	return name
}

// stripGenericsTail strips a trailing balanced [...] from src.
func stripGenericsTail(src string) string {
	src = strings.TrimSpace(src)
	if !strings.HasSuffix(src, "]") {
		return src
	}

	depth := 1
	i := len(src) - 2

	for i >= 0 && depth > 0 {
		switch src[i] {
		case ']':
			depth++
		case '[':
			depth--
		}

		i--
	}

	if depth != 0 {
		return src
	}

	return strings.TrimSpace(src[:i+1])
}

// splitLastScope splits "a::b" into ("a", "b") and "x" into ("x", "").
func splitLastScope(s string) (pkg, name string) {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:])
	}

	return strings.TrimSpace(s), ""
}

// lastTopLevelDot returns the byte index of the last `.` not inside [...],
// or -1 if none.
func lastTopLevelDot(s string) int {
	depth := 0

	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ']':
			depth++
		case '[':
			depth--
		case '.':
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// scopeMembersForName returns the names available under `name::` - either
// the exports of a `use`d package or the static methods of a known struct.
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

	return c.staticMethodsOf("", name)
}

// staticMethodsOf returns the static method names of struct `name`. With
// pkg=="" the lookup is done against the session declMap; otherwise the
// package's source files are walked.
func (c *completer) staticMethodsOf(pkg, name string) []string {
	if name == "" {
		return nil
	}

	src := c.structDeclSrc(pkg, name)
	if src == "" {
		return nil
	}

	prog, err := parseSrc(src)
	if err != nil {
		return nil
	}

	for _, n := range prog.Stmts {
		sd, ok := n.(*ast.StructDecl)
		if !ok || sd.Name != name {
			continue
		}

		var out []string

		for _, m := range sd.Methods {
			if m.IsStatic {
				out = append(out, m.Name)
			}
		}

		return out
	}

	return nil
}

// structDeclSrc returns the source containing the declaration of struct
// `name`. With pkg=="" the session declMap is consulted, otherwise the
// package's sibling .tin files are walked.
func (c *completer) structDeclSrc(pkg, name string) string {
	if pkg == "" {
		return c.s.declMap[name]
	}

	entry := c.s.findModuleSrc(pkg)
	if entry == "" {
		return ""
	}

	for _, p := range siblingTinFiles(entry) {
		data, err := os.ReadFile(p) //nolint:gosec
		if err != nil {
			continue
		}

		if hasStructNamed(string(data), name) {
			return string(data)
		}
	}

	return ""
}

func hasStructNamed(src, name string) bool {
	prog, err := parseSrc(src)
	if err != nil {
		return false
	}

	for _, n := range prog.Stmts {
		if sd, ok := n.(*ast.StructDecl); ok && sd.Name == name {
			return true
		}
	}

	return false
}

// typeOfStructField returns the bare struct name of the field `fieldName`
// declared on struct `structName`, looked up across the session and any
// loaded package directories.
func (s *session) typeOfStructField(structName, fieldName string) string {
	if structName == "" || fieldName == "" {
		return ""
	}

	srcs := []string{s.declMap[structName]}
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
			if err == nil {
				srcs = append(srcs, string(data))
			}
		}
	}

	for _, src := range srcs {
		if src == "" {
			continue
		}

		prog, err := parseSrc(src)
		if err != nil {
			continue
		}

		for _, n := range prog.Stmts {
			sd, ok := n.(*ast.StructDecl)
			if !ok || sd.Name != structName {
				continue
			}

			for _, f := range sd.Fields {
				if f.Name == fieldName && f.Type != nil {
					return canonicalTypeName(f.Type.String())
				}
			}
		}
	}

	return ""
}

// canonicalTypeName converts a Tin-printed type string (e.g.
// "sync::Channel[string]" or "*Box") to the bare struct name.
func canonicalTypeName(t string) string {
	t = strings.TrimSpace(t)

	if i := strings.IndexByte(t, '['); i >= 0 {
		t = t[:i]
	}

	t = strings.TrimLeft(t, "*")

	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}

	return t
}

// typeNameOfGlobal returns the bare struct name (e.g. "Channel") of a
// session-level global, or "" if unknown.
func (s *session) typeNameOfGlobal(name string) string {
	src, ok := s.prevGlobals[name]
	if !ok {
		return ""
	}

	parts := strings.SplitN(src, " ", 3)
	if len(parts) < 3 {
		return ""
	}

	return canonicalTypeName(parts[2])
}

// methodsAndFieldsOf returns the field and method names declared on a
// struct known to the session - either via a session-level decl or via a
// loaded package.
func (s *session) methodsAndFieldsOf(structName string) []string {
	if structName == "" {
		return nil
	}

	if src, ok := s.declMap[structName]; ok {
		if names := scanStructMembers(src, structName); len(names) > 0 {
			return names
		}
	}

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

func filepathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}

	return "."
}

// moduleExportNames parses a module source and returns the bare names listed
// in any `export { ... } as <pkgName>` declaration.
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

func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}
