package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Evimore/carryon/internal/engine"
)

// FindInhibitor returns the systemd-inhibit program when logind answers, so a backup can keep the
// machine awake, or "" when there's none (no systemd, a container, no D-Bus).
func FindInhibitor(ctx context.Context) string {
	prog, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, prog, "--list", "--no-pager", "--no-legend").Run() != nil {
		return ""
	}
	return prog
}

// inhibited runs restic under systemd-inhibit, which holds a logind lock that stops the machine
// from sleeping until restic ends.
func inhibited(inner engine.Runner, inhibitor string) engine.Runner {
	if inner == nil {
		inner = engine.ExecRunner
	}
	return func(ctx context.Context, c engine.Cmd) (int, error) {
		c.Args = append([]string{
			"--what=sleep", "--who=Carryon", "--why=Backing up", "--mode=block", "--", c.Path,
		}, c.Args...)
		c.Path = inhibitor
		return inner(ctx, c)
	}
}

// progressWriter reads `restic backup --json` as restic writes it: it keeps the messages that
// matter for ParseBackup, shows progress, and logs files restic couldn't read as they come.
//
// On a terminal (columns > 0) it redraws one line in place about once a second, with the file
// being read. Otherwise, as for the journal, it writes a plain line every so often. Either way the
// first comes with restic's first status message.
type progressWriter struct {
	mu      sync.Mutex
	out     io.Writer // progress; nil for none
	every   time.Duration
	columns int  // the terminal's width; 0 when out isn't a terminal
	drawn   bool // a line is drawn in place and the cursor is at its end
	log     Logger
	last    time.Time
	part    []byte
	keep    bytes.Buffer
	now     func() time.Time
}

func newProgressWriter(out io.Writer, every time.Duration, columns int, log Logger) *progressWriter {
	switch {
	case columns > 0:
		every = time.Second
	case every <= 0:
		every = 30 * time.Second
	}
	return &progressWriter{out: out, every: every, columns: columns, log: log, now: time.Now}
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data := append(p.part, b...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		p.line(data[:i])
		data = data[i+1:]
	}
	p.part = append([]byte(nil), data...)
	return len(b), nil
}

// status is the part of restic's status message a progress line uses.
type status struct {
	MessageType  string   `json:"message_type"`
	PercentDone  float64  `json:"percent_done"`
	TotalFiles   uint64   `json:"total_files"`
	FilesDone    uint64   `json:"files_done"`
	TotalBytes   uint64   `json:"total_bytes"`
	BytesDone    uint64   `json:"bytes_done"`
	Item         string   `json:"item"`
	CurrentFiles []string `json:"current_files"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *progressWriter) line(l []byte) {
	l = bytes.TrimSpace(l)
	if len(l) == 0 {
		return
	}
	var s status
	if l[0] != '{' || json.Unmarshal(l, &s) != nil {
		return
	}
	switch s.MessageType {
	case "status":
		if p.out != nil && p.now().Sub(p.last) >= p.every {
			p.last = p.now()
			p.show(s)
		}
		return
	case "error":
		msg := ""
		if s.Error != nil {
			msg = s.Error.Message
		}
		p.clear()
		p.log.Warn("couldn't read %s: %s", s.Item, strings.TrimSpace(msg))
	}
	p.keep.Write(l)
	p.keep.WriteByte('\n')
}

// show writes a progress line: in place on a terminal, with the file being read.
func (p *progressWriter) show(s status) {
	text := fmt.Sprintf("%3.0f%% done: %s of %s, %d of %d files", s.PercentDone*100,
		HumanBytes(s.BytesDone), HumanBytes(s.TotalBytes), s.FilesDone, s.TotalFiles)
	if p.columns <= 0 {
		fmt.Fprintln(p.out, text)
		return
	}
	width := p.columns - 1 // the last column would wrap on some terminals
	if room := width - ansi.StringWidth(text) - 2; len(s.CurrentFiles) > 0 && room >= 10 {
		file := s.CurrentFiles[0]
		if over := ansi.StringWidth(file) - room; over > 0 {
			file = ansi.TruncateLeft(file, over+1, "…") // keep the end, where the file's name is
		}
		text += ", " + file
	}
	fmt.Fprint(p.out, "\r\033[K"+ansi.Truncate(text, width, ""))
	p.drawn = true
}

// clear removes the line drawn in place, so what's written next starts on a clean line. The next
// status message draws it again.
func (p *progressWriter) clear() {
	if p.drawn {
		fmt.Fprint(p.out, "\r\033[K")
		p.drawn = false
	}
}

// clearing returns a writer that writes to w on a clean line: restic's own messages then don't
// land in the middle of the line drawn in place.
func (p *progressWriter) clearing(w io.Writer) io.Writer { return clearingWriter{p: p, w: w} }

type clearingWriter struct {
	p *progressWriter
	w io.Writer
}

func (c clearingWriter) Write(b []byte) (int, error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.p.clear()
	return c.w.Write(b)
}

func (p *progressWriter) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.part) > 0 {
		p.line(p.part)
		p.part = nil
	}
	p.clear()
}

// output is what restic reported, without the status lines.
func (p *progressWriter) output() engine.BackupOutput {
	p.mu.Lock()
	defer p.mu.Unlock()
	out, _ := engine.ParseBackup(bytes.NewReader(p.keep.Bytes()))
	return out
}
