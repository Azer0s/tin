package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

const usage = `tin - the tin language compiler

Usage:
  tin run   <file.tin>              compile and execute
  tin build <file.tin> [-o out]     compile to native binary
  tin build -lib <file.tin> [-o out] compile to object file (library)
  tin ir    <file.tin>              emit LLVM IR to stdout

Linker flags (passed after the source file):
  -lNAME       link with libNAME (e.g. -lm for libmath)
  -LDIR        add DIR to the library search path
  file.o/.a    link with extra object/archive file
`

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	cmd := os.Args[1]

	// Parse flags: -lib means compile to object file, not a binary.
	libMode := false
	fileArgIdx := 2
	if cmd == "build" && len(os.Args) > 2 && os.Args[2] == "-lib" {
		libMode = true
		fileArgIdx = 3
		if len(os.Args) <= fileArgIdx {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(1)
		}
	}
	file := os.Args[fileArgIdx]

	src, err := os.ReadFile(file)
	if err != nil {
		die("error reading file: %v", err)
	}

	// ── Lex ──────────────────────────────────────────────────────────────────
	l := lexer.New(string(src))
	tokens, lexErr := l.Tokenize()
	if lexErr != nil {
		die("lex error: %v", lexErr)
	}

	// ── Parse ─────────────────────────────────────────────────────────────────
	p := parser.New(tokens)
	prog, parseErr := p.Parse()
	if parseErr != nil {
		die("parse error: %v", parseErr)
	}

	// ── Codegen ───────────────────────────────────────────────────────────────
	cg := codegen.New(file)
	mod, cgErr := cg.Generate(prog)
	if cgErr != nil {
		die("codegen error: %v", cgErr)
	}

	irText := mod.String()

	// Collect linker flags from codegen (source-level link directives).
	srcLinkFlags := make([]string, 0)
	for _, lib := range cg.LinkLibs() {
		srcLinkFlags = append(srcLinkFlags, "-l"+lib)
	}

	switch cmd {
	case "ir":
		fmt.Print(irText)

	case "build":
		out := strings.TrimSuffix(file, filepath.Ext(file))
		if libMode {
			out += ".o"
		}
		// Collect extra link inputs: .o/.a files, -lNAME, -LDIR, -o flag.
		var extraObjs []string
		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if a == "-o" {
				i++
				if i < len(os.Args) {
					out = os.Args[i]
				}
			} else if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}
		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, out, false, libMode, extraObjs); err != nil {
			die("compile error: %v", err)
		}

	case "run":
		tmpRel := strings.TrimSuffix(file, filepath.Ext(file)) + ".tin.out"
		tmp, _ := filepath.Abs(tmpRel)
		// Collect extra link inputs for run mode too.
		var extraObjs []string
		for i := fileArgIdx + 1; i < len(os.Args); i++ {
			a := os.Args[i]
			if strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a") {
				extraObjs = append(extraObjs, a)
			} else if strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-L") {
				extraObjs = append(extraObjs, a)
			}
		}
		extraObjs = append(srcLinkFlags, extraObjs...)
		if err := compileIR(irText, tmp, true, false, extraObjs); err != nil {
			die("compile error: %v", err)
		}
		defer os.Remove(tmp)
		run := exec.Command(tmp)
		run.Stdout = os.Stdout
		run.Stderr = os.Stderr
		if err := run.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			die("run error: %v", err)
		}

	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// compileIR writes the LLVM IR to a temp .ll file and invokes clang.
// If libMode is true, compile to an object file with -c (no linking).
// extraObjs are additional .o/.a files to link with.
func compileIR(ir, outBin string, addRuntime bool, libMode bool, extraObjs []string) error {
	// Write IR to temp file
	llFile, err := os.CreateTemp("", "tin-*.ll")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	defer os.Remove(llFile.Name())
	if _, err := llFile.WriteString(ir); err != nil {
		return err
	}
	llFile.Close()

	// Find runtime .c alongside the tin binary
	ex, _ := os.Executable()
	rtC := filepath.Join(filepath.Dir(ex), "runtime", "runtime.c")

	if libMode {
		// Library mode: compile to object file only (-c), no runtime, no linking.
		args := []string{"-O2", "-c", llFile.Name(), "-o", outBin}
		clang := exec.Command("clang", args...)
		clang.Stdout = os.Stdout
		clang.Stderr = os.Stderr
		return clang.Run()
	}

	args := []string{"-O2", llFile.Name()}
	if _, err := os.Stat(rtC); err == nil {
		args = append(args, rtC)
	}
	args = append(args, extraObjs...)
	args = append(args, "-o", outBin)

	clang := exec.Command("clang", args...)
	clang.Stdout = os.Stdout
	clang.Stderr = os.Stderr
	return clang.Run()
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tin: "+format+"\n", args...)
	os.Exit(1)
}
