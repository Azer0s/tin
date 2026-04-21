// Package repl implements the interactive REPL for the Tin language.
package repl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// pkgCSource represents one C file to compile from a package directive.
type pkgCSource struct {
	path  string
	flags []string
}

// ensurePkgCSources scans the given package source paths for //!+ and //!- directives,
// compiles any C sources that haven't been compiled yet, and loads the resulting
// shared library with RTLD_GLOBAL so the symbols are available to cell .so files.
func (s *session) ensurePkgCSources(pkgSrcPaths []string) error {
	var newCSources []pkgCSource

	var newLinkerFlags []string

	stdlibDir := s.stdlibOverride
	if stdlibDir == "" {
		if ex, err := os.Executable(); err == nil {
			stdlibDir = filepath.Join(filepath.Dir(ex), "stdlib")
		}
	}

	seenInBatch := map[string]bool{}
	seenFlag := map[string]bool{}

	for _, srcPath := range pkgSrcPaths {
		data, err := os.ReadFile(srcPath)
		if err != nil {
			continue
		}

		srcDir := filepath.Dir(srcPath)

		links, cSrcs := parsePkgDirectives(string(data), srcDir, stdlibDir)
		for _, f := range links {
			if !seenFlag[f] {
				seenFlag[f] = true
				newLinkerFlags = append(newLinkerFlags, f)
			}
		}

		for _, cs := range cSrcs {
			if s.compiledCSrcPaths[cs.path] || seenInBatch[cs.path] {
				continue
			}

			seenInBatch[cs.path] = true
			newCSources = append(newCSources, cs)
		}
	}

	if len(newCSources) == 0 {
		return nil
	}

	// Mark all as compiled before attempting, so partial failures don't retry.
	for _, cs := range newCSources {
		s.compiledCSrcPaths[cs.path] = true
	}

	// Compile all new C sources into a single extras .so.
	extrasIdx := len(s.loadedLibs) // use current lib count as unique index

	extrasSo := filepath.Join(s.workDir, fmt.Sprintf("pkgext%d.so", extrasIdx))
	if err := s.compilePkgExtras(newCSources, newLinkerFlags, extrasSo); err != nil {
		return err
	}

	extLib, err := openLib(extrasSo, true)
	if err != nil {
		return fmt.Errorf("load pkg extras: %w", err)
	}

	s.loadedLibs = append(s.loadedLibs, extLib)
	s.darwinLinkLibs = append(s.darwinLinkLibs, extLib)

	return nil
}

// compilePkgExtras compiles a set of C sources into a shared library.
func (s *session) compilePkgExtras(cSrcs []pkgCSource, linkerFlags []string, outSo string) error {
	rtDir := s.runtimeDir

	args := []string{"-shared", "-fPIC", "-O2", "-pthread", "-I" + rtDir}

	for _, cs := range cSrcs {
		args = append(args, cs.flags...)
		args = append(args, cs.path)
	}

	args = append(args, linkerFlags...)
	// On Darwin, link explicitly against the runtime to satisfy the static linker.
	if runtime.GOOS == "darwin" {
		args = append(args, s.runtimeLib.path)
	}

	args = append(args, "-o", outSo)

	out, err := exec.Command("clang", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("compile pkg extras: %w\n%s", err, out)
	}

	return nil
}

// parsePkgDirectives scans the leading lines of src for //!+ and //!- directives.
func parsePkgDirectives(src, srcDir, stdlibDir string) (linkerFlags []string, cSources []pkgCSource) {
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || (strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "//!")) {
			continue
		}

		if !strings.HasPrefix(trimmed, "//!") {
			break
		}

		rest := strings.TrimSpace(trimmed[3:])
		if rest == "" {
			continue
		}

		if strings.HasPrefix(rest, "+") {
			spec := strings.TrimSpace(rest[1:])
			parts := strings.SplitN(spec, " -- ", 2)

			fileAndQual, archQual := extractPkgArchQualifier(strings.TrimSpace(parts[0]))
			if !pkgArchMatches(archQual) {
				continue
			}

			cpath := filepath.Join(srcDir, fileAndQual)

			var extraFlags []string

			if len(parts) == 2 {
				expanded := expandPkgShellExprs(parts[1])
				for _, f := range strings.Fields(expanded) {
					f = strings.ReplaceAll(f, "$TIN_RUNTIME", filepath.Dir(srcDir)+"/../runtime")
					if ex, err := os.Executable(); err == nil {
						f = strings.ReplaceAll(f, "$TIN_STDLIB", filepath.Join(filepath.Dir(ex), "stdlib"))
					}

					if strings.HasPrefix(f, "-I") && len(f) > 2 {
						iPath := f[2:]
						if !filepath.IsAbs(iPath) {
							iPath = filepath.Join(srcDir, iPath)
						}

						extraFlags = append(extraFlags, "-I"+iPath)
					} else {
						extraFlags = append(extraFlags, f)
					}
				}
			}

			cSources = append(cSources, pkgCSource{path: cpath, flags: extraFlags})
		} else if strings.HasPrefix(rest, "-") {
			flag, archQual := extractPkgArchQualifier(rest)
			if pkgArchMatches(archQual) {
				linkerFlags = append(linkerFlags, flag)
			}
		}
	}

	return
}

func extractPkgArchQualifier(s string) (base, qualifier string) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "["); i >= 0 && strings.HasSuffix(s, "]") {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1 : len(s)-1])
	}

	return s, ""
}

func pkgArchMatches(qualifier string) bool {
	if qualifier == "" {
		return true
	}

	goos := runtime.GOOS
	goarch := runtime.GOARCH

	for _, tok := range strings.Split(qualifier, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "x86_64":
			if goarch != "amd64" {
				return false
			}
		case "aarch64":
			if goarch != "arm64" {
				return false
			}
		case "386":
			if goarch != "386" {
				return false
			}
		case "darwin":
			if goos != "darwin" {
				return false
			}
		case "linux":
			if goos != "linux" {
				return false
			}
		}
	}

	return true
}

func expandPkgShellExprs(s string) string {
	for {
		start := strings.Index(s, "$(")
		if start == -1 {
			break
		}

		end := strings.Index(s[start+2:], ")")
		if end == -1 {
			break
		}

		end += start + 2
		cmd := s[start+2 : end]
		out, err := exec.Command("sh", "-c", cmd).Output()

		val := ""
		if err == nil {
			val = strings.TrimSpace(string(out))
		}

		s = s[:start] + val + s[end+1:]
	}

	return s
}
