package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
)

// Restoring as you go works from a kept source: a copy of the source machine's record and the
// snapshot it came from, in the state folder, root-only like the record itself. carryon init
// --connect keeps one when the user picks "as you go", and every restore from a snapshot keeps
// the one it used. The pacman hook offers its bundles as apps get installed, and restore --bundle
// uses it by default. See docs/design.md, "As you go".

// KeptDirName is the kept source's folder in the state folder.
const KeptDirName = "source"

// Files in the kept source's folder.
const (
	keptRecordDir = "record"      // the record
	keptInfoFile  = "source.json" // KeptInfo
	offersFile    = "offers.json" // the fingerprint of each open offer's plan
)

// SourceTag is the restic tag on the snapshot a kept source comes from, when this machine backs up
// under the source machine's name: retention keeps snapshots with it (forget --keep-tag), so this
// machine's own backups never forget the snapshot its offers restore from.
const SourceTag = "carryon-source"

// ErrNoKept means no source is kept: nothing is offered and restore --bundle needs a machine.
var ErrNoKept = errors.New("no record is kept to restore from as you go")

// KeptInfo describes the kept source. Its fields are in alphabetical order of their JSON names.
type KeptInfo struct {
	Kept     time.Time       `json:"kept"`               // when it was kept
	Machine  string          `json:"machine"`            // the source machine's name
	Restored []string        `json:"restored,omitempty"` // bundles restored from it: not offered again
	Snapshot engine.Snapshot `json:"snapshot"`           // the snapshot; its ID follows tagging
	Tagged   bool            `json:"tagged,omitempty"`   // the snapshot has SourceTag
}

// KeptDir is the kept source's folder.
func KeptDir(stateDir string) string { return filepath.Join(stateDir, KeptDirName) }

// HasKept reports whether a source is kept.
func HasKept(stateDir string) bool {
	_, err := os.Stat(filepath.Join(KeptDir(stateDir), keptInfoFile))
	return err == nil
}

// ReadKept reads what is known of the kept source. It returns ErrNoKept when there is none.
func ReadKept(stateDir string) (KeptInfo, error) {
	var k KeptInfo
	file := filepath.Join(KeptDir(stateDir), keptInfoFile)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return k, ErrNoKept
	}
	if err != nil {
		return k, fmt.Errorf("reading %s: %w", file, err)
	}
	if err := json.Unmarshal(data, &k); err != nil {
		return k, fmt.Errorf("%s is damaged (%v): keep a source again with sudo carryon restore", file, err)
	}
	return k, nil
}

// SaveKept writes what is known of the kept source.
func SaveKept(stateDir string, k KeptInfo) error {
	slices.Sort(k.Restored)
	k.Restored = slices.Compact(k.Restored)
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(KeptDir(stateDir), keptInfoFile), append(data, '\n'))
}

// OpenKept opens the kept source for planning. Its Snapshot is the kept one. It returns
// ErrNoKept when there is none.
func OpenKept(stateDir string) (*Source, KeptInfo, error) {
	k, err := ReadKept(stateDir)
	if err != nil {
		return nil, k, err
	}
	dir := filepath.Join(KeptDir(stateDir), keptRecordDir)
	r, err := record.Load(dir)
	if err != nil {
		return nil, k, fmt.Errorf("the kept record of %s can't be read (%v): keep it again with sudo carryon restore", k.Machine, err)
	}
	snap := k.Snapshot
	return &Source{Dir: dir, Record: r, Snapshot: &snap}, k, nil
}

