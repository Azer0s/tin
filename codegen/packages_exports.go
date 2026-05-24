package codegen

import (
	"fmt"
	"path/filepath"
	"strings"
)

func (cg *CodeGen) validatePackageExports(pkgName string, exportedNames map[string]bool) error {
	for name := range exportedNames {
		if cg.isVariantOfExportedAdt(name, exportedNames) {
			return fmt.Errorf("export of %q: ADT variant constructors are inferred from the parent ADT; remove %q from `export { ... } as %s`",
				name, name, pkgName)
		}

		if cg.isExportable(name, pkgName) {
			continue
		}

		return fmt.Errorf("export of %q: no top-level decl (struct, data, enum, trait, fn, macro) named %q is visible in package %q",
			name, name, pkgName)
	}

	return nil
}

// isVariantOfExportedAdt reports whether name is a variant of some
// ADT in the same export list. Used to flag redundant variant
// exports.
func (cg *CodeGen) isVariantOfExportedAdt(name string, exportedNames map[string]bool) bool {
	owners := cg.dataVariantLookup[name]
	for _, adt := range owners {
		// Strip the leading "pkg__" prefix so we compare against the
		// user-facing source name.
		bare := adt
		if idx := strings.Index(adt, "__"); idx >= 0 {
			bare = adt[idx+2:]
		}

		if exportedNames[bare] || exportedNames[adt] {
			return true
		}
	}

	return false
}

// isExportable reports whether name refers to a top-level decl that
// can appear in an export list.
func (cg *CodeGen) isExportable(name, pkgName string) bool {
	if cg.structTypeFor(CanonKey(name)) != nil {
		return true
	}

	if cg.structTypeFor(CanonKey(pkgName+"__"+name)) != nil {
		return true
	}

	if _, ok := cg.genericStructsByArity[name]; ok {
		return true
	}

	if cg.dataDeclFor(CanonKey(name)) != nil {
		return true
	}

	if cg.dataDeclFor(CanonKey(pkgName+"__"+name)) != nil {
		return true
	}

	if cg.isEnumFor(CanonKey(name)) {
		return true
	}

	if cg.traitFor(CanonKey(name)) != nil {
		return true
	}

	if _, ok := cg.funcDecls[name]; ok {
		return true
	}

	if _, ok := cg.overloads[name]; ok {
		return true
	}

	if _, ok := cg.constrainedFuncs[name]; ok {
		return true
	}

	if _, ok := cg.genericFuncs[name]; ok {
		return true
	}

	if _, ok := cg.macros[name]; ok {
		return true
	}
	// Macros are stored in cg.macros under multiple keys depending
	// on the load path: bare name (from direct file-path imports),
	// `pkg::name`, `pkg.name`, with and without a trailing `!`.
	// Look through every shape that could match this export name.
	bare := strings.TrimSuffix(name, "!")
	if _, ok := cg.macros[bare]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"::"+bare]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"::"+bare+"!"]; ok {
		return true
	}

	if _, ok := cg.macros[pkgName+"."+bare]; ok {
		return true
	}

	if cg.aliasTypeFor(CanonKey(name)) != nil {
		return true
	}

	if _, ok := cg.curScope.lookup(name); ok {
		return true
	}
	// Re-exported child-package names (e.g. `export { log } as std`
	// where log is itself a package): a name matches if any macro or
	// type alias is registered under `name::`.
	for k := range cg.macros {
		if strings.HasPrefix(k, name+"::") {
			return true
		}
	}

	for canon, r := range cg.types {
		if r.Alias != nil && strings.HasPrefix(string(canon), name+"::") {
			return true
		}
	}

	return false
}

