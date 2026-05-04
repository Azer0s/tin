package main

// Required-toolchain check.
//
// tin needs `clang` (frontend + C source compile) and `opt` (LLVM IR
// optimizer / ThinLTO pre-link prep / coro split). Cross-compile also
// needs `ld.lld`, but we defer that check to the cross-compile path so
// native builds don't fail on a missing-but-unused tool.
//
// On the fast path we touch a marker file under .build/host-info/tools/
// per detected tool. On subsequent invocations, presence of the marker
// + presence of the tool in $PATH skips the more expensive --version
// probe path. Negative results are NOT cached: a missing tool today may
// be installed tomorrow.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// toolMarkerDir is where the per-tool presence markers live. Sits under
// .build/host-info/ so a single `rm -rf .build/host-info` (or `tin
// clean`) clears every cached probe at once.
const toolMarkerDir = ".build/host-info/tools"

// requireTools verifies the toolchain tin needs to drive the backend.
// On any miss, prints a per-distro install hint to stderr and exits.
// Skipped on warm runs (marker file present + tool still in $PATH).
func requireTools() {
	missing := []string{}

	for _, name := range []string{"clang", "opt"} {
		if !toolAvailable(name) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		printMissingToolsHint(missing)
		os.Exit(1)
	}
}

// toolAvailable returns true when `name` is in $PATH. Touches a marker
// under toolMarkerDir on first detection so the next run can skip the
// LookPath syscall + cache-dir mkdir.
func toolAvailable(name string) bool {
	marker := filepath.Join(toolMarkerDir, name)

	// Fast path: marker present AND the binary still resolves.
	if _, err := os.Stat(marker); err == nil {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
		// Marker stale -- the user uninstalled or moved the tool. Drop
		// the marker so a successful re-detect re-creates it.
		_ = os.Remove(marker)

		return false
	}

	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	// First-time detection: persist the marker. Best-effort; failures
	// just mean the next run repeats the LookPath, which is cheap.
	_ = os.MkdirAll(toolMarkerDir, 0o755)
	_ = os.WriteFile(marker, nil, 0o644)

	return true
}

// printMissingToolsHint writes a tailored install hint to stderr based
// on the detected package manager (or distro family on Linux).
func printMissingToolsHint(missing []string) {
	fmt.Fprintf(os.Stderr, "error: tin needs these tools but couldn't find them in $PATH: %s\n\n",
		strings.Join(missing, ", "))

	switch runtime.GOOS {
	case "darwin":
		fmt.Fprintln(os.Stderr, "Install with Homebrew:")
		fmt.Fprintln(os.Stderr, "    brew install llvm")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Homebrew's LLVM is keg-only -- if it's already installed,")
		fmt.Fprintln(os.Stderr, "you likely need to add its bin directory to your PATH:")
		fmt.Fprintln(os.Stderr, "    export PATH=\"$(brew --prefix llvm)/bin:$PATH\"")

	case "linux":
		switch detectLinuxPM() {
		case "apt":
			fmt.Fprintln(os.Stderr, "Install on Debian / Ubuntu:")
			fmt.Fprintln(os.Stderr, "    sudo apt-get install clang lld llvm")
		case "pacman":
			fmt.Fprintln(os.Stderr, "Install on Arch / Manjaro:")
			fmt.Fprintln(os.Stderr, "    sudo pacman -S clang lld llvm")
		case "dnf":
			fmt.Fprintln(os.Stderr, "Install on Fedora / RHEL / CentOS Stream:")
			fmt.Fprintln(os.Stderr, "    sudo dnf install clang lld llvm")
		case "zypper":
			fmt.Fprintln(os.Stderr, "Install on openSUSE:")
			fmt.Fprintln(os.Stderr, "    sudo zypper install clang lld llvm")
		case "apk":
			fmt.Fprintln(os.Stderr, "Install on Alpine:")
			fmt.Fprintln(os.Stderr, "    apk add clang lld llvm")
		case "xbps":
			fmt.Fprintln(os.Stderr, "Install on Void:")
			fmt.Fprintln(os.Stderr, "    sudo xbps-install -S clang lld llvm")
		default:
			fmt.Fprintln(os.Stderr, "Install via your distro's package manager. Typical names:")
			fmt.Fprintln(os.Stderr, "    clang, lld, llvm")
		}

		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "If LLVM is already installed but the binaries aren't on PATH,")
		fmt.Fprintln(os.Stderr, "look under /usr/lib/llvm-*/bin or /opt/llvm/bin and add that to PATH.")

	default:
		fmt.Fprintln(os.Stderr, "Install LLVM (clang + opt + lld) via your platform's package manager.")
	}
}

// detectLinuxPM returns the canonical name of the most popular package
// manager whose driver binary is on $PATH. Best-effort -- when nothing
// matches we return "" and the caller falls back to a generic message.
func detectLinuxPM() string {
	for _, candidate := range []struct{ bin, name string }{
		{"apt-get", "apt"},
		{"pacman", "pacman"},
		{"dnf", "dnf"},
		{"zypper", "zypper"},
		{"apk", "apk"},
		{"xbps-install", "xbps"},
	} {
		if _, err := exec.LookPath(candidate.bin); err == nil {
			return candidate.name
		}
	}

	return ""
}
