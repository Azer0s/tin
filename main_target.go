package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// clangTripleForTarget returns the canonical LLVM target triple for the
// current targetGOOS/targetGOARCH pair.
func clangTripleForTarget() string {
	switch targetGOOS + "/" + targetGOARCH {
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	case "linux/386":
		return "i386-unknown-linux-gnu"
	case "darwin/amd64":
		return "x86_64-apple-macosx11.0.0"
	case "darwin/arm64":
		return "arm64-apple-macosx11.0.0"
	default:
		return ""
	}
}

// clangTargetFlag returns the cross-compile flags for an explicit
// -target invocation: `-target <triple>` plus, on darwin, an
// `-isysroot <macOS SDK path>` so clang can find the Darwin headers
// (malloc/malloc.h, libunwind.h, etc). Returns nil when no
// cross-compile is requested (host triple is used implicitly).
//
// The SDK path is taken from -macos-sdk if given, then $TIN_MACOS_SDK,
// then well-known locations (Xcode CommandLineTools, Darling install).
// If none is found we fall through to bare `-target` and let clang
// produce a clear "header not found" error rather than guessing.
func clangTargetFlag() []string {
	if !explicitTarget {
		return nil
	}

	t := clangTripleForTarget()
	if t == "" {
		return nil
	}

	flags := []string{"-target", t}

	switch targetGOOS {
	case "darwin":
		// Cross-compiling to Darwin from any host: clang needs the
		// macOS SDK so it can find malloc/malloc.h, libunwind.h, and
		// the Mach-O system frameworks. Native Apple clang on macOS
		// finds it automatically via xcrun, so this is mostly a
		// concern when host != Darwin.
		if sdk := macosSDKPath(); sdk != "" {
			flags = append(flags, "-isysroot", sdk)
		}
	case "linux":
		// Cross-compiling to Linux needs a sysroot with glibc headers
		// + the target-arch dynamic linker. The native-Linux host can
		// resolve /usr/include for its own arch but not for foreign
		// archs (Arch x86 host can't satisfy /lib/ld-linux-aarch64.so.1
		// without the cross-toolchain sysroot).
		if runtime.GOOS != "linux" || runtime.GOARCH != targetGOARCH {
			if sysroot := linuxSysrootPath(); sysroot != "" {
				flags = append(flags, "--sysroot", sysroot)
			}
		}
	}

	return flags
}

// hostClangTargetFlag returns the cross-compile flags for a HOST-arch
// build, deliberately ignoring -target. Used by emitPureFnCache: per-fn
// .so files are dlopen'd by the running tin process during CTFE, so they
// must match the host's ABI even when the user asked for a Darwin
// cross-compile. Returns nil so clang uses its built-in default triple
// (which IS the host triple).
func hostClangTargetFlag() []string { return nil }

// macosSDKPath returns the path to a macOS SDK (the directory whose
// usr/include holds Darwin headers). Resolved in this order:
//  1. -macos-sdk CLI flag
//  2. $TIN_MACOS_SDK env var
//  3. xcrun --show-sdk-path on Darwin hosts (or via Darling on Linux)
//  4. Well-known locations: Xcode CommandLineTools, Darling install
//
// Returns "" when no SDK is found; clang will then error with a clear
// "header not found" message rather than producing weird link failures.
func macosSDKPath() string {
	if macosSDKOverride != "" {
		return macosSDKOverride
	}

	if env := os.Getenv("TIN_MACOS_SDK"); env != "" {
		return env
	}

	if out, err := exec.Command("xcrun", "--show-sdk-path").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if _, statErr := os.Stat(p); statErr == nil {
				return p
			}
		}
	}

	candidates := []string{
		"/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk",
		"/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk",
	}
	if home, err := os.UserHomeDir(); err == nil {
		// Darling mounts the macOS / at $HOME/.darling/. The SDK shows
		// up at the same path inside that prefix when CommandLineTools
		// is installed via `darling shell -- xcode-select --install`.
		candidates = append(candidates,
			filepath.Join(home, ".darling/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk"))
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	return ""
}

