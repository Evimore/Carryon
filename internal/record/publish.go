package record

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/sysexec"
)

// Files the record store writes next to what capture stages.
const (
	GitignoreFile    = ".gitignore"
	LastCaptureFile  = "last-capture"     // when the record was last published; not in the history
	NotBackedUpFile  = "not-backed-up.md" // what was left out on purpose; not in the history
	maxSubject       = 200                // runes in a commit subject
	maxBodyLines     = 300                // change lines in a commit body
	briefChanges     = 4                  // change lines named in a commit subject
	firstRecordTitle = "first record"
)

// Names next to the record folder DIR, used while publishing:
//
//	DIR.tmp-*  a stage being copied in from another filesystem (incomplete; removed on recovery)
//	DIR.new    a complete new record, waiting to replace DIR
//	DIR.old    the previous record, just replaced; its git history moves into DIR
//	DIR.lock   held while publishing, so two publishes don't interleave
const (
	tmpSuffix  = ".tmp-"
	newSuffix  = ".new"
	oldSuffix  = ".old"
	lockSuffix = ".lock"
)

// ErrHistory marks a Publish error that happened after the new record was in place: the record is
// published, only its git history wasn't updated.
var ErrHistory = errors.New("the system record was saved, but its history wasn't updated")

// PublishOptions are the choices for Publish.
type PublishOptions struct {
	// Commit records the change in the record's git history. Captures written somewhere else
	// than the machine's own record (for a look, or a test) leave it off.
	Commit bool
	// Git runs git. Nil, or git not installed, means no history (not an error).
	Git sysexec.Runner
	// Now is the clock for last-capture and commit dates. Nil means time.Now.
	Now func() time.Time
}

// PublishResult says what changed between the previous record and the published one.
type PublishResult struct {
	Added, Removed, Changed []Item // sorted by key; empty on the first record
	First                   bool   // there was no record before
	Committed               bool   // a commit was added to the history
}

// Changes describes the changes, one line per item (see ChangeLines).
func (r PublishResult) Changes() []string { return ChangeLines(r.Added, r.Removed, r.Changed) }

// Publish makes the record staged in stage the record in dir, all at once.
//
// stage is a folder written by capture: meta.json, items.jsonl, bundles.json and the files
// beside them. Publish adds SUMMARY.md, .gitignore and last-capture, keeps the previous
// not-backed-up.md when the stage has none (quick captures don't make one), and moves the stage
// into place: on success stage no longer exists. On an error before that, stage is left for the
// caller to remove. Stage next to dir, on the same filesystem; otherwise Publish copies the stage
// first, which still works but isn't a single step.
//
// A reader of dir sees the old record or the new one, never a mix of the two. If the process
// dies part way, the next Publish (or Recover) finishes or undoes it. dir's git history is kept
// and dir is made readable by its owner only (0700).
//
// With opts.Commit, a commit is added when items changed, or "first record" when the history is
// empty. An error from git comes back wrapped in ErrHistory: the record itself is published.
func Publish(ctx context.Context, stage, dir string, opts PublishOptions) (PublishResult, error) {
	var res PublishResult
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	dir = filepath.Clean(dir)
	next, err := Load(stage)
	if err != nil {
		return res, fmt.Errorf("the new system record is incomplete: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(stage, ".git")); err == nil {
		return res, fmt.Errorf("%s holds a .git folder: a staged record can't bring its own history", stage)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return res, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := recoverLocked(dir); err != nil {
		return res, err
	}

	old, hadOld, err := readItemsIn(dir)
	if err != nil {
		return res, err
	}
	res.First = !hadOld
	if hadOld {
		res.Added, res.Removed, res.Changed = Diff(old, next.Items)
	}

	stamp := now()
	extra := map[string]string{
		SummaryFile:     next.Summary(),
		GitignoreFile:   LastCaptureFile + "\n" + NotBackedUpFile + "\n",
		LastCaptureFile: stamp.Format(time.RFC3339) + "\n",
	}
	for name, text := range extra {
		if err := os.WriteFile(filepath.Join(stage, name), []byte(text), 0o644); err != nil {
			return res, err
		}
	}
	if err := keepNotBackedUp(stage, dir); err != nil {
		return res, err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		return res, err
	}
	if err := syncTree(stage); err != nil {
		return res, err
	}
	if err := moveIn(stage, dir); err != nil {
		return res, err
	}
	if err := swap(dir); err != nil {
		return res, err
	}

	if opts.Commit {
		g, ok := gitFor(opts.Git, dir)
		if ok {
			res.Committed, err = commit(ctx, g, stamp, res.Changes())
			if err != nil {
				return res, fmt.Errorf("%w: %w", ErrHistory, err)
			}
		}
	}
	return res, nil
}

// Recover finishes or undoes a publish into dir that was interrupted, and removes its leftovers.
// Publish does this itself first; Recover is for callers that read the record without publishing.
func Recover(dir string) error {
	dir = filepath.Clean(dir)
	if _, err := os.Stat(filepath.Dir(dir)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	return recoverLocked(dir)
}

// lock takes dir's publish lock, waiting for another publish to finish.
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(dir+lockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking the system record: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking the system record: %w", err)
	}
	return func() { f.Close() }, nil // closing releases the lock
}

// recoverLocked brings dir back to one complete record after an interrupted publish:
//
//   - DIR.tmp-* are incomplete copies: removed.
//   - DIR missing, DIR.new present: the swap stopped half way; DIR.new is complete, so it goes in.
//   - DIR missing, only DIR.old present: DIR.old goes back.
//   - DIR present, DIR.new present: the new record never went in; the old one stays.
//   - DIR.old present next to DIR: its history moves into DIR, the rest is removed.
func recoverLocked(dir string) error {
	parent, base := filepath.Split(dir)
	entries, err := os.ReadDir(filepath.Clean(parent))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base+tmpSuffix) {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				return err
			}
		}
	}
	newDir, oldDir := dir+newSuffix, dir+oldSuffix
	if !exists(dir) {
		switch {
		case exists(newDir):
			if err := os.Rename(newDir, dir); err != nil {
				return fmt.Errorf("recovering the system record: %w", err)
			}
		case exists(oldDir):
			if err := os.Rename(oldDir, dir); err != nil {
				return fmt.Errorf("recovering the system record: %w", err)
			}
		}
	}
	if err := os.RemoveAll(newDir); err != nil {
		return err
	}
	if exists(oldDir) {
		if err := adoptHistory(oldDir, dir); err != nil {
			return err
		}
		if err := os.RemoveAll(oldDir); err != nil {
			return err
		}
	}
	return syncDir(parent)
}