// loadPackageSelective loads a package and then registers only the named symbols
// as bare names in the current scope (no pkg:: prefix required).
// Used by `use { name1, name2! } from pkg` declarations.
func (cg *CodeGen) loadPackageSelective(path string, names []string, isFile bool) error {
	// Load the package normally (registers pkg-qualified names).
	var err error
	if isFile {
		err = cg.loadPackageFromFilePath(path)
	} else {
		err = cg.loadPackage(path)
	}

	if err != nil {
		return err
	}

	// Determine package name for scope lookups.
	parts := strings.Split(path, "::")
	pkgName := parts[len(parts)-1]
	// For file paths, derive from basename.
	if isFile {
		base := filepath.Base(path)

		pkgName = strings.TrimSuffix(strings.TrimSuffix(base, ".tin"), "./")
		if pkgName == "" {
			pkgName = base
		}
	}

	// Re-register each requested name as a bare (unqualified) name.
	for _, name := range names {
		bareName := strings.TrimSuffix(name, "!")
		isMacroCall := strings.HasSuffix(name, "!")

		if isMacroCall {
			// Macro: look up by pkg-qualified key and register as bare name.
			found := false

			for _, key := range []string{
				pkgName + "." + bareName + "!",
				pkgName + "::" + bareName + "!",
				pkgName + "." + bareName,
				pkgName + "::" + bareName,
			} {
				if m, ok := cg.macros[key]; ok {
					cg.macros[bareName+"!"] = m
					cg.macros[bareName] = m
					found = true

					break
				}
			}

			if !found {
				// Macro may not be in cg.macros yet; try to find it bare.
				if m, ok := cg.macros[bareName+"!"]; ok {
					cg.macros[bareName] = m
				}
			}

			continue
		}

		// Function: look up by pkg-qualified key and register as bare name.
		for _, key := range []string{
			pkgName + "." + bareName,
			pkgName + "::" + bareName,
		} {
			if entry, ok := cg.curScope.lookup(key); ok {
				cg.curScope.set(bareName, entry)

				break
			}
		}

		// Type/struct alias: look up pkg-qualified and register as bare.
		for _, key := range []string{
			pkgName + "::" + bareName,
			pkgName + "." + bareName,
		} {
			if te := cg.aliasTypeFor(CanonKey(key)); te != nil {
				if cg.aliasTypeFor(CanonKey(bareName)) == nil {
					cg.setTypeAlias(bareName, te)
				}

				break
			}
		}
		// Mark bareName visible as a type so the strict-bare-type
		// resolver lets `Name` resolve through cg.dataDecls /
		// cg.structTypes / cg.enumTypes / cg.traits. Without this,
		// `use { Result } from result` only registers an alias for
		// non-ADT shapes; ADTs live in cg.dataDecls (flat global
		// namespace) and only the visibility set gates bare access.
		if cg.isTypeName(bareName, pkgName) {
			cg.curScope.markTypeVisible(bareName)
		}
	}

	return nil
}

// isTypeName reports whether bareName names a top-level type (data,
// struct, enum, trait, type alias) - either as the raw bare key or as
// the package-prefixed key. Used by selective-import to decide whether
// to add the name to the importer's visibleTypes set.
func (cg *CodeGen) isTypeName(bareName, pkgName string) bool {
	if cg.dataDeclFor(CanonKey(bareName)) != nil {
		return true
	}

	if cg.dataDeclFor(CanonKey(pkgName+"__"+bareName)) != nil {
		return true
	}

	if cg.structTypeFor(CanonKey(bareName)) != nil {
		return true
	}

	if cg.structTypeFor(CanonKey(pkgName+"__"+bareName)) != nil {
		return true
	}

	if _, ok := cg.genericStructsByArity[bareName]; ok {
		return true
	}

	if cg.isEnumFor(CanonKey(bareName)) {
		return true
	}

	if cg.traitFor(CanonKey(bareName)) != nil {
		return true
	}

	if cg.aliasTypeFor(CanonKey(bareName)) != nil {
		return true
	}

	if cg.aliasTypeFor(CanonKey(pkgName+"::"+bareName)) != nil {
		return true
	}

	if cg.aliasTypeFor(CanonKey(pkgName+"."+bareName)) != nil {
		return true
	}

	return false
}

// ScanImportedNoParensMacros scans a token stream for `use { names } from path`
// patterns, loads the corresponding .tin.mod files, and returns a map of
// macroName -> backtick_expansion for any #no_parens macros that would be
// selectively imported. Call this before Parse() so the parser can do token
// substitution for these macros.
