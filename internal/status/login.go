package status

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

// LoginLine is the line shown at console and SSH logins, or "" when nothing needs attention:
// "Carryon: the last backup failed (Oct 18 14:02). See: carryon status". It names the same
// problem as Summary.
func (r Report) LoginLine() string {
	if !r.NeedsAttention() || r.login == "" {
		return ""
	}
	return "Carryon: " + r.login + ". See: carryon status"
}

// LoginOptions are the settings the login message follows.
type LoginOptions struct {
	Enabled bool          // notify.login_message
	MaxAge  time.Duration // verify.max_age_hours
}

// UpdateLogin writes the login message for s, which was just written, or removes it when all is
// well or the message is off. pam_motd shows it at console and SSH logins, so nothing runs then.
// It decides by the same rules as carryon status. Nothing is written when d has no motd folder.
func UpdateLogin(d paths.Dirs, s state.Status, opt LoginOptions, now time.Time) error {
	file := d.LoginMessageFile()
	if file == "" {
		return nil
	}
	line := ""
	if opt.Enabled {
		in := Input{Status: s, Found: true, MaxAge: opt.MaxAge, Now: now, RestoringFlag: d.RestoringFlag()}
		_, err := state.ReadDestination(d)
		in.NotSetUp = errors.Is(err, state.ErrNotSetUp)
		if runs, err := state.ReadRuns(d); err == nil {
			in.Runs = runs
		}
		line = Build(in).LoginLine()
	}
	if line == "" {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing the login message %s: %w", file, err)
		}
		return nil
	}
	if err := writeLogin(file, line+"\n"); err != nil {
		return fmt.Errorf("writing the login message %s: %w", file, err)
	}
	return nil
}

// writeLogin replaces file with data, readable by everyone, through a temporary file so a login
// never shows half a line.
func writeLogin(file, data string) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".carryon-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