// linuxSysrootPath returns the path to a Linux rootfs (the directory
// whose usr/include holds glibc/musl headers and whose usr/lib has the
// target-arch dynamic linker). Resolution order:
//  1. --linux-sysroot CLI flag
//  2. $TIN_LINUX_SYSROOT env var
//  3. Well-known Homebrew cross-toolchain paths (x86_64-linux-gnu)
//  4. /opt/cross/<arch>-linux-gnu (osxcross-style)
//
// Returns "" when no sysroot is found; clang will then error with
// "stdio.h: not found" or similar, which is clear enough.
func linuxSysrootPath() string {
	if linuxSysrootOverride != "" {
		return linuxSysrootOverride
	}

	if env := os.Getenv("TIN_LINUX_SYSROOT"); env != "" {
		return env
	}

	arch := targetGOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}

	candidates := []string{
		"/opt/cross/" + arch + "-linux-gnu",
		"/usr/local/" + arch + "-linux-gnu",
		"/opt/homebrew/" + arch + "-linux-gnu",
		"/usr/" + arch + "-linux-gnu",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	return ""
}

// archMatches reports whether the optional [arch] qualifier in a directive
// matches the compilation target. qualifier is the raw bracket content, e.g.
// "x86_64" or "aarch64,darwin". Returns true when qualifier is empty (no
// constraint) or every comma-separated token matches.
//
// Supported tokens:
//
//	x86_64   - targetGOARCH == "amd64"
//	aarch64  - targetGOARCH == "arm64"
//	386      - targetGOARCH == "386"
//	darwin   - targetGOOS  == "darwin"
//	linux    - targetGOOS  == "linux"
func archMatches(qualifier string) bool {
	if qualifier == "" {
		return true
	}

	for _, tok := range strings.Split(qualifier, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "x86_64":
			if targetGOARCH != "amd64" {
				return false
			}
		case "aarch64":
			if targetGOARCH != "arm64" {
				return false
			}
		case "386":
			if targetGOARCH != "386" {
				return false
			}
		case "darwin":
			if targetGOOS != "darwin" {
				return false
			}
		case "linux":
			if targetGOOS != "linux" {
				return false
			}
		}
	}

	return true
}

// extractArchQualifier splits a directive token (file path or linker flag) into
// the base value and an optional arch qualifier. The qualifier is the content
// inside trailing [...], e.g.:
//
//	"helper.c [x86_64]"  -> ("helper.c", "x86_64")
//	"-lm [darwin]"       -> ("-lm", "darwin")
//	"helper.c"           -> ("helper.c", "")
func extractArchQualifier(s string) (base, qualifier string) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "["); i >= 0 && strings.HasSuffix(s, "]") {
		qualifier = strings.TrimSpace(s[i+1 : len(s)-1])
		base = strings.TrimSpace(s[:i])

		return
	}

	return s, ""
}

// expandShellExprs replaces $(cmd args...) tokens in s with the trimmed stdout
// of running that command via the shell. Tokens whose command fails are left
// as empty strings so the caller surfaces a meaningful compiler error instead
// of a cryptic path.
func expandShellExprs(s string) string {
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

		var val string
		if err == nil {
			val = strings.TrimSpace(string(out))
		}

		s = s[:start] + val + s[end+1:]
	}

	return s
}

