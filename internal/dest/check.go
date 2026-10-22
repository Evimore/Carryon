package dest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/engine"
)

// Status is the outcome of checking a destination. Only OK allows writing.
type Status int

const (
	// OK: the destination is the one recorded at init and can be written to.
	OK Status = iota
	// NotMounted: the path now sits on a different filesystem than recorded, usually the system disk
	// because the drive isn't plugged in. Writing would fill the system disk.
	NotMounted
	// DifferentDisk: a filesystem is mounted at the recorded place, but not the recorded one.
	DifferentDisk
	// RepoMismatch: the repository there isn't the one recorded at init.
	RepoMismatch
	// Missing: the destination folder isn't there, on the drive that should hold it.
	Missing
	// Unreachable: the destination can't be reached or read right now (offline, permission denied,
	// no recorded identity to compare with).
	Unreachable
	// CantOpen: the destination answered, but the repository there can't be opened: a wrong
	// password, no repository, an expired login. Waiting won't fix it.
	CantOpen
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case NotMounted:
		return "not mounted"
	case DifferentDisk:
		return "different disk"
	case RepoMismatch:
		return "different repository"
	case Missing:
		return "missing"
	case Unreachable:
		return "unreachable"
	case CantOpen:
		return "can't open"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Result says whether a destination can be written to and, if not, why and what to do.
type Result struct {
	Status  Status
	Message string // what is wrong, in plain words ("" when OK)
	Fix     string // what the user can do about it
	Mount   Mount  // the mount the destination is on now (local destinations)
}

// OK reports whether the destination may be written to.
func (r Result) OK() bool { return r.Status == OK }

func (r Result) String() string {
	if r.OK() {
		return "the backup destination is ready"
	}
	if r.Fix == "" {
		return r.Message
	}
	return r.Message + "\n" + r.Fix
}

// Runner runs a program and returns its standard output. The caller passes one already set to run as
// the right user (for rclone, the user whose rclone config holds the login). When the program fails,
// the error should carry its standard error, like *exec.ExitError does.
type Runner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Checker checks destinations before a run.
type Checker struct {
	Root   string     // the system root: "/", or a fake tree in tests
	Repo   Repository // reads the repository's ID
	Runner Runner     // runs rclone for rclone: destinations; may be nil for other backends
}

// remoteSchemes are restic's repository URL prefixes that aren't a local folder.
var remoteSchemes = []string{"sftp:", "rest:", "s3:", "b2:", "azure:", "gs:", "swift:", "rclone:"}

// IsLocal reports whether a restic repository URL is a folder on this machine ("/path" or
// "local:/path").
func IsLocal(repo string) bool {
	for _, s := range remoteSchemes {
		if strings.HasPrefix(repo, s) {
			return false
		}
	}
	return true
}

// LocalPath returns the folder of a local repository URL.
func LocalPath(repo string) string { return strings.TrimPrefix(repo, "local:") }

// Check decides whether Carryon may write to the repository at url. For a local folder it compares the
// filesystem and repository with want, recorded at init. For other backends it checks the remote
// answers and holds the recorded repository. It never returns OK unless every recorded part matches.
func (c Checker) Check(ctx context.Context, url string, want Identity) Result {
	if want.RepoID == "" {
		return Result{
			Status:  Unreachable,
			Message: "Carryon has no record of which repository belongs at " + url + ".",
			Fix:     "Run `carryon init` to connect to the repository again.",
		}
	}
	if IsLocal(url) {
		return c.checkLocal(ctx, LocalPath(url), want)
	}
	if strings.HasPrefix(url, "rclone:") {
		if r, bad := c.checkRclone(ctx, url); bad {
			return r
		}
	}
	return c.checkRepoID(ctx, url, want.RepoID)
}

func (c Checker) checkLocal(ctx context.Context, path string, want Identity) Result {
	if want.MountPoint == "" {
		return Result{
			Status:  Unreachable,
			Message: "Carryon has no record of which drive holds the backup at " + path + ".",
			Fix:     "Run `carryon init` to connect to the repository again.",
		}
	}
	root := c.Root
	if root == "" {
		root = "/"
	}
	if !filepath.IsAbs(path) {
		return Result{
			Status:  Unreachable,
			Message: fmt.Sprintf("The backup destination %q isn't an absolute path.", path),
			Fix:     "Set repository.url in the config to the full path, starting with /.",
		}
	}
	mounts, err := ReadMounts(root)
	if err != nil {
		return Result{
			Status:  Unreachable,
			Message: "Carryon couldn't read the list of mounted drives, so it can't tell whether the backup drive is there: " + err.Error(),
			Fix:     "Check that /proc is mounted, then try again.",
		}
	}
	resolved, err := ResolvePath(root, path)
	if err != nil {
		return Result{Status: Unreachable, Message: "Carryon couldn't follow the path " + path + ": " + err.Error()}
	}
	m, ok := mounts.Containing(resolved)
	if !ok {
		return Result{
			Status:  NotMounted,
			Message: "No drive is mounted at " + path + ".",
			Fix:     "Plug in or mount the backup drive. Carryon backs up when it appears.",
		}
	}
	if m.MountPoint != filepath.Clean(want.MountPoint) {
		where := "the system disk"
		if m.MountPoint != "/" {
			where = describe(m)
		}
		return Result{
			Status:  NotMounted,
			Message: fmt.Sprintf("The backup drive isn't mounted: %s is on %s right now, not on the drive Carryon set up at %s. Carryon wrote nothing, so it won't fill the wrong disk.", path, where, want.MountPoint),
			Fix:     "Plug in or mount the backup drive. Carryon backs up when it appears.",
			Mount:   m,
		}
	}
	if want.FSUUID != "" {
		uuid, err := FSUUID(root, m)
		if err != nil || !strings.EqualFold(uuid, want.FSUUID) {
			msg := fmt.Sprintf("A different drive is mounted at %s (filesystem %s, expected %s). Carryon wrote nothing to it.", m.MountPoint, orUnknown(uuid), want.FSUUID)
			if err != nil && !errors.Is(err, ErrNoUUID) {
				msg = fmt.Sprintf("Carryon couldn't confirm the drive at %s is the backup drive: %v. It wrote nothing to it.", m.MountPoint, err)
			}
			return Result{
				Status:  DifferentDisk,
				Message: msg,
				Fix:     "Unmount it and plug in the backup drive. If you replaced the drive on purpose, run `carryon init` again.",
				Mount:   m,
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, resolved)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{
				Status:  Missing,
				Message: "The backup folder " + path + " isn't there on the backup drive.",
				Fix:     "If you moved or renamed it, set repository.url in the config to its new place.",
				Mount:   m,
			}
		}
		return Result{
			Status:  Unreachable,
			Message: "Carryon can't open the backup folder " + path + ": " + err.Error(),
			Fix:     "Check the drive works and that root may read it.",
			Mount:   m,
		}
	}
	r := c.checkRepoID(ctx, path, want.RepoID)
	r.Mount = m
	return r
}

// checkRclone asks rclone to list the top of the remote, the cheapest round trip that proves the
// remote answers and the login works. bad is true when it doesn't.
func (c Checker) checkRclone(ctx context.Context, url string) (r Result, bad bool) {
	remote, err := RcloneRemote(url)
	if err != nil {
		return Result{Status: Unreachable, Message: err.Error(), Fix: "Check repository.url in the config."}, true
	}
	if c.Runner == nil {
		return Result{Status: Unreachable, Message: "Carryon has no way to run rclone for " + remote + "."}, true
	}
	if _, err := c.Runner.Output(ctx, "rclone", "lsf", "--max-depth", "1", remote); err != nil {
		if engine.IsExpiredLogin(stderrOf(err)) {
			return Result{
				Status:  CantOpen,
				Message: fmt.Sprintf("The login to the rclone remote %s has expired or was refused: %s", remote, lastLine(err)),
				Fix:     fmt.Sprintf("Log in again with (as yourself, not root): rclone reconnect %s", remote),
			}, true
		}
		return Result{
			Status:  Unreachable,
			Message: fmt.Sprintf("The rclone remote %s didn't answer: %s", remote, lastLine(err)),
			Fix:     fmt.Sprintf("Check you're online. If the login expired, run (as yourself): rclone reconnect %s", remote),
		}, true
	}
	return Result{}, false
}

func (c Checker) checkRepoID(ctx context.Context, where, want string) Result {
	if c.Repo == nil {
		return Result{Status: Unreachable, Message: "Carryon has no way to read the repository at " + where + "."}
	}
	got, err := c.Repo.RepositoryID(ctx)
	var re *engine.Error
	if errors.As(err, &re) && re.Refused {
		return Result{
			Status:  CantOpen,
			Message: "Carryon couldn't open the backup repository at " + where + ": " + re.Cause,
			Fix:     re.Fix,
		}
	}
	if err != nil {
		return Result{
			Status:  Unreachable,
			Message: "Carryon couldn't read the backup repository at " + where + ": " + lastLine(err),
			Fix:     "Check you're online and the destination is available, then try again.",
		}
	}
	if got != want {
		return Result{
			Status:  RepoMismatch,
			Message: fmt.Sprintf("The repository at %s isn't the one Carryon set up (its ID is %s, expected %s). Carryon wrote nothing to it.", where, orUnknown(got), want),
			Fix:     "Check repository.url in the config. If you replaced the repository on purpose, run `carryon init` again.",
		}
	}
	return Result{Status: OK}
}

// RcloneRemote returns the remote of an rclone: repository URL, with its colon, for rclone commands:
// "rclone:iCloud:backups" gives "iCloud:", and an on-the-fly remote such as
// "rclone::s3,provider=AWS:bucket" gives ":s3,provider=AWS:".
func RcloneRemote(url string) (string, error) {
	rest, ok := strings.CutPrefix(url, "rclone:")
	if !ok {
		return "", fmt.Errorf("%q isn't an rclone repository", url)
	}
	start := 0
	if strings.HasPrefix(rest, ":") {
		start = 1
	}
	i := strings.Index(rest[start:], ":")
	if i < 0 || start+i == 0 {
		return "", fmt.Errorf("the rclone repository %q names no remote (expected rclone:remote:path)", url)
	}
	return rest[:start+i+1], nil
}

// lastLine returns the last non-empty line of a program's error output, or of the error itself.
func lastLine(err error) string {
	lines := strings.Split(strings.TrimSpace(stderrOf(err)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// stderrOf returns a failed program's error output, or the error's own text.
func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(strings.TrimSpace(string(ee.Stderr))) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

func describe(m Mount) string {
	if m.Source != "" && m.Source != "none" {
		return fmt.Sprintf("%s (%s)", m.MountPoint, m.Source)
	}
	return m.MountPoint
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
