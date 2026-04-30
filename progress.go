package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const progressLineWidth = 80

// compileProgress prints stage progress to stderr when verbose is true.
//
// Two render modes, picked at construction by inspecting stderr:
//
//   - TTY: each top-level stage / detail event uses \r to overwrite the
//     previous line, so only the current activity is visible — Zig-style
//     in-place progress. detail() events stream through.
//
//   - non-TTY (file or pipe): \r overwrites become noise (every event
//     becomes its own line in the log). Print one line per top-level
//     stage transition with the elapsed time of the stage that just
//     finished, and drop detail() events entirely. clear() emits the
//     total wall time.
//
// Both modes pay the same cost when verbose is false (early return).
type compileProgress struct {
	verbose      bool
	isTTY        bool // stderr is a character device
	current      int
	total        int
	sourceFile   string    // entry .tin used by stages without their own file
	currentFile  string    // filename shown in the current line
	currentStage string    // top-level stage name (e.g. "codegen")
	stageStart   time.Time // when currentStage began
	overallStart time.Time // when the first step() fired
}

func (p *compileProgress) detectTTY() {
	if fi, err := os.Stderr.Stat(); err == nil {
		p.isTTY = (fi.Mode() & os.ModeCharDevice) != 0
	}
}

func (p *compileProgress) setTotal(n int) {
	if p.verbose {
		p.total = n
	}
}

// step starts a new top-level stage (increments the counter and updates the
// current file / stage name). Call before the stage's work begins.
func (p *compileProgress) step(filename, stage string) {
	if !p.verbose {
		return
	}

	// Lazy TTY detect on first call; cheap and avoids needing a constructor.
	if p.overallStart.IsZero() {
		p.detectTTY()
		p.overallStart = time.Now()
	}

	// Non-TTY: finalize the previous stage with its elapsed time on its
	// own line before starting the next.
	if !p.isTTY && p.currentStage != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", formatElapsed(time.Since(p.stageStart)))
	}

	p.current++
	p.currentFile = filename
	p.currentStage = stage
	p.stageStart = time.Now()

	if p.isTTY {
		p.render("")
	} else {
		name := filepath.Base(p.currentFile)
		// Right-padded stage label with no trailing newline; the next
		// step() (or clear()) will append the elapsed time and newline.
		fmt.Fprintf(os.Stderr, "[%d/%d] %-26s %-32s",
			p.current, p.total, name, p.currentStage)
	}
}

// detail shows a sub-step within the current top-level stage without
// incrementing the stage counter. TTY only: in non-TTY mode these would
// flood the log without any way to overwrite, so they are dropped.
func (p *compileProgress) detail(info string) {
	if !p.verbose || !p.isTTY {
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

// clear finalizes the progress display. TTY: erase the in-place line.
// non-TTY: close the last stage with its elapsed time and emit one
// summary line with total wall time.
func (p *compileProgress) clear() {
	if !p.verbose {
		return
	}

	if p.isTTY {
		_, _ = fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", progressLineWidth))

		return
	}

	if p.currentStage != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", formatElapsed(time.Since(p.stageStart)))
	}

	if !p.overallStart.IsZero() {
		fmt.Fprintf(os.Stderr, "done in %s\n", formatElapsed(time.Since(p.overallStart)))
	}

	// Reset so a second use of the same compileProgress (e.g. CTFE cache
	// emit after main link) starts fresh instead of double-printing.
	p.currentStage = ""
	p.overallStart = time.Time{}
}

// formatElapsed renders d in a compact, log-friendly form: ms with
// no decimal under 10s; seconds with one decimal beyond. Avoids the
// noisy "1.234567ms" Go default.
func formatElapsed(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}

	return fmt.Sprintf("%.1fs", d.Seconds())
}
