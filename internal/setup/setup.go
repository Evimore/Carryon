// Package setup sets Carryon up on a machine: `carryon init`, `init --connect` and the
// recovery kit. See docs/design.md, "Getting back in".
//
// Everything it touches outside the repository goes through Env, so tests run it against temp
// folders, a fake mount table, a fake systemctl and a restic repository in a temp folder.
package setup

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// Env is what setup uses from the machine. The zero value of every field but Dirs, Prompt, Log and
// Out has a working default for the real system.
type Env struct {
	Dirs   paths.Dirs
	Prompt ui.Prompter
	Log    *ui.Logger
	Out    io.Writer // the recovery kit and other output the user reads

	// Runner runs systemctl. Nil means real programs.
	Runner sysexec.Runner
	// Root is the system root the mount checks read (/proc/self/mountinfo, /dev/disk/by-uuid):
	// "/" unless set.
	Root string
	// SystemdDir is where schedule drop-ins go: /etc/systemd/system unless set.
	SystemdDir string

	// Hostname is the machine's hostname; os.Hostname unless set.
	Hostname func() (string, error)
	// Fingerprint identifies this machine's hardware ("" when unknown); facts' fingerprint of the
	// real system unless set.
	Fingerprint func() string
	// Now is the time; time.Now unless set.
	Now func() time.Time
	// Rand is the source of generated passwords; crypto/rand unless set.
	Rand io.Reader
	// Engine returns a restic engine for a repository; engine.New unless set. Tests use it to give
	// restic a clean environment.
	Engine func(repo config.Repository, machine string) *engine.Engine
}

func (e *Env) root() string       { return orDefault(e.Root, "/") }
func (e *Env) systemdDir() string { return orDefault(e.SystemdDir, "/etc/systemd/system") }

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) rand() io.Reader {
	if e.Rand != nil {
		return e.Rand
	}
	return rand.Reader
}

func (e *Env) fingerprint() string {
	if e.Fingerprint != nil {
		return e.Fingerprint()
	}
	return facts.New("/", nil).Fingerprint()
}

func (e *Env) hostname() (string, error) {
	if e.Hostname != nil {
		return e.Hostname()
	}
	return os.Hostname()
}

func (e *Env) runner() sysexec.Runner {
	if e.Runner != nil {
		return e.Runner
	}
	r := sysexec.New()
	r.Timeout = 2 * time.Minute
	return r
}

func (e *Env) engine(repo config.Repository, machine string) *engine.Engine {
	if e.Engine != nil {
		return e.Engine(repo, machine)
	}
	return engine.New(repo, e.Dirs, machine)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// KitConfirmedFile holds the time the user confirmed they saved the recovery kit.
func KitConfirmedFile(d paths.Dirs) string { return filepath.Join(d.State, "recovery-kit-confirmed") }

// writePrivate writes a file only root can read, creating its folder.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	// WriteFileAtomic keeps an existing file's mode; a secret must not stay readable.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protecting %s: %w", path, err)
	}
	return nil
}

// writePublic writes a file anyone may read, creating its folder.
func writePublic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return config.WriteFileAtomic(path, data, 0o644)
}

// replaceFile writes data to path. When path already holds something else, that is kept next to
// it as path+suffix first, so reusing another machine's settings never loses this one's.
func replaceFile(path string, data []byte, suffix string) (kept string, err error) {
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("reading %s: %w", path, err)
	case string(old) == string(data):
		return "", nil
	default:
		kept = path + suffix
		if err := config.WriteFileAtomic(kept, old, 0o644); err != nil {
			return "", err
		}
	}
	return kept, writePublic(path, data)
}

// markKitConfirmed records that the user saved the recovery kit.
func (e *Env) markKitConfirmed() error {
	return writePublic(KitConfirmedFile(e.Dirs), []byte(e.now().Format(time.RFC3339)+"\n"))
}

// KitConfirmed reports whether the user confirmed they saved the recovery kit.
func KitConfirmed(d paths.Dirs) bool {
	_, err := os.Stat(KitConfirmedFile(d))
	return err == nil
}

// setRestoring pauses automatic backups until `carryon restore` finishes.
func (e *Env) setRestoring() error {
	return writePublic(e.Dirs.RestoringFlag(), []byte(e.now().Format(time.RFC3339)+"\n"))
}

// quote quotes s for a shell when it needs it, for commands the user copies.
func quote(s string) string { return sysexec.Join([]string{s}) }

// splitList splits a list the user typed, separated by commas or spaces.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}
