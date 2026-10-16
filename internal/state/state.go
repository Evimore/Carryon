// Package state holds what Carryon keeps between runs, next to the record in /var/lib/carryon:
// which destination init set up, the restoring and pending flags, the lock that keeps backups to
// one at a time, the run history (runs.jsonl) and the world-readable summary, status.json. See
// docs/design.md, "Running by itself".
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
)

// Files in the state folder besides the ones internal/paths names.
const (
	// DestinationFile holds the destination's identity (dest.Identity as JSON), written by init.
	// Every run checks the destination against it before writing anything.
	DestinationFile = "destination.json"
	// LastPruneFile holds when the repository was last pruned.
	LastPruneFile = "last-prune"
	// LockFile is held while a backup runs, so there is only ever one.
	LockFile = "backup.lock"
)

// DestinationPath is where init records the destination's identity.
func DestinationPath(d paths.Dirs) string { return filepath.Join(d.State, DestinationFile) }

// ErrNotSetUp means init hasn't recorded a destination yet.
var ErrNotSetUp = errors.New("backups aren't set up on this machine yet: run `sudo carryon init`")

// ReadDestination reads the destination's identity recorded at init. It returns ErrNotSetUp when
// there is none.
func ReadDestination(d paths.Dirs) (dest.Identity, error) {
	var id dest.Identity
	file := DestinationPath(d)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return id, ErrNotSetUp
	}
	if err != nil {
		return id, fmt.Errorf("reading %s: %w", file, err)
	}
	if err := json.Unmarshal(data, &id); err != nil {
		return id, fmt.Errorf("%s is damaged (%v): run `sudo carryon init` to record the destination again", file, err)
	}
	return id, nil
}

// WriteDestination records the destination's identity, replacing any earlier one. The file is
// readable by root only.
func WriteDestination(d paths.Dirs, id dest.Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(DestinationPath(d), append(data, '\n'), 0o600)
}

// Restoring reports whether a restore is in progress, which pauses automatic runs.
func Restoring(d paths.Dirs) bool {
	_, err := os.Lstat(d.RestoringFlag())
	return err == nil
}

// Why the pending marker was written.
const (
	// ReasonChange: the pacman hook saw a package change. A backup follows once changes settle.
	ReasonChange = "change"
	// ReasonWaiting: a backup couldn't reach its destination. It runs as soon as it's back.
	ReasonWaiting = "waiting"
)

// Pending is the marker for a backup that is still owed.
type Pending struct {
	Reason string    `json:"reason"` // ReasonChange or ReasonWaiting
	Since  time.Time `json:"since"`  // when it was last written: the last change, or the last try
}

// ReadPending reads the pending marker. ok is false when there is none.
func ReadPending(d paths.Dirs) (p Pending, ok bool, err error) {
	file := d.PendingFlag()
	fi, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return p, false, err
	}
	p.Reason = strings.TrimSpace(string(data))
	if p.Reason != ReasonWaiting {
		p.Reason = ReasonChange // an empty or unknown marker is a change
	}
	p.Since = fi.ModTime()
	return p, true, nil
}

// MarkPending writes the pending marker. Its time is when, so the settle timer can wait for
// changes to settle.
func MarkPending(d paths.Dirs, reason string, when time.Time) error {
	file := d.PendingFlag()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(file, []byte(reason+"\n"), 0o644); err != nil {
		return err
	}
	return os.Chtimes(file, when, when)
}

// ClearPending removes the pending marker if it was written no later than upTo: a change made
// while a backup ran still needs the next one. It reports whether it removed the marker.
func ClearPending(d paths.Dirs, upTo time.Time) (bool, error) {
	p, ok, err := ReadPending(d)
	if err != nil || !ok || p.Since.After(upTo) {
		return false, err
	}
	if err := os.Remove(d.PendingFlag()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// ErrBusy means another backup holds the lock.
var ErrBusy = errors.New("another backup is running")

// Lock takes the backup lock without waiting. It returns ErrBusy when another run holds it. The
// lock goes away with the process, so a crashed run never leaves it behind.
func Lock(d paths.Dirs) (unlock func(), err error) {
	if err := os.MkdirAll(d.State, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d.State, LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("taking the backup lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("taking the backup lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// LastPrune returns when the repository was last pruned; ok is false if never.
func LastPrune(d paths.Dirs) (t time.Time, ok bool) {
	fi, err := os.Stat(filepath.Join(d.State, LastPruneFile))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// SetLastPrune records a prune at when.
func SetLastPrune(d paths.Dirs, when time.Time) error {
	file := filepath.Join(d.State, LastPruneFile)
	if err := writeFileAtomic(file, []byte(when.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Chtimes(file, when, when)
}

// writeFileAtomic replaces file with data in one step, so readers never see half of it.
func writeFileAtomic(file string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
