package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/paths"
)

// The inbox is inbox.json: suggestions waiting for the user. The pacman hook, the nightly backup
// and the commands write it, each under the inbox lock; anyone may read it, so the user agent and
// `carryon inbox` work without root. It holds no secrets and no file contents: paths, sizes, app
// and bundle names.
//
// Writers change it with UpdateInbox, which waits for the lock no longer than they ask: the pacman
// hook must never block pacman, so it passes a short wait and gives up on ErrInboxBusy. Readers
// call ReadInbox and need no lock, since the file is always replaced in one step.

// InboxFormat is the version of inbox.json's layout. Readers ignore fields they don't know.
const InboxFormat = 1

// InboxLockFile is held while a writer changes the inbox.
const InboxLockFile = "inbox.lock"

// InboxWait is how long commands wait for the inbox lock. The pacman hook waits less.
const InboxWait = 10 * time.Second

// What an inbox entry asks.
const (
	// KindPath: back up this folder? Entry.Path names it.
	KindPath = "path"
	// KindRestore: restore this app's saved settings? Entry.Bundle names the bundle.
	KindRestore = "restore"
)

// What found an entry, for Entry.By.
const (
	ByHook    = "pacman-hook"
	ByBackup  = "nightly-backup"
	ByFlatpak = "flatpak"
)

// Entry is one suggestion. Its fields are in alphabetical order of their JSON names. A restore
// offer's Changes say in places and counts what restoring would change ("enables
// obs-studio.service"), never file contents: anyone can read the inbox.
type Entry struct {
	App     string    `json:"app,omitempty"`     // the probable app: a package name
	Bundle  string    `json:"bundle,omitempty"`  // the bundle it would join, or the one to restore
	By      string    `json:"by,omitempty"`      // what found it: ByHook, ByBackup...
	Changes []string  `json:"changes,omitempty"` // for KindRestore: what it would change, in short lines
	Found   time.Time `json:"found"`             // when it was first found
	ID      string    `json:"id"`                // see EntryID
	Kind    string    `json:"kind"`              // KindPath or KindRestore
	Large   bool      `json:"large,omitempty"`   // bigger than detect.large_path_gb
	Path    string    `json:"path,omitempty"`    // for KindPath: the folder
	Size    int64     `json:"size,omitempty"`    // in bytes, when measured
}

// Target is what the entry is about: its path, or for a restore offer its bundle.
func (e Entry) Target() string {
	if e.Kind == KindRestore {
		return e.Bundle
	}
	return e.Path
}

// NewEntry makes an entry of kind about target (a path for KindPath, a bundle ID for KindRestore),
// with its ID set.
func NewEntry(kind, target string) Entry {
	e := Entry{Kind: kind, ID: EntryID(kind, target)}
	if kind == KindRestore {
		e.Bundle = target
	} else {
		e.Path = filepath.Clean(target)
	}
	return e
}

// EntryID is the ID of the entry of kind about target: the kind and ten hex digits of a hash of
// the target ("path-3fa9c2d1e0"). It is the same every time the same thing is found, short, and
// safe as a systemd instance name (carryon-add@path-3fa9c2d1e0.service), so the agent's buttons
// never pass a path.
func EntryID(kind, target string) string {
	if kind == KindPath {
		target = filepath.Clean(target)
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + target))
	return kind + "-" + hex.EncodeToString(sum[:5])
}

var entryIDRE = regexp.MustCompile(`^(path|restore)-[0-9a-f]{10}$`)

// ValidEntryID reports whether s has the form of an entry ID. It says nothing of whether the
// inbox holds it.
func ValidEntryID(s string) bool { return entryIDRE.MatchString(s) }

// Inbox is inbox.json.
type Inbox struct {
	Format  int     `json:"format"`
	Entries []Entry `json:"entries"`
	// IgnoredBundles are bundles whose restore offer the user ignored, so it isn't made again.
	// Ignored paths live in detect.ignore in config.toml instead, so they travel with the backup.
	IgnoredBundles []string `json:"ignored_bundles,omitempty"`
}

