package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// parallel-job tracking. parallelEvent is called from worker
	// goroutines inside runParallelClang; parMu serializes stderr writes
	// so the per-job lines don't interleave. activeJobs is the set of
	// currently-running job descriptions, used to render the TTY status
	// line as a compact "running: a, b, c (3/N)" summary.
	parMu      sync.Mutex
	activeJobs map[string]bool
	parTotal   int
	parDone    int
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

// parallelStart prepares the progress instance for a parallel-jobs phase
// of size total. The current sequential stage line (if any in non-TTY
// mode) is closed out so per-job lines aren't appended onto its trailing
// elapsed-time placeholder.
func (p *compileProgress) parallelStart(total int) {
	if !p.verbose {
		return
	}

	p.parMu.Lock()
	defer p.parMu.Unlock()

	p.parTotal = total
	p.parDone = 0
	p.activeJobs = make(map[string]bool, total)

	if !p.isTTY && p.currentStage != "" {
		// Close out the pending step()'s "  <elapsed>" line so the
		// per-job lines below it start on their own line.
		fmt.Fprintln(os.Stderr)

		p.currentStage = ""
	}
}

// parallelEvent is the runParallelClang callback. kind is "start" or
// "done"; for "done", elapsed is the wall time the job took. Safe to
// call from worker goroutines (parMu serializes stderr writes).
//
// Render strategy:
//   - non-TTY: one line per event; "start" prints `[par] desc start`,
//     "done" prints `[par] desc done <elapsed>`. Easy to grep in CI logs.
//   - TTY: in-place status line summarizing the active set,
//     `running 3/N: foo, bar, baz`. "done" lines are not printed
//     individually; the counter advances and dropped jobs disappear.
func (p *compileProgress) parallelEvent(desc, kind string, elapsed time.Duration) {
	if !p.verbose {
		return
	}

	p.parMu.Lock()
	defer p.parMu.Unlock()

	switch kind {
	case "start":
		if p.activeJobs == nil {
			p.activeJobs = make(map[string]bool)
		}

		p.activeJobs[desc] = true
	case "done":
		delete(p.activeJobs, desc)
		p.parDone++
	}

	if p.isTTY {
		p.renderParallelLine()
	} else if kind == "start" {
		fmt.Fprintf(os.Stderr, "[par %d/%d] %s start\n", p.parDone+len(p.activeJobs), p.parTotal, desc)
	} else {
		fmt.Fprintf(os.Stderr, "[par %d/%d] %s done %s\n", p.parDone, p.parTotal, desc, formatElapsed(elapsed))
	}
}

// renderParallelLine paints a TTY-friendly summary of the active job
// set. Caller must hold parMu.
func (p *compileProgress) renderParallelLine() {
	descs := make([]string, 0, len(p.activeJobs))
	for d := range p.activeJobs {
		descs = append(descs, d)
	}
	// Sort for stable display; map iteration order is randomized.
	for i := 1; i < len(descs); i++ {
		for j := i; j > 0 && descs[j-1] > descs[j]; j-- {
			descs[j-1], descs[j] = descs[j], descs[j-1]
		}
	}

	body := strings.Join(descs, ", ")
	msg := fmt.Sprintf("running %d/%d: %s", p.parDone+len(descs), p.parTotal, body)

	if len(msg) > progressLineWidth {
		msg = msg[:progressLineWidth-3] + "..."
	}

	pad := ""
	if len(msg) < progressLineWidth {
		pad = strings.Repeat(" ", progressLineWidth-len(msg))
	}

	_, _ = fmt.Fprintf(os.Stderr, "\r%s%s", msg, pad)
}

// parallelEnd closes out a parallel section. TTY: erases the running
// line so the next sequential step() starts clean. Non-TTY: prints a
// closing summary with the parallel-section wall time.
func (p *compileProgress) parallelEnd(elapsed time.Duration) {
	if !p.verbose {
		return
	}

	p.parMu.Lock()
	defer p.parMu.Unlock()

	if p.isTTY {
		_, _ = fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", progressLineWidth))
	} else {
		fmt.Fprintf(os.Stderr, "[par] %d jobs done in %s\n", p.parTotal, formatElapsed(elapsed))
	}

	p.activeJobs = nil
	p.parTotal = 0
	p.parDone = 0
}
