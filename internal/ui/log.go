// Package ui contains the small terminal helpers used by dbxdl: a leveled
// logger, size/duration formatting and a download progress renderer.
//
// Everything in this package degrades gracefully when stdout is not a TTY: no
// escape sequences are emitted and the progress bar falls back to plain lines.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Level controls how much the Logger emits.
type Level int

const (
	// LevelDebug prints everything, including per-file download detail.
	LevelDebug Level = iota
	// LevelInfo is the default: steps, warnings and errors.
	LevelInfo
	// LevelWarn hides informational output.
	LevelWarn
	// LevelError only prints failures ("--quiet").
	LevelError
)

// ANSI attributes, applied only when color is enabled.
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

// Logger is a mutex-protected, level-filtered writer. A single Logger is
// shared by every goroutine that produces output, which keeps interleaved
// download logs readable.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	color bool
}

// NewLogger builds a Logger writing to w.
func NewLogger(w io.Writer, level Level, color bool) *Logger {
	return &Logger{w: w, level: level, color: color}
}

// SetLevel changes the verbosity threshold.
func (l *Logger) SetLevel(v Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = v
}

// Level returns the current verbosity threshold.
func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// Writer exposes the underlying destination, used for tables and reports that
// are not log records.
func (l *Logger) Writer() io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w
}

// Color reports whether ANSI color is enabled.
func (l *Logger) Color() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.color
}

// SetColor enables or disables ANSI color.
func (l *Logger) SetColor(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.color = v
}

func (l *Logger) emit(lv Level, tag, color, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if l.color && color != "" {
		fmt.Fprintf(l.w, "%s%s%s %s\n", color, tag, reset, msg)
		return
	}
	fmt.Fprintf(l.w, "%s %s\n", tag, msg)
}

// Debugf logs at LevelDebug.
func (l *Logger) Debugf(format string, args ...any) {
	l.emit(LevelDebug, "DEBUG", dim, format, args...)
}

// Infof logs at LevelInfo.
func (l *Logger) Infof(format string, args ...any) {
	l.emit(LevelInfo, "INFO", cyan, format, args...)
}

// Stepf logs a progress step at LevelInfo, prefixed with an arrow.
func (l *Logger) Stepf(format string, args ...any) {
	l.emit(LevelInfo, "  ->", blue, format, args...)
}

// Okf logs a success line at LevelInfo.
func (l *Logger) Okf(format string, args ...any) {
	l.emit(LevelInfo, "  ok", green, format, args...)
}

// Warnf logs at LevelWarn.
func (l *Logger) Warnf(format string, args ...any) {
	l.emit(LevelWarn, "WARN", yellow, format, args...)
}

// Errorf logs at LevelError.
func (l *Logger) Errorf(format string, args ...any) {
	l.emit(LevelError, "FAIL", red, format, args...)
}

// Heading prints a bold section heading, unless output is very quiet.
func (l *Logger) Heading(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.level > LevelInfo {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if l.color {
		fmt.Fprintf(l.w, "\n%s%s%s\n", bold, msg, reset)
		return
	}
	fmt.Fprintf(l.w, "\n%s\n", msg)
}

// IsTerminal reports whether w is attached to a character device.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// ShouldColor decides whether ANSI color may be used for w: a TTY with a
// usable TERM and no NO_COLOR opt-out.
func ShouldColor(w io.Writer, force bool) bool {
	if force {
		return true
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(w)
}

// HumanBytes formats a byte count using binary multiples.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// HumanSpeed formats a transfer rate in bytes per second.
func HumanSpeed(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "-"
	}
	return HumanBytes(int64(bytesPerSecond)) + "/s"
}

// padLeft left-pads s with spaces so that it occupies exactly width columns.
func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}
