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
// matter for ParseBackup, writes a progress line now and then, and logs files restic couldn't
// read as they come.
type progressWriter struct {
	mu    sync.Mutex
	out   io.Writer // progress lines; nil for none
	every time.Duration
	log   Logger
	last  time.Time
	part  []byte
	keep  bytes.Buffer
	now   func() time.Time
}

func newProgressWriter(out io.Writer, every time.Duration, log Logger) *progressWriter {
	if every <= 0 {
		every = 30 * time.Second
	}
	return &progressWriter{out: out, every: every, log: log, now: time.Now}
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
	MessageType string  `json:"message_type"`
	PercentDone float64 `json:"percent_done"`
	TotalFiles  uint64  `json:"total_files"`
	FilesDone   uint64  `json:"files_done"`
	TotalBytes  uint64  `json:"total_bytes"`
	BytesDone   uint64  `json:"bytes_done"`
	Item        string  `json:"item"`
	Error       *struct {
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
			fmt.Fprintf(p.out, "%3.0f%% done: %s of %s, %d of %d files\n", s.PercentDone*100,
				humanBytes(s.BytesDone), humanBytes(s.TotalBytes), s.FilesDone, s.TotalFiles)
		}
		return
	case "error":
		msg := ""
		if s.Error != nil {
			msg = s.Error.Message
		}
		p.log.Warn("couldn't read %s: %s", s.Item, strings.TrimSpace(msg))
	}
	p.keep.Write(l)
	p.keep.WriteByte('\n')
}

func (p *progressWriter) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.part) > 0 {
		p.line(p.part)
		p.part = nil
	}
}

// output is what restic reported, without the status lines.
func (p *progressWriter) output() engine.BackupOutput {
	p.mu.Lock()
	defer p.mu.Unlock()
	out, _ := engine.ParseBackup(bytes.NewReader(p.keep.Bytes()))
	return out
}
