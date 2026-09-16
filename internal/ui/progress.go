package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Progress renders the aggregate state of a multi-file download.
//
// On a TTY it draws a single self-updating line; otherwise it prints one plain
// line per file transition so that logs stay readable when redirected to a
// file or a CI console.
type Progress struct {
	mu    sync.Mutex
	w     io.Writer
	tty   bool
	color bool

	total       int64
	done        int64
	transferred int64
	skipped     int64
	filesTotal  int
	filesDone   int
	filesSkip   int
	filesFail   int

	label    string
	started  time.Time
	lastDraw time.Time
	drewBar  bool
	closed   bool
}

// NewProgress creates a renderer. total may be 0 when sizes are unknown, in
// which case bytes are shown without a percentage.
func NewProgress(w io.Writer, color, tty bool, total int64, files int) *Progress {
	return &Progress{
		w:          w,
		tty:        tty,
		color:      color,
		total:      total,
		filesTotal: files,
		started:    time.Now(),
	}
}

// SetTotal updates the expected total byte count once it is known.
func (p *Progress) SetTotal(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = total
}

// Start records that label has begun downloading.
func (p *Progress) Start(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label = label
	if p.tty {
		p.drawLocked()
		return
	}
	fmt.Fprintf(p.w, "  .. %s\n", label)
}

// Add accumulates bytes read from the network in this run.
func (p *Progress) Add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	p.transferred += n
	if p.tty {
		p.drawLocked()
	}
}

// Resume accounts for bytes that were already on disk from an interrupted run
// and therefore did not travel over the network now.
func (p *Progress) Resume(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	if p.tty {
		p.drawLocked()
	}
}

// Finish marks label as completed successfully.
func (p *Progress) Finish(label string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesDone++
	if p.tty {
		p.drawLocked()
		return
	}
	fmt.Fprintf(p.w, "  ok %s (%s)\n", label, HumanBytes(size))
}

// Skip records a file that was already present and valid. Its bytes count
// towards the progress bar but not towards the transferred total.
func (p *Progress) Skip(label string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesDone++
	p.filesSkip++
	if size > 0 {
		p.done += size
		p.skipped += size
	}
	if p.tty {
		p.drawLocked()
		return
	}
	fmt.Fprintf(p.w, "  == %s (already present, %s)\n", label, HumanBytes(size))
}

// Fail records a file that could not be downloaded.
func (p *Progress) Fail(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filesDone++
	p.filesFail++
	if p.tty {
		p.drawLocked()
		return
	}
	fmt.Fprintf(p.w, "  !! %s (failed)\n", label)
}

// Close finalizes the renderer, emitting a trailing newline on a TTY.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.tty && p.drewBar {
		fmt.Fprint(p.w, "\n")
	}
}

// Stats is a snapshot of the counters.
type Stats struct {
	// Done is the number of bytes accounted for (transferred + resumed +
	// cached), which is what the progress bar fills with.
	Done int64
	// Total is the expected size of every planned file.
	Total int64
	// Transferred is the number of bytes actually read from the network in
	// this run.
	Transferred int64
	// Skipped is the byte count of files that were already present and valid.
	Skipped   int64
	FilesDone int
	FilesSkip int
	FilesFail int
}

// Stats returns the counters accumulated so far.
func (p *Progress) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Done:        p.done,
		Total:       p.total,
		Transferred: p.transferred,
		Skipped:     p.skipped,
		FilesDone:   p.filesDone,
		FilesSkip:   p.filesSkip,
		FilesFail:   p.filesFail,
	}
}

// Elapsed returns the wall-clock time since the renderer was created.
func (p *Progress) Elapsed() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.started)
}

func (p *Progress) drawLocked() {
	now := time.Now()
	// Throttle redraws so a fast local download does not spend all of its time
	// formatting progress lines. The final state is always drawn because
	// Finish/Skip/Fail bypass the throttle when the counter is complete.
	if now.Sub(p.lastDraw) < 120*time.Millisecond && p.filesDone < p.filesTotal {
		return
	}
	p.lastDraw = now

	const width = 28
	ratio := 0.0
	if p.total > 0 {
		ratio = float64(p.done) / float64(p.total)
		if ratio > 1 {
			ratio = 1
		}
	}
	filled := int(ratio * width)
	bar := strings.Repeat("=", filled)
	if filled < width {
		bar += ">" + strings.Repeat(" ", width-filled-1)
	}

	speed := HumanSpeed(float64(p.done) / time.Since(p.started).Seconds())

	line := fmt.Sprintf("[%s] %s %s/%s  %d/%d files  %s",
		bar,
		padLeft(fmt.Sprintf("%3.0f%%", ratio*100), 4),
		padLeft(HumanBytes(p.done), 9),
		padLeft(HumanBytes(p.total), 9),
		p.filesDone, p.filesTotal,
		speed,
	)
	if p.color {
		line = cyan + line + reset
	}
	// Truncate to the widest reason, and clear the previous, possibly longer,
	// line with an erase-to-end-of-line escape.
	if len(line) > 100 {
		line = line[:100]
	}
	fmt.Fprintf(p.w, "\r%s\033[K", line)
	p.drewBar = true
}
