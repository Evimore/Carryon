// Package ui writes Carryon's messages to the terminal or the journal.
//
// Lines start with a marker: "::" for information, "!!" for warnings and "xx" for errors. When
// stderr is the systemd journal, each line also gets a syslog level prefix ("<6>", "<4>", "<3>"),
// so `journalctl -p err` finds the errors. The prefix is added only when stderr really is the
// journal: systemd sets JOURNAL_STREAM to the device and inode of the stream it connected, and a
// child process whose stderr was redirected elsewhere must not print the prefixes.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Syslog levels used for the journal prefix.
const (
	levelError = 3
	levelWarn  = 4
	levelInfo  = 6
)

// Logger writes marked messages.
type Logger struct {
	mu      sync.Mutex
	w       io.Writer
	journal bool // prefix lines with syslog levels
	color   bool // colour the markers
}

// NewLogger writes to w. journal adds syslog level prefixes; color colours the markers.
func NewLogger(w io.Writer, journal, color bool) *Logger {
	return &Logger{w: w, journal: journal, color: color}
}

// Stderr returns a logger for the process's stderr, detecting the journal and a terminal.
func Stderr() *Logger {
	return NewLogger(os.Stderr, stderrIsJournal(), isTerminal(os.Stderr))
}

// Info reports progress.
func (l *Logger) Info(format string, a ...any) { l.emit(levelInfo, "::", "1;34", format, a...) }

// Warn reports something the user should know about that didn't stop the run.
func (l *Logger) Warn(format string, a ...any) { l.emit(levelWarn, "!!", "1;33", format, a...) }

// Error reports a failure.
func (l *Logger) Error(format string, a ...any) { l.emit(levelError, "xx", "1;31", format, a...) }

func (l *Logger) emit(level int, mark, color, format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(l.w, l.format(level, mark, color, fmt.Sprintf(format, a...)))
}

func (l *Logger) format(level int, mark, color, msg string) string {
	if l.color {
		mark = "\033[" + color + "m" + mark + "\033[0m"
	}
	text := mark + " " + msg
	if !l.journal {
		return text + "\n"
	}
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(&b, "<%d>%s\n", level, line)
	}
	return b.String()
}

// journalStreamMatches reports whether JOURNAL_STREAM ("device:inode") names the given stream.
func journalStreamMatches(env string, dev, ino uint64) bool {
	return env != "" && env == fmt.Sprintf("%d:%d", dev, ino)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
