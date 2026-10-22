package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

const (
	testPassword    = "correct horse battery staple"
	testFingerprint = "0123456789abcdef"
	testUUID        = "7d1e4b90-2a5c-4f63-8b11-aabbccddeeff"
)

// harness is one machine for setup's tests: temp folders for every Carryon directory, a fake
// system root with a mount table, a fake systemctl and a place for a real restic repository.
type harness struct {
	t      *testing.T
	env    *Env
	script *ui.Script
	fake   *sysexec.Fake
	log    *syncBuffer
	out    *syncBuffer
	tmp    string
	root   string // the fake system root (mount table, /dev)
	drive  string // a folder on the fake backup drive
	repo   string // a local repository URL on the drive
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func needRestic(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed; setup's tests need it")
	}
}

// newHarness makes a machine named desktop. answers are the scripted answers to init's questions.
func newHarness(t *testing.T, answers ...any) *harness {
	t.Helper()
	needRestic(t)
	tmp := t.TempDir()
	h := &harness{
		t:      t,
		script: &ui.Script{Answers: answers},
		fake:   &sysexec.Fake{},
		log:    &syncBuffer{},
		out:    &syncBuffer{},
		tmp:    tmp,
		root:   filepath.Join(tmp, "root"),
		drive:  filepath.Join(tmp, "drive"),
	}
	h.repo = filepath.Join(h.drive, "carryon")
	dirs := paths.Dirs{
		Conf:  filepath.Join(tmp, "etc/carryon"),
		State: filepath.Join(tmp, "var/lib/carryon"),
		Cache: filepath.Join(tmp, "var/cache/carryon"),
		Run:   filepath.Join(tmp, "run/carryon"),
		Share: filepath.Join(tmp, "usr/share/carryon"),
	}
	// The backup drive is mounted at h.drive; everything else is on the system disk.
	h.mounts(
		"20 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw",
		fmt.Sprintf("40 20 8:17 / %s rw,relatime shared:9 - ext4 /dev/sdb1 rw", h.drive),
	)
	h.writeFile(filepath.Join(h.root, "dev/sdb1"), "", 0o644)
	h.mkdir(filepath.Join(h.root, dest.ByUUIDDir))
	if err := os.Symlink("../../sdb1", filepath.Join(h.root, dest.ByUUIDDir, testUUID)); err != nil {
		t.Fatal(err)
	}
	h.mkdir(h.drive)
	// restic uses the real folder; the destination check looks for it in the fake root.
	h.mkdir(filepath.Join(h.root, h.repo))
	home := filepath.Join(tmp, "roothome")
	h.env = &Env{
		Dirs:        dirs,
		Prompt:      h.script,
		Log:         ui.NewLogger(h.log, false, false),
		Out:         h.out,
		Runner:      h.fake,
		Root:        h.root,
		SystemdDir:  filepath.Join(tmp, "etc/systemd/system"),
		Hostname:    func() (string, error) { return "desktop", nil },
		Fingerprint: func() string { return testFingerprint },
		Now:         func() time.Time { return time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC) },
		Engine: func(repo config.Repository, machine string) *engine.Engine {
			e := engine.New(repo, dirs, machine)
			e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
			e.Stderr = io.Discard
			return e
		},
	}
	h.timers(0)
	return h
}

// mounts replaces the fake mount table.
func (h *harness) mounts(lines ...string) {
	h.writeFile(filepath.Join(h.root, dest.MountinfoPath), strings.Join(lines, "\n")+"\n", 0o644)
}

// timers makes the fake systemctl answer daemon-reload and enabling Carryon's timers and the user
// agent with code.
func (h *harness) timers(code int) {
	h.fake.On("systemctl daemon-reload", sysexec.Response{})
	for _, t := range Timers {
		r := sysexec.Response{Code: code}
		if code != 0 {
			r.Stderr = "Failed to enable unit: Unit file " + t + " does not exist."
		}
		h.fake.On("systemctl enable --now "+t, r)
	}
	r := sysexec.Response{Code: code}
	if code != 0 {
		r.Stderr = "Failed to enable unit: Unit file " + AgentUnit + " does not exist."
	}
	h.fake.On("systemctl --global enable "+AgentUnit, r)
}

func (h *harness) mkdir(dir string) {
	h.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeFile(path, body string, mode os.FileMode) {
	h.t.Helper()
	h.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) readFile(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

// engineFor opens the repository at url with password, as machine.
func (h *harness) engineFor(url, password, machine string) *engine.Engine {
	h.t.Helper()
	pw := filepath.Join(h.tmp, "test-password-"+machine)
	h.writeFile(pw, password+"\n", 0o600)
	return h.env.Engine(config.Repository{URL: url, PasswordFile: pw}, machine)
}

// makeRepo creates a repository at url with password.
func (h *harness) makeRepo(url, password string) *engine.Engine {
	h.t.Helper()
	e := h.engineFor(url, password, "test")
	h.restic(e, "init")
	return e
}

func (h *harness) restic(e *engine.Engine, args ...string) string {
	h.t.Helper()
	r, err := e.Run(context.Background(), args...)
	if err == nil {
		err = r.Err()
	}
	if err != nil {
		h.t.Fatalf("restic %s: %v", strings.Join(args, " "), err)
	}
	return r.Stdout
}

// backup backs up files (path: content) as machine with tags, then removes them, as if they were
// on another machine or before a reinstall.
func (h *harness) backup(e *engine.Engine, machine string, tags []string, files map[string]string) {
	h.t.Helper()
	args := []string{"backup", "--host", machine}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	for p, body := range files {
		h.writeFile(p, body, 0o644)
		args = append(args, p)
	}
	h.restic(e, args...)
	for p := range files {
		if err := os.Remove(p); err != nil {
			h.t.Fatal(err)
		}
	}
}

// meta is a record meta.json for a machine with this fingerprint.
func meta(fingerprint string) string {
	data, _ := json.Marshal(map[string]any{"format": 2, "host": "desktop", "machine": fingerprint, "machine_name": "desktop"})
	return string(data)
}

func (h *harness) config() config.Config {
	h.t.Helper()
	cfg, warnings, err := config.Load(h.env.Dirs.ConfigFile())
	if err != nil || len(warnings) > 0 {
		h.t.Fatalf("loading the written config: %v %v", err, warnings)
	}
	if err := cfg.Validate(); err != nil {
		h.t.Fatalf("the written config isn't valid: %v", err)
	}
	return cfg
}

func (h *harness) identity() dest.Identity {
	h.t.Helper()
	id, err := state.ReadDestination(h.env.Dirs)
	if err != nil {
		h.t.Fatalf("no destination identity recorded: %v", err)
	}
	return id
}

func (h *harness) mode(path string) os.FileMode {
	h.t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return st.Mode().Perm()
}

func (h *harness) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// asked returns the titles of the questions the script answered.
func (h *harness) asked() []string {
	var titles []string
	for _, a := range h.script.Asked {
		titles = append(titles, a.Title)
	}
	return titles
}

func (h *harness) wantNoAnswersLeft() {
	h.t.Helper()
	if len(h.script.Answers) > 0 {
		h.t.Errorf("answers left over: %v (asked: %q)", h.script.Answers, h.asked())
	}
}

var sysexecOK = sysexec.Response{}

func randReader() io.Reader { return rand.Reader }