// Keep keeps src, fetched from a snapshot, as the source to restore from as you go, replacing
// what was kept before. restored are the bundles just restored from it. When the same snapshot was
// kept already, what was restored from it before stays restored. replaced is the source kept
// before from another snapshot, or nil, so the caller can take the tag off its snapshot.
func Keep(stateDir string, src *Source, restored []string, now time.Time) (replaced *KeptInfo, err error) {
	if src.Snapshot == nil {
		return nil, errors.New("only a record fetched from a snapshot can be kept")
	}
	k := KeptInfo{Kept: now, Machine: src.Machine(), Snapshot: *src.Snapshot, Restored: slices.Clone(restored)}
	before, berr := ReadKept(stateDir)
	same := berr == nil && before.Snapshot.ID == src.Snapshot.ID
	switch {
	case same:
		k.Kept, k.Tagged = before.Kept, before.Tagged
		k.Restored = append(k.Restored, before.Restored...)
	case berr == nil:
		replaced = &before
	}
	fail := func(err error) (*KeptInfo, error) {
		return nil, fmt.Errorf("keeping the record of %s: %w", k.Machine, err)
	}
	dir := KeptDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fail(err)
	}
	if dst := filepath.Join(dir, keptRecordDir); src.Dir != dst {
		tmp, err := os.MkdirTemp(dir, ".record-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(tmp)
		if err := copyRecord(src.Dir, tmp); err != nil {
			return fail(err)
		}
		if err := os.RemoveAll(dst); err != nil {
			return fail(err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return fail(err)
		}
	}
	if !same {
		// Offers made from another record don't hold for this one.
		if err := os.Remove(filepath.Join(dir, offersFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(err)
		}
	}
	if err := SaveKept(stateDir, k); err != nil {
		return fail(err)
	}
	return replaced, nil
}

// MarkRestored notes that bundles were restored from the kept source, so they aren't offered
// again, and closes their offers.
func MarkRestored(stateDir string, bundles []string) error {
	k, err := ReadKept(stateDir)
	if err != nil {
		return err
	}
	k.Restored = append(k.Restored, bundles...)
	if err := SaveKept(stateDir, k); err != nil {
		return err
	}
	return changeOffers(stateDir, func(o map[string]offerPrint) {
		for _, b := range bundles {
			delete(o, b)
		}
	})
}

// DropKept removes the kept source: nothing more is offered.
func DropKept(stateDir string) error {
	if err := os.RemoveAll(KeptDir(stateDir)); err != nil {
		return fmt.Errorf("removing the kept record: %w", err)
	}
	return nil
}

// TagSource tags the kept snapshot with SourceTag, so retention keeps it, and follows its new ID:
// restic gives a snapshot a new ID when its tags change. It does nothing when it's tagged
// already. The caller has checked the destination: this writes into the repository.
func TagSource(ctx context.Context, repo Repo, stateDir string) error {
	k, err := ReadKept(stateDir)
	if err != nil || k.Tagged {
		return err
	}
	res, err := repo.Run(ctx, "tag", "--add", SourceTag, k.Snapshot.ID)
	if err == nil {
		err = res.Err()
	}
	if err != nil {
		return fmt.Errorf("tagging snapshot %s, so retention keeps it: %w", k.Snapshot.ShortID, err)
	}
	snaps, err := repo.Snapshots(ctx, k.Snapshot.Hostname)
	if err != nil {
		return fmt.Errorf("finding snapshot %s after tagging it: %w", k.Snapshot.ShortID, err)
	}
	for _, s := range snaps {
		if s.Time.Equal(k.Snapshot.Time) && slices.Contains(s.Tags, SourceTag) && slices.Equal(s.Paths, k.Snapshot.Paths) {
			k.Snapshot, k.Tagged = s, true
			return SaveKept(stateDir, k)
		}
	}
	return fmt.Errorf("snapshot %s was tagged, but can't be found again: run sudo carryon restore --stop-offers, then keep a source again with sudo carryon restore", k.Snapshot.ShortID)
}

// Untag takes SourceTag off a snapshot kept before, so retention may forget it again. The caller
// has checked the destination.
func Untag(ctx context.Context, repo Repo, k KeptInfo) error {
	if !k.Tagged {
		return nil
	}
	res, err := repo.Run(ctx, "tag", "--remove", SourceTag, k.Snapshot.ID)
	if err == nil {
		err = res.Err()
	}
	if err != nil {
		return fmt.Errorf("taking the tag %s off snapshot %s: %w (remove it with: sudo carryon restic tag --remove %s %s)",
			SourceTag, k.Snapshot.ShortID, err, SourceTag, k.Snapshot.ShortID)
	}
	return nil
}

// copyRecord copies a record folder, leaving out its git history.
func copyRecord(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, to)
		case !d.Type().IsRegular():
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// writePrivate writes a file only root can read.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