// parseFileDirectives scans the leading lines of src for //! directives and
// returns linker flags, C source files to compile in, and valgrind
// suppression paths to apply when the test runs under --valgrind.
//
//	//!-lm                         -> linker flag -lm
//	//!-lm [x86_64]                -> linker flag -lm, x86_64 only
//	//!-framework Cocoa [darwin]   -> two argv entries: -framework, Cocoa
//	//!+helper.c                   -> compile helper.c alongside the module
//	//!+src/foo.c -- -DDEBUG       -> compile src/foo.c with extra flag -DDEBUG
//	//!+src/foo.c [arch]           -> compile only on matching arch
//	//!+src/foo.c [arch] -- FLAGS  -> arch-specific file with extra flags
//	//!-suppressions=PATH          -> pass --suppressions=PATH to valgrind
//	                                   for this file (no effect outside --valgrind)
//
// Linker-flag directives are tokenized on whitespace AFTER $ENV / $(cmd)
// expansion so multi-token flags like `-framework Cocoa` reach the linker
// as separate argv entries.  Embed `$(brew --prefix foo)/lib/libfoo.a`
// when you need to keep a path containing spaces in one token; that
// command-substitution form is expanded before tokenization.
//
// srcDir is the directory of the .tin file; relative paths are resolved
// against it.  $TIN_RUNTIME / $TIN_STDLIB / $ENV variables expand in
// suppression paths the same way they do in //!+file flags.  Scanning
// stops at the first non-comment, non-blank line.
func parseFileDirectives(src, srcDir, stdlibDir string) (linkerFlags []string, cSources []cSource, vgSuppressions []string) {
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "//!") {
			continue
		}

		if strings.HasPrefix(trimmed, "//!") {
			rest := strings.TrimSpace(trimmed[3:])
			if rest == "" {
				continue
			}

			if strings.HasPrefix(rest, "+") {
				spec := strings.TrimSpace(rest[1:])
				// Split on " -- " to separate file+qualifier from extra flags.
				parts := strings.SplitN(spec, " -- ", 2)
				fileAndQualifier, archQualifier := extractArchQualifier(strings.TrimSpace(parts[0]))

				if !archMatches(archQualifier) {
					continue
				}

				cpath := filepath.Join(srcDir, fileAndQualifier)

				var extraFlags []string

				if len(parts) == 2 {
					rtDir := tinRuntimeDir()

					expandVars := func(s string) string {
						s = strings.ReplaceAll(s, "$TIN_RUNTIME", rtDir)
						s = strings.ReplaceAll(s, "$TIN_STDLIB", stdlibDir)
						// Expand any remaining $VAR tokens from the environment.
						return os.ExpandEnv(s)
					}

					// Expand $(cmd) expressions before field splitting so
					// commands with spaces (e.g. "brew --prefix foo") aren't
					// split across multiple fields.
					fields := strings.Fields(expandShellExprs(parts[1]))
					for i := 0; i < len(fields); i++ {
						f := expandVars(fields[i])

						var iPath string

						if f == "-I" && i+1 < len(fields) {
							// "-I path" (space-separated)
							i++
							iPath = expandVars(fields[i])
						} else if strings.HasPrefix(f, "-I") && len(f) > 2 {
							// "-Ipath" (no space)
							iPath = f[2:]
						}

						if iPath != "" {
							if !filepath.IsAbs(iPath) {
								iPath = filepath.Join(srcDir, iPath)
							}

							extraFlags = append(extraFlags, "-I"+iPath)
						} else {
							extraFlags = append(extraFlags, f)
						}
					}
				}

				cSources = append(cSources, cSource{path: cpath, flags: extraFlags})
			} else if strings.HasPrefix(rest, "-suppressions=") {
				// Valgrind-only directive: register a suppressions file
				// that applies when the binary runs under --valgrind.
				// Honors the same `[arch]` qualifier as the other
				// directives so platform-specific suppressions stay
				// scoped (e.g. a glibc-only file is skipped on macOS).
				specAndQualifier, archQualifier := extractArchQualifier(strings.TrimPrefix(rest, "-suppressions="))
				if !archMatches(archQualifier) {
					continue
				}

				rtDir := tinRuntimeDir()
				expanded := strings.ReplaceAll(specAndQualifier, "$TIN_RUNTIME", rtDir)
				expanded = strings.ReplaceAll(expanded, "$TIN_STDLIB", stdlibDir)
				expanded = os.ExpandEnv(expanded)

				if !filepath.IsAbs(expanded) {
					expanded = filepath.Join(srcDir, expanded)
				}

				vgSuppressions = append(vgSuppressions, expanded)
			} else {
				// Linker flag: check for optional arch qualifier, expand
				// $ENV / $(cmd) tokens, then split into individual argv
				// entries.  Multi-token flags like `-framework Cocoa` or
				// `-Xlinker -rpath -Xlinker $ORIGIN` need to reach the
				// linker as separate argv elements -- ld looks up
				// `-framework` and `Cocoa` independently and rejects the
				// concatenated form.  Mirrors how //!+file.c -- FLAGS
				// tokenizes its trailing flag list.
				flagAndQualifier, archQualifier := extractArchQualifier(rest)
				if archMatches(archQualifier) {
					expanded := os.ExpandEnv(expandShellExprs(flagAndQualifier))
					linkerFlags = append(linkerFlags, strings.Fields(expanded)...)
				}
			}

			continue
		}

		break
	}

	return
}

// dedupLinkerFlags removes duplicate linker flags while keeping `(flag, value)`
// pairs together for flags that take a positional argument (-framework,
// -Xlinker, ...). Naive per-token dedup orphans the value -- e.g.
// `-framework Foundation -framework AppKit` would collapse to
// `-framework Foundation AppKit` and ld treats AppKit as a plain input file.
func dedupLinkerFlags(flags []string) []string {
	takesValue := map[string]bool{
		"-framework":      true,
		"-weak_framework": true,
		"-Xlinker":        true,
		"-rpath":          true,
	}

	seen := map[string]bool{}
	out := flags[:0]

	for i := 0; i < len(flags); i++ {
		f := flags[i]
		if takesValue[f] && i+1 < len(flags) {
			key := f + " " + flags[i+1]

			if !seen[key] {
				seen[key] = true

				out = append(out, f, flags[i+1])
			}

			i++

			continue
		}

		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}

	return out
}
