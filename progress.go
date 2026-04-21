package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const progressLineWidth = 80

// compileProgress prints Zig-style stage progress to stderr when verbose is
// true.  Each call to step overwrites the previous line so only the current
// stage is visible.  clear erases the line when compilation completes.
type compileProgress struct {
	verbose      bool
	current      int
	total        int
	sourceFile   string // original .tin source, used by stages without their own file
	currentFile  string // filename shown in the current line
	currentStage string // top-level stage name (e.g. "codegen")
}

func (p *compileProgress) setTotal(n int) {
	if p.verbose {
		p.total = n
	}
}

// step starts a new top-level stage (increments the counter and updates the
// current file / stage name).  Call before the stage's work begins.
func (p *compileProgress) step(filename, stage string) {
	if !p.verbose {
		return
	}

	p.current++
	p.currentFile = filename
	p.currentStage = stage
	p.render("")
}

// detail shows a sub-step within the current top-level stage without
// incrementing the stage counter.  Used for pass boundaries, per-function
// events, imports, CTFE evaluations, and macro expansions reported by codegen.
func (p *compileProgress) detail(info string) {
	if !p.verbose {
		return
	}

	p.render(info)
}

func (p *compileProgress) render(detail string) {
	name := filepath.Base(p.currentFile)
	stageStr := p.currentStage

	if detail != "" {
		stageStr = p.currentStage + ": " + detail
	}

	msg := fmt.Sprintf("[%d/%d] %-26s %s", p.current, p.total, name, stageStr)
	pad := ""

	if len(msg) < progressLineWidth {
		pad = strings.Repeat(" ", progressLineWidth-len(msg))
	}

	_, _ = fmt.Fprintf(os.Stderr, "\r%s%s", msg, pad)
}

// clear erases the progress line.  Call after the final stage succeeds.
func (p *compileProgress) clear() {
	if !p.verbose {
		return
	}

	_, _ = fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", progressLineWidth))
}