// Find returns the entry with ID arg, or else the path entry for the path arg.
func (in Inbox) Find(arg string) (Entry, bool) {
	for _, e := range in.Entries {
		if e.ID == arg {
			return e, true
		}
	}
	if filepath.IsAbs(arg) {
		p := filepath.Clean(arg)
		for _, e := range in.Entries {
			if e.Kind == KindPath && e.Path == p {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// Put adds e, or replaces the entry with its ID. A replaced entry keeps when and by what it was
// first found. isNew reports whether e wasn't there before. e needs an ID; NewEntry sets it.
func (in *Inbox) Put(e Entry) (isNew bool) {
	for i, have := range in.Entries {
		if have.ID == e.ID {
			e.Found, e.By = have.Found, have.By
			in.Entries[i] = e
			return false
		}
	}
	in.Entries = append(in.Entries, e)
	return true
}

// Remove takes out the entry with ID id. ok is false when there is none.
func (in *Inbox) Remove(id string) (removed Entry, ok bool) {
	for i, e := range in.Entries {
		if e.ID == id {
			in.Entries = append(in.Entries[:i:i], in.Entries[i+1:]...)
			return e, true
		}
	}
	return Entry{}, false
}

// ErrInboxBusy means another writer held the inbox lock for longer than the caller would wait.
var ErrInboxBusy = errors.New("the inbox is busy: another Carryon command is changing it")

// ErrInboxDamaged means inbox.json can't be read. UpdateInbox sets such a file aside.
var ErrInboxDamaged = errors.New("the inbox file is damaged")

// ReadInbox reads the inbox. No file is an empty inbox. A damaged file returns an empty inbox and
// an error wrapping ErrInboxDamaged; the next write sets it aside.
func ReadInbox(d paths.Dirs) (Inbox, error) {
	in := Inbox{Format: InboxFormat, Entries: []Entry{}}
	file := d.InboxFile()
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("reading %s: %w", file, err)
	}
	var got Inbox
	if err := json.Unmarshal(data, &got); err != nil {
		return in, fmt.Errorf("%w: %s (%v)", ErrInboxDamaged, file, err)
	}
	if got.Entries == nil {
		got.Entries = []Entry{}
	}
	got.Format = InboxFormat
	return got, nil
}

// InboxCount is the number of entries in the inbox, 0 when it can't be read.
func InboxCount(d paths.Dirs) int { return inboxCount(d.InboxFile()) }

// UpdateInbox changes the inbox under its lock: it reads the file, calls change and writes the
// result, world-readable, in one step. It waits for the lock no longer than wait and then returns
// ErrInboxBusy. Nothing is written when change fails. A damaged file is set aside next to the
// inbox (setAside names where) and change starts from an empty inbox.
func UpdateInbox(d paths.Dirs, wait time.Duration, change func(*Inbox) error) (setAside string, err error) {
	unlock, err := lockInbox(d, wait)
	if err != nil {
		return "", err
	}
	defer unlock()
	in, err := ReadInbox(d)
	if errors.Is(err, ErrInboxDamaged) {
		setAside = d.InboxFile() + ".damaged"
		if err := os.Rename(d.InboxFile(), setAside); err != nil {
			return "", fmt.Errorf("setting the damaged inbox aside: %w", err)
		}
	} else if err != nil {
		return "", err
	}
	if err := change(&in); err != nil {
		return setAside, err
	}
	sort.SliceStable(in.Entries, func(i, j int) bool {
		if !in.Entries[i].Found.Equal(in.Entries[j].Found) {
			return in.Entries[i].Found.Before(in.Entries[j].Found)
		}
		return in.Entries[i].ID < in.Entries[j].ID
	})
	sort.Strings(in.IgnoredBundles)
	in.Format = InboxFormat
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return setAside, err
	}
	if err := writeFileAtomic(d.InboxFile(), append(data, '\n'), 0o644); err != nil {
		return setAside, fmt.Errorf("writing %s: %w", d.InboxFile(), err)
	}
	return setAside, nil
}

// lockInbox takes the inbox lock, trying until wait has passed. Like the backup lock, it goes
// away with the process.
func lockInbox(d paths.Dirs, wait time.Duration) (unlock func(), err error) {
	if err := os.MkdirAll(d.State, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d.State, InboxLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("taking the inbox lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("taking the inbox lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrInboxBusy
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}