// moveIn moves the finished stage to DIR.new. Across filesystems, it copies it to DIR.tmp-*
// first, so DIR.new only ever names a complete record.
func moveIn(stage, dir string) error {
	newDir := dir + newSuffix
	err := renameStage(stage, newDir)
	if err == nil {
		return syncDir(filepath.Dir(dir))
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("moving the new system record into place: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+tmpSuffix)
	if err != nil {
		return err
	}
	if err := copyTree(stage, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("copying the new system record into place: %w", err)
	}
	if err := syncTree(tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, newDir); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

// renameStage moves the stage; tests replace it to act out a move across filesystems.
var renameStage = os.Rename

// swap puts DIR.new in DIR's place and moves the git history over.
func swap(dir string) error {
	newDir, oldDir := dir+newSuffix, dir+oldSuffix
	if exists(dir) {
		if err := os.Rename(dir, oldDir); err != nil {
			return fmt.Errorf("replacing the system record: %w", err)
		}
		if err := os.Rename(newDir, dir); err != nil {
			if back := os.Rename(oldDir, dir); back != nil {
				return fmt.Errorf("replacing the system record: %w (and putting the old one back: %v)", err, back)
			}
			return fmt.Errorf("replacing the system record: %w", err)
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
		if err := adoptHistory(oldDir, dir); err != nil {
			return err
		}
		return os.RemoveAll(oldDir)
	}
	if err := os.Rename(newDir, dir); err != nil {
		return fmt.Errorf("moving the new system record into place: %w", err)
	}
	return syncDir(filepath.Dir(dir))
}

// adoptHistory moves from's .git folder into to, unless to has one already.
func adoptHistory(from, to string) error {
	src, dst := filepath.Join(from, ".git"), filepath.Join(to, ".git")
	if !exists(src) || exists(dst) {
		return nil
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("keeping the system record's history: %w", err)
	}
	return syncDir(to)
}

// keepNotBackedUp copies dir's not-backed-up.md into stage when stage has none.
func keepNotBackedUp(stage, dir string) error {
	dst := filepath.Join(stage, NotBackedUpFile)
	if exists(dst) {
		return nil
	}
	src := filepath.Join(dir, NotBackedUpFile)
	info, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return nil
	}
	if err != nil {
		return err
	}
	return copyFile(src, dst, info.Mode().Perm())
}

// readItemsIn reads the items of the record in dir, if there is one. A record whose items can't
// be read counts as having none, so a broken record never blocks its replacement.
func readItemsIn(dir string) ([]Item, bool, error) {
	f, err := os.Open(filepath.Join(dir, ItemsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	items, err := ReadItems(f)
	if err != nil {
		return nil, true, nil
	}
	return items, true, nil
}

// commit adds the published record to the history when items changed, or as "first record" when
// the history is empty. It reports whether it made a commit.
func commit(ctx context.Context, g gitRepo, when time.Time, lines []string) (bool, error) {
	ok, err := g.exists()
	if err != nil {
		return false, err
	}
	if !ok {
		if _, err := g.output(ctx, time.Time{}, "init", "-q", "--initial-branch=main"); err != nil {
			return false, err
		}
	}
	hasCommits, err := g.hasCommits(ctx)
	if err != nil {
		return false, err
	}
	if hasCommits && len(lines) == 0 {
		return false, nil
	}
	subject, body := commitMessage(lines, !hasCommits)
	if _, err := g.output(ctx, time.Time{}, "add", "-A"); err != nil {
		return false, err
	}
	args := []string{"commit", "-q", "--no-verify", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	if _, err := g.output(ctx, when, args...); err != nil {
		return false, err
	}
	return true, nil
}

var spaces = regexp.MustCompile(`\s+`)

// commitMessage is the subject and body of a commit for the change lines, like the prototype's:
// "3 change(s): + package vim [extra], ..." and up to 300 change lines.
func commitMessage(lines []string, first bool) (subject, body string) {
	if first {
		return firstRecordTitle, ""
	}
	short := make([]string, len(lines))
	for i, l := range lines {
		short[i] = spaces.ReplaceAllString(l, " ")
	}
	subject = fmt.Sprintf("%d change(s): %s", len(lines), Brief(short, briefChanges))
	if r := []rune(subject); len(r) > maxSubject {
		subject = string(r[:maxSubject])
	}
	if len(lines) > maxBodyLines {
		lines = lines[:maxBodyLines]
	}
	return subject, strings.Join(lines, "\n")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// syncTree flushes every file and folder under root to disk.
func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// copyTree copies the folders, files and symlinks under src into the existing folder dst,
// keeping permission bits.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel != "." {
				if err := os.Mkdir(target, 0o700); err != nil {
					return err
				}
			}
			return os.Chmod(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		}
		return fmt.Errorf("%s: not a file, folder or symlink", path)
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm) // the umask may have narrowed it
}
